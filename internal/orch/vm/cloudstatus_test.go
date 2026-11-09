package vm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A realistic `openstack quota show --usage -f json`: other resources are
// present, gigabytes is limited, volumes is unlimited (-1).
const realQuotaJSON = `[
  {"Resource": "backup-gigabytes", "In Use": 0, "Reserved": 0, "Limit": 1000},
  {"Resource": "backups", "In Use": 0, "Reserved": 0, "Limit": 10},
  {"Resource": "cores", "In Use": 4, "Reserved": 0, "Limit": 20},
  {"Resource": "gigabytes", "In Use": 180, "Reserved": 30, "Limit": 300},
  {"Resource": "gigabytes___DEFAULT__", "In Use": 180, "Reserved": 0, "Limit": -1},
  {"Resource": "snapshots", "In Use": 2, "Reserved": 0, "Limit": 10},
  {"Resource": "volumes", "In Use": 7, "Reserved": 0, "Limit": -1},
  {"Resource": "volumes___DEFAULT__", "In Use": 7, "Reserved": 0, "Limit": -1}
]`

// cloudFake is the fake openstack runner: it records every command (after the
// "os" prefix) and never executes anything.
type cloudFake struct {
	mu       sync.Mutex
	cmds     []string
	quota    string
	status   string
	volumes  string
	failWith error         // every call fails with it
	block    chan struct{} // when set, calls wait for it or ctx
	delay    time.Duration
}

func (f *cloudFake) run(ctx context.Context, argv []string) ([]byte, error) {
	cmd := strings.Join(argv[1:], " ")
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	fail, block, delay := f.failWith, f.block, f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail != nil {
		return nil, fail
	}
	switch {
	case strings.HasPrefix(cmd, "quota show --usage -f json"):
		return []byte(f.quota), nil
	case strings.HasPrefix(cmd, "server show test-vm -f value -c status"):
		return []byte(f.status + "\n"), nil
	case strings.HasPrefix(cmd, "volume list -f value -c ID -c Name"):
		return []byte(f.volumes), nil
	}
	return nil, errors.New("fake: unknown command " + cmd)
}

func (f *cloudFake) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *cloudFake) count(prefix string) int {
	n := 0
	for _, c := range f.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type clk struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clk) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clk) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const goodVolumes = "11111111-1111-4111-8111-111111111111 test-vm-root\n" +
	"22222222-2222-4222-8222-222222222222 test-vm-root-20261109-100000-0a1b\n" +
	"33333333-3333-4333-8333-333333333333 test-vm-root-20261108-090000-0c2d\n" +
	"44444444-4444-4444-8444-444444444444 unrelated\n"

func newCloudTest() (*TestVM, *cloudFake, *clk) {
	f := &cloudFake{quota: realQuotaJSON, status: "ACTIVE", volumes: goodVolumes}
	c := &clk{t: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
	cli := &OpenStackCLI{Prefix: []string{"os"}, Run: f.run}
	return &TestVM{Cloud: cli, Now: c.now}, f, c
}

func TestCloudStatusParsesQuota(t *testing.T) {
	tv, _, c := newCloudTest()
	got := tv.CloudStatus(context.Background())
	if !got.OK || got.Code != "" {
		t.Fatalf("not ok: %+v", got)
	}
	if got.Gigabytes != (CloudLimit{Limit: 300, InUse: 180, Reserved: 30}) {
		t.Errorf("gigabytes %+v", got.Gigabytes)
	}
	if !got.Volumes.Unlimited || got.Volumes.InUse != 7 || got.Volumes.Limit != -1 {
		t.Errorf("volumes %+v", got.Volumes)
	}
	if got.VMName != "test-vm" || got.VMStatus != "ACTIVE" {
		t.Errorf("vm %q %q", got.VMName, got.VMStatus)
	}
	if got.RestoreVolumes != 2 { // the golden test-vm-root is not a restore volume
		t.Errorf("restore volumes %d", got.RestoreVolumes)
	}
	if !got.CheckedAt.Equal(c.now()) {
		t.Errorf("checked_at %v", got.CheckedAt)
	}
}

func TestCloudStatusCachedWithinTTL(t *testing.T) {
	tv, f, c := newCloudTest()
	tv.CloudStatus(context.Background())
	c.add(59 * time.Second)
	tv.CloudStatus(context.Background())
	if n := f.count("quota show"); n != 1 {
		t.Fatalf("quota calls within TTL: %d", n)
	}
	c.add(2 * time.Second) // 61 s: stale
	tv.CloudStatus(context.Background())
	if n := f.count("quota show"); n != 2 {
		t.Fatalf("quota calls after TTL: %d", n)
	}
}

func TestCloudStatusSingleFlight(t *testing.T) {
	tv, f, _ := newCloudTest()
	f.block = make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if v := tv.CloudStatus(context.Background()); !v.OK {
				bad.Add(1)
			}
		}()
	}
	close(start)
	time.Sleep(100 * time.Millisecond) // let all callers join the one flight
	close(f.block)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d callers got a failure", bad.Load())
	}
	for _, p := range []string{"quota show", "server show", "volume list"} {
		if n := f.count(p); n != 1 {
			t.Errorf("%s: %d external calls, want 1", p, n)
		}
	}
}

func TestCloudStatusOnlyAllowedVerbs(t *testing.T) {
	tv, f, _ := newCloudTest()
	if v := tv.CloudStatus(context.Background()); !v.OK {
		t.Fatalf("not ok: %+v", v)
	}
	allowed := map[string]bool{
		"quota show --usage -f json":             true,
		"server show test-vm -f value -c status": true,
		"volume list -f value -c ID -c Name":     true,
	}
	for _, c := range f.calls() {
		if !allowed[c] {
			t.Errorf("command outside the read-only set: %q", c)
		}
		if err := (&OpenStackCLI{}).allowed(strings.Fields(c)); err != nil {
			t.Errorf("%q is refused by the driver allow-list: %v", c, err)
		}
	}
	if len(f.calls()) != 3 {
		t.Errorf("calls %v", f.calls())
	}
}

func TestCloudStatusFailureIsCodeOnly(t *testing.T) {
	tv, f, _ := newCloudTest()
	f.failWith = errors.New("ssh jump.internal.example: Permission denied (publickey) 10.0.0.9")
	v := tv.CloudStatus(context.Background())
	if v.OK || v.Code != "cloud_unavailable" {
		t.Fatalf("%+v", v)
	}
	if v.VMStatus != "" || v.VMName != "" {
		t.Errorf("failure carries data: %+v", v)
	}
}

func TestCloudStatusTimeoutIsCloudTimeout(t *testing.T) {
	tv, f, _ := newCloudTest()
	f.block = make(chan struct{}) // never released: only the refresh timeout ends the calls
	tv.CloudTimeout = 50 * time.Millisecond
	start := time.Now()
	v := tv.CloudStatus(context.Background())
	if v.OK || v.Code != "cloud_timeout" {
		t.Fatalf("%+v", v)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestCloudStatusStaleIsNeverServedAsFresh(t *testing.T) {
	tv, f, c := newCloudTest()
	if v := tv.CloudStatus(context.Background()); !v.OK {
		t.Fatalf("%+v", v)
	}
	// Past the TTL the cloud breaks: the old good value must NOT be served.
	c.add(61 * time.Second)
	f.mu.Lock()
	f.failWith = errors.New("boom")
	f.mu.Unlock()
	v := tv.CloudStatus(context.Background())
	if v.OK || v.Code != "cloud_unavailable" || v.Gigabytes.Limit != 0 {
		t.Fatalf("stale good value served after a failed refresh: %+v", v)
	}
	// A failure is remembered only briefly (no call per request while down)...
	n := f.count("quota show")
	c.add(2 * time.Second)
	tv.CloudStatus(context.Background())
	if f.count("quota show") != n {
		t.Errorf("failure was not cached for the short fail TTL")
	}
	// ...and recovery is picked up after it.
	f.mu.Lock()
	f.failWith = nil
	f.mu.Unlock()
	c.add(cloudFailTTL)
	if v := tv.CloudStatus(context.Background()); !v.OK {
		t.Fatalf("no recovery: %+v", v)
	}
}

func TestCloudStatusHistoryNewestFirstMax10(t *testing.T) {
	tv, _, c := newCloudTest()
	for i := 0; i < 12; i++ {
		tv.history = append(tv.history, RestoreRecord{At: c.now().Add(time.Duration(i) * time.Minute), Volume: "v"})
	}
	v := tv.CloudStatus(context.Background())
	if len(v.History) != 10 || !v.History[0].At.After(v.History[1].At) || !v.History[0].At.Equal(c.now().Add(11*time.Minute)) {
		t.Errorf("history %d first %v", len(v.History), v.History[0].At)
	}
}

func TestCloudStatusWithoutReaderIsUnavailable(t *testing.T) {
	tv := &TestVM{}
	if v := tv.CloudStatus(context.Background()); v.OK || v.Code != "cloud_unavailable" {
		t.Errorf("%+v", v)
	}
}

func TestCloudStatusFirstCallerCancelDoesNotPoisonOthers(t *testing.T) {
	tv, f, _ := newCloudTest()
	f.block = make(chan struct{})
	ctxA, cancelA := context.WithCancel(context.Background())
	aDone := make(chan CloudInfo, 1)
	go func() { aDone <- tv.CloudStatus(ctxA) }()
	time.Sleep(50 * time.Millisecond) // A started the refresh
	bDone := make(chan CloudInfo, 1)
	go func() { bDone <- tv.CloudStatus(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	cancelA()
	if a := <-aDone; a.OK || a.Code != CloudTimeout {
		t.Errorf("A: %+v", a)
	}
	close(f.block)
	if b := <-bDone; !b.OK {
		t.Fatalf("B poisoned by A's cancel: %+v", b)
	}
	if v := tv.CloudStatus(context.Background()); !v.OK || f.count("quota show") != 1 {
		t.Errorf("cache not filled: %+v calls %d", v, f.count("quota show"))
	}
}

type panicReader struct{ which string }

func (p panicReader) QuotaUsage(context.Context) (string, error) {
	if p.which == "quota" {
		panic("boom quota")
	}
	return realQuotaJSON, nil
}
func (p panicReader) ServerStatus(context.Context, string) (string, error) {
	if p.which == "server" {
		panic("boom server")
	}
	return "ACTIVE", nil
}
func (p panicReader) CountRestoreVolumes(context.Context) (int, error) {
	if p.which == "volumes" {
		panic("boom volumes")
	}
	return 1, nil
}
func (p panicReader) DeleteServer(context.Context, string) error                     { return nil }
func (p panicReader) CreateVolumeFromSnapshot(context.Context, string, string) error { return nil }
func (p panicReader) CreateServer(context.Context, string, string) error             { return nil }
func (p panicReader) WaitSSH(context.Context, string) error                          { return nil }

func TestCloudStatusReaderPanicIsUnavailable(t *testing.T) {
	for _, w := range []string{"quota", "server", "volumes"} {
		tv := &TestVM{Cloud: panicReader{w}}
		v := tv.CloudStatus(context.Background())
		if w == "server" { // an unreadable server is "unknown", not an outage
			if !v.OK || v.VMStatus != "unknown" {
				t.Errorf("%s: %+v", w, v)
			}
			continue
		}
		if v.OK || v.Code != CloudUnavailable {
			t.Errorf("%s: %+v", w, v)
		}
	}
}
