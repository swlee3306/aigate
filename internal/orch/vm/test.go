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

	quotaSkipped bool // the quota gate skipped the pre-restore prune
	preRan       bool // a pre-restore prune ran (so no post-restore prune is needed)
}

// Phase is the timing of one restore phase: prune_wait (the restore waited
// for an earlier background prune), delete_server, quota_ok (the quota had
// room, so the pre-restore prune was skipped) or quota_check (it had not, or
// could not be read) followed by pre_prune, create_volume, create_server,
// wait_ssh, and post_prune (synchronous) or post_prune_bg (added to the
// record when the background prune finishes).
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
	// guards in the package doc: after DeleteServer and before the new volume
	// is created (the quota counts snapshots too, so a full quota would
	// otherwise fail the create), or, when the QuotaGate shows room, after the
	// restore in the background. A failure only adds a warning to the record.
	PruneRestoreVolumes bool
	// OnPrune, when set, receives the result of every background prune (the
	// one that follows a restore whose pre-prune the quota gate skipped), from
	// the background goroutine, after the result was merged into the matching
	// RestoreRecord. A panic in it is recovered.
	OnPrune func(PruneResult, error)
	// PruneWait bounds how long a Restore waits for an in-flight background
	// prune before it deletes the server (default 2 minutes). After the bound
	// it goes on, with a warning phase; prunes still never overlap (pruneMu).
	PruneWait time.Duration

	pruneMu ctxMutex // at most one prune of any kind at a time
	bgMu    sync.Mutex
	bgDone  chan struct{} // closed when the latest background prune has finished and reported

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
// are deleted only when PruneRestoreVolumes is set (guarded): before the new
// volume is created, unless the quota gate shows room for this restore and the
// next, in which case after success in a background goroutine that the next
// Restore waits for. Otherwise PruneCandidates lists them for manual deletion.
func (t *TestVM) Restore(ctx context.Context) error {
	// Before the lock: the background prune needs t.mu to merge its result.
	wait := t.waitBackgroundPrune(ctx)
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
	if wait != nil {
		rec.Phases = append(rec.Phases, *wait)
		if wait.Err != "" {
			rec.PruneWarnings = append(rec.PruneWarnings, "background prune still running ("+wait.Err+"): restoring anyway")
		}
	}
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
	background := false
	if t.PruneRestoreVolumes {
		switch {
		case rec.quotaSkipped:
			// Nothing was pruned before the restore: do it now, off the critical path.
			if _, ok := t.Cloud.(Pruner); ok {
				background = true
			} else {
				rec.PruneWarnings = append(rec.PruneWarnings, "prune requested but the cloud cannot prune")
			}
		case rec.preRan:
			// The pre-restore prune just ran with the server absent; nothing new can have appeared.
		default: // a cloud that cannot pre-prune: the old post-restore prune, in line
			t.timed(&rec, "post_prune", func() error { t.prune(ctx, &rec); return nil })
		}
	}
	t.history = append(t.history, rec)
	if background {
		t.startBackgroundPrune(len(t.history)-1, volume)
	}
	return nil
}

// ctxMutex is a mutex whose waiters give up with their context.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ctxMutex) lock(ctx context.Context) error {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *ctxMutex) unlock() { <-m.ch }

// waitBackgroundPrune blocks (bounded by PruneWait and ctx) while a background
// prune from an earlier restore is in flight, so that restore does not delete
// the server under it. It returns the prune_wait phase, or nil when nothing
// was in flight.
func (t *TestVM) waitBackgroundPrune(ctx context.Context) *Phase {
	t.bgMu.Lock()
	done := t.bgDone
	t.bgMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	limit := t.PruneWait
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	start := time.Now()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ph := &Phase{Name: "prune_wait"}
	select {
	case <-done:
	case <-timer.C:
		ph.Err = "timed out after " + limit.String()
	case <-ctx.Done():
		ph.Err = ctx.Err().Error()
	}
	ph.Dur = time.Since(start)
	return ph
}

// startBackgroundPrune prunes after a restore in its own goroutine and context
// (not the task's, bounded to 5 minutes), serialized with every other prune.
// It merges the outcome into history[idx] (checked by volume name), reports it
// through OnPrune and only then closes bgDone. Called with t.mu held.
func (t *TestVM) startBackgroundPrune(idx int, volume string) {
	p := t.Cloud.(Pruner)
	done := make(chan struct{})
	t.bgMu.Lock()
	t.bgDone = done
	t.bgMu.Unlock()
	go func() {
		defer close(done)
		begin := time.Now()
		var res PruneResult
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("prune panicked: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if lerr := t.pruneMu.lock(ctx); lerr != nil {
				err = fmt.Errorf("waiting for another prune: %w", lerr)
				return
			}
			defer t.pruneMu.unlock()
			res, err = p.Prune(ctx)
		}()
		ph := Phase{Name: "post_prune_bg", Dur: time.Since(begin)}
		if err != nil {
			ph.Err = err.Error()
		}
		t.mu.Lock()
		if idx < len(t.history) && t.history[idx].Volume == volume {
			rec := &t.history[idx]
			rec.merge(res, err, "prune")
			rec.Phases = append(rec.Phases, ph)
		}
		t.mu.Unlock()
		if t.OnPrune != nil {
			func() {
				defer func() { _ = recover() }()
				t.OnPrune(res, err)
			}()
		}
	}()
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
	if err := t.pruneMu.lock(pctx); err != nil {
		rec.PruneWarnings = append(rec.PruneWarnings, "prune skipped, another prune is running: "+err.Error())
		return
	}
	defer t.pruneMu.unlock()
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
	rec.preRan = true
	defer func() {
		if r := recover(); r != nil {
			rec.PruneWarnings = append(rec.PruneWarnings, fmt.Sprintf("pre-restore prune panicked: %v", r))
		}
	}()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := t.pruneMu.lock(pctx); err != nil {
		rec.PruneWarnings = append(rec.PruneWarnings, "pre-restore prune skipped, another prune is running: "+err.Error())
		return
	}
	defer t.pruneMu.unlock()
	res, err := p.PrunePreRestore(pctx)
	rec.merge(res, err, "pre-restore prune")
}

// quotaGate asks a cloud that can (QuotaGate, and PreRestorePruner so that
// skipping means something) whether the quota already has room for this
// restore and the next. True means: skip the pre-restore prune (phase
// quota_ok, and the post-restore prune moves to the background). Anything
// else, including an error or a panic, records quota_check with the reason
// and returns false: the pre-restore prune then runs exactly as without a gate.
func (t *TestVM) quotaGate(ctx context.Context, rec *RestoreRecord) (skip bool) {
	g, ok := t.Cloud.(QuotaGate)
	if _, pre := t.Cloud.(PreRestorePruner); !ok || !pre {
		return false
	}
	start := t.now()
	var fits bool
	var detail string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				fits, err = false, fmt.Errorf("quota gate panicked: %v", r)
			}
		}()
		qctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		fits, detail, err = g.RestoreFits(qctx)
	}()
	ph := Phase{Name: "quota_ok", Dur: t.now().Sub(start)}
	switch {
	case err == nil && fits:
		rec.quotaSkipped = true
	default:
		ph.Name = "quota_check"
		if err != nil {
			ph.Err = err.Error()
		} else {
			ph.Err = detail
			if ph.Err == "" {
				ph.Err = "not enough quota headroom"
			}
		}
	}
	rec.Phases = append(rec.Phases, ph)
	return rec.quotaSkipped
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
	if t.PruneRestoreVolumes && !t.quotaGate(ctx, rec) {
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
