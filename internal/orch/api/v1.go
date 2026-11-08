package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

const v1Prefix = "/api/v1"

// mountV1 exposes the task and approval handlers under /api/v1. The handlers
// are the legacy ones; v1Wrap (applied in ServeHTTP to every /api/v1 path,
// including paths the mux itself rejects) renders their errors as the v1
// envelope {"error":{"code","message"}}.
func (h *handler) mountV1(mux *http.ServeMux) {
	mux.HandleFunc("POST "+v1Prefix+"/tasks", h.submit)
	mux.HandleFunc("GET "+v1Prefix+"/tasks", h.list)
	mux.HandleFunc("GET "+v1Prefix+"/tasks/{id}", h.get)
	mux.HandleFunc("GET "+v1Prefix+"/tasks/{id}/events", h.events)
	mux.HandleFunc("POST "+v1Prefix+"/tasks/{id}/cancel", h.cancel)
	mux.HandleFunc("POST "+v1Prefix+"/tasks/{id}/retry", h.retry)
	mux.HandleFunc("GET "+v1Prefix+"/tasks/{id}/report", h.report)
	mux.HandleFunc("GET "+v1Prefix+"/tasks/{id}/diff", h.diff)
	mux.HandleFunc("GET "+v1Prefix+"/approvals", h.approvals)
	mux.HandleFunc("GET "+v1Prefix+"/approvals/{id}", h.approvalGet)
	mux.HandleFunc("POST "+v1Prefix+"/approvals/{id}/reverify", h.reverify)
}

// v1Wrap serves next with errors rewritten into the v1 envelope.
func v1Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ew := &envWriter{ResponseWriter: w}
		next.ServeHTTP(ew, r)
		ew.finish()
	})
}

func isV1Path(p string) bool { return p == v1Prefix || strings.HasPrefix(p, v1Prefix+"/") }

// codeOf maps an HTTP status to the envelope's machine-readable code.
func codeOf(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if status >= 500 {
		return "internal"
	}
	return "bad_request"
}

// writeV1Err writes the v1 error envelope.
func writeV1Err(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// envWriter buffers error responses (status >= 400) so finish can rewrite
// them into the envelope; successful responses pass straight through.
type envWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (e *envWriter) WriteHeader(code int) {
	if code >= 400 && e.status == 0 {
		e.status = code
		return
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *envWriter) Write(b []byte) (int, error) {
	if e.status != 0 {
		return e.buf.Write(b)
	}
	return e.ResponseWriter.Write(b)
}

func (e *envWriter) finish() {
	if e.status == 0 {
		return
	}
	h := e.ResponseWriter.Header()
	h.Del("Content-Length")
	msg := ""
	if strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		var flat struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(e.buf.Bytes(), &flat) == nil {
			msg = flat.Error
		}
	}
	if msg == "" {
		switch e.status {
		case http.StatusNotFound:
			msg = "찾을 수 없는 경로입니다"
		case http.StatusMethodNotAllowed:
			msg = "허용되지 않는 메서드입니다"
		default:
			msg = "요청을 처리할 수 없습니다"
		}
	}
	writeV1Err(e.ResponseWriter, e.status, codeOf(e.status), msg)
}
