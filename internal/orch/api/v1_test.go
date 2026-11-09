package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type v1Err struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func wantEnvelope(t *testing.T, code int, b []byte, wantStatus int, wantCode string) {
	t.Helper()
	if code != wantStatus {
		t.Fatalf("status %d want %d: %s", code, wantStatus, b)
	}
	var e v1Err
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("not an envelope: %s (%v)", b, err)
	}
	if e.Error.Code != wantCode || e.Error.Message == "" {
		t.Fatalf("envelope %+v want code %q with a message", e.Error, wantCode)
	}
}

func TestV1UnknownRouteEnvelope(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.do(t, "GET", "/api/v1/nope", "")
	wantEnvelope(t, code, b, http.StatusNotFound, "not_found")
}

func TestV1WrongMethodEnvelope(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.do(t, "DELETE", "/api/v1/tasks", "")
	wantEnvelope(t, code, b, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestLegacyRoutesStillWork(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	if code, b := e.do(t, "GET", "/tasks", ""); code != 200 {
		t.Fatalf("GET /tasks %d %s", code, b)
	}
	// legacy errors keep the old flat shape
	code, b := e.do(t, "GET", "/tasks/abc", "")
	if code != 400 || errText(b) == "" {
		t.Fatalf("legacy error %d %s", code, b)
	}
}

func TestV1TasksListMatchesLegacy(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	for i := 0; i < 2; i++ {
		if code, b := e.submit(t, e.repo, "HEAD", fmt.Sprintf("p%d", i), "버그수정", "claude"); code != 201 {
			t.Fatalf("submit %d %s", code, b)
		}
	}
	ids := func(path string) []int64 {
		code, b := e.do(t, "GET", path, "")
		if code != 200 {
			t.Fatalf("%s %d %s", path, code, b)
		}
		var vs []TaskView
		if err := json.Unmarshal(b, &vs); err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	a, b := ids("/tasks"), ids("/api/v1/tasks")
	if len(a) != 2 || fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("legacy %v v1 %v", a, b)
	}
}

func TestV1ErrorCodes(t *testing.T) {
	e := newEnv(t, noSteps{}, 1)
	code, b := e.do(t, "GET", "/api/v1/tasks/abc", "")
	wantEnvelope(t, code, b, 400, "bad_request")
	code, b = e.do(t, "GET", "/api/v1/tasks/999", "")
	wantEnvelope(t, code, b, 404, "not_found")

	code, b = e.submit(t, e.repo, "HEAD", "x", "버그수정", "claude")
	if code != 201 {
		t.Fatalf("submit %d %s", code, b)
	}
	var v TaskView
	_ = json.Unmarshal(b, &v)
	path := fmt.Sprintf("/api/v1/tasks/%d/cancel", v.ID)
	if code, b = e.do(t, "POST", path, ""); code != 200 {
		t.Fatalf("cancel %d %s", code, b)
	}
	code, b = e.do(t, "POST", path, "")
	wantEnvelope(t, code, b, 409, "already_final")
}
