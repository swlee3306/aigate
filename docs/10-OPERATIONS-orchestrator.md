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
- `orch approve <승인항목ID>`는 **실제 터미널(TTY)에서 사람이** 한다(`ssh -t`). **ID는 `orch approvals`의 ID 열(승인 항목 번호)이고 작업 번호가 아니다**(작업 2의 항목이 1번이면 `approve 1`; 승인 파일 위치는 `--gate-home /home/orch/orchd/gate-home`, 플래그는 명령 앞에). 예: `ssh -t ... 'sudo -u orch -H /home/orch/bin/orch --socket /home/orch/orchd/run/orchd.sock --gate-home /home/orch/orchd/gate-home approve 1'`. 데몬은 승인을 줄 수 없고 승인 파일(GateHome)에 맞는 승인이 있을 때만 재판정한다.
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
