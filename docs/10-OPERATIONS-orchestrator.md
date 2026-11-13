# 오케스트레이터 운영 안내서 (2026-11-07 기준)

구현·시험 결과는 `09-RESULTS-orchestrator.md`, 이 문서는 **지금 돌고 있는 환경을 다루는 방법**과 함정을 모았다.

## 1. 구성 (홈서버 OpenStack, 네트워크 aigate-net 10.70.0.0/24)
| 장비 | 주소 | 역할 | 비고 |
|---|---|---|---|
| orch-vm | 10.70.0.11, FIP 192.168.1.200 | `orchd`(systemd 서비스), `orch` CLI, 판정(게이트), 이력·승인·DB | 사용자 `orch`, 홈 `~/orchd`, 바이너리 `~/bin/{orchd,orch,aigate}` |
| agent-vm | 10.70.0.12 | Claude·Codex 실행(구독 로그인 완료) | 강제 명령 래퍼 `/usr/local/sbin/agent-runner`, FIP 없음 |
| test-vm | 10.70.0.13 | 검사 명령 실행(원격 실행기), 작업마다 스냅샷 복원 | **지금은 서버가 없을 수 있다**(복원이 만든다) |
| homeserver | 192.168.1.121 (고정) | OpenStack, 위 VM들 | BIOS PEGATRON IPMMB-MH1 / AMI 0202 |
| gpu-pc | 192.168.1.129 (DHCP 예약) | 별개(k3s 워커, GPU) | 이 문서의 범위 밖 |

- 모든 VM은 VM 안 nftables(기본 차단)이고 **ICMP도 막는다**. 생존 확인은 ping이 아니라 TCP 22/ssh로 한다.
- 방화벽: orch-vm만 홈서버 OpenStack API(192.168.1.121의 5000 8774 8776 9292 9696 8778)로 나갈 수 있다. agent/test VM은 LAN·상호·orch-vm 접속 불가.
- orchd 설정: 정책 `~/orchd/policy.yml`(종류 toy·sample-app = L1, 허용 목록 push_branch), 게시 대상은 로컬 bare `~/repos/remote.git`(**원격 push 없음**), 복원 스냅샷 `test-vm-clean3`(Go 모듈·빌드 캐시를 데워 둔 상태), 복원 직후/직전 자동 정리 켬(`--prune-restore-volumes`).
- 서비스 파일은 저장소의 `infra/systemd/orchd-orchvm.service`(설치본은 `/etc/systemd/system/orchd.service`, 이전 판은 `*.pre-prune`).

## 2. 접속 경로
- 관리자 키 `~/.ssh/aigate_admin_ed25519`는 이 Mac에만 둔다(VM으로 복사·포워딩 금지). orch-vm: `ssh -i <키> -o IdentitiesOnly=yes ubuntu@192.168.1.200`, agent/test는 orch-vm을 `ProxyCommand`로 거친다.
- orch-vm은 LAN 주소 **.121, .124, .128**에서만 ssh를 받는다. 이 Mac은 공유기 예약으로 `.124`. 막히면 homeserver(.121)를 점프 호스트로 쓴다(`power-check.sh`, `power-off.sh`가 이미 그렇게 한다).
- orchestrator 전용 키는 orch-vm `~/.ssh/id_aigate`(개인 키는 거기서만), 호스트키 고정 `~/.ssh/known_hosts_aigate`. test-vm 스냅샷은 호스트키를 보존한다(`ssh_deletekeys: false`).

## 3. 작업 다루기 (orch-vm에서, 사용자 orch)
```
orch --socket ~/orchd/run/orchd.sock submit --repo ~/repos/<저장소> --base <SHA> --class <종류> --agent claude|codex --prompt '...'
orch --socket ~/orchd/run/orchd.sock list | show <작업ID> | cancel <작업ID> | retry <작업ID> | approvals | approve <승인항목ID> | reverify <승인항목ID>
orch --socket ~/orchd/run/orchd.sock propose | policy show | policy clear-demotion <종류>
```
- 저장소는 `~/repos/` 아래에 있어야 한다(정책 `repo_roots`). 지금: `toy`, `sample-app`(로컬 클론, 원격 없음).
- `orch approve <승인항목ID>`는 **실제 터미널(TTY)에서 사람이** 한다(`ssh -t`). **ID는 `orch approvals`의 ID 열(승인 항목 번호)이고 작업 번호가 아니다**(작업 2의 항목이 1번이면 `approve 1`; 승인 파일 위치는 `--gate-home /home/orch/orchd/gate-home`, 플래그는 명령 앞에). 예: `ssh -t ... 'sudo -u orch -H /usr/local/lib/aigate/orch --socket /home/orch/orchd/run/orchd.sock --gate-home /home/orch/orchd/gate-home approve 1'`. 데몬은 승인을 줄 수 없고 승인 파일(GateHome)에 맞는 승인이 있을 때만 재판정한다.
- 상태: queued → preparing → agent_running → collecting → verifying → passed → publishing → done. 판정이 아니라 **인프라 오류**는 error로 재시도(최대 3회) 후 failed. 이력에는 원인이 짧게 남는다(`task_events.note`).
- 정책 변경은 다음 작업 시작 때 반영되고 해시가 기록된다. 게시 시점에 정책 해시가 시작 때와 다르면 사람 승인으로 넘어간다.
- 그 "정책 변경" 승인 항목(종류 `publish_outside_allowlist`, 이유 "policy changed since the task started: 옛12 -> 새12")도 `orch approve <항목ID>`로 승인한다. 데몬이 현재 적용 중인 정책 해시를 내려주고 CLI가 항목 해시를 다시 계산해 일치할 때만 승인을 기록하므로, 승인 후 정책이 또 바뀌면 옛 항목은 거부된다(`approve 거부: …알려진 게시 행위와 맞지 않습니다`). 이때는 새 작업이 정책을 다시 읽은 뒤 `orch reverify`로 새 항목을 만든다. (2026-11-08 수정 `7604b80` 이전에는 이 종류가 승인되지 않는 결함이 있었다. 실제로 작업 2에서 확인·수정·승인·`done`까지 검증됨.)

### 3.1 인프라 오류로 실패한 작업 다시 시도 (`orch retry`)
```
orch --socket ~/orchd/run/orchd.sock retry <ID>      # 작업 #N 다시 시도: <단계>부터
```
- 대상: `failed`이면서 **인프라 오류로 시도 한도(3회)를 다 쓰고** 실패한 작업(복원 쿼터 413, VM 정지, ssh 불통 등). `orch list`에는 `failed (재시도 가능)`, `orch show`에는 `재시도:` 줄로 표시된다(API `retryable`, `retries`).
- 효과: 실패한 단계(`preparing`/`agent_running`/`collecting`/`verifying`)부터 다시 돈다. 시도 횟수와 오류 문구는 0으로 지워지고, 이력에 `retry requested by operator (was: <원인>)`이 남는다. 이미 가져온 커밋(head)과 base는 바뀌지 않는다.
- 거절(409): 판정(behavior_failure, check_weakened, verification_incomplete)·승인 거절로 실패한 작업, cancelled/done/진행 중/승인 대기 작업, **`publishing`에서 실패한 작업**(push가 이미 일어났을 수 있어 재게시를 보장할 수 없음), 작업당 재시도 3회를 모두 쓴 작업. 이 경우는 새로 제출한다.
- 구분 기준은 DB(스키마 v5의 `infra_failed`, `retry_state`, `retries`). v5로 올릴 때 기존 failed 작업은 `error_text`가 있고 마지막 실패 이력이 `<단계> failed after N attempts`인 것만 인프라 실패로 표시한다. 그 밖(크래시 중단 등)은 재시도 불가로 둔다.

## 4. 전원 끄고 켜기
- 끌 때 `sh infra/power-off.sh --apply`(gpu-pc 먼저, homeserver 마지막, 실행 중인 작업이 있으면 거부). homeserver는 VM 메모리를 저장하느라 몇 분 걸린다.
- 켤 때 homeserver 먼저 → 약 5분 → gpu-pc. 그다음 `sh infra/power-check.sh`(읽기 전용, FAIL 없으면 정상; test-vm 없음은 경고).
- **DHCP 함정:** 꺼져 있는 동안 공유기가 비어 있는 주소를 다른 기기(스마트폰)에 준 적이 있다. `.121`에서 응답하는 MAC이 homeserver(`aa:bb:cc:dd:ee:ff`)가 맞는지 ARP로 확인한다. ping 응답만으로 생존을 판단하지 않는다. 공유기 DHCP 예약 5건(gpu-pc .129, homeserver .121, Mac mini .128, openclaw .122, MacBook .124)이 이를 막는다. 충돌 중이면 해당 기기의 Wi-Fi를 껐다 켜게 한다(몇 분 걸림).
- 공유기(LG U+ GAPD-7500R) 설정은 `상태정보 > DHCP 할당정보 > DHCP 고정 할당`. 적용하면 세션이 끊겨 재로그인(비밀번호+캡차, 사용자)이 필요하다. 원격제어(포트 88)가 켜져 있는 점은 사용자 확인 대기.

## 5. 용량(쿼터)과 스냅샷: 가장 흔한 고장
- Cinder `gigabytes` 쿼터 300 GB는 **볼륨과 스냅샷을 합산**한다. 복원마다 30 GB 볼륨이 새로 생기고, 스냅샷도 30 GB씩 차지한다. 가득 차면 복원이 `HTTP 413 Request Entity Too Large`로 실패하고 작업은 failed(인프라 오류)로 끝난다.
- 자동 정리(`--prune-restore-volumes`)가 복원 직전·직후에 자기가 만든 옛 복원 볼륨만 지운다(이름 패턴, 스냅샷 원본 제외, 현재 부팅 볼륨 제외, 살아 있는 서버에 붙은 것 제외, 호출당 최대 5개, 실패해도 작업은 실패하지 않음).
- 사람이 해야 하는 정리(삭제는 되돌릴 수 없어 사용자가 직접 실행): `sh infra/cleanup-restore-volumes.sh [--apply]`(복원 잔여 볼륨), `sh infra/cleanup-old-snapshots.sh [--apply]`(대체된 옛 스냅샷 `test-vm-clean`, `-clean2`와 그 원본). 모두 기본은 드라이런.
- 새 깨끗한 스냅샷 만들기: test-vm에서 도구·캐시를 준비하고 남은 저장소·작업 폴더를 지운 뒤 `AIGATE_SNAPSHOT=<이름> bash infra/snapshot.sh create test-vm`, 그리고 서비스 파일의 `--test-snapshot`을 바꿔 재시작. 이전 스냅샷은 위 정리 스크립트로 지운다.
- `volume revert`는 이 홈서버에서 동작하지 않는다(Cinder `lock_path` 미설정). 복원 = 서버 삭제 → 스냅샷에서 새 볼륨 → 서버 생성(약 50 s).

## 6. 시간 감각 (측정값)
단계별 시간은 `sh infra/task-timing.sh [작업ID...]`로 읽는다(이력의 타임스탬프 + `verifying`의 복원·게이트 분해 문구).

실저장소 sample-collector, 복원 켬, 캐시를 데운 스냅샷 `test-vm-clean3`, 작업 4회(2026-11-07, 작업 17~20, 편차 ±2 s):
| 구간 | 시간 | 비고 |
|---|---|---|
| 준비 / 수집 / 게시 | 2.4 s / 1.2 s / 0.1 s | |
| 에이전트(Claude) | 25 s | 23~26 s |
| 판정 합계 | 251 s | 복원 + 게이트 |
| ├ 복원 | 101 s | 서버 삭제 9.5 + **복원 직전 정리 36** + 볼륨 생성 5.7 + 서버 생성 10~16 + ssh 대기 9~14 + **복원 직후 정리 26** |
| └ 게이트 | 150 s | base 77 + head 70 |
| **작업 총합** | **280 s** | |

- **정리 최적화 후 측정(2026-11-08, 작업 21~24, 편차 ±3 s):** 총 **238 s**(236~243), 이전 280 s보다 -42 s(-15%).
  | 구간 | 이전 | 이후 |
  |---|---|---|
  | 복원 | 101 s | **42 s** (서버 삭제 9.5 + 쿼터 확인 3.1 + 볼륨 생성 5.7 + 서버 생성 16 + ssh 대기 8) |
  | 게이트 | 150 s | 164 s (base 81 + head 72) |
  | 판정 합계 | 251 s | 206 s |
  | 에이전트 | 25 s | 29 s (26~33) |
  쿼터 조회 1회(`quota show --usage`)로 여유(gigabytes ≥ 스냅샷 2배, volumes ≥ 2)가 확인되면 복원 직전 정리를 건너뛰고(단계 `quota_ok`), 복원 직후 정리는 백그라운드로 돌린다(다음 복원은 최대 2분 그 정리를 기다린 뒤 서버를 지운다). 쿼터를 읽지 못하면 예전처럼 정리부터 한다.
  백그라운드 정리는 매번 "이전 복원 볼륨 1개 삭제, 건너뜀 2개(스냅샷 원본, 현재 부팅 볼륨)"를 로그에 남기고(`journalctl -u orchd | grep 'background prune'`), 볼륨 5개·쿼터 150/300 GB가 유지된다.
  **게이트가 150 s → 164 s로 14 s 느려졌다**(4회 모두 일정, 잡음 아님). 원인은 아직 모른다.
  - 실험 1(2026-11-08, 작업 25~28): "백그라운드 정리가 orch-vm(1 vCPU)에서 게이트와 CPU를 나눠 쓴다"는 가설로 정리를 게이트가 끝난 뒤에 시작하게 바꿨다. **가설은 기각**: 게이트는 158~161 s로 거의 그대로였다(150 s로 돌아오지 않음). 오히려 연속 작업에서 다음 복원이 정리를 기다리는 일이 생겼다(작업 28: `prune_wait` 38 s, 총 326 s; 정리 40~65 s vs 다음 작업 복원까지 여유 약 27 s). 평균 258 s(이상치 제외 236 s)로 이득이 없어 **되돌렸다**(`27361da`).
  - 남은 후보(미검증): 홈서버·테스트 VM 쪽의 시간대별 부하 차이, 복원 볼륨의 첫 읽기 지연 등. 게이트 150 s → 160 s대는 2026-11-07 밤과 11-08 새벽 측정 사이의 차이이므로, 같은 조건에서 다시 재 보기 전까지 결론을 내지 않는다.
- **실제 GitLab push(2026-11-08, 작업 38):** sample-app 작업만 `ssh://git@100.64.0.10/example-group/sample-collector.git`(GitLab ssh 22번)로 `refs/heads/orch/<번호>`를 push한다(`--git-remote-for REPO=REMOTE`, 다른 저장소는 로컬 `remote.git`). 인증은 이 프로젝트 전용 Deploy Key(`~orch/orchd/ssh/id_gitlab_deploy`, 쓰기 권한 체크 필수)이고 호스트 키는 `known_hosts_gitlab`로 고정(지문 SHA256:OYP7CL…, 서버의 `/etc/ssh/ssh_host_ed25519_key.pub`와 일치 확인). `main`·`dev`는 보호/미접촉. 작업 37은 키에 쓰기 권한이 없어 `This deploy key does not have write access`로 publishing 실패(재시도 불가라 38로 재제출). 이 저장소엔 push 웹훅 2개가 있다(`dev`만 반응하는 것, 필터 없는 `192.168.0.37:30093`은 `orch/*` push 때 호출됨, 사용자 승인). 이전 바이너리/유닛: `orchd.prev-gitlab-ssh`, `/etc/systemd/system/orchd.service.pre-gitlab-ssh`.
- **base 결과 재사용(2026-11-08, `70c41ff`, 작업 34~36, 같은 base):** 첫 작업은 base 79 s를 실행(총 237 s), 이어진 두 작업은 base 0 s(`base cache 1/1`)로 게이트 82~84 s, **작업 자체 시간 약 160 s(-32%)**. 판정은 모두 ok. 키는 저장소·base SHA·검사 요청 전체·게이트 빌드(orchd 바이너리의 VCS 리비전)·테스트 VM 이미지(`<이름>/<스냅샷>`)이고, 깨끗한 실행(RC 0, 시간 초과·setup 실패 없음)만 `~orch/orchd/gate-home/.aigate/base-cache/`에 0600으로 저장, TTL 24 h. head 실행은 저장하지 않는다. 끄기: orchd `--base-cache=false`, 비우기: 그 디렉터리 삭제. **새 orchd를 배포하면 빌드 키가 바뀌어 한 번은 다시 실행된다. 스냅샷을 새로 만들면 이름을 바꿔라**(같은 이름 재사용 시 TTL까지는 옛 결과가 남음).
- **최종 배포 상태의 회귀 측정(2026-11-08, 작업 31~33):** 총 **235 s**(232~237), 에이전트 25 s, 판정 207 s(복원 42 s + 게이트 165 s[base 80~82, head 71~72]). 정리 방식은 복원 직후 백그라운드(`11ffde2`), 쿼터 확인 3 s로 복원 직전 정리 생략. 정리 로그는 매번 "이전 복원 볼륨 1개 삭제", 볼륨 5개·쿼터 150/300 GB 유지. 이 값이 현재 기준선이다.
- 비교: 이전 측정(차가운 캐시, 복원 켬) 345 s(판정 315 s) → 280 s(-19%). 게이트만 보면 261 s → 150 s(-43%, 캐시 데운 효과).
- **자동 정리가 작업당 약 61 s(22%)를 쓴다.** 복원 자체(정리 제외)는 약 40 s. 정리가 오래 걸리는 이유는 `openstack` CLI를 호출마다 새로 띄우는 비용(호출당 수 초)이 상태 확인에 여러 번 쓰이기 때문으로 보인다(미확인, 호출 수 계측은 안 함).
- 게이트의 base(77 s)는 같은 base SHA에서 매번 똑같이 다시 돈다. base 결과를 base SHA+설정 해시로 재사용하면 이론상 약 77 s를 줄일 수 있으나, 같은 환경이 아닌 결과를 믿는 것이므로 신뢰 모델(깨끗한 환경에서 항상 실행) 논의가 필요하다. 구현하지 않았다.
- 참고(이전 측정): 새 테스트 파일 추가형 toy 작업 약 20 s(복원 제외), 복원 포함 약 75 s. 복원 없음·차가운 캐시 sample-app 300 s.

## 7. 알려진 한계·함정
- exit-code 어댑터는 VM이 말하는 종료 코드를 믿는다. 검사 코드는 같은 uid로 돌아 자기 결과 파일을 위조할 수 있다(스냅샷 복원과 크기 상한으로 완화).
- 복원 켠 채 게시 설정이 있으면 서비스가 시작을 거부하는 장치가 있다(`--allow-publish-without-restore`).
- 강등 트리거 "게시 후 회귀"는 현재 구조에서 도달 불가.
- `orchd`가 죽은 뒤 재시도와 겹치는 에이전트는 래퍼가 같은 작업의 `run`/`prepare` 시작 때 `run.pid`로 정리한다(같은 작업에 살아 있는 다른 실행이 남으면 종료 코드 5). **2026-11-08 agent-vm에 설치·실제 검증함**(작업 30: 에이전트 실행 중 `orchd` kill -9 → 자동 재시작 → 옛 `claude`가 정리되고 새 `claude` 하나만 돌아 커밋 1개로 done). 이전 래퍼는 `/usr/local/sbin/agent-runner.prev-reap`(롤백: 그 파일을 `agent-runner`로 복사). 한계: 에이전트의 Bash 도구가 별도 세션으로 띄운 자식 프로세스는 프로세스 그룹 종료를 피해 자기 일이 끝날 때까지 남을 수 있다(시험에서 `sleep 45`가 약 10 s 남았다). 래퍼 설치 전에 시작된 실행은 `run.pid`가 없어 정리되지 않는다. `ps` 출력은 Ubuntu에서 실제 동작으로 확인했다.
- DB를 교체해도 orchd는 시작할 때 저장소의 `refs/orch/*`와 로컬 원격의 `orch/*` 브랜치에서 가장 큰 작업 ID를 찾아 그 다음 번호부터 시작한다(읽지 못한 저장소는 시작 로그에 경고).
- Codex는 agent-vm에서 전용 AppArmor 프로파일(`infra/agent-codex-sandbox.sh`)로 동작한다. Codex 업그레이드로 바이너리 경로가 바뀌면 `--apply`를 다시 실행한다.
- Mac 쪽: Wi-Fi 비공개 주소는 `home-wifi-5G`에서 "고정식"(aa:bb:cc:dd:ee:ff). Orca Computer Use의 접근성 권한은 목록 표시와 실제가 다를 수 있어 실제 호출로 확인하고, 안 되면 "기기 제어 및 데이터 접근"에서 껐다 켠 뒤 Orca를 재시작한다.

## 8. 스크립트 모음 (`infra/`)
| 스크립트 | 용도 | 기본 동작 |
|---|---|---|
| `create-vms.sh` | VM·네트워크·볼륨 생성 | 드라이런 |
| `apply-firewall.sh <역할>` | nftables 적용(180 s 자동 롤백) | 드라이런 |
| `verify-firewall.sh`, `test/security-checks.sh --run` | 차단·자격 증명·권한 점검 표 | 읽기 전용 |
| `snapshot.sh create\|restore\|time\|prune` | 스냅샷·복원 측정 | 실행 |
| `agent-runner/install.sh`, `agent-login.sh`, `agent-codex-sandbox.sh` | 에이전트 VM 설치·로그인·Codex 샌드박스 | 드라이런(로그인은 사용자 TTY) |
| `orch-openstack-access.sh` | orch-vm의 OpenStack API 접근(방화벽+자격 증명 복사) | 드라이런, 사용자 실행 |
| `cleanup-restore-volumes.sh`, `cleanup-old-snapshots.sh` | 용량 정리 | 드라이런, 사용자 실행 |
| `power-off.sh`, `power-check.sh` | 홈서버 종료·재가동 확인 | 드라이런 / 읽기 전용 |

## 9. 웹 콘솔 W1 컷오버 런북
설계·결과는 `11-DESIGN-webconsole-W1.md`, `13-RESULTS-webconsole-W1.md`. **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다**(코드는 이 저장소의 시험으로만 확인함). 아래 "예상 결과"는 코드·시험·스크립트를 읽고 쓴 것이지 서버에서 본 출력이 아니다. 처음 실행할 때 다르면 그 단계에서 멈추고 롤백(6단계)을 고려한다. 순서는 바꾸지 않는다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요한 단계, **[되돌림 가능]** 아래 되돌리기 한 줄로 원상 복구, **[주의]** 되돌리려면 DB 복원 등이 필요.
표기한 `<…>`는 실행 때 채우는 값이다. `ORCHVM=ubuntu@192.168.1.200`, 관리자 키는 2장의 `~/.ssh/aigate_admin_ed25519`. orch-vm 안의 경로는 3장과 같다(바이너리 `/usr/local/lib/aigate`(root 소유, 아래 단계 1), 상태 `/home/orch/orchd/state`, 소켓 `/home/orch/orchd/run/orchd.sock`).

### 9.0 시작 전에 모두 참이어야 하는 것
- 실행 중이거나 승인 대기인 작업이 없다(`orch list`, `orch approvals`). 1·3단계는 `orchd`를 재시작하고, 4단계부터는 서명 없는 기존 승인 항목이 무효가 된다(이미 `done`인 이력은 보존).
- **Tailscale Funnel은 켜지 않는다**(공인 인터넷 노출). 스크립트는 Funnel 명령을 쓰지 않고 `--apply`는 `tailscale serve status`에 Funnel이 켜져 있으면 중단한다.
- **orch-vm에서 신뢰할 수 없는 로컬 프로세스를 돌리지 않는다**(CF20). HTTP 리스너는 루프백이라 로컬 프로세스가 `Tailscale-User-Login` 같은 헤더를 마음대로 보내 횟수 제한 키를 바꿀 수 있다. 신뢰할 수 없는 코드는 agent-vm/test-vm에서만 돈다.
- **`--allowed-host`, `--origin`(`https://<FQDN>`, 포트 없음), `--rp-id`(`<FQDN>`)는 브라우저 주소창의 tailnet 이름(`<노드>.<tailnet>.ts.net`)과 글자 그대로 같아야 한다.** 다르면 요청이 전부 421이 되거나 패스키가 등록되지 않는다. 패스키는 이 이름에 묶이므로 나중에 이름이 바뀌면 다시 등록해야 한다.
- Mac과 orch-vm의 시계 차이가 5분 이내(`date -u`로 비교). 로그인·서명 메시지의 `issued`가 ±5분을 벗어나면 거부된다.
- Mac은 공유기 예약 `.124`에서 orch-vm에 ssh할 수 있다(2장). 서명에 쓸 ssh 키는 **승인 전용으로 새로 만든 ed25519 키**이고 비밀번호가 걸려 있어야 한다(오케스트레이터 키·VM 접속 키는 쓰지 않는다). 예: 터미널 앱에서 `ssh-keygen -t ed25519 -C "aigate-approval" -f ~/.ssh/aigate_approve_ed25519`(**`!`나 자동 실행으로는 비밀번호를 입력할 수 없어 빈 비밀번호가 된다**; 만든 뒤 `ssh-keygen -y -P "" -f <키>`가 실패해야 비밀번호가 걸린 것). 승인할 때는 `--sign-key ~/.ssh/aigate_approve_ed25519 --signer <서명자 ID>`를 준다(`~/.ssh/id_ed25519`는 기본값일 뿐 없을 수 있다).
- `orchd` 소켓을 쓰는 운영 명령(`admin enroll` 등)을 `sudo`로 하려면 유닛에 `--socket-uid 0`이 있어야 한다(3단계 유닛). `--socket-uid`는 orchd 자신의 uid에 **더해지며** 대체하지 않는다. 없으면 `sudo orch`는 403(`forbidden_uid`).
- 저장소(Mac)의 `infra/` 스크립트 중 `signers-init.sh`, `orch-web-serve.sh`는 orch-vm에서 돌린다. 저장소가 VM에 없으면 `scp -i <관리자 키> infra/signers-init.sh infra/orch-web-serve.sh $ORCHVM:/tmp/`로 복사한다(스크립트만, 키는 복사하지 않음).

### 단계 1. 세 바이너리 백업·배포, 레거시 동작 확인 [사용자][주의: DB 스키마가 v6로 올라간다]
**백업(전부 먼저).** orch-vm에서, `orchd`를 멈춘 상태로:
```
sudo systemctl stop orchd
B=/home/orch/orchd/backup-w1; sudo mkdir -p $B
# 옛 바이너리는 /home/orch/bin에 있다(orch 소유). 새 위치는 root 소유 폴더 /usr/local/lib/aigate이고, 백업도 거기에 root 소유로 둔다.
sudo install -d -o root -g root -m 0755 /usr/local/lib/aigate
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /home/orch/bin/$f /usr/local/lib/aigate/$f.prev-w1; done
sudo sh -c "cp -a /home/orch/orchd/state/orchd.db* $B/ && cp -a /home/orch/orchd/gate-home $B/gate-home && cp -a /etc/systemd/system/orchd.service /etc/systemd/system/orchd.service.pre-w1"
```
**배포.** 새 `orchd`·`orch`·`aigate` 세 개를 **한꺼번에** 올린다(옛 `aigate`는 서명 필드를 몰라 서명 없는 항목도 통과시킨다). Mac에서 `GOOS=linux GOARCH=<VM의 uname -m: x86_64→amd64, aarch64→arm64> go build -o /tmp/w1/orchd ./cmd/orchd`를 세 명령에 대해 하고(`./cmd/orch`, `./cmd/aigate`), `scp`로 VM `/tmp/w1/`에 보낸 뒤:
```
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w1/$f /usr/local/lib/aigate/$f; done
# 설치된 유닛의 ExecStart가 옛 경로(/home/orch/bin/orchd)를 가리키므로 경로만 먼저 바꾼다(.pre-w1 백업은 위에서 만들었다). infra 스크립트(power-check.sh 등)는 저장소에서 이미 새 경로를 쓴다.
sudo sed -i 's#/home/orch/bin/#/usr/local/lib/aigate/#g' /etc/systemd/system/orchd.service && sudo systemctl daemon-reload
sudo systemctl start orchd
```
**왜 root 소유 폴더인가.** `orch` 계정이 쓸 수 있는 곳(`/home/orch/bin`, 또는 orch 소유 홈 안의 폴더 — 홈 주인은 폴더 이름을 바꿔 바꿔치기할 수 있다)에 바이너리를 두면, orch로 도는 아무 프로세스(ssh 로그인, `sudo -u orch` 셸)나 `orchd`/`aigate`를 바꿔 서명 검증을 없앨 수 있고, `sudo /usr/local/lib/aigate/orch admin …`은 root로 실행되므로 바꿔치기가 곧 root 실행이 된다. 그래서 상위 폴더까지 root 소유여야 한다(`ls -ld /usr/local/lib /usr/local/lib/aigate`, `ls -l /usr/local/lib/aigate` 모두 `root root`, 폴더 0755, 파일 0755 확인). 스크립트(`power-check.sh` 등)는 이 바이너리를 **실행만** 한다(쓰는 곳 없음, grep으로 확인). 배포를 마치고 검증이 끝나면 옛 `/home/orch/bin`은 `sudo mv /home/orch/bin /home/orch/bin.old-w1`로 치워 두어(롤백 후보 아님: 롤백은 `.prev-w1`) 실수로 쓰이지 않게 한다.

**전제.** `/etc/aigate`가 아직 없다(2단계 전). `signers.json`이 없으므로 서명 모드는 꺼져 있다(`/etc` 아래가 root 소유 폴더라 "없음"으로 판정, R16).

**검증 체크리스트**
- `journalctl -u orchd -n 40`: `orchd: config: approvals: mode legacy (no signers registry at /etc/aigate/signers.json)`가 보이고 오류 없이 대기 중. 첫 시작에서 DB 스키마가 v6으로 올라간다(코드상 자동 마이그레이션).
- toy 작업 하나를 3장 명령으로 제출 → 승인 대기까지 가는 작업(보호 파일 변경 등 기존에 쓰던 종류)을 하나 만들고 **기존 TTY 방식**(`ssh -t … 'sudo -u orch -H /usr/local/lib/aigate/orch --socket … --gate-home … approve <항목ID>'`)으로 승인 → `done`.
- 새 `orchd`는 게이트 빌드 키가 바뀌어 base 결과 캐시를 한 번 다시 실행한다(6장). 판정이 느려 보여도 정상.
- **배포 전부터 대기 중이던 승인은 다시 승인해야 한다(R23, 2026-11-09).** 새 빌드의 diff 해시는 SHA-256 전체 64자리이고 옛 빌드는 앞 16자리를 저장했다. 마이그레이션은 없다: 옛 16자리 대기 행과 `approvals.json`의 16자리 승인은 새 해시와 맞지 않아 무효다(fail closed). 가장 쉬운 길은 **백업 전에 `orch approvals`를 비우는 것**(옛 빌드로 승인해 끝내거나 취소). 배포 뒤 남은 16자리 행: `orch approve <ID>`는 해시 불일치로 거부한다. 옛 빌드가 이미 `approvals.json`에 승인을 써 둔 행이면 `orch reverify <ID>`로 게이트가 새 해시로 다시 판정해 64자리 항목이 대기로 올라오고, 그것을 다시 승인한다(`cmd/orch` `TestOldShortHashPendingEntry`로 확인, 레거시 모드). 승인 기록이 없는 행은 `orch cancel <작업ID>` 후 다시 제출한다. 확인: `orch --json approvals`의 `hash`가 모두 64자리.

**그래도 안 되면:** 6단계의 바이너리·DB 복원. **옛 `orchd`는 v6 DB를 열지 않는다**(`database schema version 6 is newer than this build`, 옛 코드 읽기로 확인) — 바이너리 복원은 반드시 `$B`의 DB 백업 복원과 함께 한다. 그 사이의 작업 기록은 사라진다.

### 단계 2. `/etc/aigate` 만들기 [사용자][되돌림 가능: `sudo rmdir /etc/aigate`]
**전제:** 단계 1 검증 통과. 
```
sudo sh /tmp/signers-init.sh            # 드라이런: 상태·계획만 출력
sudo sh /tmp/signers-init.sh --apply    # root 아니면 "nothing changed"로 거부
```
스크립트는 `/etc/aigate`를 `root:root 0755`로 만들 뿐 `signers.json`은 **만들지 않는다**(만들면 서명 필수 모드). 상위·대상 폴더가 심볼릭 링크이거나 그룹·기타 쓰기 가능이면 거부한다.

**검증 체크리스트**
- 출력 끝 `done: /etc/aigate root:root 0755 (signers.json not created)`, `ls -ld /etc/aigate` → `drwxr-xr-x … root root`, `ls /etc/aigate` 비어 있음.
- 단계 1의 TTY 승인을 한 번 더 해도 그대로 된다(레거시 유지; 재시작 불필요).

**그래도 안 되면:** `sudo rmdir /etc/aigate`(비어 있을 때). 스크립트가 거부하면 메시지의 폴더 권한부터 고친다(우회하지 않는다).

### 단계 3. HTTPS 노출 (3-a 인증서, 3-b 유닛, 3-c serve, 3-d 방화벽)
**3-a. [사용자 직접] Tailscale 관리 콘솔에서 HTTPS 인증서 기능을 켠다.** 관리 콘솔의 DNS 설정에서 MagicDNS를 켠 상태로 HTTPS Certificates를 켠다(README의 안내를 따른 것, 화면 문구는 확인하지 않음). 스크립트가 대신하지 않는다. 알려진 점(이 작업 중 재확인하지 않음): HTTPS를 켜면 tailnet 이름이 인증서 투명성 로그에 공개될 수 있다. 되돌리려면 같은 화면에서 끈다. 그다음 이 노드의 전체 이름 `<FQDN>`을 관리 콘솔의 머신 목록에서 적어 둔다.

**3-b. 유닛 갱신 [사용자][되돌림 가능: `.pre-w1` 복원].** `infra/systemd/orchd-orchvm.service`의 예시 값 `orch-vm.example.ts.net`을 `<FQDN>`으로 바꾼다(`--allowed-host`, `--origin https://<FQDN>`, `--rp-id` 세 곳). 전제: **설치된 유닛과 저장소 유닛의 차이를 먼저 본다** — `diff /etc/systemd/system/orchd.service <수정한 파일>`이 `ExecStart` 경로(단계 1에서 이미 바꾼 `/usr/local/lib/aigate/orchd`)와 새 플래그(`--http-listen`, `--allowed-host`, `--origin`, `--rp-id`, `--signers-file`, `--pending-file`, `--socket-uid 0`)만 보여야 한다. 다른 차이가 있으면 설치된 쪽이 기준이다(저장소 파일이 낡은 것).
```
sudo cp <수정한 유닛> /etc/systemd/system/orchd.service && sudo systemctl daemon-reload && sudo systemctl restart orchd
```
**검증:** 로그에 `orchd: socket uids allowed: [0 <orch uid>]`, `orchd: HTTP listener on 127.0.0.1:8808 (allowed hosts [<FQDN>], origin https://<FQDN>)`, `approvals: mode legacy`. VM에서 `curl -s -H 'Host: <FQDN>' http://127.0.0.1:8808/api/v1/healthz` → `{"ok":true}`, Host 헤더 없이(기본 `127.0.0.1`)는 421. **그래도 안 되면:** `/etc/systemd/system/orchd.service.pre-w1`을 복원하고 재시작. `--allowed-host`/`--origin`/`--rp-id` 형식 오류는 orchd가 시작을 거부하고 `journalctl`에 이유를 남긴다.

**3-c. `tailscale serve` [사용자][되돌림 가능: `--off`].** orch-vm에서:
```
sh /tmp/orch-web-serve.sh 8808            # 계획만 출력(드라이런)
sh /tmp/orch-web-serve.sh 8808 --apply    # 권한 오류면 sudo로
```
`--apply`는 먼저 Funnel 여부를 보고 켜져 있으면 중단한 뒤 `tailscale serve --bg --https=443 http://127.0.0.1:8808`을 실행하고 `tailscale serve status`를 출력한다(실행 후에도 Funnel이 보이면 중단하고 끄는 명령을 안내). 설계상 `tailscale serve`는 Host를 그대로 넘기고 클라이언트가 보낸 `Tailscale-User-*` 헤더를 지운다(tailscale 소스를 읽어 확인한 것, 실행해 본 것이 아님 — X1).

**검증:** `tailscale serve status`에 `https://<FQDN>` → `http://127.0.0.1:8808`이 있고 Funnel 표시가 없다. tailnet의 다른 기기(이 Mac)에서 `curl -s https://<FQDN>/api/v1/healthz` → `{"ok":true}`; 브라우저로 `https://<FQDN>/` → 인증서 경고 없이 콘솔의 로그인 화면. 모든 요청이 421이면 `serve`가 보내는 Host가 `--allowed-host`와 다른 것이므로 3-b의 세 값을 주소창 이름과 대조한다. **그래도 안 되면:** `sh /tmp/orch-web-serve.sh 8808 --off --apply`(`tailscale serve --https=443 off`) 후 3-a 설정과 `tailscale status`의 로그인 상태부터 본다.

**3-d. 방화벽 443 규칙 [사용자][주의].** Mac에서 `sh infra/apply-firewall.sh orchestrator`(드라이런), 적용은 `sh infra/apply-firewall.sh --apply orchestrator`. 스크립트가 하는 일(읽어서 확인한 내용): ① 관리자 키로 `ubuntu@192.168.1.200`에 접속해 `infra/nftables/orchestrator.nft`를 `nft -c -f`로 문법 검사, ② `sudo cp -n /etc/nftables.conf /etc/nftables.conf.pre-task2`(**이미 있으면 덮어쓰지 않는다**), ③ systemd 일회성 타이머 `aigate-rollback`을 180초 뒤에 `pre-task2` 규칙을 복원·적용하도록 걸고, ④ 새 규칙을 `/etc/nftables.conf`에 쓰고 `nft -f`로 적재, ⑤ **새 ssh 연결로 로그인을 다시 시험**하고 성공하면 타이머를 멈춘다(`confirmed: rollback timer stopped`). 로그인이 실패하면 180초 안에 규칙이 복원된다.
- **함정:** 자동 롤백이 되돌리는 대상은 *지금 규칙*이 아니라 Task 2 이전의 `pre-task2`(임시, 아웃바운드 전부 허용 규칙)일 수 있다. 그래서 먼저 지금 규칙을 따로 저장한다: `ssh … 'sudo cp /etc/nftables.conf /etc/nftables.conf.pre-w1'`. 그리고 저장소 규칙과 설치된 규칙의 차이가 443 한 줄뿐인지 본다: `ssh … 'cat /etc/nftables.conf' | diff - infra/nftables/orchestrator.nft`. 더 있으면 적용하지 않는다.
- 이 규칙(`tailscale0`의 tcp 443)이 `tailscale serve`에 **필요한지는 확인하지 않았다**(서브가 443을 tailscaled 내부에서 받으면 필요 없을 수 있다). 규칙은 무해하며 커널 경로 방어로 둔다. nft 문법은 이 Mac에 nft가 없어 이 저장소에서 검사하지 못했다(스크립트의 ①이 VM에서 처음 검사).

**검증:** 스크립트가 `confirmed: rollback timer stopped`로 끝난다. `ssh … 'sudo nft list ruleset | grep 443'`에 tailscale0 규칙이 있다. 3-c 검증을 다시 한다. 22번 접속이 계속 된다. **그래도 안 되면:** 180초를 기다리면 `pre-task2`로 복원되므로 그 뒤 로그인해 `sudo cp /etc/nftables.conf.pre-w1 /etc/nftables.conf && sudo nft -f /etc/nftables.conf`로 직전 규칙으로 되돌린다. 접속이 안 되면 홈서버(.121)를 점프 호스트로 쓴다(2장).

### 단계 4. 이 Mac의 SSH 키 등록 = 서명 필수 모드 전환, 서명 승인 끝까지 시험 [사용자][주의: 이전에 승인된 서명 없는 대기 항목은 무효]
**전제:** 단계 1~3 검증 통과. 위 9.0 전부. 서명 없는 승인 대기 항목 없음. 이 명령을 실행하는 순간부터 **서명이 검증되는 항목만 유효**하고, 목록이 "없음"이 아닌 어떤 문제(소유자·권한·형식·심볼릭 링크)도 모든 승인을 거부한다(fail closed).

Mac에서 공개 키만 VM으로 보낸다: `scp -i ~/.ssh/aigate_admin_ed25519 -o IdentitiesOnly=yes ~/.ssh/id_ed25519.pub $ORCHVM:/tmp/mac-signer.pub`. orch-vm에서(root만 쓸 수 있다. `orch` 사용자는 `/etc/aigate`에 쓸 수 없어 이 파일을 만들거나 지울 수 없다):
```
sudo /usr/local/lib/aigate/orch admin signers add --id mac-ssh --label "MacBook ssh" --key /tmp/mac-signer.pub
sudo /usr/local/lib/aigate/orch admin signers list
```
`add`는 서명자 ID(`--id`, 소문자·숫자·`._-`, 63자 이하)를 정한다. **이 ID를 승인할 때 `--signer mac-ssh`로 줘야 한다**(또는 `AIGATE_SIGNER=mac-ssh`); 없으면 `orch approve`가 거부한다. `admin signers`는 소켓을 쓰지 않으므로 `--socket-uid`와 무관하다(`--signers-file` 기본값은 `/etc/aigate/signers.json`). 서버는 파일을 승인 검사마다 다시 읽으므로 **재시작이 필요 없다**(코드 읽기 기준).

**검증 체크리스트**
- `ls -l /etc/aigate/signers.json` → `root root`, 그룹·기타 쓰기 없음. `list` 출력에 `mac-ssh`, 종류 `ssh-ed25519`, 키 지문(Mac의 `ssh-keygen -lf ~/.ssh/id_ed25519.pub`와 같은 키인지 대조).
- 시작 로그의 `approvals: mode` 줄은 **시작할 때만** 찍힌다. 재시작 전에는 `mode legacy`로 남아 있는 것이 정상이고, `sudo systemctl restart orchd` 뒤에는 `mode signed (1 signers in /etc/aigate/signers.json)`가 된다(재시작해서 확인하는 것을 권장).
- Mac에서: `orch login --server https://<FQDN> --signer mac-ssh`(챌린지에 `ssh-keygen -Y sign`, 네임스페이스 `aigate-login`; 키 비밀번호 입력) → `~/.config/orch/token`(0600) 저장. 이어서 `orch --server https://<FQDN> --signer mac-ssh submit …`로 toy 작업(승인 대기가 생기는 종류) 제출 → `orch --server https://<FQDN> approvals`에서 항목 ID 확인 → `orch --server https://<FQDN> --signer mac-ssh approve <항목ID>`: 서버가 준 diff로 해시를 다시 계산해 일치해야 서명 화면(저장소 id, diff, 서명할 메시지)이 나오고, `yes`를 치면 서명해 `approved (서명자 mac-ssh). 작업 #N 상태: …` → → 작업이 판정·게시를 거쳐 `done`. **원격 `approve`는 서명을 기록하는 순간 서버가 재판정을 걸어 작업을 진행시키므로 `reverify`는 필요 없다**(그 뒤에 부르면 "대기 중인 항목이 아닙니다(상태: approved)"로 거부되는 것이 정상, 2026-11-09 실서버 확인).
- 음성 확인: VM에서 서명 없이 옛 방식 TTY 승인(`approve <ID>` 소켓)은 서명 키가 없어 `서명 키 파일이 없습니다`로 거부된다(코드 읽기 기준; 서명 키를 VM에 두지 않는다).
- 원격 승인이 잘린 diff(256 KiB 초과)이면 CLI와 웹 모두 서명을 거부하고 저장소에서 `git diff`로 보라고 안내한다(R22) — 오류가 아니다.

**그래도 안 되면:** `bad_signature`(키가 `--signer`로 등록된 것인지), `stale_signature`(서명 시각이 ±5분 밖: 시계 확인), `hash_changed`(그 사이 작업이 바뀜: 항목을 다시 확인), `signers_unavailable`/503(파일 소유자·권한·상위 폴더 확인; 고칠 수 없으면 즉시 6단계 `rm`), 429(분당 10회 제한, 1분 뒤 한 번만 재시도). 로그인이 계속 실패하면 서버 로그의 거부 사유(`known=true/false`)를 본다.

### 단계 5. 웹: 패스키 등록, 활성화, 로그인, 웹 서명 승인 [사용자]
**전제:** 단계 4 통과(ssh 서명 승인이 끝까지 된다). 3-b의 `--rp-id`·`--origin`이 주소창 이름과 같다. 패스키를 쓸 브라우저·기기(이 Mac)는 tailnet에 연결돼 있다.

1. orch-vm에서 등록 코드 발급(소켓 전용, `--server`와 함께 쓸 수 없음):
   `sudo /usr/local/lib/aigate/orch --socket /home/orch/orchd/run/orchd.sock admin enroll --id mac-passkey --label "MacBook passkey"`
   → `패스키 등록 코드: XXXX-XXXX`, 10분·일회용. 코드는 등록 시도를 시작하는 순간 소모되므로 의식을 끝내지 못하면 다시 발급한다. `forbidden_uid` 거부면 유닛에 `--socket-uid 0`이 있는지 본다.
2. Mac 브라우저에서 `https://<FQDN>/` → 패스키 등록 화면 → 코드 입력 → OS의 패스키 확인(지문 등). 완료되면 화면에 **지문**이 뜨고, 이 패스키는 **대기 목록에만** 올라가며 아직 로그인·승인에 쓸 수 없다.
3. orch-vm에서: `sudo /usr/local/lib/aigate/orch admin signers pending` → 목록의 ID(`mac-passkey`), 지문, `rp_id`, `origin`을 **브라우저 화면의 지문과 눈으로 대조**한다. 대기 항목 파일은 `/home/orch/orchd/state/signers.pending.json`(orchd가 쓰고 root CLI가 읽는다; 파일이 없으면 CLI가 한국어 안내를 출력). 같으면:
   `sudo /usr/local/lib/aigate/orch admin signers add --pending mac-passkey --fingerprint '<지문>'`
   (`--id`로 활성화할 ID를 바꿀 수 있음). 7일이 지난 대기 항목은 만료되고, 버리려면 `sudo /usr/local/lib/aigate/orch admin signers pending remove <ID>`. 지문이 다르면 활성화하지 않고 버린다.
4. 브라우저에서 패스키로 웹 로그인 → 개요·작업 화면. 승인 대기가 있는 toy 작업(보호 파일 변경)을 만들어 승인 화면에서 메시지·저장소 id·diff를 확인하고 패스키(사용자 확인 필수)로 서명 → 작업이 `done`.

**검증 체크리스트**
- `sudo orch admin signers list`에 `mac-ssh`(ssh-ed25519)와 `mac-passkey`(webauthn) 둘이 있다.
- 웹 로그인 후 새로고침해도 세션이 유지되고(유휴 8시간·절대 24시간), 로그아웃하면 승인·쓰기 요청이 401.
- 이력(ledger)의 승인 이벤트에 서명자 ID와 경로(`via`)가 남는다(시험으로 확인, 실서버에서 읽는 위치는 확인하지 않음).
- 두 번째 탭을 열면 첫 탭의 첫 쓰기가 한 번 `csrf_failed`가 날 수 있고 화면이 한 번 재시도한다(`GET /auth/session`이 CSRF를 갱신하는 설계, R20-1).
- 잘린 diff는 웹에서도 서명이 거부되고 `orch approve` 사용을 안내한다(R22).

**그래도 안 되면:** 패스키 등록 화면이 막히면 `curl -sv https://<FQDN>/…`로 421·인증서 문제를 본다. WebAuthn 오류는 `rp_id`/`origin`이 주소창 이름과 다른 경우가 대부분이다(3-b 값 수정 후 재시작, 기존 대기 항목은 `pending remove`로 버리고 다시 등록). 패스키를 못 쓰는 동안에도 4단계의 ssh 서명 승인은 그대로 쓸 수 있다. 실제 브라우저·기기의 패스키 동작은 이 저장소에서 합성 인증기로만 시험했다(`13-RESULTS` 6장).

### 단계 6. 롤백
되돌리는 범위를 고른다. **위에서부터 필요한 만큼만** 한다.
1. **서명 필수 모드 해제 [사용자][되돌림 가능: 다시 `signers add`]:** `sudo rm /etc/aigate/signers.json`. 파일은 `/etc/aigate`(root 소유)에 있어 `orch` 사용자는 지울 수 없고 root만 한다. 서버는 승인 검사(`Valid`)마다 목록을 읽으므로 **다음 검사부터 적용되고 재시작은 필요 없다**(코드 읽기 기준, 실서버 미확인). 확인: 단계 1의 TTY 승인이 다시 된다(서명 없이). 서명되지 않은 기록이 필요 없으면 이 한 줄이 가장 빠른 안전 롤백이다.
2. **웹 노출 끄기 [되돌림 가능]:** orch-vm에서 `sh /tmp/orch-web-serve.sh 8808 --off --apply`(`tailscale serve --https=443 off`), 확인은 `tailscale serve status`가 비어 있음. 필요하면 방화벽은 `sudo cp /etc/nftables.conf.pre-w1 /etc/nftables.conf && sudo nft -f /etc/nftables.conf`(3-d에서 만든 백업. `.pre-task2`는 더 옛 규칙이다).
3. **유닛 복원:** `sudo cp /etc/systemd/system/orchd.service.pre-w1 /etc/systemd/system/orchd.service && sudo systemctl daemon-reload && sudo systemctl restart orchd`(HTTP 리스너가 사라진다).
4. **바이너리 복원 [주의]:** **1번을 먼저** 한다(옛 바이너리는 서명 목록을 모르므로 파일이 남아 있으면 서명이 검증되지 않은 채 통과한다). `sudo systemctl stop orchd` → `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /usr/local/lib/aigate/$f.prev-w1 /usr/local/lib/aigate/$f; done` → **DB 복원: `sudo cp -a /home/orch/orchd/backup-w1/orchd.db* /home/orch/orchd/state/`**(옛 orchd는 v6 DB를 열지 않는다. 백업 이후에 생긴 작업·승인 기록은 사라지므로, 필요하면 복원 전에 현재 DB를 따로 복사한다) → 승인 파일이 필요하면 `backup-w1/gate-home`에서 → `sudo systemctl start orchd`(유닛 `ExecStart`는 새 경로 그대로 둔다. `.pre-w1` 유닛으로 되돌리면 옛 `/home/orch/bin` 경로를 가리키므로, 그 폴더를 치웠다면 유닛의 경로도 같이 맞춘다). 세 바이너리는 항상 같이 되돌린다.
   - **diff 해시 길이(R23):** 옛 바이너리는 diff 해시를 16자리로 계산하므로, 업그레이드 뒤 새 빌드로 만든 승인(64자리 `approvals.json` 항목, 64자리 대기 행)은 옛 바이너리에서 맞지 않는다. DB를 `backup-w1`로 되돌리면 그 사이의 대기 행은 어차피 사라지고, `approvals.json`의 64자리 항목은 남아도 무해하지만 쓰이지 않는다. 롤백 뒤 승인 대기 작업은 옛 바이너리로 다시 승인한다.
5. `/etc/aigate`는 비어 있으면 둬도 무해하다. 지우려면 `sudo rmdir /etc/aigate`. Tailscale 관리 콘솔의 HTTPS 인증서 기능은 [사용자]가 직접 끈다.

**검증:** `journalctl -u orchd`에 `approvals: mode legacy`, TTY 승인으로 toy 작업이 `done`, `orch list`가 단계 1 이전 작업을 보여 준다. **그래도 안 되면:** 아무것도 더 지우지 말고 멈춰서 상태(로그, `ls -l /etc/aigate /home/orch/bin`)를 사용자에게 보고한다.

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 1 바이너리 3개 | 승인 | 바이너리·DB 복원(DB 스키마 v6) | 필요(stop/start) |
| 2 `/etc/aigate` | 승인(root) | `sudo rmdir /etc/aigate` | 불필요 |
| 3-a HTTPS 인증서 | **사용자 직접**(관리 콘솔) | 같은 화면에서 끄기 | — |
| 3-b 유닛 | 승인 | `.pre-w1` 복원 | 필요 |
| 3-c `serve` | 승인 | `--off --apply` | 불필요 |
| 3-d 방화벽 443 | 승인 | `.pre-w1` 복원(자동 롤백 대상은 `.pre-task2`) | 불필요 |
| 4 SSH 키 등록(서명 필수) | **사용자 직접**(root) | `sudo rm /etc/aigate/signers.json` | 불필요 |
| 5 패스키 활성화 | **사용자 직접**(지문 대조) | `signers remove <ID>` / `pending remove` | 불필요 |
| 6 롤백 | 승인 | — | 4번만 필요 |

## 10. W2a 컷오버 런북 (제출·취소·재시도·서버 상태 화면)
설계는 `14-DESIGN-webconsole-W2.md` 3장, 결과·한계는 `16-RESULTS-webconsole-W2a.md`. **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상 결과"는 코드와 이 저장소의 시험을 읽고 쓴 것이지 서버에서 본 출력이 아니다(W1 9장과 같은 원칙). 처음 실행할 때 다르면 그 단계에서 멈추고 롤백(단계 3)을 고려한다. 순서는 바꾸지 않는다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[되돌림 가능]** 아래 한 줄로 원상 복구, **[주의]** 되돌리려면 DB 복원이 필요하고 그 사이 기록을 잃는다. `ORCHVM`, 관리자 키, 경로는 9장과 같다(바이너리 `/usr/local/lib/aigate`, DB `/home/orch/orchd/state/orchd.db`, 소켓 `/home/orch/orchd/run/orchd.sock`).

### 10.0 시작 전에 모두 참이어야 하는 것
- **W1 컷오버가 끝난 상태가 출발점이다**(13-RESULTS 10장): 세 바이너리가 root 소유 `/usr/local/lib/aigate`에 있고 그 옆에 W1의 `*.prev-w1`이 있다. 서명 필수 모드(`/etc/aigate/signers.json`), `tailscale serve`, 유닛의 웹 플래그는 그대로 둔다. 이 컷오버는 **바이너리만** 바꾼다(유닛·정책·서명자 목록은 바꾸지 않는다).
- 실행 중이거나 대기 중인 작업이 없다: `orch list`에 `queued`·진행 중 상태가 없고 `orch approvals`가 비어 있다. 단계 1은 `orchd`를 재시작하고, 롤백은 DB를 되돌린다.
- **동작이 바뀌는 것 두 가지를 미리 안다.**
  1. **정책이 종류(class) 목록을 가지면 목록에 없는 종류의 제출은 거부된다**(`400 unknown_class`). 웹만이 아니라 **CLI `orch submit`도 같다**(같은 검증 함수). 지금 orch-vm 정책에는 `toy`, `sample-app`가 있다고 알고 있다(3장, 실서버 정책 파일은 이번에 다시 읽지 않았다). 다른 종류 이름을 쓰던 스크립트가 있으면 먼저 정책에 넣거나 이름을 바꾼다.
  2. **이미 끝난 작업을 취소하면** 코드 `already_final`(409)이 온다(전에는 `conflict`). 한 주체(세션·토큰·소켓 사용자)가 대기 중인 작업이 20개(`--queue-limit` 기본, **주체별**)이거나 서버 전체가 100개(`--queue-limit-total` 기본)면 새 제출은 `429 queue_full`(문장이 다르다: "내 대기 작업이 가득 찼습니다" / "서버 대기열이 가득 찼습니다").

### 단계 1. 백업, 세 바이너리 한꺼번에 배포, 재시작 [사용자][주의: DB 스키마가 v7로 올라간다]
**백업(전부 먼저).** orch-vm에서:
```
sudo systemctl stop orchd
B=/home/orch/orchd/backup-w2a; sudo mkdir -p $B
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /usr/local/lib/aigate/$f /usr/local/lib/aigate/$f.prev-w2a; done
sudo sh -c "cp -a /home/orch/orchd/state/orchd.db* $B/ && cp -a /home/orch/orchd/gate-home $B/gate-home && cp -a /etc/systemd/system/orchd.service /etc/systemd/system/orchd.service.pre-w2a"
ls -l /usr/local/lib/aigate $B
```
`*.prev-w1`은 지우지 않는다(W1 이전으로 가야 할 때를 위해). DB 백업은 반드시 **`orchd`를 멈춘 뒤** 뜬다(WAL 파일 `orchd.db-wal`, `-shm`까지 `orchd.db*`로 함께).

**배포.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w2a/<이름> ./cmd/<이름>`을 `orchd`, `orch`, `aigate` 세 개에 대해 하고(W1 기록상 orch-vm은 x86_64. 다르면 `uname -m`에 맞춘다), `scp`로 VM `/tmp/w2a/`에 보낸 뒤:
```
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w2a/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
```
유닛은 바꾸지 않는다(`--queue-limit`은 주체별 기본 20, `--queue-limit-total`은 서버 전체 기본 100을 쓴다. 바꾸려면 유닛에 `--queue-limit N`·`--queue-limit-total M`을 더하고 `daemon-reload` 후 재시작; 음수, 그리고 total < 주체별이면 시작 거부).

> **[주의] 롤백은 DB 복원을 동반한다.** 새 `orchd`는 첫 시작에서 DB를 스키마 **v7**로 올린다(`task_events.actor` 열 추가, 자동 마이그레이션). **옛 `orchd`(W1)는 v7 DB를 열지 않는다**(W1 단계 1과 같은 버전 검사, 코드 읽기 기준). 그래서 바이너리를 되돌릴 때는 **반드시 `$B`의 DB 백업도 되돌려야 하고, 백업 뒤에 만든 작업·이벤트·승인 대기 기록은 모두 사라진다.** 되돌릴 가능성이 있으면 단계 2의 시험 작업 외에 실제 작업을 넣기 전에 판단한다.

**검증 체크리스트**
- `journalctl -u orchd -n 40`: 오류 없이 대기, `approvals: mode signed (… signers …)`(W1과 같음), `socket uids allowed: [0 <orch uid>]`, `HTTP listener on 127.0.0.1:8808 …`.
- `orch list`: 백업 전의 작업이 모두 그대로 보인다(마지막 번호 확인).
- `orch submit`으로 toy 작업 하나를 제출(3장 명령, 종류 `toy`) → 제출자는 **웹 작업 화면의 "시간 분포" 타임라인**에서 본다: 첫 단계(대기)의 `처리:` 줄이 제출자이고 `socket:uid:<n>`이어야 한다(소켓으로 제출했으므로 `socket:uid:` + orch uid. `sudo`로 했으면 `socket:uid:0`). 워커가 옮긴 단계(준비·에이전트 등)는 `처리: 자동`으로 보이는 것이 **정상**이다(실패가 아니다). 보조 확인(gate-home은 orch 소유 0700이라 **`sudo` 필요**): `sudo grep task_submitted /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl | tail -1`의 `actor`·`via`가 같은 값. 선택(orch-vm에 `sqlite3`가 있을 때만; 설치 여부 미확인; DB도 orch 소유라 `sudo`): `sudo sqlite3 /home/orch/orchd/state/orchd.db "select actor from task_events where note='created' order by id desc limit 1"`도 같은 값. **`orch show`의 `주체:` 줄로 제출자를 확인하지 않는다.** 그 줄은 *가장 최근 이벤트*의 주체이고, 워커가 `queued`→`preparing`을 약 250 ms 안에 `system`으로 옮기므로 정상 서버에서도 `system`으로 보인다(코드 읽기 기준, 실서버 미확인). 옛 작업은 마이그레이션 전 이벤트라 `주체:` 줄이 없을 수 있다(정상).
- 새 `orchd`의 base 결과 캐시는 빌드 키가 바뀌어 첫 판정에서 한 번 다시 돈다(6장). 첫 작업이 느려도 정상.

**그래도 안 되면:** 단계 3(롤백) 전체. 바이너리만 되돌리면 옛 `orchd`가 v7 DB를 거부하고 시작하지 않는다.

### 단계 2. 웹에서 확인 [사용자]
**전제:** 단계 1 통과. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인(W1과 같음). 각 항목의 "예상"은 코드·시험(`internal/orch/web/test/*.test.js`, `internal/orch/api/*_test.go`)에서 읽은 것이다.

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 2-1 | 메뉴 "새 작업"(`#/new`) 열기 | 저장소 선택 상자에 `/home/orch/repos` 아래 저장소(`toy`, `sample-app`), 기준 커밋 칸에 그 저장소 HEAD, 종류 상자에 정책의 종류 목록 | 저장소가 비면 2-9(소유자·`safe.directory`)부터 본다. 종류가 글 상자이면 정책에 종류 목록이 없는 것(R3: 아무 종류나 받음) |
| 2-2 | toy 작업 제출: 네 칸 중 "목표"에 **구체적인 요청**을 적고(예: `calc_w2a_web_test.go 라는 새 파일을 package toy 로 만들고 Sum([]int{7,8}) == 15 를 검사하는 TestSumW2aWeb 테스트를 추가해라. 기존 파일은 수정하지 마라. 커밋해라.`) 미리보기를 확인한 뒤 "제출". **"계산기 기능 생성"처럼 모호한 한 줄은 에이전트가 커밋을 만들지 않아 `agent made no commit`으로 실패한다**(2026-11-09 실제 사례, 작업 #44) | 미리보기에 `Goal: …`, 바이트 수 표시. 제출하면 `#/tasks/<ID>`로 이동, 상태 `queued` → 진행 줄 "진행 중 · <단계> · N초 경과" | 오류 문구는 코드별 고정 문장이다(서버 원문은 보이지 않음. 예외로 `queue_full`은 서버의 고정 두 문장 중 하나를 그대로 보여 준다). `queue_full`이면 큐 확인, `unknown_class`면 정책 확인 |
| 2-3 | 그 작업 화면의 정보 칸 | "마지막 변경"에 `session:<서명자 ID>`(예: `session:mac-passkey`). 승인이 필요한 작업이면 W1 방식(웹 패스키 서명)으로 승인 → `done` | 이 시점에는 보통 이미 `system`(워커가 옮김)이라 이 칸으로 제출자를 가리지 않는다. 제출자는 **같은 화면의 시간 분포 타임라인 첫 단계(대기)의 `처리:` 줄**(`session:<ID>`)로 본다. 워커가 옮긴 단계가 `처리: 자동`인 것은 정상이다. 보조: `sudo grep task_submitted …`(원장, 단계 1). 취소 후 주체는 2-6에서 본다 |
| 2-4 | 정책에 **없는** 종류로 제출해 보기. 웹은 목록에서만 고르게 하므로 Mac CLI로: `orch --server https://<FQDN> --signer mac-ssh submit … --class nosuchclass …` | `400 unknown_class` "정책에 없는 작업 종류입니다: nosuchclass", 작업이 생기지 않음(`orch list`의 마지막 번호 그대로) | 작업이 생기면 단계 3 |
| 2-5 | 큐 상한(`queue_full`) | **컷오버에서 확인하지 않는다.** 유닛을 바꿔 상한을 낮추는 것은 이 컷오버의 범위가 아니고, 주체별 20개를 실제로 쌓는 것은 VM 시간을 쓴다. 자동 시험(`TestQueueFloodBoundedAcrossPrincipals`, `TestQueueLimitIsPerActor`, `TestQueueLimitTotalCeiling`, `TestQueueLimitRaceNeverExceedsPerActorOrTotal`)으로만 확인됨 | — |
| 2-6 | 취소: 대기 중이거나 진행 중인 toy 작업 화면에서 "작업 취소" → 확인 칸 → "취소 실행" | 확인 칸에서 아무것도 보내지 않고(Escape나 "돌아가기"로 닫힘), 실행하면 상태 `cancelled`, 정보 칸 주체 `session:<ID>`(`cancelled`는 종결 상태라 이 값이 남는다: **주체 전달을 가리는 믿을 만한 확인은 이 단계다**: 타임라인의 `cancelled` 단계 `처리:` 줄이 취소한 사람 `session:<ID>`). 이미 끝난 작업에는 취소 단추가 없다 | 409 `state_changed`가 반복되면 작업이 계속 움직이는 것: 잠시 뒤 다시 |
| 2-7 | 재시도 | **인프라 오류로 실패한 작업이 있어야만 시험할 수 있다**(복원 쿼터 413 등). 일부러 만들지 않는다. 그런 실패가 생기면 그때 "다시 시도" → 실패한 단계부터 재개되는지 본다. 그 전까지는 **"검증 못 함"** | — |
| 2-8 | 메뉴 "서버 상태"(`#/ops`) | 클라우드 칸에 볼륨 용량·개수 쿼터(사용/한도, 예약), test-vm 이름과 상태(`ACTIVE` 등 또는 "알 수 없음"; 이름은 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` 모양일 때만 보이고 아니면 빈칸이다 — `test-vm`은 보임), 복원 볼륨 수, 최근 복원 기록. 클라우드가 늦으면 "응답이 늦어 이번에는 건너뜁니다"가 한 번 보이고 다음 갱신(20초)에 수치가 나온다 | **[사용자] 수치 대조:** orch-vm에서 `orchd`와 같은 OpenStack 자격으로 `openstack quota show --usage -f json`을 실행해 `gigabytes`·`volumes` 줄의 `In Use`·`Limit`·`Reserved`가 화면과 같은지 본다. **키 이름**(`Resource`, `In Use`, `Limit`, `Reserved`)이 출력과 다르면 화면은 늘 "클라우드 상태를 지금 읽을 수 없습니다"가 된다(키 이름은 기존 시험의 고정 데이터에서 가져왔고 실제 CLI로 확인한 적 없음). 다르면 결과 문서에 실제 출력의 키를 기록하고 고치는 작업을 따로 한다(데이터 손상 위험은 없음: 읽기 전용) |
| 2-9 | `#/new`의 저장소 목록 | 기대한 저장소가 모두 나온다 | **[사용자]** orch-vm에서 `ls -ld /home/orch/repos/*`로 소유자를 본다. `/repos`는 전역·시스템 git 설정을 읽지 않으므로 **orch가 아닌 사용자 소유 저장소**(전역 `safe.directory`로만 열리던 것)는 목록에서 빠진다. 그런 저장소는 CLI 제출은 여전히 된다. 소유자를 orch로 맞추거나 그대로 둔다(사용자 결정) |

**검증 체크리스트(요약):** 2-2·2-3·2-6이 예상대로이고, 2-4가 거부되고, 2-8의 수치가 CLI와 같다. 2-5·2-7은 결과 문서에 "컷오버에서 확인 안 함"/"검증 못 함"으로 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다(쓰기 동작은 CLI로도 된다). 제출·취소가 잘못된 작업을 건드리거나, 2-6의 취소 주체(타임라인 `cancelled` 단계의 `처리:`)가 `session:<ID>`가 아니거나 첫 단계(대기)의 제출자 또는 원장 `sudo grep task_submitted …`의 주체가 틀리거나, 거부되어야 할 제출이 통과하면 단계 3.

### 단계 3. 롤백 [사용자][주의]
1. `sudo systemctl stop orchd`
2. 바이너리: `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /usr/local/lib/aigate/$f.prev-w2a /usr/local/lib/aigate/$f; done`(세 개는 항상 같이).
3. **DB 복원(필수):** 필요하면 지금 DB를 먼저 따로 복사한다(`sudo cp -a /home/orch/orchd/state/orchd.db* /home/orch/orchd/backup-w2a/after-v7/` — 폴더를 먼저 만든다). 그다음 `sudo rm -f /home/orch/orchd/state/orchd.db-wal /home/orch/orchd/state/orchd.db-shm && sudo cp -a /home/orch/orchd/backup-w2a/orchd.db* /home/orch/orchd/state/`. **백업 이후의 작업·이벤트·승인 대기는 사라진다.**
4. 승인 파일은 **되돌려야 할 때만**: `sudo cp -a /home/orch/orchd/backup-w2a/gate-home/.aigate/approvals.json /home/orch/orchd/gate-home/.aigate/approvals.json`(경로는 `gate.ApprovalsPath`). 끝나면 `ls -l /home/orch/orchd/gate-home/.aigate/approvals.json`로 소유자·모드가 그대로인지 본다(`-a`가 보존하지만 확인한다). 백업에 이 파일이 없으면 W2a 이전에 파일이 없던 것이므로 복원할 것이 없다. 보통은 그대로 둔다(그 사이의 서명 승인 기록이 남는 것은 무해). **`gate-home` 전체를 되돌리지 않는다**: 원장(`.aigate/ledger/`)이 백업 시점으로 돌아가 W2a 기간의 줄이 사라진다.
   - **DB 복원은 `sessions`·`tokens`·멱등 키 표도 백업 시점으로 되돌린다**(W1과 같다). 그래서 롤백 뒤 브라우저의 로그인이 모두 무효가 되고 다시 로그인해야 한다.
5. 유닛(바꾸지 않았다면 생략 가능): `sudo cp /etc/systemd/system/orchd.service.pre-w2a /etc/systemd/system/orchd.service && sudo systemctl daemon-reload`.
6. `sudo systemctl start orchd`.

> **[주의] 원장의 작업 번호가 겹친다.** DB를 되돌리면 작업 번호가 백업 시점부터 다시 쓰인다. 원장 `orchd.jsonl`(`/home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl`, 코드 `policy.OrchLedgerPath`)에는 W2a 동안의 작업이 남긴 `task_submitted`·`task_transition` 줄이 그대로 남으므로, **같은 작업 번호가 원장에 두 번(다른 작업으로) 나타날 수 있다.** 원장 줄은 지우지 않는다(해시 사슬이 깨진다). 롤백 시각을 결과 문서에 적어 두어, 그 시각 이전의 그 번호는 W2a 시기의 다른 작업이라는 것을 구분한다.

**검증:** `journalctl -u orchd`에 스키마 오류가 없고, `orch list`가 단계 1 이전의 작업 목록과 같다(W2a 시험 작업은 없음). 웹에는 W1 화면만 보인다(메뉴 "새 작업"·"서버 상태" 없음; 브라우저 캐시면 새로고침). **그래도 안 되면:** 아무것도 더 지우지 말고 멈춰서 상태(`journalctl -u orchd -n 80`, `ls -l /usr/local/lib/aigate /home/orch/orchd/state /home/orch/orchd/backup-w2a`)를 사용자에게 보고한다. W1 이전까지 가야 하면 9장 단계 6(`*.prev-w1`, `backup-w1`).

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 1 백업·바이너리 3개 | 승인 | **[주의]** 바이너리 + DB 복원(스키마 v7, 그 사이 기록 잃음) | 필요(stop/start) |
| 2 웹 확인 | **사용자 직접**(로그인, 제출, 취소, 쿼터 수치·저장소 소유자 대조) | 시험 작업은 남겨 둬도 무해(취소 가능) | 불필요 |
| 2-5 큐 상한 | — | 컷오버에서 확인 안 함(자동 시험만) | — |
| 2-7 재시도 | — | 인프라 실패가 생길 때까지 검증 못 함 | — |
| 3 롤백 | 승인 | — | 필요 |

## 11. W2b 컷오버 런북 (AI 계획서·병합 요청, `orch-broker`)
설계는 `14-DESIGN-webconsole-W2.md` 4장, 결과·시험·한계는 `18-RESULTS-webconsole-W2b.md`. **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다. 실제 OpenAI와 GitLab의 동작은 컷오버 전까지 전부 미확인이다**(18번 문서 5장). 아래 "예상"은 코드와 이 저장소의 시험(가짜 OpenAI·GitLab)을 읽고 쓴 것이지 서버에서 본 출력이 아니다(W1 9장·W2a 10장과 같은 원칙). 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려한다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[되돌림 가능]** 아래 한 줄로 원상 복구, **[주의]** 되돌리려면 DB 복원이 필요하고 그 사이 기록을 잃는다. 경로는 W2a와 같다(바이너리 `/usr/local/lib/aigate`, DB `/home/orch/orchd/state/orchd.db`, 소켓 `/home/orch/orchd/run/orchd.sock`). 새로 생기는 것: 계정 `orch-broker`, `/etc/orch-broker`, `/var/lib/orch-broker`, 바이너리 `/usr/local/bin/orch-broker`(저장소의 유닛 `infra/systemd/orch-broker.service`가 이 경로를 쓴다), 소켓 `/run/orch-broker/broker.sock`.

### 11.0 시작 전에 모두 참이어야 하는 것
- **W2a 컷오버가 끝난 상태가 출발점이다**(16번 문서 7장): `/usr/local/lib/aigate`에 세 바이너리와 `*.prev-w2a`, DB 스키마 v7, 서명 필수 모드. 실행 중이거나 대기 중인 작업이 없다(`orch list`, `orch approvals`). 단계 2는 `orchd`를 재시작한다.
- 그룹 `orch`가 있다: `getent group orch`. **`broker-init.sh`는 이 그룹이 있는지 확인하지 않는다.** 없으면 브로커가 소켓 그룹을 정하지 못해 시작하지 못한다(유닛이 `status=216/GROUP`으로 실패하거나 `socket_group을 확인할 수 없습니다`). `orchd`가 실행되는 계정 이름도 `orch`이고 uid는 `id orch`로 확인한다(W2a 기록상 1000).
- **사용자 사전 작업 (제가 대신 할 수 없습니다. 키·토큰 값은 이 대화나 어떤 파일·셸 기록에도 붙여 넣지 않는다):**
  - **(a) OpenAI에서 이 용도 전용 프로젝트와 키를 새로 만든다.** 그 프로젝트의 **지출 한도를 주당 25달러에 맞춰** 건다. 브로커의 한도(`weekly_budget_usd` 25)에 더한 두 번째 방어선이다. 다른 용도와 키를 공유하지 않는다.
  - **(b) 사용할 모델 ID와 그 모델의 현재 단가**(입력·출력 각각 100만 토큰당 USD)를 정해 둔다. 브로커 설정에 적는다. 값이 없거나 0 이하이면 브로커는 시작하지 않는다(코드가 값을 추측하지 않는다). `weekly_budget_usd`는 25, 시간당 한도는 기본값(AI 계획서 20회, MR 30회)을 쓴다. **모델 ID와 단가가 실제와 맞는지는 이 저장소에서 확인할 수 없다. `orch-broker -check`도 모델 이름을 검증하지 않는다**(형식만 본다. 틀린 이름은 첫 실제 호출에서 `upstream_error`로 드러난다). **추론(reasoning) 모델 주의:** 추론 모델은 `max_output_tokens`(기본 4096)를 생각하는 데 다 써 버리고 답 없이 `incomplete`로 끝날 수 있다. 그러면 **모든 호출이 `upstream_error`인데 비용은 청구된다**(최악 예약이 지출로 남는다). 추론하지 않는 모델을 고르거나, `broker.json`의 `max_output_tokens`를 올린다(올리면 호출당 최악 예약 비용도 커진다).
  - **(c) AI로 코드를 보낼 저장소.** 사용자가 **어떤 저장소든 가능**하다고 결정했으므로(2026-11-09) `toy`와 `sample-app`의 경로를 `context_repos`에 넣는다(`/home/orch/repos/toy`, `/home/orch/repos/sample-app`). 이 목록은 root 소유 설정이 정하는 것이므로 `orch` 계정은 넓힐 수 없다.
  - **(d) GitLab 프로젝트 액세스 토큰**(역할 Developer, 범위 `api`, **만료일 지정**). 사용자가 **이 단계에 도달해 요청받으면 만들어 주기로** 했다(2026-11-09). 그때 대상 프로젝트의 **`main`이 보호되어 있는지**(직접 push 금지, Maintainer만 병합)도 함께 확인한다. **병합 요청을 이번 컷오버에 켤지 먼저 정한다**(아래 "알아둘 것" 2). 처음에는 AI만 켜고 MR은 나중에 켜는 것을 권한다.
  - **(e) 키·토큰 값은 사용자가 직접 서버에 둔다.** 방법은 단계 1-5(값이 명령줄·화면·셸 기록에 남지 않게 입력한다).

**알아둘 것 (코드 읽기와 시험 기준, 실서버 미확인).**
1. **키가 틀리면 실패한 호출마다 최악 비용 예약이 지출로 남는다.** 401이어도 그렇다(브로커는 응답의 사용량을 못 받으면 예약값을 그대로 지출로 남긴다). 한도는 **시간당 20회와 주당 25달러**이므로 잘못 설정해도 25달러 안에서 멈추지만, 그 예산이 헛되이 소진된다. 컷오버 중 `sudo cat /var/lib/orch-broker/budget.json`(금액과 시각만 있고 비밀은 없다)으로 소진을 본다. 7일 롤링이라 일부러 비우려면 이 파일을 지우지 말고(깨진 파일은 "예산 소진"으로 취급) 브로커를 멈춘 뒤 사용자가 판단한다.
2. **MR 켜기/끄기는 브로커 설정의 `gitlab.projects`다.** orchd에는 따로 켜는 플래그가 없다. 저장소가 `gitlab.projects`에 없으면 그 저장소의 모든 완료 작업은 `mr_state=skipped`("이 저장소는 병합 요청을 만들지 않습니다")가 된다. **AI만 쓰는 첫 컷오버라면 설정에서 `gitlab` 절 전체를 뺀다.**
3. **MR 관련 동작 세 가지.** (i) MR 훅의 제한시간은 25초(브로커의 `create_mr` 한도 20초보다 길다). (ii) 실패 코드가 `bad_url`인 작업은 서버가 재시도를 거부한다(409 `mr_not_applicable`): MR은 이미 GitLab에 있고 같은 답이 돌아올 뿐이므로 브로커를 부르지 않는다. (iii) 요청 글이 MR 설명의 요약이 되므로 브로커와 orchd 둘 다 요약·제목 힌트에 비밀 모양 검사를 해서, 걸리면 `mr_state=failed`, 코드 `looks_like_secret`(GitLab으로 아무것도 가지 않는다)이 된다.
   **충돌 창.** 작업이 `done`이 된 뒤 MR 훅이 돌기 전에 orchd가 죽으면 그 작업의 `mr_state`는 비어 있고 재시도 단추도 없다(재시도는 `failed`·`skipped`만 허용, 의도). 또 **MR이 닫히거나 병합된 뒤 재시도하면 같은 브랜치로 두 번째 MR이 만들어질 수 있다**(브로커는 *열린* MR만 찾아 재사용한다).
4. **저장된 작업 요청 글(상한을 두고 정리한 것)이 MR 설명의 요약이 된다.** 요청 글에 비밀을 붙여 넣었다면 GitLab으로 간다. 파일 내용·diff는 MR에 들어가지 않는다. 요구사항 글에는 비밀 값 모양 검사가 있으나 보조 장치일 뿐이다.
5. **유닛의 `SupplementaryGroups=orch`는 보안상의 타협이다.** 브로커는 `orch-broker` 계정으로 도는데 소켓의 그룹을 `orch`로 바꿔야 하므로(그래야 `orchd`가 접속한다) 서비스 프로세스가 `orch` 그룹의 구성원이 된다. 그 결과 **`orch` 그룹이 읽을 수 있는 파일은 브로커 프로세스도 읽을 수 있다**(브로커의 샌드박스 `ProtectHome=yes` 등이 일부 막지만 이 효과를 시험한 적은 없다). 대안은 systemd 소켓 활성화(`orch-broker.socket`의 `SocketGroup=orch`)로 브로커가 `orch` 그룹이 되지 않게 하는 것인데 **구현하지 않았다**(브로커가 넘겨받은 소켓을 쓰도록 바꿔야 함). 반대로 `orch`를 `orch-broker` 그룹에 넣는 방법은 쓰지 않는다: `/etc/orch-broker`가 `root:orch-broker 0750`이라 `orch`가 설정 폴더를 읽게 된다.
6. **스키마가 v8로 올라가고 내리는 마이그레이션이 없다.** 옛 `orchd`는 v8 DB를 열지 않는다. 그래서 **DB 복사를 `orchd` 배포 전에 한다(단계 2의 첫 일).** 롤백은 "DB 복원 + 옛 바이너리"이고, **컷오버 뒤에 만든 작업·이벤트·승인 대기는 사라진다.**
7. **한도.** (AI 계획서 요청의 슬롯은 브라우저가 중간에 취소해도 돌려주지 않는다: 요청이 브로커에 완전히 써진 뒤에는 브로커가 일을 계속하고 결과는 원장에 기록되며, 슬롯은 브로커에 닿기 전에 실패했을 때(브로커 없음, 연결 실패)와 브로커가 일하기 전에 거절했을 때(`busy`, `forbidden`, `too_large`)만 돌려준다.) `orchd`가 받는 AI로 보낼 내용의 상한은 **240 KiB**(브로커의 256 KiB는 지시문·스키마를 합친 값). 주체(사람·세션·토큰)별로 **미리보기 시간당 30회, AI 계획서 요청 시간당 5회**. 브로커 전체로 **AI 계획서 시간당 20회, MR 시간당 30회**, 주 25달러. 초과하면 고정 문장의 429가 온다(웹 화면은 429를 자동으로 다시 보내지 않는다). **미리보기는 브로커가 세는 방식 그대로(지시문 + 파일별 표지(경로가 두 번) + 요구사항 + 스키마, 그리고 인코딩된 요청 크기)로 크기를 재서, 입력 262144바이트·요청 480 KiB 안에 들 때까지 우선순위가 낮은 파일부터 빼고 "전체 크기 한도 때문에 제외"로 보여 준다.** 그래서 미리보기가 "허용"이면 브로커가 크기 때문에 거절하지 않는다(설정의 `max_input_bytes`를 기본값보다 낮추면 이 보장이 깨진다: 그때는 `too_large`가 오고 슬롯은 돌려준다).
8. **미리보기 화면의 제한시간은 25초**인데 큰 저장소에서 서버가 일하는 최악 시간은 약 38초일 수 있다. 느린 저장소에서는 화면이 시간 초과를 먼저 보일 수 있다. 미리보기는 비용이 없고 읽기 전용이라 **다시 시도해도 안전하다**. **선택은 orchd 메모리에 10분만 산다.** 미리보기 뒤 10분이 지나거나 `orchd`가 재시작되면 "미리보기가 만료되었습니다"가 나오고 미리보기부터 다시 한다. 화면에서 작성 중이던 체크리스트는 브라우저 메모리에만 있어 새로 고침하면 사라진다.
9. **계획서 화면의 키보드만 사용, 스크린 리더 사용은 시험하지 않았다.** 이 컷오버에서도 확인 항목이 아니다.
10. **실제 OpenAI·GitLab의 동작은 컷오버 전까지 전부 미확인이다**: 응답 형식, 구조화 출력, 모델 ID·단가, 토큰 사용량, 키 한도, `store: false`, GitLab의 http 접속·`main` 보호·토큰 권한, MR의 Draft 표시. 처음 한 번 성공하기 전에는 아무것도 "된다"고 말하지 않는다.

### 단계 1. 브로커 설치 [사용자][되돌림 가능: 단계 4의 A]
`orchd`와 DB는 건드리지 않는다. 먼저 **파일 백업**(서비스 영향 없음). orch-vm에서:
```
B=/home/orch/orchd/backup-w2b; sudo mkdir -p $B
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /usr/local/lib/aigate/$f /usr/local/lib/aigate/$f.prev-w2b; done
sudo cp -a /etc/systemd/system/orchd.service /etc/systemd/system/orchd.service.pre-w2b
sudo nft list ruleset > /tmp/ruleset.before-w2b 2>/dev/null; ls -l /usr/local/lib/aigate $B
```

**1-1. 계정과 폴더.** 저장소의 `infra/broker-init.sh`를 VM에 올린 뒤(`scp`) 먼저 계획만 본다:
```
sh broker-init.sh            # 드라이런: 아무것도 바꾸지 않는다
sudo sh broker-init.sh --apply
```
예상: 계정 `orch-broker`(로그인 없음, 홈 없음), `/etc/orch-broker`(root:orch-broker 0750), `/var/lib/orch-broker`(orch-broker 0700). **키·토큰 파일은 만들지 않는다.**

**1-2. 바이너리.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w2b/orch-broker ./cmd/orch-broker`(orch-vm이 x86_64가 아니면 `uname -m`에 맞춘다), `scp`로 VM `/tmp/w2b/`에 보낸 뒤 `sudo install -o root -g root -m 0755 /tmp/w2b/orch-broker /usr/local/bin/orch-broker`. 유닛: `sudo install -o root -g root -m 0644 orch-broker.service /etc/systemd/system/orch-broker.service && sudo systemctl daemon-reload`(**아직 시작하지 않는다**).

**1-3. 설정 `/etc/orch-broker/broker.json`** (root 소유, 쓰기 금지; 꺾쇠 값은 사전 작업에서 정한 값으로 바꾼다. JSON에는 주석을 쓸 수 없다). 알 수 없는 키는 거부된다.
```
{
  "version": 1,
  "socket": "/run/orch-broker/broker.sock",
  "socket_group": "orch",
  "allow_uids": ["orch", "root"],
  "openai": {
    "model": "<모델 ID>",
    "key_file": "/etc/orch-broker/openai.key",
    "max_output_tokens": 4096,
    "max_input_bytes": 262144,
    "pricing": { "input_usd_per_mtok": <입력 단가>, "output_usd_per_mtok": <출력 단가> }
  },
  "limits": { "refine_per_hour": 20, "create_mr_per_hour": 30, "weekly_budget_usd": 25 },
  "context_repos": ["/home/orch/repos/toy", "/home/orch/repos/sample-app"]
}
```
**AI만 쓰는 첫 컷오버는 위 그대로 둔다(`gitlab` 절 없음 → 모든 작업이 `skipped`).** MR까지 켤 때만 마지막 줄 뒤에 `,` 와 아래 절을 더한다(방화벽 `infra/nftables/orchestrator.nft`가 허용하는 GitLab 쪽은 `100.64.0.10`의 22·18082뿐이다. 그래서 `http://100.64.0.10:18082`가 기준 주소일 것으로 **추정**하지만 실제 주소·포트와 http/https 여부는 사용자가 확인한다):
```
  "gitlab": {
    "base_url": "http://<GitLab 주소>:<포트>",
    "token_file": "/etc/orch-broker/gitlab.token",
    "projects": { "/home/orch/repos/toy": "<namespace>/<project>", "/home/orch/repos/sample-app": "<namespace>/<project>" },
    "target_branch": "main"
  }
```
`sudo install -m 0644 -o root -g root broker.json /etc/orch-broker/broker.json`. 브로커는 설정 파일과 그 상위 폴더 전부가 root 소유이고 그룹·기타 쓰기가 없어야 시작한다. `context_repos`는 정리된 절대 경로만 받는다.

**1-4. 키 파일 둘 곳 확인.** 아직 파일이 없어야 한다: `sudo ls -l /etc/orch-broker`(`broker.json`만).

**1-5. 키와 토큰 놓기 [사용자 직접].** 값이 명령줄·화면·셸 기록에 남지 않게 **프롬프트로 입력**한다(붙여 넣은 뒤 Enter, 입력은 화면에 보이지 않는다). VM에서 bash로:
```
sudo bash -c 'read -rs -p "OpenAI 키: " K; echo; printf "%s" "$K" | install -m 0400 -o orch-broker -g orch-broker /dev/stdin /etc/orch-broker/openai.key; unset K'
```
MR을 켤 때만 같은 방식으로 `/etc/orch-broker/gitlab.token`. (`install`이 `/dev/stdin`을 받는지는 이 저장소에서 확인하지 못했다. 안 되면 `sudo bash -c 'umask 077; read -rs -p "키: " K; echo; printf "%s" "$K" > /etc/orch-broker/openai.key; unset K'` 뒤 `sudo chown orch-broker:orch-broker /etc/orch-broker/openai.key && sudo chmod 0400 /etc/orch-broker/openai.key`.) **`cat`·`echo`로 값을 보지 않는다.** 확인은 모드·소유자·크기만: `sudo ls -l /etc/orch-broker`(`-r-------- orch-broker orch-broker`), 키 파일 크기가 0이 아님. 줄 끝 개행은 브로커가 잘라 낸다. 이 대화·터미널 캡처·문서에 값을 붙이지 않는다.

**1-6. 점검 `orch-broker -check`.** 설정의 형식·신뢰(root 소유, 쓰기 금지, 상위 폴더 사슬), 키·토큰 파일의 소유자·모드(`orch-broker` 소유, `0400`/`0600`), 소켓 그룹, 상태 폴더만 확인한다. **외부 접속을 하지 않고 아무것도 쓰지 않는다**(키가 맞는지는 알 수 없다). 키 파일 소유자는 실행 계정으로 검사하므로 `orch-broker`로 실행한다:
```
sudo -u orch-broker /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…외부 접속은 하지 않았습니다)`와 종료 코드 0. 실패하면 고정 문장 하나(값은 인용하지 않음)가 나온다. 실제 키로 한 번 호출해 보는 `--live-check`는 **구현하지 않았다**(그래서 이 런북에 없다). 첫 실제 호출은 단계 3에서 웹으로 하며 토큰이 조금 든다.

**1-7. 송신 경로 확인 (키 없이).** orch-vm에서:
```
curl -sS -o /dev/null -w '%{http_code}\n' https://api.openai.com/v1/models
```
**`401`이 나오면 통과**(도달함, 인증 없음). 저장소의 `infra/nftables/orchestrator.nft`는 공인 주소로의 80·443과 `100.64.0.10`의 22·18082를 이미 허용하므로 **방화벽은 바꾸지 않는다.** 서버에 올라간 실제 규칙이 저장소와 같은지는 이 확인으로 본다(타임아웃이나 연결 거부면 단계 1에서 멈추고 `sudo nft list ruleset`을 사용자에게 보고한다. 방화벽을 임의로 고치지 않는다). MR을 켠다면 GitLab 쪽도 `curl -sS -o /dev/null -w '%{http_code}\n' http://<GitLab 주소>:<포트>/`로 도달만 확인한다(토큰 없이, 응답 코드는 무엇이든 연결되면 통과).

**1-8. 시작과 확인.**
```
sudo systemctl enable --now orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
ls -l /run/orch-broker/broker.sock
sudo -u orch test -r /etc/orch-broker/openai.key && echo READABLE || echo denied
sudo -u orch test -x /etc/orch-broker && echo ENTERABLE || echo denied
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"status\"}\n");print(s.makefile().readline())'
```
예상: 유닛 `active (running)`, 로그에 오류 없음, 소켓 `srw-rw---- orch-broker orch`. **`orch` 계정의 두 검사는 모두 `denied`여야 한다(`READABLE`·`ENTERABLE`이면 멈추고 보고. 값은 화면에 찍히지 않는다)**(`orch` 계정이 `/etc/orch-broker`를 읽지 못함. **이 저장소에서는 확인하지 못한 항목**). 마지막 줄(`orch` 계정이 소켓으로 `status` 호출, 리눅스 `SO_PEERCRED` 확인)은 `{"ok":true,"result":{"context_repos":[…],…,"weekly_budget_remaining_usd":25…}}` 형태이고 **응답에 키·토큰이 없어야** 한다. 가능하면 `systemd-analyze security orch-broker`로 하드닝 점수를 본다(유닛의 하드닝이 실제로 시작되는지도 이 저장소에서는 확인하지 못했다. 시작이 `status=218/CAPABILITIES`, `226/NAMESPACE`, `31/SYSTEMCALL` 등으로 실패하면 이 단계에서 멈추고 단계 4의 A, 로그를 사용자에게 보고한다. 유닛의 보호 옵션을 임의로 빼지 않는다).

**검증 체크리스트:** 1-6 통과, 1-7이 401, 1-8의 유닛 active·소켓 모드·`orch`의 읽기 거부·status 호출 성공. **그래도 안 되면:** `sudo systemctl disable --now orch-broker`, 단계 4의 A. 다른 서비스에는 영향이 없다(이 단계는 `orchd`를 건드리지 않았다).

### 단계 2. `orchd`에 `--broker-socket` 추가·배포 [사용자][주의: DB 스키마가 v8로 올라간다]
> **먼저 경고: 롤백은 DB 복원을 동반한다.** 새 `orchd`는 첫 시작에서 DB를 스키마 **v8**로 올린다(`tasks`에 `branch`, `mr_iid`, `mr_url`, `mr_state`, `mr_code` 열 추가). **옛 `orchd`(W2a)는 v8 DB를 열지 않는다**(버전 검사, 코드 읽기 기준). 바이너리를 되돌리려면 반드시 아래 백업의 DB로 되돌려야 하고, **백업 뒤에 만든 작업·이벤트·승인 대기·로그인 세션은 모두 사라진다.** 시간·기록 손실을 감수할 수 있을 때만 진행한다.

**백업(배포 전에, 전부 먼저).** `orchd`를 멈춘 뒤 복사한다(WAL까지 `orchd.db*`로):
```
sudo systemctl stop orchd
B=/home/orch/orchd/backup-w2b
sudo sh -c "cp -a /home/orch/orchd/state/orchd.db* $B/ && cp -a /home/orch/orchd/gate-home $B/gate-home"
ls -l $B
```
`*.prev-w2a`, `backup-w2a`는 지우지 않는다(W2a 이전으로 가야 할 때를 위해).

**배포.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w2b/<이름> ./cmd/<이름>`을 `orchd`, `orch`, `aigate` 세 개에 대해 하고 `scp`로 보낸 뒤:
```
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w2b/$f /usr/local/lib/aigate/$f; done
```
**유닛에 플래그 더하기.** 현재 `ExecStart` 줄은 서버마다 다르므로 먼저 `systemctl cat orchd`로 그대로 복사한다. 그 줄 **끝에** `--broker-socket /run/orch-broker/broker.sock`만 더한다(다른 플래그는 바꾸지 않는다). `sudo systemctl edit orchd`에서:
```
[Service]
ExecStart=
ExecStart=<systemctl cat orchd 의 기존 ExecStart 줄 전체> --broker-socket /run/orch-broker/broker.sock
```
그다음 `sudo systemctl daemon-reload && sudo systemctl start orchd`. 유닛에 `PrivateTmp`·`ProtectSystem`·`RestrictAddressFamilies`·`InaccessiblePaths` 같은 보호 옵션이 있으면 `/run/orch-broker`가 안 보이거나 유닉스 소켓이 막힐 수 있다(실제 유닛은 이번에 다시 읽지 않았다). 그러면 웹의 미리보기가 `broker_unavailable`이 되므로 단계 3-1에서 드러난다. `--broker-socket`을 비우면 AI와 MR만 꺼지고 나머지는 그대로다.

**검증 체크리스트**
- `journalctl -u orchd -n 40`: 오류 없이 대기, `approvals: mode signed`(W2a와 같음), 스키마 오류 없음.
- `orch list`: 백업 전의 작업이 모두 그대로, 마지막 번호가 같다. 새 `orchd`의 base 결과 캐시는 빌드 키가 바뀌어 첫 판정에서 한 번 다시 돈다(첫 작업이 느려도 정상).
- (선택, orch-vm에 `sqlite3`가 있을 때만; 설치 여부 미확인; DB는 orch 소유라 `sudo`) `sudo sqlite3 /home/orch/orchd/state/orchd.db "select max(version) from schema_version"`가 8. 없으면 로그와 `orch list`로 대신한다.

**그래도 안 되면:** 단계 4의 B. 바이너리만 되돌리면 옛 `orchd`가 v8 DB를 거부하고 시작하지 않는다.

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/*.test.js`, `internal/orch/api/*_test.go`, `internal/broker/*_test.go`)에서 읽은 것이다. **이 단계의 첫 실제 AI 호출이 처음으로 OpenAI와 닿는 순간이다. 토큰이 조금 든다**(입력은 선택된 파일들, 한도 주 25달러).

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | 메뉴 "새 작업" → "AI로 계획서 만들기". 저장소 `toy`, **구체적인 한 줄**(예: 단계 2-2의 계산기 예시)을 적고 "미리보기" | 보낼 파일 목록(경로·크기), 제외된 파일과 고정 사유, 총 바이트, "이 내용을 AI로 보냅니다" 확인 단추. **아직 아무것도 OpenAI로 가지 않았다** | `broker_unavailable`: 단계 1-8·2의 소켓·유닛 보호 옵션. "허용되어 있지 않습니다": `context_repos`에 그 경로(정리된 절대 경로)가 있는지 |
| 3-2 | 목록을 읽고 확인 → 로딩 → 계획서 | 요약, 체크된 항목 목록(제목·설명·파일), 질문, 범위 밖, 의견 칸. 실제 호출이므로 수~수십 초 | `upstream_error`·`bad_plan`: 모델 ID·단가·키, OpenAI 대시보드의 사용량, `sudo journalctl -u orch-broker -n 50`(고정 코드만 남는다). **실패해도 예약 비용이 예산에 남는다**(알아둘 것 1): `sudo cat /var/lib/orch-broker/budget.json` |
| 3-3 | 항목 하나의 체크를 풀고 "확정" | 아래 최종 요청 글이 읽기 전용으로 보인다. 첫 줄 `Goal: <입력한 한 줄>`, **체크 해제한 항목의 제목이 `Do not (the user excluded these; do not implement them):` 아래**에, 체크한 항목은 `Requirements (implement ALL of these):` 아래에 있다. **최종 문장에서 직접 확인한다** | 해제한 항목이 `Requirements`에 있으면 **제출하지 않고** 멈춘다(심각) |
| 3-4 | 종류·기준 커밋 확인 후 제출 | `#/tasks/<ID>`로 이동, 작업이 약 80초 안팎에 `done`(W2a 기록상 toy). 작업 요청 글이 3-3의 최종 문장과 같다 | 에이전트가 커밋을 만들지 않아 실패하면 요청이 모호한 것(W2a 7장 #44) |
| 3-5 | 원장·예산 확인 `sudo grep '"refine"' /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl \| tail -2`, `sudo cat /var/lib/orch-broker/budget.json` | 원장 줄에 행위자·저장소·파일 수·바이트·토큰 수·`ok`만 있고 **글 내용이 없다**. `budget.json`에는 금액과 시각만 있다 | 내용이 보이면 멈추고 보고 |
| 3-6 | 허용 목록 밖 저장소 | `context_repos`에 없고 허용 루트 안에 있는 저장소가 있으면 그것으로 미리보기 → "이 저장소는 AI 전송이 허용되어 있지 않습니다…아무것도 전송되지 않았습니다", 확인 단추 없음. **없으면 컷오버에서 검증 못 함**(자동 시험만) | — |
| 3-7 | **브로커를 멈춘 상태:** `sudo systemctl stop orch-broker` → 새 미리보기를 해 본다 | 고정 오류 문장(브로커에 연결할 수 없음). 제출·취소·승인 등 기존 기능은 그대로 동작한다(작은 toy 작업 하나를 제출했다 취소해 본다). 끝나면 `sudo systemctl start orch-broker` | 기존 기능까지 막히면 단계 4의 A와 보고 |
| 3-8 | **MR(선택, 사용자가 결정).** MR을 켠 경우에만: 작업이 `done`이 된 뒤 작업 화면의 "병합 요청" 구역 | `GitLab에서 병합 요청 !N 보기` 링크. GitLab에서 **Draft** 표시, 제목 `Draft: orch task <번호>: …`, 원본 브랜치 `orch/<번호>`, 대상 `main`, 설명은 고정 양식(요청 글 요약·판정·head). **병합하지 않는다.** **이것은 실제 회사 GitLab에 병합 요청을 만드는 일이라 시험할지 사용자가 정한다.** 시험했다면 끝난 뒤 MR을 닫고 브랜치를 지울지는 사용자가 정한다 | 실패 구역의 고정 문장과 코드: `repo_not_mapped`(설정의 `projects` 경로), `upstream_error`·`bad_url`(GitLab 주소·토큰·권한. `bad_url`은 다시 시도해도 같은 결과). `main`이 보호되어 있지 않으면 **멈추고 사용자에게 알린다** |
| 3-9 | `gitlab` 절을 뺀 구성이면 | 완료된 작업의 병합 요청 구역에 "이 저장소는 병합 요청을 만들지 않습니다"(`skipped`) | — |
| 3-10 | 한도·예산 초과 | **컷오버에서 확인하지 않는다**(실제로 25달러를 쓰거나 시간당 20회를 채우는 것은 비용·시간이 든다). 자동 시험으로만 확인됨 | — |

**검증 체크리스트(요약):** 3-1~3-5가 예상대로이고 3-3의 해제 항목이 `Do not`에 있으며, 3-7에서 기존 기능이 살아 있다. 3-6·3-8·3-10은 결과 문서에 "컷오버에서 확인 안 함/검증 못 함"으로 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다(제출은 CLI로도 된다). 체크 해제 항목이 요청에 `Requirements`로 들어가거나, 기대하지 않은 저장소의 코드가 나가거나, 원장·로그·화면에 키·토큰·글 내용이 보이거나, 예산이 설명 없이 빨리 줄면 **AI를 끄고(단계 4의 A) 사용자에게 보고한다.** OpenAI 키는 대시보드에서 사용자가 폐기한다.

### 단계 4. 롤백 [사용자][주의]
**A. AI·MR만 끄기(DB 복원 없음, 스키마는 v8로 남는다: 새 `orchd`는 그대로 쓴다).**
1. `sudo systemctl edit orchd`에서 `--broker-socket …`만 뺀다(또는 `sudo rm /etc/systemd/system/orchd.service.d/override.conf`), `sudo systemctl daemon-reload && sudo systemctl restart orchd`. 미리보기·계획서는 503이 되고 MR은 모든 완료 작업이 `skipped`다. 나머지는 W2a와 같다.
2. `sudo systemctl disable --now orch-broker`. 소켓·예산 파일은 남는다(`/var/lib/orch-broker`).
3. 키 정리 **[사용자]**: `sudo shred -u /etc/orch-broker/openai.key /etc/orch-broker/gitlab.token`(없는 파일은 건너뜀)과 OpenAI 대시보드에서 키 폐기, GitLab에서 토큰 폐기(사용자가 직접). 비밀이 한 번이라도 의심되면 폐기가 먼저다.

**B. W2a 상태로 완전히 되돌리기 [주의: DB 복원, 그 사이 기록을 잃는다].**
1. `sudo systemctl stop orchd`
2. 바이너리: `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /usr/local/lib/aigate/$f.prev-w2b /usr/local/lib/aigate/$f; done`(세 개는 항상 같이).
3. **DB 복원(필수):** 필요하면 지금 DB를 먼저 따로 복사한다(`sudo mkdir -p /home/orch/orchd/backup-w2b/after-v8 && sudo cp -a /home/orch/orchd/state/orchd.db* /home/orch/orchd/backup-w2b/after-v8/`). 그다음 `sudo rm -f /home/orch/orchd/state/orchd.db-wal /home/orch/orchd/state/orchd.db-shm && sudo cp -a /home/orch/orchd/backup-w2b/orchd.db* /home/orch/orchd/state/`. **백업 이후의 작업·이벤트·승인 대기·세션(로그인)은 사라진다.** 만들어진 MR은 GitLab에 남는다(사용자가 정리).
4. 유닛: `sudo cp /etc/systemd/system/orchd.service.pre-w2b /etc/systemd/system/orchd.service`(드롭인을 썼으면 그 파일도 지운다) `&& sudo systemctl daemon-reload`.
5. `sudo systemctl start orchd`. 브로커는 위 A의 2·3으로 끈다.
   - **[사용자] 백업 뒤에 만들어진 MR과 브랜치를 정리한다.** DB를 되돌리면 작업 번호가 백업 시점부터 다시 쓰이므로, 되돌리기 전에 만들어진 GitLab의 `orch/<번호>` 브랜치와 그 열린 MR이 **앞으로 같은 번호로 만들어질 새 작업의 브랜치와 겹친다**(브로커가 같은 브랜치의 열린 MR을 "이미 있음"으로 돌려주므로 새 작업이 낡은 MR을 자기 것으로 보여 줄 수 있다). 컷오버 뒤 `done`이 된 작업 번호를 되돌리기 전에 적어 두고(`orch list`), 그 번호의 MR을 닫고 `orch/<번호>` 브랜치를 지운다.
6. `gate-home` 전체를 되돌리지 않는다(원장이 백업 시점으로 돌아간다). 같은 작업 번호가 원장에 두 번 나타날 수 있으니(W2a 롤백 주의와 같다) 롤백 시각을 결과 문서에 적는다.

**검증:** `journalctl -u orchd`에 스키마 오류가 없고, `orch list`가 단계 2 이전의 작업 목록과 같다. 웹에는 "AI로 계획서 만들기" 링크가 없다(브라우저 캐시면 새로고침). **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orchd -n 80`, `ls -l /usr/local/lib/aigate /home/orch/orchd/state /home/orch/orchd/backup-w2b`를 사용자에게 보고한다. W2a 이전까지 가야 하면 10장 단계 3(`*.prev-w2a`, `backup-w2a`).

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 0 사전 작업(OpenAI 전용 프로젝트·키·한도, 모델·단가, GitLab 토큰) | **사용자 직접**(비밀은 대화에 붙이지 않음) | 키·토큰 폐기(사용자) | — |
| 1 브로커 설치·점검 | 승인 + **키 입력은 사용자 직접** | A(브로커 끄기, 키 폐기) | 브로커만 |
| 2 `orchd` 배포(`--broker-socket`) | 승인 | **[주의]** B(바이너리 + DB 복원, 스키마 v8, 그 사이 기록 잃음). `--broker-socket`만 빼는 A는 DB 복원 불필요 | 필요(stop/start) |
| 3 웹 확인 | **사용자 직접**(로그인, 제출, 체크 해제 문장 확인) | 시험 작업은 남겨 둬도 무해. MR은 사용자가 닫는다 | 불필요(3-7만 브로커 재시작) |
| 3-8 MR | **사용자가 시험할지 결정** | MR 닫기·브랜치 삭제(사용자) | — |
| 3-6·3-10 | — | 컷오버에서 확인 안 함 | — |
| 4 롤백 | 승인 | — | 필요 |

## 12. W3a 컷오버 런북 (프로젝트 등록부·서명된 변경, `orch-broker`·`orchd` 갱신)
설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `20-PLAN-webconsole-W3a.md`, 결과·시험·한계는 `21-RESULTS-webconsole-W3a.md`. **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 broker·임시 폴더)을 읽고 쓴 것이지 서버에서 본 출력이 아니다(11장과 같은 원칙). 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려한다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[되돌림 가능]** 아래 몇 줄로 원상 복구, **[주의]** 되돌리면 그 사이의 변경을 잃는다. 경로는 11장과 같다(바이너리 `/usr/local/lib/aigate`와 `/usr/local/bin/orch-broker`, 설정 `/etc/orch-broker/broker.json`, 상태 `/var/lib/orch-broker`, broker 소켓 `/run/orch-broker/broker.sock`). **orchd DB 스키마는 이번에 바뀌지 않는다(v8 그대로)**: 롤백에 DB 복원이 필요 없다. 새로 생기는 것: broker 상태 폴더의 `registry.json`(프로젝트 등록부)과 `used-requests.json`(쓴 요청 ID), `broker.json`의 선택 키 `repo_roots`, orchd의 `/api/v1/projects/registry|candidates|propose|apply`, 웹의 `#/projects` 화면.

### 12.0 시작 전에 모두 참이어야 하는 것
- **W2b 컷오버가 끝난 상태가 출발점이다**(18번 문서 7장): broker `active`, `orchd`가 `--broker-socket`으로 도는 중, DB 스키마 v8, 서명 필수 모드(`/etc/aigate/signers.json`에 `mac-ssh`·`mac-passkey`). 실행 중이거나 대기 중인 작업이 없다(`orch list`, `orch approvals`). 단계 2는 `orchd`를 재시작하고 단계 1은 broker를 재시작한다.
- **웹 서명 흐름에 쓸 패스키 서명자가 등록되어 있다**: `sudo orch admin signers list`에 `webauthn` 형식 서명자가 있다(W1 컷오버의 `mac-passkey`). 등록 화면의 서명은 이 패스키로 한다.
- 그룹 `orch`, 계정 `orch-broker`, `/etc/orch-broker`, `/var/lib/orch-broker`가 11장에서 만들어진 그대로 있다.
- **사전 점검 (단계 1 전에, 전부 값을 찍지 않는 명령):**
  - **(a) 상태 폴더 모드.** `/var/lib/orch-broker`는 **`orch-broker` 소유 0700이어야 하고 그렇게 유지해야 한다**: `sudo stat -c '%U:%G %a' /var/lib/orch-broker`가 `orch-broker:orch-broker 700`. 이 폴더의 소유·모드 검사가 어긋나면(그룹·기타 권한 비트가 하나라도 있거나 소유자가 다르면) 등록부를 "신뢰할 수 없음"으로 보고 **등록부에 의존하는 모든 것(`status`, `projects`, `refine`, `create_mr`, 등록·변경)이 `registry_unavailable`로 꺼진다**. 등록부 파일은 0600, `used-requests.json`도 0600이다.
  - **(b) 서명자 목록 읽기.** `sudo -u orch-broker test -r /etc/aigate/signers.json && echo READABLE || echo denied` → **`READABLE`이어야 한다.** (파일은 root 소유 0644, 폴더 `/etc/aigate`는 root 0755: 읽기 가능함은 이 저장소 쪽 점검으로 확인함. **broker가 실제로 이 파일을 읽어 서명을 검증하는지는 서버에서 처음 확인한다.**) 유닛의 `ProtectSystem=strict`는 `/etc`를 읽기 전용으로 보이게 할 뿐 읽기를 막지 않는다(코드·유닛 읽기 기준, 미확인).
  - **(c) `repo_roots` 철자.** broker 설정의 `repo_roots`는 **orchd가 쓰는 정책의 `repo_roots`와 글자 그대로 같아야 한다**(현재 `policy.yml`은 `repo_roots: [/home/orch/repos]`): `sudo grep -n repo_roots /home/orch/orchd/policy.yml`(실제 정책 파일 위치는 `systemctl cat orchd`의 `--policy`로 확인). orchd가 후보로 보여 주는 경로는 orchd가 정리한 실제 경로 아래이고 broker는 자기 설정의 철자와 **정확히 한 단계 아래**만 허용하므로, 한쪽이 심볼릭 링크 철자(`/home/orch/../orch/repos` 등)나 끝 `/`를 쓰면 broker가 모든 등록을 `path_not_allowed`로 거부한다.
  - **(d) (결정 불필요) 저장소 경로와 broker의 권한.** 실서버에서 `/home/orch/repos`는 `drwx------ orch:orch`이고 그 아래 `toy`도 `drwx------`이다(컨트롤러가 서버에서 직접 확인: `ls -ld`와 `systemd-run` 임시 유닛으로 읽기 전용 시험. 유닛의 `ProtectHome=yes`이든 `TemporaryFileSystem`·`BindReadOnlyPaths`를 더하든 `orch-broker` 계정은 `lstat`도 못 한다). 그래서 **broker는 등록 경로에 대해 파일 시스템을 전혀 보지 않고 문법만 검사한다**(정리된 절대 경로, `repo_roots` 항목의 바로 아래 한 단계). 저장소인지·링크인지는 orchd가 propose·apply에서 검사한다. **유닛 변경이 필요 없다: `ProtectHome=yes`를 그대로 둔다.** 이 점검에서 할 일은 없다.

**알아둘 것 (코드 읽기와 시험 기준, 실서버 미확인).**
1. **WebAuthn 서명 카운터는 어디서도 검사하지 않는다**(`signing.Verify`에도 없다). 재전송 방어는 **서명 5분 신선도 + 요청 ID 1회성**(broker가 `used-requests.json`에 시각과 함께 저장, 1시간 보관, broker를 재시작해도 유지)이다. 복제된 패스키는 카운터로 걸러지지 않는다.
2. **상태 폴더 `/var/lib/orch-broker`는 `orch-broker` 소유 0700을 유지한다**(위 사전 점검 a). 이 검사는 등록부와 `used-requests.json`에 적용된다(코드 읽기 기준: `budget.json`은 이 폴더 검사와 별개로 읽는다). 모드가 틀리면 등록부가 꺼진 것으로 보이므로 컷오버 뒤에도 `stat`으로 확인한다.
3. **`broker.json`의 `repo_roots`는 orchd가 해석한 정책의 `repo_roots`와 철자가 같아야 한다**(사전 점검 c). 값이 없으면 모든 등록이 `path_not_allowed`다(안전한 기본). broker는 이 값을 글자 그대로만 비교하고 파일 시스템은 보지 않는다.
4. **마이그레이션된 프로젝트의 `kind`는 `legacy`다.** 옛 설정에는 정책 클래스가 없었기 때문이다. W3a에서는 화면에 그대로 표시만 하며, **작업을 제출할 때 정책 클래스는 orchd가 제출 시점에 정한다**(등록부의 `kind`를 쓰지 않음).
5. **등록 경로 검사는 등록하는 순간의 검사다.** 등록 뒤에 그 경로를 심볼릭 링크로 바꿔치기해도 등록부는 그대로지만, **사용 시점에 거부된다**: AI 계획서 미리보기가 `allowed: false`(다른 저장소로의 링크) 또는 403(허용 루트 밖으로의 링크)이 되고 아무것도 전송되지 않는다(`TestRegisteredPathReplacedBySymlinkRefusedAtUse`). `repo_roots` 자체의 상위 경로 심볼릭 링크는 풀지 않는다.
6. **쓰기 op의 `timeout`은 "결과를 모른다"는 뜻이다.** 화면은 고정 문장(목록을 다시 읽어 확인)과 `outcome_unknown`(504)을 보이고 목록을 다시 읽는다. **목록을 다시 읽어 확인하기 전에는 같은 변경을 다시 시도하지 않는다**(broker가 뒤늦게 적용했을 수 있다). 서명이 끝난 변경은 한 번 쓰고 나면 같은 서명으로 다시 보낼 수 없다.
7. **broker는 깨진 등록부로도 시작하지만 op마다 `registry_unavailable`로 답한다**(옛 `broker.json`으로 되돌아가지 않는다: AI 허용이 넓어지지 않음). **`orch-broker -check`는 깨진 등록부를 거부한다**(실패, 종료 코드 1). 등록부 파일은 건드리지 않는다(고치려면 사용자가 판단해 파일을 옮기거나 지운 뒤 재시작: 지우면 옛 설정으로 **다시 마이그레이션**된다). 이전 쓰기가 남긴 `.state-*` 임시 파일은 시작할 때 1분이 지난 것만 치운다.
8. **한도(주체별).** 등록·변경 요청 만들기(`propose`) 시간당 **60회**, 서명 적용(`apply`) 시간당 **30회**. 초과하면 429 `project_rate_limited`. **슬롯은 `apply`가 broker에 닿기 전에 실패했을 때(연결 실패)와 broker가 일하기 전에 거절했을 때(`busy`, `forbidden`)만 돌려준다.** orchd가 로컬에서 하는 대상 검사(`checkRegisterTarget`: 저장소가 아니거나 링크 등)에 걸린 `apply`는 슬롯을 **돌려주지 않고 원장 줄도 남기지 않는다**(`project_change` 줄은 broker로 보낸 시도에만). 쓸데없는 경로를 계속 시도하면 시간당 30회가 소진된다.
9. **등록부 목록 엔드포인트는 `GET /api/v1/projects/registry`다.** `GET /api/v1/projects`는 W2a의 저장소별 작업 요약으로 그대로 남아 있다(설계서 8장의 `GET /projects`는 이 이유로 바뀜).
10. **이번에 확인하지 못한 것(결과 문서 21번 5장):** 키보드만, 스크린 리더로의 `#/projects` 화면 사용은 시험하지 않았다. 화면의 시각 확인은 **대역 패스키**(브라우저의 `navigator.credentials.get`을 바꿔치기)로 했다. **실제 패스키 서명을 broker가 검증하는 것, broker가 실제 `/etc/aigate/signers.json`을 읽는 것, 실제 `broker.json`으로 마이그레이션이 올바른 등록부를 만드는 것은 컷오버 전까지 전부 미확인이다.** 처음 한 번 성공하기 전에는 아무것도 "된다"고 말하지 않는다.
11. **마이그레이션은 등록부 파일이 없을 때 broker가 시작하면서 단 한 번 일어난다.** 되돌릴 때는 broker 정지 → 이전 바이너리와 `broker.json` 복원 → `registry.json`과 `used-requests.json` 삭제(단계 4). **컷오버 뒤 등록부에 한 모든 변경(프로젝트 추가, AI 전송 켜기/끄기)은 롤백하면 사라진다.** 시작하기 전에 `/var/lib/orch-broker`와 `/etc/orch-broker`를 root 전용 `/root/backup-w3a`(0700)로 통째로 복사해 둔다(단계 1). `orch`가 쓸 수 있는 폴더에는 백업을 두지 않는다.
12. **마이그레이션 뒤에는 `broker.json`의 옛 `context_repos`·`gitlab.projects`가 무시된다.** 파일에는 남아 있고 `orch-broker -check`가 **경고만** 한다(종료 코드 0, 시작을 막지 않음). 등록부가 아직 없는 첫 점검에서는 경고가 없고, 대신 옛 항목을 등록부로 옮길 수 있는지(id 충돌)를 메모리에서만 확인한다. 롤백하면 이 옛 항목이 다시 쓰이므로 **지우지 않는 것이 안전하다.**
13. **`registry_unavailable`의 보이는 모습.** 계획서 미리보기·AI 요청은 고정 문장 "프로젝트 등록부를 사용할 수 없어 AI 전송이 모두 꺼져 있습니다"(503 `registry_unavailable`)를 보인다. **병합 요청**은 broker가 `registry_unavailable`로 답하면 작업의 `mr_state=failed`, `mr_code=registry_unavailable`로 저장되고(재시도 가능, 작업 화면은 이 코드의 전용 문장 없이 일반 실패 구역으로 보인다) 등록부가 복구되면 재시도로 풀린다. **등록·변경(쓰기)에서 broker가 `registry_unavailable`로 답하면 화면은 `outcome_unknown`(504, "목록을 다시 읽어 확인")으로 보인다**: 디렉터리 fsync가 rename 뒤에 실패하면 변경이 이미 적용된 채로 오류가 나기 때문이다. 목록 조회(읽기)의 `registry_unavailable`은 그대로 `registry_unavailable`이다.
14. **`set_flag` 서명은 프로젝트 id를 묶고 경로는 묶지 않는다.** 지금은 삭제·이름 변경·재등록 op가 없어서 안전하지만, 앞으로 그런 op를 만들면 **경로 또는 등록부 세대를 `change`에 넣어야 한다**(설계서 19번 문서 경고).
15. **브라우저 제한시간.** 프로젝트 화면의 `propose`·`apply`는 25초를 기다린다(broker 쓰기 한도 15초 + 저장소 검사 < 25초). 기본값(15초)이면 화면이 먼저 포기할 수 있어서 명시했다.

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 21번 5장에 "확인함/못 함"으로 남긴다)
1. **실제 패스키 서명을 broker가 검증하는지**(3-4). 이 저장소는 시험용 키·모의 인증기만 썼다.
2. **broker가 유닛 샌드박스 안에서 `/etc/aigate/signers.json`을 여는지**: 1-4의 `systemd-run` 점검이 시작 전에 이미 본다(없거나 신뢰할 수 없으면 거부).
3. **`link(2)`(마이그레이션의 배타 생성)·`fsync`가 실서버 파일 시스템에서 동작하는지**: 1-5에서 `registry.json`이 생기고 모드 600이면 확인된 것.
4. **마이그레이션 뒤 `projects`의 실제 출력**: 정확히 `toy`와 `sample-app`, `kind` `"legacy"`, `ai_context` true, `gitlab` null(1-6).
5. **`/tmp`의 `noexec` 여부와 `sudo -u orch-broker`·`systemd-run` 동작**: 1-1·1-4에서 드러난다(`/tmp`가 `noexec`여도 실행은 `/usr/local/bin`의 사본이라 영향 없음. `systemd-run`이 없거나 옵션을 거부하면 멈추고 보고한다).
6. **`readlink -f /home/orch/repos`가 정책의 `repo_roots` 철자와 같은지**: `sudo readlink -f /home/orch/repos`가 `/home/orch/repos`(사전 점검 c).
7. 키보드만·스크린 리더 사용, 실제 `/home/orch/repos`에서 orchd의 저장소 검증(3-3)은 컷오버에서도 사용자가 해 본 만큼만 확인된다.

### 단계 1. broker 갱신·마이그레이션 [사용자][되돌림 가능: 단계 4의 A]
`orchd`와 DB는 건드리지 않는다. **순서: 백업 → (Mac) 빌드 → 옛 broker 정지 → 새 바이너리와 `broker.json`(`repo_roots` 포함)을 한 쌍으로 설치 → 샌드박스 안에서 `-check` → 시작.** 옛 바이너리는 `repo_roots` 키를 모르는 설정을 거부하므로(알 수 없는 키 거부) 바이너리와 설정은 **항상 한 쌍으로** 바꾸고 되돌린다.

**먼저 백업(서비스 영향 없음). 모든 W3a 백업은 `orch`가 쓸 수 없는 root 전용 폴더 `/root/backup-w3a`(0700)에 둔다.** (`/home/orch/orchd/` 아래는 `orch`가 쓸 수 있는 폴더라서 root가 신뢰하는 `broker.json`을 거기서 복원하면 안 된다.) orch-vm에서:
```
B=/root/backup-w3a
sudo install -d -m 0700 -o root -g root $B $B/bin
sudo stat -c '%U:%G %a %n' $B          # root:root 700 이어야 한다
sudo cp -a /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -a /usr/local/lib/aigate/$f $B/bin/$f; done
sudo cp -a /etc/orch-broker $B/etc-orch-broker
sudo cp -a /var/lib/orch-broker $B/var-lib-orch-broker
sudo cp -a /etc/systemd/system/orch-broker.service $B/orch-broker.service.pre-w3a
sudo cp -a /etc/systemd/system/orchd.service $B/orchd.service.pre-w3a; sudo cp -a /etc/systemd/system/orchd.service.d $B/ 2>/dev/null
sudo ls -lR $B | head -40
```
(백업에는 `openai.key`(0400)의 사본이 들어간다. 값은 화면에 나오지 않고 폴더는 root만 들어갈 수 있다. **키 사본을 `orch`가 쓸 수 있는 폴더에 두지 않는다.** 컷오버가 안정되면 사용자가 `sudo shred -u`로 지운다.)

**1-0. 빌드 전 확인 (Mac, 저장소에서).** 웹 화면의 상수와 서버 상수가 같은지: `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → **`--- PASS`이고 `SKIP`이 없어야 한다**(건너뛰거나 실패하면 빌드하지 않는다). 이어서 `go vet ./...`, `go test ./... -count=1 -race`, `node --test "internal/orch/web/test/*.test.js"`가 통과한 커밋인지 확인한다(결과 문서 21번 2장).

**1-1. 새 바이너리.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w3a/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고(`uname -m`에 맞춘다) `scp`로 VM `/tmp/w3a/`에 보낸다. **아직 설치하지 않는다.** (`/tmp`가 `noexec`로 마운트되어 있으면 아래 `install` 뒤 실행에는 영향이 없다: 실행은 `/usr/local/bin`의 사본으로 한다.)

**1-2. 설정에 `repo_roots` 더하기.** `sudo cp /etc/orch-broker/broker.json /tmp/w3a/broker.json`으로 복사해 편집한다(기존 키는 그대로, `context_repos`·`gitlab`도 **지우지 않는다**). 마지막에 `"repo_roots": ["/home/orch/repos"]`를 더한다(사전 점검 c의 철자). 알 수 없는 키는 거부되므로 철자를 확인한다. **아직 설치하지 않는다.**

**1-3. 옛 broker 정지 → 바이너리와 설정을 한 쌍으로 설치.** (정지한 뒤 1-4의 점검이 실패하면 **broker는 멈춘 채로** 남는다. 그때는 곧바로 단계 4의 A로 옛 짝을 복원해 시작한다.)
```
sudo systemctl stop orch-broker
sudo install -o root -g root -m 0755 /tmp/w3a/orch-broker /usr/local/bin/orch-broker
sudo install -o root -g root -m 0644 /tmp/w3a/broker.json /etc/orch-broker/broker.json
```
이 시점부터 시작까지 AI 계획서와 병합 요청은 `broker_unavailable`이다(기존 기능은 그대로). 아직 시작하지 않는다.

**1-4. 샌드박스 안에서 `-check`(시작 전).** 유닛과 같은 샌드박스 옵션으로 점검한다. 이렇게 해야 broker가 **유닛의 샌드박스 안에서 `signers.json`을 실제로 여는지**, 설정·키·상태 폴더를 `orch-broker` 계정으로 읽을 수 있는지를 시작 전에 볼 수 있다(그냥 `sudo -u orch-broker`로는 샌드박스 효과를 보지 못한다):
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (설정·신뢰·키 모드·소켓 그룹·상태 폴더만 확인했습니다. 외부 접속은 하지 않았습니다)` 한 줄(`systemd-run --wait --pipe`가 `Running as unit: …`과 실행 시간·CPU 줄을 함께 찍으니 그 줄들은 정상이다)(**경고는 없다**: 등록부가 아직 없고 `repo_roots`가 있다)과 `Finished with result: success`(종료 코드 0). `-check`는 아무것도 쓰지 않는다(`sudo ls /var/lib/orch-broker`에 `registry.json`이 아직 없어야 한다). 이 점검은 이제 다음을 모두 본다: 설정·키 신뢰, 소켓 그룹, 상태 폴더, **`/etc/aigate/signers.json`이 로드되는지(없거나 신뢰할 수 없으면 거부)**, **`used-requests.json`이 깨지지 않았는지**, **옛 항목을 등록부로 옮길 수 있는지(id 충돌이면 거부)**, 그리고 `repo_roots`가 비어 있으면 **경고**(`…path_not_allowed가 됩니다`). 실패 문장 하나가 나오면 **시작하지 않고 멈춘다 → 단계 4의 A(바이너리와 설정을 한 쌍으로 복원하고 옛 broker 시작).**

**1-5. 시작(마이그레이션이 여기서 일어난다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/used-requests.json || echo "(없음: 첫 쓰기 전에는 정상)"
```
예상: 유닛 `active (running)`, 로그에 오류 없음. 폴더는 `orch-broker:orch-broker 700`, `registry.json`이 새로 생기고 **`orch-broker:orch-broker 600`**. `used-requests.json`은 첫 쓰기 때까지 없을 수 있다(없으면 위 안내 문구). **로그에 "등록부" 관련 오류 문장이 있으면 멈춘다**(등록부가 깨졌다는 뜻이고 broker는 op마다 `registry_unavailable`로 답한다).

**1-6. 마이그레이션 결과 확인: 등록부가 현재 설정과 같은 허용 범위를 주는가.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기 두 개):
```
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"projects\"}\n");print(s.makefile().readline())'
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"status\"}\n");print(s.makefile().readline())'
```
예상: `projects`가 **정확히 `toy`와 `sample-app` 두 프로젝트**만 주고(`kind` `"legacy"`, `ai_context` true, `provider` null, `gitlab` null(`gitlab` 절이 있던 구성이면 그 매핑), `auto_merge` `{"enabled":false}`), 모양은 `{"ok":true,"result":{"projects":[{"id":"toy","path":"/home/orch/repos/toy","kind":"legacy","ai_context":true,"provider":null,"gitlab":null,"auto_merge":{"enabled":false}},{"id":"sample-app",…}],"providers":[],"auto_merge_global":{"enabled":false,"max_per_day":5}}}`이다. `status`의 `context_repos`가 **단계 1 이전과 같은 두 경로**다. 응답에 키·토큰·서명자 키가 없어야 한다. **다르면 멈추고 단계 4의 A.** 이 단계가 "실제 `broker.json`으로 마이그레이션이 올바른 등록부를 만드는가"의 첫 실제 확인이다.

**1-7. 점검을 한 번 더(이번에는 등록부가 있다).** 1-4의 명령을 그대로 다시 실행한다. 예상: **`orch-broker: 경고: 프로젝트 등록부가 있으므로 broker.json의 context_repos·gitlab.projects는 무시됩니다…` 한 줄**과 `점검 통과`, 종료 코드 0. 경고가 없으면 마이그레이션이 일어나지 않은 것이다(1-5를 다시 본다).

**1-8. 서명자 목록 읽기.** 사전 점검 (b)를 지금 다시 한다(`READABLE`).

**검증 체크리스트:** 1-4 통과(경고 없음), 1-5의 유닛 active·등록부 600·폴더 700, 1-6의 `projects`(정확히 두 프로젝트)와 `status`가 단계 1 이전의 허용 범위와 같음, 1-7의 경고 한 줄, 1-8의 `READABLE`. **그래도 안 되면:** 단계 4의 A. 다른 서비스에는 영향이 없다(이 단계는 `orchd`를 건드리지 않았다).

### 단계 2. `orchd` 갱신 [사용자]
**DB 스키마가 바뀌지 않으므로 DB를 복원할 필요가 없다**(이번 백업은 `/root/backup-w3a/bin`의 바이너리뿐). 11장의 배포 방식과 같다. 실행 중·대기 작업이 없음을 먼저 확인한다(`orch list`).
```
sudo systemctl stop orchd
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3a/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
```
유닛과 드롭인(`--broker-socket`)은 바꾸지 않는다. **검증 체크리스트**
- 로그: 오류 없이 대기, `approvals: mode signed`, 스키마 오류 없음.
- `orch list`: 단계 1 이전의 작업이 모두 그대로, 마지막 번호가 같다.
- 새 엔드포인트가 실제로 떠 있는지는 단계 3-1(웹)에서 처음 본다.

**그래도 안 되면:** 단계 4의 B(바이너리만 되돌리면 된다. DB는 그대로 호환된다).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/projects.test.js`, `internal/orch/api/projects_test.go`, `adversarial_w3a_test.go`, `internal/broker/*_test.go`)에서 읽은 것이다.

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | 메뉴 "프로젝트"(`#/projects`) | 목록에 `toy`·`sample-app`(종류 `legacy`, AI 전송 허용 "예"). GitLab 연결·자동 병합은 읽기 전용 표시(자리표시). **주소 표시줄의 해시는 `#/projects` 그대로** | 목록이 안 뜨면 `broker_unavailable`/`registry_unavailable`: 단계 1-5·1-6. 메뉴가 없으면 브라우저 캐시(새로고침) |
| 3-2 | **회귀: AI 계획서.** "새 작업" → "AI로 계획서 만들기", 저장소 `toy`, 한 줄을 적고 미리보기까지만 | 11장 3-1과 같다: 보낼 파일 목록, 제외 사유, "이 내용을 AI로 보냅니다" 단추(**누르지 않아도 된다**: 실제 OpenAI 호출은 비용이 든다) | `registry_unavailable`("프로젝트 등록부를 사용할 수 없어 AI 전송이 모두 꺼져 있습니다": 등록부가 깨졌거나 상태 폴더 소유·모드가 어긋남. 사전 점검 a와 `journalctl -u orch-broker`를 본다 → 단계 4의 A. 이 고정 문장은 W3a에서 생겼고 전에는 "AI 서비스 호출에 실패"로 보였다)이거나, "허용되어 있지 않습니다"면 `status`의 `context_repos`(1-6)가 단계 1 이전과 다른 것이다 → 단계 4의 A |
| 3-3 | **[사용자 결정] 등록 흐름.** 후보 저장소가 있으면(허용 루트 아래 아직 등록되지 않은 저장소가 후보로 보인다): "프로젝트 추가" → 후보 고르기 → 종류(정책 클래스) → AI 전송 허용(기본 꺼짐) → "변경 내용 확인" | 확인 화면이 id·경로·종류·AI 여부를 보여 주고, AI를 켜면 노란 강조 문장이 하나 더 나온다. 서버 문장이 전체 변경 내용을 말한다. 서명 단추는 **브라우저가 다시 계산한 해시가 서버 것과 같을 때만** 열린다 | 경고 문장과 서명 단추 없음 = 내용 불일치(서명하지 않는다, 기록한다). 경로가 저장소가 아니면(링크·HEAD 없음 등) **서명 전에** orchd가 거부한다 |
| 3-4 | **서명(사용자가 할지 정한다).** 서명하면 **실제로 broker의 등록부가 바뀐다.** 패스키로 서명 → 완료 | 목록에 새 행. broker가 **실제 패스키 서명을 처음으로 검증**한다(미확인 항목). 원장에 `project_change`(행위자·동작·id·성공 여부·코드, **경로·내용 없음**) | `bad_signature`/`unknown_signer`/`signers_unavailable`: 서명자 목록(사전 점검 b), 패스키 등록. `stale_signature`: 시계(5분)와 확인 화면에서 서명까지 걸린 시간. **같은 변경을 곧바로 다시 시도하지 않는다**(화면은 새 제안부터 다시 한다) |
| 3-5 | **AI 전송 켜기/끄기 시험(등록하지 않을 때의 대안, 사용자가 정한다).** 이미 등록된 두 프로젝트 중 하나(권장: 덜 중요한 쪽)의 AI 전송을 끄고 → 다시 켠다 | 각각 확인 화면(경로와 "예 → 아니오" 표시) → 패스키 서명 → 목록 갱신. 끈 동안 그 저장소로 AI 미리보기는 "허용되어 있지 않습니다". **끝나면 원래 상태(켜짐)로 되돌린다** | 같은 오류 코드 설명은 3-4와 같다 |
| 3-6 | **서명 거부 확인.** 잘못된 서명으로 변경을 시도하는 것은 **자동 시험으로만 확인**한다(실서버에서 일부러 위조 서명을 만들지 않는다) | 시험: `TestRegistryNeverWrittenWithoutValidSignature`, `TestSignatureOverDifferentChangeRefused` 등 | — |
| 3-7 | **브로커를 멈춘 상태**(선택): `sudo systemctl stop orch-broker` → `#/projects`를 연다 | 고정 오류(브로커에 연결할 수 없음). 작업 제출·취소 등 기존 기능은 그대로. 끝나면 `sudo systemctl start orch-broker`(**등록부가 이미 있으므로 다시 마이그레이션되지 않는다**) | 기존 기능까지 막히면 단계 4의 B |
| 3-8 | 확인 | `sudo sh -c 'stat -c "%U:%G %a %n" /var/lib/orch-broker /var/lib/orch-broker/*.json'`: 폴더 700, 파일 600. `sudo grep project_change /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl \| tail -3`: 내용 없는 줄만 | 내용(경로·서명)이 보이면 멈추고 보고 |

**검증 체크리스트(요약):** 3-1이 예상대로, 3-2 회귀 없음, 3-3~3-5 중 사용자가 한 것이 예상대로, 3-7에서 기존 기능이 살아 있고, 3-8의 모드. 3-4(실제 패스키 서명 검증)와 사전 점검 (b)·(d)의 결과는 결과 문서 5장에 "컷오버에서 확인함/못 함"으로 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다. 등록부가 의도와 다르게 바뀌었거나(서명하지 않은 변경), 원장·로그·화면에 서명·경로 원문이 보이거나, AI 허용 범위가 단계 1 이전보다 넓어졌으면 **즉시 broker를 멈추고(단계 4의 A) 사용자에게 보고한다.**

### 단계 4. 롤백 [사용자][주의]
**A. broker를 W2b 상태로 되돌리기(등록부 폐기, DB는 건드리지 않는다).**
1. `sudo systemctl stop orch-broker`
2. **바이너리와 설정을 한 쌍으로 복원**(옛 바이너리는 `repo_roots`가 있는 설정을 거부한다): `sudo install -o root -g root -m 0755 /root/backup-w3a/bin/orch-broker /usr/local/bin/orch-broker`, `sudo install -o root -g root -m 0644 /root/backup-w3a/etc-orch-broker/broker.json /etc/orch-broker/broker.json`(**root 전용 백업에서만 복원한다**: `orch`가 쓸 수 있는 곳의 파일을 root 신뢰 설정으로 설치하지 않는다. 대안: 설정에서 `repo_roots` 줄만 지워도 된다. 이 파일은 옛 `context_repos`·`gitlab.projects`를 담고 있다: 1-2에서 지우지 않았다).
3. **등록부 파일 삭제:** `sudo rm -f /var/lib/orch-broker/registry.json /var/lib/orch-broker/used-requests.json`(옛 broker는 이 파일을 쓰지 않으므로 남겨 둬도 동작하지만, **마이그레이션은 등록부가 없을 때만 일어나므로 나중에 새 broker를 다시 올릴 때를 위해 지운다**). `budget.json`은 지우지 않는다(주 예산 기록).
4. `sudo systemctl start orch-broker`; `status` 호출(11장 1-8의 python 줄)이 옛 `context_repos` 두 경로를 주는지 본다.
5. **경고:** **컷오버 뒤 등록부에 한 모든 변경(추가한 프로젝트, 바꾼 AI 전송 허용)은 사라진다.** 다시 새 broker를 올리면 옛 `broker.json`으로 다시 마이그레이션되어 그 변경 전 상태가 된다(필요하면 폐기 전에 `projects` 응답을 따로 저장해 둔다: 값이 든 파일은 없다). 문제가 시작되기 전 복사본이 필요하면 `/root/backup-w3a/var-lib-orch-broker`에 있다(**복원하지 않는다**: 이 폴더 전체를 되돌리면 예산 기록도 되돌아간다).

**B. `orchd`·CLI를 W2b 상태로 되돌리기(DB 복원 불필요).**
1. `sudo systemctl stop orchd`
2. `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3a/bin/$f /usr/local/lib/aigate/$f; done`(세 개는 항상 같이)
3. `sudo systemctl start orchd`. DB 스키마는 이번에 바뀌지 않았으므로 복원이 필요 없다. 웹의 "프로젝트" 메뉴가 사라진다(브라우저 캐시면 새로고침).

**검증:** `journalctl -u orchd`·`journalctl -u orch-broker`에 오류가 없고, `orch list`가 단계 2 이전과 같으며, W2b의 AI 계획서 미리보기(11장 3-1)가 동작한다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`, `ls -l /usr/local/bin /usr/local/lib/aigate /var/lib/orch-broker`와 `sudo ls -lR /root/backup-w3a | head -40`을 사용자에게 보고한다. W2b 이전까지 가야 하면 11장 단계 4(`*.prev-w2b`, `backup-w2b`).

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 0 사전 점검(상태 폴더 0700, `signers.json` 읽기, `repo_roots` 철자) | 확인만(유닛 변경 없음) | — | — |
| 1 broker 갱신·마이그레이션 | 승인 | A(바이너리·`broker.json`을 한 쌍으로 `/root/backup-w3a`에서 복원 + 등록부 삭제, 컷오버 뒤 등록부 변경은 사라짐) | broker만 |
| 2 `orchd` 갱신 | 승인 | B(바이너리만, DB 복원 불필요) | 필요(stop/start) |
| 3 웹 확인 | **사용자 직접**(로그인, 등록·켜기/끄기 **서명할지 결정**) | 등록한 프로젝트는 A로 사라짐. AI 켜기/끄기는 같은 화면으로 되돌림 | 불필요(3-7만 broker 재시작) |
| 4 롤백 | 승인 | — | 필요 |

## 13. W3b 컷오버 런북 (토큰 메뉴·만료 경고·OpenAI 키 교체, `orch-broker`·`orchd` 갱신)
설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `22-PLAN-webconsole-W3b.md`, 결과·시험·한계는 `23-RESULTS-webconsole-W3b.md`(**배포 현실 표**와 "확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 broker·임시 폴더)을 읽고 쓴 것이지 서버에서 본 출력이 아니다(11·12장과 같은 원칙). **W3a에서 임시 폴더 시험이 실서버의 권한 문제를 숨긴 적이 있어서(21번 문서 8장), 이번에는 0단계에 컨트롤러가 서버에서 읽기 전용으로 먼저 보는 사전 점검 목록을 따로 둔다.** 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려한다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[컨트롤러]** 읽기 전용 확인(서버 상태를 바꾸지 않음), **[되돌림 가능]** 아래 몇 줄로 원상 복구, **[주의]** 되돌리면 그 사이의 변경을 잃는다. 경로는 11·12장과 같다(바이너리 `/usr/local/lib/aigate`와 `/usr/local/bin/orch-broker`, 설정 `/etc/orch-broker/broker.json`, 상태 `/var/lib/orch-broker`, broker 소켓 `/run/orch-broker/broker.sock`). **orchd DB 스키마는 이번에 바뀌지 않는다(v8 그대로)**: 롤백에 DB 복원이 필요 없다. **`broker.json`과 유닛(`orch-broker.service`, `orchd.service`)도 바뀌지 않는다**(새 설정 키·새 플래그 없음). 새로 생기는 것: broker 상태 폴더의 `secrets/`(토큰 값, 0700)와 `tokens.json`(토큰 메타데이터, 0600, 값 없음)(**`secrets/` 폴더는 broker가 시작할 때** 만들어지고, `tokens.json`은 웹에서 토큰을 처음 등록할 때 만들어진다), broker op `tokens`·`set_token`·`delete_token`, orchd의 `/api/v1/settings/tokens|deploy-key`와 `/api/v1/tokens/begin|message|apply`, 웹의 `#/tokens` 화면과 만료 배너.

### 13.0 시작 전에 모두 참이어야 하는 것
- **W3a 컷오버가 끝난 상태가 출발점이다**(21번 문서 10장): broker `active`(등록부 `registry.json` 있음), `orchd`가 `--broker-socket`으로 도는 중, DB 스키마 v8, 서명 필수 모드, `/etc/aigate/signers.json`에 `mac-passkey`. 실행 중이거나 대기 중인 작업이 없다(`orch list`, `orch approvals`). 단계 1은 broker를, 단계 2는 `orchd`를 재시작한다.
- **웹 서명에 쓸 패스키 서명자가 등록되어 있다**(`sudo orch admin signers list`에 `webauthn` 형식). 토큰 변경의 서명도 같은 패스키로 한다.
- **백업은 `/root/backup-w3b`(root 소유 0700)에만 둔다.** `orch`가 쓸 수 있는 폴더(`/home/orch/orchd/…`)에는 두지 않는다(21번 문서 9장 I1과 같은 이유: root가 신뢰하는 파일을 `orch`가 바꿔치기할 수 있는 곳에서 복원하면 안 된다).
- **사전 점검 (단계 1 전에, 전부 읽기 전용이고 값·개인 키를 찍지 않는 명령. 컨트롤러가 서버에서 직접 한다).** 하나라도 예상과 다르면 **시작하지 않고** 보고한다.
  - **(a) 상태 폴더.** `sudo stat -c '%U:%G %a' /var/lib/orch-broker` → **`orch-broker:orch-broker 700`**. `secrets/`가 이 폴더 **안에** 만들어지고, 폴더의 소유·모드 검사가 어긋나면(그룹·기타 권한 비트 하나라도, 소유자가 다름) 토큰 저장소 전체가 "신뢰할 수 없음"으로 꺼진다(그리고 W3a의 등록부도 꺼진다). 지금 `secrets/`·`tokens.json`이 **없어야** 한다: `sudo ls -la /var/lib/orch-broker`.
  - **(b) 설정의 OpenAI 키 파일.** 경로는 `sudo grep -n key_file /etc/orch-broker/broker.json`(예상 `/etc/orch-broker/openai.key`). `sudo stat -c '%U:%G %a %n' <경로>` → 소유자 `orch-broker`(또는 broker가 허용하는 소유자), 모드 `400`. `sudo -u orch-broker test -r <경로> && echo READABLE || echo denied` → **`READABLE`**. 이 파일이 **롤백의 마지막 보루**이고 **삭제 폴백**(저장소 키를 지우면 이 키로 돌아감)이므로 컷오버 동안 건드리지 않는다.
  - **(c) 유닛 샌드박스 값.** `systemctl show orch-broker -p ReadWritePaths -p ProtectSystem -p ProtectHome -p UMask -p StateDirectory -p StateDirectoryMode -p User` → 예상 **`ReadWritePaths=/var/lib/orch-broker /run/orch-broker`**, `ProtectSystem=strict`, `ProtectHome=yes`, **`UMask=0077`**, `StateDirectory=orch-broker`, `StateDirectoryMode=0700`, `User=orch-broker`. `ReadWritePaths`에 `/var/lib/orch-broker`가 없으면 broker가 `secrets/`를 만들 수 없으므로(만들기 실패 → 토큰 저장소가 꺼진 채 시작) **멈춘다**(유닛 변경이 필요하면 사용자 결정으로 따로 한다).
  - **(d) 배포 키의 공개 키(`.pub`) — 있다(2026-11-10 컷오버에서 확인: 117바이트, `orch:orch` 644, 유효한 ED25519 한 줄, `ssh-keygen -l`로 확인). 아래는 다시 확인하는 방법이다.** `GET /api/v1/settings/deploy-key`는 `--git-ssh-key` 경로 + `.pub`만 읽는다(개인 키는 코드가 열지 않는다. 시험 있음). 실서버의 `--git-ssh-key`는 `/home/orch/orchd/ssh/id_gitlab_deploy`다(`systemctl cat orchd`로 확인). 확인(개인 키는 읽지 않는다): `sudo ls -l /home/orch/orchd/ssh/` 와 `sudo stat -c '%U:%G %a %s %n' /home/orch/orchd/ssh/id_gitlab_deploy.pub`(예상: 일반 파일, 소유자 `orch`(또는 root), 모드 `644`, 크기 4096 이하, **심볼릭 링크 아님**). 한 줄 공개 키인지: `sudo sh -c 'wc -l < /home/orch/orchd/ssh/id_gitlab_deploy.pub'`(0 또는 1: 끝 줄바꿈 한 개까지는 허용. `sudo wc -l < 파일`은 리다이렉트를 sudo가 아니라 권한 없는 셸이 열어서 실패한다)와 `sudo ssh-keygen -l -f /home/orch/orchd/ssh/id_gitlab_deploy.pub`(`.pub`만 읽어 지문을 찍는다. `ssh-ed25519` 또는 `ssh-rsa` 한 줄이어야 한다. 개인 키 파일에는 `-f`를 쓰지 않는다). **없어도 W3b의 어떤 기능도 막히지 않는다**(소비자는 W3c이고, 지금은 `deploy_key_unavailable`로만 답한다). 없으면 기록만 하고 넘어간다: 만들려면 `ssh-keygen -y`가 **개인 키를 읽으므로** 사용자 결정이다(아래 알아둘 것 12).
  - **(e) 샌드박스 안 `-check`.** 단계 1-3에서 새 바이너리로 한다(새 바이너리가 있어야 하므로 설치 뒤, 시작 전). 사전 점검은 **현재(W3a) 바이너리로도** 같은 명령을 한 번 돌려 두면 비교가 쉽다(1-3의 명령, 경고가 없는 통과 한 줄).
  - **서버에서 쓰기 시험을 따로 하지 않는다.** `systemd-run`으로 `secrets/` 생성을 시험하는 일시 유닛은 **필요 없다**: 진짜 broker 시작이 `secrets/`를 만든다(1-4). 그 한 번이 유닛 샌드박스 + `orch-broker` 계정 + `UMask=0077` 아래에서 `secrets/`가 `orch-broker:orch-broker 700`으로 생기는지를 보여 주는 **유일한 실제 시험**이다. **시작 전에 실서버로 확인할 수 없는 것(시작해야 비로소 보임):** ① `secrets/` 생성과 모드, ② 첫 `set_token`의 쓰기(`0400` 파일 생성, `rename`, 폴더 `fsync`)가 서버 파일 시스템에서 되는지(3-5), ③ 실제 패스키 서명을 broker가 `set_token`에 대해 검증하는지(3-5), ④ 실제 OpenAI 키로 교체한 뒤 `refine`이 새 키를 쓰는지(3-5), ⑤ 삭제 뒤 설정 키 폴백(3-6).

**알아둘 것 (코드 읽기와 시험 기준, 실서버 미확인).**
0. **⚠ 먼저 읽을 것: 오타 난 OpenAI 키는 일하는 키를 조용히 가린다.** `set_token`은 D3 규칙(32~512자 인쇄 가능한 ASCII)만 만족하면 **무엇이든 받는다. 키가 OpenAI에서 유효한지는 검사하지 않는다**(검사하면 AI 호출 한 번의 비용이 든다). 웹으로 잘못 친 OpenAI 키를 저장하면 그것이 **정상인 설정 키보다 우선**하므로, 그 뒤 모든 `refine`은 `upstream_error`로 실패한다. **실패한 시도마다 최악 예산 예약이 그대로 남고**(성공해야 실제 비용으로 정산됨) broker의 시간당 슬롯과 orchd의 슬롯도 소모된다. **복구: 다시 시도하지 말고(AI 호출을 더 하지 말고) 먼저 저장소의 `openai`를 삭제한다**(3-6: `delete_token openai` → 설정 키로 폴백). 그 뒤 올바른 키를 다시 정한다. 예산 파일은 쓴 만큼 예약이 남아 있으므로 주간 예산(`weekly_budget_usd`)이 줄어 있을 수 있다.
1. **웹으로 OpenAI 키를 교체하면 `secrets/openai`가 생긴다. 설정의 키 파일(`/etc/orch-broker/openai.key`)은 그대로 남는다**(broker는 `/etc`에 쓸 수 없다). 저장소의 `openai` 키가 있으면 그것을, 없으면 설정 파일의 키를 **호출마다** 읽는다(재시작 불필요, 진행 중인 호출은 시작할 때 읽은 키로 끝난다). **저장소의 `openai` 키를 지우면 설정 파일 키로 돌아간다.** **설정 키만 있는 `openai`(목록의 출처 "설정 파일")를 지우려 하면 `token_unknown`이다**(화면에는 그 행에 삭제 단추가 없다). broker 시작은 여전히 **설정 키 파일이 신뢰할 수 있게 있어야** 한다(W2b 규칙: 없으면 시작을 거부).
2. **키가 아예 없으면(`ai_key_missing`)**: 저장소 키를 지웠고 설정 키 파일도 없거나 읽을 수 없는 경우, AI 계획서(`refine`)는 고정 문장 "AI 키가 등록되어 있지 않습니다…"(503 `ai_key_missing`)로 **실패하고 예산·한도·upstream 호출을 쓰지 않는다**(orchd의 시간당 refine 슬롯도 돌려준다). 키를 다시 등록하면 바로 풀린다.
3. **롤백은 옛 바이너리(broker, orchd, 웹은 orchd에 들어 있다)를 `/root/backup-w3b`에서 복원하는 것이다. 옛 broker는 `secrets/`와 `tokens.json`을 모른다**: 컷오버 뒤 웹으로 정한 토큰(**교체한 OpenAI 키 포함**)은 옛 broker에게는 **없는 것**이다. 옛 broker는 설정 파일의 키만 쓰므로 **W3a 동작을 그대로 되돌리려면 설정 키 파일이 아직 있어야 한다.** 그래서 **컷오버가 안정될 때까지 `/etc/orch-broker/openai.key`를 지우지 않고 `secrets/`도 `shred`하지 않는 것을 권한다.** `secrets/`와 `tokens.json`은 롤백 때 **지우지 않아도 된다**(옛 broker는 읽지 않는다). 다시 새 broker를 올리면 그대로 이어서 쓴다.
4. **토큰 목록은 전부 아니면 전무다.** broker가 준 행 중 하나라도 이상하면(예: 손으로 고친 `tokens.json`의 한도 밖 값) `GET /api/v1/settings/tokens`가 502 `broker_error`이고 **화면에 토큰이 하나도 안 보이며 하나도 지울 수 없다.** 서버에서 `/var/lib/orch-broker/tokens.json`을 고치거나(broker를 멈추고) 옮겨야 한다. broker 자신은 깨진 `tokens.json`(문법 오류, 한도 밖 값, **2000년 이전 연도의 만료일**처럼 orchd의 규칙을 어기는 행 포함: broker와 orchd가 같은 날짜 규칙 `registrymsg.ValidExpires`를 쓴다)을 `tokens_unavailable`로 거부하고 **다른 기능(프로젝트·`refine`의 설정 키)은 계속 돈다**; 저장소에 들어 있던 키는 그동안 쓰지 않는다(`TestBrokenTokenMetaDoesNotDisableOtherFeatures`).
5. **쓰기에서 `tokens_unavailable`은 "결과를 모른다"(`outcome_unknown`, 504)다.** 저장 중 디렉터리 `fsync` 실패처럼 변경이 이미 적용된 채로 오류가 날 수 있다. **목록을 다시 읽어 확인하기 전에는 같은 변경을 다시 시도하지 않는다.** 화면은 이때 이전 목록을 보여 주지 않는다(목록을 다시 읽지 못하면 실패 화면).
6. **만료일의 하루는 UTC로 센다**(KST로는 00:00~09:00 사이에 하루 차이). **만료일은 사용자가 직접 적은 값이고 GitLab·GitHub에 물어 확인하지 않는다**(화면에 그렇게 적혀 있다). 만료 배너("토큰 N개가 14일 안에 만료됩니다")는 **브라우저가** 목록의 `days_left`와 `warn_days`(14)로 계산한다(`days_left ≤ warn_days`, 이미 지난 것 포함). 이미 지난 날짜로는 등록할 수 없다.
7. **요청 ID 정책.** 모양이 틀린 입력(이름·종류·만료일 형식, 서명 모양)은 서명 검증 전에 거부되어 **ID를 소모하지 않는다.** 서명이 한 번 유효하다고 확인되면 **ID는 소모된다**: 그 뒤 값 규칙·만료일·저장소 오류로 거부되어도 같은 서명으로 다시 보낼 수 없고 **새로 서명해야 한다.** 그래서 브라우저가 D3 값 규칙(32~512자, 인쇄 가능한 ASCII, 앞뒤 공백 없음)을 서명 전에 미리 검사한다.
8. **알려진 한계(비밀이 어디를 거치는가).** 토큰 값은 **브라우저 JS 메모리 → orchd 메모리 → broker 소켓 → broker 메모리 → 파일**을 거친다. JS 문자열은 바꿀 수 없어서 지우는 것은 참조를 버리는 것뿐이고 GC가 덮어쓸 때까지 글자가 남는다. orchd는 값을 바이트로 받아 쓰고 처리 뒤 가능한 범위에서 0으로 덮지만(최선), JSON 디코더 내부 버퍼·커널 소켓 버퍼·GC가 옮긴 사본은 지울 수 없다: **`apply` 도중 코어 덤프에는 값이 들어 있을 수 있다.** `last4`(마지막 4자)가 목록에 보인다(식별용, 알려진 노출). 값 입력칸은 평범한 `type=text`이고 글자는 투명, 점은 페이지가 그리는 겹침 요소다(알아둘 것 14). 칸의 **복사·잘라내기·끌어 놓기 시작(`copy`·`cut`·`dragstart`)은 막아 두었지만 붙여넣기는 된다.** 남는 것: 브라우저 개발자 도구와 보조기술(**스크린 리더는 입력한 글자를 읽어 주고 검토할 때 값을 읽는다 — 비밀번호 칸의 의미가 없기 때문이며, `type=password`나 `contenteditable` 없이는 막을 방법이 없어 받아들인 한계다**), 화면을 보는 사람(점은 글자 수만 보여 준다), 일부 브라우저의 우회 단축키. 이 모두 브라우저 휴리스틱을 피하려는 의도적 선택이며 보장이 아니다. **두 가지를 더 알아 둔다:** ① 브라우저의 `api.js`는 서버가 403 `csrf_failed`를 주면 세션을 한 번 새로 고치고 **같은 요청 본문(값과 서명 포함)을 한 번 다시 보낸다**(이때는 broker 호출이 일어나지 않았다). ② 유닛에 `LimitCORE=0`이 없으므로 `apply` 도중 충돌하면 systemd-coredump가 값을 담은 메모리를 저장할 수 있다(선택적 유닛 강화로 적어 두고 **적용하지 않았다**: 유닛은 이번에 바꾸지 않는다). 이 한계 전부 설계서 6장에 있고 결과 문서 6장에 다시 적었다.
9. **`gitlab`·`github` 종류의 토큰은 W3c 전까지 쓰는 코드가 없다.** 저장·목록·삭제만 된다. 기존 `create_mr`은 계속 설정의 `gitlab.token_file`을 쓴다(바뀌지 않음). 그러므로 **지금 등록해도 어떤 동작도 바뀌지 않는다.**
10. **한도(주체별).** `begin`+`message`를 합쳐 시간당 **60회**, `apply` 시간당 **20회**. 초과하면 429 `token_rate_limited`. `apply`의 슬롯은 broker에 닿기 전에 실패했을 때(연결 실패)와 broker가 일하기 전에 거절했을 때(`busy`, `forbidden`)만 돌려준다.
11. **컷오버에서만 사용자가 할 수 있는 확인(3-3, 3-5). [2026-11-10 실제 Chrome에서 `type=text`+CSS 가림이 실패했다: 아래 알아둘 것 14와 결과 문서 10장]** 브라우저에 시험용 값을 쳐서 **"비밀번호 저장" 제안이 안 뜨는지**, 글자가 가려지는지, 붙여넣기가 되는지, **실제 패스키로 `set_token`을 서명**하는 것. 이 저장소는 대역 패스키(`navigator.credentials.get` 바꿔치기)와 시험용 서명자만 썼고 실제 브라우저의 비밀번호 관리자 동작은 한 번도 보지 못했다.
12. **배포 키의 `.pub`이 없을 때.** `ssh-keygen -y -f /home/orch/orchd/ssh/id_gitlab_deploy`는 개인 키를 **읽는다**(코드가 아니라 `ssh-keygen`이). W3c 전에는 쓸 곳이 없으므로 **지금 만들지 않아도 된다.** 만든다면 사용자 결정으로 `orch` 계정에서 `ssh-keygen -y -f <개인 키> > <개인 키>.pub; chmod 0644 <개인 키>.pub`(결과가 한 줄 `ssh-ed25519 …`인지 `ssh-keygen -l -f <.pub>`로 확인)이고, 되돌리려면 `.pub`만 지운다.
13. **가짜 토큰을 진짜처럼 등록하지 않는다.** `gitlab` 종류의 더미 값을 "시험삼아" 넣지 않는다(나중에 W3c가 그 이름으로 실제 호출을 할 수 있다). 시험은 3-5의 선택지(**교체 없음 / 사용자가 가진 같은 OpenAI 키 값으로 교체**)와 직후의 삭제로 한다.
14. **Chrome 저장 제안 (실제 Chrome에서 관찰된 문제, 핫픽스).** **관찰(Chrome, 사용자, 컷오버 3-3):** `type=text` + CSS `text-security` 가림 설계에서 식별(이름) 칸에 비밀번호 자동완성 팝업이 뜨고, 더미 값을 친 뒤 "비밀번호 저장" 제안이 떴다. **원인은 추정이다**(Chrome이 글자를 가리는 칸을 비밀번호 칸처럼 다루고, "이름" 레이블 칸을 사용자 이름처럼 본다). `type=password`는 **써 본 적이 없고 배제한 것은 추론이다**(Chrome이 비밀번호 칸의 `autocomplete=off`를 무시한다). 저장되면 토큰이 브라우저·Google 비밀번호 저장소로 간다. 현재 방식: 값 칸은 평범한 `type=text`, 글자는 투명(`color: transparent`, 캐럿 보임, 강제 색상/고대비 모드에서도 `forced-color-adjust: none`: **명세에서 추론한 것이며 여기서는 확인하지 못했다**), **점은 페이지가 그리는 겹침 요소**(글자 수만큼 `•`, `aria-hidden`, 긴 값에서는 칸과 같은 가로 위치로 스크롤), `autocomplete="one-time-code"`, 중립 이름(`x-field-b`), 관리자 옵트아웃 속성, `<form>` 없음, 복사·잘라내기·끌어내기 차단(붙여넣기 가능). 식별 라벨 칸은 레이블이 "식별 라벨", `autocomplete=off`, 포커스 전까지 readonly(**페이지를 열 때나 자동 포커스 때의 팝업만 막는다. 눌러서 들어가거나 Tab으로 들어갈 때의 팝업은 못 막는다: 보조 수단일 뿐**). 만료일·삭제 확인 칸도 `autocomplete=off`와 중립 이름이다. **실제 Chrome에서 다시 확인하기 전에는 이 화면에 진짜 토큰을 넣지 않는다.** 그래도 실패하면 `contenteditable` 기반 칸으로 바꾼다(IME·맞춤법·붙여넣기·접근성을 직접 다뤄야 하는 비용). **알려진 부작용:** `one-time-code`라서 모바일 Safari/Chrome이 키보드 위에 최근 SMS·메일 인증 코드를 제안할 수 있다(유출은 아니고 의도하지 않은 제안이다). 스크린 리더는 입력한 글자를 읽는다(위 알아둘 것 8).

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 23번 6장에 "확인함/못 함"으로 남긴다)
1. **`secrets/`가 `orch-broker:orch-broker 700`으로 생기는지**(1-4): 유닛 샌드박스(`ProtectSystem=strict`, `ReadWritePaths`), 계정 `orch-broker`, `UMask=0077` 아래에서.
2. **첫 `set_token`이 서버에서 되는지**(3-5): `secrets/<ref>`가 `orch-broker:orch-broker 400`, `tokens.json`이 `600`으로 생기고 `.state-*` 임시 파일이 남지 않음.
3. **실제 패스키 서명을 broker가 `set_token`에 대해 검증하는지**(3-5). W3a는 `set_flag`에 대해서만 확인했다.
4. **실제 OpenAI 키를 웹에서 교체한 뒤 `refine`이 새 키를 쓰는지**(3-5. 사용자 결정, 한 번의 AI 호출 비용), **삭제 뒤 설정 키 폴백**(3-6).
5. **브라우저의 "비밀번호 저장" 제안이 안 뜨는지, 가림·붙여넣기**(3-3, 사용자).
6. **배포 키 `.pub` 존재와 형식**(사전 점검 d), 키보드만·스크린 리더 사용.

### 단계 1. broker 갱신 [사용자][되돌림 가능: 단계 4의 A]
`orchd`와 DB는 건드리지 않는다. **순서: 백업 → (Mac) 게이트 → 옛 broker 정지 → 새 바이너리 설치 → 샌드박스 안에서 `-check` → 시작.** `broker.json`은 바뀌지 않는다(새 설정 키 없음).

**먼저 백업(서비스 영향 없음). `/root/backup-w3b`(root 0700).** orch-vm에서:
```
B=/root/backup-w3b
sudo install -d -m 0700 -o root -g root $B $B/bin
sudo stat -c '%U:%G %a %n' $B          # root:root 700 이어야 한다
sudo cp -a /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -a /usr/local/lib/aigate/$f $B/bin/$f; done
sudo cp -a /etc/orch-broker $B/etc-orch-broker
sudo cp -a /var/lib/orch-broker $B/var-lib-orch-broker
sudo cp -a /etc/systemd/system/orch-broker.service $B/orch-broker.service.pre-w3b
sudo cp -a /etc/systemd/system/orchd.service $B/orchd.service.pre-w3b; sudo cp -a /etc/systemd/system/orchd.service.d $B/ 2>/dev/null
sudo ls -lR $B | head -40
```
(백업에는 `openai.key`(0400)와 등록부의 사본이 들어간다. 값은 화면에 나오지 않고 폴더는 root만 들어갈 수 있다. **`orch`가 쓸 수 있는 폴더에 두지 않는다.** 컷오버가 안정되면 사용자가 `sudo shred -u`로 키 사본을 지운다.)

**1-0. 빌드 전 게이트 (Mac, 저장소에서). 하나라도 실패하면 빌드하지 않는다.**
```
go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api     # --- PASS 이고 SKIP 이 없어야 한다
go vet ./... ; gofmt -l .                                         # gofmt 출력 없음
GOOS=darwin go vet ./cmd/... ./internal/... ; GOOS=linux go vet ./cmd/... ./internal/...
go test ./... -count=1 -race
node --test "internal/orch/web/test/*.test.js"
```
(결과 문서 23번 2장의 숫자와 같은 커밋인지 확인한다.)

**1-1. 새 바이너리.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w3b/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고(`uname -m`에 맞춘다) `scp`로 VM `/tmp/w3b/`에 보낸다. **아직 설치하지 않는다.**

**1-2. 옛 broker 정지 → 새 바이너리 설치.** (정지한 뒤 1-3의 점검이 실패하면 **broker는 멈춘 채로** 남는다. 그때는 곧바로 단계 4의 A로 옛 바이너리를 복원해 시작한다.)
```
sudo systemctl stop orch-broker
sudo install -o root -g root -m 0755 /tmp/w3b/orch-broker /usr/local/bin/orch-broker
```
이 시점부터 시작까지 AI 계획서와 병합 요청은 `broker_unavailable`이다(기존 기능은 그대로). 아직 시작하지 않는다.

**1-3. 샌드박스 안에서 `-check`(시작 전).** 유닛과 같은 샌드박스 옵션으로 점검한다(12장 1-4와 같은 명령):
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…)` 한 줄(`systemd-run --wait --pipe`가 `Running as unit: …` 줄 등을 함께 찍는 것은 정상)과 `Finished with result: success`(종료 코드 0). **경고는 W3a 컷오버 뒤 상태와 같아야 한다**: 옛 `context_repos`·`gitlab.projects`를 무시한다는 경고 한 줄(21번 문서 10장)은 있어도 되고, **토큰 저장소에 대한 경고는 없어야 한다**(`secrets/`·`tokens.json`이 아직 없으면 정상으로 통과한다). 새 점검이 보는 것: **`secrets/` 폴더가 있으면 소유자·모드(0700)가 올바른지(어긋나면 거부)**, **`tokens.json`이 있으면 소유자·모드(0600)·문법이 올바른지(깨졌거나 0644면 거부)**, 개별 비밀 파일의 모드(0400)·메타데이터와의 일치(어긋나면 그 토큰만 쓸 수 없다는 **경고**). **`-check`는 아무것도 만들지 않는다**(`sudo ls /var/lib/orch-broker`에 `secrets/`가 아직 없어야 한다). 실패 문장 하나가 나오면 **시작하지 않고 멈춘다 → 단계 4의 A.**

**1-4. 시작(`secrets/`가 여기서 만들어진다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/secrets /var/lib/orch-broker/registry.json
sudo ls -la /var/lib/orch-broker/secrets
sudo ls /var/lib/orch-broker/tokens.json 2>&1 || echo "(없음: 첫 set_token 전에는 정상)"
```
예상: 유닛 `active (running)`, 로그에 오류 없음. 폴더 `orch-broker:orch-broker 700`, **`secrets`가 새로 생기고 `orch-broker:orch-broker 700`**, 안은 비어 있음, `registry.json`은 W3a 그대로 `600`. `tokens.json`은 없다. **`secrets/`가 안 생겼거나 모드가 700이 아니면 멈춘다**(사전 점검 a·c를 다시 본다 → 단계 4의 A). 로그에 토큰 저장소 오류 문장이 있으면 멈춘다(broker는 시작하지만 토큰 op가 `tokens_unavailable`).

**1-5. 새 읽기 op와 회귀.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기 둘):
```
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"tokens\"}\n");print(s.makefile().readline())'
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"status\"}\n");print(s.makefile().readline())'
sudo -u orch python3 -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"projects\"}\n");print(s.makefile().readline())'
```
예상: `tokens`는 **`openai` 한 항목만, `"source":"config"`**, `"kind":"openai"`, `"last4":""`, `"expires":""`, `"days_left":null`, `"usable":true`, 그리고 `"warn_days":14`: `{"ok":true,"result":{"tokens":[{"days_left":null,"expires":"","kind":"openai","last4":"","ref":"openai","set_at":"","source":"config","usable":true}],"warn_days":14}}`(키 순서는 Go의 맵 출력이라 사전순일 것으로 읽은 것이고 실제 출력은 미확인). 응답에 키 값이 없어야 한다. `status`는 W3a와 같은 `context_repos`에 `tokens_available`(true)·`warn_days`·`expiring`(빈 목록)이 더해진다. `projects`는 W3a 컷오버 뒤와 같다(`toy`·`sample-app`). **`openai`가 두 번 나오거나 다른 항목이 있거나 `source`가 `store`면 멈춘다 → 단계 4의 A.** **회귀(기존 `refine`):** `tokens`의 `openai` 행이 `source:config, usable:true`라는 것은 broker가 설정 키 파일을 호출 경로로 읽을 수 있다는 뜻이다. 실제 AI 호출은 비용이 들어 3-2에서 사용자가 정한다.

**검증 체크리스트:** 1-3 통과(토큰 경고 없음, `secrets/` 아직 없음), 1-4의 유닛 active·`secrets` 700·`registry.json` 600, 1-5의 `tokens`(정확히 `openai` 한 항목, source config)와 `status`·`projects`가 W3a 컷오버 뒤와 같음. **그래도 안 되면:** 단계 4의 A. 다른 서비스에는 영향이 없다(이 단계는 `orchd`를 건드리지 않았다).

### 단계 2. `orchd` 갱신 [사용자]
**DB 스키마가 바뀌지 않으므로 DB를 복원할 필요가 없다**(이번 백업은 `/root/backup-w3b/bin`의 바이너리뿐). 실행 중·대기 작업이 없음을 먼저 확인한다(`orch list`).
```
sudo systemctl stop orchd
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3b/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
```
유닛과 드롭인(`--broker-socket`, `--git-ssh-key`)은 바꾸지 않는다. **검증 체크리스트**
- 로그: 오류 없이 대기, `approvals: mode signed`, 스키마 오류 없음.
- `orch list`: 단계 1 이전의 작업이 모두 그대로, 마지막 번호가 같다.
- 새 엔드포인트가 떠 있고 로그인 없이는 닫혀 있는지: `curl -sk -o /dev/null -w '%{http_code}\n' https://<FQDN>/api/v1/settings/tokens` → **401**(같은 방식으로 `/api/v1/settings/deploy-key`, `POST /api/v1/tokens/begin`도 401). 404면 옛 바이너리다.

**그래도 안 되면:** 단계 4의 B(바이너리만 되돌리면 된다. DB는 그대로 호환된다).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/tokens.test.js`, `internal/orch/api/tokens_test.go`, `internal/broker/tokens_test.go`, `adversarial_w3b_test.go`)에서 읽은 것이다.

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | 메뉴 "토큰"(`#/tokens`) | 표에 **`openai` 한 줄**: 종류 `openai`, 마지막 4자 비어 있음, 만료일 없음, 출처 "설정 파일", **삭제 단추 없음**, "교체" 가능. 상단 만료 배너 **없음**. 주소 표시줄은 `#/tokens` 그대로. 프로젝트 화면(`#/projects`)도 배너 없음 | 목록이 안 뜨면 `broker_unavailable`(단계 1-4·1-5), `broker_error`(목록의 한 행이 이상함: 알아둘 것 4). 메뉴가 없으면 브라우저 캐시(새로고침) |
| 3-2 | **회귀: AI 계획서.** "새 작업" → "AI로 계획서 만들기", 저장소 `toy`, 한 줄을 적고 미리보기까지 | 12장 3-2와 같다(보낼 파일 목록, 제외 사유). **[사용자 결정] 실제 AI 호출까지 해 볼지**: 비용이 든다(소액). 하면 계획서가 나온다 = 설정 키를 broker가 호출 경로로 읽는다 | `ai_key_missing`("AI 키가 등록되어 있지 않습니다…")이면 설정 키 파일을 읽지 못한 것(사전 점검 b) → 단계 4의 A |
| 3-3 | **[사용자] 입력칸 점검(서명하지 않는다).** "토큰 추가"에서 종류 `gitlab`, 이름 `check-only`, **임의의 더미 문자열 32자 이상**을 값 칸에 직접 입력하고 붙여넣기도 해 본다. 만료일은 먼 미래. 그 뒤 **"취소"**(또는 화면 이동). **Chrome으로 다시 확인(핫픽스 뒤, 진짜 토큰 금지)** | ⓐ **"식별 라벨" 칸을 마우스로 누르고, 또 Tab으로 들어가도 비밀번호 자동완성 팝업이 없다(둘 다 필수)**, ① **글자가 점으로 보인다**(입력 중·붙여넣은 뒤. 점은 페이지가 그린 것), ② **붙여넣기가 된다**, ③ **"비밀번호 저장/업데이트/강력한 비밀번호 제안"이 값을 친 뒤·취소·화면 이동 어느 때에도 안 뜬다**(`chrome://password-manager/passwords`에도 새 항목이 없다). 모바일이면 키보드 위에 SMS/메일 코드 제안이 떠도 유출은 아니니 기록만 한다, ④ 확인 화면(마지막 4자와 값 해시 앞 8자만)에서 값 칸이 비어 있고 전체 값이 다시 보이지 않는다, ⑤ 취소하면 아무것도 저장되지 않는다(목록 그대로, `sudo ls /var/lib/orch-broker/secrets`가 비어 있음) | ③이 보이면 **기록하고(어느 브라우저인지) 그 값은 거절한다.** 점이 안 보이면 기록(겹침 요소 문제). 값이 DOM에 남는다고 보이면 멈추고 보고 |
| 3-4 | **서명 거부 확인.** 잘못된 서명·바꿔치기된 값은 **자동 시험으로만 확인**한다(실서버에서 일부러 위조하지 않는다) | 시험: `TestValueSwapAfterSignatureRefused`, `TestSetTokenForOtherRefOrKindRefused`, `TestApprovalAndRegistryMessagesStillSeparate` 등(결과 문서 23번 2장) | — |
| 3-5 | **[사용자 결정] 실제 서명으로 `set_token`.** 선택지 **(a) 하지 않는다**(권장 기본: 컷오버를 닫는다), **(b) OpenAI 키 교체**: 종류 `openai`, 이름 `openai`, 값은 **사용자가 직접 입력한다(이 대화·채팅·터미널에 붙이지 않는다)**, 확인 화면에서 마지막 4자가 맞는지 보고 패스키로 서명. **⚠ 키를 오타 없이 입력해야 한다(알아둘 것 0: 틀리면 일하는 설정 키를 가려 `refine`이 예산 예약만 태운다. 서명 전에 확인 화면의 마지막 4자를 원본과 눈으로 대조하고, 이상하면 취소한다).** **설정 키와 *같은* 값으로 교체하면 핫 리로드나 저장소 우선순위는 확인되지 않는다**(저장소 키든 설정 키든 같은 결과). 같은 값으로 교체해서 실제로 확인되는 것은 세 가지뿐이다: ① **실제 패스키 `set_token` 서명을 broker가 검증함**, ② **0400 파일 쓰기 + `rename`이 실서버에서 됨**, ③ 목록에 **`source: store`**로 나옴. 핫 리로드·저장소 우선순위를 진짜로 보려면 **설정 키와 다른, 유효한 OpenAI 키**가 필요하다(사용자 결정: 그 키가 있을 때만, 그리고 키가 유효한지는 위 한 번의 AI 호출로만 알 수 있다). **가짜 토큰을 등록하는 선택지는 없다**(알아둘 것 13) | (b)이면 "토큰을 저장했습니다", 목록의 `openai` 행이 **출처 "저장소"**, 마지막 4자, 삭제 단추 생김. 서버: `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/secrets/openai /var/lib/orch-broker/tokens.json` → **`orch-broker:orch-broker 400`**과 **`600`**, `sudo ls -la /var/lib/orch-broker/secrets`에 `.state-*` 임시 파일 없음. 원장에 `token_change`(행위자·동작·`openai`·종류·성공 여부·코드 `ok`뿐). **같은 값으로 교체했다면 이 호출은 "키가 계속 유효하다"만 보여 준다. *다른 유효한 키*로 교체했을 때만 계획서가 나온다는 것이 "교체한 뒤 `refine`이 새 키를 쓴다"의 첫 확인이다.** 이것이 broker가 실제 패스키 서명을 `set_token`에 대해 검증하는 첫 확인이기도 하다 | `bad_signature`/`unknown_signer`/`signers_unavailable`/`stale_signature`: 서명자 목록, 패스키 등록, 시계(5분)·확인 화면에서 서명까지 걸린 시간. `tokens_unavailable`은 `outcome_unknown`(알아둘 것 5): 목록을 다시 읽어 확인한 뒤 판단. **같은 변경을 곧바로 다시 시도하지 않는다**(화면은 새 서명을 요구한다) |
| 3-6 | **[3-5(b)를 했다면] 시험 흔적을 지워 원래 상태로.** 목록의 저장소 `openai` 행 "삭제" → 이름 `openai`를 입력해 확인 → 패스키 서명 | 목록의 `openai`가 **출처 "설정 파일"**로 돌아간다(삭제 폴백). 서버: `sudo ls -la /var/lib/orch-broker/secrets`가 비어 있음. 3-2의 AI 호출(해 본다면)이 여전히 된다(설정 키). 3-5의 값이 틀렸다면 이 삭제가 복구다 | 삭제 뒤 `ai_key_missing`이면 설정 키 파일을 읽지 못하는 것 → 단계 4의 A(사전 점검 b). 삭제는 파일을 **0으로 한 번 덮은 뒤 unlink**한다(`shred`가 아니다. 최선의 노력이고 SSD·COW 파일 시스템에서는 옛 블록이 남을 수 있다) |
| 3-7 | **브로커를 멈춘 상태**(선택): `sudo systemctl stop orch-broker` → `#/tokens`를 연다 | 고정 오류(브로커에 연결할 수 없음). 작업 제출·취소·AI 아닌 기능은 그대로. 끝나면 `sudo systemctl start orch-broker`(`secrets/`·`tokens.json`은 그대로 이어서 쓴다) | 기존 기능까지 막히면 단계 4의 B |
| 3-8 | **[컨트롤러] 확인(읽기 전용)** | `sudo sh -c 'stat -c "%U:%G %a %n" /var/lib/orch-broker /var/lib/orch-broker/secrets /var/lib/orch-broker/*.json'`: 폴더 700, 파일 600(3-5를 했고 3-6을 안 했다면 `secrets/openai`만 400). `sudo grep token_change /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl \| tail -3`: **값·해시·마지막 4자·요청 ID가 없는 줄만**. `sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager`을 훑어 토큰 값·`Authorization` 줄이 없는지(값을 알고 있는 사용자가 마지막 4자로 `grep -c`를 해도 된다) | 값이나 해시가 보이면 **멈추고 보고한다.** 그 값(해당하면 OpenAI 키)을 즉시 폐기·재발급하는 것이 안전하다(사용자 결정) |

**검증 체크리스트(요약):** 3-1이 예상대로, 3-2 회귀 없음, 3-3의 네 가지(가림·붙여넣기·저장 제안 없음·확인 화면에서 빈 칸), 3-5·3-6 중 사용자가 한 것이 예상대로, 3-7에서 기존 기능이 살아 있고, 3-8의 모드와 원장. 사전 점검 (d)(`.pub`)와 3-3·3-5의 결과는 결과 문서 6장에 "컷오버에서 확인함/못 함"으로 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다. **저장소 토큰이 의도 없이 바뀌었거나, 원장·로그·화면에 값(또는 해시·서명)이 보이거나, `secrets/`·`tokens.json`의 모드가 어긋났으면 즉시 broker를 멈추고(단계 4의 A) 사용자에게 보고한다.**

### 단계 4. 롤백 [사용자][주의]
**A. broker를 W3a 상태로 되돌리기(DB는 건드리지 않는다).**
1. `sudo systemctl stop orch-broker`
2. 바이너리를 **root 전용 백업에서** 복원: `sudo install -o root -g root -m 0755 /root/backup-w3b/bin/orch-broker /usr/local/bin/orch-broker`. `broker.json`은 이번에 바뀌지 않았으므로 복원할 필요가 없다(바꿨다면 `/root/backup-w3b/etc-orch-broker/broker.json`에서만 복원한다).
3. `sudo systemctl start orch-broker`; 12장 1-6의 `status`·`projects` 호출이 W3a 상태(`toy`·`sample-app`)를 주는지 본다.
4. **경고(알아둘 것 3):** **옛 broker는 `secrets/`와 `tokens.json`을 읽지 않는다.** 컷오버 뒤 웹으로 등록·교체한 토큰은 옛 broker에게는 없다. 교체한 OpenAI 키가 있었다면 옛 broker는 **설정 파일의 키**로 돌아간다. **그래서 `/etc/orch-broker/openai.key`가 있어야 *그리고 아직 유효해야* W3a 동작이 그대로 돌아온다**(없으면 옛 broker는 시작을 거부한다. 웹으로 새 키를 정한 뒤 옛 키를 OpenAI에서 폐기했다면 옛 broker는 그 폐기된 키로 `upstream_error`를 내며 알아둘 것 0과 같이 예산 예약을 태운다: 롤백 전에 설정 파일의 키가 아직 유효한지 먼저 정한다). `secrets/`·`tokens.json`은 **지우지 않는다**(옛 broker는 무시하고, 다시 새 broker를 올리면 이어서 쓴다). 지우고 싶으면(예: 값이 새어 폐기한 뒤) broker를 멈춘 상태에서 `sudo find /var/lib/orch-broker/secrets -type f -exec shred -u {} +; sudo rm -f /var/lib/orch-broker/tokens.json`(`secrets/` 폴더는 둔다: 새 broker가 없으면 만든다). 그러면 웹에서 등록한 토큰이 모두 사라지고 `openai`는 설정 키로 돌아간다. 문제가 시작되기 전 복사본은 `/root/backup-w3b/var-lib-orch-broker`에 있다(**복원하지 않는다**: 폴더 전체를 되돌리면 예산 기록·등록부·쓴 요청 ID도 되돌아간다).

**B. `orchd`·CLI를 W3a 상태로 되돌리기(DB 복원 불필요).**
1. `sudo systemctl stop orchd`
2. `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3b/bin/$f /usr/local/lib/aigate/$f; done`(세 개는 항상 같이. **웹은 `orchd`에 들어 있으므로** 토큰 메뉴와 만료 배너도 함께 사라진다)
3. `sudo systemctl start orchd`. DB 스키마는 이번에 바뀌지 않았으므로 복원이 필요 없다(브라우저 캐시면 새로고침).

**검증:** `journalctl -u orchd`·`journalctl -u orch-broker`에 오류가 없고, `orch list`가 단계 2 이전과 같으며, W3a의 프로젝트 화면(12장 3-1)과 AI 계획서 미리보기(12장 3-2)가 동작한다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`, `ls -l /usr/local/bin /usr/local/lib/aigate /var/lib/orch-broker`와 `sudo ls -lR /root/backup-w3b | head -40`을 사용자에게 보고한다. W3a 이전까지 가야 하면 12장 단계 4(`/root/backup-w3a`).

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 0 사전 점검(상태 폴더 0700, 키 파일 읽기, 유닛 샌드박스 값, 배포 키 `.pub`) | **컨트롤러**가 읽기 전용으로 확인(유닛 변경 없음) | — | — |
| 1 broker 갱신 | 승인 | A(바이너리를 `/root/backup-w3b`에서 복원. `secrets/`·`tokens.json`은 둔다. 컷오버 뒤 웹으로 정한 토큰은 옛 broker에게 없음) | broker만 |
| 2 `orchd` 갱신 | 승인 | B(바이너리만, DB 복원 불필요) | 필요(stop/start) |
| 3 웹 확인 | **사용자 직접**(로그인, 입력칸 점검, **실제 서명·키 교체 여부 결정**, 값은 직접 입력) | 등록·교체한 토큰은 3-6의 삭제로 되돌림(OpenAI는 설정 키로 폴백) | 불필요(3-7만 broker 재시작) |
| 4 롤백 | 승인 | — | 필요 |

## 14. W3c 컷오버 런북 (제공자 설정·새 프로젝트 생성·복제, `orch-broker`·`orchd` 갱신)
설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `24-PLAN-webconsole-W3c.md`, 결과·시험·한계는 `25-RESULTS-webconsole-W3c.md`(**배포 현실 표**와 "실서버에서 확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 GitLab·가짜 broker·임시 폴더·가짜 `git` 실행 파일)을 읽고 쓴 것이지 서버에서 본 출력이 아니다(11~13장과 같은 원칙). **W3c는 처음으로 사내 GitLab에 실제 쓰기 호출(그룹·프로젝트·파일·보호 브랜치·배포 키 등록)을 하는 단계다.** 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려한다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[컨트롤러]** 읽기 전용 확인(서버 상태를 바꾸지 않음), **[되돌림 가능]** 아래 몇 줄로 원상 복구, **[주의]** 되돌리면 그 사이의 변경을 잃는다. 경로는 11~13장과 같다(바이너리 `/usr/local/lib/aigate`와 `/usr/local/bin/orch-broker`, 설정 `/etc/orch-broker/broker.json`, 상태 `/var/lib/orch-broker`, broker 소켓 `/run/orch-broker/broker.sock`, orchd 설정 `/home/orch/orchd`, 저장소 `/home/orch/repos`). **orchd DB 스키마는 이번에 바뀌지 않는다(v8 그대로)**: 롤백에 DB 복원이 필요 없다. **유닛(`orch-broker.service`, `orchd.service`)·orchd 플래그·`broker.json`은 바뀌지 않는다**(새 설정 키·새 플래그 없음: 게시 원격 해석기는 이미 있는 `--broker-socket`·`--git-remote`·`--git-ssh-key`·`--git-ssh-known-hosts`가 모두 있으면 켜진다). 새로 생기는 것: **등록부 `registry.json`이 버전 2로 올라가며 `providers`·`orphans`와 프로젝트의 `provider`·`gitlab.project_id`를 담는다(아래 ⚠ 2)**, broker op `providers`·`list_groups`·`set_provider`·`delete_provider`·`create_group`·`create_project`, orchd의 `/api/v1/providers`·`/providers/{id}/groups`·`/providers/propose|apply`·`/projects/create/propose|apply`·`/projects/{id}/clone`, 웹의 `#/providers` 화면과 `#/projects`의 "새 프로젝트 만들기", orchd가 `/home/orch/repos/<이름>`(과 복제 도중에만 `.clone-<무작위>`)을 만드는 것.

> **⚠ 먼저 읽을 것 (결정과 위험).**
> 1. **이 도구는 GitLab의 그룹·프로젝트를 지우지 못한다(삭제 코드가 없다).** 3-4에서 만든 시험용 그룹·프로젝트는 **사용자가 GitLab 화면에서 직접 지워야** 한다. 그리고 **만든 프로젝트는 이 도구의 등록부에서도 지울 수 없다**(등록 해제 op가 없다: W3a~W3c 어디에도). 시험용이라도 등록부 항목과 `/home/orch/repos/<이름>` 폴더가 남는다(AI 전송은 기본 꺼짐이라 해롭지는 않다). 이름을 신중히 정한다.
> 2. **등록부가 버전 2로 올라간다. W3b 이하 broker는 버전 2 파일을 "깨짐"으로 보고 fail closed 한다(등록부에 의존하는 모든 op가 `registry_unavailable`).** 그래서 **롤백은 바이너리만 되돌려서는 안 되고 `registry.json`을 `/root/backup-w3c`의 버전 1 사본으로 되돌려야 한다**(단계 4의 A). 새 broker가 첫 시작에서 파일을 올릴 때 **옛 broker가 같은 상태 폴더에서 돌고 있으면 안 되므로**(버전 1을 읽는 일과 버전 2로 바꾸는 일 사이에 잠금이 없다) **옛 broker를 먼저 멈춘다**(1-2).
> 3. **관리자 토큰의 영향 범위.** broker가 GitLab에 보내는 호출은 코드에 고정된 9가지(그룹 읽기·만들기, 프로젝트 확인·만들기, 파일 만들기, 보호 브랜치 읽기·만들기, 배포 키 등록, 그룹 목록)뿐이고 삭제·변경·병합·토큰 호출은 코드에 없다(시험으로 고정). 그래도 **토큰 자체는 그 사용자의 권한을 모두 가진 `api` 범위 토큰**이다: 한 번 유출되면 위 9가지 밖의 일도 GitLab에서 할 수 있다. 만료일을 두고(사용자 결정), 시험이 끝나면 불필요한 권한은 줄이고, 의심되면 GitLab에서 즉시 폐기한다. 토큰 값은 **사용자가 웹 토큰 화면에 직접 입력한다. 이 대화·채팅·터미널에 붙이지 않는다.**
> 4. **첫 커밋은 원자적이지 않다.** 빈 저장소에 커밋 API가 안 된다는 보고(GitLab 이슈 #577329, 실서버 19.0.8에서 확인하지 못함) 때문에 파일 API로 템플릿 파일을 **하나씩** 올린다(`README.md`가 먼저). 중간에 실패하면 일부 파일만 있는 프로젝트가 GitLab에 남고 화면은 `create_partial`(단계 `first_commit`)과 고아 프로젝트 경로를 알려 준다. 재개는 없다: 사용자가 GitLab에서 지운 뒤 같은 이름으로 다시 만든다.
> 5. **같은 배포 키를 여러 프로젝트에 등록하는 것: 키 `id_gitlab_deploy`는 이미 `example-group/sample-collector`에 등록되어 있다.** GitLab 문서상 이미 있는 공개 키를 다른 프로젝트에 추가하는 것은 지원된다(재사용 자체는 가능). 다만 **GitLab은 토큰의 사용자가 그 키가 등록된 원래 프로젝트(`example-group/sample-collector`)에 접근할 수 있을 때만 기존 키에 붙여 준다.** 접근할 수 없으면 **첫 프로젝트부터** `create_partial`(단계 `deploy_key`)로 끝나고 GitLab에 고아 프로젝트가 남는다(아래 사용자 사전 작업 2의 접근 확인). 실제 동작은 시험으로 확인할 수 없고 컷오버에서만 드러난다. 같은 키를 두 번째 프로젝트에 붙이는 것이 다른 이유로 거부되면 GitLab의 "배포 키를 이 프로젝트에 켜기" 호출이 필요한데 **허용 목록 밖**이라 코드 변경과 결정이 필요하다.
> 6. **기본 템플릿(`basic`)의 `.aigate.yml`은 `checks: []`다.** 시험은 이것이 게이트 설정 파서를 통과함을 확인하지만 **빈 검사 목록이 "통과"로 읽히는 곳이 있는지는 확인하지 않았다.** W3c의 자동 병합은 없다(항상 꺼짐). W3d에서 자동 병합을 만들 때 **빈 `checks`를 통과로 세지 않아야 한다**(이월 항목).

### 14.0 시작 전에 모두 참이어야 하는 것
- **W3b 컷오버가 끝난 상태가 출발점이다**(23번 문서 11장, 2026-11-10): broker `active`(등록부 버전 1, 프로젝트 `toy`·`sample-app`), `secrets/`·`tokens.json`(웹에서 등록한 토큰이 아직 없으면 `secrets/`는 비어 있음), `orchd`가 `--broker-socket`으로 도는 중, DB 스키마 v8, 서명 필수 모드. **실행 중이거나 대기 중인 작업이 없다**(`orch list`, `orch approvals`): 단계 1은 broker를, 단계 2는 `orchd`를 재시작한다.
- **웹 서명에 쓸 패스키 서명자가 등록되어 있다**(`sudo orch admin signers list`에 `webauthn` 형식). **W3b의 3-5(실제 서명으로 `set_token`)는 컷오버에서 하지 않았으므로 실제 패스키로 만든 `set_token` 서명을 broker가 검증하는지, `secrets/<이름>` 0400 쓰기와 `rename`이 서버에서 되는지는 아직 한 번도 확인하지 못했다.** 관리자 토큰 등록(아래 사용자 사전 작업 2)이 그 첫 실제 사용이다. 안 되면 거기서 멈춘다.
- **백업은 `/root/backup-w3c`(root 소유 0700)에만 둔다.** `orch`가 쓸 수 있는 폴더에는 두지 않는다(13장과 같은 이유).
- **사전 점검 (단계 1 전에, 전부 읽기 전용이고 값·개인 키를 찍지 않는 명령. 컨트롤러가 서버에서 직접 한다).** 하나라도 예상과 다르면 **시작하지 않고** 보고한다.
  - **(a) 등록부와 상태 폴더.** `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json` → 폴더 **`orch-broker:orch-broker 700`**, 파일 `orch-broker:orch-broker 600`. `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → **`"version":1`**(이미 2라면 누군가 새 broker를 먼저 시작한 것이니 멈추고 확인한다). `sudo ls /var/lib/orch-broker`에 `registry.json`, `used-requests.json`, `budget.json`, `secrets`, (있다면) `tokens.json`이 있다.
  - **(b) `repo_roots`의 첫 항목.** `sudo grep -n -A3 repo_roots /etc/orch-broker/broker.json` → **첫 항목이 `/home/orch/repos`**(글자 그대로). 새 프로젝트의 경로는 broker가 **`repo_roots[0]` + 이름**으로 만든다. 첫 항목이 다르면 모든 `create_project`가 `path_not_allowed`다. `sudo readlink -f /home/orch/repos`가 `/home/orch/repos`와 같은지, orchd 정책의 `repo_roots`(`sudo grep -n repo_roots /home/orch/orchd/policy.yml`)에도 같은 철자가 있는지.
  - **(c) orchd가 `/home/orch/repos`에 새 폴더를 만들 수 있는지.** `sudo stat -c '%U:%G %a %F' /home/orch/repos` → **`orch:orch 700 directory`**(심볼릭 링크가 아님). `systemctl show orchd -p User -p ReadWritePaths -p ProtectSystem -p ProtectHome -p PrivateTmp` → `User=orch`, `ReadWritePaths`에 **`/home/orch/repos`와 `/home/orch/orchd`**, `ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp=yes`(복제는 임시 폴더 `orch-clone-*`를 HOME으로 쓴다). `df -h /home/orch/repos`로 여유 공간. **시험으로는 이것을 보증하지 못했다**(시험은 임시 폴더에서 같은 사용자로만 돌았다). 쓰기 가능성은 3-4의 첫 복제에서 실제로 드러난다.
  - **(d) 배포 키와 `.pub`(W3b 사전 점검 d와 같은 방법, 개인 키는 읽지 않는다).** `sudo stat -c '%U:%G %a %s %n' /home/orch/orchd/ssh/id_gitlab_deploy /home/orch/orchd/ssh/id_gitlab_deploy.pub` → 개인 키 `orch:orch 600`, `.pub`은 일반 파일(링크 아님) 4096바이트 이하. **`.pub`이 한 줄이고 주석(셋째 칸)이 100자 이하이며 공백이 없는지**: `sudo awk '{print NF" fields, comment length "length($3)}' /home/orch/orchd/ssh/id_gitlab_deploy.pub`(예상 `3 fields, comment length N`, N ≤ 100). broker가 받는 공개 키 모양(`ssh-ed25519`·`ssh-rsa`·`ecdsa-sha2-nistp*`·`sk-ssh-ed25519@openssh.com` + base64 + 선택 주석)과 다르면 **서명 전에** `deploy_key_unavailable`로 막힌다. 지문: `sudo ssh-keygen -l -f /home/orch/orchd/ssh/id_gitlab_deploy.pub`(웹 확인 화면의 지문과 같아야 한다).
  - **(e) `known_hosts_gitlab`.** `sudo stat -c '%U:%G %a %n' /home/orch/orchd/ssh/known_hosts_gitlab` → 소유자 `orch`, **그룹·기타 쓰기 불가**. **제공자 ssh 주소의 호스트가 이 파일에 "정확히" 있어야 한다**: `sudo ssh-keygen -F 100.64.0.10 -f /home/orch/orchd/ssh/known_hosts_gitlab`(ssh 포트가 22가 아니면 `'[100.64.0.10]:포트'`). **`ssh-keygen -F`는 와일드카드 줄도 보여 주므로 출력의 첫 열이 정확히 `100.64.0.10`(또는 `[100.64.0.10]:포트`) 또는 `|1|…`인지 눈으로 확인한다**(`*`·`?`·`!`가 든 첫 열은 고정이 아니다). 평문 항목과 해시(`|1|`) 항목만 인정하고 **와일드카드·`!`·`@cert-authority`·`@revoked` 항목은 고정으로 치지 않는다**: 그런 줄만 있으면 복제는 `host_not_pinned`로 시도조차 하지 않는다.
  - **(f) `git`·`ssh` 실행 파일.** orchd는 시작할 때 **`/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin`에서만** 찾아(환경 변수는 보지 않는다) 절대 경로로 쓴다: `sudo ls -l /usr/bin/git /usr/bin/ssh`(둘 다 있어야 한다. 없으면 복제는 `git_missing`·`ssh_missing`이고 프로젝트 생성 자체는 성공한다). `sudo -u orch sh -c 'git --version; ssh -V'`.
  - **(g) 도달 가능성(방화벽을 바꾸지 않는다).** broker(uid `orch-broker`)→사내 GitLab HTTP `100.64.0.10:18082`: `sudo nft list ruleset | grep -n 18082`로 허용 규칙을 보고, 샌드박스에서 인증 없는 읽기 한 번: `sudo systemd-run --wait --pipe -p User=orch-broker -p ProtectSystem=strict -p ProtectHome=yes /usr/bin/curl -s -o /dev/null -w '%{http_code}\n' http://100.64.0.10:18082/api/v4/version`(예상 `401` 또는 `200`: 닿는다는 뜻이고 토큰은 쓰지 않는다. **다른 주소를 제공자로 쓰려면 새 방화벽 규칙이 필요하고 이 절차는 방화벽을 바꾸지 않는다.**) orchd(`orch`)→GitLab ssh 22: `sudo -u orch ssh -F /dev/null -i /home/orch/orchd/ssh/id_gitlab_deploy -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o UserKnownHostsFile=/home/orch/orchd/ssh/known_hosts_gitlab -o ConnectTimeout=10 git@100.64.0.10`(GitLab은 환영 문장을 찍고 끊는다. **ssh가 `orch` 계정으로 개인 키를 읽는다**: 게시가 이미 하는 일과 같다. 거부되면 배포 키가 GitLab에 등록되어 있는지·방화벽을 본다).
  - **(h) 정책 클래스 `registered`.** `sudo grep -n -B1 -A2 registered /home/orch/orchd/policy.yml` → 지금은 **없어야 한다**(있으면 누군가 이미 넣었다). **새 프로젝트의 `kind`는 `registered`이고, 정책에 이 클래스가 없으면 orchd가 제안 단계에서 서명 전에 `policy_class_missing`으로 거부한다**(없으면 기본 단계 L0). 추가는 사용자 결정이다(아래 사용자 사전 작업 1).
  - **(i) 샌드박스 안 `-check`.** 단계 1-3에서 새 바이너리로 한다. 사전 점검은 **현재(W3b) 바이너리로도** 같은 명령을 한 번 돌려 두면 비교가 쉽다(1-3의 명령).
  - **(j) 아무것도 돌고 있지 않다.** `orch list`, `orch approvals`. 그리고 지울 프로젝트(`w3e-smoke-1112`·`1113`)의 모든 작업 상태가 `done`·`failed`·`cancelled` 중 하나인지 `orch show <번호>`로 본다. `error`·`passed`·`needs_approval`·`publishing`은 "끝난" 것이 아니라 `project_busy`의 원인이다: 3-2 전에 `orch cancel`로 끝낸다(작업 #47이 여기 해당할 수 있다).
  - **(k) GitLab의 기본 브랜치 보호 설정.** 인스턴스 설정(관리자 영역 > 설정 > 저장소 > 기본 브랜치)과 **새 프로젝트를 만들 그룹의 같은 설정** 모두 **"완전 보호(Fully protected)" 또는 "보호 안 함(Not protected)"** 이어야 한다(사용자가 GitLab 화면에서 확인한다). "개발자가 푸시 가능" 같은 더 약한 값이면 GitLab이 `main`에 약한 보호 규칙을 자동으로 만들고, 이 도구는 약한 기존 규칙을 고치지 않으므로(고칠 호출이 허용 목록에 없다) **모든 생성이 `create_partial`(단계 `protect`)로 끝나 지울 수 없는 고아 프로젝트만 남긴다.** 이 설정을 확인하지 못했거나 약하면 **3-4(c)를 하지 않는다**(3-1~3-3까지만 하고 닫는다).
  - **(l) 같은 이름의 폴더가 없다.** `sudo ls -a /home/orch/repos`에 3-4에서 쓸 프로젝트 이름이 **없어야** 한다(이제 orchd가 그런 이름의 생성을 `path_exists`로 서명 전에 거부하지만 미리 본다). `.clone-*` 항목도 없어야 한다.
  - **(m) orchd에 옛 GitLab MR 플래그가 없다.** `systemctl cat orchd | grep -cE -- '--gitlab-(url|project|token-file)'`가 **`0`**을 찍어야 한다(그 플래그가 있으면 게시 경로에 옛 초안 MR 호출이 켜진다: W3d 전까지 W3c와 섞지 않는다).
  - **서버에서 쓰기 시험을 따로 하지 않는다.** 위 점검으로 알 수 없는 것(첫 `set_provider` 서명 검증, GitLab의 실제 응답, 복제 권한)은 단계 3에서 처음 드러난다.

**사용자 사전 작업 (컨트롤러가 대신할 수 없다).**
1. **[사용자 결정] 정책에 `registered` 클래스를 넣는다.** `/home/orch/orchd/policy.yml`의 `classes:` 아래에 기존 `toy`·`sample-app`와 같은 형식으로 `registered: {level: L1}` 한 줄(`sudo cp -a`로 먼저 백업: `/root/backup-w3c/policy.yml.pre-w3c`). **정책 해시가 바뀐다**(정책은 작업이 시작될 때 다시 읽고, 게시 때 해시가 시작 때와 다르면 사람 승인으로 넘어간다): **그러므로 실행 중인 작업이 없을 때 한다**(14.0의 조건). `orch policy`는 **로컬 명령**이고 데몬에 묻지 않는다(`--socket`을 무시하고, `sudo`는 환경 변수를 지우므로 `--policy`를 직접 준다): `sudo /usr/local/lib/aigate/orch policy show --policy /home/orch/orchd/policy.yml`은 **파일만** 검증하며 파일을 고치자마자 `registered`가 보인다(데몬이 읽었다는 뜻이 아니다). **실행 중인 orchd는 정책을 시작할 때와 작업이 시작될 때 읽으므로, 파일을 고친 뒤 orchd를 재시작해야 한다**(단계 2에서 어차피 재시작한다). 데몬이 실제로 `registered`를 아는지는 웹의 정책 화면(`GET /api/v1/policy`, 로그인 필요)의 클래스 목록에 `registered`가 있는지, 또는 `#/projects`의 새 프로젝트 제안이 `policy_class_missing`이 아닌 결과(확인 화면)를 내는지로 본다. L0로 두려면 이 줄을 넣지 않는다(그러면 프로젝트 생성을 제안할 수 없다).
2. **관리자 토큰.** GitLab에서 **새 프로젝트를 만들 그룹의 Maintainer 이상이고 그룹을 만들 권한이 있는 사용자의** 개인 액세스 토큰을 `api` 범위로 만들고 **만료일을 둔다**. **그 사용자는 `example-group/sample-collector`에도 접근할 수 있어야 한다(관리자이거나 구성원)**: 배포 키가 거기에 등록되어 있어서, 접근할 수 없으면 모든 생성이 `create_partial`(단계 `deploy_key`)로 끝난다(⚠ 5). 먼저 13장 3-3의 입력칸 점검(더미 값, 서명하지 않고 "취소")을 **항목별로** 하고 결과를 기록한다(23번 문서 11장: W3b 컷오버의 사용자 보고는 항목별이 아니었다). 이상이 없으면 `#/tokens`에서 종류 **`gitlab`**, 이름(예 `gitlab-admin`, 소문자·숫자·`-`), 값, 만료일을 **사용자가 직접 입력**하고 확인 화면의 마지막 4자를 GitLab 화면과 눈으로 대조한 뒤 패스키로 서명한다. 그 뒤 서버에서 `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/secrets/<이름> /var/lib/orch-broker/tokens.json` → **`orch-broker:orch-broker 400`과 `600`**(W3b가 확인하지 못한 첫 실제 쓰기)이고 목록에 **출처 "저장소"**로 보인다. 안 되면 **멈춘다**(W3c 단계로 가지 않는다).
3. **새 프로젝트를 만들 그룹을 정한다 — 새 전용 그룹을 권장한다**(예 `aigate-projects`: 이 도구가 만든 프로젝트를 한곳에 모으고 정리를 쉽게 한다). 기존 그룹도 가능하다(`min_access_level=40`, Maintainer 이상인 그룹만 목록에 나온다). 그룹을 GitLab 화면에서 직접 만들어도 되고 3-4에서 웹으로 만들 수도 있다(웹으로 만든 그룹도 지울 수 없다).
4. **[사용자 결정] 시험용 이름과 게시 권한.** 3-4에서 만들 프로젝트의 이름(예 `w3c-smoke-날짜`: `^[a-z][a-z0-9-]{1,39}$`)과 게시 권한(**읽기**: 이 서버는 브랜치를 올리지 못함 / **쓰기**: 3-6에서 push까지 보려면 필요). 둘 다 정하지 않았으면 3-4는 하지 않는다.

**알아둘 것 (코드 읽기와 시험 기준, 실서버 미확인).**
0. **웹에서 만든 모든 것은 서명이 필요하다.** 제공자 등록·삭제, 그룹 만들기, 프로젝트 만들기가 각각 패스키 서명 한 번이다. 확인 화면에 변경 전체(제공자 주소, 그룹 경로, 이름, 템플릿, AI 전송, 게시 권한, 배포 키 지문과 `.pub` 앞부분)가 나오고 **브라우저가 `change` 해시를 다시 계산해 서버의 값과 같을 때만** 서명 단추가 켜진다. 배포 키 공개 키의 해시가 서명에 들어가므로 서명 뒤에 키가 바뀌면(`.pub` 교체) orchd와 broker가 거부한다.
1. **제공자 주소를 바꾸는 것도 서명된 변경이다**(토큰이 새 주소로 가므로). 이미 프로젝트가 참조하는 제공자의 주소·ssh 주소는 바꾸지 못한다(`provider_in_use`). 제공자 주소는 `http`는 사설·tailnet IP 리터럴에만, 그 밖은 `https`만 되고 루프백·링크 로컬(`169.254.169.254` 포함)·예약 주소·`localhost`는 거부되며 리다이렉트는 따르지 않는다.
2. **동시성·멱등.** 같은 이름의 동시 생성은 하나만 진행되고 나머지는 `project_name_taken`이다. **GitLab에 이미 있는 이름은 인수하지 않는다**(`project_name_taken`). 4~7단계(프로젝트 만들기 → 첫 파일 → 기본 브랜치 확인 → 보호 브랜치 → 배포 키) 중 하나라도 오류(또는 시간 초과·패닉)면 **지우지 않고** 고아 기록을 남기고 `create_partial`로 답한다. **"결과 불확실"인 오류(시간 초과, 연결 끊김)도 같은 취급이다**: 고아 기록이 가리키는 프로젝트가 GitLab에 실제로는 없을 수 있다.
3. **서명이 유효하면 요청 ID가 소모된다**(W3b와 같은 규칙): 서명 뒤의 모든 거부(`project_name_taken`, `group_unknown`, `rate_limited`, `provider_unreachable` …)는 같은 서명으로 다시 보낼 수 없고 새로 서명해야 한다. 모양 오류와 `deploy_key_mismatch`·`path_not_allowed`·`registry_unavailable`·`tokens_unavailable`은 서명 전에 거부되어 ID를 소모하지 않는다.
4. **시간 제한이 겹친다.** broker의 `create_project` 전체 60초 < orchd의 broker 호출 제한 70초 < 웹 화면 130초. 프로젝트 만들기 + 첫 파일들 + 자동 복제는 **최대 약 90초**까지 걸릴 수 있고 화면은 그동안 "만드는 중"이다. **서버 앞의 역방향 프록시·라우터·tailnet 구간의 제한시간이 130초보다 짧으면** 서버는 끝냈는데 브라우저가 연결을 잃고 "결과를 모름"으로 보일 수 있다(그러면 목록을 다시 읽어 확인한다: 같은 변경을 다시 서명하지 않는다). 이 구간의 제한시간은 이 저장소가 알 수 없다(사전 점검에서 `tailscale serve` 등 앞단을 확인한다).
5. **복제.** 생성 직후 한 번 자동으로 `/home/orch/repos/<이름>`에 복제한다(`git clone --no-recurse-submodules --no-local`, 프로토콜은 ssh만, 훅·fsmonitor 끔, `UpdateHostKeys=no`, 고정 호스트 키 파일의 호스트만, 환경 비움, 100초). **복제가 실패해도 프로젝트는 만들어진 채**이고 응답이 200과 `clone.status: failed`(+ 고정 코드)이다. 화면의 "다시 복제"(서명 없음, 사용자당 시간당 10회)로 다시 한다. 복제는 120초가 아니라 **100초**로 제한되고(브라우저는 130초를 기다리고 서버 `http.Server`의 `WriteTimeout`은 120초라, 100초 복제와 그 앞뒤의 로컬 작업이 120초 안에 들어가도록) 템플릿 디렉터리를 쓰지 않는다(`--template=`). 복제는 `.clone-<무작위>` 폴더에서 한 뒤 이름을 바꿔 들이므로 도중에 죽어도 `/home/orch/repos/<이름>`이 반쯤 만들어진 채 남지 않는다(남은 `.clone-*`는 orchd 시작 때 지운다).
6. **게시 원격 해석(`publish`).** `--git-remote-for`에 없는 저장소는 등록부의 `provider`·`gitlab.project`로 원격을 만든다(호스트가 `known_hosts_gitlab`에 있을 때만). **등록부를 읽지 못하고 캐시가 30초보다 낡았으면 "알 수 없음 = 거부"**: broker가 꺼져 있거나 막 바뀐 직후(캐시 무효화 뒤)에는 **`--git-remote`만 쓰는 저장소(예: `toy`의 `/home/orch/repos/remote.git`)의 게시도** broker가 다시 읽힐 때까지 고정 오류로 실패한다(재시도 가능). **이 거부를 받아들일지는 사용자 결정이다**(받아들이지 않으려면 코드 변경이 필요하다). 정적 `--git-remote-for`(예: sample-app)는 그대로 우선한다.
7. **롤백하면 옛 broker는 등록부 버전 2를 모른다**(⚠ 2). 컷오버 뒤 만든 프로젝트·제공자·고아 기록은 복원한 등록부에 없다(GitLab과 `/home/orch/repos`에는 남는다).
8. **한도(주체별).** `create_project` 시간당 5, `create_group` 5, `set_provider`·`delete_provider` 합쳐 10, `list_groups` 60, 복제 10, 제안 60. broker 전체에도 같은 종류의 상한이 있다(`create_project` 시간당 10). 서명이 소모된 거부도 한도를 쓸 수 있다(W3c의 한도는 첫 GitLab 쓰기 직전에만 소모된다).
9. **고아 알림.** 등록부의 `orphans`(최대 100개, 값 없음: 제공자·프로젝트 경로·ID·단계·시각)는 `#/projects`에 알림 배지로 보인다. **정리 op가 없다**: 사용자가 GitLab에서 프로젝트를 지운 뒤에도 기록은 남고 가득 차면 가장 오래된 것이 밀려난다(`orphan_dropped`). 경로를 직접 적어 두는 것이 안전하다.

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 25번 6장에 "확인함/못 함"으로 남긴다)
1. **실제 GitLab 19.0.8에서** `POST /groups`·`POST /projects`(`visibility: private`, `initialize_with_readme: false`)·**빈 저장소에 파일 API로 첫 파일(`main` 생성과 기본 브랜치 지정)**·보호 브랜치(`main`이 이미 보호되어 오는지, `POST`가 201인지, Maintainer 푸시·병합·강제 푸시 금지)·배포 키 `POST`(상태 코드 201 가정)·`min_access_level=40` 그룹 목록의 실제 동작과 **관리자 토큰의 권한**(1-5 이후 3-3, 3-4).
2. **같은 배포 키를 두 번째 프로젝트에 등록할 수 있는지**(⚠ 5): 프로젝트를 둘 만들어 봐야 안다(사용자 결정).
3. **orchd가 `/home/orch/repos`에 복제할 수 있는지**(소유자·`ReadWritePaths`·임시 폴더, 3-4), **배포 키로 새 프로젝트를 `git clone`할 수 있는지**(읽기 키·쓰기 키 모두 clone 가능해야 한다), **쓰기 키로 `push`가 되는지**(3-6, 사용자 결정).
4. **`git`·`ssh` 실행 파일과 `known_hosts_gitlab`**(사전 점검 e·f), **broker가 GitLab HTTP에 닿는지**(nft, 사전 점검 g), **등록부 v1→v2 마이그레이션 결과와 롤백 영향**(1-4, 4-A).
5. **`basic`·`go` 템플릿이 실제 게이트를 통과하는지**(3-6의 작은 작업), 첫 커밋이 파일 수만큼의 커밋이 되는 모양.
6. **실제 패스키로 만든 `set_provider`·`create_group`·`create_project` 서명을 broker가 검증하는지**(3-2, 3-4. W3a는 `set_flag`만 확인했고 W3b의 `set_token`은 사용자 사전 작업 2에서 처음 확인).
7. 키보드만·스크린 리더로의 `#/providers`·새 프로젝트 화면 사용, 브라우저에서 제공자 화면에 **비밀 입력칸이 없는지**(3-1).

### 단계 1. broker 갱신·등록부 마이그레이션 [사용자][주의: 등록부가 버전 2로 올라간다 → 롤백은 단계 4의 A]
`orchd`와 DB는 건드리지 않는다. **순서: 백업 → (Mac) 게이트·빌드 → 옛 broker 정지 → 새 바이너리 설치 → 샌드박스 안에서 `-check` → 시작.** `broker.json`은 바뀌지 않는다.

**먼저 백업(서비스 영향 없음). `/root/backup-w3c`(root 0700).** orch-vm에서:
```
B=/root/backup-w3c
sudo install -d -m 0700 -o root -g root $B $B/bin
sudo stat -c '%U:%G %a %n' $B          # root:root 700 이어야 한다
sudo cp -a /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -a /usr/local/lib/aigate/$f $B/bin/$f; done
sudo cp -a /etc/orch-broker $B/etc-orch-broker
sudo cp -a /var/lib/orch-broker $B/var-lib-orch-broker       # registry.json(버전 1)이 여기 있다: 롤백의 핵심
sudo cp -a /home/orch/orchd/policy.yml $B/policy.yml.pre-w3c
sudo cp -a /etc/systemd/system/orch-broker.service $B/orch-broker.service.pre-w3c
sudo cp -a /etc/systemd/system/orchd.service $B/orchd.service.pre-w3c; sudo cp -a /etc/systemd/system/orchd.service.d $B/ 2>/dev/null
sudo grep -o '"version": *[0-9]*' $B/var-lib-orch-broker/registry.json    # "version":1
sudo ls -lR $B | head -50
```
(백업에는 `openai.key`(0400)와 **토큰 파일 사본(`secrets/`, 사용자 사전 작업 2를 했다면 GitLab 관리자 토큰 포함)**이 들어간다. 값은 화면에 나오지 않고 폴더는 root만 들어갈 수 있다. **`orch`가 쓸 수 있는 폴더에 두지 않는다.** 컷오버가 안정되면 사용자가 `sudo find /root/backup-w3c -path '*secrets*' -type f -exec shred -u {} +`와 `sudo shred -u /root/backup-w3c/etc-orch-broker/openai.key`로 비밀 사본을 지운다.)

**1-0. 빌드 전 게이트 (Mac, 저장소에서). 하나라도 실패하면 빌드하지 않는다.**
```
go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api     # --- PASS 이고 SKIP 이 없어야 한다
go vet ./... ; gofmt -l .                                         # gofmt 출력 없음
GOOS=darwin go vet ./cmd/... ./internal/... ; GOOS=linux go vet ./cmd/... ./internal/...
go test ./... -count=1 -race
node --test "internal/orch/web/test/*.test.js"
```
(결과 문서 25번 2장의 숫자와 같은 커밋인지 확인한다.)

**1-1. 새 바이너리.** Mac에서 `GOOS=linux GOARCH=amd64 go build -o /tmp/w3c/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고(`uname -m`에 맞춘다) `scp`로 VM `/tmp/w3c/`에 보낸다. **아직 설치하지 않는다.**

**1-2. 옛 broker 정지 → 새 바이너리 설치.** **옛 broker를 반드시 먼저 멈춘다**(⚠ 2). (정지한 뒤 1-3이 실패하면 broker는 멈춘 채로 남는다: 곧바로 단계 4의 A.)
```
sudo systemctl stop orch-broker
sudo install -o root -g root -m 0755 /tmp/w3c/orch-broker /usr/local/bin/orch-broker
```
이 시점부터 시작까지 AI 계획서·병합 요청·프로젝트 변경은 `broker_unavailable`이다(기존 기능은 그대로).

**1-3. 샌드박스 안에서 `-check`(시작 전).** 유닛과 같은 샌드박스 옵션(13장 1-3과 같은 명령):
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…)` 한 줄과 `Finished with result: success`(종료 코드 0). 새 점검은 **등록부의 제공자 설정(주소 모양·주소 규칙, 아무것도 만들거나 연결하지 않는다)**을 본다: 제공자가 아직 없으므로 경고가 없어야 한다(W3b 컷오버 뒤와 같은 경고만 허용). **`-check`는 등록부를 올리지 않는다**: 끝난 뒤에도 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 **`"version":1`** 이어야 한다. 실패 문장이 나오면 **시작하지 않고 멈춘다 → 단계 4의 A.**

**1-4. 시작(등록부가 여기서 버전 2로 올라간다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json
sudo grep -o '"providers": *\[[^]]*\]\|"orphans": *\[[^]]*\]' /var/lib/orch-broker/registry.json
sudo ls -a /var/lib/orch-broker | grep -c '^\.state-' || true
```
예상: 유닛 `active (running)`, 로그에 오류 없음. 폴더 `orch-broker:orch-broker 700`, `registry.json`은 **`600`이고 `"version":2`**, `"providers":[]`, `"orphans":[]`, 임시 파일 `.state-*` 없음(0). 프로젝트 항목은 그대로(`toy`·`sample-app`, 값 변화 없음: 1-5에서 비교). **파일이 버전 1 그대로이거나 broker가 시작하지 않으면(마이그레이션 쓰기 실패 → broker가 시작을 거부하고 버전 1 파일은 건드리지 않는다) 멈춘다 → 사전 점검 a·유닛 샌드박스를 다시 보고 단계 4의 A.**

**1-5. 새 읽기 op와 회귀.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기):
```
for op in providers projects status tokens; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op; done
```
예상: `providers`는 `{"ok":true,"result":{"providers":[]}}`. `projects`는 W3b 컷오버 뒤와 같은 프로젝트(`toy`·`sample-app`, 각 항목의 `provider`는 `null`)에 `"providers":[]`·`"orphans":[]`가 더해진 것. `status`·`tokens`는 W3b 뒤와 같다(`tokens`의 값 없음: 사용자 사전 작업 2를 했다면 `gitlab` 종류 한 줄이 더 있다). **`projects`의 프로젝트 목록이 1단계 전과 다르거나 `ai_context`가 바뀌었으면 멈춘다 → 단계 4의 A.**

**검증 체크리스트:** 1-3 통과(등록부 아직 v1), 1-4의 유닛 active·`registry.json` 600·`"version":2`, 1-5의 `providers`(빈 목록)·`projects`(변화 없음). **그래도 안 되면:** 단계 4의 A. 다른 서비스에는 영향이 없다(`orchd`를 건드리지 않았다).

### 단계 2. `orchd` 갱신 [사용자]
**DB 스키마가 바뀌지 않으므로 DB를 복원할 필요가 없다.** 실행 중·대기 작업이 없음을 먼저 확인한다(`orch list`).
```
sudo systemctl stop orchd
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3c/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
```
유닛과 드롭인은 바꾸지 않는다. **검증 체크리스트**
- 로그: 오류 없이 대기, `approvals: mode signed`, 스키마 오류 없음, 게시 해석기·복제 설정에 대한 오류 없음. 시작 때 `.clone-*` 정리가 한 일이 없다면 로그에 아무것도 없다.
- `orch list`: 단계 1 이전의 작업이 모두 그대로, 마지막 번호가 같다.
- 새 엔드포인트가 떠 있고 로그인 없이는 닫혀 있는지: `curl -sk -o /dev/null -w '%{http_code}\n' https://<FQDN>/api/v1/providers` → **401**(같은 방식으로 `/api/v1/providers/x/groups`, `POST /api/v1/providers/propose`, `POST /api/v1/projects/create/propose`, `POST /api/v1/projects/x/clone`도 401). 404면 옛 바이너리다.
- 기존 게시가 그대로인지(선택): `--git-remote-for`의 sample-app 설정은 바뀌지 않았다.
- **정책 클래스(사용자 사전 작업 1을 했다면):** 파일 검증은 `sudo /usr/local/lib/aigate/orch policy show --policy /home/orch/orchd/policy.yml`(파일만 본다). **재시작된 데몬이 아는지**는 웹의 정책 화면(`GET /api/v1/policy`)에 `registered`가 있는지, 또는 새 프로젝트 제안이 `policy_class_missing`이 아닌 결과를 내는지로 확인한다.

**그래도 안 되면:** 단계 4의 B(바이너리만 되돌리면 된다. DB는 그대로 호환된다. 단 broker가 버전 2 등록부를 쓰고 있다면 옛 orchd가 새 `projects` 응답의 알 수 없는 필드를 어떻게 다루는지는 확인하지 않았다: 롤백은 A와 B를 함께 한다).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과, **사용자 사전 작업 1(정책 클래스)·2(관리자 토큰 등록)를 마쳤다.** Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/providers.test.js`·`projects.test.js`, `internal/orch/api/w3c_*_test.go`·`adversarial_w3c_test.go`, `internal/broker/create_project_test.go`·`adversarial_w3c_test.go`)에서 읽은 것이다.

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | 메뉴 "제공자"(`#/providers`) 열기 | 제공자 목록이 **비어 있음**("제공자 추가" 단추). **비밀 값을 입력하는 칸이 어디에도 없다**(토큰은 이름만 고른다: `type=password`·값 칸이 없는지 눈으로 본다). 도달 여부는 표시하지 않는다. 프로젝트 화면(`#/projects`)에 기존 프로젝트와 새 "새 프로젝트 만들기"가 있고 고아 배지는 없음 | `broker_unavailable`(단계 1), 메뉴가 없으면 브라우저 캐시(강력 새로고침: W3b 컷오버에서 옛 화면이 남은 적이 있다) |
| 3-2 | **제공자 등록(실제 서명).** "제공자 추가": id `gl`, 종류 `gitlab`, 주소 `http://100.64.0.10:18082`, ssh 주소 `ssh://git@100.64.0.10`, 토큰(사용자 사전 작업 2의 이름 선택) → 확인 화면(주소·ssh 주소·토큰 이름·종류) → 패스키 서명 | "제공자를 저장했습니다". 목록에 한 줄: 주소, ssh 주소, 토큰 이름, **토큰의 마지막 4자와 만료일**(W3b `tokens`에서), 삭제 단추. 서버(컨트롤러): 단계 1-5의 `providers` 호출에 항목이 있고 **`token_ref`는 이름뿐 값이 없다**. 원장에 `provider_change`(`ok:true`) 한 줄, 값 없음 | `bad_url`(주소 규칙: 사설·tailnet IP 리터럴이 아니면 `https`가 필요), `token_ref_unknown`(토큰이 `gitlab` 종류로 저장되어 있는지), `bad_signature`(**실제 패스키 서명 검증은 W3a의 `set_flag`에서만 확인했다**: 여기서 실패하면 멈추고 보고), `registry_unavailable`(1-4) |
| 3-3 | **그룹 목록(읽기, GitLab 호출).** "새 프로젝트 만들기"에서 제공자 `gl` 선택 | 그룹 목록이 나온다(Maintainer 이상인 그룹만: `min_access_level=40`, 최대 500개·`truncated`일 수 있음. **경로의 각 조각이 `^[a-z][a-z0-9-]{1,39}`(소문자 시작, 2~40자)가 아닌 그룹(대문자·`_`·`.` 포함)은 목록에 나오지 않는다**: 일부 그룹이 빠져 있어도 오류가 아니다). 이것이 **관리자 토큰이 실제 GitLab에서 통하는 첫 확인**이다. 목록 호출은 시간당 60회 한도에 들어간다 | `gitlab_refused`(토큰 만료·범위·권한: 상태 코드 범주만 알려 준다), `provider_unreachable`(nft·주소: 사전 점검 g), `bad_url`(연결 시점 주소 검사에 걸림). **그룹이 비어 있으면** 그룹이 없는 것이니 GitLab에서 만들거나 3-4의 "새 그룹 만들기"를 쓴다 |
| 3-4 | **[사용자 결정] 실제 그룹·프로젝트 만들기.** ⚠ **GitLab에 실제 리소스가 남고 이 도구는 지우지 못한다**: 시험용 이름(사전 작업 4)을 쓰고, 끝난 뒤 **사용자가 GitLab에서 직접 지운다**(⚠ 1: 등록부·`/home/orch/repos` 폴더는 남는다). 선택지 (a) **하지 않는다**(컷오버를 3-3에서 닫는다: 권장 기본), (b) 그룹만 만들어 본다(웹 "새 그룹 만들기"), (c) 프로젝트까지 만든다(그룹 선택 또는 새 그룹 → 이름·설명·템플릿 `basic`·**AI 전송 끔(기본)**·게시 권한 → 확인 화면에서 **배포 키 지문이 사전 점검 d의 `ssh-keygen -l`과 같은지 눈으로 대조** → 패스키 서명). 두 번째 프로젝트(⚠ 5의 배포 키 재사용 확인)는 별도 결정 | (c)면 약 10~90초 걸린다(화면: 단계 안내, 닫아도 서버는 계속한다). 성공: "만들었습니다"와 복제 결과 `cloned`. **서버(컨트롤러, 읽기 전용):** `sudo ls -la /home/orch/repos/<이름>`(`orch:orch 700`, `.git`, `README.md`, `.gitignore`, `.aigate.yml`), `sudo -u orch git -C /home/orch/repos/<이름> log --oneline`(**파일 수만큼 커밋**: 파일 API로 하나씩 올렸다, `README.md`가 첫 커밋)·`git remote -v`(ssh 주소 + 프로젝트 경로 + `.git`)·`git status`(깨끗함)·`sudo ls -a /home/orch/repos`에 `.clone-*` 없음. **GitLab 화면(사용자):** 프로젝트가 비공개, 기본 브랜치 `main`, `main`이 보호됨(푸시·병합 Maintainer, 강제 푸시 금지), 배포 키 `orchd <이름>`이 `can_push`가 선택한 권한과 같음. 원장에 `project_create`(`ok:true`)와 `project_clone`(`ok:true`) 한 줄씩. 등록부(1-5 호출)에 새 항목: `kind: registered`, `path: /home/orch/repos/<이름>`, `ai_context: false`, `provider: gl`, `gitlab.project: 그룹/이름`, **`project_id`가 정수** | **`create_partial`**: 화면이 **GitLab에 남은 것의 경로·ID와 단계**를 보여 준다("이 도구는 지우지 않습니다"): **그 경로를 적어 두고 GitLab에서 직접 지운다**(같은 이름은 지우기 전까지 `project_name_taken`). 단계별 의미: `project`(프로젝트가 만들어졌는지 불확실), `first_commit`(파일 일부만), `default_branch`(기본 브랜치가 `main`이 아님: 빈 저장소 첫 파일이 `main`을 만들지 않았다는 뜻 → 설계 가정이 틀림, 보고), `protect`(보호 규칙이 더 약함 또는 호출 실패), `deploy_key`(**⚠ 5: 두 번째 이후 프로젝트에서만 나오면 키 재사용 거부**), `register`(등록부 쓰기 실패: GitLab 프로젝트는 있고 등록이 안 됨). `clone.status: failed`(`host_not_pinned`·`clone_failed`·`path_not_allowed`·`git_missing`·`ssh_missing`·`clone_unavailable`): 프로젝트는 만들어졌고 화면의 "다시 복제"로 서명 없이 다시 한다(사전 점검 c·e·f를 다시 본다). `project_name_taken`: GitLab·등록부에 이미 있음(인수하지 않는다). **`path_exists`**: `/home/orch/repos`에 같은 이름이 이미 있어 **서명·GitLab 호출 전에** 거부됨(폴더를 확인한다). 생성은 성공했는데 자동 복제가 `already_cloned`면 화면이 "확인 필요"를 보인다(성공이 아님: 그 폴더가 이 프로젝트의 것인지 사람이 확인한다) |
| 3-5 | **[3-4(c)를 했다면] "다시 복제" 동작 확인(선택).** 복제가 성공했다면 이미 폴더가 있어 "이미 복제됨"이어야 한다 | `already_cloned`(409). 폴더를 일부러 지우는 시험은 하지 않는다 | — |
| 3-6 | **[사용자 결정] 새 프로젝트에서 작은 작업 한 번(게시 확인).** 게시 권한을 **쓰기**로 만들었고 `registered`가 L1일 때만 의미가 있다. 선택지 (a) 하지 않는다(기본), (b) `orch submit --repo /home/orch/repos/<이름> --base <SHA> --class registered --agent claude --prompt '…'`로 문서 한 줄 같은 작은 작업 | 게이트(`basic` 템플릿의 `.aigate.yml`: **검사 없음**)를 통과하고(W3c 등록 프로젝트는 등록부에 제공자 연결이 있으므로 **옛 W2b `create_mr` 경로를 타지 않는다**: broker가 `repo_not_mapped`로 거부한다. 그래도 사전에 `sudo grep -c '"gitlab"' /etc/orch-broker/broker.json`으로 옛 `gitlab` 설정 절이 있는지 보고 **있으면 그 내용(토큰 파일 경로 등)이 예상과 같은지만 확인한다**: 이 절은 W2b 프로젝트(`toy`·`sample-app`)용이며 바꾸지 않는다) `push_branch`가 **해석기가 만든 원격**(ssh 주소 + 프로젝트)으로 `orch/<작업번호>` 브랜치를 올린다. GitLab 화면에 그 브랜치가 보인다(**MR은 만들지 않는다**: W3d). **빈 검사 목록이 어떻게 판정되는지가 여기서 처음 드러난다** | `publish` 고정 오류(해석기가 "알 수 없음 = 거부"를 낸 경우: broker·등록부 상태, `known_hosts_gitlab`), push 거부(배포 키가 읽기 전용이거나 GitLab 보호 규칙: `main`이 아닌 `orch/*` 브랜치는 허용되어야 한다) |
| 3-7 | **브로커를 멈춘 상태**(선택): `sudo systemctl stop orch-broker` → `#/providers`와 새 프로젝트 화면을 연다 | 고정 오류(브로커에 연결할 수 없음). 작업 제출·취소·AI 아닌 기능·**이미 복제된 프로젝트로의 작업 실행**은 그대로(게시는 "알 수 없음 = 거부" 가능: 알아둘 것 6). 끝나면 `sudo systemctl start orch-broker` | 기존 기능까지 막히면 단계 4의 B |
| 3-8 | **[컨트롤러] 확인(읽기 전용)** | `sudo sh -c 'stat -c "%U:%G %a %n" /var/lib/orch-broker /var/lib/orch-broker/*.json /var/lib/orch-broker/secrets'`: 폴더 700, 파일 600. `sudo grep -E 'provider_change|project_create|project_clone' /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl \| tail -6`: **키 `action,actor,code,ok,subject`뿐**(주소·토큰 이름·배포 키·GitLab 경로·값이 없음). `sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager`을 훑어 토큰 값·`PRIVATE-TOKEN`·`Authorization`이 없는지(값을 아는 사용자가 마지막 4자로 `grep -c`를 해도 된다). `sudo ls -a /home/orch/repos`에 `.clone-*`가 없음 | 값이나 관리자 토큰이 보이면 **멈추고 보고한다.** GitLab에서 그 토큰을 즉시 폐기한다(사용자 결정) |
| 3-9 | **[사용자] 정리.** 3-4에서 만든 시험용 그룹·프로젝트를 GitLab에서 직접 지운다. 필요 없으면 `#/providers`의 제공자를 지우지 않는다(프로젝트가 참조하면 `provider_in_use`) | 이 도구는 아무것도 지우지 않는다. 등록부의 시험 프로젝트·고아 기록과 `/home/orch/repos/<이름>`은 남는다(⚠ 1). 정말 없애야 하면 broker를 멈추고 `registry.json`을 사람이 고치는 수밖에 없다(**엄격한 검사라 한 글자 틀리면 등록부 전체가 `registry_unavailable`로 닫힌다**: 반드시 백업 뒤, root로, 같은 소유·모드 0600으로) | — |

**검증 체크리스트(요약):** 3-1(비밀 입력칸 없음), 3-2(실제 서명으로 제공자 저장), 3-3(GitLab 그룹 목록), 3-4~3-6 중 사용자가 한 것이 예상대로, 3-7에서 기존 기능이 살아 있고, 3-8의 모드·원장·로그. 결과(**확인함/못 함**)는 결과 문서 25번 6장에 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다. **GitLab에 의도하지 않은 것이 만들어졌거나, 원장·로그·화면에 토큰 값이 보이거나, 등록부가 깨졌으면(`registry_unavailable`) 즉시 broker를 멈추고(단계 4의 A) 사용자에게 보고한다.** 의도하지 않은 GitLab 리소스는 **자동으로 지워지지 않는다**: 사용자가 GitLab에서 지운다.

### 단계 4. 롤백 [사용자][주의]
**A. broker를 W3b 상태로 되돌리기(DB는 건드리지 않는다). 바이너리와 등록부를 함께 되돌린다.**
1. `sudo systemctl stop orch-broker`
2. 바이너리를 **root 전용 백업에서** 복원: `sudo install -o root -g root -m 0755 /root/backup-w3c/bin/orch-broker /usr/local/bin/orch-broker`. `broker.json`은 이번에 바뀌지 않았다.
3. **등록부를 버전 1 사본으로 되돌린다**(옛 broker는 버전 2를 깨짐으로 본다): `sudo install -o orch-broker -g orch-broker -m 0600 /root/backup-w3c/var-lib-orch-broker/registry.json /var/lib/orch-broker/registry.json`, 이어서 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 `"version":1`인지. **상태 폴더 전체를 되돌리지 않는다**(예산 기록·쓴 요청 ID·W3b가 쓴 토큰이 되돌아간다).
4. `sudo systemctl start orch-broker`; 13장 1-5의 `tokens`·`status`·`projects`가 W3b 상태(`toy`·`sample-app`)를 주는지 본다.
5. **경고:** 컷오버 뒤 이 도구가 만든 프로젝트·제공자·고아 기록은 복원한 등록부에 **없다**. GitLab의 프로젝트와 `/home/orch/repos/<이름>` 폴더는 그대로 남는다(자동 삭제 없음). **고아 기록의 경로를 롤백 전에 화면에서 적어 둔다.** `secrets/`·`tokens.json`(W3b 컷오버가 쓰는 파일)은 그대로 둔다: 관리자 토큰이 들어 있다면 **의심이 있으면 GitLab에서 폐기하고 `#/tokens`로 지운다**(옛 broker는 이 토큰을 쓰지 않는다).

**B. `orchd`·CLI를 W3b 상태로 되돌리기(DB 복원 불필요).**
1. `sudo systemctl stop orchd`
2. `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3c/bin/$f /usr/local/lib/aigate/$f; done`(세 개는 항상 같이. **웹은 `orchd`에 들어 있으므로** 제공자 화면과 새 프로젝트 만들기도 함께 사라진다)
3. `sudo systemctl start orchd`. 브라우저 캐시면 강력 새로고침.

**C. 정책 클래스.** `registered` 줄은 지우지 않아도 해롭지 않다(쓰는 프로젝트가 없다). 지우려면 `/root/backup-w3c/policy.yml.pre-w3c`를 비교해 사람이 고치고(정책 해시가 바뀌므로 실행 중 작업이 없을 때), 복제된 `registered` 프로젝트가 있다면 그 작업은 L0가 된다.

**검증:** `journalctl -u orchd`·`journalctl -u orch-broker`에 오류가 없고, `orch list`가 단계 2 이전과 같으며, W3b의 토큰 화면(13장 3-1)과 W3a의 프로젝트 화면·AI 계획서 미리보기가 동작한다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`, `ls -l /usr/local/bin /usr/local/lib/aigate /var/lib/orch-broker`와 `sudo ls -lR /root/backup-w3c | head -50`을 사용자에게 보고한다. W3b 이전까지 가야 하면 13장 단계 4(`/root/backup-w3b`).

### 요약표: 누가·되돌림
| 단계 | 사용자 작업·승인 | 되돌리기 | 재시작 |
|---|---|---|---|
| 0 사전 점검(등록부 v1, `repo_roots[0]`, `/home/orch/repos`, 배포 키·`.pub`, `known_hosts_gitlab`, `git`·`ssh`, 도달성, 정책 클래스) | **컨트롤러**가 읽기 전용으로 확인 | — | — |
| 0 사용자 사전 작업(정책 클래스 추가, **관리자 토큰을 웹에 직접 입력**, 그룹·이름 결정) | **사용자 직접**(값은 채팅·터미널에 붙이지 않음) | 정책 줄은 사람이 지움(C), 토큰은 `#/tokens`에서 지움 | 정책은 필요하면 orchd |
| 1 broker 갱신 | 승인 | A(**바이너리 + 등록부 버전 1 사본**, 둘 다) | broker만 |
| 2 `orchd` 갱신 | 승인 | B(바이너리만, DB 복원 불필요) | 필요(stop/start) |
| 3 웹 확인 | **사용자 직접**(로그인, 실제 서명, **실제 GitLab 리소스를 만들지 여부와 이름 결정**) | **GitLab의 그룹·프로젝트는 사용자가 GitLab에서 지운다.** 등록부·`/home/orch/repos` 폴더는 남는다 | 불필요(3-7만 broker 재시작) |
| 4 롤백 | 승인 | — | 필요 |

## 15. W3e 컷오버 런북 (GitHub 제공자·새 저장소 생성·기존 저장소 연결·저장소별 배포 키, `orch-broker`·`orchd` 갱신)
설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `26-PLAN-webconsole-W3e.md`, 결과·시험·한계는 `27-RESULTS-webconsole-W3e.md`(**배포 현실 표**와 "실서버에서 확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더·가짜 `git` 실행 파일)을 읽고 쓴 것이지 서버나 실제 GitHub에서 본 출력이 아니다(11~14장과 같은 원칙). **W3e는 처음으로 GitHub에 실제 쓰기 호출(저장소 생성·파일 올리기·보호 브랜치·배포 키 등록)을 하는 단계이고, 이미 있는 저장소의 설정에 배포 키를 붙이는 단계다.** 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려한다.

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[컨트롤러]** 읽기 전용 확인(서버 상태를 바꾸지 않음), **[되돌림 가능]** 아래 몇 줄로 원상 복구, **[주의]** 되돌리면 그 사이의 변경을 잃거나 이 도구가 되돌리지 못함. 경로는 11~14장과 같다(바이너리 `/usr/local/lib/aigate`와 `/usr/local/bin/orch-broker`, 설정 `/etc/orch-broker/broker.json`, 상태 `/var/lib/orch-broker`, broker 소켓 `/run/orch-broker/broker.sock`, orchd 설정 `/home/orch/orchd`, 저장소 `/home/orch/repos`). 리다이렉트(`>`·`>>`)가 필요한 명령은 `sudo sh -c '…'`로 쓴다(`sudo cat > 파일`은 리다이렉트가 사용자 권한으로 실행되어 실패한다). **비밀 값(GitHub 토큰, 개인 키)은 이 절의 어떤 명령에도 넣지 않고 출력하지도 않으며, 채팅·터미널에 붙이지 않는다.**

**orchd DB 스키마는 이번에 바뀌지 않는다(v8 그대로)**: 롤백에 DB 복원이 필요 없다. **유닛(`orch-broker.service`, `orchd.service`)·orchd 플래그·`broker.json`은 바뀌지 않는다**(새 설정 키·새 플래그 없음: 저장소별 키 폴더는 `--git-ssh-key`가 있는 폴더 아래 `keys/`·`keys-staging/`로 정해진다). 새로 생기는 것: **등록부 `registry.json`이 버전 3으로 올라간다(프로젝트의 `provider`·`gitlab` 연결이 `remote:{path,id}`가 되고 고아 기록에 `origin`이 붙는다: 아래 ⚠ 2)**, 올리기 전 원본을 **`registry.json.v2.bak`**(0600, broker가 한 번만 쓰고 덮어쓰지 않음)로 남긴다, broker op `link_project`·`attach_remote`와 `create_project`의 `visibility`, orchd의 `/api/v1/projects/link|attach/propose|apply`, 웹의 제공자 종류 선택(`github`)·소유자 선택·공개 여부 선택·"기존 저장소 연결"·"원격 연결", orchd가 `/home/orch/orchd/ssh/keys/<프로젝트 id>/`(저장소별 키 쌍)와 제안 도중에만 `ssh/keys-staging/<무작위>/`(최대 15분)를 만드는 것, `known_hosts_gitlab`에 사용자가 검증해 넣는 `github.com` 항목.

> **⚠ 먼저 읽을 것 (결정과 위험).**
> 1. **이 도구는 GitHub의 저장소를 지우지 못하고(삭제 코드가 없다), 저장소에 붙인 배포 키도 지우지 못한다.** 새로 만든 저장소와 `link_project`·`attach_remote`로 이미 있던 저장소에 붙인 배포 키(제목 `orchd <이름>`)는 **사용자가 GitHub 화면에서 직접** 지워야 한다(저장소 Settings > Danger Zone, Settings > Deploy keys). 등록부에서 프로젝트를 지우는 op도 없다(W3a~W3e 어디에도): 시험용이라도 등록부 항목, `/home/orch/repos/<이름>` 폴더, `ssh/keys/<이름>/` 키 쌍이 남는다. **실제 저장소를 만들지 여부, 공개/비공개, 이름은 모두 사용자 결정이다(3-6).**
> 2. **등록부가 버전 3으로 올라간다. W3c 이하 broker는 버전 3 파일을 "깨짐"으로 보고 fail closed 한다(등록부에 의존하는 모든 op가 `registry_unavailable`).** 그래서 **롤백은 바이너리만 되돌려서는 안 되고 `registry.json`을 버전 2 사본(`/root/backup-w3e`의 사본, 또는 broker가 남긴 `registry.json.v2.bak`)으로 되돌려야 한다**(단계 4의 A). 컷오버 뒤에 등록한 프로젝트·제공자·고아 기록은 되돌린 파일에 없다. 새 broker가 첫 시작에서 파일을 올릴 때 **옛 broker가 같은 상태 폴더에서 돌고 있으면 안 되므로**(읽기와 쓰기 사이에 잠금이 없다) **옛 broker를 먼저 멈춘다**(1-3).
> 3. **broker와 orchd의 버전 차이는 위험하다(실제 서버 기준으로 분석함).** 이번에 broker가 orchd에 주는 `projects` 응답의 연결 필드 이름이 `gitlab`에서 `remote`로 바뀐다. **옛(W3c) orchd + 새 broker:** 옛 해석기는 `gitlab`이 비어 있는 제공자 연결 프로젝트를 "제공자 프로젝트 아님"으로 보고 **기본 원격(`--git-remote`, 실서버에는 `/home/orch/repos/remote.git`이 있다)으로 게시한다(fail open)**. **새 orchd + 옛 broker:** 새 해석기는 제공자 연결이 있으면 원격을 못 읽어도 "거부"로 분류한다(fail closed). 그래서 **순서는 "빌드·시험(Mac) → orchd를 먼저 멈춤(1-2) → broker 갱신 → orchd 갱신"이다**(옛 orchd가 새 broker를 만나는 순간이 없다). 이 사이 웹 콘솔은 몇 분 닫힌다. orchd를 멈추지 않고 broker만 먼저 갱신하는 것은 **권장하지 않는다**: 그렇게 한다면 실행 중·대기·게시 대기·승인 대기 작업이 없고 그동안 작업을 제출하지 않으며 곧바로 단계 2를 하는 것이 최소 조건이다. 롤백 순서도 같다(단계 4: orchd를 먼저 멈춤).
> 4. **GitHub 토큰의 영향 범위.** broker가 GitHub에 보내는 호출은 코드에 고정된 9가지(자기 계정·조직 읽기, 저장소 확인, 개인·조직 저장소 만들기, 파일 만들기, 보호 브랜치 읽기·만들기, 배포 키 등록)뿐이고 삭제·변경(`DELETE`·`PATCH`)·병합·토큰·협업자·`/user/keys` 호출은 코드에 없다(시험으로 고정). 그래도 **토큰 자체는 그 사용자의 권한을 모두 가진 토큰**이다(클래식 `repo` 범위면 모든 비공개 저장소 접근): 한 번 유출되면 위 9가지 밖의 일도 GitHub에서 할 수 있다. **만료일을 두고(사용자 결정), 시험이 끝나면 폐기하거나 권한을 줄이고, 의심되면 GitHub에서 즉시 폐기한다.** 토큰 값은 **사용자가 웹 토큰 화면에 직접 입력한다. 이 대화·채팅·터미널에 붙이지 않는다.**
> 5. **공개 저장소는 누구나 읽는다. 그리고 무료 요금제에서는 브랜치 보호가 공개 저장소에서만 켜진다.** 이 도구는 새 저장소의 `main`에 보호 규칙(강제 푸시·삭제 금지, 관리자 포함)을 켜야 성공으로 친다. **무료 요금제에서 비공개 저장소를 만들면 보호 단계에서 거부되어 `create_partial`(단계 `protect`)로 끝나고, 파일이 올라간 저장소만 GitHub에 남는다**(배포 키 등록과 등록부 기록은 그 뒤 단계라 일어나지 않는다: 쓸 수 없는 저장소). 요금제는 사용자가 안다: 비공개를 만들려면 유료 요금제(보호 브랜치 지원)여야 한다. 아니면 공개로 만들되 **시험용 이름으로, 템플릿 파일만 들어간다는 것을 알고, 끝난 뒤 지운다**(공개 시험 저장소도 한동안 세상에 보인다).
> 6. **같은 배포 키를 두 번 쓰지 않는다.** GitHub는 한 공개 키를 저장소 하나에만 등록하게 한다. 그래서 GitHub 프로젝트는 저장소마다 orchd가 새 키 쌍을 만든다(GitLab 프로젝트는 W3c의 공용 키 그대로). **제안(propose)은 키만 만들고 등록부·GitHub에는 아무것도 쓰지 않는다.** 서명 뒤 적용이 성공하면 `ssh/keys/<id>/`로 옮기고, 적용되지 않은 키는 15분 뒤와 orchd 시작 때 지운다(자기가 만든 것만). **orchd를 재시작하면 서명 대기 중이던 제안은 `key_expired`("처음부터 다시 제안")가 된다.**
> 7. **받아들인 한계(W3f F1b 이후의 현재 규칙으로 고침):** **프로젝트 id는 원격 저장소 이름과 같을 필요가 없다**(연결 화면의 "프로젝트 이름" 입력칸에 저장소 이름에서 만든 제안 값이 채워지고 사람이 바꿀 수 있다: 예 저장소 `Sulee_Portfolio` → 이름 `sulee-portfolio`). `remote_name_mismatch` 오류는 없어졌다. 서버 폴더·키 폴더 이름은 프로젝트 이름이다. GitHub 저장소는 이름에 `_`·`.`·대문자가 있어도 연결할 수 있다. GitLab은 경로의 마지막 조각이 소문자 `^[a-z][a-z0-9-]{1,39}$`여야 하는 broker 규칙이 그대로여서 `_`·대문자·`.`가 있는 GitLab 프로젝트는 연결할 수 없다. **이 도구가 새로 만드는(`created`) 프로젝트는 id가 저장소 이름과 같고 새 저장소는 소문자 이름만 만든다.** GitHub의 "그룹 만들기"는 없다(조직 생성 API가 없다): 소유자는 내 계정 또는 이미 있는 조직이다.
> 8. **절차가 요구하는 비밀 취급.** 이 절차의 어디에도 비밀을 채팅으로 보내라는 단계는 없다. GitHub 토큰은 웹 토큰 화면에만, 개인 키는 서버 안에서 `orch` 계정만 읽는다(이 절의 점검 명령도 개인 키를 열지 않는다. 예외는 단계 4의 E "키가 없어졌을 때": 그곳에서만 사람이 새 키를 만든다).

> **W3f 이후의 읽는 법(2026-11-13).** 이 15장은 W3e 컷오버 때의 절차 기록이다. 그 뒤 W3f(프로젝트 삭제, 16장)가 구현되어 "이 도구는 GitHub의 저장소·배포 키를 지우지 못한다"(⚠ 1, 3-6·3-7·3-12의 같은 문구)는 W3e 시점의 사실이다: W3f 컷오버 뒤에는 이 도구가 **자기가 만든 저장소만** 서명 한 번으로 지우고(`origin: created`), 연결한 저장소에서는 **자기가 붙인 배포 키만** 뗀다. ⚠ 7과 3-7은 W3f F1b의 현재 규칙으로 고쳤다(프로젝트 이름과 저장소 이름이 달라도 된다).

### 15.0 시작 전에 모두 참이어야 하는 것
- **출발점: W3c 컷오버가 끝난 상태**(14장). **실서버의 현재 상태가 이것이다(2026-11-12 기준 사용자·컨트롤러가 알려 준 사실, 이 저장소에서는 확인하지 못함):** broker·`orchd`가 W3c main `56dfe3e`, 등록부 버전 2, 제공자 `example-group-gitlab`, 웹에서 등록한 토큰 `orch-api-token`, nft는 GitLab `100.64.0.10`(22·18082)만 허용, `git` 2.43·OpenSSH 9.6, orchd에 기본 `--git-remote /home/orch/repos/remote.git`이 있음. 아래 점검 (a)로 이 사실을 서버에서 다시 본다.
  - **W3c 컷오버가 끝나지 않았다면:** 14장을 먼저 끝까지 한다(정책 클래스 `registered`, GitLab 관리자 토큰, 등록부 버전 2). **14장과 15장을 한 번에 합치지 않는다**: 새 바이너리는 버전 1 등록부를 곧바로 버전 3으로 올릴 수 있지만(`registry.json.v1.bak`을 남김) 그 경로는 서버에서 한 번도 실행하지 않았고, 14장 3단계가 확인하는 것(첫 실제 `set_token` 서명, GitLab 도달, 복제 권한)이 15장의 전제다.
- **실행 중·대기 중·게시 대기·승인 대기 작업이 없다**(`orch list`, `orch approvals`에 running/queued/승인 대기가 없음. 끝난 작업의 기록은 있어도 된다: 끝난 = `done`·`failed`·`cancelled`만. 그 밖의 상태(`error`·`passed`·`needs_approval`·`publishing`)는 진행 중으로 센다): 단계 1은 broker를, 단계 2는 `orchd`를 재시작한다(작업 제출 중단).
- **웹 서명에 쓸 패스키 서명자가 등록되어 있다**(`sudo orch admin signers list`에 `webauthn` 형식). **15장의 실제 서명(GitHub 제공자 저장, 저장소 생성, 연결)은 아직 한 번도 실제 패스키로 확인하지 않았다.**
- **백업은 `/root/backup-w3e`(root 소유 0700)에만 둔다.** `orch`가 쓸 수 있는 폴더에는 두지 않는다. **사전 점검 전에 [컨트롤러/root]가 먼저 만든다**(점검 결과도 여기 저장한다): `sudo install -d -m 0700 -o root -g root /root/backup-w3e /root/backup-w3e/bin`(이미 있으면 그대로). **백업 복사는 모두 `cp -an`(덮어쓰지 않음)이다**: 이전 컷오버 시도의 사본이 남아 있으면 그것이 이번 시작 상태(등록부 v2)인지 `sudo grep -o '"version": *[0-9]*' …`로 보고, 아니면 옛 사본을 `이름.old-<날짜>`로 옮긴 뒤 복사한다.
- **사전 점검 (단계 1 전에, 전부 읽기 전용이고 값·개인 키를 찍지 않는 명령. 컨트롤러가 서버에서 직접 한다).** 하나라도 예상과 다르면 **시작하지 않고** 보고한다.
  - **(a) W3c 상태와 등록부.** `systemctl is-active orch-broker orchd` → `active` 둘. `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd`(결과를 기록: 롤백 확인에 쓴다). `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json` → 폴더 **`orch-broker:orch-broker 700`**, 파일 `orch-broker:orch-broker 600`. `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → **`"version":2`**(이미 3이면 누군가 새 broker를 먼저 시작한 것: 멈추고 확인). 제공자와 프로젝트: 14장 1-5와 같은 읽기 호출 `providers`·`projects`(값이 없는 읽기)에서 제공자 `example-group-gitlab`과 프로젝트 `toy`·`sample-app`(그리고 W3c에서 만든 프로젝트가 있다면 그것)를 본다. `sudo ls /var/lib/orch-broker`에 `registry.json`, `used-requests.json`, `budget.json`, `secrets`, `tokens.json`이 있고 **`registry.json.v2.bak`는 아직 없다.** 이 두 응답은 컷오버 전후 비교용으로 단계 1-2에서 `/root/backup-w3e`에 저장한다.
  - **(b) `ssh` 폴더와 키 폴더를 만들 수 있는지(실제 유닛 샌드박스 기준).** `sudo stat -c '%U:%G %a %F %n' /home/orch/orchd/ssh` → 소유자 **`orch:orch`**, 모드 0700 또는 0750 이하, **`directory`(심볼릭 링크가 아님)**. `sudo ls -la /home/orch/orchd/ssh`에 `id_gitlab_deploy`(0600 `orch`), `id_gitlab_deploy.pub`, `known_hosts_gitlab`이 있고 **`keys`·`keys-staging`은 아직 없다**(orchd가 첫 제안 때 만든다). 유닛과 같은 샌드박스에서 쓰기 가능성만 본다(만들지 않는다): `sudo systemd-run --wait --pipe -p User=orch -p ProtectSystem=strict -p ProtectHome=read-only -p 'ReadWritePaths=/home/orch/orchd /home/orch/repos' -p PrivateTmp=yes /bin/sh -c 'test -d /home/orch/orchd/ssh && test -w /home/orch/orchd/ssh && echo writable'`(예상 `writable`). `systemctl cat orchd | grep -- --git-ssh-key`(예상 `--git-ssh-key /home/orch/orchd/ssh/id_gitlab_deploy`: 키 폴더는 이 파일이 있는 폴더 아래로 정해지므로 다르면 멈춘다), `systemctl show orchd -p User -p ReadWritePaths -p ProtectSystem -p ProtectHome -p PrivateTmp` → `User=orch`, `ReadWritePaths`에 **`/home/orch/orchd`**와 `/home/orch/repos`, `ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp=yes`. **이 점검은 폴더 만들기(`mkdir`)·0700 모드·`umask`를 보증하지 못한다**: 시험은 임시 폴더에서 같은 사용자로만 돌았다. 실제로 만들어지는지는 3-6의 첫 제안에서 처음 드러나고, 못 만들면 `key_staging_failed`(아무것도 쓰지 않는 안전한 실패)다.
  - **(c) `git`·`ssh` 실행 파일과 OpenSSH 버전.** `sudo ls -l /usr/bin/git /usr/bin/ssh`, `sudo -u orch sh -c 'git --version; ssh -V'`(예상 git 2.43, OpenSSH 9.6). orchd가 만드는 키는 Go 표준 라이브러리의 ed25519 키를 OpenSSH 형식으로 저장한 것이다: **OpenSSH 9.6이 그 개인 키 파일을 받아들이는지는 서버에서 한 번도 확인하지 못했다**(3-6의 첫 복제가 확인한다).
  - **(d) `known_hosts_gitlab`과 GitHub 호스트 키.** `sudo stat -c '%U:%G %a %n' /home/orch/orchd/ssh/known_hosts_gitlab`(소유자 `orch`, 그룹·기타 쓰기 불가). `sudo ssh-keygen -F github.com -f /home/orch/orchd/ssh/known_hosts_gitlab; echo "exit $?"` → 지금은 **`exit 1`(항목 없음)이 정상**이다(ssh를 443 포트로 쓰는 선택 B(아래 (e))라면 `sudo ssh-keygen -F '[ssh.github.com]:443' -f …`). GitHub 항목은 **사용자가 GitHub의 공식 SSH 지문과 대조한 뒤에만** 넣는다(아래 사용자 사전 작업 5). **GitHub 호스트 키를 컨트롤러가 자동으로 받아 넣지 않는다.** 항목이 이미 있다면 누가 언제 넣었는지 확인하고, 사용자 사전 작업 5의 대조를 다시 한다.
  - **(e) 방화벽 상태(바꾸지 않는다, 있는 것만 보고한다).** 저장소의 `infra/nftables/orchestrator.nft`(36행)는 `tcp dport { 80, 443 } ip daddr != @private4 accept`로 **공인 주소로의 80·443을 이미 허용한다**(OpenAI용). 실서버의 규칙이 이 파일과 같다면 **broker의 `api.github.com:443`은 방화벽 결정 없이 이미 열려 있고, 막힌 것은 orchd의 `github.com:22`뿐**이다. 읽기 전용으로 확인한다: `sudo nft list ruleset | grep -n -E '443|dport \{? ?22|18082'`와 `getent hosts api.github.com github.com ssh.github.com`(규칙이 파일과 다르면 그대로 보고). **ssh 경로는 사용자 결정이다(이 절차는 방화벽을 바꾸지 않는다):** (A) `github.com:22`를 여는 규칙을 사용자가 정한다(GitHub의 IP가 바뀌므로 `https://api.github.com/meta`의 `git` 대역을 허용하는 식), 또는 (B) **새 규칙 없이** 제공자의 ssh 주소를 `ssh://git@ssh.github.com:443`으로 쓴다(GitHub가 443 포트로 ssh를 받는다고 공식 문서가 안내하는 주소: **공식 문서에서 사용자가 확인**한다. 코드는 포트가 있는 ssh 주소를 받으며 그 철자 분리·호스트 고정은 시험했으나 `ssh.github.com:443` 자체는 서버에서 확인하지 못했다). (B)를 고르면 3-3의 제공자 등록에서 ssh 주소 칸의 기본값을 이 주소로 직접 바꿔 적고, `known_hosts_gitlab`의 줄은 `[ssh.github.com]:443 ssh-ed25519 …` 꼴이어야 한다. 규칙이 없고 (A)도 (B)도 아니면 복제·게시가 실패로 끝난다(안전하게 실패, 아무것도 만들어지지 않음). **방화벽 규칙을 새로 만드는 일은 컨트롤러가 하지 않는다.** 도달 점검: broker 샌드박스에서 `sudo systemd-run --wait --pipe -p User=orch-broker -p ProtectSystem=strict -p ProtectHome=yes /usr/bin/curl -s -o /dev/null -w '%{http_code}\n' https://api.github.com/`(예상 `200`: 토큰을 쓰지 않는 읽기). orchd 쪽 ssh 도달(키 없이 `publickey` 거부가 나오면 도달과 호스트 키 고정이 된 것이다): `sudo -u orch ssh -F /dev/null -i /dev/null -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o UserKnownHostsFile=/home/orch/orchd/ssh/known_hosts_gitlab -o ConnectTimeout=10 -T git@github.com`((B)는 `-p 443 … -T git@ssh.github.com`; 예상 `Permission denied (publickey).`; `Host key verification failed`는 (d)의 항목이 없거나 틀림, `timed out`은 방화벽).
  - **(f) 정책 클래스 `registered`.** `sudo grep -n -B1 -A2 registered /home/orch/orchd/policy.yml` → W3c 컷오버에서 넣었다면 **있어야 한다**(없으면 새 프로젝트 생성·연결 제안이 서명 전에 `policy_class_missing`: 14장 사용자 사전 작업 1). `attach_remote`는 클래스가 필요 없다.
  - **(g) 저장소 루트와 이름.** `sudo grep -n -A3 repo_roots /etc/orch-broker/broker.json`의 첫 항목이 `/home/orch/repos`, `sudo ls -a /home/orch/repos`에 3-6~3-8에서 쓸 이름이 **없다**(`.clone-*`도 없다).
  - **(h) 샌드박스 안 `-check` 기준선.** 현재(W3c) 바이너리로 단계 1-4의 `-check` 명령을 한 번 돌려 출력을 기록한다(새 바이너리 비교용).
  - **(i) 아무것도 돌고 있지 않다.** `orch list`, `orch approvals`.
  - **서버에서 쓰기 시험을 따로 하지 않는다.** 위 점검으로 알 수 없는 것(GitHub의 실제 응답, 키 폴더 만들기, 복제 권한, 실제 패스키 서명)은 단계 3에서 처음 드러난다.

**사용자 사전 작업 (컨트롤러가 대신할 수 없다).**
1. **[사용자 결정] GitHub 토큰을 만든다.** 개인 계정의 토큰이고 **만료일을 둔다**(짧을수록 안전하다: 며칠~수십 일, 사용자 결정). 어떤 종류가 이 도구의 호출을 덮는지는 **확인하지 못한 부분이 있다:**
   - *확인함(GitHub 공식 문서를 구현 중에 읽음, 2026-11-12):* 클래식 토큰은 `repo` 범위가 비공개 저장소 생성을, `public_repo`(또는 `repo`)가 공개 저장소 생성을 덮는다.
   - *확인하지 못함:* 세분화(fine-grained) 토큰의 권한 이름과 범위. 문서 쪽 페이지가 그 목록을 명시하지 않았다. 기대는 저장소 권한 `Administration`(쓰기: 저장소 만들기·보호 브랜치·배포 키)·`Contents`(쓰기: 첫 파일)·`Metadata`(읽기)이지만, **세분화 토큰이 개인 계정에 새 저장소를 만들 수 있는지**와 아직 없는 저장소를 "선택한 저장소"로 고를 수 없다는 점(새 저장소 생성에는 "모든 저장소"가 필요할 수 있음)은 확인하지 못했다.
   - *권고:* 먼저 사용자가 각 엔드포인트의 공식 문서(저장소 만들기, 파일 만들기, 보호 브랜치, 배포 키) 페이지의 "세분화 토큰" 부분을 직접 읽고(ego-browser 등) 만들 수 있다고 적혀 있는 권한으로 **세분화 토큰**을 시도한다. 그것이 안 되면 **클래식 토큰의 `public_repo`**(공개만 만들 때) 또는 `repo`(비공개도 만들 때)로 하되 `repo`는 모든 비공개 저장소에 닿는다는 점을 감수한다. **기존 저장소 연결(link·attach)만 할 토큰은 그 저장소들에 접근하고 배포 키(`Administration` 쓰기)를 등록할 수 있으면 된다**(확인하지 못함). 어느 쪽이든 토큰 값은 **웹 `#/tokens`에서 종류 `github`, 이름(예 `github-main`, 소문자·숫자·`-`), 값, 만료일을 사용자가 직접 입력**하고 확인 화면의 마지막 4자를 GitHub 화면과 눈으로 대조한 뒤 패스키로 서명한다. 그 뒤 서버에서 `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/secrets/<이름> /var/lib/orch-broker/tokens.json` → **`orch-broker:orch-broker 400`과 `600`**.
2. **소유자: 개인 계정**(2026-11-12 사용자 결정). 소유자 목록에는 내가 속한 조직도 나올 수 있으나 쓰지 않는다. 조직에 만들려면 그 조직이 토큰을 허용해야 한다(확인하지 못함).
3. **[사용자 결정] 공개/비공개, 요금제, 시험 저장소 이름, 실제로 만들지 여부.** 위 ⚠ 5. 정하지 않았으면 3-6은 하지 않는다. 이름은 `^[a-z][a-z0-9-]{1,39}$`(예 `w3e-smoke-1112`).
4. **[사용자 결정] ssh 경로**(위 (e)): (A) `github.com:22`를 여는 방화벽 규칙(사용자가 정함) 또는 (B) 규칙 없이 `ssh://git@ssh.github.com:443`. `api.github.com:443`은 이미 열려 있을 가능성이 높다(확인은 (e)). 이 도구와 컨트롤러는 nft를 바꾸지 않는다.
5. **GitHub 호스트 키를 GitHub의 공식 지문과 사용자가 대조해서 `known_hosts_gitlab`에 넣는다.** **`ssh-keyscan`의 출력은 그것만으로는 믿지 않는다**(중간자가 줄 수 있다). 아래 `github.com`은 선택 B라면 `-p 443 ssh.github.com`으로, 서버의 줄은 `[ssh.github.com]:443 …`로 바꿔 읽는다. 절차는 정확히 이렇다:
   1. 사용자의 브라우저에서 GitHub의 공식 문서 "GitHub's SSH key fingerprints"(docs.github.com, 주소를 직접 쳐서 연다)에 적힌 **ED25519 SHA256 지문**을 읽는다. 가능하면 두 번째 출처로 `https://api.github.com/meta`의 `ssh_key_fingerprints` 항목도 읽어 두 값이 같은지 본다. **값은 이 문서에 적지 않는다**(바뀔 수 있고, 기억이 아니라 그때 공식 페이지에서 읽는다). (B)라면 `ssh.github.com`이 같은 키를 쓴다고 공식 문서가 말하는지도 읽는다.
   2. 후보 줄을 만든다: 네트워크가 닿는 곳(사용자의 Mac 등)에서 `ssh-keyscan -t ed25519 github.com > /tmp/gh_host_candidate`((B): `ssh-keyscan -t ed25519 -p 443 ssh.github.com > /tmp/gh_host_candidate`). 이 줄은 아직 **믿지 않는다.**
   3. 지문을 계산한다: `ssh-keygen -lf /tmp/gh_host_candidate` → `256 SHA256:… github.com (ED25519)`. 이 `SHA256:…` 문자열을 1에서 읽은 공식 지문과 **한 글자씩** 대조한다. 한 글자라도 다르면 **넣지 않고 멈춘다**(다른 네트워크에서 2를 다시 해 본다. 계속 다르면 보고).
   4. 대조가 맞았을 때만 서버에 넣는다. 백업은 15.0의 백업 단계(1-2)에서 `known_hosts_gitlab.pre-w3e`로 이미 했거나, 지금 `sudo cp -an /home/orch/orchd/ssh/known_hosts_gitlab /root/backup-w3e/known_hosts_gitlab.pre-w3e`. 후보 줄은 공개 정보(호스트 키)라 터미널로 옮겨도 된다. 서버의 임시 파일 `/tmp/gh_host_candidate`에 놓고 `sudo sh -c 'cat /tmp/gh_host_candidate >> /home/orch/orchd/ssh/known_hosts_gitlab'`(소유자·모드 불변이어야 한다: 다음 줄로 확인).
   5. 서버에서 다시 확인한다: `sudo stat -c '%U:%G %a %n' /home/orch/orchd/ssh/known_hosts_gitlab`(소유자 `orch`, 쓰기 불가 유지), `sudo ssh-keygen -F github.com -f /home/orch/orchd/ssh/known_hosts_gitlab`((B): `-F '[ssh.github.com]:443'`; 첫 열이 정확히 `github.com`(또는 `[ssh.github.com]:443`)인 평문 줄 하나. `*`·`?`·`!`·`@cert-authority`·`@revoked`가 든 줄은 고정으로 치지 않는다), 그리고 그 줄의 지문을 서버에서도 한 번 계산해 공식 지문과 같은지 본다: `sudo ssh-keygen -F github.com -f /home/orch/orchd/ssh/known_hosts_gitlab | grep -v '^#' > /tmp/gh_host_line; ssh-keygen -lf /tmp/gh_host_line`. 임시 파일 `/tmp/gh_host_candidate`와 `/tmp/gh_host_line`은 지운다.
6. **[사용자 결정] 기존 저장소 연결·원격 연결을 시험할지**(3-7, 3-8): 이미 있는 저장소의 설정에 배포 키가 **등록되고 이 도구는 지우지 못한다.**

**알아둘 것 (코드 읽기와 시험 기준, 실서버·실제 GitHub 미확인).**
0. **웹에서 만든 모든 변경은 서명이 필요하다.** GitHub 제공자 등록, 저장소 만들기, 기존 저장소 연결, 원격 연결이 각각 패스키 서명 한 번이다. 확인 화면에 변경 전체(소유자, 이름, **공개/비공개(굵게)**, 템플릿, AI 전송, 게시 권한, 이 저장소 하나를 위해 방금 만든 **배포 키의 지문**)가 나오고 **브라우저가 `change` 해시를 다시 계산해 서버의 값과 같을 때만** 서명 단추가 켜진다. 배포 키의 해시가 서명에 들어가므로 서명 뒤에 키가 바뀌면 orchd와 broker가 거부한다.
1. **공개/비공개는 기본값이 없다**(항상 고르고 서명된다). GitLab 프로젝트는 계속 비공개 고정이고 이 필드를 받지 않는다. 공개 저장소는 서명하지 않은 채 만들어지지 않는다(시험으로 고정).
2. **GitHub 쪽 순서(`create_project`):** 소유자 확인(내 계정·조직 목록에서 서명된 소유자를 찾음, 서명된 글자는 믿지 않고 GitHub의 철자를 씀) → 같은 이름이 GitHub에 있으면(대소문자 무시) `project_name_taken`(인수 안 함) → 저장소 만들기 → 첫 파일들(`README.md`가 먼저, 첫 파일은 브랜치를 지정하지 않음) → 기본 브랜치가 `main`인지 → `main` 보호(방금 만든 저장소에만 쓴다) → 배포 키 → **마지막에** 등록부. 중간 실패는 지우지 않고 고아 기록 + `create_partial`(저장소 만들기 호출이 4xx로 거부되면 만들어진 것이 없으므로 고아 기록 없이 `github_refused`).
3. **`link_project`·`attach_remote`는 저장소를 만들거나 지우거나 설정을 바꾸지 않는다**: 저장소가 있는지 읽고(없으면 `repo_not_found`), **배포 키 하나를 등록**하고(같은 키가 이미 있으면 성공으로 보지 않고 거부), 등록부에 쓴다. 키만 붙고 등록이 안 되면 `link_partial`("저장소는 지우지 마세요, 배포 키만 직접 제거하세요")과 `origin: linked` 고아 기록이다(**"만든 것"이라고 말하지 않는다**). `attach_remote`는 provider와 remote만 바꾸고 `ai_context`·경로·종류는 그대로다. **정적 `--git-remote-for` 항목이 있는 프로젝트(예: `sample-app`)는 정적이 우선이라 `static_remote_wins`로 서명 전에 거부된다.** 이미 원격 매핑이 있는 프로젝트는 `already_linked`로 거부된다(조용한 재지정 없음).
4. **서명이 유효하면 요청 ID가 소모된다**(W3b와 같은 규칙): 서명 뒤의 모든 거부는 같은 서명으로 다시 보낼 수 없고 새로 서명해야 한다. 모양 오류와 서명 전 거부(`visibility_required` 등)는 ID를 소모하지 않는다.
5. **시간 제한이 겹친다.** 저장소 만들기 + 첫 파일들 + 자동 복제와 link·attach의 broker 호출(제안·적용 모두 broker의 `projects` 읽기가 있다)은 오래 걸릴 수 있어 웹은 생성·연결 적용을 130초 이상, link·attach 제안을 30초 기다린다. 앞단 프록시가 더 짧으면 서버는 끝냈는데 화면은 "결과를 모름"이 될 수 있다(목록을 다시 읽어 확인, **같은 변경을 다시 서명하지 않는다**).
6. **복제와 게시.** 생성·연결 직후 한 번 자동 복제한다(ssh만, `known_hosts_gitlab`에 고정된 호스트만, **그 프로젝트의 키 하나만**: `-i <keys/<id>/id_ed25519> -o IdentitiesOnly=yes`). 키가 없거나 모드가 느슨하면 `key_unavailable`로 git을 실행하지 않는다. 게시도 등록부에서 만든 원격과 그 프로젝트의 키만 쓴다(정적 `--git-remote-for` 우선, 등록부를 못 읽고 캐시가 낡으면 "알 수 없음 = 거부").
7. **한도(주체별).** GitHub 쓰기도 `create_project`·`create_group` 계열(시간당 5)에 합산, `link_project`·`attach_remote`는 합쳐 시간당 10. GitHub 응답의 한도 헤더는 한도 계산에 쓰지 않고 오류 응답의 분류(`provider_rate_limited`)에만 쓴다.
8. **고아 알림.** `#/projects`에 알림 배지: `created`(이 도구가 만든 것: GitHub에서 직접 지우세요)와 `linked`(원래 있던 저장소: 지우지 마세요, 배포 키만 제거하세요), 출처를 모르면 "출처 불명: 지우기 전에 확인하세요". **정리 op가 없다.**
9. **롤백하면 옛 broker는 등록부 버전 3을 모른다**(⚠ 2). 컷오버 뒤 만든 프로젝트·제공자·고아 기록은 복원한 등록부에 없다(GitHub·`/home/orch/repos`·`ssh/keys/`에는 남는다).

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 27번 6장에 "확인함/못 함"으로 남긴다)
1. **실제 GitHub에서** 빈 저장소에 콘텐츠 API로 첫 파일(`branch` 없이)이 기본 브랜치 `main`을 만드는지, 보호 브랜치 `PUT`의 본문·응답과 무료 요금제 비공개 저장소의 거부(403 가정), 배포 키 `POST`의 응답(201 가정)·같은 키 중복 거부 응답·`read_only` 응답, 토큰 범위(클래식 `repo`·`public_repo`, 세분화 권한), 한도 응답 헤더, `POST /user/repos`의 응답 모양.
2. **orchd가 `ssh/keys`·`ssh/keys-staging`을 실제 유닛에서 만들 수 있는지**(소유자·0700·`ReadWritePaths`), **OpenSSH 9.6이 orchd가 만든 개인 키를 받아들이는지**, GitHub가 그 공개 키(주석 `orchd-deploy` 포함)를 받아들이는지.
3. **`github.com`(또는 `ssh.github.com:443`) 호스트 키 고정과 ssh 도달**(사전 점검 d·e, 사용자 사전 작업 4·5), `api.github.com:443`이 실제로 열려 있는지.
4. **등록부 v2→v3 마이그레이션을 실서버 파일·uid·샌드박스로**(1-4)와 `registry.json.v2.bak`, **broker·orchd 버전 차이 구간의 동작**(이 절차는 그 구간을 없앤다).
5. **GitLab의 공용 배포 키가 두 번째 프로젝트에 합류하는지**와 GitLab 기본 브랜치 보호 설정(W3c에서 이월, 14장 ⚠ 5·사전 점검 k): GitLab 프로젝트를 더 만들 때만 드러난다.
6. **실제 패스키로 만든 GitHub `set_provider`·`create_project`·`link_project`·`attach_remote` 서명을 broker가 검증하는지.**
7. 키보드만·스크린 리더로의 새 화면 사용, 실제 Chrome에서 폼 다시 그리기 때 포커스 유지, 130초 대기.

### 단계 1. broker 갱신·등록부 마이그레이션 [사용자][주의: 등록부가 버전 3으로 올라간다 → 롤백은 단계 4의 A]
**순서: (orchd 정지) → 백업 → (Mac) 게이트·빌드 → 옛 broker 정지 → 새 바이너리 설치 → 샌드박스 안에서 `-check` → 시작 → 읽기 점검.** `broker.json`은 바뀌지 않는다.

**1-0. 빌드 전 게이트 (Mac, 저장소에서). 하나라도 실패하면 빌드하지 않는다.**
```
go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api     # --- PASS 이고 SKIP 이 없어야 한다
go vet ./... ; gofmt -l .                                         # gofmt 출력 없음
GOOS=darwin go vet ./cmd/... ./internal/... ; GOOS=linux go vet ./cmd/... ./internal/...
go test ./... -count=1 -race
node --test "internal/orch/web/test/*.test.js"
```
(결과 문서 27번 2장의 숫자와 같은 커밋인지 확인한다.)

**1-1. 새 바이너리 (Mac).** `GOOS=linux GOARCH=amd64 go build -o /tmp/w3e/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고(`uname -m`에 맞춘다) `scp`로 VM `/tmp/w3e/`에 보낸다. **아직 설치하지 않는다.** 빌드·시험이 끝나기 전에는 서비스를 멈추지 않는다(웹 콘솔이 닫히는 시간을 줄인다).

**1-2. `orchd`를 멈추고(⚠ 3, 권장 순서) 백업한다. [사용자]** 먼저 실행 중·대기·게시 대기·승인 대기 작업이 없는지 본다(`orch list`, `orch approvals`: 끝난 작업의 기록은 있어도 된다). 없으면 `sudo systemctl stop orchd`. 이 시점부터 단계 2의 시작까지 **웹 콘솔·작업 처리·게시가 멈춘다**(broker는 아직 W3c 그대로다). 멈추지 않고 진행하는 것은 ⚠ 3의 이유로 권장하지 않는다.

**백업(`cp -an`: 덮어쓰지 않음). `/root/backup-w3e`(root 0700, 15.0에서 이미 만듦).** orch-vm에서(broker의 상태 폴더는 **broker를 멈춘 뒤 1-3에서** 복사한다: 쓰는 도중의 사본을 피한다):
```
B=/root/backup-w3e
sudo install -d -m 0700 -o root -g root $B $B/bin       # 이미 있으면 그대로
sudo stat -c '%U:%G %a %n' $B          # root:root 700 이어야 한다
sudo cp -an /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -an /usr/local/lib/aigate/$f $B/bin/$f; done
sudo cp -an /etc/orch-broker $B/etc-orch-broker
sudo cp -an /home/orch/orchd/policy.yml $B/policy.yml.pre-w3e
sudo cp -an /home/orch/orchd/ssh/known_hosts_gitlab $B/known_hosts_gitlab.pre-w3e
sudo cp -an /etc/systemd/system/orch-broker.service $B/orch-broker.service.pre-w3e
sudo cp -an /etc/systemd/system/orchd.service $B/orchd.service.pre-w3e; sudo cp -an /etc/systemd/system/orchd.service.d $B/ 2>/dev/null
# 컷오버 전후 비교용 읽기 응답(값 없음)
for op in providers projects; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op | sudo tee $B/$op-before.json >/dev/null; done
sudo ls -lR $B | head -50
```
(소켓은 `orch` 계정만 쓸 수 있어 읽기는 `sudo -u orch`로 하고, root 전용 폴더에는 `sudo tee`로 저장한다. 읽기 응답에는 비밀이 없다.) 백업에는 `openai.key`(0400)와 **토큰 파일 사본(1-3의 상태 폴더 사본의 `secrets/`: GitLab 관리자 토큰, 이미 만들었다면 GitHub 토큰)**이 들어간다. 값은 화면에 나오지 않고 폴더는 root만 들어갈 수 있다. **`orch`가 쓸 수 있는 폴더에 두지 않는다.** 컷오버가 안정되면 사용자가 `sudo find /root/backup-w3e -path '*secrets*' -type f -exec shred -u {} +`와 `sudo shred -u /root/backup-w3e/etc-orch-broker/openai.key`로 비밀 사본을 지운다.

**1-3. 옛 broker 정지 → 새 바이너리 설치.** **옛 broker를 반드시 먼저 멈춘다**(⚠ 2). (정지한 뒤 1-4가 실패하면 broker는 멈춘 채로 남는다: 곧바로 단계 4의 A.)
```
sudo systemctl stop orch-broker
sudo cp -an /var/lib/orch-broker $B/var-lib-orch-broker       # registry.json(버전 2)이 여기 있다: 롤백의 핵심. broker가 멈춘 뒤의 사본이다
sudo grep -o '"version": *[0-9]*' $B/var-lib-orch-broker/registry.json    # "version":2 (아니면 15.0의 `cp -an` 주의를 본다)
sudo install -o root -g root -m 0755 /tmp/w3e/orch-broker /usr/local/bin/orch-broker
```
**1-4. 샌드박스 안에서 `-check`(시작 전).** 유닛과 같은 샌드박스 옵션(13·14장과 같은 명령):
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…)` 한 줄과 `Finished with result: success`(종료 코드 0), 사전 점검 (h)의 기준선과 같은 경고만. **`-check`는 등록부를 올리지 않는다**: 끝난 뒤에도 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 **`"version":2`** 이어야 한다. 실패 문장이 나오면 **시작하지 않고 멈춘다 → 단계 4의 A.**

**1-5. 시작(등록부가 여기서 버전 3으로 올라간다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json /var/lib/orch-broker/registry.json.v2.bak
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json
sudo sha256sum /var/lib/orch-broker/registry.json.v2.bak /root/backup-w3e/var-lib-orch-broker/registry.json
sudo ls -a /var/lib/orch-broker | grep -c '^\.state-' || true
```
예상: 유닛 `active (running)`, 로그에 오류 없음. 폴더 `orch-broker:orch-broker 700`, `registry.json`은 **`600`이고 `"version":3`**, **`registry.json.v2.bak`가 `orch-broker:orch-broker 600`으로 있고 root 백업의 `registry.json`과 sha256이 같다**, 임시 파일 `.state-*` 없음(0). **파일이 버전 2 그대로이거나 broker가 시작하지 않으면(마이그레이션 쓰기나 백업 쓰기 실패 → broker가 시작을 거부하고 버전 2 파일은 건드리지 않는다) 멈춘다 → 사전 점검 a·b와 유닛 샌드박스를 다시 보고 단계 4의 A.**

**1-6. 읽기 op와 회귀.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기, 14장 1-5와 같은 명령):
```
for op in providers projects status tokens; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op; done
```
예상: `providers`는 `example-group-gitlab`(내용 불변). `projects`는 1단계 전(`$B/projects-before.json`)과 같은 프로젝트·`ai_context`·`provider`이고 **프로젝트의 연결 필드가 `gitlab:{project,project_id}`가 아니라 `remote:{path,id}`로 보이며(내용은 같은 값)** 고아 기록에는 `"origin":"created"`가 붙는다. `status`·`tokens`는 이전과 같다. **프로젝트 목록·`ai_context`가 바뀌었거나 `remote`의 값이 이전 `gitlab`의 값과 다르면 멈춘다 → 단계 4의 A.**

**검증 체크리스트:** 1-4 통과(등록부 아직 v2), 1-5의 유닛 active·`registry.json` 600·`"version":3`·`.v2.bak` 600과 sha256 일치, 1-6의 읽기 응답. **그래도 안 되면:** 단계 4의 A. orchd는 멈춘 상태다(1-2).

### 단계 2. `orchd` 갱신 [사용자]
**DB 스키마가 바뀌지 않으므로 DB를 복원할 필요가 없다.** `orchd`는 1-2에서 이미 멈춰 있다.
```
sudo systemctl is-active orchd          # inactive
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3e/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
```
유닛과 드롭인은 바꾸지 않는다. **검증 체크리스트**
- 로그: 오류 없이 대기, `approvals: mode signed`, 스키마 오류 없음, 게시 해석기·복제 설정에 대한 오류 없음. 시작 때 `ssh/keys-staging`의 남은 키 정리는 한 일이 없다면 로그에 아무것도 없다(폴더가 아직 없다).
- `orch list`: 단계 1 이전의 작업이 모두 그대로, 마지막 번호가 같다.
- **새 바이너리가 설치되어 도는지: 해시를 비교한다.** `sudo sha256sum /usr/local/lib/aigate/orchd /tmp/w3e/orchd`의 두 값이 같아야 한다(`orch`·`aigate`도 같은 방식으로). 시작 시각은 `systemctl show orchd -p ActiveEnterTimestamp`.
- 로그인 없이는 닫혀 있는지: `curl -sk -o /dev/null -w '%{http_code}\n' https://<FQDN>/api/v1/providers` → **401**(같은 방식으로 `/api/v1/providers/x/groups`, `POST /api/v1/projects/link/propose`, `POST /api/v1/projects/attach/propose`도 401). **이것은 "로그인 없이는 닫혀 있다"는 것만 증명한다**(옛 바이너리도 같은 경로가 401/404 중 하나로 답할 수 있어 버전의 증거가 아니다: 버전은 위 해시로 본다).
- **W3c 프로젝트가 여전히 목록에 보이고 연결이 이어지는지**: 웹은 3-1에서 본다. 게시 해석기는 `remote` 필드를 읽는다: 14장 3-6의 게시 확인을 W3c에서 했던 프로젝트가 있으면 (사용자 결정) 한 번 더 한다.

**그래도 안 되면:** 단계 4(롤백: orchd를 먼저 멈춘 채 A→B 순서).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과, 사용자 사전 작업 1(토큰)·4(ssh 경로)·5(호스트 키)를 마쳤다. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/w3e.test.js`, `internal/orch/api/w3e_e4_test.go`·`adversarial_w3e_test.go`, `internal/broker/create_project_github_test.go`·`link_project_test.go`·`adversarial_w3e_test.go`)에서 읽은 것이다.

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | **기존 화면 회귀.** `#/providers`, `#/projects` 열기 | 제공자 목록에 `example-group-gitlab`(종류 열 `gitlab`), 기존 프로젝트와 W3c에서 만든 프로젝트가 이전과 같고 **"기존 저장소 연결" 단추**와(제공자·연결이 없는 행에만) "원격 연결" 단추가 새로 보인다. 제공자 화면에 **비밀 값을 입력하는 칸이 없다**(토큰은 이름만 고른다). 고아 알림은 이전과 같다 | 메뉴·단추가 없으면 브라우저 캐시(강력 새로고침), 목록이 비면 단계 1·2 |
| 3-2 | **GitHub 토큰 등록**(사용자 사전 작업 1, **값은 웹 토큰 화면에만**) | 목록에 종류 `github`·마지막 4자·만료일, 출처 "저장소". 서버(컨트롤러): `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/secrets/<이름> /var/lib/orch-broker/tokens.json` → `400`·`600` | 14장 사용자 사전 작업 2와 같음. 안 되면 멈춘다 |
| 3-3 | **GitHub 제공자 등록(실제 서명).** "제공자 추가": id `github`, 종류 **`github`**(주소 `https://api.github.com`과 ssh 주소 `ssh://git@github.com`이 채워지고 토큰 목록에는 `github` 종류 토큰만 나온다), 토큰 선택 → 확인 화면 → 패스키 서명 | "제공자를 저장했습니다". 목록에 `github` 종류 한 줄. 서버: `providers` 호출에 항목이 있고 `token_ref`는 이름뿐. 원장에 `provider_change`(`ok:true`) 한 줄, 값 없음. **`github.com`·`www.github.com`을 주소로 쓰면 `bad_url`로 거부된다**(API 주소는 `api.github.com`) | `bad_url`, `token_ref_unknown`(토큰 종류), `bad_signature`(**GitHub 종류의 실제 패스키 서명 검증은 처음**: 실패하면 멈추고 보고), `registry_unavailable`(1-5) |
| 3-4 | **소유자 목록(읽기, GitHub 호출).** "새 프로젝트 만들기"에서 제공자 `github` 선택 | **소유자 선택**에 "내 계정"(첫 줄)과 속한 조직이 나온다. **"새 그룹 만들기"는 없다.** **공개 여부 선택은 "선택하세요(필수)"로 시작하고 기본값이 없으며, 고르기 전에는 "변경 내용 확인" 단추가 꺼져 있다.** 이것이 **토큰과 방화벽이 실제 GitHub에서 통하는 첫 확인**이다 | `provider_unreachable`(`api.github.com:443` 도달: 사전 점검 e), `github_refused`(토큰 만료·범위: 상태 범주만 알려 준다), `provider_rate_limited`. **소유자 목록이 비거나 오류면 3-6을 하지 않는다** |
| 3-5 | **[컨트롤러] 도달·고정 확인(읽기 전용).** 사전 점검 (e)의 두 명령을 다시 | `curl`이 `200`, ssh가 `Permission denied (publickey).`(호스트 키 검증 통과) | ssh 경로·호스트 키(사용자 사전 작업 4·5) |
| 3-6 | **[사용자 결정] 실제 저장소 만들기.** ⚠ **GitHub에 실제 저장소가 남고 이 도구는 지우지 못한다**(⚠ 1·5). 선택지 (a) **하지 않는다**(컷오버를 3-5에서 닫는다: 권장 기본), (b) **공개** 시험 저장소(누구나 읽는다, 끝난 뒤 사용자가 지운다), (c) **비공개**(유료 요금제일 때만: 무료면 `create_partial`(`protect`)로 끝나고 파일만 있는 저장소가 남는다). 소유자 "내 계정" → 이름·설명·템플릿 `basic`·**AI 전송 끔(기본)**·게시 권한 → **공개 여부를 직접 고름** → 확인 화면에서 **공개 여부 문구(굵게)와 저장소별 배포 키 지문**을 확인 → 패스키 서명 | (b)/(c)면 약 10~90초(화면: 단계 안내, 닫아도 서버는 계속한다). 성공: "만들었습니다"와 복제 `cloned`. **서버(컨트롤러, 읽기 전용):** `sudo ls -la /home/orch/repos/<이름>`(`orch:orch 700`, `.git`, `README.md`…), `sudo -u orch git -C /home/orch/repos/<이름> log --oneline`(**파일 수만큼 커밋**), `git remote -v`(`ssh://git@github.com/<GitHub 철자의 소유자>/<이름>.git`), `git status`(깨끗함), `sudo stat -c '%U:%G %a %n' /home/orch/orchd/ssh/keys /home/orch/orchd/ssh/keys/<이름> /home/orch/orchd/ssh/keys/<이름>/id_ed25519 /home/orch/orchd/ssh/keys/<이름>/id_ed25519.pub`(**`orch:orch 700`, 700, 600, 644**: 첫 실제 키 폴더 만들기), `sudo ls -A /home/orch/orchd/ssh/keys-staging`(**비어 있음**), `sudo ssh-keygen -l -f /home/orch/orchd/ssh/keys/<이름>/id_ed25519.pub`(**웹 확인 화면의 지문과 같아야 한다**), `.clone-*` 없음. **GitHub 화면(사용자):** 저장소가 고른 공개 여부, 기본 브랜치 `main`, `main` 보호(강제 푸시·삭제 금지, 관리자 포함), 배포 키 `orchd <이름>`이 1개이고 쓰기 여부가 고른 게시 권한과 같음, **계정에 다른 배포 키가 늘지 않음**. 원장에 `project_create`(`ok:true`)와 `project_clone`(`ok:true`) 한 줄씩. 등록부(1-6 호출)에 새 항목: `provider: github`, `remote.path: 소유자/이름`(GitHub 철자), `ai_context: false` | **`create_partial`**: 화면이 **GitHub에 남은 것의 경로와 단계**를 보여 준다("이 도구는 지우지 않습니다"): 경로를 적어 두고 GitHub에서 직접 지운다(같은 이름은 지우기 전까지 `project_name_taken`). 단계: `project`(만들어졌는지 불확실), `first_commit`(일부 파일만), `default_branch`(첫 파일이 `main`을 기본 브랜치로 만들지 않음 → 가정이 틀림, 보고), **`protect`(무료 요금제의 비공개이거나 보호 규칙 호출 실패: 배포 키·등록부는 만들어지지 않았다)**, `deploy_key`, `register`. `key_staging_failed`: `ssh` 폴더에 키 폴더를 만들 수 없음(사전 점검 b, 아무것도 쓰지 않았다). `key_expired`: 제안 뒤 15분이 지났거나 orchd가 재시작됨 → 처음부터 다시. `clone.status: failed`(`host_not_pinned`·`key_unavailable`·`clone_failed`…): 프로젝트는 만들어졌고 화면의 "다시 복제"로 서명 없이 다시 한다. **복제가 키를 못 받아들이면 OpenSSH 9.6과 Go가 만든 키 형식의 문제다: 보고** |
| 3-7 | **[사용자 결정] 기존 저장소 연결(`link_project`).** ⚠ **이미 있는 저장소의 Deploy keys에 키가 등록되고 이 도구는 지우지 못한다.** 선택지 (a) 하지 않는다(기본), (b) 저장소 하나를 연결(**프로젝트 이름은 입력칸에서 정한다**: 저장소 이름과 달라도 되고 `^[a-z][a-z0-9-]{1,39}$`이면 된다). "기존 저장소 연결": 제공자·원격 경로(`소유자/이름`)·게시 권한(**기본 읽기**)·AI 전송(끔) → 확인 화면의 경고문("이미 있는 저장소의 설정에 이 서버의 배포 키가 등록됩니다…")과 키 지문 → 서명 | 성공 후 `/home/orch/repos/<id>` 복제, `keys/<id>/` 키 쌍, 등록부에 `remote.path`가 GitHub 철자, 원장 `project_link`. **GitHub(사용자):** 그 저장소에 `orchd <id>` 키 1개, 저장소의 다른 설정·파일은 그대로. **같은 키가 이미 있으면 성공이 아니라 거부**(`github_refused`) | `repo_not_found`(토큰이 볼 수 없는 저장소), `already_linked`(이미 그 저장소가 다른 프로젝트에 연결됨), **`link_partial`**(키가 붙었을 수 있다: **저장소는 지우지 말고** GitHub Deploy keys에서 `orchd`로 시작하는 키만 제거) |
| 3-8 | **원격 연결(`attach_remote`).** (a) **[사용자] 정적 원격 거부 확인(GitHub에 아무것도 쓰지 않음):** `sample-app` 행의 "원격 연결"에서 아무 GitHub 저장소 경로를 넣어 확인. **게시 권한을 쓰기로 고르면 확인 화면에 굵은 경고("[중요] 게시 권한이 쓰기입니다: 이 프로젝트의 로컬 저장소 기록(커밋 이력)이 이 원격으로 올라갈 수 있습니다. 원격이 공개 저장소라면 누구나 그 기록을 읽을 수 있습니다")가 한 번 나온다**(읽기에는 없다). attach는 **그 프로젝트에 대기·게시 대기·승인 대기 작업이 없을 때만** 한다 | **`sample-app`의 `remote`가 비어 있으면(`--git-remote-for` 정적 항목만 있음)** **`static_remote_wins`** 고정 문장이 서명 전에 나온다. **`remote`가 이미 있으면(옛 매핑이 있는 프로젝트)** "원격 연결" 단추가 아예 없거나 `already_linked`가 나오며 **이것은 정상이고 멈출 이유가 아니다**(옛 매핑이 있는 프로젝트에는 연결하지 않는다) | 제안이 열리면(둘 다 아님) 멈추고 보고 |
| 3-8b | **[사용자 결정] 실제 연결:** 이미 등록되어 provider·원격이 없는 **새 시험용 로컬 프로젝트**에만(`toy`·`sample-app` 같은 운영 프로젝트의 게시 경로를 바꾸지 않는다) | `ai_context`·경로·종류 불변, `provider`·`remote`만 설정, 배포 키 1개 등록(GitHub), `keys/<id>/` | `already_linked`(원격 매핑이 이미 있음), `link_partial` |
| 3-9 | **[사용자 결정] 새 GitHub 프로젝트에서 작은 작업 한 번(게시 확인).** 게시 권한을 **쓰기**로 만들었고 `registered`가 L1일 때만 의미가 있다. `orch submit --repo /home/orch/repos/<이름> --base <SHA> --class registered --agent claude --prompt '…'`(문서 한 줄 같은 작은 작업) | 게이트(`basic` 템플릿: **검사 없음**)를 통과하고 `push_branch`가 **해석기가 만든 원격(`ssh://git@github.com/…`)과 그 프로젝트의 키**로 `orch/<작업번호>` 브랜치를 올린다. GitHub 화면에 그 브랜치가 보인다(**PR은 만들지 않는다**: W3d). 보호된 `main`은 건드리지 않는다 | `publish` 고정 오류(해석기가 "알 수 없음 = 거부": 등록부·`known_hosts_gitlab`·키 모드), push 거부(읽기 키이거나 보호 규칙) |
| 3-10 | **브로커를 멈춘 상태**(선택): `sudo systemctl stop orch-broker` → `#/providers`와 새 프로젝트 화면(GitHub·연결)을 연다 | 고정 오류(브로커에 연결할 수 없음). 작업 제출·취소·**이미 복제된 프로젝트로의 작업 실행**은 그대로(게시는 "알 수 없음 = 거부" 가능). 끝나면 `sudo systemctl start orch-broker` | 기존 기능까지 막히면 단계 4 |
| 3-11 | **[컨트롤러] 누출 점검(읽기 전용).** 표 아래 "3-11 점검 명령"의 코드 블록을 실행한다 | 모드: 폴더 700, 파일 600. **원장:** 키 **`action,actor,code,ok,subject`뿐**(경로·키·소유자·값이 없음). **로그:** 개수 **`0`**. **개인 키 위치:** **`/home/orch/orchd/ssh/id_gitlab_deploy`와 `/home/orch/orchd/ssh/keys/<이름>/id_ed25519`뿐**(`keys-staging`·원장·로그·DB·broker 상태에는 없다). `.clone-*` 없음 | 값이나 토큰, 스테이징 개인 키가 보이면 **멈추고 보고한다.** GitHub에서 그 토큰을 즉시 폐기한다(사용자 결정) |
| 3-12 | **[사용자] 정리.** 3-6~3-8에서 만든 시험 저장소를 GitHub에서 직접 지우고, 연결한 기존 저장소의 `orchd` 배포 키를 **연결을 계속 쓰지 않을 때만** 지운다(지우면 그 프로젝트의 복제·게시는 실패한다). 토큰은 시험이 끝나면 GitHub에서 폐기하거나 권한을 줄이고 `#/tokens`에서 지운다(**서명 필요**) | 이 도구는 아무것도 지우지 않는다. 등록부·`/home/orch/repos`·`ssh/keys/`는 남는다(⚠ 1) | — |

**3-11 점검 명령** (표 안에 `\|`를 쓰지 않으려고 표 밖에 둔다):
```
sudo sh -c 'stat -c "%U:%G %a %n" /var/lib/orch-broker /var/lib/orch-broker/*.json /var/lib/orch-broker/secrets'
sudo grep -e provider_change -e project_create -e project_clone -e project_link -e project_attach /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl | tail -10
sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager | grep -c -e 'PRIVATE KEY' -e ghp_ -e github_pat_ -e Authorization -e Bearer -e PRIVATE-TOKEN
sudo grep -rl 'PRIVATE KEY' /home/orch/orchd /var/lib/orch-broker /run/orch-broker 2>/dev/null
sudo ls -a /home/orch/repos
```
(`grep -c`가 0을 찍으면 종료 코드 1이다: 정상이다. 원장 줄에서 키 이름을 눈으로 본다: `action,actor,code,ok,subject`.)

**검증 체크리스트(요약):** 3-1(기존 화면과 비밀 입력칸 없음), 3-2·3-3(실제 서명으로 토큰·제공자 저장), 3-4(GitHub 소유자 목록, 공개 여부 기본값 없음), 3-5(도달·호스트 키), 3-6~3-9 중 사용자가 한 것이 예상대로(특히 키 폴더 모드, 지문 일치, 복제), 3-10에서 기존 기능이 살아 있고, 3-11의 모드·원장·로그·개인 키 위치. 결과(**확인함/못 함**)는 결과 문서 27번 6장에 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다. **GitHub에 의도하지 않은 것이 만들어졌거나, 원장·로그·화면에 토큰·개인 키가 보이거나, 등록부가 깨졌으면(`registry_unavailable`) 즉시 broker를 멈추고(단계 4의 A) 사용자에게 보고한다.** 의도하지 않은 GitHub 저장소·배포 키는 **자동으로 지워지지 않는다**: 사용자가 GitHub에서 지운다.

### 단계 4. 롤백 [사용자][주의]
**순서: orchd를 먼저 멈춘다 → A(broker) → B(orchd).** 옛(W3c) orchd와 새 broker가 함께 도는 순간이 없어야 한다(⚠ 3: 그 조합은 W3c에서 만든 프로젝트를 기본 원격으로 게시할 수 있다).
**전제(I4):** `orch list`와 `orch approvals`에 실행 중·대기·게시 대기·승인 대기 작업이 없다. **W3e로 만든 프로젝트(GitHub 프로젝트, 연결·원격 연결한 프로젝트)로 제출된 작업이 있으면 먼저 취소한다.** **롤백한 뒤에는 그런 프로젝트 폴더로 작업을 제출하지 않는다**: 옛(W3c) orchd는 `remote`를 알지 못해 그 프로젝트를 기본 원격(`--git-remote`)으로 게시할 수 있다(등록부를 v2로 되돌리면 그 프로젝트는 등록부에도 없다).

**A. broker를 W3c 상태로 되돌리기(DB는 건드리지 않는다). 바이너리와 등록부를 함께 되돌린다.**
1. `sudo systemctl stop orchd`, 이어서 `sudo systemctl stop orch-broker`
2. 바이너리를 **root 전용 백업에서** 복원: `sudo install -o root -g root -m 0755 /root/backup-w3e/bin/orch-broker /usr/local/bin/orch-broker`. `broker.json`은 이번에 바뀌지 않았다.
3. **등록부를 버전 2 사본으로 되돌린다**(W3c broker는 버전 3을 깨짐으로 본다): `sudo install -o orch-broker -g orch-broker -m 0600 /root/backup-w3e/var-lib-orch-broker/registry.json /var/lib/orch-broker/registry.json`(broker가 남긴 `registry.json.v2.bak`도 같은 내용이다. root 백업을 우선한다), 이어서 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 `"version":2`인지. **상태 폴더 전체를 되돌리지 않는다**(예산 기록·쓴 요청 ID·웹에서 등록한 토큰이 되돌아간다).
4. **다시 컷오버할 때를 위해, 이 롤백으로 남은 낡은 `.v2.bak`을 치운다**: `sudo mv /var/lib/orch-broker/registry.json.v2.bak /root/backup-w3e/registry.json.v2.bak.rolledback-$(date +%Y%m%d-%H%M%S)`(그대로 두면 다음 마이그레이션이 "이미 있는 백업"을 덮어쓰지 않고 낡은 파일을 그대로 둔다: 시작은 거부되지 않으므로 낡은 백업을 새것으로 오해하기 쉽다). `ssh/keys/<id>/`에 남은 폴더는 **같은 id를 다시 만드는 것을 막는다**(`project_exists`/`key_staging_failed`): 다시 만들 id의 키 폴더는 GitHub의 배포 키를 먼저 지운 뒤 사용자가 정리한다(아래 C).
5. `sudo systemctl start orch-broker`; 14장 1-5의 `providers`·`projects`·`status`·`tokens`가 W3c 상태(제공자 `example-group-gitlab`, `gitlab:{project,project_id}` 필드)를 주는지 본다.
6. **경고:** 컷오버 뒤 이 도구가 등록한 프로젝트·제공자·고아 기록은 복원한 등록부에 **없다**. GitHub의 저장소·배포 키, `/home/orch/repos/<이름>`, `ssh/keys/<이름>/`은 그대로 남는다(자동 삭제 없음). **고아 기록의 경로를 롤백 전에 화면에서 적어 둔다.** `secrets/`·`tokens.json`은 그대로 둔다: GitHub 토큰이 들어 있다면(옛 broker는 쓰지 않는다) **의심이 있으면 GitHub에서 폐기하고 `#/tokens`로 지운다.**

**B. `orchd`·CLI를 W3c 상태로 되돌리기(DB 복원 불필요).**
1. (orchd는 A-1에서 멈춰 있다)
2. `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3e/bin/$f /usr/local/lib/aigate/$f; done`(세 개는 항상 같이. **웹은 `orchd`에 들어 있으므로** GitHub 화면과 연결 화면도 함께 사라진다)
3. `sudo systemctl start orchd`. 브라우저 캐시면 강력 새로고침.

**C. `keys/` 폴더 처리.** `/home/orch/orchd/ssh/keys/`와 `keys-staging/`는 **그대로 둔다**(W3c orchd는 무시한다). 이미 GitHub에 등록된 배포 키의 개인 키가 거기 있다: **지우는 것은 사용자 결정이다**(되돌릴 수 없다). 지울 때는 먼저 GitHub의 Deploy keys에서 해당 키를 제거하고, 그다음 root가 `shred -u`로 개인 키를 지운다(`sudo find /home/orch/orchd/ssh/keys -type f -name id_ed25519 -exec shred -u {} +`). 롤백 직후에 지우면 W3e를 다시 올릴 때 그 프로젝트의 복제·게시가 `key_unavailable`이 된다(아래 E의 복구가 필요). `keys-staging`의 남은 항목은 옛 orchd가 정리하지 않는다: 서명 대기용 임시 키라 시작 때 새 orchd가 지운다.

**D. GitHub 쪽 정리.** 만든 저장소와 붙인 배포 키는 **사용자가 GitHub에서 직접** 지운다(⚠ 1). `known_hosts_gitlab`의 `github.com` 항목은 두어도 해롭지 않다(필요하면 `/root/backup-w3e/known_hosts_gitlab.pre-w3e`와 비교해 사람이 고친다). 정책 클래스는 바뀌지 않았다.

**E. 키가 없어졌을 때(운영자 복구).** GitHub 프로젝트가 등록되어 있는데 `keys/<id>/id_ed25519`가 없거나 쓸 수 없으면(키 옮기기 실패, 재시작이 보관 중이던 키를 잊음, 폴더 삭제 등) 그 프로젝트의 복제·게시는 `key_unavailable`/"안전한 원격 없음"으로 거부된다. 복구는 서버에서 **`orch` 계정으로**:
1. 필요하면 `mkdir -m 0700 /home/orch/orchd/ssh/keys`(없을 때), 이어서 `mkdir -m 0700 /home/orch/orchd/ssh/keys/<id>`(이미 있고 키가 틀리면 먼저 치운다).
2. `ssh-keygen -t ed25519 -N '' -C orchd-deploy -f /home/orch/orchd/ssh/keys/<id>/id_ed25519`(손으로 만든 키: orchd의 키는 Go가 만들지만 파일 형식은 같다. **이 단계만 개인 키가 사람의 터미널에서 만들어진다: 출력에 개인 키는 나오지 않는다.**)
3. `chmod 600 …/id_ed25519`, 소유자 `orch:orch`, 폴더 0700.
4. GitHub에서 저장소의 Settings > Deploy keys를 열어 그 프로젝트의 옛 `orchd` 키가 보이면 지우고(제목이 `orchd`로 시작), 새 `id_ed25519.pub`을 등록한다(쓰기 권한은 프로젝트의 게시 권한이 `write`일 때만).
5. 재시작은 필요 없다(키는 사용할 때마다 검사한다). 복제하지 않은 프로젝트면 `POST /api/v1/projects/<id>/clone`(웹의 "다시 복제"), 아니면 작은 시험 작업의 게시로 확인한다. 아직 재시작하지 않았고 `keys-staging`에 승격되지 않은 키가 남아 있으면 그 개인 키를 대신 쓸 수 있다: **어느 폴더인지는 시각이 아니라 지문으로 정한다** — `sudo ssh-keygen -lf /home/orch/orchd/ssh/keys-staging/*/id_ed25519.pub`의 지문이 GitHub 저장소의 Deploy keys에 그 프로젝트(`orchd <id>`)로 등록된 키의 지문과 같은 폴더를 찾아 그 폴더를 `keys/<id>/`로 옮기고 모드를 위 3처럼 맞춘다. 맞는 지문이 없으면 위 2의 새 키를 만든다.

**검증:** `journalctl -u orchd`·`journalctl -u orch-broker`에 오류가 없고, `orch list`가 단계 2 이전과 같으며, W3b의 토큰 화면(13장 3-1)·W3c의 제공자·프로젝트 화면(14장 3-1)이 동작한다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`, `ls -l /usr/local/bin /usr/local/lib/aigate /var/lib/orch-broker`와 `sudo ls -lR /root/backup-w3e | head -50`을 사용자에게 보고한다. W3c 이전까지 가야 하면 14장 단계 4(`/root/backup-w3c`).

### 관찰 기록 (컷오버 때 채운다: 결과 문서 27번 9장으로 옮긴다)
| 항목 | 예상 | 본 것 | 판정 |
|---|---|---|---|
| 1-5 등록부 | `"version":3`, 600, `.v2.bak` 600 + sha256 일치 | | |
| 1-6 `projects` 비교 | 같은 프로젝트·값, 연결 필드가 `remote` | | |
| 3-3 GitHub 제공자 실제 서명 | 저장됨 | | |
| 3-4 소유자 목록 | 내 계정 첫 줄, 그룹 만들기 없음, 공개 여부 기본값 없음 | | |
| 3-6 키 폴더 만들기·모드 | `orch:orch 700`/600/644, `keys-staging` 비어 있음 | | |
| 3-6 지문 일치(웹·`ssh-keygen -l`·GitHub) | 같음 | | |
| 3-6 첫 파일·기본 브랜치·보호 | `main`, 보호 켜짐(공개/유료) | | |
| 3-6 OpenSSH 9.6의 키 수용·복제 | `cloned` | | |
| 3-7/3-8 키 중복 거부, 연결 범위 | 거부 / `ai_context` 불변 | | |
| 3-11 원장·로그·개인 키 위치 | 키 5개뿐·0건·예상 두 곳 | | |


## 16. W3f 컷오버 런북 (프로젝트 삭제: 서버의 복제본·등록·키 지우기, 이 도구가 만든 원격 저장소 지우기, 연결한 저장소에서 배포 키만 떼기, 고아 기록 지우기, `orch-broker`·`orchd` 갱신)
설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `28-PLAN-webconsole-W3f.md`, 결과·시험·한계는 `29-RESULTS-webconsole-W3f.md`(**배포 현실 표**와 "실서버에서 확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더)을 읽고 쓴 것이지 서버나 실제 GitHub·GitLab에서 본 출력이 아니다(11~15장과 같은 원칙). **W3f는 이 도구가 처음으로 삭제를 하는 단계다:** 서버의 폴더·키·등록을 지우고, GitHub·GitLab의 저장소를 지우고(이 도구가 만든 것만), 배포 키를 뗀다. **원격 저장소 삭제는 되돌릴 수 없다.** 순서는 바꾸지 않는다. 다르게 보이면 그 단계에서 멈추고 단계 4(롤백)를 고려하되, **삭제가 이미 일어난 뒤의 롤백은 지운 것을 되살리지 못한다**(⚠ 2).

표기: **[사용자]** 사용자 본인의 작업 또는 명시적 승인이 필요, **[컨트롤러]** 읽기 전용 확인(서버 상태를 바꾸지 않음), **[되돌림 가능]** 아래 몇 줄로 원상 복구, **[주의]** 되돌리면 그 사이의 변경을 잃거나 이 도구가 되돌리지 못함. 경로는 11~15장과 같다(바이너리 `/usr/local/lib/aigate`와 `/usr/local/bin/orch-broker`, 설정 `/etc/orch-broker/broker.json`, 상태 `/var/lib/orch-broker`, broker 소켓 `/run/orch-broker/broker.sock`, orchd 설정 `/home/orch/orchd`, 저장소 `/home/orch/repos`). 리다이렉트(`>`·`>>`)가 필요한 명령은 `sudo sh -c '…'`로 쓴다. **비밀 값(토큰, 개인 키)은 이 절의 어떤 명령에도 넣지 않고 출력하지도 않으며, 채팅·터미널에 붙이지 않는다.**

**orchd DB 스키마는 이번에 바뀌지 않는다(v8 그대로)**: 롤백에 DB 복원이 필요 없다. **유닛·orchd 플래그·`broker.json`·방화벽·`known_hosts_gitlab`·정책은 바뀌지 않는다**(새 설정 키·새 플래그 없음). 새로 생기는 것: **등록부 `registry.json`이 버전 4로 올라간다(프로젝트에 `origin`과 선택 필드 `remote_deleted_at`이 붙는다: 아래 ⚠ 2)**, 올리기 전 원본을 **`registry.json.v3.bak`**(0600, broker가 한 번만 쓰고 덮어쓰지 않음)로 남긴다, broker op `remove_project`·`delete_remote_repo`·`remove_deploy_key`·`clear_orphan`(모두 패스키 서명 필요), orchd의 `/api/v1/projects/remove|delete-remote|remove-key/propose|apply`·`/api/v1/orphans/clear/propose|apply`·`GET /api/v1/projects/{id}/removal-preview`, 웹의 프로젝트 행 "삭제…"와 고아 목록의 "기록 지우기", orchd가 `/home/orch/repos/<id>`와 `/home/orch/orchd/ssh/keys/<id>`를 `.deleting-<무작위>`로 이름을 바꾼 뒤 지우는 것(시작 때 남은 것을 치움). **웹 파일은 orchd 바이너리 안에 있으므로 orchd를 갱신해야 새 화면이 보인다**(단계 2 전에는 옛 화면이다).

> **⚠ 먼저 읽을 것 (결정과 위험).**
> 1. **원격 저장소 삭제는 되돌릴 수 없다(사용자 결정).** GitHub의 `DELETE`는 즉시 지운다. GitLab은 인스턴스 설정에 따라 "삭제 예약"(복구 가능 기간)이 될 수 있으나 **이 도구는 복구하지 않고 응답을 그대로 보여 주지도 않는다**(화면은 "삭제 요청됨" 고정 문장). 삭제할 수 있는 것은 **이 도구가 만든 저장소(`origin: created`)뿐**이다. 연결한 저장소는 절대 지우지 못하고 이 도구가 붙인 배포 키만 뗄 수 있다. 서명 화면에서 **저장소 이름을 글자 그대로(대소문자까지) 직접 입력**해야 서명 단추가 켜진다.
> 2. **등록부가 버전 4로 올라간다. W3e broker는 버전 4 파일을 "깨짐"으로 보고 fail closed 한다(`registry_unavailable`).** 롤백은 바이너리만 되돌려서는 안 되고 `registry.json`을 버전 3 사본으로 되돌려야 한다(단계 4의 A). **`registry.json.v3.bak`은 broker가 마이그레이션 직전에 한 번만 쓰고 덮어쓰지 않는다: 이전 시도가 남긴 낡은 파일일 수 있으므로 롤백 전에 반드시 그것이 지금 기대하는 파일(루트 백업 `/root/backup-w3f`의 `registry.json`과 sha256이 같고 `"version":3`)인지 확인한다.** 그리고 **삭제가 이미 일어난 뒤에 등록부를 v3로 되돌리면 지워진 프로젝트의 행이 되살아난다**(폴더·키·원격은 없는데 행만 있다: 이 도구에는 그 행을 지우는 W3e 이하 기능이 없다). 삭제를 한 뒤에는 롤백보다 앞으로 고치는 쪽이 낫다.
> 3. **삭제 권한은 일반 토큰에 이미 있다(사용자 결정 2026-11-12, 별도 삭제 토큰 없음).** 코드는 삭제 호출을 정확히 정해진 행(GitHub 저장소·GitHub 배포 키·GitLab 프로젝트·GitLab 프로젝트의 배포 키 비활성, 키 찾기 읽기)으로만, `origin: created`인 저장소에만, 서명과 저장소 번호·이름 확인 뒤에만 보낸다. 그래도 **토큰이 유출되면 그 계정의 저장소를 지울 수 있다**(사용자가 감수). **GitHub 토큰의 종류에 따라 삭제 권한이 다르다: 클래식 토큰은 `delete_repo` 범위가, 세분화 토큰은 저장소 권한 `Administration: Read and write`가 있어야 저장소 삭제가 된다(공식 문서에서 사용자가 직접 확인: 이 문서는 값을 적지 않는다).** 권한이 없으면 삭제는 `remove_refused`로 끝나고 아무것도 지워지지 않는다. GitLab은 토큰 범위 `api`와 그 프로젝트의 소유자(또는 관리자) 권한이 필요하다. **만료일을 짧게 두고 시험이 끝나면 권한을 줄이는 것을 권한다.**
> 4. **시험 저장소 `w3e-smoke-1112`·`w3e-smoke-1113`의 원격은 이 도구로 지우지 못한다**(마이그레이션 뒤 `origin: unknown` → `origin_unknown`). 서버의 복제본·등록·키 폴더는 로컬 정리로 지울 수 있다. **GitHub의 저장소는 사용자가 GitHub 화면에서 직접 지운다**(저장소를 지우면 거기 등록된 배포 키도 함께 사라진다). 로컬 정리만 하고 저장소를 두면 그 저장소에는 `orchd <이름>` 배포 키가 남는다(화면의 "배포 키가 남을 수 있음" 안내).
> 5. **원격 삭제 시험은 이 컷오버 뒤에 새로 만든 시험 저장소(`origin: created`)에서만 한다.** 이미 있는 저장소(`toy`·`sample-app`·스모크 둘·W3c가 만든 프로젝트)로는 하지 않는다. 사용자가 이름을 정한 새 시험 저장소를 공개로 만들고 그것만 지운다.
> 6. **레거시 프로젝트(`toy`, `sample-app`)도 서버 쪽 삭제는 되지만 두 번째 확인과 서명되는 `legacy_ack`가 필요하다.** 원격 저장소(예: `sample-app`의 사내 GitLab)는 어떤 경우에도 이 도구로 지우지 못한다. **이 컷오버에서는 레거시 두 번째 확인 화면을 보기만 하고 서명하지 않는다**(3-6: 서명 전에 취소). 정적 `--git-remote-for`가 있는 `sample-app`를 나중에 지우면 orchd 유닛의 그 설정은 남는다(사람이 정리: 아래 "정적 원격 정리").
> 7. **삭제 한도는 메모리에만 있다.** 원격 저장소 삭제는 broker 전체 하루 3회·시간당 2회, 로컬 정리·키 떼기·고아 지우기는 시간당 10회이고, **broker를 재시작하면 이 카운터가 0으로 돌아간다**(한도를 우회하려고 재시작하지 않는다). 주체별 한도(orchd): 원격 삭제 시간당 3, 로컬 정리 10, 키 떼기 10, 고아 20, 미리보기 30.
> 8. **GitHub에는 이름으로만 지우는 `DELETE`가 있다(번호로 지우는 호출이 없다).** broker는 지우기 직전에 저장소를 읽어 번호와 이름이 등록부와 같은지 확인하지만 **그 읽기와 `DELETE` 사이에 같은 이름의 저장소가 다시 만들어지는 아주 짧은 틈은 남는다**(받아들인 위험, 서명한 번호가 확인 시점의 저장소와만 맞는다).
> 9. **결과를 알 수 없으면 아무것도 더 하지 않는다.** `DELETE` 뒤의 시간 초과·5xx·연결 끊김은 `delete_partial`("일부만 되었거나 결과를 알 수 없음")이고 **자동 재시도도 로컬 정리도 일어나지 않으며 서명은 소모된다.** 상태를 다시 읽고 필요하면 새 서명으로 한다(아래 "delete_partial와 `delete_partial_local` 다루기").
> 10. **미리보기는 서버의 `git`을 실행한다**(쓰기 등급 + CSRF 헤더 필요). 저장소 설정의 필터·훅은 실행하지 않도록 막았지만(시험으로 고정) **큰 복제본에서의 실제 시간(상한 25초)은 서버에서 본 적이 없다.** 결과는 개수와 예·아니오뿐이다(이름·경로·메시지 없음). 변경 개수를 알 수 없으면 화면은 "변경이 있을 수 있습니다"로 쓰고 지우겠다는 확인을 요구한다.
> 11. **시험 저장소를 지우는 일도 서명이 필요하고 되돌릴 수 없다.** 필요하지 않으면 3-4(원격 삭제)를 하지 않고 컷오버를 3-3에서 닫는다(권장 기본: 로컬 정리만 확인).
> 12. **절차가 요구하는 비밀 취급.** 이 절차의 어디에도 비밀을 채팅으로 보내라는 단계는 없다. 토큰 값은 웹 토큰 화면에만 입력한다.

### 16.0 시작 전에 모두 참이어야 하는 것
- **출발점: W3e 컷오버가 끝난 상태(15장).** **실서버의 현재 상태(2026-11-12 사용자·컨트롤러가 알려 준 사실, 이 저장소에서는 확인하지 못함):** broker·`orchd`가 W3e main `3830546`, 등록부 버전 3(`registry.json.v2.bak`가 남아 있음), 제공자 `example-group-gitlab`·`swlee3306-github`(ssh `ssh://git@ssh.github.com:443`), 토큰 `orch-api-token`·`swlee3306-github-token`, 정책 클래스 `registered` 있음, `known_hosts_gitlab`에 GitLab과 `[ssh.github.com]:443` 항목, 프로젝트 `toy`·`sample-app`(레거시)·W3c/W3e에서 만든 것·`w3e-smoke-1112`(`basic`)·`w3e-smoke-1113`(`go`), GitHub에 시험 저장소 둘(공개). 아래 점검 (a)로 이 사실을 서버에서 다시 본다.
  - **W3e 컷오버가 끝나지 않았다면** 15장을 먼저 끝까지 한다. 15장과 16장을 한 번에 합치지 않는다.
- **실행 중·대기 중·게시 대기·승인 대기 작업이 없다**(`orch list`, `orch approvals`: 끝난 작업의 기록은 있어도 된다). 단계 1은 broker를, 단계 2는 `orchd`를 재시작한다. **지울 시험 프로젝트에는 어떤 작업도 제출하지 않는다**(진행 중 작업이 있으면 `project_busy`).
- **웹 서명에 쓸 패스키 서명자가 등록되어 있다**(`sudo orch admin signers list`에 `webauthn` 형식). **새 서명 동작 4개는 아직 한 번도 실제 패스키로 확인하지 않았다.**
- **백업은 `/root/backup-w3f`(root 소유 0700)에만 둔다.** `orch`가 쓸 수 있는 폴더에는 두지 않는다. **사전 점검 전에 [컨트롤러/root]가 먼저 만든다**: `sudo install -d -m 0700 -o root -g root /root/backup-w3f /root/backup-w3f/bin`. **백업 복사는 모두 `cp -an`(덮어쓰지 않음)이다**: 이전 시도의 사본이 있으면 그것이 이번 시작 상태(등록부 v3)인지 `sudo grep -o '"version": *[0-9]*' …`로 보고, 아니면 옛 사본을 `이름.old-<날짜>`로 옮긴 뒤 복사한다.
- **사전 점검 (단계 1 전에, 전부 읽기 전용이고 값·개인 키를 찍지 않는 명령. 컨트롤러가 서버에서 직접 한다).** 하나라도 예상과 다르면 **시작하지 않고** 보고한다.
  - **(a) W3e 상태와 등록부.** `systemctl is-active orch-broker orchd` → `active` 둘. `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd`(결과를 기록: 롤백 확인에 쓴다). `sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json` → **`orch-broker:orch-broker 700`**, 파일 `orch-broker:orch-broker 600`. `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → **`"version":3`**(이미 4면 누군가 새 broker를 먼저 시작한 것: 멈추고 확인). `sudo ls /var/lib/orch-broker`에 `registry.json`, **`registry.json.v2.bak`**, `used-requests.json`, `budget.json`, `secrets`, `tokens.json`이 있고 **`registry.json.v3.bak`는 아직 없다**(있으면 낡은 파일이다: broker는 그 파일을 덮어쓰지 않고 그대로 두며 시작도 거부하지 않는다. 그러면 1-5의 `.v3.bak` sha256 비교가 root 백업과 어긋나 롤백 보조 사본이 쓸모없어진다. 단계 1 전에 `/root/backup-w3f/registry.json.v3.bak.old-<날짜>`로 옮기고 보고). 제공자와 프로젝트 읽기(값 없음)는 단계 1-2에서 `/root/backup-w3f`에 저장한다.
  - **(b) 저장소 루트와 프로젝트 경로.** `sudo stat -c '%U:%G %a %F %n' /home/orch/repos` → **`orch:orch 700 directory`**(심볼릭 링크가 아님). `sudo grep -n -A3 repo_roots /etc/orch-broker/broker.json`의 첫 항목이 `/home/orch/repos`. **등록부의 모든 프로젝트 경로가 `<루트>/<id>`와 글자 그대로 같은지**(같지 않은 행은 로컬 정리가 `local_unsafe`로 거부된다): 
    ```
    sudo -u orch python3 -c 'import socket,json;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"projects\"}\n");r=json.loads(s.makefile().readline())["result"];[print(p["id"],p["path"],"OK" if p["path"]=="/home/orch/repos/"+p["id"] else "DIFFERENT") for p in r["projects"]]'
    ```
    모든 줄이 `OK`여야 한다. `DIFFERENT`인 프로젝트는 이 기능으로 서버 쪽 삭제를 할 수 없다(원격 삭제와 키 떼기는 된다). `sudo find /home/orch/repos -mindepth 1 -maxdepth 1 -printf '%y %u:%g %m %f\n'` → 각 프로젝트가 `d orch:orch <모드> <이름>`(`l`이면 심볼릭 링크라 `local_unsafe`로 거부된다. 소유자가 `orch`가 아니면 그 프로젝트는 지우지 않는다: 등록이 먼저 지워진 뒤 폴더가 `.deleting-*`로 옮겨진 채 남고 시작 청소도 지우지 못한다). 각 프로젝트 폴더에 `.git` **디렉터리**가 있는지(`sudo sh -c 'ls -d /home/orch/repos/*/.git'`: `sudo` 앞에서는 `orch` 소유 0700 폴더의 글롭이 풀리지 않으므로 반드시 `sh -c`).
  - **(c) 키 폴더 소유자·모드.** `sudo find /home/orch/orchd/ssh/keys -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n'` → 프로젝트 폴더 `d orch:orch 700`, `id_ed25519` `f orch:orch 600`, `id_ed25519.pub` `f orch:orch 644`(키 폴더 소유자가 `orch`가 아니면 그 프로젝트의 키 폴더 제거가 `local_unsafe`/`key_unavailable_for_removal`로 거부된다). 키 폴더 자체도 본다: `sudo stat -c '%U:%G %a %F %n' /home/orch/orchd/ssh/keys` → `orch:orch 700 directory`, `sudo readlink -f /home/orch/orchd/ssh/keys` → 정확히 `/home/orch/orchd/ssh/keys`(경로 중간에 심볼릭 링크가 있으면 모든 로컬 정리가 `local_unsafe`). 같은 글롭 주의: `find`는 괜찮지만 `sudo ls …/keys/*`는 `sudo sh -c`로 감싼다.
  - **(d) 저장소 루트 아래에 마운트·바인드 마운트·다른 파일 시스템이 없다**(이름 바꾸기가 한 파일 시스템 안에서만 된다): `findmnt -rn -o TARGET | grep -e '^/home/orch/repos' -e '^/home/orch/orchd'`(출력 없음, 종료 코드 1이 정상), `sudo sh -c 'stat -c "%d %n" /home/orch/repos /home/orch/repos/* /home/orch/orchd/ssh/keys /home/orch/orchd/ssh/keys/*'`(같은 줄의 첫 열 값이 같아야 한다: 한 폴더 그룹 안에서 다르면 멈춘다).
  - **(e) 유닛 샌드박스(바뀌지 않았는지).** `systemctl show orchd -p User -p ReadWritePaths -p ProtectSystem -p ProtectHome -p PrivateTmp` → `User=orch`, `ReadWritePaths`에 **`/home/orch/orchd`**와 **`/home/orch/repos`**, `ProtectSystem=strict`, `ProtectHome=read-only`, `PrivateTmp=yes`. 쓰기 가능성만 본다(만들지 않는다): `sudo systemd-run --wait --pipe -p User=orch -p ProtectSystem=strict -p ProtectHome=read-only -p 'ReadWritePaths=/home/orch/orchd /home/orch/repos' -p PrivateTmp=yes /bin/sh -c 'test -w /home/orch/repos && test -w /home/orch/orchd/ssh/keys && echo writable'`(예상 `writable`). **이 점검은 이름 바꾸기·`RemoveAll`이 실제로 되는지를 보증하지 못한다**(그것은 3-2의 첫 실제 로컬 정리에서 처음 드러난다: 못 하면 `delete_partial_local`/`local_unsafe`, 등록은 먼저 지워졌을 수 있다: 아래 복구).
  - **(f) 서버의 `git`.** `sudo -u orch sh -c 'git --version'`(예상 2.43). 미리보기가 이 `git`을 쓴다.
  - **(g) 토큰과 삭제 권한(사용자 몫: 서버에서는 볼 수 없다).** [사용자] GitHub 토큰 화면에서 `swlee3306-github-token`의 종류와 권한을 본다: 클래식이면 `delete_repo` 범위, 세분화면 대상 저장소(새 시험 저장소가 "선택한 저장소"에 들어가는지 포함)와 `Administration: Read and write`. 권한이 없으면 3-4의 삭제가 `remove_refused`로 끝난다(안전한 실패). [사용자] GitLab 토큰 `orch-api-token`의 범위(`api`)와 소유자 권한. 이 도구와 컨트롤러는 토큰 값을 읽지 않는다.
  - **(h) 정책 `registered`와 `known_hosts_gitlab`(바뀌지 않음).** `sudo grep -n -B1 -A2 registered /home/orch/orchd/policy.yml`, `sudo ssh-keygen -F '[ssh.github.com]:443' -f /home/orch/orchd/ssh/known_hosts_gitlab`(항목 있음). 새 시험 저장소를 만들 때(3-4)만 쓰인다.
  - **(i) 샌드박스 안 `-check` 기준선.** 현재(W3e) 바이너리로 단계 1-4의 `-check` 명령을 한 번 돌려 출력을 기록한다.
  - **(j) 아무것도 돌고 있지 않다.** `orch list`, `orch approvals`.
  - **서버에서 쓰기 시험을 따로 하지 않는다.** 위 점검으로 알 수 없는 것(GitHub·GitLab의 실제 `DELETE` 응답, 실제 폴더 삭제, 실제 패스키 서명, 큰 복제본의 미리보기 시간)은 단계 3에서 처음 드러난다.

**사용자 사전 작업 (컨트롤러가 대신할 수 없다).**
1. **[사용자 결정] 이 컷오버에서 무엇을 할지.** (1) 로컬 정리 시험(3-2: `w3e-smoke-1112`의 서버 쪽만 지움, 원격은 그대로), (2) 배포 키만 떼기 시험(3-3: `w3e-smoke-1113`, 이 프로젝트는 그 뒤 게시를 못 한다), (3) **원격 저장소 삭제 시험(3-4, 되돌릴 수 없음)은 새로 만든 시험 저장소 하나에만**(없으면 하지 않는다), (4) 레거시 두 번째 확인 화면 보기(3-6, 서명하지 않음), (5) 고아 기록이 있으면 지우기(3-7). 하지 않을 것은 건너뛴다.
2. **[사용자 결정] GitHub 토큰 권한**(위 ⚠ 3, (g)). 삭제 시험이 끝나면 권한을 줄일지·토큰을 새로 만들지.
3. **스모크 저장소 둘의 GitHub 쪽 정리는 사용자가 한다**(3-9). 이 도구로는 지우지 못한다.
4. **새 시험 저장소의 이름과 공개 여부**(3-4용): `^[a-z][a-z0-9-]{1,39}$`, 예 `w3f-smoke-1114`. 공개 저장소는 누구나 읽는다. 끝난 뒤 이 도구로 지운다.

**알아둘 것 (코드 읽기와 시험 기준, 실서버·실제 GitHub 미확인).**
0. **웹에서 만든 모든 삭제는 서명이 필요하다.** 확인 화면에 지울 대상 전체가 평문으로 나오고(원격 이름과 주소, 복제본 경로, 키 폴더, 원격에 올라가지 않은 커밋 수·변경 파일 수, **되돌릴 수 없음**) **브라우저가 `change` 해시를 다시 계산해 서버의 값과 같고 서버가 보낸 문장이 브라우저가 기대하는 문장과 정확히 같을 때만** 서명 단추가 켜진다.
1. **지우기의 종류 넷.** (a) "복제본과 등록 지우기": `remove_project`(서버의 복제본·키 폴더·등록, 원격은 그대로, 되돌릴 수 있는 쪽), (b) "복제본과 원격 저장소 지우기": `delete_remote_repo`(+ 선택 `also_local`: 같은 서명으로 원격을 지운 뒤 복제본·키 폴더까지 지움, **등록은 "원격 없음" 상태로 남고 지우려면 두 번째 서명인 (a)가 필요하다. 자동으로 하지 않는다**), (c) "배포 키만 떼기": `remove_deploy_key`(저장소와 등록은 그대로, 연결한 저장소를 정리할 때), (d) 고아 목록의 "기록 지우기": `clear_orphan`(외부 호출 없음).
2. **서버 쪽 삭제가 거부되는 경우(서명 전):** 진행 중 작업이 있음(`project_busy`), 폴더가 심볼릭 링크이거나 `<루트>/<id>`가 아님(`local_unsafe`), 다른 프로젝트의 경로 안/위에 있음(`local_unsafe`), 레거시를 두 번째 확인 없이(`legacy_ack_required`), 원격 삭제에서 출처가 `linked`(`not_created_by_tool`)·`unknown`·`legacy`(`origin_unknown`), 저장소 번호·이름이 달라짐(`remote_changed`), 입력한 이름이 다름(`confirm_mismatch`). 이 거부들은 서명을 소모하지 않는다.
3. **순서(되돌릴 수 없는 일부터 하지 않는다).** 원격 삭제: 서명·모양 검증과 `project_busy` 확인 → broker가 저장소를 읽어 번호·이름 확인 → `DELETE` 한 번 → 등록부에 `remote: 없음`+`remote_deleted_at` → (`also_local`이면) orchd가 복제본·키 폴더 삭제. **`Provider`는 등록부에 남는다**(그래서 이 프로젝트에 다른 저장소를 `attach`할 수 없다: `already_linked`). `remove_project`는 broker가 서명을 확인하고 행을 지운 뒤 orchd가 폴더를 지운다(등록이 먼저 지워진다: 폴더 삭제가 실패하면 등록 없는 폴더가 남는다: 아래 복구).
4. **복제본에 원격에 올라가지 않은 커밋·수정이 있으면** 확인 화면이 개수를 보이고 지우겠다는 체크를 요구한다. **원격이 이미 지워진 프로젝트(또는 `also_local`)에서는 복제본이 유일한 사본일 수 있다는 경고가 항상 나온다.**
5. **키 떼기**는 orchd가 그 프로젝트의 `id_ed25519.pub`을 읽어 broker에 주고(브라우저는 키를 보내지 않는다), broker가 제목 `orchd <프로젝트 id>`**와** 공개 키 본문이 둘 다 맞는 키 하나만 뗀다(다른 키는 건드리지 않는다). GitLab은 그 프로젝트에서만 키를 비활성화한다(공용 키를 지우는 호출은 하지 않는다. 다만 GitLab은 그 키를 쓰는 마지막 프로젝트에서 떼면 키 레코드를 스스로 지운다). 공개 키 파일이 없으면 `key_unavailable_for_removal`: GitHub 화면에서 사람이 지운다.
6. **알 수 없는 결과.** `delete_partial`·`outcome_unknown`·시간 초과·연결 끊김: 화면이 상태를 다시 읽고 안내하며 자동으로 다시 하지 않는다. 같은 서명은 쓸 수 없다.
7. **원장:** 새 이벤트 `project_remove`·`remote_delete`·`key_remove`·`orphan_clear`, 키는 `actor,action,subject,ok,code`뿐(경로·이름·원격 주소 없음).
8. **롤백하면 W3e broker는 등록부 버전 4를 모른다**(⚠ 2).

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 29번 6장에 "확인함/못 함"으로 남긴다)
1. **실제 GitHub `DELETE /repos/{owner}/{repo}`:** 토큰 종류(클래식 `delete_repo` / 세분화 `Administration: write`)별 성공(204 가정)과 권한 부족(403/404 가정 → `remove_refused`), 지연이 있는지.
2. **GitLab `DELETE /projects/{id}`:** 성공 상태(202 가정, 문서에 없음)와 인스턴스의 지연 삭제, 키 비활성(`DELETE /projects/{id}/deploy_keys/{key_id}`)의 성공(204 가정)과 공용 키가 다른 프로젝트에서 계속 쓰이는지.
3. **GitHub 이름 수준 TOCTOU**(⚠ 8: 관찰 불가능한 위험, 시험으로 보이지 못함).
4. **orchd가 실제 유닛 샌드박스에서 이름 바꾸기+`RemoveAll`을 하는지**(소유자·`ReadWritePaths`·마운트 없음), `.deleting-*` 시작 청소.
5. **큰 복제본의 미리보기 시간(상한 25초)과 `git` 2.43에서 개수가 맞는지.**
6. **등록부 v3→v4 마이그레이션을 실서버 파일·uid·샌드박스로**(1-4)와 `registry.json.v3.bak`.
7. **IME·붙여넣기·포커스(저장소 이름 입력칸)를 실제 Chrome에서**, 실제 orchd에서의 CSRF 헤더(미리보기 GET), 실제 패스키가 만든 새 동작 4개의 서명을 broker가 검증하는지.
8. **390px에서 보지 못한 화면**(결과 문서 29번 6장의 목록).

### 단계 1. broker 갱신·등록부 마이그레이션 [사용자][주의: 등록부가 버전 4로 올라간다 → 롤백은 단계 4의 A]
**순서: (orchd 정지) → 백업 → (Mac) 게이트·빌드 → 옛 broker 정지 → 새 바이너리 설치 → 샌드박스 안에서 `-check` → 시작 → 읽기 점검.** `broker.json`은 바뀌지 않는다.

**1-0. 빌드 전 게이트 (Mac, 저장소에서). 하나라도 실패하면 빌드하지 않는다.**
```
go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api     # --- PASS 이고 SKIP 이 없어야 한다
go vet ./... ; gofmt -l .                                         # gofmt 출력 없음
GOOS=darwin go vet ./cmd/... ./internal/... ; GOOS=linux go vet ./cmd/... ./internal/...
go test ./... -count=1 -race
node --test "internal/orch/web/test/*.test.js"
```
(결과 문서 29번 2장의 숫자와 같은 커밋인지 확인한다.)

**1-1. 새 바이너리 (Mac).** `GOOS=linux GOARCH=amd64 go build -o /tmp/w3f/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고(`uname -m`에 맞춘다) `scp`로 VM `/tmp/w3f/`에 보낸다. **아직 설치하지 않는다.**

**1-2. `orchd`를 멈추고 백업한다. [사용자]** 먼저 실행 중·대기·게시 대기·승인 대기 작업이 없는지 본다(`orch list`, `orch approvals`). 없으면 `sudo systemctl stop orchd`. 이 시점부터 단계 2의 시작까지 **웹 콘솔·작업 처리·게시가 멈춘다.** (W3f는 broker→orchd 응답에 필드를 더하기만 해서 옛 orchd와 새 broker가 만나도 게시가 열려 있지는 않지만, 15장과 같은 순서를 지킨다: 옛 orchd는 `origin`을 무시하고, 새 orchd와 옛 broker는 삭제 op에 `unknown_op`을 받아 "broker 연결 불가"로 보인다.)

**백업(`cp -an`: 덮어쓰지 않음). `/root/backup-w3f`(root 0700, 16.0에서 이미 만듦).** broker의 상태 폴더는 **broker를 멈춘 뒤 1-3에서** 복사한다.
```
B=/root/backup-w3f
sudo install -d -m 0700 -o root -g root $B $B/bin       # 이미 있으면 그대로
sudo stat -c '%U:%G %a %n' $B          # root:root 700 이어야 한다
sudo cp -an /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -an /usr/local/lib/aigate/$f $B/bin/$f; done
sudo cp -an /etc/orch-broker $B/etc-orch-broker
sudo cp -an /home/orch/orchd/policy.yml $B/policy.yml.pre-w3f
sudo cp -an /home/orch/orchd/ssh/known_hosts_gitlab $B/known_hosts_gitlab.pre-w3f
sudo cp -an /etc/systemd/system/orch-broker.service $B/orch-broker.service.pre-w3f
sudo cp -an /etc/systemd/system/orchd.service $B/orchd.service.pre-w3f; sudo cp -an /etc/systemd/system/orchd.service.d $B/ 2>/dev/null
# 컷오버 전후 비교용 읽기 응답(값 없음)
for op in providers projects; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op | sudo tee $B/$op-before.json >/dev/null; done
# 목록만(개인 키는 복사하지 않는다): 키 폴더·저장소 폴더의 이름·소유자·모드
sudo sh -c "find /home/orch/orchd/ssh/keys /home/orch/repos -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n' > $B/tree-before.txt"
sudo ls -lR $B | head -50
```
(소켓은 `orch` 계정만 쓸 수 있어 읽기는 `sudo -u orch`로 하고 root 전용 폴더에는 `sudo tee`로 저장한다.) 백업에는 `openai.key`(0400)와 **토큰 파일 사본(1-3의 상태 폴더 사본의 `secrets/`)**이 들어간다: **`orch`가 쓸 수 있는 폴더에 두지 않는다.** 안정되면 사용자가 `sudo find /root/backup-w3f -path '*secrets*' -type f -exec shred -u {} +`와 `sudo shred -u /root/backup-w3f/etc-orch-broker/openai.key`로 비밀 사본을 지운다. **개인 키는 백업하지 않는다**: 지운 키 폴더는 되살릴 수 없고, 필요하면 새 키로 다시 연결한다.

**1-3. 옛 broker 정지 → 새 바이너리 설치.** **옛 broker를 반드시 먼저 멈춘다**(읽기와 쓰기 사이에 잠금이 없다). (정지한 뒤 1-4가 실패하면 broker는 멈춘 채로 남는다: 곧바로 단계 4의 A.)
```
sudo systemctl stop orch-broker
sudo cp -an /var/lib/orch-broker $B/var-lib-orch-broker       # registry.json(버전 3)이 여기 있다: 롤백의 핵심. broker가 멈춘 뒤의 사본이다
sudo grep -o '"version": *[0-9]*' $B/var-lib-orch-broker/registry.json    # "version":3 (아니면 16.0의 `cp -an` 주의를 본다)
sudo sha256sum $B/var-lib-orch-broker/registry.json /var/lib/orch-broker/registry.json       # 같아야 한다(기록)
sudo install -o root -g root -m 0755 /tmp/w3f/orch-broker /usr/local/bin/orch-broker
```
**1-4. 샌드박스 안에서 `-check`(시작 전).** 유닛과 같은 샌드박스 옵션(13~15장과 같은 명령):
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…)` 한 줄과 `Finished with result: success`(종료 코드 0), 사전 점검 (i)의 기준선과 같은 경고만. **`-check`는 등록부를 올리지 않는다**: 끝난 뒤에도 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 **`"version":3`** 이어야 한다. 실패 문장이 나오면 **시작하지 않고 멈춘다 → 단계 4의 A.**

**1-5. 시작(등록부가 여기서 버전 4로 올라간다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker /var/lib/orch-broker/registry.json /var/lib/orch-broker/registry.json.v3.bak
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json
sudo sha256sum /var/lib/orch-broker/registry.json.v3.bak /root/backup-w3f/var-lib-orch-broker/registry.json
sudo ls -a /var/lib/orch-broker | grep -c '^\.state-' || true
```
예상: 유닛 `active (running)`, 로그에 오류 없음. 폴더 `orch-broker:orch-broker 700`, `registry.json`은 **`600`이고 `"version":4`**, **`registry.json.v3.bak`가 `orch-broker:orch-broker 600`으로 있고 root 백업의 `registry.json`과 sha256이 같다**(같지 않으면 낡은 `.v3.bak`가 있었던 것: 멈추고 보고), `registry.json.v2.bak`는 그대로, 임시 파일 `.state-*` 없음(0). **파일이 버전 3 그대로이거나 broker가 시작하지 않으면 멈춘다 → 사전 점검 a·e와 유닛 샌드박스를 다시 보고 단계 4의 A.**

**1-6. 읽기 op와 회귀.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기, 15장 1-6과 같은 명령):
```
for op in providers projects status tokens; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op; done
```
예상: `providers`는 두 제공자(내용 불변). `projects`는 단계 1 전(`$B/projects-before.json`)과 같은 프로젝트·`ai_context`·`provider`·`remote`이고 **모든 프로젝트에 `"origin"`이 있다: `toy`·`sample-app`는 `legacy`, 그 밖의 모든 프로젝트(`w3e-smoke-1112`·`w3e-smoke-1113` 포함)는 `unknown`**. `remote_deleted_at`은 없다. `status`·`tokens`는 이전과 같다. **프로젝트 목록·`ai_context`·`remote`가 바뀌었거나 `origin`이 위와 다르면 멈춘다 → 단계 4의 A.**

**검증 체크리스트:** 1-4 통과(등록부 아직 v3), 1-5의 유닛 active·`registry.json` 600·`"version":4`·`.v3.bak` 600과 sha256 일치, 1-6의 읽기 응답. **그래도 안 되면:** 단계 4의 A. orchd는 멈춘 상태다(1-2).

### 단계 2. `orchd` 갱신 [사용자]
**DB 스키마가 바뀌지 않으므로 DB를 복원할 필요가 없다.** `orchd`는 1-2에서 이미 멈춰 있다. **웹 파일이 orchd 안에 있으므로 이 단계가 끝나야 새 화면이 보인다.**
```
sudo systemctl is-active orchd          # inactive
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3f/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
```
유닛과 드롭인은 바꾸지 않는다. **검증 체크리스트**
- 로그: 오류 없이 대기, `approvals: mode signed`, 스키마 오류 없음. 시작 때 `.deleting-*` 청소는 한 일이 없다면 로그에 아무것도 없다.
- `orch list`: 단계 1 이전의 작업이 모두 그대로, 마지막 번호가 같다.
- **새 바이너리가 설치되어 도는지: 해시를 비교한다.** `sudo sha256sum /usr/local/lib/aigate/orchd /tmp/w3f/orchd`의 두 값이 같아야 한다(`orch`·`aigate`도). 시작 시각은 `systemctl show orchd -p ActiveEnterTimestamp`.
- 로그인 없이는 닫혀 있는지: `curl -sk -o /dev/null -w '%{http_code}\n' https://<FQDN>/api/v1/projects/w3e-smoke-1112/removal-preview` → **401**(같은 방식으로 `POST /api/v1/projects/remove/propose`, `POST /api/v1/projects/delete-remote/propose`, `POST /api/v1/projects/remove-key/propose`, `POST /api/v1/orphans/clear/propose`도 401). **이것은 "로그인 없이는 닫혀 있다"는 것만 증명한다**(버전은 위 해시로 본다).
- 저장소 루트 아래에 `.deleting-*`·`.remove-refused-*` 이름이 없다: `sudo ls -a /home/orch/repos /home/orch/orchd/ssh/keys`.

**그래도 안 되면:** 단계 4(롤백: orchd를 먼저 멈춘 채 A→B 순서).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과, 사용자 사전 작업을 마쳤다. Mac 브라우저에서 `https://<FQDN>/`에 패스키로 로그인. "예상"은 코드·시험(`internal/orch/web/test/delete.test.js`, `internal/orch/api/w3f_f3_test.go`·`adversarial_w3f_test.go`, `internal/broker/delete_ops_test.go`·`adversarial_w3f_test.go`)에서 읽은 것이다. **순서를 지킨다: 되돌릴 수 있는 것(로컬 정리) 먼저, 되돌릴 수 없는 것(원격 삭제)은 마지막, 그리고 새로 만든 시험 저장소에만.**

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | **기존 화면 회귀.** `#/providers`, `#/projects` 열기 | 프로젝트 목록·제공자 목록이 단계 1 전과 같고 **각 프로젝트 행에 "삭제…" 단추**가 새로 보인다(진행 중인 일이 있는 동안은 꺼져 있고 이유 문장이 있다). 고아 알림이 있으면 "기록 지우기" 단추가 새로 보인다. 제공자 화면에 비밀 입력칸이 없다 | 단추가 없으면 브라우저 캐시(강력 새로고침), 목록이 비면 단계 1·2 |
| 3-2 | **[사용자] 로컬 정리 시험: `w3e-smoke-1112`(출처 `unknown`, 원격은 그대로).** 행의 "삭제…" → "복제본과 등록 지우기…"(원격 저장소는 선택지에 없거나 이유 문장과 함께 꺼져 있다: 출처를 모르는 프로젝트) → **미리보기**(원격에 올라가지 않은 커밋·수정한 파일 개수(작업 #47의 `orch/47` 브랜치는 원격에 올라가지 않았으므로 0이 아닐 것으로 예상한다: 개수를 읽고 "그래도 지운다" 체크 후 진행), 크기, 지울 폴더 경로·키 폴더, "원격은 그대로", GitHub 프로젝트라 **"배포 키가 남을 수 있음"** 안내) → 확인 화면(브라우저가 `change`를 다시 계산, 서버 문장이 기대와 정확히 같음) → 패스키 서명 | 결과 "완료": 단계별 표시(등록 지움, 복제본 지움, 키 폴더 지움). **서버(컨트롤러, 읽기 전용):** `sudo ls -a /home/orch/repos`(**`w3e-smoke-1112`가 없고 `.deleting-*`도 없다**), `sudo ls -a /home/orch/orchd/ssh/keys`(그 이름의 폴더 없음), 같은 방식으로 `projects` 읽기에 그 프로젝트가 없다. 원장에 `project_remove`(`ok:true`) 한 줄, 키는 `actor,action,subject,ok,code`뿐. **GitHub(사용자):** `w3e-smoke-1112` 저장소가 **그대로 있고** 배포 키 `orchd w3e-smoke-1112`가 **그대로 남아 있다**(3-9에서 저장소를 지우면 함께 사라진다). 다른 프로젝트(`w3e-smoke-1113` 등)의 폴더·키는 그대로 | `project_busy`(진행 중 작업: 끝나거나 취소한 뒤), `local_unsafe`(폴더가 심볼릭 링크이거나 소유자·경로가 이상함: 사전 점검 b·c·d 다시), 서명 거부(`bad_signature`: **새 동작의 실제 패스키 서명 검증은 처음**: 멈추고 보고), `delete_partial_local`(등록은 지워졌는데 폴더가 남음: 아래 복구), `busy_check_failed` |
| 3-3 | **[사용자 결정] 배포 키만 떼기 시험: `w3e-smoke-1113`.** 행의 "삭제…" → "배포 키만 떼기" → 확인 화면(**지문**이 서버가 계산한 `ssh-keygen -l`과 같아야 한다) → 서명. ⚠ 이 프로젝트는 그 뒤 **복제·게시를 못 한다**(키를 뗐다). 하지 않으면 건너뛴다 | 결과 "완료". **GitHub(사용자):** `w3e-smoke-1113` 저장소는 그대로이고 배포 키 `orchd w3e-smoke-1113`이 사라졌고 **다른 키는 그대로**(계정의 다른 배포 키가 줄지 않았다). 등록부·폴더·`ssh/keys/w3e-smoke-1113/`는 그대로(키 떼기는 서버의 파일을 지우지 않는다). 원장 `key_remove`. 연결한 기존 저장소(`origin: linked`)에서도 같은 동작이다(이 컷오버에는 연결한 저장소가 없다) | `key_not_found`(제목과 키 본문이 맞는 키가 없음: 이미 지웠거나 사람이 바꿈), `remove_refused`(제공자 거부 또는 같은 키가 둘 이상), `key_unavailable_for_removal`(공개 키 파일 없음: GitHub 화면에서 사람이 지운다), `github_refused`(토큰 권한: "아무것도 지우지 않았습니다" 문장) |
| 3-4 | **[사용자 결정, 되돌릴 수 없음] 원격 저장소 삭제 시험: 이 컷오버에서 새로 만든 시험 저장소 하나에만.** (a) "새 프로젝트 만들기"로 `w3f-smoke-1114`(예) 만들기: 제공자 `github`, 소유자 내 계정, 템플릿 `basic`, **공개**, 서명(15장 3-6과 같다). 복제 `cloned`, 등록부에 `origin: created` 확인(`projects` 읽기). (b) 그 행의 "삭제…" → "복제본과 원격 저장소 지우기…": 미리보기(원격 이름·주소, 복제본 경로, 키 폴더, 커밋·변경 개수, **굵은 "되돌릴 수 없음"**) → **저장소 이름을 글자 그대로 입력**(틀리면 서명 단추가 꺼져 있다: 대소문자 틀린 값으로 한 번 확인) → "이어서 복제본도 지움"을 고르면 **같은 서명으로 서버의 복제본도 지워지고 복제본이 유일한 사본일 수 있다는 경고와 지우겠다는 체크가 필요하다** → 서명 | (b) 결과: "원격 저장소 삭제를 요청했습니다"(GitHub는 완료) + 단계 표시(원격 삭제, 복제본·키 폴더 지움, **등록은 "원격 없음"으로 남음**). 원장에 `remote_delete`(이어서 복제본 지우기가 실패하면 `remote_delete` 줄이 두 개다: `ok:true` 뒤 `ok:false code:delete_partial_local`. 모순이 아니다). **GitHub(사용자):** `https://github.com/<소유자>/w3f-smoke-1114`가 **404**. **서버:** 복제본·키 폴더 없음. `projects` 읽기에서 그 프로젝트의 `remote`는 없고 `remote_deleted_at`이 있으며 `provider`는 **그대로**(그래서 같은 프로젝트에 다른 저장소를 `attach`할 수 없다). (c) **후속 "복제본과 등록 지우기…"**(결과 화면의 단추: 두 번째, 따로 서명): 미리보기는 "원격은 이미 지워졌다"를 강하게 경고 → 서명 → 등록이 지워진다. 이 서명은 자동으로 시작되지 않는다(단추를 눌러야 한다) | `remove_refused`(**토큰 권한 부족**: 위 ⚠ 3: 아무것도 지워지지 않았다: 저장소가 그대로 있는지 GitHub에서 확인), `remote_changed`(번호·이름이 등록부와 다름: 지우지 않았다), `not_created_by_tool`/`origin_unknown`(출처: 만든 저장소가 아니다), `confirm_mismatch`, `delete_rate_limited`(하루 3회·시간당 2회), `delete_partial`(**아래 "delete_partial와 `delete_partial_local` 다루기"**), `delete_partial_local` |
| 3-5 | **`attach_remote` 거부 확인(3-4 뒤, 선택, 저장소에 아무것도 쓰지 않음):** 3-4(b) 뒤 등록이 남은 동안 그 프로젝트에 "원격 연결"을 시도 | `already_linked`(제공자가 남아 있어서). 등록을 지운 뒤(3-4 c)에는 프로젝트가 없다 | 연결이 되면 멈추고 보고(`Provider`를 지우는 결함) |
| 3-6 | **레거시 두 번째 확인 화면 확인(`toy` 또는 `sample-app`): 서명하지 않는다 — 서명 전에 취소한다.** 행의 "삭제…" → "복제본과 등록 지우기…" → 미리보기(첫 화면에는 **서명으로 가는 단추가 없다**, "다음: 두 번째 확인"만) → 필요한 체크 후 "다음: 두 번째 확인" → **두 번째 화면**(굵은 문장 "이것은 처음부터 있던 프로젝트입니다. 서버의 복제본과 등록이 지워지고…", 원격에 안 올라간 커밋·변경 개수, **프로젝트 이름을 한 번 더 직접 입력**) → 틀린 이름 몇 개(대소문자, 공백)로 단추가 꺼진 채인지 보고 **여기서 "취소"(또는 이전 화면)로 나간다. 변경 확인·서명 단추를 누르지 않는다** | 예상 문장이 모두 보이고, 정확한 이름을 넣기 전에는 진행 단추가 꺼져 있다. **서버에는 아무 일도 일어나지 않는다:** `sudo ls -a /home/orch/repos`에 `toy`·`sample-app`가 그대로, 원장에 `project_remove` 없음, `orch` 로그에 propose 요청 없음. (`sample-app`의 정적 `--git-remote-for` 안내는 서명 전 확인 화면에 나오는 문장이라 이 단계에서는 보이지 않을 수 있다) | 첫 화면에 서명 단추가 보이거나 두 번째 화면 없이 서명으로 가면 **멈추고 보고한다**(레거시 안전장치의 결함) |
| 3-7 | **고아 기록 지우기(고아가 있을 때만).** 고아 알림이 있으면 항목의 "기록 지우기" → 출처별 문장(`created`: "이 도구가 원격 저장소를 지우지 않습니다", `linked`, 출처 불명) → 서명 | "고아 기록을 지웠습니다. 제공자에는 아무것도 하지 않았습니다." 외부 요청 없음. 원장 `orphan_clear`. 고아가 없으면 건너뛴다 | `orphan_unknown`(이미 지워짐) |
| 3-8 | **[컨트롤러] 누출 점검(읽기 전용).** 표 아래 "3-8 점검 명령"의 코드 블록을 실행한다 | 모드: 폴더 700, 파일 600. **원장:** 키 **`action,actor,code,ok,subject`뿐**. **로그:** 개수 **`0`**. **개인 키 위치:** `/home/orch/orchd/ssh/id_gitlab_deploy`와 남은 프로젝트의 `keys/<이름>/id_ed25519`뿐. `.deleting-*`·`.remove-refused-*` 없음. 삭제한 프로젝트의 폴더·키 폴더 없음 | 토큰·개인 키·경로가 보이면 **멈추고 보고한다**(GitHub에서 토큰을 즉시 폐기하는 것은 사용자 결정) |
| 3-9 | **[사용자] GitHub 쪽 정리(이 도구로 지우지 못하는 것).** `w3e-smoke-1112`(3-2 뒤)와 `w3e-smoke-1113`을 GitHub 화면에서 직접 지운다(저장소 Settings > Danger Zone). 지우면 거기 등록된 배포 키도 사라진다. 3-4의 저장소는 이미 지워졌다 | GitHub에서 둘 다 404. 이 도구의 등록부에는 `w3e-smoke-1113`이 남아 있을 수 있다: 3-2와 같은 로컬 정리로 지운다(`remote`가 이미 없는 것과 상관없이 `remove_project`는 된다) | — |
| 3-10 | **브로커를 멈춘 상태**(선택): `sudo systemctl stop orch-broker` → 프로젝트 행의 "삭제…"와 미리보기를 연다 | 미리보기·삭제 모두 고정 오류(브로커에 연결할 수 없음: 미리보기는 등록부에서 경로를 읽으므로 503 `broker_unavailable`). 작업 제출·취소·이미 복제된 프로젝트로의 작업 실행은 그대로. 끝나면 `sudo systemctl start orch-broker`(**한도 카운터가 0으로 돌아간다**: ⚠ 7) | 기존 기능까지 막히면 단계 4 |
| 3-11 | **[컨트롤러] 삭제 한도·재시작 확인(선택).** 3-4를 한 번 했다면 broker 로그에 그 삭제가 한 줄 있다(`delete_remote_repo` 성공, 토큰·이름 없음). | 한도 거부(`delete_rate_limited`)는 일부러 만들지 않는다 | — |

**3-8 점검 명령** (표 안에 `\|`를 쓰지 않으려고 표 밖에 둔다):
```
sudo sh -c 'stat -c "%U:%G %a %n" /var/lib/orch-broker /var/lib/orch-broker/*.json /var/lib/orch-broker/secrets'
sudo grep -e project_remove -e remote_delete -e key_remove -e orphan_clear /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl | tail -10
sudo grep -e project_remove -e remote_delete -e key_remove -e orphan_clear /home/orch/orchd/gate-home/.aigate/ledger/orchd.jsonl | grep -c -e '/home/' -e 'ssh://' -e 'https://' -e '.git"'
sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager | grep -c -e 'PRIVATE KEY' -e ghp_ -e github_pat_ -e glpat- -e Authorization -e Bearer -e PRIVATE-TOKEN
sudo grep -rl 'PRIVATE KEY' /home/orch/orchd /var/lib/orch-broker /run/orch-broker 2>/dev/null
sudo ls -a /home/orch/repos /home/orch/orchd/ssh/keys /home/orch/orchd/ssh/keys-staging
```
(`grep -c`가 0을 찍으면 종료 코드 1이다: 정상이다. 원장 줄에서 키 이름을 눈으로 본다: `action,actor,code,ok,subject`. 둘째 `grep -c`(경로·원격 주소가 원장에 있는지)는 `0`이어야 한다. 제공자 id(`example-group-gitlab` 등)나 오류 코드(`gitlab_refused`)는 원장에 있어도 정상이라 그 글자로는 찾지 않는다.)

**검증 체크리스트(요약):** 3-1(삭제… 단추), 3-2(로컬 정리가 폴더·키·등록을 지우고 원격은 그대로), 3-3(키만 떼고 다른 키·저장소는 그대로, 한 것만), 3-4(새 시험 저장소만 원격 삭제, 이름 입력 게이트, 등록 후속 서명이 자동이 아님), 3-6(레거시는 두 번째 화면 없이 서명으로 못 가고 서명 전에 취소), 3-8의 모드·원장·로그·개인 키 위치. 결과(**확인함/못 함**)는 결과 문서 29번 6장에 남긴다.

**그래도 안 되면:** 화면 문제(표시·문구)만이면 롤백하지 않고 기록한다. **의도하지 않은 것이 지워졌거나, 원장·로그·화면에 토큰·개인 키가 보이거나, 등록부가 깨졌으면(`registry_unavailable`) 즉시 broker를 멈추고(단계 4의 A) 사용자에게 보고한다.** 지워진 GitHub 저장소·폴더·키는 되살릴 수 없다.

### delete_partial와 `delete_partial_local` 다루기 (서버에서 일부러 만들지 않는다)
삭제 도중의 부분 실패는 서버에서 일부러 만들 수 없다. 일어나면 **화면의 안내를 먼저 읽고, 같은 서명을 다시 쓰지 말고, 아래 순서로 상태를 읽는다.** 화면은 자동으로 다시 하지 않는다.
- **`delete_partial`(504, 원격 삭제 결과 불확실):** `DELETE`가 나갔을 수 있다. **로컬 정리는 일어나지 않았다.** (1) **[사용자] GitHub(또는 GitLab) 화면에서 저장소가 있는지 본다.** (2) 저장소가 **없다** → 등록부는 아직 원격을 가리킨다: 웹의 "복제본과 등록 지우기…"(`remove_project`: 서명 필요, `legacy_ack` 불필요)로 정리한다. 다시 "원격 삭제"를 시도하면 `remote_changed`가 나온다(저장소가 없으므로). (3) 저장소가 **있다** → 웹의 "원격 저장소 다시 지우기…"(새 제안·새 서명)로 다시 한다. 화면은 이 프로젝트를 "원격이 이미 지워졌을 수 있다"로 취급해 유일한 사본 경고와 체크를 요구한다.
- **`delete_partial_local`(502):** 원격은 지워졌거나(원격 삭제의 `also_local`) 등록이 지워졌는데(`remove_project`) 로컬 정리가 덜 됨. 화면이 항목별 상태(`removed`·`already_gone`·`kept_in_place`·`moved_aside`)와 이유(`project_busy`·`busy_check_failed`·`in_progress`·`remove_failed`)를 보인다. **`kept_in_place`**: 손대지 않았다(원래 자리에 그대로). **`moved_aside`**: 폴더가 프로젝트 이름에서 `.deleting-<무작위>`로 옮겨졌고 삭제가 도중에 멈췄다 → **orchd를 재시작하면 시작 때 청소가 이어서 지운다**(`sudo systemctl restart orchd`: 작업이 없을 때). `.remove-refused-<hex>`는 바꿔치기가 의심되어 옮겨진 폴더이며 청소가 건드리지 않는다: 내용을 사람이 보고 판단한다.
- **GitLab의 "삭제 요청됨" 뒤 GitLab 화면에서 프로젝트를 복구한 경우:** 등록부는 여전히 원격이 지워진 상태(`remote_deleted_at`)이고 제공자가 남아 다시 연결할 수 없다. `remove_project`로 등록을 지운 뒤 "기존 저장소 연결"로 다시 연결한다.
- **등록이 지워졌는데 폴더가 남은 경우(등록 없는 폴더):** 그 폴더는 등록부에 없으므로 작업 제출이 여전히 받아들여지고, 게시는 그 프로젝트의 GitHub·GitLab이 아니라 orchd의 기본 `--git-remote`(`/home/orch/repos/remote.git`)로 간다. 다시 등록하거나 지울 때까지 그 폴더에 작업을 내지 않는다. 두 방법. (A) 그 경로를 웹의 기존 폴더 등록(`register_project`, 서명 필요)으로 다시 등록한 뒤 "복제본과 등록 지우기…"(`remove_project`)를 다시 한다(레거시였다면 `legacy_ack`를 다시 한다). (B) 사람이 지운다: `sudo ls -la /home/orch/repos/<이름>`로 **정말 그 폴더인지** 보고 `sudo -u orch rm -rf -- /home/orch/repos/<이름>`(키 폴더도 같은 방식으로 `…/ssh/keys/<이름>`). **되돌릴 수 없다.** 지우기 전에 원격에 올라가지 않은 커밋이 없는지(`sudo -u orch git -C <폴더> log --branches --not --remotes --oneline`) 본다.
- **`remote_key_may_remain`:** 연결한 저장소를 정리할 때는 로컬 정리 전에 "배포 키만 떼기"를 먼저 하라는 안내가 보인다. 키를 떼지 않고 등록을 지우면 GitHub 저장소에 `orchd <이름>` 키가 남는다: GitHub에서 사람이 지운다.
- **⚠ 함정(W3f 컷오버에서 실제로 일어남: 29번 9장의 `sulee-portfolio`): 키를 남기지 않으려면 키를 먼저 뗀다.** 원격 저장소를 지우지 않는 정리(연결한 저장소, 출처를 모르는 저장소)에서는 **"배포 키만 떼기"(`remove_deploy_key`)를 먼저 서명하고, 그다음 "복제본과 등록 지우기"**를 한다. 순서를 바꾸면 등록과 키 폴더가 먼저 사라져 이 도구로는 그 키를 더 뗄 수 없다(공개 키 줄도 프로젝트도 없다): 제공자 화면에서 사람이 지워야 한다. 원격 저장소 삭제는 이 도구가 만든 저장소에만 되므로 "원격을 지우면 키도 사라진다"에 기대지 않는다. **W3g(17장)부터는 "복제본과 등록 지우기"의 "배포 키도 함께 떼기"(기본 체크)로 키 떼기 → 등록 → 폴더가 서명 한 번에 이 순서로 된다.**

### 단계 4. 롤백 [사용자][주의]
**순서: orchd를 먼저 멈춘다 → A(broker) → B(orchd).** **삭제가 일어난 뒤의 롤백은 지운 것을 되살리지 못한다(⚠ 2): 먼저 `projects` 읽기·`/root/backup-w3f/tree-before.txt`와 지금 상태를 비교해 무엇이 지워졌는지 적어 둔다.**
**전제:** `orch list`와 `orch approvals`에 실행 중·대기·게시 대기·승인 대기 작업이 없다.

**A. broker를 W3e 상태로 되돌리기(DB는 건드리지 않는다). 바이너리와 등록부를 함께 되돌린다.**
1. `sudo systemctl stop orchd`, 이어서 `sudo systemctl stop orch-broker`
2. 바이너리를 **root 전용 백업에서** 복원: `sudo install -o root -g root -m 0755 /root/backup-w3f/bin/orch-broker /usr/local/bin/orch-broker`. `broker.json`은 이번에 바뀌지 않았다.
3. **등록부를 버전 3 사본으로 되돌린다**(W3e broker는 버전 4를 깨짐으로 본다). **먼저 어느 사본을 쓸지 확인한다:** `sudo sha256sum /root/backup-w3f/var-lib-orch-broker/registry.json /var/lib/orch-broker/registry.json.v3.bak` — **두 값이 같고 `sudo grep -o '"version": *[0-9]*' /root/backup-w3f/var-lib-orch-broker/registry.json`이 `"version":3`이면** root 백업을 쓴다. **`.v3.bak`는 broker가 한 번만 쓰고 덮어쓰지 않으므로 이전 시도의 낡은 파일일 수 있다: 두 값이 다르면 `.v3.bak`를 믿지 않고 root 백업만 쓴다(root 백업이 없고 `.v3.bak`만 있을 때는 그 안의 `"version":3`과 프로젝트 목록(`toy`·`sample-app`…)이 지금 기대하는 것인지 사람이 보고 정한다).** `sudo install -o orch-broker -g orch-broker -m 0600 /root/backup-w3f/var-lib-orch-broker/registry.json /var/lib/orch-broker/registry.json`, 이어서 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 `"version":3`인지. **상태 폴더 전체를 되돌리지 않는다**(예산 기록·쓴 요청 ID·웹에서 등록한 토큰이 되돌아간다).
4. **다시 컷오버할 때를 위해, 이 롤백으로 남은 낡은 `.v3.bak`을 치운다**: `sudo mv /var/lib/orch-broker/registry.json.v3.bak /root/backup-w3f/registry.json.v3.bak.rolledback-$(date +%Y%m%d-%H%M%S)`(그대로 두면 다음 마이그레이션이 "이미 있는 백업"을 덮어쓰지 않고 시작을 거부할 수 있다).
5. `sudo systemctl start orch-broker`; 15장 1-6의 `providers`·`projects`·`status`·`tokens`가 W3e 상태(`origin` 필드 없음)를 주는지 본다.
6. **경고:** 복원한 등록부는 컷오버 전 상태이므로 **컷오버 뒤 이 도구가 지운 프로젝트의 행이 되살아난다**(폴더·키·원격은 없다). 3-4의 시험 저장소 행이 `remote`를 가진 채 되살아나지만 GitHub의 저장소는 이미 지워졌다. 이 행들을 지우는 기능은 W3e 이하에 없다: **사람이 위 `tree-before.txt`와 비교해 어떤 행이 죽었는지 적고 W3f를 다시 올려 `remove_project`로 정리하거나(권장) 등록부를 사람이 고친다(서명 없는 파일 편집: 위험, 하지 않는 것을 권한다).** `secrets/`·`tokens.json`은 그대로 둔다. **또한 컷오버 뒤 만들거나 연결한 프로젝트는 v3 등록부에 없으므로 그 폴더는 등록 없는 폴더가 된다: 작업 제출이 받아들여지고 게시는 그 프로젝트의 원격이 아니라 orchd의 기본 `--git-remote`(`/home/orch/repos/remote.git`)로 간다. 그 폴더에 작업을 내지 않는다.**

**B. `orchd`·CLI를 W3e 상태로 되돌리기(DB 복원 불필요).**
1. (orchd는 A-1에서 멈춰 있다)
2. `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3f/bin/$f /usr/local/lib/aigate/$f; done`(세 개는 항상 같이. **웹은 `orchd`에 들어 있으므로** 삭제 화면도 함께 사라진다)
3. `sudo systemctl start orchd`. 브라우저 캐시면 강력 새로고침.

**C. 지운 것은 되살릴 수 없다.** GitHub의 저장소(되살릴 수 없음), GitLab의 저장소(인스턴스 설정에 따라 삭제 예약 상태에서 GitLab 화면의 복구가 가능할 수 있으나 이 도구 밖의 일이고 확인하지 못했다), 서버의 복제본(원격이 남아 있으면 같은 원격을 "기존 저장소 연결"로 다시 연결해 새로 복제할 수 있다: 새 키가 생긴다), 키 폴더(개인 키는 되살릴 수 없다: 같은 방식으로 새 키). `.deleting-*`로 옮겨진 폴더는 orchd 재시작이 지운다.

**검증:** `journalctl -u orchd`·`journalctl -u orch-broker`에 오류가 없고, `orch list`가 단계 2 이전과 같으며, 15장의 화면(GitHub 제공자, 새 프로젝트, 연결)이 동작한다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`, `ls -l /usr/local/bin /usr/local/lib/aigate /var/lib/orch-broker`와 `sudo ls -lR /root/backup-w3f | head -50`을 사용자에게 보고한다. W3e 이전까지 가야 하면 15장 단계 4(`/root/backup-w3e`).

### 정적 `--git-remote-for` 정리 (`sample-app`를 지운 뒤에만, 사용자 결정)
`sample-app`처럼 orchd 유닛에 정적 `--git-remote-for <id>=<주소>`가 있는 프로젝트를 지워도 그 설정은 남는다. 없는 폴더를 가리키는 정적 원격은 해롭지 않지만 지저분하다. 정리는 **사람이** 한다: `systemctl cat orchd | grep -e git-remote-for`로 줄을 찾고 `/root/backup-w3f/orchd.service.pre-w3f`와 비교해 백업한 뒤 유닛(또는 드롭인)에서 그 인자를 지우고 `sudo systemctl daemon-reload && sudo systemctl restart orchd`(작업이 없을 때). 이 컷오버에서는 `sample-app`를 지우지 않으므로 하지 않는다.

### 관찰 기록 (컷오버 때 채운다: 결과 문서 29번 9장으로 옮긴다)
| 항목 | 예상 | 본 것 | 판정 |
|---|---|---|---|
| 1-5 등록부 | `"version":4`, 600, `.v3.bak` 600 + sha256이 root 백업과 일치 | | |
| 1-6 `projects` 비교 | 같은 프로젝트·값, `toy`·`sample-app` `legacy`, 나머지 `unknown` | | |
| 3-2 로컬 정리(실제 서명, 실제 폴더 삭제) | 폴더·키·등록 지워짐, 원격·배포 키 그대로 | | |
| 3-2 `.deleting-*` 남음 | 없음 | | |
| 3-3 키 떼기 | 그 키만 사라짐, 다른 키·저장소 그대로 | | |
| 3-4 원격 삭제(토큰 권한, 상태 코드) | 저장소 404, `remote_deleted_at`, `Provider` 유지 | | |
| 3-4 이름 입력 게이트·IME/붙여넣기 | 틀리면 단추 꺼짐, 포커스 유지 | | |
| 3-4 후속 등록 지우기 | 자동 시작 없음, 별도 서명 | | |
| 3-6 레거시 두 번째 화면 | 서명 단추 없이 서명으로 못 감, 취소 후 변화 없음 | | |
| 3-8 원장·로그·개인 키 위치 | 키 5개뿐·0건·예상 위치 | | |
| 미리보기 시간(가장 큰 복제본) | 25초 안 | | |

## 17. W3g 컷오버 런북 (삭제를 서명 한 번으로: 배포 키 함께 떼기, 원격 저장소와 복제본·등록 함께 지우기, `orch-broker`·`orchd` 갱신)
계획은 `30-PLAN-webconsole-W3g.md`, 결과·시험·한계는 `31-RESULTS-webconsole-W3g.md`(**배포 현실 표**와 "실서버에서 확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더)을 읽고 쓴 것이지 서버나 실제 GitHub·GitLab에서 본 출력이 아니다(11~16장과 같은 원칙). 표기·경로·비밀 취급은 16장과 같다(**[사용자]**, **[컨트롤러]** 읽기 전용, **[되돌림 가능]**, **[주의]**; 리다이렉트는 `sudo sh -c '…'`; **토큰·개인 키 값은 어떤 명령에도 넣지 않고 출력하지 않으며 채팅·터미널에 붙이지 않는다**).

**바뀌는 것.** broker의 `remove_project`가 서명 필드 `detach_key`를 받고(켜면 키 목록 → 키 `DELETE` → 성공일 때만 등록 삭제), `delete_remote_repo`에 `also_local=true`가 서명되면 원격 `DELETE` 뒤 **등록 행까지 지운다**. orchd는 키 줄을 키 폴더(GitHub)·공용 `.pub`(GitLab)에서 읽어 broker에 준다. 웹의 "복제본과 등록 지우기"에 체크 상자 "배포 키도 함께 떼기"(기본 체크)가 생기고 `also_local` 결과에서 "등록 지우기" 후속 서명이 사라진다. broker `remove_project` 시간 제한 10초 → 60초, orchd client 15초 → 70초. **바뀌지 않는 것: 등록부 버전(v4 그대로, 마이그레이션 없음), orchd DB 스키마(v8), 유닛·플래그·`broker.json`·방화벽·정책·`known_hosts_gitlab`, 허용 목록(새 DELETE 행 없음).** **W3f의 `remove_project` 변경 해시가 달라진다**(W3f 화면에서 만든 서명은 W3g broker에서 쓸 수 없다: 갱신 전에 보류 중인 서명이 없게 한다).

> **⚠ 먼저 읽을 것.**
> 1. **갱신 순서는 W3f와 같다: orchd 정지 → broker → orchd.** 섞이면(아래 "혼합 버전") 둘 다 안전한 쪽으로 실패하지만, W3g orchd + W3f broker의 `also_local`은 **원격이 지워진 뒤 `outcome_unknown`**이 되고, W3f orchd + W3g broker의 `also_local`은 **등록이 지워졌는데 화면은 옛 후속 서명을 권한다.** 순서를 지켜 섞이는 시간을 0으로 만든다.
> 2. **등록부는 v4 그대로다.** 롤백에 등록부 복원이 필요 없다(W3f broker가 v4를 읽는다). 대신 **W3g로 지운 것(키·등록·폴더·원격)은 롤백해도 돌아오지 않는다.**
> 3. **원격 저장소 삭제는 되돌릴 수 없다.** 3-3은 이 컷오버에서 새로 만든 시험 저장소 하나에만 한다(사용자 결정).
> 4. **키 떼기는 저장소를 지우지 않는다**: 3-2의 시험 저장소는 키만 사라지고 남는다(그 뒤 사람이 GitHub에서 지우거나 둔다).
> 5. **GitLab은 키를 본문으로만 찾는다**(제목은 보지 않는다: 공용 키는 첫 프로젝트의 제목으로 남기 때문). 그래서 사전 점검 (c)로 실제 GitLab이 두 번째 프로젝트에 어떤 제목을 보이는지 먼저 본다. 확인하지 못하면 **GitLab 키 떼기 시험(3-4)은 하지 않는다**(권장 기본: GitHub만).
> 6. **결과를 알 수 없으면 아무것도 더 하지 않는다**(`delete_partial`·`outcome_unknown`·시간 초과): 같은 서명은 쓸 수 없고 자동 재시도도 없다. 아래 "복구"를 본다.

### 17.0 시작 전에 모두 참이어야 하는 것
- **출발점: W3f 컷오버가 끝난 상태(16장, 29번 9장).** 서버의 broker·orchd가 W3f(main `263d0e1`), 등록부 `"version":4`, 프로젝트 `toy`·`sample-app`(legacy), 제공자 `example-group-gitlab`·`swlee3306-github`(사용자·컨트롤러가 2026-11-13에 알려 준 사실: 아래 (a)로 다시 본다).
- **실행 중·대기·게시 대기·승인 대기 작업이 없고, W3f 화면에서 만든 보류 중인 삭제 서명이 없다**(`orch list`, `orch approvals`).
- **백업은 `/root/backup-w3g`(root 0700)에만**: `sudo install -d -m 0700 -o root -g root /root/backup-w3g /root/backup-w3g/bin`. 복사는 모두 `cp -an`.
- **사전 점검(단계 1 전, 전부 읽기 전용, 컨트롤러가 서버에서 하거나 사용자가 제공자 화면·자기 셸에서 한다). 하나라도 예상과 다르면 시작하지 않고 보고한다.**
  - **(a) W3f 상태.** `systemctl is-active orch-broker orchd` → `active` 둘. `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd /var/lib/orch-broker/registry.json`(기록: 롤백·1-5 비교에 쓴다). `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → **`"version":4`**. 16장 1-6의 `projects` 읽기로 프로젝트 목록을 `/root/backup-w3g/projects-before.json`에 저장한다(값 없음).
  - **(b) 키 폴더·공용 키(16장 (c)와 같은 명령).** `sudo find /home/orch/orchd/ssh/keys -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n'`(지금은 비어 있을 것으로 예상: 29번 9장), `sudo stat -c '%U:%G %a %n' /home/orch/orchd/ssh/id_gitlab_deploy.pub`.
  - **(c) [사용자] GitLab 공용 키의 두 번째 프로젝트 제목(읽기 전용).** 공용 키 `orchd …`가 붙은 GitLab 프로젝트가 **둘 이상** 있으면, 처음 붙인 프로젝트가 아닌 쪽의 **Settings > Repository > Deploy keys**에서 그 키의 **제목**을 본다(또는 사용자 셸에서 이미 로그인된 CLI로 `GET /projects/:id/deploy_keys`: 토큰 값은 명령에 쓰지 않는다). 예상: 공용 키 `id_gitlab_deploy`는 사람이 `example-group/sample-collector`에 처음 등록했으므로(10장의 실제 GitLab push 기록, 14장 ⚠ 5) **그때 사람이 붙인 제목일 수 있고 `orchd <id>` 꼴이 아닐 수 있다.** 보이는 제목을 **기록만 한다: 다른 제목이어도 실패가 아니다**(GitLab은 본문으로만 찾으므로 제목은 떼기에 쓰이지 않는다: R-1). 두 번째 프로젝트가 없으면 이 점검은 할 수 없다: **3-4를 하지 않는다.**
  - **(d) [사용자] GitHub 키 본문 모양(읽기 전용).** 배포 키가 붙은 GitHub 저장소(예: 3-2에서 만든 직후의 저장소, 또는 기존 저장소)에서 `gh api repos/<소유자>/<저장소>/keys --jq '.[] | [.id, .title, (.key | split(" ") | length)] | @tsv'`(gh는 이미 로그인된 사용자 계정을 쓴다: 토큰을 명령에 쓰지 않는다). 예상: `orchd <id>` 키의 `key`가 **두 조각**(`type base64`, 주석 없음). 서버 쪽은 `sudo -u orch ssh-keygen -lf /home/orch/orchd/ssh/keys/<id>/id_ed25519.pub`의 지문이 GitHub 화면(Settings > Deploy keys)의 지문과 같아야 한다. 다르면 떼기는 "닮은 키"로 거부된다(안전한 실패): 3-2를 하지 않고 보고. **키가 붙은 GitHub 저장소가 아직 없으면(키 폴더가 비어 있으면) 이 점검은 3-2 (a)에서 A를 만든 직후에 하고, 단계 1의 시작 조건이 아니다.**
  - **(e) [컨트롤러] 공용 `.pub` 주석에 공백이 없는지.** `sudo awk '{print NF}' /home/orch/orchd/ssh/id_gitlab_deploy.pub` → **`2` 또는 `3`**. `4` 이상(주석에 공백)이면 broker의 키 줄 규칙 밖이라 **GitLab 프로젝트의 키 떼기는 `key_unavailable_for_removal`로 서명 전에 거부된다**(시험 `TestSharedKeyLineWithASpacedCommentIsUnavailableBeforeSignatureW3g`). 그러면 GitLab 프로젝트는 떼기를 끄고 지우고 키는 GitLab 화면에서 사람이 뗀다(파일을 고치지 않는다: 다른 프로젝트의 게시가 그 키를 쓴다).
  - **(f) 아무것도 돌고 있지 않다.** `orch list`, `orch approvals`.
  - **키 `DELETE`의 실제 상태 코드와 지연은 읽기 전용으로 볼 수 없다**: 아래 목록의 1·4이고 3-2에서 처음 드러난다.

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 31번 6장에 "확인함/못 함"으로 남긴다)
1. **실제 GitHub 키 떼기**(`GET …/keys` → `DELETE …/keys/{id}`의 상태 204 가정) 뒤 등록·복제본·키 폴더 삭제가 한 서명에 되는지, 다른 키는 그대로인지.
2. **GitLab**: 공용 키의 두 번째 프로젝트 제목(17.0 (c)), 키 비활성 `DELETE`의 상태(204 가정), 다른 프로젝트에서 그 키가 계속 쓰이는지(3-4를 할 때만).
3. **GitHub 키 본문이 로컬 `.pub`의 앞 두 조각과 같은지**(17.0 (d)).
4. **지연**: 저장소 GET + 키 목록 + `DELETE`가 60초 안에(실제 소요를 broker 로그 시각으로 본다). 사슬은 broker 60초 < orchd client 70초 < orchd HTTP 서버 쓰기 제한 120초(`WriteTimeout`: apply의 실제 상한) < 웹 기다림 130초다. 앞단 프록시가 120초보다 짧게 자르지 않는지 본다.
5. **실제 패스키가 만든 `detach_key`·`also_local` 서명**을 broker가 검증하는지.
6. **`also_local=true`로 원격 삭제와 복제본·등록이 서명 한 번에 지워지는지**와 후속 서명 안내가 없는지(3-3).
7. **브라우저로 본 적이 없는 화면**: `broker_unavailable`(3-6에서 볼 수 있음), 결과를 알 수 없음(일부러 만들지 않는다).

### 단계 1. broker 갱신 [사용자][되돌림 가능: 단계 4]
**순서: (Mac) 게이트·빌드 → orchd 정지 → 백업 → 옛 broker 정지 → 새 바이너리 설치 → 샌드박스 안 `-check` → 시작 → 읽기 점검.** 등록부는 바뀌지 않는다.

**1-0. 빌드 전 게이트 (Mac, 저장소에서).** 16장 1-0과 같은 명령(`TestConstantsAgreeWithWeb` PASS·SKIP 없음, `gofmt -l` 출력 없음, vet 셋, `go test … -count=1 -race`, `node --test "internal/orch/web/test/*.test.js"`). 결과 문서 31번 2장의 숫자와 같은 커밋인지 본다.

**1-1. 새 바이너리 (Mac).** `GOOS=linux GOARCH=amd64 go build -o /tmp/w3g/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고 `scp`로 VM `/tmp/w3g/`에 보낸다. **아직 설치하지 않는다.**

**1-2. orchd 정지와 백업. [사용자]** 작업이 없는지 본 뒤 `sudo systemctl stop orchd`(이 시점부터 단계 2 끝까지 웹 콘솔이 멈춘다).
```
B=/root/backup-w3g
sudo install -d -m 0700 -o root -g root $B $B/bin
sudo cp -an /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -an /usr/local/lib/aigate/$f $B/bin/$f; done
sudo sha256sum $B/bin/* | sudo tee $B/bin.sha256 >/dev/null
sudo sh -c "find /home/orch/orchd/ssh/keys /home/orch/repos -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n' > $B/tree-before.txt"
```
(개인 키는 복사하지 않는다. W3g는 설정·유닛을 바꾸지 않으므로 그것들은 `/root/backup-w3f`의 사본으로 충분하다.)

**1-3. 옛 broker 정지 → 등록부 사본 → 새 바이너리 설치.**
```
sudo systemctl stop orch-broker
sudo install -o root -g root -m 0600 /var/lib/orch-broker/registry.json $B/registry.json.w3f     # 기록용 사본(v4). 롤백에는 필요 없다
sudo sha256sum $B/registry.json.w3f /var/lib/orch-broker/registry.json                             # 같아야 한다
sudo install -o root -g root -m 0755 /tmp/w3g/orch-broker /usr/local/bin/orch-broker
```
**1-4. 샌드박스 안 `-check`(시작 전).** 16장 1-4와 같은 `systemd-run … -check` 명령. 예상: 통과 한 줄, 종료 코드 0, `"version":4` 그대로. 실패하면 시작하지 않는다 → 단계 4.

**1-5. 시작.**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json
sudo sha256sum /var/lib/orch-broker/registry.json /root/backup-w3g/registry.json.w3f
```
예상: `active (running)`, 오류 없음, **`"version":4`이고 sha256이 1-3의 사본과 같다**(W3g broker는 시작할 때 v4 등록부를 다시 쓰지 않는다고 예상: 코드 기준, 서버 미확인. 다르면 멈추고 `sudo diff`가 아니라 두 파일의 `projects` 읽기 결과를 비교해 보고한다).

**1-6. 읽기 op.** 16장 1-6과 같은 명령. `providers`·`projects`·`status`·`tokens`가 17.0 (a)와 같다(`origin`·`remote` 포함). 다르면 단계 4.

### 단계 2. orchd 갱신 [사용자]
```
sudo systemctl is-active orchd          # inactive
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3g/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
sudo sha256sum /usr/local/lib/aigate/orchd /tmp/w3g/orchd
```
**검증:** 로그 오류 없음, `approvals: mode signed`, `orch list`의 마지막 번호 그대로, 해시 일치, 로그인 없이 `GET /api/v1/projects/toy/removal-preview`와 `POST /api/v1/projects/remove/propose`가 **401**. `sudo ls -a /home/orch/repos /home/orch/orchd/ssh/keys`에 `.deleting-*`·`.remove-refused-*` 없음. **그래도 안 되면:** 단계 4.

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과. 패스키로 로그인. "예상"은 `internal/orch/web/test/delete.test.js`, `internal/orch/api/w3g_g2_test.go`·`adversarial_w3g_test.go`, `internal/broker/delete_w3g_test.go`·`adversarial_w3g_test.go`에서 읽은 것이다. **새로 만든 시험 저장소로만 한다**(이름은 사용자 결정, 예: 키 떼기용 `w3g-smoke-a`, 원격 삭제용 `w3g-smoke-b`, `^[a-z][a-z0-9-]{1,39}$`, GitHub, 공개, 템플릿 `basic`).

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | **회귀.** `#/projects`, `#/providers` | 목록이 단계 1 전과 같다. `toy`·`sample-app`의 "삭제…" → "복제본과 등록 지우기…" 미리보기에는 **"배포 키도 함께 떼기"가 없다**(원격·제공자 없음) — 확인만 하고 취소 | 체크 상자가 보이면 멈추고 보고(G-D3) |
| 3-2 | **[사용자] 키 떼기 + 복제본·등록 정리(서명 한 번): 새 저장소 A.** (a) "새 프로젝트 만들기"로 A를 만든다(15장 3-6과 같다, 복제 `cloned`). 17.0 (d)를 A로 한다. (b) A의 "삭제…" → "복제본과 등록 지우기…" → 미리보기: **"배포 키도 함께 떼기"가 체크되어 있고** 저장소(경로·주소·GitHub), 키 제목 `orchd <A>`, 지문(GitHub 화면의 지문과 같아야 한다)이 보인다. (c) **떼기 끄기 확인(서명하지 않음):** 체크를 끄면 굵은 "[중요] 키가 원격에 남습니다. 직접 지우세요"가 나오고 대상 목록에서 키가 빠진다 → 다시 체크하면 경고가 사라진다. (d) 체크된 채로 확인 화면: "배포 키도 함께 떼기(detach_key): 예", 저장소·제목·지문, 서버 문장에 "먼저 원격 저장소 '…'에서 이 서버의 배포 키(지문 …)를 뗍니다…" → 서명 | 결과 "완료": 단계 "원격 저장소의 배포 키 — 뗐습니다", 등록 지움, 복제본·키 폴더 지움. **GitHub(사용자):** A는 **그대로 있고** Deploy keys에 `orchd <A>`가 **없다**, 다른 키는 그대로. **서버(컨트롤러):** `sudo ls -a /home/orch/repos /home/orch/orchd/ssh/keys`에 A가 없고 `.deleting-*`도 없다, `projects` 읽기에 A가 없다, 원장 `project_remove` `ok:true` 한 줄, broker 로그 `remove_project: removed project <A> from the registry`(시각으로 지연을 본다: 60초 안). 그 뒤 A는 사람이 GitHub에서 지우거나 둔다 | `key_unavailable_for_removal`(키 줄을 못 읽음: 서명 전, 체크를 끄고 진행할 수 있으나 그러면 키가 남는다), `remove_refused`(닮은 키·둘 이상·제공자 거부: **아무것도 지우지 않았다**: GitHub 화면을 보고 보고), `delete_partial`(아래 복구 (b)), `github_refused`(토큰 권한), 서명 거부(실제 패스키로 `detach_key` 서명은 처음: 멈추고 보고) |
| 3-3 | **[사용자 결정, 되돌릴 수 없음] 원격 삭제 + 복제본 + 등록(서명 한 번): 새 저장소 B.** B를 만든 뒤 "삭제…" → "원격 저장소까지 지우기…" → 저장소 이름 입력 → **"이어서 복제본도 지움"** 체크 → 유일한 사본 경고와 확인 체크 → 대상 목록에 "프로젝트 등록(B)"이 있고 남는 목록에 "원격 없음/별도 서명"이 없다 → 확인 화면 → 서명 | 결과: 원격 삭제 완료, 복제본·키 폴더 지움, **"프로젝트 등록 — 지웠습니다(같은 서명)"**, **후속 "복제본과 등록 지우기…" 단추가 없다.** GitHub: B가 **404**. 서버: 폴더·키 폴더 없음, `projects` 읽기에 B가 **없다**(W3f는 `remote_deleted_at` 행이 남았다), 원장 `remote_delete` `ok:true` 한 줄(로컬 실패 시 `ok:true` 뒤 `ok:false code:delete_partial_local`), `project_remove` 줄 없음, broker 로그 `delete_remote_repo: removed the remote repository and the registry row of project <B>` | `delete_partial`(아래 복구 (a)), `delete_partial_local`(복구 (d)), `remove_refused`(토큰 권한), `delete_rate_limited` |
| 3-4 | **[선택, 17.0 (c)를 확인했을 때만] GitLab 키 떼기.** 이 도구로 만든 GitLab 시험 프로젝트에서 3-2와 같은 흐름. 미리보기·확인 화면은 **지문만** 보이고 "공용 키" 안내가 있다(제목을 주장하지 않는다). 이 점검은 제목에 기대지 않는다: broker는 본문으로만 찾으므로 GitLab 화면의 제목이 무엇이든 그 프로젝트의 그 키 하나만 떼어야 한다 | GitLab: 그 프로젝트의 Deploy keys에서 키가 사라지고 **같은 키를 쓰는 다른 프로젝트(`example-group/sample-collector`)에서는 그대로**, `sample-app` 게시가 계속 된다 | 다른 프로젝트에서 키가 사라졌으면 **즉시 멈추고 보고**(게시가 막힌다). **복구:** GitLab 화면에서 `sample-collector`의 Settings > Repository > Deploy keys에 서버의 공용 `.pub`(`/home/orch/orchd/ssh/id_gitlab_deploy.pub`, 공개 키)를 **쓰기 권한(Grant write permissions)으로** 다시 붙이고, `sample-app` 작업 하나로 게시(push)가 되는지 확인한다 |
| 3-5 | **[컨트롤러] 누출 점검.** 16장 3-8 점검 명령과 표 아래의 한 줄 | 원장 키 `action,actor,code,ok,subject`뿐, 경로·원격 주소 0건, 로그의 토큰·`PRIVATE KEY` 0건, **로그에 키 본문(`AAAAC3Nza…`) 0건** | 보이면 멈추고 보고 |
| 3-6 | **[선택] broker를 멈춘 상태의 화면.** `sudo systemctl stop orch-broker` → 남은 프로젝트의 "삭제…" 미리보기 | 고정 오류(`broker_unavailable`), 아무것도 지우지 않음(이 화면은 브라우저로 본 적이 없다: 결과 문서 31번 6장). 끝나면 `sudo systemctl start orch-broker`(**한도 카운터가 0으로 돌아간다**) | 기존 기능까지 막히면 단계 4 |

**3-5 추가 명령**(표 안에 `|`를 쓰지 않으려고 표 밖에 둔다. `grep -c`가 0이면 종료 코드 1: 정상):
```
sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager | grep -c AAAAC3Nza
```

**검증 체크리스트(요약):** 3-1(원격 없는 행에 선택지 없음), 3-2(키만 사라지고 저장소·다른 키는 그대로, 등록·폴더 지움, 떼기 끄기 경고), 3-3(원격·복제본·등록이 서명 한 번, 후속 없음), 3-5. 결과는 결과 문서 31번 9장에 남긴다.

### 혼합 버전 (순서를 지키면 생기지 않는다)
| 조합 | `remove_project` | `delete_remote_repo` + `also_local=true` |
|---|---|---|
| **W3f orchd + W3g broker** | W3f orchd는 `detach_key`를 보내지 않는다 → broker가 모양 단계에서 `bad_request`(서명 전, ID 미소모): 아무것도 지우지 않음 | **broker가 원격을 지우고 등록 행까지 지운다**, W3f orchd는 W3f 답처럼 받아 복제본·키 폴더를 지우고 화면은 **"등록은 남음" + 후속 "등록 지우기"**를 권한다(이미 없다: 후속 서명은 `project_unknown`) |
| **W3g orchd + W3f broker** | broker가 모르는 인자 `detach_key`/`deploy_key` → `bad_request` → 화면 `broker_unavailable`("버전이 맞지 않거나…, 아무것도 지우지 않았습니다"). **한도 칸은 쓰인다** | 서명 필드가 같으므로 W3f broker가 **원격을 지우고 행은 `remote: 없음`으로 남긴다**(답에 `registry_removed` 없음) → orchd는 **`outcome_unknown`**, **로컬 파일은 건드리지 않는다**, 한도 칸은 쓰인다. 복구: 갱신을 마친 뒤 "복제본과 등록 지우기"(떼기 꺼짐: 원격이 없다) |

### 복구 (서버에서 일부러 만들지 않는다)
화면의 안내를 먼저 읽고, 같은 서명을 다시 쓰지 말고, 상태를 읽는다(`projects` 읽기, 제공자 화면, `sudo ls -a /home/orch/repos /home/orch/orchd/ssh/keys`).
- **(a) `also_local`을 고른 원격 삭제가 `delete_partial`(504).** 원격 `DELETE`가 나갔을 수 있고 **등록 행은 원래대로(원격이 그대로 적힌 채) 남았고 로컬은 손대지 않았다.** [사용자] 제공자 화면에서 저장소가 있는지 본다. **없다** → 웹의 "복제본과 등록 지우기…"를 **"배포 키도 함께 떼기"를 끈 채로** 서명한다(같은 방문이면 화면이 이미 꺼 둔다: 판정 R-2. 켜고 서명하면 broker가 저장소를 찾지 못해 `remote_changed`로 거부하고 아무것도 지우지 않는다: 서명만 쓴다). **있다** → "원격 저장소 다시 지우기…"(새 서명).
- **(b) 키 떼기의 `delete_partial`(504) 또는 떼기를 켠 `outcome_unknown`.** 키가 떼어졌을 수 있고 **등록과 폴더는 그대로다.** 제공자 화면(Deploy keys)을 본 뒤 "복제본과 등록 지우기"를 새로 서명한다: 떼기를 켜 둬도 된다(broker가 목록을 먼저 읽어 키가 이미 없으면 "이미 없음"으로 등록을 지운다). 키가 남아 있는데 계속 실패하면 떼기를 끄고 지운 뒤 키는 제공자 화면에서 사람이 지운다.
- **(c) broker 시간 초과(`timeout`·`outcome_unknown`) 뒤 등록이 사라진 경우.** `projects` 읽기에 그 프로젝트가 없는데 폴더가 남았으면 broker는 끝까지 했고 orchd만 답을 못 받은 것이다(키는 떼어졌거나 원격이 지워졌다). 남은 폴더는 **등록 없는 폴더**다: 아래 (e). 키가 남아 있는지는 제공자 화면에서 본다(다시 등록한 행에는 제공자·원격이 없으므로 이 도구로는 키를 뗄 수 없다: 사람이 지운다).
- **(d) `delete_partial_local`(502) — 원격 삭제(`also_local`) 또는 키 떼기 뒤 로컬 정리가 덜 됨.** 원격·키는 끝났고 **등록도 이미 지워졌다**(상세의 `registry_removed: true`, 떼기면 `key_state`·`key_detached: true`). `moved_aside`는 orchd 재시작이 이어서 지운다(작업이 없을 때 `sudo systemctl restart orchd`), `kept_in_place`는 아래 (e).
- **(e) 등록 없는 폴더.** 16장 "등록이 지워졌는데 폴더가 남은 경우"와 같다: (A) 그 경로를 기존 폴더 등록(`register_project`, 서명)으로 다시 등록한 뒤 **"배포 키도 함께 떼기"가 없는 상태로**(다시 등록한 행에는 원격이 없다) "복제본과 등록 지우기", 또는 (B) 사람이 `sudo -u orch rm -rf -- /home/orch/repos/<이름>`(먼저 `ls -la`로 그 폴더인지, `git log --branches --not --remotes`로 올라가지 않은 커밋이 없는지 본다; 키 폴더도 같은 방식). **되돌릴 수 없다.** 다시 등록하거나 지울 때까지 그 폴더에 작업을 내지 않는다(게시가 기본 `--git-remote`로 간다).

### 단계 4. 롤백 [사용자][주의]
**순서: orchd 먼저 멈춤 → A(broker) → B(orchd).** **W3g로 지운 것은 돌아오지 않는다**: 먼저 `projects` 읽기와 `/root/backup-w3g/tree-before.txt`를 지금과 비교해 무엇이 지워졌는지 적는다.
**A. broker.** `sudo systemctl stop orchd; sudo systemctl stop orch-broker` → `sudo install -o root -g root -m 0755 /root/backup-w3g/bin/orch-broker /usr/local/bin/orch-broker` → `sudo systemctl start orch-broker` → 16장 1-6의 읽기. **등록부는 되돌리지 않는다**(v4 그대로, W3f broker가 읽는다: `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → `"version":4`). W3g가 지운 행은 지워진 채로 남는다(그것이 맞는 상태다: 키·폴더·원격도 없다).
**B. orchd·CLI.** `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3g/bin/$f /usr/local/lib/aigate/$f; done` → `sudo systemctl start orchd`. 브라우저는 강력 새로고침(웹은 orchd 안에 있다).
**주의:** W3f와 W3g의 `remove_project` **변경 해시가 다르다**: 롤백 전에 W3g 화면에서 만든 제안·서명은 W3f broker에서 쓸 수 없고(모르는 인자로 `bad_request`), 롤백 뒤에는 W3f 화면에서 새로 제안·서명한다. 롤백 뒤에는 키 떼기가 다시 "배포 키만 떼기"를 먼저 하는 두 단계다(16장의 함정 안내).
**검증:** `journalctl -u orch-broker -u orchd`에 오류 없음, `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd`가 17.0 (a)의 값과 같음, `orch list`가 그대로. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`과 `sudo ls -lR /root/backup-w3g | head -50`을 보고한다.

### 관찰 기록 (컷오버 때 채운다: 결과 문서 31번 9장으로 옮긴다)
| 항목 | 예상 | 본 것 | 판정 |
|---|---|---|---|
| 17.0 (c) GitLab 두 번째 프로젝트의 키 제목 | 기록만(첫 등록 때의 제목일 수 있음, 다른 제목도 실패 아님) | | |
| 17.0 (d) GitHub 키 본문 조각 수·지문 | 2조각, 지문 일치 | | |
| 17.0 (e) 공용 `.pub` 필드 수 | 2 또는 3 | | |
| 1-5 등록부 | `"version":4`, sha256 불변 | | |
| 3-1 원격 없는 행 | 선택지 없음 | | |
| 3-2 키 떼기(실제 `DELETE` 상태, 지연) | 키만 사라짐, 등록·폴더 지움, 60초 안 | | |
| 3-2 떼기 끄기 경고 | 굵은 경고, 키가 대상에서 빠짐 | | |
| 3-3 원격+복제본+등록 한 서명 | B 404, 행 없음, 후속 없음 | | |
| 3-5 원장·로그 | 키 5개뿐·0건 | | |

## 18. W3h 컷오버 런북 (배포 키 떼기를 등록 때 서명된 키 해시에 묶기: 등록부 v5, 403 거부 진단 로그, `orch-broker`·`orchd` 갱신)
계획은 `32-PLAN-webconsole-W3h.md`, 결과·시험·한계는 `33-RESULTS-webconsole-W3h.md`(**배포 현실 표**와 "실서버에서 확인하지 못한 것"은 거기 4·6장). **이 절차는 아직 실제 서버에서 한 번도 실행하지 않았다.** 아래 "예상"은 코드와 이 저장소의 시험(가짜 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더)을 읽고 쓴 것이지 서버나 실제 GitHub·GitLab에서 본 출력이 아니다(11~17장과 같은 원칙). 표기·경로·비밀 취급은 17장과 같다(**[사용자]**, **[컨트롤러]** 읽기 전용, **[되돌림 가능]**, **[주의]**; 리다이렉트는 `sudo sh -c '…'`; **토큰·개인 키 값은 어떤 명령에도 넣지 않고 출력하지 않으며 채팅·터미널에 붙이지 않는다**. 키 줄과 그 sha256은 공개 정보지만 로그·채팅에는 sha256 앞 8자리까지만 옮긴다).

**바뀌는 것.** broker가 프로젝트를 만들거나(`create_project`) 연결할 때(`link_project`, `attach_remote`) 이미 서명·검증된 `deploy_key_sha256`을 등록 행의 `remote.key_sha256`으로 저장한다(**등록부 v4 → v5**, 시작할 때 한 번 마이그레이션, `registry.json.v4.bak`을 한 번만 쓴다). 키 떼기(`remove_project`의 `detach_key=true`, `remove_deploy_key`)는 orchd가 보낸 키 줄의 sha256이 그 값과 같아야 하고(서명 검증·요청 ID 소모 전에 확인), **해시가 없는 GitLab 행은 떼기가 거부된다**(`deploy_key_mismatch`, 409). GitHub 행은 해시가 없으면 W3g 규칙(제목+본문) 그대로다. orchd는 같은 규칙을 **서명 전에** 먼저 보고(미리보기의 `detach_key_available`, propose·apply의 409), broker의 `deploy_key_mismatch`를 "결과 알 수 없음"이 아닌 확정 거부(409, 변경 없음)로 보인다. orchd는 HTTP 403으로 거부할 때마다 고정 모양 한 줄 `orchd: refused 403 <코드> <메서드> <경로>`를 로그에 남긴다(쿼리·헤더·본문·서명·토큰 없음). 웹은 떼기 거부 문장(`detachRefused`)의 원인 목록에 "이 서버의 배포 키가 등록할 때의 키와 다름"이 하나 늘었다. **바뀌지 않는 것: 서명 필드·변경 해시(W3g 화면에서 만든 서명도 W3h broker에서 그대로 검증된다), `REGISTRY_MESSAGE_RE`·웹 벡터, orchd DB 스키마(v8), 유닛·플래그·`broker.json`·방화벽·정책·`known_hosts_gitlab`, 허용 목록(새 호출 행 없음), 오류 코드 목록(`deploy_key_mismatch`는 기존 코드), 원장 모양.**

> **⚠ 먼저 읽을 것.**
> 1. **갱신 순서는 W3g와 같다: orchd 정지 → 백업 → broker → orchd.**
> 2. **등록부가 v5로 올라간다. W3g broker는 v5를 읽지 못한다**(`-check`가 "프로젝트 등록부를 신뢰할 수 없습니다"로 거부하고, 그대로 시작하면 등록부를 쓰는 모든 op가 `registry_unavailable`). 그래서 롤백(단계 4)에는 바이너리와 함께 **등록부 편집**이 필요하다. **`registry.json.v4.bak`을 그대로 되살리는 것은 권하지 않는다**: 전환 뒤의 등록부 변경 **전부**(만들거나 연결한 행, 지운 행)가 함께 되돌아간다(단계 4).
> 3. **전환 전에 만든 GitLab 행은 이 도구로 배포 키를 뗄 수 없게 된다**(의도한 닫힌 실패: 해시가 없다). 그런 프로젝트는 "배포 키도 함께 떼기"가 보이지 않고, 지울 때는 떼기 없이 지운 뒤 GitLab 화면에서 키를 사람이 뗀다. 지금 서버에는 그런 행이 없다고 예상한다(`toy`·`sample-app`는 제공자 없는 legacy 행: 31번 9장).
> 4. **해시는 `.pub` 줄 전체(주석 포함)의 sha256이다.** GitLab 공용 `.pub`(또는 GitHub 프로젝트의 키 폴더 `.pub`)을 바꾸면, **주석만 바꿔도**, 그 전에 만든 행의 떼기가 거부된다(아래 "키 교체"). 컷오버 중에는 키 파일을 건드리지 않는다.
> 5. **이 컷오버 자체는 아무것도 지우지 않는다.** 시험(단계 3)은 새로 만든 시험 저장소로만 한다(사용자 결정).

### 18.0 시작 전에 모두 참이어야 하는 것
- **출발점: W3g 컷오버가 끝난 상태(17장, 31번 9장).** broker·orchd가 W3g(main `6bc81b4`), 등록부 `"version":4`, 프로젝트 `toy`·`sample-app`(legacy), 제공자 `example-group-gitlab`·`swlee3306-github`(31번 9장의 기록: 아래 (a)로 다시 본다).
- **실행 중·대기·게시 대기·승인 대기 작업이 없고, 보류 중인 삭제 서명이 없다**(`orch list`, `orch approvals`). 서명 정의는 바뀌지 않지만, 갱신 중 orchd가 멈추므로 열린 화면의 제안은 처음부터 다시 한다.
- **백업은 `/root/backup-w3h`(root 0700)에만**: `sudo install -d -m 0700 -o root -g root /root/backup-w3h /root/backup-w3h/bin`. 복사는 모두 `cp -an`(덮어쓰지 않음).
- **사전 점검(단계 1 전, 전부 읽기 전용, [컨트롤러]가 서버에서). 하나라도 예상과 다르면 시작하지 않고 보고한다.**
  - **(a) W3g 상태.** `systemctl is-active orch-broker orchd` → `active` 둘. `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd /var/lib/orch-broker/registry.json`(기록: 1-5·롤백 비교에 쓴다). `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json` → **`"version":4`**. 16장 1-6의 `projects` 읽기(아래 1-6과 같은 명령)로 프로젝트 목록을 `/root/backup-w3h/projects-before.json`에 저장한다(`sudo tee`, 값 없음). 예상: `toy`·`sample-app`만.
  - **(b) 낡은 `.v4.bak`이 없다.** `sudo ls -la /var/lib/orch-broker/` → `registry.json.v2.bak`·`registry.json.v3.bak`은 있을 수 있고 **`registry.json.v4.bak`은 없어야 한다**(W3g까지는 그 이름을 쓰지 않는다). 있으면(이전 시도·롤백의 잔재) 단계 1 전에 옮긴다: `sudo mv /var/lib/orch-broker/registry.json.v4.bak /root/backup-w3h/registry.json.v4.bak.stale-$(date +%Y%m%d-%H%M%S)`. **그대로 두면 broker는 그 파일을 덮어쓰지 않고 새 백업을 쓰지 않는다**(1-5의 sha256 비교가 어긋난다).
  - **(c) 키 파일 기준값(키 교체를 알아보는 데 쓴다).** `sudo sha256sum /home/orch/orchd/ssh/id_gitlab_deploy.pub`(기록), `sudo awk '{print NF}' /home/orch/orchd/ssh/id_gitlab_deploy.pub` → **`2` 또는 `3`**(17.0 (e)와 같다). `sudo find /home/orch/orchd/ssh/keys -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n'`(지금은 비어 있을 것으로 예상).
  - **(d) 아무것도 돌고 있지 않다.** `orch list`, `orch approvals`.

### 컷오버에서만 확인할 수 있는 것 (사전 목록: 결과 문서 33번 6장에 "확인함/못 함"으로 남긴다)
1. **실제 등록부의 v4 → v5 마이그레이션**: 행이 바뀌지 않고 버전만 5가 되는지, `registry.json.v4.bak`이 한 번 생기고 그 sha256이 전환 직전 사본과 같은지.
2. **실제 GitHub(그리고 고르면 GitLab)에서 새로 만든 행이 해시를 갖고, 그 해시로 키 떼기가 되는지**(17장 3-2와 같은 흐름).
3. **403 진단 로그**가 운영 리스너(HTTPS 앞단 뒤)에서 한 줄만 남고 쿼리·헤더가 없는지.
4. **GitLab 키 떼기**(17장 3-4: W3g 컷오버에서도 하지 않았다)는 여전히 실서버에서 해 본 적이 없다.

### 단계 1. broker 갱신·등록부 마이그레이션 [사용자][주의: 등록부가 버전 5로 올라간다 → 롤백은 단계 4]
**순서: (Mac) 게이트·빌드 → orchd 정지 → 백업 → 옛 broker 정지 → 등록부 사본 → 새 바이너리 설치 → 샌드박스 안 `-check` → 시작 → 읽기 점검.** `broker.json`은 바뀌지 않는다.

**1-0. 빌드 전 게이트 (Mac, 저장소에서).** 16장 1-0과 같은 명령(`TestConstantsAgreeWithWeb` PASS·SKIP 없음, `gofmt -l` 출력 없음, vet 셋, `go test ./internal/... ./cmd/... -count=1 -race -timeout 30m`, `node --test "internal/orch/web/test/*.test.js"`). 결과 문서 33번 2장의 숫자와 같은 커밋인지 본다.

**1-1. 새 바이너리 (Mac).** `GOOS=linux GOARCH=amd64 go build -o /tmp/w3h/<이름> ./cmd/<이름>`을 `orch-broker`, `orchd`, `orch`, `aigate`에 대해 하고 `scp`로 VM `/tmp/w3h/`에 보낸다. **아직 설치하지 않는다.**

**1-2. orchd 정지와 백업. [사용자]** 작업이 없는지 본 뒤 `sudo systemctl stop orchd`(이 시점부터 단계 2 끝까지 웹 콘솔이 멈춘다).
```
B=/root/backup-w3h
sudo install -d -m 0700 -o root -g root $B $B/bin
sudo stat -c '%U:%G %a %n' $B          # root:root 700
sudo cp -an /usr/local/bin/orch-broker $B/bin/orch-broker
for f in orchd orch aigate; do sudo cp -an /usr/local/lib/aigate/$f $B/bin/$f; done
sudo sh -c "sha256sum $B/bin/* > $B/bin.sha256"
sudo sh -c "find /home/orch/orchd/ssh/keys /home/orch/repos -mindepth 1 -maxdepth 2 -printf '%y %u:%g %m %p\n' > $B/tree-before.txt"
```
(개인 키·토큰은 복사하지 않는다. W3h는 설정·유닛을 바꾸지 않으므로 그것들은 `/root/backup-w3f`의 사본으로 충분하다.)

**1-3. 옛 broker 정지 → 등록부 사본 → 새 바이너리 설치.** **옛 broker를 반드시 먼저 멈춘다**(사본이 실행 중인 broker의 쓰기와 겹치지 않게).
```
sudo systemctl stop orch-broker
sudo cp -an /var/lib/orch-broker/registry.json $B/registry.json.w3g     # v4 사본: 1-5의 .v4.bak 비교와 롤백 판단에 쓴다
sudo grep -o '"version": *[0-9]*' $B/registry.json.w3g                  # "version":4
sudo sha256sum $B/registry.json.w3g /var/lib/orch-broker/registry.json   # 같아야 한다(기록)
sudo install -o root -g root -m 0755 /tmp/w3h/orch-broker /usr/local/bin/orch-broker
```
(정지한 뒤 1-4가 실패하면 broker는 멈춘 채로 남는다: 곧바로 단계 4. 등록부는 아직 v4이므로 단계 4의 A-1·A-2만 하면 된다.)

**1-4. 샌드박스 안 `-check`(시작 전).** 16장 1-4와 같은 명령:
```
sudo systemd-run --wait --pipe -p User=orch-broker -p Group=orch-broker -p SupplementaryGroups=orch -p ProtectSystem=strict -p ProtectHome=yes -p SystemCallFilter=@system-service /usr/local/bin/orch-broker -check -config /etc/orch-broker/broker.json -state-dir /var/lib/orch-broker
```
예상: `orch-broker: 점검 통과 (…)` 한 줄과 `Finished with result: success`(종료 코드 0). **`-check`는 등록부를 쓰지 않는다**: 끝난 뒤에도 `sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json`이 **`"version":4`**이고 `registry.json.v4.bak`이 없다. 실패하면 시작하지 않는다 → 단계 4.

**1-5. 시작(등록부가 여기서 버전 5로 올라간다).**
```
sudo systemctl start orch-broker
systemctl status orch-broker --no-pager; sudo journalctl -u orch-broker -n 30 --no-pager
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/registry.json /var/lib/orch-broker/registry.json.v4.bak
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json
sudo sha256sum /var/lib/orch-broker/registry.json.v4.bak /root/backup-w3h/registry.json.w3g
sudo grep -o key_sha256 /var/lib/orch-broker/registry.json | wc -l
sudo ls -a /var/lib/orch-broker | grep -c '^\.state-' || true
```
예상: 유닛 `active (running)`, 로그에 오류 없음. `registry.json`은 `orch-broker:orch-broker 600`이고 **`"version":5`**, **`registry.json.v4.bak`이 `orch-broker:orch-broker 600`으로 있고 1-3의 사본과 sha256이 같다**(다르면 낡은 `.v4.bak`이 있었던 것: 멈추고 보고, 18.0 (b)), `key_sha256`은 **0개**(전환 전의 행은 해시를 받지 않는다), 임시 파일 `.state-*` 없음(0). **버전이 4 그대로이거나 broker가 시작하지 않으면 멈춘다 → 단계 4.**

**1-6. 읽기 op와 회귀.** `orch` 계정이 소켓으로 호출한다(값을 찍지 않는 읽기, 16장 1-6과 같은 명령):
```
for op in providers projects status tokens; do sudo -u orch python3 -c 'import socket,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(("{\"v\":1,\"op\":\""+sys.argv[1]+"\"}\n").encode());print(s.makefile().readline())' $op; done
```
예상: `providers` 두 제공자(내용 불변). `projects`는 18.0 (a)의 `projects-before.json`과 **같은 프로젝트·`ai_context`·`provider`·`remote`·`origin`**이고 어느 `remote`에도 `key_sha256`이 없다. `status`·`tokens`는 이전과 같다. **다르면 멈춘다 → 단계 4.**

**검증 체크리스트:** 1-4 통과(등록부 아직 v4), 1-5의 active·`"version":5`·`.v4.bak` 600과 sha256 일치·해시 0개, 1-6의 읽기 응답. **그래도 안 되면:** 단계 4. orchd는 멈춘 상태다(1-2).

### 단계 2. orchd 갱신 [사용자]
```
sudo systemctl is-active orchd          # inactive
for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /tmp/w3h/$f /usr/local/lib/aigate/$f; done
sudo systemctl start orchd
journalctl -u orchd -n 40 --no-pager
sudo sha256sum /usr/local/lib/aigate/orchd /tmp/w3h/orchd
```
**검증:** 로그 오류 없음, `approvals: mode signed`, `orch list`의 마지막 번호 그대로, 해시 일치, 로그인 없이 `curl -sk -o /dev/null -w '%{http_code}\n' https://<FQDN>/api/v1/projects/toy/removal-preview`가 **401**(같은 방식으로 `POST /api/v1/projects/remove/propose`, `POST /api/v1/projects/remove-key/propose`도 401: "로그인 없이는 닫혀 있다"는 것만 증명하고 버전은 위 해시로 본다).

**2-1. 403 진단 로그(일부러 만든 403 하나). [컨트롤러]** 로그인하지 않은 채 다른 출처를 주장하는 요청 한 번(아무것도 바꾸지 않는 읽기 경로, 쿼리에 표식 문자열):
```
curl -sk -o /dev/null -w '%{http_code}\n' -H 'Origin: https://w3h-probe.invalid' 'https://<FQDN>/api/v1/auth/session?probe=w3hmarker'
sudo journalctl -u orchd --since '5 min ago' --no-pager | grep 'orchd: refused 403'
sudo journalctl -u orchd --since '5 min ago' --no-pager | grep -c w3hmarker
```
예상: 응답 **403**, 거부 줄이 **정확히 한 줄** `orchd: refused 403 bad_origin GET /api/v1/auth/session`(시각 접두어 뒤), 표식 `w3hmarker`는 **0건**(종료 코드 1: 정상; 쿼리·헤더는 로그에 남지 않는다). 앞단 프록시가 `Origin`을 바꾸거나 지우면 403이 아닐 수 있다: 그때는 기록만 하고 넘어간다(결과 문서 33번 6장). **로그인한 사용자의 정상 사용 중에는 이 줄이 나오지 않아야 한다**: 나오면 코드(`csrf_failed`·`bad_signature` 등)를 기록해 보고한다(W3g 컷오버 3-3 첫 시도의 원인 미확정 403을 다시 보면 이 줄로 원인을 본다: 31번 9장).

### 단계 3. 웹에서 확인 [사용자]
**전제:** 단계 1·2 통과. 패스키로 로그인. "예상"은 `internal/broker/delete_w3h_test.go`·`adversarial_w3h_test.go`, `internal/orch/api/w3h_h2_test.go`·`adversarial_w3h_test.go`, `internal/orch/web/test/delete.test.js`에서 읽은 것이다. **새로 만든 시험 저장소로만 한다**(이름은 사용자 결정, 예: `w3h-smoke-a`, `^[a-z][a-z0-9-]{1,39}$`, GitHub, 공개, 템플릿 `basic`).

| # | 할 일 | 예상(코드·시험 기준) | 다르면 |
|---|---|---|---|
| 3-1 | **회귀.** `#/projects`, `#/providers` | 목록이 단계 1 전과 같다. `toy`·`sample-app`의 "삭제…" → "복제본과 등록 지우기…" 미리보기에 **"배포 키도 함께 떼기"가 없다**(원격·제공자 없음, W3g와 같음) — 확인만 하고 취소 | 체크 상자가 보이면 멈추고 보고 |
| 3-2 | **[사용자] 새 저장소 A: 만들기 → 해시 확인 → 키 떼기 + 복제본·등록 정리(서명 한 번).** (a) "새 프로젝트 만들기"로 A를 만든다(15장 3-6과 같다, 복제 `cloned`). (b) **[컨트롤러]** 아래 "3-2 (b) 명령"으로 A의 `remote.key_sha256`이 있고(64자리 소문자 16진) **A의 키 폴더 `.pub` 줄의 sha256과 같다**는 것을 본다(앞 8자리만 기록). (c) 17장 3-2 (b)~(d)와 같은 흐름: 미리보기에 "배포 키도 함께 떼기"가 체크되어 있고 저장소·키 제목 `orchd <A>`·지문이 보인다 → 확인 → 서명 | 17장 3-2와 같다: 결과 "완료", 키만 사라지고 A는 GitHub에 **남는다**, 등록·복제본·키 폴더 지움, broker 로그 `remove_project: removed project <A> from the registry`. 거부 줄(`orchd: refused 403`) 없음 | 해시가 없거나 `.pub`의 sha256과 다르면 **떼기를 하지 않고 보고**(서명 전 `deploy_key_mismatch`가 날 것이다). `deploy_key_mismatch`(409): 키 파일이 등록 때와 다르다 — 아무것도 지우지 않았다, 보고. 그 밖은 17장 3-2의 "다르면" |
| 3-3 | **[있을 때만] 전환 전에 만든 GitLab 행.** 제공자가 GitLab이고 원격이 있는 행이 1-6에 있으면(지금은 없을 것으로 예상) 그 행의 "복제본과 등록 지우기…" 미리보기만 연다 | **"배포 키도 함께 떼기"가 보이지 않는다**(해시 없는 GitLab 행: `detach_key_available:false`). 확인만 하고 취소 | 보이면 멈추고 보고(H-D3 위반) |
| 3-4 | **[선택, 17장 3-4의 조건을 모두 만족할 때만] GitLab 키 떼기.** 이 도구로 **전환 뒤에** 만든 GitLab 시험 프로젝트에서 3-2와 같은 흐름(3-2 (b)의 해시 확인은 공용 `.pub`으로) | 17장 3-4와 같다(그 프로젝트에서만 키가 사라지고 `sample-collector`에서는 그대로) | 17장 3-4의 "다르면"과 복구 |
| 3-5 | **[컨트롤러] 누출 점검.** 17장 3-5와 같은 명령, 그리고 아래 "3-5 추가 명령" | 원장 키 `action,actor,code,ok,subject`뿐, 로그에 토큰·`PRIVATE KEY`·키 본문(`AAAAC3Nza…`) 0건, 거부 줄은 모두 `orchd: refused 403 <코드> <메서드> <경로>` 모양(물음표 없음) | 보이면 멈추고 보고 |

**3-2 (b) 명령**(표 안에 `|`를 쓰지 않으려고 표 밖에 둔다. `<A>`는 프로젝트 이름):
```
sudo -u orch python3 -c 'import socket,json,sys;s=socket.socket(socket.AF_UNIX);s.connect("/run/orch-broker/broker.sock");s.sendall(b"{\"v\":1,\"op\":\"projects\"}\n");r=json.loads(s.makefile().readline());p=[x for x in r["result"]["projects"] if x["id"]==sys.argv[1]][0];print((p["remote"] or {}).get("key_sha256","(없음)"))' <A>
sudo -u orch sh -c 'head -n 1 /home/orch/orchd/ssh/keys/<A>/id_ed25519.pub | tr -d "\n" | sha256sum'
```
예상: 두 값이 같다(키 줄은 orchd가 `.pub`의 한 줄을 끝 줄바꿈 없이 보낸 것이므로; 코드 기준 예상, 서버 미확인). GitLab(3-4)은 두 번째 명령의 경로가 `/home/orch/orchd/ssh/id_gitlab_deploy.pub`.

**3-5 추가 명령**(`grep -c`가 0이면 종료 코드 1: 정상):
```
sudo journalctl -u orch-broker -u orchd --since '1 hour ago' --no-pager | grep -c AAAAC3Nza
sudo journalctl -u orchd --since '1 hour ago' --no-pager | grep 'orchd: refused 403' | grep -c '?'
```

**검증 체크리스트(요약):** 1-5(v5, `.v4.bak` 일치, 해시 0개), 2-1(403 한 줄, 표식 0건), 3-2(새 행에 해시, 그 해시로 떼기), 3-3(있으면: 옛 GitLab 행에 선택지 없음), 3-5. 결과는 결과 문서 33번 9장에 남긴다.

### 혼합 버전 (순서를 지키면 생기지 않는다)
| 조합 | 동작 |
|---|---|
| **W3h broker + W3g orchd** | 서명·인자가 같으므로 만들기·연결·삭제가 동작하고, 새 행은 해시를 갖는다. 떼기는 broker가 대조한다: W3g orchd는 미리보기에서 해시를 보지 않으므로 **해시 없는 GitLab 행에도 선택지를 보이고**, 서명 뒤 broker가 모양 단계에서 `deploy_key_mismatch`로 거부한다(아무것도 바뀌지 않음, 요청 ID 소모 없음). W3g orchd는 이 코드를 모르므로 화면은 **"결과를 알 수 없음"**이고 한도 칸을 쓴다. 403 진단 로그는 없다 |
| **W3g broker + v5 등록부**(바이너리만 되돌린 롤백) | **`-check`가 거부하고, 시작하면 등록부를 읽거나 쓰는 모든 op가 `registry_unavailable`**(등록부에 기대는 orchd 기능 전부가 막힌다). 단계 4의 등록부 편집 없이는 쓸 수 없다 |
| **W3g orchd + W3h broker의 웹 화면** | 웹은 orchd 안에 있으므로 W3g 화면이다(`detachRefused` 문장의 새 원인 없음). 기능 차이는 위 첫 줄과 같다 |

### 복구 (서버에서 일부러 만들지 않는다)
- **(a) 1-5에서 `.v4.bak`의 sha256이 1-3 사본과 다르다.** 낡은 `.v4.bak`이 있었다(18.0 (b)를 건너뜀). 등록부 자체는 정상일 수 있다. 멈추고 보고한다. 롤백할 때는 `.v4.bak`을 믿지 않는다(단계 4는 어차피 `.v4.bak`을 쓰지 않는다).
- **(b) 전환 뒤 만든 행의 떼기가 `deploy_key_mismatch`.** 키 파일이 등록 때와 다르다: `sudo sha256sum /home/orch/orchd/ssh/id_gitlab_deploy.pub`를 18.0 (c)의 값과 비교한다(GitHub은 그 프로젝트의 키 폴더 `.pub`). 바뀌었으면 아래 "키 교체"를 따른다. 아무것도 지워지지 않았다(서명 전 거부).
- **(c) 로그인한 정상 사용 중 403.** `sudo journalctl -u orchd --since '10 min ago' --no-pager | grep 'orchd: refused 403'`의 코드로 원인을 본다: `csrf_failed`(오래된 화면·세션: 강력 새로고침·재로그인), `bad_origin`(주소창의 주소가 콘솔 주소와 다름), `bad_signature`·`stale_signature`·`unknown_signer`(서명·시계·서명자 목록), `forbidden_uid`(소켓 사용자: 같은 시각의 `socket peer uid N refused` 줄도 함께 남는다). 로그 줄이 없는데 403이면 orchd가 W3h가 아니다(해시를 본다).
- **(d) 롤백 뒤 `registry_unavailable`.** W3g broker가 v5 등록부를 읽고 있다: 단계 4 A-3을 하지 않았다.
- W3g의 복구(17장 "복구" (a)~(e))는 그대로다.

### 키 교체 (공용 GitLab `.pub` 또는 프로젝트 키 폴더를 바꾼 경우) [사용자][주의]
해시는 등록할 때의 `.pub` **줄 전체**(종류·본문·주석)의 sha256이다. 그래서 **키를 새로 만들거나 주석만 고쳐도** 그 전에 만든 행은 떼기가 거부된다(미리보기에 선택지가 없고, `remove_deploy_key`는 서명 전 409 `deploy_key_mismatch`). 이것은 의도한 안전한 실패이고 이 도구로 해시를 다시 채우는 길은 없다.
1. 키 파일을 바꾸기 전에 `projects` 읽기로 **어떤 행이 그 키의 해시를 가졌는지** 적는다(GitLab: 해시가 공용 `.pub`의 sha256과 같은 행).
2. 바꾼 뒤 그 행을 지울 때는 **"배포 키도 함께 떼기"를 끈 채로** "복제본과 등록 지우기"(떼기 없이)를 서명한다.
3. 옛 키는 **제공자 화면에서 사람이 뗀다**: GitLab은 그 프로젝트의 Settings > Repository > Deploy keys에서 옛 키를 그 프로젝트에서만 비활성(다른 프로젝트가 아직 옛 공용 키로 게시하면 그 프로젝트에서는 떼지 않는다), GitHub은 그 저장소의 Settings > Deploy keys.
4. 바꾼 뒤 새로 만들거나 연결한 행은 새 키의 해시를 갖고 정상으로 떼어진다.
(GitLab 공용 키는 다른 프로젝트의 게시가 쓰므로 키 파일 교체 자체는 이 런북의 범위가 아니다: 17.0 (e)와 같이 파일을 고치지 않는 것이 기본이다.)

### 단계 4. 롤백 [사용자][주의]
**순서: orchd 먼저 멈춤 → broker 멈춤 → 지금 등록부의 사본 → 등록부 편집(필요할 때) → W3g broker 바이너리 → 샌드박스 `-check` → 낡은 `.v4.bak` 치우기 → broker 시작 → orchd.** **전제:** `orch list`와 `orch approvals`에 작업이 없다.

**왜 `.v4.bak` 복원이 아닌가.** `registry.json.v4.bak`은 **전환 직전의 등록부 그대로**다. 그것을 되살리면 전환 뒤의 등록부 변경이 **모두** 되돌아간다: 전환 뒤 만들거나 연결한 프로젝트의 행이 사라져 그 폴더가 **등록 없는 폴더**가 되고(게시가 기본 `--git-remote`로 간다), 전환 뒤 지운 행이 되살아난다(폴더·키·원격은 없다). 그래서 **지금 등록부를 W3g가 읽을 수 있게 고치는 쪽**을 쓴다: 해시를 가진 행이 없으면 `version`만 5 → 4로, 있으면 `key_sha256` 구성원을 지운 뒤 `version`을 4로(행·순서·그 밖의 값은 그대로). 아래 스크립트는 두 경우를 함께 처리한다(해시가 0개면 버전만 바꾼다). `.v4.bak` 복원은 등록부 파일이 깨진 경우의 마지막 수단이고, 그때는 위 부작용을 사람이 정리한다(16장 단계 4 A-6과 같은 문제).

**A. broker (등록부 편집 + W3g 바이너리).**
1. `sudo systemctl stop orchd`, 이어서 `sudo systemctl stop orch-broker`.
2. **지금 등록부의 사본(되돌릴 수 있게)과 상태 확인:**
```
B=/root/backup-w3h
R=/var/lib/orch-broker/registry.json
sudo install -o root -g root -m 0600 $R $B/registry.json.w3h-before-rollback-$(date +%Y%m%d-%H%M%S)
sudo grep -o '"version": *[0-9]*' $R                 # "version":5
sudo grep -o key_sha256 $R | wc -l                   # 해시를 가진 행의 수(0이면 버전만 바뀐다)
```
3. **등록부 편집(root, `python3`).** `key_sha256`을 모두 지우고 `version`을 4로, 임시 파일에 쓴 뒤 원자적으로 바꾼다(소유자 `orch-broker`·모드 600 유지, 파일·폴더 fsync). 시험 `TestRunbookRollbackEditMakesAW3gRegistryW3h`가 이 스크립트를 글자 그대로 돌려 W3g 로더가 받아들이고 그 밖의 값이 그대로인지 확인한다(그 시험의 `rollbackScript`와 같아야 한다). 버전이 5가 아니면 아무것도 바꾸지 않고 멈춘다.
```
sudo python3 -I - /var/lib/orch-broker/registry.json <<'EOF'
import json, os, sys
p = sys.argv[1]
with open(p, encoding="utf-8") as f:
    r = json.load(f)
if r.get("version") != 5:
    sys.exit("not a version 5 registry: nothing changed")
n = 0
for x in r["projects"]:
    rem = x.get("remote")
    if isinstance(rem, dict) and "key_sha256" in rem:
        del rem["key_sha256"]
        n += 1
r["version"] = 4
st = os.stat(p)
tmp = p + ".rollback-tmp"
fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as f:
    json.dump(r, f, ensure_ascii=False, separators=(",", ":"))
    f.write("\n")
    f.flush()
    os.fsync(f.fileno())
os.chown(tmp, st.st_uid, st.st_gid)
os.replace(tmp, p)
dfd = os.open(os.path.dirname(p) or ".", os.O_RDONLY)
os.fsync(dfd)
os.close(dfd)
print("removed key_sha256 from", n, "rows; version 4")
EOF
sudo grep -o '"version": *[0-9]*' /var/lib/orch-broker/registry.json      # "version":4
sudo grep -o key_sha256 /var/lib/orch-broker/registry.json | wc -l        # 0
sudo stat -c '%U:%G %a %n' /var/lib/orch-broker/registry.json             # orch-broker:orch-broker 600
sudo ls -a /var/lib/orch-broker | grep -c rollback-tmp || true            # 0
```
(스크립트가 `.rollback-tmp`가 이미 있다고 멈추면 이전 시도의 잔재다: 내용을 보고 `/root/backup-w3h`로 옮긴 뒤 다시.)
4. **W3g broker 바이너리와 `-check`:** `sudo install -o root -g root -m 0755 /root/backup-w3h/bin/orch-broker /usr/local/bin/orch-broker` → 1-4와 같은 `systemd-run … -check`. 예상: **통과**(W3g broker가 고친 v4 등록부를 받아들인다). 거부("프로젝트 등록부를 신뢰할 수 없습니다")면 시작하지 않고 2에서 만든 사본과 함께 보고한다.
5. **낡은 `.v4.bak` 치우기(다시 올릴 때를 위해):** `sudo mv /var/lib/orch-broker/registry.json.v4.bak /root/backup-w3h/registry.json.v4.bak.rolledback-$(date +%Y%m%d-%H%M%S)`. 그대로 두면 다음 W3h 전환이 그 파일을 덮어쓰지 않아 새 백업이 생기지 않는다(18.0 (b)).
6. `sudo systemctl start orch-broker`; 1-6의 읽기가 단계 4 직전과 같은 프로젝트를 주고(`key_sha256`만 없다) 오류가 없는지 본다.
7. **경고:** 롤백 뒤에는 전환 뒤 만든 행도 해시가 없다. 그 행은 W3g 규칙으로 떼어진다(GitLab은 본문만: W3g의 남은 위험, 31번 7장 1이 다시 열린다). **다시 W3h로 올리면** 그 행들은 해시 없는 행이 되어 GitLab 떼기가 거부된다(⚠ 3과 같은 처리).

**B. orchd·CLI.** `for f in orchd orch aigate; do sudo install -o root -g root -m 0755 /root/backup-w3h/bin/$f /usr/local/lib/aigate/$f; done` → `sudo systemctl start orchd`. 브라우저는 강력 새로고침(웹은 orchd 안에 있다).

**검증:** `journalctl -u orch-broker -u orchd`에 오류 없음, `sudo sha256sum /usr/local/bin/orch-broker /usr/local/lib/aigate/orchd`가 18.0 (a)의 값과 같음, `orch list`가 그대로, 로그인한 화면에서 `#/projects`가 보인다. **그래도 안 되면:** 아무것도 더 지우지 말고 `journalctl -u orch-broker -n 80`과 `sudo ls -lR /root/backup-w3h | head -50`을 보고한다. 등록부를 마지막 수단으로 `.v4.bak`이나 `$B/registry.json.w3g`(같은 내용)에서 되살리는 것은 위 "왜 `.v4.bak` 복원이 아닌가"의 부작용을 사용자가 받아들일 때만 한다.

### 관찰 기록 (컷오버 때 채운다: 결과 문서 33번 9장으로 옮긴다)
| 항목 | 예상 | 본 것 | 판정 |
|---|---|---|---|
| 18.0 (a) 등록부 | `"version":4`, `toy`·`sample-app`만 | | |
| 18.0 (b) `registry.json.v4.bak` | 없음 | | |
| 18.0 (c) 공용 `.pub` 필드 수, sha256 앞 8자리 | 2 또는 3, (기록) | | |
| 1-4 `-check` | 통과, 등록부 v4 그대로 | | |
| 1-5 등록부 | `"version":5`, `.v4.bak` 600·sha256이 1-3 사본과 같음, `key_sha256` 0개 | | |
| 1-6 읽기 | 프로젝트·`remote`·`origin` 불변 | | |
| 2 로그인 없는 요청 | 401 | | |
| 2-1 일부러 만든 403 | 403, 거부 줄 한 줄 `bad_origin GET /api/v1/auth/session`, 표식 0건 | | |
| 3-1 원격 없는 행 | 선택지 없음 | | |
| 3-2 새 행의 해시 | 있음, `.pub` 줄의 sha256과 같음(앞 8자리) | | |
| 3-2 키 떼기(실제 상태·지연) | 키만 사라짐, 등록·폴더 지움, 60초 안 | | |
| 3-3 전환 전 GitLab 행 | (없을 것으로 예상) 있으면 선택지 없음 | | |
| 3-4 GitLab 키 떼기 | (선택) | | |
| 3-5 원장·로그 | 키 5개뿐·키 본문 0건·거부 줄에 `?` 0건 | | |
