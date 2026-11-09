package api

import (
	"errors"
	"net/http"

	"aigate/internal/orch/steps"
	"aigate/internal/orch/store"
)

// mrRetry serves POST /api/v1/tasks/{id}/mr (write tier): create the Draft
// merge request of a done task whose MR failed or was skipped, as the
// requesting principal. Answers 200 with the task view; 409 mr_not_applicable
// (not a done task with a failed/skipped MR, no broker, repository not
// mapped) or mr_already_created; 502 upstream_error when the broker failed
// (the failure is recorded on the task with its fixed code; for bad_url a
// retry only reaches the same answer). The sentences are fixed; no broker or
// GitLab text is ever echoed.
func (h *handler) mrRetry(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if h.retryMR == nil {
		writeErr(w, errMRNotApplicable)
		return
	}
	t, err := h.retryMR(r.Context(), id)
	var me *steps.MRError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, viewOf(t))
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, apiErr(http.StatusNotFound, "not_found", "작업을 찾을 수 없습니다"))
	case errors.Is(err, steps.ErrMRNotApplicable):
		writeErr(w, errMRNotApplicable)
	case errors.Is(err, steps.ErrMRAlreadyCreated):
		writeErr(w, apiErr(http.StatusConflict, "mr_already_created", "병합 요청이 이미 만들어져 있습니다"))
	case errors.As(err, &me):
		e := apiErr(http.StatusBadGateway, "upstream_error", "병합 요청을 만들지 못했습니다. 작업 결과는 그대로입니다")
		e.reason = "코드: " + me.Code
		writeErr(w, e)
	default:
		writeErr(w, err)
	}
}

var errMRNotApplicable = apiErr(http.StatusConflict, "mr_not_applicable",
	"이 작업에는 병합 요청을 다시 만들 수 없습니다 (완료된 작업이고 병합 요청이 실패했거나 건너뛴 상태여야 하며, 저장소가 연결되어 있어야 합니다)")
