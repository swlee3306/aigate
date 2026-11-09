package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"aigate/internal/orch/store"
)

// StepTime is one state a task entered and how long it stayed there.
type StepTime struct {
	State   string    `json:"state"`
	Seconds float64   `json:"seconds"`
	Note    string    `json:"note"`
	At      time.Time `json:"at"`
	// Actor is who moved the task INTO this step ("kind:id", "system" or
	// "invalid"; empty for events recorded before schema v7). It is the value
	// the server recorded, never client input.
	Actor string `json:"actor,omitempty"`
}

// VerifyBreakdown is the timing summary parsed from a verify note.
type VerifyBreakdown struct {
	RestoreSec    float64 `json:"restore_sec"`
	GateSec       float64 `json:"gate_sec"`
	BaseSec       float64 `json:"base_sec"`
	HeadSec       float64 `json:"head_sec"`
	BaseCacheHits int     `json:"base_cache_hits"`
	BaseCacheRuns int     `json:"base_cache_runs"`
}

// Timeline turns a task's events (oldest first) into steps. A step lasts
// until the next event; the last one is still current (or final) and gets 0.
func Timeline(evs []store.Event) []StepTime {
	out := make([]StepTime, len(evs))
	for i, ev := range evs {
		out[i] = StepTime{State: string(ev.To), Note: ev.Note, At: ev.At, Actor: ev.Actor}
		if i+1 < len(evs) {
			out[i].Seconds = evs[i+1].At.Sub(ev.At).Seconds()
		}
	}
	return out
}

// verifyNoteRE matches the suffix steps.timingSummary appends:
// "restore 55s (...) | gate 153s (base 79s, head 71s) | base cache 1/1".
// The restore part and the base cache part are optional.
var verifyNoteRE = regexp.MustCompile(
	`(?:restore (\d+(?:\.\d+)?)s(?: \([^)]*\))? \| )?` +
		`gate (\d+(?:\.\d+)?)s \(base (\d+(?:\.\d+)?)s, head (\d+(?:\.\d+)?)s\)` +
		`(?: \| base cache (\d+)/(\d+))?`)

// ParseVerifyNote reads the timing figures from a verify note. ok is false
// when the note carries no gate timing.
func ParseVerifyNote(note string) (VerifyBreakdown, bool) {
	m := verifyNoteRE.FindStringSubmatch(note)
	if m == nil {
		return VerifyBreakdown{}, false
	}
	f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return VerifyBreakdown{RestoreSec: f(m[1]), GateSec: f(m[2]), BaseSec: f(m[3]), HeadSec: f(m[4]),
		BaseCacheHits: n(m[5]), BaseCacheRuns: n(m[6])}, true
}

// events serves GET /api/v1/tasks/{id}/events: the task, its timeline and the
// timing breakdown of the latest verify note that has one (null otherwise).
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.st.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &httpError{code: 404, msg: "작업을 찾을 수 없습니다"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	evs, err := h.st.Events(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var verify *VerifyBreakdown
	for i := len(evs) - 1; i >= 0; i-- {
		if b, ok := ParseVerifyNote(evs[i].Note); ok {
			verify = &b
			break
		}
	}
	writeJSON(w, 200, map[string]any{"task": viewOf(t), "steps": Timeline(evs), "verify": verify})
}
