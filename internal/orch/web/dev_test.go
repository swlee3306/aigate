package web_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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
		{"id": "billing", "path": "/home/orch/repos/billing", "kind": "registered", "ai_context": false, "provider": "gl", "gitlab": map[string]any{"project": "team/billing", "project_id": 77}, "auto_merge": map[string]any{"enabled": false}, "cloned": false},
	}
	projPropose, projApply := "ok", "ok"
	projStatus := map[string]int{"not_signed": 400, "bad_signature": 403, "unknown_signer": 403, "stale_signature": 403, "signers_unavailable": 503,
		"request_reused": 409, "change_mismatch": 400, "project_exists": 409, "project_unknown": 404, "path_not_allowed": 400,
		"registry_unavailable": 503, "not_supported": 400, "outcome_unknown": 504, "project_rate_limited": 429}
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
	// W3b: the token menu. State in memory; a successful apply changes the list.
	// No signature is verified and no value is stored: the dev apply only checks
	// the rules the real broker checks (D3, hash bound to the request) and then
	// drops the value. Select behaviour with /dev-set-tokens?apply=ok|<code>&propose=ok|mismatch.
	var tokMu sync.Mutex
	dd := func(n int) *int { return &n }
	tokRows := []api.TokenView{
		{Ref: "openai", Kind: "openai", Last4: "ZQxw", Expires: "2027-06-30", SetAt: "2026-11-09T10:04:05Z", Source: "store", DaysLeft: dd(233), Usable: true},
		{Ref: "gitlab-main", Kind: "gitlab", Last4: "a1B2", Expires: "2026-11-13", SetAt: "2026-10-01T09:00:00Z", Source: "store", DaysLeft: dd(3), Usable: true},
		{Ref: "github-old", Kind: "github", Last4: "x9Y8", Expires: "2026-11-08", SetAt: "2026-05-01T09:00:00Z", Source: "store", DaysLeft: dd(-2), Usable: true},
		{Ref: "gitlab-docs", Kind: "gitlab", Last4: "q7w6", Source: "store", Usable: true},
		{Ref: "github-lost", Kind: "github", Last4: "m5n4", Expires: "2027-01-31", SetAt: "2026-08-01T09:00:00Z", Source: "store", DaysLeft: dd(82), Usable: false},
	}
	if os.Getenv("ORCH_WEB_LONGTOKEN") != "" { // long names and path-like strings, for the 390px check
		tokRows = append(tokRows,
			api.TokenView{Ref: "gitlab-" + strings.Repeat("abcdefghij", 3) + "xy", Kind: "gitlab", Last4: "w1w2", Expires: "2026-11-20", Source: "store", DaysLeft: dd(10), Usable: true},
			api.TokenView{Ref: "<img src=x onerror=alert(1)>", Kind: "javascript:" + strings.Repeat("k", 80), Last4: "<b>x", Expires: "2026-11-20", Source: "/very/long/path/" + strings.Repeat("segment/", 12), DaysLeft: dd(10), Usable: true})
	}
	tokApply, tokPropose := "ok", "ok"
	tokStatus := map[string]int{"not_signed": 400, "bad_signature": 403, "unknown_signer": 403, "stale_signature": 403, "signers_unavailable": 503,
		"request_reused": 409, "change_mismatch": 400, "bad_request": 400, "token_unknown": 404, "bad_kind": 400, "token_too_short": 400,
		"token_bad_format": 400, "value_mismatch": 400, "openai_singleton": 400, "token_exists_other_kind": 409, "tokens_unavailable": 503,
		"outcome_unknown": 504, "broker_unavailable": 503, "token_rate_limited": 429, "internal_error": 500, "broker_error": 502}
	tokErr := func(w http.ResponseWriter, code string) {
		st := tokStatus[code]
		if st == 0 {
			st = 500
		}
		w.WriteHeader(st)
		json(w, `{"error":{"code":"`+code+`","message":"dev <img src=x onerror=alert(1)> SERVER-WORDS"}}`)
	}
	mux.HandleFunc("/api/v1/settings/tokens", func(w http.ResponseWriter, r *http.Request) {
		tokMu.Lock()
		defer tokMu.Unlock()
		b, _ := stdjson.Marshal(map[string]any{"tokens": tokRows, "warn_days": 14})
		serve(w, b)
	})
	type tokBody struct {
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Change    string         `json:"change"`
		Request   string         `json:"request"`
		Issued    string         `json:"issued"`
		Value     string         `json:"value"`
		Signature any            `json:"signature"`
	}
	mux.HandleFunc("/api/v1/tokens/begin", func(w http.ResponseWriter, r *http.Request) {
		var in tokBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		tokMu.Lock()
		mode := tokApply
		tokMu.Unlock()
		if mode == "token_rate_limited_begin" {
			tokErr(w, "token_rate_limited")
			return
		}
		b, _ := stdjson.Marshal(map[string]string{"request": registrymsg.NewRequestID(), "issued": time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/tokens/message", func(w http.ResponseWriter, r *http.Request) {
		var in tokBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		change, err := registrymsg.ChangeHash(in.Action, in.Subject, in.Fields)
		issued, ierr := time.Parse("2006-01-02T15:04:05Z", in.Issued)
		if err != nil || ierr != nil {
			w.WriteHeader(400)
			json(w, `{"error":{"code":"bad_request","message":"dev"}}`)
			return
		}
		tokMu.Lock()
		mode := tokPropose
		tokMu.Unlock()
		shown := change
		if mode == "mismatch" {
			shown = strings.Repeat("0", 64)
		}
		msg := signing.RegistryMessage{Action: in.Action, Subject: in.Subject, Change: shown, Request: in.Request, Issued: issued.UTC()}
		stamp := "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"
		var pv []string
		if in.Action == "delete_token" {
			pv = []string{"토큰 '" + in.Subject + "'을(를) 삭제합니다. 이 이름으로 저장된 값이 지워집니다", "삭제 서명에는 토큰의 종류가 들어가지 않습니다. 이름이 맞는지만 확인하세요", stamp}
		} else {
			kind, _ := in.Fields["kind"].(string)
			exp, _ := in.Fields["expires"].(string)
			pv = []string{"토큰 '" + in.Subject + "'을(를) 저장합니다(종류: " + kind + "). 같은 이름이 같은 종류로 이미 있으면 새 값으로 바꿉니다"}
			if exp == "" {
				pv = append(pv, "만료일: 없음")
			} else {
				pv = append(pv, "만료일: "+exp+" (직접 적은 날짜입니다. 서버가 확인하지 않으며 날짜는 UTC로 셉니다)")
			}
			pv = append(pv, "이 토큰은 저장만 되고 아직 어떤 기능도 쓰지 않습니다(이후 연결 기능이 이름으로 가져다 씁니다)",
				"토큰 값은 이 화면과 서명 메시지에 들어가지 않고, 서명에는 값의 해시만 묶입니다", stamp)
		}
		b, _ := stdjson.Marshal(map[string]any{"message": msg.Text(), "change": shown, "request": in.Request, "issued": in.Issued, "preview": pv})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/tokens/apply", func(w http.ResponseWriter, r *http.Request) {
		var in tokBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		tokMu.Lock()
		defer tokMu.Unlock()
		if tokApply != "ok" {
			tokErr(w, tokApply)
			return
		}
		if in.Action == "delete_token" {
			if in.Value != "" {
				tokErr(w, "bad_request")
				return
			}
			for i, row := range tokRows {
				if row.Ref == in.Subject && row.Source == "store" {
					tokRows = append(tokRows[:i], tokRows[i+1:]...)
					b, _ := stdjson.Marshal(map[string]any{"ref": in.Subject, "deleted": true})
					serve(w, b)
					return
				}
			}
			tokErr(w, "token_unknown")
			return
		}
		// the rules the real broker applies; the value is dropped, never stored or logged
		kind, _ := in.Fields["kind"].(string)
		exp, _ := in.Fields["expires"].(string)
		hash, _ := in.Fields["value_hash"].(string)
		if hash != registrymsg.ValueHash(in.Request, in.Value) {
			tokErr(w, "value_mismatch")
			return
		}
		if len(in.Value) < 32 {
			tokErr(w, "token_too_short")
			return
		}
		for _, c := range []byte(in.Value) {
			if c < 0x21 || c > 0x7e {
				tokErr(w, "token_bad_format")
				return
			}
		}
		row := api.TokenView{Ref: in.Subject, Kind: kind, Last4: in.Value[len(in.Value)-4:], Expires: exp, SetAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), Source: "store", Usable: true}
		if exp != "" {
			if d, err := time.Parse("2006-01-02", exp); err == nil {
				row.DaysLeft = dd(int(time.Until(d).Hours() / 24))
			}
		}
		replaced := false
		for i := range tokRows {
			if tokRows[i].Ref == in.Subject && tokRows[i].Source == "store" {
				tokRows[i], replaced = row, true
			}
		}
		if !replaced {
			tokRows = append(tokRows, row)
		}
		b, _ := stdjson.Marshal(row)
		serve(w, b)
	})
	// W3c: providers, groups and project creation. State in memory. No signature
	// is verified and nothing real is called: this is a look at the screens. The
	// deploy key is a real ed25519 key from a fixed seed, so its fingerprint and
	// line hash are the ones the browser recomputes. Select what the next call
	// does with /dev-set-w3c?create=ok|partial|clone_failed|unknown|taken&clone=ok|fail|unknown.
	var w3cMu sync.Mutex
	type devProvider struct{ ID, BaseURL, SSHURL, TokenRef string }
	devProviders := []devProvider{
		{"gl", "http://100.64.0.10:18082", "ssh://git@100.64.0.10", "gitlab-main"},
		{"gl-spare", "https://git.example.com", "ssh://git@git.example.com:2222", "gitlab-docs"},
	}
	devGroups := []map[string]string{{"path": "team", "name": "team"}, {"path": "team/sub", "name": "sub"}, {"path": "docs-group", "name": "docs-group"}}
	devOrphans := []map[string]any{}
	createMode, cloneMode := "ok", "ok"
	devPub, devFP, devKeyHash := func() (string, string, string) {
		priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
		pk, err := ssh.NewPublicKey(priv.Public())
		if err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " orchd@deploy"
		sum := sha256.Sum256([]byte(line))
		return line, ssh.FingerprintSHA256(pk), hex.EncodeToString(sum[:])
	}()
	provUsed := func(id string) bool {
		for _, p := range projects {
			if p["provider"] == id {
				return true
			}
		}
		return false
	}
	provViews := func(withUse bool) []map[string]any {
		out := []map[string]any{}
		for _, p := range devProviders {
			m := map[string]any{"id": p.ID, "kind": "gitlab", "base_url": p.BaseURL, "ssh_url": p.SSHURL, "token_ref": p.TokenRef}
			if withUse {
				m["in_use"] = provUsed(p.ID)
			}
			out = append(out, m)
		}
		return out
	}
	// the registry: projects (+ created ones), providers, orphans
	mux.HandleFunc("/api/v1/projects/registry", func(w http.ResponseWriter, r *http.Request) {
		projMu.Lock()
		w3cMu.Lock()
		defer projMu.Unlock()
		defer w3cMu.Unlock()
		b, _ := stdjson.Marshal(map[string]any{"projects": projects, "providers": provViews(true), "orphans": devOrphans, "auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5}})
		serve(w, b)
	})
	mux.HandleFunc("/api/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		projMu.Lock()
		w3cMu.Lock()
		defer projMu.Unlock()
		defer w3cMu.Unlock()
		b, _ := stdjson.Marshal(map[string]any{"providers": provViews(true)})
		serve(w, b)
	})
	mux.HandleFunc("GET /api/v1/providers/{id}/groups", func(w http.ResponseWriter, r *http.Request) {
		w3cMu.Lock()
		defer w3cMu.Unlock()
		b, _ := stdjson.Marshal(map[string]any{"groups": devGroups, "truncated": false})
		serve(w, b)
	})
	devErr := func(w http.ResponseWriter, status int, code string) {
		w.WriteHeader(status)
		json(w, `{"error":{"code":"`+code+`","message":"dev <img src=x onerror=alert(1)> SERVER-WORDS"}}`)
	}
	type w3cBody struct {
		Action  string         `json:"action"`
		Subject string         `json:"subject"`
		Fields  map[string]any `json:"fields"`
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	devPropose := func(w http.ResponseWriter, r *http.Request) {
		var in w3cBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		fields := in.Fields
		var extra map[string]any
		if in.Action == "create_project" {
			fields = map[string]any{}
			for k, v := range in.Fields {
				fields[k] = v
			}
			fields["deploy_key_sha256"] = devKeyHash
			extra = map[string]any{"deploy_key": map[string]string{"public_key": devPub, "fingerprint": devFP}}
		}
		change, err := registrymsg.ChangeHash(in.Action, in.Subject, fields)
		if err != nil {
			devErr(w, 400, "bad_request")
			return
		}
		issued := time.Now().UTC().Truncate(time.Second)
		msg := signing.RegistryMessage{Action: in.Action, Subject: in.Subject, Change: change, Request: registrymsg.NewRequestID(), Issued: issued}
		stamp := "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"
		var pv []string
		switch in.Action {
		case "set_provider":
			pv = []string{"제공자 '" + in.Subject + "'를 저장합니다(종류: gitlab). 같은 id가 있으면 이 값으로 바뀝니다", "GitLab 주소: " + str(fields, "base_url"), "ssh 주소: " + str(fields, "ssh_url"),
				"사용할 토큰 이름: " + str(fields, "token_ref") + " (토큰 값은 이 화면에 나오지 않습니다)", "[중요] 이 토큰의 권한으로 위 주소의 GitLab에 그룹과 프로젝트를 만듭니다",
				"이 제공자를 쓰는 프로젝트가 있으면 주소는 바꿀 수 없습니다", stamp}
		case "delete_provider":
			pv = []string{"제공자 '" + in.Subject + "'를 삭제합니다", "이 제공자를 쓰는 프로젝트가 있으면 삭제되지 않습니다", "GitLab의 그룹과 프로젝트는 지워지지 않습니다", stamp}
		case "create_group":
			parent := str(fields, "parent_path")
			if parent == "" {
				parent = "(최상위)"
			}
			pv = []string{"제공자 '" + in.Subject + "'의 GitLab에 새 그룹을 만듭니다", "그룹 이름: " + str(fields, "name"), "그룹 경로: " + str(fields, "path"), "상위 그룹: " + parent,
				"비공개 그룹으로 만들어집니다. 이 도구는 그룹을 삭제하지 않습니다", stamp}
		default:
			ai := "이 프로젝트의 파일 내용은 AI 서비스로 전송되지 않습니다(전송 허용 꺼짐)"
			if v, _ := fields["ai_context"].(bool); v {
				ai = "[중요] 이 프로젝트의 파일 내용이 외부 AI 서비스(OpenAI)로 전송될 수 있게 허용합니다. AI 계획 기능을 쓰면 요구사항과 저장소의 일부 파일 내용이 서비스 밖으로 나갑니다"
			}
			pub := "쓰기: 이 서버가 브랜치를 올릴 수 있는 배포 키로 등록합니다"
			if str(fields, "publish") == "read" {
				pub = "읽기: 배포 키는 읽기 전용으로 등록합니다(이 서버는 브랜치를 올리지 못합니다)"
			}
			desc := str(fields, "description")
			if desc == "" {
				desc = "(없음)"
			}
			pv = []string{"GitLab 제공자 '" + str(fields, "provider") + "'의 그룹 '" + str(fields, "group_path") + "'에 새 프로젝트 '" + in.Subject + "'를 만듭니다",
				"템플릿: " + str(fields, "template") + " (코드에 고정된 파일들이 첫 커밋으로 들어갑니다)", "설명: " + desc, ai, "게시 권한 — " + pub,
				"배포 키 지문(SHA-256): " + devFP + " (이 서버의 배포 키 공개 키 파일 기준)", "등록 후 정책 종류(kind)는 registered 입니다",
				"GitLab 프로젝트는 비공개로 만들어지고, 만든 뒤 이 도구는 삭제하지 않습니다", "만든 뒤 이 서버에 자동으로 한 번 복제합니다", stamp}
		}
		out := map[string]any{"action": in.Action, "subject": in.Subject, "fields": fields, "message": msg.Text(), "change": change,
			"request": msg.Request, "issued": issued.Format("2006-01-02T15:04:05Z"), "preview": pv}
		for k, v := range extra {
			out[k] = v
		}
		b, _ := stdjson.Marshal(out)
		serve(w, b)
	}
	mux.HandleFunc("POST /api/v1/providers/propose", devPropose)
	mux.HandleFunc("POST /api/v1/projects/create/propose", devPropose)
	mux.HandleFunc("POST /api/v1/providers/apply", func(w http.ResponseWriter, r *http.Request) {
		var in w3cBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		projMu.Lock()
		w3cMu.Lock()
		defer projMu.Unlock()
		defer w3cMu.Unlock()
		switch in.Action {
		case "set_provider":
			p := devProvider{in.Subject, str(in.Fields, "base_url"), str(in.Fields, "ssh_url"), str(in.Fields, "token_ref")}
			for i := range devProviders {
				if devProviders[i].ID == p.ID {
					devProviders[i] = p
					b, _ := stdjson.Marshal(map[string]any{"provider": map[string]any{"id": p.ID, "kind": "gitlab", "base_url": p.BaseURL, "ssh_url": p.SSHURL, "token_ref": p.TokenRef}})
					serve(w, b)
					return
				}
			}
			devProviders = append(devProviders, p)
			b, _ := stdjson.Marshal(map[string]any{"provider": map[string]any{"id": p.ID, "kind": "gitlab", "base_url": p.BaseURL, "ssh_url": p.SSHURL, "token_ref": p.TokenRef}})
			serve(w, b)
		case "delete_provider":
			if provUsed(in.Subject) {
				devErr(w, 409, "provider_in_use")
				return
			}
			for i := range devProviders {
				if devProviders[i].ID == in.Subject {
					devProviders = append(devProviders[:i], devProviders[i+1:]...)
					break
				}
			}
			json(w, `{"id":"`+in.Subject+`","deleted":true}`)
		default: // create_group
			path := str(in.Fields, "path")
			if p := str(in.Fields, "parent_path"); p != "" {
				path = p + "/" + path
			}
			devGroups = append(devGroups, map[string]string{"path": path, "name": str(in.Fields, "name")})
			b, _ := stdjson.Marshal(map[string]any{"provider": in.Subject, "group": map[string]string{"path": path, "name": str(in.Fields, "name")}})
			serve(w, b)
		}
	})
	mux.HandleFunc("POST /api/v1/projects/create/apply", func(w http.ResponseWriter, r *http.Request) {
		var in w3cBody
		_ = stdjson.NewDecoder(r.Body).Decode(&in)
		time.Sleep(700 * time.Millisecond) // long enough to see the waiting screen
		projMu.Lock()
		w3cMu.Lock()
		defer projMu.Unlock()
		defer w3cMu.Unlock()
		group := str(in.Fields, "group_path")
		switch createMode {
		case "taken":
			devErr(w, 409, "project_name_taken")
			return
		case "unknown":
			devErr(w, 504, "outcome_unknown")
			return
		case "partial":
			devOrphans = append(devOrphans, map[string]any{"provider": str(in.Fields, "provider"), "project": group + "/" + in.Subject, "project_id": 4242, "step": "deploy_key", "at": time.Now().UTC().Format("2006-01-02T15:04:05Z")})
			w.WriteHeader(502)
			b, _ := stdjson.Marshal(map[string]any{"error": map[string]any{"code": "create_partial", "message": "dev <img src=x onerror=alert(1)> SERVER-WORDS", "detail": map[string]any{
				"provider": str(in.Fields, "provider"), "project": group + "/" + in.Subject, "project_id": 4242, "step": "deploy_key", "orphan_recorded": true, "orphan_dropped": false}}})
			serve(w, b)
			return
		}
		cloned := createMode != "clone_failed"
		p := map[string]any{"id": in.Subject, "path": "/home/orch/repos/" + in.Subject, "kind": "registered", "ai_context": in.Fields["ai_context"], "provider": in.Fields["provider"],
			"gitlab": map[string]any{"project": group + "/" + in.Subject, "project_id": 100 + len(projects)}, "auto_merge": map[string]any{"enabled": false}, "cloned": cloned}
		projects = append(projects, p)
		clone := map[string]any{"status": "cloned", "code": "ok"}
		if !cloned {
			clone = map[string]any{"status": "failed", "code": "host_not_pinned"}
		}
		b, _ := stdjson.Marshal(map[string]any{"project": p, "clone": clone})
		serve(w, b)
	})
	mux.HandleFunc("POST /api/v1/projects/{id}/clone", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		time.Sleep(500 * time.Millisecond)
		projMu.Lock()
		w3cMu.Lock()
		defer projMu.Unlock()
		defer w3cMu.Unlock()
		switch cloneMode {
		case "fail":
			devErr(w, 502, "clone_failed")
			return
		case "unknown":
			devErr(w, 504, "outcome_unknown")
			return
		}
		for _, p := range projects {
			if p["id"] == id {
				p["cloned"] = true
				b, _ := stdjson.Marshal(map[string]any{"project": p, "clone": map[string]any{"status": "cloned", "code": "ok"}})
				serve(w, b)
				return
			}
		}
		devErr(w, 404, "project_unknown")
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
			if r.URL.Path == "/dev-set-tokens" {
				tokMu.Lock()
				if v := r.URL.Query().Get("propose"); v != "" {
					tokPropose = v
				}
				if v := r.URL.Query().Get("apply"); v != "" {
					tokApply = v
				}
				tokMu.Unlock()
				_, _ = w.Write([]byte("ok"))
				return
			}
			if r.URL.Path == "/dev-set-w3c" {
				w3cMu.Lock()
				if v := r.URL.Query().Get("create"); v != "" {
					createMode = v
				}
				if v := r.URL.Query().Get("clone"); v != "" {
					cloneMode = v
				}
				w3cMu.Unlock()
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
