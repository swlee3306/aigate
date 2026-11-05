#!/usr/bin/env bash
# Create the aigate orchestrator VMs on the homeserver OpenStack. DRY-RUN by default.
#   create-vms.sh [--apply] [--only keypair|flavors|network|orchestrator|agent|test|fip]
#   create-vms.sh --render orchestrator|agent|test   (print rendered user-data, no ssh)
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"

AIGATE_KEY=${AIGATE_KEY:-$HOME/.ssh/aigate_ed25519}
PUBFILE="$AIGATE_KEY.pub"                         # ROLE public key -> role users in cloud-init
AIGATE_ADMIN_KEY=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
ADMIN_PUB="$AIGATE_ADMIN_KEY.pub"                 # ADMIN public key -> OpenStack keypair (ubuntu user)
WAIT_TRIES=${AIGATE_WAIT_TRIES:-60}; WAIT_SLEEP=${AIGATE_WAIT_SLEEP:-5}
IMAGE="Ubuntu 24.04"; NET=aigate-net; SUBNET=aigate-subnet; CIDR=10.70.0.0/24; GW=10.70.0.1; DNS=8.8.8.8
ROUTER=router-main; KEYPAIR=aigate-key; EXTNET=external-net
FIP_POOL_PREFIX=192.168.1; FIP_FIRST=200; FIP_LAST=254
FIP_NEVER="204 251 230 253"   # from infra/discovery.md: bastion, k8s-cp-01, router gateways

role_vm()     { case $1 in orchestrator) echo orch-vm ;; agent) echo agent-vm ;; test) echo test-vm ;; esac; }
role_flavor() { case $1 in orchestrator) echo aigate.orch ;; agent) echo aigate.agent ;; test) echo aigate.test ;; esac; }
role_ip()     { case $1 in orchestrator) echo 10.70.0.11 ;; agent) echo 10.70.0.12 ;; test) echo 10.70.0.13 ;; esac; }
role_size()   { case $1 in orchestrator) echo 10 ;; agent) echo 20 ;; test) echo 30 ;; esac; }
role_vcpus()  { case $1 in orchestrator) echo 1 ;; agent) echo 2 ;; test) echo 4 ;; esac; }
role_ram()    { case $1 in orchestrator) echo 2048 ;; agent) echo 4096 ;; test) echo 6144 ;; esac; }

usage() { sed -n '2,3p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

DO_APPLY=0; ONLY=""; RENDER=""
while [ $# -gt 0 ]; do
  case $1 in
    --apply) DO_APPLY=1 ;;
    --only) [ $# -ge 2 ] || usage 2; ONLY=$2; shift ;;
    --render) [ $# -ge 2 ] || usage 2; RENDER=$2; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
  shift
done
case "$ONLY" in ''|keypair|flavors|network|orchestrator|agent|test|fip) ;; *) die "invalid --only value: $ONLY" ;; esac
want() { [ -z "$ONLY" ] || [ "$ONLY" = "$1" ]; }

TMPD=$(mktemp -d); trap 'rm -rf "$TMPD"' EXIT
N=0
step() { N=$((N+1)); printf '[%d] %s\n' "$N" "$*"; }
skip() { printf '[--] SKIP %s (already exists)\n' "$*"; }

render_userdata() { # role -> path of rendered file (public key substituted)
  local src="$HERE/cloud-init/$1.yaml" dst="$TMPD/$1.yaml" pub content
  [ -f "$src" ] || die "missing $src"
  if [ -f "$PUBFILE" ]; then pub=$(cat "$PUBFILE"); else
    [ "${APPLY}" = 1 ] || [ -n "$RENDER" ] && die "public key $PUBFILE missing; run infra/keygen.sh first"
    pub="<public key from $PUBFILE>"
  fi
  content=$(cat "$src")
  printf '%s\n' "${content//@@AIGATE_PUBKEY@@/$pub}" > "$dst"
  printf %s "$dst"
}

stage_keypair() {
  if has_name keypair list -f value -c Name -- "$KEYPAIR"; then
    if [ -f "$ADMIN_PUB" ]; then
      local lfp rfp
      lfp=$(ssh-keygen -E md5 -lf "$ADMIN_PUB" | awk '{print $2}' | sed 's/^MD5://')
      rfp=$(os_read keypair show "$KEYPAIR" -f value -c fingerprint) || die "read query failed: keypair show"
      [ "$lfp" = "$rfp" ] || die "keypair $KEYPAIR exists but its fingerprint ($rfp) differs from the local admin key ($lfp); see README rollback"
      skip "keypair $KEYPAIR (fingerprint matches admin key)"
    else
      echo "WARN: $ADMIN_PUB missing, cannot compare keypair fingerprint" >&2
      skip "keypair $KEYPAIR"
    fi
    return
  fi
  step "CREATE keypair $KEYPAIR"
  os_in "$ADMIN_PUB" keypair create --public-key /dev/stdin "$KEYPAIR"
}

stage_flavors() {
  local r
  for r in orchestrator agent test; do
    if has_name flavor list -f value -c Name -- "$(role_flavor $r)"; then skip "flavor $(role_flavor $r)"; continue; fi
    step "CREATE flavor $(role_flavor $r)"
    os flavor create --vcpus "$(role_vcpus $r)" --ram "$(role_ram $r)" --disk 0 "$(role_flavor $r)"
  done
}

stage_network() {
  if has_name network list -f value -c Name -- "$NET"; then skip "network $NET"; else
    step "CREATE network $NET"; os network create "$NET"
  fi
  if has_name subnet list -f value -c Name -- "$SUBNET"; then skip "subnet $SUBNET"; else
    step "CREATE subnet $SUBNET"
    os subnet create --network "$NET" --subnet-range "$CIDR" --gateway "$GW" --dns-nameserver "$DNS" "$SUBNET"
  fi
  local ports
  ports=$(os_read port list --router "$ROUTER" -f value -c 'Fixed IP Addresses') || die "read query failed: port list"
  if printf '%s\n' "$ports" | grep -qF "'$GW'"; then skip "router-interface $ROUTER $SUBNET"; else
    step "CREATE router-interface $ROUTER $SUBNET"; os router add subnet "$ROUTER" "$SUBNET"
  fi
}

wait_volume() { # apply only: wait until the volume is available
  [ "$APPLY" = 1 ] || return 0
  local i s
  for i in $(seq 1 "$WAIT_TRIES"); do
    s=$(os_read volume show "$1" -f value -c status) || die "volume show failed"
    [ "$s" = available ] && return 0
    [ "$s" = error ] && die "volume $1 is in error state"
    sleep "$WAIT_SLEEP"
  done
  die "volume $1 not available after $((WAIT_TRIES * WAIT_SLEEP))s"
}

stage_volume() {
  local r=$1 vm; vm=$(role_vm "$r")
  if has_name volume list -f value -c Name -- "$vm-root"; then skip "volume $vm-root"; return; fi
  step "CREATE volume $vm-root"
  os volume create --image "$IMAGE" --size "$(role_size "$r")" "$vm-root"
}

net_id() {
  local id
  id=$(os_read network show "$NET" -f value -c id 2>/dev/null) || id=""
  if [ -z "$id" ]; then
    [ "$APPLY" = 1 ] && die "network $NET not found; create it first (--only network)"
    id="<id of $NET>"
  fi
  printf %s "$id"
}

# server_state <vm>: die unless an existing server is ACTIVE (never treat ERROR etc. as done)
require_active() {
  local st
  st=$(os_read server show "$1" -f value -c status) || die "read query failed: server show $1"
  [ "$st" = ACTIVE ] || die "server $1 exists in state $st - see README rollback"
}

stage_server() {
  local r=$1 vm; vm=$(role_vm "$r")
  if has_name server list -f value -c Name -- "$vm"; then require_active "$vm"; skip "server $vm (ACTIVE)"; return; fi
  step "CREATE server $vm"
  local ud nid
  ud=$(render_userdata "$r") || die "cannot render user-data for $r"
  [ -s "$ud" ] || die "rendered user-data for $r is empty"
  nid=$(net_id) || die "cannot resolve network id"
  wait_volume "$vm-root"
  os_in "$ud" server create --flavor "$(role_flavor "$r")" --volume "$vm-root" \
    --nic "net-id=$nid,v4-fixed-ip=$(role_ip "$r")" --key-name "$KEYPAIR" \
    --user-data /dev/stdin --wait -f value -c id -c status "$vm"
  [ "$APPLY" = 1 ] && require_active "$vm"
  return 0
}

pick_fip() { # first unused address in the pool; never a documented-used one
  local fips ports used i
  fips=$(os_read floating ip list -f value -c 'Floating IP Address') || die "read query failed: floating ip list"
  ports=$(os_read port list --network "$EXTNET" -f value -c 'Fixed IP Addresses') || die "read query failed: port list"
  used=$(printf '%s\n%s\n' "$fips" "$ports" | grep -oE "${FIP_POOL_PREFIX//./\\.}\\.[0-9]+" | sort -u || true)
  for i in $(seq "$FIP_FIRST" "$FIP_LAST"); do
    case " $FIP_NEVER " in *" $i "*) continue ;; esac
    printf '%s\n' "$used" | grep -qxF "$FIP_POOL_PREFIX.$i" && continue
    printf '%s.%s' "$FIP_POOL_PREFIX" "$i"; return 0
  done
  die "no free floating IP in $FIP_POOL_PREFIX.$FIP_FIRST-$FIP_LAST"
}

stage_fip() {
  local portid="<port id of orch-vm>" attached unatt ip
  if has_name server list -f value -c Name -- orch-vm; then
    require_active orch-vm
    portid=$(os_read port list --server orch-vm -f value -c ID | head -1) || die "read query failed: port list"
    [ -n "$portid" ] || die "orch-vm has no port"
    attached=$(os_read floating ip list --port "$portid" -f value -c 'Floating IP Address') \
      || die "read query failed: floating ip list"
    if [ -n "$attached" ]; then skip "floating-ip for orch-vm ($(printf %s "$attached" | head -1))"; return; fi
    unatt=$(os_read floating ip list --network "$EXTNET" --status DOWN -f value -c ID -c 'Floating IP Address') \
      || die "read query failed: floating ip list"
    if [ -n "$unatt" ]; then
      die "unattached floating IP(s) exist on $EXTNET (possible leftover): $(printf %s "$unatt" | tr '\n' ';'). If it is ours, remove with: openstack --os-cloud $OS_CLOUD floating ip delete <ID>"
    fi
  elif [ "$APPLY" = 1 ]; then
    die "orch-vm does not exist yet; create it before the floating ip"
  fi
  ip=$(pick_fip) || exit 1
  step "CREATE floating-ip $ip orch-vm"
  os floating ip create --port "$portid" --floating-ip-address "$ip" "$EXTNET"
}

run_all() {
  want keypair && stage_keypair
  want flavors && stage_flavors
  want network && stage_network
  if [ -z "$ONLY" ]; then
    local r
    for r in orchestrator agent test; do stage_volume "$r"; done
    for r in orchestrator agent test; do stage_server "$r"; done
  else
    case "$ONLY" in orchestrator|agent|test) stage_volume "$ONLY"; stage_server "$ONLY" ;; esac
  fi
  want fip && stage_fip
  return 0
}

count_steps() { grep -cE '^\[[0-9]+\] ' <<<"$1" || true; }

if [ -n "$RENDER" ]; then
  case "$RENDER" in orchestrator|agent|test) ;; *) die "invalid --render value: $RENDER" ;; esac
  cat "$(render_userdata "$RENDER")"; exit 0
fi
if [ "$DO_APPLY" = 1 ]; then
  [ -f "$PUBFILE" ] || die "role public key $PUBFILE missing; run infra/keygen.sh first"
  [ -f "$ADMIN_PUB" ] || die "admin public key $ADMIN_PUB missing; run infra/keygen.sh first"
fi
echo "aigate create-vms (host: $OS_HOST, cloud: $OS_CLOUD, only: ${ONLY:-all})"
plan=$(APPLY=0 run_all) || exit 1
printf '%s\n' "$plan"
n=$(count_steps "$plan")

if [ "$DO_APPLY" = 0 ]; then
  echo "== $n planned (dry-run: nothing was created; only list/show queries ran). Use --apply to execute."
  exit 0
fi
if [ "$n" -eq 0 ]; then echo "== 0 planned: nothing to do."; exit 0; fi
echo
echo "About to CREATE the $n resources/steps listed above on $OS_HOST."
echo "Type exactly 'apply aigate' to continue:"
read -r answer
[ "$answer" = "apply aigate" ] || { echo "aborted: confirmation did not match."; exit 1; }
N=0
APPLY=1
run_all
echo "== done. Verify with: ssh $OS_HOST 'openstack --os-cloud $OS_CLOUD server list'"
