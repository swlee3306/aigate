#!/bin/sh
# Runs all infra checks. Usage: sh infra/test/run.sh
d=$(cd "$(dirname "$0")" && pwd)
rc=0
for t in check-create-dryrun.sh check-cloudinit.sh check-keygen.sh check-agent-runner-install.sh check-security-script.sh; do
  echo "=== $t"
  bash "$d/$t" || rc=1
done
[ $rc -eq 0 ] && echo "ALL PASSED" || echo "FAILED"
exit $rc
