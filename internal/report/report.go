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

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
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

// Check returns Korean problem descriptions; empty means the evidence is fresh.
func Check(r Report, headNow string, dirty bool, approvalValid func(hash string) bool) []string {
	var p []string
	if r.SchemaVersion != 1 {
		p = append(p, fmt.Sprintf("보고서 스키마 버전이 지원되지 않습니다: %d", r.SchemaVersion))
	}
	if r.Head != headNow {
		p = append(p, fmt.Sprintf("증거는 %s 기준인데 현재 HEAD는 %s 입니다", short(r.Head), short(headNow)))
	}
	if dirty {
		p = append(p, "작업 트리에 커밋되지 않은 변경이 있습니다")
	}
	if r.Category != rules.CategoryOK {
		p = append(p, fmt.Sprintf("보고서 판정이 ok가 아닙니다: %s", r.Category))
	}
	for _, a := range r.Approvals {
		if a.Valid && (approvalValid == nil || !approvalValid(a.Hash)) {
			p = append(p, fmt.Sprintf("보고서가 의존한 승인 %s 이(가) 더 이상 유효하지 않습니다", short(a.Hash)))
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return Report{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Report{}, fmt.Errorf("decode %s: trailing data", path)
	}
	if r.SchemaVersion != 1 {
		return Report{}, fmt.Errorf("%s: unsupported schema_version %d", path, r.SchemaVersion)
	}
	return r, nil
}
