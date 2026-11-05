#!/bin/sh
# apply-firewall.sh [--apply] orchestrator|agent|test
# Installs infra/nftables/<role>.nft as /etc/nftables.conf on the VM (admin key via orch-vm ProxyCommand,
# the key itself never leaves this machine). agent/test also get dnsmasq with nftset so only domains in
# infra/egress-domains.<role> populate set allow_web4. A systemd timer reverts to the previous
# ruleset after 180 s unless the new one is confirmed (login re-tested, then timer stopped).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
APPLY=0; [ "${1:-}" = "--apply" ] && { APPLY=1; shift; }
role=${1:?usage: apply-firewall.sh [--apply] orchestrator|agent|test}
case $role in orchestrator) ip=192.168.1.200 ;; agent) ip=10.70.0.12 ;; test) ip=10.70.0.13 ;; *) echo "bad role" >&2; exit 64 ;; esac
K=$HOME/.ssh/aigate_admin_ed25519
J="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@192.168.1.200"
if [ "$role" = orchestrator ]; then vm() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 "ubuntu@$ip" "$@"; }
else vm() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 -o ProxyCommand="$J" "ubuntu@$ip" "$@"; }; fi
echo "== $role ($ip): plan = nft -c check, install dnsmasq (agent/test), backup, apply with 180s auto-rollback, verify login"
[ "$APPLY" = 1 ] || { echo "dry-run: nothing changed. Re-run with --apply."; exit 0; }
vm 'sudo nft -c -f /dev/stdin' < "$here/nftables/$role.nft"
vm 'sudo cp -n /etc/nftables.conf /etc/nftables.conf.pre-task2'
if [ "$role" != orchestrator ]; then
  vm 'dpkg -s dnsmasq-base >/dev/null 2>&1 && dpkg -s dnsmasq >/dev/null 2>&1 || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq dnsmasq >/dev/null'
  { echo "no-resolv"; echo "server=8.8.8.8"; echo "listen-address=127.0.0.1"; echo "bind-interfaces"
    grep -vE '^[[:space:]]*(#|$)' "$here/egress-domains.$role" | while read -r d; do echo "nftset=/$d/4#inet#aigate#allow_web4"; done; } \
    | vm 'sudo tee /etc/dnsmasq.d/aigate.conf >/dev/null'
  printf '[Resolve]\nDNS=127.0.0.1\nDomains=~.\nDNSStubListener=yes\n' | vm 'sudo install -d /etc/systemd/resolved.conf.d && sudo tee /etc/systemd/resolved.conf.d/aigate.conf >/dev/null'
  vm 'sudo sed -i "s/^#\?\s*ENABLED=.*//" /etc/default/dnsmasq 2>/dev/null; sudo systemctl enable dnsmasq >/dev/null 2>&1; sudo systemctl restart systemd-resolved'
fi
vm 'sudo systemctl stop aigate-rollback.timer aigate-rollback.service 2>/dev/null; sudo systemd-run --quiet --unit=aigate-rollback --on-active=180 /bin/sh -c "cp /etc/nftables.conf.pre-task2 /etc/nftables.conf; nft -f /etc/nftables.conf"'
vm 'sudo tee /etc/nftables.conf >/dev/null' < "$here/nftables/$role.nft"
vm 'sudo nft -f /etc/nftables.conf'
[ "$role" = orchestrator ] || vm 'sudo systemctl restart dnsmasq'
echo "== re-test login with fresh connection"
if vm 'echo login-ok; sudo systemctl stop aigate-rollback.timer'; then echo "confirmed: rollback timer stopped"; else echo "LOGIN FAILED: rollback will restore previous rules in <=180 s" >&2; exit 1; fi
