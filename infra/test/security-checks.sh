#!/bin/sh
# security-checks.sh : security checks of the aigate VMs (design sections 5 and 8, plan Task 13 step 2).
#
#   sh infra/test/security-checks.sh [options]            dry run: prints what it WOULD try, touches nothing
#   sh infra/test/security-checks.sh --run [options]      runs the checks against the real VMs
#
# Options:
#   --admin-key FILE   admin ssh key on this machine (default $HOME/.ssh/aigate_admin_ed25519)
#   --jump USER@HOST   orch-vm as seen from here (default ubuntu@192.168.1.200); VMs are reached through it
#   --orch-key PATH    orchestrator forced-command key, path ON orch-vm (default ~/.ssh/aigate_ed25519)
#   --approvals PATH   approvals file on orch-vm: expects mode 600 in a 700 directory (optional)
#   --db PATH          orchestrator database on orch-vm: same expectation (optional)
#   --socket PATH      orchd socket on orch-vm: expects mode 600 (optional)
#   --report FILE      where to write the table (default infra/security-report.md; --run only)
#
# Like infra/verify-firewall.sh it uses the admin key via the orch-vm ProxyCommand and only makes
# connection attempts and read-only inspections. Exits 0 when every row matches, 1 on any mismatch,
# 2 on bad usage. Checks that need a path flag are reported as SKIP when the flag is not given.
# Rows marked "go test" are covered by Go tests and are NOT executed or faked here.

here=$(cd "$(dirname "$0")" && pwd)
MODE=dry
ADMIN_KEY=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
JUMP=ubuntu@192.168.1.200
ORCH_KEY='$HOME/.ssh/aigate_ed25519'
APPROVALS= DB= SOCKET=
REPORT=$here/../security-report.md

usage() { sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; }
while [ $# -gt 0 ]; do
  case $1 in
    --run) MODE=run ;;
    --admin-key|--jump|--orch-key|--approvals|--db|--socket|--report)
      [ $# -ge 2 ] || { echo "security-checks: $1 needs a value" >&2; exit 2; }
      case $1 in
        --admin-key) ADMIN_KEY=$2 ;; --jump) JUMP=$2 ;; --orch-key) ORCH_KEY=$2 ;;
        --approvals) APPROVALS=$2 ;; --db) DB=$2 ;; --socket) SOCKET=$2 ;; --report) REPORT=$2 ;;
      esac
      shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "security-checks: unknown option $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

JUMP_HOST=${JUMP#*@}
AGENT_IP=10.70.0.12 TEST_IP=10.70.0.13 ORCH_INNER=10.70.0.11
LAN="192.168.1.128:22 192.168.1.129:22 192.168.1.121:22 192.168.1.204:22 192.168.1.251:22 192.168.1.124:22 192.168.1.122:22 10.10.0.31:22 10.10.0.68:22"

# probe helpers, defined on the VM (login shell is bash there, same as verify-firewall.sh)
PROBE='t(){ h=${1%:*}; p=${1#*:}; timeout 3 bash -c "exec 3<>/dev/tcp/$h/$p" 2>/dev/null && echo OPEN || echo CLOSED; }; d(){ c=$(curl -s -o /dev/null -m 6 -w "%{http_code}" https://$1/ 2>/dev/null); [ "${c:-000}" != 000 ] && echo OPEN || echo CLOSED; }'

# run <vm> <command>: execute on the VM with the admin key (the key never leaves this machine)
run() {
  _vm=$1; _cmd=$2
  _o="-i $ADMIN_KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8"
  case $_vm in
    orch-vm) ssh $_o "$JUMP" "$_cmd" ;;
    agent-vm|test-vm)
      _ip=$AGENT_IP; [ "$_vm" = test-vm ] && _ip=$TEST_IP
      ssh $_o -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -o ProxyCommand="ssh -i $ADMIN_KEY -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p $JUMP" \
        ubuntu@$_ip "$_cmd" ;;
  esac
}

fail=0 n=0 rows=
add_row() { rows="$rows
| $1 | $2 | $3 | $4 | $5 |"; }

# check <vm> <description> <expected> <remote command whose LAST output line is the observed value>
check() {
  _vm=$1 _desc=$2 _exp=$3 _cmd=$4
  n=$((n+1))
  if [ "$MODE" = dry ]; then
    printf 'WOULD  %-8s %s\n         expect: %s\n         run   : %s\n' "$_vm" "$_desc" "$_exp" "${_cmd#"$PROBE; "}"
    return
  fi
  _r=$(run "$_vm" "$_cmd" 2>/dev/null | tail -n 1); [ -n "$_r" ] || _r=ERROR
  _mark=ok; [ "$_r" = "$_exp" ] || { _mark=FAIL; fail=1; }
  add_row "$_vm" "$_desc" "$_exp" "$_r" "$_mark"
  echo "$_mark  $_vm: $_desc (expected $_exp, got $_r)"
}
skip() { # <vm> <description> <why>
  n=$((n+1))
  if [ "$MODE" = dry ]; then printf 'SKIP   %-8s %s (%s)\n' "$1" "$2" "$3"; return; fi
  add_row "$1" "$2" "-" "-" "SKIP ($3)"; echo "SKIP  $1: $2 ($3)"
}
gotest() { # <description> <go test reference>
  n=$((n+1))
  if [ "$MODE" = dry ]; then printf 'GO-TEST %s\n         covered by: %s\n' "$1" "$2"; return; fi
  add_row "(go test)" "$1" "pass" "see \`$2\`" "n/a here"; echo "GO-TEST  $1 -> $2"
}
section() { [ "$MODE" = dry ] && printf '\n== %s\n' "$1"; [ "$MODE" = run ] && echo "== $1"; return 0; }

if [ "$MODE" = dry ]; then
  echo "security-checks: DRY RUN (nothing is executed; pass --run to run against the real VMs)"
  echo "admin key: $ADMIN_KEY   jump: $JUMP   VMs: agent-vm $AGENT_IP, test-vm $TEST_IP (via orch-vm)"
else
  [ -r "$ADMIN_KEY" ] || { echo "security-checks: admin key $ADMIN_KEY not readable" >&2; exit 2; }
fi

# ---- network: nothing the agent or the test VM must not reach -------------------------------------
for vm in agent-vm test-vm; do
  section "$vm: network (all must fail)"
  for tgt in $LAN; do check $vm "tcp to LAN host $tgt" CLOSED "$PROBE; t $tgt"; done
  check $vm "tcp to orch-vm $ORCH_INNER:22" CLOSED "$PROBE; t $ORCH_INNER:22"
  if [ $vm = agent-vm ]; then other=test-vm otherip=$TEST_IP; else other=agent-vm otherip=$AGENT_IP; fi
  check $vm "tcp to $other $otherip:22" CLOSED "$PROBE; t $otherip:22"
  check $vm "tcp to 1.1.1.1:443 (raw internet)" CLOSED "$PROBE; t 1.1.1.1:443"
  check $vm "https to non-allow-listed domain example.com" CLOSED "$PROBE; d example.com"
  check $vm "https to non-allow-listed domain github.com" CLOSED "$PROBE; d github.com"
done

# ---- agent-vm: no route or credentials toward the orchestrator ----------------------------------
section "agent-vm: no credentials, no way back to orch-vm"
check agent-vm "agent user has no private key files (id_*, *_ed25519, *_rsa)" 0 \
  "sudo -n sh -c 'find /home/agent -type f \\( -name \"id_*\" -o -name \"*_ed25519\" -o -name \"*_rsa\" -o -name \"*_ecdsa\" \\) ! -name \"*.pub\" 2>/dev/null | wc -l' | tr -d ' '"
check agent-vm "no PRIVATE KEY block anywhere under /home/agent" 0 \
  "sudo -n sh -c 'grep -rIl --exclude-dir=.npm --exclude-dir=.cache --exclude-dir=node_modules --exclude-dir=plugins --exclude-dir=pkg \"PRIVATE KEY\" /home/agent 2>/dev/null | wc -l' | tr -d ' '"
check agent-vm "no known orchestrator state in the agent home (orch-vm address or name)" 0 \
  "sudo -n sh -c 'grep -rIlE \"$ORCH_INNER|orch-vm|approvals.json|orch.db\" /home/agent 2>/dev/null | wc -l' | tr -d ' '"
check agent-vm "agent environment has no GITLAB/TOKEN/SECRET/PASSWORD variable" 0 \
  "sudo -n -u agent env | grep -ciE 'GITLAB|TOKEN|SECRET|PASSWORD|API_KEY' | tr -d ' ' || true"
check agent-vm "agent shell startup files export no GITLAB/TOKEN" 0 \
  "sudo -n sh -c 'cat /home/agent/.profile /home/agent/.bashrc /home/agent/.bash_profile 2>/dev/null | grep -ciE \"GITLAB|TOKEN\"' | tr -d ' ' || true"
check agent-vm "agent user cannot sudo" DENIED \
  "sudo -n -u agent sudo -n true >/dev/null 2>&1 && echo ALLOWED || echo DENIED"
check agent-vm "nothing but authorized_keys and known_hosts in the agent ~/.ssh" 0 \
  "sudo -n sh -c 'ls /home/agent/.ssh/ 2>/dev/null | grep -vcE \"^(authorized_keys|known_hosts)\$\" || true' | tr -d ' '"

# ---- test-vm: no credentials, no sudo for runner ------------------------------------------------
section "test-vm: no credentials, runner is unprivileged"
check test-vm "no *token*, *.env or id_* files under the runner home" 0 \
  "sudo -n sh -c 'find /home/runner \\( -path /home/runner/go/pkg -o -path /home/runner/.config/go/telemetry -o -path /home/runner/.npm -o -path /home/runner/.cache -o -path /home/runner/.local/share/vitest \\) -prune -o \\( -iname \"*token*\" -o -name \"*.env\" -o -name \"id_*\" \\) -print 2>/dev/null | wc -l' | tr -d ' '"
check test-vm "no ~/.config/aigate and no ~/.aigate for runner" 0 \
  "sudo -n sh -c 'ls -d /home/runner/.config/aigate /home/runner/.aigate 2>/dev/null | wc -l' | tr -d ' '"
check test-vm "no PRIVATE KEY block anywhere under /home/runner" 0 \
  "sudo -n sh -c 'grep -rIl --exclude-dir=.npm --exclude-dir=.cache --exclude-dir=node_modules --exclude-dir=plugins --exclude-dir=pkg \"PRIVATE KEY\" /home/runner 2>/dev/null | wc -l' | tr -d ' '"
check test-vm "runner environment has no GITLAB/TOKEN/SECRET/PASSWORD variable" 0 \
  "sudo -n -u runner env | grep -ciE 'GITLAB|TOKEN|SECRET|PASSWORD|API_KEY' | tr -d ' ' || true"
check test-vm "sudo -n true fails for runner" DENIED \
  "sudo -n -u runner sudo -n true >/dev/null 2>&1 && echo ALLOWED || echo DENIED"

# ---- agent-vm sshd: only the forced command ------------------------------------------------------
section "agent-vm sshd accepts only the forced command (tried from orch-vm with the orchestrator key)"
SSHA="sudo -n -u orch ssh -i $ORCH_KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR agent@$AGENT_IP"
check orch-vm "non-listed command 'id' with the orchestrator key is rejected" REJECTED \
  "o=\$($SSHA id 2>&1); case \$o in *uid=*) echo EXECUTED;; *'command not allowed'*) echo REJECTED;; *) echo \"OTHER\";; esac"
check orch-vm "shell metacharacters are rejected ('prepare 1; id')" REJECTED \
  "o=\$($SSHA 'prepare 1; id' 2>&1 </dev/null); case \$o in *uid=*) echo EXECUTED;; *'command not allowed'*) echo REJECTED;; *) echo \"OTHER\";; esac"
check orch-vm "no command at all (interactive shell request) gives no shell" REJECTED \
  "o=\$($SSHA 2>&1 </dev/null); case \$o in *uid=*) echo EXECUTED;; *'command not allowed'*) echo REJECTED;; *) echo \"OTHER\";; esac"
check orch-vm "port forwarding through the forced-command key is refused" REFUSED \
  "sudo -n -u orch ssh -i $ORCH_KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ExitOnForwardFailure=yes -N -L 127.0.0.1:2299:127.0.0.1:22 agent@$AGENT_IP </dev/null >/dev/null 2>&1 & p=\$!; sleep 4; if kill -0 \$p 2>/dev/null; then if timeout 4 bash -c 'exec 3<>/dev/tcp/127.0.0.1/2299; head -c 20 <&3' 2>/dev/null | grep -q ^SSH-; then echo FORWARDING; else echo REFUSED; fi; kill \$p 2>/dev/null; sudo -n -u orch pkill -f 'L 127.0.0.1:2299' 2>/dev/null; else echo REFUSED; fi"

# ---- orch-vm: private files -----------------------------------------------------------------------
section "orch-vm: modes of the orchestrator's private files"
modecheck() { # <label> <path> <what> <file mode> <dir mode> ; empty path => SKIP
  if [ -z "$2" ]; then skip orch-vm "$1 mode" "no --$3 given"; return; fi
  if [ "$MODE" = run ] && [ "$(run orch-vm "sudo -n -u orch test -e '$2' && echo YES || echo NO" 2>/dev/null | tail -n 1)" = NO ]; then
    skip orch-vm "$1 $2 mode" "file does not exist yet (nothing has been written there)"; return
  fi
  check orch-vm "$1 $2 has mode $4" "$4" "sudo -n -u orch stat -c %a '$2'"
  [ -n "$5" ] && check orch-vm "directory of $1 has mode $5" "$5" "sudo -n -u orch stat -c %a \"\$(dirname '$2')\""
}
modecheck "approvals file" "$APPROVALS" approvals 600 700
modecheck "database" "$DB" db 600 700
modecheck "orchd socket" "$SOCKET" socket 600 ""

# ---- covered by Go tests (not executed here) ------------------------------------------------------
section "covered by Go tests (run: go test ./internal/orch/... ./internal/remote/... -count=1)"
gotest "huge commit history from the agent is refused at collect" "internal/orch/vm TestCollectRejectsHugeHistory"
gotest "huge objects from the agent are refused at collect" "internal/orch/vm TestCollectRejectsHugeObjects"
gotest "extra refs (many refs) from the agent are refused at collect" "internal/orch/vm TestCollectRejectsExtraRefs"
gotest "oversize or bad-ref bundle is refused at prepare" "internal/orch/vm TestPrepareRejectsOversizeBundle, TestPrepareRejectsBadBundleRefs"
gotest "forged JUnit/report from the test VM is not a passing verdict" "internal/remote TestForgedJUnitRejected, TestOversizedOutputRejected"
gotest "a report that is not for this task's head is an error, never passed" "internal/orch/machine TestFault_CorruptedOrMissingReportIsErrorNotPassed"
gotest "approval files and the database must be private (0600/0700)" "internal/orch/approvals CheckPrivate tests"

if [ "$MODE" = dry ]; then
  echo
  echo "security-checks: dry run listed $n checks; nothing was executed and no report was written."
  exit 0
fi

mkdir -p "$(dirname "$REPORT")"
{
  echo "# Security checks report"
  echo
  echo "Generated $(date '+%Y-%m-%d %H:%M %Z') by infra/test/security-checks.sh (admin key via orch-vm; connection attempts and read-only inspections only)."
  echo
  echo "| vm | check | expected | observed | |"
  echo "|---|---|---|---|---|"
  printf '%s\n' "$rows" | sed '1{/^$/d;}'
  echo
  echo "Result: $([ $fail = 0 ] && echo PASS || echo FAIL)"
} > "$REPORT"
echo "report: $REPORT"
exit $fail
