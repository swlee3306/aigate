package main

import (
	"bytes"
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
	"aigate/internal/orch/machine"
)

const defaultSocket = "/run/orchd/orchd.sock"

const usage = `사용법: orch [--socket PATH | --server URL [--token-file PATH]] [--json] <명령>

  submit --repo R --base B --prompt P --class C --agent claude|codex   작업 제출
  list                                                              작업 목록
  show ID                                                           작업 상세
  cancel ID                                                         작업 취소
  retry ID                                                          인프라 오류로 실패한(failed) 작업을 실패한 단계부터 다시 시도(최대 3회)
  approvals                                                         승인 대기 목록
  approve [--sign-key KEY] [--signer ID] ITEM                       승인(사람이 터미널에서 직접, TTY 필요). ITEM은 approvals의 ID 열(승인 항목 번호, 작업 번호 아님)
                                                                    서명 필수 모드면 KEY(기본 ~/.ssh/id_ed25519)로 서명. ID는 --signer 또는 AIGATE_SIGNER
  reverify ITEM                                                     이미 승인한 항목의 재검증 요청(승인 기록이 있어야 함). ITEM은 approvals의 ID 열

` + policyUsage + `

` + adminUsage + `

소켓: --socket 또는 ORCHD_SOCKET (기본 ` + defaultSocket + `)
원격: --server https://주소 (소켓과 함께 쓸 수 없음), 토큰은 --token-file(기본 ~/.config/orch/token, 권한 0600)
승인 기록 위치: --gate-home 또는 ORCH_GATE_HOME (orchd의 --gate-home과 같아야 함)`

type client struct {
	hc    *http.Client
	base  string                 // scheme://host, no API prefix
	token func() (string, error) // remote mode only
	err   error                  // construction error, reported by call
}

// call returns the raw body; a non-2xx answer becomes an error with the
// server's message. Paths are relative to /api/v1.
func (c *client) call(method, path string, body any) ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+apiPrefix+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != nil {
		tok, err := c.token()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if c.token != nil {
			return nil, fmt.Errorf("orchd 서버에 연결할 수 없습니다: %w", unwrapDial(err))
		}
		return nil, fmt.Errorf("orchd에 연결할 수 없습니다: %w", unwrapDial(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &apiError{Status: resp.StatusCode, Code: errorCode(b), Message: errorMessage(b)}
	}
	return b, nil
}

// apiError is a non-2xx answer. Error() is the server's message (or the HTTP
// status), as before. Code is the v1 envelope's code: callers that must not
// echo server text (CF1, the signed grant) switch on it instead.
type apiError struct {
	Status        int
	Code, Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("서버 오류 (HTTP %d)", e.Status)
}

// errorCode reads the v1 envelope's {"error":{"code"}}; "" when absent.
func errorCode(b []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) != nil {
		return ""
	}
	return env.Error.Code
}

// errorMessage reads the v1 envelope {"error":{"message"}}, falling back to the
// legacy flat {"error":"msg"}; "" when the body is neither.
func errorMessage(b []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	var flat struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &flat) == nil {
		return flat.Error
	}
	return ""
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
	server := fs.String("server", "", "")
	tokenFile := fs.String("token-file", "", "")
	asJSON := fs.Bool("json", false, "")
	gateHomeFlag := fs.String("gate-home", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	rest := fs.Args()
	cmd, rest := rest[0], rest[1:]
	if cmd == "propose" || cmd == "policy" {
		return localCmd(cmd, rest, *asJSON, *gateHomeFlag, stdout, stderr)
	}
	if cmd == "admin" {
		return cmdAdmin(rest, stdout, stderr)
	}
	var c *client
	if *server != "" {
		socketGiven := false
		fs.Visit(func(f *flag.Flag) { socketGiven = socketGiven || f.Name == "socket" })
		if socketGiven {
			fmt.Fprintln(stderr, "오류: --server와 --socket은 함께 쓸 수 없습니다")
			return 2
		}
		tf := *tokenFile
		if tf == "" {
			tf = defaultTokenFile()
		}
		c = newRemoteClient(*server, tokenSource(tf))
	} else {
		c = newClient(*sock)
	}

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
	var so signOpts
	if cmd == "approve" {
		sub.StringVar(&so.KeyPath, "sign-key", "", "")
		sub.StringVar(&so.Signer, "signer", "", "")
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "오류:", errText(err)); return 1 }
	if err := sub.Parse(rest); err != nil {
		fmt.Fprintln(stderr, "오류:", errText(err))
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
		// approve and reverify take an APPROVAL ITEM id (the ID column of `orch approvals`),
		// every other command takes a TASK id: say which, it is an easy mix-up.
		what := "작업 ID"
		if cmd == "approve" || cmd == "reverify" {
			what = "승인 항목 ID(`orch approvals`의 ID 열, 작업 번호가 아님)"
		}
		if sub.NArg() != 1 {
			return "", fmt.Errorf("%s에는 %s 하나가 필요합니다", cmd, what)
		}
		if n, err := strconv.ParseInt(sub.Arg(0), 10, 64); err != nil || n <= 0 {
			return "", fmt.Errorf("%s가 올바르지 않습니다: %s", what, sub.Arg(0))
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
				state := v.State
				if v.Retryable {
					state += " (재시도 가능)"
				}
				fmt.Fprintf(stdout, "%-5d %-14s %-9s %-12s %s\n", v.ID, state, v.Agent, v.Class, v.Repo)
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
			if v.Retryable {
				fmt.Fprintf(stdout, "  재시도:   가능 (%s부터, 지금까지 %d/%d회) — orch retry %d\n", v.RetryState, v.Retries, machine.MaxRetries, v.ID)
			} else if v.Retries > 0 {
				fmt.Fprintf(stdout, "  재시도:   %d/%d회 사용\n", v.Retries, machine.MaxRetries)
			}
			fmt.Fprintf(stdout, "  프롬프트:\n%s\n", indent(v.Prompt))
		})
	case "retry":
		id, err := idArg()
		if err != nil {
			return fail(err)
		}
		raw, err := c.call("POST", "/tasks/"+id+"/retry", nil)
		if err != nil {
			return fail(err)
		}
		return out(raw, func() {
			var v api.TaskView
			json.Unmarshal(raw, &v)
			fmt.Fprintf(stdout, "작업 #%s 다시 시도: %s부터\n", id, v.State)
		})
	case "approvals":
		raw, err := c.call("GET", "/approvals", nil)
		if err != nil {
			return fail(err)
		}
		return out(raw, func() {
			var vs []api.ApprovalView
			json.Unmarshal(raw, &vs)
			if len(vs) == 0 {
				fmt.Fprintln(stdout, "승인 대기 항목이 없습니다.")
				return
			}
			fmt.Fprintf(stdout, "%-5s %-5s %-26s %-19s %-12s %s\n", "ID", "작업", "종류", "생성", "해시", "이유")
			for _, v := range vs {
				fmt.Fprintf(stdout, "%-5d %-5d %-26s %-19s %-12s %s\n", v.ID, v.TaskID, v.Kind,
					v.CreatedAt.Local().Format(time.DateTime), short(v.Hash), safeLine(v.Reason))
			}
		})
	case "approve":
		id, err := idArg()
		if err != nil {
			return fail(err)
		}
		return cmdApprove(c, id, *gateHomeFlag, so, stdout, stderr)
	case "reverify":
		id, err := idArg()
		if err != nil {
			return fail(err)
		}
		return reverify(c, id, stdout, stderr)
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
