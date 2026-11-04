package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"aigate/internal/approval"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/report"
)

// CheckOptions configures CheckReport. Base is required: a report is only
// evidence relative to the base the caller fixed when the task started.
type CheckOptions struct {
	Repo, Base, ReportPath, HomeDir string
}

// CheckReport gathers the current state (HEAD, dirty tree, base config hash,
// approvals, the ledger's last run_result for head and base, the sha256 of
// the report file) and returns report.Check's problems; empty means fresh.
// A non-nil error is a gate error (unreadable report, unresolvable base,
// unreadable home) and never means fresh.
func CheckReport(o CheckOptions) ([]string, error) {
	if o.Base == "" {
		return nil, errors.New("check needs the base the run was made against (--base)")
	}
	data, err := os.ReadFile(o.ReportPath)
	if err != nil {
		return nil, err
	}
	rep, err := report.Decode(data, o.ReportPath)
	if err != nil {
		return nil, err
	}
	id, top, err := RepoID(o.Repo)
	if err != nil {
		return nil, err
	}
	head, err := gitx.Resolve(top, "HEAD")
	if err != nil {
		return nil, err
	}
	base, err := gitx.Resolve(top, o.Base)
	if err != nil {
		return nil, err
	}
	dirty, err := gitx.Dirty(top)
	if err != nil {
		return nil, err
	}
	cfgHash, err := ConfigHashAt(top, base)
	if err != nil {
		return nil, err
	}
	home, err := Home(o.HomeDir)
	if err != nil {
		return nil, err
	}
	st := approval.Store{Path: ApprovalsPath(home)}
	sum := sha256.Sum256(data)
	e := report.Expect{Head: head, Base: base, Repo: top, ConfigHash: cfgHash, Dirty: dirty != "",
		ApprovalValid: func(h string) bool { return st.Valid(id, base, h) },
		ReportSHA256:  hex.EncodeToString(sum[:])}
	e.LedgerRun, e.LedgerErr = lastRun(LedgerPath(home, id), id, head, base)
	return report.Check(rep, e), nil
}

// lastRun returns the last run_result event of repo id for (head, base), nil
// when there is none. The ledger must verify first.
func lastRun(path, id, head, base string) (*report.LedgerRun, error) {
	if _, err := ledger.Verify(path); err != nil {
		return nil, err
	}
	evs, err := ledger.Events(path)
	if err != nil {
		return nil, err
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	var last *report.LedgerRun
	for _, ev := range evs {
		if ev.Type != "run_result" || ev.Repo != id || str(ev.Payload, "head") != head || str(ev.Payload, "base") != base {
			continue
		}
		last = &report.LedgerRun{Category: str(ev.Payload, "category"), ConfigHash: str(ev.Payload, "config_hash"),
			ReportSHA256: str(ev.Payload, "report_sha256")}
	}
	if last == nil {
		return nil, nil
	}
	return last, nil
}
