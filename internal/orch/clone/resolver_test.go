package clone

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigate/internal/orch/broker"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	r, _ := filepath.EvalSymlinks(d)
	return r
}

// fakeRegistry is a stand-in broker answering `projects`.
type fakeRegistry struct {
	mu    sync.Mutex
	view  map[string]any
	calls atomic.Int32
	down  atomic.Bool
	sock  string
	ln    net.Listener
}

func startRegistry(t *testing.T, view map[string]any) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{view: view, sock: filepath.Join(shortDir(t), "b.sock")}
	f.listen(t)
	return f
}

func (f *fakeRegistry) listen(t *testing.T) {
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadBytes('\n')
				var req struct{ Op string }
				_ = json.Unmarshal(line, &req)
				f.calls.Add(1)
				f.mu.Lock()
				v := f.view
				f.mu.Unlock()
				out, _ := json.Marshal(map[string]any{"ok": true, "result": v})
				c.Write(append(out, '\n'))
			}()
		}
	}()
}

func (f *fakeRegistry) set(v map[string]any) { f.mu.Lock(); f.view = v; f.mu.Unlock() }
func (f *fakeRegistry) stop()                { f.ln.Close(); os.Remove(f.sock) }

func proj(id, path, provider, gl string) map[string]any {
	p := map[string]any{"id": id, "path": path, "kind": "registered", "ai_context": false, "auto_merge": map[string]any{"enabled": false}}
	if provider == "" {
		p["provider"] = nil
	} else {
		p["provider"] = provider
	}
	if gl == "" {
		p["remote"] = nil
	} else {
		p["remote"] = map[string]any{"path": gl, "id": 7}
	}
	return p
}

// created marks a registry row as made by this tool (origin "created": its id IS the repository name).
func created(p map[string]any) map[string]any {
	p["origin"] = "created"
	return p
}

func view(projects ...map[string]any) map[string]any {
	return map[string]any{"projects": projects, "providers": []any{
		map[string]any{"id": "gl", "kind": "gitlab", "base_url": "http://gitlab.example:18082", "ssh_url": "ssh://git@gitlab.example", "token_ref": "gl-admin"},
		map[string]any{"id": "evil", "kind": "gitlab", "base_url": "http://evil.example", "ssh_url": "ssh://git@evil.example", "token_ref": "gl-admin"},
	}, "auto_merge_global": map[string]any{"enabled": false, "max_per_day": 5}, "orphans": []any{}}
}

func newResolver(t *testing.T, f *fakeRegistry) (*Resolver, *time.Time) {
	t.Helper()
	known := filepath.Join(shortDir(t), "known_hosts")
	if err := os.WriteFile(known, []byte("gitlab.example "+keyField+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	return &Resolver{Client: &broker.Client{Socket: f.sock, Timeout: 2 * time.Second}, KnownHosts: known, Now: func() time.Time { return now }}, &now
}

func TestRemoteForValidation(t *testing.T) {
	for _, tc := range []struct {
		ssh, project, want string
	}{
		{"ssh://git@gitlab.example", "team/app-one", "ssh://git@gitlab.example/team/app-one.git"},
		{"ssh://git@gitlab.example:2222", "team/sub/app-one", "ssh://git@gitlab.example:2222/team/sub/app-one.git"},
		{"ssh://git@100.1.2.3", "t1/aa/bb/cc/dd", "ssh://git@100.1.2.3/t1/aa/bb/cc/dd.git"},
		{"ssh://git@gitlab.example", "app-one", ""},           // a bare name has no group
		{"ssh://git@gitlab.example", "aa/bb/cc/dd/ee/ff", ""}, // too deep
		{"ssh://git@gitlab.example", "team/../x", ""},         // traversal
		{"ssh://git@gitlab.example", "team/App", ""},          // upper case
		{"ssh://git@gitlab.example", "team/a b", ""},          // space
		{"ssh://git@gitlab.example", "team/x.git", ""},        // dot
		{"ssh://git@gitlab.example", "-oProxy/xx", ""},        // option-like
		{"ssh://git@gitlab.example/", "team/app-one", ""},     // path in provider address
		{"ssh://root@gitlab.example", "team/app-one", ""},     // wrong user
		{"https://gitlab.example", "team/app-one", ""},        // wrong scheme
		{"ssh://git@gitlab.example?x=1", "team/app-one", ""},  // query
		{"", "team/app-one", ""},
	} {
		got, ok := RemoteFor(tc.ssh, tc.project)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("RemoteFor(%q,%q) = %q,%v want %q", tc.ssh, tc.project, got, ok, tc.want)
		}
	}
}

func TestResolverBuildsFromRegistryOnly(t *testing.T) {
	f := startRegistry(t, view(
		proj("app-one", "/r/app-one", "gl", "team/app-one"),
		proj("legacy", "/r/legacy", "", ""),
		proj("plain", "/r/plain", "", "team/plain"), // a link without provider: not resolvable here
		created(proj("odd-name", "/r/odd-name", "gl", "team/other")),
		proj("gone-prov", "/r/gone-prov", "nope", "team/gone-prov"),
		proj("bad-path", "/r/bad-path", "gl", "team/../bad-path"),
	))
	r, _ := newResolver(t, f)
	for path, want := range map[string]struct {
		remote string
		ok     bool
	}{
		"/r/app-one":   {"ssh://git@gitlab.example/team/app-one.git", true},
		"/r/legacy":    {"", false},
		"/r/plain":     {"", false},
		"/r/odd-name":  {"", true}, // created by this tool and its name differs from the id: refused
		"/r/gone-prov": {"", true}, // unknown provider: refused
		"/r/bad-path":  {"", true},
		"/r/unknown":   {"", false},
		"":             {"", false},
	} {
		got, _, ok := r.Resolve(path)
		if got != want.remote || ok != want.ok {
			t.Errorf("Resolve(%q) = %q,%v want %q,%v", path, got, ok, want.remote, want.ok)
		}
	}
}

func TestResolverRequiresPinnedHost(t *testing.T) {
	f := startRegistry(t, view(
		proj("app-one", "/r/app-one", "gl", "team/app-one"),
		proj("evil-one", "/r/evil-one", "evil", "team/evil-one"), // host evil.example is not pinned
	))
	r, _ := newResolver(t, f)
	if got, _, ok := r.Resolve("/r/app-one"); !ok || got == "" {
		t.Fatalf("pinned host refused: %q %v", got, ok)
	}
	if got, _, ok := r.Resolve("/r/evil-one"); !ok || got != "" {
		t.Fatalf("unpinned host must be refused (empty remote, ok), got %q %v", got, ok)
	}
	// An emptied pinning file pins nothing.
	if err := os.WriteFile(r.KnownHosts, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	if got, _, ok := r.Resolve("/r/app-one"); !ok || got != "" {
		t.Fatalf("empty known_hosts: %q %v", got, ok)
	}
}

func TestResolverCacheInvalidation(t *testing.T) {
	f := startRegistry(t, view(proj("app-one", "/r/app-one", "gl", "team/app-one")))
	r, now := newResolver(t, f)
	r.Resolve("/r/app-one")
	r.Resolve("/r/app-one")
	r.Resolve("/r/other")
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d broker calls within the TTL, want 1", n)
	}
	// A project created meanwhile is unknown until the cache is dropped...
	f.set(view(proj("app-one", "/r/app-one", "gl", "team/app-one"), proj("app-two", "/r/app-two", "gl", "team/app-two")))
	if _, _, ok := r.Resolve("/r/app-two"); ok {
		t.Fatal("a new project resolved from a stale cache")
	}
	// ...and known right after Invalidate.
	r.Invalidate()
	if got, _, ok := r.Resolve("/r/app-two"); !ok || got != "ssh://git@gitlab.example/team/app-two.git" {
		t.Fatalf("after invalidate: %q %v", got, ok)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("%d calls", n)
	}
	// The TTL is 30 s.
	*now = now.Add(29 * time.Second)
	r.Resolve("/r/app-one")
	if f.calls.Load() != 2 {
		t.Fatal("refetched before the TTL ended")
	}
	*now = now.Add(2 * time.Second)
	r.Resolve("/r/app-one")
	if f.calls.Load() != 3 {
		t.Fatal("did not refetch after the TTL")
	}
}

func TestResolverFailsClosedWhenBrokerDown(t *testing.T) {
	f := startRegistry(t, view(proj("app-one", "/r/app-one", "gl", "team/app-one")))
	r, now := newResolver(t, f)
	// Cold start with the broker down: nothing is known, so nothing is
	// pushed anywhere by guessing (the default remote is not used).
	cold := &Resolver{Client: &broker.Client{Socket: filepath.Join(shortDir(t), "none.sock")}, KnownHosts: r.KnownHosts}
	if got, _, ok := cold.Resolve("/r/app-one"); !ok || got != "" {
		t.Fatalf("cold start, broker down: %q %v", got, ok)
	}
	// Warm, then the broker goes away: a repo known to be provider-linked is
	// refused once the cache is stale; one the last registry did not list is
	// left to the caller's default.
	r.Resolve("/r/app-one")
	f.stop()
	*now = now.Add(time.Minute)
	if got, _, ok := r.Resolve("/r/app-one"); !ok || got != "" {
		t.Fatalf("stale + broker down, linked repo: %q %v", got, ok)
	}
	// An unlisted repo is UNKNOWN while the table is stale (it may be the one
	// just created): refused too. Tradeoff: repos that only use the default
	// remote cannot publish until the registry can be read again.
	if got, _, ok := r.Resolve("/r/never-listed"); !ok || got != "" {
		t.Fatalf("stale + broker down, unlisted repo must be refused: %q %v", got, ok)
	}
	// The broker comes back: it resolves again, and with a FRESH table a repo
	// the registry does not list is known to be unlisted: the default applies.
	f.listen(t)
	if got, _, ok := r.Resolve("/r/app-one"); !ok || got == "" {
		t.Fatalf("after recovery: %q %v", got, ok)
	}
	if got, _, ok := r.Resolve("/r/never-listed"); ok || got != "" {
		t.Fatalf("fresh table, unlisted repo: %q %v", got, ok)
	}
	// Invalidated (a create just happened) and the broker gone again: a repo
	// absent from the stale table is unknown, not unlisted.
	r.Invalidate()
	f.stop()
	if got, _, ok := r.Resolve("/r/brand-new"); !ok || got != "" {
		t.Fatalf("invalidated + broker down, new repo: %q %v", got, ok)
	}
}

func TestResolverNilClientNeverResolves(t *testing.T) {
	var r *Resolver
	if got, _, ok := r.Resolve("/r/x"); ok || got != "" {
		t.Fatalf("%q %v", got, ok)
	}
	r.Invalidate() // must not panic
}

// A provider-linked project whose remote link is missing (new orchd with an
// old broker, or a broker-only rollback) is refuse-class, never the default
// remote; an unlisted or legacy project still uses the default.
func TestResolverProviderLinkedWithoutRemoteIsRefused(t *testing.T) {
	f := startRegistry(t, view(
		proj("no-link", "/r/no-link", "gl", ""),
		proj("legacy", "/r/legacy", "", ""),
	))
	r, _ := newResolver(t, f)
	if got, _, ok := r.Resolve("/r/no-link"); got != "" || !ok {
		t.Errorf("provider-linked without remote: %q,%v want refuse (\"\",true)", got, ok)
	}
	if got, _, ok := r.Resolve("/r/legacy"); got != "" || ok {
		t.Errorf("legacy project: %q,%v want default (\"\",false)", got, ok)
	}
	if got, _, ok := r.Resolve("/r/unlisted"); got != "" || ok {
		t.Errorf("unlisted project: %q,%v want default", got, ok)
	}
}
