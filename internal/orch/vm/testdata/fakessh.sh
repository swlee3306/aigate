#!/bin/sh
# Fake ssh for tests. Honours the host-key options like the real ssh, then behaves like sshd with a forced
# command: runs $AGENT_RUNNER locally with SSH_ORIGINAL_COMMAND set to the requested command.
# AIGATE_FAKE_ARGLOG: append the argument list (one per line, then "--call--").
if [ -n "$AIGATE_FAKE_ARGLOG" ]; then
	{ printf '%s\n' "$@"; echo "--call--"; } >> "$AIGATE_FAKE_ARGLOG"
fi
strict=ask kh= host=
while [ $# -gt 0 ]; do
	case $1 in
	-o)
		shift
		case $1 in
		StrictHostKeyChecking=*) strict=${1#*=} ;;
		UserKnownHostsFile=*) kh=${1#*=} ;;
		esac
		;;
	-i | -l | -F) shift ;;
	--)
		shift
		[ -z "$host" ] && { host=$1; shift; }
		break
		;;
	-*) ;;
	*) host=$1; shift; break ;;
	esac
	shift
done
[ "$1" = "--" ] && shift
host=${host#*@}
case $strict in
yes | ask)
	if [ -z "$kh" ] || ! grep -q "^$host " "$kh" 2>/dev/null; then
		echo "Host key verification failed." >&2
		exit 255
	fi
	;;
esac
unset GIT_DIR GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_INDEX_FILE GIT_WORK_TREE GIT_CONFIG_COUNT
SSH_ORIGINAL_COMMAND="$*" exec "$AGENT_RUNNER"
