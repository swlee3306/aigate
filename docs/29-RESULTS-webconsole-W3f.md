# 웹 콘솔 W3f — 결과 기록 (2026-11-13)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w3f`)에서 끝났고, 실제 서버(orch-vm)와 실제 GitHub·GitLab에는 아직 아무것도 하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 16장, 설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `28-PLAN-webconsole-W3f.md`. 출발점은 W3e 컷오버가 끝난 서버(main `3830546`, 등록부 v3 운영 중, 제공자 `example-group-gitlab`·`swlee3306-github`)다(W3e의 실서버 결과는 `27-RESULTS-webconsole-W3e.md` 6장 6.0·9장).
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더·명백한 더미 값**)과 ego-browser로 개발용 고정 서버를 본 것(F4 보고서)만 가리킨다. **실제 GitHub·GitLab의 `DELETE`, 실제 토큰의 삭제 권한, 실제 패스키 서명(새 동작 4개), 리눅스 systemd 샌드박스 안의 orchd가 하는 폴더 이름 바꾸기·삭제, 실서버의 등록부 v3→v4 마이그레이션은 한 번도 하지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다. 시험 값은 `ghp_canary-NOT-A-REAL-TOKEN-…` 꼴의 더미다.
> **W3a~W3e의 교훈(임시 폴더 시험이 실서버의 권한 문제를 숨겼음)을 구조에 넣었다:** 새 코드가 실행 사용자·유닛 샌드박스 아래에서 만지는 경로·소유자·모드는 4장의 표로 모았고, 컨트롤러가 컷오버 전에 서버에서 읽기 전용으로 보는 목록은 런북 16.0에 있다.
> **이 기능은 W3a~W3e의 가장 큰 원칙("삭제 코드가 아예 없다")을 의도적으로 부분 해제한다.** 토큰이 새면 그 계정의 저장소를 지울 수 있다는 점은 사용자가 감수했다(별도 삭제 토큰 없음, 사용자 결정 2026-11-12). 코드는 그 위험을 키우지 않도록 삭제 호출을 정해진 행으로만, `origin: created`인 저장소에만, 서명·번호·이름 확인 뒤에만 보낸다.

## 1. 만든 것

**F1. 등록부 버전 4와 새 서명 동작 4개.** 프로젝트에 `origin`(`created`|`linked`|`unknown`|`legacy`)과 선택 필드 `remote_deleted_at`이 붙는다. v3→v4 마이그레이션은 원자적이고 원본을 `registry.json.v3.bak`(0600, 한 번만, 덮어쓰지 않음)로 남기며 기존 프로젝트는 `unknown`, 레거시 종류는 `legacy`가 된다. W3e broker는 v4를 깨짐으로 보고 fail closed 한다. 서명 동작이 `remove_project`(`scope=local`, `legacy_ack`), `delete_remote_repo`(`remote_path`, `remote_id`, `confirm_name`, `also_local`), `remove_deploy_key`(`remote_path`), `clear_orphan`(`project`)으로 늘고 기존 열 가지 동작의 바이트는 그대로다(시험으로 고정). `kind == legacy` ⇔ `origin == legacy`, `origin == created`이면 제공자가 있어야 한다는 불변식을 등록부가 읽을 때마다·쓸 때마다 강제한다.
**F1b. 연결의 프로젝트 이름 제약 해제**(사용자 요청 2026-11-12, W3e 후속): 프로젝트 id가 원격 저장소 이름과 같을 필요가 없다(`remote_name_mismatch` 삭제). 연결 화면에 "프로젝트 이름" 입력칸(저장소 이름에서 만든 제안 값, 바꿀 수 있음), 만든(`created`) 프로젝트만 이름 일치를 계속 요구한다. 실서버의 `sulee_portfolio` 연결이 막혀 있던 이유(W3e 컷오버 기록)를 푼다.
**F2. broker의 서명된 삭제 op.** `delete_remote_repo`는 서명·모양 검증(출처 `created`, 서명된 `remote_path`·`remote_id`가 등록부와 같음, `confirm_name`이 등록부의 저장소 이름과 글자 그대로 같음) → 잠금 → 등록부 다시 읽기 → **제공자에서 저장소를 읽어 번호와 이름이 둘 다 같은지** → 한도 → `DELETE` 한 번 → 등록부에 `remote: 없음`+`remote_deleted_at`(제공자는 남김). `DELETE` 동사는 코드에 네 곳(허용 목록 두 표의 행과 네 발송 함수)뿐이고 허용 목록은 옛 표를 그대로 두고 새 행 여섯(GitHub: 저장소 삭제·배포 키 목록·배포 키 삭제, GitLab: 프로젝트 삭제·프로젝트 배포 키 목록·그 프로젝트에서 키 비활성)을 별도 표로 더한다. 결과를 알 수 없는 `DELETE`(시간 초과·5xx·3xx·문서에 없는 2xx·연결 끊김·`DELETE` 뒤의 등록부 쓰기 실패·패닉)는 `delete_partial`이고 자동 재시도·로컬 동작·등록부 변경이 없다. `remove_deploy_key`는 orchd의 공개 키 줄을 필수 인자로 받아 **제목 `orchd <id>`와 키 본문이 둘 다 맞는 키 하나만** 뗀다. 한도는 원격 삭제 하루 3회·시간당 2회(broker 전체), 로컬 정리·키 떼기·고아 지우기 시간당 10회(메모리, 재시작하면 0).
**F3. orchd의 로컬 정리와 엔드포인트.** `internal/orch/remove`: 대상은 `repo_roots` 바로 아래 한 단계 `<루트>/<id>`, `Lstat`로 심볼릭 링크 아님, `.git` 디렉터리, **`.deleting-<무작위>`로 이름을 바꾼 뒤 `SameFile`로 같은 폴더임을 다시 확인하고서야 `RemoveAll`**, 시작 때 자기 이름 모양의 진짜 폴더만 청소. 키 폴더도 같은 규칙. 4개의 propose/apply 쌍과 `GET /projects/{id}/removal-preview`(쓰기 등급 + CSRF), `project_busy`(진행 중 작업·실행 중 복제·진행 중 제출을 서명 전과 지우기 직전에 확인), 경로 보호(`pathGuard`: 제출·복제·삭제가 서로 배제), 복제본과 키 폴더를 따로 시도하고 항목별 상태 보고, 겹치는 등록 행 거부, 미리보기는 개수만(저장소 설정의 필터·훅·`include`·서브모듈·`commondir`는 실행하지 않음).
**F4. 웹.** 프로젝트 행 "삭제…"와 고아 "기록 지우기": 고르기 → 미리보기(대상 전체를 평문으로) → [레거시: 별도 두 번째 확인 화면 + 이름 다시 입력] → 확인(브라우저가 `change`를 다시 계산하고 서버 문장이 기대와 정확히 같을 때만 서명) → 서명 → 단계별 결과, `delete_partial`·`delete_partial_local`의 두 갈래 안내, 후속 "복제본과 등록 지우기…"는 단추를 눌러야만 시작하는 두 번째 서명. 원격 삭제는 저장소 이름을 직접 입력해야 한다. 변경 개수를 알 수 없으면 "변경이 있을 수 있습니다". 낡은 서버 문장("이 도구는 삭제하지 못한다")을 Go·JS 양쪽에서 고쳤다.
**F5(이 작업).** 적대 시험 8개(broker 4, orchd api 2, node 2), 변이 시험 33개(3장), 이 문서, 런북 16장, 문서 10 §15 낡은 문구·19·27·`infra/README.md` 정리.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-13, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0. 세는 방식은 W3a~W3e와 같다: 최상위 시험 함수와 하위 시험을 따로 센다(`go test -json` 결과 줄에서 집계). 이 숫자는 **적대 시험 커밋(`test: adversarial suite for W3f`) 위에서 처음부터 다시 돌린 전체 실행**의 것이다(문서만 그 뒤에 바뀐다).
- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**, 빌드 오류 출력 없음. 최상위 시험 함수 1754개 중 **1752 통과, 0 실패, 2 건너뜀**(하위 시험 1005개 모두 통과). 건너뜀 2개는 W1~W3e와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollisionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사). 시험 파일이 없는 패키지 4개(`internal/approval/ui`, `internal/gittest`, `internal/orch/fsx`, `internal/refinesize`)는 표에서 뺐다(총 43개 패키지 중 시험이 있는 39개 모두 `ok`). 이번에는 전체 실행 중 실패한 시험이 없었다(변이 시험과 겹치지 않게 순서대로 돌렸다: 27번 문서가 기록한 부하 아래의 시간 의존 시험 실패를 피하려는 것).
- `node --test "internal/orch/web/test/*.test.js"`: **300개 전부 통과**(F4 보고서의 298개 + 이 작업의 2개).
- **게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, 하위 `tokens`·`providers`·`w3e`·`w3f` 모두 PASS, **`SKIP` 없음**.
- W3e(27번 문서) 대비 최상위 통과 1652 → 1752(+100), 하위 시험 982 → 1005(+23), node 257 → 300(+43). F5가 더한 시험은 8개(Go 6, node 2), 나머지는 F1~F4가 더한 것이다.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 14 | 0 | 0 | 0 |
| `cmd/orchd` | 34 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 70 | 0 | 0 | 76 |
| `internal/broker` | 412 | 0 | 0 | 297 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 52 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 408 | 0 | 0 | 35 |
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
| `internal/registrymsg` | 36 | 0 | 0 | 6 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

### F5가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3f_test.go`(4개), orchd `internal/orch/api/adversarial_w3f_test.go`(2개), node `internal/orch/web/test/delete.test.js` 끝부분(2개). F1~F4가 이미 같은 일을 하는 시험은 복사하지 않고 **전체 흐름 위의 얇은 시험**만 새로 썼다(별칭 시험 없음). 새 시험이 증명하는 것:

| 시험 | 하는 일 |
|---|---|
| `TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f` (broker) | **모든 서명 동작 16개(W3e의 12개 + `remove_project`·`delete_remote_repo`·`remove_deploy_key`·`clear_orphan`)의 모든 순서쌍 240개** × (서명만 / 서명+`change`+요청 ID+시각) → 전부 거부, 제공자 요청 0(GET도 DELETE도 없음), 등록부·토큰 저장소·사용한 ID 불변, 거부 코드에 `bad_signature`·`change_mismatch`가 실제로 나옴(통제). 통제: 네 삭제 동작은 **자기 서명으로는** 실제로 지운다(키 하나, 저장소, 고아 기록, 프로젝트 행) |
| `TestUnsignedOrForeignSignedDeletionDoesNothingW3f` (broker) | 네 op 각각에 서명 없음·쓰레기 서명·등록되지 않은 서명자·`change`/`request`/`issued` 없음·**다른 대상에 대한 유효한 서명**: 모두 거부, 제공자 요청 0, `DELETE` 0, 요청 ID 소모 없음, 프로젝트·고아 기록이 그대로 |
| `TestReplayedDeleteSignatureAfterRecreateIsRefusedW3f` (broker) | 저장소가 같은 이름으로 다시 만들어져 번호가 바뀐 경우: ① 등록부·서명은 77, GitHub에는 새 저장소 78 → `remote_changed`(서명은 소모), 같은 서명 재전송은 `request_reused`, 새 서명도 `remote_changed`, `DELETE` 0 ② 삭제 뒤 재생성·재연결(번호 78): **예전 서명의 재전송과 번호 77을 적은 새 서명이 둘 다 제공자 요청 없이 `remote_changed`**, 통제: 번호 78로 서명하면 지워진다 |
| `TestEveryDeletionRequestIsAllowlistedAndBoundToTheProjectW3f` (broker) | 12가지 흐름(GitHub·GitLab 성공, 403 거부, 500·연결 끊김의 불확실, **라이브 번호·이름 불일치**, 키 떼기 둘, 키 없음, 고아·`remove_project`, 거부되는 출처)의 모든 요청이 **시험 안에 다시 쓴 허용 목록**(제공자마다 저장소 읽기 1행 + 새 행 3개, 합 8행; 옛 표의 나머지 행은 이 흐름에서 쓰이지 않는다)에 들어 있고, 모든 `DELETE`가 서명된 프로젝트의 저장소·키 번호(`alice/Demo`, 키 2, GitLab 21, 키 9)만 가리키며, 그룹·조직·사용자·토큰·협업자·훅·브랜치·콘텐츠·보호 브랜치·전역 `deploy_keys`·다른 저장소가 나타나지 않고, **네 가지 `DELETE` 모양이 모두 실제로 나갔다**(비어 있지 않은 검사) |
| `TestNoUnsignedOrMisdirectedRequestRemovesAnythingW3f` (orchd) | 한 프로젝트의 제안을 **다른 세 엔드포인트의 apply**에 모두 보냄(12쌍, 원래 action과 엔드포인트 action으로 바꾼 것 둘), 서명 없음·`null`·빈 서명·`change` 0·다른 주체·요청 ID/시각 없음·여분 필드·빈 `fields`·`../`·대문자 주체·깨진 본문 등 수십 종, 모든 delete 경로에 `GET/PUT/DELETE/PATCH`, 프로젝트·미리보기 경로에 `DELETE/PUT/PATCH/POST`(**HTTP `DELETE` 경로는 어디에도 없다**), 적대적 id의 미리보기: **레거시 프로젝트(키 폴더 포함)와 만든 프로젝트의 파일 트리가 바이트 단위로 그대로**, broker 쓰기 0, 원장 0. 통제: 정상 서명 요청 하나는 실제로 폴더를 지운다 |
| `TestPreviewEndpointRunsNothingFromHostileRepoConfigW3f` (orchd) | 실제 `git` 저장소에 `core.fsmonitor`·`core.hooksPath`·`core.sshCommand`·`core.editor`·`core.pager`·`diff.external`·필터(`clean`/`smudge`/`process`)·`credential.helper`·`include.path`(더 많은 fsmonitor·필터가 든 파일)·훅 5종·`.git/hooks`를 심고 엔드포인트로 두 번 읽음: **표식 파일이 만들어지지 않음**, 응답에 이름·경로·메시지·브랜치가 없고 문자열 값은 `id`·`origin`뿐, 필터가 있으므로 변경 개수는 **알 수 없음**(`changed_known: false`), 통제: 저장소를 실제로 봤다(`is_git_repo`), 기록이 그대로 |
| node `TestEachUnknownCountAloneForcesTheAcknowledgementW3f` | 변경 파일 수 또는 원격에 올라가지 않은 커밋 수를 **각각 단독으로** 알 수 없을 때: 지우겠다는 체크가 나오고 체크 전에는 propose 단추가 꺼져 있고 눌러도 propose 요청이 0이며 화면은 "알 수 없음"(숫자 아님). 통제: 둘 다 알고 0이면 체크가 없다(변이 W05가 처음 살아남아 더한 시험) |
| node `TestAdversarialSelfConsistentForgedProposalIsNeverSignableW3f` | 서버가 **자체로 모순 없는**(`change`·메시지가 자기가 되풀이한 값의 것) 제안을 주지만 사람이 요청한 것과 다른 경우 9가지(다른 번호·확인 이름·저장소 경로·`also_local` 뒤집기·다른 주체, `legacy_ack` 강제, 다른 키 경로·주체), 레거시의 `legacy_ack` 강등, 다른 고아 기록: 모두 고정 경고만, **서명 단추 없음, 패스키를 묻지 않음, apply 0** |

**이미 F1~F4의 시험이 증명하는 것은 복사하지 않는다.** 아래 표는 브리프의 속성이 어느 시험에 있는지의 지도다(시험 이름으로 `go test -run`/`node --test`에서 찾을 수 있다).

| 속성 | 시험 |
|---|---|
| 네 op 모두 서명 없이는 삭제 불가, 서명된 모든 필드를 하나씩 바꾸면 거부 | `TestSignedFieldsCoverEveryDeleteInput`, `TestDeleteActionsInSchema`·`TestW3fSignaturesDoNotCrossActions`(F1), orchd `TestApplyRecomputesChange`, 새 시험 위 둘 |
| 연결(`linked`)·출처 불명·레거시의 원격 삭제 거부 | `TestRemoteDeleteRefusedForLinkedUnknownLegacy`, orchd `TestRemoveDeleteRemoteRefusalsBeforeSignature`, 웹 `TestLegacyRemoteNeverDeletable` |
| `legacy_ack` 양방향 | `TestRemoveProjectLegacyAckBothDirections`, orchd `TestRemoveLegacyOnlyWithLegacyAck`, 웹 `TestLegacyDeleteNeedsSecondConfirmationAndLegacyAck`·`TestLegacyProposalWithoutAckLineIsRefused`, 새 node 시험 |
| `confirm_name` 정확성(대소문자, 등록부의 저장소 이름 기준) | `TestConfirmNameMustEqualRepositoryNameExactly`, `TestConfirmNameField`(registrymsg), 웹 `TestTypedNameRequired` |
| 라이브 번호·이름 불일치 → `remote_changed`, `DELETE` 없음 | `TestDeleteChecksRemoteIdAndName`(모양 단계와 라이브 단계), 새 broker 시험 둘 |
| 허용 목록의 `DELETE` 행(옛 표와의 차집합)·금지 엔드포인트 | `TestOnlyDeleteRowsAddedToAllowlist`, `TestDeleteVerbOnlyInRows`(AST), `TestNoGroupOrOrgOrTokenDeletion`, 새 시험(전체 흐름) |
| GitLab 공용 키는 지우지 않음, 키는 제목과 본문이 모두 맞을 때만 | `TestGitLabKeyRemovalOnlyThisProjectsKeyId`, `TestDeleteKeyOnlyMatchingTitleAndFingerprint` |
| 원격 삭제 뒤 `Provider` 유지·`attach_remote` 거부, created⇒Provider | `TestRemoteDeleteHappyPathAndRegistryEffects`, `TestCreatedOriginNeedsProviderAndIsNeverAttached` |
| 알 수 없는 결과: 자동 재시도·로컬 동작·등록부 변경 없음 | `TestDeletePartialAndUnknownOutcomeNoAutoRetry`, `TestRegistryWriteFailureAfterDeleteIsPartial`, orchd `TestLocalFilesNeverRemovedOnUnsignedOrUnknownOutcome`, 웹 `TestOutcomeUnknownNoAutoRetry` |
| 토큰은 제공자의 `token_ref`에서만, 카나리 토큰이 어떤 출력·로그·원장·오류에도 없음 | `TestDeleteUsesOnlyTheProvidersOwnToken`, `TestDeleteCanaryTokenNeverInAnyOutput`, orchd `TestLedgerNoSecrets`·`TestBrokerCodesMapToFixedSentences`(원문 비반영) |
| 로컬 정리: `<루트>/<id>` 한 단계, 심볼릭 링크·바꿔치기 거부, `.deleting-*` 청소는 자기 것만 | `TestRemoveOnlyUnderRepoRootsOneLevel`, `TestRemoveRefusesSymlinkAndSwap`, `TestStartupSweepOnlyOwnDeleting`, `TestSwapWithFailedRenameBackIsParkedUnderRefusedName`, `TestKeyDirParentChainMustNotBeASymlink`, orchd `TestStartupSweepRemovesOnlyOwnDeletingDirs` |
| 진행 중 작업 거부(서명 전·지우는 동안), 제출·복제와의 경로 보호, 복제본·키 폴더 독립 삭제, 겹치는 행 거부 | `TestRemoveRefusesBusy`, `TestPathGuardSerialisesSubmitCloneAndRemoval`, `TestSubmitRacingARemovalIsRefused`, `TestCloneAndRemovalExcludeEachOther`, `TestRemoveLocalItemsAreIndependentAndStatesAreExact`, `TestLocalPlanRefusesOverlappingRegistryRows` |
| 서명 없는 경로는 아무 것도 지우지 않음 | 새 orchd 시험, `TestLocalRemovalOnlyInTheSignedTail`(AST), `TestProposeStoresNothingNoSideEffects` |
| 미리보기: 개수만, 적대적 git 설정은 실행하지 않음, 쓰기 등급 + CSRF | `TestUnpushedWorkWarning`, `TestWorkCounts*`(gitx 7개: 필터·fsmonitor·서브모듈·`commondir`·계산된 설정), `TestRequireWriteAndCSRFDelete`, 새 orchd 시험 |
| orchd는 broker가 꺼져도 동작, orchd에서 `internal/broker` 가져오기 없음 | `TestOrchdWorksWithBrokerDownW3f`, `TestNoBrokerImportInOrchdW3c`(`go list -deps ./cmd/orchd`: 새 파일도 포함) |
| 웹: 서명 게이트, 레거시 두 번째 화면, 이름 입력 게이트, 만들지 않은 원격은 제안하지 않음, 알 수 없는 개수는 "변경이 있을 수 있습니다", 후속 서명 자동 시작 없음, `delete_partial` 두 갈래 | `TestSignOnlyWhenChangeMatchesW3f`, `TestSignGateForEveryDeleteAction`, `TestLegacyDeleteNeedsSecondConfirmationAndLegacyAck`, `TestTypedNameRequired`, `TestLegacyRemoteNeverDeletable`, `TestUnpushedWarningShown`, `TestFollowUpRemoveRegistryIsSecondSignedChange`, `TestDeletePartialGuidanceTwoPathsAndSessionMemory`, `TestPreviewLinesAreVerifiedExactly` |

**시험을 새로 쓰는 중 발견한 것.** (a) **제품 코드의 결함은 발견하지 못했다.** 적대 시험은 모두 첫 실행에서 통과했거나 아래의 시험 작성 실수만 드러냈고 제품 코드를 바꾸지 않았다.
(b) **시험 작성 중의 정정(사실 확인):** 서명이 없는 요청의 답은 `bad_request`가 아니라 `not_signed`이고, `delete_remote_repo`·`remove_deploy_key`에서 "다른 주체로 옮긴 서명"은 서명 검증 전에 모양 단계에서 `remote_changed`로 거부된다(그 두 행은 "아무것도 지워지지 않음"을 증명하지 서명 수준의 주체 결합을 증명하지는 않는다: 그 결합은 모든 동작 쌍 시험과 F1의 `TestW3fSignaturesDoNotCrossActions`가 증명한다). 미리보기 시험에서 `"main"`이 응답에 "새어 나갔다"고 한 첫 실패는 `remote_key_may_remain`이라는 키 이름의 부분 문자열이었다(누출 아님: 시험의 검사를 따옴표 포함 `"main"`으로 좁혔다). 적대적 id 목록의 `app-one/`은 Go의 경로 정리로 `app-one`과 같은 프로젝트가 되어 200을 돌려줬다(다른 프로젝트에 닿지 않음: 목록에서 뺐다). 변이 시험이 밝힌 것: **(c) 웹의 "변경 파일 수를 모르면 확인 체크를 요구한다"는 코드는 있었지만 시험이 없었다**(변이 W05가 처음에 살아남음) → 새 node 시험 `TestEachUnknownCountAloneForcesTheAcknowledgementW3f`(두 개수 각각 단독으로)를 더하고 W05·W08을 다시 돌려 잡혔다.

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖(`/tmp/aigate-mut-w3f/copy`)으로 복사**(`rsync`, `.git`·`.superpowers`·`graphify-out` 제외)해 한 곳만 망가뜨리고 관련 시험을 돌렸다. 변이마다 복사본의 원래 파일로 되돌렸고 작업 트리는 건드리지 않았다. 도구는 `/tmp/aigate-mut-w3f/mutate.py`(순차 실행, 각 변이의 정확한 바꿈 문자열은 `mutants.py`). 전부 구현자 실행이다(검토자가 아직 재현하지 않음). **패키지 전체를 변이마다 돌리지 않았다(broker 110초, api 4분이 넘어서):** 아래 `-run` 부분집합을 썼다. broker: `go test ./internal/broker -run 'W3f$|Delete|Remove|Orphan|Created|Confirm|GitLab|Cross|Legacy|Allowlist|Signature|Registry|Canary|Provider|Concurrent|Sign'`, orchd: `go test ./internal/orch/api -run 'W3f|Remove|Delete|Preview|Local|Legacy|Orphan|Clone|Guard|Submit|Sweep|Ledger|Partial|Busy|Propose|Apply|Unpushed|Order|Broker|Require'`, 로컬 정리: `go test ./internal/orch/remove ./internal/gitx`(전체), 미리보기 하드닝(O07): `go test ./internal/gitx ./internal/orch/api -run 'WorkCounts|W3f|Unpushed|Preview'`, 웹: `node --test`로 `delete.test.js`·`w3e.test.js`·`providers.test.js`·`projects.test.js` 네 파일. 굵게 = F5의 새 시험.

| # | 망가뜨린 곳 | 패키지 | 실패한 시험 (개수) |
|---|---|---|---|
| B01 | 서명 검증 제거(`VerifyRegistry` 결과를 항상 성공으로) | broker | TestApprovalAndRegistryMessagesStillSeparate, TestApprovalMessageCannotAuthorizeRegistryChange, TestCrossActionSignaturesRefused, TestCrossActionSignaturesRefusedForAllPairsW3e, TestCrossActionSignaturesRefusedW3e, **TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f** … (+17) (23) |
| B02 | `change` 일치 검사 제거 | broker | TestCrossActionSignaturesRefusedForAllPairsW3e, **TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f**, TestInvalidSignatureDoesNotConsumeRequestID, TestProviderBaseURLChangeNeedsSignatureAndShowsInChange, TestRegistryNeverWrittenWithoutValidSignature, TestSetProviderRules … (+10) (16) |
| B03 | 원격 삭제의 `origin` 검사 제거(`linked`·`unknown`·`legacy`도 통과) | broker | TestDeleteCodesFixedSentences, TestRemoteDeleteRefusedForLinkedUnknownLegacy (2) |
| B04 | 등록부의 `remote_id` 비교 제거(모양 단계) | broker | TestDeleteChecksRemoteIdAndName, **TestReplayedDeleteSignatureAfterRecreateIsRefusedW3f** (2) |
| B05 | `confirm_name`의 등록부 저장소 이름 비교 제거 | broker | TestConfirmNameMustEqualRepositoryNameExactly, TestDeleteChecksRemoteIdAndName (2) |
| B06 | 라이브 읽기의 번호 비교 제거 | broker | TestDeleteChecksRemoteIdAndName, TestDeleteKeyOnlyMatchingTitleAndFingerprint, **TestReplayedDeleteSignatureAfterRecreateIsRefusedW3f** (3) |
| B07 | 라이브 읽기의 이름 비교 제거 | broker | TestDeleteChecksRemoteIdAndName (1) |
| B08 | GitHub 허용 목록에 `DELETE` 행 하나 추가(브랜치 보호 삭제) | broker | TestDeleteVerbOnlyInRows, TestEveryGitHubRequestIsAllowlisted, TestOnlyDeleteRowsAddedToAllowlist (3) |
| B09 | GitLab 허용 목록에 전역 `DELETE /deploy_keys/{id}` 행 추가(공용 키 삭제 가능) | broker | TestDeleteVerbOnlyInRows, TestGitLabKeyRemovalOnlyThisProjectsKeyId, TestOnlyDeleteRowsAddedToAllowlist (3) |
| B10 | 키 정체를 제목만으로(본문 비교 제거) | broker | TestDeleteKeyOnlyMatchingTitleAndFingerprint, **TestEveryDeletionRequestIsAllowlistedAndBoundToTheProjectW3f**, TestGitLabKeyRemovalOnlyThisProjectsKeyId (3) |
| B11 | `legacy_ack`: 레거시를 확인 없이 지움 | broker | TestRemoveProjectLegacyAckBothDirections (1) |
| B12 | `legacy_ack`: 레거시가 아닌데 확인이 있어도 통과 | broker | TestRemoveProjectLegacyAckBothDirections (1) |
| B13 | 원격 삭제 뒤 `Provider`를 지움 | broker | TestConcurrentDeleteOfOneProjectSendsOneDelete, TestCreatedOriginNeedsProviderAndIsNeverAttached, TestDeleteChecksRemoteIdAndName, TestDeleteUsesOnlyTheProvidersOwnToken, **TestDeletionNeedsItsOwnSignatureForAllActionPairsW3f**, TestRemoteDeleteHappyPathAndRegistryEffects … (+3) (9) |
| B14 | 불확실한 `DELETE` 뒤 자동 재시도 | broker | TestDeletePartialAndUnknownOutcomeNoAutoRetry, TestGitLabDeleteRateLimitAnswerIsUncertain (2) |
| B15 | GitHub 요청이 제공자의 `token_ref`가 아닌 다른 저장된 토큰을 씀 | broker | TestCreateProjectGitHubRefusedCreateIsNotAnOrphan, TestCreateProjectGitHubRegistryWriteFailureIsOrphan, TestCrossActionSignaturesRefusedForAllPairsW3e, TestDeleteUsesOnlyTheProvidersOwnToken, TestEveryGitHubRequestIsAllowlisted, TestGitHubCreatePanicIsOrphanAndNeverLeaks … (+12) (18) |
| O01 | 서명 없는 로컬 삭제 경로(propose 단계가 폴더를 지움) | orchd api | TestBrokerCodesMapToFixedSentences, TestCloneAndRemovalExcludeEachOther, TestLocalFilesNeverRemovedOnUnsignedOrUnknownOutcome, TestLocalRemovalOnlyInTheSignedTail, **TestNoUnsignedOrMisdirectedRequestRemovesAnythingW3f**, TestOrchdWorksWithBrokerDownW3f … (+5) (11) |
| O02 | `SameFile` 재확인 제거(바뀐 디렉터리를 받아들임) | orchd remove | TestRemoveRefusesSymlinkAndSwap, TestSwapWithFailedRenameBackIsParkedUnderRefusedName (2) |
| O03 | `project_busy` 확인 제거(`projectBusy`가 항상 한가함) | orchd api | TestCloneAndRemovalExcludeEachOther, TestRemoveRefusesBusy (2) |
| O04 | 경로 보호 제거: 삭제가 제출·복제와 배제되지 않음(`tryRemove`) | orchd api | TestCloneAndRemovalExcludeEachOther, TestPathGuardSerialisesSubmitCloneAndRemoval, TestSubmitRacingARemovalIsRefused (3) |
| O05 | 경로 보호 제거: 삭제 중에도 제출이 거부되지 않음(`enterSubmit`) | orchd api | TestCSRFHeaderRequiredOnWrite, TestIdempotentSubmitAcrossRestartWindow, TestIdempotentSubmitReturnsSameTask, TestKeyedSubmitLedgerAfterIdemRecord, TestLedgerSubmittedEventHasActor, TestOrchdWorksWithBrokerDown … (+15) (21) |
| O06 | 복제본 실패가 키 폴더를 건너뛰는 단락 복원(독립 삭제 제거) | orchd api | TestDeletePartialShowsStateAndRetry, TestRemoveLocalItemsAreIndependentAndStatesAreExact (2) |
| O07 | 미리보기 git 하드닝 제거(필터·`include`·`commondir`·계산된 설정 검사) | gitx, orchd api | **TestPreviewEndpointRunsNothingFromHostileRepoConfigW3f**, TestWorkCountsCommondirRedirectDoesNotRun, TestWorkCountsRunNothingFromRepoConfig (3) |
| O08 | orchd의 `legacy_ack` 검사 제거 | orchd api | TestRemoveLegacyOnlyWithLegacyAck (1) |
| O09 | orchd의 `origin` 검사 제거(`linked`·`unknown`·`legacy`의 원격 삭제를 제안) | orchd api | TestRemoveDeleteRemoteRefusalsBeforeSignature, TestRemoveLegacyOnlyWithLegacyAck (2) |
| O10 | `also_local`을 서명된 필드에서 읽지 않음(항상 로컬도 지움) | orchd api | TestApplyRecomputesChange, TestOrderRemoteBeforeLocal (2) |
| W01 | 웹 서명 게이트에서 `change` 비교 제거 | node | TestSignGateForEveryDeleteAction, TestSignOnlyWhenChangeMatches, TestSignOnlyWhenChangeMatchesW3e, TestSignOnlyWhenChangeMatchesW3f (4) |
| W02 | 웹 이름 입력 게이트 제거 | node | TestTypedNameRequired (1) |
| W03 | 웹이 `linked` 저장소의 원격 삭제를 제공 | node | TestLegacyRemoteNeverDeletable (1) |
| W04 | 웹 `also_local`이 등록 지우기를 자동으로 이어서 시작 | node | TestRemoteDeleteSignedFieldsAndAlsoLocalSingleSignature (1) |
| W05 | 웹: 변경 파일 수를 모르면 확인 체크를 요구하지 않음 | node | **TestEachUnknownCountAloneForcesTheAcknowledgementW3f** (1) |
| W06 | 웹: 변경 파일 수를 모르면 숫자로 보임(두 곳) | node | TestUnpushedWarningShown (1) |
| W07 | 웹: 서버가 되풀이한 값(echo) 비교 제거(`change`·메시지 비교와 겹침) | node | TestSignOnlyWhenChangeMatchesW3f (1) |
| W08 | 웹: 푸시 안 된 커밋 수를 모르면 확인 체크를 요구하지 않음 | node | **TestEachUnknownCountAloneForcesTheAcknowledgementW3f** (1) |

**33개 모두 잡혔다. 살아남은 변이는 없다(아래 두 항목 이후).** 솔직히 기록할 것:
1. **W05는 처음에 살아남았다**(웹: 변경 파일 수를 모르면 확인 체크를 요구하지 않음). 원인은 시험이 없었던 것(코드는 있었다). `TestEachUnknownCountAloneForcesTheAcknowledgementW3f`를 더한 뒤 W05와 짝인 W08(푸시 안 된 커밋 수)이 둘 다 잡힌다. 제품 코드는 바꾸지 않았다.
2. **B04·B10·O02는 처음에 컴파일 오류**(`strconv`·`wantBody`·`fi`가 안 쓰임)로 "잡힘"처럼 보였고, 컴파일되게 `false &&`·`true ||`로 다시 쓴 변이의 결과가 위 표다(27번 문서 3장과 같은 정직성). W06은 같은 표현이 두 곳에 있어 둘 다 바꿨다.
3. **B03(`origin` 검사 제거)이 `TestDeleteCodesFixedSentences`도 실패시킨 것은 부수 효과**다(그 코드를 쓰는 곳이 사라져 "쓰이는 코드 = 등록된 문장" 검사가 어긋남). 실제로 속성을 잡는 시험은 `TestRemoteDeleteRefusedForLinkedUnknownLegacy`다.
4. **O05(`enterSubmit`이 항상 허용)는 부수 피해가 크다(29개):** 이 변이는 진입을 세지 않고 `leaveSubmit`만 세어 보호의 균형을 깨 제출을 쓰는 다른 시험도 실패시킨다. 속성을 잡는 시험은 `TestSubmitRacingARemovalIsRefused`·`TestPathGuardSerialisesSubmitCloneAndRemoval`이다(둘 다 실패 목록에 있음).
5. **W07(서버가 되풀이한 값의 비교 제거)은 `change`·메시지 비교와 겹쳐 있어** 하나만 지우면 `TestSignOnlyWhenChangeMatchesW3f`의 한 사례만 잡는다(겹친 방어, 의도).
6. **F2·F3 보고서의 동등 변이(살아남은 것)를 그대로 옮긴다**(이번 실행에서는 다시 돌리지 않았다): 등록부의 `remote_deleted_at` 가드 제거(`Remote == nil` 가드와 등록부 검증이 같은 행을 거부해 동등), GitLab 경로 비교의 대소문자 무시(종류 검사기가 이미 대문자 GitLab 경로를 거부), 하루 한도의 사전 검사 제거(뒤의 `Allow()`가 같은 것을 거부), `scope` 검사 제거(`registrymsg`가 `scope != local`을 `ChangeHash`에서 거부), `remove`의 `checkDir`을 `Lstat` 대신 `Stat`(이름 바꾸기 뒤의 `SameFile`·`Lstat` 재확인이 같은 바꿔치기를 막는 겹친 방어), 키 폴더 소유자 검사 제거(시험이 모두 우리 소유라 비루트로는 시험할 수 없음), orchd의 알 수 없는 결과 매핑 제거(쓰기 경로의 다른 매핑이 같은 결과), gitx의 `commondir`·계산된 설정 가드는 각각 단독 제거 시 서로가 막아 살아남고(겹친 방어) 둘을 함께 지우면 잡힌다.

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

F1~F4가 각자 적은 표를 모았다. **이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험에 기초한 것이고 실서버에서 본 것이 아니다.** 컷오버 전 확인 목록은 런북 16.0. **실행 사용자와 샌드박스(변경 없음, 13~15장과 같음).** broker: `orch-broker`(uid 999), `ProtectSystem=strict`, **`ProtectHome=yes`(`/home`을 보지 못한다)**, `PrivateTmp`, `NoNewPrivileges`, `CapabilityBoundingSet=` 비어 있음, `MemoryDenyWriteExecute`, `SystemCallFilter=@system-service`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, 쓰기는 `/var/lib/orch-broker`·`/run/orch-broker`만. orchd: `orch`, `ProtectSystem=strict`, `ProtectHome=read-only`, **`ReadWritePaths=/home/orch/orchd /home/orch/repos`**, `PrivateTmp=yes`(`infra/systemd/*.service`, 바뀌지 않음).

| 경로·대상 | 소유자 / 모드 | 접근하는 쪽 | 하는 일 | 샌드박스·보호 규칙 |
|---|---|---|---|---|
| `/var/lib/orch-broker/registry.json` | `orch-broker` 0600(폴더 0700) | broker만 | **버전 3 → 4 마이그레이션(첫 시작에서 원자적 쓰기)**, 삭제 op의 등록부 쓰기(임시 파일 → rename) | `ReadWritePaths`. **W3e broker는 버전 4를 깨짐으로 봄 → 롤백은 등록부 복원** |
| `/var/lib/orch-broker/registry.json.v3.bak` | `orch-broker` 0600 | broker가 한 번 씀 | 마이그레이션 직전의 v3 원본(덮어쓰기 불가). 쓰기 실패는 시작 실패(v3 파일은 그대로). **이미 있으면 덮어쓰지 않는다: 낡은 파일일 수 있으므로 롤백 전에 root 백업과 sha256을 비교한다** | `ReadWritePaths` 안. 롤백의 보조 사본(root 백업이 우선) |
| `/var/lib/orch-broker/registry.json.v2.bak` | `orch-broker` 0600 | — | W3e가 남긴 것, 그대로 | — |
| `/var/lib/orch-broker/{tokens.json, secrets/<이름>}` | 0600 / 0400(폴더 0700) | broker | **호출마다 읽기만**: 삭제 호출에도 제공자의 `token_ref` 토큰 하나만(값은 `Authorization` 헤더로만 나감). 새 토큰 파일 없음 | 변경 없음 |
| 제공자 `https://api.github.com`(공개 IP)·사내 GitLab | — | **broker** | 새 호출: GitHub `DELETE /repos/{o}/{r}`·`GET …/keys`·`DELETE …/keys/{id}`, GitLab `DELETE /projects/{id}`·`GET …/deploy_keys`·`DELETE …/deploy_keys/{key_id}`. 연결 10초·전체 15초, 리다이렉트 안 따름(3xx는 불확실로 취급) | 방화벽 변경 없음(W3e와 같은 목적지) |
| `/home/orch/repos` | **`orch:orch` 0700**, 진짜 폴더 | **orchd**(`orch`) | 로컬 정리가 `<루트>/<id>`를 `<루트>/.deleting-<16 hex>`로 **이름 바꾸기**(같은 폴더 안, 덮어쓰지 않음) 뒤 `RemoveAll`. 시작 때 자기 이름 모양의 진짜 폴더만 청소 | **`ReadWritePaths=/home/orch/repos` 안.** 같은 파일 시스템이어야 한다(루트 아래 마운트·바인드 마운트 금지: 런북 16.0 (d)). 실서버에서 이름 바꾸기·`RemoveAll`이 되는지 **미확인** |
| `/home/orch/repos/<id>` | `orch:orch` 0700, 진짜 폴더(심볼릭 링크 아님), `.git` 디렉터리 | orchd | 삭제 대상. 등록부 경로가 정확히 `<루트>/<id>`가 아니면 `local_unsafe`로 거부 | 같음 |
| `/home/orch/orchd/ssh/keys/<id>/` | `orch:orch` 0700, 키 `id_ed25519` 0600 | orchd | 같은 방식(키 폴더의 부모는 우리 소유의 진짜 폴더여야 함, 아니면 `local_unsafe`) | **`ReadWritePaths=/home/orch/orchd` 안** |
| `…/keys/<id>/id_ed25519.pub` | `orch:orch` 0644 | orchd | **읽기만**: 키 떼기에서 공개 키 줄을 broker에 줌(개인 키는 열지 않는다). 없으면 `key_unavailable_for_removal` | 같음 |
| `/home/orch/repos/.deleting-*`, `…/keys/.deleting-*` | `orch:orch` | orchd | 삭제 도중의 이름. 시작 때 `^\.deleting-[0-9a-f]{16}$`이고 우리 소유의 진짜 폴더인 것만 지움. 바꿔치기가 의심되어 원래 자리로 못 돌려놓은 폴더는 `.remove-refused-<hex>`로 두고 청소가 건드리지 않음 | 같음 |
| orchd 원장 | 기존 | orchd | 새 이벤트 `project_remove`·`remote_delete`·`key_remove`·`orphan_clear`(키 `actor,action,subject,ok,code`) | 기존과 같음 |
| 정적 `--git-remote-for` | orchd 유닛 | 사람 | `sample-app`를 지워도 설정은 남는다: 사람이 정리(런북 16장) | 바뀌지 않음 |
| `/root/backup-w3f` | root 0700 | 사람(root) | 롤백용 바이너리·`registry.json`(v3)·정책·`known_hosts`·`tree-before.txt`(이름·소유자·모드 목록, **개인 키 사본 없음**) | — |
| 웹 정적 파일 | orchd 바이너리에 내장 | orchd | 새 `views/delete.js` 등(경로·모드 변경 없음): **orchd를 갱신해야 새 화면이 보인다** | — |

새 유닛·새 플래그·새 설정 키·새 포트·DB 스키마 변경(v8 그대로)은 없다. 서버에서 `git` 2.43을 실행하는 곳이 하나 늘었다(미리보기: 읽기 전용 질의, 훅·fsmonitor 꺼짐, 선택적 잠금 없음). **실서버에서만 알 수 있는 것은 6장.**

## 5. 계획과 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| W3f-1 | **별도 삭제 토큰 없음**(초안의 `delete_token_ref`를 사용자 결정으로 철회, 계획 v0.3). 삭제도 제공자의 기존 토큰 | 토큰이 새면 그 계정의 저장소를 지울 수 있다(사용자가 감수) |
| W3f-2 | **`remove_deploy_key`는 `origin`과 상관없이 제공자·원격이 있는 모든 프로젝트에 허용**(계획 v0.4 보정): 레거시 행에 `attach`로 붙은 키도 뗄 수 있어야 함 | — |
| W3f-3 | **키 정체는 제목 `orchd <id>` AND 키 본문**: 서명되는 변경에는 키 해시를 묶을 수 없어 orchd가 공개 키 줄을 비서명 인자로 주고 broker가 그 본문과 같은 키만 뗀다(넓히지 못하고 좁히기만). GitLab은 `DELETE /projects/{id}/deploy_keys/{key_id}`(그 프로젝트에서만 비활성) | 공개 키 파일이 없으면 키 떼기가 안 된다(사람이 GitHub에서) |
| W3f-4 | **허용 목록에 계획에 없던 읽기 행 하나(GitLab `GET /projects/{id}/deploy_keys`)**. GitLab은 키 추가 응답의 id를 저장하지 않아 키를 찾을 수 없다 | 읽기 전용 |
| W3f-5 | **GitLab 성공 상태는 202(프로젝트)·204(키)만 인정**하고 다른 2xx는 `delete_partial`(불확실). GitHub는 204. GitLab 문서는 성공 상태를 적지 않는다 | 가정이 틀리면 GitLab 삭제가 항상 `delete_partial`로 끝난다(안전한 실패) |
| W3f-6 | **`confirm_name`은 등록부의 원격 경로의 마지막 조각과 글자 그대로**(대소문자 포함) 같아야 함(서명된 `remote_path`와의 비교가 아니라 등록부 기준, 검토 지적) | — |
| W3f-7 | **하루 3회 한도에 시간당 2회를 더함**(계획은 하루 3회·주체별 시간당 3회). broker 전체 시간당 2회, 둘을 한 뮤텍스 안에서 함께 검사·소모 | 시험을 몰아서 하면 두 번째 지우기가 `delete_rate_limited` |
| W3f-8 | **`also_local`은 등록을 지우지 않는다**: 원격 삭제 + 복제본·키 폴더까지만, 등록은 "원격 없음"으로 남는다. 등록까지 지우려면 **두 번째 서명(`remove_project`)** — 웹은 단추를 눌러야만 시작 | 삭제 한 번에 서명이 두 번 필요 |
| W3f-9 | **독립 `remove_project`의 순서는 broker 먼저(서명 확인 + 행 삭제), 그 뒤 orchd가 폴더**(계획의 "로컬 먼저"는 두 서명 흐름에서만 가능, 서명 전에 이름을 바꾸면 죽었을 때 시작 청소가 서명 없이 폴더를 지울 수 있음). 그래서 폴더 삭제가 실패하면 등록 없는 폴더가 남을 수 있다(`delete_partial_local`, 서명 전에 `CheckClone`/`CheckKeyDir` 사전 점검으로 드물게 함) | 남은 폴더는 다시 등록해 지우거나 사람이 지움(런북) |
| W3f-10 | **`delete_partial` 뒤의 재시도는 `remote_changed`**(저장소가 없으므로): 정리는 `remove_project` | 화면이 두 갈래를 안내 |
| W3f-11 | **`origin == created`이면 제공자가 있어야 함**(불변식, `attach_remote`는 created 행을 `already_linked`로 거부), 원격 삭제 뒤에도 `Provider`를 남김 | — |
| W3f-12 | **미리보기의 "원격에 올라가지 않은 커밋"은 `--branches --not --remotes`**(어느 로컬 브랜치든 어느 원격 추적 참조에도 없는 커밋: `@{u}..HEAD`의 상위 집합, 낡은 원격 추적 참조면 과대 계산하고 과소는 하지 않음). 필터·`include` 섹션이 있는 저장소의 변경 개수는 **알 수 없음**(`git status`를 돌리지 않음) | 알 수 없음은 "변경이 있을 수 있습니다"와 확인 체크 |
| W3f-13 | **GitHub 이름 수준 TOCTOU 받아들임**: 번호로 지우는 호출이 없어, 읽기와 `DELETE` 사이에 같은 이름이 다시 만들어지는 짧은 틈은 남는다 | 같은 이름의 새 저장소가 지워질 수 있다(극히 좁은 틈) |
| W3f-14 | **원격 삭제는 이 도구가 만든 저장소만**: 이미 만든 시험 저장소 `w3e-smoke-1112`·`1113`은 `origin: unknown`이라 원격 삭제 불가(`claim_origin`은 넣지 않음) | 사용자가 GitHub에서 직접 지운다 |

## 6. 실서버·실제 GitHub·GitLab에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 16장)에서 처음 확인한다.** 시험은 가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더로 요청 모양과 흐름만 확인했다.
- **GitHub `DELETE /repos/{owner}/{repo}`의 실제 권한과 응답:** 클래식 토큰의 `delete_repo` 범위 대 세분화 토큰의 `Administration: write`(문서가 둘의 관계를 이 도구의 호출에 대해 명시하는지 확인하지 못함), 권한 부족 시 403/404 → `remove_refused`가 되는지, 조직 소유자가 구성원의 삭제를 막는 403, 성공 204, 이름이 바뀐 저장소의 307(우리는 따르지 않고 불확실로 취급).
- **GitLab:** `DELETE /projects/:id`의 성공 상태(202 가정, 문서에 없음), 지연 삭제("삭제 예약")와 복구 가능 기간, `permanently_remove`는 쓰지 않음, **키 비활성(`DELETE /projects/:id/deploy_keys/:key_id`)의 성공 상태(204 가정)와 공용 키가 다른 프로젝트에서 계속 쓰이는지**, `GET /projects/:id/deploy_keys`가 `key` 본문을 주석 없이 주고 공용 키도 포함하는지, 429에 대한 동작.
- **GitHub 배포 키 `key` 본문에 주석이 없는지**(우리 비교는 `type base64`만 쓰므로 어느 쪽이든 된다).
- **GitHub 이름 수준 TOCTOU**(번호를 확인한 읽기와 이름으로 지우는 `DELETE` 사이의 틈: 시험으로 만들 수 없는 위험).
- **orchd가 실제 유닛 샌드박스에서 `<루트>/<id>`를 `.deleting-*`로 이름 바꾸고 `RemoveAll` 하는 것**(`ReadWritePaths`, 소유자 `orch`, 같은 파일 시스템, `RENAME_NOREPLACE`는 리눅스에서는 컴파일만 확인), 시작 때 `.deleting-*` 청소, 열린 워크트리를 가진 에이전트가 있을 때의 동작.
- **큰 복제본의 미리보기 시간**(`git` 2.43, 상한 25초, 크기 계산 포함)과 개수의 정확함.
- **등록부 v3→v4 마이그레이션을 실서버 파일(제공자 둘, 프로젝트 `toy`·`sample-app`·스모크 둘 등)·uid·샌드박스로** 하는 것, `registry.json.v3.bak` 쓰기(폴더 fsync 포함), 롤백의 실제 영향, 샌드박스 안 `-check`, 마이그레이션 뒤 `origin`이 기대대로(`legacy` 둘, 나머지 `unknown`)인지.
- **실제 Chrome에서** 저장소 이름 입력칸의 IME·붙여넣기·포커스(화면 전환 때 입력칸이 옮겨져 포커스를 잃는 문제를 개발용 서버에서 찾아 고쳤으나 실제 IME로는 보지 못함), **실제 orchd에서 미리보기 GET의 CSRF 헤더**(개발용 서버는 인증·CSRF가 없다), 130초·앞단 제한시간.
- **실제 패스키가 만든 새 동작 4개(`remove_project`·`delete_remote_repo`·`remove_deploy_key`·`clear_orphan`)의 서명**을 broker가 검증하는지.
- **ego-browser로 보지 못한 화면(F4 보고서):** 390px의 확인 화면들(r1-32, r1-35)·출처를 모르는 고아 화면(r2-41)·1280px의 후속 결과(r1-16)와 키 미리보기(r1-17)는 사진을 찍고 열어 보지 않았다. 만든·출처 불명 고아 화면, 키 떼기 미리보기·결과(390px), 연결한 고아 결과, 다크/라이트의 다른 변형은 DOM 문장과 시험으로만 확인했다. 개발용 서버의 패스키는 **대역**(가짜 `navigator.credentials.get`)이었다.
- **리눅스의 파일·프로세스 동작**은 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- W2b·W3a~W3e에서 이월된 미확인(실제 MR, 한도·예산 초과 실동작, 무료 요금제 비공개 저장소의 보호 거부, GitLab 공용 키의 두 번째 프로젝트 합류 등)은 각 결과 문서(27번 6장 6.0 포함) 그대로다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **삭제 권한은 일반 토큰에 이미 있다**(별도 삭제 토큰 없음): 토큰이 유출되면 그 계정의 저장소를 지울 수 있다. 코드는 정해진 행만 보내지만 토큰 자체의 권한은 그대로다. 서명된 변경·만료일·폐기가 방어다.
2. **원격 삭제는 되돌릴 수 없다**(GitHub 즉시, GitLab은 인스턴스 설정에 따라 복구 가능 기간이 있을 수 있으나 이 도구는 복구하지 않는다).
3. **이미 만든 시험 저장소(`w3e-smoke-1112`·`1113`)와 마이그레이션 때의 모든 기존 프로젝트는 `origin: unknown`이라 이 도구로 원격을 지우지 못한다**(로컬 정리만). `claim_origin`은 만들지 않았다(잘못 주장하면 연결한 저장소를 지울 수 있다).
4. **레거시 프로젝트의 원격 저장소(`sample-app`의 사내 GitLab 등)는 이 도구로 지우지 못한다.** 서버 쪽 삭제는 두 번째 확인·서명된 `legacy_ack`가 있을 때만 된다.
5. **GitHub 이름 수준 TOCTOU**(읽기와 `DELETE` 사이의 틈, 번호로 지우는 API가 없음).
6. **독립 `remove_project`는 등록이 먼저 지워지므로 폴더 삭제 실패 시 등록 없는 폴더가 남을 수 있다.** 복구는 다시 등록해 지우거나 사람이 지운다(런북).
7. **삭제 한도는 메모리에만 있다**: broker를 재시작하면 하루 3회 카운터가 0이 된다.
8. **`delete_partial`의 `DELETE`가 실제로 나갔는지는 제공자 화면에서 사람이 본다**(자동 재시도·자동 정리 없음). 재시도는 `remote_changed`가 되므로 정리는 `remove_project`다.
9. **정적 `--git-remote-for`는 프로젝트를 지워도 남는다**(사람이 정리). 열린 워크트리를 가진 에이전트와 로컬 정리 사이의 경합은 `pathGuard`와 `project_busy`로 막지만 실서버에서 확인하지 못했다.
10. **미리보기의 변경 개수는 저장소 설정에 필터·`include`가 있으면 알 수 없다**(화면은 "변경이 있을 수 있습니다"), 원격에 올라가지 않은 커밋 수는 낡은 원격 추적 참조 때문에 실제보다 많을 수 있다(적게는 안 나온다).
11. **저장소의 공개 여부는 등록부에도 미리보기에도 없다**: 화면은 "이 화면이 알지 못합니다"라고 말한다.
12. W3c·W3e의 한계(27번 7장)가 그대로 적용된다(단, 7장의 1번 "이 도구는 지우지 못한다"와 2번 "프로젝트 id는 저장소 이름과 같아야 한다"는 W3f가 바꿨다).

## 8. 사용자 결정 목록

이미 정해진 것(2026-11-12):
1. **삭제 권한은 일반 토큰에 이미 있다**(별도 삭제 토큰 없음): 사용자가 감수.
2. **레거시 프로젝트의 서버 쪽 삭제는 두 번째 확인과 함께 허용**(원격은 불가).
3. **원격 삭제 시험은 새로 만든 시험 저장소에서만**(`origin: created`).
4. **이미 만든 시험 저장소 `w3e-smoke-1112`·`1113`은 원격을 이 도구로 지우지 못한다**(출처 `unknown`): GitHub에서 사람이 직접 지운다. 로컬 정리는 된다.

컷오버 때 사용자가 정하거나 해야 하는 것(런북 16.0, 3단계):
5. **GitHub 토큰의 삭제 권한 확인**(클래식 `delete_repo` / 세분화 `Administration: write`)과 GitLab 토큰의 범위·소유자 권한: 값은 채팅·터미널에 붙이지 않는다.
6. **이 컷오버에서 무엇을 할지**(로컬 정리 시험 / 키만 떼기 시험 / 원격 삭제 시험 / 레거시 두 번째 화면 보기 / 고아 지우기), **원격 삭제 시험을 할지(되돌릴 수 없음)와 새 시험 저장소의 이름**.
7. **스모크 저장소 둘을 GitHub에서 직접 지우는 시점**(그 저장소의 `orchd` 배포 키도 함께 사라진다).
8. **`sample-app`·`toy` 같은 레거시 프로젝트를 정말 지울지**(이 컷오버에서는 지우지 않는다), 지운다면 정적 `--git-remote-for` 정리.
9. **시험이 끝난 뒤 GitHub 토큰의 삭제 권한을 줄이거나 토큰을 폐기할지**, `/root/backup-w3f`를 언제 지울지(토큰 사본이 들어 있어 `shred`).
10. **한도(하루 3회·시간당 2회)가 맞는지**, 앞단 프록시의 제한시간이 130초보다 짧다면 올릴지.
11. **W3d(PR·MR, 조건부 자동 병합)로 갈지**(빈 `checks`를 통과로 세지 않는 규칙: 컷오버에서 작업 #47이 이미 보여 줬다).

## 9. 컷오버 기록
아직 없음(컷오버 전). 컷오버를 하면 이 장에 서버에서 본 것만 적는다(21·23·25·27번 문서와 같은 방식). 런북 16장 끝의 "관찰 기록" 표를 옮긴다.
