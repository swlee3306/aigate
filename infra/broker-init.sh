#!/bin/sh
# broker-init.sh [--apply]
# Prepares the home of orch-broker: the account `orch-broker` (no login, no home), /etc/orch-broker
# (root:orch-broker 0750) and /var/lib/orch-broker (orch-broker:orch-broker 0700). Default is a dry run.
# It NEVER creates a key or token file: you place them yourself (instructions are printed).
set -eu
ETC=${BROKER_ETC_DIR:-/etc/orch-broker}
STATE=${BROKER_STATE_DIR:-/var/lib/orch-broker}
ACCOUNT=orch-broker
APPLY=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    *) echo "usage: $0 [--apply]" >&2; exit 2 ;;
  esac
done
die() { echo "broker-init: $*" >&2; exit 1; }

mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null || echo '?'; }
unsafe() { # symlink or group/other-writable
  [ -L "$1" ] && { echo "$1 is a symlink"; return 0; }
  [ -e "$1" ] || return 1
  m=$(mode_of "$1")
  case "$m" in ''|'?') echo "cannot read mode of $1"; return 0 ;; esac
  g=$(( (m / 10) % 10 )); o=$(( m % 10 ))
  if [ $((g & 2)) -ne 0 ] || [ $((o & 2)) -ne 0 ]; then echo "$1 is group/other-writable (mode $m)"; return 0; fi
  return 1
}

echo "== state"
if id "$ACCOUNT" >/dev/null 2>&1; then echo "account $ACCOUNT: exists"; else echo "account $ACCOUNT: missing"; fi
for d in "$ETC" "$STATE"; do
  if [ -L "$d" ]; then echo "$d: SYMLINK"; elif [ -d "$d" ]; then echo "$d: exists, mode $(mode_of "$d")"; else echo "$d: missing"; fi
done
for f in "$ETC/broker.json" "$ETC/openai.key" "$ETC/gitlab.token"; do
  if [ -e "$f" ]; then echo "$f: exists, mode $(mode_of "$f")"; else echo "$f: absent (this script never creates it)"; fi
done

echo "== plan"
echo "useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin $ACCOUNT   (if missing)"
echo "install -d -o root -g $ACCOUNT -m 0750 $ETC"
echo "install -d -o $ACCOUNT -g $ACCOUNT -m 0700 $STATE"
echo "== next steps (text only; run them yourself)"
echo "  # broker.json: root-owned, mode 0644 or 0640, written by you (see docs/14-DESIGN-webconsole-W2.md)"
echo "  sudo install -m 0644 -o root -g root broker.json $ETC/broker.json"
echo "  # the AI key and GitLab token: put them yourself, owner orch-broker, mode 0400"
echo "  sudo install -m 0400 -o $ACCOUNT -g $ACCOUNT /path/to/openai.key $ETC/openai.key"
echo "  sudo install -m 0400 -o $ACCOUNT -g $ACCOUNT /path/to/gitlab.token $ETC/gitlab.token"

if [ "$APPLY" = 0 ]; then echo "(dry run: add --apply as root to create the account and directories)"; exit 0; fi
[ "$(id -u)" = 0 ] || die "--apply needs root; nothing changed"
for p in "$(dirname "$ETC")" "$(dirname "$STATE")" "$ETC" "$STATE"; do
  if r=$(unsafe "$p"); then die "refusing: $r"; fi
done
if ! id "$ACCOUNT" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$ACCOUNT" || die "cannot create account $ACCOUNT"
fi
install -d -o root -g "$ACCOUNT" -m 0750 "$ETC"
install -d -o "$ACCOUNT" -g "$ACCOUNT" -m 0700 "$STATE"
for p in "$ETC" "$STATE"; do
  if r=$(unsafe "$p"); then die "refusing after create: $r"; fi
done
echo "done: $ETC root:$ACCOUNT 0750, $STATE $ACCOUNT:$ACCOUNT 0700 (no key or token file created)"
