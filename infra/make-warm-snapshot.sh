#!/bin/sh
# make-warm-snapshot.sh [--apply] --name <test-vm-clean...> [--repo <name>] [--warm-cmd '<cmd>'] [--switch-service]
# Builds a cache-warm test-vm snapshot (Go module + build caches pre-filled, no leftovers) so that every
# restored test-vm starts with warm caches. Default: DRY-RUN (read-only preflight + numbered plan). --apply executes.
#   --name <n>          new snapshot, must match ^test-vm-clean[A-Za-z0-9._-]*$ and must not exist yet
#   --repo <r>          repo under ~orch/repos on orch-vm used to warm the caches (default sample-app)
#   --warm-cmd '<c>'    warm-up command run in the clone (default: go build ./... && go vet ./... && go test -count=1 ./...)
#   --switch-service    also point orchd (--test-snapshot in /etc/systemd/system/orchd.service on orch-vm) at <n> and restart it
# Preflight (always, read-only): name free, no orchestrator task running, test-vm ACTIVE and answering ssh, Cinder quota
#   headroom (gigabytes in use + 30 <= limit), repo present, and with --switch-service the unit has --test-snapshot exactly once.
# Apply: 1) bundle the repo to test-vm  2) clone + warm caches  3) clean leftovers and assert clean
#        4) snapshot.sh create test-vm (stops the VM, snapshots, starts it)  5) optional service switch  6) report.
# This script never deletes snapshots or volumes (step 3 only cleans files inside test-vm).
# Env: AIGATE_ADMIN_KEY (admin key), AIGATE_SNAPSHOT_SCRIPT (tests only: replaces infra/snapshot.sh).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
SNAP_SCRIPT=${AIGATE_SNAPSHOT_SCRIPT:-$here/snapshot.sh}
OSH=openstack-host
OSC='openstack --os-cloud exporter-internal'
ORCH=ubuntu@192.168.1.200
VMIP=10.70.0.13
UNIT=/etc/systemd/system/orchd.service
NEED_GB=30
DEFAULT_WARM='go build ./... && go vet ./... && go test -count=1 ./...'
# strict-host-key orchestrator ssh to the runner (same options as the orchestrator's own runner login)
RSSH_OPTS='-F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/home/orch/.ssh/known_hosts_aigate -o IdentitiesOnly=yes -o IdentityAgent=none -i /home/orch/.ssh/id_aigate'

usage() { echo "usage: make-warm-snapshot.sh [--apply] --name <test-vm-clean...> [--repo <name>] [--warm-cmd '<command>'] [--switch-service]" >&2; exit 64; }
stop() { echo "STOP $*" >&2; exit 1; }
ok() { echo "ok   $*"; }
# quote one argument for a POSIX shell (always single quotes)
q() { printf "'%s'" "$(printf %s "$1" | sed "s/'/'\\\\''/g")"; }

APPLY=0; NAME=; REPO=sample-app; WARM=$DEFAULT_WARM; SWITCH=0
while [ $# -gt 0 ]; do
  case $1 in
    --apply) APPLY=1 ;;
    --switch-service) SWITCH=1 ;;
    --name) [ $# -ge 2 ] || usage; NAME=$2; shift ;;
    --repo) [ $# -ge 2 ] || usage; REPO=$2; shift ;;
    --warm-cmd) [ $# -ge 2 ] || usage; WARM=$2; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
  shift
done
[ -n "$NAME" ] || { echo "--name is required" >&2; usage; }
printf %s "$NAME" | grep -Eq '^test-vm-clean[A-Za-z0-9._-]*$' || { echo "bad --name '$NAME' (must match ^test-vm-clean[A-Za-z0-9._-]*\$)" >&2; exit 64; }
printf %s "$REPO" | grep -Eq '^[A-Za-z0-9._-]+$' || { echo "bad --repo (must match ^[A-Za-z0-9._-]+\$)" >&2; exit 64; }
case $REPO in .|..) echo "bad --repo" >&2; exit 64 ;; esac
case $WARM in
  *'
'*) echo "bad --warm-cmd: newline not allowed" >&2; exit 64 ;;
  *'`'*|*'$('*) echo "bad --warm-cmd: backtick and \$( not allowed" >&2; exit 64 ;;
esac
[ -n "$WARM" ] || { echo "bad --warm-cmd: empty" >&2; exit 64; }

# --- remote helpers (stdin always /dev/null) ---
osq() { ssh -o BatchMode=yes "$OSH" "$OSC $1" < /dev/null; }
orchssh() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 -o ProxyCommand="ssh -o BatchMode=yes $OSH -W %h:%p" "$ORCH" "$1" < /dev/null; }
runner_cmd() { printf %s "sudo -u orch ssh $RSSH_OPTS runner@$VMIP $(q "$1")"; }   # command string for orch-vm
runner() { orchssh "$(runner_cmd "$1")"; }
# printable form of an orchssh call (for the "do it later" one-liner)
orchssh_line() { printf '%s' "ssh -i $(q "$K") -o IdentitiesOnly=yes -o BatchMode=yes -o ProxyCommand='ssh -o BatchMode=yes $OSH -W %h:%p' $ORCH $(q "$1")"; }

# --- every remote command string is built exactly once ---
CMD_TASKS="sudo -u orch /usr/local/lib/aigate/orch --socket /home/orch/orchd/run/orchd.sock list"
CMD_REPO="sudo -u orch git -C /home/orch/repos/$REPO rev-parse --git-dir"
CMD_UNIT_COUNT="grep -c -- --test-snapshot $UNIT || true"
BUNDLE_INNER="git -C /home/orch/repos/$REPO bundle create - HEAD | ssh $RSSH_OPTS runner@$VMIP 'cat > /tmp/warm.bundle'"
CMD_BUNDLE="sudo -u orch sh -c $(q "$BUNDLE_INNER")"
WARM_INNER="set -e; export PATH=\"\$PATH:/usr/local/go/bin\"; rm -rf /tmp/warm; git clone -q /tmp/warm.bundle /tmp/warm; cd /tmp/warm; t0=\$(date +%s); timeout 1500 sh -c $(q "$WARM"); echo \"warm wall time: \$(( \$(date +%s) - t0 )) s\"; du -sh \$HOME/.cache/go-build \$HOME/go/pkg/mod"
CLEAN_INNER='rm -rf /tmp/warm /tmp/warm.bundle /tmp/aigate-*; rm -rf /srv/aigate/repo.git /srv/aigate/work/* 2>/dev/null || sudo -n rm -rf /srv/aigate/repo.git /srv/aigate/work/*; rm -f "$HOME/.bash_history"'
ASSERT_INNER='bad=""; [ ! -e /srv/aigate/repo.git ] || bad="$bad repo.git"; [ -z "$(ls -A /srv/aigate/work 2>/dev/null)" ] || bad="$bad work-not-empty"; ls /tmp/warm /tmp/warm.bundle /tmp/aigate-* >/dev/null 2>&1 && bad="$bad tmp-leftovers"; f=$(find "$HOME" \( -path "$HOME/.cache/go-build" -o -path "$HOME/go/pkg/mod" \) -prune -o -type f \( -name "*.env" -o -name "id_*" \) -print 2>/dev/null); [ -z "$f" ] || bad="$bad files:$f"; if [ -z "$bad" ]; then echo CLEAN; else echo "DIRTY:$bad"; exit 1; fi'
CMD_CLEAN=$(runner_cmd "$CLEAN_INNER")
CMD_ASSERT=$(runner_cmd "$ASSERT_INNER")
CMD_WARM=$(runner_cmd "$WARM_INNER")
SWITCH_INNER="set -e; f=$UNIT; n=\$(grep -o -- --test-snapshot \$f | wc -l); [ \"\$n\" = 1 ] || { echo \"STOP: --test-snapshot appears \$n times in \$f\"; exit 3; }; [ ! -e \$f.pre-$NAME ] || { echo \"STOP: \$f.pre-$NAME exists\"; exit 3; }; sudo cp -p \$f \$f.pre-$NAME; sudo sed -i 's/--test-snapshot [^ ]*/--test-snapshot $NAME/' \$f; [ \"\$(grep -c -- '--test-snapshot $NAME' \$f)\" = 1 ] || { echo \"STOP: edit failed, backup is \$f.pre-$NAME\"; exit 3; }; sudo systemctl daemon-reload; sudo systemctl restart orchd; i=0; while [ \$i -lt 10 ] && ! systemctl is-active --quiet orchd; do sleep 1; i=\$((i+1)); done; echo \"orchd is-active: \$(systemctl is-active orchd)\"; systemctl is-active --quiet orchd"
CMD_SWITCH="bash -c $(q "$SWITCH_INNER")"

# --- preflight (read-only) ---
echo "== preflight (read-only) for snapshot $NAME, repo $REPO"
snaps=$(osq "volume snapshot list -f value -c Name") || stop "cannot list snapshots"
printf '%s\n' "$snaps" | grep -qxF -- "$NAME" && stop "snapshot $NAME already exists; pick a new name"
ok "snapshot name $NAME is free"
tasks=$(orchssh "$CMD_TASKS") || stop "cannot read the orchestrator task list (orch-vm unreachable?)"
busy=$(printf '%s\n' "$tasks" | awk 'NR>1 && $2 ~ /^(queued|preparing|agent_running|collecting|verifying|publishing|passed|error)$/ {print $1":"$2}' | tr '\n' ' ')
[ -z "$busy" ] || stop "orchestrator tasks still active: $busy"
ok "no orchestrator task is running"
orchssh "$CMD_REPO" >/dev/null 2>&1 || stop "repo /home/orch/repos/$REPO not found on orch-vm"
ok "repo ~orch/repos/$REPO present"
st=$(osq "server show test-vm -f value -c status") || st=unknown
[ "$st" = ACTIVE ] || stop "test-vm is '$st', not ACTIVE; run one orchestrator task first (its restore re-creates test-vm), then retry"
runner true >/dev/null 2>&1 || stop "test-vm does not answer ssh; run one orchestrator task first (its restore re-creates test-vm), then retry"
ok "test-vm ACTIVE and answers ssh"
qj=$(osq "quota show --usage -f json") || stop "cannot read quota"
# prints "<used> <limit>" for gigabytes (tolerates list-of-rows and dict layouts)
ul=$(printf %s "$qj" | python3 -I -c '
import sys, json
d = json.load(sys.stdin)
def norm(r): return {str(k).lower().replace(" ", "_"): v for k, v in r.items()}
if isinstance(d, dict) and "gigabytes" in d and isinstance(d["gigabytes"], dict): r = norm(d["gigabytes"])
elif isinstance(d, list): r = next(norm(x) for x in d if norm(x).get("resource") == "gigabytes")
else: raise SystemExit(1)
print(int(r["in_use"]), int(r["limit"]))' 2>/dev/null) || stop "cannot parse quota output (gigabytes)"
used=${ul% *}; limit=${ul#* }
if [ "$limit" -ge 0 ] && [ $((used + NEED_GB)) -gt "$limit" ]; then
  stop "Cinder quota: gigabytes $used used of $limit, need $NEED_GB free. Free space first with infra/cleanup-old-snapshots.sh and infra/cleanup-restore-volumes.sh"
fi
ok "quota gigabytes $used/$limit (+$NEED_GB fits)"
if [ $SWITCH = 1 ]; then
  n=$(orchssh "$CMD_UNIT_COUNT") || stop "cannot read $UNIT on orch-vm"
  [ "$n" = 1 ] || stop "--test-snapshot appears $n times in $UNIT (need exactly once); not switching"
  ok "$UNIT has --test-snapshot exactly once"
fi

SWITCH_LATER="$(orchssh_line "$CMD_SWITCH")"
echo "== plan"
echo "  1) on orch-vm as orch: git bundle of ~orch/repos/$REPO HEAD piped over the strict-host-key ssh to runner@$VMIP:/tmp/warm.bundle"
echo "  2) on test-vm as runner: clone to /tmp/warm, run (timeout 1500): $WARM ; print du -sh of ~/.cache/go-build ~/go/pkg/mod and wall time"
echo "  3) on test-vm: remove /tmp/warm /tmp/warm.bundle /srv/aigate/repo.git /srv/aigate/work/* /tmp/aigate-* ~/.bash_history; assert clean (no repo, empty work, no *.env / id_* outside go caches)"
echo "  4) AIGATE_SNAPSHOT=$NAME bash $SNAP_SCRIPT create test-vm ; verify snapshot is available"
if [ $SWITCH = 1 ]; then echo "  5) orch-vm: back up $UNIT as orchd.service.pre-$NAME, set --test-snapshot $NAME, daemon-reload, restart orchd, check is-active"
else echo "  5) (not requested) service switch; run later with the one-liner printed at the end"; fi
echo "  6) print snapshot list and quota usage"
if [ $APPLY != 1 ]; then echo "dry-run: nothing was changed. Re-run with --apply."; exit 0; fi

# --- apply ---
echo "== 1) bundle $REPO -> test-vm:/tmp/warm.bundle"
orchssh "$CMD_BUNDLE" || stop "bundle transfer failed"
echo "== 2) clone and warm caches on test-vm (timeout 1500 s)"
orchssh "$CMD_WARM" || stop "warm-up failed (test-vm still holds /tmp/warm, caches and bundle; no snapshot taken)"
echo "== 3) clean leftovers on test-vm and assert clean"
orchssh "$CMD_CLEAN" || stop "cleanup command failed; no snapshot taken"
res=$(orchssh "$CMD_ASSERT") || stop "test-vm is not clean ($res); no snapshot taken"
case $res in CLEAN) ok "test-vm clean" ;; *) stop "test-vm is not clean ($res); no snapshot taken" ;; esac
echo "== 4) snapshot $NAME (stops test-vm, snapshots, starts it)"
AIGATE_SNAPSHOT=$NAME bash "$SNAP_SCRIPT" create test-vm || stop "snapshot.sh create failed"
s=$(osq "volume snapshot show $(q "$NAME") -f value -c status") || s=unknown
[ "$s" = available ] || stop "snapshot $NAME status is '$s', expected available"
ok "snapshot $NAME is available"
if [ $SWITCH = 1 ]; then
  echo "== 5) switch orchd to $NAME"
  orchssh "$CMD_SWITCH" || stop "service switch failed (backup: $UNIT.pre-$NAME on orch-vm if it was created)"
  ok "orchd restarted with --test-snapshot $NAME"
  echo "NOTE: update infra/systemd/orchd-orchvm.service in the repo (--test-snapshot $NAME) to match; this script does not edit it."
else
  echo "== 5) service switch not requested. To do it later run:"
  echo "  $SWITCH_LATER"
  echo "  (and update infra/systemd/orchd-orchvm.service in the repo to --test-snapshot $NAME)"
fi
echo "== 6) result"
osq "volume snapshot list" || true
osq "quota show --usage" || true
echo "The previous snapshot can be removed with infra/cleanup-old-snapshots.sh, but only after editing its CURRENT and OLD_SNAPS (and OLD_VOLS) constants by hand: they are hard-coded and this script does not change them."
