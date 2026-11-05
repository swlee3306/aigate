package policy

import (
	"errors"
	"testing"
)

var floorKinds = []string{
	"delete_branch", "force_push", "delete_repo", "delete_file_outside_workspace",
	"change_permissions", "modify_account", "restart_service", "deploy", "merge",
	"DELETE_BRANCH", "deleteBranch", "Force-Push", "push_force", "destroy_vm",
	"admin_grant", "create_token", "rotate_token", "set_permission", "close_account",
	"deploy_prod", "merge_request_merge", "ｄｅｌｅｔｅ_branch", "de​lete_branch", "  merge  ",
	"delete", "", "push_branch_force",
}

// The floor denies these even when an allowlist lists them explicitly.
func TestFloorAlwaysDenies(t *testing.T) {
	allow := NewAllowlist(floorKinds...)
	for _, k := range floorKinds {
		if err := Floor(k); !errors.Is(err, ErrFloor) {
			t.Errorf("Floor(%q) = %v, want ErrFloor", k, err)
		}
		if err := allow.Permit(k); !errors.Is(err, ErrFloor) {
			t.Errorf("Permit(%q) with allowlist = %v, want ErrFloor", k, err)
		}
	}
}

func TestFloorLeavesPublishKindsAndUnknownsAlone(t *testing.T) {
	for _, k := range []string{"push_branch", "draft_pr", "notify", "comment", "some_new_kind"} {
		if err := Floor(k); err != nil {
			t.Errorf("Floor(%q) = %v", k, err)
		}
	}
	// Unknown kinds are not in the floor, but not allowed by default either.
	if err := (Allowlist{}).Permit("some_new_kind"); !errors.Is(err, ErrNotAllowlisted) {
		t.Errorf("unknown kind: %v", err)
	}
	if err := NewAllowlist("push_branch").Permit("push_branch"); err != nil {
		t.Errorf("listed kind: %v", err)
	}
	if err := NewAllowlist("push_branch").Permit("draft_pr"); !errors.Is(err, ErrNotAllowlisted) {
		t.Errorf("unlisted kind: %v", err)
	}
}
