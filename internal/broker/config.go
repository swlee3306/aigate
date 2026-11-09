// Package broker is the orch-broker service: the only process that holds the
// AI key and the GitLab token. It serves one JSON request per connection on a
// unix socket, for allow-listed uids only, under hourly and rolling weekly
// limits. Secrets never appear in replies, logs or error strings.
package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config is /etc/orch-broker/broker.json (shape fixed by decision D2).
type Config struct {
	Version      int          `json:"version"`
	Socket       string       `json:"socket"`
	SocketGroup  string       `json:"socket_group"`
	AllowUIDs    []string     `json:"allow_uids"`
	OpenAI       OpenAIConfig `json:"openai"`
	Limits       LimitsConfig `json:"limits"`
	ContextRepos []string     `json:"context_repos"` // legacy: read once for migration, then the registry wins
	RepoRoots    []string     `json:"repo_roots"`    // optional: directories whose direct children may be registered
	GitLab       GitLabConfig `json:"gitlab"`
	uids         []uint32     // allow_uids resolved at start
}

// OpenAIConfig names the model and its price; both are required.
type OpenAIConfig struct {
	Model           string  `json:"model"`
	KeyFile         string  `json:"key_file"`
	MaxOutputTokens int64   `json:"max_output_tokens"`
	MaxInputBytes   int64   `json:"max_input_bytes"`
	Pricing         Pricing `json:"pricing"`
}

// Pricing is USD per million tokens. Cached input is billed at the input rate
// (conservative).
type Pricing struct {
	InputPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputPerMTok float64 `json:"output_usd_per_mtok"`
}

// LimitsConfig holds the hourly caps and the rolling 7-day USD budget.
type LimitsConfig struct {
	RefinePerHour   int     `json:"refine_per_hour"`
	CreateMRPerHour int     `json:"create_mr_per_hour"`
	WeeklyBudgetUSD float64 `json:"weekly_budget_usd"`
}

// GitLabConfig is the single GitLab the broker may talk to. An empty BaseURL
// means MR creation is not configured.
type GitLabConfig struct {
	BaseURL      string            `json:"base_url"`
	TokenFile    string            `json:"token_file"`
	Projects     map[string]string `json:"projects"`
	TargetBranch string            `json:"target_branch"`
}

// Configured reports whether a GitLab base URL is set.
func (g GitLabConfig) Configured() bool { return g.BaseURL != "" }

// UIDs returns the allow-listed uids resolved from allow_uids.
func (c Config) UIDs() []uint32 { return append([]uint32(nil), c.uids...) }

// LoadOptions are the test seams of LoadConfig.
type LoadOptions struct {
	Trust      TrustOptions
	LookupUser func(name string) (uint32, error) // nil: os/user
}

// LoadConfig reads the config after the trust checks (root-owned, no
// group/other write, trusted ancestor chain). Fails closed.
func LoadConfig(path string) (Config, error) { return LoadConfigOpts(path, LoadOptions{}) }

// Defaults for settings whose key is absent. An explicit 0 or negative value
// is refused: a safety limit is never silently widened.
const (
	defaultMaxOutputTokens = 4096
	defaultMaxInputBytes   = 262144
	defaultRefinePerHour   = 20
	defaultCreateMRPerHour = 30
	defaultWeeklyBudgetUSD = 25
)

var (
	errConfigFormat = errors.New("설정 형식이 올바르지 않습니다")
	errBadLimit     = errors.New("한도 값은 생략하거나 양수여야 합니다(0 이하는 시작 거부)")
	projectRE       = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)
	branchRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
)

// LoadConfigOpts is LoadConfig with injectable trust and user lookup. Error
// texts are fixed sentences; they never reflect configuration values.
func LoadConfigOpts(path string, o LoadOptions) (Config, error) {
	data, err := readTrustedFile(path, o.Trust)
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Absent sections take their defaults (present ones go through the
	// presence-aware UnmarshalJSON methods).
	c := Config{Limits: LimitsConfig{RefinePerHour: defaultRefinePerHour, CreateMRPerHour: defaultCreateMRPerHour, WeeklyBudgetUSD: defaultWeeklyBudgetUSD}}
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, errBadLimit) {
			return Config{}, errBadLimit
		}
		return Config{}, errConfigFormat // not wrapped: json errors can quote values
	}
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, errors.New("설정 뒤에 불필요한 데이터가 있습니다")
	}
	lookup := o.LookupUser
	if lookup == nil {
		lookup = lookupUserUID
	}
	if err := c.validate(lookup); err != nil {
		return Config{}, err
	}
	return c, nil
}

func lookupUserUID(name string) (uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}

func goodPositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && v < 1e9 }

func (c *Config) validate(lookup func(string) (uint32, error)) error {
	if c.Version != 1 {
		return errors.New("지원하지 않는 설정 버전입니다")
	}
	if !filepath.IsAbs(c.Socket) || filepath.Clean(c.Socket) != c.Socket {
		return errors.New("소켓 경로는 정리된 절대 경로여야 합니다")
	}
	if c.SocketGroup == "" {
		return errors.New("socket_group이 비어 있습니다")
	}
	if len(c.AllowUIDs) == 0 {
		return errors.New("allow_uids가 비어 있습니다")
	}
	c.uids = nil
	for _, name := range c.AllowUIDs {
		uid, err := lookup(name)
		if name == "" || err != nil {
			return errors.New("allow_uids의 계정을 확인할 수 없습니다")
		}
		c.uids = append(c.uids, uid)
	}
	oa := &c.OpenAI
	if oa.Model == "" {
		return errors.New("openai.model이 비어 있습니다(사용자가 설정에 적어야 합니다)")
	}
	if !goodPositive(oa.Pricing.InputPerMTok) || !goodPositive(oa.Pricing.OutputPerMTok) {
		return errors.New("openai.pricing의 입력·출력 단가는 양수여야 합니다(사용자가 설정에 적어야 합니다)")
	}
	if !filepath.IsAbs(oa.KeyFile) {
		return errors.New("openai.key_file은 절대 경로여야 합니다")
	}
	if oa.MaxOutputTokens <= 0 || oa.MaxInputBytes <= 0 {
		return errBadLimit
	}
	l := &c.Limits
	if l.RefinePerHour <= 0 || l.CreateMRPerHour <= 0 || !goodPositive(l.WeeklyBudgetUSD) {
		return errBadLimit
	}
	for _, r := range c.ContextRepos {
		if !filepath.IsAbs(r) || filepath.Clean(r) != r {
			return errors.New("context_repos는 정리된 절대 경로여야 합니다")
		}
	}
	for _, r := range c.RepoRoots {
		if !cleanAbs(r) {
			return errors.New("repo_roots는 정리된 절대 경로여야 합니다")
		}
	}
	return c.GitLab.validate()
}

func (g *GitLabConfig) validate() error {
	if g.BaseURL == "" {
		if g.TokenFile != "" || len(g.Projects) != 0 {
			return errors.New("gitlab.base_url 없이 다른 gitlab 값을 쓸 수 없습니다")
		}
		return nil
	}
	u, err := url.Parse(g.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("gitlab.base_url은 http(s)://주소[:포트] 형식이어야 합니다")
	}
	if !filepath.IsAbs(g.TokenFile) {
		return errors.New("gitlab.token_file은 절대 경로여야 합니다")
	}
	for repo, proj := range g.Projects {
		if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo || !projectRE.MatchString(proj) || strings.Contains("/"+proj+"/", "/../") || strings.Contains("/"+proj+"/", "/./") {
			return errors.New("gitlab.projects 항목이 올바르지 않습니다")
		}
	}
	if g.TargetBranch == "" {
		g.TargetBranch = "main"
	}
	if !branchRE.MatchString(g.TargetBranch) {
		return errors.New("gitlab.target_branch가 올바르지 않습니다")
	}
	return nil
}

// Secrets are the AI key and the GitLab token read from their files. The
// values are only reachable through accessors; formatting a Secrets value
// prints a fixed placeholder.
type Secrets struct {
	openai string
	gitlab string
}

// OpenAIKey returns the AI key (only for the outbound request code).
func (s Secrets) OpenAIKey() string { return s.openai }

// GitLabToken returns the GitLab token (only for the outbound request code).
func (s Secrets) GitLabToken() string { return s.gitlab }

func (Secrets) String() string   { return "[secrets]" }
func (Secrets) GoString() string { return "[secrets]" }

func (s Secrets) values() []string {
	var v []string
	if s.openai != "" {
		v = append(v, s.openai)
	}
	if s.gitlab != "" {
		v = append(v, s.gitlab)
	}
	return v
}

// LoadSecrets reads the key and token files. A key file the broker's own
// account does not own, or that anyone else can access, refuses start.
// ownerOK nil means "owned by the current effective uid".
func LoadSecrets(c Config, ownerOK func(uint32) bool) (Secrets, error) {
	if ownerOK == nil {
		euid := uint32(os.Geteuid())
		ownerOK = func(uid uint32) bool { return uid == euid }
	}
	var s Secrets
	k, err := readSecretFile(c.OpenAI.KeyFile, ownerOK)
	if err != nil {
		return Secrets{}, errors.New("OpenAI " + err.Error())
	}
	s.openai = k
	if c.GitLab.Configured() {
		t, err := readSecretFile(c.GitLab.TokenFile, ownerOK)
		if err != nil {
			return Secrets{}, errors.New("GitLab 토큰 " + err.Error())
		}
		s.gitlab = t
	}
	return s, nil
}

// strictDecode decodes b into v refusing unknown keys.
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// UnmarshalJSON makes the limits presence-aware: an absent key takes its
// default, an explicit 0 or negative value is refused.
func (l *LimitsConfig) UnmarshalJSON(b []byte) error {
	var a struct {
		Refine   *int     `json:"refine_per_hour"`
		CreateMR *int     `json:"create_mr_per_hour"`
		Budget   *float64 `json:"weekly_budget_usd"`
	}
	if err := strictDecode(b, &a); err != nil {
		return errConfigFormat
	}
	*l = LimitsConfig{RefinePerHour: defaultRefinePerHour, CreateMRPerHour: defaultCreateMRPerHour, WeeklyBudgetUSD: defaultWeeklyBudgetUSD}
	if a.Refine != nil {
		if *a.Refine <= 0 {
			return errBadLimit
		}
		l.RefinePerHour = *a.Refine
	}
	if a.CreateMR != nil {
		if *a.CreateMR <= 0 {
			return errBadLimit
		}
		l.CreateMRPerHour = *a.CreateMR
	}
	if a.Budget != nil {
		if !goodPositive(*a.Budget) {
			return errBadLimit
		}
		l.WeeklyBudgetUSD = *a.Budget
	}
	return nil
}

// UnmarshalJSON makes max_output_tokens and max_input_bytes presence-aware
// in the same way.
func (o *OpenAIConfig) UnmarshalJSON(b []byte) error {
	var a struct {
		Model           string  `json:"model"`
		KeyFile         string  `json:"key_file"`
		MaxOutputTokens *int64  `json:"max_output_tokens"`
		MaxInputBytes   *int64  `json:"max_input_bytes"`
		Pricing         Pricing `json:"pricing"`
	}
	if err := strictDecode(b, &a); err != nil {
		return errConfigFormat
	}
	*o = OpenAIConfig{Model: a.Model, KeyFile: a.KeyFile, Pricing: a.Pricing,
		MaxOutputTokens: defaultMaxOutputTokens, MaxInputBytes: defaultMaxInputBytes}
	if a.MaxOutputTokens != nil {
		if *a.MaxOutputTokens <= 0 {
			return errBadLimit
		}
		o.MaxOutputTokens = *a.MaxOutputTokens
	}
	if a.MaxInputBytes != nil {
		if *a.MaxInputBytes <= 0 {
			return errBadLimit
		}
		o.MaxInputBytes = *a.MaxInputBytes
	}
	return nil
}
