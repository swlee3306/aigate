#!/bin/sh
# power-check.sh : after powering the home servers back on, verify that everything came back by itself.
# Read-only. Exit 0 if no FAIL (WARN is allowed), 1 otherwise.
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
fail=0; ok() { echo "ok    $*"; }; warn() { echo "WARN  $*"; }; bad() { echo "FAIL  $*"; fail=1; }
OSC='openstack --os-cloud exporter-internal'
echo "== homeserver (openstack-host)"
if ssh -o BatchMode=yes -o ConnectTimeout=8 openstack-host true 2>/dev/null; then
  ok "ssh reachable, $(ssh -o BatchMode=yes openstack-host 'uptime -p')"
  n=$(ssh openstack-host 'systemctl --failed --no-legend | wc -l'); [ "$n" = 0 ] && ok "no failed systemd units" || bad "$n failed systemd units: $(ssh openstack-host 'systemctl --failed --no-legend | cut -d" " -f2 | tr "\n" " "')"
  srv=$(ssh openstack-host "$OSC server list -f value -c Name -c Status" 2>&1)
  for v in k8s-cp-01 vm-bastion orch-vm agent-vm; do echo "$srv" | grep -q "^$v ACTIVE" && ok "VM $v ACTIVE" || bad "VM $v not ACTIVE ($(echo "$srv" | grep "^$v " || echo missing))"; done
  echo "$srv" | grep -q "^test-vm ACTIVE" && ok "VM test-vm ACTIVE" || warn "test-vm not ACTIVE (it is re-created by the next task's restore; see 'orch' docs)"
  echo "$srv" | grep -qE "ERROR|SHUTOFF" && warn "VMs in ERROR/SHUTOFF: $(echo "$srv" | grep -E 'ERROR|SHUTOFF' | tr '\n' ';')"
  ssh openstack-host "$OSC volume list -f value -c Status" | grep -qx error && bad "a Cinder volume is in error state" || ok "no Cinder volume in error"
  ssh openstack-host "$OSC compute service list -f value -c Binary -c State" | grep -qv up && bad "a compute service is down" || ok "nova services up"
else bad "homeserver not reachable"; fi
echo "== orch-vm"
if ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 ubuntu@192.168.1.200 true 2>/dev/null; then
  ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes ubuntu@192.168.1.200 'systemctl is-active orchd >/dev/null && echo "ok    orchd service active ($(systemctl show orchd -p NRestarts --value) restarts)" || echo "FAIL  orchd service not active"; sudo -u orch /home/orch/bin/orch --socket /home/orch/orchd/run/orchd.sock list 2>&1 | awk "NR>1{print \$2}" | sort | uniq -c | tr "\n" " " | sed "s/^/ok    tasks in DB: /"; echo; sudo nft list ruleset 2>/dev/null | grep -c "policy drop" | sed "s/^/ok    nft default-drop chains loaded: /"' | tee /dev/stderr | grep -q '^FAIL' && fail=1
else bad "orch-vm not reachable"; fi
echo "== gpu-pc (gpu-server)"
if ssh -o BatchMode=yes -o ConnectTimeout=8 gpu-server true 2>/dev/null; then
  ssh -o BatchMode=yes gpu-server 'echo "ok    $(uname -sr), $(uptime -p)"; systemctl is-active k3s-agent >/dev/null && echo "ok    k3s-agent active" || echo "FAIL  k3s-agent not active"; nvidia-smi --query-gpu=name --format=csv,noheader >/dev/null 2>&1 && echo "ok    nvidia-smi works" || echo "FAIL  nvidia-smi fails"; n=$(docker ps -q | wc -l); echo "ok    $n containers running (monitoring stack expects 3)"; systemctl --failed --no-legend | wc -l | sed "s/^/ok    failed units: /"' | tee /dev/stderr | grep -q '^FAIL' && fail=1
else bad "gpu-pc not reachable (is it in Windows? default boot is Ubuntu)"; fi
[ $fail = 0 ] && echo "RESULT: all good" || echo "RESULT: something needs attention"; exit $fail
