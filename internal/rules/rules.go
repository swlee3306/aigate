// Package rules holds the pure verdict rules: given a base run and a head run
// of one check, decide which invariants were broken. No I/O.
package rules

import (
	"fmt"
	"sort"
	"strings"
)

type Status string

const (
	Passed  Status = "passed"
	Failed  Status = "failed"
	Error   Status = "error"
	Skipped Status = "skipped"
)

type Results map[string]Status

type Category string

const (
	CategoryOK         Category = "ok"
	CategoryBehavior   Category = "behavior_failure"
	CategoryIncomplete Category = "verification_incomplete"
	CategoryWeakened   Category = "check_weakened"
)

// ExitCode maps a category to the process exit code (0/1/2/3).
func (c Category) ExitCode() int {
	switch c {
	case CategoryOK:
		return 0
	case CategoryBehavior:
		return 1
	case CategoryIncomplete:
		return 2
	case CategoryWeakened:
		return 3
	}
	return 2 // unknown category is never treated as success
}

type RunInfo struct {
	Results     Results
	RC          int
	TimedOut    bool
	ParseFailed bool
	// Tail is the cleaned end of the runner output, kept only for diagnostics;
	// the rules never read it.
	Tail string
}

type Input struct {
	CheckID    string
	Required   bool
	Base, Head RunInfo
	Dirty      bool
	// ProtectedChanged lists the protected files that existed at base and
	// were changed in any way at head: any text hunk (pure additions
	// included, user decision 2026-11-04 option 1), deletion (also of an
	// empty file), type or mode change, binary content change. Any entry
	// needs approval. Files new at head are not listed here (see
	// AddedGuardedFiles).
	ProtectedChanged  []string
	AddedGuardedFiles []string
	ApprovalHashValid bool
}

type Violation struct {
	Rule     string   `json:"rule"`
	Category Category `json:"category"`
	Detail   string   `json:"detail"`
	IDs      []string `json:"ids,omitempty"`
}

func viol(rule string, c Category, detail string, ids []string) Violation {
	return Violation{Rule: rule, Category: c, Detail: detail, IDs: ids}
}

// Evaluate applies the invariant "every id passed at base must pass at head".
// It acts on exactly the input it is given; repo-level protected-diff fields
// are set by the caller only where they should apply.
func Evaluate(in Input) (violations []Violation, waived []string) {
	if in.Dirty {
		violations = append(violations, viol("dirty_tree", CategoryIncomplete, "커밋되지 않은 변경이 있습니다", nil))
	}
	violations = append(violations, baseRules(in)...)

	headOK := true
	if in.Head.TimedOut {
		headOK = false
		violations = append(violations, viol("timeout", CategoryIncomplete, "head 실행이 시간 초과되었습니다", nil))
	}
	if in.Head.ParseFailed {
		headOK = false
		violations = append(violations, viol("parse_failed", CategoryIncomplete, "head 결과를 해석하지 못했습니다", nil))
	} else if len(in.Head.Results) == 0 && !in.Head.TimedOut {
		headOK = false
		violations = append(violations, viol("no_tests_ran", CategoryIncomplete, "head에서 실행된 테스트가 0건입니다", nil))
	}
	if headOK {
		if RunnerError(in.Head) {
			violations = append(violations, runnerErrorViolation(in.Head.RC))
		}
		vs, w := compare(in.Base.Results, in.Head.Results)
		violations = append(violations, vs...)
		waived = w
	}

	if !in.ApprovalHashValid {
		if files := sorted(dedupe(in.ProtectedChanged)); len(files) > 0 {
			violations = append(violations, viol("protected_modified", CategoryWeakened,
				fmt.Sprintf("기존 보호 파일 %d개가 변경·삭제되었습니다(줄 추가만 해도 승인 대상): %s (승인 없음)", len(files), nameList(files)), files))
		}
		if len(in.AddedGuardedFiles) > 0 {
			violations = append(violations, viol("protected_modified", CategoryWeakened,
				"동작을 바꿀 수 있는 파일이 새로 추가되었습니다 (승인 없음)", sorted(in.AddedGuardedFiles)))
		}
	}
	return violations, waived
}

// RunnerError reports a run whose runner exited non-zero although no result
// is failed or error (e.g. a crash or INTERNALERROR after the report file was
// written): the results do not explain the exit status, so they cannot be
// trusted as a green run.
func RunnerError(r RunInfo) bool {
	if r.RC == 0 || r.TimedOut || r.ParseFailed || len(r.Results) == 0 {
		return false
	}
	for _, s := range r.Results {
		if s == Failed || s == Error {
			return false
		}
	}
	return true
}

func runnerErrorViolation(rc int) Violation {
	return viol("runner_error", CategoryIncomplete,
		fmt.Sprintf("head 러너가 실패 코드(rc=%d)로 끝났지만 실패·오류 결과가 없습니다(러너 자체 오류)", rc), nil)
}

// RunnerErrorViolation is the runner_error violation for callers outside
// Evaluate (initial mode).
func RunnerErrorViolation(rc int) Violation { return runnerErrorViolation(rc) }

func baseRules(in Input) []Violation {
	var vs []Violation
	unreliable := in.Base.TimedOut || in.Base.ParseFailed || len(in.Base.Results) == 0
	if !unreliable {
		for _, s := range in.Base.Results {
			if s == Error {
				unreliable = true
				break
			}
		}
	}
	if unreliable {
		vs = append(vs, viol("base_unreliable", CategoryIncomplete, "base 실행이 불안정합니다(시간 초과·결과 없음·오류)", nil))
	}
	if in.Required && !in.Base.ParseFailed && len(in.Base.Results) > 0 {
		// A non-zero rc is normal when some base tests fail (those are waived).
		// It is only "not green" when there is nothing to protect (no passed id)
		// or the failure is not explained by a failed id.
		var passed, failed int
		for _, s := range in.Base.Results {
			switch s {
			case Passed:
				passed++
			case Failed:
				failed++
			}
		}
		if passed == 0 || (in.Base.RC != 0 && failed == 0) {
			vs = append(vs, viol("base_not_green", CategoryIncomplete,
				fmt.Sprintf("필수 검사의 base에 보호할 통과 항목이 없거나 실패 원인이 불명확합니다 (rc=%d, passed=%d, failed=%d)", in.Base.RC, passed, failed), nil))
		}
	}
	return vs
}

func compare(base, head Results) ([]Violation, []string) {
	var regress, missing, skipped, errored, newFail, newSkip, waived []string
	for _, id := range sortedKeys(base) {
		bs := base[id]
		hs, present := head[id]
		if bs == Passed {
			switch {
			case !present:
				missing = append(missing, id)
			case hs == Failed:
				regress = append(regress, id)
			case hs == Skipped:
				skipped = append(skipped, id)
			case hs == Error:
				errored = append(errored, id)
			}
			continue
		}
		if present && (hs == Failed || hs == Skipped) {
			waived = append(waived, id)
		} else if present && hs == Error {
			errored = append(errored, id)
		}
	}
	var added []string
	for _, id := range sortedKeys(head) {
		if _, ok := base[id]; ok {
			continue
		}
		added = append(added, id)
		switch head[id] {
		case Failed:
			newFail = append(newFail, id)
		case Skipped:
			newSkip = append(newSkip, id)
		case Error:
			errored = append(errored, id)
		}
	}

	var vs []Violation
	if len(errored) > 0 {
		vs = append(vs, viol("setup_or_collection_error", CategoryIncomplete, "head에서 수집·컴파일·설정 오류가 발생했습니다", errored))
	}
	if len(regress) > 0 {
		vs = append(vs, viol("regression", CategoryBehavior, "base에서 통과하던 테스트가 head에서 실패합니다", regress))
	}
	if len(missing) > 0 {
		vs = append(vs, viol("test_missing", CategoryWeakened, missingDetail(missing, added), missing))
	}
	if len(skipped) > 0 {
		vs = append(vs, viol("test_skipped", CategoryWeakened, "base에서 통과하던 테스트가 head에서 skip됩니다", skipped))
	}
	if len(newFail) > 0 {
		vs = append(vs, viol("new_test_fails", CategoryBehavior, "새 테스트가 실패합니다", newFail))
	}
	if len(newSkip) > 0 {
		vs = append(vs, viol("new_test_skipped", CategoryWeakened, "새 테스트가 skip됩니다", newSkip))
	}
	return vs, waived
}

// missingDetail adds a rename hint for a removed id that pairs 1:1 with an
// added id, either by the same suffix (last path / "::" segment) or by the
// same parent path (everything before the last segment).
func missingDetail(missing, added []string) string {
	d := "base에서 통과하던 테스트가 head에 없습니다(삭제·이름 변경·deselect)"
	var hints []string
	seen := map[string]bool{}
	for _, key := range []func(string) string{suffix, parent} {
		byMissing := map[string][]string{}
		for _, id := range missing {
			byMissing[key(id)] = append(byMissing[key(id)], id)
		}
		byAdded := map[string][]string{}
		for _, id := range added {
			byAdded[key(id)] = append(byAdded[key(id)], id)
		}
		for _, id := range missing {
			k := key(id)
			if k == "" {
				continue
			}
			if len(byMissing[k]) == 1 && len(byAdded[k]) == 1 {
				h := fmt.Sprintf("%s -> %s", id, byAdded[k][0])
				if !seen[h] {
					seen[h] = true
					hints = append(hints, h)
				}
			}
		}
	}
	if len(hints) > 0 {
		d += "; 이름 변경 의심: " + strings.Join(hints, ", ")
	}
	return d
}

// parent is the id without its last segment ("::" separated if present, else
// "/" separated); "" when there is no separator.
func parent(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[:i]
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i]
	}
	return ""
}

func suffix(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		id = id[i+2:]
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// Verdict returns the highest-priority category:
// incomplete > weakened > behavior > ok. A violation with an unknown (or
// empty) category counts as incomplete: a violation never passes as ok.
func Verdict(vs []Violation) Category {
	rank := map[Category]int{CategoryOK: 0, CategoryBehavior: 1, CategoryWeakened: 2, CategoryIncomplete: 3}
	best := CategoryOK
	for _, v := range vs {
		c := v.Category
		if _, known := rank[c]; !known || c == CategoryOK {
			c = CategoryIncomplete
		}
		if rank[c] > rank[best] {
			best = c
		}
	}
	return best
}

func sortedKeys(r Results) []string {
	ks := make([]string, 0, len(r))
	for k := range r {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// dedupe returns the distinct elements of s in first-seen order.
func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// nameList joins file names for a violation detail, at most 20 of them.
func nameList(files []string) string {
	const max = 20
	if len(files) <= max {
		return strings.Join(files, ", ")
	}
	return strings.Join(files[:max], ", ") + fmt.Sprintf(" 외 %d개", len(files)-max)
}

func sorted(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
