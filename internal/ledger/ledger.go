// Package ledger implements an append-only, hash-chained event log stored as
// one JSON object per line (design 3.9). Each line carries the hash of the
// previous line, so any edit, deletion, reordering or truncation is detected
// by Verify.
package ledger

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Event is one ledger line.
type Event struct {
	Seq      int            `json:"seq"`
	TS       string         `json:"ts"`
	PrevHash string         `json:"prev_hash"`
	Hash     string         `json:"hash"`
	Type     string         `json:"type"`
	Repo     string         `json:"repo"`
	Payload  map[string]any `json:"payload"`
}

const (
	genesisPrev = "0000000000000000000000000000000000000000000000000000000000000000"
	lockWait    = 10 * time.Second
)

// computeHash returns hex sha256 over the fields seq, ts, prev_hash, type,
// repo, payload. Fields are joined by a literal '|', but each field is
// length-prefixed as "<len>:<value>" so a '|' inside a value cannot shift a
// field boundary. payload is the canonical JSON of the map (encoding/json
// sorts map keys); nil is encoded as {}.
func computeHash(seq int, ts, prev, typ, repo string, payload map[string]any) (string, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	pj, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ledger: marshal payload: %w", err)
	}
	fields := []string{strconv.Itoa(seq), ts, prev, typ, repo, string(pj)}
	var sb strings.Builder
	for i, f := range fields {
		if i > 0 {
			sb.WriteByte('|')
		}
		sb.WriteString(strconv.Itoa(len(f)))
		sb.WriteByte(':')
		sb.WriteString(f)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:]), nil
}

// parseLine decodes one line; numbers are kept as json.Number so that
// re-marshalling the payload reproduces the original bytes.
func parseLine(b []byte) (Event, error) {
	var e Event
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return Event{}, err
	}
	if dec.More() {
		return Event{}, errors.New("trailing data after JSON object")
	}
	return e, nil
}

func lockFile(path string) (*os.File, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lf, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			lf.Close()
			return nil, fmt.Errorf("ledger: flock: %w", err)
		}
		if time.Now().After(deadline) {
			lf.Close()
			return nil, fmt.Errorf("ledger: timed out after %s waiting for lock %s.lock", lockWait, path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lastLine returns the last line (without newline) of f, or nil for an empty
// file. It errors if the file does not end in a newline.
func lastLine(f *os.File) ([]byte, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, size-1); err != nil {
		return nil, err
	}
	if one[0] != '\n' {
		return nil, errors.New("ledger: last line lacks trailing newline (partial write); refusing to append")
	}
	// Scan backwards (excluding the final newline) for the previous newline.
	const chunk = 4096
	end := size - 1
	start := int64(0)
	for pos := end; pos > 0; {
		lo := pos - chunk
		if lo < 0 {
			lo = 0
		}
		buf := make([]byte, pos-lo)
		if _, err := f.ReadAt(buf, lo); err != nil {
			return nil, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			start = lo + int64(i) + 1
			break
		}
		pos = lo
	}
	line := make([]byte, end-start)
	if _, err := f.ReadAt(line, start); err != nil {
		return nil, err
	}
	return line, nil
}

// Append adds one event to the ledger at path under an exclusive file lock.
func Append(path string, typ, repo string, payload map[string]any, now time.Time) error {
	if typ == "" || repo == "" {
		return errors.New("ledger: type and repo must be non-empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lf, err := lockFile(path)
	if err != nil {
		return err
	}
	defer lf.Close() // closing releases the flock

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	seq, prev := 1, genesisPrev
	tail, err := lastLine(f)
	if err != nil {
		return err
	}
	if tail != nil {
		last, perr := parseLine(tail)
		if perr != nil {
			return fmt.Errorf("ledger: tail line is corrupt (%v); refusing to append", perr)
		}
		if last.Seq < 1 || len(last.Hash) != 64 {
			return errors.New("ledger: tail line is corrupt (bad seq/hash); refusing to append")
		}
		seq, prev = last.Seq+1, last.Hash
	}

	if payload == nil {
		payload = map[string]any{}
	}
	ts := now.UTC().Format(time.RFC3339)
	h, err := computeHash(seq, ts, prev, typ, repo, payload)
	if err != nil {
		return err
	}
	line, err := json.Marshal(Event{Seq: seq, TS: ts, PrevHash: prev, Hash: h, Type: typ, Repo: repo, Payload: payload})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if n, err := f.Write(line); err != nil || n != len(line) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("ledger: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ledger: fsync: %w", err)
	}
	if d, err := os.Open(dir); err == nil { // best effort
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Verify walks the whole ledger. It returns the number of valid lines before
// the first problem, and an error describing that problem.
func Verify(path string) (n int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	prev := genesisPrev
	for {
		raw, rerr := r.ReadBytes('\n')
		if len(raw) == 0 && rerr == io.EOF {
			return n, nil
		}
		line := n + 1
		if rerr != nil && rerr != io.EOF {
			return n, rerr
		}
		if rerr == io.EOF {
			return n, fmt.Errorf("ledger: line %d: missing trailing newline (truncated or partial write)", line)
		}
		e, perr := parseLine(bytes.TrimSuffix(raw, []byte("\n")))
		if perr != nil {
			return n, fmt.Errorf("ledger: line %d: bad json: %v", line, perr)
		}
		if e.Seq != n+1 {
			return n, fmt.Errorf("ledger: line %d: seq gap: got seq %d, want seq %d (line deleted or reordered)", line, e.Seq, n+1)
		}
		if e.PrevHash != prev {
			return n, fmt.Errorf("ledger: line %d (seq %d): tamper: prev_hash does not match previous line's hash", line, e.Seq)
		}
		want, herr := computeHash(e.Seq, e.TS, e.PrevHash, e.Type, e.Repo, e.Payload)
		if herr != nil {
			return n, fmt.Errorf("ledger: line %d (seq %d): %v", line, e.Seq, herr)
		}
		if want != e.Hash {
			return n, fmt.Errorf("ledger: line %d (seq %d): tamper: hash mismatch (line content was modified)", line, e.Seq)
		}
		prev = e.Hash
		n++
	}
}
