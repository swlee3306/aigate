// Command orchd is the orchestrator daemon: it runs the task state machine and
// serves the local JSON API on a unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return fmt.Sprint(*s) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// placeholderSteps is used when no VM flags are given. Every
// step reports an infrastructure error, so a task retries and ends in failed.
// It can never produce passed.
type placeholderSteps struct{}

type placeholderStep struct{ name machine.State }

func (p placeholderStep) Name() machine.State { return p.name }
func (placeholderStep) Run(context.Context, store.Task) (machine.Outcome, error) {
	return machine.Outcome{}, errors.New("step not implemented")
}

func (placeholderSteps) Steps() []machine.Step {
	var out []machine.Step
	for _, s := range []machine.State{machine.StatePreparing, machine.StateAgentRunning,
		machine.StateCollecting, machine.StateVerifying, machine.StatePublishing} {
		out = append(out, placeholderStep{s})
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("orchd", flag.ContinueOnError)
	var cfg api.Config
	var repos stringList
	fs.StringVar(&cfg.DB, "db", envOr("ORCHD_DB", "/var/lib/orchd/orchd.db"), "SQLite database path")
	fs.StringVar(&cfg.Socket, "socket", envOr("ORCHD_SOCKET", "/run/orchd/orchd.sock"), "unix socket path")
	fs.IntVar(&cfg.Concurrency, "concurrency", 1, "tasks run in parallel")
	fs.Var(&repos, "allow-repo", "repository root tasks may use (repeatable; none = refuse all)")
	var vf vmFlags
	vf.register(fs)
	policyPath := fs.String("policy", envOr("ORCHD_POLICY", ""), "policy.yml (autonomy levels, allowlist, limits); re-read when each task starts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg.AllowedRepos = repos
	if *policyPath != "" {
		live, err := applyPolicy(fs, *policyPath, &cfg, &vf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
		vf.live = live
	}
	if vf.enabled() {
		// Refuse before touching the database or the socket.
		if err := vf.validate(); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
	}

	if !vf.enabled() && vf.publishConfigured() {
		fmt.Fprintln(os.Stderr, "orchd: publishing flags need the VM flags too")
		return 2
	}
	if vf.enabled() {
		if err := vf.checkLocations(cfg.DB, cfg.AllowedRepos); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
		cfg.GateHome = vf.gateHome(cfg.DB)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orchd: open db:", err)
		return 1
	}
	defer st.Close()
	if vf.enabled() {
		if err := approvals.CheckPrivate(cfg.DB); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
	}
	var provider api.StepProvider = placeholderSteps{}
	if vf.enabled() {
		if provider, err = vf.provider(st, cfg.DB); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
	}
	d, err := api.NewDaemon(cfg, st, provider)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orchd:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "orchd:", err)
		return 1
	}
	return 0
}

// applyPolicy loads the policy and merges it with the flags. A value given
// both by a flag and by the policy must be the same, otherwise orchd refuses
// to start: neither silently wins. The publish allowlist comes only from the
// policy when one is given.
func applyPolicy(fs *flag.FlagSet, path string, cfg *api.Config, vf *vmFlags) (*policy.Live, error) {
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	home := vf.gateHome(cfg.DB)
	live, err := policy.NewLive(path, policy.OrchLedgerPath(home))
	if err != nil {
		return nil, err
	}
	pol := live.Current()
	conflict := func(what string) error {
		return fmt.Errorf("%s is set by a flag and by the policy %s with different values; remove one of them", what, path)
	}
	if pol.Limits.MaxConcurrent != 0 {
		if setFlags["concurrency"] && cfg.Concurrency != pol.Limits.MaxConcurrent {
			return nil, conflict("concurrency (limits.max_concurrent)")
		}
		cfg.Concurrency = pol.Limits.MaxConcurrent
	}
	if pol.Limits.AgentTimeout != 0 {
		if setFlags["agent-timeout"] && vf.AgentTimeout != pol.Limits.AgentTimeout {
			return nil, conflict("agent-timeout (limits.agent_timeout)")
		}
		vf.AgentTimeout = pol.Limits.AgentTimeout
	}
	if len(pol.RepoRoots) > 0 {
		if len(cfg.AllowedRepos) > 0 && !sameSet(cfg.AllowedRepos, pol.RepoRoots) {
			return nil, conflict("the repository roots (--allow-repo, repo_roots)")
		}
		cfg.AllowedRepos = pol.RepoRoots
	}
	if vf.PublishAllow != "" {
		return nil, errors.New("--publish-allow cannot be combined with --policy: put the allowlist in the policy file")
	}
	if cfg.AllowedRepos != nil {
		if err := approvals.CheckOutside(path, cfg.AllowedRepos); err != nil {
			return nil, fmt.Errorf("policy file: %w", err)
		}
	}
	return live, nil
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}
