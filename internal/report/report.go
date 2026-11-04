// Package report defines the evidence report (schema v1), its Markdown view,
// persistence, and the freshness check run before completion claims.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aigate/internal/rules"
	"aigate/internal/textsafe"
)

type Counts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Error   int `json:"error"`
	Skipped int `json:"skipped"`
}

type RunSummary struct {
	RC      int     `json:"rc"`
	Counts  Counts  `json:"counts"`
	Seconds float64 `json:"seconds"`
}

type CheckResult struct {
	ID   string     `json:"id"`
	Base RunSummary `json:"base"`
	Head RunSummary `json:"head"`
}

type ApprovalState struct {
	Hash  string `json:"hash"`
	Valid bool   `json:"valid"`
}

type Report struct {
	SchemaVersion int               `json:"schema_version"`
	Category      rules.Category    `json:"category"`
	ExitCode      int               `json:"exit_code"`
	Repo          string            `json:"repo"`
	Base          string            `json:"base"`
	Head          string            `json:"head"`
	ConfigHash    string            `json:"config_hash"`
	ToolVersion   string            `json:"tool_version"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    string            `json:"finished_at"`
	Checks        []CheckResult     `json:"checks"`
	Violations    []rules.Violation `json:"violations"`
	Waived        []string          `json:"waived"`
	Approvals     []ApprovalState   `json:"approvals"`
	Initial       bool              `json:"initial"`
}

// JSON renders the report with 2-space indent. Nil slices become [] and
// waived ids are sorted; the receiver is not modified.
func (r Report) JSON() []byte {
	if r.Checks == nil {
		r.Checks = []CheckResult{}
	}
	if r.Violations == nil {
		r.Violations = []rules.Violation{}
	}
	if r.Approvals == nil {
		r.Approvals = []ApprovalState{}
	}
	w := append([]string{}, r.Waived...)
	sort.Strings(w)
	r.Waived = w
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil { // only plain data types; cannot happen
		panic(err)
	}
	return append(b, '\n')
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// oneLine renders a value on one markdown line with every control, invalid
// UTF-8 or bidi byte escaped (`\x1b`, `\xff`, `\u202e`), so a file name or
// runner tail in a detail cannot drive the terminal `run` prints to.
func oneLine(s string) string {
	return textsafe.Escape(strings.Join(strings.Fields(s), " "))
}

func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 검증 보고서: %s (exit %d)\n\n", r.Category, r.ExitCode)
	fmt.Fprintf(&b, "- head: `%s`\n- base: `%s`\n", short(r.Head), short(r.Base))
	if r.Initial {
		b.WriteString("- 모드: 초기(initial)\n")
	}
	b.WriteString("\n## 검사\n\n")
	if len(r.Checks) == 0 {
		b.WriteString("- 없음\n")
	}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "- **%s**: base %s / head %s\n", oneLine(c.ID), countsLine(c.Base), countsLine(c.Head))
	}
	b.WriteString("\n## 승인\n\n")
	if len(r.Approvals) == 0 {
		b.WriteString("- 없음\n")
	}
	for _, a := range r.Approvals {
		st := "무효"
		if a.Valid {
			st = "유효"
		}
		fmt.Fprintf(&b, "- 보호 파일 diff `%s`: %s\n", short(a.Hash), st)
	}
	b.WriteString("\n## 위반\n\n")
	if len(r.Violations) == 0 {
		b.WriteString("- 없음\n")
	}
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "- [%s] **%s**: %s\n", v.Category, oneLine(v.Rule), oneLine(v.Detail))
	}
	return b.String()
}

func countsLine(s RunSummary) string {
	c := s.Counts
	return fmt.Sprintf("rc=%d 통과 %d 실패 %d 오류 %d 건너뜀 %d", s.RC, c.Passed, c.Failed, c.Error, c.Skipped)
}

// LedgerRun is the last run_result ledger event for (repo, head, base).
type LedgerRun struct{ Category, ConfigHash, ReportSHA256 string }

// Expect is what the current repository state says a fresh report must match.
type Expect struct {
	Head, Base, Repo, ConfigHash string
	Dirty                        bool
	ApprovalValid                func(hash string) bool
	ReportSHA256                 string
	LedgerErr                    error
	LedgerRun                    *LedgerRun
}

// Check returns Korean problem descriptions; empty means the evidence is
// fresh. It is pure: the caller (gate.CheckReport) gathers Expect from git,
// the base config and the ledger. A report is fresh only when it is an ok,
// internally consistent compare-mode report for exactly this repo, HEAD,
// base and base config, and the ledger's last run_result for (head, base)
// says ok with the same config hash and the sha256 of this very file.
func Check(r Report, e Expect) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	if r.SchemaVersion != 1 {
		add("보고서 스키마 버전이 지원되지 않습니다: %d", r.SchemaVersion)
	}
	if r.Head == "" || r.Head != e.Head {
		add("증거는 %s 기준인데 현재 HEAD는 %s 입니다", short(r.Head), short(e.Head))
	}
	if e.Dirty {
		p = append(p, "작업 트리에 커밋되지 않은 변경이 있습니다")
	}
	if r.Category != rules.CategoryOK {
		add("보고서 판정이 ok가 아닙니다: %s", r.Category)
	}
	switch {
	case r.Initial:
		p = append(p, "초기(initial) 모드 보고서는 회귀 비교가 없어 완료 증거로 인정하지 않습니다")
	case r.Base == "" || r.Base != e.Base:
		add("보고서의 base %q 가 지정한 base %s 와 다릅니다", short(r.Base), short(e.Base))
	}
	if r.Base != "" && r.Base == r.Head {
		p = append(p, "보고서의 base와 head가 같은 커밋입니다(자기 비교는 증거가 아님)")
	}
	if r.Repo != e.Repo {
		add("보고서의 저장소 %q 가 현재 저장소 %q 와 다릅니다", r.Repo, e.Repo)
	}
	if r.ConfigHash == "" || r.ConfigHash != e.ConfigHash {
		add("보고서의 설정 해시 %q 가 base의 .aigate.yml 해시 %q 와 다릅니다", r.ConfigHash, e.ConfigHash)
	}
	if r.Category == rules.CategoryOK && (len(r.Violations) > 0 || r.ExitCode != 0) {
		add("보고서가 모순됩니다: 판정 ok인데 위반 %d건, 종료 코드 %d", len(r.Violations), r.ExitCode)
	}
	for _, a := range r.Approvals {
		switch {
		case !a.Valid && r.Category == rules.CategoryOK:
			add("보고서가 모순됩니다: 판정 ok인데 무효 승인 %s 이(가) 있습니다", short(a.Hash))
		case a.Valid && (e.ApprovalValid == nil || !e.ApprovalValid(a.Hash)):
			add("보고서가 의존한 승인 %s 이(가) 더 이상 유효하지 않습니다", short(a.Hash))
		}
	}
	switch lr := e.LedgerRun; {
	case e.LedgerErr != nil:
		add("이력(ledger)을 검증할 수 없습니다: %v", e.LedgerErr)
	case lr == nil:
		p = append(p, "이력(ledger)에 이 head·base의 run_result가 없습니다(aigate run 없이 만든 보고서)")
	default:
		if lr.Category != string(rules.CategoryOK) {
			add("이력(ledger)의 마지막 run_result 판정이 ok가 아닙니다: %s", lr.Category)
		}
		if lr.ConfigHash != r.ConfigHash {
			add("이력(ledger)의 설정 해시 %q 가 보고서 %q 와 다릅니다", lr.ConfigHash, r.ConfigHash)
		}
		if e.ReportSHA256 == "" || lr.ReportSHA256 != e.ReportSHA256 {
			p = append(p, "이력(ledger)에 기록된 보고서 sha256이 이 파일과 다릅니다(실행 후 보고서가 바뀜)")
		}
	}
	return p
}

// Save writes <repoDir>/.aigate/reports/<head10>.json and .md atomically.
func Save(repoDir string, r Report) (string, error) {
	if len(r.Head) < 10 {
		return "", fmt.Errorf("report head %q is shorter than 10 chars", r.Head)
	}
	dir := filepath.Join(repoDir, ".aigate", "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, r.Head[:10])
	if err := writeAtomic(base+".md", []byte(r.Markdown())); err != nil {
		return "", err
	}
	if err := writeAtomic(base+".json", r.JSON()); err != nil {
		return "", err
	}
	return base + ".json", nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load strictly decodes a schema v1 report.
func Load(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	return Decode(data, path)
}

// Decode strictly decodes a schema v1 report from data (name is for errors).
func Decode(data []byte, name string) (Report, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return Report{}, fmt.Errorf("decode %s: %w", name, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Report{}, fmt.Errorf("decode %s: trailing data", name)
	}
	if r.SchemaVersion != 1 {
		return Report{}, fmt.Errorf("%s: unsupported schema_version %d", name, r.SchemaVersion)
	}
	return r, nil
}
