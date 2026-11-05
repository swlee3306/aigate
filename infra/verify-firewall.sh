#!/usr/bin/env bash
# verify-firewall.sh : tries connections from each VM and compares with the expected result.
# Writes infra/firewall-report.md; exits 1 if any row differs from the expectation.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd); K=$HOME/.ssh/aigate_admin_ed25519
J="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@192.168.1.200"
run() { # <vm> <script...>: run a bash script (stdin) on the VM, admin key never leaves this Mac
  local vm=$1 ip; shift
  case $vm in orch-vm) ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 ubuntu@192.168.1.200 "$@" ;;
    agent-vm|test-vm) ip=10.70.0.12; [ $vm = test-vm ] && ip=10.70.0.13
      ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$J" ubuntu@$ip "$@" ;; esac; }
probe='t(){ h=${1%:*}; p=${1#*:}; timeout 3 bash -c "exec 3<>/dev/tcp/$h/$p" 2>/dev/null && echo OPEN || echo CLOSED; }; d(){ c=$(curl -s -o /dev/null -m 6 -w "%{http_code}" https://$1/ 2>/dev/null); [ "${c:-000}" != 000 ] && echo OPEN || echo CLOSED; }'
fail=0; out=$here/firewall-report.md
{ echo "# Firewall verification report"; echo; echo "Generated $(date '+%Y-%m-%d %H:%M %Z') by infra/verify-firewall.sh (admin key via orch-vm; connection attempts only)."; echo
  echo "| from | target | expected | observed | |"; echo "|---|---|---|---|---|"; } > "$out"
check() { # vm kind target expected
  local r; r=$(run "$1" "$probe; $2 $3" 2>/dev/null | tail -1); [ -n "$r" ] || r=ERROR
  local mark=ok; [ "$r" = "$4" ] || { mark=FAIL; fail=1; }
  echo "| $1 | $3 | $4 | $r | $mark |" >> "$out"; echo "$mark  $1 -> $3 (expected $4, got $r)"; }
LAN="192.168.1.128:22 192.168.1.129:22 192.168.1.121:22 192.168.1.204:22 192.168.1.251:22 192.168.1.124:22 192.168.1.122:22 10.10.0.31:22 10.10.0.68:22"
for vm in agent-vm test-vm; do
  for t in $LAN 10.70.0.11:22 1.1.1.1:443 8.8.8.8:443; do check $vm t $t CLOSED; done
  for o in agent-vm test-vm; do [ $o != $vm ] && check $vm t "$([ $o = agent-vm ] && echo 10.70.0.12 || echo 10.70.0.13):22" CLOSED; done
done
for d in api.anthropic.com api.openai.com; do check agent-vm d $d OPEN; done
for d in example.com github.com proxy.golang.org; do check agent-vm d $d CLOSED; done
for d in proxy.golang.org registry.npmjs.org pypi.org; do check test-vm d $d OPEN; done
for d in example.com github.com api.anthropic.com; do check test-vm d $d CLOSED; done
for t in 10.70.0.12:22 10.70.0.13:22 1.1.1.1:443; do check orch-vm t $t OPEN; done
for t in $LAN; do check orch-vm t $t CLOSED; done
{ echo; echo "Result: $([ $fail = 0 ] && echo PASS || echo FAIL)"; } >> "$out"
echo "report: $out"; exit $fail
