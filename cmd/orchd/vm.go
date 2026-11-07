package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/orch/vm"
	"aigate/internal/remote"
)

// vmFlags configures the real VM drivers. Only paths of keys and known_hosts
// files are ever held or printed, never key contents.
type vmFlags struct {
	AgentHost, AgentUser, AgentKey, AgentKnownHosts, AgentJump string
	TestHost, TestUser, TestKey, TestKnownHosts, TestRepoDir   string
	TestWorkDir, TestJump                                      string
	CloudCmd, TestSnapshot                                     string
	AgentTimeout                                               time.Duration
	RestoreEachJob, AllowPublishWithoutRestore                 bool
	PruneRestoreVolumes                                        bool
	PruneMax                                                   int
	ReportDir, GateHome                                        string
	// Publishing (all optional; none given = local-only, nothing is pushed).
	// live is the policy in force (nil without --policy); limits come from it.
	live                                                                       *policy.Live
	PublishAllow, GitRemote, GitLabURL, GitLabProject, GitLabTarget, TokenFile string
}

func (v *vmFlags) register(fs *flag.FlagSet) {
	s := func(p *string, name, def, usage string) {
		fs.StringVar(p, name, envOr("ORCHD_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_")), def), usage)
	}
	// Publishing settings come from flags only: an environment variable in the
	// service unit must not silently widen what orchd may publish.
	sf := func(p *string, name, def, usage string) { fs.StringVar(p, name, def, usage) }
	s(&v.AgentHost, "agent-host", "", "agent VM host (enables the real steps)")
	s(&v.AgentUser, "agent-user", "", "agent VM ssh user (default: the driver's \"agent\")")
	s(&v.AgentKey, "agent-key", "", "path of the agent VM's forced-command ssh key")
	s(&v.AgentKnownHosts, "agent-known-hosts", "", "known_hosts file pinning the agent VM")
	s(&v.AgentJump, "agent-jump", "", "optional user@host jump host for the agent VM")
	s(&v.TestHost, "test-host", "", "test VM host")
	s(&v.TestUser, "test-user", "", "test VM ssh user")
	s(&v.TestKey, "test-key", "", "path of the test VM ssh key")
	s(&v.TestKnownHosts, "test-known-hosts", "", "known_hosts file pinning the test VM")
	s(&v.TestRepoDir, "test-repo-dir", "", "absolute repo directory on the test VM")
	s(&v.TestWorkDir, "test-work-dir", "", "absolute work directory on the test VM (default /tmp)")
	s(&v.TestJump, "test-jump", "", "optional user@host jump host for the test VM")
	s(&v.CloudCmd, "cloud-cmd", "", "command prefix for the openstack CLI used by TestVM.Restore, whitespace separated (default: the driver's)")
	s(&v.TestSnapshot, "test-snapshot", "", "Cinder snapshot the test VM is restored from (default <test VM name>-clean)")
	s(&v.ReportDir, "report-dir", "", "directory for verification reports (default <db dir>/reports)")
	s(&v.GateHome, "gate-home", "", "gate home for approvals and ledger (default <db dir>/gate-home)")
	sf(&v.PublishAllow, "publish-allow", "", "comma separated publish actions that may run without a human (push_branch,draft_pr,notify); empty = every action waits for approval")
	sf(&v.GitRemote, "git-remote", "", "where push_branch pushes refs/heads/orch/<task>: absolute path or https URL without credentials")
	sf(&v.GitLabURL, "gitlab-url", "", "GitLab base URL for draft merge requests")
	sf(&v.GitLabProject, "gitlab-project", "", "GitLab project id or path")
	sf(&v.GitLabTarget, "gitlab-target", "main", "target branch of the draft merge request")
	sf(&v.TokenFile, "gitlab-token-file", "", "0600 file holding the GitLab write token (never printed)")
	fs.DurationVar(&v.AgentTimeout, "agent-timeout", steps.DefaultAgentTimeout, "limit of one agent run")
	fs.BoolVar(&v.RestoreEachJob, "restore-each-job", true, "restore the test VM before every verification")
	fs.BoolVar(&v.PruneRestoreVolumes, "prune-restore-volumes", false,
		"after a successful restore, delete the volumes earlier restores created (guarded: exact driver name, not the boot volume, not a snapshot origin, no live attachment)")
	fs.IntVar(&v.PruneMax, "prune-max", 5, "most volumes --prune-restore-volumes deletes per restore")
	fs.BoolVar(&v.AllowPublishWithoutRestore, "allow-publish-without-restore", false,
		"allow --restore-each-job=false together with publishing (a check can then leave a process that forges the next task's results)")
}

// enabled reports whether any VM flag that selects the real drivers is set.
func (v *vmFlags) enabled() bool {
	for _, x := range []string{v.AgentHost, v.AgentUser, v.AgentKey, v.AgentKnownHosts, v.AgentJump,
		v.TestHost, v.TestUser, v.TestKey, v.TestKnownHosts, v.TestRepoDir, v.TestWorkDir, v.TestJump, v.CloudCmd} {
		if x != "" {
			return true
		}
	}
	return false
}

// validate refuses a half-specified VM configuration.
func (v *vmFlags) validate() error {
	if v.PruneMax < 0 || v.PruneMax > 50 {
		return errors.New("--prune-max must be between 0 (default 5) and 50")
	}
	var missing []string
	for _, r := range []struct{ name, val string }{
		{"agent-host", v.AgentHost}, {"agent-key", v.AgentKey}, {"agent-known-hosts", v.AgentKnownHosts},
		{"test-host", v.TestHost}, {"test-user", v.TestUser}, {"test-key", v.TestKey},
		{"test-known-hosts", v.TestKnownHosts}, {"test-repo-dir", v.TestRepoDir},
	} {
		if r.val == "" {
			missing = append(missing, "--"+r.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("VM flags are only partly specified; missing %s (give all of them or none)", strings.Join(missing, ", "))
	}
	if v.AgentTimeout <= 0 {
		return errors.New("--agent-timeout must be positive")
	}
	return nil
}

// publishConfigured reports whether any publishing flag is set.
func (v *vmFlags) publishConfigured() bool {
	for _, x := range []string{v.GitRemote, v.GitLabURL, v.TokenFile, v.PublishAllow} {
		if x != "" {
			return true
		}
	}
	return false
}

// gateHome is the gate home orchd and the steps use (and `orch approve` must).
func (v *vmFlags) gateHome(dbPath string) string {
	if v.GateHome != "" {
		return v.GateHome
	}
	return filepath.Join(filepath.Dir(dbPath), "gate-home")
}

// checkLocations refuses a database, gate home or token file that the agent or
// test VM can see (they live outside every allowed repository root) and a
// token file that is not private.
func (v *vmFlags) checkLocations(dbPath string, exposed []string) error {
	for name, p := range map[string]string{"database": dbPath, "gate home": v.gateHome(dbPath), "token file": v.TokenFile} {
		if p == "" {
			continue
		}
		if err := approvals.CheckOutside(p, exposed); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// provider builds the real step provider. Nothing connects to a VM here.
func (v *vmFlags) provider(st *store.Store, dbPath string) (api.StepProvider, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(dbPath)
	reportDir, gateHome := v.ReportDir, v.gateHome(dbPath)
	if reportDir == "" {
		reportDir = filepath.Join(dir, "reports")
	}
	agent := &vm.AgentVM{Host: v.AgentHost, User: v.AgentUser, KeyPath: v.AgentKey,
		KnownHosts: v.AgentKnownHosts, JumpHost: v.AgentJump}
	if v.live != nil {
		lim := v.live.Current().Limits
		agent.MaxCommits = lim.MaxCommits
		agent.MaxBytes = lim.MaxFetchBytes
	}
	sshCfg := remote.Config{Host: v.TestHost, User: v.TestUser, KeyPath: v.TestKey,
		KnownHosts: v.TestKnownHosts, RepoDir: v.TestRepoDir, WorkDir: v.TestWorkDir, JumpHost: v.TestJump}
	if _, err := remote.NewSSH(sshCfg); err != nil { // validates host, user, paths early
		return nil, fmt.Errorf("test VM: %w", err)
	}
	cloud := &vm.OpenStackCLI{Prefix: strings.Fields(v.CloudCmd), Probe: tcpProbe(v.TestHost), Snapshot: v.TestSnapshot,
		PruneRestoreVolumes: v.PruneRestoreVolumes, MaxPrune: v.PruneMax}
	test := &vm.TestVM{Cloud: cloud, SSH: sshCfg, Snapshot: v.TestSnapshot, PruneRestoreVolumes: v.PruneRestoreVolumes,
		OnPrune: pruneLogger(os.Stderr)}
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = agent, test
	cfg.AgentTimeout, cfg.RestoreEachJob = v.AgentTimeout, v.RestoreEachJob
	cfg.ReportDir, cfg.GateHome = reportDir, gateHome
	cfg.Policy = v.live
	if v.live != nil && !v.publishConfigured() {
		// Nothing is published, but a policy still decides (and a human can
		// only approve what the publisher plans): LocalOnlyPublisher plans nothing.
		cfg.Publisher = steps.LocalOnlyPublisher{}
	}
	if v.publishConfigured() {
		pcfg := publish.Config{Remote: v.GitRemote, TokenFile: v.TokenFile,
			Allow:  policy.NewAllowlist(splitList(v.PublishAllow)...),
			GitLab: publish.GitLab{BaseURL: v.GitLabURL, Project: v.GitLabProject, TargetBranch: v.GitLabTarget},
			Grants: approvals.Grants{Home: gateHome},
			Ledger: publish.LedgerRecorder(policy.OrchLedgerPath(gateHome))}
		if v.live != nil {
			pcfg.AllowSource = func() policy.Allowlist { return v.live.Current().AllowList() }
		}
		pub, err := publish.New(pcfg)
		if err != nil {
			return nil, err
		}
		cfg.Publisher = pub
	}
	p, err := steps.NewProvider(st, cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// pruneLogger returns the TestVM.OnPrune callback that writes one line per
// background prune: counts, then the deleted and skipped volume NAMES (the
// "name id" entries are cut to the name; no ids, no credentials).
func pruneLogger(w io.Writer) func(vm.PruneResult, error) {
	names := func(l []string) string {
		var n []string
		for _, e := range l {
			n = append(n, strings.Fields(e + " ")[0])
		}
		return strings.Join(n, ",")
	}
	return func(res vm.PruneResult, err error) {
		line := fmt.Sprintf("orchd: background prune: deleted %d [%s], skipped %d [%s], warnings %d",
			len(res.Deleted), names(res.Deleted), len(res.Skipped), names(res.Skipped), len(res.Warnings))
		if err != nil {
			line += ", error: " + err.Error()
		}
		fmt.Fprintln(w, line)
	}
}

// tcpProbe checks that something answers on the ssh port. It deliberately
// does not authenticate: the executor does that on first use.
func tcpProbe(host string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		h := host
		if !strings.Contains(h, ":") || strings.Count(h, ":") > 1 {
			h = net.JoinHostPort(h, "22")
		}
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", h)
		if err != nil {
			return err
		}
		return c.Close()
	}
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// checkRestoreMode refuses --restore-each-job=false together with any
// publishing configuration (flags, or an allowlist in the policy) unless
// --allow-publish-without-restore is given: without a restore, test code of one
// job can persist into the next verification.
func (v *vmFlags) checkRestoreMode() error {
	if v.RestoreEachJob || v.AllowPublishWithoutRestore {
		return nil
	}
	if v.publishConfigured() || v.live != nil && len(v.live.Current().Allowlist) > 0 {
		return errors.New("--restore-each-job=false with publishing configured (allowlist, git remote or GitLab) is refused; " +
			"restore the test VM per job or pass --allow-publish-without-restore explicitly")
	}
	return nil
}

// describe lists the effective configuration for the startup log. It names
// hosts, remotes and the source of the allowlist, never key files' contents,
// token values or paths of secrets.
func (v *vmFlags) describe(db string, concurrency int) []string {
	allow, source := "none (every action waits for approval)", "none"
	switch {
	case v.live != nil:
		pol := v.live.Current()
		allow, source = strings.Join(pol.Allowlist, ","), "policy file"
		if allow == "" {
			allow = "empty (every action waits for approval)"
		}
	case v.PublishAllow != "":
		allow, source = v.PublishAllow, "--publish-allow flag"
	}
	l := []string{
		"database " + db,
		fmt.Sprintf("concurrency %d, agent timeout %s", concurrency, v.AgentTimeout),
		fmt.Sprintf("restore test VM before each job: %t", v.RestoreEachJob),
		fmt.Sprintf("prune old restore volumes after a restore: %t (max %d per restore)", v.PruneRestoreVolumes, v.PruneMax),
		"publish allowlist: " + allow + " (source: " + source + ")",
	}
	if v.live != nil {
		l = append(l, "policy hash "+v.live.Current().Hash())
	} else {
		l = append(l, "policy: none")
	}
	if v.GitRemote != "" {
		l = append(l, "git remote (push_branch): "+v.GitRemote)
	}
	if v.GitLabURL != "" {
		l = append(l, fmt.Sprintf("GitLab draft MR: %s project %s target %s (token file set: %t)",
			v.GitLabURL, v.GitLabProject, v.GitLabTarget, v.TokenFile != ""))
	}
	if v.AllowPublishWithoutRestore && !v.RestoreEachJob && v.publishConfigured() {
		l = append(l, "WARNING: publishing without a test VM restore per job (--allow-publish-without-restore)")
	}
	if v.enabled() {
		l = append(l, "agent VM "+v.AgentHost+", test VM "+v.TestHost)
	} else {
		l = append(l, "no VM flags: placeholder steps (tasks end failed)")
	}
	return l
}
