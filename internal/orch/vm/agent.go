// Package vm drives the two untrusted VMs from the orchestrator: AgentVM (runs
// Claude/Codex on a bundle of the repository) and TestVM (restored from a
// clean snapshot, runs the checks through the Task 4 ssh executor).
//
// Direction of connections: the orchestrator always connects out. Nothing on
// either VM connects back; the agent VM has no way to write to the
// orchestrator. Everything an VM returns is untrusted input and is
// size-capped and strictly parsed here.
//
// Open decision (NOT decided here): OpenStackCLI reaches the homeserver's
// admin OpenStack credentials over ssh (default `ssh openstack-host openstack
// --os-cloud exporter-internal`). Giving orch-vm that path is a privilege
// concern, because orch-vm is the machine whose compromise "breaks
// everything" (docs/07 section 8). Least privilege would be a restricted
// OpenStack user for the orchestrator that can only touch test-vm's server,
// volumes and snapshot. OpenStackCLI limits itself with a verb and name
// allow-list, but that is a guard against bugs, not against a compromised
// orch-vm that holds the ssh path.
//
// Open issue (needs the real VM): a restore creates a new server, and
// cloud-init regenerates the ssh host keys for a new instance id, so the
// pinned known_hosts of test-vm would no longer match. Either keep host keys
// across restores (cloud-init `ssh_deletekeys: false` baked into the clean
// snapshot) or re-pin them from the console log after each restore.
package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigate/internal/remote"
)

// Collection and transfer limits. The caps can be tightened, not loosened,
// for the byte limits that the wrapper enforces as well.
const (
	DefaultMaxCommits = 100
	DefaultMaxBytes   = 50 << 20
	PrepareMaxBytes   = 200 << 20 // bundle size, same as the wrapper's PREP_MAX
	PromptMaxBytes    = 64 << 10
	TailMaxBytes      = 64 << 10
)

var (
	// ErrNoCommit means the agent left no commit on its task branch.
	ErrNoCommit = errors.New("vm: agent made no commit")
	// ErrOverLimit means the agent's result exceeds the commit or size limit;
	// it is rejected (before fetching when the summary already shows it).
	ErrOverLimit = errors.New("vm: agent result exceeds limits")

	taskRE = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// RunResult is what the wrapper reports for one agent run.
type RunResult struct {
	RC        int
	TimedOut  bool
	Committed bool // the wrapper itself committed leftover changes
	Tail      string
	Seconds   float64
}

// AgentVM is the agent VM reached through the restricted forced-command key.
type AgentVM struct {
	Host, KeyPath string
	User          string // default "agent"
	KnownHosts    string // required: host keys are always verified
	JumpHost      string
	SSHBin        string
	// ConnectTimeout bounds the ssh handshake, Grace is added to Run's limit.
	ConnectTimeout, Grace time.Duration
	// MaxCommits (default 100) and MaxBytes (default 50 MiB) bound what Collect accepts.
	MaxCommits int
	MaxBytes   int64
	// FetchTimeout bounds the git fetch (default 5m).
	FetchTimeout time.Duration
}

func (a *AgentVM) ssh() (*remote.SSH, error) {
	user := a.User
	if user == "" {
		user = "agent"
	}
	return remote.NewSSH(remote.Config{
		Host: a.Host, User: user, KeyPath: a.KeyPath, KnownHosts: a.KnownHosts,
		RepoDir: "/unused", JumpHost: a.JumpHost, SSHBin: a.SSHBin,
		ConnectTimeout: a.ConnectTimeout, Grace: a.Grace,
	})
}

func checkTask(task string) error {
	if !taskRE.MatchString(task) {
		return fmt.Errorf("vm: invalid task id %q", task)
	}
	return nil
}

// Prepare sends a git bundle of baseSHA from repo and has the wrapper reset
// the task workspace to it (branch task-<task>, hooks disabled).
func (a *AgentVM) Prepare(ctx context.Context, task, repo, baseSHA string) error {
	if err := checkTask(task); err != nil {
		return err
	}
	ex, err := a.ssh()
	if err != nil {
		return err
	}
	path, cleanup, err := remote.MakeBundle(repo, []string{baseSHA})
	if err != nil {
		return err
	}
	defer cleanup()
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() > PrepareMaxBytes {
		return fmt.Errorf("vm: bundle is %d bytes, over the %d byte limit", st.Size(), PrepareMaxBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute+graceOf(a.Grace))
	defer cancel()
	out, err := ex.Call(cctx, "prepare "+task, f, 256, 512)
	if err != nil {
		return fmt.Errorf("agent prepare: %w", err)
	}
	if want := "AGENT-RUNNER-PREPARE-OK\nbase=" + baseSHA + "\n"; string(out) != want {
		return fmt.Errorf("agent prepare: unexpected reply %q", remote.Clip(out))
	}
	return nil
}

func graceOf(g time.Duration) time.Duration {
	if g <= 0 {
		return 60 * time.Second
	}
	return g
}

// Run sends the prompt to `agent-runner run <task> <agent>`. limit bounds this
// call (plus Grace); the process-level timeout that kills the agent is the
// wrapper's own RUN_TIMEOUT, which the caller cannot change.
func (a *AgentVM) Run(ctx context.Context, task, agent, prompt string, limit time.Duration) (RunResult, error) {
	if err := checkTask(task); err != nil {
		return RunResult{}, err
	}
	if agent != "claude" && agent != "codex" {
		return RunResult{}, fmt.Errorf("vm: unknown agent %q", agent)
	}
	if len(prompt) == 0 || len(prompt) > PromptMaxBytes {
		return RunResult{}, fmt.Errorf("vm: prompt must be 1..%d bytes, got %d", PromptMaxBytes, len(prompt))
	}
	if limit <= 0 {
		return RunResult{}, errors.New("vm: run limit must be positive")
	}
	ex, err := a.ssh()
	if err != nil {
		return RunResult{}, err
	}
	budget := limit + graceOf(a.Grace)
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	out, err := ex.Call(cctx, "run "+task+" "+agent, strings.NewReader(prompt), TailMaxBytes+4096, 512)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return RunResult{}, fmt.Errorf("agent run: no reply within %v", budget)
		}
		return RunResult{}, fmt.Errorf("agent run: %w", err)
	}
	res, err := parseRun(out)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent run: %w", err)
	}
	res.Seconds = time.Since(start).Seconds()
	return res, nil
}

// parseRun checks the frame:
//
//	AGENT-RUNNER-RUN1\nrc=N\ntimedout=0|1\ncommitted=0|1\ntail=N\n<N bytes>\nEND\n
func parseRun(b []byte) (RunResult, error) {
	var r RunResult
	line := func() (string, bool) {
		i := strings.IndexByte(string(b[:min(len(b), 64)]), '\n')
		if i < 0 {
			return "", false
		}
		s := string(b[:i])
		b = b[i+1:]
		return s, true
	}
	num := func(name string, lo, hi int) (int, error) {
		s, ok := line()
		v, found := strings.CutPrefix(s, name+"=")
		if !ok || !found {
			return 0, fmt.Errorf("malformed reply: missing %s", name)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi || strconv.Itoa(n) != v {
			return 0, fmt.Errorf("malformed reply: bad %s %q", name, v)
		}
		return n, nil
	}
	if s, ok := line(); !ok || s != "AGENT-RUNNER-RUN1" {
		return r, errors.New("malformed reply: no header")
	}
	rc, err := num("rc", 0, 255)
	if err != nil {
		return r, err
	}
	to, err := num("timedout", 0, 1)
	if err != nil {
		return r, err
	}
	cm, err := num("committed", 0, 1)
	if err != nil {
		return r, err
	}
	tl, err := num("tail", 0, TailMaxBytes)
	if err != nil {
		return r, err
	}
	if len(b) < tl+1 || b[tl] != '\n' {
		return r, errors.New("malformed reply: short tail")
	}
	r.RC, r.TimedOut, r.Committed, r.Tail = rc, to == 1, cm == 1, string(b[:tl])
	b = b[tl+1:]
	if s, ok := line(); !ok || s != "END" || len(b) != 0 {
		return r, errors.New("malformed reply: bad trailer")
	}
	return r, nil
}

type info struct {
	head, base string
	commits    int
	bytes      int64
}

// parseInfo checks AGENT-RUNNER-INFO1\nhead=<sha>\nbase=<sha>\ncommits=N\nbytes=N|-1\nEND\n
func parseInfo(b []byte) (info, error) {
	var in info
	lines := strings.Split(string(b), "\n")
	if len(lines) != 7 || lines[0] != "AGENT-RUNNER-INFO1" || lines[5] != "END" || lines[6] != "" {
		return in, errors.New("malformed collect-info reply")
	}
	get := func(i int, key string) (string, error) {
		v, ok := strings.CutPrefix(lines[i], key+"=")
		if !ok {
			return "", fmt.Errorf("malformed collect-info reply: missing %s", key)
		}
		return v, nil
	}
	var err error
	if in.head, err = get(1, "head"); err != nil {
		return in, err
	}
	if in.base, err = get(2, "base"); err != nil {
		return in, err
	}
	if !remote.ValidSHA(in.head) || !remote.ValidSHA(in.base) {
		return in, errors.New("malformed collect-info reply: bad commit id")
	}
	c, err := get(3, "commits")
	if err != nil {
		return in, err
	}
	if in.commits, err = strconv.Atoi(c); err != nil || in.commits < 0 || strconv.Itoa(in.commits) != c {
		return in, fmt.Errorf("malformed collect-info reply: bad commits %q", c)
	}
	by, err := get(4, "bytes")
	if err != nil {
		return in, err
	}
	if in.bytes, err = strconv.ParseInt(by, 10, 64); err != nil || in.bytes < -1 || strconv.FormatInt(in.bytes, 10) != by {
		return in, fmt.Errorf("malformed collect-info reply: bad bytes %q", by)
	}
	return in, nil
}

// Collect brings the agent's commits home. baseSHA is the task's own base
// (never the VM's idea of it): commits and size are counted locally over
// baseSHA..fetched head. It asks collect-info first and
// rejects before fetching when the commit count or size is over the limit or
// there is no commit. The fetch then goes over ssh to the wrapper's
// upload-pack subcommand (read-only, advertises only refs/heads/task-<task>)
// into a scratch repository, with fsck on, hooks off, only the ssh protocol
// allowed, no tags, and the scratch directory's growth capped. Only after the
// result is re-checked locally are the objects copied into localRepo, and the
// only ref written there is refs/orch/<task>/head.
func (a *AgentVM) Collect(ctx context.Context, task, localRepo, baseSHA string) (headSHA string, commits int, err error) {
	if err := checkTask(task); err != nil {
		return "", 0, err
	}
	if !remote.ValidSHA(baseSHA) {
		return "", 0, fmt.Errorf("vm: collect needs the task's base commit sha, got %q", baseSHA)
	}
	maxCommits, maxBytes := a.MaxCommits, a.MaxBytes
	if maxCommits <= 0 {
		maxCommits = DefaultMaxCommits
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if strings.Contains(a.Host, ":") {
		return "", 0, fmt.Errorf("vm: host %q cannot be used in a git ssh url", a.Host)
	}
	ex, err := a.ssh()
	if err != nil {
		return "", 0, err
	}
	raw, err := ex.Call(ctx, "collect-info "+task, nil, 1024, 512)
	if err != nil {
		return "", 0, fmt.Errorf("agent collect-info: %w", err)
	}
	in, err := parseInfo(raw)
	if err != nil {
		return "", 0, err
	}
	// The base file lives in the agent's workspace and the agent can rewrite
	// it: only the task's own base counts. A different one is a tampered
	// reply, not a result.
	if in.base != baseSHA {
		return "", 0, fmt.Errorf("agent collect-info reports base %s, not the task's base %s", in.base, baseSHA)
	}
	if in.commits == 0 || in.head == in.base {
		return "", 0, ErrNoCommit
	}
	if in.commits > maxCommits {
		return "", 0, fmt.Errorf("%w: %d commits (limit %d)", ErrOverLimit, in.commits, maxCommits)
	}
	if in.bytes < 0 || in.bytes > maxBytes {
		return "", 0, fmt.Errorf("%w: %d bytes (limit %d)", ErrOverLimit, in.bytes, maxBytes)
	}
	repo, err := filepath.Abs(localRepo)
	if err != nil {
		return "", 0, err
	}
	if _, err := gitRun(ctx, nil, repo, "cat-file", "-e", in.base+"^{commit}"); err != nil {
		return "", 0, fmt.Errorf("vm: base commit %s is not in %s", in.base, repo)
	}
	scratch, err := os.MkdirTemp("", "aigate-collect-")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(scratch)
	if _, err := gitRun(ctx, nil, scratch, "init", "-q", "--bare", "."); err != nil {
		return "", 0, err
	}
	objs, err := gitRun(ctx, nil, repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objs+"\n"), 0o600); err != nil {
		return "", 0, err
	}
	// the scratch repo already has base (through alternates): advertise it as "have" so only new objects travel
	if _, err := gitRun(ctx, nil, scratch, "update-ref", "refs/have/base", in.base); err != nil {
		return "", 0, err
	}

	ref := "refs/orch/" + task + "/head"
	branch := "refs/heads/task-" + task
	fetchTimeout := a.FetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = 5 * time.Minute
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	grew := watchSize(fctx, cancel, filepath.Join(scratch, "objects"), maxBytes+maxBytes/2+(1<<20))
	sshCmd := remote.Quote(ex.Bin())
	for _, o := range ex.SSHOptions() {
		sshCmd += " " + remote.Quote(o)
	}
	env := append(gitEnvFor(), "GIT_SSH_COMMAND="+sshCmd, "GIT_SSH_VARIANT=ssh")
	_, ferr := gitRun(fctx, env, scratch,
		"-c", "protocol.allow=never", "-c", "protocol.ssh.allow=always", "-c", "protocol.version=0",
		"-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true", "-c", "fetch.recurseSubmodules=false",
		"fetch", "-q", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		"--upload-pack=upload-pack", "ssh://"+ex.Host()+"/"+task, "+"+branch+":"+ref)
	if grew() {
		return "", 0, fmt.Errorf("%w: the pack grew past the size limit", ErrOverLimit)
	}
	if ferr != nil {
		return "", 0, fmt.Errorf("agent fetch: %w", ferr)
	}
	got, err := gitRun(ctx, nil, scratch, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || !remote.ValidSHA(got) {
		return "", 0, fmt.Errorf("agent fetch: no commit fetched for %s", branch)
	}
	// re-verify what actually arrived; the VM may have changed since collect-info
	if _, err := gitRun(ctx, nil, scratch, "merge-base", "--is-ancestor", in.base, got); err != nil {
		return "", 0, errors.New("agent result does not descend from the base commit")
	}
	cnt, err := gitRun(ctx, nil, scratch, "rev-list", "--count", "--max-count="+strconv.Itoa(maxCommits+1), in.base+".."+got)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(cnt)
	if err != nil {
		return "", 0, fmt.Errorf("vm: bad commit count %q", cnt)
	}
	if n == 0 {
		return "", 0, ErrNoCommit
	}
	if n > maxCommits {
		return "", 0, fmt.Errorf("%w: more than %d commits", ErrOverLimit, maxCommits)
	}
	if sz := dirSize(filepath.Join(scratch, "objects")); sz > maxBytes {
		return "", 0, fmt.Errorf("%w: %d bytes (limit %d)", ErrOverLimit, sz, maxBytes)
	}
	// copy into the real repository: local only, fsck again, one ref in our own namespace
	lenv := gitEnvFor()
	if _, err := gitRun(ctx, lenv, repo,
		"-c", "protocol.allow=never", "-c", "protocol.file.allow=always",
		"-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
		"fetch", "-q", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head",
		scratch, "+"+ref+":"+ref); err != nil {
		return "", 0, fmt.Errorf("agent collect: %w", err)
	}
	return got, n, nil
}

// gitEnvFor is remote.GitEnv without user/system configuration.
func gitEnvFor() []string {
	return append(remote.GitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
}

// gitRun runs git with hooks and fsmonitor off in dir (empty env means the
// allowlisted one) and returns trimmed stdout.
func gitRun(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	if env == nil {
		env = gitEnvFor()
	}
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "gc.auto=0"}, args...)
	if dir != "" {
		full = append([]string{"-C", dir}, full...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr limitedBuf
	stdout.max, stderr.max = 1<<20, 4096
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", firstNonFlag(args), err, remote.Clip(stderr.b))
	}
	return strings.TrimSpace(string(stdout.b)), nil
}

func firstNonFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

type limitedBuf struct {
	b   []byte
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if room := l.max - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuf)(nil)

// watchSize cancels the fetch once dir grows past limit bytes. The returned
// func reports whether that happened.
func watchSize(ctx context.Context, cancel context.CancelFunc, dir string, limit int64) func() bool {
	over := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if dirSize(dir) > limit {
					close(over)
					cancel()
					return
				}
			}
		}
	}()
	return func() bool {
		cancel() // stops the watcher if the fetch ended on its own
		<-done
		select {
		case <-over:
			return true
		default:
			return false
		}
	}
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
