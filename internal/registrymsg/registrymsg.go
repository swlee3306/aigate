// Package registrymsg builds the canonical change description that a project
// registry signature covers (the "change" line of an aigate-registry-v1
// message). It is pure: no HTTP, files, clocks or provider clients, so the
// broker and orchd can share it.
package registrymsg

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Registry change actions.
const (
	ActionRegisterProject = "register_project"
	ActionSetFlag         = "set_flag"
	ActionSetToken        = "set_token"
	ActionDeleteToken     = "delete_token"
)

var (
	subjectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	kindRE    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	flagRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

	// tokenRefRE is the rule for the subject of token actions. It is a
	// separate rule from subjectRE (project ids) even though both accept the
	// same names today, so one can change without moving the other.
	tokenRefRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	expiresRE  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	hashRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// tokenKinds is the closed set of token kinds.
var tokenKinds = map[string]bool{"openai": true, "gitlab": true, "github": true}

type fieldKind int

const (
	kindString fieldKind = iota
	kindBool
)

// schema lists, per action, the exact fields a change carries.
var schema = map[string]map[string]fieldKind{
	ActionRegisterProject: {"path": kindString, "kind": kindString, "ai_context": kindBool},
	ActionSetFlag:         {"flag": kindString, "value": kindBool},
	ActionSetToken:        {"kind": kindString, "expires": kindString, "value_hash": kindString},
	ActionDeleteToken:     {},
}

func isTokenAction(action string) bool {
	return action == ActionSetToken || action == ActionDeleteToken
}

// ValidTokenRef reports whether s is a token name (the subject of token actions).
func ValidTokenRef(s string) bool { return tokenRefRE.MatchString(s) }

// ValidTokenKind reports whether s is one of openai, gitlab, github.
func ValidTokenKind(s string) bool { return tokenKinds[s] }

// ValidExpires reports whether s is "" (no expiry) or a real calendar date
// written YYYY-MM-DD.
func ValidExpires(s string) bool {
	if s == "" {
		return true
	}
	if !expiresRE.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// ValueHash is the salted hash of a token value that a set_token signature
// covers: lowercase hex SHA-256 of request ‖ ":" ‖ value. The request id is the
// salt, so the same value never hashes the same way under two requests. The
// value itself is never part of a change or message.
func ValueHash(request, value string) string {
	h := sha256.New()
	h.Write([]byte(request))
	h.Write([]byte(":"))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

// ValidSubject reports whether s is a project id.
func ValidSubject(s string) bool { return subjectRE.MatchString(s) }

// ValidKind reports whether s is a project kind (a policy class name the
// registry can carry).
func ValidKind(s string) bool { return kindRE.MatchString(s) }

// checkString rejects strings whose canonical JSON could differ between Go and
// a browser's JSON.stringify: invalid UTF-8, control characters, DEL and the
// U+2028/U+2029 separators.
func checkString(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("registrymsg: string is not valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return errors.New("registrymsg: string contains a control character")
		}
	}
	return nil
}

func validate(action, subject string, fields map[string]any) error {
	spec, ok := schema[action]
	if !ok {
		return errors.New("registrymsg: unknown action")
	}
	subjectOK := subjectRE.MatchString(subject)
	if isTokenAction(action) {
		subjectOK = tokenRefRE.MatchString(subject)
	}
	if !subjectOK {
		return errors.New("registrymsg: bad subject")
	}
	if len(fields) != len(spec) {
		return errors.New("registrymsg: fields do not match the action")
	}
	for name, kind := range spec {
		v, present := fields[name]
		if !present {
			return errors.New("registrymsg: missing field")
		}
		switch kind {
		case kindBool:
			if _, ok := v.(bool); !ok {
				return errors.New("registrymsg: field has the wrong type")
			}
		case kindString:
			s, ok := v.(string)
			// Only a token's expires may be empty ("no expiry").
			if !ok || (s == "" && !(action == ActionSetToken && name == "expires")) {
				return errors.New("registrymsg: field has the wrong type")
			}
			if err := checkString(s); err != nil {
				return err
			}
			switch name {
			case "kind":
				if action == ActionSetToken {
					if !tokenKinds[s] {
						return errors.New("registrymsg: bad kind")
					}
				} else if !kindRE.MatchString(s) {
					return errors.New("registrymsg: bad kind")
				}
			case "expires":
				if !ValidExpires(s) {
					return errors.New("registrymsg: bad expires")
				}
			case "value_hash":
				if !hashRE.MatchString(s) {
					return errors.New("registrymsg: bad value_hash")
				}
			case "flag":
				if !flagRE.MatchString(s) {
					return errors.New("registrymsg: bad flag")
				}
			}
		}
	}
	return nil
}

// CanonicalChange returns the canonical JSON of a change:
// {"action":…,"fields":{…},"subject":…} with keys in lexicographic order, no
// whitespace and no HTML escaping. Unknown or missing fields, non-string/bool
// values (so no floats) and unsafe strings are refused.
func CanonicalChange(action, subject string, fields map[string]any) ([]byte, error) {
	if err := validate(action, subject, fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]any{} // delete_token: encode {} not null
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// encoding/json writes map keys sorted.
	if err := enc.Encode(map[string]any{"action": action, "subject": subject, "fields": fields}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ChangeHash is the SHA-256 of CanonicalChange as 64 lowercase hex characters.
func ChangeHash(action, subject string, fields map[string]any) (string, error) {
	c, err := CanonicalChange(action, subject, fields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// NewRequestID returns 128 random bits as 32 lowercase hex characters.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// ParseFields strictly decodes a JSON object of change fields: duplicate keys,
// numbers, nulls, arrays, nested objects, trailing data, invalid UTF-8 and
// lone surrogate escapes (U+FFFD) are refused. (A Go
// map cannot carry duplicate keys, so they must be caught while parsing.)
func ParseFields(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("registrymsg: fields are not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("registrymsg: fields must be a JSON object")
	}
	out := map[string]any{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, errors.New("registrymsg: malformed fields")
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("registrymsg: malformed fields")
		}
		if strings.ContainsRune(key, utf8.RuneError) {
			return nil, errors.New("registrymsg: invalid character in fields")
		}
		if _, dup := out[key]; dup {
			return nil, errors.New("registrymsg: duplicate field")
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, errors.New("registrymsg: malformed fields")
		}
		switch v := vt.(type) {
		case string:
			// A lone surrogate escape decodes to U+FFFD, which a browser
			// would keep as a lone surrogate: refuse rather than diverge.
			if strings.ContainsRune(v, utf8.RuneError) {
				return nil, errors.New("registrymsg: invalid character in fields")
			}
			out[key] = v
		case bool:
			out[key] = v
		default:
			return nil, errors.New("registrymsg: field must be a string or boolean")
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, errors.New("registrymsg: malformed fields")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("registrymsg: trailing data after fields")
	}
	return out, nil
}
