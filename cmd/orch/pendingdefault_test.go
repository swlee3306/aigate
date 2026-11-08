package main

import (
	"os"
	"regexp"
	"testing"

	"aigate/internal/orch/api"
)

// The CLI and orchd each compile in the pending-enrollment path; they must
// agree with each other and with the unit's explicit --pending-file (R12 style:
// fails when any of the three literals changes alone).
func TestPendingDefaultsAgree(t *testing.T) {
	if defaultPendingFile != api.DefaultPendingFile {
		t.Fatalf("cmd/orch %q != api %q", defaultPendingFile, api.DefaultPendingFile)
	}
	unit, err := os.ReadFile("../../infra/systemd/orchd-orchvm.service")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*--pending-file\s+(\S+)`).FindSubmatch(unit)
	if m == nil {
		t.Fatal("unit has no --pending-file")
	}
	if string(m[1]) != api.DefaultPendingFile {
		t.Fatalf("unit %q != api %q", m[1], api.DefaultPendingFile)
	}
}
