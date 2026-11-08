package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubServer(t *testing.T) string {
	t.Helper()
	dir, _ := os.MkdirTemp("/tmp", "orchr")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	const failedJSON = `{"id":%d,"repo":"/r","base":"b","prompt":"p","class":"c","agent":"claude","state":"failed","attempts":0,"retry_state":"verifying","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","error":"restore quota 413","retryable":true,"retries":1}`
	mux.HandleFunc("GET /api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[` + strings.Replace(failedJSON, "%d", "7", 1) + `,` +
			`{"id":8,"repo":"/r","base":"b","prompt":"p","class":"c","agent":"claude","state":"failed","attempts":0,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`))
	})
	mux.HandleFunc("GET /api/v1/tasks/7", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(failedJSON, "%d", "7", 1)))
	})
	mux.HandleFunc("POST /api/v1/tasks/7/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(strings.Replace(failedJSON, "%d", "7", 1), `"state":"failed"`, `"state":"verifying"`, 1)))
	})
	mux.HandleFunc("POST /api/v1/tasks/8/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		w.Write([]byte(`{"error":{"code":"conflict","message":"판정·승인 거절 등으로 실패한 작업은 다시 시도할 수 없습니다"}}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestCLIRetry(t *testing.T) {
	sock := stubServer(t)
	do := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := run(append([]string{"--socket", sock}, args...), &o, &e)
		return code, o.String(), e.String()
	}
	if code, out, errS := do("retry", "7"); code != 0 || out != "작업 #7 다시 시도: verifying부터\n" {
		t.Fatalf("retry %d %q %q", code, out, errS)
	}
	if code, out, errS := do("retry", "8"); code != 1 || out != "" || !strings.Contains(errS, "다시 시도할 수 없습니다") {
		t.Fatalf("refused %d %q %q", code, out, errS)
	}
	if code, _, errS := do("retry"); code != 1 || !strings.Contains(errS, "작업 ID") {
		t.Fatalf("no id %d %q", code, errS)
	}
	code, out, _ := do("--json", "retry", "7")
	if code != 0 || !strings.Contains(out, `"state":"verifying"`) {
		t.Fatalf("json %d %q", code, out)
	}
	// list/show mark retryable tasks
	code, out, _ = do("list")
	lines := strings.Split(out, "\n")
	if code != 0 || !strings.Contains(lines[1], "failed") || !strings.Contains(lines[1], "재시도 가능") || strings.Contains(lines[2], "재시도 가능") {
		t.Fatalf("list %d %q", code, out)
	}
	code, out, _ = do("show", "7")
	if code != 0 || !strings.Contains(out, "재시도:") || !strings.Contains(out, "orch retry 7") || !strings.Contains(out, "1/3") {
		t.Fatalf("show %d %q", code, out)
	}
}
