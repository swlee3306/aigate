package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const example = `version: 1
protected:
  - "tests/**"
  - "go.mod"
approval_on_add:
  - "**/conftest.py"
  - "vitest.config.*"
checks:
  - id: unit
    preset: pytest
    timeout: 300s
    required: true
  - id: build
    adapter: exit-code
    command: "go build ./..."
    timeout: 120s
    required: true
`

func TestParseExample(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || len(c.Checks) != 2 {
		t.Fatalf("bad config: %+v", c)
	}
	if c.Checks[0].Preset != "pytest" || c.Checks[0].Timeout != 300*time.Second || !c.Checks[0].Required {
		t.Errorf("check0: %+v", c.Checks[0])
	}
	b := c.Checks[1]
	if b.Adapter != "exit-code" || !reflect.DeepEqual(b.Command, []string{"go", "build", "./..."}) || b.Timeout != 120*time.Second {
		t.Errorf("check1: %+v", b)
	}
	found := false
	for _, p := range c.ApprovalOnAdd {
		if p == "**/conftest.py" {
			found = true
		}
	}
	if !found {
		t.Errorf("approval_on_add: %v", c.ApprovalOnAdd)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("version: 1\nchecks:\n  - id: a\n    preset: go\n  - id: b\n    preset: go\n    required: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Checks[0].Required || c.Checks[0].Timeout != 300*time.Second {
		t.Errorf("defaults: %+v", c.Checks[0])
	}
	if c.Checks[1].Required {
		t.Errorf("explicit false lost: %+v", c.Checks[1])
	}
}

func TestCommandForms(t *testing.T) {
	cases := map[string][]string{
		`"go build ./..."`:           {"go", "build", "./..."},
		`"sh -c 'echo hi there'"`:    {"sh", "-c", "echo hi there"},
		`["sh", "-c", "echo  a  b"]`: {"sh", "-c", "echo  a  b"},
		`'echo "a b" c\ d'`:          {"echo", "a b", "c d"},
	}
	for in, want := range cases {
		y := "version: 1\nchecks:\n  - id: a\n    adapter: exit-code\n    command: " + in + "\n"
		c, err := Parse([]byte(y))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if !reflect.DeepEqual(c.Checks[0].Command, want) {
			t.Errorf("%s: got %q want %q", in, c.Checks[0].Command, want)
		}
	}
}

func TestSetup(t *testing.T) {
	c, err := Parse([]byte("version: 1\nchecks:\n  - id: a\n    preset: vitest\n    setup: npm ci\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Checks[0].Setup, []string{"npm", "ci"}) {
		t.Errorf("setup: %q", c.Checks[0].Setup)
	}
}

func TestErrors(t *testing.T) {
	chk := func(body string) string { return "version: 1\nchecks:\n" + body }
	cases := []struct{ name, yaml, want string }{
		{"no version", "checks: []\n", "version"},
		{"bad version", "version: 2\n", "version"},
		{"unknown top key", "version: 1\nbogus: 1\n", "bogus"},
		{"unknown check key", chk("  - id: a\n    preset: go\n    bogus: 1\n"), "bogus"},
		{"dup id", chk("  - id: a\n    preset: go\n  - id: a\n    preset: go\n"), "duplicate"},
		{"empty id", chk("  - preset: go\n"), "id"},
		{"bad timeout", chk("  - id: a\n    preset: go\n    timeout: soon\n"), "timeout"},
		{"zero timeout", chk("  - id: a\n    preset: go\n    timeout: 0s\n"), "timeout"},
		{"negative timeout", chk("  - id: a\n    preset: go\n    timeout: -5s\n"), "timeout"},
		{"neither", chk("  - id: a\n"), "a"},
		{"preset+command", chk("  - id: a\n    preset: go\n    command: go test\n"), "a"},
		{"preset+adapter", chk("  - id: a\n    preset: go\n    adapter: junit\n"), "a"},
		{"adapter only", chk("  - id: a\n    adapter: junit\n"), "command"},
		{"command only", chk("  - id: a\n    command: go test\n"), "adapter"},
		{"bad adapter", chk("  - id: a\n    adapter: nope\n    command: x\n"), "adapter"},
		{"bad preset", chk("  - id: a\n    preset: nope\n"), "preset"},
		{"unterminated quote", chk("  - id: a\n    adapter: exit-code\n    command: \"sh -c 'oops\"\n"), "quote"},
		{"empty command", chk("  - id: a\n    adapter: exit-code\n    command: \"  \"\n"), "command"},
		{"empty doc", "", "version"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, err, tc.want)
		}
	}
}

func TestErrorNamesCheckID(t *testing.T) {
	_, err := Parse([]byte("version: 1\nchecks:\n  - id: myid\n    preset: go\n    command: x\n"))
	if err == nil || !strings.Contains(err.Error(), "myid") {
		t.Fatalf("want error naming myid, got %v", err)
	}
}

func TestHash(t *testing.T) {
	a, _ := Parse([]byte(example))
	reordered := `# comment
checks:
  - required: true
    timeout: 300s
    preset: pytest
    id: unit
  - command:   ["go", "build", "./..."]
    adapter: exit-code
    id: build
    timeout: 2m
approval_on_add: ["**/conftest.py", "vitest.config.*"]
protected: ['tests/**', 'go.mod']
version: 1
`
	b, err := Parse([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	h := a.Hash()
	if len(h) != 16 || h != b.Hash() {
		t.Errorf("hash mismatch: %q vs %q", h, b.Hash())
	}
	variants := map[string]string{
		"protected order": strings.Replace(example, "  - \"tests/**\"\n  - \"go.mod\"", "  - \"go.mod\"\n  - \"tests/**\"", 1),
		"approval order":  strings.Replace(example, "  - \"**/conftest.py\"\n  - \"vitest.config.*\"", "  - \"vitest.config.*\"\n  - \"**/conftest.py\"", 1),
		"timeout":         strings.Replace(example, "300s", "301s", 1),
		"required":        strings.Replace(example, "required: true", "required: false", 1),
		"command":         strings.Replace(example, "go build ./...", "go build ./cmd", 1),
		"check order":     "version: 1\nprotected: [\"tests/**\", \"go.mod\"]\napproval_on_add: [\"**/conftest.py\", \"vitest.config.*\"]\nchecks:\n  - {id: build, adapter: exit-code, command: \"go build ./...\", timeout: 120s}\n  - {id: unit, preset: pytest}\n",
	}
	for name, y := range variants {
		v, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.Hash() == h {
			t.Errorf("%s: hash should differ", name)
		}
	}
}
