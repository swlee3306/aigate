# infra/ — 오케스트레이터 VM 3대 생성 스크립트

홈서버 OpenStack(`ssh openstack-host`)에 VM 3대를 만든다. **기본은 dry-run**이며 `--apply` 전에는 `list`/`show` 조회 외에 아무것도 실행하지 않는다. 조사 결과는 `discovery.md` 참고.

| VM | 사양(flavor) | 루트 볼륨 | 고정 IP | 로그인 사용자 | 외부 접근 |
|---|---|---|---|---|---|
| `orch-vm` | `aigate.orch` 1 vCPU / 2 GB | 10 GB | 10.70.0.11 | `orch` | floating IP 1개 (LAN ssh) |
| `agent-vm` | `aigate.agent` 2 / 4 GB | 20 GB | 10.70.0.12 | `agent` | 없음 |
| `test-vm` | `aigate.test` 4 / 6 GB | 30 GB | 10.70.0.13 | `runner` | 없음 |

네트워크: `aigate-net` / `aigate-subnet` 10.70.0.0/24 (gw 10.70.0.1, DNS 8.8.8.8)를 기존 `router-main`에 연결(SNAT 인터넷). flavor는 disk 0이며 루트 디스크는 Cinder 부팅 볼륨(이미지 `Ubuntu 24.04`).

## GATE (필수 선행 조건)
**agent-vm과 test-vm에서는 Task 2의 egress deny가 적용되고 검증(`tests/verify-firewall`)되기 전까지 Claude/Codex 또는 어떤 신뢰할 수 없는 코드도 실행하지 않는다.** Task 1의 임시 nftables는 아웃바운드를 전부 허용한다(LAN·다른 VM·Tailscale 대역 도달 가능).

## 파일
- `keygen.sh` — 키 **두 개**를 각각 없을 때만 만든다(덮어쓰기 거부, 개인 키 비출력, 지문만 출력). 둘 다 이미 있으면 종료코드 1.
  - 역할 키 `~/.ssh/aigate_ed25519`: 나중에 오케스트레이터가 `orch`/`agent`/`runner`로 로그인할 때 사용. 공개키는 cloud-init의 역할 사용자 authorized_keys(`@@AIGATE_PUBKEY@@`)로 들어간다.
  - 관리자 키 `~/.ssh/aigate_admin_ed25519`: 사람/컨트롤러가 프로비저닝용 `ubuntu`로 접속할 때만 사용. 공개키는 OpenStack 키페어 `aigate-key`로만 등록된다(user-data에는 없음). **관리자 키는 어떤 VM에도 복사하거나 agent 포워딩하지 않는다.**
- `lib.sh` — `os`(쓰기: dry-run은 출력만, `APPLY=1`이면 ssh 실행), `os_read`(list/show 전용), `die`, 따옴표 처리(`eval` 없음). `os`는 허용 목록(server/volume/flavor/network/subnet/keypair create, router add subnet, floating ip create) 밖 동사를 `APPLY=1`이어도 거부.
- `create-vms.sh [--apply] [--only keypair|flavors|network|orchestrator|agent|test|fip]`, `--render <role>`(치환된 user-data 출력, ssh 없음) — 멱등. 이미 있는 이름은 SKIP, 단 서버가 ACTIVE가 아니면(예: ERROR) 중단. 기존 `aigate-key`는 로컬 관리자 공개키와 지문이 다르면 중단.
- `cloud-init/*.yaml`, `test/run.sh`(가짜 `ssh` 사용, 실제 OpenStack 미접촉: `sh infra/test/run.sh`).

OpenStack 관리자 자격 증명은 홈서버 `/etc/openstack/clouds.yaml`(`exporter-internal`)에만 있고 스크립트는 값을 읽거나 복사하지 않는다.

## 사용 흐름
1. `bash infra/keygen.sh` (두 키 생성)
2. `bash infra/create-vms.sh` — 계획 확인(dry-run).
3. 컨트롤러가 승인 후 **한 리소스씩** 적용: `--apply --only keypair` → `flavors` → `network` → `orchestrator` → (접속·호스트 부하 확인) → `agent` → (확인) → `test` → `fip`. `--apply`는 계획을 다시 보여 주고 `apply aigate` 입력을 요구한다.
4. floating IP는 orch-vm의 포트 id를 읽어 `floating ip create --port <id> --floating-ip-address <미사용 주소> external-net`로 **한 번에** 만든다(생성·연결 분리 없음). 풀 192.168.1.200–254에서 실시간 조회, .204/.251/.230/.253 제외. 연결 안 된 FIP가 풀에 있으면 중단하고 삭제 명령을 안내한다.
5. 서버 생성 출력은 `-f value -c id -c status`로 제한(adminPass가 로그에 남지 않음). 볼륨은 `available`까지 대기(최대 `AIGATE_WAIT_TRIES`×`AIGATE_WAIT_SLEEP`초, 기본 60×5).

## 접속 / 복구
- orch-vm ssh 허용 출발지: 192.168.1.121(홈서버), .124(MacBook), .128(Mac mini), `tailscale0`. 홈서버에서의 복구 경로: `ssh openstack-host` → `ssh -i <키> orch@<orch-vm FIP>`(같은 LAN이므로 FIP로 접속). 홈서버 호스트에서 10.70.0.0/24 직접 라우팅은 **확인 못 함**: 서브넷 연결 전 `ip route get 10.70.0.11`은 LAN 게이트웨이로 향함(읽기 전용 조회). agent/test는 orch-vm 경유만 가능하며, orch-vm을 복구하면 나머지도 복구된다(홈서버 출발지는 agent/test에 열지 않음).
- MacBook(.124)은 DHCP이므로 주소가 바뀌면 접속이 막힌다 → 공유기에서 .124 **DHCP 예약**을 권장(또는 Mac mini/홈서버 경유).
- 계정은 모두 비밀번호 잠금이라 콘솔 로그인은 불가. 잠겼을 때: `server rescue`(볼륨 부팅 인스턴스 지원 여부는 Nova 버전에 따라 다름, **미확인**)를 시도하거나, 서버만 삭제(기존 볼륨은 유지됨)하고 `orch-vm-root`를 임시 VM에 붙여 `/etc/nftables.conf`를 고친 뒤 재생성.

## 롤백 (나열만 함, 스크립트는 delete를 실행하지 않음)
홈서버에서 `openstack --os-cloud exporter-internal` 뒤에 붙여 수동 실행. 생성의 역순:
```
floating ip list --port $(openstack --os-cloud exporter-internal port list --server orch-vm -f value -c ID | head -1)   # FIP id/주소 확인
floating ip delete <FIP 주소 또는 ID>                 # 명시적으로 지정 (연결 여부와 무관)
server delete test-vm agent-vm orch-vm
# 볼륨 분리 대기 후 삭제
for v in test-vm-root agent-vm-root orch-vm-root; do
  for i in $(seq 1 60); do s=$(openstack --os-cloud exporter-internal volume show $v -f value -c status); [ "$s" = available ] && break; sleep 5; done
  openstack --os-cloud exporter-internal volume delete $v
done
router remove subnet router-main aigate-subnet
subnet delete aigate-subnet
network delete aigate-net
flavor delete aigate.test aigate.agent aigate.orch
keypair delete aigate-key
```
FIP가 어느 포트에도 안 붙은 채 남았다면 `floating ip list --network external-net --status DOWN`으로 id를 찾아 `floating ip delete <ID>`. 서버가 ERROR로 남았으면 위 `server delete` 후 같은 볼륨으로 재시도. 로컬 키는 직접 판단해 삭제.

## 보안 메모
- 이 OpenStack은 보안그룹이 **적용되지 않는다**(OVS `firewall_driver=noop`). VM 내부 nftables가 유일한 격리 수단이다.
- 임시 nftables: 입력 기본 drop, established/related·loopback·서브넷 DHCP 응답만 추가 허용. orch-vm은 22/tcp를 위 출발지에서만, agent/test는 10.70.0.11에서만. **아웃바운드는 전부 허용**(Task 2에서 조임, 위 GATE 참고). `/etc/nftables.conf`는 `defer: true`로 패키지 설치 이후에 기록된다. 규칙 문법은 홈서버의 `nft -c -f -`(검사 전용)로 확인했다.
- 로그인: 비밀번호 인증 끔, root 로그인 끔. 역할 사용자는 sudo 설정 없음. `ubuntu`(이미지 기본, NOPASSWD sudo)는 관리자 공개키로만 접속된다. agent/test에는 sshd 드롭인 `/etc/ssh/sshd_config.d/10-aigate.conf`(AllowAgentForwarding/AllowTcpForwarding/X11Forwarding no, PermitRootLogin no, PasswordAuthentication no)가 들어간다.
- **Task 2 이후 agent-vm/test-vm의 `ubuntu` 잠금(실행하지 말고 그때 사용; 먼저 관리자 접속 필요한 작업을 모두 끝낼 것):**
  ```
  sudo rm -f /etc/sudoers.d/90-cloud-init-users
  sudo passwd -l ubuntu
  sudo usermod -s /usr/sbin/nologin ubuntu
  sudo rm -f /home/ubuntu/.ssh/authorized_keys
  ```
- Tailscale은 설치하지 않았다. 인증 키는 사용자가 직접 만들어 입력하며 저장소·cloud-init에 넣지 않는다.

## Task 2: firewall and snapshots
- `nftables/{orchestrator,agent,test}.nft`: default drop on all chains; domain allow-lists in `egress-domains.{agent,test}` (dnsmasq `nftset`).
- `apply-firewall.sh [--apply] <role>`: installs rules with a 180 s auto-rollback timer, then re-tests login. Previous rules: `/etc/nftables.conf.pre-task2` on the VM.
- `verify-firewall.sh`: connection attempts from each VM vs expectation; writes `firewall-report.md`; exit 1 on any difference.
- `snapshot.sh create|restore|time|prune <vm>`: Cinder snapshot and restore (new volume from snapshot + new server; see report for why not `volume revert`).
- Rollback of the rules: `sudo cp /etc/nftables.conf.pre-task2 /etc/nftables.conf && sudo nft -f /etc/nftables.conf` (via the admin key through orch-vm).
