#!/usr/bin/env bash
# Shared helpers for aigate infra scripts. Source this file; do not execute it.
# OpenStack is reached ONLY through: ssh <host> openstack --os-cloud exporter-internal ...
# Credentials stay on the homeserver (/etc/openstack/clouds.yaml); nothing here reads them.

APPLY=${APPLY:-0}
OS_HOST=${AIGATE_OS_HOST:-openstack-host}
OS_CLOUD=exporter-internal

die() { echo "ERROR: $*" >&2; exit 1; }

# Quote one argument for a POSIX shell (single quotes only when needed). No eval anywhere.
q() {
  case $1 in
    ''|*[!A-Za-z0-9_@%+=:,./-]*) printf "'%s'" "$(printf %s "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf %s "$1" ;;
  esac
}

remote_cmd() {
  local out="openstack --os-cloud $OS_CLOUD" a
  for a in "$@"; do out="$out $(q "$a")"; done
  printf %s "$out"
}

# Planned write verbs (allow-list). Everything else is refused, even with APPLY=1.
_allowed_write() {
  case "${1:-} ${2:-} ${3:-}" in
    "router add subnet"|"floating ip create") return 0 ;;
  esac
  case "${1:-} ${2:-}" in
    "server create"|"volume create"|"flavor create"|"network create"|"subnet create"|"keypair create") return 0 ;;
  esac
  return 1
}

_is_read() {
  case "${2:-}" in list|show) return 0 ;; esac
  [ "${1:-} ${2:-}" = "floating ip" ] && case "${3:-}" in list|show) return 0 ;; esac
  return 1
}

# os: planned write. Prints the command; executes it only when APPLY=1. stdin is /dev/null.
# os_in FILE args...: same, but pipes FILE to the remote command's stdin (public key, user-data).
# stdin is a per-call argument (never a global/env), so it cannot leak into other commands.
os() { _os "" "$@"; }
os_in() { local f=$1; shift; _os "$f" "$@"; }
_os() {
  local stdin_file=$1; shift
  _allowed_write "$@" || die "refused: 'openstack $*' is not in the allow-list"
  local cmd; cmd=$(remote_cmd "$@")
  local redir=""
  [ -n "$stdin_file" ] && redir=" < $stdin_file"
  if [ "$APPLY" = 1 ]; then
    echo "    + ssh $OS_HOST $cmd$redir"
    ssh -o BatchMode=yes "$OS_HOST" "$cmd" < "${stdin_file:-/dev/null}" || die "command failed: openstack $1 $2 ..."
  else
    echo "    \$ ssh $OS_HOST $cmd$redir   # dry-run, not executed"
  fi
}

# os_read: list/show only. Always runs (read-only), in dry-run too.
os_read() {
  _is_read "$@" || die "refused: os_read only allows list/show, got 'openstack $*'"
  ssh -o BatchMode=yes "$OS_HOST" "$(remote_cmd "$@")" < /dev/null
}

# has_name <list-args...> -- <name> : true when <name> is an exact line of the list output
has_name() {
  local name out
  local args=() 
  while [ "$1" != "--" ]; do args+=("$1"); shift; done
  shift; name=$1
  out=$(os_read "${args[@]}") || die "read query failed: openstack ${args[*]}"
  printf '%s\n' "$out" | grep -qxF -- "$name"
}
