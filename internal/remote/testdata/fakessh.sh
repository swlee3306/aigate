#!/bin/sh
# Fake ssh for tests: honours the host-key options the real ssh would, then
# runs the remote command locally, joined with spaces like ssh does.
# AIGATE_FAKE_ARGLOG: append the argument list (one per line, then "--call--").
# AIGATE_FAKE_MODE makes the pretend VM misbehave:
#   unreachable  exit 255 (connection failure)
#   flood        endless output instead of the real reply
#   rc_lie       the reply says rc=0 whatever the command returned
#   cutframe     the reply is cut off after 60 bytes
#   bigframe     the reply declares a 9 MiB result
#   lenlie       the reply declares a 4 byte result and sends 9 MiB
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
		break
		;;
	-*) ;;
	*) host=$1 ;;
	esac
	shift
done
[ "$AIGATE_FAKE_MODE" = unreachable ] && { echo "ssh: connect to host $host: Connection refused" >&2; exit 255; }
case $strict in
yes | ask)
	if [ -z "$kh" ] || ! grep -q "^$host " "$kh" 2>/dev/null; then
		echo "Host key verification failed." >&2
		exit 255
	fi
	;;
esac
cmd="$*"
case $AIGATE_FAKE_MODE in
flood) exec yes AIGATE1 ;;
rc_lie) sh -c "$cmd" | awk '!d && /^rc=/ {print "rc=0"; d=1; next} {print}' ;;
cutframe) sh -c "$cmd" | head -c 60 ;;
bigframe)
	printf 'AIGATE1\nrc=0\ntimedout=0\nsetupfailed=0\ntail=0\n\nresult=9437185\n'
	head -c 9437185 /dev/zero
	printf '\nEND\n'
	;;
lenlie)
	printf 'AIGATE1\nrc=0\ntimedout=0\nsetupfailed=0\ntail=0\n\nresult=4\n'
	head -c 9437185 /dev/zero
	printf '\nEND\n'
	;;
*) exec sh -c "$cmd" ;;
esac
