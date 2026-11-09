package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
)

// Merge request step: once a task is done (its branch is published), orchd
// asks the broker to open a Draft merge request for it. The task's outcome is
// already final and never depends on this: every failure only becomes
// mr_state=failed plus a fixed mr_code. orchd never merges; the broker is the
// only party that talks to GitLab.

// MRResult is what the broker answered for a created (or already open) MR.
type MRResult struct {
	IID     int64
	URL     string
	Created bool // false when the broker found an open MR for the branch
}

// MRCreator opens the merge request of a done task. A failure is an *MRError
// (fixed code); any other error is stored as upstream_error. An error with
// the code repo_not_mapped means "this repository has no GitLab project":
// the task's MR is then skipped, not failed.
type MRCreator interface {
	CreateMR(ctx context.Context, t store.Task) (MRResult, error)
}

// MRError is a failed create_mr: a fixed lowercase code and nothing else (the
// broker's own text is never read, see broker.Client).
type MRError struct{ Code string }

func (e *MRError) Error() string { return "merge request: " + e.Code }

var (
	// ErrMRNotApplicable: the task is not a done task whose MR failed or was
	// skipped (or no broker is configured / the repository is not mapped).
	ErrMRNotApplicable = errors.New("steps: no merge request can be created for this task")
	// ErrMRAlreadyCreated: the task's merge request exists.
	ErrMRAlreadyCreated = errors.New("steps: the merge request already exists")
)

// Fixed codes of this layer (the broker's own are passed through).
const (
	mrCodeUpstream   = "upstream_error"
	mrCodeTimeout    = "timeout"
	mrCodeBadURL     = "bad_url"
	mrCodeBadRequest = "bad_request"
	mrCodeInternal   = "internal_error"
	mrCodeNotMapped  = "repo_not_mapped"
)

const (
	// mrHookTimeout bounds the hook that runs after the done transition.
	mrHookTimeout = 15 * time.Second
	// mrRetryTimeout bounds one retry request (the broker's own limit is 20 s).
	mrRetryTimeout = 25 * time.Second
	maxMRURL       = 2048
	maxTitleHint   = 120
	maxSummary     = 400
	maxVerdict     = 60
)

var (
	mrCodeRe = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
	headRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// mrBranch is the branch the publisher pushed the task to ("orch/<id>"); the
// same value publish.RefFor uses, without refs/heads/.
func mrBranch(id int64) string { return strings.TrimPrefix(publish.RefFor(id), "refs/heads/") }

// afterTransition runs after a transition committed and its ledger event was
// written. The merge request hook runs on publishing -> done only: that
// transition happens in exactly one place (the worker that ran the publishing
// step, machine.advance), after the compare-and-set succeeded, so a task that
// failed, was cancelled or lost a race never gets here, and the hook runs in
// the same worker (no extra goroutine) right after the task became done.
func (p *Provider) afterTransition(ctx context.Context, t store.Task, from, to machine.State, note string) {
	p.recordTransition(ctx, t, from, to, note)
	if from == machine.StatePublishing && to == machine.StateDone {
		p.mrHook(ctx, t)
	}
}

// mrHook creates the merge request of a task that just became done. It never
// returns an error and never touches the task's state: a failure is stored
// as mr_state=failed. A task whose mr_state is already set (a republish of a
// task that was done before, or a retry endpoint that got there first) is
// left alone: the claim is the store's compare-and-set from "not attempted".
func (p *Provider) mrHook(ctx context.Context, t store.Task) {
	defer func() {
		if recover() != nil {
			log.Printf("orchd: merge request hook for task %d panicked", t.ID)
		}
	}()
	wctx := context.WithoutCancel(ctx)
	cur, err := p.st.GetTask(wctx, t.ID)
	if err != nil || cur.State != machine.StateDone || cur.MRState != "" {
		return
	}
	if p.cfg.MR == nil {
		if err := p.st.SetMRFrom(wctx, cur.ID, []string{""}, "", 0, "", store.MRSkipped, ""); err != nil && !errors.Is(err, store.ErrStateMismatch) {
			log.Printf("orchd: task %d merge request state not recorded", cur.ID)
		}
		return
	}
	_, _, serr := p.attemptMR(wctx, cur, []string{""}, p.mrTimeout)
	if serr != nil && !errors.Is(serr, store.ErrStateMismatch) {
		log.Printf("orchd: task %d merge request outcome not recorded", cur.ID)
	}
}

// MRConfigured reports whether a merge request creator is wired in.
func (p *Provider) MRConfigured() bool { return p.cfg.MR != nil }

// RetryMR is the retry endpoint's work: it creates the merge request of a
// done task whose MR failed or was skipped, as the actor of ctx. It returns
// the updated task. Errors: store.ErrNotFound; ErrMRNotApplicable (not done,
// never attempted, no broker, repository not mapped); ErrMRAlreadyCreated; or
// an *MRError (the broker failed; the failure is recorded as mr_state=failed
// with its fixed code, together with the task so the caller can show it).
func (p *Provider) RetryMR(ctx context.Context, id int64) (store.Task, error) {
	t, err := p.st.GetTask(ctx, id)
	if err != nil {
		return store.Task{}, err
	}
	if t.MRState == store.MRCreated {
		return t, ErrMRAlreadyCreated
	}
	if p.cfg.MR == nil || t.State != machine.StateDone || (t.MRState != store.MRFailed && t.MRState != store.MRSkipped) {
		return t, ErrMRNotApplicable
	}
	state, code, serr := p.attemptMR(ctx, t, []string{store.MRFailed, store.MRSkipped}, mrRetryTimeout)
	if serr != nil {
		// Lost a race (another retry created it, or the task changed).
		if errors.Is(serr, store.ErrStateMismatch) {
			if cur, gerr := p.st.GetTask(ctx, id); gerr == nil && cur.MRState == store.MRCreated {
				return cur, ErrMRAlreadyCreated
			}
			return t, ErrMRNotApplicable
		}
		return t, serr
	}
	cur, gerr := p.st.GetTask(ctx, id)
	if gerr != nil {
		return t, gerr
	}
	switch state {
	case store.MRFailed:
		return cur, &MRError{Code: code}
	case store.MRSkipped:
		return cur, ErrMRNotApplicable
	}
	return cur, nil
}

// attemptMR calls the creator (bounded by limit, immune to the caller's own
// cancellation so the outcome is always recorded), classifies the result,
// stores it with SetMRFrom(prev) and appends the ledger record. It returns the
// recorded state and code; a store error means nothing was recorded.
func (p *Provider) attemptMR(ctx context.Context, t store.Task, prev []string, limit time.Duration) (state, code string, err error) {
	wctx := context.WithoutCancel(ctx)
	cctx, cancel := context.WithTimeout(wctx, limit)
	res, cerr := p.callMR(cctx, t)
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	cancel()
	branch, iid, murl := mrBranch(t.ID), 0, ""
	switch {
	case cerr == nil && res.IID > 0 && res.IID <= 1<<31-1 && validMRURL(res.URL):
		state, iid, murl = store.MRCreated, int(res.IID), res.URL
	case cerr == nil && res.IID > 0 && res.IID <= 1<<31-1:
		state, code = store.MRFailed, mrCodeBadURL
	case cerr == nil:
		state, code = store.MRFailed, mrCodeUpstream
	default:
		c := codeOf(cerr, timedOut)
		if c == mrCodeNotMapped {
			state, branch = store.MRSkipped, ""
		} else {
			state, code = store.MRFailed, c
		}
	}
	if state == store.MRFailed && code == mrCodeBadURL {
		iid, murl = 0, "" // never keep what failed validation
	}
	if state == store.MRSkipped && t.MRState == store.MRSkipped {
		// Skipped again: nothing changed, so nothing is rewritten or recorded.
		return state, code, nil
	}
	if err := p.st.SetMRFrom(wctx, t.ID, prev, branch, iid, murl, state, code); err != nil {
		return "", "", err
	}
	p.recordMR(wctx, t.ID, branch, iid, state, code)
	return state, code, nil
}

// callMR runs the creator; a panic inside it is a fixed internal_error, its
// text (which could hold anything) is dropped.
func (p *Provider) callMR(ctx context.Context, t store.Task) (res MRResult, err error) {
	defer func() {
		if recover() != nil {
			res, err = MRResult{}, &MRError{Code: mrCodeInternal}
		}
	}()
	return p.cfg.MR.CreateMR(ctx, t)
}

// codeOf is the fixed code stored for a failed call: the creator's own code
// when it is well formed, timeout when the call outlived its limit, else
// upstream_error. No error text is ever kept.
func codeOf(err error, timedOut bool) string {
	var me *MRError
	if errors.As(err, &me) && mrCodeRe.MatchString(me.Code) {
		if timedOut && me.Code != mrCodeNotMapped {
			return mrCodeTimeout
		}
		return me.Code
	}
	if timedOut || errors.Is(err, context.DeadlineExceeded) {
		return mrCodeTimeout
	}
	return mrCodeUpstream
}

// validMRURL accepts only an absolute http:// or https:// address with a
// host, no credentials, no whitespace or control characters, at most 2048
// bytes. The broker already pinned it to the GitLab base; this keeps the
// stored (and later rendered as a link) value harmless whatever it says.
func validMRURL(s string) bool {
	if len(s) == 0 || len(s) > maxMRURL || !(strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f || c == '\\' {
			return false
		}
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && u.Hostname() != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// recordMR appends the merge_request ledger event (best effort, like every
// orchd ledger record): who, the task, branch, iid, state and fixed code.
func (p *Provider) recordMR(ctx context.Context, id int64, branch string, iid int, state, code string) {
	home, err := gate.Home(p.cfg.GateHome)
	if err != nil {
		log.Printf("orchd: ledger: merge request of task %d not recorded", id)
		return
	}
	actor := store.ActorFrom(ctx)
	err = ledger.Append(policy.OrchLedgerPath(home), "merge_request", policy.OrchRepo, map[string]any{
		"task": int(id), "branch": branch, "iid": iid, "state": state, "code": code, "actor": actor,
	}, time.Now().UTC())
	if err != nil {
		log.Printf("orchd: ledger: merge request of task %d not recorded", id)
	}
}

// BrokerMR creates merge requests through the broker's create_mr operation.
type BrokerMR struct {
	Client *broker.Client
}

// CreateMR sends exactly the D9 arguments: repo, branch and task from the
// task (the task number is the number in the branch), a title hint (the
// prompt's first line) and a summary (the prompt, one line), both
// CRLF-normalised and length-capped, the actor as kind:id, the verdict and
// the head commit. No file content is ever sent. The broker builds the Draft
// title and description itself.
func (b *BrokerMR) CreateMR(ctx context.Context, t store.Task) (MRResult, error) {
	if !headRe.MatchString(t.HeadSHA) || t.ID < 1 || t.ID > 999999999 {
		return MRResult{}, &MRError{Code: mrCodeBadRequest}
	}
	actor := store.ActorFrom(ctx)
	if actor == store.ActorSystem || actor == store.ActorInvalid {
		actor = actor + ":orchd"
	}
	args := map[string]any{
		"repo":       t.Repo,
		"branch":     mrBranch(t.ID),
		"task":       t.ID,
		"title_hint": titleHint(t.Prompt),
		"summary":    summaryOf(t.Prompt),
		"actor":      actor,
		"verdict":    capRunes(oneLine(t.Verdict), maxVerdict),
		"head":       t.HeadSHA,
	}
	raw, oerr := b.Client.Do(ctx, "create_mr", args)
	if oerr != nil {
		return MRResult{}, &MRError{Code: oerr.Code}
	}
	var r struct {
		IID     *int64  `json:"iid"`
		URL     *string `json:"url"`
		Created *bool   `json:"created"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.IID == nil || r.URL == nil || r.Created == nil {
		return MRResult{}, &MRError{Code: mrCodeUpstream}
	}
	return MRResult{IID: *r.IID, URL: *r.URL, Created: *r.Created}, nil
}

// normalizeText turns CRLF and CR into LF and drops control and format
// characters other than the line feed.
func normalizeText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
}

// oneLine collapses all whitespace (including line breaks) to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(normalizeText(s)), " ") }

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// titleHint is the first non-empty line of the prompt, capped.
func titleHint(prompt string) string {
	for _, line := range strings.Split(normalizeText(prompt), "\n") {
		if l := oneLine(line); l != "" {
			return capRunes(l, maxTitleHint)
		}
	}
	return ""
}

// summaryOf is the whole prompt as one capped line.
func summaryOf(prompt string) string { return capRunes(oneLine(prompt), maxSummary) }
