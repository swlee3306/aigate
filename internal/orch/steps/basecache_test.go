package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aigate/internal/gate"
)

type countExec struct {
	calls int
	out   gate.CheckOutput
	err   error
}

func (c *countExec) Prepare(context.Context, string, []string) error { return nil }
func (c *countExec) RunCheck(context.Context, gate.CheckRequest) (gate.CheckOutput, error) {
	c.calls++
	return c.out, c.err
}
func (c *countExec) Close() error { return nil }

const baseSHA = "1111111111111111111111111111111111111111"
const headSHA = "2222222222222222222222222222222222222222"

func req(sha string) gate.CheckRequest {
	return gate.CheckRequest{SHA: sha, CheckID: "unit", Argv: []string{"go", "test"}, Adapter: "exit-code", Timeout: time.Minute}
}

func newBC(t *testing.T) (*BaseCache, *time.Time) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return &BaseCache{Dir: filepath.Join(t.TempDir(), "bc"), TTL: time.Hour, Scope: "build1|vm/snap1", Now: func() time.Time { return now }}, &now
}

func okOut() gate.CheckOutput { return gate.CheckOutput{Tail: "ok", Seconds: 40} }

func TestBaseCacheHitSkipsRun(t *testing.T) {
	bc, _ := newBC(t)
	ex := &countExec{out: okOut()}
	w := bc.Wrap(ex, "repo", baseSHA)
	if _, err := w.RunCheck(context.Background(), req(baseSHA)); err != nil || ex.calls != 1 {
		t.Fatalf("first: err=%v calls=%d", err, ex.calls)
	}
	o, err := bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
	if err != nil || ex.calls != 1 || o.Tail != "ok" || o.Seconds != 0 {
		t.Fatalf("second: err=%v calls=%d out=%+v", err, ex.calls, o)
	}
}

func TestBaseCacheNeverCachesHead(t *testing.T) {
	bc, _ := newBC(t)
	ex := &countExec{out: okOut()}
	for i := 0; i < 2; i++ {
		if _, err := bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(headSHA)); err != nil {
			t.Fatal(err)
		}
	}
	if ex.calls != 2 {
		t.Fatalf("head runs were cached: calls=%d", ex.calls)
	}
	if ents, _ := os.ReadDir(bc.Dir); len(ents) != 0 {
		t.Fatalf("cache dir not empty: %v", ents)
	}
}

func TestBaseCacheKeyParts(t *testing.T) {
	bc, _ := newBC(t)
	ex := &countExec{out: okOut()}
	run := func(c *BaseCache, repo string, r gate.CheckRequest) {
		t.Helper()
		if _, err := c.Wrap(ex, repo, baseSHA).RunCheck(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	run(bc, "repo", req(baseSHA))
	other := *bc
	other.Scope = "build2|vm/snap1"
	argv := req(baseSHA)
	argv.Argv = []string{"go", "vet"}
	setup := req(baseSHA)
	setup.Setup = []string{"make"}
	to := req(baseSHA)
	to.Timeout = 2 * time.Minute
	id := req(baseSHA)
	id.CheckID = "lint"
	for name, f := range map[string]func(){
		"scope":   func() { run(&other, "repo", req(baseSHA)) },
		"repo":    func() { run(bc, "repo2", req(baseSHA)) },
		"argv":    func() { run(bc, "repo", argv) },
		"setup":   func() { run(bc, "repo", setup) },
		"timeout": func() { run(bc, "repo", to) },
		"checkid": func() { run(bc, "repo", id) },
	} {
		before := ex.calls
		f()
		if ex.calls != before+1 {
			t.Fatalf("%s change still hit the cache", name)
		}
	}
	before := ex.calls
	run(bc, "repo", req(baseSHA)) // the original key still hits
	if ex.calls != before {
		t.Fatal("original key no longer hits")
	}
}

func TestBaseCacheOnlyCleanResultsStored(t *testing.T) {
	for name, o := range map[string]gate.CheckOutput{
		"failed rc": {RC: 1}, "timeout": {TimedOut: true}, "setup failed": {SetupFailed: true, RC: 2},
	} {
		bc, _ := newBC(t)
		ex := &countExec{out: o}
		for i := 0; i < 2; i++ {
			if _, err := bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA)); err != nil {
				t.Fatal(err)
			}
		}
		if ex.calls != 2 {
			t.Fatalf("%s result was cached", name)
		}
	}
	bc, _ := newBC(t)
	ex := &countExec{err: errors.New("ssh broke")}
	for i := 0; i < 2; i++ {
		_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
	}
	if ex.calls != 2 {
		t.Fatal("an executor error was cached")
	}
}

func TestBaseCacheCancelledRunNotStored(t *testing.T) {
	bc, _ := newBC(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ex := &countExec{out: okOut()}
	_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(ctx, req(baseSHA))
	if ents, _ := os.ReadDir(bc.Dir); len(ents) != 0 {
		t.Fatal("a cancelled run was stored")
	}
}

func TestBaseCacheTTL(t *testing.T) {
	bc, now := newBC(t)
	ex := &countExec{out: okOut()}
	_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
	*now = now.Add(59 * time.Minute)
	_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
	if ex.calls != 1 {
		t.Fatal("entry expired too early")
	}
	*now = now.Add(2 * time.Minute)
	_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
	if ex.calls != 2 {
		t.Fatal("expired entry was used")
	}
}

func TestBaseCacheRejectsDamagedEntries(t *testing.T) {
	file := func(bc *BaseCache) string {
		ents, _ := os.ReadDir(bc.Dir)
		if len(ents) != 1 {
			t.Fatalf("entries: %v", ents)
		}
		return filepath.Join(bc.Dir, ents[0].Name())
	}
	for name, damage := range map[string]func(path string){
		"changed output": func(p string) {
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), `"Tail":"ok"`, `"Tail":"forged"`, 1)), 0o600)
		},
		"loose mode": func(p string) { os.Chmod(p, 0o644) },
		"garbage":    func(p string) { os.WriteFile(p, []byte("{not json"), 0o600) },
		"symlink": func(p string) {
			b, _ := os.ReadFile(p)
			real := p + ".real"
			os.WriteFile(real, b, 0o600)
			os.Remove(p)
			os.Symlink(real, p)
		},
	} {
		bc, _ := newBC(t)
		ex := &countExec{out: okOut()}
		_, _ = bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
		damage(file(bc))
		o, _ := bc.Wrap(ex, "repo", baseSHA).RunCheck(context.Background(), req(baseSHA))
		if ex.calls != 2 || o.Tail == "forged" {
			t.Fatalf("%s: damaged entry was used (calls=%d out=%+v)", name, ex.calls, o)
		}
	}
}

func TestBaseCacheNote(t *testing.T) {
	bc, _ := newBC(t)
	ex := &countExec{out: okOut()}
	w := bc.Wrap(ex, "repo", baseSHA)
	if w.note() != "" {
		t.Fatal("note without requests")
	}
	_, _ = w.RunCheck(context.Background(), req(baseSHA))
	w2 := bc.Wrap(ex, "repo", baseSHA)
	_, _ = w2.RunCheck(context.Background(), req(baseSHA))
	if w.note() != " | base cache 0/1" || w2.note() != " | base cache 1/1" {
		t.Fatalf("notes %q %q", w.note(), w2.note())
	}
}
