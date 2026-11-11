package api

import (
	"os"
	"path/filepath"
	"testing"
)

// Final review I1: a name whose folder already exists under a repo root is
// refused BEFORE anything is signed or sent to GitLab (the clone would later
// find the folder and the created project would be left without a checkout).
// Any kind of entry counts: directory, file, symlink (dangling or not).
func TestCreateRefusesExistingFolderBeforeSignatureOrBroker(t *testing.T) {
	kinds := map[string]func(p string){
		"directory": func(p string) { os.Mkdir(p, 0o700) },
		"file":      func(p string) { os.WriteFile(p, []byte("x"), 0o600) },
		"symlink":   func(p string) { os.Symlink("/etc", p) },
		"dangling":  func(p string) { os.Symlink("/nonexistent-target", p) },
	}
	for name, mk := range kinds {
		// at propose time
		e := newW3cEnv(t)
		s, f := e.fieldsFor("create_project")
		mk(filepath.Join(e.root, s))
		code, b := e.sockJSON(t, "POST", e.proposePath("create_project"), jsonBody(map[string]any{"action": "create_project", "subject": s, "fields": f}))
		if code != 409 || v1Code(b) != "path_exists" {
			t.Errorf("%s propose: %d %s", name, code, b)
		}
		// the folder appears between propose and apply
		e2 := newW3cEnv(t)
		p := e2.proposeAction(t, "create_project")
		mk(filepath.Join(e2.root, p.Subject))
		code, b = e2.apply(t, p)
		if code != 409 || v1Code(b) != "path_exists" {
			t.Errorf("%s apply: %d %s", name, code, b)
		}
		if ops := e2.wb.allOps(); len(ops) != 0 {
			t.Errorf("%s: the broker was contacted: %v", name, ops)
		}
		if len(e2.cloneCalls()) != 0 {
			t.Errorf("%s: cloned", name)
		}
	}
	// control: no folder, everything works
	e := newW3cEnv(t)
	if code, b := e.apply(t, e.proposeAction(t, "create_project")); code != 200 {
		t.Fatalf("control: %d %s", code, b)
	}
}
