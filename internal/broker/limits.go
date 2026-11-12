package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// HourlyLimiter is a sliding-window counter: at most limit allowed calls in
// any trailing hour. Refused calls are not recorded.
type HourlyLimiter struct {
	mu    sync.Mutex
	limit int
	clock func() time.Time
	hits  []time.Time
	win   time.Duration // 0: one hour
}

// NewWindowLimiter is a limiter over a trailing window other than an hour (W3f:
// remote repository deletion has a rolling-day cap). In memory only: a broker
// restart resets it.
func NewWindowLimiter(limit int, window time.Duration, clock func() time.Time) *HourlyLimiter {
	l := NewHourlyLimiter(limit, clock)
	l.win = window
	return l
}

// NewHourlyLimiter returns a limiter; clock nil means time.Now.
func NewHourlyLimiter(limit int, clock func() time.Time) *HourlyLimiter {
	if clock == nil {
		clock = time.Now
	}
	return &HourlyLimiter{limit: limit, clock: clock}
}

func (l *HourlyLimiter) prune(now time.Time) {
	w := l.win
	if w <= 0 {
		w = time.Hour
	}
	cut := now.Add(-w)
	i := 0
	for i < len(l.hits) && !l.hits[i].After(cut) {
		i++
	}
	l.hits = l.hits[i:]
}

// Allow records a call and reports whether it fits the limit.
func (l *HourlyLimiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	l.prune(now)
	if len(l.hits) >= l.limit {
		return false
	}
	l.hits = append(l.hits, now)
	return true
}

// Remaining is how many more calls fit now (read-only).
func (l *HourlyLimiter) Remaining() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(l.clock())
	if r := l.limit - len(l.hits); r > 0 {
		return r
	}
	return 0
}

// BudgetWindow is the rolling spend window (decision R-B1: 7 days, not a
// calendar week).
const BudgetWindow = 7 * 24 * time.Hour

// ErrBudgetExceeded means the reservation does not fit the remaining budget
// (or the budget file is unusable: fail closed).
var ErrBudgetExceeded = errors.New("budget exceeded")

// ErrBadAmount is returned for a negative or overflowing amount.
var ErrBadAmount = errors.New("bad amount")

const maxBudgetFile = 4 << 20

type budgetEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	MicroUSD int64     `json:"micro_usd"`
}

type budgetFile struct {
	Version int           `json:"version"`
	Entries []budgetEntry `json:"entries"`
}

// Budget is the rolling 7-day USD budget, in micro-USD, persisted atomically.
// A call first Reserves its worst-case cost (under the lock, written to disk
// before the call), then Settles to the real cost. A crash between the two
// leaves the reservation as spend (conservative).
type Budget struct {
	mu      sync.Mutex
	path    string
	limit   int64
	clock   func() time.Time
	entries []budgetEntry
	corrupt bool
	seq     atomic.Uint64
	// OnPersistError, when set, is called (no arguments, no values) each time
	// writing the budget file fails.
	OnPersistError func()
}

// OpenBudget loads path (absent = empty). A file that cannot be parsed makes
// the budget exhausted for this run and is left untouched. An error is
// returned only when the state directory cannot be prepared.
func OpenBudget(path string, limitMicros int64, clock func() time.Time) (*Budget, error) {
	if clock == nil {
		clock = time.Now
	}
	b := &Budget{path: path, limit: limitMicros, clock: clock}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.New("예산 상태 폴더를 준비할 수 없습니다")
	}
	data, err := readBudgetFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		b.corrupt = true
	default:
		if err := b.parse(data); err != nil {
			b.corrupt = true
			b.entries = nil
		}
	}
	return b, nil
}

func readBudgetFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBudgetFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBudgetFile {
		return nil, errors.New("too large")
	}
	return data, nil
}

func (b *Budget) parse(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f budgetFile
	if err := dec.Decode(&f); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	if f.Version != 1 {
		return errors.New("version")
	}
	for _, e := range f.Entries {
		if e.MicroUSD < 0 || e.At.IsZero() {
			return errors.New("entry")
		}
	}
	b.entries = f.Entries
	return nil
}

func (b *Budget) prune(now time.Time) bool {
	cut := now.Add(-BudgetWindow)
	kept := b.entries[:0:0]
	for _, e := range b.entries {
		if e.At.After(cut) {
			kept = append(kept, e)
		}
	}
	changed := len(kept) != len(b.entries)
	b.entries = kept
	return changed
}

func (b *Budget) spent() int64 {
	var s int64
	for _, e := range b.entries {
		if s > math.MaxInt64-e.MicroUSD {
			return math.MaxInt64
		}
		s += e.MicroUSD
	}
	return s
}

// Exhausted reports a corrupt budget file (everything is refused).
func (b *Budget) Exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.corrupt
}

// RemainingMicros is the unspent part of the rolling budget (read-only; 0
// when the budget file was corrupt).
func (b *Budget) RemainingMicros() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.corrupt {
		return 0
	}
	b.prune(b.clock())
	if r := b.limit - b.spent(); r > 0 {
		return r
	}
	return 0
}

// Reserve books micros as spend before the call. It returns ErrBudgetExceeded
// without side effects when it does not fit, or when the state cannot be
// persisted (fail closed).
func (b *Budget) Reserve(micros int64) (string, error) {
	if micros < 0 {
		return "", ErrBadAmount
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.corrupt {
		return "", ErrBudgetExceeded
	}
	now := b.clock()
	pruned := b.prune(now)
	if s := b.spent(); s > b.limit || micros > b.limit-s {
		if pruned {
			b.persist() // best effort; the in-memory view is already correct
		}
		return "", ErrBudgetExceeded
	}
	id := fmt.Sprintf("%d-%d", now.UnixNano(), b.seq.Add(1))
	b.entries = append(b.entries, budgetEntry{ID: id, At: now, MicroUSD: micros})
	if err := b.persist(); err != nil {
		b.entries = b.entries[:len(b.entries)-1]
		b.persistFailed()
		return "", ErrBudgetExceeded
	}
	return id, nil
}

// Settle replaces a reservation by the real cost, which must be known and
// non-negative (0 means "the call really cost nothing"). When no usage was
// reported, or the call failed or timed out, call Keep (or SettleUsage with
// missing usage) instead: the reservation then stays as spend.
func (b *Budget) Settle(id string, actual int64) error {
	if actual < 0 {
		return ErrBadAmount
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.corrupt {
		return nil
	}
	for i := range b.entries {
		if b.entries[i].ID == id {
			b.entries[i].MicroUSD = actual
			if err := b.persist(); err != nil {
				b.persistFailed()
				return err
			}
			return nil
		}
	}
	return nil // already pruned out of the window
}

// Keep leaves the reservation as the final spend (no usage known).
func (b *Budget) Keep(id string) error { return nil }

// Usage is the token usage a response reported. A nil count means the
// response did not report it (a decoded 0 must be a non-nil pointer to 0).
type Usage struct {
	InputTokens, OutputTokens *int64
}

// SettleUsage settles to the cost of u, or keeps the reservation when either
// count is missing.
func (b *Budget) SettleUsage(id string, p Pricing, u Usage) error {
	if u.InputTokens == nil || u.OutputTokens == nil {
		return b.Keep(id)
	}
	cost, err := ActualMicros(p, *u.InputTokens, *u.OutputTokens)
	if err != nil {
		return b.Keep(id)
	}
	return b.Settle(id, cost)
}

func (b *Budget) persistFailed() {
	if b.OnPersistError != nil {
		b.OnPersistError()
	}
}

// persist writes the entries atomically (temp file, fsync, rename).
func (b *Budget) persist() error {
	data, err := json.Marshal(budgetFile{Version: 1, Entries: b.entries})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(b.path), ".budget-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, b.path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(filepath.Dir(b.path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Micros converts USD to micro-USD (rounded to nearest).
func Micros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

func ceilMicros(tokens int64, usdPerMTok float64) (int64, error) {
	if tokens < 0 || math.IsNaN(usdPerMTok) || usdPerMTok < 0 {
		return 0, ErrBadAmount
	}
	v := math.Ceil(float64(tokens) * usdPerMTok)
	if v > float64(math.MaxInt64/4) {
		return 0, ErrBadAmount
	}
	return int64(v), nil
}

func addMicros(a, b int64, err1, err2 error) (int64, error) {
	if err1 != nil || err2 != nil || a > math.MaxInt64/4 || b > math.MaxInt64/4 {
		return 0, ErrBadAmount
	}
	return a + b, nil
}

// WorstCaseMicros is the reservation made before an AI call: ceil(input
// bytes / 3) tokens at the input rate plus maxOutputTokens at the output rate.
// Negative or overflowing inputs are an error, never clamped.
func WorstCaseMicros(p Pricing, inputBytes, maxOutputTokens int64) (int64, error) {
	if inputBytes < 0 || inputBytes > math.MaxInt64-2 {
		return 0, ErrBadAmount
	}
	in, e1 := ceilMicros((inputBytes+2)/3, p.InputPerMTok)
	out, e2 := ceilMicros(maxOutputTokens, p.OutputPerMTok)
	return addMicros(in, out, e1, e2)
}

// ActualMicros is the cost of the reported usage (cached input at the input
// rate).
func ActualMicros(p Pricing, inputTokens, outputTokens int64) (int64, error) {
	in, e1 := ceilMicros(inputTokens, p.InputPerMTok)
	out, e2 := ceilMicros(outputTokens, p.OutputPerMTok)
	return addMicros(in, out, e1, e2)
}
