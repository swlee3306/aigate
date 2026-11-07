#!/usr/bin/env bash
# Tests infra/make-warm-snapshot.sh with a fake ssh first on PATH and AIGATE_SNAPSHOT_SCRIPT (test-only override)
# pointing at a fake snapshot.sh. Nothing real is contacted; ~/.ssh is untouched.
# Fake ssh env: FAKE_SNAPS FAKE_TASKS FAKE_VM_STATUS FAKE_VM_SSH_FAIL FAKE_GB_USED FAKE_GB_LIMIT FAKE_UNIT_COUNT FAKE_DIRTY
set -u
D=$(cd "$(dirname "$0")" && pwd); INFRA=$(cd "$D/.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fails=0
ok()  { echo "  ok   - $1"; }
bad() { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
mkdir -p "$T/bin"
cat > "$T/bin/ssh" <<'FAKE'
#!/bin/sh
n=$(wc -c | tr -d " ")   # stdin size
for a; do last=$a; done
echo "ssh stdin=$n :: $*" >> "$FAKE_LOG"
case $last in
  *"volume snapshot list"*) printf '%s\n' ${FAKE_SNAPS:-test-vm-clean3} ;;
  *"volume snapshot show"*) echo available ;;
  *"quota show --usage -f json"*) printf '[{"Resource":"gigabytes","In Use":%s,"Reserved":0,"Limit":%s}]\n' "${FAKE_GB_USED:-100}" "${FAKE_GB_LIMIT:-300}" ;;
  *"quota show"*) echo "gigabytes ..." ;;
  *"server show test-vm"*) echo "${FAKE_VM_STATUS:-ACTIVE}" ;;
  *"orchd.sock list"*) echo "ID STATE"; printf '%s\n' "${FAKE_TASKS:-}" ;;
  *"rev-parse --git-dir"*) echo .git ;;
  *"runner@10.70.0.13 'true'"*) [ -z "${FAKE_VM_SSH_FAIL:-}" ] || exit 255 ;;
  *"grep -c -- --test-snapshot"*) echo "${FAKE_UNIT_COUNT:-1}" ;;
  *"ls -A /srv/aigate/work"*) if [ -n "${FAKE_DIRTY:-}" ]; then echo "DIRTY: work-not-empty"; exit 1; else echo CLEAN; fi ;;
esac
exit 0
FAKE
chmod +x "$T/bin/ssh"
cat > "$T/snap.sh" <<'FAKE'
#!/bin/sh
echo "SNAPSHOT-SCRIPT $* name=${AIGATE_SNAPSHOT:-}" >> "$FAKE_LOG"
FAKE
export FAKE_LOG="$T/log" AIGATE_SNAPSHOT_SCRIPT="$T/snap.sh" AIGATE_ADMIN_KEY="$T/nokey"
S="$INFRA/make-warm-snapshot.sh"
unset FAKE_SNAPS FAKE_TASKS FAKE_VM_STATUS FAKE_VM_SSH_FAIL FAKE_GB_USED FAKE_GB_LIMIT FAKE_UNIT_COUNT FAKE_DIRTY
# run <args...>: fresh log, output in $T/out, rc in $rc
run() { : > "$FAKE_LOG"; PATH="$T/bin:$PATH" sh "$S" "$@" > "$T/out" 2>&1; rc=$?; }
# mutating lines = everything in the log that is not a known read-only query
muts() { grep -Ev 'volume snapshot (list|show)|quota show|server show|orchd.sock list|rev-parse --git-dir|runner@10.70.0.13 .true.|grep -c -- --test-snapshot' "$FAKE_LOG" || true; }
nomut() { [ -z "$(muts)" ]; }

echo "# syntax"
assert "sh -n make-warm-snapshot.sh" "sh -n '$S'"
assert "sh -n this test" "bash -n '$D/check-make-warm-snapshot.sh'"

echo "# dry-run"
run --name test-vm-clean4
assert "dry-run exits 0" "[ $rc -eq 0 ]"
assert "dry-run prints numbered plan 1)..6)" "grep -q '^  1) ' '$T/out' && grep -q '^  6) ' '$T/out'"
assert "dry-run says nothing changed" "grep -q 'dry-run: nothing was changed' '$T/out'"
assert "dry-run did preflight reads" "grep -q 'volume snapshot list' '$FAKE_LOG' && grep -q 'quota show' '$FAKE_LOG'"
assert "dry-run made no mutating call" "nomut"
run --name test-vm-clean4 --switch-service
assert "dry-run + --switch-service still read-only" "[ $rc -eq 0 ] && nomut && grep -q 'exactly once' '$T/out'"

echo "# preflight STOPs (no mutating call, even with --apply)"
FAKE_SNAPS="test-vm-clean3 test-vm-clean4" run --apply --name test-vm-clean4
assert "existing snapshot stops" "[ $rc -eq 1 ] && grep -q 'already exists' '$T/out' && nomut"
FAKE_TASKS="abc123 agent_running" run --apply --name test-vm-clean4
assert "running task stops" "[ $rc -eq 1 ] && grep -q 'tasks still active' '$T/out' && nomut"
FAKE_VM_STATUS=SHUTOFF run --apply --name test-vm-clean4
assert "test-vm not ACTIVE stops" "[ $rc -eq 1 ] && grep -q 'not ACTIVE' '$T/out' && grep -q 'one orchestrator task first' '$T/out' && nomut"
FAKE_VM_SSH_FAIL=1 run --apply --name test-vm-clean4
assert "test-vm ssh down stops" "[ $rc -eq 1 ] && grep -q 'does not answer ssh' '$T/out' && nomut"
FAKE_GB_USED=275 run --apply --name test-vm-clean4
assert "quota exceeded stops, points at cleanup scripts" "[ $rc -eq 1 ] && grep -q 'cleanup-old-snapshots.sh' '$T/out' && grep -q 'cleanup-restore-volumes.sh' '$T/out' && nomut"
FAKE_GB_USED=270 run --name test-vm-clean4
assert "quota exactly at limit (270+30=300) passes" "[ $rc -eq 0 ]"
FAKE_UNIT_COUNT=0 run --apply --name test-vm-clean4 --switch-service
assert "--switch-service refuses when --test-snapshot absent" "[ $rc -eq 1 ] && grep -q 'appears 0 times' '$T/out' && nomut"
FAKE_UNIT_COUNT=2 run --apply --name test-vm-clean4 --switch-service
assert "--switch-service refuses when --test-snapshot appears twice" "[ $rc -eq 1 ] && grep -q 'appears 2 times' '$T/out' && nomut"
FAKE_UNIT_COUNT=2 run --apply --name test-vm-clean4
assert "unit count irrelevant without --switch-service" "[ $rc -eq 0 ]"

echo "# argument validation (no ssh at all)"
run --apply --name evil-name
assert "bad name rejected (64)" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name 'test-vm-clean4;x'
assert "name with metachar rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --repo 'a b'
assert "bad --repo rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --repo '../x/y'
assert "path-like --repo rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --warm-cmd 'echo `id`'
assert "backtick in --warm-cmd rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --warm-cmd 'echo $(id)'
assert "\$( in --warm-cmd rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --warm-cmd "$(printf 'a\nb')"
assert "newline in --warm-cmd rejected" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply --name test-vm-clean4 --bogus
assert "unknown flag exits 64" "[ $rc -eq 64 ] && [ ! -s '$FAKE_LOG' ]"
run --apply
assert "missing --name exits 64" "[ $rc -eq 64 ]"

echo "# --apply order"
q2="it'\\''\\'\\'''\\''s warm"
run --apply --name test-vm-clean4 --warm-cmd "echo it's warm"
assert "apply exits 0" "[ $rc -eq 0 ]"
ln() { grep -n -m1 -- "$1" "$FAKE_LOG" | cut -d: -f1; }
b=$(ln 'bundle create - HEAD'); w=$(ln 'git clone -q /tmp/warm.bundle'); c=$(ln 'rm -rf /tmp/warm /tmp/warm.bundle'); a=$(ln 'ls -A /srv/aigate/work'); s=$(ln 'SNAPSHOT-SCRIPT')
assert "order: bundle < warm < cleanup < assert < snapshot" "[ -n '$b' ] && [ '$b' -lt '$w' ] && [ '$w' -lt '$c' ] && [ '$c' -lt '$a' ] && [ '$a' -lt '$s' ]"
assert "snapshot script got create test-vm and the name" "grep -q 'SNAPSHOT-SCRIPT create test-vm name=test-vm-clean4' '$FAKE_LOG'"
assert "snapshot is the last mutating call without --switch-service" "muts | tail -1 | grep -q '^SNAPSHOT-SCRIPT create test-vm'"
assert "no switch call without --switch-service, one-liner printed" "! grep -q 'systemctl' '$FAKE_LOG' && grep -q 'To do it later run' '$T/out'"
assert "bundle goes through the strict ssh options" "grep -q 'StrictHostKeyChecking=yes.*known_hosts_aigate.*IdentityAgent=none.*id_aigate' '$FAKE_LOG'"
assert "warm-cmd with a quote is passed (quoted) intact" "grep -qF \"\$q2\" '$FAKE_LOG'"
assert "timeout 1500 used" "grep -q 'timeout 1500' '$FAKE_LOG'"
assert "every logged stdin is empty" "! grep -q 'stdin=[1-9]' '$FAKE_LOG'"
assert "no delete of snapshots/volumes" "! grep -Eq 'snapshot delete|volume delete' '$FAKE_LOG'"
FAKE_DIRTY=1 run --apply --name test-vm-clean4
assert "dirty test-vm stops before the snapshot" "[ $rc -eq 1 ] && ! grep -q SNAPSHOT-SCRIPT '$FAKE_LOG' && grep -q 'not clean' '$T/out'"
run --apply --name test-vm-clean4 --switch-service --repo my-repo
sw=$(ln 'cp -p'); s=$(ln 'SNAPSHOT-SCRIPT')
assert "switch call after snapshot, with backup .pre-<name> and daemon-reload" "[ '$s' -lt '$sw' ] && grep -q 'orchd.service.pre-test-vm-clean4\\|\\.pre-test-vm-clean4' '$FAKE_LOG' && grep -q 'daemon-reload' '$FAKE_LOG'"
assert "reminds to update orchd-orchvm.service" "grep -q 'infra/systemd/orchd-orchvm.service' '$T/out'"
assert "--repo used for the bundle" "grep -q '/home/orch/repos/my-repo bundle' '$FAKE_LOG'"
assert "limitation of cleanup-old-snapshots stated" "grep -q 'hard-coded' '$T/out'"

[ $fails -eq 0 ] && echo "check-make-warm-snapshot: all passed" || { echo "check-make-warm-snapshot: $fails failed"; exit 1; }
