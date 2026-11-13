package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
)

// H-D6 (W3h): every HTTP 403 orchd answers (signature, session/CSRF, origin,
// socket peer and the other forbidden refusals) leaves ONE fixed log line:
//
//	orchd: refused 403 <code> <METHOD> <path>
//
// <code> is the answer's own machine code (v1 envelope error.code or the
// legacy flat "code"; "forbidden" when it has none), <path> the request path
// only, as escaped on the wire. Never the query string, a header, the body, a
// signature, a token or a key line. The ledger is not touched.
//
// logRefusals is the outermost layer of both listeners (httpHandler,
// socketHandler), so refusals written before the API handler runs (origin,
// socket peer) are covered too.

const (
	// refusalBodyMax bounds what is kept of a 403 body to read its code.
	refusalBodyMax = 8 << 10
	// refusalPathMax bounds the logged path.
	refusalPathMax = 256
)

var (
	refusalCodeRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	refusalMethodRE = regexp.MustCompile(`^[A-Z]{1,16}$`)
)

func logRefusals(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &refusalWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if rw.status != http.StatusForbidden {
			return
		}
		method := r.Method
		if !refusalMethodRE.MatchString(method) {
			method = "OTHER"
		}
		path := r.URL.EscapedPath()
		if len(path) > refusalPathMax {
			path = path[:refusalPathMax] + "..."
		}
		log.Printf("orchd: refused 403 %s %s %s", refusalCode(rw.body.Bytes()), method, path)
	})
}

// refusalCode is the machine code of a 403 body: the v1 envelope's error.code
// or the legacy flat code; "forbidden" (the envelope's code for a bare 403)
// when there is none or it is not a plain code.
func refusalCode(b []byte) string {
	var v1 struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v1) == nil && refusalCodeRE.MatchString(v1.Error.Code) {
		return v1.Error.Code
	}
	var flat struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(b, &flat) == nil && refusalCodeRE.MatchString(flat.Code) {
		return flat.Code
	}
	return codeOf(http.StatusForbidden)
}

// refusalWriter records the status and keeps (bounded) the body of a 403 only.
type refusalWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (rw *refusalWriter) WriteHeader(code int) {
	if rw.status == 0 {
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *refusalWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	if rw.status == http.StatusForbidden && rw.body.Len() < refusalBodyMax {
		n := len(b)
		if room := refusalBodyMax - rw.body.Len(); n > room {
			n = room
		}
		rw.body.Write(b[:n])
	}
	return rw.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rw *refusalWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

// Flush passes a flush through when the underlying writer can.
func (rw *refusalWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
