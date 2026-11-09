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
| 2-2 | toy 작업 제출: 네 칸 중 "목표"만 적고 미리보기를 확인한 뒤 "제출" | 미리보기에 `Goal: …`, 바이트 수 표시. 제출하면 `#/tasks/<ID>`로 이동, 상태 `queued` → 진행 줄 "진행 중 · <단계> · N초 경과" | 오류 문구는 코드별 고정 문장이다(서버 원문은 보이지 않음. 예외로 `queue_full`은 서버의 고정 두 문장 중 하나를 그대로 보여 준다). `queue_full`이면 큐 확인, `unknown_class`면 정책 확인 |
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
