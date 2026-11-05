#!/usr/bin/env bash
# Dry-run / idempotency / allow-list checks for infra/create-vms.sh using a fake ssh.
set -u
D=$(cd "$(dirname "$0")" && pwd)
INFRA=$(cd "$D/.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir -p "$T/home/.ssh"
ssh-keygen -q -t ed25519 -N '' -C aigate-orchestrator -f "$T/home/.ssh/aigate_ed25519"
ssh-keygen -q -t ed25519 -N '' -C aigate-admin -f "$T/home/.ssh/aigate_admin_ed25519"
export AIGATE_KEY="$T/home/.ssh/aigate_ed25519" AIGATE_ADMIN_KEY="$T/home/.ssh/aigate_admin_ed25519"
export AIGATE_WAIT_SLEEP=0 AIGATE_WAIT_TRIES=2
ROLEKEY=$(awk '{print $2}' "$AIGATE_KEY.pub"); ADMINKEY=$(awk '{print $2}' "$AIGATE_ADMIN_KEY.pub")
MD5=$(ssh-keygen -E md5 -lf "$AIGATE_ADMIN_KEY.pub" | awk '{print $2}' | sed 's/^MD5://')
run() { # name fixturedir [args...]  -> $T/out_name, $T/log_name, rc in $rc  (stdin from caller)
  local n=$1 fx=$2; shift 2
  FIXTURE_DIR="$fx" SHIM_LOG="$T/log_$n" bash "$INFRA/create-vms.sh" "$@" > "$T/out_$n" 2>&1; rc=$?
}
mkfx() { rm -rf "$T/fx_$1"; cp -R "$2" "$T/fx_$1"; }
cp -R "$D/fixtures/all" "$T/fx_all"; printf '%s\n' "$MD5" > "$T/fx_all/keypair_show.txt"
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
CREATE floating-ip 192.168.1.200 orch-vm
X
plan_of < "$T/out1" > "$T/plan1"
assert "plan lists exactly the expected resources in order" "diff '$T/expected1' '$T/plan1' >/dev/null"
diff "$T/expected1" "$T/plan1" | head -5
for s in '--vcpus 1 --ram 2048 --disk 0 aigate.orch' '--vcpus 2 --ram 4096 --disk 0 aigate.agent' '--vcpus 4 --ram 6144 --disk 0 aigate.test' \
  '--subnet-range 10.70.0.0/24' '--gateway 10.70.0.1' '--dns-nameserver 8.8.8.8' 'router add subnet router-main aigate-subnet' \
  "--image 'Ubuntu 24.04' --size 10 orch-vm-root" "--image 'Ubuntu 24.04' --size 20 agent-vm-root" "--image 'Ubuntu 24.04' --size 30 test-vm-root" \
  '--flavor aigate.orch --volume orch-vm-root' '--flavor aigate.agent --volume agent-vm-root' '--flavor aigate.test --volume test-vm-root' \
  'v4-fixed-ip=10.70.0.11' 'v4-fixed-ip=10.70.0.12' 'v4-fixed-ip=10.70.0.13' '--key-name aigate-key' '--user-data /dev/stdin' \
  "floating ip create --port '<port id of orch-vm>' --floating-ip-address 192.168.1.200 external-net" \
  'keypair create --public-key /dev/stdin aigate-key' '--wait -f value -c id -c status orch-vm'; do
  assert "command contains: $s" "grep -qF -- \"$s\" '$T/out1'"
done
assert "exactly one floating ip create" "[ \$(grep -c 'floating ip create' '$T/out1') -eq 1 ]"
assert "no separate attach step (atomic --port create)" "! grep -q 'server add floating' '$T/out1'"
assert "FIP never a used address" "! grep -E 'floating-ip-address 192.168.1.(204|251|230|253) ' '$T/out1'"
assert "summary says 14 planned" "grep -q '14 planned' '$T/out1'"
assert "no pubkey material printed" "! grep -q AAAAC3NzaC1lZDI1NTE5 '$T/out1'"

echo "[dry-run, everything present => idempotent]"
FIXTURE_DIR="$T/fx_all" SHIM_LOG="$T/log2" bash "$INFRA/create-vms.sh" > "$T/out2" 2>&1; rc=$?
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
assert "--only fip plans 1 step (dry-run, orch-vm absent => placeholder port)" "[ \$(plan_of < '$T/out5' | wc -l) -eq 1 ]"
FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log6" bash "$INFRA/create-vms.sh" --only bogus > "$T/out6" 2>&1; rc=$?
assert "--only bogus fails" "[ $rc -ne 0 ]"

echo "[--apply without confirmation]"
printf 'no\n' | FIXTURE_DIR="$D/fixtures/empty" SHIM_LOG="$T/log7" bash "$INFRA/create-vms.sh" --apply --only flavors > "$T/out7" 2>&1; rc=$?
assert "wrong confirmation aborts non-zero" "[ $rc -ne 0 ]"
assert "prompt listed the resources" "grep -q 'aigate.orch' '$T/out7'"
assert "no write verb reached ssh" "only_reads '$T/log7'"

echo "[allow-list in lib.sh]"
for v in "server delete orch-vm" "volume delete x" "router remove subnet router-main aigate-subnet" "network delete aigate-net" "server set --name y x" "floating ip delete 1.2.3.4" "port create p" "server add floating ip orch-vm 1.2.3.4"; do
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

echo "[I1 server in ERROR state is never skipped]"
mkfx err "$T/fx_all"; printf 'ERROR\n' > "$T/fx_err/server_show.txt"
run err "$T/fx_err"
assert "dry-run dies on ERROR server" "[ $rc -ne 0 ] && grep -q 'exists in state ERROR' '$T/out_err'"
run err2 "$T/fx_err" --only fip
assert "fip stage dies on ERROR orch-vm" "[ $rc -ne 0 ] && grep -q 'exists in state ERROR' '$T/out_err2'"

echo "[I2 atomic floating ip]"
mkfx fip "$D/fixtures/empty"; printf 'orch-vm\n' >> "$T/fx_fip/server_list.txt"; printf 'ACTIVE\n' > "$T/fx_fip/server_show.txt"; printf 'port-uuid-9\n' > "$T/fx_fip/port_list__server.txt"
run fip "$T/fx_fip" --only fip
assert "plans one atomic create with the real port id" "[ \$(plan_of < '$T/out_fip' | wc -l) -eq 1 ] && grep -qF -- 'floating ip create --port port-uuid-9 --floating-ip-address 192.168.1.200 external-net' '$T/out_fip'"
mkfx fip2 "$T/fx_fip"; printf '192.168.1.222\tx\n' > "$T/fx_fip2/floating_ip_list__status.txt"
run fip2 "$T/fx_fip2" --only fip
assert "unattached leftover FIP => die with cleanup command" "[ $rc -ne 0 ] && grep -q 'floating ip delete <ID>' '$T/out_fip2'"
assert "attached FIP => skipped (all fixtures)" "grep -q 'SKIP floating-ip' '$T/out2'"

echo "[a/b/c keypair fingerprint, network id]"
mkfx kp "$T/fx_all"; printf '%s\n' "$MD5" > "$T/fx_kp/keypair_show.txt"
run kp "$T/fx_kp" --only keypair
assert "matching fingerprint => SKIP" "[ $rc -eq 0 ] && grep -q 'SKIP keypair' '$T/out_kp'"
printf 'aa:bb:cc\n' > "$T/fx_kp/keypair_show.txt"
run kp2 "$T/fx_kp" --only keypair
assert "fingerprint mismatch dies" "[ $rc -ne 0 ] && grep -q 'differs' '$T/out_kp2'"
assert "keypair is registered from the ADMIN public key" "grep -qF -- '< $AIGATE_ADMIN_KEY.pub' '$T/out1'"
mkfx nonet "$D/fixtures/empty"; : > "$T/fx_nonet/network_show.txt"
printf 'apply aigate\n' | run nonet "$T/fx_nonet" --apply --only orchestrator
assert "apply without network dies, no server create" "[ $rc -ne 0 ] && ! grep -q 'server create' '$T/log_nonet'"

echo "[h partial failures in apply mode]"
mkfx wv "$D/fixtures/empty"; printf 'creating\n' > "$T/fx_wv/volume_show.txt"
printf 'apply aigate\n' | run wv "$T/fx_wv" --apply --only orchestrator
assert "wait_volume timeout dies" "[ $rc -ne 0 ] && grep -q 'not available' '$T/out_wv'"
assert "no server create after volume timeout" "! grep -q 'server create' '$T/log_wv'"
mkfx sc "$D/fixtures/empty"; printf '1\n' > "$T/fx_sc/server_create.rc"
printf 'apply aigate\n' | run sc "$T/fx_sc" --apply --only orchestrator
assert "server create failing dies" "[ $rc -ne 0 ] && grep -q 'command failed' '$T/out_sc'"
mkfx se "$D/fixtures/empty"; printf 'ERROR\n' > "$T/fx_se/server_show.txt"
printf 'apply aigate\n' | run se "$T/fx_se" --apply --only orchestrator
assert "server create ok but state ERROR dies" "[ $rc -ne 0 ] && grep -q 'exists in state ERROR' '$T/out_se'"
assert "server create restricted output (no adminPass)" "grep -q 'server create .*-f value -c id -c status' '$T/log_se'"
mkfx fc "$T/fx_fip"; printf '1\n' > "$T/fx_fc/floating_ip_create.rc"
printf 'apply aigate\n' | run fc "$T/fx_fc" --apply --only fip
assert "floating ip create failing dies" "[ $rc -ne 0 ] && grep -q 'command failed' '$T/out_fc'"
assert "no attach attempted" "! grep -q 'add floating' '$T/log_fc'"

echo "[f rendered user-data]"
for role in orchestrator agent test; do
  bash "$INFRA/create-vms.sh" --render $role > "$T/ud_$role" 2>&1; rc=$?
  assert "$role: render ok" "[ $rc -eq 0 ]"
  assert "$role: role pubkey exactly once" "[ \$(grep -c -F '$ROLEKEY' '$T/ud_$role') -eq 1 ]"
  assert "$role: admin pubkey absent from user-data" "! grep -qF '$ADMINKEY' '$T/ud_$role'"
  assert "$role: no PRIVATE KEY, no @@ placeholder" "! grep -q -e 'PRIVATE KEY' -e '@@' '$T/ud_$role'"
  if ruby -ryaml -e1 2>/dev/null; then
    assert "$role: rendered YAML valid, key under role user" "ruby -ryaml -e 'd=YAML.safe_load(File.read(ARGV[0])); u=d[\"users\"][1]; abort unless d[\"users\"][0]==\"default\" && u[\"ssh_authorized_keys\"].size==1 && u[\"ssh_authorized_keys\"][0].include?(ARGV[1])' '$T/ud_$role' '$ROLEKEY'"
  else echo "  skip - ruby yaml unavailable"; fi
done

[ $fails -eq 0 ] && echo "check-create-dryrun: PASS" || { echo "check-create-dryrun: $fails FAILED"; exit 1; }
