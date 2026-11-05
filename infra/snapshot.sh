#!/usr/bin/env bash
# snapshot.sh create|restore|time|prune <vm> [N]     vm: orch-vm | agent-vm | test-vm  (nothing else is accepted)
#   create   stop VM, Cinder snapshot "<vm>-clean" of its root volume, start VM
#   restore  delete server (volume kept), revert root volume to "<vm>-clean", re-create server, wait for ssh; prints seconds
#   time     N restores (default 3); before each, leaves a marker file in the VM and checks it is gone afterwards
# Only the aigate VMs' own volume/snapshot and start/stop are touched. Uses the admin key via orch-vm ProxyCommand.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd); . "$here/lib.sh"
cmd=${1:-}; vm=${2:-}; n=${3:-3}
case $vm in orch-vm) ip=192.168.1.200 ;; agent-vm) ip=10.70.0.12 ;; test-vm) ip=10.70.0.13 ;; *) die "vm must be orch-vm|agent-vm|test-vm" ;; esac
case $vm in orch-vm) ip_inner=10.70.0.11 ;; agent-vm) ip_inner=10.70.0.12 ;; test-vm) ip_inner=10.70.0.13 ;; esac
snap="${AIGATE_SNAPSHOT:-$vm-clean}"; vol="${vm}-root"; K=$HOME/.ssh/aigate_admin_ed25519
J="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@192.168.1.200"
sshvm() { if [ "$vm" = orch-vm ]; then ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=3 "ubuntu@$ip" "$@"
          else ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=3 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$J" "ubuntu@$ip" "$@"; fi; }  # inner VMs: host keys change on re-create
osw() { ssh -o BatchMode=yes "$OS_HOST" "$(remote_cmd "$@")" < /dev/null; }
now() { date +%s; }
wait_status() { # <server|volume|snapshot> <name> <status>
  local i s; for i in $(seq 1 120); do s=$(osw $1 show "$2" -f value -c status 2>/dev/null || true); [ "$s" = "$3" ] && return 0; [ "$s" = ERROR ] && die "$1 $2 is ERROR"; sleep 2; done; die "timeout waiting $1 $2 -> $3"; }
stop_vm()  { osw server stop "$vm"; wait_status server "$vm" SHUTOFF; }
start_vm() { osw server start "$vm"; wait_status server "$vm" ACTIVE; }
wait_ssh() { local i; for i in $(seq 1 120); do sshvm true 2>/dev/null && return 0; sleep 2; done; die "ssh to $vm did not come back"; }
case $cmd in
  create)
    osw volume snapshot show "$snap" -f value -c id >/dev/null 2>&1 && die "snapshot $snap exists; delete it first"
    t0=$(now); stop_vm
    # snapshot the volume the server really boots from (after a restore it is "<vm>-root-YYYYMMDD-HHMMSS-xxxx", not "<vm>-root")
    cur=$(osw server show "$vm" -f json -c volumes_attached | python3 -c 'import sys,json; print(json.load(sys.stdin)["volumes_attached"][0]["id"])')
    osw volume snapshot create --force --volume "$cur" "$snap" >/dev/null; wait_status volume\ snapshot "$snap" available
    start_vm; wait_ssh; echo "create: $(( $(now)-t0 )) s (stop+snapshot+start+ssh)" ;;
  restore)
    # `volume revert` fails on the homeserver (Cinder has no oslo_concurrency lock_path) and needs an
    # unattached volume anyway. So: delete the server (volume kept), create a NEW volume from the clean
    # snapshot (LVM thin clone), boot the server from it. Old volumes are never deleted here; list them with
    # `snapshot.sh prune <vm>` and delete by hand. No user-data on re-create: the snapshot already holds users,
    # rules and config (cloud-init would otherwise rewrite /etc/nftables.conf with the boot-time version).
    case $vm in orch-vm) flavor=aigate.orch ;; agent-vm) flavor=aigate.agent ;; test-vm) flavor=aigate.test ;; esac
    t0=$(now); osw server show "$vm" -f value -c id >/dev/null 2>&1 && osw server delete --wait "$vm"; t1=$(now)
    size=$(osw volume snapshot show "$snap" -f value -c size); newvol="$vm-root-$(date +%Y%m%d-%H%M%S)-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
    osw volume create --snapshot "$snap" --size "$size" "$newvol" >/dev/null; wait_status volume "$newvol" available; t2=$(now)
    net=$(osw network show aigate-net -f value -c id)
    osw server create --flavor "$flavor" --volume "$newvol" --nic "net-id=$net,v4-fixed-ip=$ip_inner" --key-name aigate-key --wait "$vm" -f value -c status >/dev/null; t3=$(now)
    wait_ssh; t4=$(now)
    echo "restore: total $((t4-t0)) s = delete-server $((t1-t0)) + volume-from-snapshot $((t2-t1)) + create-server $((t3-t2)) + ssh-ready $((t4-t3))  [volume $newvol]" ;;
  prune)
    echo "volumes of $vm (candidates for manual deletion except the one the server boots from; golden volume is $vol, snapshot origin):"
    osw volume list -f value -c ID -c Name | grep -E " $vm-root(-[0-9]+)*(-[0-9a-f]{4})?$" || true ;;
  time)
    for i in $(seq 1 "$n"); do
      sshvm "sudo touch /var/tmp/dirty-$i"
      printf 'run %s: ' "$i"; "$0" restore "$vm"
      sshvm "test ! -e /var/tmp/dirty-$i" && echo "       marker gone (volume really reverted)" || die "marker still present: revert did not take effect"
    done ;;
  *) die "usage: snapshot.sh create|restore|time|prune|prune <vm> [N]" ;;
esac
