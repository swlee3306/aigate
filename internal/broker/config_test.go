package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsUnknownKeys(t *testing.T) {
	e := newTestEnv(t)
	e.set(t, []string{"surprise"}, true)
	if _, err := LoadConfigOpts(e.CfgPath, e.loadOpts()); err == nil {
		t.Fatal("unknown top-level key accepted")
	}
	e = newTestEnv(t)
	e.set(t, []string{"openai", "temperature"}, 1)
	if _, err := LoadConfigOpts(e.CfgPath, e.loadOpts()); err == nil {
		t.Fatal("unknown nested key accepted")
	}
	// trailing data is refused too
	e = newTestEnv(t)
	b, _ := os.ReadFile(e.CfgPath)
	os.WriteFile(e.CfgPath, append(b, []byte(` {"x":1}`)...), 0o644)
	if _, err := LoadConfigOpts(e.CfgPath, e.loadOpts()); err == nil {
		t.Fatal("trailing data accepted")
	}
	// and the happy path loads
	e = newTestEnv(t)
	cfg := e.load(t)
	if cfg.OpenAI.Model != "test-model" || len(cfg.UIDs()) != 1 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestConfigRequiresModelAndPricing(t *testing.T) {
	cases := []struct {
		name string
		path []string
		v    any
	}{
		{"empty model", []string{"openai", "model"}, ""},
		{"missing model", []string{"openai", "model"}, nil},
		{"missing pricing", []string{"openai", "pricing"}, nil},
		{"zero input", []string{"openai", "pricing", "input_usd_per_mtok"}, 0},
		{"negative input", []string{"openai", "pricing", "input_usd_per_mtok"}, -1.5},
		{"missing output", []string{"openai", "pricing", "output_usd_per_mtok"}, nil},
		{"zero output", []string{"openai", "pricing", "output_usd_per_mtok"}, 0},
		{"negative output", []string{"openai", "pricing", "output_usd_per_mtok"}, -3},
		{"string price", []string{"openai", "pricing", "output_usd_per_mtok"}, "LEAK-ME-12345"},
		{"huge price", []string{"openai", "pricing", "output_usd_per_mtok"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			if c.name == "huge price" {
				// 1e400 cannot be marshalled by encoding/json; write it by hand.
				b, _ := os.ReadFile(e.CfgPath)
				s := strings.Replace(string(b), `"output_usd_per_mtok":10`, `"output_usd_per_mtok":1e400`, 1)
				os.WriteFile(e.CfgPath, []byte(s), 0o644)
			} else {
				e.set(t, c.path, c.v)
			}
			_, err := LoadConfigOpts(e.CfgPath, e.loadOpts())
			if err == nil {
				t.Fatal("accepted")
			}
			for _, leak := range []string{"LEAK-ME-12345", "-1.5", "1e400"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error reflects config value %q: %v", leak, err)
				}
			}
		})
	}
}

func TestConfigValidatesOtherFields(t *testing.T) {
	bad := []struct {
		name string
		path []string
		v    any
	}{
		{"version", []string{"version"}, 2},
		{"relative socket", []string{"socket"}, "b.sock"},
		{"empty group", []string{"socket_group"}, ""},
		{"no uids", []string{"allow_uids"}, []string{}},
		{"unknown user", []string{"allow_uids"}, []string{"no-such-user-xyz"}},
		{"relative repo", []string{"context_repos"}, []string{"repo"}},
		{"relative key file", []string{"openai", "key_file"}, "openai.key"},
		{"budget negative", []string{"limits", "weekly_budget_usd"}, -1},
		{"gitlab bad url", []string{"gitlab", "base_url"}, "ftp://x"},
		{"gitlab userinfo", []string{"gitlab", "base_url"}, "http://u:p@host"},
		{"gitlab project shape", []string{"gitlab", "projects"}, map[string]any{"/abs": "../evil"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.set(t, c.path, c.v)
			opts := e.loadOpts()
			opts.LookupUser = func(n string) (uint32, error) {
				if n == "no-such-user-xyz" {
					return 0, os.ErrNotExist
				}
				return 1000, nil
			}
			if _, err := LoadConfigOpts(e.CfgPath, opts); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// context_repos may be absent/empty (default empty), gitlab may be absent.
	e := newTestEnv(t)
	e.set(t, []string{"context_repos"}, nil)
	e.set(t, []string{"gitlab"}, nil)
	cfg := e.load(t)
	if len(cfg.ContextRepos) != 0 || cfg.GitLab.Configured() {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestConfigTrustRootOwnedOnly(t *testing.T) {
	// Default rule: only uid 0 owns a trusted config. A test user is not root,
	// so a file it created is refused by LoadConfig's default options.
	e := newTestEnv(t)
	if os.Geteuid() != 0 {
		if _, err := LoadConfigOpts(e.CfgPath, LoadOptions{Trust: TrustOptions{StopAt: e.Dir}}); err == nil {
			t.Fatal("non-root-owned config accepted by default rule")
		}
	}
	// Owner rule through the seam.
	deny := TrustOptions{OwnerOK: func(uint32) bool { return false }, StopAt: e.Dir}
	if _, err := LoadConfigOpts(e.CfgPath, LoadOptions{Trust: deny}); err == nil {
		t.Fatal("owner refused by seam but config accepted")
	}
	// Mode bits are tested directly.
	for _, m := range []os.FileMode{0o664, 0o646, 0o666, 0o620, 0o602} {
		e := newTestEnv(t)
		os.Chmod(e.CfgPath, m)
		if _, err := LoadConfigOpts(e.CfgPath, e.loadOpts()); err == nil {
			t.Fatalf("mode %04o accepted", m)
		}
	}
	// A writable parent directory is refused.
	e = newTestEnv(t)
	sub := filepath.Join(e.Dir, "etc")
	os.Mkdir(sub, 0o755)
	cfgPath := filepath.Join(sub, "broker.json")
	b, _ := os.ReadFile(e.CfgPath)
	os.WriteFile(cfgPath, b, 0o644)
	opts := e.loadOpts()
	if _, err := LoadConfigOpts(cfgPath, opts); err != nil {
		t.Fatalf("clean chain refused: %v", err)
	}
	os.Chmod(sub, 0o775)
	if _, err := LoadConfigOpts(cfgPath, opts); err == nil {
		t.Fatal("group-writable parent accepted")
	}
	os.Chmod(sub, 0o755)
	// The whole ancestor chain counts, not only the parent.
	deep := filepath.Join(sub, "deeper")
	os.Mkdir(deep, 0o755)
	deepCfg := filepath.Join(deep, "broker.json")
	os.WriteFile(deepCfg, b, 0o644)
	if _, err := LoadConfigOpts(deepCfg, opts); err != nil {
		t.Fatalf("clean deep chain refused: %v", err)
	}
	os.Chmod(sub, 0o757)
	if _, err := LoadConfigOpts(deepCfg, opts); err == nil {
		t.Fatal("other-writable grandparent accepted")
	}
	os.Chmod(sub, 0o755)
	// Symlinked config file and symlinked parent are refused.
	link := filepath.Join(sub, "link.json")
	os.Symlink(cfgPath, link)
	if _, err := LoadConfigOpts(link, opts); err == nil {
		t.Fatal("symlinked config accepted")
	}
	dlink := filepath.Join(e.Dir, "dlink")
	os.Symlink(sub, dlink)
	if _, err := LoadConfigOpts(filepath.Join(dlink, "broker.json"), opts); err == nil {
		t.Fatal("symlinked parent accepted")
	}
	// Missing file and directory are errors (no "absent means off").
	if _, err := LoadConfigOpts(filepath.Join(sub, "none.json"), opts); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestKeyFileMustBeOwnerOnly(t *testing.T) {
	e := newTestEnv(t)
	cfg := e.load(t)
	ownerOK := func(uint32) bool { return true }
	if _, err := LoadSecrets(cfg, ownerOK); err != nil {
		t.Fatalf("0400 key refused: %v", err)
	}
	for _, m := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o700} {
		os.Chmod(e.KeyFile, m)
		_, err := LoadSecrets(cfg, ownerOK)
		if err == nil {
			t.Fatalf("key mode %04o accepted", m)
		}
		if strings.Contains(err.Error(), testOpenAIKey) || strings.Contains(err.Error(), e.KeyFile) || strings.Contains(err.Error(), e.Dir) {
			t.Fatalf("error leaks key or path: %v", err)
		}
		// New (the start-up path) refuses too.
		if _, err := New(cfg, Options{StateDir: filepath.Join(e.Dir, "st"), KeyOwnerOK: ownerOK}); err == nil {
			t.Fatalf("New started with key mode %04o", m)
		}
	}
	os.Chmod(e.KeyFile, 0o600)
	if _, err := LoadSecrets(cfg, ownerOK); err != nil {
		t.Fatalf("0600 key refused: %v", err)
	}
	// wrong owner through the seam
	if _, err := LoadSecrets(cfg, func(uint32) bool { return false }); err == nil {
		t.Fatal("foreign-owned key accepted")
	}
	// symlink refused, empty file refused
	link := filepath.Join(e.Dir, "k.link")
	os.Symlink(e.KeyFile, link)
	cfg2 := cfg
	cfg2.OpenAI.KeyFile = link
	if _, err := LoadSecrets(cfg2, ownerOK); err == nil {
		t.Fatal("symlinked key accepted")
	}
	os.WriteFile(e.KeyFile, []byte("\n"), 0o600)
	if _, err := LoadSecrets(cfg, ownerOK); err == nil {
		t.Fatal("empty key accepted")
	}
	// the gitlab token file follows the same rule
	e = newTestEnv(t)
	cfg = e.load(t)
	os.Chmod(e.TokFile, 0o644)
	if _, err := LoadSecrets(cfg, ownerOK); err == nil {
		t.Fatal("0644 token accepted")
	}
}
