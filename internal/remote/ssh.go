// Package remote runs gate checks on a separate test VM over ssh. The VM and
// everything it sends back are untrusted: replies are framed, size-capped and
// validated here, and a bad reply is an infrastructure error, never a verdict.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigate/internal/gate"
)

// Config describes the test VM. RepoDir must be an absolute path of the bare
// repository on the VM; WorkDir (default /tmp) holds per-check temp dirs.
type Config struct {
	Host, User, KeyPath, RepoDir string
	// KnownHosts is the known_hosts file the host key is checked against.
	KnownHosts string
	WorkDir    string
	// ConnectTimeout bounds the ssh handshake (default 10s). Grace is added to
	// the check timeouts for the whole ssh call (default 60s).
	ConnectTimeout, Grace time.Duration
	// SSHBin overrides the ssh binary; empty means $AIGATE_SSH_BIN, then "ssh".
	SSHBin string
	Limits Limits
}

// SSH implements gate.Executor by running checks on a VM over ssh.
type SSH struct {
	cfg Config
	bin string
}

var (
	hostRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	userRE   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	envRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	junitRef = regexp.MustCompile(`\$\{AIGATE_JUNIT\}|\$AIGATE_JUNIT\b`)
)

// NewSSH validates cfg. It does not connect.
func NewSSH(cfg Config) (*SSH, error) {
	if !hostRE.MatchString(cfg.Host) {
		return nil, fmt.Errorf("remote: invalid host %q", cfg.Host)
	}
	if !userRE.MatchString(cfg.User) {
		return nil, fmt.Errorf("remote: invalid user %q", cfg.User)
	}
	for name, p := range map[string]string{"key": cfg.KeyPath, "known_hosts": cfg.KnownHosts} {
		if p == "" || strings.ContainsAny(p, " \t\n\"'\\%") || strings.HasPrefix(p, "-") {
			return nil, fmt.Errorf("remote: invalid %s path %q", name, p)
		}
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/tmp"
	}
	for name, p := range map[string]string{"RepoDir": cfg.RepoDir, "WorkDir": cfg.WorkDir} {
		if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
			return nil, fmt.Errorf("remote: %s must be an absolute path, got %q", name, p)
		}
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 60 * time.Second
	}
	cfg.Limits = cfg.Limits.norm()
	bin := cfg.SSHBin
	if bin == "" {
		bin = os.Getenv("AIGATE_SSH_BIN")
	}
	if bin == "" {
		bin = "ssh"
	}
	return &SSH{cfg: cfg, bin: bin}, nil
}

// sshArgs is every ssh option, ending with the target host. Host keys are
// always verified against the configured known_hosts file; there is no option
// that relaxes that. Only the configured key is offered; agent, X11 and port
// forwarding are off and ~/.ssh/config is not read.
func (e *SSH) sshArgs() []string {
	secs := int((e.cfg.ConnectTimeout + time.Second - 1) / time.Second)
	return []string{
		"-F", "/dev/null", "-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + e.cfg.KnownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=" + strconv.Itoa(secs),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-i", e.cfg.KeyPath,
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "LogLevel=ERROR",
		"-l", e.cfg.User,
		e.cfg.Host,
	}
}

// shq is the only place arguments are escaped for the remote shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remoteCmd is `bash -c <wrapper> aigate-remote <mode> <args...>`, every word
// escaped by shq. No argument is ever spliced into script text.
func remoteCmd(mode string, args []string) (string, error) {
	words := append([]string{"bash", "-c", wrapper, "aigate-remote", mode}, args...)
	for i, w := range words {
		if strings.ContainsRune(w, 0) {
			return "", errors.New("remote: argument contains a NUL byte")
		}
		words[i] = shq(w)
	}
	return strings.Join(words, " "), nil
}

// Prepare sends a git bundle of shas over ssh stdin and unpacks it into the
// bare repository at RepoDir, creating it if needed.
func (e *SSH) Prepare(ctx context.Context, repo string, shas []string) error {
	path, cleanup, err := makeBundle(repo, shas)
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd, err := remoteCmd("prep", append([]string{e.cfg.RepoDir}, shas...))
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute+e.cfg.Grace)
	defer cancel()
	out, err := e.call(cctx, cmd, f, 256, 512)
	if err != nil {
		return fmt.Errorf("remote prepare: %w", err)
	}
	if string(out) != "AIGATE-PREP-OK\n" {
		return fmt.Errorf("remote prepare: unexpected reply %q", clip(out))
	}
	return nil
}

// Close has nothing to release: every call is its own ssh process.
func (e *SSH) Close() error { return nil }

// RunCheck runs req.Setup then req.Argv in a fresh worktree of req.SHA on the
// VM and returns what the wrapper framed. Any malformed or over-limit reply is
// an error.
func (e *SSH) RunCheck(ctx context.Context, req gate.CheckRequest) (gate.CheckOutput, error) {
	switch req.Adapter {
	case "junit", "gotest-json", "exit-code":
	default:
		return gate.CheckOutput{}, fmt.Errorf("check %q: unknown adapter %q", req.CheckID, req.Adapter)
	}
	if !shaRE.MatchString(req.SHA) {
		return gate.CheckOutput{}, fmt.Errorf("remote: %q is not a full commit id", req.SHA)
	}
	for _, argv := range [][]string{req.Setup, req.Argv} {
		if len(argv) > 0 && (argv[0] == "" || strings.HasPrefix(argv[0], "-") || strings.Contains(argv[0], "=")) {
			return gate.CheckOutput{}, fmt.Errorf("check %q: unsupported command name %q", req.CheckID, argv[0])
		}
	}
	if len(req.Argv) == 0 {
		return gate.CheckOutput{}, fmt.Errorf("check %q: empty command", req.CheckID)
	}
	var rb [8]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return gate.CheckOutput{}, err
	}
	rid := hex.EncodeToString(rb[:])
	env := append([]string{}, req.Env...)
	argv := req.Argv
	if req.Adapter == "junit" {
		out := e.cfg.WorkDir + "/aigate-" + rid + "/out"
		env = append(env, "AIGATE_JUNIT="+out)
		argv = make([]string, len(req.Argv))
		for i, a := range req.Argv {
			argv[i] = junitRef.ReplaceAllLiteralString(a, out)
		}
	}
	for _, kv := range env {
		if !envRE.MatchString(kv) {
			return gate.CheckOutput{}, fmt.Errorf("check %q: invalid env entry %q", req.CheckID, kv)
		}
	}
	lim := e.cfg.Limits
	resultMax := 0
	switch req.Adapter {
	case "junit":
		resultMax = lim.JUnitBytes
	case "gotest-json":
		resultMax = lim.JSONBytes
	}
	tmo, budget := "0", e.cfg.Grace
	if req.Timeout > 0 {
		tmo = strconv.FormatFloat(req.Timeout.Seconds(), 'f', 3, 64)
		n := 1
		if len(req.Setup) > 0 {
			n = 2
		}
		budget += time.Duration(n) * req.Timeout
	}
	args := []string{e.cfg.RepoDir, e.cfg.WorkDir, rid, req.SHA, req.Adapter, tmo,
		strconv.Itoa(resultMax), strconv.Itoa(lim.TailBytes),
		strconv.Itoa(len(req.Setup)), strconv.Itoa(len(env)), strconv.Itoa(len(argv))}
	args = append(append(append(args, req.Setup...), env...), argv...)
	cmd, err := remoteCmd("run", args)
	if err != nil {
		return gate.CheckOutput{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	reply, err := e.call(cctx, cmd, nil, lim.maxReply(), 512)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return gate.CheckOutput{}, fmt.Errorf("remote check %q: no reply within %v", req.CheckID, budget)
		}
		return gate.CheckOutput{}, fmt.Errorf("remote check %q: %w", req.CheckID, err)
	}
	out, err := parseReply(reply, req.Adapter, resultMax, lim.TailBytes)
	if err != nil {
		return gate.CheckOutput{}, fmt.Errorf("remote check %q: %w", req.CheckID, err)
	}
	out.Seconds = time.Since(start).Seconds()
	return out, nil
}

// call runs one ssh command and returns its stdout, at most maxOut bytes: the
// call is cancelled and fails the moment more arrives. Exit 255 (ssh's own
// failure) and any non-zero exit are errors.
func (e *SSH) call(ctx context.Context, remote string, stdin io.Reader, maxOut, maxErr int) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, append(e.sshArgs(), "--", remote)...)
	cmd.Stdin = stdin
	cmd.WaitDelay = 2 * time.Second
	stdout := &capBuf{max: maxOut, cancel: cancel}
	stderr := &capBuf{max: maxErr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.over {
		return nil, fmt.Errorf("reply exceeds %d bytes", stdout.max)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			kind := "remote command failed"
			if ee.ExitCode() == 255 {
				kind = "ssh connection failed"
			}
			return nil, fmt.Errorf("%s (exit %d): %s", kind, ee.ExitCode(), clip(stderr.b.Bytes()))
		}
		return nil, fmt.Errorf("ssh: %w", err)
	}
	return stdout.b.Bytes(), nil
}

// capBuf keeps at most max bytes. With a cancel func (stdout) one byte more
// is fatal and cancels the call, so an endless stream never grows memory;
// without one (stderr) the head is kept and the rest dropped.
type capBuf struct {
	b      bytes.Buffer
	max    int
	over   bool
	cancel context.CancelFunc
}

func (c *capBuf) Write(p []byte) (int, error) {
	room := c.max - c.b.Len()
	if len(p) <= room {
		return c.b.Write(p)
	}
	c.b.Write(p[:room])
	if c.cancel == nil {
		return len(p), nil
	}
	c.over = true
	c.cancel()
	return room, errors.New("output over limit")
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// parseReply validates the wrapper's framing:
//
//	AIGATE1\nrc=N\ntimedout=0|1\nsetupfailed=0|1\ntail=N\n<N bytes>\nresult=absent|N\n[<N bytes>\n]END\n
//
// and nothing after END. resultMax is the cap for the adapter's result.
func parseReply(b []byte, adapter string, resultMax, tailMax int) (gate.CheckOutput, error) {
	var out gate.CheckOutput
	bad := func(why string) (gate.CheckOutput, error) {
		return gate.CheckOutput{}, fmt.Errorf("malformed reply: %s", why)
	}
	line := func() (string, bool) {
		i := bytes.IndexByte(b, '\n')
		if i < 0 || i > 64 {
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
			return 0, fmt.Errorf("missing %s", name)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi || strconv.Itoa(n) != v {
			return 0, fmt.Errorf("bad %s %q", name, v)
		}
		return n, nil
	}
	if s, ok := line(); !ok || s != "AIGATE1" {
		return bad("no header")
	}
	rc, err := num("rc", 0, 255)
	if err != nil {
		return bad(err.Error())
	}
	to, err := num("timedout", 0, 1)
	if err != nil {
		return bad(err.Error())
	}
	sf, err := num("setupfailed", 0, 1)
	if err != nil {
		return bad(err.Error())
	}
	tl, err := num("tail", 0, tailMax)
	if err != nil {
		return bad(err.Error())
	}
	if len(b) < tl+1 || b[tl] != '\n' {
		return bad("short tail")
	}
	out.RC, out.TimedOut, out.SetupFailed, out.Tail = rc, to == 1, sf == 1, string(b[:tl])
	b = b[tl+1:]
	rs, ok := line()
	rv, found := strings.CutPrefix(rs, "result=")
	if !ok || !found {
		return bad("missing result")
	}
	var res []byte
	if rv != "absent" {
		n, err := strconv.Atoi(rv)
		if err != nil || n < 0 || strconv.Itoa(n) != rv {
			return bad("bad result length " + rv)
		}
		if n > resultMax {
			return gate.CheckOutput{}, fmt.Errorf("check output exceeds %d bytes", resultMax)
		}
		if len(b) < n+1 || b[n] != '\n' {
			return bad("short result")
		}
		res = append([]byte{}, b[:n]...)
		b = b[n+1:]
	}
	if s, ok := line(); !ok || s != "END" || len(b) != 0 {
		return bad("bad trailer")
	}
	if res != nil {
		if adapter == "exit-code" || out.TimedOut || out.SetupFailed || out.RC == 127 {
			return bad("result present for a run that cannot have one")
		}
		if adapter == "junit" {
			out.JUnit = res
		} else {
			out.GoTestJSON = res
		}
	}
	return out, nil
}

// wrapper is the one fixed script run on the VM (bash: it needs job control to
// kill whole process groups, and arrays). Arguments arrive only as positional
// parameters. prep: stdin is a bundle, unpacked into the bare repo $2; the
// commits $3.. must then exist. run: check out $5 of bare repo $2 into a temp
// dir under $3, run setup then the command with output bounded to the last
// $9 bytes, a hard timeout that kills the process group, and the result file
// read through head -c limit+1; reply with the frame parseReply checks.
const wrapper = `set -m
mode=$1
case $mode in
prep)
	repo=$2
	shift 2
	umask 077
	[ -d "$repo" ] || git init --bare -q "$repo" >&2 || exit 3
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/aigate-prep.XXXXXX") || exit 3
	trap 'rm -rf "$tmp"' EXIT
	cat > "$tmp/b" || exit 3
	git -C "$repo" fetch -q --no-tags "$tmp/b" '+refs/aigate/*:refs/aigate/*' >&2 || exit 4
	for s in "$@"; do git -C "$repo" cat-file -e "$s^{commit}" || exit 5; done
	echo AIGATE-PREP-OK
	exit 0
	;;
run) ;;
*) exit 2 ;;
esac
repo=$2 work=$3 rid=$4 sha=$5 adapter=$6 tmo=$7 lim=$8 tailmax=$9
ns=${10} ne=${11} na=${12}
shift 12
setup=("${@:1:$ns}")
envv=("${@:$((ns+1)):$ne}")
argv=("${@:$((ns+ne+1)):$na}")
umask 077
mkdir -p "$work" && d="$work/aigate-$rid" && mkdir "$d" || exit 3
cleanup() {
	cd /
	git -C "$repo" worktree remove --force "$d/wt" >/dev/null 2>&1
	git -C "$repo" worktree prune >/dev/null 2>&1
	rm -rf "$d"
}
trap cleanup EXIT
git -C "$repo" -c core.hooksPath=/dev/null worktree add -q --detach "$d/wt" "$sha" >/dev/null || exit 6
cd "$d/wt" || exit 6
exec 2>/dev/null
phase() {
	local kind=$1 r2= wd= g=
	shift
	rm -f "$d/ot" "$d/so" "$d/to" "$d/tail"
	mkfifo "$d/ot" || exit 3
	tail -c "$tailmax" < "$d/ot" > "$d/tail" &
	local r1=$!
	if [ "$kind" = json ]; then
		mkfifo "$d/so" || exit 3
		( tee "$d/ot" < "$d/so" | head -c "$((lim+1))" > "$d/out" ) >/dev/null 2>&1 &
		r2=$!
		env "$@" < /dev/null > "$d/so" 2> "$d/ot" &
	else
		env "$@" < /dev/null > "$d/ot" 2>&1 &
	fi
	local pid=$!
	if [ "$tmo" != 0 ]; then
		( sleep "$tmo"; : > "$d/to"; kill -s KILL -- "-$pid" ) >/dev/null 2>&1 < /dev/null &
		wd=$!
	fi
	wait "$pid"
	prc=$?
	[ -n "$wd" ] && kill -s KILL -- "-$wd" >/dev/null 2>&1
	kill -s KILL -- "-$pid" >/dev/null 2>&1
	( sleep 5; kill -s KILL -- "-$r1" ${r2:+"-$r2"} ) >/dev/null 2>&1 < /dev/null &
	g=$!
	wait "$r1"
	[ -n "$r2" ] && wait "$r2"
	kill -s KILL -- "-$g" >/dev/null 2>&1
	pto=0
	if [ -f "$d/to" ] && [ "$prc" -eq 137 ]; then
		pto=1
		prc=124
	fi
	: >> "$d/tail"
}
emit() {
	printf 'AIGATE1\nrc=%s\ntimedout=%s\nsetupfailed=%s\n' "$1" "$2" "$3"
	printf 'tail=%s\n' "$(wc -c < "$d/tail" | tr -d ' ')"
	cat "$d/tail"
	printf '\n'
	if [ "$4" = present ]; then
		printf 'result=%s\n' "$(wc -c < "$d/res" | tr -d ' ')"
		cat "$d/res"
		printf '\n'
	else
		printf 'result=absent\n'
	fi
	printf 'END\n'
}
if [ "$ns" -gt 0 ]; then
	phase plain "${setup[@]}"
	if [ "$pto" = 1 ] || [ "$prc" -ne 0 ]; then
		emit "$prc" "$pto" 1 absent
		exit 0
	fi
fi
kind=plain
[ "$adapter" = gotest-json ] && kind=json
rm -f "$d/out"
phase "$kind" "${envv[@]}" "${argv[@]}"
have=absent
if [ "$pto" != 1 ] && [ "$prc" -ne 127 ] && { [ "$adapter" = junit ] || [ "$adapter" = gotest-json ]; } && [ -f "$d/out" ] && [ ! -L "$d/out" ]; then
	head -c "$((lim+1))" < "$d/out" > "$d/res" && have=present
fi
emit "$prc" "$pto" 0 "$have"
exit 0
`
