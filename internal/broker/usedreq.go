package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Used request IDs (decision D4): signing.Verify has no WebAuthn counter check,
// so a signed registry change is protected by 5-minute freshness plus a
// one-time request ID. IDs are kept for an hour (longer than the freshness
// window) in a 0600 file so a restart does not forget them.

const (
	usedRequestTTL = time.Hour
	maxUsedEntries = 5000
)

var (
	// ErrRequestReused: the request ID was already used.
	ErrRequestReused = errors.New("이미 사용된 요청 ID입니다")
	// ErrUsedRequestsBroken: the used-request file cannot be trusted; writes are refused.
	ErrUsedRequestsBroken = errors.New("사용된 요청 ID 목록을 사용할 수 없습니다")
	// ErrBadRequestID: not 32 lowercase hex digits.
	ErrBadRequestID = errors.New("요청 ID 형식이 올바르지 않습니다")

	errUsedSave = errors.New("사용된 요청 ID를 저장할 수 없습니다")
	errUsedFull = errors.New("사용된 요청 ID 목록이 가득 찼습니다")

	requestIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type usedFile struct {
	Version int              `json:"version"`
	Entries map[string]int64 `json:"entries"` // request id -> unix seconds
}

// UsedRequests is the one-time request ID store. Safe for concurrent use.
type UsedRequests struct {
	mu      sync.Mutex
	path    string
	opts    RegistryOptions
	entries map[string]int64
	broken  bool
}

// OpenUsedRequests opens used-requests.json in dir. A missing file is an empty
// store; a file that cannot be trusted yields a store whose Mark always fails,
// together with ErrUsedRequestsBroken (the file is left untouched).
func OpenUsedRequests(dir string, o RegistryOptions) (*UsedRequests, error) {
	u := &UsedRequests{path: filepath.Join(dir, UsedRequestsFileName), opts: o, entries: map[string]int64{}}
	if err := ensureStateDir(dir, o.ownerOK()); err != nil {
		if errors.Is(err, errStateUntrusted) {
			u.broken = true
			return u, ErrUsedRequestsBroken
		}
		return nil, err
	}
	data, exists, err := readStateFile(u.path, o.fileOwner())
	if err != nil {
		u.broken = true
		return u, ErrUsedRequestsBroken
	}
	if !exists {
		return u, nil
	}
	entries, err := parseUsedFile(data)
	if err != nil {
		u.broken = true
		return u, ErrUsedRequestsBroken
	}
	u.entries = entries
	return u, nil
}

func parseUsedFile(data []byte) (map[string]int64, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f usedFile
	if err := dec.Decode(&f); err != nil || f.Version != 1 {
		return nil, ErrUsedRequestsBroken
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrUsedRequestsBroken
	}
	out := map[string]int64{}
	for id, ts := range f.Entries {
		if !requestIDRE.MatchString(id) || ts <= 0 {
			return nil, ErrUsedRequestsBroken
		}
		out[id] = ts
	}
	return out, nil
}

// UsedRequestsStatus inspects used-requests.json without writing or creating
// anything (for orch-broker -check). A missing dir or file is fine; anything
// that OpenUsedRequests would call broken is ErrUsedRequestsBroken.
func UsedRequestsStatus(dir string, o RegistryOptions) error {
	if _, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return ErrUsedRequestsBroken
	}
	if checkStateDir(dir, o.ownerOK()) != nil {
		return ErrUsedRequestsBroken
	}
	data, exists, err := readStateFile(filepath.Join(dir, UsedRequestsFileName), o.fileOwner())
	if err != nil {
		return ErrUsedRequestsBroken
	}
	if exists {
		if _, err := parseUsedFile(data); err != nil {
			return ErrUsedRequestsBroken
		}
	}
	return nil
}

// Mark records id as used at now. A second Mark of the same id within an hour
// fails with ErrRequestReused. The entry is on disk before Mark returns nil; if
// the write fails nothing is recorded and an error is returned.
func (u *UsedRequests) Mark(id string, now time.Time) error {
	if u == nil {
		return ErrUsedRequestsBroken
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.broken {
		return ErrUsedRequestsBroken
	}
	if !requestIDRE.MatchString(id) {
		return ErrBadRequestID
	}
	cut := now.Add(-usedRequestTTL).Unix()
	next := make(map[string]int64, len(u.entries)+1)
	for k, ts := range u.entries {
		if ts > cut {
			next[k] = ts
		}
	}
	if _, used := next[id]; used {
		return ErrRequestReused
	}
	if len(next) >= maxUsedEntries {
		return errUsedFull
	}
	next[id] = now.Unix()
	b, err := json.Marshal(usedFile{Version: 1, Entries: next})
	if err != nil {
		return errUsedSave
	}
	renamed, err := writeAtomic(u.path, append(b, '\n'), u.opts.failAt, false)
	if renamed {
		// The entry is on disk (even if the directory fsync failed): keep
		// memory equal to disk so the ID cannot be used again, and fail closed.
		u.entries = next
	}
	if err != nil {
		return errUsedSave
	}
	return nil
}
