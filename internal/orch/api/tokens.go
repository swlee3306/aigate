package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/broker"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
	"aigate/internal/registrymsg"
)

// Token endpoints (W3b). The token store lives in the broker (orchd cannot even
// read its directory). orchd lists it, builds the text a person signs
// (begin -> message, read-only, nothing stored, no broker write) and forwards a
// signed change to the broker (apply). orchd never judges a signature.
//
// apply is the ONLY request that carries a token value. Rules for that value:
//   - the body is capped at 4 KiB and read into ONE fixed buffer;
//   - the value is decoded into a byte slice, never kept as a Go string by this
//     code, and held in a type whose String/GoString/Format/MarshalJSON redact;
//   - it is never logged (a panic is logged by TYPE only), never put in the
//     ledger, a response or an error;
//   - the read buffer and the value slice are overwritten when the handler
//     returns. Go memory limit: encoding/json's decoder buffer, the RawMessage
//     copies of other keys, the encoder's pooled buffer and the kernel socket
//     buffers are copies this code cannot reach, and the garbage collector may
//     have moved a slice before it is wiped. The wipe shortens the exposure of
//     the buffers orchd owns; it does not make the process memory secret-free.

const (
	// TokenPrepPerActorHour bounds begin + message together per actor;
	// TokenApplyPerActorHour bounds apply (separate buckets).
	TokenPrepPerActorHour  = 60
	TokenApplyPerActorHour = 20

	// maxTokenApplyBody is the cap of an apply body (D7).
	maxTokenApplyBody = 4 << 10
	// Value bounds of D3 (the broker checks again).
	minTokenValue = 32
	maxTokenValue = 512
	// maxTokenRows bounds the list orchd re-encodes.
	maxTokenRows = 300

	maxDeployKeyFile    = 4096
	maxDeployKeyComment = 200
	redactedText        = "[redacted]"
)

// Fixed refusals. No broker text, path, value or hostile input is echoed.
var (
	errTokBadKind       = apiErr(http.StatusBadRequest, "bad_kind", "토큰 종류가 올바르지 않습니다. openai, gitlab, github 중 하나여야 합니다")
	errTokSingleton     = apiErr(http.StatusBadRequest, "openai_singleton", "openai 종류는 이름이 openai인 토큰 하나만 둘 수 있습니다")
	errTokPast          = apiErr(http.StatusBadRequest, "bad_request", "만료일이 이미 지났습니다. 오늘(UTC) 이후의 날짜를 쓰거나 비워 두세요")
	errTokTooShort      = apiErr(http.StatusBadRequest, "token_too_short", "토큰 값이 너무 짧습니다. 변경 없음")
	errTokBadFormat     = apiErr(http.StatusBadRequest, "token_bad_format", "토큰 값의 형식이 올바르지 않습니다. 공백 없는 인쇄 가능한 ASCII 문자여야 하고 너무 길면 안 됩니다. 변경 없음")
	errTokValueMismatch = apiErr(http.StatusBadRequest, "value_mismatch", "서명한 토큰 값과 보낸 토큰 값이 다릅니다. 값을 다시 입력하고 새로 서명하세요. 변경 없음")
	errTokUnknown       = apiErr(http.StatusNotFound, "token_unknown", "등록되지 않은 토큰입니다. 변경 없음")
	errTokOtherKind     = apiErr(http.StatusConflict, "token_exists_other_kind", "같은 이름의 토큰이 다른 종류로 이미 있습니다. 변경 없음")
	errTokensGone       = apiErr(http.StatusServiceUnavailable, "tokens_unavailable", "토큰 저장소를 사용할 수 없습니다. 관리자가 확인해야 합니다")
	errTokBrokerGone    = apiErr(http.StatusServiceUnavailable, "broker_unavailable", "토큰 저장 서비스에 연결할 수 없습니다")
	errTokBrokerBad     = apiErr(http.StatusBadGateway, "broker_error", "토큰 저장 서비스가 예상하지 못한 응답을 했습니다")
	errTokUnknownResult = apiErr(http.StatusGatewayTimeout, "outcome_unknown", "변경이 적용되었는지 알 수 없습니다. 자동으로 다시 시도하지 않았습니다. 토큰 목록을 다시 읽어 확인한 뒤 필요하면 다시 변경하세요")
	errTokRateLimited   = apiErr(http.StatusTooManyRequests, "token_rate_limited", "토큰 변경 요청 횟수 한도를 초과했습니다. 잠시 후 다시 시도하세요")
	errTokInternal      = apiErr(http.StatusInternalServerError, "internal_error", "내부 오류가 발생했습니다. 변경 여부를 토큰 목록에서 확인하세요")
	errDeployKeyGone    = apiErr(http.StatusServiceUnavailable, "deploy_key_unavailable", "배포 키의 공개 키를 읽을 수 없습니다. 관리자가 공개 키 파일(.pub)을 확인해야 합니다")
)

// ---- the value ----

// secretValue holds a token value as bytes. Every formatting and encoding
// path prints [redacted], so logging a request struct (%v, %+v, %#v, %s, JSON,
// text) cannot leak it. Reading the bytes needs an explicit conversion.
type secretValue []byte

func (secretValue) String() string               { return redactedText }
func (secretValue) GoString() string             { return redactedText }
func (secretValue) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedText + `"`), nil }
func (secretValue) MarshalText() ([]byte, error) { return []byte(redactedText), nil }
func (secretValue) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redactedText) }

// wireSecret is the value on its way to the broker: its MarshalJSON writes the
// real (printable ASCII, validated) value into the request line, and the bytes
// it produced are kept so they can be wiped after the call. It redacts in every
// other format, and exists only inside the args map of one broker call.
type wireSecret struct {
	src secretValue
	out []byte
}

func (wireSecret) String() string   { return redactedText }
func (wireSecret) GoString() string { return redactedText }
func (wireSecret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redactedText)
}

// MarshalJSON quotes the value; the value is printable ASCII, so only the
// quote and the backslash need an escape.
func (w *wireSecret) MarshalJSON() ([]byte, error) {
	out := make([]byte, 0, len(w.src)+len(w.src)/8+2)
	out = append(out, '"')
	for _, c := range w.src {
		if c == '"' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	out = append(out, '"')
	w.out = out
	return out, nil
}

func (w *wireSecret) wipe() {
	clear(w.out)
	clear(w.src)
}

// tokenSeams are test-only hooks (nil in production, like pauseWorkers):
// buffers sees the request buffer and the value buffer of an apply (to prove
// they are wiped), panicHook runs while the value is in memory (to prove a
// panic does not log it).
type tokenSeams struct {
	buffers   func(body, value []byte)
	panicHook func()
	// rawValue sees the RawMessage copy of the value key as soon as the body
	// was scanned, also when the scan refused the body.
	rawValue func(raw []byte)
	// afterWrite runs right after the broker call returned.
	afterWrite func()
}

// unquoteSecret decodes the raw JSON string of the value into dst (a slice with
// room for the longest legal value, so it never reallocates) and applies the
// D3 character and length rules in the broker's order: characters first, then
// length. The broker checks again.
func unquoteSecret(raw []byte, dst []byte) ([]byte, *httpError) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return dst, errProjBadBody
	}
	in := raw[1 : len(raw)-1]
	put := func(c byte) bool {
		if c < 0x21 || c > 0x7e || len(dst) >= maxTokenValue {
			return false
		}
		dst = append(dst, c)
		return true
	}
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c != '\\' {
			if !put(c) {
				return dst, errTokBadFormat
			}
			continue
		}
		i++
		if i >= len(in) {
			return dst, errProjBadBody
		}
		switch in[i] {
		case '"', '\\', '/':
			if !put(in[i]) {
				return dst, errTokBadFormat
			}
		case 'u':
			if i+4 > len(in)-1 {
				return dst, errProjBadBody
			}
			r := 0
			for k := 1; k <= 4; k++ {
				d := hexVal(in[i+k])
				if d < 0 {
					return dst, errProjBadBody
				}
				r = r<<4 | d
			}
			i += 4
			if r < 0x21 || r > 0x7e || !put(byte(r)) {
				return dst, errTokBadFormat
			}
		default: // \b \f \n \r \t and anything else: a control character or invalid
			return dst, errTokBadFormat
		}
	}
	if len(dst) < minTokenValue {
		return dst, errTokTooShort
	}
	return dst, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// valueHashOf is registrymsg.ValueHash over bytes: SHA-256 of request ":" value
// (hex). It works on the byte slice so no unwipeable string copy of the value
// is made; TestSetTokenForwardsExactArgs pins it against registrymsg.ValueHash.
func valueHashOf(request string, value []byte) string {
	h := sha256.New()
	h.Write([]byte(request))
	h.Write([]byte(":"))
	h.Write(value)
	return hex.EncodeToString(h.Sum(nil))
}

// ---- the change ----

type tokenChange struct {
	Action  string
	Subject string
	Fields  map[string]any
}

func (c tokenChange) hash() (string, error) {
	return registrymsg.ChangeHash(c.Action, c.Subject, c.Fields)
}

func (c tokenChange) kind() string {
	k, _ := c.Fields["kind"].(string)
	return k
}

// parseTokenChange validates action, subject and fields of a token change.
// withHash says whether value_hash belongs to the fields (message and apply:
// yes; begin: not yet, the browser computes it from the request id begin gives
// out). Nothing here touches the disk or the broker.
func (h *handler) parseTokenChange(rawAction, rawSubject, rawFields json.RawMessage, withHash bool) (tokenChange, error) {
	action, ok1 := rawString(rawAction)
	subject, ok2 := rawString(rawSubject)
	if !ok1 || !ok2 || hostileText(action) || hostileText(subject) {
		return tokenChange{}, errProjBadBody
	}
	if action != registrymsg.ActionSetToken && action != registrymsg.ActionDeleteToken {
		return tokenChange{}, errProjBadBody
	}
	if !registrymsg.ValidTokenRef(subject) {
		return tokenChange{}, errProjBadBody
	}
	fields, err := registrymsg.ParseFields(rawFields)
	if err != nil {
		return tokenChange{}, errProjBadBody
	}
	c := tokenChange{Action: action, Subject: subject, Fields: fields}
	if action == registrymsg.ActionDeleteToken {
		if len(fields) != 0 {
			return tokenChange{}, errProjBadBody
		}
		return c, nil
	}
	want := 2
	if withHash {
		want = 3
	}
	kind, okK := fields["kind"].(string)
	expires, okE := fields["expires"].(string)
	if len(fields) != want || !okK || !okE {
		return tokenChange{}, errProjBadBody
	}
	if _, has := fields["value_hash"]; has != withHash {
		return tokenChange{}, errProjBadBody
	}
	if !registrymsg.ValidTokenKind(kind) {
		return tokenChange{}, errTokBadKind
	}
	if (kind == "openai") != (subject == "openai") {
		return tokenChange{}, errTokSingleton
	}
	if !registrymsg.ValidExpires(expires) {
		return tokenChange{}, errProjBadBody
	}
	if expires != "" && expires < h.clock().UTC().Format("2006-01-02") {
		return tokenChange{}, errTokPast
	}
	if withHash {
		if _, err := registrymsg.CanonicalChange(action, subject, fields); err != nil {
			return tokenChange{}, errProjBadBody
		}
	}
	return c, nil
}

// tokenPreview is the confirmation screen's fixed Korean text. The variable
// parts are the name, the kind and the expiry that passed the schema; the value
// and its hash are never part of it.
func tokenPreview(c tokenChange) []string {
	const stamp = "서명하면 이 변경이 바로 적용됩니다. 이 서명은 5분 안에 한 번만 쓸 수 있습니다"
	if c.Action == registrymsg.ActionDeleteToken {
		out := []string{
			"토큰 '" + c.Subject + "'을(를) 삭제합니다. 이 이름으로 저장된 값이 지워집니다",
			"삭제 서명에는 토큰의 종류가 들어가지 않습니다. 이름이 맞는지만 확인하세요",
		}
		if c.Subject == "openai" {
			out = append(out, "[중요] openai 키를 지우면 설정 파일에 키가 있으면 그 키를 쓰고, 없으면 AI 계획 기능이 멈춥니다")
		}
		return append(out, stamp)
	}
	kind := c.kind()
	expires, _ := c.Fields["expires"].(string)
	out := []string{
		"토큰 '" + c.Subject + "'을(를) 저장합니다(종류: " + kind + "). 같은 이름이 같은 종류로 이미 있으면 새 값으로 바꿉니다",
	}
	if expires == "" {
		out = append(out, "만료일: 없음")
	} else {
		out = append(out, "만료일: "+expires+" (직접 적은 날짜입니다. 서버가 확인하지 않으며 날짜는 UTC로 셉니다)")
	}
	if kind == "openai" {
		out = append(out, "[중요] 이 키는 AI 계획 기능이 외부 AI 서비스(OpenAI)를 호출할 때 쓰이며, 저장하면 다음 호출부터 바로 쓰입니다")
	} else {
		out = append(out, "이 토큰은 저장만 되고 아직 어떤 기능도 쓰지 않습니다(이후 연결 기능이 이름으로 가져다 씁니다)")
	}
	return append(out, "토큰 값은 이 화면과 서명 메시지에 들어가지 않고, 서명에는 값의 해시만 묶입니다", stamp)
}

// ---- views and broker error mapping ----

// TokenView is one token as the console receives it (never a value).
type TokenView struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Last4    string `json:"last4"`
	Expires  string `json:"expires"`
	SetAt    string `json:"set_at"`
	Source   string `json:"source"`
	DaysLeft *int   `json:"days_left"`
	Usable   bool   `json:"usable"`
}

// tokenViewOf re-bounds a broker row; false when it is not a plausible one.
// maxDaysLeftSpan bounds a row's days_left. The broker counts exact UTC days and
// an expiry is a legal date from 2000-01-01 to 9999-12-31, so |days_left| can
// never exceed the span between those two dates (about 2.93 million), however
// far the broker's own clock is from them. The same bound, symmetric, covers
// past dates down to year 2000. (A smaller bound once made a legal far-future
// expiry turn the whole token list into a 502.)
var maxDaysLeftSpan = int(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).Unix()/86400 - time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()/86400)

func tokenViewOf(r broker.TokenRow) (TokenView, bool) {
	if !registrymsg.ValidTokenRef(r.Ref) || !registrymsg.ValidTokenKind(r.Kind) || (r.Kind == "openai") != (r.Ref == "openai") ||
		!registrymsg.ValidExpires(r.Expires) || (r.Source != "store" && r.Source != "config") || len(r.Last4) > 4 {
		return TokenView{}, false
	}
	for i := 0; i < len(r.Last4); i++ {
		if r.Last4[i] < 0x21 || r.Last4[i] > 0x7e {
			return TokenView{}, false
		}
	}
	if r.SetAt != "" {
		if len(r.SetAt) > 40 || hostileText(r.SetAt) {
			return TokenView{}, false
		}
		if _, err := time.Parse(time.RFC3339, r.SetAt); err != nil {
			return TokenView{}, false
		}
	}
	if r.DaysLeft != nil && (*r.DaysLeft < -maxDaysLeftSpan || *r.DaysLeft > maxDaysLeftSpan) {
		return TokenView{}, false
	}
	return TokenView{Ref: r.Ref, Kind: r.Kind, Last4: r.Last4, Expires: r.Expires, SetAt: r.SetAt,
		Source: r.Source, DaysLeft: r.DaysLeft, Usable: r.Usable}, true
}

// tokensBrokerError maps a broker failure to orchd's fixed refusal. Only the
// code is looked at. write says the call was set_token/delete_token: then a
// failure that may have happened (timeout, no intact answer, a store the broker
// calls unavailable, an unregistered code) is outcome_unknown, never a retry.
func tokensBrokerError(e *broker.OpError, write bool) *httpError {
	if write && e.UnknownOutcome() {
		return errTokUnknownResult
	}
	switch e.Code {
	case "not_signed":
		return errProjNotSigned
	case "bad_signature":
		return errProjBadSignature
	case "unknown_signer":
		return errProjUnknownSigner
	case "stale_signature":
		return errProjStale
	case "signers_unavailable":
		return errProjSignersGone
	case "request_reused":
		return errProjReused
	case "change_mismatch":
		return errProjChangeMismatch
	case "token_unknown":
		return errTokUnknown
	case "bad_kind":
		return errTokBadKind
	case "token_too_short":
		return errTokTooShort
	case "token_bad_format":
		return errTokBadFormat
	case "value_mismatch":
		return errTokValueMismatch
	case "token_exists_other_kind":
		return errTokOtherKind
	case "openai_singleton":
		return errTokSingleton
	case "tokens_unavailable", "registry_unavailable":
		if write {
			return errTokUnknownResult // may already be applied (failed dir fsync after rename)
		}
		return errTokensGone
	case "bad_request", broker.CodeTooLarge:
		return errProjBadBody
	case broker.CodeUnavailable, "forbidden", "busy", "unknown_op", "not_implemented":
		return errTokBrokerGone
	}
	if write {
		return errTokUnknownResult
	}
	return errTokBrokerBad
}

// ---- list ----

// tokensList is GET /api/v1/settings/tokens: the broker's secret-free list,
// each row re-bounded. All or nothing, like the project registry.
func (h *handler) tokensList(w http.ResponseWriter, r *http.Request) {
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errTokBrokerGone)
		return
	}
	tl, oerr := h.broker.Tokens(r.Context())
	if oerr != nil {
		writeErr(w, tokensBrokerError(oerr, false))
		return
	}
	if len(tl.Tokens) > maxTokenRows || tl.WarnDays < 0 || tl.WarnDays > 3650 {
		writeErr(w, errTokBrokerBad)
		return
	}
	out := make([]TokenView, 0, len(tl.Tokens))
	for _, row := range tl.Tokens {
		v, ok := tokenViewOf(row)
		if !ok {
			writeErr(w, errTokBrokerBad)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "warn_days": tl.WarnDays})
}

// ---- begin and message (read-only: nothing stored, no broker call) ----

// tokensBegin is POST /api/v1/tokens/begin: the server makes only the one-time
// request id (128 random bits) and the issue time (UTC, whole seconds). The
// browser needs the request id to compute the salted value hash. No value is
// accepted here; nothing is stored and the broker is not called.
func (h *handler) tokensBegin(w http.ResponseWriter, r *http.Request) {
	raw, err := readProjectsBody(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	top, ok := objectKeys(raw, "action", "subject", "fields")
	if !ok || len(top) != 3 {
		writeErr(w, errProjBadBody)
		return
	}
	if _, err := h.parseTokenChange(top["action"], top["subject"], top["fields"], false); err != nil {
		writeErr(w, err)
		return
	}
	// Nothing is sent anywhere, so the slot is never given back.
	if !h.tokenPrepLimit.take(store.ActorFrom(r.Context()), h.clock()) {
		writeErr(w, errTokRateLimited)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"request": registrymsg.NewRequestID(),
		"issued":  h.clock().UTC().Truncate(time.Second).Format(registryTimeFormat),
	})
}

func parseRegistryIssued(s string) (time.Time, bool) {
	t, err := time.Parse(registryTimeFormat, s)
	if err != nil || t.Location() != time.UTC || t.Format(registryTimeFormat) != s {
		return time.Time{}, false
	}
	return t, true
}

// tokensMessage is POST /api/v1/tokens/message: the server builds the canonical
// change, the message to sign and the confirmation sentences. No value is
// accepted; nothing is stored and the broker is not called.
func (h *handler) tokensMessage(w http.ResponseWriter, r *http.Request) {
	raw, err := readProjectsBody(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	top, ok := objectKeys(raw, "action", "subject", "fields", "request", "issued")
	if !ok || len(top) != 5 {
		writeErr(w, errProjBadBody)
		return
	}
	c, err := h.parseTokenChange(top["action"], top["subject"], top["fields"], c0(top["action"]))
	if err != nil {
		writeErr(w, err)
		return
	}
	request, ok1 := rawString(top["request"])
	issuedText, ok2 := rawString(top["issued"])
	if !ok1 || !ok2 || !isLowerHex(request, 32) {
		writeErr(w, errProjBadBody)
		return
	}
	issued, okI := parseRegistryIssued(issuedText)
	if !okI {
		writeErr(w, errProjBadBody)
		return
	}
	change, herr := c.hash()
	if herr != nil {
		writeErr(w, errProjBadBody)
		return
	}
	msg := signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change, Request: request, Issued: issued}
	text := msg.Text()
	if text == "" {
		writeErr(w, errProjBadBody)
		return
	}
	if !h.tokenPrepLimit.take(store.ActorFrom(r.Context()), h.clock()) {
		writeErr(w, errTokRateLimited)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message": text, "change": change, "request": request, "issued": issuedText, "preview": tokenPreview(c),
	})
}

// c0 reports whether the (raw) action is set_token: message and apply carry a
// value_hash for set_token only. Anything else is judged by parseTokenChange.
func c0(rawAction json.RawMessage) bool {
	a, ok := rawString(rawAction)
	return ok && a == registrymsg.ActionSetToken
}

// ---- apply ----

// readTokenApplyBody reads at most 4 KiB into ONE fixed buffer (no growth, so no
// abandoned copies). buf is returned in full so the caller can wipe it.
func readTokenApplyBody(r *http.Request) (buf, raw []byte, err *httpError) {
	buf = make([]byte, maxTokenApplyBody+1)
	n, rerr := io.ReadFull(r.Body, buf)
	switch {
	case n == len(buf):
		return buf, nil, errBodyTooLarge
	case rerr == nil || rerr == io.ErrUnexpectedEOF || rerr == io.EOF:
		return buf, buf[:n], nil
	}
	return buf, nil, errProjBadBody
}

// tokensApply is POST /api/v1/tokens/apply. The server recomputes the change
// from the fields, checks that the request is self-consistent (including that
// the value is the one whose salted hash the fields carry) and forwards it all
// to the broker unchanged. The broker verifies the signature and checks again.
func (h *handler) tokensApply(w http.ResponseWriter, r *http.Request) {
	var buf []byte
	var valueBuf secretValue // redacts in every format; real bytes only via []byte(valueBuf)
	var wire *wireSecret
	var attempted bool // a broker write was started
	var wActor, wAction, wSubject, wKind string
	defer func() {
		clear(buf)
		clear(valueBuf[:cap(valueBuf)])
		if wire != nil {
			wire.wipe()
		}
	}()
	defer func() {
		if rec := recover(); rec != nil {
			// The panic value may hold request text: only its type is logged.
			log.Printf("orchd: tokens apply panic (type %T); details withheld", rec)
			if attempted {
				// The broker may have applied the change: say so, and leave the trace.
				h.recordTokenChange(wActor, wAction, wSubject, wKind, false, errTokUnknownResult.v1Code)
				writeErr(w, errTokUnknownResult)
				return
			}
			writeErr(w, errTokInternal)
		}
	}()

	buf, raw, rerr := readTokenApplyBody(r)
	if rerr != nil {
		writeErr(w, rerr)
		return
	}
	if !utf8.Valid(raw) {
		writeErr(w, errProjBadBody)
		return
	}
	top, ok := scanObject(raw, "action", "subject", "fields", "change", "request", "issued", "signature", "value")
	defer func() { // the RawMessage copy of the value, also when the scan refused the body
		clear(top["value"])
	}()
	if h.tokenSeams.rawValue != nil {
		h.tokenSeams.rawValue(top["value"])
	}
	if !ok {
		writeErr(w, errProjBadBody)
		return
	}
	if sg, has := top["signature"]; !has || bytes.Equal(bytes.TrimSpace(sg), []byte("null")) {
		writeErr(w, errProjNotSigned)
		return
	}
	for _, k := range []string{"action", "subject", "fields", "change", "request", "issued"} {
		if _, has := top[k]; !has {
			writeErr(w, errProjBadBody)
			return
		}
	}
	isSet := c0(top["action"])
	c, err := h.parseTokenChange(top["action"], top["subject"], top["fields"], isSet)
	if err != nil {
		writeErr(w, err)
		return
	}
	claimed, ok1 := rawString(top["change"])
	request, ok2 := rawString(top["request"])
	issuedText, ok3 := rawString(top["issued"])
	if !ok1 || !ok2 || !ok3 || !isLowerHex(claimed, 64) || !isLowerHex(request, 32) {
		writeErr(w, errProjBadBody)
		return
	}
	issued, okI := parseRegistryIssued(issuedText)
	if !okI {
		writeErr(w, errProjBadBody)
		return
	}
	change, herr := c.hash()
	if herr != nil {
		writeErr(w, errProjBadBody)
		return
	}
	if (signing.RegistryMessage{Action: c.Action, Subject: c.Subject, Change: change, Request: request, Issued: issued}).Validate() != nil {
		writeErr(w, errProjBadBody)
		return
	}
	// The change the signer saw must be the change of the fields we hold.
	if claimed != change {
		writeErr(w, errProjChangeMismatch)
		return
	}
	sig, err := parseSignature(top["signature"])
	if err != nil {
		writeErr(w, err)
		return
	}
	rawValue, hasValue := top["value"]
	if hasValue != isSet {
		writeErr(w, errProjBadBody) // set_token needs the value, delete_token must not carry one
		return
	}
	if isSet {
		vb, verr := unquoteSecret(rawValue, make([]byte, 0, maxTokenValue+1))
		valueBuf = secretValue(vb)
		if h.tokenSeams.buffers != nil {
			h.tokenSeams.buffers(buf, []byte(valueBuf[:cap(valueBuf)]))
		}
		if verr != nil {
			writeErr(w, verr)
			return
		}
		// The value must be the one whose salted hash was signed. A mismatch is
		// refused here: forwarding it would only burn the one-time request id.
		want := valueHashOf(request, []byte(valueBuf))
		have, _ := c.Fields["value_hash"].(string)
		if subtle.ConstantTimeCompare([]byte(want), []byte(have)) != 1 {
			writeErr(w, errTokValueMismatch)
			return
		}
	}
	if h.broker == nil || h.broker.Socket == "" {
		writeErr(w, errTokBrokerGone)
		return
	}
	actor := store.ActorFrom(r.Context())
	slotAt := h.clock()
	if !h.tokenApplyLimit.take(actor, slotAt) {
		writeErr(w, errTokRateLimited)
		return
	}
	if h.tokenSeams.panicHook != nil {
		h.tokenSeams.panicHook()
	}
	wActor, wAction, wSubject, wKind = actor, c.Action, c.Subject, c.kind()
	args := map[string]any{"ref": c.Subject, "change": change, "request": request, "issued": issuedText, "signature": sig}
	if isSet {
		wire = &wireSecret{src: valueBuf}
		args["kind"], args["expires"], args["value_hash"], args["value"] = c.Fields["kind"], c.Fields["expires"], c.Fields["value_hash"], wire
	}
	// The write goes on even if the browser goes away. The client's own time
	// limit bounds it. One attempt, never retried.
	attempted = true
	res, oerr := h.broker.WriteToken(context.WithoutCancel(r.Context()), c.Action, args)
	if h.tokenSeams.afterWrite != nil {
		h.tokenSeams.afterWrite()
	}
	if wire != nil {
		wire.wipe()
	}
	kind := c.kind() // "" for delete_token: its signed fields carry no kind
	if oerr != nil {
		// Only a failure that provably cost nothing gives the slot back (the
		// request never completely left, or the broker refused before looking
		// at it). A refusal after the broker read the signature keeps it.
		if oerr.Unsent || oerr.Code == "busy" || oerr.Code == "forbidden" {
			h.tokenApplyLimit.refund(actor, slotAt)
		}
		herr := tokensBrokerError(oerr, true)
		h.recordTokenChange(actor, c.Action, c.Subject, kind, false, herr.v1Code)
		writeErr(w, herr)
		return
	}
	if isSet {
		var row broker.TokenRow
		v, vok := TokenView{}, false
		if json.Unmarshal(res, &row) == nil {
			v, vok = tokenViewOf(row)
		}
		if !vok || v.Ref != c.Subject || v.Kind != kind {
			// The broker said ok, so the change may well have happened.
			h.recordTokenChange(actor, c.Action, c.Subject, kind, false, errTokUnknownResult.v1Code)
			writeErr(w, errTokUnknownResult)
			return
		}
		h.recordTokenChange(actor, c.Action, c.Subject, kind, true, "ok")
		writeJSON(w, http.StatusOK, v)
		return
	}
	var del struct {
		Ref     string `json:"ref"`
		Deleted bool   `json:"deleted"`
	}
	if json.Unmarshal(res, &del) != nil || del.Ref != c.Subject || !del.Deleted {
		h.recordTokenChange(actor, c.Action, c.Subject, kind, false, errTokUnknownResult.v1Code)
		writeErr(w, errTokUnknownResult)
		return
	}
	h.recordTokenChange(actor, c.Action, c.Subject, kind, true, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"ref": del.Ref, "deleted": true})
}

// recordTokenChange appends the token_change ledger event: who, which action,
// which token name and kind, the outcome and its code. Never a value, a hash,
// the last four characters, an expiry or any part of the signature. The kind is
// "" for delete_token (its signed fields carry none; no extra broker lookup).
func (h *handler) recordTokenChange(actor, action, subject, kind string, ok bool, code string) {
	home, err := gate.Home(h.grants.Home)
	if err != nil {
		log.Printf("orchd: token change not recorded in the ledger: home unknown")
		return
	}
	err = ledger.Append(policy.OrchLedgerPath(home), "token_change", policy.OrchRepo, map[string]any{
		"actor": actor, "action": action, "subject": subject, "kind": kind, "ok": ok, "code": code,
	}, h.clock().UTC())
	if err != nil {
		log.Printf("orchd: token change not recorded in the ledger")
	}
}

// ---- deploy key (public half) ----

// deployKey is GET /api/v1/settings/deploy-key: the PUBLIC key of the SSH deploy
// key orchd publishes with, to paste into GitLab. It reads only
// "<--git-ssh-key>.pub" (a regular file, not a symlink) and never opens the
// private key. The content must be one ssh-ed25519 or ssh-rsa line.
func (h *handler) deployKey(w http.ResponseWriter, r *http.Request) {
	pub, fp, ok := readDeployKey(h.deployKeyPath)
	if !ok {
		writeErr(w, errDeployKeyGone)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": pub, "fingerprint": fp})
}

func readDeployKey(path string) (pub, fingerprint string, ok bool) {
	if path == "" {
		return "", "", false
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxDeployKeyFile {
		return "", "", false
	}
	// No symlink followed and no blocking on a FIFO swapped in after the Lstat.
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollowNonblock, 0)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	// The file opened is the file inspected (no symlink swapped in between).
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		return "", "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDeployKeyFile+1))
	if err != nil || len(data) > maxDeployKeyFile {
		return "", "", false
	}
	line := strings.TrimSuffix(string(data), "\n")
	if line == "" || !utf8.ValidString(line) || hostileText(line) {
		return "", "", false
	}
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 || line[i] == 0x7f { // also excludes CR and a second line
			return "", "", false
		}
	}
	var keyType string
	switch {
	case strings.HasPrefix(line, "ssh-ed25519 "):
		keyType = "ssh-ed25519"
	case strings.HasPrefix(line, "ssh-rsa "):
		keyType = "ssh-rsa"
	default:
		return "", "", false
	}
	pk, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) != 0 || len(rest) != 0 || pk.Type() != keyType || len(comment) > maxDeployKeyComment {
		return "", "", false
	}
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if comment != "" {
		pub += " " + comment
	}
	return pub, ssh.FingerprintSHA256(pk), true
}
