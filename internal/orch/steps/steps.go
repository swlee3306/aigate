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
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
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
	Collect(ctx context.Context, task, localRepo string) (headSHA string, commits int, err error)
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
	Agent AgentDriver
	Test  TestDriver
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
	// LedgerPath is the hook point for Task 11's ledger writer. Unused here:
	// the gate itself appends its own run_result event under GateHome.
	LedgerPath string
	// Gate defaults to gate.Run.
	Gate GateFunc
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
	return &Provider{st: st, cfg: cfg}, nil
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
	base, err := gitx.Resolve(t.Repo, t.Base)
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
	head, commits, err := p.cfg.Agent.Collect(ctx, taskID(t), t.Repo)
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
	if p.cfg.RestoreEachJob {
		if err := p.cfg.Test.Restore(ctx); err != nil {
			return machine.Outcome{}, fmt.Errorf("restore test vm: %w", err)
		}
	}
	ex, err := p.cfg.Test.Executor()
	if err != nil {
		return machine.Outcome{}, fmt.Errorf("test vm executor: %w", err)
	}
	defer ex.Close()
	r, err := p.cfg.Gate(ctx, gate.Options{Repo: t.Repo, Base: t.Base, Head: t.HeadSHA,
		TaskClass: t.Class, HomeDir: p.cfg.GateHome, Executor: ex})
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
	switch r.Category {
	case rules.CategoryOK:
		return machine.Outcome{Next: machine.StatePassed, Note: "verdict ok"}, nil
	case rules.CategoryWeakened:
		if onlyProtected(r) {
			return p.needApproval(ctx, t, r)
		}
		fallthrough
	case rules.CategoryBehavior, rules.CategoryIncomplete:
		return failed(fmt.Sprintf("%s: %s", r.Category, ruleList(r)))
	}
	return machine.Outcome{}, fmt.Errorf("gate returned unknown category %q", r.Category)
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
	err := p.cfg.Publisher.Publish(ctx, t)
	var na *publish.NeedsApprovalError
	switch {
	case errors.As(err, &na):
		reason := fmt.Sprintf("%s: %s", na.Action.Kind, na.Reason)
		if qerr := approvals.New(p.st).Enqueue(ctx, t.ID, store.KindPublishOutsideAllowlist, t.Base, t.HeadSHA, na.Hash, reason); qerr != nil {
			return machine.Outcome{}, qerr
		}
		return machine.Outcome{Next: machine.StateNeedsApproval, Note: "publish_outside_allowlist: " + na.Action.Kind + " needs approval"}, nil
	case errors.Is(err, policy.ErrFloor):
		// Record only: nothing can approve it. The task fails.
		sum := approvals.PublishHash(t.ID, t.HeadSHA, "floor")
		_ = approvals.New(p.st).Enqueue(ctx, t.ID, store.KindDestructiveBlocked, t.Base, t.HeadSHA, sum, err.Error())
		return failed("publishing blocked by the hard floor: " + err.Error())
	case err != nil:
		return machine.Outcome{}, fmt.Errorf("publish: %w", err)
	}
	note := "published"
	if n, ok := p.cfg.Publisher.(interface{ PublishNote() string }); ok {
		note = n.PublishNote()
	}
	return machine.Outcome{Next: machine.StateDone, Note: note}, nil
}
