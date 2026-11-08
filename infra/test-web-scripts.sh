#!/bin/sh
# Local checks for orch-web-serve.sh and signers-init.sh. Touches nothing outside a temp dir; never calls tailscale (dry-run paths only).
set -u
d=$(cd "$(dirname "$0")" && pwd); fail=0
ok() { echo "ok   $1"; }; bad() { echo "FAIL $1"; fail=1; }
out=$(sh "$d/orch-web-serve.sh" 8808 2>&1); rc=$?
[ $rc = 0 ] && ok "serve dry-run exits 0" || bad "serve dry-run rc=$rc"
echo "$out" | grep -q 'tailscale serve --bg --https=443 http://127.0.0.1:8808' && ok "serve dry-run prints the serve command" || bad "serve command missing"
out=$(sh "$d/orch-web-serve.sh" 8808 --off 2>&1); rc=$?
[ $rc = 0 ] && echo "$out" | grep -q 'tailscale serve --https=443 off' && ok "serve --off dry-run" || bad "serve --off dry-run"
for p in 80 1023 65536 abc 8808x 0 -1 ""; do
  sh "$d/orch-web-serve.sh" "$p" >/dev/null 2>&1 && bad "bad port '$p' accepted" || ok "bad port '$p' refused"
done
sh "$d/orch-web-serve.sh" >/dev/null 2>&1 && bad "missing port accepted" || ok "missing port refused"
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
if [ "$(id -u)" != 0 ]; then
  AIGATE_ETC_DIR=$t/aigate sh "$d/signers-init.sh" --apply >/dev/null 2>&1 && bad "signers-init --apply as non-root succeeded" || ok "signers-init --apply as non-root refused"
  [ ! -e "$t/aigate" ] && ok "signers-init created nothing" || bad "signers-init created $t/aigate"
else echo "skip signers-init non-root test (running as root)"; fi
out=$(AIGATE_ETC_DIR=$t/aigate sh "$d/signers-init.sh" 2>&1); rc=$?
[ $rc = 0 ] && [ ! -e "$t/aigate" ] && echo "$out" | grep -q 'signers add' && ok "signers-init dry-run exits 0, prints next steps, creates nothing" || bad "signers-init dry-run"
# no executable use of Funnel (comments and the refusal text may mention it)
if grep -nE '^[[:space:]]*(sudo[[:space:]]+)?tailscale[[:space:]]+funnel' "$d/orch-web-serve.sh" "$d/signers-init.sh"; then bad "tailscale funnel used as a command"; else ok "no tailscale funnel command"; fi
grep -n 'tailscale funnel' "$d/orch-web-serve.sh" | grep -v '^[0-9]*:.*\(die\|echo\|#\)' && bad "unexpected funnel string" || ok "funnel string only in refusal text"
exit $fail
