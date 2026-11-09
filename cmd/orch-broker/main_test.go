package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRefusesMissingConfigWithFixedMessage(t *testing.T) {
	var buf bytes.Buffer
	if rc := run([]string{"-config", "/nonexistent-dir-xyz/broker.json", "-state-dir", t.TempDir()}, &buf); rc == 0 {
		t.Fatal("started without a config")
	}
	if !strings.Contains(buf.String(), "orch-broker") {
		t.Fatalf("no fixed message: %q", buf.String())
	}
}

func TestRunBadFlagIsUsageError(t *testing.T) {
	var buf bytes.Buffer
	if rc := run([]string{"-nope"}, &buf); rc != 2 {
		t.Fatalf("rc=%d", rc)
	}
	if rc := run([]string{"extra-arg"}, &buf); rc != 2 {
		t.Fatalf("positional arg rc=%d", rc)
	}
}
