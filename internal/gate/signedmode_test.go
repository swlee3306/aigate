package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aigate/internal/approval"
	"aigate/internal/approval/signing"
	"aigate/internal/rules"
)

// Review point 2: in signed-only mode (a registry file exists) the gate must
// treat an unsigned approvals.json entry as "no approval".
func TestGateRunsHonorSignedMode(t *testing.T) {
	oldPath, oldOpts := signing.RegistryPath(), signing.DefaultOptions
	t.Cleanup(func() {
		signing.SetRegistryPath(oldPath)
		signing.DefaultOptions = oldOpts
	})
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(dir, "signers.json")
	signing.SetRegistryPath(reg)
	signing.DefaultOptions = signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == uint32(os.Getuid()) }}

	e, base := protectedScenario(t)
	r, err := Run(context.Background(), e.opts(base))
	if err != nil || len(r.Approvals) != 1 {
		t.Fatalf("%v %+v", err, r.Approvals)
	}
	id, _, _ := RepoID(e.repo)
	// Legacy mode (registry absent): the unsigned grant approves.
	if err := (approval.Store{Path: ApprovalsPath(e.home)}).Grant(id, base, r.Approvals[0].Hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err = Run(context.Background(), e.opts(base))
	if err != nil || !r.Approvals[0].Valid || r.Category != rules.CategoryOK {
		t.Fatalf("legacy: %v %s %+v", err, r.Category, r.Violations)
	}
	// Registry appears: the same unsigned entry no longer approves.
	if err := os.WriteFile(reg, []byte(`{"version":1,"signers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reg, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = Run(context.Background(), e.opts(base))
	if err != nil {
		t.Fatal(err)
	}
	if r.Approvals[0].Valid || r.Category != rules.CategoryWeakened || !hasRule(r.Violations, "protected_modified") {
		t.Fatalf("signed mode: ApprovalHashValid must be false: %s %+v %+v", r.Category, r.Approvals, r.Violations)
	}
}
