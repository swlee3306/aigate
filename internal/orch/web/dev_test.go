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

	"aigate/internal/approval/signing"
	"aigate/internal/orch/api"
	"aigate/internal/orch/web"
	"aigate/internal/registrymsg"
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
		// A8: steps 0 and 1 carry token:mac-ssh and system (golden fixture), the
		// open step gets a hostile actor; task 38 shows session:alice, system and empty ones.
		steps[2].(map[string]any)["actor"] = "kind:<img src=x onerror=alert(1)>\u202eevil\x1b[31m" + strings.Repeat("x", 120)
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
	// B6: the AI plan flow (preview: /srv/repos/docs is "not allowed", any other
	// repository is allowed; refine: plan.json, or a hostile plan when the
	// requirement contains "hostile") and the merge request states of tasks
	// 51 (created), 52 (failed) and 53 (skipped).
	mux.HandleFunc("/api/v1/refine/preview", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Repo string }
		_ = stdjson.NewDecoder(r.Body).Decode(&req)
		if req.Repo == "/srv/repos/docs" {
			serve(w, fixture("preview-not-allowed.json"))
			return
		}
		serve(w, fixture("preview.json"))
	})
	mux.HandleFunc("/api/v1/refine", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		_ = stdjson.NewDecoder(r.Body).Decode(&req)
		time.Sleep(1500 * time.Millisecond) // so the loading text can be looked at
		if strings.Contains(req.Text, "hostile") {
			hostile := "<img src=x onerror=alert(1)> javascript:alert(1) \x1b[31mred\u202eevil\u0000"
			b, _ := stdjson.Marshal(map[string]any{"summary": hostile + strings.Repeat("A", 3000),
				"items": []map[string]any{{"id": "r1", "title": hostile + strings.Repeat("B", 400), "description": hostile + "\n" + strings.Repeat("C", 2000),
					"files": []string{hostile, "javascript:alert(1)", "../../etc/passwd"}}, {"id": "r2\"><img src=x onerror=alert(1)>", "title": "둘째", "description": "", "files": []string{}}},
				"questions": []map[string]any{{"id": "q1", "text": hostile}}, "out_of_scope": []string{hostile},
				"usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
			serve(w, b)
			return
		}
		serve(w, fixture("plan.json"))
	})
	for id, name := range map[string]string{"51": "task-mr-created.json", "52": "task-mr-failed.json", "53": "task-mr-skipped.json"} {
		file := name
		mux.HandleFunc("/api/v1/tasks/"+id+"/events", func(w http.ResponseWriter, r *http.Request) { serve(w, fixture(file)) })
	}
	mux.HandleFunc("/api/v1/tasks/52/mr", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		json(w, `{"error":{"code":"upstream_error","message":"dev <img src=x onerror=alert(1)>","reason":"코드: upstream_error"}}`)
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
	// W3a: the project registry. The state lives in memory. A successful apply
	// changes it (a register adds a row, a set_flag flips ai_context), so the
	// list can be seen to change. No signature is verified here: this is a
	// look at the screens, not a test of the broker. Select behaviour with
	// /dev-set-projects?propose=ok|mismatch&apply=ok|<error code>.
	var projMu sync.Mutex
	projects := []map[string]any{
		{"id": "docs", "path": "/home/orch/repos/docs", "kind": "docs", "ai_context": false, "gitlab": map[string]any{"project": "team/docs", "project_id": nil}, "auto_merge": map[string]any{"enabled": false}},
		{"id": "calc", "path": "/home/orch/repos/calc", "kind": "code", "ai_context": true, "gitlab": nil, "auto_merge": map[string]any{"enabled": false}},
	}
	projPropose, projApply := "ok", "ok"
	projStatus := map[string]int{"not_signed": 400, "bad_signature": 403, "unknown_signer": 403, "stale_signature": 403, "signers_unavailable": 503,
		"request_reused": 409, "change_mismatch": 400, "project_exists": 409, "project_unknown": 404, "path_not_allowed": 400,
		"registry_unavailable": 503, "not_supported": 400, "outcome_unknown": 504, "project_rate_limited": 429}
	mux.HandleFunc("/api/v1/projects/registry", func(w http.ResponseWriter, r *http.Request) {
		projMu.Lock()
		defer projMu.Unlock()
		b, _ := stdjson.Marshal(map[string]any{"projects": projects, "providers": []any{}, "auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5}})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/projects/candidates", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("ORCH_WEB_LONGPATH") != "" { // a 300-character candidate path, for the 390px check
			long := "/home/orch/repos/" + strings.Repeat("abcdefghij", 28) + "x"
			b, _ := stdjson.Marshal(map[string]any{"candidates": []map[string]any{{"path": long, "head": "0123456789abcdef0123456789abcdef01234567"}}, "kinds": []string{"docs"}})
			serve(w, b)
			return
		}
		serve(w, fixture("projects-candidates.json"))
	})
	type projBody struct {
		Action  string         `json:"action"`
		Subject string         `json:"subject"`
		Fields  map[string]any `json:"fields"`
		Change  string         `json:"change"`
	}
	mux.HandleFunc("/api/v1/projects/propose", func(w http.ResponseWriter, r *http.Request) {
		var in projBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		change, err := registrymsg.ChangeHash(in.Action, in.Subject, in.Fields)
		if err != nil {
			w.WriteHeader(400)
			json(w, `{"error":{"code":"bad_request","message":"dev"}}`)
			return
		}
		projMu.Lock()
		mode := projPropose
		projMu.Unlock()
		shown := change
		if mode == "mismatch" {
			shown = strings.Repeat("0", 64) // a server that signs something else than the screen says
		}
		issued := time.Now().UTC().Truncate(time.Second)
		msg := signing.RegistryMessage{Action: in.Action, Subject: in.Subject, Change: shown, Request: registrymsg.NewRequestID(), Issued: issued}
		ai, _ := in.Fields["ai_context"].(bool)
		if in.Action == "set_flag" {
			ai, _ = in.Fields["value"].(bool)
		}
		on := "[중요] 이 저장소의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다"
		off := "이 저장소의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)"
		line := off
		if ai {
			line = on
		}
		pv := []string{"새 프로젝트 '" + in.Subject + "'를 등록합니다", line, "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"}
		if in.Action == "set_flag" {
			pv = []string{"프로젝트 '" + in.Subject + "'의 AI 전송 허용 설정을 바꿉니다", line, "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"}
		}
		b, _ := stdjson.Marshal(map[string]any{"action": in.Action, "subject": in.Subject, "fields": in.Fields, "message": msg.Text(), "change": shown,
			"request": msg.Request, "issued": issued.Format("2006-01-02T15:04:05Z"), "preview": pv})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/projects/apply", func(w http.ResponseWriter, r *http.Request) {
		var in projBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		projMu.Lock()
		defer projMu.Unlock()
		if projApply != "ok" {
			st := projStatus[projApply]
			if st == 0 {
				st = 500
			}
			w.WriteHeader(st)
			json(w, `{"error":{"code":"`+projApply+`","message":"dev <img src=x onerror=alert(1)> SERVER-WORDS"}}`)
			return
		}
		switch in.Action {
		case "register_project":
			p := map[string]any{"id": in.Subject, "path": in.Fields["path"], "kind": in.Fields["kind"], "ai_context": in.Fields["ai_context"], "gitlab": nil, "auto_merge": map[string]any{"enabled": false}}
			projects = append(projects, p)
			b, _ := stdjson.Marshal(p)
			serve(w, b)
		default:
			for _, p := range projects {
				if p["id"] == in.Subject {
					p["ai_context"] = in.Fields["value"]
					b, _ := stdjson.Marshal(p)
					serve(w, b)
					return
				}
			}
			w.WriteHeader(404)
			json(w, `{"error":{"code":"project_unknown","message":"dev"}}`)
		}
	})
	console := web.Handler()
	srv := &http.Server{Addr: "127.0.0.1:" + port, ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", api.ContentSecurityPolicy)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.URL.Path == "/dev-set-projects" {
				projMu.Lock()
				if v := r.URL.Query().Get("propose"); v != "" {
					projPropose = v
				}
				if v := r.URL.Query().Get("apply"); v != "" {
					projApply = v
				}
				projMu.Unlock()
				_, _ = w.Write([]byte("ok"))
				return
			}
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
