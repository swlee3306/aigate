package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"aigate/internal/gate"
	"aigate/internal/remote"
)

// Cloud is what Restore needs from the infrastructure. Tests use a fake;
// OpenStackCLI is the real one. Deleting old restore volumes is not part of it: that is the optional Pruner,
// used only when TestVM.PruneRestoreVolumes is set.
type Cloud interface {
	// DeleteServer removes the server (its volume is kept). A missing server is not an error.
	DeleteServer(ctx context.Context, server string) error
	// CreateVolumeFromSnapshot makes a NEW volume from the clean snapshot and waits until it is usable.
	CreateVolumeFromSnapshot(ctx context.Context, snapshot, volume string) error
	// CreateServer boots server from volume and waits until it is ACTIVE.
	CreateServer(ctx context.Context, server, volume string) error
	// WaitSSH returns once the server answers ssh; ctx bounds the wait.
	WaitSSH(ctx context.Context, server string) error
}

// RestoreRecord describes one restore attempt.
type RestoreRecord struct {
	At       time.Time
	Duration time.Duration
	Slow     bool   // Duration exceeded TestVM.SlowAfter
	Volume   string // the new boot volume
	Err      string // empty on success
	// Pruned lists old restore volumes deleted after this restore; a
	// PruneWarning is a tolerated prune problem (never a restore failure).
	// It also holds what the pre-restore prune deleted, and PruneSkipped
	// the volumes either prune left alone.
	Pruned        []string
	PruneSkipped  []string
	PruneWarnings []string
	// Phases times each restore phase in execution order (only phases that
	// ran). The failing phase has Err set; earlier phases are kept.
	Phases []Phase
}

// Phase is the timing of one restore phase: pre_prune, delete_server,
// create_volume, create_server, wait_ssh or post_prune.
type Phase struct {
	Name string
	Dur  time.Duration
	Err  string // empty on success
}

// TestVM is the throwaway check VM. After a failed restore it is unusable
// (Executor returns an error) until a restore succeeds.
type TestVM struct {
	// Name is the server name (default "test-vm"); Snapshot defaults to Name+"-clean".
	Name, Snapshot string
	Cloud          Cloud
	// SSH configures the Task 4 executor for this VM.
	SSH remote.Config
	// RestoreTimeout bounds one whole restore (default 10m); SlowAfter marks a
	// restore as slow in its record (default 3m, the plan measured ~1 min).
	RestoreTimeout, SlowAfter time.Duration
	// Now is the clock (default time.Now); tests replace it.
	Now func() time.Time
	// Suffix returns the random part of a restore volume name (default: 4 hex
	// digits from crypto/rand); tests replace it.
	Suffix func() string
	// PruneRestoreVolumes (default false) deletes the volumes of earlier
	// restores, through Cloud if it is a Pruner / PreRestorePruner, under the
	// guards in the package doc: once after DeleteServer and before the new
	// volume is created (the quota counts snapshots too, so a full quota would
	// otherwise fail the create), and once after a successful restore. A
	// failure only adds a warning to the restore record.
	PruneRestoreVolumes bool

	mu       sync.Mutex
	broken   error // set by a failed restore, cleared by a successful one
	history  []RestoreRecord
	created  []string
	active   string
	restores int
}

func (t *TestVM) name() string {
	if t.Name == "" {
		return "test-vm"
	}
	return t.Name
}

func (t *TestVM) suffix() string {
	if t.Suffix != nil {
		return t.Suffix()
	}
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(b[:])
}

func (t *TestVM) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Restore recreates the VM from its clean snapshot (see infra/snapshot.sh):
// delete the server (volume kept), create a new volume from the snapshot, boot
// the server from it, wait until ssh answers, all within RestoreTimeout. Any
// failure marks the VM unusable until a later Restore succeeds. Old restore volumes
// are deleted only when PruneRestoreVolumes is set (guarded, before the new
// volume is created and again after success);
// otherwise PruneCandidates lists it for manual deletion.
func (t *TestVM) Restore(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Cloud == nil {
		return errors.New("vm: TestVM has no Cloud")
	}
	timeout := t.RestoreTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	slow := t.SlowAfter
	if slow <= 0 {
		slow = 3 * time.Minute
	}
	snap := t.Snapshot
	if snap == "" {
		snap = t.name() + "-clean"
	}
	start := t.now()
	t.broken = errors.New("restore in progress")
	t.restores++
	// date + time + random suffix: restore volumes may stay around, and a
	// name that repeats (every 24 h, or twice in one second) would make later
	// `volume show <name>` calls ambiguous.
	volume := fmt.Sprintf("%s-root-%s-%s", t.name(), start.Format("20060102-150405"), t.suffix())
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rec RestoreRecord
	err := t.restore(rctx, snap, volume, &rec)
	rec.At, rec.Duration, rec.Volume = start, t.now().Sub(start), volume
	rec.Slow = rec.Duration > slow
	if err != nil {
		rec.Err = err.Error()
		t.broken = fmt.Errorf("vm: %s is unusable after a failed restore: %w", t.name(), err)
		t.history = append(t.history, rec)
		return fmt.Errorf("restore %s: %w", t.name(), err)
	}
	t.broken = nil
	t.active = volume
	if t.PruneRestoreVolumes {
		t.timed(&rec, "post_prune", func() error { t.prune(ctx, &rec); return nil })
	}
	t.history = append(t.history, rec)
	return nil
}

// prune runs after a successful restore, with its own bound, and records the
// outcome in rec. It cannot fail the restore.
func (t *TestVM) prune(ctx context.Context, rec *RestoreRecord) {
	p, ok := t.Cloud.(Pruner)
	if !ok {
		rec.PruneWarnings = append(rec.PruneWarnings, "prune requested but the cloud cannot prune")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			rec.PruneWarnings = append(rec.PruneWarnings, fmt.Sprintf("prune panicked: %v", r))
		}
	}()
	res, err := p.Prune(pctx)
	rec.merge(res, err, "prune")
}

func (rec *RestoreRecord) merge(res PruneResult, err error, what string) {
	rec.Pruned = append(rec.Pruned, res.Deleted...)
	rec.PruneSkipped = append(rec.PruneSkipped, res.Skipped...)
	rec.PruneWarnings = append(rec.PruneWarnings, res.Warnings...)
	if err != nil {
		rec.PruneWarnings = append(rec.PruneWarnings, what+" failed: "+err.Error())
	}
}

// prePrune runs between DeleteServer and CreateVolumeFromSnapshot, when the
// server is absent on purpose, with its own 5-minute bound. Best effort: it
// recovers panics and can never fail the restore. A cloud that cannot
// pre-prune is skipped silently (the post-restore prune warns about it).
func (t *TestVM) prePrune(ctx context.Context, rec *RestoreRecord) {
	p, ok := t.Cloud.(PreRestorePruner)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			rec.PruneWarnings = append(rec.PruneWarnings, fmt.Sprintf("pre-restore prune panicked: %v", r))
		}
	}()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := p.PrunePreRestore(pctx)
	rec.merge(res, err, "pre-restore prune")
}

// timed runs fn and appends its duration (by t.now deltas) to rec.Phases;
// a failure is recorded with Err set.
func (t *TestVM) timed(rec *RestoreRecord, name string, fn func() error) error {
	start := t.now()
	err := fn()
	ph := Phase{Name: name, Dur: t.now().Sub(start)}
	if err != nil {
		ph.Err = err.Error()
	}
	rec.Phases = append(rec.Phases, ph)
	return err
}

func (t *TestVM) restore(ctx context.Context, snap, volume string, rec *RestoreRecord) error {
	if err := t.timed(rec, "delete_server", func() error {
		if err := t.Cloud.DeleteServer(ctx, t.name()); err != nil {
			return fmt.Errorf("delete server: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if t.PruneRestoreVolumes {
		t.timed(rec, "pre_prune", func() error { t.prePrune(ctx, rec); return nil })
	}
	t.created = append(t.created, volume) // recorded first: a half-made volume is a prune candidate too
	if err := t.timed(rec, "create_volume", func() error {
		if err := t.Cloud.CreateVolumeFromSnapshot(ctx, snap, volume); err != nil {
			return fmt.Errorf("create volume: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := t.timed(rec, "create_server", func() error {
		if err := t.Cloud.CreateServer(ctx, t.name(), volume); err != nil {
			return fmt.Errorf("create server: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return t.timed(rec, "wait_ssh", func() error {
		if err := t.Cloud.WaitSSH(ctx, t.name()); err != nil {
			return fmt.Errorf("wait for ssh: %w", err)
		}
		return nil
	})
}

// Usable reports whether the VM may be used, and why not.
func (t *TestVM) Usable() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.broken
}

// Executor returns the Task 4 ssh executor for this VM, or an error while the
// VM is unusable (never a stale executor). The caller must not use an
// executor across a Restore.
func (t *TestVM) Executor() (gate.Executor, error) {
	t.mu.Lock()
	broken := t.broken
	t.mu.Unlock()
	if broken != nil {
		return nil, broken
	}
	return remote.NewSSH(t.SSH)
}

// LastRestore returns a copy of the most recent restore record.
func (t *TestVM) LastRestore() (RestoreRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.history) == 0 {
		return RestoreRecord{}, false
	}
	rec := t.history[len(t.history)-1]
	rec.Pruned = append([]string(nil), rec.Pruned...)
	rec.PruneSkipped = append([]string(nil), rec.PruneSkipped...)
	rec.PruneWarnings = append([]string(nil), rec.PruneWarnings...)
	rec.Phases = append([]Phase(nil), rec.Phases...)
	return rec, true
}

// History returns every restore record, oldest first.
func (t *TestVM) History() []RestoreRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]RestoreRecord(nil), t.history...)
}

// PruneCandidates lists the restore volumes this TestVM created that the
// server no longer boots from. They are only listed: deleting them is manual.
func (t *TestVM) PruneCandidates() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, v := range t.created {
		if v != t.active || t.broken != nil {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
