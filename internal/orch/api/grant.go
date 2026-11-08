package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"aigate/internal/approval"
	"aigate/internal/approval/signing"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/store"
)

// Signed approvals (design 8): the detail endpoint shows the canonical message
// the server rebuilt from the pending entry and the task; POST
// /api/v1/approvals/{id}/grant takes a signature over that message. The server
// rebuilds the message itself from stored values: from the request body only
// issued and the signature (signer, alg, sig, webauthn) are used, every other
// field is ignored. Errors carry fixed codes and texts; signing, registry and
// approval errors are never echoed (CF1).

// grantRequest is the grant body. Unknown fields are ignored, not refused.
type grantRequest struct {
	signing.Signature
	Issued string `json:"issued"`
}

// Fixed refusals of the grant endpoint.
var (
	errDestructive    = apiErr(http.StatusForbidden, "forbidden", "destructive_blocked 항목은 승인할 수 없습니다")
	errNotSignedMode  = apiErr(http.StatusBadRequest, "not_signed_mode", "서명 필수 모드가 아닙니다. 서명 승인은 서명자 목록이 있을 때만 받습니다")
	errSignersBroken  = apiErr(http.StatusServiceUnavailable, "signers_unavailable", "서명자 목록을 신뢰할 수 없어 서명 승인을 처리할 수 없습니다")
	errEntryNotPend   = apiErr(http.StatusConflict, "not_pending", "대기 중인 승인 항목이 아닙니다. 변경 없음")
	errTaskNotWaiting = apiErr(http.StatusConflict, "not_pending", "작업이 승인 대기 상태가 아닙니다. 변경 없음")
	errHashChanged    = apiErr(http.StatusConflict, "hash_changed", "작업의 현재 base·head·diff 해시가 이 항목과 다릅니다. 변경 없음")
	errBadBody        = apiErr(http.StatusBadRequest, "bad_request", "요청 본문이 올바른 JSON이 아닙니다")
	errBodyTooLarge   = apiErr(http.StatusRequestEntityTooLarge, "too_large", "요청이 너무 큽니다")
	errInvalidMessage = apiErr(http.StatusBadRequest, "invalid_message", "서명 메시지를 만들 수 없습니다(issued는 초 단위 UTC RFC 3339여야 합니다)")
	errBadSignature   = apiErr(http.StatusForbidden, "bad_signature", "서명이 올바르지 않습니다")
	errUnknownSigner  = apiErr(http.StatusForbidden, "unknown_signer", "등록되지 않은 서명자입니다")
	errStale          = apiErr(http.StatusForbidden, "stale_signature", "서명 시각(issued)이 허용 범위(±5분)를 벗어났습니다")
	errInternal       = apiErr(http.StatusInternalServerError, "internal", "내부 오류")
)

// grantErr maps a GrantSigned error to a fixed refusal. The detail is logged
// server-side only.
func grantErr(err error) *httpError {
	switch {
	case errors.Is(err, signing.ErrBadSignature):
		return errBadSignature
	case errors.Is(err, signing.ErrUnknownSigner):
		return errUnknownSigner
	case errors.Is(err, signing.ErrStale):
		return errStale
	case errors.Is(err, signing.ErrInvalidMessage):
		return errInvalidMessage
	case errors.Is(err, approval.ErrRegistryUnavailable):
		log.Printf("orchd: signed grant: signers registry unavailable: %v", err)
		return errSignersBroken
	case errors.Is(err, approval.ErrNotSignedMode):
		return errNotSignedMode
	}
	log.Printf("orchd: signed grant: %v", err)
	return errInternal
}

// viaOf names the path a write came in on, for the ledger. Only the local
// socket exists so far; Task 17 (sessions and tokens) replaces this with the
// request's principal ("session" or "token").
func viaOf(*http.Request) string { return "socket" }

// checkCurrent refuses an entry that cannot be granted now: destructive,
// not pending, a task that no longer waits, or a base/head/diff hash that is
// not the task's current one. For protected_modified it returns the prepared
// diff (nil for other kinds).
func checkCurrent(a store.Approval, t store.Task) (*gate.Protected, *httpError) {
	switch {
	case a.Kind == store.KindDestructiveBlocked:
		return nil, errDestructive
	case a.Status != store.ApprovalPending:
		return nil, errEntryNotPend
	case t.State != store.StateNeedsApproval:
		return nil, errTaskNotWaiting
	case a.Head == "" || t.Base != a.Base || t.HeadSHA != a.Head:
		return nil, errHashChanged
	}
	if a.Kind != store.KindProtectedModified {
		// publish_outside_allowlist: the hash is a function of (task, head,
		// action), so an unchanged head keeps it current.
		return nil, nil
	}
	p, err := gate.Prepare(gate.Options{Repo: t.Repo, Base: t.Base, Head: t.HeadSHA})
	if err != nil {
		log.Printf("orchd: approval %d: cannot rebuild diff: %v", a.ID, err)
		return nil, errHashChanged
	}
	if p.Base != a.Base || p.Head != a.Head || p.Diff.Hash != a.Hash {
		return nil, errHashChanged
	}
	return &p, nil
}

// messageFor rebuilds the canonical message for entry a of task t. Every field
// but issued comes from stored values. The result is validated (CF4).
func messageFor(a store.Approval, t store.Task, issued time.Time) (signing.Message, error) {
	rid, _, err := gate.RepoID(t.Repo)
	if err != nil {
		return signing.Message{}, err
	}
	m := signing.Message{Kind: a.Kind, Repo: rid, Task: t.ID, Base: a.Base, Hash: a.Hash, Issued: issued}
	if err := m.Validate(); err != nil {
		return signing.Message{}, err
	}
	return m, nil
}

// parseIssued accepts an RFC 3339 time in UTC (offset zero) only.
func parseIssued(s string) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	if _, off := ts.Zone(); off != 0 {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// meaningOf answers "무엇을 허용하나요?" from server values only (never the
// agent-influenced reason text).
func meaningOf(a store.Approval, t store.Task, p *gate.Protected, files int) string {
	switch a.Kind {
	case store.KindProtectedModified:
		n := ""
		if p != nil {
			n = fmt.Sprintf(" %d개", files)
		}
		return fmt.Sprintf("작업 #%d의 변경(base %s → head %s) 가운데 보호 파일%s의 수정을 허용합니다. "+
			"승인은 diff 해시 %s 에만 묶이며, 내용이 조금이라도 바뀌면 다시 승인해야 합니다.",
			t.ID, short12(a.Base), short12(a.Head), n, a.Hash)
	case store.KindPublishOutsideAllowlist:
		return fmt.Sprintf("작업 #%d의 head %s 에 대해 허용 목록 밖의 게시 행위 하나를 허용합니다. "+
			"승인은 게시 해시 %s 에만 묶이며, head나 행위가 바뀌면 다시 승인해야 합니다.",
			t.ID, short12(a.Head), a.Hash)
	case store.KindDestructiveBlocked:
		return "기록 전용 항목입니다. 파괴적인 변경은 승인할 수 없습니다."
	}
	return "알 수 없는 종류의 항목이라 승인할 수 없습니다."
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// fillSigning adds the signing view to an approval detail. Read-only: it never
// writes anything. The message (and its digest and hashed text) is present
// only when the entry can be granted now.
func fillSigning(det *ApprovalDetail, a store.Approval, t store.Task, now time.Time) {
	signed, err := approval.SignedMode()
	if err != nil {
		log.Printf("orchd: approval detail: signers registry unavailable: %v", err)
	}
	det.RequiresSignature = signed
	issued := now.UTC().Truncate(time.Second)
	det.Issued = issued.Format(time.RFC3339)
	p, herr := checkCurrent(a, t)
	det.ProtectedFiles = []string{} // always an array, like the diff view
	if p != nil {
		det.HashedText, det.Truncated, det.ProtectedFiles = protectedView(*p)
	}
	det.Meaning = meaningOf(a, t, p, len(det.ProtectedFiles))
	if herr != nil {
		return
	}
	msg, err := messageFor(a, t, issued)
	if err != nil {
		return
	}
	det.Message = msg.Text()
	d := msg.Digest()
	det.Digest = hex.EncodeToString(d[:])
}

// grantSigned serves POST /api/v1/approvals/{id}/grant. Order: the entry must
// be grantable and current before the signature is looked at; the signature
// is verified (inside GrantSigned) before anything is written; only then is
// the task re-judged through the same path as reverify.
func (h *handler) grantSigned(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, apiErr(http.StatusBadRequest, "bad_request", "승인 항목 ID가 올바르지 않습니다"))
		return
	}
	ctx := r.Context()
	a, err := h.st.GetApproval(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, apiErr(http.StatusNotFound, "not_found", "승인 항목을 찾을 수 없습니다"))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if a.Kind == store.KindDestructiveBlocked {
		writeErr(w, errDestructive)
		return
	}
	switch signed, err := approval.SignedMode(); {
	case err != nil:
		log.Printf("orchd: signed grant: signers registry unavailable: %v", err)
		writeErr(w, errSignersBroken)
		return
	case !signed:
		writeErr(w, errNotSignedMode)
		return
	}
	t, err := h.st.GetTask(ctx, a.TaskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, herr := checkCurrent(a, t); herr != nil {
		writeErr(w, herr)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, errBodyTooLarge)
		} else {
			writeErr(w, errBadBody)
		}
		return
	}
	var req grantRequest
	if json.Unmarshal(raw, &req) != nil {
		writeErr(w, errBadBody)
		return
	}
	issued, ok := parseIssued(req.Issued)
	if !ok {
		writeErr(w, errInvalidMessage)
		return
	}
	msg, err := messageFor(a, t, issued)
	if err != nil {
		writeErr(w, errInvalidMessage)
		return
	}
	path, err := h.grants.Path()
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := (approval.Store{Path: path}).GrantSigned(msg, req.Signature, time.Now(), signing.VerifyOptions{}); err != nil {
		writeErr(w, grantErr(err))
		return
	}
	// The grant is recorded and the signer is a verified registry id. The
	// ledger event follows; if it fails the grant still stands.
	if home, err := gate.Home(h.grants.Home); err != nil {
		log.Printf("orchd: approval %d granted but ledger home unknown: %v", a.ID, err)
	} else if err := ledger.Append(gate.LedgerPath(home, msg.Repo), "approval_granted", msg.Repo, map[string]any{
		"base": a.Base, "hash": a.Hash, "kind": a.Kind, "task": t.ID, "approval": a.ID,
		"signer": req.Signer, "alg": req.Alg, "via": viaOf(r),
	}, time.Now().UTC()); err != nil {
		log.Printf("orchd: approval %d granted but the ledger append failed: %v", a.ID, err)
	}
	v, err := h.judgeGranted(ctx, a, t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// judgeGranted is the approval-resolution path shared by reverify and grant:
// the daemon itself checks the approvals file for a grant of exactly this
// entry, marks it approved and lets the task be re-judged.
func (h *handler) judgeGranted(ctx context.Context, a store.Approval, t store.Task) (TaskView, error) {
	ok, err := h.grants.Granted(t.Repo, a)
	if err != nil {
		return TaskView{}, err
	}
	if !ok {
		return TaskView{}, &httpError{code: http.StatusConflict,
			msg: "승인 기록(grant)이 없습니다. 사람이 터미널에서 `orch approve`로 승인해야 합니다. 변경 없음"}
	}
	switch err := h.m.Approve(ctx, a.ID); {
	case errors.Is(err, store.ErrNotPending):
		return TaskView{}, errEntryNotPend
	case errors.Is(err, store.ErrNotApprovable):
		return TaskView{}, errDestructive
	case err != nil:
		return TaskView{}, err
	}
	t, err = h.st.GetTask(ctx, a.TaskID)
	if err != nil {
		return TaskView{}, err
	}
	return viewOf(t), nil
}
