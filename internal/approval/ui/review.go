package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/gitx"
	"aigate/internal/ledger"
	"aigate/internal/textsafe"
)

// ExitGateError is the exit code of every refusal and failure.
const ExitGateError = 64

// BeginMark and EndMark frame the exact text the approval hash covers.
const BeginMark, EndMark = "----- BEGIN HASHED DIFF -----\n", "----- END HASHED DIFF -----\n"

// Decision is the outcome of Review.
type Decision int

const (
	// Refused: a safety check stopped the review; the refusal was printed
	// and recorded in the ledger. Nothing may be granted.
	Refused Decision = iota
	// NothingToApprove: the diff needs no approval.
	NothingToApprove
	// NotConfirmed: the human did not type "yes".
	NotConfirmed
	// Interrupted: ctx ended while waiting for the answer.
	Interrupted
	// Confirmed: the human typed "yes" for exactly this diff.
	Confirmed
)

// Review runs every safety check on p BEFORE anything is printed, shows the
// whole hashed diff and asks for confirmation. It grants nothing: on Confirmed
// the caller records the grant with Record. prog names the command in
// messages ("aigate" or "orch"). ledgerPath receives approval_refused events.
func Review(ctx context.Context, p gate.Protected, ledgerPath, prog string, stdin io.Reader, stdout, stderr io.Writer) Decision {
	// Everything shown is checked BEFORE anything is printed. A file that
	// became binary or became text, or any file binary at head that is not an
	// allowlisted genuine binary type (BinaryRefusal: never loadable code, and
	// the content must match the extension), and any added/modified/
	// type-changed binary under a loaded-code directory (LoadedCodeDirs,
	// binary_in_loaded_dir), is refused: a summary line would
	// let one NUL byte hide the content the human approves. Then a
	// control sequence (ESC, lone CR, ...) or bidi override in a line or path
	// could make the screen hide what the human approves. Allowlisted binary
	// files and deleted binaries are shown as a summary line; the hash stays
	// over the raw bytes (HashedText).
	//
	// refuse records approval_refused FIRST (reason code and whether the
	// diff needs approval at all, never raw bytes or paths), then explains; a
	// ledger failure is a gate error too.
	refuse := func(msg, reason, why string) Decision {
		lerr := ledger.Append(ledgerPath, "approval_refused", p.RepoID, map[string]any{"base": p.Base, "reason": reason, "needed_approval": p.NeedsApproval()}, time.Now().UTC())
		fmt.Fprintf(stderr, "%s: approve 거부: %s (이유 코드 %s). %s\n", prog, msg, reason, why)
		if lerr != nil {
			fmt.Fprintf(stderr, "%s: gate error: 거부 사건을 이력(ledger)에 기록하지 못했습니다: %v\n", prog, lerr)
		}
		return Refused
	}
	// A head whose tree has a case/Unicode-folding collision is not checked
	// out as committed (fix wave 7 (B)): run never executes its checks
	// (path_case_collision), so no diff shown for it could be approved.
	if len(p.Collisions) > 0 {
		refuse(fmt.Sprintf("head 트리에 대소문자·유니코드 정규화를 구분하지 않는 파일 시스템에서 같은 경로가 되는 이름이 있습니다(충돌 %d건): %s", len(p.Collisions), gate.CollisionList(p.Collisions)),
			ReasonPathCaseCollision, "그런 파일 시스템에서는 이 경로들이 서로 덮어쓰거나 가려 checkout 결과가 커밋된 트리와 같지 않으므로 승인하지 않습니다(승인은 기록하지 않음). 충돌하는 이름을 고친 커밋으로 다시 실행하세요.")
		return Refused
	}
	if msg, reason := BinaryRefusal(p); msg != "" {
		why := "바이너리 요약 한 줄로는 사람이 내용을 볼 수 없어 승인하지 않습니다(승인은 기록하지 않음). NUL 바이트를 없앤 커밋으로 다시 실행하세요."
		switch reason {
		case ReasonBinaryBecameText:
			why = "승인하지 않습니다(승인은 기록하지 않음). 변경을 '이 파일 삭제 + 텍스트를 새 이름의 파일로 추가'로 나눈 커밋으로 다시 실행하세요: 삭제된 바이너리는 요약 한 줄로, 추가한 텍스트는 그대로 보여 검토할 수 있습니다."
		case ReasonBinaryMagicMismatch:
			why = "확장자와 내용의 형식이 맞지 않는 바이너리는 러너가 다른 방법(예: Node require()는 모르는 확장자를 JS로 실행)으로 코드로 불러올 수 있어 요약 한 줄로 승인하지 않습니다(승인은 기록하지 않음). 진짜 그 형식의 파일로 바꾸거나, 직접 `git diff --text`로 검토하고 맞는 이름의 텍스트 파일로 둔 커밋으로 다시 실행하세요."
		case ReasonBinaryLoadable:
			why = "러너가 코드로 불러올 수 있는 바이너리는 요약 한 줄로 검토할 수 없어 승인하지 않습니다(승인은 기록하지 않음). 사람이 저장소 밖에서 직접 검토하세요: 산출물을 직접 확인한 뒤 사람이 직접 커밋해 다음 base에 들어가게 합니다. 우회 플래그는 의도적으로 없습니다."
		case ReasonBinaryInLoadedDir:
			why = "러너가 코드를 불러오는 디렉터리(" + strings.Join(LoadedCodeDirs, "·") + ") 아래의 바이너리는 형식에 맞는 파일이 동시에 올바른 코드일 수 있어(예: GIF89a로 시작하는 JS) 요약 한 줄로 검토할 수 없으므로 승인하지 않습니다(승인은 기록하지 않음). 사람이 저장소 밖에서 산출물을 직접 검토한 뒤 사람이 직접 커밋해 다음 base에 들어가게 하세요. 우회 플래그는 의도적으로 없습니다. (삭제는 요약 한 줄로 승인할 수 있습니다.)"
		case ReasonBinaryExecutable, ReasonBinaryUnknown:
			why = "바이너리 요약 한 줄은 허용 목록의 진짜 바이너리 형식(실행 권한·'#!' 시작·'.bin' 경로가 없는 파일)에만 쓰고, 그 밖의 바이너리는 사람이 내용을 볼 수 없어 승인하지 않습니다(승인은 기록하지 않음). 직접 `git diff --text`로 검토하고 텍스트 파일로 두거나 허용 목록 형식의 바이너리로 바꾼 커밋으로 다시 실행하세요."
		}
		return refuse(msg, reason, why)
	}
	display := p.Diff.DisplayText(textsafe.Escape)
	if msg, reason := UnsafeApproveText(p, display); msg != "" {
		return refuse(msg, reason, "터미널 제어·비정상 문자가 있는 diff는 화면에서 내용을 숨길 수 있어 승인하지 않습니다(승인은 기록하지 않음). 해당 문자를 없앤 커밋으로 다시 실행하세요.")
	}
	fmt.Fprintf(stdout, "base: %s\nhead: %s\n", p.Base, p.Head)
	fmt.Fprintf(stdout, "files touched (%d):\n", len(p.Diff.TouchedFiles))
	for _, f := range p.Diff.TouchedFiles {
		fmt.Fprintf(stdout, "  %s\n", f)
	}
	fmt.Fprintf(stdout, "changed or deleted existing protected files (any hunk, additions too): %d; removed lines: %d; added guarded files: %d\n",
		len(p.Diff.Changed()), len(p.Diff.RemovedLines), len(p.AddedGuarded))
	// A symlink is marked with its target (checked like a path by
	// UnsafeApproveText); the target file's content is not in the diff.
	links, shown := p.Diff.HeadSymlinks(), false
	for _, l := range []struct {
		mark  string
		paths []string
	}{{"!", p.Diff.Changed()}, {"+", p.AddedGuarded}} {
		for _, f := range l.paths {
			_, isLink := links[f]
			shown = shown || isLink
			fmt.Fprintf(stdout, "  %s %s\n", l.mark, gitx.SymlinkMark(links, f))
		}
	}
	if shown {
		fmt.Fprintln(stdout, "note: a symlink's target content is not part of the reviewed diff (only the link and its target path are); check the target file itself")
	}
	// The full text the hash is computed over, untruncated: the human
	// approves exactly what they read (binary files as a summary line).
	fmt.Fprint(stdout, "the approval binds exactly this text (sha256 prefix = hash):\n", BeginMark, display, EndMark)
	if n := BinaryCount(p); n > 0 {
		fmt.Fprintf(stdout, "(%d binary file(s) shown as a summary line; the hash covers their raw bytes)\n", n)
	}
	fmt.Fprintf(stdout, "hash: %s\n", p.Diff.Hash)
	if !p.NeedsApproval() {
		fmt.Fprintln(stdout, "nothing needs approval.")
		return NothingToApprove
	}
	if !Confirm(ctx, "Type 'yes' to approve this exact diff: ", prog, stdin, stdout, stderr) {
		if ctx.Err() != nil {
			return Interrupted
		}
		return NotConfirmed
	}
	return Confirmed
}

// Confirm prints prompt and reports whether the human typed exactly "yes".
// It prints the reason itself when the answer is anything else.
func Confirm(ctx context.Context, prompt, prog string, stdin io.Reader, stdout, stderr io.Writer) bool {
	fmt.Fprint(stdout, prompt)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		answer <- line
	}()
	var line string
	select {
	case line = <-answer:
	case <-ctx.Done():
		fmt.Fprintf(stderr, "\n%s: interrupted; not approved\n", prog)
		return false
	}
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintf(stderr, "%s: not approved\n", prog)
		return false
	}
	return true
}

// Record writes the grant for p into the approvals store under home (the
// store the gate reads) and then the approval_granted ledger event. It
// returns 0 or ExitGateError after printing what happened.
func Record(p gate.Protected, home, prog string, stdout, stderr io.Writer) int {
	st := approval.Store{Path: gate.ApprovalsPath(home)}
	if err := st.Grant(p.RepoID, p.Base, p.Diff.Hash, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return ExitGateError
	}
	fmt.Fprintln(stdout, "approved.")
	// The grant is done; the ledger event comes after it. If the append
	// fails, say plainly that the approval itself WAS recorded.
	if err := ledger.Append(gate.LedgerPath(home, p.RepoID), "approval_granted", p.RepoID, map[string]any{
		"base": p.Base, "hash": p.Diff.Hash, "files_changed": len(p.Diff.TouchedFiles),
	}, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "%s: gate error: 승인은 %s 에 기록되었습니다(유효함). 그러나 이력(ledger)에 approval_granted 사건을 기록하지 못했습니다: %v\n", prog, gate.ApprovalsPath(home), err)
		return ExitGateError
	}
	return 0
}
