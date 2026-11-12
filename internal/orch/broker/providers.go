package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"aigate/internal/registrymsg"
)

// Provider, group and project-creation ops (W3c). `providers` and `list_groups`
// are unsigned reads; the other four are signed writes (the broker verifies the
// signature, orchd never judges it). Like the other op files this one only
// shapes calls and replies: the broker's own text is never read. The single
// exception to "a failure carries only a code" is create_partial, whose
// `detail` object (public orphan facts) is validated field by field.

// Op names.
const (
	OpProviders      = "providers"
	OpListGroups     = "list_groups"
	OpSetProvider    = "set_provider"
	OpDeleteProvider = "delete_provider"
	OpCreateGroup    = "create_group"
	OpCreateProject  = "create_project"
)

func init() {
	// A little above the broker's own limits (5, 20, 15, 10, 20 and 60 s). The
	// create_project limit is the middle of the chain broker 60 s < orchd 70 s
	// < web 90 s.
	defaultTimeouts[OpProviders] = 8 * time.Second
	defaultTimeouts[OpListGroups] = 25 * time.Second
	defaultTimeouts[OpSetProvider] = 20 * time.Second
	defaultTimeouts[OpDeleteProvider] = 15 * time.Second
	defaultTimeouts[OpCreateGroup] = 25 * time.Second
	defaultTimeouts[OpCreateProject] = 70 * time.Second
}

// CodeCreatePartial is the reply code of a create_project that left a GitLab
// project behind.
const CodeCreatePartial = "create_partial"

// CodeLinkPartial is the reply code of a link_project/attach_remote that
// stopped after the repository was read: an existing repository may hold a
// deploy key of this server (never a thing to delete).
const CodeLinkPartial = "link_partial"

// Orphan origins of the broker's records.
const (
	OriginCreated = "created"
	OriginLinked  = "linked"
)

// ValidOrphanOrigin: "" (a broker that predates the field: read as created) or one of the two origins.
func ValidOrphanOrigin(s string) bool { return s == "" || s == OriginCreated || s == OriginLinked }

// Orphan steps (the broker's fixed list).
var partialSteps = map[string]bool{"project": true, "first_commit": true, "default_branch": true, "protect": true, "deploy_key": true, "register": true}

// ValidOrphanStep reports whether s is one of the broker's fixed step names.
func ValidOrphanStep(s string) bool { return partialSteps[s] }

// PartialDetail is the validated `detail` of a create_partial reply: which
// GitLab project may be left behind and at which step the creation stopped.
// ProjectID is nil when GitLab did not confirm an id.
type PartialDetail struct {
	Provider  string
	Project   string
	ProjectID *int64
	Step      string
	Recorded  bool
	Dropped   bool
	// Origin: OriginCreated or OriginLinked ("created" when an older broker sent none).
	Origin string
}

// ValidGitLabProjectPath reports whether s is a GitLab project path as the
// registry holds it: 2 to 5 slash-separated names (a group path of at most 4
// segments plus the project name).
func ValidGitLabProjectPath(s string) bool {
	segs := strings.Split(s, "/")
	if len(segs) < 2 || len(segs) > registrymsg.MaxGroupDepth+1 {
		return false
	}
	for _, seg := range segs {
		if !registrymsg.ValidName(seg) {
			return false
		}
	}
	return true
}

// parsePartialDetail returns nil unless raw is exactly the documented object.
func parsePartialDetail(raw json.RawMessage) *PartialDetail {
	if len(raw) == 0 || len(raw) > 2048 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	vals := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		key, ok := kt.(string)
		if err != nil || !ok {
			return nil
		}
		if _, dup := vals[key]; dup {
			return nil
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return nil
		}
		vals[key] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil
	}
	// the six documented members, plus `origin` (additive: an older broker has none)
	want := 6
	if _, hasOrigin := vals["origin"]; hasOrigin {
		want = 7
	}
	if len(vals) != want {
		return nil
	}
	d := &PartialDetail{}
	str := func(key string, dst *string) bool {
		v, ok := vals[key]
		return ok && json.Unmarshal(v, dst) == nil
	}
	boolean := func(key string, dst *bool) bool {
		v, ok := vals[key]
		return ok && json.Unmarshal(v, dst) == nil
	}
	if !str("provider", &d.Provider) || !str("project", &d.Project) || !str("step", &d.Step) ||
		!boolean("orphan_recorded", &d.Recorded) || !boolean("orphan_dropped", &d.Dropped) {
		return nil
	}
	d.Origin = OriginCreated
	if raw, ok := vals["origin"]; ok {
		var o string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &o) != nil || o == "" || !ValidOrphanOrigin(o) {
			return nil
		}
		d.Origin = o
	}
	idRaw, ok := vals["project_id"]
	if !ok {
		return nil
	}
	if !bytes.Equal(bytes.TrimSpace(idRaw), []byte("null")) {
		var id int64
		dec := json.NewDecoder(bytes.NewReader(idRaw))
		dec.UseNumber()
		var n json.Number
		if dec.Decode(&n) != nil {
			return nil
		}
		if _, err := dec.Token(); err != io.EOF {
			return nil
		}
		if json.Unmarshal([]byte(n.String()), &id) != nil || id <= 0 || bytes.ContainsAny(idRaw, "\"") {
			return nil
		}
		d.ProjectID = &id
	}
	if !registrymsg.ValidProviderID(d.Provider) || !(ValidGitLabProjectPath(d.Project) || registrymsg.ValidRemotePath(d.Project)) || !partialSteps[d.Step] {
		return nil
	}
	return d
}

// ProviderList is the `providers` reply.
type ProviderList struct {
	Providers []ProviderListRow `json:"providers"`
}

// ProviderListRow is one provider with its in-use flag.
type ProviderListRow struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	SSHURL   string `json:"ssh_url"`
	TokenRef string `json:"token_ref"`
	InUse    bool   `json:"in_use"`
}

// Providers reads the provider list (no signature, no side effect, no values).
func (c *Client) Providers(ctx context.Context) (*ProviderList, *OpError) {
	res, oerr := c.Do(ctx, OpProviders, map[string]any{})
	if oerr != nil {
		return nil, oerr
	}
	var v ProviderList
	if err := json.Unmarshal(res, &v); err != nil {
		return nil, unavailable()
	}
	return &v, nil
}
