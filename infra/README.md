# infra/ — 오케스트레이터 VM 3대 생성 스크립트

홈서버 OpenStack(`ssh openstack-host`)에 VM 3대를 만든다. **기본은 dry-run**이며 `--apply`를 주기 전에는 `list`/`show` 조회 외에 아무것도 실행하지 않는다. 조사 결과는 `discovery.md` 참고.

| VM | 사양(flavor) | 루트 볼륨 | 고정 IP | 로그인 사용자 | 외부 접근 |
|---|---|---|---|---|---|
| `orch-vm` | `aigate.orch` 1 vCPU / 2 GB | 10 GB | 10.70.0.11 | `orch` | floating IP 1개 (LAN ssh) |
| `agent-vm` | `aigate.agent` 2 / 4 GB | 20 GB | 10.70.0.12 | `agent` | 없음 |
| `test-vm` | `aigate.test` 4 / 6 GB | 30 GB | 10.70.0.13 | `runner` | 없음 |

네트워크: `aigate-net` / `aigate-subnet` 10.70.0.0/24 (gw 10.70.0.1, DNS 8.8.8.8)를 기존 `router-main`에 연결(SNAT 인터넷). flavor는 disk 0이며 루트 디스크는 Cinder 부팅 볼륨(이미지 `Ubuntu 24.04`)이다.

## 파일
- `keygen.sh` — `~/.ssh/aigate_ed25519`(ed25519, 패스프레이즈 없음)를 **없을 때만** 만든다. 덮어쓰기 거부(종료코드 1), 비공개 키는 출력하지 않고 공개키 지문만 출력.
- `lib.sh` — `os`(쓰기: dry-run이면 명령만 출력, `APPLY=1`이면 ssh로 실행), `os_read`(list/show 전용, 항상 실행), `die`, 인자 따옴표 처리(`eval` 없음). `os`는 허용 목록(server/volume/flavor/network/subnet/keypair create, router add subnet, floating ip create, server add floating ip) 밖의 동사를 `APPLY=1`이어도 거부한다.
- `create-vms.sh [--apply] [--only keypair|flavors|network|orchestrator|agent|test|fip]` — 멱등(이미 있는 이름은 `SKIP`), 번호 붙은 계획과 정확한 명령을 출력.
- `cloud-init/{orchestrator,agent,test}.yaml` — 사용자, ssh 공개키(`@@AIGATE_PUBKEY@@`를 실행 시 `~/.ssh/aigate_ed25519.pub`로 치환), 패키지, 최소 nftables, 작업 디렉터리.
- `test/run.sh` — 로컬 점검(가짜 `ssh` 사용, 실제 OpenStack 미접촉). `sh infra/test/run.sh`

OpenStack 관리자 자격 증명은 홈서버 `/etc/openstack/clouds.yaml`(`exporter-internal`)에만 있고, 스크립트는 값을 읽거나 복사하지 않는다.

## 사용 흐름
1. `bash infra/keygen.sh` (최초 1회)
2. `bash infra/create-vms.sh` — 계획 확인(dry-run).
3. 컨트롤러가 승인 후 **한 리소스씩** 적용(권장 순서):
   `--apply --only keypair` → `flavors` → `network` → `orchestrator` → (접속·호스트 부하 확인) → `agent` → (확인) → `test` → `fip`
   (`--only fip`는 orch-vm이 이미 있어야 한다.) `--apply`는 계획을 다시 보여 주고 `apply aigate`를 정확히 입력해야 진행한다.
4. 각 VM마다 ssh 접속과 홈서버 CPU/메모리 변화를 확인하고, 이상이 있으면 중단·롤백.
5. 전체를 한 번에: `create-vms.sh --apply` (keypair → flavors → network → 볼륨 3개 → 서버 3개 → floating IP).

floating IP는 `external-net` 풀(192.168.1.200–254)에서 실시간 조회로 **미사용 주소**를 고른다(.204, .251, .230, .253은 항상 제외).

## 롤백 (나열만 함, 스크립트가 실행하지 않음 — 허용 목록에 delete 없음)
홈서버에서 `openstack --os-cloud exporter-internal` 뒤에 붙여 수동 실행. 생성의 역순:
```
server remove floating ip orch-vm <FIP>
floating ip delete <FIP>
server delete test-vm agent-vm orch-vm
volume delete test-vm-root agent-vm-root orch-vm-root
router remove subnet router-main aigate-subnet
subnet delete aigate-subnet
network delete aigate-net
flavor delete aigate.test aigate.agent aigate.orch
keypair delete aigate-key
```
`<FIP>`는 `floating ip list --fixed-ip-address 10.70.0.11`로 확인. 로컬 키(`~/.ssh/aigate_ed25519*`)는 직접 판단해 삭제.

## 보안 메모
- 이 OpenStack은 보안그룹이 **적용되지 않는다**(OVS `firewall_driver=noop`). VM 내부 nftables가 유일한 격리 수단이다.
- cloud-init의 임시 nftables: 입력 기본 drop. orch-vm은 22/tcp를 192.168.1.124(MacBook), 192.168.1.128(Mac mini), `tailscale0`에서만 허용. agent-vm·test-vm은 22/tcp를 10.70.0.11(orch-vm)에서만 허용. established/related, loopback, 서브넷 DHCP 응답만 추가 허용. **아웃바운드는 아직 전부 허용**이며 Task 2에서 조인다.
- 로그인: 모든 VM에서 비밀번호 인증 끄기, root 로그인 끄기. 역할 사용자(`orch`/`agent`/`runner`)는 sudo 없음. 관리용 sudo는 이미지 기본 사용자 `ubuntu`(키 전용)만 가진다 — 초기 설정 후 필요 없으면 막을지 사용자가 결정.
- Tailscale은 설치하지 않았다. 인증 키는 사용자가 직접 만들어 VM에 입력하며 저장소·cloud-init에 넣지 않는다.
- agent-vm/test-vm은 orch-vm에서만 ssh 가능하다. orch-vm에서 이들로 접속할 키 배치는 이후 작업에서 정한다(개인 키를 VM에 복사하지 않도록 주의).
