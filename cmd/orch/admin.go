package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"aigate/internal/approval/signing"
	"aigate/internal/textsafe"
)

// `orch admin signers ...` manages the signers registry, the root of trust
// for signed approvals (design 8.4, R2, R3). It runs locally (no daemon):
// reads are open to anyone, writes need euid 0 and are refused before any
// file is touched. The registry is replaced atomically: a temp file in the
// same directory, mode 0644, fsynced, then renamed over the old one.
//
// Pending passkey enrollments (Task 19 writes them, the web console cannot
// activate them) live in a JSON file, by default defaultPendingFile:
//
//	{"version":1,"pending":[{"id":"phone","label":"...","credential_id":"<b64url>",
//	  "public_key":"<b64url 65-byte P-256 point>","rp_id":"host","origin":"https://host",
//	  "created":"<RFC 3339>"}]}
//
// That file is writable by orchd, so it is untrusted input: `add --pending ID`
// validates the entry with the registry's own rules and requires the
// fingerprint the administrator compared out of band (--fingerprint). It
// never writes the pending file (root writing into orchd's directory would
// leave a file orchd cannot rewrite); orchd prunes entries whose id is in
// the registry.

const adminUsage = `관리(로컬, 쓰기는 root만):
  admin signers list    [--signers-file F]                               서명자 목록(ID, 종류, 라벨, 키 지문)
  admin signers add     [--signers-file F] --id ID --key KEY.pub [--label L]   ssh-ed25519 공개 키 등록
  admin signers add     [--signers-file F] --pending ID --fingerprint FP [--pending-file P] [--id ID] [--label L]
                                                                         웹에서 등록한 대기 패스키 활성화(지문 확인 필수)
  admin signers remove  [--signers-file F] ID                            서명자 제거
  admin signers pending [--pending-file P]                               대기 중인 패스키 목록
  서명자 목록 기본 위치: ` + signing.DefaultRegistryPath + `, 대기 목록 기본 위치: ` + defaultPendingFile

// defaultPendingFile is where orchd records pending passkey enrollments (next
// to its database, a directory orchd can write).
const defaultPendingFile = "/var/lib/orchd/signers.pending.json"

// geteuid is a seam: tests cannot run as root.
var geteuid = os.Geteuid

const maxKeyFile = 16 << 10

var rawURLStrict = base64.RawURLEncoding.Strict()

type pendingFile struct {
	Version int                 `json:"version"`
	Pending []pendingCredential `json:"pending"`
}

// pendingCredential is one passkey registered on the web and waiting for an
// administrator. RPID and Origin are the server's at registration (R8).
type pendingCredential struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	CredentialID string    `json:"credential_id"`
	PublicKey    string    `json:"public_key"`
	RPID         string    `json:"rp_id"`
	Origin       string    `json:"origin"`
	Created      time.Time `json:"created"`
}

func (p pendingCredential) signer() signing.Signer {
	return signing.Signer{ID: p.ID, Type: signing.TypeWebAuthnES256, Label: p.Label,
		PublicKey: p.PublicKey, CredentialID: p.CredentialID, RPID: p.RPID, Origin: p.Origin}
}

// registryOut mirrors signing's registry file format ({"version":1,...}).
type registryOut struct {
	Version int              `json:"version"`
	Signers []signing.Signer `json:"signers"`
}

func cmdAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "signers" {
		fmt.Fprintln(stderr, adminUsage)
		return 2
	}
	sub, rest := args[1], args[2:]
	fs := flag.NewFlagSet("signers", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regPath := fs.String("signers-file", signing.RegistryPath(), "")
	pendPath := fs.String("pending-file", defaultPendingFile, "")
	id := fs.String("id", "", "")
	label := fs.String("label", "", "")
	keyFile := fs.String("key", "", "")
	pendingID := fs.String("pending", "", "")
	fingerprint := fs.String("fingerprint", "", "")
	pos, err := parseInterleaved(fs, rest)
	if err != nil {
		fmt.Fprintf(stderr, "오류: %v\n%s\n", err, adminUsage)
		return 2
	}
	write := sub == "add" || sub == "remove"
	if write && geteuid() != 0 {
		fmt.Fprintf(stderr, "orch: admin signers %s 거부: 서명자 목록은 root만 바꿀 수 있습니다. `sudo orch admin signers %s ...`로 실행하세요. 변경 없음\n", sub, sub)
		return 1
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "orch: admin signers "+sub+": "+format+"\n", a...)
		return 1
	}
	switch sub {
	case "list":
		return signersList(*regPath, stdout, stderr)
	case "pending":
		ps, err := readPending(*pendPath)
		if err != nil {
			return fail("%v", err)
		}
		if len(ps) == 0 {
			fmt.Fprintln(stdout, "대기 중인 패스키가 없습니다.")
			return 0
		}
		fmt.Fprintf(stdout, "%-16s %-20s %-46s %-19s %-24s %s\n", "ID", "라벨", "지문", "등록", "rp_id", "origin")
		for _, p := range ps {
			fp, err := signing.Fingerprint(p.signer())
			if err != nil {
				fp = "(형식 오류: 활성화할 수 없음)"
			}
			fmt.Fprintf(stdout, "%-16s %-20s %-46s %-19s %-24s %s\n", textsafe.Escape(p.ID), textsafe.Escape(p.Label), fp,
				p.Created.UTC().Format(time.DateTime), textsafe.Escape(p.RPID), textsafe.Escape(p.Origin))
		}
		return 0
	case "add":
		var s signing.Signer
		switch {
		case *keyFile != "" && *pendingID != "":
			return fail("--key와 --pending은 함께 쓸 수 없습니다")
		case *keyFile != "":
			if *id == "" {
				return fail("--id가 필요합니다")
			}
			line, comment, err := readEd25519(*keyFile)
			if err != nil {
				return fail("%v", err)
			}
			s = signing.Signer{ID: *id, Type: signing.TypeSSHEd25519, Label: *label, PublicKey: line}
			if s.Label == "" && comment != "" {
				if _, bad := textsafe.FirstUnsafe(comment); !bad && len(comment) <= 64 {
					s.Label = comment
				}
			}
		case *pendingID != "":
			if *fingerprint == "" {
				return fail("--fingerprint가 필요합니다: `orch admin signers pending`에서 본 지문을 등록한 사람과 직접 대조한 뒤 넣으세요")
			}
			ps, err := readPending(*pendPath)
			if err != nil {
				return fail("%v", err)
			}
			var p *pendingCredential
			for i := range ps {
				if ps[i].ID == *pendingID {
					p = &ps[i]
				}
			}
			if p == nil {
				return fail("대기 중인 패스키 %q가 없습니다", *pendingID)
			}
			s = p.signer()
			if *id != "" {
				s.ID = *id
			}
			if *label != "" {
				s.Label = *label
			}
			fp, err := signing.Fingerprint(s)
			if err != nil {
				return fail("대기 항목이 올바르지 않습니다: %v", err)
			}
			if fp != *fingerprint {
				return fail("지문이 다릅니다(대기 항목 %s, 입력 %s). 활성화하지 않음", fp, textsafe.Escape(*fingerprint))
			}
		default:
			return fail("--key KEY.pub 또는 --pending ID가 필요합니다")
		}
		s.AddedAt = time.Now().UTC().Truncate(time.Second)
		if err := signing.ValidateSigner(s); err != nil {
			return fail("서명자가 올바르지 않습니다: %v", err)
		}
		return signersAdd(*regPath, s, stdout, stderr)
	case "remove":
		rid := *id
		if rid == "" && len(pos) == 1 {
			rid = pos[0]
		}
		if rid == "" {
			return fail("제거할 서명자 ID 하나가 필요합니다")
		}
		return signersRemove(*regPath, rid, stdout, stderr)
	}
	fmt.Fprintf(stderr, "알 수 없는 명령: admin signers %s\n%s\n", sub, adminUsage)
	return 2
}

// parseInterleaved parses flags that may follow positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// trustedForWrite is LoadRegistry's ownership rule for the writer: root, or
// the invoking user (equal to root in production, where writes run as root;
// lets tests and a non-root dry run read their own files).
func trustedForWrite() signing.RegistryOptions {
	me := uint32(os.Geteuid())
	return signing.RegistryOptions{OwnerOK: func(uid uint32) bool { return uid == 0 || uid == me }}
}

// loadForWrite reads the current registry; absent is an empty one. Any other
// problem (permissions, ownership, format) refuses the write: the
// administrator must fix the file or directory first.
func loadForWrite(path string) (signing.Registry, error) {
	r, err := signing.LoadRegistry(path, trustedForWrite())
	if errors.Is(err, signing.ErrRegistryAbsent) {
		return signing.Registry{}, nil
	}
	if err != nil {
		return signing.Registry{}, fmt.Errorf("현재 서명자 목록을 신뢰할 수 없어 쓰지 않습니다: %v", err)
	}
	return r, nil
}

func signersList(path string, stdout, stderr io.Writer) int {
	r, err := signing.LoadRegistry(path, signing.DefaultOptions)
	if errors.Is(err, signing.ErrRegistryAbsent) {
		fmt.Fprintf(stdout, "서명자 목록이 없습니다(%s): 서명 필수 모드가 꺼져 있습니다.\n", path)
		return 0
	}
	if err != nil {
		// Show what is there, with the reason the daemon and gate refuse it.
		r2, err2 := signing.LoadRegistry(path, trustedForWrite())
		if errors.Is(err2, signing.ErrRegistryAbsent) {
			fmt.Fprintf(stderr, "경고: 서명자 목록 폴더를 신뢰할 수 없습니다(서명 승인 모두 거부): %v\n", err)
			fmt.Fprintf(stdout, "서명자 목록이 없습니다(%s).\n", path)
			return 0
		}
		if err2 != nil {
			fmt.Fprintf(stderr, "orch: admin signers list: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "경고: 이 목록은 서명 필수 모드에서 신뢰되지 않습니다(서명 승인 모두 거부): %v\n", err)
		r = r2
	}
	if len(r.Signers) == 0 {
		fmt.Fprintln(stdout, "등록된 서명자가 없습니다(서명 필수 모드: 어떤 승인도 받을 수 없음).")
		return 0
	}
	fmt.Fprintf(stdout, "%-16s %-15s %-20s %-46s %s\n", "ID", "종류", "라벨", "지문", "origin")
	for _, s := range r.Signers {
		fp, _ := signing.Fingerprint(s)
		fmt.Fprintf(stdout, "%-16s %-15s %-20s %-46s %s\n", s.ID, s.Type, textsafe.Escape(s.Label), fp, textsafe.Escape(s.Origin))
	}
	return 0
}

// sameKey compares decoded key material (CF2), never encoded strings.
func sameKey(a, b signing.Signer) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Type == signing.TypeSSHEd25519 {
		pa, _, _, _, ea := ssh.ParseAuthorizedKey([]byte(a.PublicKey))
		pb, _, _, _, eb := ssh.ParseAuthorizedKey([]byte(b.PublicKey))
		return ea == nil && eb == nil && bytes.Equal(pa.Marshal(), pb.Marshal())
	}
	dec := func(s string) []byte { b, _ := rawURLStrict.DecodeString(s); return b }
	return bytes.Equal(dec(a.PublicKey), dec(b.PublicKey)) && bytes.Equal(dec(a.CredentialID), dec(b.CredentialID)) &&
		a.RPID == b.RPID && a.Origin == b.Origin
}

func signersAdd(path string, s signing.Signer, stdout, stderr io.Writer) int {
	r, err := loadForWrite(path)
	if err != nil {
		fmt.Fprintf(stderr, "orch: admin signers add: %v\n", err)
		return 1
	}
	fp, _ := signing.Fingerprint(s)
	for _, cur := range r.Signers {
		switch {
		case cur.ID == s.ID && sameKey(cur, s):
			fmt.Fprintf(stdout, "서명자 %s는 이미 같은 키(%s)로 등록되어 있습니다. 변경 없음\n", s.ID, fp)
			return 0
		case cur.ID == s.ID:
			fmt.Fprintf(stderr, "orch: admin signers add: 서명자 %s는 다른 키로 이미 등록되어 있습니다. 바꾸려면 먼저 `orch admin signers remove %s`. 변경 없음\n", s.ID, s.ID)
			return 1
		case sameKey(cur, s):
			fmt.Fprintf(stderr, "orch: admin signers add: 같은 키가 이미 서명자 %s로 등록되어 있습니다. 변경 없음\n", cur.ID)
			return 1
		}
	}
	r.Signers = append(r.Signers, s)
	if err := writeRegistry(path, r.Signers); err != nil {
		fmt.Fprintf(stderr, "orch: admin signers add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "서명자 %s(%s) 등록: %s\n", s.ID, s.Type, fp)
	if s.Type == signing.TypeWebAuthnES256 {
		fmt.Fprintf(stdout, "  rp_id: %s\n  origin: %s\n", s.RPID, s.Origin)
	}
	return 0
}

func signersRemove(path, id string, stdout, stderr io.Writer) int {
	r, err := loadForWrite(path)
	if err != nil {
		fmt.Fprintf(stderr, "orch: admin signers remove: %v\n", err)
		return 1
	}
	kept := []signing.Signer{}
	for _, s := range r.Signers {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	if len(kept) == len(r.Signers) {
		fmt.Fprintf(stderr, "orch: admin signers remove: 서명자 %q가 없습니다. 변경 없음\n", textsafe.Escape(id))
		return 1
	}
	if err := writeRegistry(path, kept); err != nil {
		fmt.Fprintf(stderr, "orch: admin signers remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "서명자 %s 제거\n", id)
	if len(kept) == 0 {
		fmt.Fprintln(stdout, "주의: 서명자가 하나도 없습니다. 목록 파일이 있는 한 서명 필수 모드이므로 어떤 승인도 받을 수 없습니다.")
	}
	return 0
}

// writeRegistry replaces the registry atomically: temp file in the same
// directory, 0644, fsync, rename, fsync of the directory. The directory is
// created (0755) only if missing; its trust was checked by loadForWrite.
func writeRegistry(path string, signers []signing.Signer) error {
	data, err := json.MarshalIndent(registryOut{Version: 1, Signers: signers}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	// Directories created here get 0755 explicitly: MkdirAll's mode is
	// masked by the umask, and a 0700 directory would hide the registry
	// from orchd and the gate.
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || !errors.Is(err, os.ErrNotExist) || filepath.Dir(d) == d {
			break
		}
		created = append(created, d)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("서명자 목록 폴더를 만들 수 없습니다: %w", err)
	}
	for _, d := range created {
		if err := os.Chmod(d, 0o755); err != nil {
			return fmt.Errorf("서명자 목록 폴더 권한을 정할 수 없습니다: %w", err)
		}
	}
	f, err := os.CreateTemp(dir, ".signers-*.tmp")
	if err != nil {
		return fmt.Errorf("임시 파일을 만들 수 없습니다: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("서명자 목록을 쓸 수 없습니다: %w", err)
	}
	if err := f.Chmod(0o644); err != nil {
		return fmt.Errorf("서명자 목록 권한을 정할 수 없습니다: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("서명자 목록을 디스크에 쓸 수 없습니다: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("서명자 목록을 닫을 수 없습니다: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("서명자 목록을 바꿀 수 없습니다: %w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// readEd25519 reads an ssh-ed25519 public key file (one authorized_keys
// line, no options) and returns its canonical form without the comment.
func readEd25519(path string) (line, comment string, err error) {
	b, err := readSmall(path, maxKeyFile)
	if err != nil {
		return "", "", fmt.Errorf("공개 키 파일을 읽을 수 없습니다: %v", err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return "", "", errors.New("공개 키 파일은 한 줄짜리 ssh-ed25519 공개 키여야 합니다")
	}
	pub, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(opts) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", "", errors.New("공개 키를 해석할 수 없습니다(옵션 없는 authorized_keys 한 줄이어야 함)")
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return "", "", fmt.Errorf("ssh-ed25519 키만 등록할 수 있습니다(%s)", pub.Type())
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), comment, nil
}

// readPending reads the (untrusted) pending-enrollment file. A missing file
// is an empty list.
func readPending(path string) ([]pendingCredential, error) {
	b, err := readSmall(path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("대기 목록을 읽을 수 없습니다: %v", err)
	}
	var pf pendingFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil || pf.Version != 1 {
		return nil, fmt.Errorf("대기 목록 형식이 올바르지 않습니다(%s)", path)
	}
	return pf.Pending, nil
}

// readSmall reads a regular file (no symlink) of at most max bytes.
// It checks the path with Lstat before opening (a FIFO would block open),
// opens with O_NOFOLLOW|O_NONBLOCK in case it was swapped meanwhile, and
// requires the opened file to be the one checked.
func readSmall(path string, max int64) ([]byte, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, errors.New("일반 파일이 아닙니다(심볼릭 링크·FIFO 등 불가)")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return nil, errors.New("파일이 확인 도중 바뀌었습니다")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("파일이 너무 큽니다(최대 %d바이트)", max)
	}
	return b, nil
}
