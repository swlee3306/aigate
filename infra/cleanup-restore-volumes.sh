#!/bin/sh
# cleanup-restore-volumes.sh [--apply]      (default: dry-run, lists what would be deleted)
# Deletes leftover test-vm restore volumes (scratch clones made by infra/snapshot.sh / TestVM.Restore).
# A volume is deleted ONLY if all hold:
#   - its name is test-vm-root-<HHMMSS> or test-vm-root-<YYYYMMDD>-<HHMMSS>-<hex4>   (never the golden test-vm-root)
#   - it is not the source volume of any snapshot
#   - it is not attached to a server that still exists (attachments to deleted servers are stale and are cleared first)
# Run it yourself (`! sh infra/cleanup-restore-volumes.sh --apply`): deletion is irreversible.
set -eu
APPLY=0; [ "${1:-}" = "--apply" ] && APPLY=1
OS='ssh -o BatchMode=yes openstack-host openstack --os-cloud exporter-internal'
export APPLY
python3 - "$OS" <<'PY'
import json, re, subprocess, sys, os
OS = sys.argv[1].split()
def osc(*a, v=None):
    cmd = OS + ([f"--os-volume-api-version", v] if v else []) + list(a)
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
vols = json.loads(osc("volume", "list", "-f", "json", "-c", "ID", "-c", "Name", "-c", "Status"))
snaps = [json.loads(osc("volume", "snapshot", "show", s["Name"], "-f", "json", "-c", "volume_id"))["volume_id"]
         for s in json.loads(osc("volume", "snapshot", "list", "-f", "json", "-c", "Name"))]
servers = {s["ID"] for s in json.loads(osc("server", "list", "--all-projects", "-f", "json", "-c", "ID"))}
pat = re.compile(r"^test-vm-root-(\d{6}|\d{8}-\d{6}-[0-9a-f]{4})$")
todo = []
for v in vols:
    n = v["Name"]
    if not pat.match(n): continue
    d = json.loads(osc("volume", "show", v["ID"], "-f", "json", "-c", "attachments"))
    atts = d["attachments"]
    why = None
    if v["ID"] in snaps: why = "source volume of a snapshot (kept)"
    elif any(a["server_id"] in servers for a in atts): why = "attached to an existing server (kept)"
    print(f'{n:40s} {v["ID"]}  ', why or "DELETE (stale attachments: %d)" % len(atts))
    if not why: todo.append((v["ID"], n, atts))
if not todo: print("nothing to delete"); sys.exit(0)
if os.environ.get("APPLY") != "1":
    print(f"\ndry-run: {len(todo)} volume(s) would be deleted. Re-run with --apply."); sys.exit(0)
for vid, n, atts in todo:
    for a in atts:
        osc("volume", "attachment", "delete", a["attachment_id"], v="3.44")
    osc("volume", "delete", vid)
    print("deleted", n, vid)
PY
