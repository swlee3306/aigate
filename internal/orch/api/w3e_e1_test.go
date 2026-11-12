package api

import (
	"strings"
	"testing"

	"aigate/internal/orch/broker"
)

func TestE1ProjectViewRemotePathCap(t *testing.T) {
	seg := func(n int) string { return strings.Repeat("a", n) }
	mk := func(path string) broker.Project {
		return broker.Project{ID: "app-one", Path: "/r/app-one", Kind: "registered", Remote: &broker.RemoteLink{Path: path}}
	}
	ok := seg(100) + "/" + seg(100) + "/" + seg(53) // 255 bytes
	if _, good := projectView(mk(ok)); !good {
		t.Error("255-byte remote path refused")
	}
	if _, good := projectView(mk(ok + "a")); good {
		t.Error("256-byte remote path accepted")
	}
}

func TestE1TokenRefGoneSentenceIsProviderNeutral(t *testing.T) {
	if m := strings.ToLower(errTokenRefGone.Error()); strings.Contains(m, "gitlab") || strings.Contains(m, "github") {
		t.Fatalf("%q", m)
	}
}
