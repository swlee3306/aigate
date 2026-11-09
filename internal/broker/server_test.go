package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func echoHandler(context.Context, json.RawMessage) (any, *OpError) {
	return map[string]any{"hello": "world"}, nil
}

func TestSocketServesOneRequestPerConnection(t *testing.T) {
	e := newTestEnv(t)
	s, _ := e.newServer(t, func(s *Server) { s.Handlers["echo"] = echoHandler })
	sock := e.Raw["socket"].(string)

	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o660 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode wrong: %v %v", fi, err)
	}
	_ = s

	// two requests in one connection: only the first is answered, then EOF.
	out := roundTrip(t, sock, []byte(`{"v":1,"op":"echo"}`+"\n"+`{"v":1,"op":"status"}`+"\n"))
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("got %d replies: %q", len(lines), out)
	}
	var r reply
	if err := json.Unmarshal(lines[0], &r); err != nil || !r.OK || !strings.Contains(string(r.Result), "world") {
		t.Fatalf("bad reply %q", out)
	}
	// the server keeps serving new connections
	r, _ = call(t, sock, `{"v":1,"op":"echo"}`)
	if !r.OK {
		t.Fatal("second connection not served")
	}
	// the connection is closed after the reply
	c, _ := net.Dial("unix", sock)
	defer c.Close()
	c.SetDeadline(time.Now().Add(testShortTimeout))
	c.Write([]byte(`{"v":1,"op":"echo"}` + "\n"))
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	if n == 0 {
		t.Fatal("no reply")
	}
	if _, err := c.Read(buf); err == nil {
		t.Fatal("connection stayed open after the reply")
	}
}

func TestForeignUIDRefused(t *testing.T) {
	e := newTestEnv(t)
	called := false
	e.newServer(t, func(s *Server) {
		s.PeerUID = func(net.Conn) (uint32, error) { return 4242, nil }
		s.AllowUID = func(uid uint32) bool { return uid == 1 }
		s.Handlers["echo"] = func(context.Context, json.RawMessage) (any, *OpError) { called = true; return nil, nil }
	})
	sock := e.Raw["socket"].(string)
	r, out := call(t, sock, `{"v":1,"op":"echo"}`)
	if r.OK || r.Code != "forbidden" || r.Message == "" {
		t.Fatalf("foreign uid not refused: %s", out)
	}
	if called {
		t.Fatal("handler ran for a foreign uid")
	}
	// a failing peer-credential lookup is a refusal too
	e2 := newTestEnv(t)
	e2.newServer(t, func(s *Server) {
		s.PeerUID = func(net.Conn) (uint32, error) { return 0, os.ErrInvalid }
	})
	r, _ = call(t, e2.Raw["socket"].(string), `{"v":1,"op":"status"}`)
	if r.Code != "forbidden" {
		t.Fatalf("uid lookup failure not refused: %+v", r)
	}
}

func TestAllowedUIDServedWithRealPeerCred(t *testing.T) {
	e := newTestEnv(t)
	e.newServer(t, nil) // default seams: real peercred, allow list from config (current user)
	r, out := call(t, e.Raw["socket"].(string), `{"v":1,"op":"status"}`)
	if !r.OK {
		t.Fatalf("own uid refused: %s", out)
	}
	e2 := newTestEnv(t)
	e2.newServer(t, func(s *Server) { s.AllowUID = nil; s.Config = withUIDs(s.Config, 4242) })
	r, _ = call(t, e2.Raw["socket"].(string), `{"v":1,"op":"status"}`)
	if r.Code != "forbidden" {
		t.Fatalf("uid outside the allow list served: %+v", r)
	}
}

func TestRequestSizeAndTimeoutLimits(t *testing.T) {
	e := newTestEnv(t)
	e.newServer(t, func(s *Server) { s.ReadTimeout = 200 * time.Millisecond })
	sock := e.Raw["socket"].(string)

	big := append([]byte(`{"v":1,"op":"status","args":"`), bytes.Repeat([]byte("a"), 600*1024)...)
	big = append(big, []byte(`"}`+"\n")...)
	out := roundTrip(t, sock, big)
	var r reply
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil || r.Code != "too_large" {
		t.Fatalf("oversize not refused: %q", out)
	}
	// without any newline, too
	out = roundTrip(t, sock, bytes.Repeat([]byte("a"), 600*1024))
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil || r.Code != "too_large" {
		t.Fatalf("oversize (no newline) not refused: %q", out)
	}
	// exactly at the limit is accepted (padding inside args)
	pad := MaxRequestBytes - len(`{"v":1,"op":"status","args":{"p":""}}`)
	ok := []byte(`{"v":1,"op":"status","args":{"p":"` + strings.Repeat("a", pad) + `"}}`)
	if len(ok) != MaxRequestBytes {
		t.Fatalf("test arithmetic: %d", len(ok))
	}
	if rr, out := call(t, sock, string(ok)); !rr.OK {
		t.Fatalf("limit-sized request refused: %.200s", out)
	}

	// slow client: a partial line and silence
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(testShortTimeout))
	c.Write([]byte(`{"v":1,`))
	start := time.Now()
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow client held the connection too long")
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf[:n]), &r); err != nil || r.Code != "timeout" {
		t.Fatalf("slow client reply %q", buf[:n])
	}
	// a client that sends nothing is cut off as well
	c2, _ := net.Dial("unix", sock)
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(testShortTimeout))
	n, _ = c2.Read(buf)
	if err := json.Unmarshal(bytes.TrimSpace(buf[:n]), &r); err != nil || r.Code != "timeout" {
		t.Fatalf("silent client reply %q", buf[:n])
	}
}

func TestMalformedRequestsFixedErrors(t *testing.T) {
	e := newTestEnv(t)
	e.newServer(t, nil)
	sock := e.Raw["socket"].(string)
	const marker = "REFLECT-ME-9f3a"
	cases := []struct {
		name, line, code string
	}{
		{"broken json", `{"v":1,"op":"status` + marker, "bad_request"},
		{"not an object", `[1,2,3]` + marker, "bad_request"},
		{"empty line", ``, "bad_request"},
		{"unknown op", `{"v":1,"op":"` + marker + `"}`, "unknown_op"},
		{"missing op", `{"v":1}`, "bad_request"},
		{"missing v", `{"op":"status"}`, "bad_request"},
		{"wrong v", `{"v":2,"op":"status"}`, "bad_request"},
		{"op wrong type", `{"v":1,"op":["` + marker + `"]}`, "bad_request"},
		{"unknown field", `{"v":1,"op":"status","` + marker + `":1}`, "bad_request"},
		{"trailing junk", `{"v":1,"op":"status"} ` + marker, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, out := call(t, sock, c.line)
			if r.OK || r.Code != c.code || r.Message == "" {
				t.Fatalf("got %s", out)
			}
			if strings.Contains(string(out), marker) {
				t.Fatalf("reply reflects input: %s", out)
			}
		})
	}
	// no newline then EOF
	c, _ := net.Dial("unix", sock)
	defer c.Close()
	c.SetDeadline(time.Now().Add(testShortTimeout))
	c.Write([]byte(`{"v":1,"op":"status"}`))
	c.(*net.UnixConn).CloseWrite()
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	var r reply
	if err := json.Unmarshal(bytes.TrimSpace(buf[:n]), &r); err != nil || r.Code != "bad_request" {
		t.Fatalf("unterminated request reply %q", buf[:n])
	}
}

func TestStatusIsReadOnlyAndSecretFree(t *testing.T) {
	e := newTestEnv(t)
	openai := newFakeHTTP(t, nil)
	gitlab := newFakeHTTP(t, nil)
	e.set(t, []string{"gitlab", "base_url"}, gitlab.URL)
	_ = openai
	s, _ := e.newServer(t, nil)
	sock := e.Raw["socket"].(string)

	before, _ := os.ReadFile(e.Dir + "/state/budget.json")
	r, out := call(t, sock, `{"v":1,"op":"status","args":{"ignored":true}}`)
	if !r.OK {
		t.Fatalf("status failed: %s", out)
	}
	for _, secret := range []string{testOpenAIKey, testGitLabToken, e.KeyFile, e.TokFile, "openai.key", "gitlab.token"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("status leaks %q: %s", secret, out)
		}
	}
	var res struct {
		ContextRepos     []string `json:"context_repos"`
		RefineRemaining  *int     `json:"refine_remaining_hour"`
		TokensRemaining  *int64   `json:"tokens_remaining_today"`
		GitLabConfigured *bool    `json:"gitlab_configured"`
		MRProjects       []string `json:"mr_projects"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.RefineRemaining == nil || *res.RefineRemaining != 3 || res.TokensRemaining == nil || res.GitLabConfigured == nil || !*res.GitLabConfigured ||
		len(res.ContextRepos) != 1 || len(res.MRProjects) != 1 {
		t.Fatalf("status shape wrong: %s", out)
	}
	// read-only: no limiter or budget movement, no outbound request, budget file untouched
	if s.State.Refine.Remaining() != 3 || s.State.Budget.RemainingMicros() != usd(25) {
		t.Fatal("status changed limiter or budget state")
	}
	call(t, sock, `{"v":1,"op":"status"}`)
	after, _ := os.ReadFile(e.Dir + "/state/budget.json")
	if !bytes.Equal(before, after) {
		t.Fatal("status wrote the budget file")
	}
	if openai.Hits() != 0 || gitlab.Hits() != 0 {
		t.Fatal("status made an outbound request")
	}
}

func TestPlaceholdersNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	e.newServer(t, nil)
	for _, op := range []string{"refine", "create_mr"} {
		r, out := call(t, e.Raw["socket"].(string), `{"v":1,"op":"`+op+`","args":{}}`)
		if r.OK || r.Code != "not_implemented" {
			t.Fatalf("%s: %s", op, out)
		}
	}
}

func TestPanicInHandlerIsFixedError(t *testing.T) {
	e := newTestEnv(t)
	_, logs := e.newServer(t, func(s *Server) {
		s.Handlers["boom"] = func(context.Context, json.RawMessage) (any, *OpError) {
			panic("exploded with sk-NOTREAL1234567890 and " + testOpenAIKey)
		}
		s.Handlers["echo"] = echoHandler
	})
	sock := e.Raw["socket"].(string)
	r, out := call(t, sock, `{"v":1,"op":"boom"}`)
	if r.OK || r.Code != "internal_error" || strings.Contains(string(out), "exploded") || strings.Contains(string(out), "sk-") {
		t.Fatalf("panic reply %s", out)
	}
	// the socket keeps working
	if r, _ := call(t, sock, `{"v":1,"op":"echo"}`); !r.OK {
		t.Fatal("server died after a handler panic")
	}
	l := logs.String()
	if !strings.Contains(l, "panic") {
		t.Fatalf("panic was not logged: %q", l)
	}
	if strings.Contains(l, "sk-NOTREAL") || strings.Contains(l, testOpenAIKey) {
		t.Fatalf("panic log leaks a key: %q", l)
	}
}

func TestHandlerTimeoutAndConcurrencyLimit(t *testing.T) {
	e := newTestEnv(t)
	release := make(chan struct{})
	e.newServer(t, func(s *Server) {
		s.OpTimeouts = map[string]time.Duration{"slow": 150 * time.Millisecond}
		s.Handlers["slow"] = func(ctx context.Context, _ json.RawMessage) (any, *OpError) {
			select {
			case <-ctx.Done():
			case <-release:
			}
			return nil, nil
		}
		s.Handlers["hold"] = func(ctx context.Context, _ json.RawMessage) (any, *OpError) {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return map[string]any{}, nil
		}
	})
	sock := e.Raw["socket"].(string)
	if r, _ := call(t, sock, `{"v":1,"op":"slow"}`); r.Code != "timeout" {
		t.Fatalf("handler timeout reply %+v", r)
	}
	time.Sleep(300 * time.Millisecond) // the timed-out request's slot is released after its bounded drain
	// MaxConcurrent (4) held connections; the fifth is refused with busy.
	var conns []net.Conn
	for i := 0; i < MaxConcurrent; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(`{"v":1,"op":"hold"}` + "\n"))
		conns = append(conns, c)
	}
	time.Sleep(200 * time.Millisecond)
	r, out := call(t, sock, `{"v":1,"op":"status"}`)
	if r.Code != "busy" {
		t.Fatalf("5th concurrent request: %s", out)
	}
	close(release)
	for _, c := range conns {
		c.Close()
	}
}

func TestNoSecretInLogs(t *testing.T) {
	e := newTestEnv(t)
	_, logs := e.newServer(t, func(s *Server) {
		s.ReadTimeout = 150 * time.Millisecond
		s.Handlers["leaky"] = func(context.Context, json.RawMessage) (any, *OpError) {
			return nil, &OpError{Code: "leaky_code", Message: "oops " + testOpenAIKey + " " + testGitLabToken}
		}
		s.Handlers["panicky"] = func(context.Context, json.RawMessage) (any, *OpError) {
			panic(testOpenAIKey + " / " + testGitLabToken)
		}
	})
	sock := e.Raw["socket"].(string)
	var replies bytes.Buffer
	for _, raw := range [][]byte{
		[]byte(`{"v":1,"op":"leaky"}` + "\n"),
		[]byte(`{"v":1,"op":"panicky"}` + "\n"),
		[]byte(`garbage ` + testOpenAIKey + "\n"),
		[]byte(`{"v":1,"op":"` + testOpenAIKey + `"}` + "\n"),
		[]byte(`{"v":1,"op":"status"` + testGitLabToken),
		bytes.Repeat([]byte("k"), 600*1024),
	} {
		replies.Write(roundTrip(t, sock, raw))
	}
	time.Sleep(100 * time.Millisecond)
	for _, secret := range []string{testOpenAIKey, testGitLabToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q:\n%s", secret, logs.String())
		}
		if strings.Contains(replies.String(), secret) {
			t.Fatalf("reply leaks %q", secret)
		}
	}
	if logs.String() == "" {
		t.Fatal("nothing was logged; the test checks nothing")
	}
}

func TestRedact(t *testing.T) {
	in := "a sk-abcdef123456 b glpat-xyzxyz123456 c Bearer abc.def d test-key-NOT-REAL e"
	got := Redact(in, []string{testOpenAIKey})
	for _, bad := range []string{"sk-abcdef", "glpat-xyz", "abc.def", testOpenAIKey} {
		if strings.Contains(got, bad) {
			t.Fatalf("not redacted %q in %q", bad, got)
		}
	}
	if !strings.Contains(got, "a ") || !strings.Contains(got, " e") {
		t.Fatalf("redaction mangled the rest: %q", got)
	}
}
