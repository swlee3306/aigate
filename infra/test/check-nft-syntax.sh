#!/bin/sh
# Static policy checks for infra/nftables/*.nft (syntax is checked on the homeserver with `nft -c`
# when reachable). Fails if default policies are not drop, or agent/test can reach orch-vm / LAN.
set -u
d=$(cd "$(dirname "$0")/.." && pwd); fail=0; ok() { echo "ok   $1"; }; bad() { echo "FAIL $1"; fail=1; }
for r in orchestrator agent test; do
  f=$d/nftables/$r.nft
  [ "$(grep -c 'policy drop' "$f")" = 3 ] && ok "$r: input/forward/output policy drop" || bad "$r: all three chains must be policy drop"
done
for r in agent test; do
  f=$d/nftables/$r.nft
  grep -E 'daddr' "$f" | grep -qE '10\.70\.0\.11|192\.168|10\.10\.|10\.0\.0\.0|100\.' && bad "$r: outbound rule names orch-vm or a LAN range" || ok "$r: no outbound rule toward orch-vm/LAN"
  grep -qE 'oifname "tailscale0"' "$f" && bad "$r: must not use tailscale0" || ok "$r: no tailscale egress"
done
grep -q 'ip saddr 10.70.0.11 tcp dport 22' "$d/nftables/agent.nft" && ok "agent: ssh in only from orch-vm" || bad "agent: ssh-in rule missing"
grep -q 'ip saddr 10.70.0.11 tcp dport 22' "$d/nftables/test.nft" && ok "test: ssh in only from orch-vm" || bad "test: ssh-in rule missing"
grep -q '! *=' "$d/nftables/orchestrator.nft" && grep -q 'private4' "$d/nftables/orchestrator.nft" && ok "orchestrator: internet egress excludes private ranges" || bad "orchestrator: private-range exclusion missing"
exit $fail
