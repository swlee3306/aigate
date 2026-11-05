package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aigate/internal/remote"
)

type fakeCloud struct {
	calls          []string
	failAt         string // method name to fail
	clock          *time.Time
	cost           time.Duration // clock advance per call
	deletedVolumes int
}

func (c *fakeCloud) step(name string) error {
	c.calls = append(c.calls, name)
	if c.clock != nil {
		*c.clock = c.clock.Add(c.cost)
	}
	if c.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}
func (c *fakeCloud) DeleteServer(ctx context.Context, s string) error { return c.step("DeleteServer") }
func (c *fakeCloud) CreateVolumeFromSnapshot(ctx context.Context, snap, v string) error {
	c.calls = append(c.calls, "vol:"+snap+":"+v)
	return c.step("CreateVolumeFromSnapshot")
}
func (c *fakeCloud) CreateServer(ctx context.Context, s, v string) error {
	return c.step("CreateServer")
}
func (c *fakeCloud) WaitSSH(ctx context.Context, s string) error { return c.step("WaitSSH") }

func newTestVM(c Cloud, clock *time.Time) *TestVM {
	return &TestVM{Cloud: c, Now: func() time.Time { return *clock }, Suffix: func() string { return "ab12" },
		SSH: remote.Config{Host: "test-vm", User: "runner", KeyPath: "/k", KnownHosts: "/kh", RepoDir: "/r"}}
}

func TestRestoreOrderAndExecutor(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &fakeCloud{}
	v := newTestVM(c, &now)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.calls, ","); got != "DeleteServer,vol:test-vm-clean:test-vm-root-20261106-100000-ab12,CreateVolumeFromSnapshot,CreateServer,WaitSSH" {
		t.Fatalf("calls: %s", got)
	}
	ex, err := v.Executor()
	if err != nil || ex == nil {
		t.Fatalf("executor: %v", err)
	}
	if _, ok := ex.(*remote.SSH); !ok {
		t.Fatalf("executor is %T, want *remote.SSH", ex)
	}
}

func TestRestoreFailureMarksVMUnusable(t *testing.T) {
	for _, at := range []string{"DeleteServer", "CreateVolumeFromSnapshot", "CreateServer", "WaitSSH"} {
		now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
		c := &fakeCloud{failAt: at}
		v := newTestVM(c, &now)
		if err := v.Restore(context.Background()); err == nil || !strings.Contains(err.Error(), at+" failed") {
			t.Fatalf("%s: err = %v", at, err)
		}
		if ex, err := v.Executor(); err == nil || ex != nil {
			t.Fatalf("%s: executor handed out after a failed restore (%v, %v)", at, ex, err)
		}
		if v.Usable() == nil {
			t.Fatalf("%s: VM reported usable", at)
		}
		rec, _ := v.LastRestore()
		if rec.Err == "" {
			t.Fatalf("%s: failure not recorded", at)
		}
		// stays unusable until a restore succeeds
		c.failAt = ""
		now = now.Add(time.Hour)
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ex, err := v.Executor(); err != nil || ex == nil {
			t.Fatalf("%s: not usable after a successful restore: %v", at, err)
		}
	}
}

func TestRestoreIsBoundedAndFailsOnTimeout(t *testing.T) {
	v := &TestVM{Cloud: blockingCloud{}, RestoreTimeout: 50 * time.Millisecond,
		SSH: remote.Config{Host: "test-vm", User: "runner", KeyPath: "/k", KnownHosts: "/kh", RepoDir: "/r"}}
	if err := v.Restore(context.Background()); err == nil {
		t.Fatal("restore did not time out")
	}
	if _, err := v.Executor(); err == nil {
		t.Fatal("executor after timed-out restore")
	}
}

type blockingCloud struct{ fakeCloud }

func (blockingCloud) WaitSSH(ctx context.Context, s string) error                    { <-ctx.Done(); return ctx.Err() }
func (blockingCloud) DeleteServer(context.Context, string) error                     { return nil }
func (blockingCloud) CreateVolumeFromSnapshot(context.Context, string, string) error { return nil }
func (blockingCloud) CreateServer(context.Context, string, string) error             { return nil }

func TestRestoreSlowIsRecorded(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &fakeCloud{clock: &now, cost: 20 * time.Second} // 4 steps = 80 s
	v := newTestVM(c, &now)
	v.SlowAfter = time.Minute
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := v.LastRestore()
	if !ok || rec.Duration != 80*time.Second || !rec.Slow || rec.Err != "" {
		t.Fatalf("%+v", rec)
	}
	c.cost = time.Second
	now = now.Add(time.Hour)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ = v.LastRestore()
	if rec.Duration != 4*time.Second || rec.Slow {
		t.Fatalf("%+v", rec)
	}
	if h := v.History(); len(h) != 2 {
		t.Fatalf("history %d", len(h))
	}
}

func TestPruneCandidatesListOldVolumesOnly(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &fakeCloud{}
	v := newTestVM(c, &now)
	for i := 0; i < 3; i++ {
		now = now.Add(time.Minute)
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := v.PruneCandidates()
	if strings.Join(got, ",") != "test-vm-root-20261106-100100-ab12,test-vm-root-20261106-100200-ab12" {
		t.Fatalf("candidates %v (active volume must not be listed)", got)
	}
}

// ---- OpenStackCLI allow-list ----

type recorder struct {
	argv [][]string
	out  map[string]string
}

func (r *recorder) run(ctx context.Context, argv []string) ([]byte, error) {
	r.argv = append(r.argv, argv)
	key := strings.Join(argv[7:], " ")
	for k, v := range r.out {
		if strings.HasPrefix(key, k) {
			return []byte(v + "\n"), nil
		}
	}
	return []byte("\n"), nil
}

func TestOpenStackCLIFullRestoreUsesOnlyAllowedVerbs(t *testing.T) {
	r := &recorder{out: map[string]string{
		"server show test-vm -f value -c id": "abc", "volume snapshot show": "30", "volume show": "available",
		"network show": "11111111-2222-3333-4444-555555555555",
	}}
	o := &OpenStackCLI{Run: r.run, Probe: func(context.Context) error { return nil }, PollInterval: time.Millisecond}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(o, &now)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, a := range r.argv {
		if strings.Join(a[:7], " ") != "ssh -o BatchMode=yes openstack-host openstack --os-cloud exporter-internal" {
			t.Fatalf("prefix: %v", a[:7])
		}
		all = append(all, strings.Join(a[7:], " "))
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"server delete --wait test-vm", "volume create --snapshot test-vm-clean --size 30 test-vm-root-20261106-100000-ab12",
		"server create --flavor aigate.test --volume test-vm-root-20261106-100000-ab12 --nic net-id=11111111-2222-3333-4444-555555555555,v4-fixed-ip=10.70.0.13 --key-name aigate-key --wait test-vm -f value -c status"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "volume delete") || strings.Contains(joined, "user-data") {
		t.Fatalf("forbidden verb:\n%s", joined)
	}
}

func TestOpenStackCLIRefusesEverythingElse(t *testing.T) {
	r := &recorder{}
	o := &OpenStackCLI{Run: r.run}
	bad := [][]string{
		{"volume", "delete", "test-vm-root-100000"},
		{"volume", "delete", "--purge", "x"},
		{"server", "delete", "--wait", "orch-vm"},
		{"server", "delete", "--wait", "agent-vm"},
		{"server", "delete", "test-vm"},
		{"server", "show", "agent-vm", "-f", "value", "-c", "id"},
		{"volume", "create", "--snapshot", "agent-vm-clean", "--size", "30", "test-vm-root-100000"},
		{"volume", "create", "--snapshot", "test-vm-clean", "--size", "30", "orch-vm-root-100000"},
		{"volume", "create", "--snapshot", "test-vm-clean", "--size", "30", "test-vm-root"},
		{"volume", "create", "--snapshot", "test-vm-clean", "--size", "30; id", "test-vm-root-100000"},
		{"server", "create", "--flavor", "aigate.orch", "--volume", "test-vm-root-100000", "--nic", "net-id=11111111,v4-fixed-ip=10.70.0.13", "--key-name", "aigate-key", "--wait", "test-vm", "-f", "value", "-c", "status"},
		{"server", "create", "--flavor", "aigate.test", "--volume", "test-vm-root-100000", "--nic", "net-id=11111111,v4-fixed-ip=10.70.0.12", "--key-name", "aigate-key", "--wait", "test-vm", "-f", "value", "-c", "status"},
		{"server", "create", "--flavor", "aigate.test", "--volume", "orch-vm-root", "--nic", "net-id=11111111,v4-fixed-ip=10.70.0.13", "--key-name", "aigate-key", "--wait", "test-vm", "-f", "value", "-c", "status"},
		{"server", "create", "--flavor", "aigate.test", "--volume", "test-vm-root-100000", "--nic", "net-id=11111111,v4-fixed-ip=10.70.0.13", "--key-name", "aigate-key", "--user-data", "x", "--wait", "test-vm", "-f", "value", "-c", "status"},
		{"snapshot", "delete", "test-vm-clean"},
		{"volume", "snapshot", "delete", "test-vm-clean"},
		{"volume", "revert", "test-vm-root"},
		{"volume", "set", "--size", "1", "test-vm-root"},
		{"keypair", "delete", "aigate-key"},
		{"server", "stop", "test-vm"},
		{"server", "show", "test-vm", "-f", "value", "-c", "id", "--extra"},
		{"server", "show", "$(id)", "-f", "value", "-c", "id"},
		{"volume", "show", "test-vm-root", "-f", "value", "-c", "status"},
		{},
	}
	for _, a := range bad {
		if _, err := o.run(context.Background(), a...); err == nil {
			t.Errorf("%v was run", a)
		}
	}
	if len(r.argv) != 0 {
		t.Fatalf("something was executed: %v", r.argv)
	}
	if err := o.DeleteServer(context.Background(), "orch-vm"); err == nil {
		t.Fatal("DeleteServer(orch-vm) accepted")
	}
	if err := o.CreateVolumeFromSnapshot(context.Background(), "agent-vm-clean", "test-vm-root-100000"); err == nil {
		t.Fatal("foreign snapshot accepted")
	}
}

func TestOpenStackCLIListsPruneCandidatesReadOnly(t *testing.T) {
	r := &recorder{out: map[string]string{"volume list": "id1 test-vm-root\nid2 test-vm-root-180814\nid3 agent-vm-root\nid4 test-vm-root-181005\nid5 other"}}
	o := &OpenStackCLI{Run: r.run}
	got, err := o.ListRestoreVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "test-vm-root id1,test-vm-root-180814 id2,test-vm-root-181005 id4" {
		t.Fatalf("%v", got)
	}
}

// Two restores in the same second (and the same time on another day) must not
// share a volume name; every name is accepted by the restore allowlist.
func TestRestoreVolumeNamesAreUnique(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &fakeCloud{}
	v := &TestVM{Cloud: c, Now: func() time.Time { return now },
		SSH: remote.Config{Host: "test-vm", User: "runner", KeyPath: "/k", KnownHosts: "/kh", RepoDir: "/r"}}
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		if i == 10 {
			now = now.AddDate(0, 0, 1) // the next day, same time of day
		}
	}
	for _, h := range v.History() {
		if seen[h.Volume] {
			t.Fatalf("volume name %s repeated", h.Volume)
		}
		seen[h.Volume] = true
		if !restoreRE("test-vm").MatchString(h.Volume) {
			t.Fatalf("name %q not accepted by the restore allowlist", h.Volume)
		}
	}
	if !restoreRE("test-vm").MatchString("test-vm-root-153012") {
		t.Fatal("legacy names must stay listable")
	}
}
