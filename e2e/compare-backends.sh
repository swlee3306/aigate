#!/bin/sh
# Runs the whole e2e package under the local and the fakessh backend and
# compares the outcome scenario by scenario: PASS/FAIL of every (sub)test and
# the gate verdict each scenario recorded (AIGATE_E2E_RESULTS). Exit 0 only if
# both are identical; otherwise the differing scenario names are listed.
#
#   sh e2e/compare-backends.sh [extra go test args, e.g. -run Go]
#
# Nothing to set up: the fakessh backend wires itself to
# internal/remote/testdata/fakessh.sh and a per-test temp sandbox.
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root" || exit 2
out=$(mktemp -d "${TMPDIR:-/tmp}/aigate-compare.XXXXXX") || exit 2
trap 'rm -rf "$out"' EXIT
command -v jq >/dev/null || { echo "compare-backends: jq is required" >&2; exit 2; }

for b in local fakessh; do
	echo "== backend $b" >&2
	AIGATE_E2E_BACKEND=$b AIGATE_E2E_RESULTS="$out/$b.json" AIGATE_FAKE_MODE= AIGATE_FAKE_ARGLOG= \
		go test ./e2e -count=1 -v "$@" >"$out/$b.log" 2>&1
	echo "$?" >"$out/$b.rc"
	# "--- PASS: Test/name (1.23s)" -> "PASS Test/name"
	sed -n -E 's/^ *--- (PASS|FAIL|SKIP): (.*) \([0-9.]+s\)$/\1 \2/p' "$out/$b.log" | sort >"$out/$b.status"
	if [ -s "$out/$b.json" ]; then
		jq -r '.[] | [.stack, .name, .got] | @tsv' "$out/$b.json" | sort >"$out/$b.verdict"
	else
		: >"$out/$b.verdict"
	fi
	echo "   go test exit $(cat "$out/$b.rc"): $(grep -c '^PASS ' "$out/$b.status") pass, $(grep -c '^FAIL ' "$out/$b.status") fail, $(grep -c '^SKIP ' "$out/$b.status") skip; $(wc -l <"$out/$b.verdict" | tr -d ' ') recorded verdicts" >&2
done

rc=0
if ! diff "$out/local.status" "$out/fakessh.status" >"$out/status.diff"; then
	rc=1
	echo "PASS/FAIL differs between local (<) and fakessh (>):"
	cat "$out/status.diff"
	echo "differing scenarios:"
	grep '^[<>]' "$out/status.diff" | sed -E 's/^[<>] (PASS|FAIL|SKIP) //' | sort -u
fi
if ! diff "$out/local.verdict" "$out/fakessh.verdict" >"$out/verdict.diff"; then
	rc=1
	echo "verdict differs between local (<) and fakessh (>) [stack, scenario, verdict]:"
	cat "$out/verdict.diff"
	echo "differing scenarios:"
	grep '^[<>]' "$out/verdict.diff" | cut -f2 | sort -u
fi
if [ "$rc" -ne 0 ]; then
	# keep the logs for inspection
	keep="${TMPDIR:-/tmp}/aigate-compare-logs"
	rm -rf "$keep"; mkdir -p "$keep"; cp "$out"/*.log "$keep"/
	echo "full logs: $keep" >&2
	exit 1
fi
if [ "$(cat "$out/local.rc")" != 0 ] || [ "$(cat "$out/fakessh.rc")" != 0 ]; then
	echo "backends agree, but go test failed (local exit $(cat "$out/local.rc"), fakessh exit $(cat "$out/fakessh.rc"))"
	exit 1
fi
echo "backends agree: $(wc -l <"$out/local.status" | tr -d ' ') (sub)tests, $(wc -l <"$out/local.verdict" | tr -d ' ') verdicts identical"
