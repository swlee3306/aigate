#!/usr/bin/env bash
# Create the two dedicated aigate ssh keys (ed25519, no passphrase). Each only if absent; never overwrites.
#   role  key ~/.ssh/aigate_ed25519        : used later by the orchestrator to log in as orch/agent/runner
#   admin key ~/.ssh/aigate_admin_ed25519  : used only by the human/controller to provision as 'ubuntu'
# Exit 0 if at least one key was created, 1 if everything already existed. Private keys are never printed.
set -u
ROLE=${AIGATE_KEY:-$HOME/.ssh/aigate_ed25519}
ADMIN=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
created=0
mk() { # path comment
  if [ -e "$1" ] || [ -e "$1.pub" ]; then
    echo "exists, not overwriting: $1" >&2
    [ -f "$1.pub" ] && ssh-keygen -lf "$1.pub" >&2
    return 0
  fi
  mkdir -p "$(dirname "$1")" && chmod 700 "$(dirname "$1")"
  ssh-keygen -q -t ed25519 -N '' -C "$2" -f "$1" || { echo "ssh-keygen failed" >&2; exit 2; }
  chmod 600 "$1"
  echo "created $1 (+ .pub). Public key fingerprint:"
  ssh-keygen -lf "$1.pub"
  created=$((created+1))
}
mk "$ROLE" aigate-orchestrator
mk "$ADMIN" aigate-admin
[ "$created" -gt 0 ] || { echo "refusing: all keys already exist, nothing created." >&2; exit 1; }
