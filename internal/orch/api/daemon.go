package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"aigate/internal/orch/broker"
	"aigate/internal/orch/machine"
	"aigate/internal/orch/store"
	"aigate/internal/orch/web"
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
	// CloudStatus supplies ops.cloud (quota and test VM status). It must be
	// cached and bounded by the caller (the vm package does both); nil omits
	// the "cloud" key from /ops.
	CloudStatus func(ctx context.Context) CloudView
	// SocketUIDs are the uids whose processes may use the unix socket (read
	// and general write), checked with the kernel's peer credentials on every
	// connection. The daemon process's own uid is always added (CF9); root is
	// not implied. 4294967295 (the "-1" uid) is refused.
	SocketUIDs []uint32
	// Auth resolves HTTP bearer tokens and session cookies (nil: the store's
	// sessions table, StoreAuthenticator).
	Auth Authenticator
	// RPID and Origin are the WebAuthn relying party ID and the exact web
	// console origin (ValidateWebAuthnConfig); both empty turns passkey
	// enrollment off. They are recorded in every pending credential and
	// copied into the signer entry at activation (R8).
	RPID, Origin string
	// PendingFile is the pending passkey enrollment file (R18; empty:
	// DefaultPendingFile, the default of `orch admin signers --pending-file`).
	PendingFile string
	// HTTPListen is the loopback address of the HTTP listener ("" = off),
	// e.g. 127.0.0.1:8808. Only 127.0.0.0/8 and ::1 are accepted
	// (ResolveLoopbackListen); it must be reached only through
	// `tailscale serve`. It requires RPID, Origin and AllowedHosts.
	HTTPListen string
	// AllowedHosts is the Host header allowlist of the HTTP listener (port
	// ignored, case-insensitive); it must include Origin's host.
	AllowedHosts []string
	// SessionTTL is the idle expiry of a browser session (0: SessionIdleTTL;
	// MinSessionTTL..MaxSessionTTL). The absolute cap SessionMaxLifetime stays.
	SessionTTL time.Duration
	// QueueLimit caps the tasks ONE principal has waiting in StateQueued; a
	// submit beyond it is refused with 429 queue_full (0: DefaultQueueLimit;
	// negative refused). QueueLimitTotal caps all waiting tasks (0:
	// DefaultQueueLimitTotal; negative refused; below the effective
	// QueueLimit refused).
	QueueLimit      int
	QueueLimitTotal int
	// BrokerSocket is the orch-broker unix socket, the only path to the AI
	// plan service (absolute and clean; empty = the AI plan feature is off
	// and every refine call is 503 broker_unavailable).
	BrokerSocket string
}

const (
	// purgeEvery is how often expired sessions, challenges and idempotency
	// keys are deleted (also once at startup; CF8/CF13).
	purgeEvery = 5 * time.Minute
	// shutdownTimeout bounds the drain of in-flight requests at shutdown.
	shutdownTimeout = 5 * time.Second
)

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

	// HTTP listener (httpBind "" = off): the resolved loopback address, the
	// Host allowlist, the auth rate limiter and the bound address once Run
	// listens.
	httpBind     string
	allowedHosts map[string]bool
	limiter      *rateLimiter
	httpAddr     net.Addr

	// purgeEvery/shutdownTimeout are the constants above; tests shorten them.
	// purgeHook, when set, sees every purge result (tests).
	purgeEvery      time.Duration
	shutdownTimeout time.Duration
	purgeHook       func(n int64, err error)

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
	if err := ValidateWebAuthnConfig(cfg.RPID, cfg.Origin); err != nil {
		return nil, err
	}
	bind, hosts, err := ValidateHTTPConfig(context.Background(), cfg.HTTPListen, cfg.AllowedHosts, cfg.RPID, cfg.Origin)
	if err != nil {
		return nil, err
	}
	if err := ValidateSessionTTL(cfg.SessionTTL); err != nil {
		return nil, err
	}
	if err := ValidateSocketUIDs(cfg.SocketUIDs); err != nil {
		return nil, err
	}
	if cfg.QueueLimit < 0 || cfg.QueueLimitTotal < 0 {
		return nil, errors.New("queue limits must be >= 0 (0 = default)")
	}
	if cfg.BrokerSocket != "" && !broker.ValidSocketPath(cfg.BrokerSocket) {
		return nil, errors.New("broker socket must be an absolute clean path")
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
	if cfg.BrokerSocket != "" {
		h.broker = &broker.Client{Socket: cfg.BrokerSocket}
	}
	if sr, ok := p.(interface {
		RecordSubmitted(context.Context, store.Task)
	}); ok {
		h.onSubmitted = sr.RecordSubmitted
	}
	h.reportDir = cfg.ReportDir
	h.policy, h.ops, h.cloud = cfg.Policy, cfg.Ops, cfg.CloudStatus
	h.socketUIDs = socketUIDSet(cfg.SocketUIDs)
	h.auth = cfg.Auth
	h.rpID, h.origin = cfg.RPID, cfg.Origin
	if cfg.PendingFile != "" {
		h.pendingFile = cfg.PendingFile
	}
	if cfg.SessionTTL > 0 {
		h.sessionTTL = cfg.SessionTTL
	}
	if cfg.QueueLimit > 0 {
		h.queueLimit = cfg.QueueLimit
	}
	if cfg.QueueLimitTotal > 0 {
		h.queueLimitTotal = cfg.QueueLimitTotal
	}
	if h.queueLimitTotal < h.queueLimit {
		return nil, errors.New("queue limit total must be >= the per-principal queue limit")
	}
	if h.auth == nil {
		h.auth = &StoreAuthenticator{St: st, IdleTTL: h.sessionTTL}
	}
	log.Printf("orchd: socket uids allowed: %v", sortedUIDs(h.socketUIDs))
	return &Daemon{cfg: cfg, st: st, m: m, handler: h, ready: make(chan struct{}),
		httpBind: bind, allowedHosts: hosts,
		limiter:    newRateLimiter(DefaultAuthRatePerKey, DefaultAuthRateGlobal, DefaultRateKeys),
		purgeEvery: purgeEvery, shutdownTimeout: shutdownTimeout}, nil
}

// ValidateSocketUIDs refuses uids no process can have: 4294967295 is (uid_t)-1,
// the "no change" value of setuid/chown.
func ValidateSocketUIDs(uids []uint32) error {
	for _, u := range uids {
		if u == math.MaxUint32 {
			return fmt.Errorf("--socket-uid %d는 올바른 uid가 아닙니다", u)
		}
	}
	return nil
}

func sortedUIDs(set map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HTTPAddr is the bound address of the HTTP listener (nil when it is off or
// before Ready).
func (d *Daemon) HTTPAddr() net.Addr { return d.httpAddr }

// httpHandler is the handler stack of the HTTP listener (see secure.go for the
// order). The socket serves socketHandler.
func (d *Daemon) httpHandler() http.Handler {
	h := splitWeb(d.handler, web.Handler())
	h = rateLimit(d.limiter, d.handler.credentialRateKey, h)
	h = jsonOnly(h)
	h = limitBody(MaxBodyBytes, h)
	h = originCheck(d.cfg.Origin, h)
	h = noCORS(h)
	h = hostCheck(d.allowedHosts, h)
	return secureHeaders(h)
}

// splitWeb sends /api and /api/... to the API (auth, JSON errors) and
// everything else to the embedded console. The split is by path prefix, not a
// mux fallback, because the API handler answers every path (its root route is
// "/" behind authentication), so the console must be chosen before it. Being
// the innermost layer, the console inherits the whole chain of httpHandler:
// Host check, security headers and CSP.
func splitWeb(api, console http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		console.ServeHTTP(w, r)
	})
}

// socketHandler keeps the socket as it was (trusted by peer uid): only the
// body cap is added.
func (d *Daemon) socketHandler() http.Handler { return limitBody(MaxBodyBytes, d.handler) }

// purge deletes expired auth rows once.
func (d *Daemon) purge(ctx context.Context) {
	n, err := d.st.PurgeExpired(ctx)
	if err != nil && ctx.Err() == nil {
		log.Printf("orchd: purge expired sessions/challenges: %v", err)
	}
	if d.purgeHook != nil {
		d.purgeHook(n, err)
	}
}

// purgeLoop purges every purgeEvery until ctx ends.
func (d *Daemon) purgeLoop(ctx context.Context) {
	t := time.NewTicker(d.purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.purge(ctx)
		}
	}
}

// listenLoopback binds the resolved HTTP address and double-checks that the
// kernel bound a loopback address.
func listenLoopback(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("HTTP 리스너를 열 수 없습니다(%s): %w", addr, err)
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || !ta.IP.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("HTTP 리스너가 루프백이 아닌 주소 %s에 바인딩되어 닫았습니다", ln.Addr())
	}
	return ln, nil
}

// Ready is closed once the socket is listening and recovery is done.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Run serves until ctx is cancelled. On cancel both listeners stop accepting,
// in-flight requests drain (up to shutdownTimeout, then their connections are
// closed) and the workers stop; a step that was running sees its context
// cancelled, its result is dropped and its claim stays, so Recover re-runs it
// at the next start (it counts as one attempt).
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
	// Expired auth rows go before any listener answers, then periodically.
	d.purge(ctx)
	var hln net.Listener
	if d.httpBind != "" {
		if hln, err = listenLoopback(d.httpBind); err != nil {
			ln.Close()
			return err
		}
		d.httpAddr = hln.Addr()
		log.Printf("orchd: HTTP listener on %s (allowed hosts %v, origin %s); reach it only through tailscale serve",
			hln.Addr(), sortedKeys(d.allowedHosts), d.cfg.Origin)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	servers := []*http.Server{{Handler: d.socketHandler(), ConnContext: ConnContext,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}}
	listeners := []net.Listener{ln}
	if hln != nil {
		servers = append(servers, &http.Server{Handler: d.httpHandler(), ConnContext: ConnContext,
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute,
			IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10})
		listeners = append(listeners, hln)
	}
	var wg sync.WaitGroup
	for i := 0; i < d.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.work(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.purgeLoop(ctx) }()
	serveErr := make(chan error, len(servers))
	for i, srv := range servers {
		go func(srv *http.Server, l net.Listener) { serveErr <- srv.Serve(l) }(srv, listeners[i])
	}
	close(d.ready)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer scancel()
	var sw sync.WaitGroup
	for _, srv := range servers {
		sw.Add(1)
		go func(srv *http.Server) {
			defer sw.Done()
			if err := srv.Shutdown(sctx); err != nil {
				_ = srv.Close() // deadline passed: drop the remaining connections
			}
		}(srv)
	}
	sw.Wait()
	for _, l := range listeners {
		_ = l.Close() // Shutdown may race Serve; make sure the socket file goes
	}
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
