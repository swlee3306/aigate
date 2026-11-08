package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
)

// Config is everything orchd needs from the outside. Task 12 will fill it
// from policy.yml; today it comes from flags.
type Config struct {
	DB           string   // SQLite path (used by main to open the store)
	Socket       string   // unix socket path
	Concurrency  int      // parallel Tick workers; < 1 means 1
	AllowedRepos []string // absolute roots a task's repo must live under; empty refuses all
	PollInterval time.Duration
	// GateHome is the gate's home whose .aigate/approvals.json holds the
	// human grants (empty: the gate's default, AIGATE_HOME or the user home).
	GateHome string
	// ReportDir is the directory the steps provider saves reports in; the
	// report endpoint serves only files under it (empty: no reports served).
	ReportDir string
	// PolicyHash returns the hash of the policy in force (nil without a
	// policy). `orch approve` uses it to recognise a policy-change approval.
	PolicyHash func() string
	// Policy and Ops supply the read-only /policy and /ops views (nil:
	// both endpoints answer an empty object).
	Policy func() PolicyView
	Ops    func() OpsView
}

// StepProvider supplies the steps the machine runs.
type StepProvider interface {
	Steps() []machine.Step
}

// Daemon ties the state machine loop and the socket API together.
type Daemon struct {
	cfg     Config
	st      *store.Store
	m       *machine.Machine
	handler *handler
	ready   chan struct{}

	// tickMu/paused exist only so tests can stop the worker loop deterministically
	// (see pauseWorkers in the test files); production never sets paused.
	tickMu sync.RWMutex
	paused bool
}

// NewDaemon builds the machine from the provider and the API handler.
func NewDaemon(cfg Config, st *store.Store, p StepProvider) (*Daemon, error) {
	if cfg.Socket == "" {
		return nil, errors.New("socket path is required")
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	var opts []machine.Option
	if mo, ok := p.(interface{ MachineOptions() []machine.Option }); ok {
		opts = mo.MachineOptions()
	}
	m := machine.New(st, p.Steps(), opts...)
	h, err := newHandler(st, m, cfg.AllowedRepos, cfg.GateHome)
	if err != nil {
		return nil, err
	}
	h.policyHash = cfg.PolicyHash
	h.reportDir = cfg.ReportDir
	h.policy, h.ops = cfg.Policy, cfg.Ops
	return &Daemon{cfg: cfg, st: st, m: m, handler: h, ready: make(chan struct{})}, nil
}

// Ready is closed once the socket is listening and recovery is done.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Run serves until ctx is cancelled. On cancel the HTTP server drains and the
// workers stop; a step that was running sees its context cancelled, its result
// is dropped and its claim stays, so Recover re-runs it at the next start (it
// counts as one attempt).
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := listenUnix(d.cfg.Socket)
	if err != nil {
		return err
	}
	// Recover assumes a single orchd per database: it releases every claim.
	// Owning the socket is not enough (a second orchd may use another socket),
	// so also hold an exclusive lock on <db>.lock for the whole run.
	unlock, err := lockDB(d.st.Path())
	if err != nil {
		ln.Close()
		return err
	}
	defer unlock()
	if err := d.m.Recover(ctx); err != nil {
		ln.Close()
		return fmt.Errorf("recover: %w", err)
	}
	srv := &http.Server{Handler: d.handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	var wg sync.WaitGroup
	for i := 0; i < d.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.work(ctx) }()
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	close(d.ready)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	_ = ln.Close() // Shutdown may race Serve; make sure the socket file goes
	wg.Wait()
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return runErr
}

func (d *Daemon) work(ctx context.Context) {
	for ctx.Err() == nil {
		d.tickMu.RLock()
		var err error
		if d.paused {
			err = machine.ErrNoWork
		} else {
			err = d.m.Tick(ctx)
		}
		d.tickMu.RUnlock()
		if err == nil {
			continue
		}
		wait := d.cfg.PollInterval
		if !errors.Is(err, machine.ErrNoWork) {
			if ctx.Err() != nil {
				return
			}
			log.Printf("orchd: tick: %v", err)
			wait = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

// lockDB takes an exclusive, non-blocking flock on <db>.lock. The lock is
// released when the returned func runs or the process exits (also on a crash).
func lockDB(dbPath string) (func(), error) {
	path := dbPath + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open the database lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another orchd already runs on database %s (lock %s is held); refusing to start a second one", dbPath, path)
		}
		return nil, fmt.Errorf("cannot lock %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

var umaskMu sync.Mutex

// listenUnix creates the socket with mode 0600. It refuses a path that exists
// and is not a socket, and a socket something still answers on; a stale socket
// (nobody answers) is removed.
func listenUnix(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("socket path %q must be absolute", path)
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		_ = os.Chmod(dir, 0o700)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to touch it", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another orchd is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	umaskMu.Lock()
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
