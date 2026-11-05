#!/usr/bin/env bash
# Dry-run / idempotency / allow-list checks for infra/create-vms.sh using a fake ssh.
set -u
D=$(cd "$(dirname "$0")" && pwd)
INFRA=$(cd "$D/.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir -p "$T/home/.ssh"
echo "ssh-ed25519 AAAATESTKEYONLY aigate-orchestrator" > "$T/home/.ssh/aigate_ed25519.pub"
export AIGATE_KEY="$T/home/.ssh/aigate_ed25519"
export PATH="$D/shim:$PATH"
fails=0
ok()   { echo "  ok   - $1"; }
bad()  { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

WRITE_RE='^(create|add|set|delete|remove|unset|purge|reboot|stop|start|rebuild|resize|migrate|lock|unlock)$'
only_reads() { # $1 = log; every line must be an openstack list/show
  awk -v re="$WRITE_RE" '
    { if ($1!="openstack" || $2!="--os-cloud" || $3!="exporter-internal") { print "foreign: " $0; bad=1; next }
      verbs=""; for (i=4;i<=NF;i++){ if ($i ~ /^-/) break; verbs=verbs" "$i; if ($i ~ re) { print "write: " $0; bad=1 } }
      if (verbs !~ / (list|show)( |$)/) { print "not read: " $0; bad=1 } }
    END { exit bad }' "$1"
}
plan_of() { grep -E '^\[[0-9]+\] ' | sed -E 's/^\[[0-9]+\] //'; }

echo "[dry-run, empty cloud]"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log1" bash "$INFRA/create-vms.sh" > "$T/out1" 2>"$T/err1"; rc=$?
assert "exit 0" "[ $rc -eq 0 ]"
assert "log non-empty (read queries happened)" "[ -s '$T/log1' ]"
assert "only read verbs reached ssh" "only_reads '$T/log1'"
cat > "$T/expected1" <<'X'
CREATE keypair aigate-key
CREATE flavor aigate.orch
CREATE flavor aigate.agent
CREATE flavor aigate.test
CREATE network aigate-net
CREATE subnet aigate-subnet
CREATE router-interface router-main aigate-subnet
CREATE volume orch-vm-root
CREATE volume agent-vm-root
CREATE volume test-vm-root
CREATE server orch-vm
CREATE server agent-vm
CREATE server test-vm
CREATE floating-ip 192.168.1.200
ATTACH floating-ip 192.168.1.200 orch-vm
X
plan_of < "$T/out1" > "$T/plan1"
assert "plan lists exactly the expected resources in order" "diff '$T/expected1' '$T/plan1' >/dev/null"
diff "$T/expected1" "$T/plan1" | head -5
for s in '--vcpus 1 --ram 2048 --disk 0 aigate.orch' '--vcpus 2 --ram 4096 --disk 0 aigate.agent' '--vcpus 4 --ram 6144 --disk 0 aigate.test' \
  '--subnet-range 10.70.0.0/24' '--gateway 10.70.0.1' '--dns-nameserver 8.8.8.8' 'router add subnet router-main aigate-subnet' \
  "--image 'Ubuntu 24.04' --size 10 orch-vm-root" "--image 'Ubuntu 24.04' --size 20 agent-vm-root" "--image 'Ubuntu 24.04' --size 30 test-vm-root" \
  '--flavor aigate.orch --volume orch-vm-root' '--flavor aigate.agent --volume agent-vm-root' '--flavor aigate.test --volume test-vm-root' \
  'v4-fixed-ip=10.70.0.11' 'v4-fixed-ip=10.70.0.12' 'v4-fixed-ip=10.70.0.13' '--key-name aigate-key' '--user-data /dev/stdin' \
  'floating ip create --floating-ip-address 192.168.1.200 external-net' 'server add floating ip orch-vm 192.168.1.200' \
  'keypair create --public-key /dev/stdin aigate-key'; do
  assert "command contains: $s" "grep -qF -- \"$s\" '$T/out1'"
done
assert "exactly one floating ip create" "[ \$(grep -c 'floating ip create' '$T/out1') -eq 1 ]"
assert "exactly one floating ip attach (orch-vm only)" "[ \$(grep -c 'server add floating ip' '$T/out1') -eq 1 ]"
assert "FIP never a used address" "! grep -E 'floating-ip-address 192.168.1.(204|251|230|253) ' '$T/out1'"
assert "summary says 15 planned" "grep -q '15 planned' '$T/out1'"
assert "no pubkey material printed" "! grep -q AAAATESTKEYONLY '$T/out1'"

echo "[dry-run, everything present => idempotent]"
FIXTURE_DIR="$D/fixtures/all" SHIM_LOG="$T/log2" bash "$INFRA/create-vms.sh" > "$T/out2" 2>&1; rc=$?
assert "exit 0" "[ $rc -eq 0 ]"
assert "only read verbs" "only_reads '$T/log2'"
assert "no planned steps" "[ \$(plan_of < '$T/out2' | wc -l) -eq 0 ]"
assert "summary says 0 planned" "grep -q '0 planned' '$T/out2'"
assert "skips are reported" "[ \$(grep -c 'SKIP' '$T/out2') -ge 12 ]"

echo "[--only network]"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log3" bash "$INFRA/create-vms.sh" --only network > "$T/out3" 2>&1; rc=$?
printf 'CREATE network aigate-net\nCREATE subnet aigate-subnet\nCREATE router-interface router-main aigate-subnet\n' > "$T/expected3"
assert "exit 0" "[ $rc -eq 0 ]"
assert "plans only network pieces" "plan_of < '$T/out3' | diff '$T/expected3' - >/dev/null"
assert "only read verbs" "only_reads '$T/log3'"

echo "[--only orchestrator / fip / bogus]"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log4" bash "$INFRA/create-vms.sh" --only orchestrator > "$T/out4" 2>&1
printf 'CREATE volume orch-vm-root\nCREATE server orch-vm\n' > "$T/expected4"
assert "--only orchestrator = its volume + server" "plan_of < '$T/out4' | diff '$T/expected4' - >/dev/null"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log5" bash "$INFRA/create-vms.sh" --only fip > "$T/out5" 2>&1
assert "--only fip plans the 2 fip steps" "[ \$(plan_of < '$T/out5' | wc -l) -eq 2 ]"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log6" bash "$INFRA/create-vms.sh" --only bogus > "$T/out6" 2>&1; rc=$?
assert "--only bogus fails" "[ $rc -ne 0 ]"

echo "[--apply without confirmation]"
printf 'no\n' | FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log7" bash "$INFRA/create-vms.sh" --apply --only flavors > "$T/out7" 2>&1; rc=$?
assert "wrong confirmation aborts non-zero" "[ $rc -ne 0 ]"
assert "prompt listed the resources" "grep -q 'aigate.orch' '$T/out7'"
assert "no write verb reached ssh" "only_reads '$T/log7'"

echo "[allow-list in lib.sh]"
for v in "server delete orch-vm" "volume delete x" "router remove subnet router-main aigate-subnet" "network delete aigate-net" "server set --name y x" "floating ip delete 1.2.3.4" "port create p"; do
  : > "$T/log8"
  ( export APPLY=1 FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log8"; . "$INFRA/lib.sh"; # shellcheck disable=SC2086
    os $v ) > "$T/out8" 2>&1; rc=$?
  assert "os refuses '$v' with APPLY=1" "[ $rc -ne 0 ] && [ ! -s '$T/log8' ]"
done
: > "$T/log9"
( export APPLY=1 FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log9"; . "$INFRA/lib.sh"; os_read server delete x ) >/dev/null 2>&1; rc=$?
assert "os_read refuses non-read verb" "[ $rc -ne 0 ] && [ ! -s '$T/log9' ]"
: > "$T/log10"
( export APPLY=0 FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log10"; . "$INFRA/lib.sh"; os flavor create --vcpus 1 --ram 1 --disk 0 f ) > "$T/out10" 2>&1; rc=$?
assert "os in dry-run prints, does not execute" "[ $rc -eq 0 ] && [ ! -s '$T/log10' ] && grep -q 'flavor create' '$T/out10'"
: > "$T/log11"
( export APPLY=1 FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log11"; . "$INFRA/lib.sh"; os flavor create --vcpus 1 --ram 1 --disk 0 f ) >/dev/null 2>&1; rc=$?
assert "os allowed verb runs via ssh with APPLY=1" "[ $rc -eq 0 ] && grep -q '^openstack --os-cloud exporter-internal flavor create' '$T/log11'"

[ $fails -eq 0 ] && echo "check-create-dryrun: PASS" || { echo "check-create-dryrun: $fails FAILED"; exit 1; }
