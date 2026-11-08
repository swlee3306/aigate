#!/bin/sh
# orch-web-serve.sh PORT [--apply] [--off]
# Publishes the orchd web console (loopback http://127.0.0.1:PORT) on the tailnet through `tailscale serve` (HTTPS 443).
# Default is a dry run that only prints the plan. Run it on orch-vm. Never uses Funnel (public internet exposure).
set -eu

usage() { echo "usage: $0 PORT [--apply] [--off]   (PORT 1024-65535; default is a dry run)" >&2; exit 2; }
die() { echo "orch-web-serve: $*" >&2; exit 1; }

PORT=""; APPLY=0; OFF=0
for a in "$@"; do
  case "$a" in
    --apply) APPLY=1 ;;
    --off) OFF=1 ;;
    -h|--help) usage ;;
    -*) echo "unknown option: $a" >&2; usage ;;
    *) [ -z "$PORT" ] || usage; PORT=$a ;;
  esac
done
[ -n "$PORT" ] || usage
case "$PORT" in *[!0-9]*|'') die "PORT must be numeric: $PORT" ;; esac
[ "${#PORT}" -le 5 ] || die "PORT out of range 1024-65535: $PORT"
{ [ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ]; } || die "PORT out of range 1024-65535: $PORT"

if [ "$OFF" = 1 ]; then
  echo "plan: tailscale serve --https=443 off"
  if [ "$APPLY" = 0 ]; then echo "(dry run: add --apply to run it)"; exit 0; fi
  command -v tailscale >/dev/null 2>&1 || die "tailscale not found"
  tailscale serve --https=443 off
  tailscale serve status || true
  exit 0
fi

echo "plan: tailscale serve --bg --https=443 http://127.0.0.1:$PORT"
echo "expected orchd flags (the node's tailnet FQDN as typed in the browser):"
echo "  --http-listen 127.0.0.1:$PORT --allowed-host <this-node>.<tailnet>.ts.net --origin https://<this-node>.<tailnet>.ts.net --rp-id <this-node>.<tailnet>.ts.net"
echo "never: Funnel. This script does not use it and --apply refuses to continue if serve status shows it on."
if [ "$APPLY" = 0 ]; then echo "(dry run: add --apply to run it)"; exit 0; fi

command -v tailscale >/dev/null 2>&1 || die "tailscale not found"
# Funnel check on the current state, before and after (serve status prints 'Funnel on' for public exposure)
if tailscale serve status 2>&1 | grep -qi 'funnel on'; then die "Funnel is on in 'tailscale serve status'; turn Funnel off first (a user decision). Refusing."; fi
tailscale serve --bg --https=443 "http://127.0.0.1:$PORT"
st=$(tailscale serve status 2>&1 || true)
echo "$st"
if echo "$st" | grep -qi 'funnel on'; then die "Funnel appears on after serve; run: tailscale serve --https=443 off"; fi
