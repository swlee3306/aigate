package signing

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Signer types.
const (
	TypeSSHEd25519    = "ssh-ed25519"
	TypeWebAuthnES256 = "webauthn-es256"
)

const maxRegistrySize = 1 << 20

// ErrRegistryAbsent is returned (possibly wrapped) only when the registry file
// does not exist. Every other problem is a different error.
var ErrRegistryAbsent = errors.New("서명자 목록 파일이 없습니다")

// Signer is one trusted approver key.
type Signer struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Label        string    `json:"label"`
	PublicKey    string    `json:"public_key"`
	CredentialID string    `json:"credential_id"`
	RPID         string    `json:"rp_id"`
	Origin       string    `json:"origin"`
	AddedAt      time.Time `json:"added_at"`
}

// Registry is the set of trusted signers.
type Registry struct {
	Signers []Signer
}

// RegistryOptions tunes the trust checks. OwnerOK reports whether a file owner
// uid is trusted; nil means uid 0 only.
type RegistryOptions struct {
	OwnerOK func(uid uint32) bool
}

// DefaultOptions is the process-wide option set used by callers that load the
// registry from RegistryPath().
var DefaultOptions RegistryOptions

var (
	pathMu       sync.RWMutex
	registryPath = DefaultRegistryPath
)

// RegistryPath returns the process-wide registry path.
func RegistryPath() string {
	pathMu.RLock()
	defer pathMu.RUnlock()
	return registryPath
}

// SetRegistryPath changes the process-wide registry path.
func SetRegistryPath(path string) {
	pathMu.Lock()
	defer pathMu.Unlock()
	registryPath = path
}

// Find returns the signer with the given id.
func (r Registry) Find(id string) (Signer, bool) {
	for _, s := range r.Signers {
		if s.ID == id {
			return s, true
		}
	}
	return Signer{}, false
}

// LoadRegistry reads and validates the registry at path. It fails closed: any
// doubt about ownership, permissions, file identity or content is an error.
// Ancestors above the parent directory are not checked.
func LoadRegistry(path string, o RegistryOptions) (Registry, error) {
	ownerOK := o.OwnerOK
	if ownerOK == nil {
		ownerOK = func(uid uint32) bool { return uid == 0 }
	}
	// The parent (or, if it does not exist, the nearest existing ancestor) is
	// checked FIRST: "absent" switches signed mode off, so it may only be
	// reported when nobody untrusted could have removed the file.
	dir := filepath.Dir(path)
	absent := false
	di, err := os.Lstat(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Registry{}, fmt.Errorf("서명자 목록 폴더 확인 실패: %w", err)
		}
		di, err = nearestExistingAncestor(dir)
		if err != nil {
			return Registry{}, err
		}
		absent = true
	}
	if !di.IsDir() {
		return Registry{}, fmt.Errorf("서명자 목록 상위 경로가 폴더가 아닙니다(심볼릭 링크 불가): %s", dir)
	}
	if err := checkTrusted(di, "서명자 목록 폴더", ownerOK); err != nil {
		return Registry{}, err
	}
	if absent {
		return Registry{}, fmt.Errorf("%w: %s", ErrRegistryAbsent, path)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Registry{}, fmt.Errorf("%w: %s", ErrRegistryAbsent, path)
		}
		return Registry{}, fmt.Errorf("서명자 목록 확인 실패: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Registry{}, fmt.Errorf("서명자 목록은 일반 파일이어야 합니다(심볼릭 링크 불가): %s", path)
	}
	if err := checkTrusted(fi, "서명자 목록 파일", ownerOK); err != nil {
		return Registry{}, err
	}
	if fi.Size() > maxRegistrySize {
		return Registry{}, fmt.Errorf("서명자 목록이 너무 큽니다(최대 %d바이트)", maxRegistrySize)
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Registry{}, fmt.Errorf("서명자 목록 열기 실패: %w", err)
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil {
		return Registry{}, fmt.Errorf("서명자 목록 확인 실패: %w", err)
	}
	if !os.SameFile(fi, fi2) {
		return Registry{}, errors.New("서명자 목록 파일이 확인 도중 바뀌었습니다")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRegistrySize+1))
	if err != nil {
		return Registry{}, fmt.Errorf("서명자 목록 읽기 실패: %w", err)
	}
	if len(data) > maxRegistrySize {
		return Registry{}, fmt.Errorf("서명자 목록이 너무 큽니다(최대 %d바이트)", maxRegistrySize)
	}
	return parseRegistry(data)
}

// checkTrusted requires no group/other write and a trusted owner.
func checkTrusted(fi os.FileInfo, what string, ownerOK func(uint32) bool) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s 소유자를 확인할 수 없습니다", what)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s에 그룹·기타 쓰기 권한이 있습니다(%04o)", what, fi.Mode().Perm())
	}
	if !ownerOK(st.Uid) {
		return fmt.Errorf("%s의 소유자(uid %d)를 신뢰할 수 없습니다", what, st.Uid)
	}
	return nil
}

type registryFile struct {
	Version int      `json:"version"`
	Signers []Signer `json:"signers"`
}

func parseRegistry(data []byte) (Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rf registryFile
	if err := dec.Decode(&rf); err != nil {
		return Registry{}, fmt.Errorf("서명자 목록 형식 오류: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Registry{}, errors.New("서명자 목록 뒤에 불필요한 데이터가 있습니다")
	}
	if rf.Version != 1 {
		return Registry{}, fmt.Errorf("지원하지 않는 서명자 목록 버전: %d", rf.Version)
	}
	seen := map[string]bool{}
	for _, s := range rf.Signers {
		if err := validateSigner(s); err != nil {
			return Registry{}, fmt.Errorf("서명자 %q: %w", s.ID, err)
		}
		if seen[s.ID] {
			return Registry{}, fmt.Errorf("서명자 id가 중복됩니다: %q", s.ID)
		}
		seen[s.ID] = true
	}
	return Registry{Signers: rf.Signers}, nil
}

var (
	idRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	hostLabel  = `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`
	hostnameRE = regexp.MustCompile(`^` + hostLabel + `(\.` + hostLabel + `)*$`)
)

func validHostname(h string) bool { return len(h) <= 253 && hostnameRE.MatchString(h) }

func validateSigner(s Signer) error {
	if !idRE.MatchString(s.ID) {
		return errors.New("id 형식이 올바르지 않습니다")
	}
	switch s.Type {
	case TypeSSHEd25519:
		if s.CredentialID != "" || s.RPID != "" || s.Origin != "" {
			return errors.New("ssh 서명자에는 credential_id, rp_id, origin을 쓸 수 없습니다")
		}
		return validateSSHKey(s.PublicKey)
	case TypeWebAuthnES256:
		return validateWebAuthn(s)
	default:
		return fmt.Errorf("알 수 없는 type: %q", s.Type)
	}
}

func validateSSHKey(line string) error {
	if line == "" || strings.ContainsAny(line, "\r\n") || strings.TrimSpace(line) != line {
		return errors.New("public_key는 앞뒤 공백·줄바꿈 없는 한 줄이어야 합니다")
	}
	pub, _, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return fmt.Errorf("public_key를 해석할 수 없습니다: %w", err)
	}
	if len(opts) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return errors.New("public_key에 옵션이나 불필요한 내용이 있습니다")
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("public_key는 ed25519여야 합니다(%s)", pub.Type())
	}
	return nil
}

var rawURL = base64.RawURLEncoding.Strict()

func validateWebAuthn(s Signer) error {
	raw, err := rawURL.DecodeString(s.PublicKey)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		return errors.New("public_key는 비압축 P-256 점(65바이트, base64url)이어야 합니다")
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return errors.New("public_key가 P-256 곡선 위의 점이 아닙니다")
	}
	cred, err := rawURL.DecodeString(s.CredentialID)
	if err != nil || len(cred) == 0 || len(cred) > 1023 {
		return errors.New("credential_id는 1~1023바이트의 base64url이어야 합니다")
	}
	if !validHostname(s.RPID) {
		return errors.New("rp_id는 소문자 호스트 이름이어야 합니다")
	}
	return validateOrigin(s.Origin)
}

func validateOrigin(origin string) error {
	bad := errors.New("origin은 https://호스트[:포트] 형식이어야 합니다")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return bad
	}
	if !validHostname(u.Hostname()) || origin != "https://"+u.Host {
		return bad
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		// 443 is refused too: browsers send the origin without the default
		// port, so such an entry could never match.
		if err != nil || n < 1 || n > 65535 || n == 443 || strconv.Itoa(n) != p {
			return bad
		}
	}
	return nil
}

// nearestExistingAncestor walks up from a missing directory to the first
// ancestor that exists. Any error other than "does not exist" is returned.
func nearestExistingAncestor(dir string) (os.FileInfo, error) {
	cur := dir
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil, fmt.Errorf("서명자 목록의 상위 폴더를 찾을 수 없습니다: %s", dir)
		}
		fi, err := os.Lstat(parent)
		if err == nil {
			return fi, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("서명자 목록 폴더 확인 실패: %w", err)
		}
		cur = parent
	}
}

// ValidateSigner applies the registry's per-entry rules (id format, type,
// key encoding, WebAuthn rp_id/origin) to one signer. Writers (orch admin
// signers) use it so that what they write is what LoadRegistry accepts.
func ValidateSigner(s Signer) error { return validateSigner(s) }

// Fingerprint names a signer's public key for humans: "SHA256:" and the
// unpadded standard base64 of the SHA-256 of the key bytes (the SSH wire
// form for ssh-ed25519, the same as `ssh-keygen -l`; the 65-byte uncompressed
// P-256 point for webauthn-es256). The signer must be valid.
func Fingerprint(s Signer) (string, error) {
	if err := validateSigner(s); err != nil {
		return "", err
	}
	if s.Type == TypeSSHEd25519 {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.PublicKey))
		if err != nil {
			return "", err
		}
		return ssh.FingerprintSHA256(pub), nil
	}
	raw, err := rawURL.DecodeString(s.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}
