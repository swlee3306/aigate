package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const apiPrefix = "/api/v1"

// tokenSource builds the bearer-token reader for a --token-file path (a seam
// for tests). Its errNoUsableToken (missing or expired token) makes the
// remote client log in (autoLogin, login.go); any other error is final.
var tokenSource = fileToken

// newClient talks to a local orchd over its unix socket (no auth: the socket's
// file permissions and peer uid are the credential).
func newClient(sock string) *client {
	return &client{base: "http://orchd", hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}}
}

// newRemoteClient talks to orchd's HTTPS API with a bearer token. It uses the
// system trust roots and never disables certificate verification. A server URL
// that is not https is refused (plain http is allowed only for loopback, which
// the tests use); the refusal is reported by the first call.
func newRemoteClient(server string, token func() (string, error)) *client {
	c := &client{token: token, remote: true}
	u, err := url.Parse(server)
	switch {
	case err != nil || u.Host == "":
		c.err = fmt.Errorf("--server 주소가 올바르지 않습니다: %q (예: https://orch-vm.tailnet.ts.net)", server)
		return c
	case u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")):
		c.err = fmt.Errorf("--server는 https 주소여야 합니다: %q", server)
		return c
	}
	c.base = strings.TrimRight(u.String(), "/")
	c.hc = &http.Client{
		Timeout:   30 * time.Second,
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
		// A redirect could carry the token somewhere else; the API never redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// fileToken reads the bearer token from the token file (format in login.go).
// The file must be a regular file of mode 0600 owned by the current user. The
// token itself never appears in an error.
func fileToken(path string) func() (string, error) {
	return func() (string, error) {
		d, err := readTokenFile(path, time.Now())
		if err != nil {
			return "", err
		}
		return d.Token, nil
	}
}

// fileUID is the owner uid of fi, when the platform reports it.
func fileUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// defaultTokenFile is where --token-file points when --server is used alone.
func defaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "orch-token"
	}
	return home + "/.config/orch/token"
}
