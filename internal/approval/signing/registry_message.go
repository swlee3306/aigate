package signing

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// NamespaceRegistry is the SSH signature namespace for project registry
// changes. It differs from NamespaceApproval and NamespaceLogin, so a
// signature made for one purpose never verifies for another.
const NamespaceRegistry = "aigate-registry"

// Registry change actions (they equal registrymsg.ActionRegisterProject and
// registrymsg.ActionSetFlag).
const (
	RegistryActionRegisterProject = "register_project"
	RegistryActionSetFlag         = "set_flag"
)

const registryMagic = "aigate-registry-v1"

var registrySubjectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// RegistryMessage is the content signed to change the project registry.
type RegistryMessage struct {
	Action  string // register_project or set_flag
	Subject string // project id
	Change  string // 64 lowercase hex: SHA-256 of the canonical change JSON
	Request string // 32 lowercase hex: one-time request id
	Issued  time.Time
}

// Validate reports whether the message can be rendered canonically.
func (m RegistryMessage) Validate() error {
	switch {
	case m.Action != RegistryActionRegisterProject && m.Action != RegistryActionSetFlag:
		return fmt.Errorf("signing: unknown registry action %q", m.Action)
	case !registrySubjectRE.MatchString(m.Subject):
		return errors.New("signing: subject is not a valid project id")
	case !isLowerHex(m.Change, 64):
		return errors.New("signing: change must be 64 lowercase hex characters")
	case !isLowerHex(m.Request, 32):
		return errors.New("signing: request must be 32 lowercase hex characters")
	case m.Issued.IsZero() || m.Issued.Location() != time.UTC || m.Issued.Nanosecond() != 0:
		return errors.New("signing: issued must be a UTC time with whole seconds")
	}
	return nil
}

// Text returns the canonical message, or "" if the message is invalid.
func (m RegistryMessage) Text() string {
	if m.Validate() != nil {
		return ""
	}
	return fmt.Sprintf("%s\naction: %s\nsubject: %s\nchange: %s\nrequest: %s\nissued: %s\n",
		registryMagic, m.Action, m.Subject, m.Change, m.Request, m.Issued.Format(timeLayout))
}

// Digest is the SHA-256 of Text(); the zero value for an invalid message.
func (m RegistryMessage) Digest() [32]byte {
	t := m.Text()
	if t == "" {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(t))
}

// ParseRegistryMessage is the strict inverse of Text: only byte-exact
// canonical text is accepted.
func ParseRegistryMessage(text string) (RegistryMessage, error) {
	if !strings.HasSuffix(text, "\n") {
		return RegistryMessage{}, errors.New("signing: message must end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 6 || lines[0] != registryMagic {
		return RegistryMessage{}, errors.New("signing: not an aigate-registry-v1 message")
	}
	keys := []string{"action", "subject", "change", "request", "issued"}
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, ok := strings.CutPrefix(lines[i+1], k+": ")
		if !ok {
			return RegistryMessage{}, fmt.Errorf("signing: line %d must be %q", i+2, k+": <value>")
		}
		vals[i] = v
	}
	issued, err := time.Parse(timeLayout, vals[4])
	if err != nil {
		return RegistryMessage{}, fmt.Errorf("signing: bad issued: %w", err)
	}
	m := RegistryMessage{Action: vals[0], Subject: vals[1], Change: vals[2], Request: vals[3], Issued: issued.UTC()}
	if err := m.Validate(); err != nil {
		return RegistryMessage{}, err
	}
	if m.Text() != text {
		return RegistryMessage{}, errors.New("signing: message is not canonical")
	}
	return m, nil
}

// VerifyRegistry checks sig over msg with the same steps as Verify (valid
// message, known signer, algorithm equal to the signer type, Issued within
// MaxAge of Now), except that SSH signatures must be in NamespaceRegistry and
// freshness can never be skipped: registry changes always expire.
func VerifyRegistry(reg Registry, msg RegistryMessage, sig Signature, o VerifyOptions) error {
	if msg.Validate() != nil {
		return ErrInvalidMessage
	}
	o.SkipFreshness = false
	signer, err := checkSignerAndFreshness(reg, msg.Issued, sig, o)
	if err != nil {
		return err
	}
	return verifySignature(signer, NamespaceRegistry, msg.Text(), msg.Digest(), sig)
}
