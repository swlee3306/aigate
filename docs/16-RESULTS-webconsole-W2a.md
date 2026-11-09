# 웹 콘솔 W2a — 결과 기록 (2026-11-09)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w2a`)에서 끝났고, 실제 서버(orch-vm)에는 아직 아무것도 배포하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 10장, 설계는 `14-DESIGN-webconsole-W2.md` 3장, 계획은 `15-PLAN-webconsole-W2a.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, 가짜 클라우드, 실제 `git`)만 가리킨다. 실제 OpenStack CLI, 실제 정책 파일, 실제 브라우저의 키보드·스크린 리더, 실제 부하는 **한 번도 하지 않았다**(6장).

## 1. 만든 것

**A1. 누가 했는지 기록(스키마 v7).** `task_events`에 `actor` 열을 더했다(옛 이벤트는 비어 있음). 주체는 인증된 요청의 principal에서 한 곳(`withPrincipal`)에서만 정해진다: `socket:uid:<n>`, `session:<서명자 ID>`, `token:<서명자 ID>`, 워커·복구는 `system`. 형식(`[A-Za-z0-9_.:@-]`, 80바이트 이하, `kind:id`)에 맞지 않으면 `invalid`로 저장하고 쓰기는 실패하지 않는다. 원장(`orchd.jsonl`)의 `task_transition`, 새 `task_submitted`, `approval_granted`의 `via`·`actor`는 같은 `kind:id` 값이다(R6). `TaskView.actor`(웹 "마지막 변경"), `orch show`의 `주체:` 줄. **이 둘은 *가장 최근 이벤트*의 주체이지 제출자가 아니다**: 워커가 `queued`→`preparing`을 약 250 ms 안에 `system`으로 옮기므로 정상 서버에서도 곧 `system`이 된다. 제출자는 웹 작업 화면 타임라인의 첫 단계(대기)의 주체(A8), 또는 원장 `task_submitted`(`task_events`의 `created` 행)에서 본다. 이벤트마다의 주체는 A8에서 웹에 보이게 했다(CLI는 그대로). 클라이언트가 보내는 헤더나 본문 필드로는 주체를 바꿀 수 없다(5장 적대 시험).

**A2. 취소·재시도 오류 코드.** 취소: `not_found` 404, `already_final` 409, `state_changed` 409(비교-교체에 5번 진 경우; 전에는 일반 500). 재시도: `not_found`, `not_retryable` 409(+`reason`, 기계가 만든 고정 한국어 문장), `state_changed` 409. `TaskView.cancellable`은 상태 표(`machine.Allowed`)에서 계산한다.

**A3. 제출 검증과 `/repos`.** 정책에 종류 목록이 있으면 목록에 없는 종류는 `400 unknown_class`(R3: 정책이 없거나 목록이 비면 예전처럼 받음). 대기 작업 상한은 **주체별**(`--queue-limit`, 기본 20)과 **서버 전체**(`--queue-limit-total`, 기본 100)이다. 두 개수 세기와 삽입이 **한 트랜잭션**이라 동시 제출로도 넘지 않는다(`429 queue_full`, 문장만 다름, R5·R11·R12). `GET /api/v1/repos`: 허용 루트 아래 깊이 2까지의 git 작업 트리, HEAD·현재 브랜치·최근 브랜치 10개. git은 읽기 전용(훅·fsmonitor 끔, 전역·시스템 설정 무시, 상위 폴더로 올라가지 않음)이고, 작업 트리 최상위 = 나열한 경로, git 폴더가 루트 안에 있는 것만 보인다(R8).

**A4. 서버 상태의 클라우드 칸(`ops.cloud`).** Cinder 쿼터(gigabytes, volumes: 한도·사용·예약·무제한), test-vm 서버 상태, 복원 볼륨 수, 최근 복원 기록. 읽기 전용 명령 세 개만(`quota show --usage -f json`, `server show <vm> -f value -c status`, `volume list`), 60초 캐시, 한 번에 하나의 갱신, 갱신 20초 제한, `/ops`는 클라우드를 최대 6초만 기다린다(넘으면 `cloud_timeout`, 갱신은 뒤에서 계속). 실패는 고정 코드 두 개(`cloud_unavailable`, `cloud_timeout`)뿐이다. 이번 작업(A6)에서 마지막 관문을 더했다: API가 공급자의 값을 정규화해 실패면 코드 두 개 중 하나와 빈 블록, 성공이면 코드 없음·상태 단어(아니면 `unknown`)·평범한 VM 이름만 내보낸다. **VM 이름이 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`가 아니면 빈칸이 된다**(실제 이름 `test-vm`은 보이고, 이상한 이름은 화면에 빈 이름으로 나온다). vm 쪽도 CLI 출력에서 상태 단어만 받는다.

**A5. 웹 화면.** `#/new`(저장소·기준 커밋·종류·에이전트를 서버 목록에서 고르고, 네 칸(목표·범위·완료 조건·하지 말 것)을 고정 템플릿으로 합친 프롬프트를 **읽기 전용 미리보기**로 보여 준 그대로 보냄(R9), 64 KiB(UTF-8 바이트) 상한, 보이지 않는 문자·제어 문자 거부, 같은 내용은 같은 `Idempotency-Key`), 작업 화면의 취소·재시도(서버 플래그가 정확히 `true`일 때만 단추, 화면 안 확인 칸을 거쳐 요청 1번), 진행 줄("진행 중 · 단계 · N초 경과"), `#/ops`(쿼터 막대와 80% 경고, 클라우드 네 상태). 서버 문자열은 모두 텍스트 노드.

**A8. 단계마다 누가 옮겼는지 웹에 표시(사용자 결정, 컷오버 전).** `StepTime.Actor`(`actor`, 비면 생략)는 **그 단계로 들어가게 한 이벤트의 주체**다(`Timeline`이 복사). 제출·전이·취소·재시도 이벤트가 각각 단계가 되고, 주체 열이 NULL인 v7 이전 이벤트는 빈 값이다. 웹 작업 화면의 시간 분포 줄 아래에 `처리: <주체>`를 **텍스트 노드**로 보인다: `system` → "자동"(워커가 옮긴 것, 정상), `invalid` → "알 수 없음", 빈 값 → 표시 없음, 그 밖(`session:<ID>`, `token:<ID>`, `socket:uid:<n>`)은 `inline()`(제어·방향 전환·너비 0 문자 치환, 80자 상한)을 거친 그대로. 막대는 여전히 `aria-hidden`이고 주체는 글자 목록 쪽에 있어 스크린 리더가 읽는다. 주체 값을 담는 새 속성은 없고, API에 더한 필드는 `actor` 하나다. 시험: `TestTimelineStepCarriesActor`, `TestTimelineActorNeverEchoesClientInput`(위조 헤더·본문 `actor`를 붙여도 응답의 주체는 서버가 기록한 값뿐, 저장된 이상한 값은 `invalid`), JS `TestTaskTimelineShowsActorPerStep`, `TestTaskTimelineActorHostileText`, 골든 `events.json`·`events-running.json`을 실제 구조체에서 다시 생성. 변이: `Timeline`의 `Actor` 복사 제거 → Go 시험 둘 실패; 웹에서 sanitizer 제거(원문) → 적대 문자 시험 실패; 주체를 `title` 속성에 넣음 → 같은 시험 실패. **차이:** 이벤트별 주체는 이제 웹에서 보이지만 `orch show`/CLI에는 여전히 없다(가장 최근 주체 한 줄뿐). 시험 수(`go test -list`/node): `internal/orch/api` 209 → 211(+2), 웹(node) 106 → 108(+2).

**A6. 적대 시험·변이 시험·빚 정리(이번 작업).** 아래 2·3·5장. 적대 시험이 실제 결함 두 개를 찾아 고쳤다: (1) `encoding/json`이 잘못된 UTF-8을 U+FFFD로 조용히 바꿔 프롬프트 검사가 그것을 볼 수 없었다 → 본문 원문을 먼저 검사해 400; (2) `ops.cloud`가 공급자의 코드 문자열과 CLI 표준 출력의 상태를 그대로 내보냈다 → 정규화(위 A4).

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-09, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0, 커밋 `9d4309c`(코드 마지막 커밋; 이 문서 커밋은 코드를 바꾸지 않음). 세는 방식은 W1(13번 문서 2장)과 같다: 최상위 시험 함수(`Test…`) 수와 하위 시험(`t.Run`) 수를 따로 센다(`go test -json`의 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**. 최상위 시험 함수 927개 중 925 통과, 0 실패, 2 건너뜀(하위 시험 681개 모두 통과). 건너뜀 2개는 W1과 같다: `internal/orch/web` `TestDevServer`(환경 변수로만 켜는 수동 개발 서버), `internal/pathfold` `TestCollisionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사). 표준 오류 출력 없음. 전체 시간 약 3분 10초(벽시계).
- `node --test "internal/orch/web/test/*.test.js"`: **105개 전부 통과**(Task 5 끝의 98개 + 이번 7개).
- **A7(주체별 큐 상한, 결정 R11/R12) 뒤 다시 센 값**(`go test -list '.*' <패키지>`로 센 최상위 시험 수; 위 표는 A6 시점 값이고 아래만 바뀜): `internal/orch/api` 204 → 209(+5: `TestQueueLimitIsPerActor`, `TestQueueLimitTotalCeiling`, `TestQueueLimitRaceNeverExceedsPerActorOrTotal`(하위 2), `TestQueueLimitIgnoresLegacyNullActorTasks`, `TestQueueLimitReplayNotCounted`), `internal/orch/store` 37 → 40(+3: `TestCreateTaskLimitedPerActor`, `…TotalAndOrder`, `…LegacyAndSystemCountTotalOnly`), `cmd/orchd` 29 → 29(`TestSubmitQueueLimitFlagValidation`을 `TestQueueFlagsValidation`으로 바꿈), 웹(node) 105 → 106(+`TestNewViewQueueFullShowsServerSentenceAsText`). `-list` 값은 위 표의 `go test -json` 값과 1 차이가 난다(api: 표 203, 같은 커밋의 `-list` 204)는 방식 차이이므로 증가분만 비교한다.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orchd` | 29 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 55 | 0 | 0 | 76 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 39 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 203 | 0 | 0 | 21 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 53 | 0 | 0 | 17 |
| `internal/orch/store` | 37 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` (Go) | 9 | 0 | 1 (`TestDevServer`, 수동) | 0 |
| `internal/pathfold` | 5 | 0 | 1 (-race만) | 12 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

테스트 파일이 없는 패키지(`internal/approval/ui`, `internal/gittest`)는 표에서 뺐다. W1(13번 문서)과 비교: 최상위 통과 849 → 925, `internal/orch/api` 160 → 203.

이번(A6)에 더한 시험:
- `internal/orch/api/adversarial_w2a_test.go` 8개: `TestSubmitNeedsAuthAndCSRF`, `TestSubmitHostileFields`, `TestForgedActorHeaderIgnored`, `TestQueueFloodBoundedAcrossPrincipals`, `TestCancelRetryOtherTasksUntouched`, `TestReposCannotEscapeRoots`, `TestOpsNeverLeaksCloudErrorText`(하위 시험 10개), `TestIdempotentSubmitAcrossRestartWindow`. 모두 데몬의 실제 리스너(HTTP 리스너는 미들웨어 전체, 또는 소켓)로 보낸다.
- 빚 정리에 붙은 시험: api `TestReposRefusesGitFileWithForeignWorktree`, steps `TestStepsNeverResolveAboveTheRepo`, vm `TestCloudStatusVMStatusIsAWordOnly`, `TestCloudStatusPanicValueIsLoggedNotReturned`; 고쳐 쓴 `TestCancellableFlagMatchesStateTable`, 이름을 바꾼 `TestCancelAndRetryConcurrentInvariants`(옛 `…ConcurrentOneWins`). 웹 7개: `TestOpsUnlimitedFlagNeverWarns`, `TestOpsRestoreWithoutOkIsUnknown`, `TestTaskConfirmEscapeCloses`, `TestTaskConfirmResetWhenFlagFlips`, `TestNewViewSubmitAriaDisabledKeepsFocus`, `TestNewViewProblemIsPoliteAndStable`, `TestNewViewRepoLabelsKeepTheTail`(그리고 기존 `TestTaskCancelRetryButtons`에 "409 뒤 다시 읽음" 단언 3개).

적대 시험이 확인하는 것(요약):
| 시험 | 공격 | 기대(확인함) |
|---|---|---|
| `TestSubmitNeedsAuthAndCSRF` | 자격 없음, 모르는 세션, 쓰레기·Basic·세션을 베어러로, CSRF 없음·틀림, 외부 Origin으로 제출·취소·재시도 | 401/403, 어떤 작업도 바뀌지 않음(상태·갱신 시각·횟수·이벤트·주체 스냅숏 비교), 옛 평면 경로는 HTTP에 없음(405), 대조군 3개 성공 |
| `TestSubmitHostileFields` | `..` 경로, 루트 밖 절대 경로, 상대 경로, 루트 밖을 가리키는 심볼릭 링크, 빈 `.git`(상위 저장소로 올라감, R8), 하위 폴더, `base`=`--upload-pack=x`/`-x`/공백 포함/NUL, 64 KiB+1 프롬프트, NUL, 원문 잘못된 UTF-8, 33자 종류, 정책에 없는 32자·일반 종류, 줄바꿈·대문자 종류, 허용되지 않은 에이전트, 모르는 필드 | 모두 400/403 고정 문장, 바깥 저장소의 SHA·브랜치·경로가 응답에 없음, 작업 0개. 대조군: 정확히 64 KiB와 정책에 있는 종류는 201. 이스케이프된 외톨이 서로게이트(`\ud800`)는 Go가 U+FFFD로 풀어 **받는다**(기록된 동작) |
| `TestForgedActorHeaderIgnored` | `X-Actor`, `X-Orch-Actor`, `X-Forwarded-User`, `X-Remote-User`, `Remote-User`, `X-Webauth-User`, `Tailscale-User-Login`/`-Name`, `X-Principal` 헤더를 세션·토큰(HTTP)과 소켓 요청에 붙여 제출·취소·재시도; 본문에 `actor`/`Actor`/`via`/`principal` | 이벤트의 주체, `TaskView.actor`, 원장 `task_submitted`·`task_transition`의 `actor`=`via`가 모두 실제 principal. 원장 파일 어디에도 위조 값이 없음, `ledger.Verify` 통과. 본문 필드는 무시가 아니라 **400**(모르는 필드 거부)이고 작업이 생기지 않음 |
| `TestQueueFloodBoundedAcrossPrincipals` | 주체별 상한 5·전체 상한 12에서 4 주체(세션 2, 토큰, 소켓)가 키 있는·없는 제출 40개를 동시에 | 정확히 12개 201(전체 상한), 어떤 주체도 5개를 넘지 않음, 나머지는 모두 `429 queue_full`(고정 두 문장 중 하나), 다른 응답 없음. 취소 1개 뒤 한도 아래인 주체가 정확히 1개 더, 그 뒤는 전체 문장 |
| `TestCancelRetryOtherTasksUntouched` | 끝난·판정 실패·진행 중 작업에 취소/재시도, 없는 ID, `0`, `-1`, `abc`, `1.5`, `1e3`, `1,2`, int64 넘침 | 정해진 코드(409/404/400), 어떤 작업도 바뀌지 않음. 정상 취소·재시도는 대상 하나만 바꾸고 주체를 남김 |
| `TestReposCannotEscapeRoots` | 루트 밖 저장소·`/`·`..`·이름이 접두사인 형제 루트로 가는 심볼릭 링크, 링크 고리, 빈 `.git`, 밖을 가리키는 gitfile(절대·상대), 깊이 3, `?root=/`·`?path=..` 같은 질의 | 목록은 정상 저장소 하나뿐, 비밀 SHA·브랜치·경로 문자열이 응답에 없음, 세션 없이는 401 |
| `TestOpsNeverLeaksCloudErrorText` | 공급자가 코드에 글자를 넣음, 성공인데 코드 글자, 상태·이름에 글자, 실패인데 블록 있음; 실제 `vm.TestVM`에 오류·비 JSON 쿼터·서버 오류·CLI 잡음 상태·여러 줄 상태를 내는 가짜 클라우드 | 호스트 이름·비밀번호·Traceback·`ERROR:`가 응답에 없음, 실패는 코드 두 개 중 하나에 블록 없음, 성공의 상태는 단어나 `unknown` |
| `TestIdempotentSubmitAcrossRestartWindow` | 키 있는 제출 후 데몬을 완전히 내리고(응답 유실 상황) 같은 DB로 새 데몬을 띄워 같은 키·같은 본문 재전송 | 200 `Idempotent-Replay: true`, **같은 작업 번호**, 작업 1개. 같은 키·다른 본문은 409. 같은 키라도 다른 주체(토큰)는 별개 제출(201), 그 주체의 재전송은 그 주체의 작업 |

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 커밋한 코드를 `git worktree add --detach`로 작업 디렉터리 밖(`$CLAUDE_JOB_DIR/tmp`)에 따로 꺼내 한 곳만 망가뜨리고, 해당 패키지 시험만 돌렸다(동시성 변이는 `-race -count=3`). 변이마다 `git checkout -- .`로 되돌렸고, 끝난 뒤 사본을 지웠다(`git worktree list`에 작업 트리 하나만 남음 확인). 웹은 `node --test --test-force-exit`. 전부 구현자 실행이다(이 작업의 검토자는 아직 재현하지 않음).

| # | 망가뜨린 곳 | 잡은 시험(이번에 추가한 것은 굵게) |
|---|---|---|
| ①(R11 이전, 전역 상한) | 큐 상한 검사 제거(`CreateTaskLimited`) | **TestQueueFloodBoundedAcrossPrincipals**, TestSubmitQueueFull429, TestQueueLimitRaceNeverExceeds, TestQueueLimitCountsOnlyQueued, store TestCreateTaskLimitedConcurrent·…CountsOnlyQueued |
| ② | 종류 대조 제거 | **TestSubmitHostileFields**, TestSubmitUnknownClassRefused |
| ③ | 주체 전달 제거(항상 `system`) | **TestForgedActorHeaderIgnored**, **TestCancelRetryOtherTasksUntouched**, TestEveryWriteHandlerPassesPrincipal(하위 6), TestLedgerSubmittedEventHasActor |
| ④ | `state_changed` 매핑 제거(500으로 복귀) | TestCancelRaceLostIsStateChanged409NotTheGeneric500, TestRetryStateChanged409 |
| ⑤ | `/repos` 후보의 허용 루트 검사 제거 | TestReposListsOnlyUnderAllowedRoots. **`TestReposCannotEscapeRoots`는 잡지 못했다**: R8의 "git 폴더도 루트 안" 검사가 같은 바깥 저장소를 막기 때문(겹친 방어). 잡히므로 시험은 더하지 않았다 |
| ⑥ | 클라우드 오류 원문 노출(vm이 `qErr.Error()`를 코드로, `cloudSupplier`가 코드를 그대로) | vm TestCloudStatusFailureIsCodeOnly·…ReaderPanicIsUnavailable·…StaleIsNeverServedAsFresh, orchd TestCloudSupplierMapsAndHidesText. **API 수준 시험은 통과**한다: 이번에 더한 API 정규화가 원문을 막기 때문(의도한 이중 방어) |
| ⑥b | API 정규화(`normalizeCloud`) 제거 | **TestOpsNeverLeaksCloudErrorText**(하위 7) |
| ⑦ | 웹 `Idempotency-Key` 제거(`new.js`) | TestNewViewSubmitsOnceWithIdempotencyKey, "same content keeps the key…" |
| ⑧ | `CreateTaskLimited`의 세기와 삽입을 다른 트랜잭션으로 | **TestQueueFloodBoundedAcrossPrincipals**, TestQueueLimitRaceNeverExceeds, store TestCreateTaskLimitedConcurrent(`-race -count=3`) |
| ⑨ | `/repos`의 git 천장(`GIT_CEILING_DIRECTORIES`) 제거 | gitx TestDiscoveryNeverWalksAboveRepo. **API 수준에서는 동등 변이**(잡히지 않음): 최상위 = 경로 검사가 위로 올라간 결과를 따로 거부한다(Task 3에서 이미 관찰). 관찰할 수 없는 것을 억지로 시험하지 않았다 |
| ⑩ | `/ops`가 예산(6초) 없이 클라우드를 기다림 | TestOpsReturnsWithinBudgetWhenCloudHangs |
| ⑪ | `RetryFailed`가 주체를 버림(`system`) | **TestForgedActorHeaderIgnored**, **TestCancelRetryOtherTasksUntouched**, store TestEventsRecordActorFromContext, TestEveryWriteHandlerPassesPrincipal(하위 3) |
| ⑫ | `api.js`가 `opts.headers`의 아무 헤더나 통과 | "opts.headers: anything but a well-formed Idempotency-Key on a POST is refused before any request" |
| ⑬ | 본문 원문 UTF-8 검사 제거 | **TestSubmitHostileFields** |
| ⑭ | vm의 상태 단어 검사 제거 | **TestCloudStatusVMStatusIsAWordOnly** |
| ⑮ | `ownRepo`의 최상위 = 경로 검사 제거 | **TestReposRefusesGitFileWithForeignWorktree** |
| ⑮b | 제출 `validate`의 최상위 = 경로 검사 제거 | **TestReposRefusesGitFileWithForeignWorktree**(제출 부분). TestSubmitRefusesEmptyGitDirWalkUp은 천장이 따로 막아 잡지 못함 |
| ⑯ | 단계(steps)가 다시 천장 없는 `gitx.Resolve` | **TestStepsNeverResolveAboveTheRepo** |
| ⑰ | 플래그가 사라져도 열린 확인 칸 상태 유지 | **TestTaskConfirmResetWhenFlagFlips** |
| ⑱ | Escape 처리 제거 | **TestTaskConfirmEscapeCloses** |
| ⑲ | 문제 줄을 입력마다 다시 씀 / `role=alert`로 되돌림 | **TestNewViewProblemIsPoliteAndStable**(두 변이 모두) |
| ⑳ | 저장소 이름을 끝에서 120자로 자름 | **TestNewViewRepoLabelsKeepTheTail** |
| ㉑ | 제출 단추에 진짜 `disabled` | **TestNewViewSubmitAriaDisabledKeepsFocus**, TestNewViewSubmitsOnceWithIdempotencyKey |
| ㉒ | `ok` 없는 복원 기록을 실패로 | **TestOpsRestoreWithoutOkIsUnknown** |
| ㉓ | 복구한 panic 값 서버 로그 제거 | **TestCloudStatusPanicValueIsLoggedNotReturned** |
| ㉕ | (검토 수정 1회차) 소켓 인증이 `X-Actor`/`X-Principal` 헤더로 주체를 정함 | **TestForgedActorHeaderIgnored**(소켓 행 추가 후. 그 전에는 잡히지 않았음 — 검토자가 찾음) |
| ㉔ | `cancellable` = `state != done` / `Cancel`의 종결 검사 = `state == done` | **TestCancellableFlagMatchesStateTable**(두 변이 모두) |
| A7-㉕ | 주체별 검사 제거(`perActor` 검사를 건너뜀) | **TestQueueLimitIsPerActor**(api) |
| A7-㉖ | 전체 검사 제거 | **TestQueueLimitTotalCeiling**(api) |
| A7-㉗ | 개수 세기와 삽입을 서로 다른 트랜잭션으로 | **TestQueueLimitRaceNeverExceedsPerActorOrTotal**(두 설정 모두), store TestCreateTaskLimitedConcurrent(`-race -count=3`). 복사본(`/tmp`, 작업 트리 밖)에서 실행, A7 구현자 실행 |

결과: 브리프의 12개(①~⑫) 모두 잡혔다(12/12, 잡히지 않은 변이 없음). 다만 ⑤·⑥·⑨는 **새 적대 시험이 아니라 기존·하위 패키지 시험이 잡았고**, 새 API 시험이 통과한 이유는 겹친 방어 때문이다(각 행 참고). 추가 변이 15개(⑥b, ⑬~㉔, ⑮b, ⑲·㉔의 두 형태)도 모두 잡혔다. 처음 돌렸을 때 ⑰이 실패한 채 폴링 타이머가 남아 node가 끝나지 않았다(시험이 실패하면 `ctx.leave()`가 불리지 않음) → 새 시험 두 개를 `t.after`로 정리하도록 고치고(커밋 `9d4309c`) 다시 돌려 잡힘을 확인했다.

## 4. 설계서와 달라진 곳 (결정 R1~R9, R11, R12)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| R1 | `machine.WithOnTransitionCtx`를 더해(옛 옵션 유지) 원장 기록이 요청의 ctx에서 주체를 읽음 | 기계에 옵션 하나 더, 쉽게 제거 |
| R2 | `approval_granted.via`를 `socket` 같은 종류에서 `kind:id`로, 기존 단언 수정 | `via`의 종류만 기대하는 옛 원장 독자는 첫 `:`에서 잘라야 함(이 저장소엔 없음) |
| R3 | 종류 검사는 정책이 있고 종류 목록이 비어 있지 않을 때만 | 종류 0개 정책은 아무 종류나 받음(전과 같음) |
| R4 | `ops.cloud`·`TaskView`의 JSON 키를 snake_case로 미리 고정 | 두 곳에서 이름 변경 |
| R5 | 큐 상한 검사와 삽입을 store의 한 트랜잭션(`CreateTaskLimited`)으로, 키 없는 제출 포함 | store API 하나 더 |
| R11 | 큐 상한을 주체별(기본 20)로 바꾸고 서버 전체 상한(기본 100)을 따로 둠. `CreateTaskLimited(ctx, t, perActor, total)`, 오류 둘(`ErrQueueFull`, `ErrQueueFullTotal`), 429 코드는 같고 고정 문장만 다름, 웹은 서버 문장을 텍스트로 보여 줌 | `--queue-limit`의 뜻이 바뀜(전역 → 주체별) |
| R12 | 주체별 수는 가장 이른 `created` 이벤트의 `actor`로 센다(스키마 변경 없음). `system`·`invalid`·`actor` 없는 옛 작업은 전체 수에만 센다 | 옛 작업이 주체별 한도를 못 채움(의도) |
| R6 | 모든 원장 payload에서 `via` = `actor` = `kind:id` | payload마다 한 줄 |
| R7 | 동시 취소·재시도 시험은 "정확히 하나 200"이 아니라 실제 불변식(재시도 항상 200, 취소는 최종 상태가 cancelled일 때만 200, 아니면 409 `already_final`, 5xx 없음) | "한 승자"보다 약하지만 상태 기계와 맞음 |
| R8 | `/repos`는 천장 + 최상위 = 경로 + git 폴더·공통 폴더가 루트 안; 제출은 천장 + 최상위만(연결된 worktree 허용) | 루트 밖 주 저장소를 가리키는 gitfile은 제출 가능(그 git 폴더를 읽음) |
| R9 | 제출 화면의 최종 프롬프트는 읽기 전용(네 칸으로만 편집) | 편집 가능한 칸을 나중에 더하면 "클릭 때 다시 조립해 비교"를 바꿔야 함 |

그 밖에 문서에 남길 차이·동작 변화:
- **이스케이프된 외톨이 서로게이트(`\ud800`)는 받는다(문서화된 수용 동작).** Go의 JSON 해독이 U+FFFD로 바꾸므로 저장되는 값은 올바른 UTF-8이고 서로게이트는 남지 않는다. 웹 화면은 이런 입력을 보내기 전에 거부한다(`findHostile`). `TestSubmitHostileFields`가 이 동작(201, 저장값 `a\uFFFDb`)을 고정한다. 원문 바이트의 잘못된 UTF-8은 400이다.
- **재시도의 `state_changed`는 매핑만 보장한다.** API로는 사실상 닿지 않는다(재시도 경쟁의 패자는 움직인 작업을 보고 `not_retryable`이 된다). `TestRetryStateChanged409`는 매핑을 직접 확인한다.
- **비 VM `orchd`(placeholder 단계)는 `task_submitted` 원장 이벤트를 쓰지 않는다**(원장 자체가 없음, `task_transition`도 없음). SQLite 이벤트의 주체는 남는다.
- **CLI `orch submit`도 종류 검사를 받는다**(같은 `validate`). 정책이 종류를 나열하면 목록 밖 종류는 이제 거부된다(전에는 `default_level`로 받았음).
- **이미 끝난 작업의 취소는 `already_final`**(전에는 `conflict`).
- 타임라인에서 `queued` 단계는 더 이상 흐르는 타이머를 보이지 않는다("대기 중").
- 웹 `api.js`는 `queue_full`(429)도 기본 대기 뒤 한 번 다시 보낸다(요청 하나 더, 무해).
- 페이지를 새로 고치면 멱등 키 지도가 사라진다(브라우저 저장소 금지). 응답을 잃은 뒤 **새로 고침하고** 다시 제출하면 중복이 생길 수 있다. 화면 안 재시도는 안전하다.
- 서버의 멱등 기록은 작업 삽입과 **다른 트랜잭션**이다. 삽입 뒤 기록 전에 프로세스가 죽으면 그 키의 재전송은 새 작업을 만든다(창은 짧음; 재시작 시험은 기록이 끝난 경우만 확인).
- `/repos`는 한 저장소가 자기 5초 제한을 넘으면 조용히 빼고, 요청 전체 15초가 끝날 때만 503이다(부분 목록 대신).
- **종류 검사는 멱등 재생 조회보다 먼저 돈다**(`validate`가 먼저, 재생 조회는 그 뒤). 그래서 이미 처리된 키의 재전송이라도 그 사이 정책에서 그 종류가 빠졌으면 기록된 작업이 아니라 `400 unknown_class`를 받는다.
- `viaOf`(승인 `via`)는 principal이 없으면 `unknown`으로, `ActorFrom`(이벤트 주체)은 `system`으로 떨어진다. 모든 경로가 `authenticate` 뒤에 있어 닿지 않는 분기다.
- 서버 문자열이 **속성**으로 들어가는 곳이 웹에 둘 있다: `<option value=repo.path>`와 `data-base=branch`. 둘 다 모양을 먼저 검사하고(`safePath`/`BASE_RE`) `setAttribute`로만 넣는다(파싱 없음). "텍스트 노드만"의 문자 그대로의 예외다.
- `ops.cloud.history`는 클라우드 호출이 실패하면 `null`이 아니라 빈 배열이다.
- 큐 상한을 처음에는 전역 20개로 만들었으나 한 세션이 큐를 독점할 수 있어 **주체별 20개 + 서버 전체 100개**로 바꿨다(결정 R11/R12, 컷오버 전). 남는 한계: 주체 수만큼 곱해진 양(주체당 20개, 전체 100개 안)은 쓸 수 있다.

## 5. 갚은 빚 (Task 1~5 검토에서 미룬 것)

| 출처 | 빚 | 결과 |
|---|---|---|
| Task 1 | 비 VM orchd는 `task_submitted`를 쓰지 않음 | **문서화**(4장). 코드 변경 없음(원장이 없는 구성) |
| Task 2 (1) | `TestCancellableFlagMatchesStateTable`이 구현식을 거의 그대로 비춤 | **완료**: 상태 12개마다 작업을 심고 `machine.Cancel`을 실제로 불러 플래그와 대조(종결이면 `ErrTerminal`), `Allowed`에 모든 상태 상수의 행이 있는지 확인. 변이 ㉔ 두 형태가 잡힘 |
| Task 2 (2) | 시험 이름, 고루틴 안 `t.Fatal` | **완료**: `TestCancelAndRetryConcurrentInvariants`, 결과를 모아 `Wait` 뒤 확인 |
| Task 2 (3) | 재시도 `state_changed`는 매핑만 | **문서화**(4장) |
| Task 3 (1) | `ownRepo`의 최상위 = 경로 검사를 아무 시험도 고정하지 않음 | **완료**: `TestReposRefusesGitFileWithForeignWorktree`(루트 안 gitfile → `core.worktree`가 루트 안의 다른 폴더). 변이 ⑮·⑮b가 잡힘 |
| Task 3 (2) | `GIT_CEILING_DIRECTORIES`는 콜론 구분 | **완료**: `ceilingEnv` 주석(콜론이 든 상위 경로면 천장이 무력, 최상위 검사가 겹쳐 막음) |
| Task 3 (3) | `/repos` 주석이 시간 초과 동작을 잘못 설명 | **완료**: 저장소별 5초 초과는 조용히 빠지고 요청 기한만 503 |
| Task 3 (b) | 단계(steps)가 천장 없는 `gitx.Resolve` | **완료**: 세 곳을 `gitx.ResolveAt`으로. `TestStepsNeverResolveAboveTheRepo`(바깥 저장소가 같은 커밋을 가져도 prepare·collect가 `resolve base`로 실패, 에이전트 미실행). 게이트(`internal/gate`)의 `Resolve`는 하위 폴더 경로를 받는 다른 호출자라 그대로 둠 |
| Task 4 | 복구한 panic 값 서버 로그 | **완료**: `log.Printf`(응답에는 없음), `TestCloudStatusPanicValueIsLoggedNotReturned` |
| Task 4 | info_test 준비 플래그 실패 문구 | **완료**: 전용 `t.Fatal` 문구 |
| Task 5 (1) | 409 `not_retryable`·`already_final` 뒤 다시 읽는지 단언 | **완료**: 이벤트 조회 횟수 단언 3개 |
| Task 5 (2) | `{limit:300,in_use:299,unlimited:true}` 경고 없음 단언 | **완료**: `TestOpsUnlimitedFlagNeverWarns` |
| Task 5 (4) | 제출 단추의 진짜 `disabled` | **완료**: `aria-disabled` + 처리기 가드, 누른 단추에 포커스 유지 |
| Task 5 (5) | 문제 줄이 `role=alert`이고 입력마다 다시 채워짐 | **완료**: `aria-live=polite`, 문구가 바뀔 때만 다시 씀 |
| Task 5 (6) | Escape로 확인 칸 닫기, 플래그가 사라지면 열린 상태 잊기, 120자 이름 모호, `ok` 없는 복원 기록 | **완료**: 각각 시험 1개(2장 목록) |
| 알려진 불안정 시험 | `TestOverviewCounts`, `TestRetryAPIConcurrentDouble` | 이번 전체 실행에서 실패 없음(`-race` 전체 1회. 반복 실행(`-count=N`)은 이번에 하지 않았다). 원인은 W1에서 이미 고침(작업자 일시 정지, 유휴 연결 닫기) |

## 6. 실서버에서 확인하지 못한 것

**아래는 컷오버 전 상태의 기록이다. 2026-11-09 컷오버 1단계에서 확인된 것은 7장에 있고, 그에 따라 여기 항목 중 일부는 해소되었다(해당 줄 끝에 표시).** 특히:
- **실제 `openstack quota show --usage -f json`의 키 이름.** `Resource`, `In Use`, `Limit`, `Reserved`는 이 저장소의 기존 고정 데이터(`vm/prune_test.go` 등)에서 가져왔고 실제 CLI 출력과 대조한 적이 없다. 다르면 `#/ops`가 늘 `cloud_unavailable`이 된다(런북 10장 2-8에서 사용자가 대조). `server show … -f value -c status`의 실제 출력도 같다. **→ 해소(7장): 실제 키는 그대로였고 상태는 `ACTIVE`.**
- **실제 브라우저에서 `#/new`의 키보드 전체 흐름과 스크린 리더 낭독.** DOM 대역 시험(`aria-disabled`, `aria-live`, Escape)만 했다. 이번 작업에서 화면을 다시 보지 않았다(Task 5의 ego-browser 확인은 이 수정 이전).
- **다른 사용자 소유 저장소의 `/repos`.** `/repos`는 전역 `safe.directory`를 읽지 않으므로 orch 소유가 아닌 저장소는 빠질 것으로 예상한다(코드 읽기). orch-vm의 실제 소유자(`ls -ld /home/orch/repos/*`)는 보지 않았다. **→ 해소(7장): 모두 orch 소유, 전역 git 설정 없이도 열림.**
- **실제 `policy.yml`과의 종류 대조.** 정책에 `toy`, `sample-app`가 있다는 것은 운영 문서 3장의 기록일 뿐, 실제 파일을 이번에 읽지 않았다. CLI 제출의 동작 변화도 실서버에서 보지 않았다. **→ 부분 해소(7장): 실제 정책 파일을 읽어 종류가 `toy`·`sample-app`뿐임을 확인했다. 미등록 종류 거부는 실서버에서 시험하지 않았다.**
- **`주체:`/`TaskView.actor` 기대값은 코드를 읽고 도출했다**(최근 이벤트의 주체; 정상 서버에서는 곧 `system`). 실서버에서 본 적 없다. 런북은 웹 타임라인의 단계별 주체(첫 단계 = 제출자, `cancelled` 단계 = 취소자)를 1차로, 원장 `task_submitted`를 보조로 쓰게 했다. 단계별 주체 화면은 합성 데이터와 ego-browser(1280·390px)로만 봤고 실서버에서는 보지 않았다.
- **실제 부하에서의 큐 상한.** 시험 DB(SQLite, `_txlock=immediate`)에서의 동시 제출만 확인했다.
- **실제 인프라 실패의 재시도.** 가짜로 만든 실패 작업(`failedTask`)으로만 시험했다. 재시도 확인 문구는 `machine.Retry` 주석에서 가져왔다.
- **스키마 v7 마이그레이션을 실제 운영 DB로.** 시험 DB(v6→v7)로만 확인했다. **→ 해소(7장): 운영 DB v6→v7 성공.** 옛 `orchd`가 v7 DB를 거부하는 것은 코드(`store.go`의 버전 검사)를 읽어 확인했다.
- **`tailscale serve`가 클라이언트의 `Tailscale-User-*` 헤더를 지우는지**(W1부터 남은 것). orchd는 주체를 그 헤더에서 읽지 않으므로(적대 시험으로 확인) 주체에는 영향이 없다.
- **ego-browser 시각 확인은 이번 작업에서 하지 않았다**(바뀐 것은 단추 속성·live 영역·키 처리·이름 자르기·칩 하나).

## 7. 컷오버 실행 기록 (2026-11-09, orch-vm)

`10-OPERATIONS-orchestrator.md` 10장 단계 1을 실제 서버에서 실행했다(`main` 10bd046, 작업 7·8까지 포함). 단계 2(웹 확인)는 **사용자의 브라우저 확인이 일부만 끝났다**.

### 단계 1 (백업·배포·재시작): 완료
- 시작 조건 확인: 진행·대기 작업 0, 승인 대기 0, 스키마 v6, `backup-w2a` 없음, VM `x86_64`, Mac–VM 시계 차이 3초.
- Mac에서 만든 세 바이너리의 체크섬이 VM에 복사된 것과 일치.
- 백업: `/usr/local/lib/aigate/*.prev-w2a`(root 소유 0755), `/home/orch/orchd/backup-w2a`(orchd.db — 스키마 v6, 작업 42건 확인 —, gate-home), `/etc/systemd/system/orchd.service.pre-w2a`.
- 배포 후 시작은 오류 없이 성공. 서명 모드 유지(서명자 3명), 소켓 허용 uid `[0 1000]`, 웹 창구 정상, `healthz` 정상.
- **운영 DB가 v7로 올라감:** `schema_version` 7, `task_events.actor` 열이 생겼고 기존 이벤트 390건은 모두 NULL(v7 이전). 작업 목록 보존(완료 40, 실패 2, 마지막 번호 42).
- 시험 작업 #43(toy, 소켓 제출): 제출부터 `done`까지 약 80초. 첫 이벤트 주체 `socket:uid:1000`, 이후 7개 이벤트 모두 `system`, 원장 `task_submitted`의 `actor`·`via`가 `socket:uid:1000`. `orch show`의 `주체:`는 `system`(마지막 이벤트)으로, 정정된 런북의 설명과 일치.

### 서버에서 직접 확인한 항목 (사용자의 브라우저 없이)
| 항목 | 확인한 방법 | 결과 |
|---|---|---|
| 실제 `openstack quota show --usage -f json`의 키 이름 | orch 계정으로 읽기 전용 조회 | 항목 키가 `Resource`, `Limit`, `In Use`, `Reserved` — 가정과 같음 |
| `ops.cloud` 쿼터 수치 | `/api/v1/ops`를 소켓으로 호출해 CLI와 비교 | gigabytes 한도 300·사용 150, volumes 한도 20·사용 5 — **CLI와 일치** |
| 테스트 VM 상태 | `server show test-vm -f value -c status`와 비교 | `ACTIVE` — 일치 |
| `restore_volumes` | `volume list` 이름과 비교 | 5개 볼륨 중 `test-vm-root-…` 형식 2개 — 일치(나머지 3개는 `agent-vm-root`, `orch-vm-root`, `k8s-migration-backup-30g`) |
| `ops` 응답 시간 | 소켓 호출 | 캐시가 따뜻할 때 약 0.5ms |
| 정책 파일 | 읽기 | 종류 `toy`·`sample-app`(둘 다 L1), `default_level L0`, `repo_roots [/home/orch/repos]` |
| 저장소 소유자 | `ls -ld`, `git rev-parse`(전역 설정 없이) | `sample-app`·`toy`는 orch 소유이고 열림. `remote.git`은 bare라 목록에 안 나옴(정상) |

### 웹에서 사용자가 제출한 작업 #44 (실패)
- 웹에서 패스키로 제출한 작업. 요청 내용은 `Goal: 계산기 기능 생성` 한 줄뿐이었다.
- 에이전트가 오류 없이(종료 코드 0) 끝났지만 커밋을 만들지 않아 `agent made no commit`으로 `failed`. 인프라 오류가 아니라 판정 실패(요청이 모호)이므로 재시도 대상이 아니다.
- **확인된 것:** 웹 제출의 첫 이벤트 주체가 `session:mac-passkey`로 기록되었다(런북 2-3의 예상과 일치). 이후 이벤트는 `system`. 서버 로그에 오류 없음.
- **교훈:** 런북 2-2의 "목표 칸만 적고 제출"은 구체적이지 않은 요청이라 에이전트가 아무것도 만들지 않는다. 런북 예시를 구체적인 요청으로 고쳤다. (W2b의 "한 줄 → AI 계획서" 기능이 이 문제를 푸는 것이다.)

### 사용자가 브라우저로 확인한 것 (2026-11-09 17:20대, 사용자가 화면 내용을 붙여 넣어 알려 줌)
- **작업 화면의 시간 분포 타임라인(작업 #44):** 첫 단계 `대기`의 `처리:`가 `session:mac-passkey`, 나머지 `준비`·`에이전트`·`수집`·`실패`가 모두 `처리: 자동`. 서버의 이벤트 기록(`session:mac-passkey` 다음 `system` 연속)과 일치. `head 커밋이 아직 없어 diff를 볼 수 없습니다.` 안내도 표시됨(커밋 없는 작업이라 맞는 안내).
- **서버 상태 화면:** 볼륨 용량 150 / 300 GB (50%), 볼륨 개수 5 / 20 (25%), 테스트 VM `test-vm · ACTIVE`, 복원 볼륨 2개, 확인 시각 11-09 17:23, 최근 복원 기록 성공 11-09 17:09 · 52초(단계별 시간 포함). 위 "서버에서 직접 확인한 항목"의 값과 모두 일치. 80% 경고 기준에 닿지 않아 경고가 없는 것도 맞음.
- **새 작업 화면의 저장소 목록:** `toy`와 `sample-app` 둘 다 보임(서버에서 확인한 `/repos` 예상과 일치).
- **#44 화면의 "다시 시도" 버튼:** 붙여 넣은 화면 글에 "다시 시도"가 없었다. 판정 실패(커밋 없음)이므로 없어야 정상이고 예상과 일치. (복사한 글에 버튼 이름이 항상 포함되는지는 확인하지 못했으므로 "예상과 일치"로만 기록.)

- **웹에서의 취소(작업 #45, 2026-11-09 17:24):** 사용자가 웹에서 진행 중인 작업을 취소해 확인했다. 화면: `취소를 요청했습니다. 화면을 새로 고쳤습니다.`, 타임라인 `대기` `처리: session:mac-passkey` → `준비`·`에이전트` `처리: 자동` → `취소` `처리: session:mac-passkey`. 서버 기록과 일치: DB 이벤트 `created`(session:mac-passkey) → `started`·`agent vm prepared`(system) → `agent_running→cancelled`(session:mac-passkey), 원장 `task_submitted`와 `task_transition`(`agent_running→cancelled`, note `cancelled`)의 `actor`·`via`가 모두 `session:mac-passkey`. 원장의 해시 사슬 검증은 하지 않았다(줄 수 407만 봄). **취소 전 확인 칸:** 사용자가 "확인 칸이 떴다, 취소를 물어보는"이라고 직접 알려 왔다(붙여 넣은 화면에는 나타나지 않아 화면 증거는 없고 사용자 보고로 기록). 확인 전에는 요청이 나가지 않는다는 것은 코드 시험으로 확인됨.

### 컷오버에서 확인하지 않은 것 (닫을 때의 상태)
- `#/new`의 키보드 전체 흐름·스크린 리더, 구체적인 요청의 웹 제출이 `done`까지 가는 것(웹 제출의 기록 경로는 #44·#45가 확인해 줌).

### 끝내 확인하지 못한 것
- **재시도**(인프라 오류로 실패한 작업이 있어야 시험 가능, 일부러 만들지 않음).
- **큐 상한**(실제 부하): 자동 시험만.
- **정책에 없는 종류의 제출 거부**를 실서버에서 시험하지 않음(시험 코드와 정책 파일 읽기로만).
