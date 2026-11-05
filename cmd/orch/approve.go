package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"aigate/internal/approval"
	"aigate/internal/approval/ui"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/store"
)

// approveIn is where the human's answer is read from (a seam for tests).
var approveIn io.Reader = os.Stdin

// requireTTY is a seam so tests can walk the interactive path. The real check
// needs a terminal on BOTH stdin and stdout: the diff must be seen by a human
// and the answer must come from one.
var requireTTY = func(in io.Reader, out io.Writer) error {
	inf, ok := in.(*os.File)
	if !ok {
		return fmt.Errorf("stdin is not a terminal")
	}
	if err := approval.RequireTTY(inf); err != nil {
		return err
	}
	outf, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(outf.Fd())) {
		return fmt.Errorf("stdout is not a terminal")
	}
	return nil
}

// gateHome is the home whose .aigate/approvals.json the gate and the daemon read.
func gateHome(flagValue string) (string, error) {
	if flagValue == "" {
		flagValue = os.Getenv("ORCH_GATE_HOME")
	}
	return gate.Home(flagValue)
}

func (c *client) approvalDetail(id string) (api.ApprovalDetail, error) {
	var d api.ApprovalDetail
	raw, err := c.call("GET", "/approvals/"+id, nil)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("서버 응답을 읽을 수 없습니다: %w", err)
	}
	return d, nil
}

// cmdApprove is `orch approve ID`: the human's terminal path. The grant file it
// writes is the authority; the daemon is asked to re-judge only afterwards, and
// it checks the file itself.
func cmdApprove(c *client, id, gateHomeFlag string, stdout, stderr io.Writer) int {
	const prog = "orch"
	// Refuse before anything is fetched or shown: no TTY, no approval.
	if err := requireTTY(approveIn, stdout); err != nil {
		fmt.Fprintf(stderr, "orch: approve 거부: %v (사람이 터미널에서 직접 실행해야 합니다)\n", err)
		return ui.ExitGateError
	}
	d, err := c.approvalDetail(id)
	if err != nil {
		fmt.Fprintln(stderr, "오류:", err)
		return 1
	}
	switch {
	case d.Kind == store.KindDestructiveBlocked:
		fmt.Fprintln(stderr, "orch: approve 거부: destructive_blocked 항목은 기록 전용이며 승인할 수 없습니다")
		return ui.ExitGateError
	case d.Status != store.ApprovalPending:
		fmt.Fprintf(stderr, "orch: approve 거부: 대기 중인 항목이 아닙니다(상태: %s)\n", d.Status)
		return ui.ExitGateError
	case d.TaskState != string(store.StateNeedsApproval):
		fmt.Fprintf(stderr, "orch: approve 거부: 작업 #%d 는 승인 대기 상태가 아닙니다(%s)\n", d.TaskID, d.TaskState)
		return ui.ExitGateError
	}
	home, err := gateHome(gateHomeFlag)
	if err != nil {
		fmt.Fprintln(stderr, "오류:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch d.Kind {
	case store.KindProtectedModified:
		p, err := gate.Prepare(gate.Options{Repo: d.Repo, Base: d.Base, Head: d.Head})
		if err != nil {
			fmt.Fprintln(stderr, "오류:", err)
			return 1
		}
		// The hash being granted must be the one the pending entry waits for,
		// for the task's current base and head.
		if p.Base != d.Base || p.Head != d.Head || p.Diff.Hash != d.Hash {
			fmt.Fprintf(stderr, "orch: approve 거부: 이 항목의 해시와 현재 diff의 해시가 다릅니다(항목 %s, 현재 %s). 오래된 항목일 수 있습니다. 승인은 기록하지 않음\n",
				short(d.Hash), short(p.Diff.Hash))
			return ui.ExitGateError
		}
		switch ui.Review(ctx, p, gate.LedgerPath(home, p.RepoID), prog, approveIn, stdout, stderr) {
		case ui.Confirmed:
			if code := ui.Record(p, home, prog, stdout, stderr); code != 0 {
				// Record fails after the grant only on the ledger append: the
				// grant stands, so still ask for the re-judge below.
				if !grantExists(home, p.RepoID, p.Base, p.Diff.Hash) {
					return code
				}
			}
		case ui.NothingToApprove:
			return 0
		default:
			return ui.ExitGateError
		}
	case store.KindPublishOutsideAllowlist:
		action := ""
		for _, k := range []string{publish.KindPushBranch, publish.KindDraftPR, publish.KindNotify} {
			if approvals.PublishHash(d.TaskID, d.Head, k) == d.Hash {
				action = k
			}
		}
		if action == "" {
			fmt.Fprintln(stderr, "orch: approve 거부: 이 항목의 해시가 알려진 게시 행위와 맞지 않습니다. 승인은 기록하지 않음")
			return ui.ExitGateError
		}
		repoID, _, err := gate.RepoID(d.Repo)
		if err != nil {
			fmt.Fprintln(stderr, "오류:", err)
			return 1
		}
		fmt.Fprintf(stdout, "작업 #%d 게시 행위 승인\n  행위: %s\n  base: %s\n  head: %s\n  사유: %s\n  hash: %s\n",
			d.TaskID, action, d.Base, d.Head, safeLine(d.Reason), d.Hash)
		if !ui.Confirm(ctx, "Type 'yes' to approve this exact action: ", prog, approveIn, stdout, stderr) {
			return ui.ExitGateError
		}
		if err := (approval.Store{Path: gate.ApprovalsPath(home)}).Grant(repoID, d.Base, d.Hash, time.Now().UTC()); err != nil {
			fmt.Fprintf(stderr, "orch: %v\n", err)
			return ui.ExitGateError
		}
		fmt.Fprintln(stdout, "approved.")
		if err := ledger.Append(gate.LedgerPath(home, repoID), "approval_granted", repoID,
			map[string]any{"base": d.Base, "hash": d.Hash, "action": action}, time.Now().UTC()); err != nil {
			fmt.Fprintf(stderr, "orch: 승인은 기록되었으나 이력(ledger)에 기록하지 못했습니다: %v\n", err)
		}
	default:
		fmt.Fprintf(stderr, "orch: approve 거부: 알 수 없는 종류 %q\n", d.Kind)
		return ui.ExitGateError
	}
	return reverify(c, id, stdout, stderr)
}

// reverify asks the daemon to re-judge. The daemon verifies the grant file; a
// caller without a grant changes nothing.
func reverify(c *client, id string, stdout, stderr io.Writer) int {
	raw, err := c.call("POST", "/approvals/"+id+"/reverify", nil)
	if err != nil {
		fmt.Fprintln(stderr, "오류:", err)
		return 1
	}
	var v api.TaskView
	_ = json.Unmarshal(raw, &v)
	fmt.Fprintf(stdout, "작업 #%d 재검증 시작 (상태: %s)\n", v.ID, v.State)
	return 0
}

func grantExists(home, repoID, base, hash string) bool {
	return approval.Store{Path: gate.ApprovalsPath(home)}.Valid(repoID, base, hash)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// safeLine keeps a stored reason on one line without control characters.
func safeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
