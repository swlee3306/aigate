#!/bin/sh
# signers-init.sh [--apply]
# Creates /etc/aigate (root:root 0755), the home of the signers registry. It does NOT create signers.json:
# the file's presence turns the approval gate to signed-only mode, so the cutover step creates it together with
# the first key (`orch admin signers add`). Default is a dry run (prints the state and the next steps).
set -eu
DIR=${AIGATE_ETC_DIR:-/etc/aigate}
APPLY=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    *) echo "usage: $0 [--apply]" >&2; exit 2 ;;
  esac
done
die() { echo "signers-init: $*" >&2; exit 1; }

mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1" 2>/dev/null || echo '?'; }
unsafe() { # symlink or group/other-writable
  [ -L "$1" ] && { echo "$1 is a symlink"; return 0; }
  [ -e "$1" ] || return 1
  m=$(mode_of "$1")
  case "$m" in ''|'?') echo "cannot read mode of $1"; return 0 ;; esac
  # last two octal digits: group and other; write bit = 2 (2,3,6,7)
  g=$(( (m / 10) % 10 )); o=$(( m % 10 ))
  if [ $((g & 2)) -ne 0 ] || [ $((o & 2)) -ne 0 ]; then echo "$1 is group/other-writable (mode $m)"; return 0; fi
  return 1
}

echo "== state"
if [ -d "$DIR" ] && [ ! -L "$DIR" ]; then echo "$DIR: exists, mode $(mode_of "$DIR")"; elif [ -L "$DIR" ]; then echo "$DIR: SYMLINK"; else echo "$DIR: missing"; fi
if [ -e "$DIR/signers.json" ]; then echo "$DIR/signers.json: exists, mode $(mode_of "$DIR/signers.json") (gate is in signed-only mode)"; else echo "$DIR/signers.json: absent (legacy mode; this script never creates it)"; fi

echo "== plan"
echo "mkdir -p $DIR; chown root:root $DIR; chmod 0755 $DIR"
echo "== next steps (text only, run them yourself at the cutover; step 5 switches the gate to signed-only mode)"
echo "  sudo orch admin signers add --id <ID> --key ~/.ssh/id_ed25519.pub     # creates $DIR/signers.json"
echo "  sudo orch admin signers list"

if [ "$APPLY" = 0 ]; then echo "(dry run: add --apply as root to create the directory)"; exit 0; fi
[ "$(id -u)" = 0 ] || die "--apply needs root; nothing changed"
parent=$(dirname "$DIR")
if r=$(unsafe "$parent"); then die "refusing: $r"; fi
if r=$(unsafe "$DIR"); then die "refusing: $r"; fi
mkdir -p "$DIR"
# re-check after mkdir (a race could have swapped it in)
if r=$(unsafe "$DIR"); then die "refusing: $r"; fi
chown root:root "$DIR"
chmod 0755 "$DIR"
echo "done: $DIR root:root 0755 (signers.json not created)"
