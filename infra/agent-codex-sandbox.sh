#!/bin/sh
# agent-codex-sandbox.sh [--apply|--rollback|--check]    (default: dry-run, prints the plan)
# Lets Codex's own bwrap sandbox start on agent-vm. Ubuntu 24.04 blocks unprivileged user namespaces
# (kernel.apparmor_restrict_unprivileged_userns=1), so `codex exec --sandbox workspace-write` dies with
# "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted".
# NARROW fix (default): an AppArmor profile that grants `userns` ONLY to the bwrap binary vendored inside the
# Codex npm package. The global sysctl stays 1, so no other program of user `agent` gets user namespaces.
# The path contains the package layout; after a Codex upgrade that moves the binary, re-run with --apply.
# The admin key stays on this Mac (ProxyCommand via orch-vm). Nothing else on agent-vm is changed.
set -eu
K=${AIGATE_ADMIN_KEY:-$HOME/.ssh/aigate_admin_ed25519}
BWRAP=/usr/local/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/codex-resources/bwrap
PROF=/etc/apparmor.d/aigate-codex-bwrap
P="ssh -i $K -o IdentitiesOnly=yes -o BatchMode=yes -W %h:%p ubuntu@192.168.1.200"
vm() { ssh -i "$K" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ProxyCommand="$P" ubuntu@10.70.0.12 "$@"; }
profile() { cat <<P
abi <abi/4.0>,
include <tunables/global>
# aigate: allow user namespaces for Codex's vendored bwrap only (agent-vm)
profile aigate-codex-bwrap $BWRAP flags=(unconfined) {
  userns,
}
P
}
case "${1:-}" in
  "") echo "dry-run: would install $PROF with:"; profile | sed 's/^/    /'
      echo "    then: apparmor_parser -r $PROF; verify as user agent: bwrap --unshare-net --ro-bind / / true"
      echo "rollback: sh $0 --rollback   (removes the profile and reloads AppArmor)"; echo "re-run with --apply to change agent-vm" ;;
  --apply) vm "test -x $BWRAP" || { echo "bwrap not found at $BWRAP" >&2; exit 1; }
      vm "sudo cp -n $PROF $PROF.bak 2>/dev/null || true"
      profile | vm "sudo tee $PROF >/dev/null && sudo apparmor_parser -r $PROF && echo profile-loaded"
      sh "$0" --check ;;
  --rollback) vm "sudo apparmor_parser -R $PROF 2>/dev/null; sudo rm -f $PROF; echo profile-removed" ;;
  --check) echo "global sysctl (must stay 1): $(vm 'sysctl -n kernel.apparmor_restrict_unprivileged_userns')"
      vm "sudo -u agent -H $BWRAP --unshare-net --ro-bind / / true && echo 'bwrap sandbox starts for agent' || echo 'bwrap still FAILS for agent'"
      vm "sudo -u agent -H bash -c 'unshare -Urn true 2>/dev/null && echo \"WARNING: plain unshare works for agent (profile too broad)\" || echo \"plain unshare still blocked for agent (good)\"'" ;;
  *) echo "usage: $0 [--apply|--rollback|--check]" >&2; exit 64 ;;
esac
