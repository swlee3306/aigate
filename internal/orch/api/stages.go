package api

import (
	"fmt"
	"net/http"

	"aigate/internal/orch/store"
)

// Stage is one entry of the console's stage menu.
type Stage struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"` // ✅ done, 🔄 in progress, ⬜ not started
	Components []string `json:"components"`
	Next       string   `json:"next"`
}

const noPendingNext = "대기 중인 승인이 없습니다"

// stageTable is the fixed stage menu. Source: 전체_계획서.md (2026-11-09).
// The approve stage's Next is replaced at request time (see stages).
var stageTable = []Stage{
	{"design", "설계", "⬜", []string{}, "요구사항·설계서는 아직 Claude 세션에서 작성합니다 (W4에서 웹으로)"},
	{"implement", "구현", "🔄", []string{"오케스트레이터 ②", "에이전트 VM"}, "작업을 제출하고 진행을 봅니다"},
	{"verify", "검증", "✅", []string{"게이트 ①", "검사 VM"}, "판정 결과와 보고서를 확인합니다"},
	{"approve", "승인", "🔄", []string{"승인 대기 목록"}, noPendingNext},
	{"publish", "게시·병합", "🔄", []string{"push_branch", "GitLab"}, "게시된 브랜치를 GitLab에서 검토·병합합니다 (병합은 수동)"},
	{"operate", "운영", "⬜", []string{"정책", "복원·정리"}, "백업·모니터링은 아직 없습니다"},
}

func (h *handler) stages(w http.ResponseWriter, r *http.Request) {
	as, err := h.st.PendingApprovals(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]Stage, len(stageTable))
	copy(out, stageTable)
	if len(as) > 0 {
		for i := range out {
			if out[i].ID == "approve" {
				out[i].Next = fmt.Sprintf("승인 대기 %d건", len(as))
			}
		}
	}
	writeJSON(w, 200, out)
}

// TodoItem is one thing waiting for the operator.
type TodoItem struct {
	Kind     string `json:"kind"`
	Task     int64  `json:"task"`
	Approval int64  `json:"approval"`
	Text     string `json:"text"`
}

// Overview is the dashboard summary.
type Overview struct {
	Total            int                `json:"total"`
	ByState          map[string]int     `json:"by_state"`
	PendingApprovals int                `json:"pending_approvals"`
	StageAvg         map[string]float64 `json:"stage_avg"` // seconds per visit, from recent done tasks
	Todo             []TodoItem         `json:"todo"`
}

const (
	overviewTaskLimit = 500
	stageAvgTasks     = 20
)

// avgStates are the timed states stageAvg reports.
var avgStates = map[string]bool{
	string(store.StateQueued): true, string(store.StatePreparing): true,
	string(store.StateAgentRunning): true, string(store.StateCollecting): true,
	string(store.StateVerifying): true, string(store.StatePublishing): true,
}

// stageAvg averages, per state, how long tasks stayed in it (per visit),
// using Timeline on each task's events. States never visited have no key.
func stageAvg(tasks [][]store.Event) map[string]float64 {
	sum := map[string]float64{}
	n := map[string]int{}
	for _, evs := range tasks {
		for _, st := range Timeline(evs) {
			if avgStates[st.State] {
				sum[st.State] += st.Seconds
				n[st.State]++
			}
		}
	}
	out := make(map[string]float64, len(sum))
	for k, v := range sum {
		out[k] = v / float64(n[k])
	}
	return out
}

func (h *handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ts, err := h.st.ListTasks(ctx, overviewTaskLimit)
	if err != nil {
		writeErr(w, err)
		return
	}
	ov := Overview{Total: len(ts), ByState: map[string]int{}, Todo: []TodoItem{}}
	var evs [][]store.Event
	for _, t := range ts { // newest first
		ov.ByState[string(t.State)]++
		if t.State == store.StateDone && len(evs) < stageAvgTasks {
			e, err := h.st.Events(ctx, t.ID)
			if err != nil {
				writeErr(w, err)
				return
			}
			evs = append(evs, e)
		}
	}
	ov.StageAvg = stageAvg(evs)
	as, err := h.st.PendingApprovals(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	ov.PendingApprovals = len(as)
	for _, a := range as {
		ov.Todo = append(ov.Todo, TodoItem{Kind: "approval", Task: a.TaskID, Approval: a.ID,
			Text: fmt.Sprintf("작업 #%d 승인 대기: %s", a.TaskID, a.Reason)})
	}
	writeJSON(w, 200, ov)
}
