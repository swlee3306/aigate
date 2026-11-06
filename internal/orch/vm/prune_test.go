package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---- a stub openstack: just enough state to judge pruning ----

type sVol struct {
	id, name, status string
	atts             [][2]string // attachment id, server id
}
type sCloud struct {
	vols    map[string]*sVol
	servers map[string]string // server id -> name
	bootOf  map[string]string // server name -> volume id
	snaps   map[string]string // snapshot id -> source volume id
	seq     int
	log     []string
	fail    map[string]bool // key prefix of a command that fails
}

func newSCloud() *sCloud {
	return &sCloud{vols: map[string]*sVol{}, servers: map[string]string{}, bootOf: map[string]string{},
		snaps: map[string]string{}, fail: map[string]bool{}}
}
func (c *sCloud) uid() string { c.seq++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", c.seq) }
func (c *sCloud) addVol(name, status string) *sVol {
	v := &sVol{id: c.uid(), name: name, status: status}
	c.vols[v.id] = v
	return v
}
func (c *sCloud) serverNamed(n string) string {
	for id, name := range c.servers {
		if name == n {
			return id
		}
	}
	return ""
}
func (c *sCloud) names() string {
	var n []string
	for _, v := range c.vols {
		n = append(n, v.name)
	}
	sort.Strings(n)
	return strings.Join(n, ",")
}
func (c *sCloud) deleted(substr string) bool {
	for _, l := range c.log {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (c *sCloud) run(ctx context.Context, argv []string) ([]byte, error) {
	a := argv[1:] // Prefix is {"os"}
	cmd := strings.Join(a, " ")
	c.log = append(c.log, cmd)
	for k := range c.fail {
		if strings.HasPrefix(cmd, k) {
			return nil, errors.New("stub failure: " + k)
		}
	}
	has := func(p string) bool { return strings.HasPrefix(cmd, p) }
	switch {
	case has("server show test-vm -f value -c id"):
		if id := c.serverNamed("test-vm"); id != "" {
			return []byte(id), nil
		}
		return nil, errors.New("no server")
	case has("server show test-vm -f json -c volumes_attached"):
		b, _ := json.Marshal(map[string]any{"volumes_attached": []map[string]string{{"id": c.bootOf["test-vm"]}}})
		return b, nil
	case has("server list"):
		var ids []string
		for id := range c.servers {
			ids = append(ids, id)
		}
		return []byte(strings.Join(ids, "\n")), nil
	case has("volume snapshot list"):
		var ids []string
		for id := range c.snaps {
			ids = append(ids, id)
		}
		return []byte(strings.Join(ids, "\n")), nil
	case has("volume snapshot show test-vm-clean -f value -c size"):
		return []byte("30"), nil
	case has("volume snapshot show"):
		return []byte(c.snaps[a[3]]), nil
	case has("volume list"):
		var l []string
		for _, v := range c.vols {
			l = append(l, v.id+" "+v.name)
		}
		return []byte(strings.Join(l, "\n")), nil
	case has("volume show") && strings.Contains(cmd, "-f json"):
		v := c.vols[a[2]]
		if v == nil {
			return nil, errors.New("no volume")
		}
		var atts []map[string]string
		for _, at := range v.atts {
			atts = append(atts, map[string]string{"attachment_id": at[0], "server_id": at[1], "volume_id": v.id})
		}
		b, _ := json.Marshal(map[string]any{"name": v.name, "status": v.status, "attachments": atts})
		return b, nil
	case has("volume show"):
		for _, v := range c.vols {
			if v.name == a[2] {
				return []byte(v.status), nil
			}
		}
		return nil, errors.New("no volume")
	case has("--os-volume-api-version 3.44 volume attachment delete"):
		for _, v := range c.vols {
			for i, at := range v.atts {
				if at[0] == a[5] {
					v.atts = append(v.atts[:i], v.atts[i+1:]...)
					if len(v.atts) == 0 {
						v.status = "available"
					}
					return nil, nil
				}
			}
		}
		return nil, errors.New("no attachment")
	case has("volume delete"):
		v := c.vols[a[2]]
		if v == nil {
			return nil, errors.New("no volume")
		}
		for _, src := range c.snaps {
			if src == v.id {
				return nil, errors.New("volume has snapshots") // Cinder refuses too
			}
		}
		if len(v.atts) > 0 {
			return nil, errors.New("volume in use")
		}
		delete(c.vols, v.id)
		return nil, nil
	case has("network show"):
		return []byte("11111111-2222-3333-4444-555555555555"), nil
	case has("server delete --wait test-vm"):
		if id := c.serverNamed("test-vm"); id != "" {
			// like the homeserver: the attachment record stays behind
			if v := c.vols[c.bootOf["test-vm"]]; v != nil {
				v.atts = [][2]string{{c.uid(), id}}
				v.status = "in-use"
			}
			delete(c.servers, id)
		}
		return nil, nil
	case has("volume create --snapshot test-vm-clean"):
		c.addVol(a[6], "available")
		return nil, nil
	case has("server create"):
		id := c.uid()
		c.servers[id] = "test-vm"
		for _, v := range c.vols {
			if v.name == a[5] {
				c.bootOf["test-vm"] = v.id
				v.status = "in-use"
				v.atts = [][2]string{{c.uid(), id}}
			}
		}
		return []byte("ACTIVE"), nil
	}
	return nil, fmt.Errorf("stub: unhandled %q", cmd)
}

// scene: golden volume (snapshot origin), live server booting from cur.
func scene(t *testing.T) (*sCloud, *OpenStackCLI, *sVol, *sVol) {
	c := newSCloud()
	golden := c.addVol("test-vm-root", "available")
	cur := c.addVol("test-vm-root-20261106-100000-ab12", "in-use")
	sid := c.uid()
	c.servers[sid] = "test-vm"
	c.bootOf["test-vm"] = cur.id
	cur.atts = [][2]string{{c.uid(), sid}}
	c.snaps[c.uid()] = golden.id
	o := &OpenStackCLI{Prefix: []string{"os"}, Run: c.run, PruneRestoreVolumes: true, PollInterval: time.Millisecond}
	return c, o, golden, cur
}

func stale(c *sCloud, v *sVol) { v.atts = [][2]string{{c.uid(), c.uid()}}; v.status = "in-use" }

func TestPruneDefaultOffDoesNothing(t *testing.T) {
	c, o, _, _ := scene(t)
	c.addVol("test-vm-root-20261105-100000-aaaa", "available")
	o.PruneRestoreVolumes = false
	n := len(c.log)
	res, err := o.Prune(context.Background())
	if err != nil || len(res.Deleted) != 0 || len(c.log) != n {
		t.Fatalf("%+v %v log %v", res, err, c.log[n:])
	}
	// and TestVM with the option off never calls Prune
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	o.PruneRestoreVolumes = true
	v := newTestVM(o, &now)
	o.Probe = func(context.Context) error { return nil }
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.deleted("volume delete") {
		t.Fatal("deleted although TestVM.PruneRestoreVolumes is off")
	}
}

func TestPruneGuardsNameAndGolden(t *testing.T) {
	c, o, golden, cur := scene(t)
	old := c.addVol("test-vm-root-20261105-100000-aaaa", "available")
	legacy := c.addVol("test-vm-root-180814", "available")
	for _, n := range []string{"test-vm-root-20261105-100000-AAAA", "test-vm-root-20261105-100000", "test-vm-root-1808141", "xtest-vm-root-180814",
		"agent-vm-root-180814", "test-vm-root-20261105-100000-aaaa-x", "test-vm-root-", "other", "test-vm-root-20261105-100000-aaa"} {
		c.addVol(n, "available")
	}
	before := len(c.vols)
	res, err := o.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 2 || len(c.vols) != before-2 || c.vols[old.id] != nil || c.vols[legacy.id] != nil {
		t.Fatalf("deleted %v, left %s", res.Deleted, c.names())
	}
	if c.vols[golden.id] == nil || c.vols[cur.id] == nil {
		t.Fatal("golden or boot volume deleted")
	}
}

func TestPruneSkipsBootVolumeAndSnapshotOrigin(t *testing.T) {
	c, o, _, cur := scene(t)
	// boot volume with a prunable name (it is "cur"); an old restore volume that is the origin of a snapshot
	origin := c.addVol("test-vm-root-20261101-100000-bbbb", "available")
	c.snaps[c.uid()] = origin.id
	res, err := o.Prune(context.Background())
	if err != nil || len(res.Deleted) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if c.vols[origin.id] == nil || c.vols[cur.id] == nil {
		t.Fatal("volume deleted")
	}
	if c.deleted("volume delete") {
		t.Fatalf("delete attempted: %v", c.log)
	}
	if !strings.Contains(strings.Join(res.Skipped, "|"), "source volume of a snapshot") {
		t.Fatalf("skip reason: %v", res.Skipped)
	}
	// the boot guard also stands on its own: a candidate that equals the boot volume id is never deleted
	if !strings.Contains(c.names(), cur.name) {
		t.Fatal("boot volume gone")
	}
}

func TestPruneAttachmentToLiveServerSkips(t *testing.T) {
	c, o, _, _ := scene(t)
	live := c.addVol("test-vm-root-20261101-100000-bbbb", "in-use")
	other := c.uid()
	c.servers[other] = "someone-else"
	live.atts = [][2]string{{c.uid(), other}}
	res, err := o.Prune(context.Background())
	if err != nil || len(res.Deleted) != 0 || c.vols[live.id] == nil {
		t.Fatalf("%+v %v", res, err)
	}
	if c.deleted("attachment delete") || c.deleted("volume delete") {
		t.Fatalf("something deleted: %v", c.log)
	}
}

func TestPruneClearsStaleAttachmentThenDeletes(t *testing.T) {
	c, o, _, _ := scene(t)
	v := c.addVol("test-vm-root-20261101-100000-bbbb", "available")
	stale(c, v)
	res, err := o.Prune(context.Background())
	if err != nil || len(res.Deleted) != 1 || c.vols[v.id] != nil {
		t.Fatalf("%+v %v %s", res, err, c.names())
	}
	ia, id := -1, -1
	for i, l := range c.log {
		if strings.HasPrefix(l, "--os-volume-api-version 3.44 volume attachment delete ") {
			ia = i
		}
		if l == "volume delete "+v.id {
			id = i
		}
	}
	if ia < 0 || id < 0 || ia > id {
		t.Fatalf("attachment delete at %d, volume delete at %d: %v", ia, id, c.log)
	}
}

func TestPruneCap(t *testing.T) {
	c, o, _, _ := scene(t)
	for i := 0; i < 8; i++ {
		c.addVol(fmt.Sprintf("test-vm-root-2026110%d-100000-aaaa", i+1), "available")
	}
	o.MaxPrune = 3
	res, _ := o.Prune(context.Background())
	if len(res.Deleted) != 3 {
		t.Fatalf("deleted %d, want 3 (cap)", len(res.Deleted))
	}
	o.MaxPrune = 0 // default 5
	res, _ = o.Prune(context.Background())
	if len(res.Deleted) != 5 {
		t.Fatalf("deleted %d, want 5 (default cap)", len(res.Deleted))
	}
	if n := strings.Count(c.names(), "test-vm-root-2026"); n != 1 { // 8-3-5 = 0 old + current (2026-11-06)
		t.Fatalf("left: %s", c.names())
	}
}

func TestPruneFailuresAreWarningsAndStateUncertainDeletesNothing(t *testing.T) {
	c, o, _, _ := scene(t)
	a := c.addVol("test-vm-root-20261101-100000-bbbb", "available")
	b := c.addVol("test-vm-root-20261102-100000-bbbb", "available")
	c.fail["volume delete "+a.id] = true
	res, err := o.Prune(context.Background())
	if err != nil || len(res.Warnings) != 1 || len(res.Deleted) != 1 || c.vols[b.id] != nil || c.vols[a.id] == nil {
		t.Fatalf("%+v %v", res, err)
	}
	for _, k := range []string{"server list", "volume snapshot list", "server show test-vm -f json", "volume snapshot show 0"} {
		c2, o2, _, _ := scene(t)
		c2.addVol("test-vm-root-20261101-100000-bbbb", "available")
		c2.fail[k] = true
		n := len(c2.vols)
		res, _ := o2.Prune(context.Background())
		if len(res.Deleted) != 0 || len(c2.vols) != n || c2.deleted("volume delete") {
			t.Fatalf("%s failing: still deleted %+v", k, res)
		}
	}
}

func TestPruneFailureNeverFailsRestore(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := &prunerCloud{err: errors.New("boom"), res: PruneResult{Warnings: []string{"w1"}}}
	v := newTestVM(c, &now)
	v.PruneRestoreVolumes = true
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v.Usable() != nil {
		t.Fatal("VM unusable after prune failure")
	}
	rec, _ := v.LastRestore()
	if rec.Err != "" || len(rec.PruneWarnings) != 2 || !strings.Contains(rec.PruneWarnings[1], "boom") {
		t.Fatalf("%+v", rec)
	}
	c.panic = true
	now = now.Add(time.Hour)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatalf("panicking prune failed the restore: %v", err)
	}
	// prune does not run after a failed restore
	c.calls, c.failAt = 0, "WaitSSH"
	now = now.Add(time.Hour)
	if v.Restore(context.Background()) == nil || c.calls != 0 {
		t.Fatalf("pruned after a failed restore (%d)", c.calls)
	}
	// option on but the cloud cannot prune: a warning only
	v2 := newTestVM(&fakeCloud{}, &now)
	v2.PruneRestoreVolumes = true
	if err := v2.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec, _ := v2.LastRestore(); len(rec.PruneWarnings) != 1 {
		t.Fatalf("%+v", rec)
	}
}

type prunerCloud struct {
	fakeCloud
	err   error
	res   PruneResult
	panic bool
	calls int
}

func (p *prunerCloud) Prune(context.Context) (PruneResult, error) {
	p.calls++
	if p.panic {
		panic("oops")
	}
	return p.res, p.err
}

func TestAllowListDeleteVerbs(t *testing.T) {
	c, o, _, _ := scene(t)
	uuid := "00000000-0000-4000-8000-00000000abcd"
	cases := [][]string{
		{"volume", "delete", uuid},                                                  // valid shape but never verified
		{"volume", "delete", "test-vm-root-180814"},                                 // not a uuid
		{"volume", "delete", "--purge", uuid},                                       // extra flag
		{"volume", "delete", uuid, uuid},                                            //
		{"volume", "delete", "*"},                                                   // glob
		{"volume", "delete", strings.ToUpper(uuid)},                                 // not canonical
		{"volume", "delete", uuid + ";id"},                                          //
		{"volume", "delete", "$(id)"},                                               //
		{"volume", "attachment", "delete", uuid},                                    // missing microversion
		{"--os-volume-api-version", "3.44", "volume", "attachment", "delete", uuid}, // unverified
		{"--os-volume-api-version", "3.44", "volume", "attachment", "delete", "abc"},
		{"--os-volume-api-version", "3.0", "volume", "attachment", "delete", uuid},
		{"volume", "snapshot", "delete", uuid},
		{"server", "delete", "--wait", "agent-vm"},
		{"volume", "show", "x", "-f", "json", "-c", "name", "-c", "status", "-c", "attachments"},
		{"volume", "snapshot", "show", "test-vm-clean", "-f", "value", "-c", "volume_id"},
	}
	n := len(c.log)
	for _, a := range cases {
		if _, err := o.run(context.Background(), a...); err == nil {
			t.Errorf("%v was run", a)
		}
	}
	if len(c.log) != n {
		t.Fatalf("something was executed: %v", c.log[n:])
	}
	// verified ids may be run, others still not
	o.mark(&o.okDelete, uuid, true)
	if _, err := o.run(context.Background(), "volume", "delete", uuid); err != nil && strings.Contains(err.Error(), "refused") {
		t.Fatalf("verified id refused: %v", err)
	}
	if _, err := o.run(context.Background(), "volume", "delete", "00000000-0000-4000-8000-00000000abce"); err == nil {
		t.Fatal("unverified id ran")
	}
}

func TestTwoRestoresLeaveOneNonGoldenNonOriginVolume(t *testing.T) {
	c := newSCloud()
	golden := c.addVol("test-vm-root", "available")
	c.snaps[c.uid()] = golden.id
	o := &OpenStackCLI{Prefix: []string{"os"}, Run: c.run, PruneRestoreVolumes: true, PollInterval: time.Millisecond,
		Probe: func(context.Context) error { return nil }}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(o, &now)
	v.PruneRestoreVolumes = true
	sfx := []string{"aaaa", "bbbb", "cccc"}
	i := 0
	v.Suffix = func() string { i++; return sfx[i-1] }
	for k := 0; k < 3; k++ {
		now = now.Add(time.Hour)
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		rec, _ := v.LastRestore()
		if len(rec.PruneWarnings) != 0 {
			t.Fatalf("restore %d warnings: %v", k, rec.PruneWarnings)
		}
		var rest []string
		for _, vol := range c.vols {
			if vol.id != golden.id {
				rest = append(rest, vol.name)
			}
		}
		if len(rest) != 1 || rest[0] != rec.Volume {
			t.Fatalf("after restore %d: %v (want only %s)", k, rest, rec.Volume)
		}
	}
	if c.vols[golden.id] == nil {
		t.Fatal("golden deleted")
	}
}

func TestRestoreWithoutPruneKeepsOldVolumes(t *testing.T) {
	c := newSCloud()
	golden := c.addVol("test-vm-root", "available")
	c.snaps[c.uid()] = golden.id
	o := &OpenStackCLI{Prefix: []string{"os"}, Run: c.run, PollInterval: time.Millisecond, Probe: func(context.Context) error { return nil }}
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(o, &now)
	for k := 0; k < 3; k++ {
		now = now.Add(time.Hour)
		if err := v.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.vols) != 4 || c.deleted("volume delete") {
		t.Fatalf("%s", c.names())
	}
}

// The volume is re-read right before deletion: a volume that changed after the
// listing (renamed, attached to a live server, became a snapshot origin) stays.
func TestPruneRereadsBeforeDelete(t *testing.T) {
	for _, change := range []string{"rename", "snapshot"} {
		c, o, _, _ := scene(t)
		v := c.addVol("test-vm-root-20261101-100000-bbbb", "available")
		o.Run = func(ctx context.Context, argv []string) ([]byte, error) {
			if strings.Join(argv[1:], " ") == "volume show "+v.id+" -f json -c name -c status -c attachments" {
				defer func() { // takes effect after this first read
					if change == "rename" {
						v.name = "test-vm-root"
					} else {
						c.snaps[c.uid()] = v.id
					}
				}()
			}
			return c.run(ctx, argv)
		}
		// the first read passes; with a stale attachment there is a second read before delete
		stale(c, v)
		res, err := o.Prune(context.Background())
		if err != nil || len(res.Deleted) != 0 || c.vols[v.id] == nil || c.deleted("volume delete") {
			t.Fatalf("%s: %+v %v", change, res, err)
		}
	}
}
