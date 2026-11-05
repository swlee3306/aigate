package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/gate"
	"aigate/internal/remote"
)

// Backend selects where the checks of every scenario run (AIGATE_E2E_BACKEND):
//
//	local    this machine (default; gate.Options.Executor stays nil)
//	fakessh  the ssh executor against internal/remote/testdata/fakessh.sh, with
//	         a per-test temp sandbox as the "VM"
//	vm       the real check VM, described by AIGATE_E2E_VM_HOST, _USER, _KEY,
//	         _KNOWN_HOSTS, _REPODIR, _WORKDIR and optionally _JUMP (user@host)
const (
	BackendLocal   = "local"
	BackendFakeSSH = "fakessh"
	BackendVM      = "vm"
)

// Backend returns the selected backend name.
func Backend() string {
	if b := os.Getenv("AIGATE_E2E_BACKEND"); b != "" {
		return b
	}
	return BackendLocal
}

// executorFor builds the Executor for the selected backend; nil means the
// gate's own local executor. A misconfigured backend fails the test: it never
// skips and never falls back to local.
func executorFor(t *testing.T) gate.Executor {
	t.Helper()
	switch b := Backend(); b {
	case BackendLocal:
		return nil
	case BackendFakeSSH:
		return newSSHExecutor(t, fakeSSHConfig(t))
	case BackendVM:
		return newSSHExecutor(t, vmConfig(t))
	default:
		t.Fatalf("AIGATE_E2E_BACKEND=%q: want local, fakessh or vm", b)
		return nil
	}
}

func newSSHExecutor(t *testing.T, cfg remote.Config) gate.Executor {
	t.Helper()
	ex, err := remote.NewSSH(cfg)
	if err != nil {
		t.Fatalf("e2e backend %s: %v", Backend(), err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

// fakeSSHConfig points the ssh executor at fakessh.sh and a temp sandbox that
// plays the VM (bare repo and work dir live inside it).
func fakeSSHConfig(t *testing.T) remote.Config {
	t.Helper()
	fake, err := filepath.Abs(filepath.Join("..", "internal", "remote", "testdata", "fakessh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fake); err != nil {
		t.Fatalf("fakessh backend: %v", err)
	}
	sandbox, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(sandbox, "known_hosts")
	if err := os.WriteFile(kh, []byte("vm1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return remote.Config{
		Host: "vm1", User: "ci", KeyPath: filepath.Join(sandbox, "id"), KnownHosts: kh,
		RepoDir: filepath.Join(sandbox, "repo.git"), WorkDir: filepath.Join(sandbox, "work"),
		SSHBin: fake, ConnectTimeout: 5 * time.Second, Grace: 30 * time.Second,
	}
}

// vmConfig reads the real VM description from the environment; a missing
// variable fails the test with the list of what is missing.
func vmConfig(t *testing.T) remote.Config {
	t.Helper()
	get := func(name string) string {
		v := os.Getenv("AIGATE_E2E_VM_" + name)
		if v == "" {
			t.Errorf("AIGATE_E2E_BACKEND=vm needs AIGATE_E2E_VM_%s", name)
		}
		return v
	}
	cfg := remote.Config{
		Host: get("HOST"), User: get("USER"), KeyPath: get("KEY"), KnownHosts: get("KNOWN_HOSTS"),
		RepoDir: get("REPODIR"), WorkDir: get("WORKDIR"), JumpHost: os.Getenv("AIGATE_E2E_VM_JUMP"),
	}
	if t.Failed() {
		t.FailNow()
	}
	return cfg
}

// Executor returns the workspace's executor for the selected backend, created
// on first use and shared by every run of the scenario (nil on local).
func (w *Workspace) Executor() gate.Executor {
	w.T.Helper()
	if !w.exInit {
		w.ex, w.exInit = executorFor(w.T), true
	}
	return w.ex
}
