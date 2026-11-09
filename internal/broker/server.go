package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Protocol limits (decision D1).
const (
	MaxRequestBytes = 512 * 1024
	MaxConcurrent   = 4
	DefaultReadTime = 5 * time.Second
	writeTimeout    = 5 * time.Second
)

// HandlerFunc serves one op. args is the request's raw "args" (may be nil).
// Handlers must honor ctx: the server answers `timeout` when it ends, but
// cannot stop a handler that ignores it.
type HandlerFunc func(ctx context.Context, args json.RawMessage) (any, *OpError)

// OpError is a handler failure. Code must be registered in fixedMessages; an
// unregistered code is sent as internal_error. The wire message is always the
// server's fixed sentence for the code: Message is never sent or logged.
type OpError struct {
	Code    string
	Message string
}

var fixedMessages = map[string]string{
	"forbidden":       "허용되지 않은 호출자입니다.",
	"bad_request":     "요청 형식이 올바르지 않습니다.",
	"unknown_op":      "지원하지 않는 작업입니다.",
	"too_large":       "요청이 너무 큽니다.",
	"timeout":         "요청 시간이 초과되었습니다.",
	"busy":            "처리 중인 요청이 너무 많습니다.",
	"internal_error":  "내부 오류가 발생했습니다.",
	"not_implemented": "아직 구현되지 않은 작업입니다.",
	"budget_exceeded": "AI 사용 예산 또는 호출 한도를 초과했습니다.",

	"repo_not_allowed":  "이 저장소는 AI 전송이 허용되어 있지 않습니다.",
	"bad_file":          "보낼 파일 경로나 내용이 올바르지 않습니다.",
	"looks_like_secret": "비밀 값처럼 보이는 내용이 있어 보낼 수 없습니다.",
	"rate_limited":      "AI 호출 횟수 한도를 초과했습니다. 잠시 후 다시 시도하세요.",
	"upstream_error":    "AI 서비스 호출에 실패했습니다.",
	"bad_plan":          "AI 응답이 계획서 형식을 지키지 않았습니다.",
}

// Default per-op processing time limits (decision D1).
var defaultOpTimeouts = map[string]time.Duration{
	"refine":    60 * time.Second,
	"create_mr": 20 * time.Second,
	"status":    5 * time.Second,
}

// State is the broker's mutable limit state.
type State struct {
	Refine   *HourlyLimiter
	CreateMR *HourlyLimiter
	Budget   *Budget
}

// Server is the broker socket server. Zero-valued tuning fields take their
// defaults. PeerUID and AllowUID are test seams.
type Server struct {
	Config   Config
	Handlers map[string]HandlerFunc
	State    State
	Secrets  Secrets
	OpenAI   *OpenAIClient // set by New; tests may point BaseURL at a fake

	PeerUID     func(net.Conn) (uint32, error) // nil: kernel peer credentials
	AllowUID    func(uint32) bool              // nil: Config.UIDs()
	LookupGroup func(name string) (int, error) // nil: os/user
	Log         io.Writer                      // nil: discard
	ReadTimeout time.Duration                  // 0: 5s
	OpTimeouts  map[string]time.Duration       // overrides per op

	logOnce sync.Once
	logger  *log.Logger
	sem     chan struct{}
}

// Options configures New.
type Options struct {
	Clock      func() time.Time
	StateDir   string // holds budget.json; "" = /var/lib/orch-broker
	Log        io.Writer
	KeyOwnerOK func(uint32) bool // nil: owned by the current euid
}

// DefaultStateDir is where budget.json lives.
const DefaultStateDir = "/var/lib/orch-broker"

// New builds a server: it loads the secrets (refusing unsafe key files),
// opens the budget, and registers `status` plus the `not_implemented`
// placeholders of refine and create_mr.
func New(cfg Config, o Options) (*Server, error) {
	sec, err := LoadSecrets(cfg, o.KeyOwnerOK)
	if err != nil {
		return nil, err
	}
	dir := o.StateDir
	if dir == "" {
		dir = DefaultStateDir
	}
	bud, err := OpenBudget(filepath.Join(dir, "budget.json"), Micros(cfg.Limits.WeeklyBudgetUSD), o.Clock)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Config:  cfg,
		Secrets: sec,
		OpenAI:  &OpenAIClient{Key: sec.OpenAIKey(), Model: cfg.OpenAI.Model},
		Log:     o.Log,
		State: State{
			Refine:   NewHourlyLimiter(cfg.Limits.RefinePerHour, o.Clock),
			CreateMR: NewHourlyLimiter(cfg.Limits.CreateMRPerHour, o.Clock),
			Budget:   bud,
		},
	}
	bud.OnPersistError = func() { s.logf("예산 상태 저장 실패: 요청을 거부합니다") }
	s.Handlers = map[string]HandlerFunc{
		"status":    s.statusHandler,
		"refine":    s.refineHandler,
		"create_mr": notImplemented,
	}
	return s, nil
}

func notImplemented(context.Context, json.RawMessage) (any, *OpError) {
	return nil, &OpError{Code: "not_implemented"}
}

// statusHandler is read-only and secret-free (decision D3).
func (s *Server) statusHandler(context.Context, json.RawMessage) (any, *OpError) {
	repos := append([]string{}, s.Config.ContextRepos...)
	projects := make([]string, 0, len(s.Config.GitLab.Projects))
	for repo := range s.Config.GitLab.Projects {
		projects = append(projects, repo)
	}
	sortStrings(projects)
	remaining := s.State.Budget.RemainingMicros()
	return map[string]any{
		"context_repos":               repos,
		"refine_remaining_hour":       s.State.Refine.Remaining(),
		"gitlab_configured":           s.Config.GitLab.Configured(),
		"mr_projects":                 projects,
		"weekly_budget_remaining_usd": float64(remaining) / 1e6,
	}, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (s *Server) setup() {
	w := s.Log
	if w == nil {
		w = io.Discard
	}
	s.logger = log.New(&redactWriter{w: w, secrets: s.Secrets.values()}, "orch-broker: ", log.LstdFlags)
	s.sem = make(chan struct{}, MaxConcurrent)
}

func (s *Server) logf(format string, a ...any) {
	s.init()
	s.logger.Printf(format, a...)
}

// init prepares the logger and the concurrency semaphore.
func (s *Server) init() { s.logOnce.Do(s.setup) }

// Listen creates the unix socket: stale socket removed, group set, mode 0660.
func (s *Server) Listen() (net.Listener, error) {
	path := s.Config.Socket
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("소켓 경로에 소켓이 아닌 파일이 있습니다")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("이전 소켓을 지울 수 없습니다")
		}
	}
	lookup := s.LookupGroup
	if lookup == nil {
		lookup = func(name string) (int, error) {
			g, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(g.Gid)
		}
	}
	gid, err := lookup(s.Config.SocketGroup)
	if err != nil {
		return nil, errors.New("socket_group을 확인할 수 없습니다")
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("소켓을 열 수 없습니다")
	}
	// Until the chmod below the socket has the umask-derived mode; with a
	// normal umask nobody but the owner may connect, and the uid check
	// applies in any case.
	if err := os.Chown(path, -1, gid); err != nil {
		ln.Close()
		return nil, errors.New("소켓 그룹을 설정할 수 없습니다")
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, errors.New("소켓 권한을 설정할 수 없습니다")
	}
	return ln, nil
}

// Serve accepts connections until ctx ends or ln fails, then waits for
// in-flight requests.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.init()
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case s.sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-s.sem }()
				s.handle(ctx, c)
			}()
		default:
			s.reply(c, nil, "busy", "")
			c.Close()
		}
	}
}

func (s *Server) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return DefaultReadTime
}

func (s *Server) allowed(uid uint32) bool {
	if s.AllowUID != nil {
		return s.AllowUID(uid)
	}
	for _, u := range s.Config.uids {
		if u == uid {
			return true
		}
	}
	return false
}

type request struct {
	V    int             `json:"v"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	defer func() {
		if recover() != nil { // never log the value: it could hold request text
			s.logf("connection handler panic")
			s.reply(c, nil, "internal_error", "")
		}
	}()
	peer := s.PeerUID
	if peer == nil {
		peer = peerCredUID
	}
	uid, err := peer(c)
	if err != nil || !s.allowed(uid) {
		s.logf("refused: peer not allowed")
		s.reply(c, nil, "forbidden", "")
		return
	}
	line, code := readLine(c, s.readTimeout())
	if code != "" {
		s.logf("request failed: code=%s", code)
		s.reply(c, nil, code, "")
		drain(c)
		return
	}
	var req request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.V != 1 || req.Op == "" {
		s.logf("request failed: code=bad_request")
		s.reply(c, nil, "bad_request", "")
		drain(c)
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		s.logf("request failed: code=bad_request")
		s.reply(c, nil, "bad_request", "")
		drain(c)
		return
	}
	h, ok := s.Handlers[req.Op]
	if !ok {
		s.logf("request failed: code=unknown_op")
		s.reply(c, nil, "unknown_op", "")
		drain(c)
		return
	}
	start := time.Now()
	res, oerr := s.run(ctx, req.Op, h, req.Args)
	if oerr != nil {
		s.logf("op=%s failed: code=%s dur=%s", req.Op, safeCode(oerr.Code), time.Since(start).Round(time.Millisecond))
		s.reply(c, nil, oerr.Code, oerr.Message)
	} else {
		s.logf("op=%s ok dur=%s", req.Op, time.Since(start).Round(time.Millisecond))
		s.reply(c, res, "", "")
	}
	drain(c)
}

func safeCode(code string) string {
	if _, ok := fixedMessages[code]; ok {
		return code
	}
	return "internal_error"
}

// run calls the handler with the op's time limit; a panic or a timeout
// becomes a fixed error and the server keeps running.
func (s *Server) run(ctx context.Context, op string, h HandlerFunc, args json.RawMessage) (any, *OpError) {
	d, ok := s.OpTimeouts[op]
	if !ok {
		d = defaultOpTimeouts[op]
	}
	if d <= 0 {
		d = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type out struct {
		res any
		err *OpError
	}
	ch := make(chan out, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logf("handler panic: op=%s value=%s", op, fmt.Sprint(r))
				ch <- out{nil, &OpError{Code: "internal_error"}}
			}
		}()
		res, err := h(ctx, args)
		ch <- out{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-ctx.Done():
		return nil, &OpError{Code: "timeout"}
	}
}

// readLine reads one newline-terminated request, at most MaxRequestBytes
// bytes (newline excluded), within timeout overall. A non-empty code is a
// fixed failure code.
func readLine(c net.Conn, timeout time.Duration) ([]byte, string) {
	c.SetReadDeadline(time.Now().Add(timeout))
	br := bufio.NewReaderSize(c, 32*1024)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxRequestBytes+1 || (len(buf) == MaxRequestBytes+1 && buf[len(buf)-1] != '\n') {
			return nil, "too_large"
		}
		switch {
		case err == nil:
			line := bytes.TrimSuffix(buf, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			return line, ""
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case isTimeout(err):
			return nil, "timeout"
		default: // EOF or reset before a newline
			return nil, "bad_request"
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// drain lets the peer finish writing (briefly, bounded) so closing the
// socket does not reset a reply the peer has not read yet.
func drain(c net.Conn) {
	if uc, ok := c.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	io.Copy(io.Discard, io.LimitReader(c, 2*MaxRequestBytes))
}

// reply writes one JSON response; failures carry the registered fixed
// sentence of their code (unregistered codes become internal_error).
func (s *Server) reply(c net.Conn, result any, code, msg string) {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	var body []byte
	if code == "" {
		b, err := json.Marshal(map[string]any{"ok": true, "result": result})
		if err != nil {
			code = "internal_error"
		} else {
			body = b
		}
	}
	if code != "" {
		fixed, ok := fixedMessages[code]
		if !ok {
			code, fixed = "internal_error", fixedMessages["internal_error"]
		}
		msg = fixed
		body, _ = json.Marshal(map[string]any{"ok": false, "code": code, "message": msg})
	}
	c.Write(append(body, '\n'))
}

var (
	keyShapeRE = regexp.MustCompile(`(?:sk|glpat|ghp|gho)-[A-Za-z0-9_\-]{6,}`)
	bearerRE   = regexp.MustCompile(`(?i)bearer\s+\S+`)
)

// Redact removes the known secrets and key-shaped strings from s.
func Redact(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = keyShapeRE.ReplaceAllString(s, "[redacted]")
	return bearerRE.ReplaceAllString(s, "Bearer [redacted]")
}

// redactWriter scrubs every log line before it reaches the sink.
type redactWriter struct {
	mu      sync.Mutex
	w       io.Writer
	secrets []string
}

func (r *redactWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := io.WriteString(r.w, Redact(string(p), r.secrets))
	return len(p), err
}
