// Package publish performs the few outward actions the orchestrator may take
// after a task passed: push the task branch, open a DRAFT merge request, notify.
//
// An action runs only if the hard floor (policy.Floor) allows its kind AND the
// kind is in the configured allowlist (or a human granted exactly this action
// for this head, see approvals.PublishHash). Otherwise Do returns a
// *NeedsApprovalError (errors.Is ErrNeedsApproval) and the step parks the task.
//
// The GitLab write token is read only from a 0600 file, never logged, never
// put on a command line, in an environment variable or in a git remote URL.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aigate/internal/approval"
	"aigate/internal/gate"
	"aigate/internal/ledger"
	"aigate/internal/orch/approvals"
	"aigate/internal/orch/policy"
	"aigate/internal/orch/store"
)

// Action kinds this package can execute.
const (
	KindPushBranch = "push_branch"
	KindDraftPR    = "draft_pr"
	KindNotify     = "notify"
)

// ErrNeedsApproval is matched (errors.Is) by every *NeedsApprovalError.
var ErrNeedsApproval = errors.New("publish: action needs a human approval")

// ErrBadRef is returned for a refname other than refs/heads/orch/<task>.
var ErrBadRef = errors.New("publish: only refs/heads/orch/<task-id> may be pushed")

// NeedsApprovalError says which action is waiting and the hash its approval binds.
type NeedsApprovalError struct {
	Action Action
	Hash   string // approvals.PublishHash(task, head, kind)
	Reason string
}

func (e *NeedsApprovalError) Error() string {
	return fmt.Sprintf("publish: %s for task %d needs a human approval: %s", e.Action.Kind, e.Action.Task, e.Reason)
}

// Is makes errors.Is(err, ErrNeedsApproval) true.
func (e *NeedsApprovalError) Is(target error) bool { return target == ErrNeedsApproval }

// Action is one outward step.
type Action struct {
	Kind string
	Task int64  // task id (> 0)
	Repo string // absolute path of the local repository (push_branch)
	Base string // base commit of the task (binds a publish grant)
	Head string // commit to publish (40 or 64 hex)
	Ref  string // optional; if set it must equal refs/heads/orch/<task>
	// PolicyHash is the hash of the policy that decided this action (recorded
	// in the ledger; it does not influence what runs).
	PolicyHash string
}

// Recorder appends events to the tamper-evident ledger. Publisher writes a
// "publish_action" event BEFORE every action runs (an error stops the action,
// so no action is ever unrecorded) and a "publish_result" event after it
// (best effort: a failure there is ignored, the action already happened).
type Recorder interface {
	Record(typ string, payload map[string]any) error
}

// Notifier delivers a notification. It is a stub interface: Task 12 or later
// provides a real one.
type Notifier interface {
	Notify(ctx context.Context, task int64, msg string) error
}

// GitLab configures the draft merge request.
type GitLab struct {
	BaseURL      string // https://host (http only for a loopback host)
	Project      string // numeric id or "group/project"
	TargetBranch string // default "main"
}

// Config configures a Publisher.
type Config struct {
	Allow policy.Allowlist
	// AllowSource, when set, supplies the allowlist at every action (so a
	// re-read policy takes effect); Allow is then ignored.
	AllowSource func() policy.Allowlist
	// Remote is where push_branch pushes: an absolute local path or an
	// https:// URL without credentials.
	Remote string
	// RepoRemotes overrides Remote for tasks of a given repository (absolute
	// repo path -> remote, same forms as Remote). A repository without an
	// entry pushes to Remote.
	RepoRemotes map[string]string
	GitLab      GitLab
	// TokenFile is the 0600 file holding the GitLab write token. Required
	// for an https remote and for draft_pr.
	TokenFile string
	// SSHKeyFile and SSHKnownHosts serve an ssh:// remote: the 0600 private
	// key (a repository deploy key) and a known_hosts file pinning the server.
	// Host keys are always checked strictly; no agent, no config files.
	SSHKeyFile, SSHKnownHosts string
	Notifier                  Notifier
	// Grants, when set, lets a human-approved action (publish_outside_allowlist)
	// run once for the exact head it was approved for.
	Grants approvals.Grants
	// Ledger, when set, records every action (see Recorder).
	Ledger Recorder
	// HTTP is the client for the GitLab API (default: 30 s timeout, no redirects).
	HTTP *http.Client
	// Git is the git binary (default "git").
	Git string
}

// Publisher runs publish actions.
type Publisher struct {
	cfg  Config
	plan []string
}

// New validates cfg. Which actions Publish plans follows what is configured:
// push_branch when Remote is set, draft_pr when GitLab.BaseURL is set, notify
// when Notifier is set. The token file is checked now (mode) and re-read on use.
func New(cfg Config) (*Publisher, error) {
	p := &Publisher{cfg: cfg}
	if cfg.Git == "" {
		p.cfg.Git = "git"
	}
	if cfg.HTTP == nil {
		p.cfg.HTTP = &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if cfg.Remote != "" {
		if err := checkRemote(cfg.Remote); err != nil {
			return nil, err
		}
		p.plan = append(p.plan, KindPushBranch)
	}
	for repo, r := range cfg.RepoRemotes {
		if cfg.Remote == "" {
			return nil, errors.New("publish: a per-repository remote needs a default remote too")
		}
		if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
			return nil, fmt.Errorf("publish: repository %q must be an absolute clean path", repo)
		}
		if err := checkRemote(r); err != nil {
			return nil, err
		}
	}
	if cfg.GitLab.BaseURL != "" {
		if err := checkAPIBase(cfg.GitLab.BaseURL); err != nil {
			return nil, err
		}
		if cfg.GitLab.Project == "" {
			return nil, errors.New("publish: GitLab project is required")
		}
		if p.cfg.GitLab.TargetBranch == "" {
			p.cfg.GitLab.TargetBranch = "main"
		}
		p.plan = append(p.plan, KindDraftPR)
	}
	if cfg.Notifier != nil {
		p.plan = append(p.plan, KindNotify)
	}
	if len(p.plan) == 0 {
		return nil, errors.New("publish: nothing configured (need a remote, GitLab or a notifier)")
	}
	anyRemote := func(prefix string) bool {
		if strings.HasPrefix(cfg.Remote, prefix) {
			return true
		}
		for _, r := range cfg.RepoRemotes {
			if strings.HasPrefix(r, prefix) {
				return true
			}
		}
		return false
	}
	needToken := cfg.GitLab.BaseURL != "" || anyRemote("https://")
	if needToken && cfg.TokenFile == "" {
		return nil, errors.New("publish: a token file is required for GitLab and https remotes")
	}
	if cfg.TokenFile != "" {
		if _, err := ReadToken(cfg.TokenFile); err != nil {
			return nil, err
		}
	}
	if anyRemote("ssh://") {
		if err := checkSSHFiles(cfg.SSHKeyFile, cfg.SSHKnownHosts); err != nil {
			return nil, err
		}
	} else if cfg.SSHKeyFile != "" || cfg.SSHKnownHosts != "" {
		return nil, errors.New("publish: ssh key settings given but the remote is not an ssh:// URL")
	}
	return p, nil
}

// PublishNote is the transition note of the publishing step.
func (p *Publisher) PublishNote() string { return "published: " + strings.Join(p.plan, ", ") }

// Plan returns the action kinds Publish will run, in order.
func (p *Publisher) Plan() []string { return append([]string(nil), p.plan...) }

// Granted reports whether a human granted exactly this task's head for kind
// (the publish_outside_allowlist approval).
func (p *Publisher) Granted(t store.Task, kind string) bool {
	return p.humanApproved(Action{Kind: kind, Task: t.ID, Repo: t.Repo, Base: t.Base, Head: t.HeadSHA})
}

// Publish runs the configured actions for a passed task (steps.Publisher). It
// returns a *NeedsApprovalError for the first action that may not run alone.
// It refuses to start unless EVERY planned action passes the floor and the
// allowlist (or is human-granted), so a plan is never published in part.
func (p *Publisher) Publish(ctx context.Context, t store.Task) error {
	acts := make([]Action, len(p.plan))
	for i, k := range p.plan {
		acts[i] = Action{Kind: k, Task: t.ID, Repo: t.Repo, Base: t.Base, Head: t.HeadSHA, PolicyHash: t.PolicyHash}
		if err := p.check(acts[i]); err != nil {
			return err
		}
	}
	for _, a := range acts {
		if err := p.Do(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) allowlist() policy.Allowlist {
	if p.cfg.AllowSource != nil {
		return p.cfg.AllowSource()
	}
	return p.cfg.Allow
}

// Permit reports whether kind passes the floor and the allowlist as the
// publisher enforces them (nil: it would run without a human).
func (p *Publisher) Permit(kind string) error {
	if err := policy.Floor(kind); err != nil {
		return err
	}
	return p.allowlist().Permit(kind)
}

// check is the gate in front of every action: floor, then allowlist unless a
// human granted exactly this action for this head.
func (p *Publisher) check(a Action) error {
	if err := policy.Floor(a.Kind); err != nil {
		return err
	}
	if a.Task <= 0 {
		return errors.New("publish: action needs a task id")
	}
	if err := p.allowlist().Permit(a.Kind); err != nil {
		if !p.humanApproved(a) {
			return &NeedsApprovalError{Action: a, Hash: approvals.PublishHash(a.Task, a.Head, a.Kind),
				Reason: fmt.Sprintf("%s is not in the allowlist", a.Kind)}
		}
	}
	return nil
}

// remoteFor is the remote a task of repo pushes to.
func (p *Publisher) remoteFor(repo string) string {
	if r, ok := p.cfg.RepoRemotes[repo]; ok {
		return r
	}
	return p.cfg.Remote
}

func (p *Publisher) target(kind, repo string) string {
	switch kind {
	case KindPushBranch:
		return p.remoteFor(repo)
	case KindDraftPR:
		g := p.cfg.GitLab
		return strings.TrimRight(g.BaseURL, "/") + " project=" + g.Project + " target=" + g.TargetBranch
	}
	return ""
}

// Do runs a if and only if the floor allows its kind and the allowlist lists it
// (or a human granted exactly this action for this head). A floor denial
// returns an error wrapping policy.ErrFloor; an unlisted action returns a
// *NeedsApprovalError.
func (p *Publisher) Do(ctx context.Context, a Action) error {
	if err := p.check(a); err != nil {
		return err
	}
	if p.cfg.Ledger != nil {
		err := p.cfg.Ledger.Record("publish_action", map[string]any{"kind": a.Kind, "task": int(a.Task),
			"head": a.Head, "target": p.target(a.Kind, a.Repo), "policy_hash": a.PolicyHash})
		if err != nil {
			return fmt.Errorf("publish: cannot record the action in the ledger, not running it: %w", err)
		}
	}
	err := p.run(ctx, a)
	if p.cfg.Ledger != nil {
		res := map[string]any{"kind": a.Kind, "task": int(a.Task), "head": a.Head, "ok": err == nil}
		if err != nil {
			e := err.Error()
			if len(e) > 300 {
				e = e[:300]
			}
			res["error"] = strings.ToValidUTF8(e, "")
		}
		_ = p.cfg.Ledger.Record("publish_result", res)
	}
	return err
}

func (p *Publisher) run(ctx context.Context, a Action) error {
	switch a.Kind {
	case KindPushBranch:
		return p.push(ctx, a)
	case KindDraftPR:
		return p.draftPR(ctx, a)
	case KindNotify:
		if p.cfg.Notifier == nil {
			return errors.New("publish: no notifier configured")
		}
		return p.cfg.Notifier.Notify(ctx, a.Task, fmt.Sprintf("task %d published", a.Task))
	}
	return fmt.Errorf("publish: unsupported action kind %q", a.Kind)
}

// humanApproved: the grant file holds a grant for exactly this action.
func (p *Publisher) humanApproved(a Action) bool {
	if a.Repo == "" || a.Base == "" || a.Head == "" {
		return false
	}
	id, _, err := gate.RepoID(a.Repo)
	if err != nil {
		return false
	}
	path, err := p.cfg.Grants.Path()
	if err != nil {
		return false
	}
	return approval.Store{Path: path}.Valid(id, a.Base, approvals.PublishHash(a.Task, a.Head, a.Kind))
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RefFor is the only ref a task may be pushed to.
func RefFor(task int64) string { return "refs/heads/orch/" + strconv.FormatInt(task, 10) }

func checkRemote(r string) error {
	switch {
	case strings.HasPrefix(r, "https://"):
		u, err := url.Parse(r)
		if err != nil || u.Host == "" {
			return fmt.Errorf("publish: invalid remote URL")
		}
		if u.User != nil {
			return errors.New("publish: remote URL must not contain credentials")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return errors.New("publish: remote URL must not have a query or fragment")
		}
		return nil
	case strings.HasPrefix(r, "ssh://"):
		u, err := url.Parse(r)
		if err != nil || u.Host == "" || u.Hostname() == "" {
			return fmt.Errorf("publish: invalid remote URL")
		}
		if u.User == nil || u.User.Username() == "" {
			return errors.New("publish: an ssh:// remote needs a user (ssh://git@host/group/project.git)")
		}
		if _, hasPw := u.User.Password(); hasPw {
			return errors.New("publish: remote URL must not contain credentials")
		}
		if u.RawQuery != "" || u.Fragment != "" || strings.HasPrefix(u.Hostname(), "-") || strings.HasPrefix(u.User.Username(), "-") {
			return errors.New("publish: remote URL must not have a query, fragment or option-like parts")
		}
		if u.Path == "" || u.Path == "/" {
			return errors.New("publish: remote URL needs a repository path")
		}
		return nil
	case filepath.IsAbs(r):
		if strings.ContainsAny(r, "\n\r\x00") {
			return errors.New("publish: invalid remote path")
		}
		return nil
	}
	return errors.New("publish: remote must be an absolute local path, an https:// URL or an ssh:// URL")
}

func checkAPIBase(b string) error {
	u, err := url.Parse(b)
	if err != nil || u.Host == "" {
		return errors.New("publish: invalid GitLab URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("publish: GitLab URL must not contain credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback() {
			return nil
		}
	}
	return errors.New("publish: GitLab URL must be https (http only for a loopback host)")
}

// ReadToken reads the token from path. The file must be a regular file owned
// by the current user with no group/other permission bits (0600); the token is
// never part of an error.
func ReadToken(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("publish: cannot open token file %s", path)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("publish: token file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("publish: token file %s has mode %04o; it must be 0600 (no group/other access)", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("publish: token file %s is not owned by the current user", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("publish: token file %s is unreadable or too large", path)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.IndexFunc(tok, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("publish: token file %s must hold one token without whitespace", path)
	}
	return tok, nil
}

func redact(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[token]")
	}
	return s
}

func (p *Publisher) push(ctx context.Context, a Action) error {
	want := RefFor(a.Task)
	if a.Ref != "" && a.Ref != want {
		return ErrBadRef
	}
	if !shaRE.MatchString(a.Head) {
		return errors.New("publish: head is not a commit sha")
	}
	if !filepath.IsAbs(a.Repo) {
		return errors.New("publish: repo must be an absolute path")
	}
	home, err := os.MkdirTemp("", "orch-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file:https:ssh",
	}
	remote := p.remoteFor(a.Repo)
	if strings.HasPrefix(remote, "ssh://") {
		env = append(env, "GIT_SSH_COMMAND="+sshCommand(p.cfg.SSHKeyFile, p.cfg.SSHKnownHosts))
	}
	token := ""
	if strings.HasPrefix(remote, "https://") {
		if token, err = ReadToken(p.cfg.TokenFile); err != nil {
			return err
		}
		ask, err := writeAskpass(home, p.cfg.TokenFile)
		if err != nil {
			return err
		}
		env = append(env, "GIT_ASKPASS="+ask)
	}
	// The refspec is built from a validated sha and a validated int: no
	// wildcard, no leading "+", no other destination.
	refspec := a.Head + ":" + want
	cmd := exec.CommandContext(ctx, p.cfg.Git, "-C", a.Repo,
		"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "http.followRedirects=false",
		"-c", "protocol.ext.allow=never", "-c", "push.default=nothing",
		"push", "--no-verify", "--no-recurse-submodules", "--porcelain", "--", remote, refspec)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := redact(strings.TrimSpace(out.String()), token)
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return fmt.Errorf("publish: git push failed: %v: %s", err, msg)
	}
	return nil
}

// writeAskpass writes a helper git calls for credentials. The token is not in
// the script or in the environment: the script reads the 0600 token file when
// asked. The file is owner-only (0700, it must be executable) in a 0700
// directory and goes away with it.
func writeAskpass(dir, tokenFile string) (string, error) {
	if strings.ContainsAny(tokenFile, "'\n\r\x00") {
		return "", errors.New("publish: token file path has unsupported characters")
	}
	path := filepath.Join(dir, "askpass.sh")
	script := "#!/bin/sh\ncase \"$1\" in\n  *sername*) echo oauth2 ;;\n  *) tr -d '\\r\\n' < '" + tokenFile + "' ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return path, os.Chmod(path, 0o700)
}

func (p *Publisher) draftPR(ctx context.Context, a Action) error {
	token, err := ReadToken(p.cfg.TokenFile)
	if err != nil {
		return err
	}
	g := p.cfg.GitLab
	body, _ := json.Marshal(map[string]any{
		"source_branch": "orch/" + strconv.FormatInt(a.Task, 10),
		"target_branch": g.TargetBranch,
		"title":         fmt.Sprintf("Draft: orch task %d", a.Task),
		"description":   fmt.Sprintf("Automated draft from the orchestrator for task %d (head %s). A human reviews and marks it ready.", a.Task, a.Head),
		"draft":         true,
	})
	u := strings.TrimRight(g.BaseURL, "/") + "/api/v4/projects/" + url.PathEscape(g.Project) + "/merge_requests"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("publish: cannot build the GitLab request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("publish: GitLab request failed: %s", redact(err.Error(), token))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict: // an open MR for this branch already exists: idempotent
		return nil
	}
	return fmt.Errorf("publish: GitLab answered HTTP %d", resp.StatusCode)
}

// LedgerRecorder appends events to the ledger file at path for repo
// policy.OrchRepo (the orchestrator's own ledger).
func LedgerRecorder(path string) Recorder { return fileRecorder(path) }

type fileRecorder string

func (f fileRecorder) Record(typ string, payload map[string]any) error {
	return ledger.Append(string(f), typ, policy.OrchRepo, payload, time.Now().UTC())
}

// sshSafe reports whether a path can sit inside the GIT_SSH_COMMAND string.
func sshSafe(s string) bool {
	return filepath.IsAbs(s) && !strings.ContainsAny(s, " \t'\"\\$`;&|<>(){}*?[]!#~\n\r\x00")
}

// sshCommand is the only ssh git may run for an ssh:// remote: this key only,
// the pinned host key only, never an agent, config file, proxy or prompt.
func sshCommand(key, knownHosts string) string {
	return "ssh -F /dev/null -i " + key + " -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + knownHosts + " -o GlobalKnownHostsFile=/dev/null" +
		" -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ProxyCommand=none -o ControlMaster=no -o ConnectTimeout=20"
}

// checkSSHFiles validates the key (0600, owned by us) and the known_hosts file
// (a regular file nobody else can write).
func checkSSHFiles(key, knownHosts string) error {
	if key == "" || knownHosts == "" {
		return errors.New("publish: an ssh:// remote needs an ssh key file and a known_hosts file")
	}
	for _, f := range []string{key, knownHosts} {
		if !sshSafe(f) {
			return fmt.Errorf("publish: ssh file path %q must be absolute without special characters", f)
		}
	}
	check := func(path string, forbidden os.FileMode, what string) error {
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("publish: %s %s is missing or not a regular file", what, path)
		}
		if fi.Mode().Perm()&forbidden != 0 {
			return fmt.Errorf("publish: %s %s has mode %04o, too open", what, path, fi.Mode().Perm())
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return fmt.Errorf("publish: %s %s is not owned by the current user", what, path)
		}
		return nil
	}
	if err := check(key, 0o077, "ssh key"); err != nil {
		return err
	}
	return check(knownHosts, 0o022, "known_hosts")
}
