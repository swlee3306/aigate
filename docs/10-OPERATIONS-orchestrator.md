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
  - **(d) 배포 키의 공개 키(`.pub`) — 오늘은 있는지 모른다(미확인).** `GET /api/v1/settings/deploy-key`는 `--git-ssh-key` 경로 + `.pub`만 읽는다(개인 키는 코드가 열지 않는다. 시험 있음). 실서버의 `--git-ssh-key`는 `/home/orch/orchd/ssh/id_gitlab_deploy`다(`systemctl cat orchd`로 확인). 확인(개인 키는 읽지 않는다): `sudo ls -l /home/orch/orchd/ssh/` 와 `sudo stat -c '%U:%G %a %s %n' /home/orch/orchd/ssh/id_gitlab_deploy.pub`(예상: 일반 파일, 소유자 `orch`(또는 root), 모드 `644`, 크기 4096 이하, **심볼릭 링크 아님**). 한 줄 공개 키인지: `sudo wc -l < /home/orch/orchd/ssh/id_gitlab_deploy.pub`(0 또는 1: 끝 줄바꿈 한 개까지는 허용)와 `sudo ssh-keygen -l -f /home/orch/orchd/ssh/id_gitlab_deploy.pub`(`.pub`만 읽어 지문을 찍는다. `ssh-ed25519` 또는 `ssh-rsa` 한 줄이어야 한다. 개인 키 파일에는 `-f`를 쓰지 않는다). **없어도 W3b의 어떤 기능도 막히지 않는다**(소비자는 W3c이고, 지금은 `deploy_key_unavailable`로만 답한다). 없으면 기록만 하고 넘어간다: 만들려면 `ssh-keygen -y`가 **개인 키를 읽으므로** 사용자 결정이다(아래 알아둘 것 12).
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
8. **알려진 한계(비밀이 어디를 거치는가).** 토큰 값은 **브라우저 JS 메모리 → orchd 메모리 → broker 소켓 → broker 메모리 → 파일**을 거친다. JS 문자열은 바꿀 수 없어서 지우는 것은 참조를 버리는 것뿐이고 GC가 덮어쓸 때까지 글자가 남는다. orchd는 값을 바이트로 받아 쓰고 처리 뒤 가능한 범위에서 0으로 덮지만(최선), JSON 디코더 내부 버퍼·커널 소켓 버퍼·GC가 옮긴 사본은 지울 수 없다: **`apply` 도중 코어 덤프에는 값이 들어 있을 수 있다.** `last4`(마지막 4자)가 목록에 보인다(식별용, 알려진 노출). 값 입력칸은 `type=text` + CSS 가림(`text-security`)이라서 **그 칸에서 복사하면 실제 글자가 복사될 수 있다**(비밀번호 칸이 아니므로 브라우저의 "비밀번호 저장" 제안을 피하려는 의도적 선택, 보장이 아니라 휴리스틱). **두 가지를 더 알아 둔다:** ① 브라우저의 `api.js`는 서버가 403 `csrf_failed`를 주면 세션을 한 번 새로 고치고 **같은 요청 본문(값과 서명 포함)을 한 번 다시 보낸다**(이때는 broker 호출이 일어나지 않았다). ② 유닛에 `LimitCORE=0`이 없으므로 `apply` 도중 충돌하면 systemd-coredump가 값을 담은 메모리를 저장할 수 있다(선택적 유닛 강화로 적어 두고 **적용하지 않았다**: 유닛은 이번에 바꾸지 않는다). 이 한계 전부 설계서 6장에 있고 결과 문서 6장에 다시 적었다.
9. **`gitlab`·`github` 종류의 토큰은 W3c 전까지 쓰는 코드가 없다.** 저장·목록·삭제만 된다. 기존 `create_mr`은 계속 설정의 `gitlab.token_file`을 쓴다(바뀌지 않음). 그러므로 **지금 등록해도 어떤 동작도 바뀌지 않는다.**
10. **한도(주체별).** `begin`+`message`를 합쳐 시간당 **60회**, `apply` 시간당 **20회**. 초과하면 429 `token_rate_limited`. `apply`의 슬롯은 broker에 닿기 전에 실패했을 때(연결 실패)와 broker가 일하기 전에 거절했을 때(`busy`, `forbidden`)만 돌려준다.
11. **컷오버에서만 사용자가 할 수 있는 확인(3-3, 3-5):** 브라우저에 시험용 값을 쳐서 **"비밀번호 저장" 제안이 안 뜨는지**, 글자가 가려지는지, 붙여넣기가 되는지, **실제 패스키로 `set_token`을 서명**하는 것. 이 저장소는 대역 패스키(`navigator.credentials.get` 바꿔치기)와 시험용 서명자만 썼고 실제 브라우저의 비밀번호 관리자 동작은 한 번도 보지 못했다.
12. **배포 키의 `.pub`이 없을 때.** `ssh-keygen -y -f /home/orch/orchd/ssh/id_gitlab_deploy`는 개인 키를 **읽는다**(코드가 아니라 `ssh-keygen`이). W3c 전에는 쓸 곳이 없으므로 **지금 만들지 않아도 된다.** 만든다면 사용자 결정으로 `orch` 계정에서 `ssh-keygen -y -f <개인 키> > <개인 키>.pub; chmod 0644 <개인 키>.pub`(결과가 한 줄 `ssh-ed25519 …`인지 `ssh-keygen -l -f <.pub>`로 확인)이고, 되돌리려면 `.pub`만 지운다.
13. **가짜 토큰을 진짜처럼 등록하지 않는다.** `gitlab` 종류의 더미 값을 "시험삼아" 넣지 않는다(나중에 W3c가 그 이름으로 실제 호출을 할 수 있다). 시험은 3-5의 선택지(**교체 없음 / 사용자가 가진 같은 OpenAI 키 값으로 교체**)와 직후의 삭제로 한다.

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
| 3-3 | **[사용자] 입력칸 점검(서명하지 않는다).** "토큰 추가"에서 종류 `gitlab`, 이름 `check-only`, **임의의 더미 문자열 32자 이상**을 값 칸에 직접 입력하고 붙여넣기도 해 본다. 만료일은 먼 미래. 그 뒤 **"취소"**(또는 화면 이동) | ① **글자가 점으로 가려진다**(입력 중·붙여넣은 뒤), ② **붙여넣기가 된다**, ③ **브라우저의 "비밀번호 저장/업데이트/강력한 비밀번호 제안"이 한 번도 안 뜬다**(브라우저의 비밀번호 관리자 목록에도 새 항목이 없다), ④ 확인 화면(마지막 4자와 값 해시 앞 8자만)에서 값 칸이 비어 있고 전체 값이 다시 보이지 않는다, ⑤ 취소하면 아무것도 저장되지 않는다(목록 그대로, `sudo ls /var/lib/orch-broker/secrets`가 비어 있음) | ③이 보이면 **기록하고(어느 브라우저인지) 그 값은 거절한다.** 가림이 안 되면 `text-security` 미지원 브라우저(기록). 값이 DOM에 남는다고 보이면 멈추고 보고 |
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
