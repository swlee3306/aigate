// Package policy holds the rules no configuration can relax.
package policy

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrFloor is returned (wrapped) for an action the hard floor forbids.
var ErrFloor = errors.New("policy: action is below the hard floor and is always denied")

// floorWords are substrings of the normalised action kind (lower case, only
// letters and digits, so "delete_branch", "deleteBranch" and "DELETE-BRANCH"
// all become "deletebranch"). Deleting, destroying, forcing, anything that
// touches accounts, tokens, permissions, admin rights, deploys, merges and
// service restarts is out of reach for the orchestrator, whatever a policy
// file or an allowlist says. The list errs on the side of denying.
var floorWords = []string{
	"delete", "destroy", "force", "admin", "token", "permission", "account",
	"deploy", "merge", "restart", "reboot", "shutdown", "remove", "wipe", "purge",
	"truncate", "revoke", "chmod", "chown", "sudo", "credential", "secret",
}

// Floor returns a non-nil error wrapping ErrFloor for every action kind in the
// destructive, operations or account family, and nil for everything else.
// Unknown kinds pass the floor; they are still not allowed anywhere unless an
// allowlist names them (see Allowlist).
func Floor(action string) error {
	n := normalize(action)
	if n == "" {
		return fmt.Errorf("%w: empty action kind", ErrFloor)
	}
	for _, w := range floorWords {
		if strings.Contains(n, w) {
			return fmt.Errorf("%w: %q (matches %q)", ErrFloor, action, w)
		}
	}
	return nil
}

// normalize lower-cases, folds full-width letters, and drops everything that is
// not an ASCII letter or digit (separators, zero-width and other invisible
// characters, case).
func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0xFF01 && r <= 0xFF5E { // full-width ASCII
			r -= 0xFEE0
		}
		r = unicode.ToLower(r)
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Allowlist is the set of action kinds a human enabled in the configuration.
// Its zero value allows nothing.
type Allowlist map[string]bool

// NewAllowlist builds an allowlist from kind names.
func NewAllowlist(kinds ...string) Allowlist {
	a := Allowlist{}
	for _, k := range kinds {
		a[k] = true
	}
	return a
}

// ErrNotAllowlisted is returned for a kind that passes the floor but is not listed.
var ErrNotAllowlisted = errors.New("policy: action is not in the allowlist")

// Permit reports whether kind may run without a human: the floor must allow
// it AND the allowlist must list it exactly. The floor is checked first and
// cannot be overridden by the list.
func (a Allowlist) Permit(kind string) error {
	if err := Floor(kind); err != nil {
		return err
	}
	if !a[kind] {
		return fmt.Errorf("%w: %q", ErrNotAllowlisted, kind)
	}
	return nil
}
