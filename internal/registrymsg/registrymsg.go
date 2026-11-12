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

	// W3c: provider and project-creation actions.
	ActionSetProvider    = "set_provider"
	ActionDeleteProvider = "delete_provider"
	ActionCreateGroup    = "create_group"
	ActionCreateProject  = "create_project"

	// W3e: link an existing remote repository as a NEW local project, or
	// attach a remote to an ALREADY registered project.
	ActionLinkProject  = "link_project"
	ActionAttachRemote = "attach_remote"

	// W3f: signed deletion. remove_project clears the server-side clone, key
	// folder and registry row (the remote repository stays); delete_remote_repo
	// deletes a repository this tool created; remove_deploy_key detaches the
	// tool's deploy key from a linked repository; clear_orphan drops one orphan
	// record (no external call).
	ActionRemoveProject    = "remove_project"
	ActionDeleteRemoteRepo = "delete_remote_repo"
	ActionRemoveDeployKey  = "remove_deploy_key"
	ActionClearOrphan      = "clear_orphan"
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
	requestRE  = regexp.MustCompile(`^[0-9a-f]{32}$`)
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
	// W3c (D2). Subjects: provider id for the first three, new project name
	// for create_project.
	ActionSetProvider:    {"kind": kindString, "base_url": kindString, "ssh_url": kindString, "token_ref": kindString},
	ActionDeleteProvider: {},
	ActionCreateGroup:    {"name": kindString, "path": kindString, "parent_path": kindString},
	ActionCreateProject: {"provider": kindString, "group_path": kindString, "template": kindString, "description": kindString,
		"ai_context": kindBool, "publish": kindString, "deploy_key_sha256": kindString},
	// W3e (E-D2). Subjects: the NEW local project id for link_project, the
	// EXISTING project id for attach_remote.
	ActionLinkProject: {"provider": kindString, "remote_path": kindString, "ai_context": kindBool, "publish": kindString,
		"deploy_key_sha256": kindString},
	ActionAttachRemote: {"provider": kindString, "remote_path": kindString, "publish": kindString, "deploy_key_sha256": kindString},
	// W3f (F-D3). Subjects: the EXISTING project id for the first three,
	// the provider id for clear_orphan.
	ActionRemoveProject:    {"scope": kindString, "legacy_ack": kindBool},
	ActionDeleteRemoteRepo: {"remote_path": kindString, "remote_id": kindString, "confirm_name": kindString, "also_local": kindBool},
	ActionRemoveDeployKey:  {"remote_path": kindString},
	ActionClearOrphan:      {"project": kindString},
}

// optionalFields lists, per action, fields a change MAY carry on top of its
// schema row. The schema layer does not know the provider's kind, so it cannot
// say whether visibility is required (github) or forbidden (gitlab): the
// broker op enforces that. Here it is only an optional enum. There is no
// default: absent stays absent in the canonical text.
var optionalFields = map[string]map[string]fieldKind{
	ActionCreateProject: {"visibility": kindString},
}

// emptyOK lists the only string fields that may be "": a token's expires (no
// expiry), a group's parent_path (top level) and a project's description.
// Every other string field of every action must be non-empty.
var emptyOK = map[string]map[string]bool{
	ActionSetToken:      {"expires": true},
	ActionCreateGroup:   {"parent_path": true},
	ActionCreateProject: {"description": true},
}

func isNewAction(action string) bool {
	switch action {
	case ActionSetProvider, ActionDeleteProvider, ActionCreateGroup, ActionCreateProject, ActionLinkProject, ActionAttachRemote,
		ActionRemoveProject, ActionDeleteRemoteRepo, ActionRemoveDeployKey, ActionClearOrphan:
		return true
	}
	return false
}

// validSubject applies the subject rule of the action. The rules are separate
// on purpose: project ids of the older actions, token refs, provider ids and
// new project names can each change without moving the others.
func validSubject(action, subject string) bool {
	switch action {
	case ActionSetToken, ActionDeleteToken:
		return tokenRefRE.MatchString(subject)
	case ActionSetProvider, ActionDeleteProvider, ActionCreateGroup, ActionClearOrphan:
		return providerIDRE.MatchString(subject)
	case ActionCreateProject, ActionLinkProject:
		return ValidName(subject)
	default:
		return subjectRE.MatchString(subject)
	}
}

// validateNewField checks a string field of a W3c action.
func validateNewField(action, name, s string) error {
	ok := false
	switch action + "." + name {
	case ActionSetProvider + ".kind":
		ok = ValidProviderKind(s)
	case ActionSetProvider + ".base_url":
		ok = ValidBaseURL(s)
	case ActionSetProvider + ".ssh_url":
		ok = ValidSSHURL(s)
	case ActionSetProvider + ".token_ref":
		ok = tokenRefRE.MatchString(s)
	case ActionCreateGroup + ".name", ActionCreateGroup + ".path":
		ok = ValidName(s)
	case ActionCreateGroup + ".parent_path":
		// "" is the top level; otherwise the new group must still fit the depth limit.
		ok = s == "" || (ValidGroupPath(s) && strings.Count(s, "/")+1 < MaxGroupDepth)
	case ActionCreateProject + ".provider", ActionLinkProject + ".provider", ActionAttachRemote + ".provider":
		ok = providerIDRE.MatchString(s)
	case ActionCreateProject + ".group_path":
		ok = ValidOwnerField(s) // superset; the broker op applies the provider's own rule before the signature
	case ActionCreateProject + ".template":
		ok = s == "basic" || s == "go"
	case ActionCreateProject + ".description":
		ok = ValidDescription(s)
	case ActionCreateProject + ".publish", ActionLinkProject + ".publish", ActionAttachRemote + ".publish":
		ok = s == "write" || s == "read"
	case ActionCreateProject + ".deploy_key_sha256", ActionLinkProject + ".deploy_key_sha256", ActionAttachRemote + ".deploy_key_sha256":
		ok = hashRE.MatchString(s)
	case ActionCreateProject + ".visibility":
		ok = s == "public" || s == "private"
	case ActionLinkProject + ".remote_path", ActionAttachRemote + ".remote_path",
		ActionDeleteRemoteRepo + ".remote_path", ActionRemoveDeployKey + ".remote_path":
		ok = ValidRemotePath(s)
	case ActionRemoveProject + ".scope":
		ok = s == "local"
	case ActionDeleteRemoteRepo + ".remote_id":
		ok = ValidRemoteID(s)
	case ActionDeleteRemoteRepo + ".confirm_name":
		ok = ValidConfirmName(s)
	case ActionClearOrphan + ".project":
		ok = ValidOrphanProject(s)
	}
	if !ok {
		return errors.New("registrymsg: bad " + name)
	}
	return nil
}

func isTokenAction(action string) bool {
	return action == ActionSetToken || action == ActionDeleteToken
}

// ValidTokenRef reports whether s is a token name (the subject of token actions).
func ValidTokenRef(s string) bool { return tokenRefRE.MatchString(s) }

// ValidTokenKind reports whether s is one of openai, gitlab, github.
func ValidTokenKind(s string) bool { return tokenKinds[s] }

// ValidExpires reports whether s is "" (no expiry) or a real calendar date
// written YYYY-MM-DD with a year from 2000 to 9999.
func ValidExpires(s string) bool {
	if s == "" {
		return true
	}
	if !expiresRE.MatchString(s) {
		return false
	}
	d, err := time.Parse("2006-01-02", s)
	return err == nil && d.Year() >= 2000
}

// ValueHash is the salted hash of a token value that a set_token signature
// covers: lowercase hex SHA-256 of request ‖ ":" ‖ value. The request id is the
// salt, so the same value never hashes the same way under two requests. The
// value itself is never part of a change or message.
//
// request must be exactly 32 lowercase hex characters (a NewRequestID); the
// fixed width is what makes request ":" value unambiguous. For anything else
// ValueHash returns "" and the caller must refuse. Callers must pass the
// validated request of the signed message, never a raw args string.
func ValueHash(request, value string) string {
	if !requestRE.MatchString(request) {
		return ""
	}
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
	if !validSubject(action, subject) {
		return errors.New("registrymsg: bad subject")
	}
	opt := optionalFields[action]
	for name := range fields {
		if _, req := spec[name]; !req {
			if _, ok := opt[name]; !ok {
				return errors.New("registrymsg: fields do not match the action")
			}
		}
	}
	all := make(map[string]fieldKind, len(spec)+len(opt))
	for name, kind := range spec {
		all[name] = kind
	}
	for name, kind := range opt {
		if _, present := fields[name]; present { // optional: checked only when carried
			all[name] = kind
		}
	}
	for name, kind := range all {
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
			// Only the fields listed in emptyOK may be empty.
			if !ok || (s == "" && !emptyOK[action][name]) {
				return errors.New("registrymsg: field has the wrong type")
			}
			if err := checkString(s); err != nil {
				return err
			}
			if isNewAction(action) {
				if err := validateNewField(action, name, s); err != nil {
					return err
				}
				continue
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
