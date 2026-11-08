package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"aigate/internal/gate"
)

// BaseCache keeps the output of CLEAN base-commit check runs so a later task
// on the same base need not run them again. It is written only by the
// orchestrator, from outputs the gate has already bounded; a head run is never
// stored (the wrapper only caches requests for the base SHA).
//
// A hit is valid only when every part of the key matches: repository, base
// SHA, the complete check request, and Scope (build of the gate + identity of
// the test VM image). Entries expire after TTL. A missing, unreadable, stale,
// mismatching or loosely-permissioned entry is a miss, never an error.
type BaseCache struct {
	Dir   string
	TTL   time.Duration
	Scope string
	Now   func() time.Time // default time.Now
}

type cacheEntry struct {
	Key     string           `json:"key"`
	Created time.Time        `json:"created"`
	Out     gate.CheckOutput `json:"out"`
	Sum     string           `json:"sum"` // sha256 over key, created and out: catches damage, not an attacker with write access
}

func (c *BaseCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *BaseCache) key(repoID string, req gate.CheckRequest) string {
	b, _ := json.Marshal([]any{"aigate-basecache-v1", c.Scope, repoID, req.SHA, req.CheckID, req.Argv, req.Setup,
		req.Env, req.Adapter, int64(req.Timeout)})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func entrySum(e cacheEntry) string {
	b, _ := json.Marshal([]any{e.Key, e.Created.UnixNano(), e.Out})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// clean reports whether an output is worth reusing: a plain successful run.
func clean(o gate.CheckOutput) bool { return o.RC == 0 && !o.TimedOut && !o.SetupFailed }

func (c *BaseCache) get(key string) (gate.CheckOutput, bool) {
	path := filepath.Join(c.Dir, key+".json")
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return gate.CheckOutput{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return gate.CheckOutput{}, false
	}
	var e cacheEntry
	if json.Unmarshal(raw, &e) != nil || e.Key != key || e.Sum != entrySum(e) || !clean(e.Out) {
		return gate.CheckOutput{}, false
	}
	if age := c.now().Sub(e.Created); age < 0 || age > c.TTL {
		return gate.CheckOutput{}, false
	}
	return e.Out, true
}

func (c *BaseCache) put(key string, o gate.CheckOutput) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	e := cacheEntry{Key: key, Created: c.now().UTC(), Out: o}
	e.Sum = entrySum(e)
	raw, _ := json.Marshal(e)
	tmp, err := os.CreateTemp(c.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.Dir, key+".json"))
}

// cachingExecutor wraps the real executor for one gate run.
type cachingExecutor struct {
	gate.Executor
	c            *BaseCache
	repoID, base string
	hits, runs   atomic.Int64 // base requests served from the cache / total base requests
}

// Wrap returns an executor that serves base-SHA check runs from the cache.
func (c *BaseCache) Wrap(ex gate.Executor, repoID, base string) *cachingExecutor {
	return &cachingExecutor{Executor: ex, c: c, repoID: repoID, base: base}
}

func (e *cachingExecutor) RunCheck(ctx context.Context, req gate.CheckRequest) (gate.CheckOutput, error) {
	if req.SHA != e.base {
		return e.Executor.RunCheck(ctx, req)
	}
	e.runs.Add(1)
	key := e.c.key(e.repoID, req)
	if o, ok := e.c.get(key); ok {
		e.hits.Add(1)
		o.Seconds = 0 // nothing ran now; the report's base time is the time spent
		return o, nil
	}
	o, err := e.Executor.RunCheck(ctx, req)
	if err == nil && clean(o) && ctx.Err() == nil {
		_ = e.c.put(key, o) // a cache that cannot be written only costs time
	}
	return o, err
}

// note is the verify-note fragment ("" when no base check was requested).
func (e *cachingExecutor) note() string {
	if e.runs.Load() == 0 {
		return ""
	}
	return fmt.Sprintf(" | base cache %d/%d", e.hits.Load(), e.runs.Load())
}

// scopeOf builds the cache scope from the gate build and the test VM image.
func scopeOf(build string, test any) string {
	id := ""
	if i, ok := test.(interface{ Identity() string }); ok {
		id = i.Identity()
	}
	return strings.Join([]string{build, id}, "|")
}
