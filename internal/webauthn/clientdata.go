package webauthn

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	errClientData  = errors.New("webauthn: malformed clientDataJSON")
	errType        = errors.New("webauthn: clientData type mismatch")
	errChallenge   = errors.New("webauthn: clientData challenge mismatch")
	errOrigin      = errors.New("webauthn: clientData origin mismatch")
	errCrossOrigin = errors.New("webauthn: cross-origin ceremony refused")
	errTopOrigin   = errors.New("webauthn: clientData topOrigin refused")
)

const maxClientDataJSON = 4 << 10

// CheckClientData verifies a clientDataJSON: its type equals wantType
// ("webauthn.create" or "webauthn.get"), its challenge is the unpadded
// base64url encoding of challenge, its origin equals origin exactly,
// crossOrigin is absent or false, and topOrigin is absent. Unknown keys are
// ignored. An empty expected origin is refused (ErrConfig).
func CheckClientData(clientDataJSON []byte, wantType string, challenge []byte, origin string) error {
	if len(clientDataJSON) > maxClientDataJSON {
		return fmt.Errorf("%w: clientDataJSON over %d bytes", errTooLarge, maxClientDataJSON)
	}
	if origin == "" {
		return ErrConfig
	}
	if len(challenge) == 0 {
		return fmt.Errorf("%w: empty expected challenge", errChallenge)
	}
	dec := json.NewDecoder(bytes.NewReader(clientDataJSON))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return fmt.Errorf("%w: not a JSON object", errClientData)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data", errClientData)
	}

	var typ, chal, orig string
	if !jsonString(fields["type"], &typ) || typ != wantType {
		return fmt.Errorf("%w: got %q, want %q", errType, typ, wantType)
	}
	if !jsonString(fields["challenge"], &chal) {
		return errChallenge
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(chal)
	if err != nil || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return errChallenge
	}
	if !jsonString(fields["origin"], &orig) || orig != origin {
		return fmt.Errorf("%w: got %q", errOrigin, orig)
	}
	if raw, ok := fields["crossOrigin"]; ok {
		var co bool
		if string(raw) == "null" || json.Unmarshal(raw, &co) != nil || co {
			return errCrossOrigin
		}
	}
	if _, ok := fields["topOrigin"]; ok {
		return errTopOrigin
	}
	return nil
}

// jsonString decodes raw into s; it fails for anything but a JSON string.
func jsonString(raw json.RawMessage, s *string) bool {
	if len(raw) == 0 || raw[0] != '"' {
		return false
	}
	return json.Unmarshal(raw, s) == nil
}
