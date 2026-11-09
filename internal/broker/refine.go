package broker

import (
	"aigate/internal/refinesize"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of one refine request (decision D7 and the task brief).
const (
	maxRequirementRunes = 2000
	maxRefineFiles      = 40
	maxRefineFileBytes  = 32 * 1024
)

type refineFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type refineArgs struct {
	Repo        string       `json:"repo"`
	Requirement string       `json:"requirement"`
	Files       []refineFile `json:"files"`
	Selection   string       `json:"selection"`
}

var selectionRE = regexp.MustCompile(`^[A-Za-z0-9:_-]{0,128}$`)

func fail(code string) (any, *OpError) { return nil, &OpError{Code: code} }

// bad reports control characters (except newline/tab when allowNL), bidi
// controls, line/paragraph separators and invalid UTF-8.
func badText(s string, allowNL bool) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			if !allowNL {
				return true
			}
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r): // Cf: zero-width, bidi, soft hyphen, BOM, ...
			return true
		case r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069),
			r == 0x2028 || r == 0x2029, r == 0xFEFF:
			return true
		}
	}
	return false
}

// safeRelPath reports whether p is a clean, relative, forward-slash path
// that stays inside the repository.
func safeRelPath(p string) bool {
	if p == "" || len(p) > 200 || badText(p, false) || strings.Contains(p, `\`) {
		return false
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

func (s *Server) refineHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	var a refineArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return fail("bad_request")
	}
	if _, err := dec.Token(); err != io.EOF {
		return fail("bad_request")
	}
	// 1. allow list: first, before anything is read or sent.
	if !s.repoAllowed(a.Repo) {
		return fail("repo_not_allowed")
	}
	// 2. requirement.
	if a.Requirement == "" || badText(a.Requirement, true) || !selectionRE.MatchString(a.Selection) {
		return fail("bad_request")
	}
	if utf8.RuneCountInString(a.Requirement) > maxRequirementRunes {
		return fail("too_large")
	}
	// 3. files.
	if len(a.Files) > maxRefineFiles {
		return fail("too_large")
	}
	seen := map[string]bool{}
	for _, f := range a.Files {
		if !safeRelPath(f.Path) || seen[f.Path] {
			return fail("bad_file")
		}
		seen[f.Path] = true
		if len(f.Content) > maxRefineFileBytes {
			return fail("too_large")
		}
		if !utf8.ValidString(f.Content) || strings.IndexByte(f.Content, 0) >= 0 {
			return fail("bad_file")
		}
	}
	// 4. secret scan (second line after orchd's own).
	if LooksLikeSecret(a.Requirement) {
		return fail("looks_like_secret")
	}
	for _, f := range a.Files {
		if LooksLikeSecret(f.Content) || LooksLikeSecret(f.Path) {
			return fail("looks_like_secret")
		}
	}
	nonce, err := newNonce()
	if err != nil {
		return fail("internal_error")
	}
	system := refinesize.System(nonce)
	user := buildUserMessage(nonce, a)
	// max_input_bytes counts everything sent as input: instructions, user
	// message (with markers) and the schema.
	if int64(len(system)+len(user)+len(refinesize.Schema)) > s.Config.OpenAI.MaxInputBytes {
		return fail("too_large")
	}
	// 5. hourly limit, then the reservation (worst case, before the call).
	if !s.State.Refine.Allow() {
		return fail("rate_limited")
	}
	cfg := s.Config.OpenAI
	worst, err := WorstCaseMicros(cfg.Pricing, int64(len(system)+len(user)+len(refinesize.Schema)), cfg.MaxOutputTokens)
	if err != nil {
		return fail("budget_exceeded")
	}
	id, err := s.State.Budget.Reserve(worst)
	if err != nil {
		return fail("budget_exceeded")
	}
	// 6. the call. Whatever happens, settle by reported usage or keep the
	// reservation.
	text, usage, cerr := s.OpenAI.Complete(ctx, system, user, cfg.MaxOutputTokens, refinesize.Schema)
	if usage.InputTokens != nil && *usage.InputTokens == 0 {
		usage = Usage{} // a non-trivial request cannot have used 0 input tokens: treat as missing
	}
	if err := s.State.Budget.SettleUsage(id, cfg.Pricing, usage); err != nil {
		s.logf("예산 정산 실패: 예약값이 남습니다")
	}
	if cerr != nil {
		return fail("upstream_error")
	}
	plan, ok := parsePlan(text, seen)
	if !ok {
		return fail("bad_plan")
	}
	res := map[string]any{
		"summary":      plan.Summary,
		"items":        plan.Items,
		"questions":    plan.Questions,
		"out_of_scope": plan.OutOfScope,
		"usage":        map[string]int64{"input_tokens": tokenCount(usage.InputTokens), "output_tokens": tokenCount(usage.OutputTokens)},
	}
	if a.Selection != "" {
		s.logf("refine selection=%s", a.Selection)
	}
	return res, nil
}

func tokenCount(p *int64) int64 {
	if p == nil || *p < 0 {
		return 0
	}
	return *p
}

func newNonce() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func buildUserMessage(nonce string, a refineArgs) string {
	files := make([]refinesize.File, len(a.Files))
	for i, f := range a.Files {
		files[i] = refinesize.File{Path: f.Path, Content: f.Content}
	}
	return refinesize.User(nonce, a.Requirement, files)
}

func (s *Server) repoAllowed(repo string) bool {
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return false
	}
	for _, r := range s.Config.ContextRepos {
		if r == repo {
			return true
		}
	}
	return false
}

// Plan is the validated refine result (decision D4); ids are assigned here.
type Plan struct {
	Summary    string         `json:"summary"`
	Items      []PlanItem     `json:"items"`
	Questions  []PlanQuestion `json:"questions"`
	OutOfScope []string       `json:"out_of_scope"`
}

type PlanItem struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

type PlanQuestion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type rawPlan struct {
	Summary *string `json:"summary"`
	Items   []struct {
		ID          json.RawMessage `json:"id"` // accepted and ignored
		Title       *string         `json:"title"`
		Description string          `json:"description"`
		Files       []string        `json:"files"`
	} `json:"items"`
	Questions []struct {
		ID   json.RawMessage `json:"id"` // accepted and ignored
		Text *string         `json:"text"`
	} `json:"questions"`
	OutOfScope []string `json:"out_of_scope"`
}

// exactKeys checks the field names case-sensitively (encoding/json matches
// them case-insensitively).
func exactKeys(text string) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &top) != nil {
		return false
	}
	okKeys := func(m map[string]json.RawMessage, allowed ...string) bool {
		for k := range m {
			found := false
			for _, a := range allowed {
				if k == a {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	if !okKeys(top, "summary", "items", "questions", "out_of_scope") {
		return false
	}
	var items, qs []map[string]json.RawMessage
	if raw, ok := top["items"]; ok && json.Unmarshal(raw, &items) != nil {
		return false
	}
	if raw, ok := top["questions"]; ok && json.Unmarshal(raw, &qs) != nil {
		return false
	}
	for _, m := range items {
		if !okKeys(m, "id", "title", "description", "files") {
			return false
		}
	}
	for _, m := range qs {
		if !okKeys(m, "id", "text") {
			return false
		}
	}
	return true
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// parsePlan strictly validates the model's text. It returns no detail on
// failure: the model's text must never reach an error.
func parsePlan(text string, sent map[string]bool) (Plan, bool) {
	var rp rawPlan
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rp); err != nil {
		return Plan{}, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return Plan{}, false
	}
	if !exactKeys(text) {
		return Plan{}, false
	}
	if rp.Summary == nil || *rp.Summary == "" || runes(*rp.Summary) > 600 || badText(*rp.Summary, true) {
		return Plan{}, false
	}
	if len(rp.Items) < 1 || len(rp.Items) > 30 || len(rp.Questions) > 10 || len(rp.OutOfScope) > 15 {
		return Plan{}, false
	}
	p := Plan{Summary: *rp.Summary, Items: []PlanItem{}, Questions: []PlanQuestion{}, OutOfScope: []string{}}
	for i, it := range rp.Items {
		if it.Title == nil || *it.Title == "" || runes(*it.Title) > 120 || badText(*it.Title, false) ||
			runes(it.Description) > 800 || badText(it.Description, true) || len(it.Files) > 10 {
			return Plan{}, false
		}
		files := []string{}
		for _, f := range it.Files {
			if !safeRelPath(f) {
				return Plan{}, false
			}
			if sent[f] { // paths that were not sent are dropped
				files = append(files, f)
			}
		}
		p.Items = append(p.Items, PlanItem{ID: "r" + itoa(i+1), Title: *it.Title, Description: it.Description, Files: files})
	}
	for i, q := range rp.Questions {
		if q.Text == nil || *q.Text == "" || runes(*q.Text) > 300 || badText(*q.Text, false) {
			return Plan{}, false
		}
		p.Questions = append(p.Questions, PlanQuestion{ID: "q" + itoa(i+1), Text: *q.Text})
	}
	for _, o := range rp.OutOfScope {
		if o == "" || runes(o) > 200 || badText(o, false) {
			return Plan{}, false
		}
		p.OutOfScope = append(p.OutOfScope, o)
	}
	return p, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
