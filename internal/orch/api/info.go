package api

import (
	"context"
	"net/http"
	"regexp"
	"time"
)

// projectTaskLimit bounds how many of the newest tasks the projects
// aggregation reads.
const projectTaskLimit = 5000

// PolicyView is the read-only picture of the policy in force. It carries no
// paths: only the hash, the per-class levels, the allowlist and the hard floor.
type PolicyView struct {
	Hash      string            `json:"hash"`
	Classes   map[string]string `json:"classes"`
	Allowlist []string          `json:"allowlist"`
	Floor     []string          `json:"floor"`
	BaseCache bool              `json:"base_cache"`
}

// RestorePhase is one timed phase of the last test VM restore.
type RestorePhase struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	OK      bool    `json:"ok"`
}

// RestoreView summarises the last test VM restore (no error text, no volume ids).
type RestoreView struct {
	Phases  []RestorePhase `json:"phases"`
	Seconds float64        `json:"seconds"`
	At      time.Time      `json:"at"`
	OK      bool           `json:"ok"`
}

// cloudBudget is how long /ops waits for the cloud block. It must stay below
// the web client's request timeout (15 s) so a slow cloud yields a partial
// answer (cloud_timeout) instead of an aborted request; the supplier's own
// refresh is detached and keeps running, so the next poll gets the value.
// A var so tests can shorten it.
var cloudBudget = 6 * time.Second

// Fixed failure codes of CloudView (no error text ever reaches the client).
const (
	CloudUnavailable = "cloud_unavailable"
	CloudTimeout     = "cloud_timeout"
)

// CloudLimit is one Cinder quota line: Limit, InUse and Reserved are the raw
// numbers (the limit is -1 when Unlimited).
type CloudLimit struct {
	Limit     int  `json:"limit"`
	InUse     int  `json:"in_use"`
	Reserved  int  `json:"reserved"`
	Unlimited bool `json:"unlimited"`
}

// QuotaView is the part of the Cinder quota that fails restores (HTTP 413):
// gigabytes counts volumes and snapshots together.
type QuotaView struct {
	Gigabytes CloudLimit `json:"gigabytes"`
	Volumes   CloudLimit `json:"volumes"`
}

// VMState is the test VM's name and server status.
type VMState struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// CloudView is the cached cloud picture of /ops. On failure only OK=false and
// a fixed Code are set; there is never error text, host name or CLI output.
type CloudView struct {
	OK             bool          `json:"ok"`
	Code           string        `json:"code"`
	Quota          *QuotaView    `json:"quota,omitempty"`
	TestVM         *VMState      `json:"test_vm,omitempty"`
	RestoreVolumes int           `json:"restore_volumes"`
	History        []RestoreView `json:"history"`
	CheckedAt      time.Time     `json:"checked_at"`
}

// OpsView is the operations summary. Cloud is present only when a cloud
// status supplier is configured.
type OpsView struct {
	Build            string       `json:"build"`
	Started          time.Time    `json:"started"`
	PolicyHash       string       `json:"policy_hash"`
	LastRestore      *RestoreView `json:"last_restore,omitempty"`
	BaseCacheEntries int          `json:"base_cache_entries"`
	Cloud            *CloudView   `json:"cloud,omitempty"`
}

type projectRow struct {
	Repo     string `json:"repo"`
	Class    string `json:"class"`
	Tasks    int    `json:"tasks"`
	LastTask int64  `json:"last_task"`
}

// projects aggregates tasks by repository, newest repository first. The class
// is the class of the repository's newest task.
func (h *handler) projects(w http.ResponseWriter, r *http.Request) {
	ts, err := h.st.ListTasks(r.Context(), projectTaskLimit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := []projectRow{}
	idx := map[string]int{}
	for _, t := range ts { // newest first
		if i, ok := idx[t.Repo]; ok {
			out[i].Tasks++
			continue
		}
		idx[t.Repo] = len(out)
		out = append(out, projectRow{Repo: t.Repo, Class: t.Class, Tasks: 1, LastTask: t.ID})
	}
	writeJSON(w, 200, out)
}

func (h *handler) policyView(w http.ResponseWriter, r *http.Request) {
	if h.policy == nil {
		writeJSON(w, 200, struct{}{})
		return
	}
	v := h.policy()
	if v.Classes == nil {
		v.Classes = map[string]string{}
	}
	if v.Allowlist == nil {
		v.Allowlist = []string{}
	}
	if v.Floor == nil {
		v.Floor = []string{}
	}
	writeJSON(w, 200, v)
}

func (h *handler) opsView(w http.ResponseWriter, r *http.Request) {
	if h.ops == nil {
		writeJSON(w, 200, struct{}{})
		return
	}
	v := h.ops()
	if h.cloud != nil {
		ctx, cancel := context.WithTimeout(r.Context(), cloudBudget)
		c := h.cloudView(ctx)
		cancel()
		v.Cloud = &c
	}
	writeJSON(w, 200, v)
}

// cloudView asks the supplier; a panic in it is a plain cloud_unavailable and
// never takes the rest of /ops down.
func (h *handler) cloudView(ctx context.Context) (c CloudView) {
	defer func() {
		if recover() != nil {
			c = CloudView{Code: CloudUnavailable, History: []RestoreView{}}
		}
	}()
	c = normalizeCloud(h.cloud(ctx))
	return c
}

// vmStatusRe is what a server status may look like on the wire (Nova's
// words such as ACTIVE, SHUTOFF, VERIFY_RESIZE, or "unknown").
var vmStatusRe = regexp.MustCompile(`^[A-Za-z][A-Za-z_]{0,31}$`)

// vmNameRe bounds the test VM name shown next to the status.
var vmNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// normalizeCloud is the last gate before ops.cloud reaches a client, whatever
// the supplier returned: a failure carries only one of the two fixed codes
// and no quota or VM block; a success carries no code, a status word (else
// "unknown") and a plain VM name (else empty). No free text gets through.
func normalizeCloud(c CloudView) CloudView {
	if c.History == nil {
		c.History = []RestoreView{}
	}
	if !c.OK {
		if c.Code != CloudTimeout {
			c.Code = CloudUnavailable
		}
		c.Quota, c.TestVM, c.RestoreVolumes, c.History = nil, nil, 0, []RestoreView{}
		return c
	}
	c.Code = ""
	if c.TestVM != nil {
		vm := *c.TestVM
		if !vmStatusRe.MatchString(vm.Status) {
			vm.Status = "unknown"
		}
		if !vmNameRe.MatchString(vm.Name) {
			vm.Name = ""
		}
		c.TestVM = &vm
	}
	return c
}
