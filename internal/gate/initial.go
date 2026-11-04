package gate

import (
	"fmt"

	"aigate/internal/rules"
)

// initialViolations judges head alone (no base to compare against).
func initialViolations(id string, required bool, h rules.RunInfo) []rules.Violation {
	if !required {
		return nil
	}
	p := "[" + id + "] "
	inc := func(rule, d string) rules.Violation {
		return rules.Violation{Rule: rule, Category: rules.CategoryIncomplete, Detail: p + d}
	}
	var vs []rules.Violation
	switch {
	case h.TimedOut:
		return []rules.Violation{inc("timeout", "head 실행이 시간 초과되었습니다")}
	case h.ParseFailed:
		return []rules.Violation{inc("parse_failed", "head 결과를 해석하지 못했습니다")}
	case len(h.Results) == 0:
		return []rules.Violation{inc("no_tests_ran", fmt.Sprintf("head에서 실행된 결과가 0건입니다 (rc=%d)", h.RC))}
	}
	var failed, errs []string
	for k, s := range h.Results {
		switch s {
		case rules.Failed:
			failed = append(failed, k)
		case rules.Error:
			errs = append(errs, k)
		}
	}
	if rules.RunnerError(h) {
		v := rules.RunnerErrorViolation(h.RC)
		v.Detail = p + v.Detail
		vs = append(vs, v)
	}
	if len(errs) > 0 {
		v := inc("setup_or_collection_error", "head에서 수집·컴파일·설정 오류가 발생했습니다")
		v.IDs = sortedCopy(errs)
		vs = append(vs, v)
	}
	if len(failed) > 0 {
		vs = append(vs, rules.Violation{Rule: "head_failed", Category: rules.CategoryBehavior,
			Detail: p + "초기 모드: head에서 실패한 항목이 있습니다", IDs: sortedCopy(failed)})
	}
	return vs
}
