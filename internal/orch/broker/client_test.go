package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/refinesize"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "bk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	r, _ := filepath.EvalSymlinks(d)
	return r
}

// serve starts a unix socket server; h gets each connection (one request).
func serve(t *testing.T, h func(c net.Conn, line string)) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				h(c, line)
			}()
		}
	}()
	return sock
}

func TestBrokerClientUnavailableIsFixedError(t *testing.T) {
	dir := shortDir(t)
	hang := serve(t, func(c net.Conn, _ string) { time.Sleep(3 * time.Second) })
	cases := map[string]*Client{
		"empty socket":   {Socket: ""},
		"nil client":     nil,
		"relative":       {Socket: "broker.sock"},
		"missing socket": {Socket: filepath.Join(dir, "none.sock")},
		"not a socket":   {Socket: dir},
		"hangs":          {Socket: hang, Timeout: 150 * time.Millisecond},
		"closes at once": {Socket: serve(t, func(c net.Conn, _ string) {})},
		"garbage reply":  {Socket: serve(t, func(c net.Conn, _ string) { c.Write([]byte("not json at all\n")) })},
		"no newline":     {Socket: serve(t, func(c net.Conn, _ string) { c.Write([]byte(`{"ok":true,"result":{}}`)) })},
		"trailing junk":  {Socket: serve(t, func(c net.Conn, _ string) { c.Write([]byte("{\"ok\":true,\"result\":{}} x\n")) })},
		"result not object": {Socket: serve(t, func(c net.Conn, _ string) {
			c.Write([]byte("{\"ok\":true,\"result\":\"/etc/passwd\"}\n"))
		})},
		"odd code": {Socket: serve(t, func(c net.Conn, _ string) {
			c.Write([]byte("{\"ok\":false,\"code\":\"Hostile\\nText http://x\",\"message\":\"boom\"}\n"))
		})},
		"huge reply": {Socket: serve(t, func(c net.Conn, _ string) {
			c.Write([]byte(strings.Repeat("a", MaxReplyBytes+10) + "\n"))
		})},
	}
	for name, c := range cases {
		res, err := c.Do(context.Background(), "status", map[string]any{})
		if err == nil || res != nil {
			t.Errorf("%s: got res=%s err=%v", name, res, err)
			continue
		}
		if err.Code != CodeUnavailable || err.Error() != "broker: broker_unavailable" {
			t.Errorf("%s: code %q / %q", name, err.Code, err.Error())
		}
		if c != nil && c.Socket != "" && strings.Contains(err.Error(), c.Socket) {
			t.Errorf("%s: error names the socket", name)
		}
	}
	// ctx cancellation ends a call quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&Client{Socket: hang}).Do(ctx, "status", nil); err == nil || err.Code != CodeUnavailable || time.Since(start) > time.Second {
		t.Fatalf("ctx: %v after %s", err, time.Since(start))
	}
}

func TestBrokerClientProtocol(t *testing.T) {
	var got string
	sock := serve(t, func(c net.Conn, line string) {
		got = line
		c.Write([]byte("{\"ok\":true,\"result\":{\"context_repos\":[\"/r\"]}}\n"))
	})
	res, err := (&Client{Socket: sock}).Do(context.Background(), "status", map[string]any{})
	if err != nil || !strings.Contains(string(res), "/r") {
		t.Fatalf("%s %v", res, err)
	}
	if got != "{\"v\":1,\"op\":\"status\",\"args\":{}}\n" {
		t.Fatalf("request %q", got)
	}
	// A broker failure passes only its code; the message is never read.
	fail := serve(t, func(c net.Conn, _ string) {
		c.Write([]byte("{\"ok\":false,\"code\":\"repo_not_allowed\",\"message\":\"SECRET sk-abc\"}\n"))
	})
	_, e := (&Client{Socket: fail}).Do(context.Background(), "refine", nil)
	if e == nil || e.Code != "repo_not_allowed" || strings.Contains(e.Error(), "SECRET") {
		t.Fatalf("%v", e)
	}
	// Too large a request is refused before connecting.
	_, e = (&Client{Socket: sock}).Do(context.Background(), "refine", map[string]string{"x": strings.Repeat("a", MaxRequestBytes)})
	if e == nil || e.Code != CodeTooLarge {
		t.Fatalf("%v", e)
	}
	var m map[string]any
	if json.Unmarshal([]byte(got), &m) != nil {
		t.Fatal("request not JSON")
	}
}

func TestBrokerClientDoesNotHTMLEscape(t *testing.T) {
	var got string
	sock := serve(t, func(c net.Conn, line string) {
		got = line
		c.Write([]byte("{\"ok\":true,\"result\":{}}\n"))
	})
	content := strings.Repeat("<a&b>", 48*1024) // 240 KiB of characters Go escapes by default
	args := map[string]any{"files": []map[string]string{{"path": "x.html", "content": content}}}
	if _, err := (&Client{Socket: sock}).Do(context.Background(), "refine", args); err != nil {
		t.Fatalf("%v", err)
	}
	if strings.Contains(got, "\\u003c") || strings.Contains(got, "\\u0026") || len(got) >= MaxRequestBytes || !strings.HasSuffix(got, "}\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("request %d bytes, escaped=%v", len(got), strings.Contains(got, "\\u003c"))
	}
	b, err := Encode("refine", args)
	if err != nil || len(b) != len(got)-1 || len(b) >= MaxRequestBytes {
		t.Fatalf("Encode %d vs %d: %v", len(b), len(got), err)
	}
}

func TestRequestBytesMatchesEncode(t *testing.T) {
	for _, c := range []struct {
		repo, req string
		files     []refinesize.File
	}{
		{"/r", "x", nil},
		{"/home/orch/repos/toy", "한국어 <요구사항> & \"따옴표\"\n줄", []refinesize.File{{Path: "a/b.go", Content: "package x // <&>\n\t\x01 "}, {Path: "한글.md", Content: "\\ \" é 🙂"}}},
	} {
		sel := strings.Repeat("0", 32)
		fs := make([]map[string]string, len(c.files))
		for i, f := range c.files {
			fs[i] = map[string]string{"path": f.Path, "content": f.Content}
		}
		enc, err := Encode("refine", map[string]any{"repo": c.repo, "requirement": c.req, "files": fs, "selection": sel})
		if err != nil {
			t.Fatal(err)
		}
		if got := refinesize.RequestBytes(c.repo, c.req, sel, c.files); got != len(enc) {
			t.Fatalf("RequestBytes %d, Encode %d", got, len(enc))
		}
	}
}

func TestBrokerClientUnsentFlag(t *testing.T) {
	dir := shortDir(t)
	big := map[string]any{"x": strings.Repeat("a", MaxRequestBytes)}
	for name, tc := range map[string]struct {
		c      *Client
		op     string
		args   any
		unsent bool
	}{
		"no socket configured": {&Client{}, "status", nil, true},
		"nil client":           {nil, "status", nil, true},
		"dial failure":         {&Client{Socket: filepath.Join(dir, "none.sock")}, "status", nil, true},
		"request too large":    {&Client{Socket: serve(t, func(net.Conn, string) {})}, "refine", big, true},
		"unencodable":          {&Client{Socket: serve(t, func(net.Conn, string) {})}, "refine", map[string]any{"f": func() {}}, true},
		// once the request line was written the broker may be working on it
		"closed after the request": {&Client{Socket: serve(t, func(net.Conn, string) {})}, "status", nil, false},
		"garbage reply":            {&Client{Socket: serve(t, func(c net.Conn, _ string) { c.Write([]byte("nope\n")) })}, "status", nil, false},
		"timeout while waiting":    {&Client{Socket: serve(t, func(c net.Conn, _ string) { time.Sleep(2 * time.Second) }), Timeout: 150 * time.Millisecond}, "status", nil, false},
		"broker error code":        {&Client{Socket: serve(t, func(c net.Conn, _ string) { c.Write([]byte("{\"ok\":false,\"code\":\"busy\"}\n")) })}, "status", nil, false},
	} {
		_, err := tc.c.Do(context.Background(), tc.op, tc.args)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if err.Unsent != tc.unsent {
			t.Errorf("%s: Unsent=%v, want %v (code %s)", name, err.Unsent, tc.unsent, err.Code)
		}
	}
	// a cancelled caller context after the write is still "sent"
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	c := &Client{Socket: serve(t, func(c net.Conn, _ string) { close(started); time.Sleep(2 * time.Second) })}
	go func() { <-started; cancel() }()
	if _, err := c.Do(ctx, "status", nil); err == nil || err.Unsent {
		t.Errorf("cancel after write: %+v", err)
	}
}
