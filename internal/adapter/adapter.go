// Package adapter turns test-runner output into rules.Results keyed by
// normalized test ids, so base and head runs are comparable.
package adapter

import (
	"strings"

	"aigate/internal/rules"
	"golang.org/x/text/unicode/norm"
)

// Parsed is the outcome of parsing one runner's output. ParseFailed means the
// output was unreadable (the caller maps this to verification_incomplete);
// Results may then be partial and must not be trusted.
type Parsed struct {
	Results     rules.Results
	ParseFailed bool
}

// NormalizeID trims, collapses runs of whitespace to one space and applies
// Unicode NFC, so ids from base and head compare equal.
func NormalizeID(s string) string {
	return strings.Join(strings.Fields(norm.NFC.String(s)), " ")
}

// severity orders statuses for duplicate ids: the worst one wins.
func severity(s rules.Status) int {
	switch s {
	case rules.Error:
		return 3
	case rules.Failed:
		return 2
	case rules.Passed:
		return 1
	}
	return 0
}

func put(r rules.Results, id string, s rules.Status) {
	if old, ok := r[id]; !ok || severity(s) > severity(old) {
		r[id] = s
	}
}
