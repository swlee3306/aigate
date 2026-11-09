# 웹 콘솔 W2b — 결과 기록 (2026-11-09)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w2b`)에서 끝났고, 실제 서버(orch-vm)에는 아직 아무것도 배포하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 11장, 설계는 `14-DESIGN-webconsole-W2.md` 4장, 계획은 `17-PLAN-webconsole-W2b.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **가짜 OpenAI·GitLab·브로커**, 실제 `git`)만 가리킨다. **실제 OpenAI, 실제 GitLab, 리눅스에서의 브로커(systemd 하드닝, `SO_PEERCRED`), 실제 키·토큰은 한 번도 쓰지 않았다**(5장). 이 작업에서 실제 키를 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다.

## 1. 만든 것

**B1. `orch-broker` 뼈대.** 키·토큰을 쥐는 별도 서비스(유닉스 소켓, 한 연결에 JSON 한 줄 요청 → 한 줄 응답, 512 KiB, 읽기 5초, 동시 4). 설정(`/etc/orch-broker/broker.json`)은 root 소유·쓰기 금지·상위 폴더 사슬 전부 신뢰될 때만 시작하고(그렇지 않으면 시작 거부), 키·토큰 파일은 `orch-broker` 소유 0400/0600이어야 한다. 호출자는 허용 uid 목록(`SO_PEERCRED`/`LOCAL_PEERCRED`)만. 한도: 시간당(AI 20, MR 30)과 **최근 7일 롤링 주 25달러 예산**(`/var/lib/orch-broker/budget.json`, 호출 전에 최악 비용을 예약하고 사용량으로 정산, 실패·사용량 없음이면 예약값이 지출로 남음, 깨진 파일은 예산 소진으로 취급). 오류는 고정 코드와 고정 한국어 문장뿐이고 등록되지 않은 코드는 `internal_error`다. 로그는 키 모양과 알려진 비밀을 가린다. `infra/systemd/orch-broker.service`, `infra/broker-init.sh`.
**B2. `refine`(OpenAI).** Responses API(`POST /v1/responses`, `store:false`, 엄격한 구조화 출력), 모델 ID·단가는 설정(추측 안 함). 지시문과 자료를 분리(요청마다 무작위 표지), 비밀 모양 검사(개인 키, AWS, JWT, Slack, 값 있는 `api_key=…`류), 계획서 스키마 엄격 검증(알 수 없는 필드·초과 길이·제어/방향 전환/서식(Cf) 문자 거부, id는 브로커가 다시 매김, 보낸 파일이 아닌 경로는 버림), 리다이렉트 안 따름, 업스트림 오류는 전부 `upstream_error` 하나.
**B3. `create_mr`(GitLab).** 설정에 고정된 기준 주소 하나, 프로젝트는 설정에서만(요청이 정하지 못함), 브랜치는 정확히 `orch/<번호>`이고 번호와 같아야 함, 제목·설명은 브로커가 고정 양식으로 만듦(`Draft:` 접두 + `draft:true`), 같은 브랜치의 열린 MR이 있으면 재사용(대상 브랜치까지 일치할 때만), 응답의 `web_url`이 기준 주소 아래일 때만 받음, **병합·수정·삭제 호출이 코드에 없음**, 리다이렉트 안 따름, 생성 직렬화.
**B4. orchd의 AI 계획서 경로.** `POST /api/v1/refine/preview`(보낼 파일 목록·크기·제외 사유 계산, 브로커의 허용 목록을 먼저 물음, **선택 ID** 발급)와 `POST /api/v1/refine`(선택 ID만 받아 보관된 선택을 다시 읽고 **내용 해시가 같을 때만** 브로커로 보냄, 요구사항 글도 미리보기와 같아야 함). 컨텍스트는 HEAD 커밋 기준 읽기 전용 git(작업 트리 아님), 파일 40개·32 KiB·합계 240 KiB, 비밀 이름·이진·심볼릭 링크·서브모듈 제외. 주체별 미리보기 30/h, AI 5/h, 선택 보관 10분·메모리·주체별 10개·전체 50개. 플래그 `--broker-socket`(비우면 AI·MR 꺼짐).
**B5. 게시 뒤 MR(스키마 v8).** `tasks`에 `branch`, `mr_iid`, `mr_url`, `mr_state`(`created|failed|skipped`), `mr_code`. `publishing → done` 전이 직후에만 훅이 돌고(`done`은 이미 확정, 훅이 바꾸지 못함), 한 번만, 브로커가 `repo_not_mapped`이거나 브로커가 없으면 `skipped`. `POST /api/v1/tasks/{id}/mr`(재시도, 쓰기 단계). `TaskView`에 `branch`·`mr_url`·`mr_iid`·`mr_state`·`mr_code`.
**B6. 웹.** `#/new/plan`: 한 줄 → 미리보기(파일·바이트·제외 사유 확인) → 계획서 체크리스트(항목 체크·편집·추가, 질문 답, 범위 밖, 전체 의견) → 확정(읽기 전용 최종 문장, **체크 해제 항목은 `Do not`**, Goal은 사용자가 친 한 줄) → 작업 하나로 제출. 작업 화면의 "병합 요청" 구역(링크는 안전한 주소만, 실패는 고정 문장·재시도). 429는 자동 재시도하지 않고, 로딩 중 취소·떠날 때 보호(막힘 없음).
**B7. 이번 작업.** 적대 시험(아래 2·3장), 변이 시험(3장), **`orch-broker -check`**(설정·신뢰·키 모드·소켓 그룹·상태 폴더만 확인, 외부 접속 없음, 아무것도 쓰지 않음; 시험 3개), 이 문서, 컷오버 런북(`10-OPERATIONS-orchestrator.md` 11장), `infra/README.md`·`14번` 문서의 상태 줄. **`--live-check`는 구현하지 않았다**(런북에서 뺐다. 첫 실제 호출은 컷오버 단계 3에서 웹으로 한다).

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-09, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0, 커밋 `f2a0f58`(코드·시험의 마지막 커밋, 최종 검토 수정 포함. 이 문서 커밋은 코드를 바꾸지 않음). 세는 방식은 W2a(16번 문서 2장)와 같다: 최상위 시험 함수(`Test…`)와 하위 시험(`t.Run`)을 따로 센다(`go test -json` 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**. 최상위 시험 함수 1086개 중 **1084 통과, 0 실패, 2 건너뜀**(하위 시험 730개 모두 통과). 건너뜀 2개는 W1·W2a와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollectionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사). 표준 오류 출력 없음.
- `node --test "internal/orch/web/test/*.test.js"`: **137개 전부 통과**(W2a 105개 + W2b 32개).
- W2a(16번 문서) 대비 최상위 통과 925 → 1084, 새 패키지: `cmd/orch-broker`, `internal/broker`, `internal/orch/broker`, `internal/orch/refine`, `internal/refinesize`(시험 파일 없음. 그 숫자는 broker·orch/api·orch/broker·orch/refine의 시험이 맞춰 본다).

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 5 | 0 | 0 | 0 |
| `cmd/orchd` | 31 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 55 | 0 | 0 | 76 |
| `internal/broker` | 74 | 0 | 0 | 38 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 45 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 238 | 0 | 0 | 25 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 5 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 17 | 0 | 0 | 0 |
| `internal/orch/refine` | 13 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 43 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 9 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

테스트 파일이 없는 패키지(`internal/approval/ui`, `internal/gittest`)는 표에서 뺐다.

이번(B7)에 더한 시험(이름 그대로):
- `internal/broker/adversarial_test.go` 7개: `TestBrokerKeysNeverInAnyResponseOrLog`, `TestBrokerIgnoresClientSuppliedUpstreamURLs`, `TestBrokerOpAllowlist`, `TestBrokerForeignUidAndOversizeFlood`, `TestBrokerConfigTamperRefusedAtStart`, `TestBrokerBudgetNotBypassableByParallelRequests`, `TestPromptInjectionCannotChangeSchemaOrDestination`.
- `internal/orch/api/adversarial_w2b_test.go` 6개: `TestRefineCannotLeakOutsideRoots`, `TestRefinePreviewForbiddenRepoLeaksNothing`, `TestRefineSelectionTamperAcrossPrincipals`, `TestMRRequiresPublishedDone`, `TestOrchdWorksWithBrokerDown`(하위 2), `TestRefineAndMRLedgerHaveNoSecrets`.
- `cmd/orch-broker/main_test.go` 3개: `TestCheckPassesWithoutAnyConnection`, `TestCheckRefusesBadTrustAndKeyModes`, `TestRunCheckFlag`.

적대 시험이 확인하는 것(요약):
| 시험 | 공격 | 기대(확인함) |
|---|---|---|
| `TestBrokerKeysNeverInAnyResponseOrLog` | 모든 op와 오류 경로(성공, 401·500·깨진 응답 — 키·토큰·`Authorization` 값을 되풀이하는 가짜 OpenAI·GitLab, 허용 밖 저장소, 비밀 모양 파일, 나쁜 경로, 미등록 저장소, 나쁜 브랜치, 알 수 없는 op, 깨진 JSON, 틀린 버전, 상한 초과)를 두드린 뒤 | 모든 응답·로그·`budget.json`에 시험 키·토큰·`Bearer test`·`PRIVATE-TOKEN`이 없음, 모든 실패 응답이 해당 코드의 고정 문장 |
| `TestBrokerIgnoresClientSuppliedUpstreamURLs` | 요청에 `url`·`base_url`·`host`·`proxy`·`project`·`target_branch` 등 11개 필드를 더함, 요구사항·파일·`title_hint`·`summary`·`actor`·`repo`에 공격자 URL을 넣음 | 추가 필드는 `bad_request`, 공격자 서버 요청 0, OpenAI 가짜는 `/v1/responses`와 설정의 모델만, GitLab 가짜는 설정의 프로젝트 경로와 `main`/`orch/12`만, MR 설명에 `://` 없음 |
| `TestBrokerOpAllowlist` | `Refine`, `REFINE`, 앞뒤 공백·개행·NUL, `refine\x00status`, 폭 0 문자, 키릴·전각 문자, 소프트 하이픈, 결합 문자, `../refine`, `__proto__` 등 37개 | 전부 `unknown_op`, 틀린 `v`·빈 `op`·배열 `op`·여분 필드·두 요청 이어붙임은 `bad_request`, 업스트림 호출 0. 대조군 `status`·`refine` 성공 |
| `TestBrokerForeignUidAndOversizeFlood` | 허용되지 않은 uid가 세 op와 쓰레기를 보냄; 허용된 호출자가 512 KiB 초과 줄 16개 동시, 아무것도 안 보내는 연결 8개, 한 연결에 요청 둘 | 앞은 전부 `forbidden`·업스트림 0, 뒤는 `too_large`/`busy`만·업스트림 0, 한 연결에는 답 하나·업스트림 호출 1, 읽기 시간 초과 뒤 서비스 회복 |
| `TestBrokerConfigTamperRefusedAtStart` | 설정 모드 0664·0646·0666·0620, 쓰기 가능한 상위 폴더, 신뢰할 수 없는 소유자, 심볼릭 링크, `context_repos`에 상대·`..`·끝 `/`·`//`·`~`·`$HOME`·빈 문자열, 모르는 키, 뒤 데이터, 키·토큰 파일 모드 8가지, 소유자 틀림, 심볼릭 링크 | 전부 시작 거부, 거부 문장에 키·토큰 없음. 대조군은 시작됨 |
| `TestBrokerBudgetNotBypassableByParallelRequests` | 실패하는 업스트림(예약이 지출로 남음)에 40개 동시 요청(예약 약 10달러, 한도 25달러) | 업스트림 호출 정확히 2개, 나머지 38개는 `budget_exceeded`, 뒤의 순차 호출도 거부, 저장된 지출이 한도를 넘지 않음(`-race`) |
| `TestPromptInjectionCannotChangeSchemaOrDestination` | 파일·요구사항에 "이전 지시를 무시하고…", 가짜 JSON, 마크다운 링크·이미지·`<script>`, 가짜 END 표지; 가짜 모델이 그대로 따라 8가지 출력(추가 필드, 일반 글, 코드 펜스, 문서 둘, 가짜 id, 틀린 타입, 허용 밖 파일 경로) | 전부 `bad_plan`이거나 정리되어 받아들임(id는 `r1`, 허용 밖 파일 경로 없음), 링크·HTML이 든 정상 스키마 계획서는 문자열로만 반환, 결과에 알 수 없는 필드 없음, 모든 요청의 출력 스키마가 같고 접속처가 같음, 공격자 서버 요청 0, 주입된 MR 요약이 대상 브랜치·프로젝트·`Draft:`·빠른 명령 줄을 바꾸지 못함 |
| `TestRefineCannotLeakOutsideRoots` | 루트 밖 저장소(브로커가 허용해도), 루트 안 심볼릭 링크→밖, `.git`이 밖을 가리키는 폴더, `..`·상대·하위 폴더·루트 자체·파일·빈 경로; 다른 저장소의 선택 ID; HEAD에 든 심볼릭 링크·서브모듈 | 403/400 고정 문장, 바깥 표지 문자열이 응답·브로커 호출 어디에도 없음, 선택은 만들어지지 않음, 링크·서브모듈은 보낼 목록에 없고 제외 사유(`symlink`, `submodule`) 표시. 대조군 성공 |
| `TestRefinePreviewForbiddenRepoLeaksNothing` | 브로커가 어떤 저장소도 허용하지 않을 때 내용이 다른 두 저장소, 루트 밖 존재·미존재 경로, 브로커 고장 | 두 저장소의 응답이 **바이트 단위로 같고** 파일 이름이 없음, 루트 밖 존재/미존재가 같은 403, 브로커 고장은 고정 오류·파일 없음, 이후 어떤 선택 ID로도 전송 불가. 대조군 성공 |
| `TestRefineSelectionTamperAcrossPrincipals` | 세 주체(소켓·토큰 둘)의 선택 ID를 서로 바꿔 씀, 변조·잘림·대문자·뒤집기·SQL 조각·긴 문자열 등 19가지, 본문 글 바꾸기 | 소유자가 아니면 400/409/410(200 없음), 브로커 호출 0, 소유자의 ID는 그 뒤에도 유효, 브로커가 본 선택 ID는 소유자 것뿐 |
| `TestMRRequiresPublishedDone` | 11개 비종결·실패 상태 × MR 상태 3가지에서 재시도, 깨진 ID, 이미 만들어진 MR | 전부 409/4xx, 생성기 호출 0, 대조군 1번만 호출 |
| `TestOrchdWorksWithBrokerDown` | 소켓이 없음 / 리스너가 사라진 소켓에서 제출·목록·조회·취소·승인, 미리보기, 계획서, MR 재시도 | 기존 기능 정상, 미리보기는 고정 `broker_unavailable`(소켓 경로 노출 없음), MR 재시도는 502 고정 오류이고 작업은 `done` 그대로, 기록은 `failed`/`broker_unavailable` |
| `TestRefineAndMRLedgerHaveNoSecrets` | 키·토큰 모양과 요청 글 표지가 든 브로커 오류, 비 코드 문자열을 코드로 쓴 MR 오류 | 원장 파일·`mr_code`·이벤트 메모 어디에도 비밀·요청 글·파일 이름 없음, `refine`·`merge_request` 줄이 쓰이고 해시 사슬 검증 통과 |

**최종 검토 수정 라운드(`f2a0f58`)에서 더한 시험:** `TestPreviewSelectionFitsBrokerLimits`(api: 40개 큰 파일·200자 경로·한글 2000자 요구사항의 미리보기 뒤 브로커가 받는 요청이 입력 262144바이트·요청 한도 안), `TestPreviewSelectionIsAcceptedByTheBroker`(broker: 자르기 전에는 `too_large`, 자른 뒤에는 받아들임, `max_input_bytes`를 계산값에 정확히 맞추면 받고 1 모자라면 거절), `TestFitTrimsLowestPriorityUntilBothCapsHold`, `TestFitRespectsTheRequestCapWithEscapes`(refine), `TestRequestBytesMatchesEncode`(orch/broker: 공유 패키지의 요청 크기가 실제 인코딩과 같음), `TestBrokerClientUnsentFlag`(orch/broker), `TestRefineCancelAfterWriteKeepsSlotAndLedgers`, `TestRefineRefundsOnlyWhenNothingWasSent`, `TestMRRetryBadURLIsTerminalOnTheServer`(api), `TestRetryMRBadURLIsTerminal`, `TestBrokerMRRefusesSecretPromptWithoutCalling`, `TestMRHookTimeoutOutlastsTheBrokerCreateMRLimit`(steps), `TestMRRefusesSecretLookingHintAndSummary`(broker). 새 시험은 모두 고치기 전에 실패하는 것을 확인했다(`TestPreviewSelectionFitsBrokerLimits`는 `Fit` 호출을 빼면 `input 266770 over the broker's 262144`로 실패). 이 라운드의 변이 시험은 그 한 가지(`Fit` 제거)만 했다.

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖의 새 임시 폴더(`/tmp/w2b-mut-…`)로 복사**(`rsync`, `.git` 제외)해 한 곳만 망가뜨리고 관련 패키지(`go test`, 웹은 `node --test`)를 돌렸다. 변이마다 원본에서 파일을 다시 복사해 되돌렸다. 작업 트리는 건드리지 않았다. 전부 구현자 실행이다(이 작업의 검토자는 아직 재현하지 않음). 브리프의 12개 행 모두 한 번에 잡혔고 시험을 새로 더할 필요가 없었다(잡히지 않은 변이 0).

| # | 망가뜨린 곳 | 잡은 시험(이번에 추가한 것은 굵게) |
|---|---|---|
| ① | 브로커 허용 uid 검사 제거(`allowed`가 항상 true) | **TestBrokerForeignUidAndOversizeFlood**, TestForeignUIDRefused, TestAllowedUIDServedWithRealPeerCred |
| ② | 설정 신뢰 검사 제거(`checkTrusted`가 항상 통과) | **TestBrokerConfigTamperRefusedAtStart**, **TestCheckRefusesBadTrustAndKeyModes**, **TestRunCheckFlag**, TestConfigTrustRootOwnedOnly |
| ③ | 키 파일 모드 검사 제거 | **TestBrokerConfigTamperRefusedAtStart**, **TestCheckRefusesBadTrustAndKeyModes**, TestKeyFileMustBeOwnerOnly |
| ④a | `context_repos`의 절대·정리된 경로 검증 제거(설정) | **TestBrokerConfigTamperRefusedAtStart**, TestConfigValidatesOtherFields(`relative_repo`) |
| ④b | `context_repos` 허용 목록 검사 제거(`repoAllowed`가 항상 true, refine) | **TestBrokerIgnoresClientSuppliedUpstreamURLs**, TestRefuseRepoNotInContextRepos, TestRefineRateLimit, TestPlaceholdersNotImplemented |
| ⑤a | 파일 비밀 검사 제거(브로커의 refine) | TestSecretLookingFilesRefused (새 적대 시험은 잡지 못함: 그 시험들은 비밀 모양 파일을 넣는 것이 아니라 키가 응답·로그에 새는지를 본다. `TestBrokerKeysNeverInAnyResponseOrLog`는 비밀 모양 파일 요청을 넣지만 거부 코드를 단언하지 않는다) |
| ⑤b | 파일 비밀 검사 제거(orchd `refine.Collect`) | TestCollectExcludesSecretsAndBinaries (역시 하위 패키지 시험이 잡음) |
| ⑥a | 선택의 **내용 해시** 비교 제거 | TestRefineSelectionChangedIs409 (적대 시험 `…TamperAcrossPrincipals`는 파일을 바꾸지 않으므로 잡지 못함) |
| ⑥b | 선택의 **요구사항 글 해시** 비교 제거 | **TestRefineSelectionTamperAcrossPrincipals**, TestRefineSelectionChangedIs409 |
| ⑦ | 주 25달러 예산 검사 제거(`Reserve`) | **TestBrokerBudgetNotBypassableByParallelRequests**, TestBudgetReservationPreventsParallelOvershoot, TestBudgetConcurrentReservationsCannotOvershoot, TestBudgetCountsRealUsageInUSDAndStopsAtCap, TestWeeklyBudgetPersistsAcrossRestart, TestRollingSevenDayWindow |
| ⑧ | OpenAI 리다이렉트 따름 | TestNoRedirectFollowed, TestUpstreamErrorsCollapseToOneCode |
| ⑨a | GitLab 응답 `web_url` 기준 주소 고정 제거 | TestMRUrlMustStartWithBaseUrl |
| ⑨b | GitLab 리다이렉트 따름 | TestMRNoRedirectAndPinnedHost |
| ⑩ | MR 브랜치 형식(`^orch/[0-9]{1,9}$`·번호 일치) 검사 제거 | TestMRBranchMustMatchTask, TestMRBranchPatternExact, TestMRHourlyLimit |
| ⑪ | 웹: 체크 해제 항목이 `Requirements`로 들어감 | TestUncheckedItemsGoToDoNotSection, TestOutOfScopeAndAnswersAndNotesAssembled, TestEditedAndAddedItemsAreIncluded (node) |
| ⑫ | 오류 응답에 업스트림 원문 포함(OpenAI 본문을 오류로, `OpError.Message`를 응답으로) | **TestBrokerKeysNeverInAnyResponseOrLog**, TestApiKeyOnlyInHeaderNeverInBodyOrLogs, TestHostilePlanTextNeverEchoedInErrors, TestNoSecretInLogs, TestUnregisteredHandlerCodeIsInternalError, TestUpstreamErrorsCollapseToOneCode |

결과: **12/12 잡혔다**(④·⑤·⑥·⑨는 하위 행 포함 모두). 정직하게 적는다: ⑤a·⑤b·⑥a·⑧·⑨a·⑨b는 **새 적대 시험이 아니라 기존(B1~B6) 시험이 잡았다**(위 행의 괄호). 처음 시도에서 ④a·⑤a·⑥a는 변이가 컴파일되지 않아(미사용 변수·import) 잡힌 것이 아니라 빌드 실패였다. 변이를 `false && (…)` 형태로 바꿔 다시 돌려 위의 결과를 얻었다. ⑪은 처음에 시험 이름을 뽑지 못해(`node` 출력 형식) 따로 다시 돌려 세 개의 이름을 확인했다.

## 3b. 최종 검토에서 나온 막는 항목과 수정 (커밋 `f2a0f58`)

| 항목 | 문제 | 수정 |
|---|---|---|
| **I1** (막는 항목, **수정됨**) | 미리보기가 "허용"이라고 한 선택을 브로커가 `too_large`로 거절할 수 있었다(경로가 지시문에 두 번 들어가고 표지가 붙는 크기를 orchd가 세지 않았다) | 지시문·스키마·사용자 메시지 배치와 요청 크기를 한 곳(`internal/refinesize`, 입출력 없는 순수 패키지)에 두고 브로커와 orchd가 같이 쓴다. 미리보기는 `refine.Fit`으로 우선순위가 낮은 파일부터 뺀다(이유 `total_cap`, 화면의 기존 고정 문장 "전체 크기 한도 때문에 제외"). 브로커의 `too_large`는 슬롯을 돌려준다. |
| **I2** (막는 항목, **수정됨**) | 브라우저가 요청을 취소하면 소켓이 일찍 닫히고 슬롯이 돌아가는 동안 브로커는 계속 일했다(주체별 한도 우회, 원장 누락) | 브로커 호출이 요청의 취소를 따르지 않는다(클라이언트 자체 제한시간만 적용). `OpError.Unsent`로 "요청을 다 쓰기 전에 실패"(슬롯 반환)와 "쓴 뒤 실패"(슬롯 유지, 원장 `ok:false`)를 가른다. 브로커가 일하기 전에 거절한 `busy`·`forbidden`·`too_large`도 반환. |
| **I3** (막는 항목, **수정됨**) | 런북 1-8이 `sudo -u orch cat /etc/orch-broker/openai.key`를 시켰다. 권한이 잘못된 경우 키가 화면에 찍힌다 | 값을 찍지 않는 `test -r … && echo READABLE \|\| echo denied`로 바꿨다(예상 `denied`). 런북 11장, 이 문서, `infra/README.md`의 모든 명령을 훑어 값을 찍는 명령이 더는 없음을 확인했다(`ls -l`, 크기, 소유자·모드만). 예산 파일(`budget.json`)과 원장의 `cat`/`grep`은 값이 없다고 코드로 확인한 파일들이다. |
| M2 | MR 훅 제한시간(15초)이 브로커 `create_mr` 한도(20초)보다 짧았다 | 25초 |
| M3 | 서버가 `failed`+`bad_url`의 재시도도 브로커에 보냈다 | 409 `mr_not_applicable`, 브로커 호출·MR 슬롯 사용 없음(웹은 그대로) |
| M5 | MR 요약·제목 힌트에 비밀 검사가 없었다 | 브로커(원문과 정리한 글 모두)와 orchd(보내기 전)가 검사, `looks_like_secret`로 `mr_state=failed`. orchd 쪽은 프롬프트 원문과 한 줄로 정리한 글을 본다 |
| 런북 | (a) 추론 모델과 `max_output_tokens`, `-check`는 모델 이름을 검증하지 않음, (b) 롤백 뒤 낡은 MR·`orch/<번호>` 브랜치 정리, (c) 미리보기 화면 제한시간 25초 < 서버 최악 약 38초(다시 시도 안전) | 런북 11장에 반영 |
미루어 둔 것(최종 검토의 M4, M7, M8)은 `progress.md`에 있다. 이 세 건에는 코드 변경이 없다.

## 4. 설계서와 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| R-B1 | 주 예산은 달력 주가 아니라 **최근 7일 롤링**(사용자 25달러 결정과 함께) | 설정 한 곳 |
| B1-F1 | 한도를 생략하면 기본값, **명시한 0·음수는 시작 거부**(안전 한도를 조용히 넓히지 않음) | 운영자가 0으로 "끌" 수 없음(서비스를 멈춤) |
| B1-F2 | 핸들러가 내는 코드는 `fixedMessages`에 등록되어야 하고 미등록은 `internal_error`; `OpError.Message`는 어디로도 나가지 않음 | 새 코드마다 문장 등록 |
| B1-F3 | D3의 `tokens_remaining_today`를 없애고 `status`는 `weekly_budget_remaining_usd`를 줌 | 계획서(17번)의 D3 정정됨 |
| B2 | OpenAI는 **Responses API**(공식 문서 `structured-outputs`, `migrate-to-responses`, OpenAPI 명세 확인), `store:false`; 모델 ID는 코드에 없음 | Chat Completions로 바꿀 때 클라이언트 한 파일 |
| B2-a | `refine`은 사용자의 한 줄(≤2000자)만 받는다. D5의 조립 문장은 최종 작업 요청 글(웹) | 없음 |
| B2-b | 유니코드 서식(Cf) 문자는 계획서 글과 요구사항에서 전부 거부(ZWJ 이모지도 거부됨) | 일부 정상 글 거부 |
| B2-c | 비밀 검사는 보조 장치(따옴표 없는 대입·JWT·Slack 포함, 값에 숫자나 대소문자 혼합 필요) | 평범한 단어 비밀은 놓침, 오탐은 파일 제외 |
| B2-d | 모델이 준 계획서 `files` 중 실제로 보낸 경로가 아닌 것은 브로커가 버림 | 없음 |
| B3 | GitLab에는 문서화된 `draft` 생성 인자가 없어 **`Draft:` 제목 접두**가 실제 표시이고 본문에 `draft:true`도 넣음(무시되어도 무해, 공식 문서 확인) | 접두를 쓰지 않는 GitLab 버전이면 Draft 표시가 안 됨(미확인) |
| B3-a | 기존 MR 조회는 열린 MR만, 대상 브랜치가 설정값과 같을 때만 재사용. 생성 응답의 대상 브랜치도 확인 | 닫힌·병합된 MR 뒤 재시도는 새 MR을 만들 수 있음(런북 11.0 "알아둘 것" 3) |
| B3-b | 생성 성공인데 `web_url`이 기준 주소 밖이면 `bad_url`(MR은 GitLab에 있으나 저장 안 함, 재시도는 같은 결과) | 사람이 GitLab에서 확인 |
| B4-a | 요구사항 글은 미리보기와 같아야 한다(다르면 409). 웹은 같은 글을 보냄 | 더 엄격함 |
| B4-b | 선택은 주체별 10개·전체 50개(가득 차면 새 선택이 429), 미리보기 30/h·AI 5/h 주체별, 브로커에 닿기 전 실패(`broker_unavailable`·`busy`·`forbidden`)만 AI 슬롯을 돌려줌 | 슬롯 정책 조정 |
| B4-c | orchd 내용 상한 240 KiB(브로커 256 KiB는 지시문·스키마 포함). 최종 검토 뒤: 미리보기가 브로커의 크기 계산(`internal/refinesize`)으로 입력 262144바이트·요청 480 KiB 안에 들 때까지 낮은 순위 파일을 뺌 | 브로커의 `max_input_bytes`를 기본값보다 낮추면 보장이 깨짐(`too_large`, 슬롯 반환) |
| B4-d | git 읽기는 `GIT_NO_REPLACE_OBJECTS=1` 포함(기존 읽기 전용 호출 전체) | 없음 |
| B5-a | MR 켜기/끄기는 **브로커 설정의 `gitlab.projects`**(orchd 플래그 없음). 매핑이 없으면 `skipped` | 별도 플래그를 원하면 추가 작업 |
| B5-b | `skipped`는 작업 이벤트를 쓰지 않고(기존 이력 시험 유지) 브로커가 답한 경우에만 원장 줄 | 이력 표시가 줄어듦 |
| B5-c | `done`이 된 뒤 훅 전에 죽은 작업은 `mr_state`가 비고 재시도 불가(`failed`·`skipped`만 재시도) | 사람이 CLI로 보고 GitLab에서 직접 만듦 |
| B5-d | 요청 글(정리·상한 적용)이 MR 설명의 요약, 파일·diff는 안 보냄 | 요청 글에 비밀을 붙였다면 GitLab으로 감 |
| B6-a | 최종 문장의 Goal = 사용자가 친 한 줄(AI 요약은 화면 표시만), 계획서 `files`는 문장에 안 넣음 | 없음 |
| B6-b | 계획서 화면은 `ctx.navigate`가 없어도 `location.hash`로 이동(실제 `app.js` 경로 시험) | 없음 |
| B7 | `orch-broker -check` 추가(설정·신뢰·키 모드·소켓 그룹·상태 폴더만, 네트워크 없음, 쓰기 없음). `--live-check` 미구현 | 첫 실제 호출은 웹 단계 3 |

그 밖에 문서에 남길 동작:
- **키가 틀리면 실패한 호출마다 최악 비용 예약이 지출로 남는다**(401도). 시간당 20회·주 25달러로 막힌다.
- **미루어 둔 것(원장 `progress.md`)**: B1 F7~F11, B3 F6·F7, B4 F8, B5 F3·F5·F6·F9, B6 F7~F9 — 모두 Minor(검토자 판단), 코드 변경 없이 보류(B1 F8은 런북에 문서화). 상세는 `.superpowers/sdd/17-PLAN-webconsole-W2b/progress.md`.
- 유닛의 `SupplementaryGroups=orch`는 소켓 그룹을 `orch`로 바꾸기 위한 보안상 타협이다(런북 11.0 "알아둘 것" 5). `broker-init.sh`는 `orch` 그룹이 있는지 확인하지 않는다.
- 선택은 orchd 메모리에 10분만 산다. orchd를 재시작하면 사라진다.
- 계획서 화면의 떠날 때 보호는 확인 창이 아니라 배너이고, 브라우저 뒤로 가기 뒤 기록이 약간 어색할 수 있다. 새로고침·탭 닫기는 브라우저 자체 경고.

## 5. 실서버에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 11장)에서 처음 확인한다.**
- **실제 OpenAI 응답 형식과 구조화 출력의 동작.** 공식 문서로 형식을 확인했을 뿐 실제 호출은 없다. 엄격한 스키마를 실제 모델이 받아들이는지, `status:"completed"`·`usage` 필드가 문서대로인지 미확인.
- **실제 모델 ID와 단가가 설정과 맞는지.** 코드에는 모델 ID·단가가 없다(사용자가 설정에 적음). 틀리면 예산 계산이 틀린다(단가가 낮으면 25달러를 넘겨 쓸 수 있고 OpenAI 대시보드 한도가 두 번째 방어선이다).
- **키 한도 동작(`store: false` 포함).** 프로젝트 지출 한도가 걸린 키가 한도에서 어떻게 실패하는지, `store:false`가 실제로 저장을 끄는지.
- **실제 토큰 사용량 계산.** 응답의 `usage.input_tokens`·`output_tokens`(캐시 입력 포함)로 정산하는 것이 실제 청구와 맞는지. 사용량이 없거나 0이면 최악 예약이 그대로 지출로 남는 보수적 규칙이 실제로 너무 빨리 예산을 깎는지.
- **실제 GitLab.** http 접속(방화벽·주소·포트), `main` 보호(직접 push 금지, Maintainer만 병합), 프로젝트 토큰의 권한(Developer, `api`)으로 MR 목록·생성이 되는지, **`Draft:` 제목 접두가 실제 Draft 표시가 되는지**, 응답의 `web_url`이 설정의 기준 주소로 시작하는지(`web_url`이 GitLab의 외부 URL 설정에 따라 다른 호스트를 줄 수 있어 `bad_url`이 될 수 있다), 같은 브랜치의 재시도 동작.
- **브로커의 systemd 하드닝이 실제로 시작되는지**(`ProtectSystem=strict`, `SystemCallFilter=@system-service`, `MemoryDenyWriteExecute`, 빈 `CapabilityBoundingSet` 등. Go 바이너리가 이 조합에서 뜨는지, `SupplementaryGroups=orch`로 소켓 그룹이 바뀌는지). 유닛은 `systemd-analyze`조차 돌리지 않았다(macOS).
- **`orch` 계정이 `/etc/orch-broker`를 읽지 못하는지**(폴더 `root:orch-broker 0750`, 키 `0400 orch-broker`). `orch` 계정이 `orch-broker` 그룹에 속하지 않는다는 것도 서버의 실제 `id orch`로 본 적이 없다.
- **리눅스 `SO_PEERCRED`가 실제로 동작하는지.** 코드는 `internal/orch/api`의 것을 복사했고 macOS(`LOCAL_PEERCRED`)에서만 실행·시험했다. 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인.
- **`install -m 0400 … /dev/stdin`과 프롬프트 입력(`read -rs`) 절차**가 orch-vm의 쉘·`install`에서 되는지(런북의 대체 절차 있음).
- **실제 `orchd` 유닛과 `--broker-socket`**: 실제 유닛의 `ExecStart`와 보호 옵션(`PrivateTmp` 등)이 `/run/orch-broker/broker.sock`을 막는지는 보지 않았다.
- **실제 모델이 추론 모델일 때의 동작**: `max_output_tokens`를 추론에 다 써서 `incomplete`로 끝나는지(모든 호출이 `upstream_error`이고 비용은 청구). 가짜 서버의 `incomplete` 응답이 `upstream_error`가 되는 것만 확인.
- **브로커 시간 한도와 취소 동작**: 브라우저가 취소한 요청의 브로커 쪽 처리가 실제로 끝까지 가서 원장에 기록되는지는 가짜 브로커로만 확인(실제 60초 호출은 안 해 봄).
- **크기 계산의 실제 일치**: `internal/refinesize`가 센 바이트가 브로커 핸들러가 센 바이트와 같다는 것은 시험으로 확인했으나, 실제 OpenAI가 입력을 토큰으로 세는 방식과의 관계(최악 예약의 `ceil(바이트/3)`)는 실제 호출 전에는 미확인.
- **미리보기 화면 제한시간(25초)이 느린 저장소의 서버 최악(약 38초)보다 짧을 수 있음**(재시도는 안전, 실서버 측정 없음).
- **운영 DB의 v7 → v8 마이그레이션.** 시험 DB로만 확인(행 보존 시험 있음). 옛 `orchd`가 v8 DB를 거부하는 것은 코드의 버전 검사를 읽어서만 확인.
- **송신 경로**: `curl https://api.openai.com/v1/models`가 orch-vm에서 401을 주는지(저장소의 nftables 파일이 허용한다는 것은 파일을 읽어 확인, 서버에 올라간 규칙은 미확인).
- **키보드만, 스크린 리더로의 계획서 화면 사용.** DOM 대역 시험(라벨·`for/id`·네이티브 체크박스·`aria-live`·`aria-disabled`)만. ego-browser 시각 확인은 B6에서 1280px 중심으로 했고(키보드 흐름·스크린 리더·390px 재확인 없음), 이 작업(B7)에서는 하지 않았다.
- **실제 AI가 만든 계획서의 품질**, 실제 모델이 주입 지시를 얼마나 따르는지(가짜 모델로 모든 가능한 출력이 스키마 검증과 접속처 고정에 막히는 것만 확인).
- **크래시 창과 MR 중복**(런북 11.0 "알아둘 것" 3)은 코드와 가짜로만 확인했고 실제 GitLab의 닫힌·병합된 MR과의 상호작용은 미확인.
- **실부하에서의 한도**(미리보기 30/h, AI 5/h, 브로커 20·30/h, 주 25달러): 시험의 주입 시계로만.

## 6. 사용자 결정 목록

이미 정해진 것(2026-11-09 사용자): 주 25달러 상한(7일 롤링, R-B1 확인), 코드를 보낼 저장소는 **어떤 저장소든 가능**(컷오버 설정에는 `toy`·`sample-app` 경로), GitLab **프로젝트 액세스 토큰**(Developer, `api`, 만료일 지정)은 **컷오버 단계에 도달해 요청받으면 만들어 주기로** 했고 그때 `main` 보호도 확인.
컷오버 때 사용자가 정하거나 해야 하는 것(런북 11.0):
1. OpenAI **전용 프로젝트·키**를 만들고 프로젝트 지출 한도를 주 25달러로 건다(두 번째 방어선).
2. **모델 ID와 그 모델의 현재 단가**(입력·출력 100만 토큰당 USD)를 정해 설정에 적는다.
3. **MR을 이번 컷오버에 켤지**: 켜지 않으면 설정에서 `gitlab` 절을 뺀다(모든 작업 `skipped`). 켠다면 GitLab 기준 주소(http/https·포트), 프로젝트 경로 매핑, 토큰 발급, `main` 보호 확인. **실제 회사 GitLab에 MR을 만드는 시험(단계 3-8)을 할지**, 한다면 끝난 뒤 MR을 닫고 브랜치를 지울지.
4. 키·토큰 값은 사용자가 직접 서버에 둔다(대화·파일에 붙이지 않음).
5. 유닛의 `SupplementaryGroups=orch` 타협을 받아들일지, 소켓 활성화 대안(미구현)을 따로 작업할지.
6. 컷오버 중 예산이 이상하게 줄면 AI를 끄고 키를 폐기할지(런북 단계 4의 A).

## 7. 컷오버 기록 (2026-11-09, 실서버 orch-vm, AI만 켬 / MR 끔)
`main` c3c4751 기준 빌드. 런북(docs/10 11장) 단계 1·2·3(일부)을 실행했다. 아래는 서버에서 본 것만 적는다.

**실행한 것.** 단계 1: 파일 백업(`*.prev-w2b`, `orchd.service.pre-w2b`), 계정 `orch-broker`·`/etc/orch-broker`·`/var/lib/orch-broker`, 바이너리·유닛·`broker.json`(모델 `gpt-4.1`, 단가 입력 2.00 / 출력 8.00 USD per 1M 토큰, 주 25달러, `toy`·`sample-app`, `gitlab` 절 없음). 키는 사용자가 직접 넣음(터미널 붙여넣기가 막혀 클립보드 파이프로, 값은 대화·화면에 없음). 단계 2: `orchd` 정지 → DB 백업 `backup-w2b` → 바이너리 배포 → 드롭인 `override.conf`로 `--broker-socket` 추가 → 시작.

**서버에서 확인한 것.**
- `orch-broker -check` 통과(키 없을 때는 고정 문장으로 실패, 키 뒤 통과), 유닛 `active`, `systemd-analyze security` 2.0 OK(하드닝이 실제로 시작됨), 소켓 `srw-rw---- orch-broker orch`.
- `orch` 계정은 키 파일을 읽을 수 없고 `/etc/orch-broker`에 들어갈 수 없다(`denied`/`denied`). 리눅스 `SO_PEERCRED` 동작(소켓 `status` 호출 성공, 응답에 키 없음).
- 키 없이 `api.openai.com` 접속 → 401(도달). 방화벽 규칙은 컷오버 전과 같음(카운터 제외).
- DB 스키마 v8, 작업 목록 배포 전후 동일, 승인 모드 signed 유지.
- 웹: 미리보기 성공(orchd가 sandbox 아래에서 broker 소켓에 연결됨), 실제 OpenAI 호출 성공(broker 로그 `op=refine ok`, 약 7초), 체크 해제 항목이 최종 요청 글의 `Do not`에 들어감(사용자가 화면에서 확인, 저장된 작업 요청 글에서도 확인), 작업 #46 제출 → `done`(약 2분), 원장에 `refine`(내용 없음)·`merge_request skipped`, 예산 파일에 실사용량 정산(입력 454·출력 481 토큰 = 4756 마이크로달러, `gpt-4.1` 단가로 계산한 값과 일치).
- broker를 멈춘 상태(3-7): 사용자가 미리보기에서 고정 오류를 보고 기존 기능이 동작함을 확인했다고 알려 옴(화면 문구와 작업 제출·취소 결과는 이 기록에 인용하지 않음, 서버에서 새 작업이 생겼는지는 확인하지 못함). broker는 다시 켰고 `status` 호출 성공.

**발견한 것(수정됨, 아래 7-1 참조).**
- `toy`에서 AI가 받은 파일이 0개였다(원장 `files:0, bytes:0`, 입력 454토큰). 파일 선택 규칙이 "README 또는 요구사항 단어와 경로가 겹치는 파일"인데 `toy`에는 README가 없고 한국어 요구사항이 영어 파일 이름과 겹치지 않았다(추정). 계획서는 한 줄만 보고 만들어졌다. 작은 저장소는 파일 전체를 보내는 편이 낫다. **수정됨(`d7a60a1`):** HEAD 트리가 40개 이하·합계 240 KiB 이하인 작은 저장소는 모든 파일이 후보가 된다(`why: small_repo`, 비밀·이진·큰 파일 제외 규칙은 그대로).

**이번 컷오버에서 확인하지 않은 것.** 3-6(허용 목록 밖 저장소: 시험할 저장소가 없음), 3-8·3-9(MR: 끔, `skipped`가 원장에는 기록됨. 작업 화면 문구는 사용자 확인 대기), 3-10(한도·예산 초과), 실제 GitLab 전부, 키보드·스크린 리더, 모델이 구조화 출력 스키마를 항상 지키는지(한 번 성공만 봄), 추론 모델 문제(`gpt-4.1`이라 해당 없음).

**남은 사용자 결정.** MR을 켤지(GitLab 토큰 요청 시 안내, `main` 보호 확인 필요), 파일 선택 규칙 개선 여부, 롤백 백업(`backup-w2b`, `*.prev-w2b`)을 언제 지울지(안정 확인 뒤).

### 7-1. 작은 저장소 전체 파일 전송 배포와 확인 (2026-11-09)
`main` d7a60a1을 병합하고 orch-vm의 `orchd`만 교체했다(이전 바이너리 `orchd.prev-small`, 스키마 v8·broker·유닛은 그대로). 배포 전 실행 중·대기 작업 없음을 확인했다.

**서버에서 본 것.** 같은 `toy`로 다시 AI 계획서를 만들자 원장 `refine` 줄이 `files:5, bytes:598, input_tokens:882, output_tokens:624, ok:true`였다(수정 전 호출은 `files:0, input_tokens:454`). 이 호출의 비용은 6756 마이크로달러로 기록됐고 `gpt-4.1` 단가(882×2 + 624×8 USD per 1M 토큰) 계산과 같다. 누적 지출은 약 0.0115달러, 남은 주 예산 24.988달러, 시간당 남은 호출 19회. 원장 줄에는 이번에도 글 내용이 없다.

**사용자가 화면에서 확인한 것(제가 직접 보지 않음).** 미리보기에서 파일 목록을 확인했고, 계획서가 정상으로 떴다. 계획서가 실제 코드(`calc.go` 등)를 반영했는지 판단은 사용자의 확인에 의존하며 서버 기록으로는 검증하지 못했다.

**여전히 미확인.** 위 7장의 "이번 컷오버에서 확인하지 않은 것" 목록과 같다(실제 GitLab·MR, 한도·예산 초과 실동작, 허용 목록 밖 저장소 안내, 키보드·스크린 리더, 구조화 출력 스키마 준수의 반복 안정성).

**정리 대기.** 롤백 백업(`backup-w2b`, `*.prev-w2b`, `orchd.prev-small`)은 안정 확인 뒤 지운다(아직 지우지 않음).

