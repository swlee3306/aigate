// Package gate orchestrates a verification run: it resolves base and head,
// runs each configured check on both commits in isolated worktrees, applies
// the pure rules, and records the report and ledger event.
package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aigate/internal/approval"
	"aigate/internal/config"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/report"
	"aigate/internal/rules"
)

// ToolVersion is recorded in every report.
const ToolVersion = "0.1.0"

// Options configures Run.
type Options struct {
	Repo, Base, Head, TaskClass, ApproveHash, HomeDir string
	// Initial requests initial mode explicitly (no base; head-only green
	// check, config read from head). Base must then be empty.
	Initial bool
}

func sortedCopy(s []string) []string {
	c := append([]string{}, s...)
	sort.Strings(c)
	return c
}

// Home returns the aigate home directory (override, AIGATE_HOME, user home).
func Home(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if h := os.Getenv("AIGATE_HOME"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

// RepoID returns the repo id and absolute toplevel path of repo.
func RepoID(repo string) (id, top string, err error) {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse --show-toplevel in %q: %w", repo, err)
	}
	top = strings.TrimSpace(string(out))
	sum := sha256.Sum256([]byte(top))
	return hex.EncodeToString(sum[:])[:12], top, nil
}

// ApprovalsPath is the approvals store file under home.
func ApprovalsPath(home string) string { return filepath.Join(home, ".aigate", "approvals.json") }

// LedgerPath is the ledger file of a repo under home.
func LedgerPath(home, repoID string) string {
	return filepath.Join(home, ".aigate", "ledger", repoID+".jsonl")
}

var baseCandidates = []string{"origin/HEAD", "origin/main", "origin/master", "main", "master"}

// resolveBase returns the base SHA. With base empty it uses the merge-base of
// head with the first candidate ref; when there is none, or it equals head,
// it is an error: initial mode is only ever entered explicitly
// (Options.Initial).
func resolveBase(repo, base, head string) (string, error) {
	if base != "" {
		return gitx.Resolve(repo, base)
	}
	for _, c := range baseCandidates {
		cs, err := gitx.Resolve(repo, c)
		if err != nil {
			continue
		}
		mb, err := gitx.MergeBase(repo, head, cs)
		if err != nil {
			continue
		}
		if mb == head {
			return "", fmt.Errorf("no base given and the merge-base with %s is head %s itself: pass the base the task started from, or request initial mode explicitly", c, short(head))
		}
		return mb, nil
	}
	return "", errors.New("no base given and no base candidate (origin/HEAD, origin/main, origin/master, main, master) found: pass the base the task started from, or request initial mode explicitly")
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{a, b} {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// BuiltinGuarded are approval_on_add paths that apply whatever the repo
// config says. Each one can change how the runner or its dependencies behave
// without touching any test: a committed node_modules/ can replace the test
// runner itself (npx resolves node_modules/.bin first); .gitattributes can
// change how git presents diffs; .gitmodules, go.work, vendor/, lock files,
// requirements and .npmrc decide which code `setup` installs or the build
// uses; a new pytest.ini/tox.ini/setup.cfg can take over pytest's config;
// sitecustomize/usercustomize run at interpreter start. Like every
// approval_on_add path they are also part of the protected diff, so any
// change to an existing one needs approval too.
var BuiltinGuarded = []string{
	"**/node_modules/**",
	".gitattributes", "**/.gitattributes", ".gitmodules",
	".npmrc", "**/.npmrc",
	"pytest.ini", "**/pytest.ini", "tox.ini", "**/tox.ini", "setup.cfg", "**/setup.cfg",
	"go.work", "**/go.work", "go.work.sum", "**/go.work.sum", "vendor/**", "**/vendor/**",
	"**/sitecustomize.py", "**/usercustomize.py",
	"requirements*.txt", "**/requirements*.txt", "requirements/*.txt", "**/requirements/*.txt",
	"package-lock.json", "**/package-lock.json", "yarn.lock", "**/yarn.lock", "pnpm-lock.yaml", "**/pnpm-lock.yaml",
}

// Protected describes the repo-level protected diff between base and head.
type Protected struct {
	RepoID, Top, Base, Head string
	Diff                    gitx.ProtDiff
	AddedGuarded            []string
	Cfg                     config.Config
}

// NeedsApproval reports whether the protected diff needs a human approval:
// any change to a protected file that exists at base (any hunk, pure text
// additions included — user decision 2026-11-04, option 1; deletion; type,
// mode or binary change), or a new guarded file. A new file that matches
// only the `protected` list is free (spec item [6]).
func (p Protected) NeedsApproval() bool {
	return len(p.Diff.Changed()) > 0 || len(p.AddedGuarded) > 0
}

// Prepare resolves commits and computes the protected diff exactly as Run does.
// Base must resolve (initial mode has no protected diff).
func Prepare(o Options) (Protected, error) {
	id, top, err := RepoID(o.Repo)
	if err != nil {
		return Protected{}, err
	}
	hr := o.Head
	if hr == "" {
		hr = "HEAD"
	}
	head, err := gitx.Resolve(top, hr)
	if err != nil {
		return Protected{}, err
	}
	base, err := resolveBase(top, o.Base, head)
	if err != nil {
		return Protected{}, err
	}
	if base == head {
		return Protected{}, selfCompare(base)
	}
	cfg, err := loadConfig(top, base)
	if err != nil {
		return Protected{}, err
	}
	p := Protected{RepoID: id, Top: top, Base: base, Head: head, Cfg: cfg}
	guarded := union(cfg.ApprovalOnAdd, BuiltinGuarded)
	p.Diff, err = gitx.ProtectedDiff(top, base, head, union(cfg.Protected, guarded))
	if err != nil {
		return Protected{}, err
	}
	p.AddedGuarded, err = gitx.AddedMatching(top, base, head, guarded)
	if err != nil {
		return Protected{}, err
	}
	return p, nil
}

func selfCompare(sha string) error {
	return fmt.Errorf("base and head are the same commit %s: comparing a commit with itself proves nothing (pass the base the task started from, or use initial mode explicitly)", short(sha))
}

// ConfigHashAt returns the hash of the .aigate.yml committed at sha (what a
// report made against base sha records as config_hash).
func ConfigHashAt(repo, sha string) (string, error) {
	cfg, err := loadConfig(repo, sha)
	if err != nil {
		return "", err
	}
	return cfg.Hash(), nil
}

func loadConfig(top, sha string) (config.Config, error) {
	data, err := gitx.Show(top, sha, ".aigate.yml")
	if err != nil {
		return config.Config{}, fmt.Errorf("read .aigate.yml at %s: %w", short(sha), err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return config.Config{}, fmt.Errorf(".aigate.yml at %s: %w", short(sha), err)
	}
	return cfg, nil
}

func isRepoLevel(rule string) bool { return rule == "dirty_tree" || rule == "protected_modified" }

// Run executes the gate. A non-nil error is a GATE ERROR (never a verdict).
func Run(ctx context.Context, o Options) (report.Report, error) {
	started := time.Now().UTC()
	home, err := Home(o.HomeDir)
	if err != nil {
		return report.Report{}, err
	}
	repoID, top, err := RepoID(o.Repo)
	if err != nil {
		return report.Report{}, err
	}
	hr := o.Head
	if hr == "" {
		hr = "HEAD"
	}
	head, err := gitx.Resolve(top, hr)
	if err != nil {
		return report.Report{}, err
	}
	var base string
	switch {
	case o.Initial && o.Base != "":
		return report.Report{}, errors.New("initial mode takes no base")
	case o.Initial:
	default:
		if base, err = resolveBase(top, o.Base, head); err != nil {
			return report.Report{}, err
		}
		if base == head {
			return report.Report{}, selfCompare(base)
		}
	}
	dirty, err := gitx.Dirty(top)
	if err != nil {
		return report.Report{}, err
	}

	r := report.Report{SchemaVersion: 1, Repo: top, Head: head, ToolVersion: ToolVersion,
		StartedAt: started.Format(time.RFC3339), Checks: []report.CheckResult{},
		Violations: []rules.Violation{}, Waived: []string{}, Approvals: []report.ApprovalState{}}
	counts := map[string]any{}

	switch {
	case o.Initial:
		r.Initial = true
		err = runInitial(ctx, top, head, dirty != "", &r, counts)
	default:
		r.Base = base
		var anc bool
		anc, err = gitx.IsAncestor(top, base, head)
		if err != nil {
			break
		}
		if !anc {
			r.Violations = append(r.Violations, rules.Violation{Rule: "base_not_ancestor", Category: rules.CategoryIncomplete,
				Detail: fmt.Sprintf("base %s 는 head %s 의 조상이 아닙니다", short(base), short(head))})
			break
		}
		err = runCompare(ctx, o, home, repoID, top, base, head, dirty != "", &r, counts)
	}
	if err != nil {
		return report.Report{}, err
	}

	r.Category = rules.Verdict(r.Violations)
	r.ExitCode = r.Category.ExitCode()
	r.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	tc := o.TaskClass
	if tc == "" {
		tc = "unclassified"
	}
	// Save first, then bind the saved bytes in the ledger. A report saved
	// without its ledger event (ledger failure = gate error) fails `check`.
	path, err := report.Save(top, r)
	if err != nil {
		return report.Report{}, fmt.Errorf("save report: %w", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		return report.Report{}, fmt.Errorf("read saved report: %w", err)
	}
	sum := sha256.Sum256(saved)
	if err := ledger.Append(LedgerPath(home, repoID), "run_result", repoID, map[string]any{
		"report": short(head), "category": string(r.Category), "base": r.Base, "head": head,
		"task_class": tc, "initial": r.Initial, "checks": counts,
		"config_hash": r.ConfigHash, "report_sha256": hex.EncodeToString(sum[:]),
	}, time.Now().UTC()); err != nil {
		return report.Report{}, fmt.Errorf("ledger (report %s was saved but is not bound; `aigate check` will reject it): %w", path, err)
	}
	return r, nil
}

func runInitial(ctx context.Context, top, head string, dirty bool, r *report.Report, counts map[string]any) error {
	cfg, err := loadConfig(top, head)
	if err != nil {
		return err
	}
	r.ConfigHash = cfg.Hash()
	if dirty {
		r.Violations = append(r.Violations, rules.Violation{Rule: "dirty_tree", Category: rules.CategoryIncomplete,
			Detail: "커밋되지 않은 변경이 있습니다"})
	}
	if !hasRequired(cfg) {
		r.Violations = append(r.Violations, noRequired())
	}
	for _, c := range cfg.Checks {
		hi, hs, err := runSide(ctx, top, head, c)
		if err != nil {
			return err
		}
		r.Checks = append(r.Checks, report.CheckResult{ID: c.ID, Head: hs})
		counts[c.ID] = map[string]any{"head_passed": hs.Counts.Passed, "head_failed": hs.Counts.Failed}
		for _, v := range initialViolations(c.ID, c.Required, hi) {
			r.Violations = append(r.Violations, withCause(v, rules.RunInfo{}, hi))
		}
	}
	return nil
}

func runCompare(ctx context.Context, o Options, home, repoID, top, base, head string, dirty bool, r *report.Report, counts map[string]any) error {
	p, err := Prepare(Options{Repo: top, Base: base, Head: head})
	if err != nil {
		return err
	}
	cfg := p.Cfg
	r.ConfigHash = cfg.Hash()

	hasProt := p.NeedsApproval()
	valid := false
	if hasProt {
		valid = approval.Store{Path: ApprovalsPath(home)}.Valid(repoID, base, p.Diff.Hash)
		r.Approvals = append(r.Approvals, report.ApprovalState{Hash: p.Diff.Hash, Valid: valid})
	}

	seen := map[string]bool{}
	add := func(v rules.Violation) {
		k := v.Rule + "\x00" + v.Detail
		if !seen[k] {
			seen[k] = true
			r.Violations = append(r.Violations, v)
		}
	}
	// Repo-level rules run once, independent of the number of checks.
	repoVs, _ := rules.Evaluate(rules.Input{Dirty: dirty, ProtectedChanged: p.Diff.Changed(),
		AddedGuardedFiles: p.AddedGuarded, ApprovalHashValid: valid})
	for _, v := range repoVs {
		if !isRepoLevel(v.Rule) {
			continue
		}
		if v.Rule == "protected_modified" {
			v.Detail += protectedHint(p.Diff.Hash, o.ApproveHash)
		}
		add(v)
	}
	if !hasRequired(cfg) {
		add(noRequired())
	}
	// All base runs first, then all head runs: head code must not run before
	// any base run, or it could poison shared state ($TMPDIR, caches) and turn
	// base results into waived failures.
	baseRuns := make([]rules.RunInfo, len(cfg.Checks))
	baseSums := make([]report.RunSummary, len(cfg.Checks))
	for i, c := range cfg.Checks {
		if baseRuns[i], baseSums[i], err = runSide(ctx, top, base, c); err != nil {
			return err
		}
	}
	for i, c := range cfg.Checks {
		bi, bs := baseRuns[i], baseSums[i]
		hi, hsum, err := runSide(ctx, top, head, c)
		if err != nil {
			return err
		}
		r.Checks = append(r.Checks, report.CheckResult{ID: c.ID, Base: bs, Head: hsum})
		counts[c.ID] = map[string]any{"head_passed": hsum.Counts.Passed, "head_failed": hsum.Counts.Failed}

		in := rules.Input{CheckID: c.ID, Required: c.Required, Base: bi, Head: hi, ApprovalHashValid: true}
		vs, waived := rules.Evaluate(in)
		for _, v := range vs {
			if isRepoLevel(v.Rule) || !c.Required {
				continue
			}
			v.Detail = "[" + c.ID + "] " + v.Detail
			add(withCause(v, bi, hi))
		}
		if c.Required {
			for _, w := range waived {
				r.Waived = append(r.Waived, c.ID+":"+w)
			}
		}
	}
	return nil
}

func protectedHint(real, given string) string {
	s := fmt.Sprintf(" — diff 해시 %s; 사람이 `aigate approve` 로 승인해야 합니다", real)
	if given != "" && given != real {
		s += fmt.Sprintf(" (--approve %s 는 실제 해시와 달라 무시됨)", given)
	}
	return s
}

func hasRequired(cfg config.Config) bool {
	for _, c := range cfg.Checks {
		if c.Required {
			return true
		}
	}
	return false
}

func noRequired() rules.Violation {
	return rules.Violation{Rule: "no_required_checks", Category: rules.CategoryIncomplete,
		Detail: "필수(required) 검사가 하나도 없어 검증 증거가 없습니다"}
}
