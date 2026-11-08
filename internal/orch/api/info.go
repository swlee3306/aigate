package api

import (
	"net/http"
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

// OpsView is the operations summary. W1 has no VM or quota numbers.
type OpsView struct {
	Build            string       `json:"build"`
	Started          time.Time    `json:"started"`
	PolicyHash       string       `json:"policy_hash"`
	LastRestore      *RestoreView `json:"last_restore,omitempty"`
	BaseCacheEntries int          `json:"base_cache_entries"`
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
	writeJSON(w, 200, h.ops())
}
