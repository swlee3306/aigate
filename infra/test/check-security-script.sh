#!/bin/sh
# Tests infra/test/security-checks.sh without any VM access: dry-run output, usage errors,
# and the mismatch path of --run with a fake ssh that answers OPEN to everything.
D=$(cd "$(dirname "$0")" && pwd)
S=$D/security-checks.sh
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fails=0
ok()  { echo "  ok   - $1"; }
bad() { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# a fake ssh that records every call; the dry run must never call it
mkdir -p "$T/bin"
cat > "$T/bin/ssh" <<'EOS'
#!/bin/sh
echo "$@" >> "$SSH_LOG"
echo OPEN
EOS
chmod +x "$T/bin/ssh"
export SSH_LOG="$T/ssh.log"; : > "$SSH_LOG"
PATH="$T/bin:$PATH"; export PATH

sh -n "$S" 2>"$T/syn"; assert "script parses (sh -n)" "[ $? -eq 0 ]"
sh "$S" --report "$T/should-not-exist.md" > "$T/dry" 2>&1; rc=$?
assert "dry run exits 0" "[ $rc -eq 0 ]"
assert "dry run says DRY RUN" "grep -q 'DRY RUN' '$T/dry'"
assert "dry run never calls ssh" "[ ! -s '$SSH_LOG' ]"
assert "dry run writes no report" "[ ! -e '$T/should-not-exist.md' ]"
for want in "agent-vm" "test-vm" "orch-vm" "1.1.1.1:443" "example.com" "192.168.1.128:22" \
            "sudo -n true" "command not allowed" "private key" "GITLAB" "TestCollectRejectsHugeHistory" \
            "TestCollectRejectsExtraRefs" "TestForgedJUnitRejected"; do
  assert "dry run mentions: $want" "grep -q -- '$want' '$T/dry'"
done
assert "orch-vm file modes are SKIPped without flags" "grep -q 'SKIP .*no --approvals given' '$T/dry'"
sh "$S" --approvals /srv/x/approvals.json --db /srv/x/orch.db --socket /srv/x/s.sock > "$T/dry2" 2>&1
assert "path flags add the mode checks" "grep -q \"stat -c %a '/srv/x/approvals.json'\" '$T/dry2' && grep -q \"stat -c %a '/srv/x/s.sock'\" '$T/dry2'"
assert "path flags remove the SKIP lines" "! grep -q 'SKIP' '$T/dry2'"

sh "$S" --bogus > "$T/bad" 2>&1; assert "unknown option exits 2" "[ $? -eq 2 ]"
sh "$S" --db > "$T/bad2" 2>&1; assert "option without value exits 2" "[ $? -eq 2 ]"
sh "$S" --run --admin-key "$T/nokey" --report "$T/r0.md" > "$T/nokey.out" 2>&1; rc=$?
assert "--run without the admin key exits 2 and calls no ssh" "[ $rc -eq 2 ] && [ ! -s '$SSH_LOG' ]"

# --run against the fake ssh: it answers OPEN to everything, so the table must show mismatches
: > "$T/key"
sh "$S" --run --admin-key "$T/key" --report "$T/report.md" > "$T/run.out" 2>&1; rc=$?
assert "--run with mismatching answers exits 1" "[ $rc -eq 1 ]"
assert "--run called the fake ssh" "[ -s '$SSH_LOG' ]"
assert "report written with a table header" "grep -q '^| vm | check | expected | observed |' '$T/report.md'"
assert "report records FAIL rows" "grep -q '| FAIL |' '$T/report.md'"
assert "report result is FAIL" "grep -q '^Result: FAIL' '$T/report.md'"
assert "report lists go-test rows without running them" "grep -q 'go test' '$T/report.md' && grep -q 'n/a here' '$T/report.md'"
assert "ssh used the admin key and the jump host" "grep -q -- \"-i $T/key\" '$SSH_LOG' && grep -q 'ubuntu@192.168.1.200' '$SSH_LOG'"
assert "ssh used the ProxyCommand for agent-vm" "grep -q 'ProxyCommand=ssh -i $T/key' '$SSH_LOG'"

[ $fails -eq 0 ] && echo "security-checks script: all passed" || echo "security-checks script: $fails failed"
exit $fails
