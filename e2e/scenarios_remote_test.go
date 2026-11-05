package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aigate/internal/gate"
	"aigate/internal/rules"
)

// Hostile-VM scenarios (plan task 5). The judge runs on this side; the VM and
// everything it returns are untrusted. They always go through the fakessh
// backend (whatever AIGATE_E2E_BACKEND says) and use its AIGATE_FAKE_MODE
// knobs for a VM that lies about the transport. Whatever happens, the outcome
// is never ok: either the gate says verification_incomplete, or the exchange is
// refused as a gate error (no verdict, no report, non-zero exit).

const (
	outIncomplete = "verification_incomplete"
	outGateError  = "gate_error"
)

func TestRemoteHostileVM(t *testing.T) {
	forged := `printf '<html><body>all tests passed</body></html>' > "$AIGATE_JUNIT"`
	cases := []struct {
		name, mode, adapter, cmd string
		timeout                  string
		want                     string
		note                     string
	}{
		{name: "remote/forged_junit_not_xml_schema", adapter: "junit", cmd: forged, want: outIncomplete},
		{name: "remote/forged_junit_empty_file", adapter: "junit", cmd: `: > "$AIGATE_JUNIT"`, want: outIncomplete},
		{name: "remote/malformed_xml_truncated", adapter: "junit", want: outIncomplete,
			cmd: `printf '<testsuite><testcase classname="c" name="a"' > "$AIGATE_JUNIT"`},
		{name: "remote/malformed_xml_garbage", adapter: "junit", cmd: `head -c 4096 /dev/urandom > "$AIGATE_JUNIT"`, want: outIncomplete},
		{name: "remote/malformed_gotest_json", adapter: "gotest-json", cmd: `printf '{"Action":"pass","Test"'`, want: outIncomplete},
		{name: "remote/no_result_file_rc0", adapter: "junit", cmd: `exit 0`, want: outIncomplete},
		{name: "remote/rc_lie_vm_says_0_no_result", mode: "rc_lie", adapter: "junit", cmd: `exit 1`, want: outIncomplete},
		{name: "remote/endless_output_until_timeout", adapter: "junit", timeout: "2s", want: outIncomplete,
			cmd: `while :; do echo endless-output-line-0123456789 >&2; done`},
		{name: "remote/oversized_junit_report", adapter: "junit", cmd: `head -c 9437184 /dev/zero > "$AIGATE_JUNIT"`, want: outGateError,
			note: "over-limit result is refused as a gate error, not a verdict (same on the local executor)"},
		{name: "remote/oversized_gotest_json_stdout", adapter: "gotest-json", cmd: `head -c 9437184 /dev/zero`, want: outGateError},
		{name: "remote/endless_stdout_gotest_json", adapter: "gotest-json", cmd: `while :; do echo endless-stdout; done`, want: outGateError},
		{name: "remote/vm_floods_reply", mode: "flood", adapter: "junit", cmd: `exit 0`, want: outGateError},
		{name: "remote/vm_cuts_reply", mode: "cutframe", adapter: "junit", cmd: `exit 0`, want: outGateError},
		{name: "remote/vm_declares_huge_result", mode: "bigframe", adapter: "junit", cmd: `exit 0`, want: outGateError},
		{name: "remote/vm_sends_more_than_declared", mode: "lenlie", adapter: "junit", cmd: `exit 0`, want: outGateError},
		{name: "remote/vm_unreachable", mode: "unreachable", adapter: "junit", cmd: `exit 0`, want: outGateError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout == "" {
				tc.timeout = "30s"
			}
			yaml := "version: 1\nprotected:\n  - \".aigate.yml\"\nchecks:\n  - id: u\n    adapter: " + tc.adapter +
				"\n    command: [\"sh\", \"-c\", " + strconv.Quote(tc.cmd) + "]\n    timeout: " + tc.timeout + "\n    required: true\n"
			w := NewWorkspace(t, "", yaml)
			w.Write("f", "head\n")
			w.Head = w.Commit("head")
			t.Setenv("AIGATE_FAKE_MODE", tc.mode)
			ex := newSSHExecutor(t, fakeSSHConfig(t))
			r, err := gate.Run(context.Background(), gate.Options{Repo: w.Dir, Base: w.Base, Head: w.Head, HomeDir: w.Home, Executor: ex})

			got := outIncomplete
			switch {
			case err != nil:
				got = outGateError
			case r.Category == rules.CategoryOK:
				got = string(r.Category)
			case r.Category != rules.CategoryIncomplete:
				got = string(r.Category)
			}
			res := Result{Stack: "remote", Name: tc.name, Want: tc.want, Got: got, Match: got == tc.want, Source: "self", Note: tc.note}
			if got == "ok" || (err == nil && r.ExitCode == 0) {
				t.Errorf("hostile VM produced an ok: %+v", r)
			}
			if got != tc.want {
				t.Errorf("outcome: want %s, got %s (err %v, report %+v)", tc.want, got, err, r.Violations)
			}
			// no report may claim ok anywhere, and a gate error writes none
			reports, _ := filepath.Glob(filepath.Join(w.Dir, ".aigate", "reports", "*.json"))
			for _, p := range reports {
				b, _ := os.ReadFile(p)
				if err != nil {
					t.Errorf("gate error but a report was written: %s", p)
				}
				if strings.Contains(string(b), `"category": "ok"`) {
					t.Errorf("report %s says ok", p)
				}
			}
			Record(res)
		})
	}
}
