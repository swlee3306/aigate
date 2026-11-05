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
	CloudCmd                                                   string
	AgentTimeout                                               time.Duration
	RestoreEachJob                                             bool
	ReportDir, GateHome                                        string
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
	s(&v.ReportDir, "report-dir", "", "directory for verification reports (default <db dir>/reports)")
	s(&v.GateHome, "gate-home", "", "gate home for approvals and ledger (default <db dir>/gate-home)")
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

// provider builds the real step provider. Nothing connects to a VM here.
func (v *vmFlags) provider(st *store.Store, dbPath string) (api.StepProvider, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(dbPath)
	reportDir, gateHome := v.ReportDir, v.GateHome
	if reportDir == "" {
		reportDir = filepath.Join(dir, "reports")
	}
	if gateHome == "" {
		gateHome = filepath.Join(dir, "gate-home")
	}
	agent := &vm.AgentVM{Host: v.AgentHost, User: v.AgentUser, KeyPath: v.AgentKey,
		KnownHosts: v.AgentKnownHosts, JumpHost: v.AgentJump}
	sshCfg := remote.Config{Host: v.TestHost, User: v.TestUser, KeyPath: v.TestKey,
		KnownHosts: v.TestKnownHosts, RepoDir: v.TestRepoDir, WorkDir: v.TestWorkDir, JumpHost: v.TestJump}
	if _, err := remote.NewSSH(sshCfg); err != nil { // validates host, user, paths early
		return nil, fmt.Errorf("test VM: %w", err)
	}
	cloud := &vm.OpenStackCLI{Prefix: strings.Fields(v.CloudCmd), Probe: tcpProbe(v.TestHost)}
	test := &vm.TestVM{Cloud: cloud, SSH: sshCfg}
	cfg := steps.DefaultConfig()
	cfg.Agent, cfg.Test = agent, test
	cfg.AgentTimeout, cfg.RestoreEachJob = v.AgentTimeout, v.RestoreEachJob
	cfg.ReportDir, cfg.GateHome = reportDir, gateHome
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
