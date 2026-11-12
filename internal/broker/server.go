package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol limits (decision D1).
const (
	MaxRequestBytes = 512 * 1024
	MaxConcurrent   = 4
	DefaultReadTime = 5 * time.Second
	writeTimeout    = 5 * time.Second
)

// HandlerFunc serves one op. args is the request's raw "args" (may be nil).
// Handlers must honor ctx: the server answers `timeout` when it ends, but
// cannot stop a handler that ignores it.
type HandlerFunc func(ctx context.Context, args json.RawMessage) (any, *OpError)

// OpError is a handler failure. Code must be registered in fixedMessages; an
// unregistered code is sent as internal_error. The wire message is always the
// server's fixed sentence for the code: Message is never sent or logged.
type OpError struct {
	Code    string
	Message string
	// Data is optional machine-readable detail sent as "detail" with the
	// failure. Only create_partial carries one (PartialDetail: public values,
	// never a token or provider text); the server drops it for every other code.
	Data any
}

var fixedMessages = map[string]string{
	"forbidden":       "허용되지 않은 호출자입니다.",
	"bad_request":     "요청 형식이 올바르지 않습니다.",
	"unknown_op":      "지원하지 않는 작업입니다.",
	"too_large":       "요청이 너무 큽니다.",
	"timeout":         "요청 시간이 초과되었습니다.",
	"busy":            "처리 중인 요청이 너무 많습니다.",
	"internal_error":  "내부 오류가 발생했습니다.",
	"not_implemented": "아직 구현되지 않은 작업입니다.",
	"budget_exceeded": "AI 사용 예산 또는 호출 한도를 초과했습니다.",

	"repo_not_allowed":  "이 저장소는 AI 전송이 허용되어 있지 않습니다.",
	"bad_file":          "보낼 파일 경로나 내용이 올바르지 않습니다.",
	"looks_like_secret": "비밀 값처럼 보이는 내용이 있어 보낼 수 없습니다.",
	"rate_limited":      "요청 횟수 한도를 초과했습니다. 잠시 후 다시 시도하세요.",
	"upstream_error":    "외부 서비스 호출에 실패했습니다.",
	"bad_plan":          "AI 응답이 계획서 형식을 지키지 않았습니다.",

	"repo_not_mapped": "이 저장소는 GitLab 프로젝트에 연결되어 있지 않습니다.",
	"bad_branch":      "브랜치 이름이 올바르지 않습니다.",
	"bad_url":         "제공자 주소가 허용되지 않습니다. 설정한 제공자 주소나 제공자가 돌려준 주소가 허용 범위 밖입니다.",

	"registry_unavailable": "프로젝트 등록부를 사용할 수 없습니다. 관리자가 확인해야 합니다.",

	"ai_key_missing":     "AI 키가 등록되어 있지 않습니다. 토큰 화면에서 openai 키를 등록하세요.",
	"tokens_unavailable": "토큰 저장소를 사용할 수 없습니다. 관리자가 확인해야 합니다.",

	"not_signed":          "서명이 없는 변경 요청입니다.",
	"bad_signature":       "서명이 올바르지 않습니다.",
	"unknown_signer":      "등록되지 않은 서명자입니다.",
	"stale_signature":     "서명이 만료되었거나 시각이 맞지 않습니다.",
	"signers_unavailable": "서명자 목록을 사용할 수 없습니다. 관리자가 확인해야 합니다.",
	"request_reused":      "이미 사용된 요청입니다.",
	"change_mismatch":     "서명한 변경 내용과 요청 내용이 다릅니다.",
	"project_exists":      "같은 id 또는 경로의 프로젝트가 이미 있습니다.",
	"project_unknown":     "등록되지 않은 프로젝트입니다.",
	"path_not_allowed":    "이 경로는 프로젝트로 등록할 수 없습니다.",
	"not_supported":       "이 버전에서는 지원하지 않는 변경입니다.",

	"token_unknown":           "등록되지 않은 토큰입니다.",
	"bad_kind":                "토큰 종류가 올바르지 않습니다. openai, gitlab, github 중 하나여야 합니다.",
	"token_too_short":         "토큰 값이 너무 짧습니다.",
	"token_bad_format":        "토큰 값의 형식이 올바르지 않습니다. 공백 없는 인쇄 가능한 ASCII 문자여야 하고 너무 길면 안 됩니다.",
	"value_mismatch":          "서명한 토큰 값과 보낸 토큰 값이 다릅니다.",
	"token_exists_other_kind": "같은 이름의 토큰이 다른 종류로 이미 있습니다.",
	"openai_singleton":        "openai 종류는 이름이 openai인 토큰 하나만 둘 수 있습니다.",

	// W3c: providers, groups, project creation. Fixed sentences only: nothing
	// GitLab says is ever shown.
	"provider_unknown":     "등록되지 않은 제공자입니다.",
	"provider_exists":      "같은 id의 제공자가 이미 있습니다.",
	"provider_in_use":      "이 제공자를 쓰는 프로젝트가 있어 삭제하거나 주소를 바꿀 수 없습니다.",
	"token_ref_unknown":    "제공자가 쓸 토큰이 저장소에 없거나, 제공자와 종류가 맞지 않거나, 쓸 수 없습니다. 토큰 화면에서 확인하세요.",
	"provider_unreachable": "제공자(GitLab 또는 GitHub)에 연결하지 못했거나 응답이 없었습니다. 잠시 후 다시 시도하세요.",
	"gitlab_refused":       "GitLab이 요청을 받아들이지 않았습니다. 토큰 권한과 GitLab 설정을 확인하세요.",
	"group_unknown":        "제공자에 그런 그룹(소유자)이 없습니다.",
	"group_exists":         "같은 경로의 그룹이 이미 있습니다.",
	"project_name_taken":   "같은 이름의 프로젝트가 이미 등록되어 있거나 제공자에 있습니다. 기존 프로젝트는 가져오지 않습니다.",
	"create_partial":       "제공자에 일부만 만들어졌습니다. 이 도구는 삭제하지 않으니 제공자(GitLab 또는 GitHub)에서 직접 정리한 뒤 다시 만드세요.",
	"deploy_key_mismatch":  "서명한 배포 키와 보낸 배포 키가 다릅니다.",
	"template_unknown":     "알 수 없는 템플릿입니다.",

	// W3e (E1): the codes the registry-v3 / visibility rules need. The rest of
	// the E-D3 list arrives with the broker flows that raise it (E2, E3).
	"not_supported_for_provider": "이 종류의 제공자에서는 지원하지 않는 작업입니다.",
	"visibility_required":        "공개 여부(public 또는 private)를 반드시 골라야 합니다.",
	"github_refused":             "GitHub이 요청을 받아들이지 않았습니다. 토큰 권한과 GitHub 설정을 확인하세요.",
	"provider_rate_limited":      "제공자가 요청 횟수 한도를 알려 왔습니다. 잠시 후 다시 시도하세요.",
	"visibility_not_allowed":     "이 제공자에서는 공개 여부를 정할 수 없습니다. GitLab 프로젝트는 항상 비공개로 만들어집니다.",
}

// Default per-op processing time limits (decision D1).
var defaultOpTimeouts = map[string]time.Duration{
	"refine":    60 * time.Second,
	"create_mr": 20 * time.Second,
	"status":    5 * time.Second,
	"tokens":    5 * time.Second,

	// A `timeout` reply to a write op means the outcome is unknown: the
	// handler may still complete. Re-read `projects` before retrying.
	"projects":         10 * time.Second,
	"register_project": 10 * time.Second,
	"set_flag":         10 * time.Second,

	// Same for the token writes: `timeout` means the outcome is unknown;
	// re-read `tokens` before retrying.
	"set_token":    10 * time.Second,
	"delete_token": 10 * time.Second,

	// W3c. create_project talks to GitLab many times (60 s overall, 15 s per
	// call); a `timeout` reply to any of the writes means the outcome is
	// UNKNOWN: re-read `projects`/`providers` (and the orphan records) first.
	"providers":       5 * time.Second,
	"list_groups":     20 * time.Second,
	"set_provider":    15 * time.Second,
	"delete_provider": 10 * time.Second,
	"create_group":    20 * time.Second,
	"create_project":  60 * time.Second,
}

// State is the broker's mutable limit state.
type State struct {
	Refine   *HourlyLimiter
	CreateMR *HourlyLimiter
	Budget   *Budget

	// Broker-wide caps of the W3c writes (D9; per-actor limits live in orchd).
	CreateProject *HourlyLimiter
	CreateGroup   *HourlyLimiter
}

// Server is the broker socket server. Zero-valued tuning fields take their
// defaults. PeerUID and AllowUID are test seams.
type Server struct {
	Config   Config
	Handlers map[string]HandlerFunc
	State    State
	Secrets  Secrets
	OpenAI   *OpenAIClient  // set by New; tests may point BaseURL at a fake
	GitLab   *GitLabClient  // set by New; tests may point BaseURL at a fake
	Registry *RegistryStore // set by New: the project registry (owned by the broker)
	Used     *UsedRequests  // set by New: one-time request IDs of signed changes
	Tokens   *TokenStore    // set by New: secret store and token metadata (never nil)

	Now         func() time.Time               // nil: time.Now (test seam: broker clock of signed writes)
	PeerUID     func(net.Conn) (uint32, error) // nil: kernel peer credentials
	AllowUID    func(uint32) bool              // nil: Config.UIDs()
	LookupGroup func(name string) (int, error) // nil: os/user
	Log         io.Writer                      // nil: discard
	ReadTimeout time.Duration                  // 0: 5s
	OpTimeouts  map[string]time.Duration       // overrides per op

	// apiSeams is a TEST-ONLY seam (address policy, resolver, timeouts of the
	// provider GitLab client). Production code never sets it
	// (TestProductionCannotEnableSeams).
	apiSeams *apiSeams

	keyOwnerOK func(uint32) bool // trust of the config key file (nil: current euid)

	logOnce sync.Once
	// logKey caches the config key for log redaction, keyed by the file's
	// (mtime, size); logKeyReads counts real reads (test seam).
	logKeyMu    sync.Mutex
	logKey      logKeyCache
	logKeyReads atomic.Int64
	logger      *log.Logger
	sem         chan struct{}
	mrLock      chan struct{} // serialises create_mr find+create

	creatingMu sync.Mutex
	creating   map[string]bool // project names being created (one create per name at a time)
}

// Options configures New.
type Options struct {
	Clock      func() time.Time
	StateDir   string // holds budget.json; "" = /var/lib/orch-broker
	Log        io.Writer
	KeyOwnerOK func(uint32) bool // nil: owned by the current euid
}

// DefaultStateDir is where budget.json lives.
const DefaultStateDir = "/var/lib/orch-broker"

// New builds a server: it loads the secrets (refusing unsafe key files),
// opens the budget, and registers `status` plus the `not_implemented`
// placeholders of refine and create_mr.
func New(cfg Config, o Options) (*Server, error) {
	sec, err := LoadSecrets(cfg, o.KeyOwnerOK)
	if err != nil {
		return nil, err
	}
	dir := o.StateDir
	if dir == "" {
		dir = DefaultStateDir
	}
	// Start-up order: config trust (done by the caller) -> registry (migrates a
	// missing one) -> used request IDs -> handlers. A registry or used-request
	// file that cannot be trusted does not stop the broker: everything that
	// depends on it answers registry_unavailable (fail closed, no fallback).
	sopts := RegistryOptions{OwnerOK: o.KeyOwnerOK}
	reg, err := OpenRegistry(dir, LegacySource{ContextRepos: cfg.ContextRepos, GitLabProjects: cfg.GitLab.Projects}, sopts)
	if err != nil && !errors.Is(err, ErrRegistryBroken) {
		return nil, err
	}
	used, err := OpenUsedRequests(dir, sopts)
	if err != nil && !errors.Is(err, ErrUsedRequestsBroken) {
		return nil, err
	}
	// The token store never stops the broker: a broken one answers
	// tokens_unavailable and the store's keys are treated as absent.
	toks, err := OpenTokenStore(dir, sopts)
	if err != nil && !errors.Is(err, ErrTokensBroken) {
		return nil, err
	}
	bud, err := OpenBudget(filepath.Join(dir, "budget.json"), Micros(cfg.Limits.WeeklyBudgetUSD), o.Clock)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Config:     cfg,
		Secrets:    sec,
		mrLock:     make(chan struct{}, 1),
		Tokens:     toks,
		keyOwnerOK: o.KeyOwnerOK,
		Registry:   reg,
		Used:       used,
		GitLab:     &GitLabClient{BaseURL: cfg.GitLab.BaseURL, Token: sec.GitLabToken()},
		Log:        o.Log,
		State: State{
			Refine:   NewHourlyLimiter(cfg.Limits.RefinePerHour, o.Clock),
			CreateMR: NewHourlyLimiter(cfg.Limits.CreateMRPerHour, o.Clock),
			Budget:   bud,

			CreateProject: NewHourlyLimiter(maxCreateProjectPerHour, o.Clock),
			CreateGroup:   NewHourlyLimiter(maxCreateGroupPerHour, o.Clock),
		},
	}
	s.OpenAI = &OpenAIClient{KeyFn: s.currentOpenAIKey, Model: cfg.OpenAI.Model}
	if toks.Err() != nil {
		s.logf("토큰 저장소를 신뢰할 수 없어 저장된 토큰을 쓰지 않습니다")
	}
	if reg.Err() != nil {
		s.logf("프로젝트 등록부를 신뢰할 수 없어 등록부가 필요한 작업을 거부합니다")
	}
	if used.broken {
		s.logf("사용된 요청 ID 목록을 신뢰할 수 없어 쓰기 작업을 거부합니다")
	}
	bud.OnPersistError = func() { s.logf("예산 상태 저장 실패: 요청을 거부합니다") }
	s.Handlers = map[string]HandlerFunc{
		"status":    s.statusHandler,
		"tokens":    s.tokensHandler,
		"refine":    s.refineHandler,
		"create_mr": s.createMRHandler,

		"projects":         s.projectsHandler,
		"register_project": s.registerProjectHandler,
		"set_flag":         s.setFlagHandler,

		"set_token":    s.setTokenHandler,
		"delete_token": s.deleteTokenHandler,

		"providers":       s.providersHandler,
		"list_groups":     s.listGroupsHandler,
		"set_provider":    s.setProviderHandler,
		"delete_provider": s.deleteProviderHandler,
		"create_group":    s.createGroupHandler,
		"create_project":  s.createProjectHandler,
	}
	return s, nil
}

// statusHandler is read-only and secret-free (decision D3).
func (s *Server) statusHandler(context.Context, json.RawMessage) (any, *OpError) {
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	repos := s.Registry.AIContextPaths()
	projects := s.Registry.GitLabPaths()
	remaining := s.State.Budget.RemainingMicros()
	res := map[string]any{
		"context_repos":               repos,
		"refine_remaining_hour":       s.State.Refine.Remaining(),
		"gitlab_configured":           s.Config.GitLab.Configured(),
		"mr_projects":                 projects,
		"weekly_budget_remaining_usd": float64(remaining) / 1e6,
	}
	// Expiry summary (decision D6): names and day counts only, no values.
	expiring := []map[string]any{}
	list, lerr := s.Tokens.List(s.now())
	for _, t := range list {
		if Expiring(t.DaysLeft) {
			expiring = append(expiring, map[string]any{"ref": t.Ref, "days_left": *t.DaysLeft})
		}
	}
	res["tokens_available"] = lerr == nil
	res["warn_days"] = WarnDays
	res["expiring"] = expiring
	return res, nil
}

func (s *Server) setup() {
	w := s.Log
	if w == nil {
		w = io.Discard
	}
	s.logger = log.New(&redactWriter{w: w, secrets: s.redactionValues}, "orch-broker: ", log.LstdFlags)
	s.sem = make(chan struct{}, MaxConcurrent)
}

// redactionValues lists every secret a log line must never show: the values
// loaded at start, the usable stored tokens and the CURRENT config OpenAI key
// (refine re-reads that file on every call, so it can differ from the start value).
func (s *Server) redactionValues() []string {
	v := append(s.Secrets.values(), s.Tokens.values()...)
	if k := s.cachedConfigKey(); k != "" {
		v = append(v, k)
	}
	return v
}

func (s *Server) logf(format string, a ...any) {
	s.init()
	s.logger.Printf(format, a...)
}

// init prepares the logger and the concurrency semaphore.
func (s *Server) init() { s.logOnce.Do(s.setup) }

// Listen creates the unix socket: stale socket removed, group set, mode 0660.
func (s *Server) Listen() (net.Listener, error) {
	path := s.Config.Socket
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("소켓 경로에 소켓이 아닌 파일이 있습니다")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("이전 소켓을 지울 수 없습니다")
		}
	}
	lookup := s.LookupGroup
	if lookup == nil {
		lookup = func(name string) (int, error) {
			g, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(g.Gid)
		}
	}
	gid, err := lookup(s.Config.SocketGroup)
	if err != nil {
		return nil, errors.New("socket_group을 확인할 수 없습니다")
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("소켓을 열 수 없습니다")
	}
	// Until the chmod below the socket has the umask-derived mode; with a
	// normal umask nobody but the owner may connect, and the uid check
	// applies in any case.
	if err := os.Chown(path, -1, gid); err != nil {
		ln.Close()
		return nil, errors.New("소켓 그룹을 설정할 수 없습니다")
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, errors.New("소켓 권한을 설정할 수 없습니다")
	}
	return ln, nil
}

// Serve accepts connections until ctx ends or ln fails, then waits for
// in-flight requests.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.init()
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-s.sem }()
				s.handle(ctx, c)
			}()
		default:
			s.reply(c, nil, "busy", "")
			c.Close()
		}
	}
}

func (s *Server) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return DefaultReadTime
}

func (s *Server) allowed(uid uint32) bool {
	if s.AllowUID != nil {
		return s.AllowUID(uid)
	}
	for _, u := range s.Config.uids {
		if u == uid {
			return true
		}
	}
	return false
}

type request struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	defer func() {
		if recover() != nil { // never log the value: it could hold request text
			s.logf("connection handler panic")
			s.reply(c, nil, "internal_error", "")
		}
	}()
	peer := s.PeerUID
	if peer == nil {
		peer = peerCredUID
	}
	uid, err := peer(c)
	if err != nil || !s.allowed(uid) {
		s.logf("refused: peer not allowed")
		s.reply(c, nil, "forbidden", "")
		return
	}
	line, code := readLine(c, s.readTimeout())
	if code != "" {
		s.logf("request failed: code=%s", code)
		s.reply(c, nil, code, "")
		drain(c)
		return
	}
	var req request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.V != 1 || req.Op == "" {
		s.logf("request failed: code=bad_request")
		s.reply(c, nil, "bad_request", "")
		drain(c)
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		s.logf("request failed: code=bad_request")
		s.reply(c, nil, "bad_request", "")
		drain(c)
		return
	}
	h, ok := s.Handlers[req.Op]
	if !ok {
		s.logf("request failed: code=unknown_op")
		s.reply(c, nil, "unknown_op", "")
		drain(c)
		return
	}
	start := time.Now()
	res, oerr := s.run(ctx, req.Op, h, req.Args)
	if oerr != nil {
		s.logf("op=%s failed: code=%s dur=%s", req.Op, safeCode(oerr.Code), time.Since(start).Round(time.Millisecond))
		s.replyDetail(c, nil, oerr.Code, oerr.Message, oerr.Data)
	} else {
		s.logf("op=%s ok dur=%s", req.Op, time.Since(start).Round(time.Millisecond))
		s.reply(c, res, "", "")
	}
	drain(c)
}

func safeCode(code string) string {
	if _, ok := fixedMessages[code]; ok {
		return code
	}
	return "internal_error"
}

// run calls the handler with the op's time limit; a panic or a timeout
// becomes a fixed error and the server keeps running.
func (s *Server) run(ctx context.Context, op string, h HandlerFunc, args json.RawMessage) (any, *OpError) {
	d, ok := s.OpTimeouts[op]
	if !ok {
		d = defaultOpTimeouts[op]
	}
	if d <= 0 {
		d = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type out struct {
		res any
		err *OpError
	}
	ch := make(chan out, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// never the payload: it may carry a client-supplied secret
				s.logf("handler panic: op=%s type=%T", op, r)
				ch <- out{nil, &OpError{Code: "internal_error"}}
			}
		}()
		res, err := h(ctx, args)
		ch <- out{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-ctx.Done():
		return nil, &OpError{Code: "timeout"}
	}
}

// readLine reads one newline-terminated request, at most MaxRequestBytes
// bytes (newline excluded), within timeout overall. A non-empty code is a
// fixed failure code.
func readLine(c net.Conn, timeout time.Duration) ([]byte, string) {
	c.SetReadDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(c, 32*1024)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxRequestBytes+1 || (len(buf) == MaxRequestBytes+1 && buf[len(buf)-1] != '\n') {
			return nil, "too_large"
		}
		switch {
		case err == nil:
			line := bytes.TrimSuffix(buf, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			return line, ""
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case isTimeout(err):
			return nil, "timeout"
		default: // EOF or reset before a newline
			return nil, "bad_request"
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// drain lets the peer finish writing (briefly, bounded) so closing the
// socket does not reset a reply the peer has not read yet.
func drain(c net.Conn) {
	if uc, ok := c.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	io.Copy(io.Discard, io.LimitReader(c, 2*MaxRequestBytes))
}

// reply writes one JSON response; failures carry the registered fixed
// sentence of their code (unregistered codes become internal_error).
func (s *Server) reply(c net.Conn, result any, code, msg string) {
	s.replyDetail(c, result, code, msg, nil)
}

// replyDetail is reply plus the optional "detail" of a create_partial failure.
func (s *Server) replyDetail(c net.Conn, result any, code, msg string, detail any) {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	var body []byte
	if code == "" {
		b, err := json.Marshal(map[string]any{"ok": true, "result": result})
		if err != nil {
			code = "internal_error"
		} else {
			body = b
		}
	}
	if code != "" {
		fixed, ok := fixedMessages[code]
		if !ok {
			code, fixed = "internal_error", fixedMessages["internal_error"]
		}
		msg = fixed
		out := map[string]any{"ok": false, "code": code, "message": msg}
		if detail != nil && code == "create_partial" {
			out["detail"] = detail
		}
		body, _ = json.Marshal(out)
	}
	c.Write(append(body, '\n'))
}

var (
	keyShapeRE = regexp.MustCompile(`(?:sk|glpat|ghp|gho)-[A-Za-z0-9_\-]{6,}`)
	bearerRE   = regexp.MustCompile(`(?i)bearer\s+\S+`)
)

// Redact removes the known secrets and key-shaped strings from s.
func Redact(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = keyShapeRE.ReplaceAllString(s, "[redacted]")
	return bearerRE.ReplaceAllString(s, "Bearer [redacted]")
}

// redactWriter scrubs every log line before it reaches the sink.
type redactWriter struct {
	mu      sync.Mutex
	w       io.Writer
	secrets func() []string
}

func (r *redactWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := io.WriteString(r.w, Redact(string(p), r.secrets()))
	return len(p), err
}

type logKeyCache struct {
	valid bool
	mtime time.Time
	size  int64
	key   string
}

// cachedConfigKey returns the config OpenAI key for log redaction. The file is
// re-read only when its (mtime, size) changed, so a log line normally costs one
// stat; a rewritten key file is picked up on the next line. "" when unreadable.
func (s *Server) cachedConfigKey() string {
	fi, err := os.Stat(s.Config.OpenAI.KeyFile)
	s.logKeyMu.Lock()
	defer s.logKeyMu.Unlock()
	if err != nil {
		s.logKey = logKeyCache{}
		return ""
	}
	if s.logKey.valid && s.logKey.size == fi.Size() && s.logKey.mtime.Equal(fi.ModTime()) {
		return s.logKey.key
	}
	s.logKeyReads.Add(1)
	k, _ := s.configOpenAIKey()
	s.logKey = logKeyCache{valid: true, mtime: fi.ModTime(), size: fi.Size(), key: k}
	return k
}
