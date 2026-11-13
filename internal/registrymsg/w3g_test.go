package registrymsg

import (
	"testing"
)

// W3g (G1): remove_project signs a third field, detach_key.

func removeProjectFieldsW3g(ack, detach bool) map[string]any {
	return map[string]any{"scope": "local", "legacy_ack": ack, "detach_key": detach}
}

func TestRemoveProjectDetachKeyFieldInSchema(t *testing.T) {
	want := map[string]fieldKind{"scope": kindString, "legacy_ack": kindBool, "detach_key": kindBool}
	got := schema[ActionRemoveProject]
	if len(got) != len(want) {
		t.Fatalf("schema %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("schema field %s: %v, want %v", k, got[k], v)
		}
	}
	if _, ok := optionalFields[ActionRemoveProject]; ok {
		t.Error("remove_project must have no optional fields (the key set is exact)")
	}
	for _, detach := range []bool{true, false} {
		if _, err := CanonicalChange(ActionRemoveProject, "my-proj", removeProjectFieldsW3g(false, detach)); err != nil {
			t.Errorf("detach_key %v refused: %v", detach, err)
		}
	}
	// required: the W3f field set (no detach_key) is no longer a valid change
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", map[string]any{"scope": "local", "legacy_ack": false}); err == nil {
		t.Error("remove_project without detach_key accepted")
	}
	// wrong type, null, extra
	for name, v := range map[string]any{"string": "true", "number": 1, "nil": nil} {
		f := removeProjectFieldsW3g(false, true)
		f["detach_key"] = v
		if _, err := CanonicalChange(ActionRemoveProject, "my-proj", f); err == nil {
			t.Errorf("detach_key as %s accepted", name)
		}
	}
	f := removeProjectFieldsW3g(false, true)
	f["deploy_key"] = "ssh-ed25519 AAAA"
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", f); err == nil {
		t.Error("the unsigned key line argument is not a signed field")
	}
	// no other action gained detach_key
	for _, a := range []string{ActionDeleteRemoteRepo, ActionRemoveDeployKey, ActionClearOrphan, ActionRegisterProject, ActionSetFlag} {
		if _, ok := schema[a]["detach_key"]; ok {
			t.Errorf("%s has detach_key", a)
		}
	}
	// ParseFields keeps a JSON boolean (not a string) for detach_key
	pf, err := ParseFields([]byte(`{"scope":"local","legacy_ack":false,"detach_key":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := pf["detach_key"].(bool); !ok || !b {
		t.Fatalf("ParseFields: %v", pf)
	}
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", pf); err != nil {
		t.Fatal(err)
	}
	pf, _ = ParseFields([]byte(`{"scope":"local","legacy_ack":false,"detach_key":"true"}`))
	if _, err := CanonicalChange(ActionRemoveProject, "my-proj", pf); err == nil {
		t.Error(`detach_key "true" (string) accepted from ParseFields`)
	}
}

// The expected canonical texts and SHA-256 values below were computed outside
// the Go code (printf | shasum -a 256) and pinned. The web consumes the same
// vectors through the Go-generated fixture (G3).
func TestChangeHashVectorsForRemoveProjectW3g(t *testing.T) {
	cases := []struct {
		subject     string
		ack, detach bool
		canonical   string
		hash        string
	}{
		{"my-app", false, false, `{"action":"remove_project","fields":{"detach_key":false,"legacy_ack":false,"scope":"local"},"subject":"my-app"}`, "054ff8b93db03587af50da175cf6cdd922855bda5813dc13f895ef0c5d073561"},
		{"my-app", false, true, `{"action":"remove_project","fields":{"detach_key":true,"legacy_ack":false,"scope":"local"},"subject":"my-app"}`, "964c185dd06c8b8dc43a6126642f20e5459590c44e8f5cb52e616e97af8c2439"},
		{"toy", true, false, `{"action":"remove_project","fields":{"detach_key":false,"legacy_ack":true,"scope":"local"},"subject":"toy"}`, "129db994af0d1291e156e183ffa139f86e2d519d91335ff9a51a6d0cfb7cc106"},
		{"toy", true, true, `{"action":"remove_project","fields":{"detach_key":true,"legacy_ack":true,"scope":"local"},"subject":"toy"}`, "148027ff279a7cdc2345c413a3e1eb1d418378676f148a13a005c7ed9f7380fa"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		f := removeProjectFieldsW3g(c.ack, c.detach)
		got, err := CanonicalChange(ActionRemoveProject, c.subject, f)
		if err != nil || string(got) != c.canonical {
			t.Errorf("canonical %s: %s %v", c.subject, got, err)
		}
		h, err := ChangeHash(ActionRemoveProject, c.subject, f)
		if err != nil || h != c.hash {
			t.Errorf("hash %s ack=%v detach=%v: %s %v", c.subject, c.ack, c.detach, h, err)
		}
		if seen[h] {
			t.Errorf("hash collision across detach_key values: %s", h)
		}
		seen[h] = true
	}
	// detach_key changes the hash (it is inside the signed change)
	a, _ := ChangeHash(ActionRemoveProject, "my-app", removeProjectFieldsW3g(false, false))
	b, _ := ChangeHash(ActionRemoveProject, "my-app", removeProjectFieldsW3g(false, true))
	if a == b {
		t.Error("detach_key is not covered by the change hash")
	}
}
