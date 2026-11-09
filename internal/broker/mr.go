package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// create_mr (decision D9). The project path and target branch come only from
// the broker config; title and description are built here from a fixed
// template; the merge request is a draft and is never merged.

const (
	maxTitleHintRunes = 120
	maxMRSummaryRunes = 400
	maxVerdictRunes   = 60
	maxActorBytes     = 80
	maxTaskNumber     = 999999999
)

var (
	mrBranchRE = regexp.MustCompile(`^orch/[0-9]{1,9}$`)
	mrHeadRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	wwwRE      = regexp.MustCompile(`(?i)www\.`)
)

type mrArgs struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Task      int64  `json:"task"`
	TitleHint string `json:"title_hint"`
	Summary   string `json:"summary"`
	Actor     string `json:"actor"`
	Verdict   string `json:"verdict"`
	Head      string `json:"head"`
}

var mrArgKeys = []string{"repo", "branch", "task", "title_hint", "summary", "actor", "verdict", "head"}

// decodeMRArgs is strict: one JSON object, only known keys with exactly
// their spelling (encoding/json matches names case-insensitively), no
// trailing data.
func decodeMRArgs(raw json.RawMessage) (mrArgs, bool) {
	var a mrArgs
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return a, false
	}
	for k := range keys {
		known := false
		for _, w := range mrArgKeys {
			if k == w {
				known = true
			}
		}
		if !known {
			return a, false
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&a) != nil {
		return a, false
	}
	_, err := dec.Token()
	return a, err == io.EOF
}

// cleanLine turns untrusted text into one plain line: control, format and
// separator characters are dropped, whitespace runs become one space, and
// everything GitLab markdown could turn into a link, mention, reference,
// image, heading or HTML is removed. The result is capped at max runes.
func cleanLine(s string, max int) string {
	s = strings.ReplaceAll(s, "://", " ")
	s = wwwRE.ReplaceAllString(s, "www ")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r) && !unicode.IsSpace(r), unicode.Is(unicode.Cf, r),
			r == 0x2028 || r == 0x2029:
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		case strings.ContainsRune("[]()<>`*_~|\\!@#%&$^{}", r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > max {
		out = strings.TrimSpace(string([]rune(out)[:max]))
	}
	return out
}

func buildMRTitle(task int64, hint string) string {
	t := "Draft: orch task " + strconv.FormatInt(task, 10)
	if hint != "" {
		t += ": " + hint
	}
	return t
}

func buildMRDescription(a mrArgs, summary, verdict, actor string) string {
	var b strings.Builder
	b.WriteString("Draft merge request created by orch for task " + strconv.FormatInt(a.Task, 10) + ".\n\n")
	b.WriteString("- Branch: `" + a.Branch + "`\n")
	b.WriteString("- Head: `" + a.Head + "`\n")
	b.WriteString("- Requested by: " + actor + "\n")
	b.WriteString("- Verdict: " + verdict + "\n\n")
	b.WriteString("Summary: " + summary + "\n\n")
	b.WriteString("Review it yourself. orch never merges this merge request.\n")
	return b.String()
}

func (s *Server) createMRHandler(ctx context.Context, raw json.RawMessage) (any, *OpError) {
	a, ok := decodeMRArgs(raw)
	if !ok {
		return fail("bad_request")
	}
	// The project is looked up in the registry; the request names only a repo.
	if s.Registry.Err() != nil {
		return fail("registry_unavailable")
	}
	project, mapped := s.Registry.GitLabProject(a.Repo)
	if !mapped || !s.Config.GitLab.Configured() {
		return fail("repo_not_mapped")
	}
	// The branch must be exactly orch/<task> (this also rejects orch/012).
	if !mrBranchRE.MatchString(a.Branch) || (a.Task >= 1 && a.Branch != "orch/"+strconv.FormatInt(a.Task, 10)) {
		return fail("bad_branch")
	}
	if a.Task < 1 || a.Task > maxTaskNumber || !mrHeadRE.MatchString(a.Head) ||
		a.Actor == "" || len(a.Actor) > maxActorBytes || badText(a.Actor, false) {
		return fail("bad_request")
	}
	if !utf8.ValidString(a.TitleHint) || !utf8.ValidString(a.Summary) || !utf8.ValidString(a.Verdict) {
		return fail("bad_request")
	}
	// The summary is the task's request text, which a person wrote: the same
	// best-effort secret scan as for files applies before it is sent to GitLab.
	if LooksLikeSecret(a.TitleHint) || LooksLikeSecret(a.Summary) {
		return fail("looks_like_secret")
	}
	hint := cleanLine(a.TitleHint, maxTitleHintRunes)
	summary := cleanLine(a.Summary, maxMRSummaryRunes)
	verdict := cleanLine(a.Verdict, maxVerdictRunes)
	actor := cleanLine(a.Actor, maxActorBytes)
	if summary == "" {
		summary = "-"
	}
	if verdict == "" {
		verdict = "-"
	}
	if actor == "" {
		actor = "-"
	}
	if LooksLikeSecret(hint) || LooksLikeSecret(summary) {
		return fail("looks_like_secret")
	}
	if !s.State.CreateMR.Allow() {
		return fail("rate_limited")
	}
	// One find+create at a time: two requests for the same branch must not
	// both see "no MR" and create twice.
	select {
	case s.mrLock <- struct{}{}:
		defer func() { <-s.mrLock }()
	case <-ctx.Done():
		return fail("upstream_error")
	}
	gl := s.GitLab
	mr, found, err := gl.FindOpenMR(ctx, project, a.Branch, s.Config.GitLab.TargetBranch)
	if err != nil {
		return mrFail(err)
	}
	if found {
		return mrResult(mr, false), nil
	}
	mr, err = gl.CreateMR(ctx, project, MRCreate{
		SourceBranch: a.Branch,
		TargetBranch: s.Config.GitLab.TargetBranch,
		Title:        buildMRTitle(a.Task, hint),
		Description:  buildMRDescription(a, summary, verdict, actor),
		Draft:        true,
	})
	if errors.Is(err, errConflict) {
		// Someone created it in between: return that one.
		mr, found, err = gl.FindOpenMR(ctx, project, a.Branch, s.Config.GitLab.TargetBranch)
		if err == nil && !found {
			err = errUpstream
		}
		if err != nil {
			return mrFail(err)
		}
		return mrResult(mr, false), nil
	}
	if err != nil {
		return mrFail(err)
	}
	return mrResult(mr, true), nil
}

func mrFail(err error) (any, *OpError) {
	if errors.Is(err, errBadURL) {
		return fail("bad_url")
	}
	return fail("upstream_error")
}

func mrResult(mr MR, created bool) map[string]any {
	return map[string]any{"iid": mr.IID, "url": mr.WebURL, "created": created}
}
