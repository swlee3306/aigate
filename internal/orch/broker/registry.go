package broker

import (
	"context"
	"encoding/json"
	"time"
)

// Registry ops (W3a). Reads (`projects`) are unsigned and secret-free; the two
// write ops carry a signature the broker verifies, never orchd. This file only
// shapes the calls and the replies: the broker's own text is never read.

// Op names of the registry.
const (
	OpProjects        = "projects"
	OpRegisterProject = "register_project"
	OpSetFlag         = "set_flag"
)

// Timeout is the broker's `timeout` reply code.
const CodeTimeout = "timeout"

func init() {
	// A little above the broker's own 10 s limit for these ops.
	defaultTimeouts[OpProjects] = 15 * time.Second
	defaultTimeouts[OpRegisterProject] = 15 * time.Second
	defaultTimeouts[OpSetFlag] = 15 * time.Second
}

// RemoteLink is a project's remote repository (registry v3): Path is
// group/…/name (GitLab) or owner/repo (GitHub), ID the provider's numeric id
// when it confirmed one.
type RemoteLink struct {
	Path string `json:"path"`
	ID   *int64 `json:"id"`
}

// AutoMerge is a project's auto-merge setting (always off in W3a).
type AutoMerge struct {
	Enabled bool `json:"enabled"`
}

// Project is one registry entry as the broker reports it. Unknown fields of a
// newer broker are ignored.
type Project struct {
	ID        string      `json:"id"`
	Path      string      `json:"path"`
	Kind      string      `json:"kind"`
	AIContext bool        `json:"ai_context"`
	Provider  *string     `json:"provider"`
	Remote    *RemoteLink `json:"remote"`
	AutoMerge AutoMerge   `json:"auto_merge"`
}

// AutoMergeGlobal is the registry-wide switch.
type AutoMergeGlobal struct {
	Enabled   bool `json:"enabled"`
	MaxPerDay int  `json:"max_per_day"`
}

// RegistryView is the `projects` reply.
type RegistryView struct {
	Projects        []Project       `json:"projects"`
	Providers       []ProviderRow   `json:"providers"`
	AutoMergeGlobal AutoMergeGlobal `json:"auto_merge_global"`
	Orphans         []OrphanRow     `json:"orphans"`
}

// ProviderRow is one provider as the registry lists it (W3c): addresses and
// the NAME of its token, never a value.
type ProviderRow struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	SSHURL   string `json:"ssh_url"`
	TokenRef string `json:"token_ref"`
}

// OrphanRow is a GitLab project a failed create_project left behind. Values
// only; the broker never deletes it.
type OrphanRow struct {
	Provider  string `json:"provider"`
	Project   string `json:"project"`
	ProjectID *int64 `json:"project_id"`
	Step      string `json:"step"`
	At        string `json:"at"`
	Origin    string `json:"origin"` // "created" or "linked" ("" from an older broker: created)
}

// Projects reads the registry (no signature, no side effect).
func (c *Client) Projects(ctx context.Context) (*RegistryView, *OpError) {
	res, oerr := c.Do(ctx, OpProjects, map[string]any{})
	if oerr != nil {
		return nil, oerr
	}
	var v RegistryView
	if err := json.Unmarshal(res, &v); err != nil {
		return nil, unavailable()
	}
	return &v, nil
}

// Write sends register_project or set_flag once. It never retries: see
// UnknownOutcome for what a failure may mean.
func (c *Client) Write(ctx context.Context, op string, args any) (*Project, *OpError) {
	res, oerr := c.Do(ctx, op, args)
	if oerr != nil {
		return nil, oerr
	}
	var p Project
	if err := json.Unmarshal(res, &p); err != nil {
		return nil, &OpError{Code: "bad_reply"}
	}
	return &p, nil
}

// UnknownOutcome reports whether a failed WRITE may still have happened: the
// request was completely sent and the answer was a broker `timeout`, or none
// arrived intact (dropped connection, garbage, client time limit). A failure
// before the request was fully written, or any other broker refusal, is known
// not to have changed anything.
func (e *OpError) UnknownOutcome() bool {
	return !e.Unsent && (e.Code == CodeTimeout || e.Code == CodeUnavailable)
}
