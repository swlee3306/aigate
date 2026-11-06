#!/bin/sh
# orch-openstack-access.sh [--apply|--rollback|--check]     (default: dry-run, prints the plan)
# Gives orch-vm (user orch) access to the homeserver's OpenStack API so orchd can restore the test VM per job.
# DECISION (user, 2026-11-06): single-user lab, so the existing admin credentials are copied as they are.
# What this changes, and nothing else:
#   1. orch-vm firewall: allow tcp to 192.168.1.121 ports 5000 8774 8776 9292 9696 8778 only (rest of the LAN stays blocked)
#   2. orch-vm: apt install python3-openstackclient
#   3. orch-vm: ~orch/.config/openstack/clouds.yaml (mode 600, dir 700), copied from the homeserver /etc/openstack/clouds.yaml
#      through a pipe between two ssh sessions on this Mac; values are never printed or stored anywhere else
# Risk accepted: orch-vm now holds admin OpenStack credentials; if orch-vm is compromised, the whole homeserver cloud is.
# Rollback: --rollback restores the previous firewall and deletes clouds.yaml from orch-vm.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
orch() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes ubuntu@192.168.1.200 "$@"; }
case "${1:-}" in
  "") sed -n '2,/^set -eu/p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'; echo; echo "dry-run: nothing changed. Re-run with --apply." ;;
  --apply)
    echo "== 1/4 firewall (auto-rollback timer, previous rules saved as /etc/nftables.conf.pre-openstack)"
    orch 'sudo cp -n /etc/nftables.conf /etc/nftables.conf.pre-openstack'
    sh "$here/apply-firewall.sh" --apply orchestrator | tail -2
    echo "== 2/4 openstack client"
    orch 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3-openstackclient >/dev/null 2>&1; openstack --version'
    echo "== 3/4 credentials (not printed)"
    ssh openstack-host 'sudo -n cat /etc/openstack/clouds.yaml' | orch 'sudo -u orch -H bash -c "umask 077; mkdir -p ~/.config/openstack; chmod 700 ~/.config/openstack; cat > ~/.config/openstack/clouds.yaml; chmod 600 ~/.config/openstack/clouds.yaml; ls -l ~/.config/openstack/clouds.yaml | cut -d\" \" -f1,3"'
    sh "$0" --check ;;
  --check)
    echo "== 4/4 as user orch on orch-vm: read-only server list (names and status only)"
    orch 'sudo -u orch -H openstack --os-cloud exporter-internal server list -f value -c Name -c Status'
    echo "== other LAN hosts must stay blocked"
    orch 'for t in 192.168.1.121:22 192.168.1.128:22 192.168.1.204:22; do printf "%s: " $t; timeout 3 bash -c "exec 3<>/dev/tcp/${t%:*}/${t#*:}" 2>/dev/null && echo OPEN || echo closed; done' ;;
  --rollback)
    orch 'sudo cp /etc/nftables.conf.pre-openstack /etc/nftables.conf && sudo nft -f /etc/nftables.conf && sudo -u orch rm -f /home/orch/.config/openstack/clouds.yaml && echo "rolled back: firewall restored, clouds.yaml removed"' ;;
  *) echo "usage: $0 [--apply|--rollback|--check]" >&2; exit 64 ;;
esac
