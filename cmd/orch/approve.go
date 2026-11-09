package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"aigate/internal/approval"
	"aigate/internal/approval/signing"
	"aigate/internal/approval/ui"
	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/orch/api"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/publish"
	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
	"aigate/internal/textsafe"
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
func cmdApprove(c *client, id, gateHomeFlag string, so signOpts, stdout, stderr io.Writer) int {
	const prog = "orch"
	// Refuse before anything is fetched or shown: no TTY, no approval.
	if err := requireTTY(approveIn, stdout); err != nil {
		fmt.Fprintf(stderr, "orch: approve 거부: %v (사람이 터미널에서 직접 실행해야 합니다)\n", err)
		return ui.ExitGateError
	}
	d, err := c.approvalDetail(id)
	if err != nil {
		fmt.Fprintln(stderr, "오류:", errText(err))
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
	if d.RequiresSignature {
		// Signed-only mode: the daemon verifies a signature and writes the
		// grant itself; nothing is written locally.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return approveSigned(ctx, c, id, d, so, stdout, stderr)
	}
	home, err := gateHome(gateHomeFlag)
	if err != nil {
		fmt.Fprintln(stderr, "오류:", errText(err))
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch d.Kind {
	case store.KindProtectedModified:
		p, err := gate.Prepare(gate.Options{Repo: d.Repo, Base: d.Base, Head: d.Head})
		if err != nil {
			fmt.Fprintln(stderr, "오류:", errText(err))
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
		action := deriveAction(d)
		if action == "" {
			fmt.Fprintln(stderr, "orch: approve 거부: 이 항목의 해시가 알려진 게시 행위와 맞지 않습니다. 승인은 기록하지 않음")
			return ui.ExitGateError
		}
		repoID, _, err := gate.RepoID(d.Repo)
		if err != nil {
			fmt.Fprintln(stderr, "오류:", errText(err))
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
		fmt.Fprintln(stderr, "오류:", errText(err))
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

// deriveAction rebuilds which publish action a publish_outside_allowlist
// entry's hash binds, from the task, head and action kinds (and the policy in
// force for the policy-change pseudo action). "" means none matches. The CLI
// never trusts the server's action string by itself.
func deriveAction(d api.ApprovalDetail) string {
	action := ""
	for _, k := range []string{publish.KindPushBranch, publish.KindDraftPR, publish.KindNotify} {
		if approvals.PublishHash(d.TaskID, d.Head, k) == d.Hash {
			action = k
		}
	}
	if action == "" && d.PolicyHash != "" &&
		approvals.PublishHash(d.TaskID, d.Head, steps.PolicyChangeKind(d.PolicyHash)) == d.Hash {
		action = steps.PolicyChangeKind(d.PolicyHash)
	}
	return action
}

// repoIDOf is the repo id the server puts in the message for d.Repo: the
// local gate.RepoID when the path is a repository here, otherwise the same
// hash of the path string (the server stores the symlink-resolved top level,
// which is what gate.RepoID hashes).
func repoIDOf(repo string) string {
	if id, _, err := gate.RepoID(repo); err == nil {
		return id
	}
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:])[:12]
}

// errText is an error for the terminal: server-provided text may carry
// control sequences, so it is escaped onto one line.
func errText(err error) string { return textsafe.Escape(err.Error()) }

// signOpts are `orch approve`'s signing flags.
type signOpts struct {
	KeyPath string // --sign-key; "" means defaultSignKey()
	Signer  string // --signer; "" means $AIGATE_SIGNER
}

// signCmd signs message in namespace with the SSH key at keyPath and returns
// the armored SSHSIG (R9: a seam for tests; login uses it too, with
// "aigate-login"). The default runs `ssh-keygen -Y sign`, message on stdin and
// signature on stdout; ssh-keygen asks for a passphrase on the terminal itself
// and may use the agent when keyPath names a public key.
var signCmd = func(keyPath, namespace string, message []byte) (string, error) {
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", namespace, "-f", keyPath)
	cmd.Stdin = bytes.NewReader(message)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh-keygen -Y sign 실패: %v: %s", err, safeLine(strings.TrimSpace(errb.String())))
	}
	return out.String(), nil
}

// defaultSignKey is the private key `orch approve` signs with when
// --sign-key is not given.
func defaultSignKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ssh/id_ed25519"
	}
	return filepath.Join(home, ".ssh", "id_ed25519")
}

// grantRefusals are the fixed texts for the grant endpoint's error codes
// (CF1): the server's own message is never printed, since it could echo input.
var grantRefusals = map[string]string{
	"bad_signature":       "서명이 서명자 목록의 공개 키로 검증되지 않았습니다. --sign-key의 키가 --signer로 등록된 키인지 확인하세요",
	"unknown_signer":      "서명자 ID가 서버의 서명자 목록에 없습니다(관리자가 `orch admin signers list`로 확인)",
	"stale_signature":     "서명 시각이 허용 범위(±5분)를 벗어났습니다. 다시 실행하세요",
	"invalid_message":     "서버가 서명 메시지를 다시 만들 수 없었습니다",
	"signers_unavailable": "서버의 서명자 목록을 신뢰할 수 없어 서명 승인을 처리할 수 없습니다(관리자 확인 필요)",
	"not_pending":         "대기 중인 항목이 아니거나 작업이 승인 대기 상태가 아닙니다. 변경 없음",
	"hash_changed":        "작업의 base·head·diff 해시가 이 항목과 다릅니다. 다시 확인하세요. 변경 없음",
	"not_signed_mode":     "서버가 서명 필수 모드가 아닙니다. 다시 실행하면 기존 방식으로 승인합니다",
	"forbidden":           "이 항목은 승인할 수 없습니다",
}

func refuseSigned(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "orch: approve 거부: "+format+" 서명하지 않았습니다(승인은 기록하지 않음)\n", a...)
	return ui.ExitGateError
}

// approveSigned is the signed-only-mode path of `orch approve` (design 8.2,
// 8.7). Order: check that the message the server asks us to sign is the
// entry shown and (for a diff) that the shown text hashes to the entry's
// hash, show it, require "yes", then sign and post. Nothing is signed before
// the confirmation.
func approveSigned(ctx context.Context, c *client, id string, d api.ApprovalDetail, so signOpts, stdout, stderr io.Writer) int {
	const prog = "orch"
	if d.Message == "" {
		return refuseSigned(stderr, "서버가 지금은 이 항목을 승인할 수 없다고 합니다: %s.", textsafe.Escape(d.Meaning))
	}
	// CF4: parse (which validates) before anything is shown or signed, and
	// require the message to be about exactly this entry.
	msg, err := signing.ParseMessage(d.Message)
	issued, terr := time.Parse(time.RFC3339, d.Issued)
	if err != nil || terr != nil {
		return refuseSigned(stderr, "서버가 준 서명 메시지가 올바른 형식이 아닙니다.")
	}
	if msg.Kind != d.Kind || msg.Hash != d.Hash || msg.Base != d.Base || msg.Task != d.TaskID || !msg.Issued.Equal(issued) {
		return refuseSigned(stderr, "서버가 준 서명 메시지가 이 항목(종류·작업·base·해시)과 다릅니다.")
	}
	if got := repoIDOf(d.Repo); got != msg.Repo {
		return refuseSigned(stderr, "서버가 준 서명 메시지의 저장소 id(%s)가 이 항목의 저장소 %s 의 id(%s)와 다릅니다.",
			msg.Repo, textsafe.Escape(d.Repo), got)
	}
	fmt.Fprintf(stdout, "저장소: %s (id %s)\n", textsafe.Escape(d.Repo), msg.Repo)

	switch d.Kind {
	case store.KindProtectedModified:
		// Truncation first: a cut text never hashes right, and the reason
		// to give is that it cannot be reviewed in full here.
		if d.Truncated {
			return refuseSigned(stderr, "보호 파일 diff가 API 표시 한도를 넘어 잘렸습니다. 전체를 보지 않고는 서명할 수 없습니다. "+
				"저장소에서 `git diff %s %s`로 전체를 검토하고, 보호 파일 변경을 더 작은 커밋으로 나눠 다시 실행하세요.", short(d.Base), textsafe.Escape(short(d.Head)))
		}
		if d.HashedText == "" {
			return refuseSigned(stderr, "서버가 해시 대상 diff를 주지 않았습니다.")
		}
		if got := gitx.HashOf(d.HashedText); got != d.Hash {
			return refuseSigned(stderr, "서버가 보여 준 diff의 해시(%s)가 승인 항목의 해시(%s)와 다릅니다. 보이는 내용과 서명 대상이 다를 수 있습니다.",
				short(got), short(d.Hash))
		}
		if f, bad := textsafe.FirstUnsafe(d.HashedText); bad {
			return refuseSigned(stderr, "diff %d번째 줄에 터미널 제어·비정상 문자(%s)가 있어 화면에서 내용을 숨길 수 있습니다.", f.Line, f.Desc)
		}
		fmt.Fprintf(stdout, "작업 #%d 보호 파일 변경 승인(서명)\n  base: %s\n  head: %s\n  보호 파일(%d):\n",
			d.TaskID, d.Base, textsafe.Escape(d.Head), len(d.ProtectedFiles))
		for _, f := range d.ProtectedFiles {
			fmt.Fprintf(stdout, "    %s\n", textsafe.Escape(f))
		}
		fmt.Fprint(stdout, "the approval binds exactly this text (sha256 = hash):\n", ui.BeginMark, d.HashedText, ui.EndMark)
	case store.KindPublishOutsideAllowlist:
		// No diff: the action is re-derived from the hash here (as in the
		// TTY flow) and must be the one the server names.
		action := deriveAction(d)
		if action == "" {
			return refuseSigned(stderr, "이 항목의 해시가 알려진 게시 행위와 맞지 않습니다.")
		}
		if action != d.Action {
			return refuseSigned(stderr, "서버가 말한 게시 행위(%s)가 해시로 다시 계산한 행위(%s)와 다릅니다.", textsafe.Escape(d.Action), action)
		}
		fmt.Fprintf(stdout, "작업 #%d 게시 행위 승인(서명)\n  행위: %s\n  base: %s\n  head: %s\n",
			d.TaskID, action, d.Base, textsafe.Escape(d.Head))
	default:
		return refuseSigned(stderr, "알 수 없는 종류 %q 입니다.", d.Kind)
	}
	// For a publish entry the verified line is the action derived above; the
	// server's meaning is shown only as its unverified description.
	meaningLabel := "무엇을 허용하나요"
	if d.Kind == store.KindPublishOutsideAllowlist {
		meaningLabel = "서버 설명(검증 안 됨)"
	}
	fmt.Fprintf(stdout, "hash: %s\n%s: %s\n", d.Hash, meaningLabel, textsafe.Escape(d.Meaning))

	signer := so.Signer
	if signer == "" {
		signer = os.Getenv("AIGATE_SIGNER")
	}
	if signer == "" {
		fmt.Fprintln(stderr, "orch: approve 거부: 서명자 ID가 없습니다. --signer ID 또는 AIGATE_SIGNER로 지정하세요(`orch admin signers list`의 ID)")
		return ui.ExitGateError
	}
	key := so.KeyPath
	if key == "" {
		key = defaultSignKey()
	}
	if _, err := os.Stat(key); err != nil {
		fmt.Fprintf(stderr, "orch: approve 거부: 서명 키 파일이 없습니다: %s (--sign-key로 지정)\n", key)
		return ui.ExitGateError
	}
	fmt.Fprintf(stdout, "서명할 메시지(네임스페이스 %s, 서명자 %s, 키 %s):\n%s", signing.NamespaceApproval, signer, key, msg.Text())
	if !ui.Confirm(ctx, "Type 'yes' to sign and approve exactly this: ", prog, approveIn, stdout, stderr) {
		return ui.ExitGateError
	}
	armored, err := signCmd(key, signing.NamespaceApproval, []byte(msg.Text()))
	if err != nil {
		fmt.Fprintf(stderr, "orch: 서명하지 못했습니다: %v\n", err)
		return ui.ExitGateError
	}
	raw, err := c.call("POST", "/approvals/"+id+"/grant", map[string]string{
		"signer": signer, "alg": signing.TypeSSHEd25519, "sig": armored, "issued": d.Issued})
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			fmt.Fprintln(stderr, "오류:", errText(err)) // transport error: local text, no server content
			return 1
		}
		text, ok := grantRefusals[ae.Code]
		if !ok {
			text = fmt.Sprintf("서버가 승인을 거부했습니다(HTTP %d)", ae.Status)
		}
		fmt.Fprintf(stderr, "orch: approve 거부: %s\n", text)
		return ui.ExitGateError
	}
	var v api.TaskView
	_ = json.Unmarshal(raw, &v)
	fmt.Fprintf(stdout, "approved (서명자 %s). 작업 #%d 상태: %s\n", signer, v.ID, safeLine(string(v.State)))
	return 0
}
