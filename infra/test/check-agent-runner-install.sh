#!/usr/bin/env bash
# install.sh dry-run must print the plan, validate the key and never run ssh.
set -u
D=$(cd "$(dirname "$0")" && pwd); INFRA=$(cd "$D/.." && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
fails=0
ok()  { echo "  ok   - $1"; }
bad() { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
mkdir "$T/bin"; printf '#!/bin/sh\necho ssh-called >> "%s/ssh.log"\nexit 1\n' "$T" > "$T/bin/ssh"; chmod +x "$T/bin/ssh"
ssh-keygen -q -t ed25519 -N '' -f "$T/k" -C x
ssh-keygen -q -t ed25519 -N '' -f "$T/admin" -C admin
S="$INFRA/agent-runner/install.sh"
PATH="$T/bin:$PATH" sh "$S" "$T/k.pub" > "$T/o" 2>&1; r=$?
assert "dry-run exits 0" "[ $r -eq 0 ]"
assert "dry-run says nothing changed" "grep -q 'dry-run: nothing changed' '$T/o'"
assert "plan shows forced command line with restrictions" "grep -q 'command=\"/usr/local/sbin/agent-runner\",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty ssh-ed25519' '$T/o'"
assert "no ssh was started" "[ ! -e '$T/ssh.log' ]"
assert "private key rejected" "! sh '$S' '$T/k' >/dev/null 2>&1"
assert "admin key rejected" "! AIGATE_ADMIN_KEY='$T/admin' sh '$S' '$T/admin.pub' >/dev/null 2>&1"
assert "private key never printed" "! grep -q 'PRIVATE' '$T/o'"
assert "extra args rejected" "! sh '$S' '$T/k.pub' '$T/k.pub' >/dev/null 2>&1"
[ $fails -eq 0 ]
