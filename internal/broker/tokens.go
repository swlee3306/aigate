package broker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/registrymsg"
)

// Token write ops (set_token, delete_token). Same signing rules as the
// project writes (verifySignedShape), with one more input: the value hash.
//
// Policy for what consumes the request ID (pinned by tests): a request that is
// refused before or during signature verification does not consume it; once a
// valid signature over the exact change has passed, the ID is consumed even if
// a later step refuses (value hash, value rules, expiry in the past, store
// errors). The value is held in memory only, never put into an error, a log
// line or a response. A `timeout` reply means the outcome is unknown.

const (
	minTokenValue = 32
	maxTokenValue = 512
)

var (
	setTokenKeys    = []string{"ref", "kind", "expires", "value", "value_hash", "change", "request", "issued", "signature"}
	deleteTokenKeys = []string{"ref", "change", "request", "issued", "signature"}
)

type setTokenArgs struct {
	Ref       string             `json:"ref"`
	Kind      string             `json:"kind"`
	Expires   string             `json:"expires"`
	Value     string             `json:"value"`
	ValueHash string             `json:"value_hash"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

type deleteTokenArgs struct {
	Ref       string             `json:"ref"`
	Change    string             `json:"change"`
	Request   string             `json:"request"`
	Issued    string             `json:"issued"`
	Signature *signing.Signature `json:"signature"`
}

// valueRuleCode applies decision D3: only printable ASCII 0x21-0x7e (so no
// whitespace anywhere, no trimming), 32..512 bytes. "" means valid.
func valueRuleCode(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return "token_bad_format"
		}
	}
	switch {
	case len(v) < minTokenValue:
		return "token_too_short"
	case len(v) > maxTokenValue:
		return "token_bad_format"
	}
	return ""
}

// tokenStoreFailure maps a TokenStore error to a fixed code.
func tokenStoreFailure(err error) *OpError {
	switch {
	case errors.Is(err, ErrTokenUnknown):
		return fail1("token_unknown")
	case errors.Is(err, ErrTokenKindMismatch):
		return fail1("token_exists_other_kind")
	case errors.Is(err, ErrOpenAISingleton):
		return fail1("openai_singleton")
	default: // broken store, full, cannot be saved
		return fail1("tokens_unavailable")
	}
}

func tokenShapeCode(ref, kind string) *OpError {
	if !registrymsg.ValidTokenRef(ref) {
		return fail1("bad_request")
	}
	if !registrymsg.ValidTokenKind(kind) {
		return fail1("bad_kind")
	}
	if (kind == "openai") != (ref == "openai") {
		return fail1("openai_singleton")
	}
	return nil
}

func (s *Server) setTokenHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a setTokenArgs
	if !decodeStrict(raw, &a, setTokenKeys) {
		return fail("bad_request")
	}
	if s.Tokens.Err() != nil {
		return fail("tokens_unavailable")
	}
	fields := map[string]any{"kind": a.Kind, "expires": a.Expires, "value_hash": a.ValueHash}
	shape := func() *OpError {
		if oe := tokenShapeCode(a.Ref, a.Kind); oe != nil {
			return oe
		}
		if !registrymsg.ValidExpires(a.Expires) {
			return fail1("bad_request")
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionSetToken, a.Ref, fields, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	// From here on the request ID is consumed (see the policy above).
	// The hash is recomputed from the VALIDATED request, never a raw string.
	want := registrymsg.ValueHash(a.Request, a.Value)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(a.ValueHash)) != 1 {
		return fail("value_mismatch")
	}
	if code := valueRuleCode(a.Value); code != "" {
		return fail(code)
	}
	now := s.now().UTC()
	if a.Expires != "" && a.Expires < now.Format("2006-01-02") {
		return fail("bad_request")
	}
	if err := s.Tokens.Set(a.Ref, a.Kind, a.Expires, a.Value, now); err != nil {
		return nil, tokenStoreFailure(err)
	}
	return tokenRow(a.Ref, a.Kind, a.Expires, a.Value, now), nil
}

func (s *Server) deleteTokenHandler(_ context.Context, raw json.RawMessage) (any, *OpError) {
	var a deleteTokenArgs
	if !decodeStrict(raw, &a, deleteTokenKeys) {
		return fail("bad_request")
	}
	if s.Tokens.Err() != nil {
		return fail("tokens_unavailable")
	}
	shape := func() *OpError {
		if !registrymsg.ValidTokenRef(a.Ref) {
			return fail1("bad_request")
		}
		return nil
	}
	if oe := s.verifySignedShape(registrymsg.ActionDeleteToken, a.Ref, map[string]any{}, a.Change, a.Request, a.Issued, a.Signature, shape); oe != nil {
		return nil, oe
	}
	if err := s.Tokens.Delete(a.Ref); err != nil {
		return nil, tokenStoreFailure(err)
	}
	return map[string]any{"ref": a.Ref, "deleted": true}, nil
}

// tokenRow is the secret-free listing row of what was just stored, built from
// the data written (no second read of the store, so it cannot fail or race).
func tokenRow(ref, kind, expires, value string, now time.Time) map[string]any {
	last4 := value
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	var dl any
	if d := DaysLeft(expires, now); d != nil {
		dl = *d
	}
	return map[string]any{"ref": ref, "kind": kind, "last4": last4, "expires": expires,
		"set_at": now.UTC().Format(time.RFC3339), "source": "store", "days_left": dl, "usable": true}
}
