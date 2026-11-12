package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/broker"
	"aigate/internal/registrymsg"
)

// TestWebFixturesW3cVectors pins the golden vectors the browser's W3c code
// (views/providers.js) must reproduce byte for byte: the canonical change and
// its SHA-256 for the four new registry actions, the signing message of each,
// the real preview sentences, the deploy key hash and fingerprint, the shape
// validators (names, group paths, descriptions, addresses) and the list of
// error codes the new endpoints can answer. Everything is produced by the real
// Go code (registrymsg, signing, createPreview, ssh.FingerprintSHA256), so a
// change on either side fails here instead of desynchronising the console.
func TestWebFixturesW3cVectors(t *testing.T) {
	type vec struct {
		Name      string         `json:"name"`
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Canonical string         `json:"canonical,omitempty"`
		Change    string         `json:"change,omitempty"`
	}
	hash64 := strings.Repeat("ab", 32)
	cp := func(over map[string]any) map[string]any {
		m := map[string]any{"provider": "gl", "group_path": "team/sub", "template": "go", "description": "첫 프로젝트", "ai_context": false,
			"publish": "write", "deploy_key_sha256": hash64}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	sp := func(over map[string]any) map[string]any {
		m := map[string]any{"kind": "gitlab", "base_url": "http://100.64.0.10:18082", "ssh_url": "ssh://git@100.64.0.10", "token_ref": "gitlab-main"}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	cg := func(over map[string]any) map[string]any {
		m := map[string]any{"name": "newgroup", "path": "newgroup", "parent_path": "team"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	good := []vec{
		{Name: "set_provider http ip port", Action: "set_provider", Subject: "gl", Fields: sp(nil)},
		{Name: "set_provider https host", Action: "set_provider", Subject: "gitlab-2", Fields: sp(map[string]any{"base_url": "https://git.example.com", "ssh_url": "ssh://git@git.example.com:2222"})},
		{Name: "delete_provider", Action: "delete_provider", Subject: "gl", Fields: map[string]any{}},
		{Name: "create_group top level (empty parent)", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"parent_path": ""})},
		{Name: "create_group nested", Action: "create_group", Subject: "gl", Fields: cg(nil)},
		{Name: "create_project plain", Action: "create_project", Subject: "app-one", Fields: cp(nil)},
		{Name: "create_project empty description", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": ""})},
		{Name: "create_project special description", Action: "create_project", Subject: "ab", Fields: cp(map[string]any{
			"description": "say \"hi\" \\ <b>&</b> / 한글😀", "ai_context": true, "publish": "read", "template": "basic", "group_path": "team"})},
		// W3e E3 fix round: the signed owner field is a superset of both providers' rules
		{Name: "create_project owner mixed case", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "SampleUser"})},
		{Name: "create_project owner underscore", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "corp_user"})},
		{Name: "create_project owner one char", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "a"})},
		{Name: "create_project owner five segments", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "aa/bb/cc/dd/ee"})},
	}
	for i := range good {
		c, err := registrymsg.CanonicalChange(good[i].Action, good[i].Subject, good[i].Fields)
		if err != nil {
			t.Fatalf("%s: %v", good[i].Name, err)
		}
		h, _ := registrymsg.ChangeHash(good[i].Action, good[i].Subject, good[i].Fields)
		good[i].Canonical, good[i].Change = string(c), h
	}
	refused := []vec{
		{Name: "project name one letter", Action: "create_project", Subject: "a", Fields: cp(nil)},
		{Name: "project name upper", Action: "create_project", Subject: "App", Fields: cp(nil)},
		{Name: "project name 41 chars", Action: "create_project", Subject: "a" + strings.Repeat("b", 40), Fields: cp(nil)},
		{Name: "description newline", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": "a\nb"})},
		{Name: "description zero width", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": "a\u200Bb"})},
		{Name: "description bidi override", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": "a\u202Eb"})},
		{Name: "description 201 code points", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": strings.Repeat("a", 201)})},
		{Name: "description 201 astral", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": strings.Repeat("😀", 201)})},
		{Name: "description number", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"description": float64(1)})},
		{Name: "missing deploy_key_sha256", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"deploy_key_sha256": nil})},
		{Name: "upper case key hash", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"deploy_key_sha256": strings.Repeat("AB", 32)})},
		{Name: "extra field", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"x": "y"})},
		{Name: "template unknown", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"template": "rust"})},
		{Name: "publish unknown", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"publish": "admin"})},
		{Name: "ai_context string", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"ai_context": "false"})},
		{Name: "group path six segments", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "aa/bb/cc/dd/ee/ff"})},
		{Name: "group path space", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "a b"})},
		{Name: "group path over 100 bytes", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": strings.Repeat("a", 101)})},
		{Name: "group path trailing slash", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "team/"})},
		{Name: "group path double slash", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "aa//bb"})},
		{Name: "group path dot dot", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": "aa/../bb"})},
		{Name: "group path empty", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"group_path": ""})},
		{Name: "provider upper", Action: "create_project", Subject: "app-one", Fields: cp(map[string]any{"provider": "GL"})},
		{Name: "group name one letter", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"name": "a"})},
		{Name: "group name upper", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"name": "A1"})},
		{Name: "group path with slash", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"path": "aa/bb"})},
		{Name: "group parent four segments", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"parent_path": "aa/bb/cc/dd"})},
		{Name: "group parent upper", Action: "create_group", Subject: "gl", Fields: cg(map[string]any{"parent_path": "Team"})},
		{Name: "group subject upper", Action: "create_group", Subject: "GL", Fields: cg(nil)},
		{Name: "provider kind bitbucket", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"kind": "bitbucket"})},
		{Name: "base_url with path", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": "http://gitlab.example/x"})},
		{Name: "base_url trailing slash", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": "http://gitlab.example/"})},
		{Name: "base_url ftp", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": "ftp://gitlab.example"})},
		{Name: "base_url userinfo", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": "http://u:p@gitlab.example"})},
		{Name: "base_url upper scheme", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": "HTTP://gitlab.example"})},
		{Name: "base_url empty", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"base_url": ""})},
		{Name: "ssh_url other user", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"ssh_url": "ssh://root@gitlab.example"})},
		{Name: "ssh_url with path", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"ssh_url": "ssh://git@gitlab.example/x"})},
		{Name: "token_ref upper", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"token_ref": "Gitlab"})},
		{Name: "token_ref empty", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"token_ref": ""})},
		{Name: "set_provider extra field", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"value": "x"})},
		{Name: "set_provider missing field", Action: "set_provider", Subject: "gl", Fields: sp(map[string]any{"ssh_url": nil})},
		{Name: "delete_provider with a field", Action: "delete_provider", Subject: "gl", Fields: map[string]any{"kind": "gitlab"}},
	}
	for _, r := range refused {
		if _, err := registrymsg.CanonicalChange(r.Action, r.Subject, r.Fields); err == nil {
			t.Fatalf("Go accepts %q; it must be in the refused vectors only if registrymsg refuses it", r.Name)
		}
	}

	issued := time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)
	request := "00112233445566778899aabbccddeeff"
	type msgVec struct {
		Action  string `json:"action"`
		Subject string `json:"subject"`
		Change  string `json:"change"`
		Request string `json:"request"`
		Issued  string `json:"issued"`
		Text    string `json:"text"`
		Digest  string `json:"digest"`
	}
	var messages []msgVec
	for _, g := range []vec{good[0], good[2], good[4], good[5]} {
		m := signing.RegistryMessage{Action: g.Action, Subject: g.Subject, Change: g.Change, Request: request, Issued: issued}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: %v", g.Action, err)
		}
		d := m.Digest()
		messages = append(messages, msgVec{Action: m.Action, Subject: m.Subject, Change: m.Change, Request: request,
			Issued: issued.Format(registryTimeFormat), Text: m.Text(), Digest: hex.EncodeToString(d[:])})
	}

	// Deploy keys: the line, the signed hash of its text, the ssh-keygen fingerprint.
	type keyVec struct {
		Name        string `json:"name"`
		Line        string `json:"line"`
		LineSHA256  string `json:"line_sha256"`
		Fingerprint string `json:"fingerprint"`
	}
	mkKey := func(name string, seed byte, comment string) keyVec {
		priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		pk, err := ssh.NewPublicKey(priv.Public())
		if err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
		if comment != "" {
			line += " " + comment
		}
		return keyVec{Name: name, Line: line, LineSHA256: keyHash(line), Fingerprint: ssh.FingerprintSHA256(pk)}
	}
	keys := []keyVec{mkKey("ed25519 with comment", 7, "orchd@deploy"), mkKey("ed25519 no comment", 9, ""), mkKey("ed25519 other seed", 200, "k")}
	for _, k := range keys {
		if !brokerDeployKeyRE.MatchString(k.Line) {
			t.Fatalf("%s: line does not pass the broker rule: %s", k.Name, k.Line)
		}
	}

	// Real preview sentences (so the JS fake server says what the real one says).
	previewOf := func(g vec) []string {
		c := registryChange{Action: g.Action, Subject: g.Subject, Fields: g.Fields}
		if g.Action == "create_project" {
			return createPreview(c, keys[0].Fingerprint)
		}
		return providerPreview(c)
	}
	previews := map[string][]string{}
	for _, g := range good {
		previews[g.Name] = previewOf(g)
	}

	// Shape validators, evaluated by the real registrymsg functions.
	type strOK struct {
		Value string `json:"value"`
		OK    bool   `json:"ok"`
	}
	check := func(f func(string) bool, vals ...string) []strOK {
		var out []strOK
		for _, v := range vals {
			out = append(out, strOK{Value: v, OK: f(v)})
		}
		return out
	}
	names := check(registrymsg.ValidName, "ab", "a", "", "a-", "a--b", "a1", "1a", "Ab", "a_b", "a b", "한글",
		"a"+strings.Repeat("b", 39), "a"+strings.Repeat("b", 40), "-ab", "ab-")
	groupPaths := check(registrymsg.ValidGroupPath, "team", "team/sub", "aa/bb/cc/dd", "aa/bb/cc/dd/ee", "", "/aa", "aa/", "aa//bb",
		"aa/../bb", "aa/b", "Team", "team/Sub", "aa/bb cc")
	descriptions := check(registrymsg.ValidDescription, "", "첫 프로젝트", "plain text.", strings.Repeat("a", 200), strings.Repeat("a", 201),
		strings.Repeat("😀", 200), strings.Repeat("😀", 201), "tab\there", "a\nb", "a\u200Bb", "a\u202Eb", "a\u2028b", "a\u00ADb", "a\u0085b",
		"a\uE000b", "a\uFEFFb", "a\uFFFDb", "e\u0301", "a\uFE0Fb", "a\u3164b", "a\u115Fb", "a\u1160b", "a\u034Fb", "a\u180Bb", "a\uFE00b", "a\u00A0b", "a\u3000b", "<img src=x onerror=1>")
	baseURLs := check(registrymsg.ValidBaseURL, "http://gitlab.example", "https://gitlab.example", "http://100.64.0.10:18082", "http://gitlab.example:8080",
		"http://localhost", "http://[::1]", "http://[::1]:8080", "http://[2001:db8::1]", "http://[2001:DB8::1]", "http://gitlab.example/", "http://gitlab.example/x",
		"http://gitlab.example?x=1", "http://gitlab.example#f", "http://u@gitlab.example", "http://u:p@gitlab.example", "HTTP://gitlab.example",
		"http://GitLab.example", "ftp://gitlab.example", "gitlab.example", "", "http://", "http://:80", "http://gitlab.example:", "http://gitlab.example:0",
		"http://gitlab.example:08", "http://gitlab.example:65536", "http://gitlab.example:65535", "http://-a.example", "http://a-.example", "http://a..b",
		"http://a.b.", "http://2130706433", "http://0x7f.1", "http://010.1.1.1", "http://127.0.0.1", "http://1.2.3.4.5", "http://1.2.3", "http://a b",
		"http://"+strings.Repeat("a", 64)+".example", "http://"+strings.Repeat("a", 63)+".example", "https://git.example.com:2222", "http://gitlab.한글")
	sshURLs := check(registrymsg.ValidSSHURL, "ssh://git@gitlab.example", "ssh://git@100.64.0.10", "ssh://git@git.example.com:2222", "ssh://git@[2001:db8::1]",
		"ssh://root@gitlab.example", "ssh://gitlab.example", "ssh://git@gitlab.example/x", "ssh://git@gitlab.example/", "ssh://git@", "http://git@gitlab.example",
		"SSH://git@gitlab.example", "ssh://git@Gitlab.example", "ssh://git:x@gitlab.example", "git@gitlab.example:team/x.git", "ssh://git@gitlab.example:0",
		"ssh://git@gitlab.example:22", "")
	for _, set := range [][]strOK{names, groupPaths, descriptions, baseURLs, sshURLs} {
		sawOK, sawNo := false, false
		for _, s := range set {
			sawOK = sawOK || s.OK
			sawNo = sawNo || !s.OK
		}
		if !sawOK || !sawNo {
			t.Fatal("every validator vector set needs accepted and refused values")
		}
	}

	checkFixture(t, "w3c-vectors.json", map[string]any{
		"change":       good,
		"refused":      refused,
		"messages":     messages,
		"deploy_keys":  keys,
		"previews":     previews,
		"names":        names,
		"group_paths":  groupPaths,
		"descriptions": descriptions,
		"base_urls":    baseURLs,
		"ssh_urls":     sshURLs,
	})

	// The error codes the new endpoints can answer: every apiErr code of
	// providers.go and create_project.go, found in the source, so a code added
	// there is a code the console must have a fixed sentence for.
	codeRE := regexp.MustCompile(`apiErr\(http\.Status[A-Za-z]+, "([a-z_]+)"`)
	seen := map[string]bool{}
	for _, f := range []string{"providers.go", "create_project.go", "link_project.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range codeRE.FindAllSubmatch(src, -1) {
			seen[string(m[1])] = true
		}
	}
	// The ones the shared apply path of W3a and the clone endpoint add on top.
	for _, c := range []string{"bad_request", "not_signed", "change_mismatch", "path_not_allowed", "bad_signature", "unknown_signer", "stale_signature",
		"project_unknown", "request_reused", "project_exists", "project_rate_limited", "broker_error", "broker_unavailable", "registry_unavailable",
		"tokens_unavailable", "signers_unavailable", "deploy_key_unavailable", "outcome_unknown"} {
		seen[c] = true
	}
	var codes []string
	for c := range seen {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	if len(codes) < 35 {
		t.Fatalf("only %d codes found: %v", len(codes), codes)
	}
	// W3e: the codes the GitHub, link and attach paths add are all found (a scan that missed
	// link_project.go would let the console ship without a sentence for them).
	for _, c := range []string{"repo_not_found", "already_linked", "project_exists", "project_unknown", "remote_name_mismatch", "static_remote_wins", "key_expired",
		"key_staging_failed", "key_unavailable", "github_refused", "provider_rate_limited", "not_supported_for_provider", "visibility_required",
		"visibility_not_allowed", "link_partial"} {
		if !seen[c] {
			t.Errorf("error code %s is not in the scanned sources", c)
		}
	}
	checkFixture(t, "w3c-error-codes.json", map[string]any{"codes": codes})
}

// checkProviderConstants is the "providers" subtest of TestConstantsAgreeWithWeb:
// views/providers.js must exist (a missing file is a failure, never a skip) and
// carry exactly the rules registrymsg, the broker and orchd enforce.
func checkProviderConstants(t *testing.T) {
	src, err := os.ReadFile("../web/static/views/providers.js")
	if err != nil {
		t.Fatalf("views/providers.js must exist and be readable (the W3c constants are compared with it): %v", err)
	}
	js := string(src)
	jsRegex := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = /(.*)/;\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS regex constant %s not found", name)
		}
		return m[1]
	}
	jsInt := func(name string) int {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = ([0-9_]+);\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS integer constant %s not found", name)
		}
		n, _ := strconv.Atoi(strings.ReplaceAll(m[1], "_", ""))
		return n
	}
	jsString := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = '([^']*)';\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS string constant %s not found", name)
		}
		return m[1]
	}
	jsList := func(name string) []string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = \[([^\]]*)\];\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS list constant %s not found", name)
		}
		var out []string
		for _, part := range strings.Split(m[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
		}
		return out
	}
	// Patterns: provider id and token name are the subject rule, a project or
	// group name needs two characters.
	if got := jsRegex("PROVIDER_ID_RE"); got != subjectPattern {
		t.Errorf("PROVIDER_ID_RE %q != %q", got, subjectPattern)
	}
	if got := jsRegex("TOKEN_REF_RE"); got != subjectPattern {
		t.Errorf("TOKEN_REF_RE %q != %q", got, subjectPattern)
	}
	if got, want := jsRegex("NAME_RE"), `^[a-z][a-z0-9-]{1,39}$`; got != want {
		t.Errorf("NAME_RE %q != %q", got, want)
	}
	if got, want := jsRegex("HASH_RE"), `^[0-9a-f]{64}$`; got != want {
		t.Errorf("HASH_RE %q != %q", got, want)
	}
	if got, want := jsRegex("DEPLOY_KEY_RE"), brokerDeployKeyRE.String(); got != want {
		t.Errorf("DEPLOY_KEY_RE %q != the broker's %q", got, want)
	}
	for s, want := range map[string]bool{"ab": true, "a": false, "": false, "a-b": true, "A1": false, "a" + strings.Repeat("b", 39): true, "a" + strings.Repeat("b", 40): false} {
		if registrymsg.ValidName(s) != want {
			t.Errorf("ValidName(%q) != %v", s, want)
		}
	}
	if got := jsInt("NAME_MAX"); got != 40 || !registrymsg.ValidName("a"+strings.Repeat("b", got-1)) || registrymsg.ValidName("a"+strings.Repeat("b", got)) {
		t.Errorf("NAME_MAX %d does not match registrymsg", got)
	}
	if got := jsInt("MAX_GROUP_DEPTH"); got != registrymsg.MaxGroupDepth {
		t.Errorf("MAX_GROUP_DEPTH %d != %d", got, registrymsg.MaxGroupDepth)
	}
	if got := jsInt("MAX_DESCRIPTION_RUNES"); got != registrymsg.MaxDescriptionRunes ||
		!registrymsg.ValidDescription(strings.Repeat("😀", got)) || registrymsg.ValidDescription(strings.Repeat("😀", got+1)) {
		t.Errorf("MAX_DESCRIPTION_RUNES %d does not match registrymsg (code points, not UTF-16 units)", got)
	}
	// The address cap (the unexported maxURLBytes): the longest accepted address is the cap.
	urlMax := jsInt("URL_MAX")
	longHost := func(prefix string, n int) string {
		var labels []string
		for len(strings.Join(labels, ".")) < n {
			labels = append(labels, strings.Repeat("a", 60))
		}
		h := strings.Join(labels, ".")
		return prefix + h[:n-len(prefix)]
	}
	atCap, overCap := longHost("http://", urlMax), longHost("http://", urlMax+1)
	if strings.HasSuffix(atCap, ".") {
		atCap = atCap[:len(atCap)-1] + "a"
	}
	if strings.HasSuffix(overCap, ".") {
		overCap = overCap[:len(overCap)-1] + "a"
	}
	if len(atCap) != urlMax || !registrymsg.ValidBaseURL(atCap) || len(overCap) != urlMax+1 || registrymsg.ValidBaseURL(overCap) {
		t.Errorf("URL_MAX %d does not match registrymsg (at cap %v, over cap %v)", urlMax, registrymsg.ValidBaseURL(atCap), registrymsg.ValidBaseURL(overCap))
	}
	// Enums, in the order the browser shows them.
	templates, modes := jsList("TEMPLATES"), jsList("PUBLISH_MODES")
	if strings.Join(templates, ",") != "basic,go" || strings.Join(modes, ",") != "write,read" {
		t.Errorf("TEMPLATES %v PUBLISH_MODES %v", templates, modes)
	}
	okFields := map[string]any{"provider": "gl", "group_path": "team", "template": "go", "description": "", "ai_context": false, "publish": "write", "deploy_key_sha256": strings.Repeat("a", 64)}
	for _, tpl := range templates {
		f := map[string]any{}
		for k, v := range okFields {
			f[k] = v
		}
		f["template"] = tpl
		if _, err := registrymsg.CanonicalChange("create_project", "ab", f); err != nil {
			t.Errorf("template %q is refused by Go: %v", tpl, err)
		}
	}
	for _, m := range modes {
		f := map[string]any{}
		for k, v := range okFields {
			f[k] = v
		}
		f["publish"] = m
		if _, err := registrymsg.CanonicalChange("create_project", "ab", f); err != nil {
			t.Errorf("publish mode %q is refused by Go: %v", m, err)
		}
	}
	for _, bad := range []string{"rust", "Go", "", "write"} {
		f := map[string]any{}
		for k, v := range okFields {
			f[k] = v
		}
		f["template"] = bad
		if _, err := registrymsg.CanonicalChange("create_project", "ab", f); err == nil {
			t.Errorf("template %q must be refused", bad)
		}
	}
	// The fixed creation steps: exactly the broker's orphan steps.
	steps := jsList("PARTIAL_STEPS")
	for _, s := range steps {
		if !broker.ValidOrphanStep(s) {
			t.Errorf("step %q is not a broker step", s)
		}
	}
	if len(steps) != 6 || broker.ValidOrphanStep("clone") {
		t.Errorf("PARTIAL_STEPS %v", steps)
	}
	// Actions.
	for name, want := range map[string]string{"ACTION_SET_PROVIDER": registrymsg.ActionSetProvider, "ACTION_DELETE_PROVIDER": registrymsg.ActionDeleteProvider,
		"ACTION_CREATE_GROUP": registrymsg.ActionCreateGroup, "ACTION_CREATE_PROJECT": registrymsg.ActionCreateProject} {
		if got := jsString(name); got != want {
			t.Errorf("%s %q != %q", name, got, want)
		}
	}
	// SCHEMA: the field names and types per action are exactly registrymsg's:
	// all of them are taken, none missing, none extra, and only the fields
	// registrymsg lists may be "".
	good := map[string]map[string]any{
		"set_provider":    {"kind": "gitlab", "base_url": "http://gitlab.example", "ssh_url": "ssh://git@gitlab.example", "token_ref": "gl-admin"},
		"delete_provider": {},
		"create_group":    {"name": "grp", "path": "grp", "parent_path": "team"},
		"create_project":  okFields,
		// W3e: the two new actions (create_project's optional visibility is pinned in checkW3eConstants)
		"link_project":  {"provider": "gh", "remote_path": "octo/Hello-World", "ai_context": false, "publish": "read", "deploy_key_sha256": strings.Repeat("a", 64)},
		"attach_remote": {"provider": "gh", "remote_path": "octo/Hello-World", "publish": "read", "deploy_key_sha256": strings.Repeat("a", 64)},
	}
	for action, sample := range good {
		key := "[ACTION_" + strings.ToUpper(action) + "]"
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `: \{([^}]*)\},?\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS SCHEMA entry %s not found", key)
		}
		jsFields := map[string]string{}
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, typ, ok := strings.Cut(part, ": ")
			if !ok {
				t.Fatalf("SCHEMA %s: bad entry %q", key, part)
			}
			jsFields[name] = strings.Trim(typ, "'")
		}
		if len(jsFields) != len(sample) {
			t.Errorf("SCHEMA %s fields %v, Go takes %d", key, jsFields, len(sample))
		}
		for name, v := range sample {
			want := "s"
			if _, isBool := v.(bool); isBool {
				want = "b"
			}
			if jsFields[name] != want {
				t.Errorf("SCHEMA %s field %s: %q, want %q", key, name, jsFields[name], want)
			}
		}
		subject := "gl"
		if action == "create_project" || action == "link_project" {
			subject = "ab"
		}
		if _, err := registrymsg.CanonicalChange(action, subject, sample); err != nil {
			t.Errorf("registrymsg refuses the browser's %s fields: %v", action, err)
		}
		for name := range sample {
			less := map[string]any{}
			for k, v := range sample {
				if k != name {
					less[k] = v
				}
			}
			if _, err := registrymsg.CanonicalChange(action, subject, less); err == nil {
				t.Errorf("registrymsg accepts %s without %s", action, name)
			}
		}
		more := map[string]any{"extra": "x"}
		for k, v := range sample {
			more[k] = v
		}
		if _, err := registrymsg.CanonicalChange(action, subject, more); err == nil {
			t.Errorf("registrymsg accepts an extra field for %s", action)
		}
	}
	// The only fields that may be empty: parent_path and description (the browser's EMPTY_OK).
	emptyOK := jsList("EMPTY_OK")
	sort.Strings(emptyOK)
	if strings.Join(emptyOK, ",") != "create_group.parent_path,create_project.description" {
		t.Errorf("EMPTY_OK %v", emptyOK)
	}
	for action, sample := range good {
		for name, v := range sample {
			if _, isStr := v.(string); !isStr {
				continue
			}
			f := map[string]any{}
			for k, x := range sample {
				f[k] = x
			}
			f[name] = ""
			subject := "gl"
			if action == "create_project" || action == "link_project" {
				subject = "ab"
			}
			_, err := registrymsg.CanonicalChange(action, subject, f)
			allowed := action+"."+name == "create_group.parent_path" || action+"."+name == "create_project.description"
			if (err == nil) != allowed {
				t.Errorf("%s.%s empty: Go accepts=%v, browser rule says %v", action, name, err == nil, allowed)
			}
		}
	}
	// The fixed lines of the server's preview that the browser's gate counts: exact lines, not marks.
	lineOf := func(ai bool, publish string) (aiLine, pubLine string) {
		c := registryChange{Action: "create_project", Subject: "ab", Fields: map[string]any{"provider": "gl", "group_path": "team", "template": "go", "description": "",
			"ai_context": ai, "publish": publish, "deploy_key_sha256": strings.Repeat("a", 64)}}
		pv := createPreview(c, "SHA256:x")
		return pv[3], pv[4]
	}
	aiOff, pubWrite := lineOf(false, "write")
	aiOn, pubRead := lineOf(true, "read")
	for name, want := range map[string]string{"PREVIEW_AI_OFF": aiOff, "PREVIEW_AI_ON": aiOn, "PREVIEW_PUBLISH_WRITE": pubWrite, "PREVIEW_PUBLISH_READ": pubRead} {
		if got := jsString(name); got != want {
			t.Errorf("%s %q != the server's line %q", name, got, want)
		}
	}
	if got := jsInt("GROUP_SETTLE_MS"); got < 200 || got > 2000 {
		t.Errorf("GROUP_SETTLE_MS %d", got)
	}
	// Timeouts: longer than orchd's worst case for each call.
	if got := jsInt("CREATE_APPLY_TIMEOUT_MS"); got < 130_000 {
		t.Errorf("CREATE_APPLY_TIMEOUT_MS %d must be at least 130000 (ruling: orchd's create call can take about 90 s)", got)
	}
	if got := jsInt("CLONE_TIMEOUT_MS"); got < 130_000 {
		t.Errorf("CLONE_TIMEOUT_MS %d must be at least 130000 (git clone limit is 120 s)", got)
	}
	if got := jsInt("PROVIDER_TIMEOUT_MS"); got < 30_000 {
		t.Errorf("PROVIDER_TIMEOUT_MS %d must be at least 30000 (provider and group writes take up to about 25 s)", got)
	}
	// The browser's message format knows the four new actions.
	wa, werr := os.ReadFile("../web/static/webauthn.js")
	if werr != nil {
		t.Fatal(werr)
	}
	for _, a := range []string{registrymsg.ActionSetProvider, registrymsg.ActionDeleteProvider, registrymsg.ActionCreateGroup, registrymsg.ActionCreateProject,
		registrymsg.ActionLinkProject, registrymsg.ActionAttachRemote} {
		if !strings.Contains(string(wa), a) {
			t.Errorf("webauthn.js does not know action %s", a)
		}
		m := signing.RegistryMessage{Action: a, Subject: "ab", Change: strings.Repeat("a", 64), Request: strings.Repeat("b", 32), Issued: time.Date(2026, 11, 9, 10, 4, 5, 0, time.UTC)}
		if m.Validate() != nil {
			t.Errorf("signing refuses a %s message", a)
		}
	}
	// Orphan origins: exactly the broker's two.
	origins := jsList("ORPHAN_ORIGINS")
	if len(origins) != 2 || !broker.ValidOrphanOrigin("created") || !broker.ValidOrphanOrigin("linked") || broker.ValidOrphanOrigin("bogus") {
		t.Errorf("ORPHAN_ORIGINS %v", origins)
	}
	for _, o := range origins {
		if !broker.ValidOrphanOrigin(o) {
			t.Errorf("origin %q is not a broker origin", o)
		}
	}
	// Shipped, loaded and routed.
	for _, f := range []struct{ file, needle string }{
		{"app.js", "'providers'"}, {"router.js", "case 'providers':"}, {"index.html", `href="#/providers"`},
	} {
		b, rerr := os.ReadFile(filepath.Join("../web/static", f.file))
		if rerr != nil || !strings.Contains(string(b), f.needle) {
			t.Errorf("%s does not wire the providers view (%q)", f.file, f.needle)
		}
	}
}

// checkW3eConstants is the "w3e" subtest of TestConstantsAgreeWithWeb: what the
// console copies for GitHub, visibility, link and attach is pinned to Go. A missing
// constant or file is a failure, never a skip.
func checkW3eConstants(t *testing.T) {
	src, err := os.ReadFile("../web/static/views/providers.js")
	if err != nil {
		t.Fatalf("views/providers.js must exist and be readable: %v", err)
	}
	js := string(src)
	jsRegex := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = /(.*)/;\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS regex constant %s not found", name)
		}
		return m[1]
	}
	jsInt := func(name string) int {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = ([0-9_]+);\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS integer constant %s not found", name)
		}
		n, _ := strconv.Atoi(strings.ReplaceAll(m[1], "_", ""))
		return n
	}
	jsString := func(name string) string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = '([^']*)';\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS string constant %s not found", name)
		}
		return m[1]
	}
	jsList := func(name string) []string {
		m := regexp.MustCompile(`(?m)^(?:export )?const ` + name + ` = \[([^\]]*)\];\s*$`).FindStringSubmatch(js)
		if m == nil {
			t.Fatalf("JS list constant %s not found", name)
		}
		var out []string
		for _, part := range strings.Split(m[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
		}
		return out
	}
	// The page's set_provider kind rule: every kind it names is a registrymsg kind, and so is every kind it lists.
	m := regexp.MustCompile(`ACTION_SET_PROVIDER \+ '\.kind': return ok\((.*)\);`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("the page's set_provider kind rule was not found")
	}
	ruleKinds := regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(m[1], -1)
	if len(ruleKinds) != 2 {
		t.Fatalf("the page rule names %d provider kinds: %q", len(ruleKinds), m[1])
	}
	for _, k := range ruleKinds {
		if !registrymsg.ValidProviderKind(k[1]) {
			t.Errorf("the page offers provider kind %q that registrymsg refuses", k[1])
		}
	}
	kinds := jsList("PROVIDER_KINDS")
	if strings.Join(kinds, ",") != "gitlab,github" {
		t.Errorf("PROVIDER_KINDS %v", kinds)
	}
	for _, k := range kinds {
		if !registrymsg.ValidProviderKind(k) {
			t.Errorf("PROVIDER_KINDS has %q, refused by registrymsg", k)
		}
	}
	if registrymsg.ValidProviderKind("bitbucket") || registrymsg.ValidProviderKind("GitHub") || registrymsg.ValidProviderKind("") {
		t.Error("provider kinds: the set is closed to gitlab and github")
	}
	for name, want := range map[string]string{"ACTION_LINK_PROJECT": registrymsg.ActionLinkProject, "ACTION_ATTACH_REMOTE": registrymsg.ActionAttachRemote} {
		if got := jsString(name); got != want {
			t.Errorf("%s %q != %q", name, got, want)
		}
	}
	if mm := regexp.MustCompile(`token_ref_unknown: '([^']*)'`).FindStringSubmatch(js); mm == nil || strings.Contains(strings.ToLower(mm[1]), "gitlab") {
		t.Errorf("the page's token_ref_unknown sentence must exist and be provider-neutral: %v", mm)
	}
	// Visibility: the enum, and optional exactly as registrymsg has it (with or without, never anything else).
	vis := jsList("VISIBILITIES")
	if strings.Join(vis, ",") != "public,private" {
		t.Errorf("VISIBILITIES %v", vis)
	}
	hash64 := strings.Repeat("ab", 32)
	cp := func(extra map[string]any) map[string]any {
		f := map[string]any{"provider": "gh", "group_path": "octo", "template": "go", "description": "", "ai_context": false, "publish": "write", "deploy_key_sha256": hash64}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	for _, v := range vis {
		if _, err := registrymsg.CanonicalChange("create_project", "hello", cp(map[string]any{"visibility": v})); err != nil {
			t.Errorf("visibility %q is refused by Go: %v", v, err)
		}
	}
	for _, v := range []string{"", "Public", "internal", "PRIVATE"} {
		if _, err := registrymsg.CanonicalChange("create_project", "hello", cp(map[string]any{"visibility": v})); err == nil {
			t.Errorf("visibility %q must be refused", v)
		}
	}
	optional := jsList("OPTIONAL_CREATE_FIELDS")
	if strings.Join(optional, ",") != "visibility" {
		t.Errorf("OPTIONAL_CREATE_FIELDS %v", optional)
	}
	if _, err := registrymsg.CanonicalChange("create_project", "hello", cp(nil)); err != nil {
		t.Errorf("create_project without visibility must be accepted by Go: %v", err)
	}
	if _, err := registrymsg.CanonicalChange("create_project", "hello", cp(map[string]any{"visibility": "public", "extra": "x"})); err == nil {
		t.Error("an extra field next to the optional one must be refused")
	}
	// GitHub names and the remote path bounds.
	if got, want := jsRegex("GITHUB_LOGIN_RE"), "^"+registrymsg.GitHubLoginPattern+"$"; got != want {
		t.Errorf("GITHUB_LOGIN_RE %q != %q", got, want)
	}
	if got, want := jsRegex("GITHUB_REPO_RE"), "^"+registrymsg.GitHubRepoPattern+"$"; got != want {
		t.Errorf("GITHUB_REPO_RE %q != %q", got, want)
	}
	for name, want := range map[string]int{"MAX_REMOTE_SEGMENTS": registrymsg.MaxRemoteSegments, "MAX_REMOTE_SEGMENT_LEN": registrymsg.MaxRemoteSegmentLen, "MAX_REMOTE_PATH_LEN": registrymsg.MaxRemotePathLen} {
		if got := jsInt(name); got != want {
			t.Errorf("%s %d != %d", name, got, want)
		}
	}
	if registrymsg.MaxRemoteSegments != 5 || registrymsg.MaxRemoteSegmentLen != 100 || registrymsg.MaxRemotePathLen != 255 {
		t.Errorf("remote path bounds moved: %d %d %d", registrymsg.MaxRemoteSegments, registrymsg.MaxRemoteSegmentLen, registrymsg.MaxRemotePathLen)
	}
	// The GitHub address defaults are accepted by Go and are what the page fills in.
	if got := jsString("GITHUB_API_URL"); got != "https://api.github.com" || !registrymsg.ValidBaseURL(got) {
		t.Errorf("GITHUB_API_URL %q", got)
	}
	if got := jsString("GITHUB_SSH_URL"); got != "ssh://git@github.com" || !registrymsg.ValidSSHURL(got) {
		t.Errorf("GITHUB_SSH_URL %q", got)
	}
	// The fixed preview lines of the W3e proposals, from the real previews (exact lines, not marks).
	pvCreate := func(vis string) []string {
		return createPreview(registryChange{Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": vis})}, "SHA256:x")
	}
	contains := func(lines []string, want string) int {
		n := 0
		for _, l := range lines {
			if l == want {
				n++
			}
		}
		return n
	}
	if got := jsString("PREVIEW_VIS_PUBLIC"); contains(pvCreate("public"), got) != 1 || contains(pvCreate("private"), got) != 0 {
		t.Errorf("PREVIEW_VIS_PUBLIC %q is not the server's public line", got)
	}
	if got := jsString("PREVIEW_VIS_PRIVATE"); contains(pvCreate("private"), got) != 1 || contains(pvCreate("public"), got) != 0 {
		t.Errorf("PREVIEW_VIS_PRIVATE %q is not the server's private line", got)
	}
	warn := jsString("PREVIEW_KEY_WARN")
	link := registryChange{Action: "link_project", Subject: "hello", Fields: map[string]any{"provider": "gh", "remote_path": "octo/hello", "ai_context": false, "publish": "read", "deploy_key_sha256": hash64}}
	attach := registryChange{Action: "attach_remote", Subject: "hello", Fields: map[string]any{"provider": "gh", "remote_path": "octo/hello", "publish": "read", "deploy_key_sha256": hash64}}
	for _, c := range []registryChange{link, attach} {
		for _, kind := range []string{"github", "gitlab"} {
			if contains(remotePreview(c, kind, "SHA256:x"), warn) != 1 {
				t.Errorf("PREVIEW_KEY_WARN is not the server's line for %s/%s", c.Action, kind)
			}
		}
	}
	// Timeouts: link and attach outwait orchd like a create; their propose reads the broker (limit 15 s).
	if got := jsInt("LINK_APPLY_TIMEOUT_MS"); got < 130_000 {
		t.Errorf("LINK_APPLY_TIMEOUT_MS %d must be at least 130000", got)
	}
	if got := jsInt("ATTACH_APPLY_TIMEOUT_MS"); got < 130_000 {
		t.Errorf("ATTACH_APPLY_TIMEOUT_MS %d must be at least 130000", got)
	}
	if got := jsInt("LINK_PROPOSE_TIMEOUT_MS"); got <= 15_000 {
		t.Errorf("LINK_PROPOSE_TIMEOUT_MS %d must exceed the broker read limit of 15 s", got)
	}
}

// TestWebFixturesW3eVectors writes the golden vectors for the W3e schema
// additions (github provider, visibility, link_project, attach_remote, remote
// paths) from the real Go code. Nothing in the page reads this file yet: it is
// the contract E5's JS must reproduce.
func TestWebFixturesW3eVectors(t *testing.T) {
	type vec struct {
		Name      string         `json:"name"`
		Action    string         `json:"action"`
		Subject   string         `json:"subject"`
		Fields    map[string]any `json:"fields"`
		Canonical string         `json:"canonical,omitempty"`
		Change    string         `json:"change,omitempty"`
	}
	hash64 := strings.Repeat("ab", 32)
	link := func(over map[string]any) map[string]any {
		m := map[string]any{"provider": "gh", "remote_path": "octo/Hello-World", "ai_context": false, "publish": "read", "deploy_key_sha256": hash64}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	attach := func(over map[string]any) map[string]any {
		m := map[string]any{"provider": "gh", "remote_path": "octo/Hello-World", "publish": "write", "deploy_key_sha256": hash64}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	cp := func(over map[string]any) map[string]any {
		m := map[string]any{"provider": "gh", "group_path": "octo", "template": "go", "description": "", "ai_context": false, "publish": "write", "deploy_key_sha256": hash64}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	gh := map[string]any{"kind": "github", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "github-main"}
	good := []vec{
		{Name: "github set_provider", Action: "set_provider", Subject: "gh", Fields: gh},
		{Name: "create_project private", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": "private"})},
		{Name: "create_project public", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": "public"})},
		{Name: "link_project github", Action: "link_project", Subject: "hello", Fields: link(nil)},
		{Name: "link_project gitlab nested", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"provider": "gl", "remote_path": "team/sub/app.v2", "ai_context": true})},
		{Name: "attach_remote", Action: "attach_remote", Subject: "a", Fields: attach(nil)},
	}
	for i := range good {
		c, err := registrymsg.CanonicalChange(good[i].Action, good[i].Subject, good[i].Fields)
		if err != nil {
			t.Fatalf("%s: %v", good[i].Name, err)
		}
		h, _ := registrymsg.ChangeHash(good[i].Action, good[i].Subject, good[i].Fields)
		good[i].Canonical, good[i].Change = string(c), h
	}
	refused := []vec{
		{Name: "visibility empty", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": ""})},
		{Name: "visibility upper", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": "Public"})},
		{Name: "visibility internal", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": "internal"})},
		{Name: "visibility bool", Action: "create_project", Subject: "hello", Fields: cp(map[string]any{"visibility": true})},
		{Name: "link remote_path one segment", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "solo"})},
		{Name: "link remote_path dotdot", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "a/../b"})},
		{Name: "link remote_path dot git", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "octo/repo.git"})},
		{Name: "link remote_path percent", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "octo/re%2fpo"})},
		{Name: "link remote_path unicode", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "octo/저장소"})},
		{Name: "link remote_path six segments", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"remote_path": "a/b/c/d/e/f"})},
		{Name: "link subject one letter", Action: "link_project", Subject: "a", Fields: link(nil)},
		{Name: "link without ai_context", Action: "link_project", Subject: "hello", Fields: link(map[string]any{"ai_context": nil})},
		{Name: "attach with ai_context", Action: "attach_remote", Subject: "hello", Fields: attach(map[string]any{"ai_context": false})},
		{Name: "attach with visibility", Action: "attach_remote", Subject: "hello", Fields: attach(map[string]any{"visibility": "private"})},
		{Name: "set_provider kind GitHub", Action: "set_provider", Subject: "gh", Fields: map[string]any{"kind": "GitHub", "base_url": "https://api.github.com", "ssh_url": "ssh://git@github.com", "token_ref": "github-main"}},
	}
	for i := range refused {
		if _, err := registrymsg.CanonicalChange(refused[i].Action, refused[i].Subject, refused[i].Fields); err == nil {
			t.Fatalf("Go accepts %q", refused[i].Name)
		}
		// nil entries in the override maps mean "absent"
		for k, v := range refused[i].Fields {
			if v == nil {
				delete(refused[i].Fields, k)
			}
		}
	}
	// E5: the signing messages, the real preview sentences, the deploy keys and the name rules
	// the browser's link / github code must reproduce (all evaluated by the real Go code).
	issued := time.Date(2026, 11, 12, 10, 4, 5, 0, time.UTC)
	request := "00112233445566778899aabbccddeeff"
	type msgVec struct {
		Name    string `json:"name"`
		Action  string `json:"action"`
		Subject string `json:"subject"`
		Change  string `json:"change"`
		Request string `json:"request"`
		Issued  string `json:"issued"`
		Text    string `json:"text"`
		Digest  string `json:"digest"`
	}
	var messages []msgVec
	for _, g := range good {
		m := signing.RegistryMessage{Action: g.Action, Subject: g.Subject, Change: g.Change, Request: request, Issued: issued}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		d := m.Digest()
		messages = append(messages, msgVec{Name: g.Name, Action: m.Action, Subject: m.Subject, Change: m.Change, Request: request,
			Issued: issued.Format(registryTimeFormat), Text: m.Text(), Digest: hex.EncodeToString(d[:])})
	}
	type keyVec struct {
		Name        string `json:"name"`
		Line        string `json:"line"`
		LineSHA256  string `json:"line_sha256"`
		Fingerprint string `json:"fingerprint"`
	}
	mkKey := func(name string, seed byte, comment string) keyVec {
		priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		pk, err := ssh.NewPublicKey(priv.Public())
		if err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " " + comment
		if !brokerDeployKeyRE.MatchString(line) {
			t.Fatalf("%s: line does not pass the broker rule", name)
		}
		return keyVec{Name: name, Line: line, LineSHA256: keyHash(line), Fingerprint: ssh.FingerprintSHA256(pk)}
	}
	keys := []keyVec{mkKey("per-repo key", 11, "orchd-deploy"), mkKey("other per-repo key", 12, "orchd-deploy")}
	previews := map[string][]string{}
	for _, g := range good {
		c := registryChange{Action: g.Action, Subject: g.Subject, Fields: g.Fields}
		switch g.Action {
		case "create_project":
			previews[g.Name] = createPreview(c, keys[0].Fingerprint)
		case "link_project", "attach_remote":
			kind := "gitlab"
			if g.Fields["provider"] == "gh" {
				kind = "github"
			}
			previews[g.Name] = remotePreview(c, kind, keys[0].Fingerprint)
		default:
			previews[g.Name] = providerPreview(c)
		}
	}
	type strOK struct {
		Value string `json:"value"`
		OK    bool   `json:"ok"`
	}
	check := func(f func(string) bool, vals ...string) []strOK {
		var out []strOK
		for _, v := range vals {
			out = append(out, strOK{Value: v, OK: f(v)})
		}
		return out
	}
	forKind := func(kind string, f func(string, string) bool) func(string) bool {
		return func(s string) bool { return f(kind, s) }
	}
	ghLogins := check(registrymsg.ValidGitHubOwner, "octo", "SampleUser", "a", "1abc", "corp_user", "a-b", "a-", "-a", "", "a b", "a/b", "a.b",
		strings.Repeat("a", 39), strings.Repeat("a", 40), "한글")
	ghRepos := check(registrymsg.ValidGitHubRepoName, "Hello-World", "my_repo", "Repo.js", "a", ".", "..", "x.git", "x.GIT", "x.gitx", "a b", "a/b",
		strings.Repeat("a", 100), strings.Repeat("a", 101), "")
	remotePaths := check(registrymsg.ValidRemotePath, "octo/Hello-World", "team/sub/app.v2", "a/b/c/d/e", "a/b/c/d/e/f", "solo", "a//b", "a/./b", "a/../b",
		"octo/repo.git", "octo/repo.GIT", "octo/re%2fpo", "octo/저장소", "a/"+strings.Repeat("b", 100), "a/"+strings.Repeat("b", 101), "/a/b", "a/b/")
	remoteGH := check(forKind("github", registrymsg.ValidRemotePathForKind), "octo/Hello-World", "octo/my_repo", "a/b/c", "solo", "octo/x.git", "-a/b", "a_b/c")
	remoteGL := check(forKind("gitlab", registrymsg.ValidRemotePathForKind), "team/app-one", "team/sub/app-one", "Team/app-one", "team/App", "team/app_one",
		"aa/bb/cc/dd/ee", "aa/bb/cc/dd/ee/ff", "solo")
	ownerGH := check(forKind("github", registrymsg.ValidOwnerForKind), "octo", "SampleUser", "a/b", "a-", "")
	ownerGL := check(forKind("gitlab", registrymsg.ValidOwnerForKind), "team", "team/sub", "Team", "a", "aa/bb/cc/dd", "aa/bb/cc/dd/ee")
	for _, set := range [][]strOK{ghLogins, ghRepos, remotePaths, remoteGH, remoteGL, ownerGH, ownerGL} {
		sawOK, sawNo := false, false
		for _, s := range set {
			sawOK, sawNo = sawOK || s.OK, sawNo || !s.OK
		}
		if !sawOK || !sawNo {
			t.Fatal("every validator vector set needs accepted and refused values")
		}
	}
	checkFixture(t, "w3e-vectors.json", map[string]any{
		"good": good, "refused": refused, "messages": messages, "deploy_keys": keys, "previews": previews,
		"github_owners": ghLogins, "github_repos": ghRepos, "remote_paths": remotePaths, "remote_paths_github": remoteGH,
		"remote_paths_gitlab": remoteGL, "owner_github": ownerGH, "owner_gitlab": ownerGL,
	})
}
