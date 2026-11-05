package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aigate/internal/orch/api"
)

const defaultSocket = "/run/orchd/orchd.sock"

const usage = `사용법: orch [--socket PATH] [--json] <명령>

  submit --repo R --base B --prompt P --class C --agent claude|codex   작업 제출
  list                                                              작업 목록
  show ID                                                           작업 상세
  cancel ID                                                         작업 취소

소켓: --socket 또는 ORCHD_SOCKET (기본 ` + defaultSocket + `)`

type client struct {
	hc *http.Client
}

func newClient(sock string) *client {
	return &client{hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}}
}

// call returns the raw body; a non-2xx answer becomes an error with the
// server's message.
func (c *client) call(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://orchd"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("orchd에 연결할 수 없습니다: %w", unwrapDial(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("서버 오류 (HTTP %d)", resp.StatusCode)
	}
	return b, nil
}

func unwrapDial(err error) error {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Err
	}
	return err
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := fs.String("socket", envOr("ORCHD_SOCKET", defaultSocket), "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	rest := fs.Args()
	cmd, rest := rest[0], rest[1:]
	c := newClient(*sock)

	// --json is also accepted after the command.
	sub := flag.NewFlagSet(cmd, flag.ContinueOnError)
	sub.SetOutput(io.Discard)
	sub.BoolVar(asJSON, "json", *asJSON, "")
	var repo, base, prompt, class, agent string
	if cmd == "submit" {
		sub.StringVar(&repo, "repo", "", "")
		sub.StringVar(&base, "base", "", "")
		sub.StringVar(&prompt, "prompt", "", "")
		sub.StringVar(&class, "class", "", "")
		sub.StringVar(&agent, "agent", "", "")
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "오류:", err); return 1 }
	if err := sub.Parse(rest); err != nil {
		fmt.Fprintln(stderr, "오류:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	out := func(raw []byte, human func()) int {
		if *asJSON {
			stdout.Write(raw)
			if !bytes.HasSuffix(raw, []byte("\n")) {
				fmt.Fprintln(stdout)
			}
			return 0
		}
		human()
		return 0
	}
	idArg := func() (string, error) {
		if sub.NArg() != 1 {
			return "", fmt.Errorf("%s에는 작업 ID 하나가 필요합니다", cmd)
		}
		if n, err := strconv.ParseInt(sub.Arg(0), 10, 64); err != nil || n <= 0 {
			return "", fmt.Errorf("작업 ID가 올바르지 않습니다: %s", sub.Arg(0))
		}
		return sub.Arg(0), nil
	}

	switch cmd {
	case "submit":
		var missing []string
		for _, f := range [][2]string{{"repo", repo}, {"base", base}, {"prompt", prompt}, {"class", class}, {"agent", agent}} {
			if f[1] == "" {
				missing = append(missing, "--"+f[0])
			}
		}
		if len(missing) > 0 {
			return fail(fmt.Errorf("필수 옵션이 없습니다: %s", strings.Join(missing, ", ")))
		}
		raw, err := c.call("POST", "/tasks", map[string]string{
			"repo": repo, "base": base, "prompt": prompt, "class": class, "agent": agent})
		if err != nil {
			return fail(err)
		}
		return out(raw, func() {
			var v api.TaskView
			json.Unmarshal(raw, &v)
			fmt.Fprintf(stdout, "작업 #%d 제출됨 (상태: %s)\n", v.ID, v.State)
		})
	case "list":
		raw, err := c.call("GET", "/tasks", nil)
		if err != nil {
			return fail(err)
		}
		return out(raw, func() {
			var vs []api.TaskView
			json.Unmarshal(raw, &vs)
			if len(vs) == 0 {
				fmt.Fprintln(stdout, "작업이 없습니다.")
				return
			}
			fmt.Fprintf(stdout, "%-5s %-14s %-7s %-12s %s\n", "ID", "상태", "에이전트", "종류", "저장소")
			for _, v := range vs {
				fmt.Fprintf(stdout, "%-5d %-14s %-9s %-12s %s\n", v.ID, v.State, v.Agent, v.Class, v.Repo)
			}
		})
	case "show":
		id, err := idArg()
		if err != nil {
			return fail(err)
		}
		raw, err := c.call("GET", "/tasks/"+id, nil)
		if err != nil {
			return fail(err)
		}
		return out(raw, func() {
			var v api.TaskView
			json.Unmarshal(raw, &v)
			fmt.Fprintf(stdout, "작업 #%d\n  상태:     %s\n  저장소:   %s\n  기준:     %s\n  종류:     %s\n  에이전트: %s\n  시도:     %d\n  생성:     %s\n  갱신:     %s\n",
				v.ID, v.State, v.Repo, v.Base, v.Class, v.Agent, v.Attempts,
				v.CreatedAt.Local().Format(time.DateTime), v.UpdatedAt.Local().Format(time.DateTime))
			if v.Error != "" {
				fmt.Fprintf(stdout, "  오류:     %s\n", v.Error)
			}
			fmt.Fprintf(stdout, "  프롬프트:\n%s\n", indent(v.Prompt))
		})
	case "cancel":
		id, err := idArg()
		if err != nil {
			return fail(err)
		}
		raw, err := c.call("POST", "/tasks/"+id+"/cancel", nil)
		if err != nil {
			return fail(err)
		}
		return out(raw, func() { fmt.Fprintf(stdout, "작업 #%s 취소됨\n", id) })
	default:
		fmt.Fprintf(stderr, "알 수 없는 명령: %s\n%s\n", cmd, usage)
		return 2
	}
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
