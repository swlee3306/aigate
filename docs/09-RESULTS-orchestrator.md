# 서브프로젝트 ② 오케스트레이터 — 결과 기록 (작성 중)

## Task 5: 원격 실행기 동등성
- fakessh 백엔드: 기존 e2e를 그대로 실행, local과 fakessh 모두 156 pass / 0 fail, 판정 148개 동일(`e2e/compare-backends.sh`).
- 적대 시나리오 17개(`e2e/scenarios_remote_test.go`): 위조·거대·끝없는 출력, 깨진 XML, 전송 위조. ok로 끝난 것 없음.
  - 과대 결과(9 MiB JUnit/JSON, 끝없는 JSON)와 전송 위조(flood, 잘림, 거대 선언 길이, unreachable)는 `verification_incomplete`
    **판정이 아니라 gate 오류**(종료 코드 64, 판정·보고서 없음)로 끝난다. 로컬 실행기도 같다. 설계(인프라 오류는 `error`, 판정과 분리,
    오류를 ok로 처리하지 않음)와 일치하므로 유지한다. 오케스트레이터(T7)는 gate 오류를 `error` 상태로 두고 재시도 횟수를 제한해야 한다
    (결정적 과대 출력이 무한 재시도가 되지 않게).
  - exit-code 어댑터 검사는 VM이 말하는 종료 코드를 그대로 믿으므로 rc 거짓말을 탐지할 수 없다(설계상 한계). junit/go-json 검사는
    결과 파일이 없으면 incomplete.
- 실제 VM(test-vm 10.70.0.13, orch-vm 경유 점프, 호스트키 엄격 검사, 역할 키): 대표 시나리오 9개(py·go·ts 각 3개: ok / weakened / behavior 또는 weakened)
  **전부 로컬과 같은 판정으로 통과**. 소요 시간: VM 54 s, 로컬 15 s (약 3.6배; 시나리오당 py 3~4 s, go 6~7 s, ts 8 s).
- 첫 실행에서 TS 3개가 실패한 원인은 하네스: 로컬 `node_modules` 심볼릭 링크 setup이 VM에서 깨진 경로. VM 백엔드에서는 `npm ci`로 바꿨다(결과는 verification_incomplete로 끝나 ok로 오인되지 않았다).
- VM 준비(기록): test-vm에 `python3-pytest python-is-python3`(apt), Node 22(npm `node@22`; apt의 Node 18은 vitest 5.1 요구 미달),
  runner 계정 `go env -w GOTOOLCHAIN=go1.27.1`(go.mod `go 1.27`을 Go 1.22가 `go1.27`로 해석해 실패). 이 변경은 `test-vm-clean` 스냅샷에 **포함되지 않았으므로** Task 9에서 스냅샷을 다시 만들어야 한다.

## Task 13: 통합·보안·장애 시험 (2026-11-06, 실제 VM)

구성: orch-vm에서 `orchd`(nohup, 사용자 `orch`)와 `orch` CLI, 에이전트 = agent-vm의 Claude, 검사 = test-vm(원격 실행기, 엄격한 호스트키),
게시 대상 = orch-vm 안의 로컬 bare 저장소(원격 push 없음), 대상 저장소 = orch-vm의 Go toy(`.aigate.yml`: unit + build), 정책 `toy: L1`, 허용 목록 `push_branch`.
`--restore-each-job=false`(orch-vm에는 홈서버 OpenStack 접근 경로가 없다. 복원은 아래에서 별도 측정).

### 통합 시험
| # | 작업 | 기대 | 결과 |
|---|---|---|---|
| 1 | 새 테스트 파일 추가 | passed → 게시 → done | **done**, 게이트 `ok`, `refs/heads/orch/1`이 수집한 head(14d9c60)와 같음 |
| 2 | 기존 `calc_test.go`에 단언 추가 | needs_approval | **needs_approval**, 승인 대기 1건(protected_modified, 해시 f1e1764bd264). 사람이 승인해야 하므로 **그대로 둠** |
| 3 | 의도적 회귀(Sum이 마지막 원소 무시) | failed(behavior) | **failed**: `behavior_failure: regression` |

작업 1 소요(복원 제외): 합계 약 20 s = 준비 1.3 + 에이전트(Claude) 12.4 + 수집 1.3 + 판정(base·head 각 unit+build, test-vm) 4.4 + 게시 0.03.
복원을 켜면 작업마다 약 52 s가 더 든다(아래). 즉 작업당 약 70 s. 에이전트 시간이 작업 내용에 따라 달라지는 부분이다.

### 장애 주입 (실제 VM)
- `agent_running` 중 `orchd`를 `kill -9` 후 재시작(작업 4): `recovered: step was in flight when the process stopped` 이벤트, 같은 단계 재실행, 커밋 1개, push 1회, 최종 done. 낡은 소켓 파일은 데몬이 처리.
- 판정 중 test-vm 중지(작업 5): `verifying → error` 3회 재시도 후 `failed`, 오류 원인 `ssh connection failed (exit 255) ... Connection timed out` 기록, **passed 안 됨**. test-vm을 켠 뒤(약 15 s) 다음 작업(6)은 done.
  - 한계: 인프라 장애로 끝난 `failed`와 판정으로 끝난 `failed`가 상태값으로는 같다(오류 원인 문자열로만 구분). 재제출 명령이 필요하다.
- 코드 수준 13개 시나리오(`internal/orch/machine/fault_test.go`, 설계서 9장 행 대응). 여기서 **실제 결함 1건 발견·수정**: DB 파일이 삭제돼도 `orchd`가 지워진 inode에서 작업을 계속 처리·게시함 → `store.ErrDBGone`(`cb5e2a2`).

### 보안 점검 (`infra/test/security-checks.sh --run`, 결과 `infra/security-report.md`: PASS)
- agent-vm, test-vm → LAN, 상대 VM, orch-vm, 1.1.1.1:443, 허용 목록 밖 도메인: 28행 전부 차단.
- agent-vm/test-vm에 개인 키·토큰·환경 변수 자격 증명 없음, sudo 불가. orch-vm의 DB·소켓·디렉터리 권한 600/700.
- 오케스트레이터 키로 허용되지 않은 명령(`id`, `prepare 1; id`, 명령 없음) 거부, 포트 포워딩 불가(터널로 데이터 통과 안 함, 실제 확인).
- 첫 실행에서 FAIL이 여러 개 났고 **전부 점검 스크립트의 오탐**이었다(Go 모듈 캐시·플러그인 테스트 픽스처의 문자열, vitest 로컬 토큰, orch 소유 경로를 admin으로 읽으려 한 것, 터널 판정 방식). 고친 뒤 PASS. 승인 파일은 아직 한 번도 승인이 없어 존재하지 않아 SKIP.
- 코드 테스트로만 확인된 항목(실제 VM에서 재현하지 않음): 거대 커밋·많은 ref 수집 거부, 위조 JUnit, 보고서 불일치.

### 스냅샷 복원 (실제)
- `volume revert`는 불가(앞 절). 새 볼륨+새 서버 방식 52 s(삭제 10 + 볼륨 6 + 서버 11 + ssh 25).
- 호스트키: 복원하면 서버가 새로 생겨 cloud-init이 호스트키를 다시 만든다. 스냅샷에 `ssh_deletekeys: false`(`/etc/cloud/cloud.cfg.d/99-aigate-hostkeys.cfg`)를 넣어 `test-vm-clean2`를 만든 뒤 복원하니 **호스트키 동일(SHA256:Boso/…), orch-vm의 고정 키로 재등록 없이 엄격 검사 로그인 성공**. 이전 스냅샷 `test-vm-clean`은 호스트키 문제가 남아 있다.
- `orchd --test-snapshot test-vm-clean2`로 지정. Go의 `TestVM.Restore`+`OpenStackCLI`는 스텁 테스트까지만 하고 실제 홈서버에는 실행하지 않았다(아래 결정 사항).
- 복원마다 30 GB 씬 볼륨이 남고 삭제된 서버의 attachment가 정리되지 않는다(수동 정리).

### 이번 시험으로 확인된 설계 변경·발견
1. 호스트키: 스냅샷에 보존해야 한다(위). 2. 단계 연결 코드(Task 9b)가 계획에 없었다. 3. 승인은 데몬이 아니라 TTY에서 쓰는 승인 파일이 권한이다(Task 11).
4. Codex: 처음에는 agent-vm에서 동작하지 않았다(AppArmor 사용자 네임스페이스 제한). 권한 분류기가 샌드박스 해제와 전역 sysctl 완화를 거부해, 바이너리 한 개에만 허용하는 AppArmor 프로파일(`infra/agent-codex-sandbox.sh`, 사용자가 실행)로 해결. 샌드박스 유지. 작업 8로 전체 흐름 확인(done, 34 s).

### Known limits (최종 검토에서 나왔고 고치지 않은 것)
- **"게시 후 회귀" 강등 트리거는 운영에서 도달할 수 없다.** `done`은 종료 상태라 한 번 `done`이 된 작업은 다시 `verifying`에 들어가지 않는다. `wasPublished`는 실제 경로에서 항상 거짓이다. 단위 테스트(`TestVerifyDirectlyOnDoneTaskDemotes`)는 `done` 작업에 `verify`를 직접 호출하므로 강등 로직만 확인하며 상태 머신 경로를 검증하지 않는다. 진짜 트리거는 내구성 있는 게시 기록이 필요하다(예: 같은 클래스·저장소의 이후 작업이 자동 게시된 작업 뒤에 실패). 이제 원장의 `publish_action`/`publish_result` 이벤트가 그 기록이 된다.
- **DB를 교체하면 작업 ID가 1부터 다시 시작한다.** `refs/orch/<id>`, 원격 `orch/<id>` 브랜치, `PublishHash`(작업 ID 기반)가 이전 것과 충돌할 수 있다(non-fast-forward push 실패, 오래된 MR을 "이미 있음"으로 취급, 이전 승인 재사용). 운영 규칙: **`refs/orch/*`와 원격의 `orch/*` 브랜치를 지우지 않고 DB를 교체하지 않는다.**
- **orchd가 죽으면 실행 중이던 에이전트가 재시도와 나란히 계속 돌 수 있다.** 래퍼의 `run`은 프로세스 그룹을 따로 두지만 감시 타이머만 죽이고, `Recover`는 `prepare` 없이 `agent_running`을 다시 실행한다. 운영 규칙: **orchd가 비정상 종료한 뒤에는 다시 제출하기 전에 agent-vm에 남은 에이전트 프로세스가 있는지 확인한다.** 값싼 완화책(래퍼가 `run`/`prepare`에서 pidfile에 적힌 작업의 프로세스 그룹을 먼저 죽인다)이 다음 단계다. Task 4의 보류 항목은 테스트 VM 실행기만 다룬다.
- 검토자가 평가하지 못한 것: 실제 OpenSSH/OpenStack 동작(`OpenStackCLI` 복원 end to end, 같은 이름 볼륨의 동작, 새 `test-vm-clean2` 스냅샷 복원 후 호스트키), `internal/remote/ssh.go` 정밀 검토(훑어만 봄), 에이전트 VM 하나를 공유하는 동시성 > 1(같은 `agent` uid라 한 작업의 에이전트가 다른 작업 작업공간을 건드릴 수 있음; 게이트는 모든 head를 판정), `aigate` 쪽 승인 유효성(`approval.Store.Valid`)이 `approvals.Grants.Granted`와 정확히 일치한다는 가정, 실제 v1 이전/부분 마이그레이션된 운영 DB, e2e 시험과 `infra/test/*.sh`.
- 원장 기록의 내구성: `task_transition`은 SQLite 커밋 *뒤에* 기록하는 최선 노력이라 그 사이 크래시로 하나가 빠질 수 있다(SQLite 이벤트 표에는 남는다). 외부 행동은 `publish_action`을 행동 *앞에* 기록하므로 기록 없는 행동은 없다.

## 무인 적합성 판정 (솔직하게)
**아직 사람 없이 쓰기에는 부족하다.** 근거:
- 시험은 toy 저장소에서 한 것이다. 실제 저장소(sample-collector)와 GitLab(Tailscale 필요)에서는 아직 돌려 보지 않았다.
- 승인 흐름(`orch approve`)은 가짜 TTY를 쓰는 Go 테스트로만 확인했고, 실제 사람이 터미널에서 한 번도 해 보지 않았다(작업 2가 대기 중).
- 홈서버 OpenStack 자격 증명을 orch-vm이 어떻게 쓸지 결정되지 않아 **작업마다 복원**은 실제 구성으로 검증되지 않았다.
- (Codex는 해결됨.) exit-code 어댑터는 VM이 말하는 종료 코드를 믿는다. 검사 코드는 같은 uid로 돌아 자기 결과 파일을 위조할 수 있다(스냅샷 복원과 크기 상한이 완화책).
- 인프라 장애로 끝난 작업을 재시도하는 명령과 알림(FR13)이 없다.
- 반복 사용 통계가 없다(승격 제안은 이력이 쌓여야 의미가 있다).
무인으로 허용해도 되는 범위(제안): L0~L1, 작업 종류 `새 테스트 추가`/`문서`, 게시는 로컬/작업 브랜치 push까지.

## 마지막 전체 리뷰 반영 후 실제 VM 재확인 (2026-11-06)
- 수정 라운드(커밋 `88067a9`..`f3cf960`) 뒤 재빌드한 `orchd`/`orch`를 orch-vm에 배포. 기존 DB(작업 1~6, 승인 대기 1건)는 새 스키마로 올라왔고 데이터 유지.
- 시작 로그에 실효 설정이 모두 찍힌다(복원 여부, 허용 목록과 출처, 정책 해시, 원격, VM 주소). 복원 없이 게시하는 구성은 `--allow-publish-without-restore` 없이는 시작 거부.
- 같은 DB로 두 번째 `orchd`를 띄우면 `another orchd already runs ... refusing to start a second one`으로 거부(exit 1).
- 새 작업 7: 정상 `done`. 원장에 `task_transition` 9건, `publish_action`(push 전), `publish_result`, 순서대로 기록.
