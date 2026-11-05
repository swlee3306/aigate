#!/bin/sh
# agent-login.sh claude|codex|status   -- run this in YOUR OWN terminal (needs a real TTY).
# Opens an interactive login for the `agent` user on agent-vm. The admin key stays on this Mac (ProxyCommand via
# orch-vm); the credentials are typed/approved by you and stored only in agent's home on agent-vm (mode 600 by the CLI).
# Nothing is printed by this script except what the CLI itself shows; do not paste tokens anywhere else.
set -eu
K=$HOME/.ssh/aigate_admin_ed25519
[ -f "$K" ] || { echo "admin key $K not found" >&2; exit 1; }
P="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@192.168.1.200"
run() { ssh -i "$K" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$P" "$@"; }
case "${1:-}" in
  claude) [ -t 0 ] || { echo "run this in a real terminal (TTY)" >&2; exit 1; }
          echo "Claude: in the TUI type /login, finish in the browser, then /exit."
          run -t ubuntu@10.70.0.12 'sudo -u agent -H -i claude' ;;
  codex)  [ -t 0 ] || { echo "run this in a real terminal (TTY)" >&2; exit 1; }
          run -t ubuntu@10.70.0.12 'sudo -u agent -H -i codex login --device-auth' ;;
  status) run ubuntu@10.70.0.12 'for d in .claude .codex; do printf "%s: " $d; sudo -u agent test -s /home/agent/$d/.credentials.json -o -s /home/agent/$d/auth.json && echo "credentials file present" || echo "none"; done; sudo -u agent -H -i codex login status 2>&1 | head -2' ;;
  *) echo "usage: agent-login.sh claude|codex|status" >&2; exit 64 ;;
esac
