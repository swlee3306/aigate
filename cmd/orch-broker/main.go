// Command orch-broker is the service that holds the AI key and the GitLab
// token and serves the orchestrator over a unix socket (one JSON request per
// connection). See internal/broker.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"aigate/internal/broker"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch-broker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "/etc/orch-broker/broker.json", "broker config (root-owned)")
	stateDir := fs.String("state-dir", broker.DefaultStateDir, "directory for budget.json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: orch-broker [-config FILE] [-state-dir DIR]")
		return 2
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
