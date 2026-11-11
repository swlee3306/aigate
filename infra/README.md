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

## 웹 콘솔 배포 (W1)
orchd의 HTTP 리스너(루프백)를 `tailscale serve`로 tailnet에 HTTPS(443)로 노출한다. 스크립트·유닛의 값은 **예시**이며 실제 값(FQDN, 포트)은 E2에서 확정한다. 아래 절차는 아직 실제 서버에 적용·검증하지 않았다(**미검증**).

파일: `orch-web-serve.sh PORT [--apply] [--off]`(기본 드라이런, 계획만 출력), `signers-init.sh [--apply]`(기본 드라이런, `--apply`는 root만), `test-web-scripts.sh`(로컬 검사, 서버 미접촉), `nftables/orchestrator.nft`(tailscale0의 443 허용 추가. 필수 여부는 **미확인**: `tailscale serve`가 443을 tailscaled 내부 스택에서 받으면 커널 input 훅에 닿지 않을 수 있다. 규칙은 무해하며 커널 경로에 대한 방어로 둔다), `systemd/orchd-orchvm.service`(새 플래그 예시).

### 반드시 지킬 것
- **Tailscale Funnel은 이 서비스에 절대 켜지 않는다.** Funnel은 공인 인터넷에 노출한다. 스크립트는 Funnel 명령을 쓰지 않고, `--apply`는 `tailscale serve status`에 Funnel이 켜져 있으면 중단한다.
- **orch-vm에서 신뢰할 수 없는 로컬 프로세스를 실행하지 않는다.** 리스너는 루프백 전용이라, 로컬 프로세스는 `Tailscale-User-Login` 같은 헤더를 위조해 횟수 제한 키를 마음대로 바꿀 수 있다(전역 천장은 남지만 로컬 신뢰 경계가 깨진다). 신뢰할 수 없는 코드는 agent-vm/test-vm에서만 실행한다.
- **`--allowed-host`는 브라우저 주소창에 보이는 tailnet FQDN**(`<노드>.<tailnet>.ts.net`)이어야 하고 `--origin`(`https://<FQDN>`, 포트 없음)·`--rp-id`(FQDN)와 일치해야 한다. 설계상 `tailscale serve`가 Host를 그대로 넘기고 클라이언트가 보낸 `Tailscale-User-*` 헤더를 지운다고 가정하지만, **실제 동작은 미확인**이다(X1에서 확인). Host가 다르게 오면 요청이 전부 421이 된다.
- **`--socket-uid`**: 소켓에 접근할 추가 uid(반복 가능). orchd 자신의 uid는 항상 포함된다(대체가 아니라 추가). `sudo orch`로 운영하려면 `0`을 추가한다(유닛 예시는 `--socket-uid 0`). `orch admin signers pending`/`add --pending`의 기본 `--pending-file`과 orchd의 기본값은 모두 `/home/orch/orchd/state/signers.pending.json`이고 유닛에도 같은 값을 명시했다(`ProtectSystem=strict`에서 쓰기 가능한 경로). 파일이 없으면 CLI가 한국어 안내를 stderr에 출력한다.

### 적용 순서 (사용자 본인의 작업·승인 필요 표시)
1. **[사용자 승인]** orch-vm에서 `sudo sh infra/signers-init.sh`(계획 확인) → `sudo sh infra/signers-init.sh --apply`. `/etc/aigate`를 `root:root 0755`로 만들 뿐 `signers.json`은 **만들지 않는다**(만들면 서명 필수 모드가 켜진다).
2. **[사용자 승인]** 새 플래그를 넣은 유닛 배포(FQDN 등 예시 값을 실제 값으로 바꾼 뒤): `sudo cp infra/systemd/orchd-orchvm.service /etc/systemd/system/orchd.service && sudo systemctl daemon-reload && sudo systemctl restart orchd`. `signers.json`이 없으므로 **레거시 모드로 계속 동작**한다. nft 갱신(443)은 `apply-firewall.sh`의 기존 절차(180초 자동 롤백)로 적용한다.
3. **[사용자 승인]** `sh infra/orch-web-serve.sh 8808`(계획) → `sh infra/orch-web-serve.sh 8808 --apply`. 끝에 `tailscale serve status`를 출력한다.
4. **[사용자 직접]** Tailscale 관리 콘솔에서 이 tailnet의 HTTPS 인증서를 켠다(MagicDNS와 HTTPS Certificates). 스크립트가 대신하지 않는다.
5. **[사용자 직접, 컷오버]** 첫 키 등록(패스키 등록 대기 항목은 `/home/orch/orchd/state/signers.pending.json`에 있고 `sudo orch admin signers pending`으로 본다): `sudo orch admin signers add --id <ID> --key ~/.ssh/id_ed25519.pub`. 이 명령이 `/etc/aigate/signers.json`을 만들며 **그 순간부터 서명된 승인만 유효한 모드**가 된다.
6. 확인: `sudo orch admin signers list`, 시작 로그(`HTTP listener on ...`, `socket uids allowed`, `orchd: config: approvals: mode legacy|signed`, 시작 때 한 번만 찍힘), 브라우저에서 `https://<FQDN>/` 접속, `tailscale serve status`에 Funnel이 없는지.

### 롤백
- 서명 필수 모드 해제: `sudo rm /etc/aigate/signers.json`(레거시 모드로 복귀. 레지스트리는 승인 검사(`Store.Valid`)마다 읽으므로 코드상 다음 검사부터 적용되고 재시작은 선택이다. 같은 이유로 손상되었거나 신뢰할 수 없는(소유자·권한 이상) `signers.json`은 즉시 fail-closed가 된다. 둘 다 코드 읽기 기준이며 실서버에서는 미확인).
- 웹 노출 끄기: `sh infra/orch-web-serve.sh 8808 --off --apply`(`tailscale serve --https=443 off`).
- 유닛: 이전 유닛은 git 이력에 있다(`git show c444233:infra/systemd/orchd-orchvm.service`). `.bak` 파일은 만들지 않는다.

### 검증
`sh infra/test-web-scripts.sh`(드라이런 출력, 잘못된 포트·비root `--apply` 거부, funnel 명령 부재). `nft -c -f`는 nft가 있는 호스트(홈서버 등)에서 따로 실행해야 한다.

## 브로커 배포 (W2b)
파일: `broker-init.sh [--apply]`(기본 드라이런. `--apply`는 root만. 계정 `orch-broker`, `/etc/orch-broker`(root:orch-broker 0750), `/var/lib/orch-broker`(0700)를 만들 뿐 **키·토큰 파일은 만들지 않는다**. 그룹 `orch`가 있는지는 **확인하지 않는다**), `systemd/orch-broker.service`(하드닝 유닛. `SupplementaryGroups=orch`는 소켓 그룹을 `orch`로 바꾸기 위한 보안상 타협이다), `test-broker-scripts.sh`(로컬 검사, 서버 미접촉). 설정(`/etc/orch-broker/broker.json`)과 키·토큰은 사용자가 직접 둔다. 점검: `orch-broker -check`(설정·신뢰·키 모드·소켓 그룹·상태 폴더만, 외부 접속 없음).
방화벽은 바꾸지 않는다: `nftables/orchestrator.nft`는 공인 주소로의 80·443(OpenAI)과 `100.64.0.10`의 22·18082(GitLab)를 이미 허용한다. 실서버 규칙이 같은지는 `curl -sS -o /dev/null -w '%{http_code}' https://api.openai.com/v1/models`가 401이면 확인된 것이다.
적용 순서·검증·롤백은 `docs/10-OPERATIONS-orchestrator.md` 11장(**아직 실서버에서 실행하지 않았다**).

## 프로젝트 등록부 (W3a)
코드 변경만 있고 새 스크립트·유닛은 없다. `orch-broker`가 `/var/lib/orch-broker`에 **`registry.json`(프로젝트 등록부, 0600)과 `used-requests.json`(쓴 서명 요청 ID, 0600)**을 쓴다(폴더는 `orch-broker` 소유 **0700**을 유지해야 한다: 어긋나면 등록부에 의존하는 모든 op가 `registry_unavailable`로 꺼진다). `broker.json`에 선택 키 **`repo_roots`**(예 `["/home/orch/repos"]`, orchd 정책의 `repo_roots`와 철자가 같아야 함)가 생겼고, 등록부가 처음 만들어질 때(파일이 없을 때 broker 시작) 옛 `context_repos`·`gitlab.projects`가 한 번 옮겨진다. 그 뒤 옛 항목은 무시되며 `orch-broker -check`가 경고만 한다. 등록·AI 전송 변경은 `/etc/aigate/signers.json`의 서명자(웹 패스키 또는 ssh-ed25519, 네임스페이스 `aigate-registry`)가 서명해야 한다. broker는 등록 경로의 파일 시스템을 보지 않고(문법만 검사: 실서버에서 `/home/orch/repos`는 `orch:orch 0700`이라 broker 계정이 읽지 못한다) 저장소 검증은 orchd가 한다. 유닛은 바꾸지 않는다(`ProtectHome=yes` 유지).
적용 순서·검증·롤백은 `docs/10-OPERATIONS-orchestrator.md` 12장(**아직 실서버에서 실행하지 않았다**).

## 토큰 저장소 (W3b)
코드 변경만 있고 새 스크립트·유닛·설정 키는 없다. `orch-broker`가 `/var/lib/orch-broker`에 **`secrets/`(토큰 값, 폴더 0700, 파일 `<이름>` 0400, `orch-broker` 소유)와 `tokens.json`(메타데이터, 0600, 값 없음)**을 쓴다. `secrets/`는 broker가 시작할 때 만들고(유닛의 `ReadWritePaths=/var/lib/orch-broker` 안, `UMask=0077`), 상태 폴더 자체가 `orch-broker` 소유 **0700**이어야 한다(어긋나면 토큰 저장소 전체가 꺼진다). `-check`는 읽기 전용이라 아무것도 만들지 않고, `secrets/`·`tokens.json`의 소유자·모드·문법만 검사한다(깨졌거나 모드가 틀리면 거부, 개별 비밀 파일 문제는 경고). 웹에서 OpenAI 키를 교체하면 `secrets/openai`가 설정 파일의 키(`/etc/orch-broker/openai.key`, 그대로 남음)보다 우선하고, 지우면 설정 키로 돌아간다. 롤백용 백업은 `orch`가 쓸 수 없는 `/root/backup-w3b`(0700)에 둔다. `GET /api/v1/settings/deploy-key`는 `--git-ssh-key` 경로 + `.pub`만 읽는다(개인 키는 열지 않음).
적용 순서·검증·롤백은 `docs/10-OPERATIONS-orchestrator.md` 13장(**아직 실서버에서 실행하지 않았다**).

## 제공자·새 프로젝트 생성·복제 (W3c)
코드 변경만 있고 새 스크립트·유닛·설정 키·플래그·방화벽 변경은 없다. 달라지는 것: `registry.json`이 **버전 2**로 올라가 `providers`·`orphans`를 담는다(**W3b 이하 broker는 버전 2를 깨짐으로 봐서 롤백 때 `/root/backup-w3c`의 버전 1 사본으로 등록부를 되돌려야 한다**), broker가 `repo_roots[0]`(= `/home/orch/repos`) 아래 이름으로 프로젝트 경로를 정하고(파일 시스템은 보지 않는다), orchd(`orch` 계정, `ReadWritePaths=/home/orch/repos`)가 그 아래에 `git clone`(ssh만, 고정 호스트 키 파일 `known_hosts_gitlab`의 호스트만, 배포 키 `id_gitlab_deploy`)으로 새 폴더를 만든다. broker는 제공자 주소(사내 GitLab `100.64.0.10:18082`, 이미 nft가 허용)로 HTTP 호출을 한다: 다른 주소는 새 방화벽 규칙이 필요하며 **방화벽은 바꾸지 않는다**. 정책 `policy.yml`에 클래스 `registered`가 있어야 새 프로젝트를 제안할 수 있다(사용자 결정). `orch-broker -check`는 제공자 설정(주소 모양·규칙)을 읽기 전용으로 점검한다. **이 도구는 GitLab의 그룹·프로젝트를 지우지 못한다.**
적용 순서·검증·롤백은 `docs/10-OPERATIONS-orchestrator.md` 14장(**아직 실서버에서 실행하지 않았다**).
