package adapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"aigate/internal/rules"
)

type goEvent struct {
	Action      string
	Package     string
	Test        string
	FailedBuild string
}

// ParseGoTestJSON parses `go test -json` output. Test id is
// "package/Test/Sub". A package that failed to build yields "package[build]"
// = error; a package that failed with no failing test (TestMain, init panic)
// yields "package[package]" = error; a test that started but never reported a
// result (panic, timeout) is error.
func ParseGoTestJSON(r io.Reader) Parsed {
	res := rules.Results{}
	running := map[string]bool{}
	failedIn := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e goEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return Parsed{Results: res, ParseFailed: true}
		}
		if e.Test == "" {
			if e.Action == "fail" {
				if e.FailedBuild != "" {
					put(res, NormalizeID(e.Package+"[build]"), rules.Error)
				} else if !failedIn[e.Package] {
					put(res, NormalizeID(e.Package+"[package]"), rules.Error)
				}
			}
			continue
		}
		id := NormalizeID(e.Package + "/" + e.Test)
		switch e.Action {
		case "run":
			running[id] = true
		case "pass":
			delete(running, id)
			put(res, id, rules.Passed)
		case "fail":
			delete(running, id)
			failedIn[e.Package] = true
			put(res, id, rules.Failed)
		case "skip":
			delete(running, id)
			put(res, id, rules.Skipped)
		}
	}
	if sc.Err() != nil {
		return Parsed{Results: res, ParseFailed: true}
	}
	for id := range running {
		put(res, id, rules.Error)
	}
	return Parsed{Results: res}
}
