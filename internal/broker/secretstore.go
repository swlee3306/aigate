package broker

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Secret store (W3b decision D2): one file per token under <state>/secrets/,
// 0400, owned by the broker account, in a 0700 directory. Values are written
// atomically (temp file, fsync, rename, directory fsync) and never appear in an
// error, a log or any listing: the only way to read one is Get.
//
// A file that is not a regular file, is owned by someone else, has a mode other
// than 0400, or is a symlink is treated as absent for that one token. An
// untrusted directory makes the whole store unavailable.

const (
	secretFileMode = 0o400
	maxSecretBytes = 4096
)

var (
	secretRefRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

	// ErrSecretStoreUnavailable: the secrets directory cannot be trusted.
	ErrSecretStoreUnavailable = errors.New("비밀 저장소를 사용할 수 없습니다")
	errSecretName             = errors.New("비밀 이름이 올바르지 않습니다")
	errSecretValue            = errors.New("비밀 값이 올바르지 않습니다")
	errSecretWrite            = errors.New("비밀을 저장할 수 없습니다")
	errSecretDelete           = errors.New("비밀을 지울 수 없습니다")
)

// SecretState is what Lookup finds for a name.
type SecretState int

const (
	SecretAbsent    SecretState = iota // no file (or no usable store)
	SecretOK                           // a trusted file
	SecretUntrusted                    // something is there but fails the trust rules
)

// SecretStore is the directory of token files. Dir is the secrets directory
// itself. The zero opts trust the current effective uid.
type SecretStore struct {
	Dir  string
	opts RegistryOptions
}

// OpenSecretStore creates <stateDir>/secrets (0700) when it is missing, but
// only if the state directory itself passes the trust check, and requires the
// result to be trusted.
func OpenSecretStore(stateDir string, o RegistryOptions) (*SecretStore, error) {
	s := &SecretStore{Dir: filepath.Join(stateDir, "secrets"), opts: o}
	if checkStateDir(stateDir, o.ownerOK()) != nil {
		return s, ErrSecretStoreUnavailable
	}
	if err := ensureStateDir(s.Dir, o.ownerOK()); err != nil {
		return s, ErrSecretStoreUnavailable
	}
	sweepSecretTemps(s.Dir)
	return s, nil
}

func (s *SecretStore) dirOK() bool { return checkStateDir(s.Dir, s.opts.ownerOK()) == nil }

func (s *SecretStore) path(ref string) string { return filepath.Join(s.Dir, ref) }

// Lookup reports the trust state of the file for ref without reading it.
func (s *SecretStore) Lookup(ref string) SecretState {
	if !secretRefRE.MatchString(ref) || !s.dirOK() {
		return SecretAbsent
	}
	fi, err := os.Lstat(s.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return SecretAbsent
	}
	if err != nil || !s.fileTrusted(fi) {
		return SecretUntrusted
	}
	return SecretOK
}

func (s *SecretStore) fileTrusted(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode().IsRegular() && fi.Mode().Perm() == secretFileMode &&
		s.opts.fileOwner()(st.Uid) && st.Nlink == 1 && fi.Size() > 0 && fi.Size() <= maxSecretBytes
}

// Get returns the stored value of ref exactly as written. ok is false when the
// token is absent or its file is untrusted.
func (s *SecretStore) Get(ref string) (value string, ok bool) {
	if !secretRefRE.MatchString(ref) || !s.dirOK() {
		return "", false
	}
	p := s.path(ref)
	fi, err := os.Lstat(p)
	if err != nil || !s.fileTrusted(fi) {
		return "", false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil || !os.SameFile(fi, fi2) || !s.fileTrusted(fi2) {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		return "", false
	}
	return string(data), true
}

// Put writes value for ref atomically with mode 0400. On failure the previous
// value (if any) is untouched and no temp file stays behind. An existing file
// that is untrusted (wrong mode, symlink) is replaced by rename, never written
// through. Value rules (length, characters) belong to the caller.
func (s *SecretStore) Put(ref, value string) error {
	if !secretRefRE.MatchString(ref) {
		return errSecretName
	}
	if value == "" || len(value) > maxSecretBytes {
		return errSecretValue
	}
	if !s.dirOK() {
		return ErrSecretStoreUnavailable
	}
	prev, hadPrev := s.Get(ref)
	renamed, err := writeAtomicMode(s.path(ref), []byte(value), secretFileMode, s.opts.failAt, false)
	if err == nil {
		return nil
	}
	if renamed {
		// The new file is already in place and only the directory fsync
		// failed. An error return must mean the previous value is the live
		// one (callers keep their metadata for it), so put it back; with no
		// previous value remove the new file. The restore ignores failAt.
		if hadPrev {
			if r2, e2 := writeAtomicMode(s.path(ref), []byte(prev), secretFileMode, nil, false); e2 != nil && !r2 {
				s.wipeAndRemove(ref)
			}
		} else {
			s.wipeAndRemove(ref)
		}
	}
	return errSecretWrite // the underlying error is dropped: it could quote a path
}

// Delete overwrites the file's content with zeros, syncs and removes it. A
// symlink or other non-regular entry is only removed, never written through.
// A missing file is not an error.
func (s *SecretStore) Delete(ref string) error {
	if !secretRefRE.MatchString(ref) {
		return errSecretName
	}
	if !s.dirOK() {
		return ErrSecretStoreUnavailable
	}
	if err := s.wipeAndRemove(ref); err != nil {
		return err
	}
	if d, err := os.Open(s.Dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// wipeAndRemove overwrites the file's content with zeros, syncs and unlinks
// it. Symlinks and other non-regular entries are only unlinked. If the unlink
// fails the file goes back to 0400 (never left wider than 0600). Missing is fine.
func (s *SecretStore) wipeAndRemove(ref string) error {
	p := s.path(ref)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errSecretDelete
	}
	wiped := false
	if st, ok := fi.Sys().(*syscall.Stat_t); fi.Mode().IsRegular() && ok && s.opts.fileOwner()(st.Uid) {
		if os.Chmod(p, 0o600) == nil {
			wiped = true
			if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NOFOLLOW, 0); err == nil {
				if fi2, err := f.Stat(); err == nil && os.SameFile(fi, fi2) {
					f.Write(make([]byte, fi.Size()))
					f.Sync()
				}
				f.Close()
			}
		}
	}
	err = os.Remove(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		err = os.Remove(p) // one retry
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if wiped {
			os.Chmod(p, secretFileMode)
		}
		return errSecretDelete
	}
	return nil
}

// sweepSecretTemps removes every leftover temp file (.state-*) of secrets/ at
// start. No age threshold: no writer exists yet. Content is zeroed first;
// symlinks are only unlinked, never followed.
func sweepSecretTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".state-") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Mode().IsRegular() && os.Chmod(p, 0o600) == nil {
			if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NOFOLLOW, 0); err == nil {
				if fi2, err := f.Stat(); err == nil && os.SameFile(fi, fi2) {
					f.Write(make([]byte, fi.Size()))
					f.Sync()
				}
				f.Close()
			}
		}
		os.Remove(p)
	}
}

// Refs lists the names that have an entry in the store (trusted or not),
// sorted. Temp files and names outside the naming rule are not listed.
func (s *SecretStore) Refs() []string {
	if !s.dirOK() {
		return nil
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if secretRefRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
