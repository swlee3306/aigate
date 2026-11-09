#!/bin/sh
# Local checks for broker-init.sh and the orch-broker unit. Touches nothing outside a temp dir.
set -u
d=$(cd "$(dirname "$0")" && pwd); fail=0
ok() { echo "ok   $1"; }; bad() { echo "FAIL $1"; fail=1; }
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
sh -n "$d/broker-init.sh" && ok "sh -n broker-init.sh" || bad "sh -n broker-init.sh"
sh -n "$d/test-broker-scripts.sh" && ok "sh -n test-broker-scripts.sh" || bad "sh -n test-broker-scripts.sh"
env="BROKER_ETC_DIR=$t/etc/orch-broker BROKER_STATE_DIR=$t/var/orch-broker"
out=$(env $env sh "$d/broker-init.sh" 2>&1); rc=$?
[ $rc = 0 ] && ok "dry run exits 0" || bad "dry run rc=$rc"
[ ! -e "$t/etc" ] && [ ! -e "$t/var" ] && ok "dry run created nothing" || bad "dry run created files"
echo "$out" | grep -q 'install -m 0400 -o orch-broker' && ok "dry run prints the key install instruction" || bad "key instruction missing"
echo "$out" | grep -q 'useradd --system --no-create-home' && ok "dry run prints the account plan" || bad "account plan missing"
sh "$d/broker-init.sh" --bogus >/dev/null 2>&1 && bad "unknown flag accepted" || ok "unknown flag refused"
if [ "$(id -u)" != 0 ]; then
  env $env sh "$d/broker-init.sh" --apply >/dev/null 2>&1 && bad "--apply as non-root succeeded" || ok "--apply as non-root refused"
  [ ! -e "$t/etc" ] && [ ! -e "$t/var" ] && ok "refused --apply changed nothing" || bad "refused --apply created files"
else echo "skip non-root --apply test (running as root)"; fi
# the script must never write a key or token file: no redirection/touch/cp/tee/install aimed at *.key or *.token
if grep -nE '(>|>>|touch|tee|cp|mv|dd of=)[[:space:]]*[^#]*(openai\.key|gitlab\.token|\.key|\.token)' "$d/broker-init.sh" | grep -v 'echo'; then bad "script may write a key or token file"; else ok "script never writes key or token files"; fi
# unit hardening lines
u="$d/systemd/orch-broker.service"
for l in 'User=orch-broker' 'NoNewPrivileges=yes' 'ProtectSystem=strict' 'ProtectHome=yes' 'PrivateTmp=yes' 'ProtectProc=invisible' 'ProcSubset=pid' \
  'ReadWritePaths=/var/lib/orch-broker /run/orch-broker' 'RuntimeDirectory=orch-broker' 'StateDirectory=orch-broker' \
  'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6' 'CapabilityBoundingSet=' 'MemoryDenyWriteExecute=yes' 'SystemCallFilter=@system-service'; do
  grep -qxF "$l" "$u" && ok "unit has $l" || bad "unit lacks $l"
done
grep -qE '^CapabilityBoundingSet=$' "$u" && ok "capability bounding set is empty" || bad "capability bounding set not empty"
exit $fail
