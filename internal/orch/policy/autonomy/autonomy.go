// Package autonomy decides, per task class, how much the orchestrator may do
// without a human, and lowers that autonomy when something goes wrong.
//
// Decide(level, verdict, action) returns Allow, Park (a human must approve) or
// Deny. The table, for a verdict of ok:
//
//	level  push_branch  draft_pr  notify  other, floor-clean  floor-denied kind
//	L0     Park         Park      Park    Park               Deny
//	L1     Allow        Allow     Allow   Park               Deny
//	L2     Allow        Allow     Allow   Park               Deny   (+ notify when a task parks)
//	L3     Allow        Allow     Allow   Park               Deny   (+ post-hoc notify after publishing)
//
// For a verdict of check_weakened every level answers Park for floor-clean
// actions (a human may approve the protected change; no level ever does it
// itself). For behavior_failure and verification_incomplete every level
// answers Deny. policy.Floor is checked first, at every level and for every
// verdict, and nothing can override it. An invalid level is Deny.
//
// The allowlist (policy.yml allowlist, enforced again by the publisher) can
// only narrow what Allow means; it never widens this table.
//
// Demotion is automatic and persistent: Demote lowers a class by exactly one
// level in policy.state.json (0600, atomic write) and appends an
// autonomy_demote ledger event. The effective level is min(policy file level,
// state level). Nothing here ever promotes: a human edits policy.yml and runs
// ClearDemotion (`orch policy clear-demotion <class>`).
package autonomy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aigate/internal/ledger"
	"aigate/internal/orch/policy"
	"aigate/internal/rules"
)

// Level is the autonomy level (see package policy).
type Level = policy.Level

// The levels.
const (
	L0 = policy.L0
	L1 = policy.L1
	L2 = policy.L2
	L3 = policy.L3
)

// Action is an action kind such as "push_branch".
type Action string

// Decision is the answer of Decide.
type Decision string

// The decisions.
const (
	Allow Decision = "allow"
	Park  Decision = "park" // needs a human approval
	Deny  Decision = "deny"
)

// Decide implements the table in the package documentation.
func Decide(level Level, verdict rules.Category, action Action) Decision {
	if policy.Floor(string(action)) != nil {
		return Deny
	}
	if level < L0 || level > L3 {
		return Deny
	}
	switch verdict {
	case rules.CategoryOK:
	case rules.CategoryWeakened:
		return Park
	default:
		return Deny
	}
	if level == L0 {
		return Park
	}
	switch action {
	case "push_branch", "draft_pr", "notify":
		return Allow
	}
	return Park
}

// NotifyOnPark reports whether a task that parks for a human also notifies (L2).
func NotifyOnPark(level Level) bool { return level == L2 }

// NotifyAfterPublish reports whether a post-hoc report is sent after the task
// was published automatically (L3).
func NotifyAfterPublish(level Level) bool { return level == L3 }

// ---- state file and demotion ----

// Entry is one demoted class in policy.state.json.
type Entry struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
	Task   int64  `json:"task"`
	At     string `json:"at"`
}

type stateFile struct {
	Version int              `json:"version"`
	Classes map[string]Entry `json:"classes"`
}

var stateMu sync.Mutex

func readState(path string) (stateFile, error) {
	st := stateFile{Version: 1, Classes: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Version != 1 {
		return stateFile{}, fmt.Errorf("autonomy: %s is not a valid state file", path)
	}
	if st.Classes == nil {
		st.Classes = map[string]Entry{}
	}
	return st, nil
}

func writeState(path string, st stateFile) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
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
	return os.Rename(tmp, path)
}

// Effective is min(the level the policy file grants class, the demotion state
// level). An unreadable state file is a failure to know, so the answer is L0
// together with the error.
func Effective(p policy.Policy, class string) (Level, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	lvl := p.LevelFor(class)
	st, err := readState(p.StatePath())
	if err != nil {
		return L0, err
	}
	if e, ok := st.Classes[class]; ok {
		sl, err := policy.ParseLevel(e.Level)
		if err != nil {
			return L0, fmt.Errorf("autonomy: state for class %q: %w", class, err)
		}
		if sl < lvl {
			lvl = sl
		}
	}
	return lvl, nil
}

// Incident describes why a class is demoted.
type Incident struct {
	Class  string
	Task   int64
	Reason string
}

// Demote lowers incident.Class by exactly one level (from its effective
// level; L0 stays L0 and writes nothing), persists it next to the policy and
// appends an autonomy_demote event to the ledger at ledgerPath for repo.
// The state is written before the ledger, so a ledger failure still leaves the
// class demoted (the error is returned).
func Demote(p policy.Policy, inc Incident, ledgerPath, repo string, now time.Time) (from, to Level, err error) {
	if inc.Class == "" || inc.Reason == "" {
		return 0, 0, errors.New("autonomy: demotion needs a class and a reason")
	}
	from, err = Effective(p, inc.Class)
	if err != nil {
		return L0, L0, err
	}
	if from == L0 {
		return L0, L0, nil
	}
	to = from - 1
	stateMu.Lock()
	st, err := readState(p.StatePath())
	if err == nil {
		st.Classes[inc.Class] = Entry{Level: to.String(), Reason: inc.Reason, Task: inc.Task, At: now.UTC().Format(time.RFC3339)}
		err = writeState(p.StatePath(), st)
	}
	stateMu.Unlock()
	if err != nil {
		return from, from, fmt.Errorf("autonomy: cannot persist the demotion: %w", err)
	}
	if err := ledger.Append(ledgerPath, "autonomy_demote", repo, map[string]any{
		"class": inc.Class, "from": from.String(), "to": to.String(), "reason": inc.Reason, "task": int(inc.Task),
	}, now.UTC()); err != nil {
		return from, to, fmt.Errorf("autonomy: class %q is demoted but the ledger event failed: %w", inc.Class, err)
	}
	return from, to, nil
}

// ErrNoDemotion is returned by ClearDemotion for a class that is not demoted.
var ErrNoDemotion = errors.New("autonomy: that class has no demotion entry")

// ClearDemotion removes the demotion entry of class and appends an
// autonomy_clear_demotion event. It does not change policy.yml: the class then
// runs at the level the file grants, so a human edits the file first.
func ClearDemotion(p policy.Policy, class, ledgerPath, repo string, now time.Time) error {
	stateMu.Lock()
	st, err := readState(p.StatePath())
	if err == nil {
		if _, ok := st.Classes[class]; !ok {
			stateMu.Unlock()
			return ErrNoDemotion
		}
		delete(st.Classes, class)
		err = writeState(p.StatePath(), st)
	}
	stateMu.Unlock()
	if err != nil {
		return err
	}
	return ledger.Append(ledgerPath, "autonomy_clear_demotion", repo, map[string]any{
		"class": class, "policy_hash": p.Hash(),
	}, now.UTC())
}

// Demoted lists the demotion entries (class -> entry).
func Demoted(p policy.Policy) (map[string]Entry, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := readState(p.StatePath())
	return st.Classes, err
}
