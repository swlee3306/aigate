package api

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"aigate/internal/registrymsg"
)

// checkW3fConstants is the "w3f" subtest of TestConstantsAgreeWithWeb: what the console copies for the
// four deletion actions is pinned to Go. A missing constant or file is a failure, never a skip.
func checkW3fConstants(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s must exist and be readable: %v", p, err)
		}
		return string(b)
	}
	prov := read("../web/static/views/providers.js")
	del := read("../web/static/views/delete.js")
	one := func(js, pat, what string) []string {
		m := regexp.MustCompile(pat).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("%s not found", what)
		}
		return m
	}
	jsString := func(name string) string {
		return one(prov, `(?m)^(?:export )?const `+name+` = '([^']*)';\s*$`, "JS string constant "+name)[1]
	}
	jsInt := func(name string) int {
		n, _ := strconv.Atoi(one(prov, `(?m)^(?:export )?const `+name+` = ([0-9_]+);\s*$`, "JS integer constant "+name)[1])
		return n
	}
	// action names
	for name, want := range map[string]string{"ACTION_REMOVE_PROJECT": registrymsg.ActionRemoveProject, "ACTION_DELETE_REMOTE_REPO": registrymsg.ActionDeleteRemoteRepo,
		"ACTION_REMOVE_DEPLOY_KEY": registrymsg.ActionRemoveDeployKey, "ACTION_CLEAR_ORPHAN": registrymsg.ActionClearOrphan} {
		if got := jsString(name); got != want {
			t.Errorf("%s %q != %q", name, got, want)
		}
	}
	// remote_id rule: the regexp and the behaviour
	if got, want := one(prov, `(?m)^export const REMOTE_ID_RE = /(.*)/;\s*$`, "REMOTE_ID_RE")[1], `^[1-9][0-9]{0,14}$`; got != want {
		t.Errorf("REMOTE_ID_RE %q != %q", got, want)
	}
	re := regexp.MustCompile(`^[1-9][0-9]{0,14}$`)
	for _, v := range []string{"1", "42", "123456789012345", "1234567890123456", "0", "00", "01", "-1", "+1", "", "abc", " 1"} {
		if re.MatchString(v) != registrymsg.ValidRemoteID(v) {
			t.Errorf("REMOTE_ID_RE and ValidRemoteID disagree on %q", v)
		}
	}
	// the browser's SCHEMA of the four actions is the registrymsg one: exactly these fields and types
	want := map[string]map[string]string{
		registrymsg.ActionRemoveProject:    {"scope": "s", "legacy_ack": "b", "detach_key": "b"},
		registrymsg.ActionDeleteRemoteRepo: {"remote_path": "s", "remote_id": "s", "confirm_name": "s", "also_local": "b"},
		registrymsg.ActionRemoveDeployKey:  {"remote_path": "s"},
		registrymsg.ActionClearOrphan:      {"project": "s"},
	}
	sample := map[string]any{"scope": "local", "legacy_ack": false, "detach_key": false, "remote_path": "octo/hello", "remote_id": "5", "confirm_name": "hello", "also_local": true, "project": "octo/half"}
	for action, goFields := range want {
		m := one(prov, `(?m)^\s*\[ACTION_`+strings.ToUpper(action)+`\]: \{([^}]*)\},?\s*$`, "SCHEMA entry of "+action)
		jsFields := map[string]string{}
		for _, part := range strings.Split(m[1], ",") {
			name, typ, ok := strings.Cut(strings.TrimSpace(part), ": ")
			if !ok {
				t.Fatalf("SCHEMA %s: bad entry %q", action, part)
			}
			jsFields[name] = strings.Trim(typ, "'")
		}
		if len(jsFields) != len(goFields) {
			t.Errorf("SCHEMA %s fields %v != %v", action, jsFields, goFields)
		}
		full := map[string]any{}
		for name, typ := range goFields {
			if jsFields[name] != typ {
				t.Errorf("SCHEMA %s field %s: %q != %q", action, name, jsFields[name], typ)
			}
			full[name] = sample[name]
		}
		subject := "my-app"
		if action == registrymsg.ActionClearOrphan {
			subject = "gh"
		}
		if _, err := registrymsg.CanonicalChange(action, subject, full); err != nil {
			t.Errorf("registrymsg refuses the browser's %s fields: %v", action, err)
		}
		for name := range full {
			less := map[string]any{}
			for k, v := range full {
				if k != name {
					less[k] = v
				}
			}
			if _, err := registrymsg.CanonicalChange(action, subject, less); err == nil {
				t.Errorf("registrymsg accepts %s without %s", action, name)
			}
		}
	}
	// the delete endpoints' paths are the routes of v1.go
	v1 := read("v1.go")
	for _, p := range []string{"/projects/remove/", "/projects/delete-remote/", "/projects/remove-key/", "/orphans/clear/"} {
		if !strings.Contains(v1, `"`+"POST \"+v1Prefix+\""+p+`propose"`) && !strings.Contains(v1, p+"propose") {
			t.Errorf("v1.go has no route %s", p)
		}
		if !strings.Contains(prov, "/api/v1"+strings.TrimSuffix(p, "/")+"'") {
			t.Errorf("providers.js has no path /api/v1%s", strings.TrimSuffix(p, "/"))
		}
	}
	if !strings.Contains(v1, "/projects/{id}/removal-preview") || !strings.Contains(read("../web/static/api.js"), `removal-preview$/`) {
		t.Error("the removal preview route and api.js's CSRF opt-in path must agree")
	}
	// timeouts: the apply calls outwait orchd (broker op up to 70 s plus the local removal), the preview its 25 s budget
	if got := jsInt("DELETE_APPLY_TIMEOUT_MS"); got < 130_000 {
		t.Errorf("DELETE_APPLY_TIMEOUT_MS %d must be at least 130000", got)
	}
	if got := jsInt("REMOVAL_PREVIEW_TIMEOUT_MS"); got <= int(previewBudget.Milliseconds()) {
		t.Errorf("REMOVAL_PREVIEW_TIMEOUT_MS %d must exceed the server's preview budget %v", got, previewBudget)
	}
	if got := jsInt("DELETE_PROPOSE_TIMEOUT_MS"); got < 15_000 {
		t.Errorf("DELETE_PROPOSE_TIMEOUT_MS %d must exceed the broker read limit of 15 s", got)
	}
	// the fixed deletion lines: each mark is in the server's real preview exactly when the page expects it
	marks := map[string]string{
		"PREVIEW_DEL_LEGACY_MARK": jsString("PREVIEW_DEL_LEGACY_MARK"), "PREVIEW_DEL_IRREVERSIBLE_MARK": jsString("PREVIEW_DEL_IRREVERSIBLE_MARK"),
		"PREVIEW_DEL_REMOTE_MARK": jsString("PREVIEW_DEL_REMOTE_MARK"), "PREVIEW_DEL_ALSO_MARK": jsString("PREVIEW_DEL_ALSO_MARK"),
	}
	count := func(lines []string, mark string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, mark) {
				n++
			}
		}
		return n
	}
	rpFields := func(ack bool) map[string]any {
		return map[string]any{"scope": "local", "legacy_ack": ack, "detach_key": false}
	}
	drFields := func(also bool) map[string]any {
		return map[string]any{"remote_path": "octo/hello", "remote_id": "5", "confirm_name": "hello", "also_local": also}
	}
	rp := func(ack bool) []string {
		return deletePreview(registryChange{Action: "remove_project", Subject: "my-app", Fields: rpFields(ack)}, &delInfo{path: "/r/my-app", legacy: ack})
	}
	dr := func(also bool) []string {
		return deletePreview(registryChange{Action: "delete_remote_repo", Subject: "my-app", Fields: drFields(also)}, &delInfo{path: "/r/my-app"})
	}
	for _, c := range []struct {
		name  string
		lines []string
		mark  string
		n     int
	}{
		{"legacy line with ack", rp(true), marks["PREVIEW_DEL_LEGACY_MARK"], 1},
		{"legacy line without ack", rp(false), marks["PREVIEW_DEL_LEGACY_MARK"], 0},
		{"irreversible local", rp(false), marks["PREVIEW_DEL_IRREVERSIBLE_MARK"], 1},
		{"irreversible legacy", rp(true), marks["PREVIEW_DEL_IRREVERSIBLE_MARK"], 1},
		{"remote line", dr(false), marks["PREVIEW_DEL_REMOTE_MARK"], 1},
		{"remote line also_local", dr(true), marks["PREVIEW_DEL_REMOTE_MARK"], 1},
		{"also line", dr(true), marks["PREVIEW_DEL_ALSO_MARK"], 1},
		{"no also line", dr(false), marks["PREVIEW_DEL_ALSO_MARK"], 0},
	} {
		if got := count(c.lines, c.mark); got != c.n {
			t.Errorf("%s: mark %q appears %d times, the page expects %d", c.name, c.mark, got, c.n)
		}
	}
	// the page rebuilds the WHOLE preview from templates (DEL_LINES) and signs only an exact match: every template,
	// filled with sample values, must be exactly the real server line, in the real order and count
	tpl := map[string]string{}
	block := one(del, `(?s)export const DEL_LINES = \{(.*?)\n\};`, "DEL_LINES")[1]
	for _, m := range regexp.MustCompile(`(?m)^\s+([A-Za-z0-9]+): ("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'),?$`).FindAllStringSubmatch(block, -1) {
		raw := m[2]
		raw = raw[1 : len(raw)-1]
		tpl[m[1]] = raw
	}
	if len(tpl) != 19 {
		t.Fatalf("DEL_LINES has %d templates, want 19: %v", len(tpl), tpl)
	}
	fp := "SHA256:" + strings.Repeat("a", 43)
	fill := func(k string, v map[string]string) string {
		out := tpl[k]
		for name, val := range v {
			out = strings.ReplaceAll(out, "{"+name+"}", val)
		}
		return out
	}
	eq := func(name string, got []string, want []string) {
		t.Helper()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the page's lines\n%q\n!= the server's\n%q", name, want, got)
		}
	}
	idp := map[string]string{"id": "my-app", "path": "/r/my-app"}
	eq("remove_project", rp(false), []string{fill("rp1", idp), tpl["rp2"], tpl["rp3"], tpl["rp4"], tpl["stamp"]})
	eq("remove_project legacy", rp(true), []string{fill("rp1", idp), tpl["rp2"], tpl["rp3"], tpl["rp4"], tpl["rpLegacy"], tpl["stamp"]})
	eq("remove_project static", deletePreview(registryChange{Action: "remove_project", Subject: "my-app", Fields: rpFields(false)}, &delInfo{path: "/r/my-app", static: true}),
		[]string{fill("rp1", idp), tpl["rp2"], tpl["rp3"], tpl["rp4"], tpl["rpStatic"], tpl["stamp"]})
	drv := map[string]string{"remote": "octo/hello", "name": "hello", "path": "/r/my-app"}
	eq("delete_remote_repo", dr(false), []string{fill("dr1", drv), fill("dr2", drv), tpl["dr3"], tpl["drKeep"], tpl["stamp"]})
	eq("delete_remote_repo also_local", dr(true), []string{fill("dr1", drv), fill("dr2", drv), tpl["dr3"], fill("drAlso1", drv), tpl["drAlso2"], tpl["drAlso3"], tpl["stamp"]})
	eq("remove_deploy_key", deletePreview(registryChange{Action: "remove_deploy_key", Subject: "my-app", Fields: map[string]any{"remote_path": "octo/hello"}}, &delInfo{keyFP: fp}),
		[]string{fill("dk1", map[string]string{"id": "my-app", "remote": "octo/hello", "fp": fp}), tpl["dk2"], tpl["dk3"], tpl["stamp"]})
	eq("clear_orphan", deletePreview(registryChange{Action: "clear_orphan", Subject: "gh", Fields: map[string]any{"project": "octo/half"}}, &delInfo{}),
		[]string{fill("co1", map[string]string{"provider": "gh", "project": "octo/half"}), tpl["co2"], tpl["stamp"]})
	if tpl["stamp"] != deleteStamp {
		t.Errorf("stamp %q != %q", tpl["stamp"], deleteStamp)
	}
	// the fingerprint shape the page accepts is the one ssh.FingerprintSHA256 produces (SHA256: + 43 base64 characters)
	if got := one(del, `const FP_RE = /(.*)/;`, "FP_RE")[1]; got != `^SHA256:[A-Za-z0-9+/]{43}$` {
		t.Errorf("FP_RE %q", got)
	}
	// the preview answer: the page's key list is the members of removalPreview's body
	src := read("delete_project.go")
	i := strings.Index(src, "func (h *handler) removalPreview(")
	if i >= 0 {
		i += strings.Index(src[i:], "body := map[string]any{")
	}
	if i < 0 {
		t.Fatal("removalPreview body not found")
	}
	j := strings.Index(src[i:], "\n\t}\n")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+)":`).FindAllStringSubmatch(src[i:i+j], -1) {
		keys[m[1]] = true
	}
	list := one(del, `(?s)export const PREVIEW_KEYS = \[(.*?)\];`, "PREVIEW_KEYS")[1]
	var jsKeys, goKeys []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(list, -1) {
		jsKeys = append(jsKeys, m[1])
	}
	for k := range keys {
		goKeys = append(goKeys, k)
	}
	sort.Strings(jsKeys)
	sort.Strings(goKeys)
	if strings.Join(jsKeys, ",") != strings.Join(goKeys, ",") {
		t.Errorf("PREVIEW_KEYS %v != removalPreview body %v", jsKeys, goKeys)
	}
	// every state word of a partial local removal the page knows is one orchd can send
	rm := read("remove_guard.go") + src + read("../remove/remove.go")
	for _, w := range []string{"kept_in_place", "moved_aside", "already_gone", "not_managed", "project_busy", "busy_check_failed", "in_progress", "remove_failed"} {
		if !strings.Contains(rm, `"`+w+`"`) {
			t.Errorf("orchd never sends %q, which the page knows", w)
		}
		if !strings.Contains(del, "'"+w+"'") && !strings.Contains(del, w+":") && w != "removed" {
			t.Errorf("the page does not know %q", w)
		}
	}
}
