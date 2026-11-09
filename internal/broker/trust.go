package broker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// TrustOptions tunes the file trust checks. OwnerOK reports whether an owner
// uid is trusted (nil: uid 0 only). StopAt, when set, ends the ancestor walk
// at that directory (inclusive); production leaves it empty so the whole chain
// up to "/" is checked. Tests use it to stay inside a temp dir.
type TrustOptions struct {
	OwnerOK func(uid uint32) bool
	StopAt  string
}

const maxConfigSize = 1 << 20

// readTrustedFile reads a root-owned file. Same rules as the W1 signing
// registry (internal/approval/signing): no symlinks, no group/other write, a
// trusted owner, parent trusted too, file identity stable during the read.
// In addition every ancestor directory up to "/" must satisfy the same rules,
// so an untrusted account cannot swap a directory higher up.
// It fails closed: an absent file is an error here (unlike the registry).
func readTrustedFile(path string, o TrustOptions) ([]byte, error) {
	ownerOK := o.OwnerOK
	if ownerOK == nil {
		ownerOK = func(uid uint32) bool { return uid == 0 }
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("설정 파일 경로는 절대 경로여야 합니다")
	}
	dir := filepath.Dir(path)
	di, err := os.Lstat(dir)
	if err != nil {
		return nil, errors.New("설정 파일 폴더를 확인할 수 없습니다")
	}
	if !di.IsDir() {
		return nil, errors.New("설정 파일 상위 경로가 폴더가 아닙니다(심볼릭 링크 불가)")
	}
	if err := checkTrusted(di, "설정 파일 폴더", ownerOK); err != nil {
		return nil, err
	}
	if err := checkChain(dir, o.StopAt, ownerOK); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("설정 파일을 확인할 수 없습니다")
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("설정 파일은 일반 파일이어야 합니다(심볼릭 링크 불가)")
	}
	if err := checkTrusted(fi, "설정 파일", ownerOK); err != nil {
		return nil, err
	}
	if fi.Size() > maxConfigSize {
		return nil, errors.New("설정 파일이 너무 큽니다")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("설정 파일을 열 수 없습니다")
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil || !os.SameFile(fi, fi2) {
		return nil, errors.New("설정 파일이 확인 도중 바뀌었습니다")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil || len(data) > maxConfigSize {
		return nil, errors.New("설정 파일을 읽을 수 없습니다")
	}
	return data, nil
}

// checkChain walks the real (symlink-resolved) ancestors of dir up to "/" (or
// stopAt) and requires each to be a trusted directory.
func checkChain(dir, stopAt string, ownerOK func(uint32) bool) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return errors.New("설정 파일 폴더 경로를 확인할 수 없습니다")
	}
	if stopAt != "" {
		if s, err := filepath.EvalSymlinks(stopAt); err == nil {
			stopAt = s
		}
	}
	for cur := real; ; cur = filepath.Dir(cur) {
		fi, err := os.Lstat(cur)
		if err != nil || !fi.IsDir() {
			return errors.New("설정 파일 상위 폴더를 확인할 수 없습니다")
		}
		if err := checkTrusted(fi, "설정 파일 상위 폴더", ownerOK); err != nil {
			return err
		}
		if cur == "/" || cur == stopAt || filepath.Dir(cur) == cur {
			return nil
		}
	}
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

// readSecretFile reads a key/token file. It must be a regular, non-symlink
// file owned by a trusted uid (the broker's own account) with no group/other
// access at all (0400/0600). Errors never carry the path or the content.
func readSecretFile(path string, ownerOK func(uint32) bool) (string, error) {
	const what = "키 파일"
	bad := func(why string) (string, error) { return "", fmt.Errorf("%s %s", what, why) }
	if !filepath.IsAbs(path) {
		return bad("경로가 올바르지 않습니다")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return bad("을 확인할 수 없습니다")
	}
	if !fi.Mode().IsRegular() {
		return bad("은 일반 파일이어야 합니다(심볼릭 링크 불가)")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !ownerOK(st.Uid) {
		return bad("의 소유자를 신뢰할 수 없습니다")
	}
	if fi.Mode().Perm()&0o177 != 0 {
		return bad("에 소유자 외 권한이 있습니다(0400 또는 0600 필요)")
	}
	if fi.Size() > 64<<10 {
		return bad("이 너무 큽니다")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return bad("을 열 수 없습니다")
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil || !os.SameFile(fi, fi2) {
		return bad("이 확인 도중 바뀌었습니다")
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return bad("을 읽을 수 없습니다")
	}
	v := trimSpaceBytes(data)
	if v == "" {
		return bad("이 비어 있습니다")
	}
	return v, nil
}

func trimSpaceBytes(b []byte) string {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return string(b[i:j])
}
