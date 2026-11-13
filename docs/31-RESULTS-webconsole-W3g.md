# 웹 콘솔 W3g — 결과 기록 (2026-11-13)

> 상태: **구현 완료(브랜치 `impl/webconsole-w3g`), 배포하지 않음.** 코드와 시험은 이 저장소에서 끝났고, 실제 서버(orch-vm)와 실제 GitHub·GitLab에는 아무것도 하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 17장, 계획은 `30-PLAN-webconsole-W3g.md`. 출발점은 W3f 컷오버가 끝난 서버(main `263d0e1`, 등록부 v4, 제공자 `example-group-gitlab`·`swlee3306-github`, 프로젝트 `toy`·`sample-app`(legacy)만: `29-RESULTS-webconsole-W3f.md` 9장)다.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더·명백한 더미 값**)과 G3 구현자가 ego-browser로 개발용 고정 서버를 본 것(G3 보고서, 패스키는 대역)만 가리킨다. **실제 GitHub·GitLab에서 배포 키를 떼는 `DELETE`, 실제 패스키로 만든 `detach_key` 서명, `also_local`로 등록이 같은 서명에 지워지는 것, 리눅스 systemd 샌드박스 안의 동작은 한 번도 하지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다.
> **W3g는 W3f의 삭제를 "서명 한 번"으로 줄인다.** 새 DELETE 호출 행은 없고(허용 목록은 W3f 그대로, 시험으로 고정), 등록부 버전도 v4 그대로다. 바뀐 것은 기존 호출의 **순서와 서명 범위**다: 서명이 키 떼기(`remove_project.detach_key`)와 등록 지우기(`delete_remote_repo` + `also_local`)까지 덮는다.

## 1. 만든 것

**G1. 서명 정의와 broker.** `remove_project`의 서명 필드가 `scope`·`legacy_ack`·`detach_key` 셋이 된다(모두 필수, 정확한 키 집합, `detach_key`는 JSON bool). broker는 `detach_key=true`일 때 모양 단계(서명 검증·요청 ID 소모 전)에서 행에 제공자·원격(번호 포함)이 있고 `remote_deleted_at`이 없으며 orchd가 보낸 공개 키 줄(`deploy_key`, 서명되지 않는 인자)이 키 모양인지 보고, 서명 검증 뒤 잠금(프로젝트 → 원격) → 등록부 다시 읽기 → 저장소 GET(번호·이름) → 키 목록 GET → **키 식별(5장 R-1)** → 한도 → 키 `DELETE` 한 번 → 성공일 때만 등록 행 삭제. 키 `DELETE`가 거부되면 `remove_refused`(429는 `provider_rate_limited`), 결과를 알 수 없으면(시간 초과·5xx·문서에 없는 2xx·리다이렉트·연결 끊김·`DELETE` 뒤 패닉·`DELETE` 뒤 등록부 쓰기 실패) `delete_partial`이고 **어느 경우에도 등록 행은 남는다**(자동 재시도 없음). 답은 `{"id","removed":true,"key_state":"removed"|"already_gone"}`. `detach_key=false`는 W3f와 바이트 단위로 같다. `delete_remote_repo`에 `also_local=true`가 서명되면 `DELETE`가 확실히 끝난 뒤 **등록 행을 지운다**(`sameRowCore` 재확인, 실패하면 `delete_partial`·행 유지, 답에 `registry_removed:true`). `also_local=false`는 W3f 그대로(행이 `remote: 없음`+`remote_deleted_at`, `Provider` 유지). broker의 `remove_project` 시간 제한 10초 → 60초. 성공 로그 한 줄(프로젝트 id만).
**G2. orchd.** `remove_project`의 propose·apply가 `detach_key=true`면 키 줄을 **프로젝트의 키 폴더(`keys/<id>/id_ed25519.pub`, GitHub) 또는 공용 키의 `.pub`(GitLab)에서만** 읽고(요청 데이터의 `deploy_key`는 어디에 있든 `bad_request`), 읽을 수 없으면 서명 전에 `key_unavailable_for_removal`(409), 의미 없는 행(G-D3)은 `bad_request`(400), 원격 번호가 없으면 `remote_changed`(409). 키 줄은 propose와 apply(`pre`, broker 호출 전, 그러니 폴더 삭제 전)에서 새로 읽는다. 미리보기·제안 응답에 `detach_key_available`·`deploy_key_title`·`deploy_key_fingerprint`·`deploy_key_shared`. broker 답 확인: 서명에 `detach_key`가 있으면 `key_state ∈ {removed, already_gone}`이 있어야 하고 없으면 없어야 한다, `also_local`이 서명되면 `registry_removed === true`여야 하고 아니면 없어야 한다. 그 밖의 답은 `outcome_unknown`(504)이고 로컬 파일을 건드리지 않는다. 그 뒤에만 W3f의 `removeLocal`. `detach_key=true`는 로컬 정리 한도와 키 떼기 한도(각각 시간당 10)를 둘 다 쓴다. orchd의 client 시간 제한 15초 → 70초.
**G3. 웹.** "복제본과 등록 지우기" 미리보기에 체크 "배포 키도 함께 떼기"(`#del-detach`): 미리보기가 `detach_key_available: true`이고 행이 그럴 수 있을 때만(원격 번호 있음, `remote_deleted_at` 없음) 보이고 **기본 체크**(R-2의 예외 하나). 저장소(경로·주소·제공자)와 키(GitHub은 제목, GitLab은 지문만: 공용 키는 첫 프로젝트의 제목일 수 있으므로 제목을 주장하지 않음)를 보이고, 끄면 굵은 "[중요] 키가 원격에 남습니다. 직접 지우세요"(미리보기·확인·결과). 확인 화면의 서명 게이트가 `detach_key`까지 다시 계산하고 제안의 키 정체(제목·지문·공용 여부)가 미리보기와 같아야 한다. `also_local=true` 결과에는 "등록 지우기" 후속 서명 안내가 없고(등록까지 지워짐), `also_local=false`와 W3f 때의 `remote_deleted_at` 행에는 그대로 있다. 결과 모양 검사(`okShape`)가 서명과 맞지 않는 성공 답을 "결과를 확인하세요"로 보인다. 오류 문장(5장 R-3).
**G4(이 작업).** 적대 시험 6개(broker 2, orchd api 3, node 1), 변이 시험 37개(3장), 이 문서, 런북 17장, 16장의 함정 안내, 문서 19 상태 줄.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-13, macOS(Apple Silicon), `go version` 1.27.1, Node v26.5.0. 세는 방식은 W3a~W3f와 같다(최상위 시험 함수와 하위 시험을 따로, `go test -json` 결과 줄에서 집계). 이 숫자는 **G4의 시험을 더한 작업 트리 위에서 처음부터 다시 돌린 전체 실행**의 것이고 변이 실행과 겹치지 않게 돌렸다(문서만 그 뒤에 바뀐다).
- `gofmt -l internal cmd`: 출력 없음. `go vet ./...`, `GOOS=linux go vet ./...`, `GOOS=darwin go vet ./...`: 통과.
- `go test ./internal/... ./cmd/... -count=1 -race -json`(브리프의 명령: 29번 문서의 `./...`와 달리 **`e2e` 패키지를 포함하지 않는다**): **종료 코드 0**, 오류 출력 없음. 시험이 있는 38개 패키지 모두 `ok`. 최상위 시험 함수 **1787 통과, 0 실패, 2 건너뜀**(하위 시험 858개 모두 통과). 건너뜀 2개는 이전과 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollisionsPerformance`(-race에서 건너뛰는 시간 검사). 시험 파일이 없는 패키지 4개(`internal/approval/ui`, `internal/gittest`, `internal/orch/fsx`, `internal/refinesize`)는 표에서 뺐다.
- `node --test "internal/orch/web/test/*.test.js"`: **317개 전부 통과**(0 실패, 0 건너뜀: G3 보고서의 316개 + G4의 1개).
- **게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, 하위 `tokens`·`providers`·`w3e`·`w3f` 모두 PASS, **`SKIP` 없음**.
- 29번 문서(W3f, `./...`)와 직접 비교하지 않는다(`e2e`의 최상위 9개·하위 147개가 빠진 명령). 같은 명령 기준 비교 값은 없다. G4가 더한 시험은 6개(Go 5, node 1).

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 14 | 0 | 0 | 0 |
| `cmd/orchd` | 34 | 0 | 0 | 0 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 70 | 0 | 0 | 76 |
| `internal/broker` | 436 | 0 | 0 | 297 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 52 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 426 | 0 | 0 | 35 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 12 | 0 | 0 | 0 |
| `internal/orch/clone` | 39 | 0 | 0 | 0 |
| `internal/orch/keys` | 14 | 0 | 0 | 0 |
| `internal/orch/knownhosts` | 4 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 27 | 0 | 0 | 0 |
| `internal/orch/refine` | 17 | 0 | 0 | 0 |
| `internal/orch/remove` | 9 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 44 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 11 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/registrymsg` | 38 | 0 | 0 | 6 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

### G4가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3g_test.go`(2개), orchd `internal/orch/api/adversarial_w3g_test.go`(3개), node `internal/orch/web/test/delete.test.js` 끝부분(1개, W3f F5와 같은 이유로 같은 파일에 덧붙임: 화면 시험 도우미가 그 파일 안에만 있다). G1~G3가 이미 같은 일을 하는 시험은 복사하지 않고 **전체 흐름 위의 얇은 시험**만 새로 썼다(별칭 시험 없음).

| 시험 | 하는 일 |
|---|---|
| `TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g` (broker) | 21가지 흐름(GitHub 키 떼기 성공·403·500·200·제목만 같은 키·본문만 같은 키·둘 다 같은 키 둘·이미 없음·목록 500·라이브 번호 불일치, GitLab 공용 키(첫 프로젝트 제목, 본문으로 찾음)·자기 키·본문 일치 둘·제목만 같은 키·500, `also_local` GitHub·GitLab 성공·500·403·라이브 이름 불일치, `also_local=false`)의 모든 요청이 **시험 안에 다시 쓴 8행**에 들어 있고, 흐름마다 `DELETE` 수가 정확히 허용된 수(0 또는 1)이며, **등록 행은 ok 답일 때만 사라지고**(`also_local=false`는 남음), 다른 프로젝트의 행·다른 제공자는 그대로, 어떤 로그에도 키 본문·원격 경로·상태 폴더·토큰이 없다. 다섯 가지 `DELETE` 모양(GitHub 저장소·키 2, GitLab 프로젝트·키 8·키 9)이 모두 실제로 나갔다(비어 있지 않은 검사) |
| `TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g` (broker) | **유효한 `detach_key` 서명**에 서명되지 않는 키 줄로 같은 저장소의 다른 키(다른 도구의 키, "my laptop", CI 키, 목록에 없는 키, 주석을 붙인 남의 키)를 주면: `DELETE`는 서명된 프로젝트의 키 경로 밖으로 나가지 않고, 한 번을 넘지 않고, 준 줄과 본문이 다른 키를 떼지 않는다. **GitHub은 전부 `remove_refused`(제목 `orchd demo-gh`가 목록에 있으므로 "닮은 키"), 아무것도 떼지 않고 행이 남는다.** 재전송은 `request_reused`, 다른 제공자는 연락받지 않는다. **GitLab은 제목을 보지 않으므로(R-1) 서명되지 않은 줄이 그 프로젝트의 어느 키를 뗄지 고른다**: "my laptop" 키(10)를 주면 그 키가 그 프로젝트에서 떼어지고 등록이 지워지며 `orchd demo-gl` 키는 남는다. 이 남은 위험을 시험이 그대로 고정한다(바뀌면 시험이 알린다, 7장 1) |
| `TestLocalFilesOnlyAfterTheAnswerThatMatchesTheSignatureW3g` (orchd) | 서명 네 가지(`remove_project` 떼기 켬/끔, `delete_remote_repo` `also_local` 켬/끔) × broker 답 12가지(각 모드의 ok 답, 다른 프로젝트에 대한 답, 두 op의 구성원을 함께 가진 답, `delete_partial`·`remove_refused`·`request_reused`·`internal_error`) = 48경우. **서명한 모드의 답일 때만** 200이고 그때만 복제본·키 폴더가 지워지며(`also_local=false`는 지우지 않음), 그 밖의 모든 답은 200이 아니고 그 프로젝트의 파일 트리가 바이트 단위로 같고 원장에 성공이 없다. 옆 프로젝트의 파일은 어떤 경우에도 그대로 |
| `TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g` (orchd) | 키 떼기의 두 한도(로컬 정리·키 떼기)가 **아무 일도 없었음이 증명될 때만**(broker에 닿지 못함, `busy`, `forbidden`) 둘 다 돌아오고, 무언가 일어났을 수 있으면(`remove_refused`·`delete_partial`·`request_reused`·옛 broker의 `bad_request`·`unknown_op`·broker의 `rate_limited`·믿을 수 없는 ok 답) 둘 다 쓰인다. 떼기를 끈 삭제는 키 한도를 건드리지 않는다. 키 한도가 가득 차면 먼저 잡은 로컬 한도가 돌아오고 broker에 닿지 않는다 |
| `TestSharedKeyLineWithASpacedCommentIsUnavailableBeforeSignatureW3g` (orchd) | GitLab 공용 키의 `.pub` 주석에 공백(또는 탭)이 있으면 broker의 키 줄 규칙 밖이므로: 미리보기가 선택지를 주지 않고(`detach_key_available:false`), 떼기를 켠 제안은 **서명 전에** `key_unavailable_for_removal`(409), 끄면 진행된다. 한 단어 주석·주석 없음은 된다. broker 쓰기 0. (런북 17.0 (e)의 사전 점검 근거) |
| node `TestDetachToggledAfterProposeNeverChangesWhatIsSignedW3g` | 제안 뒤(확인 화면에서) 미리보기의 체크 상자를 스크립트·엇나간 이벤트로 뒤집어도(켬→끔, 끔→켬) **확인 화면은 제안의 값을 그대로 보이고, 서명·적용되는 `detach_key`와 `change`는 제안의 것**이며 몰래 새 제안을 하지 않는다(관찰: 화면은 확인 단계에 머문다) |

**이미 G1~G3의 시험이 증명하는 것은 복사하지 않는다.** 브리프의 속성이 어느 시험에 있는지의 지도:

| 속성 | 시험 |
|---|---|
| `detach_key`가 서명 범위 안(빠지거나 다르면 거부), 브라우저·orchd·broker가 같은 값으로 다시 계산 | registrymsg `TestRemoveProjectDetachKeyFieldInSchema`, broker `TestRemoveProjectDetachKeyOutsideSignatureRefused`·`TestChangeHashVectorsForRemoveProjectW3g`·`TestCrossActionSignaturesStillRefusedW3g`(17동작 272쌍 + 같은 주체의 `detach_key` 접붙이기 양방향), `TestSignedFieldsCoverEveryDeleteInput`, orchd `TestApplyRecomputesChange`·`TestLimitsDetachKey`(문자열 `"true"`), 웹 `TestSignGateRecomputesChangeWithDetachKey`·`TestChangeVectorsMatchGoW3g`, 상수 일치 `TestConstantsAgreeWithWeb` |
| 키 떼기 실패·불확실이면 등록을 지우지 않음, 재시도 없음, 같은 서명 재사용 불가, 불확실 뒤 새 서명으로 복구 | `TestDetachFailureKeepsRegistration`(14가지), `TestDetachUncertainThenFreshSignatureRecovers`, `TestDetachRegistryWriteFailureIsPartialAndRecoverable`, `TestDetachPanicNeverRecordsFalseOutcome`, 새 broker 시험 1 |
| 본문 비교(GitHub 제목+본문, GitLab 본문), 닮은 키 → `remove_refused`, 둘 이상 → 거부 | `TestDetachKeyNotFoundContinues`, `TestDetachMultipleOrMismatchedKeyNeverDeleted`, `TestDetachGitLabSharedKeyMatchedByBody`, `TestDeleteKeyOnlyMatchingTitleAndFingerprint`·`TestGitLabKeyRemovalOnlyThisProjectsKeyId`(W3f `remove_deploy_key`도 같은 규칙), 새 broker 시험 둘 |
| `also_local=true`는 행을 지우고 `false`는 지우지 않음, W3f 안전 규칙(출처·번호·이름·확인 이름) 유지 | `TestDeleteRemoteAlsoLocalRemovesRow`, `TestDeleteRemoteWithoutAlsoLocalKeepsRowAndProvider`, `TestAlsoLocalRegistryWriteFailureIsPartial`, `TestAlsoLocalSurvivesFlagFlipButNotARealRowChange`, 새 broker 시험 1 |
| 순서(키 목록 → 키 `DELETE` → 등록 지움; orchd는 키 줄을 폴더 삭제 전에 읽음) | `TestRemoveProjectDetachesThenUnregisters`(키 `DELETE` 도중 행이 아직 있음), orchd `TestKeyLineReadBeforeLocalRemoval`(broker 호출 순간에 키 줄·키 폴더·복제본이 모두 있음), `TestAlsoLocalRemovesLocalAfterBrokerRowRemoval` |
| orchd는 `registry_removed`/`key_state` 답 없이 로컬 파일을 지우지 않음 | `TestApplyForwardsDetachKeyAndKeyLine`, `TestAlsoLocalRemovesLocalAfterBrokerRowRemoval`, `TestMixedVersionOldBrokerW3g`, 새 orchd 시험 1 |
| 키 줄은 요청에서 받지 않음(필드·최상위·apply 본문), 키 폴더 밖의 `.pub`·심볼릭 링크 무시 | `TestRemoveProposeReadsKeyLineOnlyFromKeyFolder`, broker `TestRemoveProjectDetachKeyShapeRules`(키 줄 없이·떼기 없이 키 줄) |
| G-D3(원격 없음·레거시·`remote_deleted_at`·제공자 없음)은 서명 전 거부, ID 미소모 | `TestRemoveProjectDetachKeyShapeRules`, orchd `TestDetachKeyUnavailableRefusedBeforeSignature` |
| 웹 기본 체크, 원격이 없는 행에는 선택지 없음, 끄면 경고, `delete_partial` 뒤에는 기본 꺼짐 | `TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked`, `TestUncheckedShowsKeyRemainsWarning`, `TestDetachDefaultsOffWhenRemoteMaybeGoneR1`, `TestFollowUpStillOfferedForRemoteDeletedRows`, 새 node 시험 |
| 웹 결과: `also_local`에 후속 서명 없음, 서명과 맞지 않는 성공 답은 중립 화면 | `TestAlsoLocalResultHasNoFollowUpSignature`, `TestRemoveResultShowsKeyStateW3g`, `TestPartialDetailKeyMembersMatchTheSignedChangeR1` |
| 원장·로그에 비밀·키 본문 없음 | broker `TestCanaryTokenNeverInAnyOutputW3g`·`TestSuccessLogsCarryOnlyTheProjectId`, orchd `TestLedgerNoSecretsW3g`, 새 broker 시험 1 |
| 한도: 둘 다 쓰고 아무 일 없을 때만 둘 다 돌려줌, broker 쪽 한 번만 | orchd `TestLimitsDetachKey`, broker `TestDetachRateLimitAndLocks`, 새 orchd 시험 1 |
| 허용 목록에 새 DELETE 행 없음, broker 시간 제한 | `TestNoNewDeleteRowsInAllowlist` |
| 혼합 버전(옛 broker `unknown_op`/`bad_request` → `broker_unavailable`, W3f 모양 `also_local` 답 → `outcome_unknown`) | `TestMixedVersionOldBrokerW3g`, `TestBadRequestOnOtherDeleteActionsUnchanged` |
| 동시 요청은 키 `DELETE` 하나 | `TestConcurrentDetachSendsOneKeyDelete` |

**시험을 새로 쓰는 중 발견한 것.** (a) **제품 코드의 결함은 발견하지 못했다.** 제품 코드는 바꾸지 않았다. (b) **GitLab의 남은 위험(7장 1)을 시험으로 확인했다**: 제목을 보지 않는 규칙(R-1) 때문에 서명되지 않는 키 줄이 그 프로젝트의 어느 키를 뗄지 고른다. 정상 orchd는 자기 `.pub`만 보내므로 일어나지 않지만, W3f 때(제목+본문)보다 broker 혼자의 보장이 좁아졌다. 결정은 사용자·컨트롤러 몫으로 남긴다(8장). (c) 시험 작성 중 정정: 행이 지워진 뒤의 재전송은 `request_reused`가 아니라 모양 단계의 `project_unknown`이다. orchd는 broker 답에서 자기가 물은 op의 구성원만 읽는다(다른 op의 구성원이 함께 있어도 서명과 모순되지 않으면 받아들인다: 시험에 그대로 적었다). 체크 상자를 확인 화면에서 뒤집었을 때 화면이 미리보기로 돌아가는지 확인 단계에 머무는지는 처음에 열어 두었다가, 관찰(머문다)에 맞춰 엄격하게 고정했다.

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖(`/tmp/aigate-mut-w3g/copy`)으로 복사**(`rsync`, `.git`·`.superpowers`·`graphify-out` 제외)해 한 곳만 망가뜨리고 관련 시험을 돌렸다. 변이마다 복사본의 원래 파일로 되돌렸고(다음 변이 전에 복사본 파일이 원본과 같은지 확인) 작업 트리는 건드리지 않았다. 도구는 `/tmp/aigate-mut-w3g/mutate.py`(순차 실행, 정확한 바꿈 문자열은 `mutants.py`, 바꿀 문자열이 파일에서 정확히 한 번 나와야 실행). 먼저 같은 명령을 변이 없이 돌려 모두 통과함을 확인했다(broker·registrymsg·api `ok`, node 3파일 115개 통과). 전부 G4 구현자 실행이다(검토자가 아직 재현하지 않음). 패키지 전체가 아니라 아래 `-run` 부분집합(시간 제한 240초)을 썼다. broker: `go test ./internal/broker -run 'W3g$|W3f$|Detach|Remove|Delete|Cross|Allowlist|Canary|GitLab|Key|AlsoLocal|Signed|Success|Concurrent|Registry'`, G02만 `./internal/registrymsg`를 더함, orchd: `go test ./internal/orch/api -run 'W3g|W3f|Remove|Delete|Detach|Limit|Ledger|Key|Order|Partial|Apply|Propose|AlsoLocal|Preview|Mixed|Constants'`, 웹: `node --test`로 `delete.test.js`·`providers.test.js`·`projects.test.js`. 굵게 = G4의 새 시험. 실패한 시험이 6개를 넘으면 처음 6개와 나머지 개수만 적었다.

| # | 망가뜨린 곳 | 패키지 | 결과 | 실패한 시험 (개수) |
|---|---|---|---|---|
| G01 | broker의 서명 필드에서 `detach_key`를 뺌(서명 범위) | broker | 잡힘 | TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestLocalDeleteHourlyLimit, TestRemoveProjectLegacyAckBothDirections, TestSignedFieldsCoverEveryDeleteInput, **TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g** (6) |
| G02 | registrymsg 서명 정의에서 `detach_key`를 뺌(서명 범위) | registrymsg, broker | 잡힘 | TestChangeHashVectorsForRemoveProjectW3g, TestDeleteActionsInSchema, TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f, TestEveryDeletionRequestIsAllowlistedAndBoundToTheProjectW3f, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestLocalDeleteHourlyLimit … (+5) (11) |
| G03 | 키 `DELETE`의 거부·불확실을 무시하고 등록을 지움 | broker | 잡힘 | TestCanaryTokenNeverInAnyOutputW3g, TestDetachFailureKeepsRegistration, TestDetachUncertainThenFreshSignatureRecovers, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestSuccessLogsCarryOnlyTheProjectId (5) |
| G04 | 키 `DELETE` 뒤 등록부 쓰기 실패를 성공으로 답함 | broker | 잡힘 | TestDetachRegistryWriteFailureIsPartialAndRecoverable (1) |
| G05 | 순서 바꿈: 키 `DELETE` **전에** 등록 행을 지움 | broker | 잡힘 | TestCanaryTokenNeverInAnyOutputW3g, TestDetachFailureKeepsRegistration, TestDetachGitLabSharedKeyMatchedByBody, TestDetachPanicNeverRecordsFalseOutcome, TestDetachRegistryWriteFailureIsPartialAndRecoverable, TestDetachUncertainThenFreshSignatureRecovers … (+2) (8) |
| G06 | GitHub 키 정체를 제목만으로(본문 비교 제거) | broker | 잡힘 | TestDeleteKeyOnlyMatchingTitleAndFingerprint, TestDetachKeyNotFoundContinues, TestDetachMultipleOrMismatchedKeyNeverDeleted, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestRemoveProjectDetachKeyOutsideSignatureRefused, **TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g** (6) |
| G07 | GitLab 키 정체를 본문 대신 제목으로 | broker | 잡힘 | TestDetachGitLabSharedKeyMatchedByBody, TestDetachKeyNotFoundContinues, TestEveryDeletionRequestIsAllowlistedAndBoundToTheProjectW3f, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestGitLabKeyRemovalOnlyThisProjectsKeyId, **TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g** (6) |
| G08 | 닮은 키(우리 제목 또는 본문만 맞음)를 "이미 없음"으로 봄 | broker | 잡힘 | TestDeleteKeyOnlyMatchingTitleAndFingerprint, TestDetachGitLabSharedKeyMatchedByBody, TestDetachKeyNotFoundContinues, TestDetachMultipleOrMismatchedKeyNeverDeleted, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestGitLabKeyRemovalOnlyThisProjectsKeyId … (+2) (8) |
| G09 | 맞는 키가 둘이면 첫 번째를 뗌 | broker | 잡힘 | TestDeleteKeyOnlyMatchingTitleAndFingerprint, TestDetachGitLabSharedKeyMatchedByBody, TestDetachMultipleOrMismatchedKeyNeverDeleted, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g** (4) |
| G10 | `also_local=true`인데 행을 남김(W3f 동작) | broker | 잡힘 | TestAlsoLocalSurvivesFlagFlipButNotARealRowChange, TestDeleteRemoteAlsoLocalRemovesRow, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestSuccessLogsCarryOnlyTheProjectId (4) |
| G11 | `also_local=false`인데 행을 지움 | broker | 잡힘 | TestDeleteRemoteWithoutAlsoLocalKeepsRowAndProvider, **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestRemoteDeleteHappyPathAndRegistryEffects, TestRemoteDeleteSurvivesConcurrentFlagFlip (4) |
| G12 | `also_local` 행 삭제에서 의미 있는 필드 재확인 제거 | broker | 잡힘 | TestAlsoLocalSurvivesFlagFlipButNotARealRowChange (1) |
| G13 | `detach_key` 없이 온 키 줄을 받아들임 | broker | 잡힘 | TestRemoveProjectDetachKeyShapeRules (1) |
| G14 | 떼기 경로가 한도 칸을 쓰지 않음 | broker | 잡힘 | TestDetachRateLimitAndLocks (1) |
| G15 | 성공 로그에 키 본문 | broker | 잡힘 | **TestEveryDetachAndAlsoLocalRequestIsAllowlistedAndBoundW3g**, TestSuccessLogsCarryOnlyTheProjectId (2) |
| G16 | `remote_deleted_at` 행의 떼기 거부 제거 | broker | **살아남음(동등 변이)** | — (0) |
| H01 | orchd: `key_state` 답 확인 제거 | orchd api | 잡힘 | TestApplyForwardsDetachKeyAndKeyLine, **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestLedgerNoSecretsW3g, **TestLocalFilesOnlyAfterTheAnswerThatMatchesTheSignatureW3g** (4) |
| H02 | orchd: `registry_removed` 답 확인 제거 | orchd api | 잡힘 | TestAlsoLocalRemovesLocalAfterBrokerRowRemoval, **TestLocalFilesOnlyAfterTheAnswerThatMatchesTheSignatureW3g**, TestMixedVersionOldBrokerW3g (3) |
| H03 | orchd 순서 바꿈: 키 줄을 읽기 전에 키 폴더를 지움 | orchd api | 잡힘 | TestApplyForwardsDetachKeyAndKeyLine, TestDetachKeyUnavailableRefusedBeforeSignature, **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestDetachedThenLocalFailureReportsKeyStateAndLedger, TestKeyLineReadBeforeLocalRemoval, TestLedgerNoSecretsW3g … (+10) (16) |
| H04 | orchd 순서 바꿈: 답을 확인하기 전에 로컬 파일을 지움 | orchd api | 잡힘 | TestApplyForwardsDetachKeyAndKeyLine, **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestKeyLineReadBeforeLocalRemoval, TestLedgerNoSecretsW3g, **TestLocalFilesOnlyAfterTheAnswerThatMatchesTheSignatureW3g**, TestRemoveLocalItemsAreIndependentAndStatesAreExact … (+1) (7) |
| H05 | orchd: apply 요청의 `deploy_key`를 broker에 보냄 | orchd api | 잡힘 | TestApplyForgedFieldsRefusedBeforeBrokerW3c, TestRemoveProposeReadsKeyLineOnlyFromKeyFolder (2) |
| H06 | orchd: 키 줄을 로그에 씀 | orchd api | 잡힘 | TestLedgerNoSecretsW3g (1) |
| H07 | orchd 환불: 첫 한도만 돌려줌 | orchd api | 잡힘 | **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestLimitsDetachKey (2) |
| H08 | orchd 환불: `delete_partial`·`remove_refused` 뒤에도 돌려줌 | orchd api | 잡힘 | TestDeleteLimits, **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestLimitsDetachKey (3) |
| H09 | orchd 환불: 둘째 한도가 찼을 때 첫째를 돌려주지 않음 | orchd api | 잡힘 | **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestLimitsDetachKey (2) |
| H10 | orchd: 떼기가 키 떼기 한도를 잡지 않음 | orchd api | 잡힘 | **TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g**, TestLimitsDetachKey (2) |
| H11 | orchd: GitHub 키 줄을 키 폴더 대신 공용 키에서 | orchd api | 잡힘 | TestApplyForwardsDetachKeyAndKeyLine, TestDetachKeyUnavailableRefusedBeforeSignature, TestKeyLineReadBeforeLocalRemoval, TestPreviewDetachKeyAvailableAndFingerprint, TestRemoveKeyFlow, TestRemoveProposeReadsKeyLineOnlyFromKeyFolder … (+1) (7) |
| X01 | 웹: 체크 상자가 기본 꺼짐 | node | 잡힘 | TestConfirmationNamesRepoAndKeyTitleAndFingerprint, TestDetachA11yAndLegacyScreenR1, TestDetachDefaultsOffWhenRemoteMaybeGoneR1, TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked, TestDetachRefusedAfterSignatureSaysNothingChangedR1, **TestDetachToggledAfterProposeNeverChangesWhatIsSignedW3g** … (+5) (11) |
| X02 | 웹: 이번 방문에 `delete_partial`이 있었어도 기본 켬(R-2 제거) | node | 잡힘 | TestDetachDefaultsOffWhenRemoteMaybeGoneR1 (1) |
| X03 | 웹: `detach_key`를 항상 false로 보냄 | node | 잡힘 | TestConfirmationNamesRepoAndKeyTitleAndFingerprint, TestDetachDefaultsOffWhenRemoteMaybeGoneR1, TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked, TestDetachRefusedAfterSignatureSaysNothingChangedR1, **TestDetachToggledAfterProposeNeverChangesWhatIsSignedW3g**, TestPartialDetailKeyMembersMatchTheSignedChangeR1 … (+3) (9) |
| X04 | 웹: SCHEMA에서 `detach_key`를 뺌(브라우저 해시) | node | 잡힘 | TestAdversarialSelfConsistentForgedProposalIsNeverSignableW3f, TestAllNewErrorCodesFixedTextsW3f, TestChangeVectorsMatchGoW3f, TestChangeVectorsMatchGoW3g, TestConfirmationNamesRepoAndKeyTitleAndFingerprint, TestDeletePartialScreen … (+20) (26) |
| X05 | 웹: `also_local` 답의 `registry` 확인 제거 | node | 잡힘 | TestAlsoLocalResultHasNoFollowUpSignature (1) |
| X06 | 웹: 삭제 답의 `key_state` 확인 제거 | node | 잡힘 | TestRemoveResultShowsKeyStateW3g (1) |
| X07 | 웹: `also_local` 결과에 후속 서명 안내를 다시 보임 | node | 잡힘 | TestAlsoLocalResultHasNoFollowUpSignature, TestRemoteDeleteSignedFieldsAndAlsoLocalSingleSignature (2) |
| X08 | 웹: 미리보기의 가용 여부 없이 선택지를 보임 | node | 잡힘 | TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked, TestRemoteKeyMayRemainHint (2) |
| X09 | 웹: 미리보기의 "키가 원격에 남습니다" 경고 제거 | node | 잡힘 | TestDetachA11yAndLegacyScreenR1, TestDetachRefusedAfterSignatureSaysNothingChangedR1, **TestDetachToggledAfterProposeNeverChangesWhatIsSignedW3g**, TestNoStorageNoNavigateTrap, TestPartialDetailKeyMembersMatchTheSignedChangeR1, TestRemoveFlowErrorTextsW3g … (+3) (9) |
| X10 | 웹: 확인 화면이 제안 대신 지금의 체크 상자를 보임 | node | 잡힘 | **TestDetachToggledAfterProposeNeverChangesWhatIsSignedW3g** (1) |

**37개 중 36개가 잡혔고 1개(G16)는 동등 변이로 살아남았다.** 솔직히 기록할 것:
1. **G16은 동등 변이다**(G1 보고서의 M6과 같은 것): 등록부 검증기가 `remote_deleted_at`과 원격을 함께 가진 행을 허락하지 않으므로, 그 행은 앞의 `Remote == nil` 검사에서 이미 거부된다. 지운 검사는 겹친 방어이고 유효한 등록부로는 시험할 수 없다.
2. **G08은 처음에 컴파일 오류**(`look` 변수가 안 쓰임)였고 `false &&`로 다시 쓴 변이의 결과가 위 표다.
3. **G01 실행은 10분 시간 제한에 걸렸다**(첫 실행은 `-timeout`이 없었다): 서명 필드가 어긋나 모든 `remove_project`가 `bad_request`가 되자 W3f의 `TestConcurrentDeleteOfOneProjectSendsOneDelete`가 끝없이 기다렸다(실패 경로에 시간 제한이 없는 기존 시험). 그 전에 6개 시험이 이미 실패했으므로 "잡힘"이다. 이 일 뒤 모든 명령에 `-timeout 240s`를 붙여 G02부터 다시 돌렸다.
4. **X10은 G4의 새 node 시험만 잡는다**(확인 화면이 제안 대신 지금의 체크 상자를 보이는 변이). 시험은 변이 실행 전에 썼으므로 "변이로 찾은 빈틈"은 아니지만, 이 시험이 없으면 살아남는 변이다. H01·H03·H04에서도 새 orchd 시험이 함께 잡는다.
5. 웹 변이의 실패 목록에서 node 출력의 `failing`(요약 머리글)은 뺐다.
6. **제품 코드는 하나도 바꾸지 않았다.** G1~G3 보고서의 변이(G1 M1~M15·N1~N10, G2 M1~M7·R1~R4·D·G·S, G3 M1~M16·R1~R9)는 이번에 다시 돌리지 않았다(각 보고서에 기록).

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

**이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험에 기초한 것이고 실서버에서 본 것이 아니다.** 실행 사용자와 샌드박스는 W3f와 같다(29번 4장: broker `orch-broker` `ProtectHome=yes`, 쓰기 `/var/lib/orch-broker`·`/run/orch-broker`만; orchd `orch` `ProtectHome=read-only`, `ReadWritePaths=/home/orch/orchd /home/orch/repos`, `PrivateTmp=yes`). **새 경로·새 파일·새 유닛·새 플래그·새 설정 키·새 포트·DB 스키마 변경(v8)·등록부 버전 변경(v4)은 없다.**

| 경로·대상 | 소유자 / 모드 | 접근하는 쪽 | W3g에서 달라지는 것 | 비고 |
|---|---|---|---|---|
| `/var/lib/orch-broker/registry.json` | `orch-broker` 0600 | broker만 | **버전 그대로(v4), 마이그레이션 없음.** `remove_project`(떼기 켬)는 키 `DELETE`가 확실히 끝난 뒤에만 행을 지우고, `delete_remote_repo`+`also_local`은 저장소 `DELETE` 뒤 행을 지운다(W3f는 `remote: 없음`으로 남김) | 롤백에 등록부 복원이 필요 없다(W3f broker도 v4를 읽는다). 단 W3g로 지운 행은 롤백해도 되살아나지 않는다 |
| `/home/orch/orchd/ssh/keys/<id>/id_ed25519.pub` | `orch:orch` 0644 | orchd(읽기만) | GitHub 키 떼기의 키 줄을 **propose와 apply에서 읽는다**(apply는 broker 호출 전, 그러니 폴더 삭제 전). 개인 키는 열지 않는다 | 없거나 모양이 다르면 `key_unavailable_for_removal`(서명 전) |
| `/home/orch/orchd/ssh/id_gitlab_deploy.pub` (`--git-ssh-key`의 `.pub`) | `orch:orch` | orchd(읽기만) | GitLab 키 떼기의 키 줄(공용 키). **주석에 공백이 있으면 broker 규칙 밖이라 떼기가 불가**(`key_unavailable_for_removal`) | 런북 17.0 (e) |
| 제공자 API | — | broker | 새 호출 없음: W3f의 저장소 GET·키 목록 GET·키 `DELETE`·저장소 `DELETE`를 **한 서명 안에서 이어서** 한다 | `remove_project` broker 시간 제한 **60초**(이전 10초) < orchd client **70초**(이전 15초) < orchd HTTP 서버의 쓰기 제한 **120초**(`WriteTimeout`, 실제 apply 상한) < 웹 기다림 **130초**(그대로) |
| `/home/orch/repos/<id>`, `…/keys/<id>/` | `orch:orch` | orchd | W3f와 같은 `removeLocal`(이름 바꾸기 → `RemoveAll`). `also_local`이면 broker가 **등록까지 지운 뒤**에 지운다 | 로컬 정리 실패 시 **등록 없는 폴더**가 남는다(G-D4, 런북 17장 복구) |
| orchd 원장 | 기존 | orchd | 이벤트 이름·키 그대로(`project_remove`·`remote_delete`, 키 `actor,action,subject,ok,code`). 키를 뗀 뒤 로컬 정리가 실패하면 `ok:true` 줄 다음에 `ok:false code:delete_partial_local` 줄(W3f의 `also_local`과 같은 모양) | 키 줄·지문·경로 없음(시험으로 고정) |
| broker 로그 | journald | broker | 성공 한 줄: `remove_project: removed project <id> from the registry`, `delete_remote_repo: removed the remote repository and the registry row of project <id>` | id만 |
| 웹 정적 파일 | orchd 바이너리 안 | orchd | `views/delete.js`(체크 상자·확인·결과·문장) | orchd를 갱신해야 새 화면이 보인다 |

## 5. 계획의 결정(G-D1~G-D7)과 작업 중 판정

| id | 결정 / 판정 | 틀렸을 때 비용 |
|---|---|---|
| G-D1 | `remove_project` 서명 필드 `detach_key`(bool, 필수). 서명 항목은 `scope`·`legacy_ack`·`detach_key` 셋. **W3f의 `remove_project` 변경 해시가 달라진다**(W3f 때 서명한 것은 W3g broker에서 `change_mismatch`·`bad_request`) | 서버에 보류 중인 서명이 있으면 다시 서명 |
| G-D2 | 키 떼기는 broker가 목록 → 식별 → `DELETE` → 성공이면 행 삭제. 거부·불확실이면 행 유지, 자동 재시도 없음 | `delete_partial`이면 사람이 제공자 화면을 보고 새로 서명 |
| G-D3 | 원격 없음·`remote_deleted_at`·제공자 없음(레거시 포함)에서 `detach_key=true`는 서명 전 `bad_request`. 웹은 선택지를 보이지 않음 | — |
| G-D4 | `also_local=true`면 broker가 `DELETE` 뒤 **행을 지운다**, orchd는 그 뒤에만 로컬 삭제. 로컬 삭제 실패 = 등록 없는 폴더(알려진 위험) | 남은 폴더는 다시 등록해 지우거나 사람이 지움 |
| G-D5 | 키 줄은 orchd가 키 폴더(GitHub)·공용 `.pub`(GitLab)에서만 읽고 요청에서 받지 않음, 미리보기에 `detach_key_available`·제목·지문, 한도 둘 다 | — |
| G-D6 | 새 오류 코드 없음(`bad_request`·`remove_refused`·`delete_partial`·`key_unavailable_for_removal` 재사용, `key_not_found`는 "이미 없음"으로 흡수) | — |
| G-D7 | 웹 체크 상자 기본 체크, 끄면 "키가 원격에 남습니다. 직접 지우세요", `also_local=true`면 후속 서명 안내 없음 | — |
| **R-1** (G1 검토 판정, 계획 G-D2 개정) | **키 정체: GitHub은 제목 `orchd <id>`와 키 본문이 둘 다 맞는 키, GitLab은 키 본문만(제목 무시: 공용 키는 첫 프로젝트의 제목으로 남는다).** 일치가 0개인데 목록에 **우리 제목 또는 우리 본문을 가진 키가 하나라도 있으면 "닮은 키"로 `remove_refused`**(행 유지: "찾지 못함"을 "이미 없음"으로 오해하지 않는다), 둘 다 없을 때만 "이미 없음"으로 계속. 둘 이상이면 `remove_refused`. **같은 규칙을 W3f `remove_deploy_key`에도 적용**(GitLab 두 번째 이후 프로젝트에서도 뗄 수 있게: 그래서 W3f의 GitLab "제목과 본문" 보장이 "본문"으로 좁아졌다) | GitLab: 서명되지 않는 키 줄이 키를 고른다(7장 1, 8장 1의 열린 결정). 키를 새로 만든 프로젝트는 옛 키(같은 제목, 다른 본문) 때문에 떼기가 거부된다(사람이 지운 뒤 다시) |
| **R-2** (G3 검토 판정) | **이 페이지 방문 중 그 행의 원격 삭제가 `delete_partial`로 끝났으면 체크 상자가 기본 꺼짐**(안내 "원격 저장소가 이미 지워졌을 수 있습니다…"), 체크하면 서명된다. 새 방문은 다시 기본 체크(메모리에만) | 저장소가 아직 있으면 한 번 더 누름 |
| R-3 (문장) | `delete_partial`(떼기): "배포 키 떼기가 일부만 처리되었거나 결과를 알 수 없습니다(키는 이미 떼어졌을 수 있습니다). 프로젝트 등록과 서버의 복제본은 지우지 않았습니다…"(원격 삭제 화면과 따로). 떼기를 켠 `outcome_unknown`: "키는 이미 떼어졌을 수 있습니다". 서명 뒤 떼기 거부(`remove_refused`·`remote_changed`·제공자 오류 등): "아무것도 지우지 않았습니다… 키를 남기고 지우려면 '배포 키도 함께 떼기'를 끄고 새 변경에 서명하세요". `key_unavailable_for_removal`(삭제 흐름): 선택지를 끄고 진행할 수 있다. 떼기 뒤 로컬 정리 실패(`delete_partial_local`): 상세에 `key_state`·`key_detached:true`, 문장에 "배포 키는 이미 떼어졌고 등록도 지워졌습니다", 복구 안내는 "경로를 다시 등록 → 떼기를 끄고 '복제본과 등록 지우기'" | 문장만 |
| R-4 (혼합 버전) | broker의 `unknown_op`·`not_implemented`(모든 삭제)와 `bad_request`(`remove_project`만: orchd가 모든 모양 규칙을 미리 보므로 broker의 모양 거부는 W3f broker이거나 그사이 상태가 바뀐 것)는 `broker_unavailable`(503) + "아무것도 지우지 않았습니다" 문장. 새 코드 없음 | 상태가 바뀐 경우에도 "버전" 문장이 함께 보인다 |
| R-5 (시간 제한) | broker `remove_project` 10초 → 60초(저장소 GET + 키 목록 + `DELETE`) < orchd client 15초 → 70초 < orchd HTTP 서버 `WriteTimeout` 120초(바뀌지 않음: apply의 실제 상한) < 웹 130초(node 시험이 "웹 > orchd client 70초 + 15초"를 고정; 120초는 시험으로 고정하지 않음) | 실제 지연이 60초를 넘으면 `delete_partial`/시간 초과(안전한 쪽) |
| R-6 (한도) | `detach_key=true`는 로컬 정리(시간당 10)와 키 떼기(시간당 10) 두 한도를 함께 잡고 하나라도 차면 둘 다 돌려준다. "아무 일 없음"(닿지 못함·`busy`·`forbidden`)만 돌려준다. broker 쪽은 키 `DELETE`(또는 이미 없음이면 등록부 쓰기) 직전에 한 칸 | 옛 broker와 섞이면 시도마다 칸을 쓴다(7장 5) |
| R-7 (웹 결과 모양) | 성공 답의 `detach_key`·`key_state`·`registry`가 서명한 것과 맞아야 "완료", 아니면 "결과를 확인하세요". 제안의 지문이 미리보기의 지문과 다르면 서명 단추가 없다 | 키 파일이 그사이 바뀌면 처음부터 |

## 6. 실서버·실제 GitHub·GitLab에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 17장)에서 처음 확인한다.** 시험은 가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더로 요청 모양과 흐름만 확인했다.
- **실제 키 떼기(`detach_key=true`의 `remove_project`)**: GitHub `GET /repos/{o}/{r}/keys`와 `DELETE …/keys/{id}`가 한 서명 안에서 이어지는 것, 키가 실제로 사라지고 다른 키는 그대로인지, 그 뒤 등록·복제본·키 폴더가 지워지는지. W3f 컷오버에서도 `remove_deploy_key`는 실서버에서 실행하지 않았다(29번 9장).
- **GitHub 키 목록의 `key` 본문 모양**: 로컬 `.pub`의 `type base64`(주석 제외)와 글자 그대로 같은지(우리 비교는 앞 두 조각만 쓴다). 다르면 모든 떼기가 "닮은 키"로 거부된다(안전한 실패).
- **GitLab 공용 키의 두 번째 프로젝트에서의 제목**: `GET /projects/:id/deploy_keys`가 공용 키를 어떤 제목으로 보이는지(첫 등록 때의 제목으로 예상: `id_gitlab_deploy`는 사람이 `sample-collector`에 처음 등록했으므로 `orchd <id>` 꼴이 아닐 수 있다. R-1이 GitLab에서 제목을 보지 않는 이유), `key` 본문이 주석 없이 오는지, 그 프로젝트에서만 비활성되고 다른 프로젝트에서는 계속 쓰이는지, 마지막 프로젝트에서 떼면 키 레코드가 지워지는지.
- **키 `DELETE`의 실제 상태 코드**: GitHub 204, GitLab 204 가정(다른 2xx는 `delete_partial`).
- **지연**: 저장소 GET + 키 목록(여러 쪽) + `DELETE`가 broker의 60초 안에 끝나는지, orchd client 70초·orchd HTTP 서버 쓰기 제한 120초·웹 130초·앞단 프록시 제한시간.
- **실제 패스키(WebAuthn)로 만든 `detach_key` 서명과 `also_local` 서명**을 broker가 검증하는지(바뀐 변경 해시 포함).
- **`also_local=true`가 실서버에서 등록까지 지우는 것**과 그 뒤 orchd의 로컬 삭제, 실패 시 등록 없는 폴더의 실제 모양.
- **혼합 버전 화면**: `broker_unavailable`·`outcome_unknown`(떼기를 켠 경우의 문장 포함) 화면은 **브라우저로 본 적이 없다**(node 시험의 DOM 문장으로만).
- **ego-browser로 본 화면은 G3 보고서에 적힌 것뿐이다**(개발용 서버, 패스키 대역): 1280px GitHub 미리보기(체크 켬·끔), 확인 화면, 결과(키 "뗐습니다"); 390px GitLab 미리보기(지문만, 공용 키 안내, 가로 스크롤 없음), GitLab `also_local` 결과(후속 없음), 떼기 뒤 `delete_partial_local`, 떼기 `delete_partial` 화면, `key_unavailable_for_removal`. **찍었지만 열어 보지 않은 것**: 390px GitLab `also_local` 미리보기(06). **보지 않은 것**: `broker_unavailable`·결과를 알 수 없음 화면, 레거시+떼기(브라우저), 이번 라운드의 `also_local=false` 후속, 다크/라이트 변형, G3 검토 1차 수정 뒤의 화면(DOM 시험만).
- **리눅스의 동작**은 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- W3f(29번 6장)에서 이월된 미확인(GitLab 지연 삭제, 큰 복제본의 미리보기 시간, 실제 Chrome의 IME, 390px 화면 등)은 그대로다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **GitLab: 서명되지 않는 키 줄이 뗄 키를 고른다.** R-1이 GitLab에서 제목을 보지 않으므로, orchd가 자기 공용 `.pub`이 아닌 줄을 보내면(orchd가 손상된 경우) broker는 서명된 프로젝트의 다른 키(그 본문을 가진 키)를 그 프로젝트에서 떼고 등록을 지운다. 경로는 서명된 프로젝트의 것뿐이고 키는 하나뿐이며 전역 키 삭제 호출은 없다. W3f(제목+본문)보다 broker 혼자의 보장이 좁다. **이 좁아짐은 W3g의 `remove_project`만이 아니라 W3f의 `remove_deploy_key`에도 적용된다**: 두 op가 같은 `classifyKeys`를 쓰므로(R-1), W3f 때 "제목 `orchd <id>`와 본문이 둘 다 맞는 키만"이던 `remove_deploy_key`의 GitLab 보장도 "본문이 맞는 키"로 바뀌었다. 그래서 G1 1차 수정에서 기존 시험 둘을 다시 썼다: `adversarial_w3f_test.go`의 GitLab 키 목록(`keysGL`)에서 키 10의 본문을 B → C로(그대로 두면 본문 일치가 둘이 되어 거부되고 키 `DELETE` 모양이 시험되지 않음), `TestGitLabKeyRemovalOnlyThisProjectsKeyId`의 "제목과 본문이 둘 다 맞아야" 확인을 GitLab 규칙(본문 C·A가 각각 자기 키 하나만, 본문 D에 우리 제목이 있으면 거부)으로. `TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g`가 이 동작을 고정한다(8장의 열린 결정).
2. **로컬 정리 실패 = 등록 없는 폴더**(G-D4, W3f의 `remove_project`와 같은 모양). 복구는 다시 등록 → 서명된 삭제(떼기 끔), 또는 사람이 지움(런북 17장).
3. **GitLab 공용 `.pub`을 바꾼 경우의 잘못된 "이미 없음"**: 서버의 공용 키 파일을 새 키로 바꾸면 orchd는 새 본문을 보내고, GitLab 목록의 옛 키는 본문이 다르고 제목도 `orchd <id>`가 아니므로(공용 키의 제목은 첫 등록 때의 것) "닮은 키"로도 잡히지 않는다: `key_state: already_gone`, 등록이 지워지고 **옛 키는 그 프로젝트에 붙은 채 남는다**(아무것도 지우지 않았으므로 잃는 것은 없고, 키는 GitLab 화면에서 사람이 뗀다).
4. **키를 새로 만든 프로젝트**(같은 제목 `orchd <id>`, 다른 본문의 옛 키가 남음)는 떼기가 "닮은 키"로 거부된다. 사람이 제공자 화면에서 옛 키를 지운 뒤 다시 하거나, 떼기를 끄고 지운다.
5. **옛 broker와 섞이면 시도마다 한도 칸을 쓴다**(`bad_request`·`unknown_op`은 "아무 일 없음"으로 보지 않음). 갱신 순서(orchd 정지 → broker → orchd)로 피한다.
6. **orchd는 broker 답에서 자기가 물은 op의 구성원만 읽는다**: 다른 op의 구성원이 덧붙어 있어도 서명과 모순되지 않으면 받아들인다(broker는 정확한 모양만 보낸다).
7. **키 파일이 propose와 apply 사이에 바뀌면** 웹은 지문 불일치로 서명 단추를 주지 않고, 그래도 다른 줄이 broker에 가면 "닮은 키"로 거부된다(GitHub). 처음부터 다시.
8. **`delete_partial` 뒤에는 사람이 제공자 화면을 본다**(자동 재시도 없음). 새 서명은 목록을 먼저 읽으므로 키가 이미 떼어졌으면 "이미 없음"으로 등록을 지운다.
9. W3f의 한계(29번 7장)가 그대로 적용된다. 단 7장 6(`remove_project`는 등록이 먼저 지워짐)은 떼기를 켜면 "키 떼기 → 등록 → 폴더" 순서가 된다.

## 8. 사용자 결정 목록

이미 정해진 것(2026-11-13): 키 떼기 기본 체크(G-D7), 키를 뗄 수 없으면 등록·폴더를 지우지 않고 멈춤(G-D2), `also_local=true`는 등록까지(로컬 실패 시 등록 없는 폴더를 받아들임, G-D4), 복제본 삭제는 사용자가 고를 때만, 판정 R-1·R-2(컨트롤러).

컷오버 때 사용자가 정하거나 해야 하는 것(런북 17.0, 3단계):
1. **[해결됨: 사용자가 (a)를 골랐고(2026-11-14) W3h에서 구현했다. 브랜치 `impl/webconsole-w3h`, 배포 전. 결과는 `33-RESULTS-webconsole-W3h.md`, 컷오버는 `10-OPERATIONS-orchestrator.md` 18장. 아래는 결정 당시의 기록이다.]** ~~[열린 결정, 구현하지 않음]~~ R-1의 GitLab 남은 위험(7장 1, `remove_project`와 W3f `remove_deploy_key` 둘 다)을 받아들일지, 아니면 강화할지.** 사실: 공용 키 `id_gitlab_deploy`는 **사람이 `example-group/sample-collector`에 처음 등록했다**(10장의 실제 GitLab push 기록, 14장 ⚠ 5). GitLab의 공용 키 제목은 첫 등록 때의 것이므로 **아마 사람이 붙인 제목이고 `orchd <id>` 꼴이 아니다**. 그래서 제목 조건(`orchd <이 프로젝트>`, `orchd <첫 프로젝트>`, `orchd `로 시작 등)을 더하면 **모든 GitLab 떼기가 거부된다**: 제목은 쓸 수 없다. 건전한 강화는 본문에 기반한 `deploy_key_sha256`을 묶는 것이다. (a) 이미 서명된 `create_project`/`link_project`/`attach_remote`의 `deploy_key_sha256`을 등록부 행에 저장하고, 떼기와 `remove_deploy_key`에서 받은 키 줄의 sha256을 그 값과 상수 시간으로 비교한다(등록부 스키마 변경이 필요: 다음 단계, 옛 행에는 값이 없다). (b) `remove_project`(떼기)·`remove_deploy_key`의 서명 필드에 `deploy_key_sha256`을 더한다(지금 싸다, 변경 해시가 한 번 더 바뀐다; 서명하는 JS를 orchd가 내려주므로 손상된 orchd에 대해서는 심층 방어일 뿐이다). 사용자가 정한다.
2. **사전 읽기 점검(17.0)을 누가 할지**: GitLab 두 번째 프로젝트의 키 목록 제목, GitHub 키 본문 모양은 사용자 토큰으로 사용자 셸에서(또는 제공자 화면에서) 본다. 토큰 값은 채팅·런북 명령에 넣지 않는다.
3. **시험용 새 저장소 둘의 이름**(키 떼기용 A, 원격 삭제용 B), 원격 삭제 시험(되돌릴 수 없음)을 할지.
4. **`sulee_portfolio`에 남았을 수 있는 `orchd sulee-portfolio` 키**(29번 9장): GitHub 화면에서 사람이 지운다(등록이 이미 없으므로 이 도구로는 뗄 수 없다).
5. 시험 뒤 GitHub 토큰의 삭제 권한을 줄일지, `/root/backup-w3g`를 언제 지울지.

## 9. 컷오버 기록
2026-11-13 22:24 KST, 사용자 요청("W3g 병합하고 서버 반영까지 진행해줘")으로 main(6bc81b4)에 fast-forward 병합(push 안 함)하고 서버에 반영했다. 아래는 **실제로 본 것**과 **보지 못한 것**이다.

**본 것 (컨트롤러가 서버에서 읽기 전용으로 확인)**
- 사전 점검: W3f 상태(broker·orchd active, 등록부 `"version":4`), 키 폴더 비어 있음, 공용 `.pub` 필드 3개(17.0 a·b·e 통과), 실행·대기·승인 대기 작업 없음(마지막 작업 `45 cancelled`뿐).
- 단계 1·2: 백업 `/root/backup-w3g`(옛 바이너리 4개, 등록부 사본, 폴더 목록), 새 바이너리 해시가 Mac 빌드와 일치, 샌드박스 `-check` 통과, **등록부 sha256 불변**(`cb9a3f67…`, v4), orchd `approvals: mode signed`, 로그인 없는 `removal-preview`·`propose` 401, 시작 뒤 오류·패닉 0건.
- **3-3 (원격 삭제 + 복제본 + 등록, 서명 한 번)**: 시험 저장소 `w3s-0114`(사용자가 새로 만듦). broker 로그 `delete_remote_repo: removed the remote repository and the registry row of project w3s-0114`, `op=delete_remote_repo ok dur=1.253s`(60초 한도 안), 후속 `remove_project` 호출 없음. 서버: 폴더·키 폴더 없음, 등록부 v4에 `toy`·`sample-app`만 남음(W3f처럼 `remote_deleted_at` 행이 남지 않음). 화면은 "프로젝트 등록 — 지웠습니다(같은 서명)"이고 후속 안내 없음(사용자 보고).
- **3-2 (키 떼기 + 복제본·등록 정리, 서명 한 번)**: 시험 저장소 `test-4`(GitHub, 사용자가 새로 만듦). broker 로그 `remove_project: removed project test-4 from the registry`, `op=remove_project ok dur=1.336s`. 서버: 폴더·키 폴더 없음, 등록부에 `test-4` 없음. 사용자가 "키 떼기 시험 성공"이라고 보고했다. **GitHub 화면의 Deploy keys에서 `orchd test-4`가 사라졌고 저장소는 남았는지는 사용자 확인이 필요하다(컨트롤러는 GitHub 화면을 보지 않았다).**
- 3-5 누출 점검: 로그에 키 본문(`AAAAC3Nza`)·`PRIVATE KEY` 0건, `.deleting-*`·`.remove-refused-*` 없음.

**일어난 일 (원인 미확정)**
- 3-3 첫 시도에서 화면에 "이 변경을 할 권한이 없습니다"(HTTP 403의 일반 문구)가 나왔다. 그 시도는 broker 로그에 호출이 없고 서버에도 거부 기록이 없어(서명·세션 거부는 로그에 남지 않는다) **원인을 특정하지 못했다.** 사용자가 새로고침·재로그인한 뒤 같은 삭제가 성공했다. 오래된 화면 파일·세션 만료가 의심되지만 확인하지 못했다. 같은 일이 되풀이되면 진단 로그를 넣는다(미결).

**아직 보지 못한 것**
- 17.0 (c) GitLab 두 번째 프로젝트의 키 제목, 17.0 (d) GitHub 키 본문·지문 비교, 3-4 GitLab 키 떼기, 3-1 회귀 화면(사용자가 봤는지 확인하지 못함), 3-6 `broker_unavailable` 화면, 키 `DELETE`의 실제 상태 코드(204 가정; broker 로그에는 성공으로만 남는다).
- 컷오버 뒤 정리: GitHub의 `w3s-0114`는 삭제됐고 `test-4` 저장소는 남아 있을 것이다(사용자가 지우거나 둔다).
