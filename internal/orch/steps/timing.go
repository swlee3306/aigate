package steps

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"aigate/internal/orch/vm"
)

// maxTimingBytes caps the timing summary appended to a verify note.
const maxTimingBytes = 300

// fmtDur renders a duration for a note: one decimal below 10 s ("1.2s"),
// whole seconds from there on ("52s").
func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if tenths := (d + 50*time.Millisecond) / (100 * time.Millisecond); tenths < 100 {
		return fmt.Sprintf("%d.%ds", tenths/10, tenths%10)
	}
	return fmt.Sprintf("%ds", int64((d+500*time.Millisecond)/time.Second))
}

func fmtSec(s float64) string { return fmtDur(time.Duration(s * float64(time.Second))) }

// failedPhases lists phases for a restore error: "a 10s, b FAILED 3.0s".
func failedPhases(ps []vm.Phase) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.Err != "" {
			parts = append(parts, p.Name+" FAILED "+fmtDur(p.Dur))
		} else {
			parts = append(parts, p.Name+" "+fmtDur(p.Dur))
		}
	}
	return strings.Join(parts, ", ")
}

// timingSummary is the one-line suffix for the verify note:
// " | restore 52s (a 4s, ...) | gate 145s (base 60s, head 80s)". A nil rec
// omits the restore part. It is at most maxTimingBytes long.
func timingSummary(rec *vm.RestoreRecord, restoreWall, gateWall time.Duration, baseSec, headSec float64) string {
	var b strings.Builder
	if rec != nil {
		b.WriteString(" | restore " + fmtDur(restoreWall))
		if len(rec.Phases) > 0 {
			b.WriteString(" (" + failedPhases(rec.Phases) + ")")
		}
	}
	fmt.Fprintf(&b, " | gate %s (base %s, head %s)", fmtDur(gateWall), fmtSec(baseSec), fmtSec(headSec))
	s := strings.Join(strings.Fields(b.String()), " ")
	if !strings.HasPrefix(s, "|") {
		return s
	}
	s = " " + s
	if len(s) > maxTimingBytes {
		s = s[:maxTimingBytes-len("…")]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}
