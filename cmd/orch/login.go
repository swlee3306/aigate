package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aigate/internal/approval/signing"
	"aigate/internal/orch/api"
	"aigate/internal/textsafe"
)

// SSH challenge login (`orch login`, and the remote client's automatic login).
//
// Token file (--token-file, mode 0600, owned by the user), small JSON:
//
//	{"version":1,"token":"<bearer token>","expires":"<RFC 3339 UTC>",
//	 "signer":"<signer id>","sign_key":"<local private key path>"}
//
// signer and sign_key are local conveniences so a later automatic login uses
// the same identity; neither the key path nor the key is ever sent to the
// server (only the signer id, the challenge id and the signature are). A file
// that is not JSON is a legacy plain token (no known expiry: used until the
// server answers 401).

const (
	tokenFileVersion = 1
	// tokenSkew: a token expiring within this margin counts as expired.
	tokenSkew = 30 * time.Second
	// loginIssuedSkew bounds how far a challenge's issued time may be from
	// the local clock.
	loginIssuedSkew = 5 * time.Minute
)

// tokenFileData is the token file's JSON.
type tokenFileData struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	Expires string `json:"expires"`
	Signer  string `json:"signer,omitempty"`
	SignKey string `json:"sign_key,omitempty"`
}

// errNoUsableToken: the token file is missing, expired or unreadable as a
// token; a login (automatic or `orch login`) fixes it. Permission or owner
// problems are different errors and never trigger a login.
var errNoUsableToken = errors.New("사용할 수 있는 토큰이 없습니다")

func isNoUsableToken(err error) bool { return errors.Is(err, errNoUsableToken) }

// validToken reports whether tok can go into an Authorization header.
func validToken(tok string) bool {
	if tok == "" || len(tok) > 512 {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= 0x20 || tok[i] >= 0x7f {
			return false
		}
	}
	return true
}

// loginOpts are the login identity flags.
type loginOpts struct {
	Signer  string // --signer; "" means $AIGATE_SIGNER, then the token file's
	KeyPath string // --sign-key; "" means the token file's, then defaultSignKey()
}

// resolve fills the signer and key from flags, environment and the stored
// token file (in that order) and checks the key file exists (it is never
// read here: ssh-keygen reads it).
func (o loginOpts) resolve(stored tokenFileData) (signer, key string, err error) {
	signer = o.Signer
	if signer == "" {
		signer = os.Getenv("AIGATE_SIGNER")
	}
	if signer == "" {
		signer = stored.Signer
	}
	if signer == "" {
		return "", "", errors.New("서명자 ID가 없습니다. --signer ID 또는 AIGATE_SIGNER로 지정하세요(`orch admin signers list`의 ID)")
	}
	if !signing.ValidSignerID(signer) {
		return "", "", fmt.Errorf("서명자 ID 형식이 올바르지 않습니다: %s", textsafe.Escape(signer))
	}
	key = o.KeyPath
	if key == "" {
		key = stored.SignKey
	}
	if key == "" {
		key = defaultSignKey()
	}
	if _, err := os.Stat(key); err != nil {
		return "", "", fmt.Errorf("서명 키 파일이 없습니다: %s (--sign-key로 지정)", textsafe.Escape(key))
	}
	return signer, key, nil
}

// loginRefusals are fixed texts for the login endpoints' error codes (CF1:
// the server's own message is never printed).
var loginRefusals = map[string]string{
	"bad_challenge":       "로그인 챌린지가 만료되었거나 이미 쓰였습니다. 다시 실행하세요",
	"bad_signature":       "로그인 서명이 검증되지 않았습니다. --signer의 ID와 --sign-key의 키가 서버 서명자 목록에 함께 등록된 것인지 확인하세요",
	"busy":                "서버에 진행 중인 로그인 요청이 너무 많습니다. 잠시 뒤 다시 시도하세요",
	"signers_unavailable": "서버의 서명자 목록을 신뢰할 수 없거나 없어서 로그인할 수 없습니다(관리자 확인 필요)",
	"bad_request":         "서버가 로그인 요청을 거부했습니다(형식 오류)",
}

func loginRefused(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		if msg, ok := loginRefusals[ae.Code]; ok {
			return errors.New("로그인 실패: " + msg)
		}
		return fmt.Errorf("로그인 실패: 서버가 로그인을 거부했습니다(HTTP %d)", ae.Status)
	}
	return fmt.Errorf("로그인 실패: %w", err)
}

// checkLoginMessage accepts only the exact login message for signer: magic
// line, our own signer id, a 64-hex nonce and an issued time (UTC, seconds)
// within loginIssuedSkew of now, every line ending in "\n" and nothing else.
// A hostile server thus cannot get anything else signed, even though the
// login namespace already keeps login signatures from counting as approvals.
func checkLoginMessage(msg, signer string, now time.Time) error {
	bad := errors.New("서버가 준 로그인 메시지가 올바른 형식이 아닙니다. 서명하지 않았습니다")
	rest, ok := strings.CutPrefix(msg, api.LoginMagic+"\nsigner: "+signer+"\nnonce: ")
	if !ok {
		return bad
	}
	nonce, rest, ok := strings.Cut(rest, "\nissued: ")
	if !ok || len(nonce) != 64 || strings.Trim(nonce, "0123456789abcdef") != "" {
		return bad
	}
	issuedText, ok := strings.CutSuffix(rest, "\n")
	if !ok {
		return bad
	}
	issued, err := time.Parse(time.RFC3339, issuedText)
	if err != nil || api.LoginMessage(signer, nonce, issued) != msg {
		return bad
	}
	if d := now.Sub(issued); d > loginIssuedSkew || d < -loginIssuedSkew {
		return errors.New("서버가 준 로그인 메시지의 시각이 이 컴퓨터 시계와 5분 넘게 다릅니다. 서명하지 않았습니다")
	}
	return nil
}

// doLogin runs the challenge, sign, token exchange against c's server. The
// requests carry no Authorization header. It prints one line naming the
// signer to out.
func doLogin(c *client, signer, key string, out io.Writer) (tok string, expires time.Time, err error) {
	raw, err := c.send("POST", "/auth/challenge", map[string]string{"signer": signer}, "")
	if err != nil {
		return "", time.Time{}, loginRefused(err)
	}
	var ch struct {
		ChallengeID string `json:"challenge_id"`
		Message     string `json:"message"`
	}
	if json.Unmarshal(raw, &ch) != nil || !validToken(ch.ChallengeID) {
		return "", time.Time{}, errors.New("로그인 실패: 서버의 챌린지 응답이 올바르지 않습니다")
	}
	if err := checkLoginMessage(ch.Message, signer, time.Now()); err != nil {
		return "", time.Time{}, fmt.Errorf("로그인 실패: %w", err)
	}
	fmt.Fprintf(out, "로그인: 서명자 %s\n", textsafe.Escape(signer))
	sig, err := signCmd(key, signing.NamespaceLogin, []byte(ch.Message))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("로그인 실패: %w", err)
	}
	raw, err = c.send("POST", "/auth/token", map[string]string{"challenge_id": ch.ChallengeID, "sig": sig}, "")
	if err != nil {
		return "", time.Time{}, loginRefused(err)
	}
	var tr struct {
		Token   string `json:"token"`
		Expires string `json:"expires"`
	}
	if json.Unmarshal(raw, &tr) != nil || !validToken(tr.Token) {
		return "", time.Time{}, errors.New("로그인 실패: 서버의 토큰 응답이 올바르지 않습니다")
	}
	expires, err = time.Parse(time.RFC3339, tr.Expires)
	if err != nil {
		return "", time.Time{}, errors.New("로그인 실패: 서버의 토큰 응답이 올바르지 않습니다")
	}
	return tr.Token, expires.UTC(), nil
}

// readTokenFile reads the token file. A missing, expired (within tokenSkew)
// or malformed file is errNoUsableToken, returned with whatever the file
// held (for the stored signer and key). Mode, owner and file type are
// checked first and are hard errors.
func readTokenFile(path string, now time.Time) (tokenFileData, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return tokenFileData{}, fmt.Errorf("%w: 토큰 파일이 없습니다: %s", errNoUsableToken, path)
	}
	if err := checkTokenFileInfo(path, fi, err); err != nil {
		return tokenFileData{}, err
	}
	b, err := readSmall(path, 64<<10)
	if err != nil {
		return tokenFileData{}, fmt.Errorf("토큰 파일을 읽을 수 없습니다: %s (%v)", path, err)
	}
	text := strings.TrimSpace(string(b))
	if !strings.HasPrefix(text, "{") {
		if !validToken(text) {
			return tokenFileData{}, fmt.Errorf("%w: 토큰 파일이 비어 있거나 형식이 틀립니다: %s", errNoUsableToken, path)
		}
		return tokenFileData{Token: text}, nil // legacy plain token
	}
	var d tokenFileData
	if json.Unmarshal([]byte(text), &d) != nil {
		return tokenFileData{}, fmt.Errorf("%w: 토큰 파일 형식이 틀립니다: %s", errNoUsableToken, path)
	}
	exp, err := time.Parse(time.RFC3339, d.Expires)
	switch {
	case d.Version != tokenFileVersion || !validToken(d.Token) || err != nil:
		return d, fmt.Errorf("%w: 토큰 파일 형식이 틀립니다: %s", errNoUsableToken, path)
	case !now.Add(tokenSkew).Before(exp):
		return d, fmt.Errorf("%w: 토큰이 만료되었습니다: %s", errNoUsableToken, path)
	}
	return d, nil
}

// checkTokenFileInfo: a regular file (not a symlink), mode 0600, owned by the
// current user, since anyone who can read it can act as the user.
func checkTokenFileInfo(path string, fi os.FileInfo, err error) error {
	if err != nil {
		return fmt.Errorf("토큰 파일을 읽을 수 없습니다: %s (%v)", path, errors.Unwrap(err))
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("토큰 파일이 심볼릭 링크입니다: %s — 일반 파일만 씁니다", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("토큰 파일이 일반 파일이 아닙니다: %s", path)
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("토큰 파일 권한이 %04o입니다: %s — `chmod 600 %s`로 고치세요", fi.Mode().Perm(), path, path)
	}
	if uid, ok := fileUID(fi); ok && uid != os.Geteuid() {
		return fmt.Errorf("토큰 파일의 소유자가 현재 사용자가 아닙니다: %s — 소유자를 고치세요", path)
	}
	return nil
}

// writeTokenFile replaces path atomically with d: a symlink (or any other
// non-regular file) at path is refused; the new file is created with O_EXCL
// in the same directory at mode 0600, synced, then renamed over path. A
// missing directory is created 0700.
func writeTokenFile(path string, d tokenFileData) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("토큰 폴더를 만들 수 없습니다: %s (%v)", dir, errors.Unwrap(err))
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("토큰 파일 자리가 심볼릭 링크라 쓰지 않았습니다: %s", path)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("토큰 파일 자리가 일반 파일이 아니라 쓰지 않았습니다: %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("토큰 파일을 확인할 수 없습니다: %s (%v)", path, errors.Unwrap(err))
	}
	d.Version = tokenFileVersion
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".orch-token-*.tmp") // O_CREATE|O_EXCL, 0600
	if err != nil {
		return fmt.Errorf("토큰 파일을 쓸 수 없습니다: %s (%v)", dir, errors.Unwrap(err))
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("토큰 파일을 쓸 수 없습니다: %s (%v)", path, errors.Unwrap(err))
	}
	ok = true
	return nil
}

// autoLogin is the remote client's token supplier: the token file's token
// while it is valid, otherwise a fresh SSH challenge login (saved back to the
// file). relogin forces a login; client.call uses it once after a 401.
type autoLogin struct {
	c      *client
	path   string
	read   func() (string, error) // tokenSource(path)
	opts   loginOpts
	stderr io.Writer
}

func (a *autoLogin) token() (string, error) {
	tok, err := a.read()
	if isNoUsableToken(err) {
		return a.relogin()
	}
	return tok, err
}

func (a *autoLogin) relogin() (string, error) {
	stored, _ := readTokenFile(a.path, time.Now()) // best effort: signer and key only
	signer, key, err := a.opts.resolve(stored)
	if err != nil {
		return "", fmt.Errorf("토큰이 없거나 만료되어 로그인해야 하지만 할 수 없습니다: %w", err)
	}
	tok, exp, err := doLogin(a.c, signer, key, a.stderr)
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(a.path, tokenFileData{Token: tok, Expires: exp.Format(time.RFC3339), Signer: signer, SignKey: key}); err != nil {
		fmt.Fprintf(a.stderr, "orch: 경고: 새 토큰을 저장하지 못했습니다: %s\n", errText(err))
	}
	return tok, nil
}

// cmdLogin is `orch login [--server URL] [--token-file PATH] [--sign-key PATH]
// [--signer ID]`. Global flags given before the command are the defaults.
func cmdLogin(args []string, server, tokenFile string, opts loginOpts, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&server, "server", server, "")
	fs.StringVar(&tokenFile, "token-file", tokenFile, "")
	fs.StringVar(&opts.KeyPath, "sign-key", opts.KeyPath, "")
	fs.StringVar(&opts.Signer, "signer", opts.Signer, "")
	fail := func(err error) int { fmt.Fprintln(stderr, "오류:", errText(err)); return 1 }
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "사용법: orch login --server URL [--token-file PATH] [--sign-key KEY] [--signer ID]")
		return 2
	}
	if server == "" {
		return fail(errors.New("login에는 --server https://주소 가 필요합니다"))
	}
	if tokenFile == "" {
		tokenFile = defaultTokenFile()
	}
	stored, err := readTokenFile(tokenFile, time.Now())
	if err != nil && !isNoUsableToken(err) {
		stored = tokenFileData{}
	}
	signer, key, err := opts.resolve(stored)
	if err != nil {
		return fail(err)
	}
	c := newRemoteClient(server, nil)
	if c.err != nil {
		return fail(c.err)
	}
	tok, exp, err := doLogin(c, signer, key, stdout)
	if err != nil {
		return fail(err)
	}
	if err := writeTokenFile(tokenFile, tokenFileData{Token: tok, Expires: exp.Format(time.RFC3339), Signer: signer, SignKey: key}); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "토큰을 %s 에 저장했습니다(만료 %s)\n", tokenFile, exp.Local().Format(time.DateTime))
	return 0
}
