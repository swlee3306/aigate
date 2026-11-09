// Package signing defines what an approval signature covers: a canonical,
// strictly parseable text message. Its only internal import is the stdlib-only
// internal/webauthn (registry.go and sshsig.go use golang.org/x/crypto/ssh), so
// internal/approval can import it.
package signing

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NamespaceApproval is the SSH signature namespace for approval signatures.
const NamespaceApproval = "aigate-approval"

// NamespaceLogin is the SSH signature namespace for login challenges (the
// orchd bearer-token login). It must differ from NamespaceApproval so a login
// signature can never stand in for an approval, and vice versa.
const NamespaceLogin = "aigate-login"

// ValidSignerID reports whether id has the registry's signer id format.
func ValidSignerID(id string) bool { return idRE.MatchString(id) }

// Approval kinds. They must equal store.KindProtectedModified and
// store.KindPublishOutsideAllowlist (checked by a test elsewhere).
const (
	KindProtectedModified       = "protected_modified"
	KindPublishOutsideAllowlist = "publish_outside_allowlist"
)

const (
	magic      = "aigate-approval-v1"
	timeLayout = "2006-01-02T15:04:05Z"
)

// Message is the content an approver signs.
type Message struct {
	Kind   string
	Repo   string // gate.RepoID: 12 lowercase hex
	Task   int64
	Base   string // 40 or 64 lowercase hex
	Hash   string // 64 lowercase hex for every kind (full SHA-256 diff hash or publish hash)
	Issued time.Time
}

func isLowerHex(s string, lengths ...int) bool {
	ok := false
	for _, n := range lengths {
		if len(s) == n {
			ok = true
		}
	}
	if !ok {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Validate reports whether the message can be rendered canonically.
func (m Message) Validate() error {
	switch {
	case m.Kind != KindProtectedModified && m.Kind != KindPublishOutsideAllowlist:
		return fmt.Errorf("signing: unknown kind %q", m.Kind)
	case !isLowerHex(m.Repo, 12):
		return errors.New("signing: repo must be 12 lowercase hex characters")
	case m.Task <= 0:
		return errors.New("signing: task must be positive")
	case !isLowerHex(m.Base, 40, 64):
		return errors.New("signing: base must be 40 or 64 lowercase hex characters")
	case !isLowerHex(m.Hash, 64):
		// Every kind carries a full SHA-256. The 16-hex diff hash of builds
		// before 2026-11-09 is refused, so a message cannot be downgraded to
		// a short, collision-prone hash.
		return errors.New("signing: hash must be 64 lowercase hex characters")
	case m.Issued.IsZero() || m.Issued.Location() != time.UTC || m.Issued.Nanosecond() != 0:
		return errors.New("signing: issued must be a UTC time with whole seconds")
	}
	return nil
}

// Text returns the canonical message, or "" if the message is invalid.
func (m Message) Text() string {
	if m.Validate() != nil {
		return ""
	}
	return fmt.Sprintf("%s\nkind: %s\nrepo: %s\ntask: %d\nbase: %s\nhash: %s\nissued: %s\n",
		magic, m.Kind, m.Repo, m.Task, m.Base, m.Hash, m.Issued.Format(timeLayout))
}

// Digest is the SHA-256 of Text(). It is the zero value for an invalid
// message; callers that sign must call Validate first.
func (m Message) Digest() [32]byte {
	t := m.Text()
	if t == "" {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(t))
}

// ParseMessage is the strict inverse of Text: it accepts only byte-exact
// canonical text.
func ParseMessage(text string) (Message, error) {
	if !strings.HasSuffix(text, "\n") {
		return Message{}, errors.New("signing: message must end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 7 || lines[0] != magic {
		return Message{}, errors.New("signing: not an aigate-approval-v1 message")
	}
	keys := []string{"kind", "repo", "task", "base", "hash", "issued"}
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, ok := strings.CutPrefix(lines[i+1], k+": ")
		if !ok {
			return Message{}, fmt.Errorf("signing: line %d must be %q", i+2, k+": <value>")
		}
		vals[i] = v
	}
	task, err := strconv.ParseInt(vals[2], 10, 64)
	if err != nil {
		return Message{}, fmt.Errorf("signing: bad task: %w", err)
	}
	issued, err := time.Parse(timeLayout, vals[5])
	if err != nil {
		return Message{}, fmt.Errorf("signing: bad issued: %w", err)
	}
	m := Message{Kind: vals[0], Repo: vals[1], Task: task, Base: vals[3], Hash: vals[4], Issued: issued.UTC()}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	if m.Text() != text {
		return Message{}, errors.New("signing: message is not canonical")
	}
	return m, nil
}
