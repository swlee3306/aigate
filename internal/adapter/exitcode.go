package adapter

import "aigate/internal/rules"

// ExitCode is the pass/fail-only adapter (builds): the single result is keyed
// by the check id; rc==0 is passed, anything else failed.
func ExitCode(id string, rc int) Parsed {
	st := rules.Failed
	if rc == 0 {
		st = rules.Passed
	}
	return Parsed{Results: rules.Results{NormalizeID(id): st}}
}
