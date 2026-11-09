package web_test

import (
	stdjson "encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigate/internal/orch/api"
	"aigate/internal/orch/web"
)

// TestDevServer is a manual aid, off by default: ORCH_WEB_DEV=<seconds> serves
// the console on 127.0.0.1:ORCH_WEB_PORT (default 18808) with the real CSP and
// MOCKED API fixtures (no auth, no Host check), so the page can be looked at in
// a browser. It is not a security test.
func TestDevServer(t *testing.T) {
	secs, _ := strconv.Atoi(os.Getenv("ORCH_WEB_DEV"))
	if secs <= 0 {
		t.Skip("set ORCH_WEB_DEV=<seconds> to run the mocked dev server")
	}
	port := os.Getenv("ORCH_WEB_PORT")
	if port == "" {
		port = "18808"
	}
	loggedOut := os.Getenv("ORCH_WEB_LOGGEDOUT") != ""
	// Task views use the golden fixtures (marshalled from the real API types by
	// api.TestWebFixturesGolden) plus a hostile row, so what is looked at is
	// what the JS tests check.
	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("test", "fixtures", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	json := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(s))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/session", func(w http.ResponseWriter, r *http.Request) {
		if loggedOut {
			w.WriteHeader(401)
			json(w, `{"error":{"code":"unauthenticated","message":"인증이 필요합니다"}}`)
			return
		}
		json(w, `{"principal":"phone","kind":"session","csrf":"dev-csrf","expires":"2026-11-10T00:00:00Z"}`)
	})
	mux.HandleFunc("/api/v1/stages", func(w http.ResponseWriter, r *http.Request) {
		json(w, `[{"id":"design","name":"설계","status":"⬜","components":[],"next":"요구사항·설계서는 아직 Claude 세션에서 작성합니다 (W4에서 웹으로)"},
{"id":"implement","name":"구현","status":"🔄","components":["오케스트레이터 ②","에이전트 VM"],"next":"작업을 제출하고 진행을 봅니다"},
{"id":"verify","name":"검증","status":"✅","components":["게이트 ①","검사 VM"],"next":"판정 결과와 보고서를 확인합니다"},
{"id":"approve","name":"승인","status":"🔄","components":["승인 대기 목록"],"next":"승인 대기 1건"},
{"id":"publish","name":"게시·병합","status":"🔄","components":["push_branch","GitLab"],"next":"게시된 브랜치를 GitLab에서 검토·병합합니다 (병합은 수동)"},
{"id":"operate","name":"운영","status":"⬜","components":["정책","복원·정리"],"next":"백업·모니터링은 아직 없습니다"}]`)
	})
	mux.HandleFunc("/api/v1/overview", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"total":38,"by_state":{"done":36,"failed":2,"needs_approval":1},"pending_approvals":1,
"stage_avg":{"queued":0.2,"preparing":2,"agent_running":25,"collecting":1.3,"verifying":134,"publishing":3},
"todo":[{"kind":"approval","task":5,"approval":9,"text":"작업 #5 승인 대기: 정책 변경 <img src=x onerror=alert(1)>"}]}`)
	})
	serve := func(w http.ResponseWriter, b []byte) { json(w, string(b)) }
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// submit: 201 with the first task of the actions fixture (id 41)
			var rows []map[string]any
			_ = stdjson.Unmarshal(fixture("tasks-actions.json"), &rows)
			b, _ := stdjson.Marshal(rows[0])
			w.WriteHeader(201)
			serve(w, b)
			return
		}
		var rows []map[string]any
		if err := stdjson.Unmarshal(fixture("tasks.json"), &rows); err != nil {
			t.Fatal(err)
		}
		var more []map[string]any
		_ = stdjson.Unmarshal(fixture("tasks-actions.json"), &more)
		rows = append(rows, more...)
		hostile := "<img src=x onerror=alert(1)> \x1b[31mred\u202eevil\u0000"
		rows = append(rows, map[string]any{"id": 40, "repo": hostile, "base": "main", "prompt": hostile, "class": "docs",
			"agent": "claude", "state": hostile, "attempts": 1, "created_at": "2026-11-09T11:00:00Z", "updated_at": "2026-11-09T11:00:00Z"})
		b, _ := stdjson.Marshal(rows)
		serve(w, b)
	})
	// W2a: repos, policy classes, an in-progress task (41, cancellable), a
	// retryable failed task (42) and the ops variants (select with
	// /dev-set-ops?name=<fixture file>, default ops.json).
	for _, n := range []string{"repos", "policy"} {
		name := n + ".json"
		mux.HandleFunc("/api/v1/"+n, func(w http.ResponseWriter, r *http.Request) { serve(w, fixture(name)) })
	}
	var opsMu sync.Mutex
	opsName := "ops.json"
	mux.HandleFunc("/api/v1/ops", func(w http.ResponseWriter, r *http.Request) {
		opsMu.Lock()
		n := opsName
		opsMu.Unlock()
		serve(w, fixture(n))
	})
	mux.HandleFunc("/api/v1/tasks/41/events", func(w http.ResponseWriter, r *http.Request) {
		var d map[string]any
		_ = stdjson.Unmarshal(fixture("events-running.json"), &d)
		// the open step started 95 s ago, so the progress line shows a live elapsed time
		steps := d["steps"].([]any)
		steps[2].(map[string]any)["at"] = time.Now().Add(-95 * time.Second).UTC().Format(time.RFC3339)
		b, _ := stdjson.Marshal(d)
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/tasks/42/events", func(w http.ResponseWriter, r *http.Request) {
		var rows []map[string]any
		_ = stdjson.Unmarshal(fixture("tasks-actions.json"), &rows)
		var d map[string]any
		_ = stdjson.Unmarshal(fixture("events-running.json"), &d)
		steps := d["steps"].([]any)[:3]
		steps = append(steps, map[string]any{"state": "failed", "seconds": 0, "note": "verify: restore failed", "at": "2026-11-09T13:03:00Z"})
		b, _ := stdjson.Marshal(map[string]any{"task": rows[1], "steps": steps, "verify": nil})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/tasks/41/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json(w, `{"error":{"code":"state_changed","message":"dev"}}`)
	})
	mux.HandleFunc("/api/v1/tasks/42/retry", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json(w, `{"error":{"code":"not_retryable","message":"dev","reason":"재시도 한도(3회)를 모두 썼습니다"}}`)
	})
	mux.HandleFunc("/api/v1/tasks/38/events", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture("events.json")) })
	mux.HandleFunc("/api/v1/tasks/38/diff", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture("diff.json")) })
	mux.HandleFunc("/api/v1/tasks/37/events", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture("events.json")) })
	mux.HandleFunc("/api/v1/tasks/37/diff", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture("diff-truncated.json")) })
	mux.HandleFunc("/api/v1/tasks/38/report", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"verdict":"weakened","note":"<script>alert(1)</script> \u001b[31m","checks":[{"id":"unit","ok":true}]}`)
	})
	// Approvals: the golden signed-mode details (9: complete, 10: truncated)
	// and a passkey list for the login screen. Grants are refused (no real
	// passkey here): the dev server never accepts a signature.
	detail := func(name string, id int) []byte {
		var d map[string]any
		if err := stdjson.Unmarshal(fixture(name), &d); err != nil {
			t.Fatal(err)
		}
		d["id"] = id
		b, _ := stdjson.Marshal(d)
		return b
	}
	mux.HandleFunc("/api/v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		json(w, `[{"id":9,"task_id":38,"kind":"protected_modified","hash":"3273763058ed786269abfd318c01a425c417aeb4b196118f5c43206a0396f4ef","base":"1111111111111111111111111111111111111111","head":"89abcdef0123456789abcdef0123456789abcdef","status":"pending","reason":"r"},
{"id":10,"task_id":38,"kind":"protected_modified","hash":"3273763058ed786269abfd318c01a425c417aeb4b196118f5c43206a0396f4ef","base":"1111111111111111111111111111111111111111","head":"89abcdef0123456789abcdef0123456789abcdef","status":"pending","reason":"r"},
{"id":11,"task_id":38,"kind":"publish_outside_allowlist","hash":"dafbe2fdc61a991ad960a71e7d456a2e817b5ed26af792479b6b7e35f8fbe7d6","base":"1111111111111111111111111111111111111111","head":"89abcdef0123456789abcdef0123456789abcdef","status":"pending","reason":"push"}]`)
	})
	mux.HandleFunc("/api/v1/approvals/9", func(w http.ResponseWriter, r *http.Request) { serve(w, detail("approval.json", 9)) })
	mux.HandleFunc("/api/v1/approvals/11", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture("approval-publish.json")) })
	mux.HandleFunc("/api/v1/approvals/10", func(w http.ResponseWriter, r *http.Request) { serve(w, detail("approval-truncated.json", 10)) })
	mux.HandleFunc("/api/v1/approvals/9/grant", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		json(w, `{"error":{"code":"hash_changed","message":"dev"}}`)
	})
	mux.HandleFunc("/api/v1/auth/signers", func(w http.ResponseWriter, r *http.Request) {
		json(w, `{"signers":[{"id":"phone","credential_id":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","label":"내 폰 <b>x</b>"},
{"id":"mac.backup","credential_id":"HxwdHhscGxoZGBcWFRQTEhEQDw4NDAsKCQgHBgUEAwIBAA","label":"예비 키"}]}`)
	})
	console := web.Handler()
	srv := &http.Server{Addr: "127.0.0.1:" + port, ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", api.ContentSecurityPolicy)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.URL.Path == "/dev-set-ops" {
				n := r.URL.Query().Get("name")
				if !strings.HasPrefix(n, "ops") || !strings.HasSuffix(n, ".json") || strings.ContainsAny(n, "/\\") {
					http.Error(w, "bad name", 400)
					return
				}
				opsMu.Lock()
				opsName = n
				opsMu.Unlock()
				_, _ = w.Write([]byte("ok " + n))
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				mux.ServeHTTP(w, r)
				return
			}
			console.ServeHTTP(w, r)
		})}
	go func() { _ = srv.ListenAndServe() }()
	t.Logf("dev console on http://127.0.0.1:%s/ for %ds", port, secs)
	time.Sleep(time.Duration(secs) * time.Second)
	_ = srv.Close()
}
