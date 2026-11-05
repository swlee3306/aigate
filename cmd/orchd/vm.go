package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
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
	RestoreEachJob                                             bool
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
	s(&v.PublishAllow, "publish-allow", "", "comma separated publish actions that may run without a human (push_branch,draft_pr,notify); empty = every action waits for approval")
	s(&v.GitRemote, "git-remote", "", "where push_branch pushes refs/heads/orch/<task>: absolute path or https URL without credentials")
	s(&v.GitLabURL, "gitlab-url", "", "GitLab base URL for draft merge requests")
	s(&v.GitLabProject, "gitlab-project", "", "GitLab project id or path")
	s(&v.GitLabTarget, "gitlab-target", "main", "target branch of the draft merge request")
	s(&v.TokenFile, "gitlab-token-file", "", "0600 file holding the GitLab write token (never printed)")
	fs.DurationVar(&v.AgentTimeout, "agent-timeout", steps.DefaultAgentTimeout, "limit of one agent run")
	fs.BoolVar(&v.RestoreEachJob, "restore-each-job", true, "restore the test VM before every verification")
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
	cloud := &vm.OpenStackCLI{Prefix: strings.Fields(v.CloudCmd), Probe: tcpProbe(v.TestHost), Snapshot: v.TestSnapshot}
	test := &vm.TestVM{Cloud: cloud, SSH: sshCfg, Snapshot: v.TestSnapshot}
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
			Grants: approvals.Grants{Home: gateHome}}
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
