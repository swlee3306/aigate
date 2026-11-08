package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"aigate/internal/orch/store"
)

// maxReportBytes caps a served report; larger files answer 413.
const maxReportBytes = 2 << 20

// report serves the saved verification report of a task as raw JSON. The
// task row's report_path is data, not a capability: whatever it says, only a
// regular file that really lives under the (symlink-resolved) report dir is
// opened.
func (h *handler) report(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := h.st.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, &httpError{code: 404, msg: "작업을 찾을 수 없습니다"})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	notFound := &httpError{code: 404, msg: "보고서를 찾을 수 없습니다"}
	if h.reportDir == "" || t.ReportPath == "" || !filepath.IsAbs(t.ReportPath) {
		writeErr(w, notFound)
		return
	}
	root, err := filepath.EvalSymlinks(h.reportDir)
	if err != nil {
		writeErr(w, notFound)
		return
	}
	real, err := filepath.EvalSymlinks(t.ReportPath)
	if err != nil || !strings.HasPrefix(real, filepath.Clean(root)+string(filepath.Separator)) {
		writeErr(w, notFound)
		return
	}
	// O_NOFOLLOW closes the window between the check above and the open.
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		writeErr(w, notFound)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		writeErr(w, notFound)
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReportBytes+1))
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(b) > maxReportBytes {
		writeErr(w, &httpError{code: http.StatusRequestEntityTooLarge, msg: "보고서가 너무 큽니다"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
