// Package approval stores hash-bound human approvals outside the repository.
// An approval is valid only for the exact (repo, base, hash) triple.
package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/term"
)

const fileVersion = 1

// isTerminal is a seam so the positive TTY path is testable.
var isTerminal = term.IsTerminal

// Store is a JSON approvals file at Path.
type Store struct{ Path string }

type entry struct {
	Repo      string `json:"repo"`
	Base      string `json:"base"`
	Hash      string `json:"hash"`
	GrantedAt string `json:"granted_at"`
}

type fileData struct {
	Version   int     `json:"version"`
	Approvals []entry `json:"approvals"`
}

// load reads the store. A missing file yields an empty store (nil error);
// corrupt content or an unknown version yields an error.
func (s Store) load() (fileData, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fileData{Version: fileVersion}, nil
	}
	if err != nil {
		return fileData{}, err
	}
	var d fileData
	if err := json.Unmarshal(b, &d); err != nil {
		return fileData{}, fmt.Errorf("approvals file %s is corrupt: %w", s.Path, err)
	}
	if d.Version != fileVersion {
		return fileData{}, fmt.Errorf("approvals file %s has unsupported version %d", s.Path, d.Version)
	}
	return d, nil
}

// Valid reports whether (repoID, baseSHA, hash) was granted. It never panics
// and returns false for a missing, corrupt or unknown-version file.
func (s Store) Valid(repoID, baseSHA, hash string) bool {
	if repoID == "" || baseSHA == "" || hash == "" {
		return false
	}
	d, err := s.load()
	if err != nil {
		return false
	}
	for _, e := range d.Approvals {
		if e.Repo == repoID && e.Base == baseSHA && e.Hash == hash {
			return true
		}
	}
	return false
}

// Grant records an approval. It is idempotent for an identical triple and
// refuses (leaving the file untouched) on empty arguments or a bad existing file.
func (s Store) Grant(repoID, baseSHA, hash string, now time.Time) error {
	if repoID == "" || baseSHA == "" || hash == "" {
		return errors.New("approval: repo, base and hash must be non-empty")
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	d, err := s.load()
	if err != nil {
		return err
	}
	for _, e := range d.Approvals {
		if e.Repo == repoID && e.Base == baseSHA && e.Hash == hash {
			return nil
		}
	}
	d.Approvals = append(d.Approvals, entry{
		Repo: repoID, Base: baseSHA, Hash: hash,
		GrantedAt: now.UTC().Format(time.RFC3339),
	})
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.Path, append(out, '\n'))
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".approvals-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// RequireTTY returns an error unless stdin is a real terminal.
func RequireTTY(stdin *os.File) error {
	if stdin == nil || !isTerminal(int(stdin.Fd())) {
		return errors.New("approval requires an interactive terminal (stdin is not a TTY)")
	}
	return nil
}
