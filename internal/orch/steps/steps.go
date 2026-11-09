// Package steps wires the VM drivers and the gate into the task state machine.
//
// A step S runs while a task is in state S. A step that returns an error
// reports an INFRASTRUCTURE failure (ssh, restore, gate error): the machine
// retries it and, past the attempt limit, fails the task. It is never a
// verdict and never leads to passed. Verdict-like results (agent timeout, no
// commit, limits exceeded, gate verdict) are returned as an Outcome.
//
// Nothing here pushes anything itself. Publishing goes through Config.Publisher,
// which defaults to LocalOnlyPublisher; publish.Publisher is the real,
// allowlisted one. Approvals are queued through the approvals package.
package steps

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/policy/autonomy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
	"aigate/internal/report"
	"aigate/internal/rules"
)

// DefaultAgentTimeout bounds one agent run when Config.AgentTimeout is zero.
const DefaultAgentTimeout = 30 * time.Minute

// AgentDriver is what the steps need from the agent VM (*vm.AgentVM).
type AgentDriver interface {
	Prepare(ctx context.Context, task, repo, baseSHA string) error
	Run(ctx context.Context, task, agent, prompt string, limit time.Duration) (vm.RunResult, error)
	Collect(ctx context.Context, task, localRepo, baseSHA string) (headSHA string, commits int, err error)
}

// restoreReporter is optionally implemented by a TestDriver (*vm.TestVM does)
// to expose the phase timings of its last restore.
type restoreReporter interface {
	LastRestore() (vm.RestoreRecord, bool)
}

// TestDriver is what the steps need from the test VM (*vm.TestVM).
type TestDriver interface {
	Restore(ctx context.Context) error
	Executor() (gate.Executor, error)
}

var (
	_ AgentDriver = (*vm.AgentVM)(nil)
	_ TestDriver  = (*vm.TestVM)(nil)
)

// Publisher publishes a passed task. publish.Publisher is the real one: it may
// return a *publish.NeedsApprovalError (the task parks in needs_approval) or an
// error wrapping policy.ErrFloor (the task fails and a destructive_blocked
// record is kept).
type Publisher interface {
	Publish(ctx context.Context, task store.Task) error
}

// Planner is what a Publisher must offer when a policy is in force: the
// action kinds it will run (so each can be decided before any runs) and
// whether a human granted exactly this task's head for a kind.
// publish.Publisher and LocalOnlyPublisher implement it.
type Planner interface {
	Plan() []string
	Granted(t store.Task, kind string) bool
	// Permit reports whether kind passes the floor and the allowlist the
	// publisher itself enforces (nil = it would run without a human).
	Permit(kind string) error
}

// PolicyChangeKind is the pseudo action a human approves when the policy
// changed after the task started (see decidePublish). Planner.Granted is
// asked about it like about a real action kind.
func PolicyChangeKind(hash string) string { return "policy_changed:" + hash }

// Plan returns no actions.
func (LocalOnlyPublisher) Plan() []string { return nil }

// Granted is always false.
func (LocalOnlyPublisher) Granted(store.Task, string) bool { return false }

// Permit is always nil: there is nothing to run.
func (LocalOnlyPublisher) Permit(string) error { return nil }

// LocalOnlyPublisher publishes nothing; it only lets the task finish locally.
type LocalOnlyPublisher struct{}

// Publish does nothing.
func (LocalOnlyPublisher) Publish(context.Context, store.Task) error { return nil }

// PublishNote is the transition note recorded for the publishing step.
func (LocalOnlyPublisher) PublishNote() string { return "local-only: nothing pushed" }

// GateFunc runs the gate (gate.Run in production; tests replace it).
type GateFunc func(ctx context.Context, o gate.Options) (report.Report, error)

// Config is everything the steps need.
type Config struct {
	// BaseCache, when set, lets a task reuse clean base-commit check output
	// from an earlier task on the same base (see BaseCache).
	BaseCache *BaseCache
	Agent     AgentDriver
	Test      TestDriver
	// AgentTimeout bounds one agent run (default DefaultAgentTimeout).
	AgentTimeout time.Duration
	// RestoreEachJob restores the test VM before every verification. The
	// zero value is false, so callers must set it (DefaultConfig sets true).
	RestoreEachJob bool
	// Publisher defaults to LocalOnlyPublisher.
	Publisher Publisher
	// ReportDir holds <task>.json copies of the verification reports (required).
	ReportDir string
	// GateHome is the gate's home (approvals, ledger); empty uses the gate default.
	GateHome string
	// Gate defaults to gate.Run.
	Gate GateFunc
	// Policy, when set, is re-read at the start of every task (its hash is
	// recorded on the task), and publishing then goes through
	// autonomy.Decide at the class's effective level. Publisher must be a
	// Planner. Nil keeps the behaviour without a policy.
	Policy *policy.Live
	// Notifier receives the L2/L3 notifications (best effort; may be nil).
	Notifier publish.Notifier
}

// DefaultConfig returns a Config with the documented defaults.
func DefaultConfig() Config {
	return Config{AgentTimeout: DefaultAgentTimeout, RestoreEachJob: true}
}

// Provider implements api.StepProvider.
type Provider struct {
	st  *store.Store
	cfg Config
	// vmMu serialises use of the single test VM (restore + gate run).
	vmMu sync.Mutex
	// now is the clock for the verify timings (default time.Now; tests replace it).
	now func() time.Time
}

// NewProvider validates cfg and fills the defaults.
func NewProvider(st *store.Store, cfg Config) (*Provider, error) {
	if st == nil {
		return nil, errors.New("steps: store is required")
	}
	if cfg.Agent == nil || cfg.Test == nil {
		return nil, errors.New("steps: Agent and Test drivers are required")
	}
	if cfg.ReportDir == "" {
		return nil, errors.New("steps: ReportDir is required")
	}
	if cfg.AgentTimeout <= 0 {
		cfg.AgentTimeout = DefaultAgentTimeout
	}
	if cfg.Publisher == nil {
		cfg.Publisher = LocalOnlyPublisher{}
	}
	if cfg.Gate == nil {
		cfg.Gate = gate.Run
	}
	if cfg.Policy != nil {
		if _, ok := cfg.Publisher.(Planner); !ok {
			return nil, errors.New("steps: with a policy the Publisher must implement Planner")
		}
	}
	return &Provider{st: st, cfg: cfg, now: time.Now}, nil
}

// maxLedgerNote caps the note written into a task_transition event.
const maxLedgerNote = 300

// MachineOptions are the machine options the provider needs; api.NewDaemon
// applies them. They append a task_transition event to the orchestrator
// ledger (<gate home>/.aigate/ledger/orchd.jsonl, repo "orchd") for every
// transition, with the actor of the call that made it (store.ActorFrom; the
// API request's for cancel, retry and approvals, "system" for the worker) as
// both "actor" and "via".
//
// Durability: the event is written AFTER the transition committed in SQLite,
// so a crash in between can lose one transition event (the SQLite event table
// still has it). Outward actions are different: the publisher writes
// publish_action BEFORE each action (see publish.Recorder), so an action is
// never unrecorded. A ledger write failure is logged, it never fails a task.
func (p *Provider) MachineOptions() []machine.Option {
	return []machine.Option{machine.WithOnTransitionCtx(p.recordTransition)}
}

func (p *Provider) recordTransition(ctx context.Context, t store.Task, from, to machine.State, note string) {
	home, err := gate.Home(p.cfg.GateHome)
	if err != nil {
		log.Printf("orchd: ledger: %v", err)
		return
	}
	actor := store.ActorFrom(ctx)
	// Background, not ctx: a request cancelled after the transition committed
	// must not lose the head and policy hash from the event.
	if cur, err := p.st.GetTask(context.Background(), t.ID); err == nil {
		t = cur // head and policy hash may have been set by the step that just ran
	}
	if len(note) > maxLedgerNote {
		note = note[:maxLedgerNote]
		for !utf8.ValidString(note) {
			note = note[:len(note)-1]
		}
	}
	err = ledger.Append(policy.OrchLedgerPath(home), "task_transition", policy.OrchRepo, map[string]any{
		"task": int(t.ID), "from": string(from), "to": string(to), "note": note,
		"head": t.HeadSHA, "policy_hash": t.PolicyHash, "actor": actor, "via": actor,
	}, time.Now().UTC())
	if err != nil {
		log.Printf("orchd: ledger: task %d %s -> %s not recorded: %v", t.ID, from, to, err)
	}
}

// RecordSubmitted appends a task_submitted event for a task the API just
// created to the orchestrator ledger, with the submitter (store.ActorFrom(ctx))
// as "actor" and "via". api.NewDaemon wires it into the submit handler. Like
// task_transition it follows the SQLite commit: a failure is logged and never
// fails the submit.
func (p *Provider) RecordSubmitted(ctx context.Context, t store.Task) {
	home, err := gate.Home(p.cfg.GateHome)
	if err != nil {
		log.Printf("orchd: ledger: %v", err)
		return
	}
	actor := store.ActorFrom(ctx)
	err = ledger.Append(policy.OrchLedgerPath(home), "task_submitted", policy.OrchRepo, map[string]any{
		"task": int(t.ID), "repo": t.Repo, "class": t.Class, "agent": t.Agent, "actor": actor, "via": actor,
	}, time.Now().UTC())
	if err != nil {
		log.Printf("orchd: ledger: task %d submission not recorded: %v", t.ID, err)
	}
}

// Steps returns the five steps.
func (p *Provider) Steps() []machine.Step {
	return []machine.Step{
		stepFn{machine.StatePreparing, p.prepare},
		stepFn{machine.StateAgentRunning, p.runAgent},
		stepFn{machine.StateCollecting, p.collect},
		stepFn{machine.StateVerifying, p.verify},
		stepFn{machine.StatePublishing, p.publish},
	}
}

type stepFn struct {
	name machine.State
	run  func(context.Context, store.Task) (machine.Outcome, error)
}

func (s stepFn) Name() machine.State { return s.name }
func (s stepFn) Run(ctx context.Context, t store.Task) (machine.Outcome, error) {
	return s.run(ctx, t)
}

func taskID(t store.Task) string { return strconv.FormatInt(t.ID, 10) }

func failed(note string) (machine.Outcome, error) {
	return machine.Outcome{Next: machine.StateFailed, Note: note}, nil
}

func (p *Provider) prepare(ctx context.Context, t store.Task) (machine.Outcome, error) {
	if p.cfg.Policy != nil {
		// A policy that no longer loads stops the task (an infrastructure
		// error): falling back to an older policy could be more permissive.
		pol, err := p.cfg.Policy.Reload()
		if err != nil {
			return machine.Outcome{}, fmt.Errorf("policy: %w", err)
		}
		if err := p.st.SetPolicyHash(ctx, t.ID, pol.Hash()); err != nil {
			return machine.Outcome{}, err
		}
	}
	base, err := gitx.ResolveAt(t.Repo, t.Base)
	if err != nil {
		return machine.Outcome{}, fmt.Errorf("resolve base: %w", err)
	}
	if err := p.cfg.Agent.Prepare(ctx, taskID(t), t.Repo, base); err != nil {
		return machine.Outcome{}, fmt.Errorf("prepare agent vm: %w", err)
	}
	return machine.Outcome{Next: machine.StateAgentRunning, Note: "agent vm prepared"}, nil
}

func (p *Provider) runAgent(ctx context.Context, t store.Task) (machine.Outcome, error) {
	agent := t.Agent
	if agent == "" {
		agent = "claude"
	}
	res, err := p.cfg.Agent.Run(ctx, taskID(t), agent, t.Prompt, p.cfg.AgentTimeout)
	if err != nil {
		return machine.Outcome{}, err
	}
	if res.TimedOut {
		return failed("agent timeout")
	}
	return machine.Outcome{Next: machine.StateCollecting, Note: fmt.Sprintf("agent finished (rc=%d)", res.RC)}, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (p *Provider) collect(ctx context.Context, t store.Task) (machine.Outcome, error) {
	base, err := gitx.ResolveAt(t.Repo, t.Base)
	if err != nil {
		return machine.Outcome{}, fmt.Errorf("resolve base: %w", err)
	}
	head, commits, err := p.cfg.Agent.Collect(ctx, taskID(t), t.Repo, base)
	switch {
	case errors.Is(err, vm.ErrNoCommit):
		return failed("agent made no commit")
	case errors.Is(err, vm.ErrOverLimit):
		return failed("agent result over limits: " + err.Error())
	case err != nil:
		return machine.Outcome{}, err
	}
	if !shaRE.MatchString(head) {
		return machine.Outcome{}, fmt.Errorf("collect returned an invalid head %q", head)
	}
	if err := p.st.SetHeadSHA(ctx, t.ID, head); err != nil {
		return machine.Outcome{}, err
	}
	return machine.Outcome{Next: machine.StateVerifying, Note: fmt.Sprintf("collected %d commit(s), head %s", commits, head[:10])}, nil
}

func (p *Provider) verify(ctx context.Context, t store.Task) (machine.Outcome, error) {
	if t.HeadSHA == "" {
		return machine.Outcome{}, errors.New("task has no head sha")
	}
	p.vmMu.Lock()
	defer p.vmMu.Unlock()
	var restoreWall time.Duration
	var restoreRec *vm.RestoreRecord
	if p.cfg.RestoreEachJob {
		start := p.now()
		err := p.cfg.Test.Restore(ctx)
		restoreWall = p.now().Sub(start)
		rr, hasRec := p.cfg.Test.(restoreReporter)
		var rec vm.RestoreRecord
		var ok bool
		if hasRec {
			rec, ok = rr.LastRestore()
		}
		if err != nil {
			if ok && len(rec.Phases) > 0 {
				return machine.Outcome{}, fmt.Errorf("restore test vm: %w (phases: %s)", err, failedPhases(rec.Phases))
			}
			return machine.Outcome{}, fmt.Errorf("restore test vm: %w", err)
		}
		if ok {
			restoreRec = &rec
		}
	}
	ex, err := p.cfg.Test.Executor()
	if err != nil {
		return machine.Outcome{}, fmt.Errorf("test vm executor: %w", err)
	}
	defer ex.Close()
	var cacheNote func() string
	if bc := p.cfg.BaseCache; bc != nil {
		if baseSHA, rerr := gitx.ResolveAt(t.Repo, t.Base); rerr == nil {
			if repoID, _, ierr := gate.RepoID(t.Repo); ierr == nil {
				bc2 := *bc
				bc2.Scope = scopeOf(bc.Scope, p.cfg.Test)
				ce := bc2.Wrap(ex, repoID, baseSHA)
				ex, cacheNote = ce, ce.note
			}
		}
	}
	gateStart := p.now()
	r, err := p.cfg.Gate(ctx, gate.Options{Repo: t.Repo, Base: t.Base, Head: t.HeadSHA,
		TaskClass: t.Class, HomeDir: p.cfg.GateHome, Executor: ex})
	gateWall := p.now().Sub(gateStart)
	if err != nil {
		return machine.Outcome{}, fmt.Errorf("gate: %w", err)
	}
	// A report that does not describe this task's commits is not a verdict.
	if r.Head != t.HeadSHA || r.Initial || (r.Base != "" && r.Base != t.Base) {
		return machine.Outcome{}, fmt.Errorf("gate report is for base %s head %s, not this task", short(r.Base), short(r.Head))
	}
	path, err := p.saveReport(t, r)
	if err != nil {
		return machine.Outcome{}, err
	}
	if err := p.st.SetVerdict(ctx, t.ID, string(r.Category), path); err != nil {
		return machine.Outcome{}, err
	}
	if r.Category != rules.CategoryOK && p.wasPublished(ctx, t) {
		if err := p.incident(ctx, t, fmt.Sprintf("verdict %s after the task had been published", r.Category)); err != nil {
			return machine.Outcome{}, err
		}
	}
	var baseSec, headSec float64
	for _, c := range r.Checks {
		baseSec += c.Base.Seconds
		headSec += c.Head.Seconds
	}
	// A bare report (no checks) from a driver that reports no restore has
	// nothing worth timing; the note stays as it was.
	timing := ""
	if restoreRec != nil || len(r.Checks) > 0 {
		timing = timingSummary(restoreRec, restoreWall, gateWall, baseSec, headSec)
	}
	var out machine.Outcome
	switch r.Category {
	case rules.CategoryOK:
		out = machine.Outcome{Next: machine.StatePassed, Note: "verdict ok"}
	case rules.CategoryWeakened:
		if onlyProtected(r) {
			o, err := p.needApproval(ctx, t, r)
			if err != nil {
				return o, err
			}
			out = o
			break
		}
		out, _ = failed(fmt.Sprintf("%s: %s", r.Category, ruleList(r)))
	case rules.CategoryBehavior, rules.CategoryIncomplete:
		out, _ = failed(fmt.Sprintf("%s: %s", r.Category, ruleList(r)))
	default:
		return machine.Outcome{}, fmt.Errorf("gate returned unknown category %q", r.Category)
	}
	out.Note += timing
	if cacheNote != nil {
		out.Note += cacheNote()
	}
	return out, nil
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// onlyProtected reports whether every violation is protected_modified, the
// only kind a human approval can resolve.
func onlyProtected(r report.Report) bool {
	if len(r.Violations) == 0 {
		return false
	}
	for _, v := range r.Violations {
		if v.Rule != "protected_modified" {
			return false
		}
	}
	return true
}

func ruleList(r report.Report) string {
	var names []string
	seen := map[string]bool{}
	for _, v := range r.Violations {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			names = append(names, v.Rule)
		}
	}
	return strings.Join(names, ", ")
}

func (p *Provider) needApproval(ctx context.Context, t store.Task, r report.Report) (machine.Outcome, error) {
	hash := ""
	for _, a := range r.Approvals {
		if !a.Valid && a.Hash != "" {
			hash = a.Hash
			break
		}
	}
	if hash == "" {
		return machine.Outcome{}, errors.New("gate reported protected_modified without an approval hash")
	}
	var reason []string
	for _, v := range r.Violations {
		reason = append(reason, v.Detail)
	}
	text := strings.Join(reason, "; ")
	if len(text) > 1000 {
		text = text[:1000]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	if err := approvals.New(p.st).Enqueue(ctx, t.ID, store.KindProtectedModified, r.Base, r.Head, hash, text); err != nil {
		return machine.Outcome{}, err
	}
	if p.cfg.Policy != nil && autonomy.NotifyOnPark(p.level(t)) {
		p.notify(ctx, t, "task needs approval of protected-file changes")
	}
	return machine.Outcome{Next: machine.StateNeedsApproval, Note: "check_weakened: protected files changed, approval " + short(hash)}, nil
}

// saveReport writes <ReportDir>/<task>.json (0600) atomically.
func (p *Provider) saveReport(t store.Task, r report.Report) (string, error) {
	if err := os.MkdirAll(p.cfg.ReportDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(p.cfg.ReportDir, taskID(t)+".json")
	f, err := os.CreateTemp(p.cfg.ReportDir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.Write(r.JSON()); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

func (p *Provider) publish(ctx context.Context, t store.Task) (machine.Outcome, error) {
	var d pubDecision
	if p.cfg.Policy != nil {
		d = p.decidePublish(ctx, t)
		if d.handled {
			return d.out, d.err
		}
		// What the publisher (and its ledger records) see is the policy that
		// actually decided, not the one recorded when the task started.
		t.PolicyHash = d.hash
	}
	err := p.cfg.Publisher.Publish(ctx, t)
	var na *publish.NeedsApprovalError
	switch {
	case errors.As(err, &na):
		reason := fmt.Sprintf("%s: %s", na.Action.Kind, na.Reason)
		if qerr := approvals.New(p.st).Enqueue(ctx, t.ID, store.KindPublishOutsideAllowlist, t.Base, t.HeadSHA, na.Hash, reason); qerr != nil {
			return machine.Outcome{}, qerr
		}
		if p.cfg.Policy != nil && autonomy.NotifyOnPark(d.level) {
			p.notify(ctx, t, "task is waiting for approval: "+reason)
		}
		return machine.Outcome{Next: machine.StateNeedsApproval, Note: "publish_outside_allowlist: " + na.Action.Kind + " needs approval"}, nil
	case errors.Is(err, policy.ErrFloor):
		// Record only: nothing can approve it. The task fails.
		sum := approvals.PublishHash(t.ID, t.HeadSHA, "floor")
		_ = approvals.New(p.st).Enqueue(ctx, t.ID, store.KindDestructiveBlocked, t.Base, t.HeadSHA, sum, err.Error())
		if ierr := p.incident(ctx, t, "floor denial: "+err.Error()); ierr != nil {
			return machine.Outcome{}, ierr
		}
		return failed("publishing blocked by the hard floor: " + err.Error())
	case err != nil:
		return machine.Outcome{}, fmt.Errorf("publish: %w", err)
	}
	if p.cfg.Policy != nil && autonomy.NotifyAfterPublish(d.level) {
		p.notify(ctx, t, fmt.Sprintf("task %d was published automatically", t.ID))
	}
	note := "published"
	if n, ok := p.cfg.Publisher.(interface{ PublishNote() string }); ok {
		note = n.PublishNote()
	}
	if p.cfg.Policy != nil {
		note += " [policy " + short12(d.hash) + "]"
	}
	return machine.Outcome{Next: machine.StateDone, Note: note}, nil
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// level is the effective autonomy level of the task's class under the policy
// currently loaded. A state file that cannot be read yields L0 (the most
// restrictive).
func (p *Provider) level(t store.Task) autonomy.Level {
	lvl, _ := autonomy.Effective(p.cfg.Policy.Current(), t.Class)
	return lvl
}

// pubDecision is the result of decidePublish. handled means the step is
// decided here (failed, parked, or an infrastructure error in err); otherwise
// every planned action may run, under the policy with hash `hash` at `level`.
type pubDecision struct {
	out     machine.Outcome
	err     error
	handled bool
	hash    string
	level   autonomy.Level
}

// decidePublish re-reads the policy (an unloadable file is an infrastructure
// error: never a fallback to the previously loaded, possibly looser policy)
// and decides EVERY planned action before any runs: floor, autonomy level and
// the publisher's allowlist. If any action would be refused or parked, the
// whole task is decided here and nothing is published (no partial
// publication). A policy that differs from the one recorded when the task
// started parks the task for a human; the approval binds the new policy hash.
func (p *Provider) decidePublish(ctx context.Context, t store.Task) pubDecision {
	pol, err := p.cfg.Policy.Reload()
	if err != nil {
		return pubDecision{handled: true, err: fmt.Errorf("policy: %w", err)}
	}
	d := pubDecision{hash: pol.Hash()}
	d.level, _ = autonomy.Effective(pol, t.Class)
	verdict := rules.Category(t.Verdict)
	if verdict != rules.CategoryOK {
		d.handled = true
		d.out, d.err = failed(fmt.Sprintf("publishing refused: verdict is %q, not ok", t.Verdict))
		return d
	}
	pl := p.cfg.Publisher.(Planner)
	plan := pl.Plan()
	// Pass 1: anything that must fail the task outright.
	for _, kind := range plan {
		ferr := policy.Floor(kind)
		if ferr == nil && autonomy.Decide(d.level, verdict, autonomy.Action(kind)) != autonomy.Deny {
			continue
		}
		if ferr != nil {
			sum := approvals.PublishHash(t.ID, t.HeadSHA, "floor")
			_ = approvals.New(p.st).Enqueue(ctx, t.ID, store.KindDestructiveBlocked, t.Base, t.HeadSHA, sum, ferr.Error())
			if ierr := p.incident(ctx, t, "floor denial: "+ferr.Error()); ierr != nil {
				d.handled, d.err = true, ierr
				return d
			}
		}
		d.handled = true
		d.out, d.err = failed(fmt.Sprintf("publishing denied: %s at level %s", kind, d.level))
		return d
	}
	// Pass 2: anything that needs a human first.
	park := func(hash, reason, note string) pubDecision {
		d.handled = true
		if qerr := approvals.New(p.st).Enqueue(ctx, t.ID, store.KindPublishOutsideAllowlist, t.Base, t.HeadSHA, hash, reason); qerr != nil {
			d.err = qerr
			return d
		}
		if autonomy.NotifyOnPark(d.level) {
			p.notify(ctx, t, "task is waiting for approval: "+reason)
		}
		d.out = machine.Outcome{Next: machine.StateNeedsApproval, Note: note}
		return d
	}
	if len(plan) > 0 && t.PolicyHash != d.hash {
		pk := PolicyChangeKind(d.hash)
		if !pl.Granted(t, pk) {
			old := "none"
			if t.PolicyHash != "" {
				old = short12(t.PolicyHash)
			}
			reason := fmt.Sprintf("policy changed since the task started: %s -> %s", old, short12(d.hash))
			return park(approvals.PublishHash(t.ID, t.HeadSHA, pk), reason, "publish_outside_allowlist: "+reason)
		}
	}
	for _, kind := range plan {
		if pl.Granted(t, kind) {
			continue // a human approved exactly this action for this head
		}
		if autonomy.Decide(d.level, verdict, autonomy.Action(kind)) == autonomy.Park {
			reason := fmt.Sprintf("%s: autonomy level %s needs a human approval", kind, d.level)
			return park(approvals.PublishHash(t.ID, t.HeadSHA, kind), reason,
				fmt.Sprintf("publish_outside_allowlist: %s needs approval (level %s)", kind, d.level))
		}
		if perr := pl.Permit(kind); perr != nil {
			reason := fmt.Sprintf("%s: %s is not in the allowlist", kind, kind)
			return park(approvals.PublishHash(t.ID, t.HeadSHA, kind), reason,
				fmt.Sprintf("publish_outside_allowlist: %s needs approval (not in the allowlist)", kind))
		}
	}
	return d
}

func (p *Provider) notify(ctx context.Context, t store.Task, msg string) {
	if p.cfg.Notifier != nil {
		_ = p.cfg.Notifier.Notify(ctx, t.ID, msg)
	}
}

// wasPublished reports whether the task has ever reached done.
func (p *Provider) wasPublished(ctx context.Context, t store.Task) bool {
	evs, err := p.st.Events(ctx, t.ID)
	if err != nil {
		return false
	}
	for _, e := range evs {
		if e.To == machine.StateDone {
			return true
		}
	}
	return false
}

// incident lowers the task class by one level (see autonomy.Demote) and
// writes the ledger event into the task repository's ledger. A demotion that
// cannot be persisted is never dropped: an autonomy_demote_failed event goes to
// the orchestrator ledger (best effort) and the error is returned, which the
// step reports as an infrastructure error (retry, then failed). If the state
// was saved but only the ledger append failed, the class IS demoted (a retry
// would demote again), so that case is logged and not returned.
func (p *Provider) incident(ctx context.Context, t store.Task, reason string) error {
	if p.cfg.Policy == nil {
		return nil
	}
	home, err := gate.Home(p.cfg.GateHome)
	if err != nil {
		return fmt.Errorf("demotion of class %q not possible: %w", t.Class, err)
	}
	repoID := policy.OrchRepo
	ledgerPath := policy.OrchLedgerPath(home)
	if id, _, err := gate.RepoID(t.Repo); err == nil {
		repoID, ledgerPath = id, gate.LedgerPath(home, id)
	}
	from, to, err := autonomy.Demote(p.cfg.Policy.Current(), autonomy.Incident{Class: t.Class, Task: t.ID, Reason: reason},
		ledgerPath, repoID, time.Now())
	if err == nil {
		return nil
	}
	text := strings.ToValidUTF8(err.Error(), "")
	if len(text) > 300 {
		text = text[:300]
	}
	if lerr := ledger.Append(policy.OrchLedgerPath(home), "autonomy_demote_failed", policy.OrchRepo, map[string]any{
		"class": t.Class, "task": int(t.ID), "reason": reason, "error": text,
	}, time.Now().UTC()); lerr != nil {
		log.Printf("orchd: autonomy_demote_failed for class %q not recorded: %v", t.Class, lerr)
	}
	if from != to { // saved, only the ledger append failed
		log.Printf("orchd: class %q demoted but the ledger event failed: %v", t.Class, err)
		return nil
	}
	return fmt.Errorf("cannot persist the demotion of class %q (%s): %w", t.Class, reason, err)
}
