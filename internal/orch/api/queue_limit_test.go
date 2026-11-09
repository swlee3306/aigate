package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"aigate/internal/orch/store"
)

const (
	msgPerActor = "내 대기 작업이 가득 찼습니다"
	msgTotal    = "서버 대기열이 가득 찼습니다"
)

func withQueueLimits(per, total int) func(*Config) {
	return func(c *Config) { c.QueueLimit, c.QueueLimitTotal = per, total }
}

// rawDB is a second connection to the test database, for queries and for
// planting rows the API cannot make (a task from before schema v7).
func (e *env) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func errMessage(b []byte) string {
	var m struct{ Error struct{ Message string } }
	_ = json.Unmarshal(b, &m)
	return m.Error.Message
}

// queuedBy counts queued tasks by the actor of their creating event.
func queuedBy(t *testing.T, e *env) map[string]int {
	t.Helper()
	rows, err := e.rawDB(t).QueryContext(context.Background(),
		`SELECT COALESCE((SELECT actor FROM task_events x WHERE x.task_id = t.id AND x.note='created' ORDER BY x.id LIMIT 1), '') FROM tasks t WHERE t.state = 'queued'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out[a]++
	}
	return out
}

func TestQueueLimitIsPerActor(t *testing.T) {
	w := newWebEnv(t, withQueueLimits(3, 100))
	w.d.pauseWorkers()
	tokA, csrfA := w.newSession(t, "alice")
	tokB, csrfB := w.newSession(t, "bob")
	a, b := sessionHdr(tokA, csrfA), sessionHdr(tokB, csrfB)
	for i := 0; i < 3; i++ {
		if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody(fmt.Sprint("a", i)), a); code != 201 {
			t.Fatalf("alice %d: %d %s", i, code, body)
		}
	}
	code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("a-over"), a)
	if code != 429 || v1Code(body) != "queue_full" || errMessage(body) != msgPerActor {
		t.Fatalf("alice over: %d %s", code, body)
	}
	// keyed path is limited per actor too, and bob is unaffected
	kh := with(a, hdr{"Idempotency-Key": "a-keyed"})
	if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("a-keyed"), kh); code != 429 || errMessage(body) != msgPerActor {
		t.Fatalf("alice keyed over: %d %s", code, body)
	}
	for i := 0; i < 3; i++ {
		if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody(fmt.Sprint("b", i)), b); code != 201 {
			t.Fatalf("bob %d (alice's flood must not block him): %d %s", i, code, body)
		}
	}
	if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("b-over"), b); code != 429 || errMessage(body) != msgPerActor {
		t.Fatalf("bob over: %d %s", code, body)
	}
	// the socket principal has its own budget
	if code, _, body := w.sockDo(t, "POST", "/api/v1/tasks", w.submitBody("s0"), nil); code != 201 {
		t.Fatalf("socket: %d %s", code, body)
	}
	got := queuedBy(t, w.env)
	if got["session:alice"] != 3 || got["session:bob"] != 3 || len(got) != 3 {
		t.Fatalf("queued by actor: %v", got)
	}
	// a cancel frees a slot for that actor only
	ts, _ := w.st.ListTasks(context.Background(), 100)
	for _, tk := range ts {
		if tk.Prompt == "a0" {
			if err := w.st.Transition(context.Background(), tk.ID, store.StateQueued, store.StateCancelled, "x"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("b-still"), b); code != 429 {
		t.Fatalf("bob after alice's cancel: %d %s", code, body)
	}
	if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("a-again"), a); code != 201 {
		t.Fatalf("alice after her cancel: %d %s", code, body)
	}
}

func TestQueueLimitTotalCeiling(t *testing.T) {
	w := newWebEnv(t, withQueueLimits(5, 6))
	w.d.pauseWorkers()
	tokA, csrfA := w.newSession(t, "alice")
	tokB, csrfB := w.newSession(t, "bob")
	tokC, csrfC := w.newSession(t, "carol")
	a, b, c := sessionHdr(tokA, csrfA), sessionHdr(tokB, csrfB), sessionHdr(tokC, csrfC)
	for i := 0; i < 3; i++ {
		for _, h := range []hdr{a, b} {
			if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody(fmt.Sprint("p", i)), h); code != 201 {
				t.Fatalf("fill %d: %d %s", i, code, body)
			}
		}
	}
	// carol has no tasks, yet the server is full: every principal gets the total message
	for name, h := range map[string]hdr{"carol": c, "alice": with(a, hdr{"Idempotency-Key": "k-a"}), "bob": b} {
		code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("over-"+name), h)
		if code != 429 || v1Code(body) != "queue_full" || errMessage(body) != msgTotal {
			t.Fatalf("%s over the ceiling: %d %s", name, code, body)
		}
	}
	if code, _, body := w.sockDo(t, "POST", "/tasks", w.submitBody("legacy"), nil); code != 429 || flatCode(body) != "queue_full" {
		t.Fatalf("legacy: %d %s", code, body)
	}
	if n := w.taskCount(t); n != 6 {
		t.Fatalf("%d tasks, want 6", n)
	}
	// the messages never name an actor or a number
	for _, m := range []string{msgPerActor, msgTotal} {
		if strings.ContainsAny(m, "0123456789") || strings.Contains(m, "alice") {
			t.Fatalf("message leaks detail: %q", m)
		}
	}
}

// Two actors, keyed and unkeyed mixed, N+10 concurrent requests each, many
// rounds. No actor exceeds its limit, the total never exceeds the ceiling,
// and the only non-201 answers are the intended 429s.
func TestQueueLimitRaceNeverExceedsPerActorOrTotal(t *testing.T) {
	for _, tc := range []struct{ per, total int }{{3, 100}, {4, 5}} {
		t.Run(fmt.Sprintf("per%d_total%d", tc.per, tc.total), func(t *testing.T) {
			w := newWebEnv(t, withQueueLimits(tc.per, tc.total))
			w.d.pauseWorkers()
			ctx := context.Background()
			toks := map[string]hdr{}
			for _, who := range []string{"alice", "bob"} {
				tok, csrf := w.newSession(t, who)
				toks[who] = sessionHdr(tok, csrf)
			}
			url := "http://" + w.addr + "/api/v1/tasks"
			for round := 0; round < 12; round++ {
				type res struct {
					code int
					body []byte
					err  error
				}
				const n = 2 * (3 + 10)
				out := make([]res, n)
				var wg sync.WaitGroup
				start := make(chan struct{})
				i := 0
				for who, h := range toks {
					for j := 0; j < n/2; j++ {
						k := i
						i++
						hh := with(h, nil)
						if j%2 == 0 {
							hh["Idempotency-Key"] = fmt.Sprintf("r-%d-%s-%d", round, who, j)
						}
						wg.Add(1)
						go func() {
							defer wg.Done()
							<-start
							c, b, err := try(w.hc, "POST", url, webHost, []byte(w.submitBody(fmt.Sprint(who, k))), hh)
							out[k] = res{c, b, err}
						}()
					}
				}
				close(start)
				wg.Wait()
				created := 0
				for _, r := range out {
					switch {
					case r.err != nil:
						t.Fatalf("round %d: %v", round, r.err)
					case r.code == 201:
						created++
					case r.code == 429 && v1Code(r.body) == "queue_full" &&
						(errMessage(r.body) == msgPerActor || errMessage(r.body) == msgTotal):
					default:
						t.Fatalf("round %d: unexpected %d %s", round, r.code, r.body)
					}
				}
				by := queuedBy(t, w.env)
				total := 0
				for a, c := range by {
					total += c
					if c > tc.per {
						t.Fatalf("round %d: %s has %d queued, per-actor limit %d", round, a, c, tc.per)
					}
				}
				if total > tc.total || total != created {
					t.Fatalf("round %d: %d queued, %d created, ceiling %d", round, total, created, tc.total)
				}
				// every request is far over both limits, so the cap itself is reached
				want := 2 * tc.per
				if tc.total < want {
					want = tc.total
				}
				if total != want {
					t.Fatalf("round %d: %d queued, want exactly %d (%v)", round, total, want, by)
				}
				ts, err := w.st.ListTasks(ctx, 1000)
				if err != nil {
					t.Fatal(err)
				}
				for _, tk := range ts {
					if tk.State == store.StateQueued {
						if err := w.st.Transition(ctx, tk.ID, store.StateQueued, store.StateCancelled, "x"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		})
	}
}

// Tasks created before schema v7 (creating event without an actor) and tasks
// made by "system" never count toward an actor, only toward the total.
func TestQueueLimitIgnoresLegacyNullActorTasks(t *testing.T) {
	w := newWebEnv(t, withQueueLimits(2, 5))
	w.d.pauseWorkers()
	for i := 0; i < 3; i++ {
		res, err := w.rawDB(t).Exec(`INSERT INTO tasks(repo, base, prompt, class, agent, state, created_at, updated_at) VALUES ('r','b','legacy','docs','claude','queued',1,1)`)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := w.rawDB(t).Exec(`INSERT INTO task_events(task_id, from_state, to_state, note, at) VALUES (?,'','queued','created',1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	tok, csrf := w.newSession(t, "alice")
	h := sessionHdr(tok, csrf)
	for i := 0; i < 2; i++ {
		if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody(fmt.Sprint("a", i)), h); code != 201 {
			t.Fatalf("alice %d with 3 legacy tasks queued: %d %s", i, code, body)
		}
	}
	// 5 queued now: the total ceiling answers (not the per-actor limit) for a third principal
	tokB, csrfB := w.newSession(t, "bob")
	code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("b0"), sessionHdr(tokB, csrfB))
	if code != 429 || errMessage(body) != msgTotal {
		t.Fatalf("legacy tasks must count toward the total: %d %s", code, body)
	}
	if got := queuedBy(t, w.env); got[""] != 3 || got["session:alice"] != 2 {
		t.Fatalf("by actor: %v", got)
	}
}

// An idempotent replay creates nothing: it is answered 200 even when the
// actor (or the server) is full, and it never changes any count.
func TestQueueLimitReplayNotCounted(t *testing.T) {
	w := newWebEnv(t, withQueueLimits(2, 100))
	w.d.pauseWorkers()
	tok, csrf := w.newSession(t, "alice")
	h := sessionHdr(tok, csrf)
	var ids []string
	for i := 0; i < 2; i++ {
		kh := with(h, hdr{"Idempotency-Key": fmt.Sprintf("rep-%d", i)})
		code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody(fmt.Sprint("p", i)), kh)
		if code != 201 {
			t.Fatalf("%d: %d %s", i, code, body)
		}
		ids = append(ids, string(body))
	}
	// full: a NEW key is refused, the replay of an old key is not
	if code, _, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("new"), with(h, hdr{"Idempotency-Key": "rep-new"})); code != 429 {
		t.Fatalf("new key at the limit: %d %s", code, body)
	}
	for i := 0; i < 3; i++ {
		kh := with(h, hdr{"Idempotency-Key": "rep-0"})
		code, hd, body := w.web(t, "POST", "/api/v1/tasks", w.submitBody("p0"), kh)
		if code != 200 || hd.Get("Idempotent-Replay") != "true" || string(body) != ids[0] {
			t.Fatalf("replay %d: %d %s", i, code, body)
		}
	}
	if n := w.taskCount(t); n != 2 {
		t.Fatalf("%d tasks, want 2", n)
	}
	if got := queuedBy(t, w.env); got["session:alice"] != 2 {
		t.Fatalf("by actor: %v", got)
	}
}
