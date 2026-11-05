package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"aigate/internal/gate"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/policy/autonomy"
	"aigate/internal/orch/policy/propose"
)

const policyUsage = `  propose [--repo R] [--days N] [--json] [--policy P]   승격 제안(읽기 전용, 정책 파일은 바꾸지 않음)
  policy show [--policy P]                                 정책과 유효 단계(강등 반영) 보기
  policy clear-demotion [--policy P] <종류>                 강등 기록 해제(정책 파일은 사람이 먼저 고침)

정책 파일: --policy 또는 ORCH_POLICY / ORCHD_POLICY`

// policyFile resolves the policy path from the flag or the environment.
func policyFile(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return envOr("ORCH_POLICY", os.Getenv("ORCHD_POLICY"))
}

// localCmd runs the commands that read files directly instead of calling orchd.
func localCmd(cmd string, args []string, asJSON bool, gateHomeFlag string, stdout, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "오류:", err); return 1 }
	usageErr := func(err error) int {
		fmt.Fprintln(stderr, "오류:", err)
		fmt.Fprintln(stderr, policyUsage)
		return 2
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&asJSON, "json", asJSON, "")
	polFlag := fs.String("policy", "", "")
	repo := fs.String("repo", ".", "")
	days := fs.Int("days", 14, "")
	sub := ""
	if cmd == "policy" {
		if len(args) == 0 {
			return usageErr(errors.New("policy에는 하위 명령(show, clear-demotion)이 필요합니다"))
		}
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	home, err := gateHome(gateHomeFlag)
	if err != nil {
		return fail(err)
	}
	var pol policy.Policy
	havePol := false
	if p := policyFile(*polFlag); p != "" {
		if pol, err = policy.Load(p); err != nil {
			return fail(err)
		}
		havePol = true
	}
	needPol := func() error {
		if !havePol {
			return errors.New("--policy (또는 ORCH_POLICY)가 필요합니다")
		}
		return nil
	}
	enc := func(v any) int {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}

	switch {
	case cmd == "propose":
		if *days < 1 {
			return usageErr(errors.New("--days는 1 이상이어야 합니다"))
		}
		id, _, err := gate.RepoID(*repo)
		if err != nil {
			return fail(err)
		}
		opts := []propose.Option{}
		if havePol {
			opts = append(opts, propose.WithLevels(func(c string) policy.Level {
				l, _ := autonomy.Effective(pol, c)
				return l
			}))
		}
		ps, err := propose.Load(gate.LedgerPath(home, id), *days, opts...)
		if err != nil {
			return fail(err)
		}
		if asJSON {
			if ps == nil {
				ps = []propose.Proposal{}
			}
			return enc(ps)
		}
		if len(ps) == 0 {
			fmt.Fprintln(stdout, "기간 안에 실행 기록이 없습니다.")
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "종류\t현재\t제안\truns\t연속 ok\t승인필요율\t회귀\t강등\t이유")
		for _, p := range ps {
			to := "-"
			if p.Eligible {
				to = p.ToLevel
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%.0f%%\t%d\t%d\t%s\n", p.Class, p.FromLevel, to, p.Runs,
				p.ConsecutiveOK, 100*p.ApprovalRate, p.Regressions, p.Demotions, p.Reason)
		}
		tw.Flush()
		fmt.Fprintln(stdout, "주: 제안일 뿐 정책 파일은 바뀌지 않았습니다. 승격하려면 사람이 policy.yml을 고치세요.")
		return 0

	case sub == "show":
		if err := needPol(); err != nil {
			return usageErr(err)
		}
		dem, err := autonomy.Demoted(pol)
		if err != nil {
			return fail(err)
		}
		classes := make([]string, 0, len(pol.Classes))
		for c := range pol.Classes {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		type row struct {
			Class     string `json:"class"`
			Policy    string `json:"policy_level"`
			Effective string `json:"effective_level"`
			Demotion  string `json:"demotion,omitempty"`
		}
		var rows []row
		for _, c := range classes {
			eff, _ := autonomy.Effective(pol, c)
			r := row{Class: c, Policy: pol.Classes[c].String(), Effective: eff.String()}
			if e, ok := dem[c]; ok {
				r.Demotion = fmt.Sprintf("%s (%s, task %d, %s)", e.Level, e.Reason, e.Task, e.At)
			}
			rows = append(rows, r)
		}
		out := map[string]any{"path": pol.Path(), "hash": pol.Hash(), "default_level": pol.DefaultLevel.String(),
			"classes": rows, "allowlist": pol.Allowlist, "repo_roots": pol.RepoRoots,
			"limits": map[string]any{"agent_timeout": pol.Limits.AgentTimeout.String(), "max_concurrent": pol.Limits.MaxConcurrent,
				"max_commits": pol.Limits.MaxCommits, "max_fetch_bytes": pol.Limits.MaxFetchBytes}}
		if asJSON {
			return enc(out)
		}
		fmt.Fprintf(stdout, "정책 %s\n  해시: %s\n  기본 단계: %s\n  허용 목록: %v\n  저장소 루트: %v\n  제한: %v\n",
			pol.Path(), pol.Hash(), pol.DefaultLevel, pol.Allowlist, pol.RepoRoots, out["limits"])
		for _, r := range rows {
			fmt.Fprintf(stdout, "  종류 %-12s 정책 %s 유효 %s %s\n", r.Class, r.Policy, r.Effective, r.Demotion)
		}
		return 0

	case sub == "clear-demotion":
		if err := needPol(); err != nil {
			return usageErr(err)
		}
		if fs.NArg() != 1 {
			return usageErr(errors.New("clear-demotion에는 종류 하나가 필요합니다"))
		}
		class := fs.Arg(0)
		err := autonomy.ClearDemotion(pol, class, policy.OrchLedgerPath(home), policy.OrchRepo, time.Now())
		if err != nil {
			return fail(err)
		}
		if asJSON {
			return enc(map[string]string{"class": class, "level": pol.LevelFor(class).String()})
		}
		fmt.Fprintf(stdout, "종류 %s의 강등 기록을 지웠습니다. 이제 정책 파일의 단계(%s)가 적용됩니다.\n", class, pol.LevelFor(class))
		return 0
	}
	return usageErr(fmt.Errorf("알 수 없는 policy 하위 명령: %q", sub))
}
