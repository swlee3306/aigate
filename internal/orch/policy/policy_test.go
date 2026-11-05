package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/ledger"
)

const good = `version: 1
default_level: L0
classes:
  docs: {level: L2}
  bugfix: {level: L1}
allowlist: [push_branch, draft_pr, notify]
limits: {agent_timeout: 20m, max_concurrent: 2, max_commits: 50, max_fetch_bytes: 1048576}
repo_roots: [/srv/repos]
`

func write(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "policy.yml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGood(t *testing.T) {
	p, err := Load(write(t, t.TempDir(), good, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if p.LevelFor("docs") != L2 || p.LevelFor("bugfix") != L1 || p.LevelFor("other") != L0 {
		t.Fatalf("levels: %+v", p.Classes)
	}
	if p.Limits.MaxConcurrent != 2 || p.Limits.AgentTimeout.Minutes() != 20 || p.Limits.MaxCommits != 50 || p.Limits.MaxFetchBytes != 1<<20 {
		t.Fatalf("limits %+v", p.Limits)
	}
	if !p.AllowList().Permit2("push_branch") || p.Hash() == "" {
		t.Fatal("allowlist or hash")
	}
	if filepath.Base(p.StatePath()) != "policy.state.json" {
		t.Fatal(p.StatePath())
	}
}

// Permit2 is a test helper.
func (a Allowlist) Permit2(k string) bool { return a.Permit(k) == nil }

func TestStrictLoader(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown top key", "version: 1\nbogus: 1\n", "bogus"},
		{"unknown class key", "version: 1\nclasses:\n  a: {level: L1, extra: 1}\n", "extra"},
		{"unknown limit key", "version: 1\nlimits: {nope: 1}\n", "nope"},
		{"duplicate class", "version: 1\nclasses:\n  a: {level: L1}\n  a: {level: L2}\n", "already defined"},
		{"undefined level", "version: 1\nclasses:\n  a: {level: L4}\n", "undefined level"},
		{"lowercase level", "version: 1\nclasses:\n  a: {level: l1}\n", "undefined level"},
		{"undefined default level", "version: 1\ndefault_level: L9\n", "undefined level"},
		{"class without level", "version: 1\nclasses:\n  a: {}\n", "level is required"},
		{"bad class name", "version: 1\nclasses:\n  'A B': {level: L1}\n", "invalid"},
		{"no version", "classes: {}\n", "version"},
		{"version 2", "version: 2\n", "version"},
		{"empty", "", "empty"},
		{"two documents", "version: 1\n---\nversion: 1\n", "more than one"},
		{"floor denied allowlist", "version: 1\nallowlist: [delete_branch]\n", "below the hard floor"},
		{"floor denied merge", "version: 1\nallowlist: [push_branch, merge]\n", "below the hard floor"},
		{"unknown kind", "version: 1\nallowlist: [launch_rocket]\n", "not a known action kind"},
		{"duplicate allowlist", "version: 1\nallowlist: [notify, notify]\n", "twice"},
		{"bad duration", "version: 1\nlimits: {agent_timeout: soon}\n", "agent_timeout"},
		{"zero duration", "version: 1\nlimits: {agent_timeout: 0s}\n", "positive"},
		{"negative duration", "version: 1\nlimits: {agent_timeout: -5m}\n", "positive"},
		{"zero concurrency", "version: 1\nlimits: {max_concurrent: 0}\n", "positive"},
		{"negative commits", "version: 1\nlimits: {max_commits: -1}\n", "positive"},
		{"zero bytes", "version: 1\nlimits: {max_fetch_bytes: 0}\n", "positive"},
		{"relative repo root", "version: 1\nrepo_roots: [repos]\n", "absolute"},
		{"unclean repo root", "version: 1\nrepo_roots: [/a/../b]\n", "clean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, t.TempDir(), c.body, 0o600))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestLooseModeRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o664, 0o666, 0o620, 0o602} {
		if _, err := Load(write(t, t.TempDir(), good, mode)); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("mode %o: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o644, 0o640, 0o400} {
		if _, err := Load(write(t, t.TempDir(), good, mode)); err != nil {
			t.Fatalf("mode %o: %v", mode, err)
		}
	}
}

func TestSymlinkAndMissingRefused(t *testing.T) {
	dir := t.TempDir()
	target := write(t, dir, good, 0o600)
	link := filepath.Join(dir, "link.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := Load(filepath.Join(dir, "none.yml")); err == nil {
		t.Fatal("missing accepted")
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	a, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	reordered := `# comment
repo_roots:   [ /srv/repos ]
limits: {max_fetch_bytes: 1048576, max_commits: 50, max_concurrent: 2, agent_timeout: 1200s}
allowlist: [notify, push_branch, draft_pr]
classes:
    bugfix:
        level: L1
    docs:
        level: L2
default_level: L0
version: 1
`
	b, err := Parse([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("hash not stable across key order/whitespace: %s vs %s", a.Hash(), b.Hash())
	}
	for name, body := range map[string]string{
		"level":     strings.Replace(good, "docs: {level: L2}", "docs: {level: L3}", 1),
		"allowlist": strings.Replace(good, "[push_branch, draft_pr, notify]", "[push_branch]", 1),
		"limit":     strings.Replace(good, "max_concurrent: 2", "max_concurrent: 3", 1),
		"default":   strings.Replace(good, "default_level: L0", "default_level: L1", 1),
		"root":      strings.Replace(good, "/srv/repos", "/srv/other", 1),
		"class":     strings.Replace(good, "bugfix:", "bugfix2:", 1),
	} {
		c, err := Parse([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Hash() == a.Hash() {
			t.Errorf("hash did not change for %s", name)
		}
	}
	// default_level defaults to L0.
	d, _ := Parse([]byte("version: 1\n"))
	e, _ := Parse([]byte("version: 1\ndefault_level: L0\nallowlist: []\n"))
	if d.Hash() != e.Hash() || d.DefaultLevel != L0 {
		t.Fatal("defaults differ")
	}
}

func TestLiveRecordsHashOncePerChange(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, good, 0o600)
	led := filepath.Join(dir, "orchd.jsonl")
	live, err := NewLive(path, led)
	if err != nil {
		t.Fatal(err)
	}
	h1 := live.Current().Hash()
	for i := 0; i < 3; i++ {
		if _, err := live.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, strings.Replace(good, "max_concurrent: 2", "max_concurrent: 3", 1), 0o600)
	p2, err := live.Reload()
	if err != nil || p2.Hash() == h1 {
		t.Fatalf("reload: %v", err)
	}
	if _, err := live.Reload(); err != nil {
		t.Fatal(err)
	}
	evs, err := ledger.Events(led)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != "policy_changed" || evs[1].Payload["new_hash"] != p2.Hash() || evs[1].Payload["old_hash"] != h1 {
		t.Fatalf("events: %+v", evs)
	}
	if rr, _ := evs[1].Payload["restart_required"].([]any); len(rr) != 1 || rr[0] != "limits" {
		t.Fatalf("restart_required: %v", evs[1].Payload)
	}
	// A broken file is an error and the last good policy stays current.
	write(t, dir, "version: 1\nbogus: 1\n", 0o600)
	if _, err := live.Reload(); err == nil {
		t.Fatal("broken policy reloaded")
	}
	if live.Current().Hash() != p2.Hash() {
		t.Fatal("current changed on a failed reload")
	}
}

func TestLiveRestartSamePolicyNotAChange(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, good, 0o600)
	led := filepath.Join(dir, "orchd.jsonl")
	for i := 0; i < 3; i++ {
		if _, err := NewLive(path, led); err != nil {
			t.Fatal(err)
		}
	}
	if evs, _ := ledger.Events(led); len(evs) != 1 {
		t.Fatalf("%d events for 3 starts", len(evs))
	}
	write(t, dir, strings.Replace(good, "L2", "L1", 1), 0o600)
	if _, err := NewLive(path, led); err != nil {
		t.Fatal(err)
	}
	if evs, _ := ledger.Events(led); len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
}
