#!/bin/sh
# task-timing.sh [IDs...]      read-only: per-step durations of orchestrator tasks (default: the last 5 tasks)
# Reads the orch-vm database through the homeserver jump (same path as power-check.sh). Step time = time until the
# next event; the `verifying` row carries the restore/gate breakdown once the verify note has it.
set -eu
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
ids="$*"
ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 \
    -o ProxyCommand="ssh -o BatchMode=yes openstack-host -W %h:%p" ubuntu@192.168.1.200 \
    "sudo -u orch python3 - $ids" <<'PY'
import sqlite3, sys
c = sqlite3.connect("file:/home/orch/orchd/state/orchd.db?mode=ro", uri=True)
ids = [int(a) for a in sys.argv[1:] if a.isdigit()]
if not ids:
    ids = sorted([r[0] for r in c.execute("select id from tasks order by id desc limit 5")])
def fmt(sec): return f"{sec:.1f}s" if sec < 10 else f"{sec:.0f}s"
for tid in ids:
    t = c.execute("select state, class, agent from tasks where id=?", (tid,)).fetchone()
    if not t: print(f"task {tid}: not found"); continue
    ev = list(c.execute("select to_state, at, note from task_events where task_id=? order by id", (tid,)))
    print(f"== task {tid} [{t[1]}/{t[2]}] final: {t[0]}")
    total = (ev[-1][1] - ev[0][1]) / 1e9 if len(ev) > 1 else 0
    for i, (st, at, note) in enumerate(ev):
        dur = (ev[i+1][1] - at) / 1e9 if i + 1 < len(ev) else 0
        print(f"   {st:14s} {fmt(dur):>7s}  {(note or '')[:150]}")
    print(f"   {'TOTAL':14s} {fmt(total):>7s}")
PY
