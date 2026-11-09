package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestWebAndServerConstantsAgree ties constants that the browser code
// (internal/orch/web/static) and the server each carry a copy of, the way
// TestKindConstantsMatchStore does for the approval kinds: the regexes that
// validate a class, a base and a VM status, the prompt byte limit, and the
// error codes the web shows fixed texts for. The JS and the vm package are
// read as source files (relative to this package).
func TestWebAndServerConstantsAgree(t *testing.T) {
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	newJS := read("../web/static/views/new.js")
	opsJS := read("../web/static/views/ops.js")
	taskJS := read("../web/static/views/task.js")
	vmGo := read("../vm/cloudstatus.go")

	// jsRegex returns the source of `const NAME = /…/;`.
	jsRegex := func(src, name string) string {
		m := regexp.MustCompile(`(?m)^const ` + name + ` = /(.*)/;\s*$`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("JS regex constant %s not found", name)
		}
		return m[1]
	}

	// Regex sources: Go RE2 and JS spell these identically, so compare text.
	if got, want := jsRegex(newJS, "CLASS_RE"), classRe.String(); got != want {
		t.Errorf("CLASS_RE %q != classRe %q", got, want)
	}
	if got, want := jsRegex(newJS, "BASE_RE"), baseRe.String(); got != want {
		t.Errorf("BASE_RE %q != baseRe %q", got, want)
	}
	m := regexp.MustCompile("statusWordRe = regexp\\.MustCompile\\(`([^`]*)`\\)").FindStringSubmatch(vmGo)
	if m == nil {
		t.Fatal("statusWordRe not found in vm/cloudstatus.go")
	}
	if m[1] != vmStatusRe.String() {
		t.Errorf("vm statusWordRe %q != api vmStatusRe %q", m[1], vmStatusRe.String())
	}

	// Prompt limit: `export const PROMPT_MAX_BYTES = 64 * 1024;`.
	pm := regexp.MustCompile(`(?m)^export const PROMPT_MAX_BYTES = (\d+) \* (\d+);`).FindStringSubmatch(newJS)
	if pm == nil {
		t.Fatal("PROMPT_MAX_BYTES not found in new.js")
	}
	a, _ := strconv.Atoi(pm[1])
	b, _ := strconv.Atoi(pm[2])
	if a*b != MaxPromptBytes {
		t.Errorf("PROMPT_MAX_BYTES %d != MaxPromptBytes %d", a*b, MaxPromptBytes)
	}

	// Error codes the JS has fixed texts for must be codes the API can return.
	objKeys := func(src, name string) []string {
		i := strings.Index(src, "const "+name+" = {")
		if i < 0 {
			t.Fatalf("object %s not found", name)
		}
		body := src[i:]
		body = body[:strings.Index(body, "\n};")]
		var keys []string
		for _, mm := range regexp.MustCompile(`(?m)^\s+([a-z_]+):`).FindAllStringSubmatch(body, -1) {
			keys = append(keys, mm[1])
		}
		if len(keys) == 0 {
			t.Fatalf("no keys in %s", name)
		}
		return keys
	}
	var codes []string
	codes = append(codes, objKeys(newJS, "SUBMIT_ERRORS")...)
	codes = append(codes, objKeys(opsJS, "CLOUD_FAIL_TEXT")...)
	ar := taskJS[strings.Index(taskJS, "export function actionResult"):]
	ar = ar[:strings.Index(ar, "\n}\n")]
	for _, mm := range regexp.MustCompile(`[(\s]code === '([a-z_]+)'`).FindAllStringSubmatch(ar, -1) {
		codes = append(codes, mm[1])
	}
	if len(codes) < 12 {
		t.Fatalf("only %d JS codes found: %v", len(codes), codes)
	}

	// The Go side: every non-test file of this package, plus the literal.
	files, _ := filepath.Glob("*.go")
	var goSrc strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		goSrc.WriteString(read(f))
	}
	for _, c := range codes {
		if !strings.Contains(goSrc.String(), `"`+c+`"`) {
			t.Errorf("web uses error code %q that no api source returns", c)
		}
	}
}
