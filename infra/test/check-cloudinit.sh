#!/usr/bin/env bash
set -u
D=$(cd "$(dirname "$0")" && pwd); CI=$(cd "$D/../cloud-init" && pwd)
fails=0
ok()  { echo "  ok   - $1"; }
bad() { echo "  FAIL - $1"; fails=$((fails+1)); }
assert() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

parser=none
if python3 -c 'import yaml' 2>/dev/null; then parser=python-yaml
elif ruby -ryaml -e1 2>/dev/null; then parser=ruby-yaml; fi
echo "YAML parser: $parser"
yaml_ok() {
  case $parser in
    python-yaml) python3 -I -c 'import sys,yaml; d=yaml.safe_load(open(sys.argv[1])); assert isinstance(d,dict) and "write_files" in d and "packages" in d' "$1" ;;
    ruby-yaml) ruby -ryaml -e 'd=YAML.safe_load(File.read(ARGV[0])); abort unless d.is_a?(Hash) && d["write_files"] && d["packages"]' "$1" ;;
    *) head -1 "$1" | grep -q '^#cloud-config$' && ! grep -P '\t' "$1" >/dev/null 2>&1 && grep -q '^write_files:' "$1" && grep -q '^packages:' "$1" ;;
  esac
}
role_user() { case $1 in orchestrator) echo orch ;; agent) echo agent ;; test) echo runner ;; esac; }
for role in orchestrator agent test; do
  f="$CI/$role.yaml"
  echo "[$role]"
  assert "file exists" "[ -f '$f' ]"
  [ -f "$f" ] || continue
  assert "first line #cloud-config" "[ \"\$(head -1 '$f')\" = '#cloud-config' ]"
  assert "YAML parses ($parser)" "yaml_ok '$f'"
  assert "ssh_pwauth: false" "grep -q '^ssh_pwauth: false' '$f'"
  assert "disable_root: true" "grep -q '^disable_root: true' '$f'"
  assert "no private key text" "! grep -q 'PRIVATE KEY' '$f'"
  assert "no tailscale key text" "! grep -qiE 'authkey|tskey' '$f'"
  assert "placeholder exactly once" "[ \$(grep -o '@@AIGATE_PUBKEY@@' '$f' | wc -l) -eq 1 ]"
  assert "login user $(role_user $role) defined" "grep -q 'name: $(role_user $role)\$' '$f'"
  assert "no NOPASSWD sudo grant" "! grep -q 'NOPASSWD' '$f'"
  assert "nft default drop on input" "grep -qE 'hook input .*policy drop;' '$f'"
  assert "nft default drop on forward" "grep -qE 'hook forward .*policy drop;' '$f'"
  assert "nftables enabled" "grep -q 'systemctl enable --now nftables' '$f'"
  assert "timezone Asia/Seoul" "grep -q '^timezone: Asia/Seoul' '$f'"
  for p in git ca-certificates nftables curl jq; do assert "package $p" "grep -qE '^  - $p\$' '$f'"; done
  # inbound ssh rules, assert on text
  rules=$(grep -E 'dport 22' "$f" | sed -E 's/^ +//')
  case $role in
    orchestrator)
      expected=$'ip saddr { 192.168.1.124, 192.168.1.128 } tcp dport 22 accept\niifname "tailscale0" tcp dport 22 accept'
      for p in sqlite3 openssh-client; do assert "package $p" "grep -qE '^  - $p\$' '$f'"; done ;;
    agent)
      expected='ip saddr 10.70.0.11 tcp dport 22 accept'
      assert "package nodejs" "grep -qE '^  - nodejs\$' '$f'"; assert "package npm" "grep -qE '^  - npm\$' '$f'" ;;
    test)
      expected='ip saddr 10.70.0.11 tcp dport 22 accept'
      for p in golang-go nodejs npm python3-venv python3-pip build-essential; do assert "package $p" "grep -qE '^  - $p\$' '$f'"; done ;;
  esac
  assert "ssh accept rules are exactly the specified sources" "[ \"\$rules\" = \"\$expected\" ]"
  [ "$rules" = "$expected" ] || { echo "got:"; echo "$rules"; }
  assert "outbound allow-all noted as temporary (Task 2)" "grep -q 'Task 2' '$f'"
done
echo "[nft syntax]"
if command -v nft >/dev/null 2>&1; then
  for role in orchestrator agent test; do
    awk '/^      table inet aigate \{/,/^      \}$/' "$CI/$role.yaml" | sed -E 's/^      //' > "${TMPDIR:-/tmp}/aigate-$role.nft"
    assert "nft -c -f $role" "nft -c -f '${TMPDIR:-/tmp}/aigate-$role.nft'"
  done
else
  echo "  skip - nft not installed locally, syntax check skipped"
fi
[ $fails -eq 0 ] && echo "check-cloudinit: PASS" || { echo "check-cloudinit: $fails FAILED"; exit 1; }
