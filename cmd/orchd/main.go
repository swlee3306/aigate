// Command orchd is the orchestrator daemon: it runs the task state machine and
// serves the local JSON API on a unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

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

// uidList is a repeatable numeric uid flag.
type uidList []uint32

func (u *uidList) String() string { return fmt.Sprint(*u) }
func (u *uidList) Set(v string) error {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return fmt.Errorf("uid %q is not a number", v)
	}
	*u = append(*u, uint32(n))
	return nil
}

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
	var socketUIDs uidList
	fs.Var(&socketUIDs, "socket-uid", "uid allowed to use the socket (repeatable; none = only orchd's own uid)")
	fs.StringVar(&cfg.RPID, "rp-id", "", "WebAuthn relying party ID: the web console's host name (passkey enrollment; empty = off)")
	fs.StringVar(&cfg.Origin, "origin", "", "exact web console origin, https://host[:port] under --rp-id (http://localhost only for development)")
	fs.StringVar(&cfg.PendingFile, "pending-file", api.DefaultPendingFile, "pending passkey enrollments (orch admin signers pending / add --pending)")
	var vf vmFlags
	vf.register(fs)
	policyPath := fs.String("policy", envOr("ORCHD_POLICY", ""), "policy.yml (autonomy levels, allowlist, limits); re-read when each task starts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg.AllowedRepos = repos
	cfg.SocketUIDs = socketUIDs
	if err := checkWebAuthnFlags(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "orchd:", err)
		return 2
	}
	if *policyPath != "" {
		live, err := applyPolicy(fs, *policyPath, &cfg, &vf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
		vf.live = live
		cfg.PolicyHash = func() string { return live.Current().Hash() }
		if err := checkStateDirWritable(live.Current().StatePath()); err != nil {
			fmt.Fprintln(os.Stderr, "orchd: warning:", err)
		}
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
		if err := vf.checkRestoreMode(); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
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
	seedTaskIDs(context.Background(), st, cfg.AllowedRepos, vf.GitRemote, log.Printf)
	if vf.enabled() {
		if err := approvals.CheckPrivate(cfg.DB); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
	}
	var provider api.StepProvider = placeholderSteps{}
	if vf.enabled() {
		cfg.ReportDir = vf.reportDir(cfg.DB)
		if provider, err = vf.provider(st, cfg.DB); err != nil {
			fmt.Fprintln(os.Stderr, "orchd:", err)
			return 2
		}
	}
	for _, l := range vf.describe(cfg.DB, cfg.Concurrency) {
		log.Print("orchd: config: ", l)
	}
	cfg.Policy = policySupplier(vf.live, vf.baseCacheDir != "")
	cfg.Ops = opsSupplier(buildID(), time.Now(), cfg.PolicyHash, vf.restoreRecord, vf.baseCacheDir)
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

// checkStateDirWritable warns about a policy directory orchd cannot write to:
// automatic demotions are saved next to the policy file (policy.state.json), so
// there they would fail (the task then stops with an infrastructure error).
func checkStateDirWritable(statePath string) error {
	dir := filepath.Dir(statePath)
	f, err := os.CreateTemp(dir, ".orchd-write-check-*")
	if err != nil {
		return fmt.Errorf("the policy state directory %s is not writable (%v): an automatic demotion could not be saved", dir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}

// checkWebAuthnFlags validates --rp-id, --origin and --pending-file before
// anything is opened.
func checkWebAuthnFlags(cfg api.Config) error {
	if err := api.ValidateWebAuthnConfig(cfg.RPID, cfg.Origin); err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.PendingFile) || filepath.Clean(cfg.PendingFile) != cfg.PendingFile {
		return fmt.Errorf("--pending-file %q must be an absolute clean path", cfg.PendingFile)
	}
	return nil
}
