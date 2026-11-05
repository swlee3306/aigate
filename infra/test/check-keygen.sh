#!/usr/bin/env bash
set -u
D=$(cd "$(dirname "$0")" && pwd); INFRA=$(cd "$D/.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fails=0
ok()  { echo "  ok   - $1"; }
bad() { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
unset AIGATE_KEY AIGATE_ADMIN_KEY
HOME="$T" bash "$INFRA/keygen.sh" > "$T/o1" 2>&1; r1=$?
assert "first run exits 0" "[ $r1 -eq 0 ]"
assert "private key created, mode 600" "[ -f '$T/.ssh/aigate_ed25519' ] && [ \"\$(stat -f %Lp '$T/.ssh/aigate_ed25519' 2>/dev/null || stat -c %a '$T/.ssh/aigate_ed25519')\" = 600 ]"
assert "admin key created, mode 600" "[ -f '$T/.ssh/aigate_admin_ed25519' ] && [ \"\$(stat -f %Lp '$T/.ssh/aigate_admin_ed25519' 2>/dev/null || stat -c %a '$T/.ssh/aigate_admin_ed25519')\" = 600 ]"
assert "admin key has comment aigate-admin" "grep -q 'aigate-admin\$' '$T/.ssh/aigate_admin_ed25519.pub'"
assert "two different keys" "! cmp -s '$T/.ssh/aigate_ed25519.pub' '$T/.ssh/aigate_admin_ed25519.pub'"
assert "public key created" "[ -f '$T/.ssh/aigate_ed25519.pub' ]"
assert "key is ed25519 with comment aigate-orchestrator" "grep -q '^ssh-ed25519 .* aigate-orchestrator' '$T/.ssh/aigate_ed25519.pub'"
assert "no passphrase (ssh-keygen -y works non-interactively)" "ssh-keygen -y -P '' -f '$T/.ssh/aigate_ed25519' >/dev/null 2>&1"
assert "fingerprint printed" "grep -q 'SHA256:' '$T/o1'"
before=$(cksum < "$T/.ssh/aigate_ed25519"); beforea=$(cksum < "$T/.ssh/aigate_admin_ed25519")
HOME="$T" bash "$INFRA/keygen.sh" > "$T/o2" 2>&1; r2=$?
assert "second run refuses (non-zero)" "[ $r2 -ne 0 ]"
assert "admin key unchanged" "[ \"\$(cksum < '$T/.ssh/aigate_admin_ed25519')\" = \"$beforea\" ]"
assert "second run said so" "grep -qi 'exist' '$T/o2'"
assert "private key unchanged" "[ \"\$(cksum < '$T/.ssh/aigate_ed25519')\" = \"$before\" ]"
# partial: only the role key exists -> admin created, role untouched
T2=$(mktemp -d); trap 'rm -rf "$T" "$T2"' EXIT; mkdir -p "$T2/.ssh"; cp "$T/.ssh/aigate_ed25519" "$T/.ssh/aigate_ed25519.pub" "$T2/.ssh/"
b2=$(cksum < "$T2/.ssh/aigate_ed25519")
HOME="$T2" bash "$INFRA/keygen.sh" > "$T2/o" 2>&1; r3=$?
assert "partial: creates only the missing admin key (rc 0)" "[ $r3 -eq 0 ] && [ -f '$T2/.ssh/aigate_admin_ed25519' ] && [ \"\$(cksum < '$T2/.ssh/aigate_ed25519')\" = \"$b2\" ]"
bodya=$(sed -n '2,$p' "$T/.ssh/aigate_admin_ed25519" | head -1)
assert "no admin key material in outputs" "! grep -q -F \"$bodya\" '$T/o1' '$T/o2' '$T2/o'"
body=$(sed -n '2,$p' "$T/.ssh/aigate_ed25519" | head -1)
assert "no private key material in outputs" "! grep -q -e 'PRIVATE KEY' -e \"$body\" '$T/o1' '$T/o2'"
[ $fails -eq 0 ] && echo "check-keygen: PASS" || { echo "check-keygen: $fails FAILED"; exit 1; }
