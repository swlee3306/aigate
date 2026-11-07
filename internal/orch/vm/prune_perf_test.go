package vm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- quota gate: the read-only verb and the headroom rule ----

func quotaDoc(gbLimit, gbUsed, gbRes, volLimit, volUsed, volRes int) string {
	return fmt.Sprintf(`[{"Resource":"gigabytes","Limit":%d,"In Use":%d,"Reserved":%d},
 {"Resource":"volumes","Limit":%d,"In Use":%d,"Reserved":%d},
 {"Resource":"snapshots","Limit":10,"In Use":10,"Reserved":0},
 {"Resource":"backup-gigabytes","Limit":1000,"In Use":1000,"Reserved":0}]`, gbLimit, gbUsed, gbRes, volLimit, volUsed, volRes)
}

func TestAllowListQuotaShowOnlyThatVerb(t *testing.T) {
	_, o, _, _ := scene(t)
	if err := o.allowed([]string{"quota", "show", "--usage", "-f", "json"}); err != nil {
		t.Fatalf("quota show refused: %v", err)
	}
	for _, a := range [][]string{
		{"quota", "show"},
		{"quota", "show", "--usage"},
		{"quota", "show", "--usage", "-f", "json", "admin"},
		{"quota", "show", "-f", "json", "--usage"},
		{"quota", "set", "--gigabytes", "9999"},
		{"quota", "set", "--usage", "-f", "json"},
		{"quota", "list", "--usage", "-f", "json"},
		{"server", "show", "test-vm", "-f", "json", "-c", "volumes_attached"}, // replaced by the id+volumes_attached call
	} {
		if err := o.allowed(a); err == nil {
			t.Errorf("%v was allowed", a)
		}
	}
}

func TestRestoreFitsHeadroomRule(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		size  string // snapshot size answer
		want  bool
		isErr bool
	}{
		{"plenty", quotaDoc(300, 100, 0, 100, 10, 0), "30", true, false},
		{"exactly twice the size and two volumes", quotaDoc(300, 240, 0, 100, 98, 0), "30", true, false},
		{"one GB short", quotaDoc(300, 241, 0, 100, 10, 0), "30", false, false},
		{"reserved counts", quotaDoc(300, 200, 41, 100, 10, 0), "30", false, false},
		{"one volume of headroom", quotaDoc(300, 100, 0, 100, 99, 0), "30", false, false},
		{"reserved volumes count", quotaDoc(300, 100, 0, 100, 98, 1), "30", false, false},
		{"unlimited gigabytes", quotaDoc(-1, 99999, 0, 100, 10, 0), "30", true, false},
		{"unlimited volumes", quotaDoc(300, 100, 0, -1, 99999, 0), "30", true, false},
		{"garbage quota", "not json", "30", false, true},
		{"missing volumes resource", `[{"Resource":"gigabytes","Limit":300,"In Use":0,"Reserved":0}]`, "30", false, true},
		{"missing gigabytes resource", `[{"Resource":"volumes","Limit":100,"In Use":0,"Reserved":0}]`, "30", false, true},
		{"non numeric usage", `[{"Resource":"gigabytes","Limit":300,"In Use":"x","Reserved":0},{"Resource":"volumes","Limit":9,"In Use":0,"Reserved":0}]`, "30", false, true},
		{"garbage size", quotaDoc(300, 0, 0, 100, 0, 0), "thirty", false, true},
		{"zero size", quotaDoc(300, 0, 0, 100, 0, 0), "0", false, true},
		{"type specific gigabytes is tight", `[{"Resource":"gigabytes","Limit":300,"In Use":0,"Reserved":0},{"Resource":"volumes","Limit":9,"In Use":0,"Reserved":0},
			{"Resource":"gigabytes_ssd","Limit":50,"In Use":0,"Reserved":0}]`, "30", false, false},
		{"type specific roomy", `[{"Resource":"gigabytes","Limit":300,"In Use":0,"Reserved":0},{"Resource":"volumes","Limit":9,"In Use":0,"Reserved":0},
			{"Resource":"gigabytes_ssd","Limit":-1,"In Use":0,"Reserved":0},{"Resource":"volumes_ssd","Limit":-1,"In Use":0,"Reserved":0}]`, "30", true, false},
	}
	for _, tc := range cases {
		c, o, _, _ := scene(t)
		c.quotaJSON = tc.doc
		o.Run = func(ctx context.Context, argv []string) ([]byte, error) {
			if strings.Join(argv[1:], " ") == "volume snapshot show test-vm-clean -f value -c size" {
				return []byte(tc.size), nil
			}
			return c.run(ctx, argv)
		}
		got, detail, err := o.RestoreFits(context.Background())
		if (err != nil) != tc.isErr || got != tc.want {
			t.Errorf("%s: got %v %q %v, want %v (err %v)", tc.name, got, detail, err, tc.want, tc.isErr)
		}
		if err != nil && got {
			t.Errorf("%s: fits despite an error", tc.name)
		}
	}
	// a failing call is an error, not "fits"
	c, o, _, _ := scene(t)
	c.quotaJSON = quotaDoc(300, 0, 0, 100, 0, 0)
	c.fail["quota show"] = true
	if ok, _, err := o.RestoreFits(context.Background()); ok || err == nil {
		t.Fatal("failing quota show must not fit")
	}
	delete(c.fail, "quota show")
	c.fail["volume snapshot show test-vm-clean"] = true
	if ok, _, err := o.RestoreFits(context.Background()); ok || err == nil {
		t.Fatal("failing size read must not fit")
	}
}

// ---- TestVM against the openstack stub ----

func quotaScene(t *testing.T, quota string) (*sCloud, *OpenStackCLI, *TestVM, chan PruneResult) {
	c := newSCloud()
	golden := c.addVol("test-vm-root", "available")
	c.snaps[c.uid()] = golden.id
	prev := c.addVol("test-vm-root-20261105-100000-aaaa", "in-use")
	sid := c.uid()
	c.servers[sid] = "test-vm"
	c.servers[c.uid()] = "agent-vm"
	c.bootOf["test-vm"] = prev.id
	prev.atts = [][2]string{{c.uid(), sid}}
	old := c.addVol("test-vm-root-20261101-100000-bbbb", "available")
	stale(c, old)
	c.quotaJSON = quota
	o := &OpenStackCLI{Prefix: []string{"os"}, Run: c.run, PruneRestoreVolumes: true, PollInterval: time.Millisecond, Probe: func(context.Context) error { return nil }}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(o, &now)
	v.PruneRestoreVolumes = true
	ch := make(chan PruneResult, 4)
	v.OnPrune = func(r PruneResult, err error) { ch <- r }
	return c, o, v, ch
}

func phaseNames(ps []Phase) string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Name)
	}
	return strings.Join(n, ",")
}

func TestQuotaOKSkipsPrePruneAndPrunesInBackground(t *testing.T) {
	c, _, v, ch := quotaScene(t, quotaDoc(300, 100, 0, 100, 10, 0))
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	var res PruneResult
	select {
	case res = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("background prune never reported")
	}
	rec, _ := v.LastRestore()
	if got := phaseNames(rec.Phases); !strings.HasPrefix(got, "delete_server,quota_ok,create_volume,create_server,wait_ssh") || strings.Contains(got, "pre_prune") {
		t.Fatalf("phases %s", got)
	}
	if !strings.Contains(phaseNames(rec.Phases), "post_prune_bg") {
		t.Fatalf("background phase not merged: %s", phaseNames(rec.Phases))
	}
	// the old boot volume and the old stale one are gone, merged into the matching record
	if len(res.Deleted) != 2 || len(rec.Pruned) != 2 || len(rec.PruneWarnings) != 0 {
		t.Fatalf("res %+v rec %+v", res, rec)
	}
	// nothing was deleted before the new volume existed
	idx := func(sub string) int {
		for i, l := range c.log {
			if strings.Contains(l, sub) {
				return i
			}
		}
		return -1
	}
	if idx("volume delete") < idx("volume create") {
		t.Fatalf("a delete ran before create volume: %v", c.log)
	}
}

func TestQuotaTightRunsPrePruneAndNoPostPrune(t *testing.T) {
	c, _, v, ch := quotaScene(t, quotaDoc(300, 250, 0, 100, 10, 0))
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	got := phaseNames(rec.Phases)
	if strings.Contains(got, "quota_ok") || !strings.Contains(got, "pre_prune") || strings.Contains(got, "post_prune") {
		t.Fatalf("phases %s", got)
	}
	if len(rec.Pruned) != 2 {
		t.Fatalf("pre-prune deleted %v", rec.Pruned)
	}
	select {
	case <-ch:
		t.Fatal("a background prune ran after the pre-prune did")
	case <-time.After(50 * time.Millisecond):
	}
	listsAfterCreate := 0
	seen := false
	for _, l := range c.log {
		if strings.HasPrefix(l, "volume create") {
			seen = true
		} else if seen && strings.HasPrefix(l, "volume list") {
			listsAfterCreate++
		}
	}
	if listsAfterCreate != 0 {
		t.Fatalf("a prune ran after the restore: %v", c.log)
	}
}

func TestQuotaUnreadableFallsBackToPrePrune(t *testing.T) {
	for _, doc := range []string{"", "garbage"} {
		_, _, v, ch := quotaScene(t, doc)
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		rec, _ := v.LastRestore()
		got := phaseNames(rec.Phases)
		if strings.Contains(got, "quota_ok") || !strings.Contains(got, "pre_prune") || strings.Contains(got, "post_prune") {
			t.Fatalf("%q phases %s", doc, got)
		}
		if len(rec.Pruned) != 2 {
			t.Fatalf("pre-prune deleted %v", rec.Pruned)
		}
		select {
		case <-ch:
			t.Fatal("background prune ran")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ---- background prune scheduling, with a controllable cloud ----

type bgCloud struct {
	mu       sync.Mutex
	fits     bool
	fitsErr  error
	cur, max int
	prunes   int
	pre      int
	deletes  int
	fitsN    int
	block    chan struct{} // Prune waits for it when non-nil
	started  chan struct{} // receives once per Prune start
	panicBG  bool
	ctxErr   error // ctx.Err() seen by the last Prune after release
	order    []string
	res      PruneResult
}

func (c *bgCloud) enter(what string) {
	c.mu.Lock()
	c.cur++
	if c.cur > c.max {
		c.max = c.cur
	}
	c.order = append(c.order, what)
	c.mu.Unlock()
}
func (c *bgCloud) leave() { c.mu.Lock(); c.cur--; c.mu.Unlock() }
func (c *bgCloud) DeleteServer(context.Context, string) error {
	c.mu.Lock()
	c.deletes++
	c.order = append(c.order, "DeleteServer")
	c.mu.Unlock()
	return nil
}
func (c *bgCloud) CreateVolumeFromSnapshot(context.Context, string, string) error { return nil }
func (c *bgCloud) CreateServer(context.Context, string, string) error             { return nil }
func (c *bgCloud) WaitSSH(context.Context, string) error                          { return nil }
func (c *bgCloud) RestoreFits(context.Context) (bool, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fitsN++
	if c.fitsErr != nil {
		return false, "", c.fitsErr
	}
	return c.fits, "headroom low", nil
}
func (c *bgCloud) PrunePreRestore(ctx context.Context) (PruneResult, error) {
	c.enter("PrePrune")
	defer c.leave()
	c.mu.Lock()
	c.pre++
	c.mu.Unlock()
	return PruneResult{Deleted: []string{"pre 1"}}, nil
}
func (c *bgCloud) Prune(ctx context.Context) (PruneResult, error) {
	c.enter("Prune")
	defer c.leave()
	c.mu.Lock()
	c.prunes++
	c.mu.Unlock()
	if c.started != nil {
		c.started <- struct{}{}
	}
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	c.ctxErr = ctx.Err()
	c.mu.Unlock()
	if c.panicBG {
		panic("bg oops")
	}
	return c.res, nil
}

func newBG(c *bgCloud) (*TestVM, chan error) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(c, &now)
	v.Now = time.Now
	v.PruneRestoreVolumes = true
	ch := make(chan error, 8)
	v.OnPrune = func(r PruneResult, err error) { ch <- err }
	return v, ch
}

func recv(t *testing.T, ch chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return nil
	}
}

func TestRestoreReturnsBeforeSlowBackgroundPruneCompletes(t *testing.T) {
	c := &bgCloud{fits: true, block: make(chan struct{}), started: make(chan struct{}, 4), res: PruneResult{Deleted: []string{"old 9"}, Skipped: []string{"s 1: x"}}}
	v, done := newBG(c)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("prune finished although it is blocked")
	default:
	}
	rec, _ := v.LastRestore()
	if len(rec.Pruned) != 0 || strings.Contains(phaseNames(rec.Phases), "post_prune") {
		t.Fatalf("record already has the prune: %+v", rec)
	}
	if v.Usable() != nil {
		t.Fatal("VM not usable after restore")
	}
	<-c.started
	close(c.block)
	if err := recv(t, done, "OnPrune"); err != nil {
		t.Fatal(err)
	}
	rec, _ = v.LastRestore()
	if len(rec.Pruned) != 1 || rec.Pruned[0] != "old 9" || len(rec.PruneSkipped) != 1 || !strings.Contains(phaseNames(rec.Phases), "post_prune_bg") {
		t.Fatalf("result not merged: %+v", rec)
	}
	if c.pre != 0 || c.prunes != 1 {
		t.Fatalf("pre %d prunes %d", c.pre, c.prunes)
	}
}

func TestNextRestoreWaitsForBackgroundPrune(t *testing.T) {
	c := &bgCloud{fits: true, block: make(chan struct{}), started: make(chan struct{}, 4)}
	v, done := newBG(c)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-c.started
	second := make(chan error, 1)
	go func() { second <- v.Restore(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	c.mu.Lock()
	n := c.deletes
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("the next restore deleted the server (%d) while a prune was in flight", n)
	}
	select {
	case <-second:
		t.Fatal("second restore finished while the prune was in flight")
	default:
	}
	close(c.block)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	recv(t, done, "first OnPrune")
	// the second restore's own background prune (block is closed) also finishes
	recv(t, done, "second OnPrune")
	if c.max != 1 {
		t.Fatalf("prunes overlapped: max concurrent %d", c.max)
	}
}

func TestWaitTimeoutProceedsWithWarningAndPrunesStillNeverOverlap(t *testing.T) {
	c := &bgCloud{fits: true, block: make(chan struct{}), started: make(chan struct{}, 4)}
	v, done := newBG(c)
	v.PruneWait = 30 * time.Millisecond
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-c.started
	c.mu.Lock()
	c.fits = false // the second restore must pre-prune, which needs the same lock
	c.mu.Unlock()
	go func() { time.Sleep(150 * time.Millisecond); close(c.block) }()
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	var w *Phase
	for i := range rec.Phases {
		if rec.Phases[i].Name == "prune_wait" {
			w = &rec.Phases[i]
		}
	}
	if w == nil || w.Err == "" || !strings.Contains(strings.Join(rec.PruneWarnings, "|"), "background prune") {
		t.Fatalf("no wait warning: %s / %v", phaseString(rec.Phases), rec.PruneWarnings)
	}
	recv(t, done, "OnPrune")
	if c.max != 1 {
		t.Fatalf("prunes overlapped: max concurrent %d", c.max)
	}
	if c.pre != 1 {
		t.Fatalf("pre-prune ran %d times", c.pre)
	}
}

func TestBackgroundPrunePanicDoesNotCrash(t *testing.T) {
	c := &bgCloud{fits: true, panicBG: true}
	v, done := newBG(c)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := recv(t, done, "OnPrune")
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err %v", err)
	}
	rec, _ := v.LastRestore()
	if !strings.Contains(strings.Join(rec.PruneWarnings, "|"), "panicked") {
		t.Fatalf("%+v", rec)
	}
	// the VM keeps working: a second restore runs (and its prune panics again)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv(t, done, "second OnPrune")
}

func TestPanickingOnPruneCallbackDoesNotCrash(t *testing.T) {
	c := &bgCloud{fits: true}
	v, _ := newBG(c)
	called := make(chan struct{}, 1)
	v.OnPrune = func(PruneResult, error) { called <- struct{}{}; panic("callback oops") }
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-called
	if err := v.Restore(context.Background()); err != nil { // would block 2 min if done was never closed
		t.Fatal(err)
	}
}

func TestTaskContextCancelDoesNotCancelBackgroundPrune(t *testing.T) {
	c := &bgCloud{fits: true, block: make(chan struct{}), started: make(chan struct{}, 2), res: PruneResult{Deleted: []string{"x 1"}}}
	v, done := newBG(c)
	ctx, cancel := context.WithCancel(context.Background())
	if err := v.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	<-c.started
	cancel()
	time.Sleep(30 * time.Millisecond)
	close(c.block)
	if err := recv(t, done, "OnPrune"); err != nil {
		t.Fatal(err)
	}
	if c.ctxErr != nil {
		t.Fatalf("the task's cancel reached the background prune: %v", c.ctxErr)
	}
	if rec, _ := v.LastRestore(); len(rec.Pruned) != 1 {
		t.Fatalf("%+v", rec)
	}
}

func TestPruneOffStartsNothing(t *testing.T) {
	c := &bgCloud{fits: true}
	v, done := newBG(c)
	v.PruneRestoreVolumes = false
	before := runtimeGoroutines()
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if c.fitsN != 0 || c.prunes != 0 || c.pre != 0 || len(done) != 0 || v.bgDone != nil {
		t.Fatalf("something ran: fits %d prunes %d pre %d", c.fitsN, c.prunes, c.pre)
	}
	if after := runtimeGoroutines(); after > before {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
	rec, _ := v.LastRestore()
	if got := phaseNames(rec.Phases); got != "delete_server,create_volume,create_server,wait_ssh" {
		t.Fatalf("phases %s", got)
	}
}

func TestQuotaGateErrorOrPanicRunsPrePrune(t *testing.T) {
	c := &bgCloud{fitsErr: errors.New("no quota")}
	v, done := newBG(c)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	if got := phaseNames(rec.Phases); !strings.Contains(got, "quota_check") || !strings.Contains(got, "pre_prune") || strings.Contains(got, "quota_ok") {
		t.Fatalf("phases %s", got)
	}
	if c.pre != 1 || c.prunes != 0 || len(done) != 0 {
		t.Fatalf("pre %d prunes %d", c.pre, c.prunes)
	}
}

func TestFailedRestoreAfterQuotaOKStartsNoBackgroundPrune(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &bgFailCloud{bgCloud: bgCloud{fits: true}}
	v := newTestVM(c, &now)
	v.Now = time.Now
	v.PruneRestoreVolumes = true
	if v.Restore(context.Background()) == nil {
		t.Fatal("want failure")
	}
	time.Sleep(20 * time.Millisecond)
	if c.prunes != 0 || v.bgDone != nil {
		t.Fatal("prune ran after a failed restore")
	}
}

type bgFailCloud struct{ bgCloud }

func (c *bgFailCloud) CreateServer(context.Context, string, string) error { return errors.New("boom") }

func runtimeGoroutines() int { return goroutineCount() }

// ---- parallel read-only state calls ----

func TestPruneStateCallCountsAndParallelismCap(t *testing.T) {
	c, o, _, _ := scene(t)
	for i := 0; i < 8; i++ { // 9 snapshots in all
		v := c.addVol(fmt.Sprintf("other-%d", i), "available")
		c.snaps[c.uid()] = v.id
	}
	c.delay = 20 * time.Millisecond
	st, err := o.pruneState(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.origins) != 9 || len(st.boot) != 1 {
		t.Fatalf("state %+v", st)
	}
	if c.maxInflight < 2 || c.maxInflight > 4 {
		t.Fatalf("max concurrent calls %d, want 2..4", c.maxInflight)
	}
	count := func(prefix string) int {
		n := 0
		for _, l := range c.log {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	if count("server show") != 1 || count("server list") != 1 || count("volume snapshot list") != 1 || count("volume snapshot show") != 9 {
		t.Fatalf("calls: %v", c.log)
	}
	// server-absent mode too
	c2, o2, _, _ := absentScene(t)
	for i := 0; i < 8; i++ {
		v := c2.addVol(fmt.Sprintf("other-%d", i), "available")
		c2.snaps[c2.uid()] = v.id
	}
	c2.delay = 20 * time.Millisecond
	if _, err := o2.pruneState(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if c2.maxInflight < 2 || c2.maxInflight > 4 {
		t.Fatalf("absent: max concurrent calls %d", c2.maxInflight)
	}
}

func TestPruneStateAnyParallelFailureAbortsAndDeletesNothing(t *testing.T) {
	for _, absent := range []bool{false, true} {
		for _, k := range []string{"server list", "volume snapshot list", "server show test-vm -f json", "SNAP3", "SNAPBAD"} {
			if absent && k == "server show test-vm -f json" {
				continue // not used in server-absent mode
			}
			var c *sCloud
			var o *OpenStackCLI
			if absent {
				c, o, _, _ = absentScene(t)
			} else {
				c, o, _, _ = scene(t)
			}
			var snaps []string
			for i := 0; i < 5; i++ {
				v := c.addVol(fmt.Sprintf("other-%d", i), "available")
				id := c.uid()
				c.snaps[id] = v.id
				snaps = append(snaps, id)
			}
			c.addVol("test-vm-root-20261101-100000-bbbb", "available")
			c.delay = 5 * time.Millisecond
			n := len(c.vols)
			switch k {
			case "SNAP3":
				c.fail["volume snapshot show "+snaps[3]] = true
			case "SNAPBAD":
				o.Run = func(ctx context.Context, argv []string) ([]byte, error) {
					if strings.HasPrefix(strings.Join(argv[1:], " "), "volume snapshot show "+snaps[2]) {
						return []byte("not-a-uuid"), nil
					}
					return c.run(ctx, argv)
				}
			default:
				c.fail[k] = true
			}
			var res PruneResult
			var err error
			if absent {
				res, err = o.PrunePreRestore(context.Background())
			} else {
				res, err = o.Prune(context.Background())
			}
			if err == nil && len(res.Warnings) == 0 { // non-absent Prune reports an unjudgeable state per volume, as warnings
				t.Errorf("absent=%v %s: neither error nor warning", absent, k)
			}
			if len(res.Deleted) != 0 || len(c.vols) != n || c.deleted("volume delete") || c.deleted("attachment delete") {
				t.Errorf("absent=%v %s: deleted %+v err %v", absent, k, res, err)
			}
		}
	}
}

func goroutineCount() int { return runtime.NumGoroutine() }
