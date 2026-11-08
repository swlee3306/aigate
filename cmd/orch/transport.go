package main

import (
	"context"
	"errors"
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

// tokenSource builds the bearer-token supplier for a --token-file path. It is a
// seam: the SSH-key login (later task) replaces it with one that can also mint
// a token when the file is missing.
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
	c := &client{token: token}
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

// fileToken reads a bearer token from path. The file must be mode 0600 and owned
// by the current user, since anyone who can read it can act as the user. The
// token itself never appears in an error.
func fileToken(path string) func() (string, error) {
	return func() (string, error) {
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("토큰 파일을 읽을 수 없습니다: %s (%v)", path, errors.Unwrap(err))
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("토큰 파일이 일반 파일이 아닙니다: %s", path)
		}
		if fi.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("토큰 파일 권한이 %04o입니다: %s — `chmod 600 %s`로 고치세요", fi.Mode().Perm(), path, path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			return "", fmt.Errorf("토큰 파일의 소유자가 현재 사용자가 아닙니다: %s — 소유자를 고치세요", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("토큰 파일을 읽을 수 없습니다: %s (%v)", path, errors.Unwrap(err))
		}
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("토큰 파일이 비어 있습니다: %s", path)
		}
		return tok, nil
	}
}

// defaultTokenFile is where --token-file points when --server is used alone.
func defaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "orch-token"
	}
	return home + "/.config/orch/token"
}
