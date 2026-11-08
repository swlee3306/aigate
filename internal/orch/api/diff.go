package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aigate/internal/gate"
	"aigate/internal/orch/store"
)

// maxDiffBytes caps DiffView.HashedText.
const maxDiffBytes = 256 << 10

// DiffView is the diff endpoint's response: the gate's own protected diff and
// hash for the task's base..head, plus a per-file summary of the whole change.
type DiffView struct {
	// Hash is the approval hash of the full diff (gitx.HashOf of the untruncated
	// HashedText), even when HashedText below is cut.
	Hash           string     `json:"hash"`
	HashedText     string     `json:"hashed_text"`
	Truncated      bool       `json:"truncated"`
	ProtectedFiles []string   `json:"protected_files"`
	Files          []FileStat `json:"files"`
}

// FileStat is one file of `git diff --numstat` (binary files count 0/0).
type FileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

func (h *handler) diff(w http.ResponseWriter, r *http.Request) {
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
	if t.HeadSHA == "" {
		writeErr(w, &httpError{code: http.StatusConflict, msg: "작업에 아직 head 커밋이 없습니다"})
		return
	}
	p, err := gate.Prepare(gate.Options{Repo: t.Repo, Base: t.Base, Head: t.HeadSHA})
	if err != nil {
		writeErr(w, &httpError{code: http.StatusConflict, msg: "diff를 만들 수 없습니다: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	files, err := numstat(ctx, p.Top, p.Base, p.Head)
	if err != nil {
		writeErr(w, err)
		return
	}
	text, truncated, prot := protectedView(p)
	writeJSON(w, http.StatusOK, DiffView{Hash: p.Diff.Hash, HashedText: text, Truncated: truncated,
		ProtectedFiles: prot, Files: files})
}

// protectedView is what a human approves for p: the hashed text (capped at
// maxDiffBytes) and the sorted list of everything that needs approval, changed
// protected files and new guarded files (the latter only show up as trailing
// "A <path>" lines of the text, which truncation cuts first). The diff and the
// approval detail endpoints both use it.
func protectedView(p gate.Protected) (text string, truncated bool, prot []string) {
	text, truncated = capUTF8(p.Diff.HashedText(), maxDiffBytes)
	seen := map[string]bool{}
	prot = []string{}
	for _, f := range append(p.Diff.Changed(), p.AddedGuarded...) {
		if !seen[f] {
			seen[f] = true
			prot = append(prot, f)
		}
	}
	sort.Strings(prot)
	return text, truncated, prot
}

// capUTF8 cuts s to at most max bytes on a rune boundary.
func capUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

// numstat runs git diff --numstat on untrusted agent output: no hooks, no
// external diff or textconv drivers, and a minimal environment (same as the
// publisher's push).
func numstat(ctx context.Context, repo, base, head string) ([]FileStat, error) {
	home, err := os.MkdirTemp("", "orch-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "-c", "core.hooksPath=/dev/null",
		"diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", base, head, "--")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("git diff --numstat 실패: " + strings.TrimSpace(stderr.String()))
	}
	files := []FileStat{}
	for _, rec := range strings.Split(out.String(), "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" (binary) counts as 0
		d, _ := strconv.Atoi(f[1])
		files = append(files, FileStat{Path: f[2], Added: a, Deleted: d})
	}
	return files, nil
}
