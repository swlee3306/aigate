package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProtectionWeakerIsDistinctCode(t *testing.T) {
	if errors.Is(ErrProtectionWeaker, ErrGitLabRefused) || errors.Is(ErrGitLabRefused, ErrProtectionWeaker) {
		t.Fatal("ErrProtectionWeaker matches plain gitlab_refused")
	}
	if errors.Is(&CodeError{Code: CodeGitLabRefused, Status: 403}, ErrProtectionWeaker) {
		t.Fatal("a 403 matches ErrProtectionWeaker")
	}
	if WireCode(ErrProtectionWeaker) != CodeGitLabRefused || WireCode(ErrNotFound) != CodeNotFound || WireCode(nil) != "" {
		t.Fatalf("wire code: %q", WireCode(ErrProtectionWeaker))
	}
}

func TestPlainHTTPOnlyForPrivateLiterals(t *testing.T) {
	for _, s := range []string{"http://10.0.0.5", "http://172.16.0.1:8080", "http://192.168.1.1", "http://100.64.0.10:18082", "http://100.64.0.1",
		"http://[fd00::1]", "http://[fc00::1]:8080", "https://gitlab.example.com", "https://8.8.8.8", "https://gitlab", "https://[2001:db8::1]", "https://10.0.0.5"} {
		if err := ValidateProviderURL(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	for _, s := range []string{"http://gitlab.example.com", "http://gitlab", "http://8.8.8.8", "http://[2001:db8::1]", "http://gitlab.corp:8080", "http://100.128.0.1"} {
		if ErrCode(ValidateProviderURL(s)) != CodeBadURL {
			t.Errorf("%q accepted", s)
		}
	}
	// the stored-provider path uses the same rule
	p := goodProvider("x")
	p.BaseURL = "http://gitlab.example.com"
	if err := providerShape(p, defaultIPPolicy, true); ErrCode(err) != CodeBadURL {
		t.Errorf("providerShape accepted plain http to a host name: %v", err)
	}
}

func TestIPRulesFix1(t *testing.T) {
	for _, s := range []string{"192.0.0.1", "192.0.0.170", "198.18.0.1", "198.19.255.255", "fec0::1", "feff::1", "::ffff:0:7f00:1", "::ffff:0:127.0.0.1", "::ffff:0:169.254.169.254", "::ffff:0:0.0.0.0"} {
		if !ipForbidden(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"198.17.255.255", "198.20.0.1", "192.0.1.1", "192.0.2.1", "::ffff:0:10.1.1.1", "fd00::1"} {
		if ipForbidden(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{"http://192.0.0.8", "http://198.18.0.5", "http://[fec0::1]", "http://[::ffff:0:7f00:1]"} {
		if ErrCode(ValidateProviderURL(s)) != CodeBadURL {
			t.Errorf("%s accepted", s)
		}
	}
}

func TestProtectedBranchScopedEntriesAreWeaker(t *testing.T) {
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	for _, c := range []struct {
		push, merge string
		weak        bool
	}{
		{`{"access_level":40}`, `{"access_level":40}`, false},
		{`{"access_level":40,"user_id":null,"group_id":null,"deploy_key_id":null}`, `{"access_level":40,"user_id":0}`, false},
		{`{"access_level":40,"user_id":5}`, `{"access_level":40}`, true},
		{`{"access_level":40,"group_id":3}`, `{"access_level":40}`, true},
		{`{"access_level":40,"deploy_key_id":9}`, `{"access_level":40}`, true},
		{`{"access_level":40}`, `{"access_level":40,"user_id":5}`, true},
		{`{"access_level":40}`, `{"access_level":40,"group_id":3}`, true},
		{`{"access_level":40}`, `{"access_level":40,"deploy_key_id":1}`, true},
	} {
		f.override = func(w http.ResponseWriter, r *http.Request) bool {
			fmt.Fprintf(w, `{"name":"main","allow_force_push":false,"push_access_levels":[%s],"merge_access_levels":[%s]}`, c.push, c.merge)
			return true
		}
		n := f.hits()
		err := a.EnsureProtectedMain(context.Background(), 21)
		if c.weak != errors.Is(err, ErrProtectionWeaker) || (!c.weak && err != nil) || f.hits() != n+1 {
			t.Errorf("%s / %s: err=%v hits=%d", c.push, c.merge, err, f.hits()-n)
		}
	}
}

func TestDialPolicyRefusalIsBadURL(t *testing.T) {
	f := newFakeGL(t)
	ts := apiTokens(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(f.URL, "http://"), ":")
	prov := Provider{ID: "gl", Kind: "gitlab", BaseURL: "http://gitlab.example.com:" + port, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}
	look := func(ip string) func(context.Context, string) ([]netip.Addr, error) {
		return func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
	}
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "::1"} {
		a, err := newGitLabAPI(prov, ts, &apiSeams{lookup: look(ip), plainHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Group(context.Background(), "team"); ErrCode(err) != CodeBadURL {
			t.Errorf("%s: %v", ip, err)
		}
	}
	// the Control hook alone (literal address, policy flips after construction)
	var deny atomic.Bool
	a, _ := newGitLabAPI(Provider{ID: "gl", Kind: "gitlab", BaseURL: f.URL, SSHURL: "ssh://git@10.0.0.1", TokenRef: "gl-admin"}, ts,
		&apiSeams{policy: func(ad netip.Addr) bool { return !deny.Load() && allowLoopback(ad) }})
	deny.Store(true)
	if _, err := a.Group(context.Background(), "team"); ErrCode(err) != CodeBadURL {
		t.Errorf("literal: %v", err)
	}
	// ordinary connect failure stays provider_unreachable
	dead := newFakeGL(t)
	b := apiFor(t, dead, ts, nil)
	dead.Close()
	if _, err := b.Group(context.Background(), "team"); ErrCode(err) != CodeProviderUnreachable {
		t.Errorf("closed server: %v", err)
	}
	// resolver failure stays provider_unreachable
	c, _ := newGitLabAPI(prov, ts, &apiSeams{plainHTTP: true, lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("nx") }})
	if _, err := c.Group(context.Background(), "team"); ErrCode(err) != CodeProviderUnreachable {
		t.Errorf("lookup failure: %v", err)
	}
}

func TestEnsureProtectedMainRetriesAfterConflict(t *testing.T) {
	strong := `{"name":"main","allow_force_push":false,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`
	weak := `{"name":"main","allow_force_push":true,"push_access_levels":[{"access_level":40}],"merge_access_levels":[{"access_level":40}]}`
	for _, post := range []int{409, 422} {
		for _, c := range []struct {
			second string // body of the 2nd GET ("" = 404)
			want   string // "ok", "weaker", "refused"
		}{{strong, "ok"}, {weak, "weaker"}, {"", "refused"}} {
			f := newFakeGL(t)
			a := apiFor(t, f, apiTokens(t), nil)
			var gets atomic.Int32
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == "GET" {
					if gets.Add(1) == 1 || c.second == "" {
						w.WriteHeader(404)
						return true
					}
					fmt.Fprint(w, c.second)
					return true
				}
				w.WriteHeader(post)
				return true
			}
			err := a.EnsureProtectedMain(context.Background(), 21)
			switch c.want {
			case "ok":
				if err != nil {
					t.Errorf("%d/ok: %v", post, err)
				}
			case "weaker":
				if !errors.Is(err, ErrProtectionWeaker) {
					t.Errorf("%d/weaker: %v", post, err)
				}
			default:
				if err == nil || errors.Is(err, ErrProtectionWeaker) || ErrCode(err) != CodeGitLabRefused {
					t.Errorf("%d/refused: %v", post, err)
				}
			}
			if gets.Load() != 2 || len(f.recorded()) != 3 {
				t.Errorf("%d/%s: requests %v (want GET, POST, GET)", post, c.want, f.recorded())
			}
		}
	}
	// other POST failures are not retried
	f := newFakeGL(t)
	a := apiFor(t, f, apiTokens(t), nil)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" {
			w.WriteHeader(404)
		} else {
			w.WriteHeader(403)
		}
		return true
	}
	if err := a.EnsureProtectedMain(context.Background(), 21); ErrCode(err) != CodeGitLabRefused || len(f.recorded()) != 2 {
		t.Errorf("403: %v %v", err, f.recorded())
	}
}
