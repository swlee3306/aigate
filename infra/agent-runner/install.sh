#!/bin/sh
# install.sh [--apply] [--verify-key <orchestrator private key file>] <orchestrator public key file>
# Installs the agent-runner forced-command setup on agent-vm (admin key via orch-vm ProxyCommand, like
# apply-firewall.sh; the admin key stays on this machine). Default is a dry-run that only prints the plan.
#   1. /usr/local/sbin/agent-runner            root:root 0755  (the wrapper, infra/agent-runner/agent-runner)
#   2. /etc/agent-runner.conf                  root:root 0644  (only if absent; existing one is kept)
#   3. /srv/aigate/work                        agent:agent 0700
#   4. /etc/ssh/aigate/agent_authorized_keys   root:root 0644  one line: command=...,restrictions <public key>
#   5. /etc/ssh/sshd_config.d/aigate-agent.conf Match User agent -> that authorized_keys file (sshd -t before reload)
# The public key file must be the orchestrator's NEW dedicated key (public half only). This script never
# generates, copies or prints a private key, and refuses the admin key. --verify-key only passes the path to
# `ssh -i` to check that a non-whitelisted command is rejected; the key is not read otherwise.
# Backups: replaced files are copied to <file>.pre-agent-runner (once) before they change.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
APPLY=0; VERIFY_KEY=
while [ $# -gt 0 ]; do
  case $1 in
    --apply) APPLY=1; shift ;;
    --verify-key) [ $# -ge 2 ] || { echo "--verify-key needs a file" >&2; exit 64; }; VERIFY_KEY=$2; shift 2 ;;
    -*) echo "usage: install.sh [--apply] [--verify-key <private key file>] <public key file>" >&2; exit 64 ;;
    *) break ;;
  esac
done
PUB=${1:?usage: install.sh [--apply] [--verify-key <private key file>] <public key file>}
[ $# -eq 1 ] || { echo "exactly one public key file expected" >&2; exit 64; }
ADMIN_PUB=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}.pub
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
IP=10.70.0.12; JUMP=192.168.1.200

# ---- validate the public key (one ssh-ed25519 line, not a private key, not the admin key) ----
[ -f "$PUB" ] || { echo "public key file $PUB not found" >&2; exit 1; }
grep -q 'PRIVATE KEY' "$PUB" && { echo "$PUB looks like a PRIVATE key; pass the .pub file" >&2; exit 1; }
[ "$(grep -c . "$PUB")" -eq 1 ] || { echo "$PUB must hold exactly one key line" >&2; exit 1; }
set -- $(cat "$PUB")
type=${1:-}; blob=${2:-}
[ "$type" = ssh-ed25519 ] || { echo "only ssh-ed25519 keys are accepted (got '$type')" >&2; exit 1; }
case $blob in ''|*[!A-Za-z0-9+/=]*) echo "key data is not base64" >&2; exit 1 ;; esac
fp=$(ssh-keygen -lf "$PUB" 2>/dev/null | awk '{print $2}') || true
[ -n "$fp" ] || { echo "ssh-keygen cannot read $PUB" >&2; exit 1; }
if [ -f "$ADMIN_PUB" ] && [ "$(ssh-keygen -lf "$ADMIN_PUB" | awk '{print $2}')" = "$fp" ]; then
  echo "refusing: that is the admin key; create a dedicated orchestrator key" >&2; exit 1
fi
LINE="command=\"/usr/local/sbin/agent-runner\",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty $type $blob aigate-orchestrator"
if [ -n "$VERIFY_KEY" ]; then
  [ -f "$VERIFY_KEY" ] || { echo "verify key $VERIFY_KEY not found" >&2; exit 1; }
  [ "$(ssh-keygen -yf "$VERIFY_KEY" 2>/dev/null | awk '{print $2}')" = "$blob" ] || { echo "--verify-key does not match the public key" >&2; exit 1; }
fi
for f in agent-runner agent-runner.conf.example sshd-agent.conf; do [ -f "$here/$f" ] || { echo "missing $here/$f" >&2; exit 1; }; done

echo "== agent-vm ($IP): install the forced-command agent runner"
echo "   public key fingerprint: $fp"
echo "   plan:"
echo "     1. install $here/agent-runner -> /usr/local/sbin/agent-runner (root:root 0755)"
echo "     2. install $here/agent-runner.conf.example -> /etc/agent-runner.conf (root:root 0644, only if absent)"
echo "     3. create /srv/aigate/work (agent:agent 0700)"
echo "     4. write /etc/ssh/aigate/agent_authorized_keys (root:root 0644) with:"
echo "        $LINE"
echo "     5. install $here/sshd-agent.conf -> /etc/ssh/sshd_config.d/aigate-agent.conf, run sshd -t, reload ssh"
echo "        (user agent then only accepts keys from the root-owned file; the old ~/.ssh/authorized_keys of agent is ignored)"
echo "     6. verify: list installed paths, sshd -T for user agent${VERIFY_KEY:+, forced-key test of rejected commands}"
[ "$APPLY" = 1 ] || { echo "dry-run: nothing changed. Re-run with --apply."; exit 0; }

[ -f "$K" ] || { echo "admin key $K not found" >&2; exit 1; }
J="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@$JUMP"
# inner VM host keys change when a VM is re-created, same as the other infra scripts
vm() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$J" "ubuntu@$IP" "$@"; }

echo "== 1/6 wrapper"
vm 'f=/usr/local/sbin/agent-runner; [ ! -e $f ] || sudo cp -n $f $f.pre-agent-runner; sudo tee $f.new >/dev/null && sudo chown root:root $f.new && sudo chmod 0755 $f.new && sudo mv $f.new $f' < "$here/agent-runner"
echo "== 2/6 config"
vm 'test -e /etc/agent-runner.conf && echo "kept existing /etc/agent-runner.conf" || { sudo tee /etc/agent-runner.conf >/dev/null && sudo chown root:root /etc/agent-runner.conf && sudo chmod 0644 /etc/agent-runner.conf && echo "wrote /etc/agent-runner.conf"; }' < "$here/agent-runner.conf.example"
echo "== 3/6 workspace root"
vm 'sudo install -d -o agent -g agent -m 0700 /srv/aigate/work'
echo "== 4/6 authorized_keys (root-owned)"
vm 'sudo install -d -o root -g root -m 0755 /etc/ssh/aigate; f=/etc/ssh/aigate/agent_authorized_keys; [ ! -e $f ] || sudo cp -n $f $f.pre-agent-runner; sudo tee $f >/dev/null && sudo chown root:root $f && sudo chmod 0644 $f' <<EOF2
$LINE
EOF2
echo "== 5/6 sshd drop-in (checked before reload)"
vm 'f=/etc/ssh/sshd_config.d/aigate-agent.conf; sudo install -d -m 0755 /etc/ssh/sshd_config.d; [ ! -e $f ] || sudo cp $f /tmp/aigate-agent.conf.prev; sudo tee $f >/dev/null' < "$here/sshd-agent.conf"
if vm 'sudo sshd -t'; then
  vm 'sudo systemctl reload ssh'
else
  echo "sshd -t FAILED: removing the drop-in, nothing reloaded" >&2
  vm 'f=/etc/ssh/sshd_config.d/aigate-agent.conf; if [ -e /tmp/aigate-agent.conf.prev ]; then sudo mv /tmp/aigate-agent.conf.prev $f; else sudo rm -f $f; fi'
  exit 1
fi
echo "== 6/6 verify"
vm 'ls -l /usr/local/sbin/agent-runner /etc/agent-runner.conf /etc/ssh/aigate/agent_authorized_keys /etc/ssh/sshd_config.d/aigate-agent.conf; ls -ld /srv/aigate/work; sudo sshd -T -C user=agent,host=localhost,addr=127.0.0.1 | grep -E "^(authorizedkeysfile|permittty|allowtcpforwarding|passwordauthentication) "'
if [ -n "$VERIFY_KEY" ]; then
  fvm() { ssh -i "$VERIFY_KEY" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$J" "agent@$IP" "$@"; }
  bad=0
  for c in 'id' 'cat /etc/passwd' 'prepare abc' 'run 1 evil' "upload-pack '/etc'"; do
    if out=$(fvm "$c" </dev/null 2>&1); then echo "FAIL: '$c' was accepted: $out" >&2; bad=1; else echo "ok: '$c' rejected"; fi
  done
  [ $bad = 0 ] || { echo "forced-command check FAILED" >&2; exit 1; }
else
  echo "(no --verify-key: skipped the forced-key rejection test)"
fi
echo "done. Next: README.md step 5 (agent-login.sh status) and step 6 (small test task)."
