// Command orch-broker is the service that holds the AI key and the GitLab
// token and serves the orchestrator over a unix socket (one JSON request per
// connection). See internal/broker.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"

	"aigate/internal/broker"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch-broker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "/etc/orch-broker/broker.json", "broker config (root-owned)")
	stateDir := fs.String("state-dir", broker.DefaultStateDir, "directory for budget.json")
	check := fs.Bool("check", false, "check config, trust and key modes, then exit (no network, nothing written)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch-broker [-check] [-config FILE] [-state-dir DIR]")
		return 2
	}
	if *check {
		return checkConfig(*cfgPath, *stateDir, checkOptions{}, stderr)
	}
	cfg, err := broker.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "orch-broker: 시작 거부: %v\n", err)
		return 1
	}
	srv, err := broker.New(cfg, broker.Options{StateDir: *stateDir, Log: stderr})
	if err != nil {
		fmt.Fprintf(stderr, "orch-broker: 시작 거부: %v\n", err)
		return 1
	}
	ln, err := srv.Listen()
	if err != nil {
		fmt.Fprintf(stderr, "orch-broker: 시작 거부: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx, ln); err != nil {
		fmt.Fprintln(stderr, "orch-broker: 서버가 멈췄습니다")
		return 1
	}
	return 0
}

// checkOptions are the test seams of checkConfig (zero value = production).
type checkOptions struct {
	Load       broker.LoadOptions
	KeyOwnerOK func(uint32) bool
}

// checkConfig is `orch-broker -check`: it applies exactly the start-up
// refusals that need no network (config trust and format, key and token file
// trust and modes, the socket group, the state directory) and then stops. It
// builds no HTTP client, opens no connection, creates no socket and writes
// nothing (the budget file is not opened). Every message is a fixed sentence;
// none names a secret or quotes a file's content.
func checkConfig(cfgPath, stateDir string, o checkOptions, out io.Writer) int {
	refuse := func(err error) int {
		fmt.Fprintf(out, "orch-broker: 점검 실패: %v\n", err)
		return 1
	}
	cfg, err := broker.LoadConfigOpts(cfgPath, o.Load)
	if err != nil {
		return refuse(err)
	}
	if _, err := broker.LoadSecrets(cfg, o.KeyOwnerOK); err != nil {
		return refuse(err)
	}
	if _, err := user.LookupGroup(cfg.SocketGroup); err != nil {
		return refuse(errors.New("socket_group을 확인할 수 없습니다"))
	}
	if fi, err := os.Stat(stateDir); err != nil || !fi.IsDir() {
		return refuse(errors.New("상태 폴더(state-dir)가 없거나 폴더가 아닙니다"))
	}
	fmt.Fprintln(out, "orch-broker: 점검 통과 (설정·신뢰·키 모드·소켓 그룹·상태 폴더만 확인했습니다. 외부 접속은 하지 않았습니다)")
	return 0
}
