package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PruneResult is what one Prune call did. Only Deleted volumes are gone;
// Skipped ones failed a guard (with the reason); Warnings are failures that
// were tolerated. A prune never fails a restore.
type PruneResult struct {
	Deleted  []string // "name id"
	Skipped  []string // "name id: reason"
	Warnings []string
}

// Pruner is optionally implemented by a Cloud that can delete the volumes
// earlier restores created. TestVM calls it after a successful restore when
// TestVM.PruneRestoreVolumes is set.
type Pruner interface {
	Prune(ctx context.Context) (PruneResult, error)
}

// PreRestorePruner is the optional pre-restore variant. Cinder's quota counts
// volumes AND snapshots, so with the quota full the restore fails at `create
// volume` (HTTP 413) before the post-restore prune could free anything.
// TestVM calls PrunePreRestore after DeleteServer and before
// CreateVolumeFromSnapshot, when the test VM's server is deliberately absent.
// It is best effort: it can never fail a restore.
//
// Server-absent mode (used only here) requires ALL of: `server list
// --all-projects` succeeds, contains at least one server, and contains no
// server named exactly the test VM; there is then no boot volume. Otherwise
// it returns an error and deletes nothing. A failed `server show` alone never
// enters this mode. Every other guard of Prune still applies, each re-read
// right before deletion. The old boot volume (its server is gone, only a
// stale attachment remains) becomes a candidate like any other restore volume.
type PreRestorePruner interface {
	PrunePreRestore(ctx context.Context) (PruneResult, error)
}

// QuotaGate is optionally implemented by a Cloud that can tell, with one
// read-only quota lookup, whether the next restore fits without pruning first
// (see TestVM: it then skips the pre-restore prune). It must answer true only
// when it is sure; any doubt (unreadable, unparsable, tight) is false or an
// error, and TestVM then prunes before restoring exactly as without a gate.
type QuotaGate interface {
	RestoreFits(ctx context.Context) (fits bool, detail string, err error)
}

// pruneNameRE is the driver's own naming pattern: the current
// <vm>-root-YYYYMMDD-HHMMSS-xxxx and the legacy <vm>-root-HHMMSS. Never the
// golden <vm>-root.
func pruneNameRE(vm string) *regexp.Regexp {
	q := regexp.QuoteMeta(vm)
	return regexp.MustCompile(`^` + q + `-root-([0-9]{8}-[0-9]{6}-[0-9a-f]{4}|[0-9]{6})$`)
}

type attachment struct{ ID, Server string }

type volumeInfo struct {
	Name, Status string
	Atts         []attachment
}

type pruneState struct {
	boot, servers, origins map[string]bool
}

// Prune deletes volumes that earlier restores created, under the guards in the
// package doc: exact driver name, not the boot volume, not any snapshot's
// source, no attachment to an existing server (stale ones are cleared first),
// at most MaxPrune per call, each re-read right before deletion. Individual
// failures become Warnings; an error means the cloud state could not be
// judged and nothing was deleted. It does nothing unless PruneRestoreVolumes.
func (o *OpenStackCLI) Prune(ctx context.Context) (PruneResult, error) {
	return o.prune(ctx, false)
}

// PrunePreRestore is Prune in server-absent mode, for the window right after
// the test VM's server was deleted (see PreRestorePruner). Prune's own
// behavior, including its fail-safe for a missing server, is unchanged.
func (o *OpenStackCLI) PrunePreRestore(ctx context.Context) (PruneResult, error) {
	return o.prune(ctx, true)
}

func (o *OpenStackCLI) prune(ctx context.Context, absent bool) (PruneResult, error) {
	var res PruneResult
	if !o.PruneRestoreVolumes {
		return res, nil
	}
	max := o.MaxPrune
	if max <= 0 {
		max = 5
	}
	if absent { // judge the cloud once up front: an untrusted server list is an error, not per-volume warnings
		if _, err := o.pruneState(ctx, true); err != nil {
			return res, fmt.Errorf("prune (server absent): %w", err)
		}
	}
	out, err := o.run(ctx, "volume", "list", "-f", "value", "-c", "ID", "-c", "Name")
	if err != nil {
		return res, fmt.Errorf("prune: list volumes: %w", err)
	}
	rv := pruneNameRE(o.vm())
	type cand struct{ id, name string }
	var cands []cand
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && uuidRE.MatchString(f[0]) && rv.MatchString(f[1]) {
			cands = append(cands, cand{f[0], f[1]})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })
	attempts := 0
	for _, c := range cands {
		if attempts >= max {
			res.Skipped = append(res.Skipped, c.name+" "+c.id+": per-restore cap reached")
			continue
		}
		attempts++
		done, why, err := o.pruneOne(ctx, c.id, absent)
		switch {
		case err != nil:
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: %v", c.name, c.id, err))
		case done:
			res.Deleted = append(res.Deleted, c.name+" "+c.id)
		default:
			attempts-- // a skip costs nothing
			res.Skipped = append(res.Skipped, c.name+" "+c.id+": "+why)
		}
		if ctx.Err() != nil {
			res.Warnings = append(res.Warnings, "prune stopped: "+ctx.Err().Error())
			break
		}
	}
	return res, nil
}

// pruneOne deletes one volume. It returns (deleted, skipReason, error).
func (o *OpenStackCLI) pruneOne(ctx context.Context, id string, absent bool) (bool, string, error) {
	vi, stale, skip, err := o.evaluate(ctx, id, absent)
	if err != nil || skip != "" {
		return false, skip, err
	}
	if len(stale) > 0 {
		for _, a := range stale {
			o.mark(&o.okAttach, a.ID, true)
			_, err := o.run(ctx, "--os-volume-api-version", "3.44", "volume", "attachment", "delete", a.ID)
			o.mark(&o.okAttach, a.ID, false)
			if err != nil {
				return false, "", fmt.Errorf("clear stale attachment %s: %w", a.ID, err)
			}
		}
		iv := o.PollInterval
		if iv <= 0 {
			iv = 2 * time.Second
		}
		wctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		for {
			if vi, stale, skip, err = o.evaluate(wctx, id, absent); err != nil || skip != "" {
				return false, skip, err
			}
			if len(vi.Atts) == 0 && vi.Status == "available" {
				break
			}
			select {
			case <-wctx.Done():
				return false, "", fmt.Errorf("volume still %s after clearing attachments", vi.Status)
			case <-time.After(iv):
			}
		}
	}
	// evaluate above re-read the volume and everything it is judged against
	// immediately before this point
	if len(vi.Atts) != 0 || (vi.Status != "available" && vi.Status != "error") {
		return false, fmt.Sprintf("status %s with %d attachment(s)", vi.Status, len(vi.Atts)), nil
	}
	o.mark(&o.okDelete, id, true)
	_, err = o.run(ctx, "volume", "delete", id)
	o.mark(&o.okDelete, id, false)
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (o *OpenStackCLI) mark(set *map[string]bool, id string, on bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if *set == nil {
		*set = map[string]bool{}
	}
	if on {
		(*set)[id] = true
	} else {
		delete(*set, id)
	}
}

// evaluate re-reads the cloud and judges volume id against guards 1-4 (in
// server-absent mode there is no boot volume to compare with). skip
// is non-empty when the volume must stay; stale lists attachments to servers
// that no longer exist (all attachments are stale when skip is empty).
func (o *OpenStackCLI) evaluate(ctx context.Context, id string, absent bool) (vi volumeInfo, stale []attachment, skip string, err error) {
	st, err := o.pruneState(ctx, absent)
	if err != nil {
		return vi, nil, "", err
	}
	if vi, err = o.readVolume(ctx, id); err != nil {
		return vi, nil, "", err
	}
	switch {
	case !pruneNameRE(o.vm()).MatchString(vi.Name):
		return vi, nil, "name is not a restore volume name", nil
	case st.boot[id]:
		return vi, nil, "the server boots from it", nil
	case st.origins[id]:
		return vi, nil, "it is the source volume of a snapshot", nil
	}
	for _, a := range vi.Atts {
		if a.Server == "" || !uuidRE.MatchString(a.Server) || !uuidRE.MatchString(a.ID) {
			return vi, nil, "attachment with unusable ids", nil
		}
		if st.servers[a.Server] {
			return vi, nil, "attached to existing server " + a.Server, nil
		}
		stale = append(stale, a)
	}
	return vi, stale, "", nil
}

// pruneParallel caps the read-only openstack calls pruneState keeps in flight.
const pruneParallel = 4

// pruneState reads everything the guards judge against. The independent
// read-only calls run concurrently (at most pruneParallel at a time); any
// failed or untrusted answer is an error, so nothing is ever deleted on a
// partial picture.
func (o *OpenStackCLI) pruneState(parent context.Context, absent bool) (pruneState, error) {
	st := pruneState{boot: map[string]bool{}, servers: map[string]bool{}, origins: map[string]bool{}}
	vm := o.vm()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	sem := make(chan struct{}, pruneParallel)
	var wg sync.WaitGroup
	var emu sync.Mutex // guards first and the maps in st
	var first error
	fail := func(err error) {
		emu.Lock()
		if first == nil {
			first = err
			cancel() // stop the siblings early; the first error is the one reported
		}
		emu.Unlock()
	}
	call := func(args ...string) (string, error) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-sem }()
		return o.run(ctx, args...)
	}
	// spawn runs fn in a goroutine, turning a panic into an error.
	spawn := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail(fmt.Errorf("prune state: panic: %v", r))
				}
			}()
			if err := fn(); err != nil {
				fail(err)
			}
		}()
	}
	var myID string
	if absent {
		spawn(func() error {
			out, err := call("server", "list", "--all-projects", "-f", "value", "-c", "ID", "-c", "Name")
			if err != nil {
				return err
			}
			emu.Lock()
			defer emu.Unlock()
			for _, l := range strings.Split(out, "\n") {
				f := strings.Fields(l)
				if len(f) == 0 {
					continue
				}
				if !uuidRE.MatchString(f[0]) {
					return fmt.Errorf("unexpected server list line %q: not trusting it", l)
				}
				if len(f) > 1 && strings.Join(f[1:], " ") == vm {
					return fmt.Errorf("server %s still exists: not in server-absent mode", vm)
				}
				st.servers[f[0]] = true
			}
			if len(st.servers) == 0 { // an empty list is more likely a broken read than an empty cloud
				return errors.New("server list is empty: not trusting it")
			}
			return nil
		})
	} else {
		spawn(func() error {
			out, err := call("server", "show", vm, "-f", "json", "-c", "id", "-c", "volumes_attached")
			if err != nil {
				return fmt.Errorf("cannot determine %s's boot volume: server show failed", vm)
			}
			var doc struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(out), &doc) != nil || doc.ID == "" {
				return fmt.Errorf("cannot determine %s's boot volume: server show failed", vm)
			}
			ids, err := parseVolumesAttached(out)
			if err != nil || len(ids) == 0 {
				return fmt.Errorf("cannot determine %s's boot volume: %v", vm, errOr(err, "none attached"))
			}
			emu.Lock()
			defer emu.Unlock()
			myID = doc.ID
			for _, i := range ids {
				st.boot[i] = true
			}
			return nil
		})
		spawn(func() error {
			out, err := call("server", "list", "--all-projects", "-f", "value", "-c", "ID")
			if err != nil {
				return err
			}
			emu.Lock()
			defer emu.Unlock()
			for _, l := range strings.Fields(out) {
				st.servers[l] = true
			}
			return nil
		})
	}
	spawn(func() error {
		out, err := call("volume", "snapshot", "list", "--all-projects", "-f", "value", "-c", "ID")
		if err != nil {
			return err
		}
		sids := strings.Fields(out)
		for _, sid := range sids { // validate every id before any show runs
			if !uuidRE.MatchString(sid) {
				return fmt.Errorf("unexpected snapshot id %q", sid)
			}
		}
		for _, sid := range sids {
			spawn(func() error {
				v, err := call("volume", "snapshot", "show", sid, "-f", "value", "-c", "volume_id")
				if err != nil || !uuidRE.MatchString(v) {
					return fmt.Errorf("cannot read the source volume of snapshot %s", sid)
				}
				emu.Lock()
				st.origins[v] = true
				emu.Unlock()
				return nil
			})
		}
		return nil
	})
	wg.Wait()
	if first != nil {
		return st, first
	}
	if parent.Err() != nil {
		return st, parent.Err()
	}
	if !absent && !st.servers[myID] { // the list must at least contain the server we just made
		return st, errors.New("server list does not contain the test VM: not trusting it")
	}
	return st, nil
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}

func parseVolumesAttached(s string) ([]string, error) {
	var doc struct {
		V []json.RawMessage `json:"volumes_attached"`
	}
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, err
	}
	var ids []string
	for _, raw := range doc.V {
		var o struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &o) != nil || !uuidRE.MatchString(o.ID) {
			return nil, fmt.Errorf("unexpected volumes_attached entry %s", raw)
		}
		ids = append(ids, o.ID)
	}
	return ids, nil
}

func (o *OpenStackCLI) readVolume(ctx context.Context, id string) (volumeInfo, error) {
	var vi volumeInfo
	out, err := o.run(ctx, "volume", "show", id, "-f", "json", "-c", "name", "-c", "status", "-c", "attachments")
	if err != nil {
		return vi, err
	}
	var doc struct {
		Name, Status string
		Attachments  []struct {
			AttachmentID string `json:"attachment_id"`
			ID           string `json:"id"`
			ServerID     string `json:"server_id"`
		}
	}
	// json.Unmarshal matches keys case-insensitively
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return vi, fmt.Errorf("unreadable volume %s: %w", id, err)
	}
	vi.Name, vi.Status = doc.Name, doc.Status
	for _, a := range doc.Attachments {
		aid := a.AttachmentID
		if aid == "" {
			aid = a.ID
		}
		vi.Atts = append(vi.Atts, attachment{ID: aid, Server: a.ServerID})
	}
	return vi, nil
}

// RestoreFits reports whether the quota has room for the new restore volume
// AND one more (so the next restore cannot hit HTTP 413 either): headroom
// (limit - in_use - reserved; a limit of -1 is unlimited) of `gigabytes` at
// least 2 x the clean snapshot's size and of `volumes` at least 2. Per-type
// variants (gigabytes_<type>, volumes_<type>) must pass too. Two read-only
// calls: `volume snapshot show <snap> -c size` and `quota show --usage`.
// Anything unreadable or unparsable is an error and never "fits".
func (o *OpenStackCLI) RestoreFits(ctx context.Context) (bool, string, error) {
	var sizeOut, quotaOut string
	var sizeErr, quotaErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sizeOut, sizeErr = o.run(ctx, "volume", "snapshot", "show", o.snapshot(), "-f", "value", "-c", "size")
	}()
	go func() {
		defer wg.Done()
		quotaOut, quotaErr = o.run(ctx, "quota", "show", "--usage", "-f", "json")
	}()
	wg.Wait()
	if sizeErr != nil {
		return false, "", sizeErr
	}
	if quotaErr != nil {
		return false, "", quotaErr
	}
	if !sizeRE.MatchString(sizeOut) {
		return false, "", fmt.Errorf("unreadable snapshot size %q", sizeOut)
	}
	size, _ := strconv.Atoi(sizeOut)
	if size <= 0 {
		return false, "", fmt.Errorf("unusable snapshot size %q", sizeOut)
	}
	return quotaFits(quotaOut, size)
}

// quotaRow is one gigabytes*/volumes* row of `quota show --usage -f json`.
// A Limit of -1 is unlimited.
type quotaRow struct {
	Name                   string
	Limit, InUse, Reserved int64
}

// parseQuota reads the gigabytes, gigabytes_<type>, volumes and
// volumes_<type> rows of the JSON of `quota show --usage` (other resources
// are ignored). It is the one parser shared by the prune gate (quotaFits) and
// the status view (TestVM.CloudStatus). Unreadable or implausible numbers are
// an error.
func parseQuota(doc string) ([]quotaRow, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &rows); err != nil {
		return nil, fmt.Errorf("unreadable quota: %w", err)
	}
	num := func(row map[string]json.RawMessage, key string) (int64, error) {
		for k, raw := range row {
			if strings.EqualFold(k, key) {
				var n int64
				if err := json.Unmarshal(raw, &n); err != nil {
					return 0, fmt.Errorf("quota field %q is not an integer: %s", key, raw)
				}
				return n, nil
			}
		}
		return 0, fmt.Errorf("quota row has no %q", key)
	}
	var out []quotaRow
	for _, row := range rows {
		var name string
		for k, raw := range row {
			if strings.EqualFold(k, "resource") && json.Unmarshal(raw, &name) != nil {
				return nil, errors.New("unreadable quota resource name")
			}
		}
		if name != "gigabytes" && !strings.HasPrefix(name, "gigabytes_") && name != "volumes" && !strings.HasPrefix(name, "volumes_") {
			continue
		}
		limit, err := num(row, "limit")
		if err != nil {
			return nil, err
		}
		used, err := num(row, "in use")
		if err != nil {
			return nil, err
		}
		reserved, err := num(row, "reserved")
		if err != nil {
			return nil, err
		}
		if used < 0 || reserved < 0 || limit < -1 {
			return nil, fmt.Errorf("implausible quota numbers for %s", name)
		}
		out = append(out, quotaRow{Name: name, Limit: limit, InUse: used, Reserved: reserved})
	}
	return out, nil
}

// quotaFits applies the headroom rule to the JSON of `quota show --usage`.
func quotaFits(doc string, size int) (bool, string, error) {
	rows, err := parseQuota(doc)
	if err != nil {
		return false, "", err
	}
	seenGB, seenVol := false, false
	fits, detail := true, ""
	for _, r := range rows {
		var need int64
		if r.Name == "gigabytes" || strings.HasPrefix(r.Name, "gigabytes_") {
			need, seenGB = 2*int64(size), seenGB || r.Name == "gigabytes"
		} else {
			need, seenVol = 2, seenVol || r.Name == "volumes"
		}
		if r.Limit == -1 {
			continue // unlimited
		}
		if head := r.Limit - r.InUse - r.Reserved; head < need {
			fits, detail = false, fmt.Sprintf("%s headroom %d < %d", r.Name, head, need)
		}
	}
	if !seenGB || !seenVol {
		return false, "", errors.New("quota lacks gigabytes or volumes")
	}
	return fits, detail, nil
}
