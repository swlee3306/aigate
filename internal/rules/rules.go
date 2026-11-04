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
}

type Input struct {
	CheckID           string
	Required          bool
	Base, Head        RunInfo
	Dirty             bool
	ProtectedRemoved  int
	ProtectedFiles    []string
	AddedGuardedFiles []string
	ApprovalHashValid bool
}

type Violation struct {
	Rule     string
	Category Category
	Detail   string
	IDs      []string
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
		vs, w := compare(in.Base.Results, in.Head.Results)
		violations = append(violations, vs...)
		waived = w
	}

	if !in.ApprovalHashValid {
		if in.ProtectedRemoved > 0 {
			violations = append(violations, viol("protected_modified", CategoryWeakened,
				fmt.Sprintf("보호 파일의 기존 줄 %d개가 변경·삭제되었습니다 (승인 없음)", in.ProtectedRemoved), sorted(in.ProtectedFiles)))
		}
		if len(in.AddedGuardedFiles) > 0 {
			violations = append(violations, viol("protected_modified", CategoryWeakened,
				"동작을 바꿀 수 있는 파일이 새로 추가되었습니다 (승인 없음)", sorted(in.AddedGuardedFiles)))
		}
	}
	return violations, waived
}

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
	if in.Required && in.Base.RC != 0 {
		vs = append(vs, viol("base_not_green", CategoryIncomplete,
			fmt.Sprintf("필수 검사가 base에서 이미 실패합니다 (rc=%d)", in.Base.RC), nil))
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

// missingDetail adds a rename hint when a vanished id and exactly one new id
// share the same suffix (last path / "::" segment).
func missingDetail(missing, added []string) string {
	d := "base에서 통과하던 테스트가 head에 없습니다(삭제·이름 변경·deselect)"
	byMissing := map[string][]string{}
	for _, id := range missing {
		byMissing[suffix(id)] = append(byMissing[suffix(id)], id)
	}
	byAdded := map[string][]string{}
	for _, id := range added {
		byAdded[suffix(id)] = append(byAdded[suffix(id)], id)
	}
	var hints []string
	for _, id := range missing {
		s := suffix(id)
		if len(byMissing[s]) == 1 && len(byAdded[s]) == 1 {
			hints = append(hints, fmt.Sprintf("%s -> %s", id, byAdded[s][0]))
		}
	}
	if len(hints) > 0 {
		d += "; 이름 변경 의심: " + strings.Join(hints, ", ")
	}
	return d
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
// incomplete > weakened > behavior > ok.
func Verdict(vs []Violation) Category {
	rank := map[Category]int{CategoryOK: 0, CategoryBehavior: 1, CategoryWeakened: 2, CategoryIncomplete: 3}
	best := CategoryOK
	for _, v := range vs {
		if rank[v.Category] > rank[best] {
			best = v.Category
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

func sorted(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
