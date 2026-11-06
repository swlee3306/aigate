package main

import (
	"flag"
	"strings"
	"testing"
)

func TestRestoreOffWithPublishingIsRefused(t *testing.T) {
	for _, extra := range [][]string{
		{"--git-remote", "/srv/remote.git"},
		{"--publish-allow", "notify"},
		{"--gitlab-url", "https://gitlab.example", "--gitlab-project", "1", "--gitlab-token-file", "/t"},
	} {
		dir := t.TempDir()
		args := append([]string{"--db", dir + "/db/o.db", "--socket", dir + "/s"}, fullVMArgs("")...)
		args = append(append(args, "--restore-each-job=false"), extra...)
		if rc := run(args); rc != 2 {
			t.Fatalf("%v: rc %d, want 2", extra, rc)
		}
	}
	var v vmFlags
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	v.register(fs)
	if err := fs.Parse([]string{"--restore-each-job=false", "--git-remote", "/srv/r.git"}); err != nil {
		t.Fatal(err)
	}
	if err := v.checkRestoreMode(); err == nil || !strings.Contains(err.Error(), "--allow-publish-without-restore") {
		t.Fatalf("err %v", err)
	}
	v.AllowPublishWithoutRestore = true
	if err := v.checkRestoreMode(); err != nil {
		t.Fatal(err)
	}
	if out := strings.Join(v.describe("/db", 1), "\n"); !strings.Contains(out, "WARNING") {
		t.Fatalf("no warning in log:\n%s", out)
	}
	// restore off without any publishing is fine
	var w vmFlags
	fs = flag.NewFlagSet("y", flag.ContinueOnError)
	w.register(fs)
	fs.Parse([]string{"--restore-each-job=false"})
	if err := w.checkRestoreMode(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishFlagsIgnoreEnvironment(t *testing.T) {
	for k, v := range map[string]string{"ORCHD_PUBLISH_ALLOW": "push_branch", "ORCHD_GIT_REMOTE": "/srv/x.git",
		"ORCHD_GITLAB_URL": "https://g", "ORCHD_GITLAB_PROJECT": "1", "ORCHD_GITLAB_TOKEN_FILE": "/t", "ORCHD_GITLAB_TARGET": "dev",
		"ORCHD_AGENT_HOST": "envhost"} {
		t.Setenv(k, v)
	}
	var v vmFlags
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	v.register(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if v.publishConfigured() || v.GitLabTarget != "main" {
		t.Fatalf("environment widened the publish configuration: %+v", v)
	}
	if v.AgentHost != "envhost" {
		t.Fatal("non-publish flags keep their environment fallback")
	}
}

func TestStartupLogDescribesConfiguration(t *testing.T) {
	var v vmFlags
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	v.register(fs)
	fs.Parse([]string{"--agent-host", "a", "--test-host", "t", "--publish-allow", "notify", "--git-remote", "/srv/r.git", "--gitlab-token-file", "/secret/token"})
	out := strings.Join(v.describe("/var/lib/orchd/o.db", 2), "\n")
	for _, want := range []string{"restore test VM before each job: true", "publish allowlist: notify (source: --publish-allow flag)",
		"git remote (push_branch): /srv/r.git", "agent VM a, test VM t", "policy: none"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/secret/token") {
		t.Error("token file path logged")
	}
	if !strings.Contains(out, "prune old restore volumes after a restore: false (max 5 per restore)") {
		t.Errorf("prune default (off) not logged:\n%s", out)
	}
	var w vmFlags
	fs = flag.NewFlagSet("x", flag.ContinueOnError)
	w.register(fs)
	fs.Parse([]string{"--prune-restore-volumes", "--prune-max", "2"})
	if out := strings.Join(w.describe("db", 1), "\n"); !strings.Contains(out, "prune old restore volumes after a restore: true (max 2 per restore)") {
		t.Errorf("prune on not logged:\n%s", out)
	}
}
