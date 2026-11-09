# 웹 콘솔 W2a 구현 계획 (비밀 없음)

> **에이전트 작업자용:** 필수 하위 스킬은 superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans다. 단계는 체크박스(`- [ ]`)로 추적한다.

**목표:** 웹에서 작업을 제출·취소·재시도하고, 서버 상태(VM·쿼터)와 진행 상황을 보고, 모든 쓰기에 "누가 했는지"를 기록한다. 새 비밀은 없다.

**구조:** 기존 API v1 위에 읽기 API 하나(`GET /repos`)를 더하고 `ops`를 확장한다. 주체는 요청 컨텍스트로 `store`까지 전달해 `task_events.actor`(스키마 v7)와 원장에 남긴다. 웹은 W1의 뼈대(`registerView`, `el`, `api.js`, `poll`) 위에 제출 화면·서버 상태 화면·작업 버튼을 추가한다.

**기술:** Go(`internal/orch/{api,store,machine,vm}`, `cmd/orchd`), 바닐라 JS(`internal/orch/web/static`), `node --test`.

**설계서:** `docs/14-DESIGN-webconsole-W2.md` 3장(W2a). 이 계획은 설계서가 말하는 것을 작업으로 나눈 것이다. 설계서와 충돌하면 설계서가 이긴다.

## 전역 제약 (모든 작업에 적용)
- 한국어 주석·오류 문구는 기존 파일의 방식을 따른다. 코드 식별자·명령은 영어 그대로.
- 서버 응답과 원장에는 서명·레지스트리·승인·외부 명령 오류의 `Error()` 원문을 넣지 않는다. 고정 코드와 고정 문구만(W1 CF1).
- 웹: 인라인 스크립트·스타일 금지(CSP), 모든 서버 문자열은 `el()`의 텍스트 노드로, `innerHTML` 등 위험 API 금지(정적 검사 `TestNoForbiddenSinksInStaticJS`가 막는다), 동적 너비는 CSSOM(`element.style.x`), `href`는 `#/`+안전 토큰만, `id/for/name`은 `SAFE_TOKEN_RE`.
- 새 쓰기 엔드포인트는 `requireWrite`(세션이면 CSRF) 뒤에 둔다. 서명은 요구하지 않는다(승인만 서명).
- 기존 테스트의 의미를 약화하지 않는다. 바꿔야 하면 이유를 보고서에 쓴다.
- 커밋 메시지 끝: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- 실서버에는 아무것도 하지 않는다(컷오버는 A6 이후 사용자 승인으로 별도).
- `rg`가 없으므로 검색은 `grep -rn`.
- 검증 명령 기본: `go vet ./...`, `gofmt -l .`(비어 있어야 함), `GOOS=darwin go vet ./cmd/... ./internal/...`, `go test ./internal/orch/... ./cmd/... -count=1 -race`, `node --test "internal/orch/web/test/*.test.js"`. 전체 `go test ./... -count=1 -race`는 A6에서(약 5분).

## 검토 초점 (설계서가 암시하지만 개별 작업 시험이 안 다루기 쉬운 것, 가능성 순)
1. 큐 상한 검사와 작업 생성 사이의 경쟁: 두 요청이 동시에 한계 직전에 들어오면 상한을 넘겨도 되는가(→ A3의 동시성 시험).
2. 취소·재시도가 동시에 같은 작업에 오면 어느 하나만 이기고 나머지는 409여야 한다(→ A2).
3. 주체가 `system`으로 잘못 기록되는 경로(핸들러가 컨텍스트를 안 넘기는 경우)가 없어야 한다(→ A1: 모든 쓰기 핸들러 표 시험).
4. 쿼터 조회 실패·지연이 `/ops` 전체를 막으면 안 된다(→ A4: 시간 제한과 부분 응답).
5. 저장소 목록이 허용 루트 밖의 경로·심볼릭 링크를 노출하면 안 된다(→ A3).

---

### Task 1 (A1): 스키마 v7과 주체 기록

**파일:**
- 만들기: `internal/orch/store/schema_v7.sql`, `internal/orch/store/actor.go`(+ `actor_test.go`)
- 수정: `internal/orch/store/store.go`(마이그레이션 7 등록, 이벤트 삽입 4곳, `Event`에 `Actor`), `internal/orch/api/api.go`·`grant.go`(컨텍스트에 주체를 싣는 지점), `cmd/orchd`(원장 쓰는 곳은 `internal/orch/steps/steps.go`의 `recordTransition`), `internal/ledger` 호출부, `cmd/orch/cli.go`(`show`가 주체를 보여 줌)

**인터페이스:**
- 소비: `api.Principal{ID, Kind}`, `PrincipalFrom(ctx)`(auth.go), `store.CreateTask`, `Store.transitionTx`, `Store.RetryFailed`, `Store.ResolveAndAdvance`, `Store.ReleaseClaims`, `machine.WithOnTransition`(콜백 시그니처 `func(t store.Task, from, to State, note string)`는 **바꾸지 않는다**).
- 생산: `store.WithActor(ctx, actor string) context.Context`, `store.ActorFrom(ctx) string`(없으면 `"system"`), `Event.Actor string`, `TaskView.Actor string`(`json:"actor,omitempty"`, 작업의 마지막 이벤트의 주체). 형식 `kind:id`: `session:mac-passkey`, `token:mac-ssh`, `socket:uid:1000`, 시스템은 `system`. 원장 `task_transition` 페이로드와 새 `task_submitted` 이벤트에 `actor`, `via`를 추가한다. `via`는 `actor`와 같은 값 `kind:id`다(결정 R6). `approval_granted`의 `via`도 같은 형식으로 통일한다.

- [ ] **시험(이름 그대로):** `TestMigrateV6ToV7KeepsData`(v6 DB를 열면 작업·이벤트·승인 보존, 버전 7, 기존 이벤트 `actor` NULL), `TestEventsRecordActorFromContext`(제출·전이·재시도·취소 각각이 컨텍스트의 주체를 기록), `TestActorDefaultsToSystem`(컨텍스트에 없으면 `system`), `TestActorFormat`(주체 문자열은 `kind:id`이고 허용 문자는 `[A-Za-z0-9_.:@-]`, 길이 1~80. 이 규칙에 맞지 않으면 원문을 저장하지 않고 고정 문자열 `invalid`를 기록한다. 이벤트 기록은 절대 실패하지 않는다), `TestEveryWriteHandlerPassesPrincipal`(표 기반: 제출·취소·재시도·승인(grant)·reverify 등 쓰기 핸들러를 소켓·세션·토큰 주체로 호출하고 이벤트의 `actor`가 그 주체인지 단언 — 검토 초점 3), `TestLedgerSubmittedEventHasActor`(원장에 `task_submitted`, `actor`, `via`), `TestTransitionLedgerPayloadHasActor`, `TestApprovalGrantedViaIsKindAndID`.
- [ ] **구현:** (1) `schema_v7.sql`: `ALTER TABLE task_events ADD COLUMN actor TEXT`, `migrations` 슬라이스에 추가하고 기존 마이그레이션 시험이 하드코딩한 버전 숫자(6)를 7로 바꾼다. (2) `actor.go`에 컨텍스트 키·검증·정규화. (3) 이벤트를 쓰는 모든 `INSERT INTO task_events`가 `ActorFrom(ctx)`를 넣는다(`CreateTask`, `transitionTx`, `RetryFailed`, `ReleaseClaims`는 `system`). (4) API 미들웨어/핸들러가 `PrincipalFrom`에서 주체 문자열을 만들어 `store.WithActor`로 요청 컨텍스트에 싣는다: **`authenticate`가 한 곳에서** 하도록 해서 핸들러마다 빠뜨리는 일이 없게 한다. (5) 원장 `recordTransition`이 `ActorFrom`을 읽어 페이로드에 넣고, 제출 시 `task_submitted`를 쓴다(원장 쓰기 실패는 기존처럼 로그만). (6) `viewOf`/`TaskView`에 `Actor`(마지막 이벤트). (7) CLI `show`에 한 줄.
- [ ] **통과:** 위 시험 + 기존 `go test ./internal/orch/... -count=1 -race`. 커밋 `feat(store): record who did it (schema v7, actor on events and ledger)`.

---

### Task 2 (A2): 취소·재시도 오류 코드, `cancellable`, 경쟁 버그 수정

**파일:** 수정 `internal/orch/api/api.go`(`TaskView`, `cancel`, `retry`), `internal/orch/machine/machine.go`·`retry.go`(필요 시), 테스트 `internal/orch/api/cancel_retry_test.go`(만들기)

**인터페이스:**
- 생산: `TaskView.Cancellable bool`(`json:"cancellable,omitempty"`), v1 오류 코드: 취소 — `not_found`(404), `already_final`(409, "이미 끝난 작업입니다"), `state_changed`(409); 재시도 — `not_found`(404), `not_retryable`(409, 응답 본문에 `reason`을 담은 필드: 기존 `NotRetryableError.Reason` 문장, 서버가 만든 고정 문장만), `state_changed`(409). **취소가 5회 경쟁에서 지면 지금의 500이 아니라 409 `state_changed`.**
- 소비: `machine.Cancel`(`ErrTerminal`, `ErrStateMismatch`), `machine.Retry`(`*NotRetryableError`), `apiErr(status, v1Code, msg)`.

- [ ] **시험:** `TestCancelOfFinalTaskIsAlreadyFinal409`, `TestCancelRaceLostIsStateChanged409NotTheGeneric500`(경쟁을 흉내: `machine`에 5번 모두 `ErrStateMismatch`를 주는 시험용 스텁 또는 동시 호출), `TestRetryNotRetryableCarriesReason`(취소됨·done·판정 실패·게시 중 실패 각각 `not_retryable`과 `reason`), `TestRetryStateChanged409`, `TestCancellableFlagMatchesStateTable`(모든 상태에 대해 `cancellable` 값을 `machine.Allowed`의 `cancelled` 전이와 대조), `TestCancelAndRetryConcurrentOneWins`(검토 초점 2: 같은 failed 작업에 동시에 취소·재시도 → 정확히 한쪽만 200, 다른 쪽 409), `TestCancelRetryRequireWriteTier`(인증 없음 401, 세션 CSRF 누락 403).
- [ ] **구현:** `viewOf`가 `Cancellable`을 계산(기존 `Allowed` 표를 재사용하고 웹이 상태 목록을 따로 들지 않게 한다), `cancel`/`retry`의 `switch`에 코드 부여, `ErrStateMismatch` 처리 추가. 오류 응답에서 `reason`은 v1 봉투가 `reason` 키를 나르게 하는 방법을 기존 봉투 구현(`v1Wrap`)에 맞춰 선택하고 W1 봉투 시험이 깨지지 않게 한다(레거시 평평한 형태는 유지).
- [ ] **통과 + `-race`**, 커밋 `fix(api): cancel/retry error codes, cancellable flag, race loser is a 409`.

---

### Task 3 (A3): 제출 검증(종류·큐 상한)과 `GET /api/v1/repos`

**파일:** 수정 `internal/orch/api/api.go`(`validate`, `submit`, `handler`), `internal/orch/api/v1.go`(경로 등록), 만들기 `internal/orch/api/repos.go`·`repos_test.go`, 수정 `internal/orch/store/store.go`(`CountQueued`), `cmd/orchd/main.go`(플래그 `--queue-limit`)

**인터페이스:**
- 생산: `POST /api/v1/tasks`의 새 오류 — `unknown_class`(400), `queue_full`(429, 한계 도달); `GET /api/v1/repos` → `[{path, head, branch, branches:[…최근 N개]}]`(경로는 허용 루트 아래, 심볼릭 링크를 푼 실제 경로만); `Config.QueueLimit int`(0이면 기본 20), `store.CountQueued(ctx) (int, error)`.
- 소비: `h.policy()`(PolicyView.Classes), `h.roots`, `gitx.Resolve`(HEAD 해석), `h.underRoot`.

- [ ] **시험:** `TestSubmitUnknownClassRefused`(정책에 없는 종류 → 400 `unknown_class`; 정책 없음(`h.policy==nil`)이면 기존 동작 유지), `TestSubmitKnownClassAccepted`, `TestSubmitQueueFull429`(한계까지 채우면 다음이 429 `queue_full`, 작업이 하나도 만들어지지 않음), `TestQueueLimitRaceNeverExceeds`(검토 초점 1: 동시 N+10 요청 → 만들어진 대기 작업 수가 한계를 넘지 않는다. 검사와 삽입은 한 트랜잭션 또는 `idemMu`와 같은 직렬화로), `TestQueueLimitCountsOnlyQueued`(진행 중·끝난 작업은 세지 않는다), `TestReposListsOnlyUnderAllowedRoots`(허용 루트 밖 저장소·심볼릭 링크로 밖을 가리키는 항목·`.git` 없는 폴더는 나오지 않는다 — 검토 초점 5), `TestReposHeadAndBranches`(실제 임시 git 저장소), `TestReposReadOnlyAndAuthRequired`(401, 어떤 쓰기 git 명령도 호출하지 않음: 시험이 저장소 상태 해시를 전후 비교), `TestReposNoPathLeakInError`(실패 시 고정 문구).
- [ ] **구현:** `validate`에서 종류를 `h.policy().Classes`와 대조(정책이 설정된 경우에만), `submit`에서 큐 상한 검사와 생성을 원자적으로(`CreateTask` 앞에서 `CountQueued`를 쓰되 같은 뮤텍스 또는 트랜잭션 안에서), `repos.go`는 허용 루트를 얕게(깊이 제한 2) 훑어 `.git`이 있는 폴더를 모으고 `git rev-parse`·`for-each-ref`만 읽기로 호출(`GIT_OPTIONAL_LOCKS=0`, 훅·설정 없는 환경은 기존 `gitx` 방식 재사용). 목록 크기 상한 200.
- [ ] **통과 + `-race`**, 커밋 `feat(api): submit validates class and queue limit, GET /repos`.

---

### Task 4 (A4): `ops.cloud` (쿼터·VM 상태)

**파일:** 수정 `internal/orch/api/info.go`(`OpsView.Cloud`, `CloudView`), `internal/orch/api/daemon.go`(`Config.CloudStatus`), `internal/orch/vm/snapshot.go`·`prune.go`(쿼터 구조체 노출), 만들기 `internal/orch/vm/cloudstatus.go`·`cloudstatus_test.go`, 수정 `cmd/orchd/info.go`·`cmd/orchd/vm.go`(공급 연결)

**인터페이스:**
- 생산: `type CloudView struct{ OK bool; Code string; Quota *QuotaView; TestVM *VMState; RestoreVolumes int; History []RestoreView; CheckedAt time.Time }`, `QuotaView{Gigabytes, Volumes Limit{Limit,InUse,Reserved int; Unlimited bool}}`, `Config.CloudStatus func(ctx context.Context) CloudView`, `vm.(*TestVM).CloudStatus(ctx)`(허용 목록의 읽기 동사 `quota show --usage -f json`, `server show <vm> -f json`, `volume list`만 사용; 쓰기 동사 추가 금지). 캐시: TTL 60초, single-flight, 호출당 시간 제한 20초. 실패 시 `{ok:false, code:"cloud_unavailable"|"cloud_timeout"}`(원문 없음).
- 소비: `OpenStackCLI.allowed()`, 기존 `quotaFits`가 파싱하는 JSON(구조체로 공용화), `TestVM.History()`, `LastRestore()`.

- [ ] **시험:** `TestCloudStatusParsesQuota`(실제 `quota show --usage -f json` 모양의 고정 예시 JSON, 무제한 `-1`), `TestCloudStatusCachedWithinTTL`(60초 안 두 번 불러도 클라우드 호출 1회, 시계 주입), `TestCloudStatusSingleFlight`(동시 20개 호출 → 외부 호출 1회), `TestCloudStatusFailureIsPartial`(검토 초점 4: 클라우드 실패·시간 초과여도 `/ops`가 200이고 나머지 필드는 그대로, `cloud.ok=false`, 오류 원문 없음), `TestCloudStatusOnlyAllowedVerbs`(실행된 명령이 허용 목록의 읽기 동사뿐: 가짜 실행기가 호출을 기록), `TestOpsWithoutCloudSupplierStillWorks`(공급 함수 nil), `TestOpsCloudRequiresAuth`.
- [ ] **구현:** 기존 쿼터 파싱을 공용 함수로 올려 `quotaFits`와 `CloudStatus`가 같은 구조체를 쓰게 한다(동작 변경 없음을 기존 `prune` 시험으로 확인). `cmd/orchd`가 `Config.CloudStatus`를 `TestVM`에 연결(클라우드 설정이 없으면 nil). `opsView`가 `Cloud`를 채운다.
- [ ] **통과 + `-race`**, 커밋 `feat(api): ops.cloud with cached quota and VM status`.

---

### Task 5 (A5): 웹 — 제출 화면, 작업 버튼, 서버 상태 화면, 진행 표시

**파일:** 만들기 `internal/orch/web/static/views/new.js`, `views/ops.js`, 수정 `static/api.js`(`post`가 `opts.headers`로 `Idempotency-Key` 허용), `static/router.js`(경로 `#/new`, `#/ops`, `navItem`), `static/app.js`(`VIEW_MODULES`), `static/views/task.js`(취소·재시도 버튼, 진행 표시), `static/index.html`/`style.css`(필요 시), `static/render.js`(필요한 허용 속성만), 시험 `test/new.test.js`·`test/ops.test.js`·`test/task_actions.test.js`, 픽스처 `test/fixtures/*.json`(Go 골든 시험이 실제 구조체에서 만든다: `internal/orch/api/web_fixtures_test.go` 확장)

**인터페이스:**
- 소비: 위 API(`GET /repos`, `GET /policy`(종류 목록), `POST /tasks`, `POST /tasks/{id}/cancel|retry`, `GET /ops`, `TaskView.cancellable|retryable|actor`), 웹 뼈대(`registerView`, `el`, `poll`, `ID_RE`, `SAFE_TOKEN_RE`).
- 생산: `assemblePrompt({goal, scope, done, avoid}) → string`(고정 영어 템플릿 헤더 `Goal:`/`Scope:`/`Done when:`/`Do not:`, 빈 칸은 생략, 입력 안의 제어·방향 전환 문자는 거부), `newIdempotencyKey() → string`(암호학적 난수, `[A-Za-z0-9]{32}`).

- [ ] **시험(JS, 이름 그대로):** `TestAssemblePromptTemplateAndLimits`(칸이 비면 생략, 64 KiB(바이트) 초과는 거부 안내, UTF-8 바이트 길이로 셈), `TestAssemblePromptRejectsHostileChars`, `TestNewViewSubmitsOnceWithIdempotencyKey`(더블 클릭·엔터 연타에도 요청 1회, 같은 키로 재시도해도 같은 작업), `TestNewViewShowsFinalPromptBeforeSubmit`(조립된 최종 프롬프트가 화면에 그대로 보이고, 보이는 값과 전송되는 값이 같다), `TestNewViewErrorsFixedTexts`(`unknown_class`/`queue_full`/`bad_request`/429를 고정 한국어 문구로, 서버 원문 미표시), `TestNewViewReposAndClassesFromApi`(저장소·종류 선택지는 서버 응답으로만, 모르는 값 제출 불가), `TestTaskCancelRetryButtons`(`cancellable`/`retryable`에 따라 버튼 표시, 확인 창 뒤에만 요청, 409 `state_changed` 후 새로고침), `TestTaskActionsNoDoubleSend`, `TestOpsViewQuotaWarningThreshold`(80% 이상 경고와 정리 안내 문구, 쿼터 없음·실패 상태 표시), `TestOpsViewHostileText`(숫자·문자 모두 텍스트 노드), `TestTaskProgressShowsCurrentStepElapsed`(진행 중 단계와 경과 시간, 대기·완료 상태에서는 타이머 없음), `TestActorShownAsText`. 기존 정적 위험 API 검사와 `TestWebAssetsEmbedded`가 새 파일도 포함해 통과해야 한다.
- [ ] **구현:** `#/new`(저장소 드롭다운과 HEAD 기본값, 종류, 에이전트, 요구사항 네 칸, 최종 프롬프트 미리보기, 제출 버튼은 네이티브 `<button>`), 제출 성공 시 `#/tasks/<id>`로 이동(`ID_RE` 검증 후), 작업 상세에 취소·재시도 버튼(확인 창은 `<dialog>` 또는 자체 확인 단계, 인라인 `confirm()` 금지), `#/ops`(쿼터 막대는 CSSOM 너비, 텍스트 대체 제공), 메뉴에 "새 작업"·"서버 상태" 추가. 모든 폴링은 `poll`(일시정지·가시성), `/auth/*`는 폴링하지 않는다(CF19). CF15(CSRF 갱신 1회)와 429 처리는 기존 `api.js`가 이미 한다.
- [ ] **시각 확인:** ego-browser와 W1의 개발용 픽스처 서버(`web/dev_test.go`, `ORCH_WEB_DEV`)에 제출·서버 상태·진행 중 작업 픽스처를 추가해 1280px·390px 화면을 보고, **본 것만** 보고서에 적는다. 키보드·스크린리더를 했는지 여부도 사실대로 쓴다.
- [ ] **통과 + `-race` + node 테스트**, 커밋 `feat(web): submit, cancel/retry, server status and progress views`.

---

### Task 6 (A6): 적대 시험, 결과 문서, 컷오버 런북

**파일:** 만들기 `internal/orch/api/adversarial_w2a_test.go`, `docs/16-RESULTS-webconsole-W2a.md`; 수정 `docs/10-OPERATIONS-orchestrator.md`(W2a 컷오버 절), `docs/14-DESIGN-webconsole-W2.md`(상태 한 줄)

- [ ] **적대 시험(모두 거부/무효):** `TestSubmitNeedsAuthAndCSRF`(제출·취소·재시도 각각 401/403), `TestSubmitHostileFields`(경로 이탈 repo, 심볼릭 링크로 허용 루트 밖, `base`에 옵션처럼 보이는 값, 64 KiB 초과 프롬프트, NUL, 잘못된 UTF-8, 매우 긴 class), `TestForgedActorHeaderIgnored`(클라이언트가 보낸 어떤 헤더·본문 필드로도 주체를 바꿀 수 없다: `X-Actor`, `actor` 필드 등), `TestQueueFloodBoundedAcrossPrincipals`, `TestCancelRetryOtherTasksUntouched`, `TestReposCannotEscapeRoots`, `TestOpsNeverLeaksCloudErrorText`, `TestIdempotentSubmitAcrossRestartWindow`(같은 키 재전송은 같은 작업).
- [ ] **변이 확인(저장소 복사본에서만, 작업 트리에 남기지 않음):** ① 큐 상한 검사 제거 ② 종류 대조 제거 ③ 주체 전달 제거(항상 `system`) ④ `state_changed` 매핑 제거(500으로 복귀) ⑤ `repos`의 허용 루트 검사 제거 ⑥ `ops`의 클라우드 오류 원문 노출 ⑦ 웹의 `Idempotency-Key` 제거. 각각 어느 시험이 실패했는지 표로 기록한다. 안 걸리면 시험을 추가하고 다시 한다.
- [ ] **결과 문서:** 시험 목록과 실제 실행 개수(`go test ./... -count=1 -race`, `node --test`), 변이 표, 설계서와 달라진 점, **실서버에서 확인하지 못한 것**(컷오버 전이므로 전부)을 정직하게 쓴다.
- [ ] **컷오버 런북(W2a):** 순서 고정, 단계마다 백업. (1) 백업: 바이너리 `*.prev-w2a`, DB(스키마 v7로 올라가므로 **옛 `orchd`는 v7 DB를 열지 않는다**, 롤백은 DB 복원 동반), 유닛. (2) 새 바이너리 배포·재시작: `journalctl`에 오류 없음, 기존 작업 목록 보존. (3) 웹에서 제출 → 대기 → 승인 필요 작업이면 W1 방식으로 서명 승인 → `done`; 취소·재시도 버튼 시험(취소는 대기 중인 작업으로, 재시도는 인프라 실패 작업이 필요하므로 불가능하면 "검증 못 함"으로 기록). (4) `#/ops`의 쿼터 수치가 `openstack quota show --usage`와 일치하는지 사용자가 확인. (5) 이력에 주체가 남았는지 확인. 롤백 절차와 "그래도 안 되면" 줄을 단계마다 쓴다. 사용자 작업 표시.
- [ ] **`go test ./... -count=1 -race` 전체 + node 테스트**, 커밋 `test: adversarial suite for W2a`, `docs: W2a results and cutover runbook`.

---

## 자체 검토 기록
- **설계서 대비 범위:** 3.1(제출 화면·요구사항 작성기·`/repos`·종류 대조·`Idempotency-Key`·큐 상한) → A3·A5; 3.2(취소·재시도·`cancellable`·오류 코드·500 버그) → A2·A5; 3.3(`ops.cloud`·캐시·경고·진행 표시) → A4·A5; 3.4(스키마 v7·주체 전달·원장) → A1; 3.5(API 표) → A1~A4; 5장 위협 중 W2a에 해당하는 것(큐 소진, 주체 위조) → A3·A6; 6장(적대 시험·변이·정직한 한계) → A6.
- **의도적으로 W2b로 미룬 것:** 브로커, AI 다듬기, MR 자동 생성, `tasks.branch/mr_*`(스키마 v8), `POST /refine*`, `POST /tasks/{id}/mr`.
- **이름 일관성:** `Actor`/`actor`(이벤트·`TaskView`·원장), `Cancellable`/`cancellable`, 오류 코드 `already_final`·`not_retryable`·`state_changed`·`unknown_class`·`queue_full`, `CloudView`/`ops.cloud`, `QueueLimit`/`--queue-limit`가 A1~A6에서 같다.
- **미결(구현 중 구현자가 정하고 보고서에 기록):** 정책이 없는 서버에서 종류 검증을 건너뛸지(계획은 건너뜀), `reason` 키를 v1 봉투에 싣는 정확한 모양, `repos`의 훑는 깊이(계획은 2), `CloudStatus`의 `volume list` 사용 여부(필요 없으면 제외).
- **사용자 확인이 필요한 값(설계서 7장 Q1·Q2):** 큐 상한 20, 쿼터 경고 80%는 기본값으로 구현하고 플래그/상수로 바꿀 수 있게 한다. Q3~Q8(AI·GitLab 관련)은 W2b 계획에서 다룬다.
