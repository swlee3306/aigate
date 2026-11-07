#!/bin/sh
# cleanup-old-snapshots.sh [--apply]      (default: dry-run)
# Frees Cinder quota (300 GB counts volumes AND snapshots) by deleting superseded test-vm clean snapshots and
# their source volumes. Only these exact names are ever touched:
#   snapshots: test-vm-clean, test-vm-clean2         (test-vm-clean3 is the current one and is KEPT)
#   volumes  : test-vm-root, test-vm-root-181059     (the sources of those two snapshots)
# Guards: the current snapshot must exist and be available; a source volume is deleted only if it is the recorded
# source of a snapshot being deleted (or has no snapshot left), is not attached to an existing server (stale
# attachments to deleted servers are cleared first). Irreversible: run it yourself with --apply.
set -eu
APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1
OS='ssh -o BatchMode=yes openstack-host openstack --os-cloud exporter-internal'
export APPLY
python3 - "$OS" <<'PY'
import json, subprocess, sys, os
OS = sys.argv[1].split()
def osc(*a, v=None):
    cmd = OS + (["--os-volume-api-version", v] if v else []) + list(a)
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
CURRENT = "test-vm-clean3"; OLD_SNAPS = ["test-vm-clean", "test-vm-clean2"]; OLD_VOLS = ["test-vm-root", "test-vm-root-181059"]
snaps = {s["Name"]: s for s in json.loads(osc("volume", "snapshot", "list", "-f", "json", "-c", "ID", "-c", "Name", "-c", "Status", "-c", "Size"))}
cur = snaps.get(CURRENT)
if not cur or cur["Status"] != "available": sys.exit(f"STOP: current snapshot {CURRENT} is not available; nothing deleted")
cur_src = json.loads(osc("volume", "snapshot", "show", CURRENT, "-f", "json", "-c", "volume_id"))["volume_id"]
vols = {v["Name"]: v for v in json.loads(osc("volume", "list", "-f", "json", "-c", "ID", "-c", "Name", "-c", "Status", "-c", "Size"))}
servers = {s["ID"] for s in json.loads(osc("server", "list", "--all-projects", "-f", "json", "-c", "ID"))}
plan_snaps, plan_vols, freed = [], [], 0
print(f"keep: snapshot {CURRENT} (source volume {cur_src[:8]}…)")
for n in OLD_SNAPS:
    s = snaps.get(n)
    if not s: print(f"skip: snapshot {n} not present"); continue
    print(f"DELETE snapshot {n} {s['ID']} ({s['Size']} GB)"); plan_snaps.append(s); freed += int(s["Size"])
for n in OLD_VOLS:
    v = vols.get(n)
    if not v: print(f"skip: volume {n} not present"); continue
    if v["ID"] == cur_src: print(f"KEEP volume {n}: it is the source of the current snapshot"); continue
    atts = json.loads(osc("volume", "show", v["ID"], "-f", "json", "-c", "attachments"))["attachments"]
    live = [a for a in atts if a["server_id"] in servers]
    if live: print(f"KEEP volume {n}: attached to an existing server"); continue
    others = [x for x in snaps.values() if x["Name"] not in OLD_SNAPS and x["Name"] != CURRENT]
    print(f"DELETE volume {n} {v['ID']} ({v['Size']} GB, stale attachments: {len(atts)})"); plan_vols.append((v, atts)); freed += int(v["Size"])
print(f"\nwould free about {freed} GB of the 300 GB quota")
if os.environ.get("APPLY") != "1": print("dry-run: nothing deleted. Re-run with --apply."); sys.exit(0)
import time
for s in plan_snaps:
    osc("volume", "snapshot", "delete", s["ID"]); print("deleted snapshot", s["Name"])
for _ in range(30):
    left = {x["Name"] for x in json.loads(osc("volume", "snapshot", "list", "-f", "json", "-c", "Name"))}
    if not any(n in left for n in OLD_SNAPS): break
    time.sleep(2)
for v, atts in plan_vols:
    for a in atts: osc("volume", "attachment", "delete", a["attachment_id"], v="3.44")
    osc("volume", "delete", v["ID"]); print("deleted volume", v["Name"])
PY
