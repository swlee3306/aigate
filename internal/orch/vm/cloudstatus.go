package vm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Cloud status for the web console's server status screen: Cinder quota, the
// test VM's server status and the number of restore volumes, next to the
// restore history.
//
// Cost and safety: every cloud call spawns the openstack CLI (several seconds
// over ssh), so one refresh is shared by all callers (single-flight), kept for
// cloudTTL, and bounded by a timeout. Only read verbs of OpenStackCLI's
// allow-list are used: `quota show --usage -f json`, `server show <vm> -f
// value -c status` and `volume list -f value -c ID -c Name`.
//
// Freshness policy: a value is served for at most cloudTTL (60 s) after it was
// read. A refresh that fails returns the failure, never the older good value:
// the screen must not show hours-old numbers as if they were current. A
// failure is itself remembered for only cloudFailTTL so that a broken cloud is
// not called once per request, yet recovery shows up quickly.
//
// Documented behaviours: History is empty when the cloud read failed (the
// failure view is ok/code/checked_at only). VMStatus may be "unknown" while OK
// is true: the server could not be read (for example a failed restore deleted
// it) but the quota was; consumers must render that state.
const (
	cloudTTL            = 60 * time.Second
	cloudFailTTL        = 10 * time.Second
	defaultCloudTimeout = 20 * time.Second
	cloudHistoryMax     = 10

	CloudUnavailable = "cloud_unavailable"
	CloudTimeout     = "cloud_timeout"
)

// CloudLimit is one quota line; Limit is -1 when Unlimited.
type CloudLimit struct {
	Limit, InUse, Reserved int
	Unlimited              bool
}

// CloudInfo is what CloudStatus returns. On failure only OK=false and a fixed
// Code are set (never error text: the CLI's stderr can hold host names).
type CloudInfo struct {
	OK                 bool
	Code               string // "" when OK, else CloudUnavailable or CloudTimeout
	Gigabytes, Volumes CloudLimit
	VMName, VMStatus   string
	RestoreVolumes     int             // restore volumes only; the golden volume is not counted
	History            []RestoreRecord // newest first, at most 10; local, never cached
	CheckedAt          time.Time
}

// CloudReader is what CloudStatus needs from the cloud (read-only).
// OpenStackCLI implements it.
type CloudReader interface {
	QuotaUsage(ctx context.Context) (string, error)
	ServerStatus(ctx context.Context, server string) (string, error)
	CountRestoreVolumes(ctx context.Context) (int, error)
}

type cloudFlight struct {
	done chan struct{}
	res  CloudInfo
}

func failInfo(code string) CloudInfo { return CloudInfo{Code: code} }

// CloudStatus returns the cached cloud picture, refreshing it (once for all
// concurrent callers) when it is older than the TTL. It never returns an error
// text. A caller whose ctx ends first gets cloud_timeout, and the shared
// refresh goes on for the others.
func (t *TestVM) CloudStatus(ctx context.Context) CloudInfo {
	t.cloudMu.Lock()
	if !t.cloudAt.IsZero() {
		ttl := cloudTTL
		if !t.cloudRes.OK {
			ttl = cloudFailTTL
		}
		if age := t.now().Sub(t.cloudAt); age >= 0 && age < ttl {
			res := t.cloudRes
			t.cloudMu.Unlock()
			return t.withHistory(res)
		}
	}
	f := t.cloudFlight
	if f == nil {
		f = &cloudFlight{done: make(chan struct{})}
		t.cloudFlight = f
		go t.refreshCloud(f)
	}
	t.cloudMu.Unlock()
	select {
	case <-f.done:
		return t.withHistory(f.res)
	case <-ctx.Done():
		r := failInfo(CloudTimeout)
		r.CheckedAt = t.now()
		return r
	}
}

func (t *TestVM) withHistory(r CloudInfo) CloudInfo {
	if !r.OK {
		return r
	}
	h := t.History()
	for i := len(h) - 1; i >= 0 && len(r.History) < cloudHistoryMax; i-- {
		r.History = append(r.History, h[i])
	}
	return r
}

func (t *TestVM) refreshCloud(f *cloudFlight) {
	res := failInfo(CloudUnavailable)
	defer func() {
		_ = recover() // a panic in the reader is just "unavailable"
		t.cloudMu.Lock()
		if res.CheckedAt.IsZero() {
			res.CheckedAt = t.now()
		}
		t.cloudRes, t.cloudAt, t.cloudFlight = res, res.CheckedAt, nil
		f.res = res
		t.cloudMu.Unlock()
		close(f.done)
	}()
	res = t.readCloud()
	res.CheckedAt = t.now()
}

func (t *TestVM) readCloud() CloudInfo {
	rd, ok := t.Cloud.(CloudReader)
	if !ok {
		return failInfo(CloudUnavailable)
	}
	d := t.CloudTimeout
	if d <= 0 {
		d = defaultCloudTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var quotaDoc, status string
	var vols int
	var qErr, sErr, vErr error
	var wg sync.WaitGroup
	wg.Add(3)
	// Each read recovers on its own: a panic in a reader goroutine would
	// otherwise crash the daemon (the recover in refreshCloud covers only
	// its own goroutine). A panicking read counts as a failed read.
	guard := func(err *error, f func()) {
		go func() {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					*err = errors.New("cloud read panicked")
				}
			}()
			f()
		}()
	}
	guard(&qErr, func() { quotaDoc, qErr = rd.QuotaUsage(ctx) })
	guard(&sErr, func() { status, sErr = rd.ServerStatus(ctx, t.name()) })
	guard(&vErr, func() { vols, vErr = rd.CountRestoreVolumes(ctx) })
	wg.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failInfo(CloudTimeout)
	}
	if qErr != nil || vErr != nil {
		return failInfo(CloudUnavailable)
	}
	rows, err := parseQuota(quotaDoc)
	if err != nil {
		return failInfo(CloudUnavailable)
	}
	out := CloudInfo{OK: true, VMName: t.name(), RestoreVolumes: vols, CheckedAt: t.now()}
	var haveGB, haveVol bool
	for _, r := range rows {
		l := CloudLimit{Limit: int(r.Limit), InUse: int(r.InUse), Reserved: int(r.Reserved), Unlimited: r.Limit == -1}
		switch r.Name {
		case "gigabytes":
			out.Gigabytes, haveGB = l, true
		case "volumes":
			out.Volumes, haveVol = l, true
		}
	}
	if !haveGB || !haveVol {
		return failInfo(CloudUnavailable)
	}
	// A server that cannot be read (for example deleted by a failed restore)
	// is not a cloud outage: the quota above was read.
	if sErr != nil || strings.TrimSpace(status) == "" {
		out.VMStatus = "unknown"
	} else {
		out.VMStatus = strings.TrimSpace(status)
	}
	return out
}

// QuotaUsage returns the JSON of `quota show --usage` (read-only).
func (o *OpenStackCLI) QuotaUsage(ctx context.Context) (string, error) {
	return o.run(ctx, "quota", "show", "--usage", "-f", "json")
}

// ServerStatus returns the status of the named server (read-only).
func (o *OpenStackCLI) ServerStatus(ctx context.Context, server string) (string, error) {
	return o.run(ctx, "server", "show", server, "-f", "value", "-c", "status")
}

// CountRestoreVolumes counts the restore volumes (read-only `volume list`);
// the golden volume is listed by ListRestoreVolumes but is not counted here.
func (o *OpenStackCLI) CountRestoreVolumes(ctx context.Context) (int, error) {
	vols, err := o.ListRestoreVolumes(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, v := range vols {
		if !strings.HasPrefix(v, o.vm()+"-root ") {
			n++
		}
	}
	return n, nil
}
