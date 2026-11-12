package main

import (
	"reflect"
	"testing"
)

// W3e: the resolver gets the directory of the per-project GitHub keys (next to
// the shared key; no new flag) and the shared key for GitLab; the API gets the
// repo paths (never the remotes) of the static --git-remote-for entries.
func TestResolverWiringW3e(t *testing.T) {
	v := vmFlags{BrokerSocket: "/run/orch-broker/b.sock", GitRemote: "/srv/r.git", GitSSHKey: "/home/orch/orchd/ssh/id_gitlab_deploy", GitSSHKnownHosts: "/home/orch/orchd/ssh/kh"}
	r := v.remoteResolver()
	if r == nil || r.KeysDir != "/home/orch/orchd/ssh/keys" || r.SharedKey != "/home/orch/orchd/ssh/id_gitlab_deploy" {
		t.Fatalf("%+v", r)
	}
	v.GitRemoteFor = stringList{"/home/orch/repos/sample-app=ssh://git@gitlab.example/g/sample-app.git", "/srv/x=/srv/x.git", "bad", "=nothing"}
	if got := v.staticRemoteRepos(); !reflect.DeepEqual(got, []string{"/home/orch/repos/sample-app", "/srv/x"}) {
		t.Fatalf("%v", got)
	}
	if got := (&vmFlags{}).staticRemoteRepos(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
