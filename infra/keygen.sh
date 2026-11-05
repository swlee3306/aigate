#!/usr/bin/env bash
# Create the dedicated aigate ssh key (ed25519, no passphrase). Never overwrites.
set -u
KEY=${AIGATE_KEY:-$HOME/.ssh/aigate_ed25519}
if [ -e "$KEY" ] || [ -e "$KEY.pub" ]; then
  echo "refusing to overwrite: $KEY already exists." >&2
  [ -f "$KEY.pub" ] && ssh-keygen -lf "$KEY.pub" >&2
  exit 1
fi
mkdir -p "$(dirname "$KEY")" && chmod 700 "$(dirname "$KEY")"
ssh-keygen -q -t ed25519 -N '' -C aigate-orchestrator -f "$KEY" || { echo "ssh-keygen failed" >&2; exit 1; }
chmod 600 "$KEY"
echo "created $KEY (+ .pub). Public key fingerprint:"
ssh-keygen -lf "$KEY.pub"
