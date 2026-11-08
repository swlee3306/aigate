package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

func notesOf(t *testing.T, e *e2e, id int64) string {
	t.Helper()
	evs, err := e.st.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, ev := range evs {
		b.WriteString(ev.Note + "\n")
	}
	return b.String()
}

// Two tasks on the same base through the real daemon, steps and gate: the
// first runs the base checks, the second reuses them and reaches the same verdict.
func TestBaseResultsReusedAcrossTasks(t *testing.T) {
	e2eBaseCache = &steps.BaseCache{Dir: filepath.Join(t.TempDir(), "bc"), TTL: time.Hour, Scope: "test"}
	defer func() { e2eBaseCache = nil }()
	// a new file under src/ is an unprotected change: verdict ok, no approval
	e := newE2E(t, map[string]string{"src/new.txt": "n\n"}, steps.LocalOnlyPublisher{})
	id1 := e.submit()
	e.waitState(id1, store.StateDone)
	id2 := e.submit()
	e.waitState(id2, store.StateDone)
	n1, n2 := notesOf(t, e, id1), notesOf(t, e, id2)
	if !strings.Contains(n1, "base cache 0/1") || !strings.Contains(n2, "base cache 1/1") {
		t.Fatalf("first task notes:\n%s\nsecond task notes:\n%s", n1, n2)
	}
	for _, id := range []int64{id1, id2} {
		if tk, _ := e.st.GetTask(context.Background(), id); tk.Verdict != "ok" {
			t.Fatalf("task %d verdict %q", id, tk.Verdict)
		}
	}
}
