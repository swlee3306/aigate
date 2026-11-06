#!/bin/sh
# power-off.sh [--apply] : shut the home servers down in a safe order. Default: dry-run (prints the plan and checks).
#   1. refuse if orchd has a task in a running state (preparing/agent_running/collecting/verifying/publishing)
#   2. gpu-pc first (k3s worker), then the homeserver last (libvirt-guests saves every VM's memory, then it powers off)
# Power on: homeserver first, wait ~5 min, then gpu-pc; then `sh infra/power-check.sh`.
set -eu
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}; APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1
busy=$(ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes ubuntu@192.168.1.200 'sudo -u orch /home/orch/bin/orch --socket /home/orch/orchd/run/orchd.sock list' 2>/dev/null | awk 'NR>1 && $2 ~ /^(queued|preparing|agent_running|collecting|verifying|publishing|passed|error)$/ {print $1":"$2}' | tr '\n' ' ')
[ -z "$busy" ] && echo "ok   no orchestrator task is running" || { echo "STOP orchestrator tasks still active: $busy"; [ $APPLY = 1 ] && exit 1; }
echo "plan: 1) gpu-pc: sudo shutdown -h now   2) wait until it stops answering   3) homeserver: sudo shutdown -h now"
[ $APPLY = 1 ] || { echo "dry-run: nothing was shut down. Re-run with --apply."; exit 0; }
ssh -o BatchMode=yes gpu-server 'sudo shutdown -h +0' 2>&1 | tail -1 || true
i=0; while ssh -o BatchMode=yes -o ConnectTimeout=3 gpu-server true 2>/dev/null && [ $i -lt 40 ]; do sleep 3; i=$((i+1)); done; echo "gpu-pc is down (or timed out)"
ssh -o BatchMode=yes openstack-host 'sudo shutdown -h +0' 2>&1 | tail -1 || true
echo "homeserver shutdown started: libvirt-guests saves the VM memory first, give it a few minutes before cutting power"
