package api

import (
	"encoding/json"
	"testing"

	"aigate/internal/orch/broker"
)

// The console's project row gains `origin` additively: the broker's value for
// the four known origins, "unknown" for a broker that says nothing (older than
// registry v4), and a project with any other value is not shown at all.
func TestProjectViewCarriesOriginAdditively(t *testing.T) {
	base := broker.Project{ID: "app", Path: "/home/orch/repos/app", Kind: "registered"}
	for _, c := range []struct{ in, want string }{
		{"created", "created"}, {"linked", "linked"}, {"unknown", "unknown"}, {"legacy", "legacy"}, {"", "unknown"},
	} {
		p := base
		p.Origin = c.in
		v, ok := projectView(p)
		if !ok || v.Origin != c.want {
			t.Errorf("origin %q -> %q %v", c.in, v.Origin, ok)
		}
	}
	for _, bad := range []string{"adopted", "Created", " created", "created\n", "null"} {
		p := base
		p.Origin = bad
		if _, ok := projectView(p); ok {
			t.Errorf("origin %q accepted", bad)
		}
	}
	// the broker reply shape of registry v4 decodes, an older one (no key) still does
	var withOrigin, without broker.RegistryView
	if err := json.Unmarshal([]byte(`{"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false},"origin":"linked","remote_deleted_at":"2026-11-12T00:00:00Z"}]}`), &withOrigin); err != nil || withOrigin.Projects[0].Origin != "linked" {
		t.Fatalf("v4 reply: %v %+v", err, withOrigin)
	}
	if err := json.Unmarshal([]byte(`{"projects":[{"id":"a","path":"/x/a","kind":"k","ai_context":false,"provider":null,"remote":null,"auto_merge":{"enabled":false}}]}`), &without); err != nil || without.Projects[0].Origin != "" {
		t.Fatalf("old reply: %v %+v", err, without)
	}
	// the existing JSON keys of a project row are all still there, origin is the only new one
	v, _ := projectView(base)
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	if ledgerKeys(m) != "ai_context,auto_merge,cloned,gitlab,id,kind,origin,path,provider" {
		t.Fatalf("keys %s", ledgerKeys(m))
	}
}
