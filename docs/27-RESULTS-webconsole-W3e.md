# 웹 콘솔 W3e — 결과 기록 (2026-11-12)

> 상태: **컷오버 완료(2026-11-12 22:26 KST, 실서버 orch-vm, main `3830546`). 실서버·실제 GitHub에서 확인한 것과 아직 못 한 것은 6장 맨 앞(6.0)과 9장.** 아래 본문(1~5장, 7~8장과 6장의 원래 목록)은 컷오버 전, 구현을 끝낸 시점의 기록이다: 코드와 시험은 브랜치 `impl/webconsole-w3e`에서 끝났고, 그 시점에는 실제 서버(orch-vm)와 실제 GitHub에 아무것도 하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 15장, 설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `26-PLAN-webconsole-W3e.md`. 출발점은 W3c 컷오버가 끝난 서버(등록부 v2 운영 중, 제공자 `example-group-gitlab`)다.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더·가짜 `git`/`ssh` 경로·명백한 더미 값**)과 ego-browser로 개발용 고정 서버를 본 것(E5 보고서)만 가리킨다. **실제 GitHub, 실제 토큰, 실제 패스키 서명, 리눅스 systemd 샌드박스 안의 broker·orchd, 실제 `uid`의 파일 소유·모드, 실제 OpenSSH 9.6으로의 `git clone`/`push`는 한 번도 쓰지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다. 시험 값은 `ghp_canary-NOT-A-REAL-TOKEN-…` 꼴의 더미다.
> **W3a~W3c의 교훈(임시 폴더 시험이 실서버의 권한 문제를 숨겼음)을 구조에 넣었다:** 새 코드가 실행 사용자·유닛 샌드박스 아래에서 만지는 경로·소유자·모드는 4장의 표로 모았고, 컨트롤러가 컷오버 전에 서버에서 읽기 전용으로 보는 목록은 런북 15.0에 있다.

## 1. 만든 것

**E1. 등록부 버전 3과 순수 검증기.** 프로젝트의 연결이 `gitlab:{project,project_id}`에서 `remote:{path,id}`로 일반화되고, 제공자 종류에 `github`가 더해졌다(`providers[].kind` = `gitlab`|`github`). 버전 1·2 파일은 첫 시작에서 원자적으로 3으로 올라가고 내용은 그대로이며, 올리기 전에 원본을 `registry.json.v1.bak`/`registry.json.v2.bak`(0600, 한 번만, 덮어쓰지 않음)로 남긴다. W3c broker는 버전 3 파일을 깨짐으로 본다. 서명 동작 2개(`link_project`·`attach_remote`)가 늘고 `set_provider.kind`가 `gitlab|github`, `create_project`에 **서명되는 선택 필드 `visibility`**(기본값 없음)가 생겼다. `ValidRemotePath`(2~5조각, 255자), 소유자 필드(GitHub 로그인 규칙은 broker가 서명 전 모양 단계에서 종류별로 적용).
**E2. 제공자 인터페이스와 GitHub 클라이언트.** `RepoProvider`(GitLab 어댑터는 기존 `GitLabAPI`에 위임만), 코드에 고정된 **9개 (메서드, 경로)** GitHub 허용 목록(`GET /user`·`/user/orgs`·`/repos/{o}/{r}`·`/repos/{o}/{r}/branches/main/protection`, `POST /user/repos`·`/orgs/{o}/repos`·`/repos/{o}/{r}/keys`, `PUT …/contents/{path}`·`…/branches/main/protection`), 삭제·`PATCH`·병합·토큰·`/user/keys`·협업자 호출 없음, 보호 `PUT`은 **이번 흐름이 방금 만든 저장소에만**(`justCreated` + 클라이언트가 기억한 이름·ID), `github.com`·`www.github.com`을 API 주소로 거부, 리다이렉트 안 따름, 연결 시점 IP 재확인, 토큰은 `Authorization: Bearer`로만, 응답 본문은 읽지 않고 상태 범주만(`github_refused`·`provider_rate_limited`).
**E3. broker op.** GitHub `create_project`(소유자 확인 → 이름 충돌(대소문자 무시) → 저장소 → 첫 파일들 → 기본 브랜치 `main` → 보호 `PUT` → 배포 키 → **마지막에** 등록부, 중간 실패는 지우지 않고 고아 기록 + `create_partial`), `link_project`·`attach_remote`(저장소를 만들거나 지우거나 설정을 바꾸지 않고 배포 키 하나만 붙인다, 실패는 `link_partial`과 `origin: linked`), `list_groups`의 GitHub 소유자 목록(내 계정·조직).
**E4. orchd.** `internal/orch/keys`(저장소별 ed25519 키를 Go 표준 라이브러리로 만들고 `ssh/keys-staging/<무작위>/`에 두었다가 서명된 해시와 맞을 때만 `ssh/keys/<id>/`로 원자적 이름 바꾸기, 15분 만료, 시작 때 정리), 새 엔드포인트(`/projects/link|attach/propose|apply`, GitHub 생성), 정적 `--git-remote-for`가 있으면 `static_remote_wins`, 키를 인식하는 복제·게시 해석기(`Resolve → (원격, 키 경로, ok)`, 매번 키 파일 검사), 새 오류 코드의 고정 문장, 원장 `project_link`·`project_attach`.
**E5. 웹.** 제공자 종류 선택, 소유자 선택(GitHub는 "그룹 만들기" 없음), **공개/비공개 선택(기본 없음, 굵은 확인 문구)**, 저장소별 키 지문 표시, "기존 저장소 연결"·"원격 연결", 고아 `origin`별 문구, 브라우저의 `change` 재계산을 새 필드까지 확장.
**E6(이 작업).** 적대 시험 30개(broker 16, orchd api 6, keys 2, clone 2, publish 1, node 3), 변이 시험, 이 문서, 런북 15장, 문서 19·`infra/README.md` 갱신.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-12, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0. 세는 방식은 W3a~W3c와 같다: 최상위 시험 함수와 하위 시험을 따로 센다(`go test -json` 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**. 최상위 시험 함수 1654개 중 **1652 통과, 0 실패, 2 건너뜀**(하위 시험 982개 모두 통과). 건너뜀 2개는 W1~W3c와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollisionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사). **솔직히:** (E6 첫 라운드의) 전체 실행 중 한 번은 `internal/broker`의 **W2 시험 하나**(`TestBrokerForeignUidAndOversizeFlood`, "pipelined requests made 0 upstream calls")가 한 번 실패했다(변이 시험이 막 끝난 직후라 부하가 있던 시점). 그 시험만 `-race -count=15`로 15번 모두 통과했고 `internal/broker` 패키지를 `-race`로 다시 돌려 통과했으며, 이 변경은 그 시험이나 해당 코드를 건드리지 않았다. 부하 아래에서 나오는 시간 의존 시험으로 보지만 원인을 가려내지는 못했다. 그 뒤 처음부터 다시 돈 전체 실행은 깨끗했고, 최종 검토 수정 라운드의 전체 실행(위 숫자)도 첫 실행에서 깨끗했다.
- `node --test "internal/orch/web/test/*.test.js"`: **257개 전부 통과**(E5 253개 + E6 3개 + 최종 검토 1개).
- **게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, 하위 `tokens`·`providers`·`w3e` 모두 PASS, **`SKIP` 없음**.
- W3c(25번 문서) 대비 최상위 통과 1473 → 1652(+179), 하위 시험 935 → 982(+47), node 231 → 257(+26). E6가 더한 것은 적대 시험 30개(Go 27, node 3)와 최종 검토의 시험 5개(Go 4, node 1).

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 14 | 0 | 0 | 0 |
| `cmd/orchd` | 34 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 68 | 0 | 0 | 76 |
| `internal/broker` | 378 | 0 | 0 | 275 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 45 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 371 | 0 | 0 | 34 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 11 | 0 | 0 | 0 |
| `internal/orch/clone` | 36 | 0 | 0 | 0 |
| `internal/orch/keys` | 13 | 0 | 0 | 0 |
| `internal/orch/knownhosts` | 4 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 27 | 0 | 0 | 0 |
| `internal/orch/refine` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 43 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 11 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/registrymsg` | 31 | 0 | 0 | 6 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

### E6가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3e_test.go`(16개), orchd `internal/orch/api/adversarial_w3e_test.go`(6개), `internal/orch/keys/adversarial_w3e_test.go`(2개), `internal/orch/clone/adversarial_w3e_test.go`(2개), `internal/orch/publish/adversarial_w3e_test.go`(1개), node `internal/orch/web/test/w3e.test.js`의 `TestAdversarial…`(3개). 이미 같은 일을 하는 E1~E5 시험은 복사하지 않고 **전체 흐름 위의 시험**으로 새로 썼다. W3c의 적대 시험 24개는 수정 없이 모두 다시 통과한다.

| 시험 | 하는 일 |
|---|---|
| `TestGitHubTokenNeverLeavesBroker` | 읽기·쓰기 흐름 30여 가지(성공, 모든 단계 실패, GitHub가 토큰을 모든 오류 본문에 되풀이하는 경우, 한도, link/attach 실패)와 **create·link·attach 등록부 쓰기 중 패닉(값이 토큰)**, 깨진 요청: 응답·로그·상태 폴더(토큰 저장소 제외)·GitHub가 받은 요청 줄·본문·**Authorization 아닌 모든 헤더**에 토큰(GitHub·GitLab 둘 다) 없음, `Authorization`은 요청마다 정확히 하나 |
| `TestEveryGitHubRequestIsAllowlisted` | 모든 흐름과 적대적 이름의 GitHub 요청 전체가 **시험 안에 다시 쓴** 9행 허용 목록의 부분집합(메서드·경로·쿼리 키·**본문 키**), `DELETE/PATCH/HEAD` 없음, `/user/keys`·협업자·병합 등 없음, 콘텐츠·보호 `PUT`은 같은 흐름이 만든 저장소에만, 첫 파일 `sha` 없음, 배포 키 `read_only`는 불리언, 9행이 모두 쓰임. 발송 기본 동작에 파괴적 요청 12개를 직접 줘도 네트워크 0. 적대적 원격 경로·소유자는 GitHub 요청 0 |
| `TestGitHubProviderAddressesRefused` | **운영 주소 정책**에서 GitHub 종류 `set_provider`가 `github.com`·`www.github.com`(대소문자·포트·경로 변형)·루프백·메타데이터·링크 로컬·숫자 표기·`userinfo`·http 공개 주소 등 40여 `base_url`과 15개 `ssh_url`을 거부, `api.github.com`이 금지 주소로 풀리면 거부, 한 번 통과한 뒤 연결 시점 재바인딩도 거부, 함정 서버 접속 0 |
| `TestGitHubRedirectNotFollowedAndTokenNotSentToOtherHost` | 301·302·303·307·308을 모든 요청 또는 쓰기에만 → 다른 호스트가 받은 요청 0, 생성·link·attach 어느 것도 성공·등록으로 끝나지 않음. 제공자 둘의 토큰은 각자의 호스트에만 감 |
| `TestGitHubOwnerIsNeverTrustedFromTheSignedString` | 낯선 로그인은 `group_unknown`(GitHub는 소유자 목록 읽기만 봄), 계정·조직은 **GitHub의 철자**로 쓰고 등록, 한 로그인이 아닌 글자 11종은 모양 단계에서 ID 소모·요청 없이 거부 |
| `TestGitHubCaseInsensitiveCollisionForCreateAndLink` | 생성: `Demo`/`demo`(내 계정·조직) → `project_name_taken`, 쓰기 0. link: `Foo`/`foo`/`FOO` 어느 철자로든 한 번만, 둘째 link·attach는 `already_linked`, GitHub 요청 0 |
| `TestVisibilityNeverDefaultedOrForcedAndPublicOnlyWhenSigned` | 없으면 `visibility_required`(요청 0, ID 미소모), 잘못된 값 13종, **서명 뒤 바꾼 값 5가지(change를 다시 계산해도 `bad_signature`)**, GitLab에는 모든 값 거부·GitLab 요청 0, 서명된 값이 그대로 `private` 본문이 됨, link·attach는 저장소를 만들지 않음, GitLab은 항상 `private` |
| `TestCrossActionSignaturesRefusedForAllPairsW3e` | **서명 동작 12개(GitLab/GitHub `create_project`, GitLab/GitHub `set_provider` 별도)의 모든 순서쌍 132개** × (서명만 / 서명+`change`+요청 ID+시각) → 전부 거부, 제공자 요청 0, 등록부·토큰 저장소·사용한 ID 불변. 거부 코드에 `bad_signature`·`change_mismatch`가 실제로 나옴(통제), 각 동작은 자기 서명으로는 성공 |
| `TestSignatureOverDifferentGitHubFieldsRefused` | create(소유자·visibility·publish·ai_context·template·description·provider·키 해시), link(원격 경로·ai_context·publish·provider·키 해시), attach(원격 경로·publish·provider·키 해시)를 하나씩 바꾼 요청 + 주체 변경 → 거부(change를 다시 계산하면 `bad_signature`), 제공자 요청 0, 등록부 불변. 통제: 변경 없는 요청은 한 번만 성공 |
| `TestLinkAttachNeverCreateModifyOrDeleteARemoteRepo` | link·attach 9개 시나리오에서 GitHub가 받은 쓰기는 `POST …/keys` 뿐이고 재시도 없음 |
| `TestDuplicateOrRefusedDeployKeyIsNeverSuccess` | GitHub 400·401·403·404·422·429와 GitLab 400의 키 거부 → 거부 코드, 등록 없음, 고아 없음, 서명 소모 |
| `TestAttachNeverWidensOrChangesAnythingButTheRemote` | `ai_context` 참/거짓 모두 불변, 경로·종류·id·auto_merge 불변, `ai_context`·`path`·`kind`·`auto_merge` 키는 `bad_request`, 옛 매핑·이미 연결된 프로젝트는 `already_linked`(요청 0) |
| `TestPartialAndUnknownOutcomesNeverDeleteAndOrphanOriginIsRight` | create 8·link 3·attach 3 시나리오(5xx, 첫 파일, 기본 브랜치, 약한 규칙, 유료 요금제 403, 키, 등록부 쓰기 실패, 패닉): `DELETE/PATCH` 없음, 쓰기는 한 번씩, 고아의 단계와 **`created`/`linked` 출처가 맞음**(link는 `created`가 아니고 문장이 "지우지 마세요"), 생성 4xx 거부는 고아 없음 |
| `TestRegistryV3BrokenFailsClosedAndDoesNotWidenAIContext` | 깨진 v3 파일 20종(잘림·쓰레기·빈 파일·문자열 버전·`version`/`Version` 중복·알 수 없는 키·v2의 `gitlab` 키·제공자 중복·모르는 종류·자동 병합 켜짐·나쁜 고아 출처/경로·제공자 없는 `remote`·`.git`·`..` 경로·버전 4·뒤따르는 데이터): 모든 읽기·쓰기·`refine`이 `registry_unavailable`, AI 전송 0(옛 `context_repos`로 폴백 안 함), 파일 불변. **모드 5종·남의 소유자**도 거부. 유효한 v3 통제 |
| `TestRegistryV2MigrationKeepsContentWritesBackupOnceAndOldVersionsStillRefuseV3` | v2→v3에서 `ai_context`가 하나도 바뀌지 않고 옛 설정이 AI 경로를 넓히지 않음, `.v2.bak`이 원본과 같고 0600, 이후 쓰기·재시작이 바꾸지 않음, 기존 백업은 덮어쓰지 않음, v3 파일은 다시 올리지 않음, **v3 모양을 버전 2·1로 고쳐도 깨짐** |
| `TestStatusAndProjectsNoSecretsAfterGitHubFlows` | `status`·`projects`·`providers`·`tokens`·`list_groups`와 create·link·attach의 오류 응답·로그에 토큰·키·경로·서명자 줄·요청 ID·`change`·배포 키 줄이 없음, 고아는 값과 출처만 |
| `TestPrivateKeyNeverLeavesOrchdAcrossAllOutcomesW3e` | 결과 6가지(성공·거부·create/link 부분·알 수 없음·재시도) × 동작 3가지: 제안·적용·재시도·등록부 읽기·복제 응답, **broker 인자 전체**, 원장, 로그에 개인 키 내용(줄 단위)·`PRIVATE KEY`가 없고 키 폴더 경로도 응답에 없음. broker가 받은 `deploy_key`는 공개 줄이고 해시가 서명된 값 |
| `TestStagedKeyBoundToItsHashNeverReusedNeverALiveKeyW3e` | 제안 18번이 모두 새 키, 알 수 없는 결과 뒤에도 새 키, **이미 쓰이는(승격된) 키의 해시로는 다른 프로젝트를 만들 수 없음**(`key_expired`, broker 쓰기 0, 키 불변), 스테이징 파일 교체는 `deploy_key_mismatch`, 증명 가능한 거부 뒤에는 키가 사라지고 같은 서명은 `key_expired` |
| `TestKeyAreaSymlinksAndModesRefusedAtEndpointsW3e` | `keys/`·`keys/<id>`가 심볼릭 링크, 키가 공용 키로의 링크·그룹 읽기·폴더가 열림·폴더·빈 파일: 복제 `key_unavailable`, 복제 실행 0. 미리 있는 키 폴더·링크 위의 GitHub 생성은 거부, 링크 뒤에는 아무것도 쓰이지 않음 |
| `TestForgedFieldsRefusedBeforeBrokerForNewActionsW3e` | 3개 동작 × 위조 14종(여분 키·필드, 빠진 필드, 대소문자·경로 모양 주체, 시간·요청 ID, 다른 동작, 서명 없음, 거대한 설명, 클라이언트 키 해시, 문자열 아닌 visibility): 성공 없음, broker에는 스테이징된 키만 전달됨, 승격된 키 없음. 제안에 키·해시·경로를 실어도 400 |
| `TestBrokerHostileSentencesNeverReachResponsesLedgerOrLogW3e` | broker의 적대적 오류 문장·상세·결과 6종 × 동작 3개: 토큰·개인 키 문구·경로가 응답·원장·로그에 없고 원장 키는 `action,actor,code,ok,subject` |
| `TestRefusalsBeforeSignatureLeaveNoKeyNoWriteNoEventW3e` | `static_remote_wins`(제안·적용 사이에 생겨도)·`remote_name_mismatch`·`already_linked`(옛 매핑 포함)·`project_exists`·`provider_unknown`·`project_unknown`: 서명 메시지 없음, 스테이징된 키 0, broker 쓰기 0, 원장 0 |
| `TestSweepTouchesOnlyOwnDirectoriesW3e` (keys) | 스테이징에 바깥 폴더로의 링크·일반 파일·다른 이름·링크인 스테이징 이름이 섞여 있어도 만료된 자기 폴더만 지워지고 링크는 따라가지 않으며 `keys/` 아래는 불변 |
| `TestSymlinkSwapsInKeysAndStagingRefusedW3e` (keys) | `keys-staging`·`keys`·`keys/<id>`가 링크, 스테이징 폴더가 만든 뒤 링크로 바뀜: 거부, 바깥에 아무것도 안 써짐. 20개 키가 모두 새 것이고 승격·만료된 키는 다시 쓸 수 없음 |
| `TestResolverGitHubFailClosedMatrixW3e` (clone) | 이름 불일치·`.git`·`..`·옵션 모양·깊은 경로·모르는 제공자·원격 없음·키 없음, 호스트 키가 와일드카드·`@cert-authority`·`@revoked`·부정·비슷한 호스트·주석뿐: 전부 **거부(기본 원격 아님, 공용 키 아님)**. broker가 꺼지고 표가 낡으면 모르는 저장소도 거부 |
| `TestCloneGitHubCommandLineAndEnvironmentW3e` (clone) | 환경 변수에 `GIT_SSH_COMMAND`·에이전트·프록시 명령이 있어도 git은 **한 번**, 등록부의 원격과 그 프로젝트의 키 하나(`-i`는 하나, 공용 키 없음)만 쓰고, 적대적 원격 14종·키 경로 7종·고정 안 된 호스트 4종·목적지 4종은 git을 실행하지 않음 |
| `TestPublishNeverLeavesRegistryRemoteAndPerProjectKeyW3e` (publish) | 해석 못 한 제공자 연결 프로젝트는 기본 원격으로 밀지 않음, 해석기 답이 안전한 ssh 원격이 아니면 git 실행 0, 환경의 ssh 설정은 자식에 안 감, 개인 키가 느슨해지거나 링크로 바뀌면 거부 |
| node `TestAdversarialApplySendsOnlyWhatWasConfirmedW3e` | create·link·attach 적용 본문의 키는 서명 필드뿐이고 `fields`가 확인한 것과 같으며 키 해시·`change`는 브라우저 것 |
| node `TestAdversarialServerCannotAddOrDropFieldsW3e` | 서버가 GitLab 제안에 `visibility`·여분 필드를 더하거나 `publish`를 빼거나 그룹·소유자·visibility를 바꾸면 서명 단추가 없고 서명·적용 0. **알림:** 거부는 "불일치 경고"이거나 폼의 문제 문구다(`internal` 같은 열거 밖 값은 `change` 계산 자체가 거부해 경고 없이 문제 문구로 끝남). 시험은 둘 중 하나가 보일 것과 서명 단추·서명·적용 없음을 확인한다 |
| `TestProviderKindFlipBetweenShapeAndLockRefusedW3e` (broker, 최종 검토) | 모양 단계와 이름 잠금 사이에 제공자 종류가 gitlab→github(visibility 없음) 또는 github→gitlab(visibility 있음)으로 바뀌면 `visibility_required`/`visibility_not_allowed`로 거부, 제공자 요청 0, 고아·등록 없음 |
| `TestDeployKeyLineRuleVectorsW3e` (broker) + `TestBrokerDeployKeyRuleCopyMatchesVectorsW3e` (api, 최종 검토) | 같은 벡터 파일(`internal/broker/testdata/deploykey_vectors.json`, 26개)을 broker의 `deployKeyRE`와 orchd의 사본 `brokerDeployKeyRE`에 돌려 동작이 같음을 고정 |
| `TestReconcileReadsTheRegistryOncePerPassW3e` (api, 최종 검토) | 보관 키가 셋이어도 조정 한 번이 등록부를 한 번만 읽음(보관 키가 없으면 읽지 않음) |
| node `TestAttachWriteWarnsThatLocalHistoryMayBePushedW3e` (최종 검토) | 게시 권한 쓰기인 attach의 확인 화면에 굵은 경고 한 줄(로컬 이력이 원격으로 올라갈 수 있고 공개 저장소면 누구나 읽음)이 정확히 한 번, 읽기·link에는 없음. 서버가 빼거나 두 번 말하거나 읽기에 붙이면 서명 단추 없음 |
| node `TestAdversarialOrphanWithUnknownOriginNeverDroppedNorCalledCreatedW3e` | 출처가 null·빈 값·숫자·마크업·대문자·객체인 고아는 목록에 남고 "출처 불명"이며 "직접 지우세요"라고 하지 않음 |

**시험을 새로 쓰는 중 발견한 것.** **제품 코드의 결함은 발견하지 못했다.** 적대 시험은 첫 실행에서 시험 작성 실수(허용 본문 키 `description`, 등록부 중복 id, `Sprint`가 만든 공백이 든 id, 실제 인터넷 주소로 연결할 뻔한 재바인딩 사례)와 사실 확인 두 가지 외에 실패한 것이 없다: ① `SweepAll`은 **우리 사용자의 진짜 폴더이면서 스테이징 이름 모양인 것**(권한이 느슨한 것 포함)을 시작 때 지운다(설계대로, 링크·파일·다른 이름은 건드리지 않음). ② 웹은 열거 밖 `visibility`나 필드 수가 다른 `fields`를 불일치 경고가 아니라 폼 문제 문구로 거부한다(서명 단추 없음). 한 관찰: 재바인딩 시험에 공개 IP(`140.82.112.6`)와 루프백을 섞은 답을 넣으면 공개 주소로 **실제 연결**을 시도할 수 있어 그 사례를 뺐다(시험은 네트워크에 나가면 안 된다).

### 최종 검토 수정 라운드
코드: ① broker `create_project`는 이름 잠금 뒤 다시 읽은 제공자에 종류·visibility·소유자 규칙(`createProviderRules`)을 다시 적용한다(모양 단계와 같은 함수). ② orchd `reconcileKeys`는 한 번의 조정에서 등록부를 **한 번**만 읽는다(키마다 읽던 것을 고침, 20초 상한 유지). **좁은 경합(받아들임):** 등록부는 키의 해시를 담지 않으므로 "등록됨"은 "그 id가 그 제공자에 연결됨"이다. 알 수 없는 결과였던 첫 적용과 같은 id의 다른 적용이 둘 다 등록되어야 어긋나는데, broker의 id·원격 잠금과 요청 ID 소모가 그 창을 좁힌다. ③ attach의 게시 권한이 쓰기이면 서버 미리보기와 브라우저의 정확한 한 줄 검사에 경고가 더해졌다(`PREVIEW_ATTACH_PUSH_WARN`, Go와 웹 상수 일치 시험). **바뀐 기존 시험·파일(이유):** `internal/orch/api/web_fixtures_w3c_test.go`(새 상수의 고정 단언 추가: 기존 단언은 그대로), `internal/orch/web/dev_test.go`(개발용 고정 서버의 attach 미리보기가 쓰기일 때 새 줄을 냄: 실제 서버와 같게), `internal/orch/web/test/w3e.test.js`의 `previewFor` 도우미(서버의 미리보기를 복제한 것: attach+쓰기에서 새 줄), 고정 파일 `w3e-vectors.json`(재생성: attach 쓰기 미리보기가 한 줄 늘어남). 그 밖의 기존 시험은 바꾸지 않았다.

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖(`/tmp/aigate-mut-w3e/copy*`)으로 복사**(`rsync`, `.git`·`.superpowers`·`graphify-out` 제외)해 한 곳만 망가뜨리고 관련 패키지를 돌렸다. 컴파일되게 `_ = x`·`error(nil)` 형태로 바꿨다. 변이마다 복사본의 원래 파일로 되돌렸고 작업 트리는 건드리지 않았다. 도구는 `/tmp/aigate-mut-w3e/mutate.py`(3개 복사본에서 병렬). 전부 구현자 실행이다(검토자가 아직 재현하지 않음). 열 "패키지"는 돌린 패키지다: **broker 변이는 `internal/broker` 전체, `keys`·`clone`은 패키지 전체, `api` 변이는 패키지 전체가 아니라 W3c+W3e 관련 시험 부분집합**(`internal/orch/api`의 `w3e_*`·`adversarial_*`·`w3c_*` 파일의 시험 약 70개; 전체 패키지는 3분 걸려 변이마다 돌리지 않았다). 굵게 = E6의 새 시험.

| # | 망가뜨린 곳 | 패키지 | 실패한 시험 (개수) |
|---|---|---|---|
| ① | 서명 검증 제거(`VerifyRegistry` 결과를 항상 성공으로) | broker | TestApprovalAndRegistryMessagesStillSeparate, TestApprovalMessageCannotAuthorizeRegistryChange, TestChangeRequiredAndMismatchBurnsNoID, TestCrossActionSignaturesRefused, **TestCrossActionSignaturesRefusedForAllPairsW3e**, TestCrossActionSignaturesRefusedW3e, TestFailedRequestsDoNotConsumeRequestID, TestInvalidSignatureDoesNotConsumeRequestID … (+18) (26) |
| ② | `change` 일치 검사 제거 | broker | TestChangeMismatchBurnStateInUsedFile, TestChangeMismatchBurnsNoID, TestChangeRequiredAndMismatchBurnsNoID, **TestCrossActionSignaturesRefusedForAllPairsW3e**, TestInvalidSignatureDoesNotConsumeRequestID, TestProviderBaseURLChangeNeedsSignatureAndShowsInChange, TestRegisterChangeMismatchRefused, TestRegistryNeverWrittenWithoutValidSignature … (+11) (19) |
| ③ | GitHub 허용 목록 검사 제거(`do`가 아무 경로나 보냄) | broker | **TestEveryGitHubRequestIsAllowlisted**, TestGitHubDoRefusesOffList (2) |
| ④ | `github.com`·`www.github.com` API 주소 거부 제거 | broker | **TestGitHubProviderAddressesRefused**, TestGitHubProviderURLRules (2) |
| ⑤ | 리다이렉트 허용(`CheckRedirect` 제거) | broker | TestGitHubNoRedirect, **TestGitHubRedirectNotFollowedAndTokenNotSentToOtherHost**, TestSeamsNotReachableFromProduction (3) |
| ⑥ | `visibility` 필수 제거(broker 모양 단계: 없으면 `public` 주입) | broker | TestE1CodesHaveFixedSentences, TestVisibilityMustBeSigned, **TestVisibilityNeverDefaultedOrForcedAndPublicOnlyWhenSigned**, TestVisibilityRequiredForGitHubForbiddenForGitLabAtTheOp (4) |
| ⑦ | 소유자를 서명된 글자에서 그대로 씀(제공자 철자 대신) | broker | TestCreateProjectGitHubOwnerLookupNotTrustingTheString, **TestGitHubOwnerIsNeverTrustedFromTheSignedString**, TestOwnerFieldPerProviderRulesAtShape (3) |
| ⑧ | 대소문자 무시 충돌 검사 제거(`SameRepoPath`가 정확 비교) | broker | TestAlreadyLinked, **TestAttachNeverWidensOrChangesAnythingButTheRemote**, TestAttachRemoteSetsProviderAndRemoteOnly, TestAttachRemoteStaticWins, TestChangeMismatchBurnStateInUsedFile, TestChangeMismatchBurnsNoID, **TestCrossActionSignaturesRefusedForAllPairsW3e**, TestCrossActionSignaturesRefusedW3e … (+16) (24) |
| ⑨ | `justCreated` 묶기 제거(`madeHere`가 항상 참) | broker | TestGitHubCreateRepoBodyAndGuards, TestGitHubJustCreatedBoundToThisClient, TestProtectionPutOnlyForJustCreatedRepo (3) |
| ⑩ | 보호 `PUT` 응답 확인 제거 | broker | TestGitHubProtectionResponseVerified (1) |
| ⑪ | 거부·중복된 배포 키를 성공으로 취급(link/attach) | broker | **TestDuplicateOrRefusedDeployKeyIsNeverSuccess**, TestLinkKeyAlreadyInUseNotSuccess (2) |
| ⑫ | 불확실한 생성 오류에서 고아 기록 안 함 | broker | TestCanaryTokenNeverInAnyOutput, TestCreateProjectGitHubPartialFailures, TestCreateProjectGitHubRegistryWriteFailureIsOrphan, TestGitHubCreatePanicIsOrphanAndNeverLeaks, TestLinkAttachAndCreateTimeoutIsUnknownOutcomeWithOrphan, TestOrphanOriginAndLinkPartialSentence, TestPanicAfterProjectWriteLeavesOrphan, **TestPartialAndUnknownOutcomesNeverDeleteAndOrphanOriginIsRight** … (+8) (16) |
| ⑬ | 생성 4xx 거부에도 고아 기록 | broker | TestCreateProjectGitHubRefusedCreateIsNotAnOrphan, **TestPartialAndUnknownOutcomesNeverDeleteAndOrphanOriginIsRight** (2) |
| ⑭ | 제안마다 같은 키 재사용(고정 시드) | keys, api | TestEveryStageMakesANewKey, TestHoldIsBoundedAndRetainedKeysSurviveExpiry, TestStagedChecksHashExpiryAndTampering, TestStagingKeyExpiresAndSweepsOnlyOwn, **TestSymlinkSwapsInKeysAndStagingRefusedW3e**, TestCreateProposeIncludesPerRepoKeyHash, TestForwardedKeyIsNeverLostOnUnknownOutcomes, **TestStagedKeyBoundToItsHashNeverReusedNeverALiveKeyW3e** (8) |
| ⑮ | 스테이징 키 해시 비교 제거 | keys, api | TestStagedChecksHashExpiryAndTampering, **TestStagedKeyBoundToItsHashNeverReusedNeverALiveKeyW3e**, TestStagedKeyChangedOrUnknownIsRefusedBeforeTheBroker (3) |
| ⑯ | 제안 응답에 개인 키를 실음 | api | TestPrivateKeyNeverInAnyOutput, **TestPrivateKeyNeverLeavesOrchdAcrossAllOutcomesW3e** (2) |
| ⑰ | `request_reused`를 증명 가능한 거부로 보고 키를 지움 | api | TestForwardedKeyIsNeverLostOnUnknownOutcomes (1) |
| ⑱ | 해석기가 없는 저장소별 키를 받아들임 | clone, api | TestResolverRefusesUnusableGitHubKey, TestResolverReturnsKeyPathFromRegistryOnly (2) |
| ⑲ | 해석기가 호스트 고정 검사를 건너뜀 | clone, api | **TestResolverGitHubFailClosedMatrixW3e**, TestResolverGitHubHostMustBePinned, TestResolverRequiresPinnedHost, TestResolverNeverTrustsRequestData (4) |
| ⑳ | `static_remote_wins` 제거(attach) | api | TestAttachStaticRemoteWins, **TestRefusalsBeforeSignatureLeaveNoKeyNoWriteNoEventW3e** (2) |
| ㉑ | attach가 `ai_context`를 켬 | broker | **TestAttachNeverWidensOrChangesAnythingButTheRemote**, TestAttachRemoteSetsProviderAndRemoteOnly (2) |
| ㉒ | link 부분 실패의 고아 출처가 `created` | broker | TestLinkAttachAndCreateTimeoutIsUnknownOutcomeWithOrphan, TestOrphanOriginAndLinkPartialSentence, **TestPartialAndUnknownOutcomesNeverDeleteAndOrphanOriginIsRight**, **TestStatusAndProjectsNoSecretsAfterGitHubFlows** (4) |
| ㉓ | orchd: 출처 없는 link 상세를 `created`로 | api | TestOriginlessPartialDetailDefaultsByAction (1) |
| ㉔ | 웹 `change` 비교 제거(`providers.js`) | node | TestSignOnlyWhenChangeMatches, TestSignOnlyWhenChangeMatchesW3e (2) |
| ㉕ | 웹: 출처를 모르는 고아를 목록에서 버림(`projects.js`) | node | **TestAdversarialOrphanWithUnknownOriginNeverDroppedNorCalledCreatedW3e**, TestNoPasswordLikeMarkupW3e, TestOrphanOriginSafeDefaultsW3e (3) |
| ㉖ | 웹: 공개 여부 기본값 `private` 주입(`projects.js`) | node | TestVisibilityNoDefaultWhenGitHubIsTheFirstProviderW3e (1) |

**26개 모두 잡혔다. 살아남은 변이는 없다.** 솔직히 기록할 것: ① ①②⑨는 첫 실행에서 **컴파일 오류**(`declared and not used`, 안 쓴 import)로 "잡힘"처럼 보였고 변이를 컴파일되게 고쳐 다시 돌린 결과가 위 표다. ② ③(허용 목록 검사 제거)은 처음 E2의 `TestGitHubDoRefusesOffList` 하나만 잡았다(공개 메서드는 허용 목록 밖 요청을 만들 수 없어 전체 흐름 시험이 못 잡음, W3c ③과 같은 구조) → 발송 기본 동작에 파괴적·토큰 모양 요청 12개를 직접 주는 단계를 `TestEveryGitHubRequestIsAllowlisted`에 더하고 최신 복사본으로 다시 돌려 이제 둘이 잡는다. ③ ⑩(`madeHere`)·⑪(보호 응답)은 E2의 시험이 주로 잡는다(전체 흐름의 가짜 GitHub가 약한 보호 응답을 주지 않아 새 시험은 못 잡음). ④ 웹 변이는 `node --test` 전체를 돌렸고, ㉖(공개 여부 기본값 주입)은 폼이 공급자를 바꿔 값을 비우므로 **GitHub가 첫 제공자인 경우를 보는 E5의 시험 하나만** 잡는다(E5 보고서가 이미 적은 대로). ⑤ 변이는 구현자가 정한 한 곳씩이라 같은 결함의 다른 모양은 확인하지 않았다.

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

E1~E5가 각자 적은 표를 모았다. **이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험에 기초한 것이고 실서버에서 본 것이 아니다.** 컷오버 전 확인 목록은 런북 15.0. **실행 사용자와 샌드박스(변경 없음, 13·14장과 같음).** broker: `orch-broker`(uid 999), `ProtectSystem=strict`, **`ProtectHome=yes`(`/home`을 보지 못한다)**, `PrivateTmp`, `NoNewPrivileges`, `CapabilityBoundingSet=` 비어 있음, `MemoryDenyWriteExecute`, `SystemCallFilter=@system-service`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, 쓰기는 `/var/lib/orch-broker`·`/run/orch-broker`만. orchd: `orch`, `ProtectSystem=strict`, `ProtectHome=read-only`, **`ReadWritePaths=/home/orch/orchd /home/orch/repos`**, `PrivateTmp=yes`(`infra/systemd/*.service`, 바뀌지 않음).

| 경로·대상 | 소유자 / 모드 | 접근하는 쪽 | 하는 일 | 샌드박스·보호 규칙 |
|---|---|---|---|---|
| `/var/lib/orch-broker/registry.json` | `orch-broker` 0600(폴더 0700) | broker만 | **버전 2 → 3 마이그레이션(첫 시작에서 원자적 쓰기)**, GitHub 제공자·프로젝트·고아(`origin`) 추가(임시 파일 → rename) | `ReadWritePaths`. **W3c broker는 버전 3을 깨짐으로 봄 → 롤백은 등록부 복원** |
| `/var/lib/orch-broker/registry.json.v2.bak` | `orch-broker` 0600 | broker가 한 번 씀 | 마이그레이션 직전의 v2 원본(임시 파일 + `link`로 덮어쓰기 불가, 파일·폴더 fsync). 쓰기 실패는 시작 실패(v2 파일은 그대로) | `ReadWritePaths` 안. 롤백의 보조 사본(root 백업이 우선) |
| `/var/lib/orch-broker/{tokens.json, secrets/<이름>}` | 0600 / 0400(폴더 0700) | broker | **호출마다 읽기만**: GitHub 호출 때 `github` 종류 토큰. 값은 `Authorization: Bearer` 헤더로만 나감 | 변경 없음 |
| 제공자 주소 `https://api.github.com`(공개 IP) | — | **broker**(uid `orch-broker`) | HTTPS 호출 9종, 연결 시점 주소 재검사(공개 주소 허용), 리다이렉트 안 따름, 연결 10초·전체 15초·본문 1 MiB | **`infra/nftables/orchestrator.nft:36`이 공인 주소로의 80·443을 이미 허용하므로(`tcp dport { 80, 443 } ip daddr != @private4 accept`) 실서버에서도 이미 열려 있을 가능성이 높다: 방화벽 결정 불필요, 확인만(런북 15.0 (e))** |
| `github.com:22` (또는 선택 B: `ssh.github.com:443`) | — | **orchd**(`orch`)의 `ssh` 자식 | 복제·게시 | **22는 닫혀 있음: 사용자 결정**(A: 22를 여는 규칙, B: 새 규칙 없이 `ssh_url = ssh://git@ssh.github.com:443`, known_hosts 줄은 `[ssh.github.com]:443 …`). 호스트 키는 `known_hosts_gitlab`에 사용자가 검증해 넣어야 함 |
| `/home/orch/orchd/ssh/` | **`orch:orch`**, 진짜 폴더(심볼릭 링크 아님), orch가 쓸 수 있어야 함 | orchd | `keys/`·`keys-staging/`가 이 아래 만들어짐 | **`ReadWritePaths=/home/orch/orchd` 안**. 실서버에서 `mkdir`·모드가 되는지 **미확인** |
| `/home/orch/orchd/ssh/keys/` | `orch:orch` 0700 | orchd | 승격된 저장소별 키의 부모. 승격 때 처음 만들어짐 | 같음 |
| `/home/orch/orchd/ssh/keys/<프로젝트 id>/` | `orch:orch` 0700 | orchd(만들기·옮기기), **`ssh` 자식**(개인 키 읽기) | 저장소 하나의 키 쌍. **이름 바꾸기(`RENAME_NOREPLACE`)로 승격**: 이미 있는 폴더·링크는 덮어쓰지 않음 | 같음 |
| `…/keys/<id>/id_ed25519` | `orch:orch` **0600**, 일반 파일, 비어 있지 않음 | **`ssh` 자식만**(`-i`로 지정). **orchd 코드는 만들 때 한 번 쓰고 다시 읽지 않는다**(AST 시험) | 개인 키(OpenSSH 형식, Go 표준 라이브러리로 생성). 길 전체(폴더 체인 포함)를 **복제·게시 때마다** 메타데이터로 검사: 모드·소유자·링크 아님 | 같음 |
| `…/keys/<id>/id_ed25519.pub` | `orch:orch` 0644 | orchd | 공개 키 한 줄(`ssh-ed25519 … orchd-deploy`), 서명되는 해시의 대상 | 같음 |
| `/home/orch/orchd/ssh/keys-staging/<32자 hex>/` | `orch:orch` 0700(같은 내용) | orchd | 제안 도중의 임시 키. 15분 만료, 지연 정리 + **orchd 시작 때 전부 정리**(우리 사용자의 진짜 폴더이고 스테이징 이름 모양인 것만: 링크·파일·다른 이름은 건드리지 않음). 인덱스는 메모리 → **재시작하면 서명 대기 제안은 `key_expired`** | 같음 |
| `/home/orch/orchd/ssh/known_hosts_gitlab` | `orch`, 그룹·기타 쓰기 불가 | orchd(읽기), `ssh`(읽기, `UpdateHostKeys=no`) | **새 항목: 평문 `github.com`(정확히)**, 사용자가 GitHub 공식 지문과 대조한 뒤 추가. 와일드카드·`@cert-authority` 줄은 고정으로 치지 않음 | 변경(내용 추가) |
| `/home/orch/repos/<이름>` | `orch:orch` 0700 | **orchd**: `os.Mkdir` 뒤 git | 새 GitHub 프로젝트·연결한 저장소의 복제 위치(W3c와 같음) | `ReadWritePaths=/home/orch/repos` |
| orchd 원장 | 기존 | orchd | 새 이벤트 `project_link`·`project_attach`(키 `actor,action,subject,ok,code`) | 기존과 같음 |
| `policy.yml` | root/관리자 | orchd | `registered` 클래스 필요(link·create), `attach_remote`는 불필요 | W3c에서 이미 필요 |
| `/root/backup-w3e` | root 0700 | 사람(root) | 롤백용 바이너리·`registry.json`(v2)·정책·`known_hosts` 사본 | — |
| 웹 정적 파일 | orchd 바이너리에 내장 | orchd | 갱신된 `views/*.js`(경로·모드 변경 없음) | — |

새 유닛·새 플래그·새 설정 키·새 포트·DB 스키마 변경(v8 그대로)은 없다. **ssh 경로(22를 열지, 443 ssh를 쓸지)는 사용자 결정이고 이 도구는 nft를 바꾸지 않는다.** broker·orchd 사이의 `projects` 응답 필드 이름이 바뀌므로(`gitlab` → `remote`) 둘의 버전 차이 구간은 위험하다(런북 ⚠ 3: 옛 orchd + 새 broker는 W3c 제공자 연결 프로젝트를 기본 원격으로 게시할 수 있다). **실서버에서만 알 수 있는 것은 6장.**

## 5. 설계서와 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| W3e-1 | **broker→orchd `projects` 응답의 연결 필드가 `gitlab`에서 `remote`로**(콘솔 쪽 JSON은 그대로). 새 orchd의 해석기는 제공자 연결이 있으면 원격이 없어도 "거부"로 분류. **런북은 orchd를 먼저 멈추고 broker → orchd 순서로 갱신** | 어긋나면(옛 orchd + 새 broker) W3c 프로젝트가 기본 원격으로 게시될 수 있음 |
| W3e-2 | **등록부 v3, v1/v2 백업 `*.bak`을 한 번만 씀**(계획에는 없던 안전장치). 쓰기 실패는 시작 실패 | 백업 쓰기가 막히면 broker가 시작하지 않음 |
| W3e-3 | **원격 경로 상한 5조각·255자**(GitLab 깊이 4 그룹 때문, 계획의 4에서 상향), 한 곳(`ValidRemotePath`)에서 broker·등록부·orchd·웹이 같은 상한 | — |
| W3e-4 | **서명되는 소유자 필드를 상위 집합으로 넓힘**(`ValidOwnerField`): 실제 GitHub 개인 계정에 대문자·`_`·숫자 시작이 있어서. 종류별 규칙은 broker가 **서명 전 모양 단계**에서 적용, GitHub 철자를 등록부·고아·쓰기 경로에 씀 | GitLab 대문자·`_` 경로는 연결 불가(받아들인 한계) |
| W3e-5 | **보호 `PUT`은 방금 만든 저장소에만**: `justCreated` + 클라이언트가 기억한 이름·ID. 이미 있는 저장소의 약한 보호는 읽기만 하고 고치지 않음 | 연결한 저장소의 보호는 이 도구가 보장하지 않음 |
| W3e-6 | **보호 `PUT` 본문**(문서에서 필수 4개 nullable + `enforce_admins:true`, 강제 푸시·삭제 금지)과 **응답도 같은 `strong()` 판정**. 실서버 확인은 컷오버뿐 | 가정이 틀리면 모든 GitHub 생성이 `protect`에서 끝남 |
| W3e-7 | **첫 파일은 `branch` 없이**(빈 저장소) 올리고 기본 브랜치를 읽어 `main`이 아니면 `create_partial`(`default_branch`). 나머지 파일은 `branch: main` | 빈 저장소에서 콘텐츠 API가 안 되면 모든 생성이 `first_commit`에서 끝남 |
| W3e-8 | **저장소 만들기 호출이 4xx로 거부되면 고아 기록을 남기지 않는다**(만들어진 것이 없음). 2xx이지만 요청과 다르거나(`private` 불일치), 5xx·3xx·전송 오류는 불확실 → 고아 | — |
| W3e-9 | **`link_project`·`attach_remote`는 프로젝트 id가 원격 저장소 이름과 같아야 한다**(`remote_name_mismatch`, 서명 전; GitHub는 대소문자 무시, id는 소문자): W3c의 복제·해석기 이름 규칙 때문에 그렇지 않으면 복제도 게시도 안 되는 프로젝트가 등록됨 | **이름을 소문자로 바꾼 것이 id 규칙에 맞으면 연결 가능**(`MyRepo` → `myrepo`). `_`·`.`가 있는 이름(`my_repo`, `Repo.js`)과 id 규칙 밖의 이름은 연결 불가(받아들인 한계). 풀려면 등록부에 id 필드가 필요 |
| W3e-10 | **link·attach 한도는 둘이 합쳐 시간당 10**(계획은 각각 10). GitHub 쓰기는 생성 한도(시간당 5)에 합산 | — |
| W3e-11 | **고아 기록에 `origin`(`created`/`linked`)을 더함**과 별도 코드 `link_partial`(문장: "이미 있던 저장소: 지우지 마세요, 배포 키만 제거"). 출처를 모르면 웹은 "출처 불명" | 옛 기록은 `created`로 읽힘 |
| W3e-12 | **orchd가 create propose에서 종류를 `visibility` 유무로 추정**(제안이 broker를 부르지 않는 W3c 규약 유지, 최종 권한은 broker의 모양 단계). **link·attach 제안·적용은 broker `projects` 읽기를 한 번 한다**: broker가 꺼져 있으면 `broker_unavailable` | link·attach는 broker 없이 제안되지 않음 |
| W3e-13 | **서명 대기 키는 메모리 인덱스**(디스크 이름은 무작위, 서명된 해시로만 찾음): **재시작하면 서명 대기 제안은 `key_expired`**. 알 수 없는 결과 뒤 전달된 키는 **보관**(`Retain`, 최대 16, 만료 없음)하고 다음 GitHub 제안 때 등록부를 읽어 승격·정리. **증명 가능한 거부(닫힌 집합)일 때만 키를 지움** | 보관 키가 등록부를 못 읽는 동안 쌓일 수 있음(상한 16), 운영자 복구 절차 있음(런북 4의 E) |
| W3e-14 | **GitHub 응답의 한도 헤더는 오류 응답의 분류에만 읽는다**(`Retry-After`, `X-RateLimit-Remaining`), 한도 계산에는 쓰지 않고 값을 저장·기록하지 않음 | — |
| W3e-15 | **link·attach의 기본 게시 권한은 읽기**(웹, 최소 권한). 생성은 쓰기가 첫 선택(W3c와 같음) | 사용자가 쓰기로 바꾸면 쓰기 키 |
| W3e-16 | **`attach_remote`는 정적 `--git-remote-for` 프로젝트(예 `sample-app`)에 `static_remote_wins`로 서명 전 거부**. 원격 매핑이 이미 있는 프로젝트(옛 매핑 포함)는 `already_linked`. `create_project`·`link_project`도 새 폴더가 정적 항목이면 거부 | `sample-app`는 정적 원격을 유지 |
| W3e-17 | **`RenameNoReplace`를 `internal/orch/fsx`로 이동**(clone과 keys가 둘 다 필요) | — |
| W3e-18 | **키 경로에 새 플래그 없음**: `--git-ssh-key`가 있는 폴더 아래 `keys/`·`keys-staging/`. 해석기는 키 파일을 **사용할 때마다** 검사 | `--git-remote`가 없는 서버에는 해석기가 없다 |
| W3e-19 | **GitHub 소유자 목록은 페이지 번호만**(5쪽, 링크 헤더를 따르지 않음), 건너뛴 로그인이나 쪽 한도면 `truncated` | 조직이 500개를 넘으면 일부만 보임 |
| W3e-20 | **GitHub API 버전 헤더 `2022-11-28`**(2028-03까지 지원). 더 새 버전은 상수 한 곳의 변경 | — |

## 6. 실서버·실제 GitHub에서 확인하지 못한 것

### 6.0 컷오버(2026-11-12)에서 실서버·실제 GitHub로 확인한 결과
컷오버는 사용자와 컨트롤러가 2026-11-12에 했고, 아래는 그때 서버에서 본 것이다(이 문서를 쓴 구현 단계에서 본 것이 아니다). 항목은 아래 6장의 원래 목록(컷오버 전 기록)과 같은 순서이며 **확인함**/**아직 못 함**으로 표시한다.
- **절차:** broker·orchd를 22:26 KST에 갱신했다(백업 `/root/backup-w3e`). **확인함.**
- **등록부 v2→v3 마이그레이션(실서버 파일·uid·샌드박스):** 성공, `registry.json.v2.bak`이 원본과 같음. **확인함.** 롤백(등록부 복원)의 실제 영향: **아직 못 함**(롤백하지 않았다). 샌드박스 안 `-check`와 폴더 fsync 실패 경로의 별도 기록: 이 기록에 없음, **아직 못 함**으로 둔다.
- **정책 `registered`:** 컷오버에서 추가했다. **확인함.**
- **GitHub 제공자 등록(실제 패스키 서명):** `swlee3306-github`(ssh 주소 `ssh://git@ssh.github.com:443`)를 실제 패스키로 등록했다. **확인함**(GitHub 종류 `set_provider`의 실제 서명 검증).
- **GitHub 호스트 키:** `[ssh.github.com]:443`의 호스트 키를 GitHub 공식 문서와 `api.github.com/meta`의 지문(`SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`)과 한 글자씩 맞춘 뒤 고정했다. **확인함.** `api.github.com:443`은 열려 있고 `github.com:22`는 닫혀 있다(알려진 제약, 선택 B로 해결). **확인함.**
- **토큰 범위:** 첫 `create_project`가 `github_refused`(토큰 범위 부족)로 끝났고 사용자가 토큰을 바꾼 뒤 성공했다. **확인함**(범위가 모자라면 안전하게 거부됨). 바꾼 토큰의 종류(클래식/세분화)와 권한 이름은 이 기록에 없다: 세분화 토큰의 권한 이름·개인 계정 저장소 생성 가능 여부는 **아직 못 함**.
- **실제 GitHub 저장소 생성:** `swlee3306/w3e-smoke-1112`(`basic` 템플릿)와 `w3e-smoke-1113`(`go` 템플릿)을 **공개**로 만들었고 `main`이 기본 브랜치이며 보호가 켜졌고 배포 키가 등록됐고 복제가 각각 커밋 3개·6개로 끝났다. 저장소별 키 폴더는 700, 키 파일은 600. **확인함**: 빈 저장소에 `branch` 없는 첫 파일 → 기본 브랜치 `main`, 보호 `PUT`의 본문·응답, 배포 키 `POST` 성공, `POST /user/repos` 응답 모양, 키 폴더 만들기·`RENAME_NOREPLACE` 승격(실제 유닛 샌드박스), OpenSSH 9.6이 orchd의 개인 키를 받아들임, GitHub가 공개 키(주석 포함)를 받아들임, 저장소별 키로 `ssh.github.com:443`을 통한 복제. **아직 못 함:** 무료 요금제 비공개 저장소의 보호 거부(403 가정: 두 저장소 모두 공개), 같은 공개 키 중복 등록의 거부 응답, 한도 응답 헤더, 이름이 바뀐 저장소의 301, GitHub Enterprise `/api/v3`.
- **게시:** 작업 #47(`basic` 템플릿)은 판정 `verification_incomplete`(`no_required_checks`)였다. `basic` 템플릿은 검사가 비어 있고 게이트는 빈 검사를 통과로 세지 않으므로 **예상한 동작**이다. 작업 #48(`go` 템플릿)은 통과했고 `orch/48` 브랜치를 `ssh.github.com:443`으로 GitHub에 올렸다. **확인함**(해석기가 만든 원격과 그 프로젝트의 키로 게시).
- **기존 저장소 연결(`link_project`)·원격 연결(`attach_remote`)의 실제 실행:** **아직 못 함.** 연결하려던 `sulee_portfolio`는 프로젝트 id가 저장소 이름과 같아야 하는 W3e 규칙(`_`) 때문에 연결할 수 없었고 W3f F1b에서 규칙이 풀렸다. 실제 패스키 서명으로 `link_project`·`attach_remote`를 검증하는 것도 **아직 못 함.** GitLab 쪽 이월(공용 배포 키가 두 번째 프로젝트에 합류하는지, GitLab 기본 브랜치 보호 설정)도 **아직 못 함.**
- **웹 화면(키보드만·스크린 리더, 실제 Chrome의 포커스·130초 대기, 390px의 보지 못한 화면):** 이 기록에 없음, **아직 못 함.**
- **broker·orchd 버전 차이 구간:** 런북 순서(orchd를 먼저 멈춤)로 구간을 만들지 않았다. 구간의 동작은 **아직 못 함**(관찰하지 않음).
- **알려진 제약:** `api.github.com:443` 열림·`github.com:22` 닫힘(위), 이 도구는 이 시점에 GitHub의 저장소·배포 키를 지우지 못했다(스모크 저장소 둘은 사용자가 GitHub에서 지워야 한다: W3f에서 `origin: unknown`이라 원격 삭제 불가, 로컬 정리만 가능).

### 6.1 컷오버 전 기록(원문)
**아래 목록은 컷오버 전(구현 시점)의 기록이다. 컷오버 결과는 위 6.0.** 시험은 가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더로 요청 모양과 흐름만 확인했다.
- **실제 GitHub에서:** ① 빈 저장소에 콘텐츠 API로 첫 파일이 **`branch` 없이** 올라가 `main`이 기본 브랜치가 되는지(문서는 빈 저장소에 대해 침묵), 계정의 기본 브랜치 이름이 다르면 `default_branch` 단계로 끝남. ② 보호 `PUT`의 **본문 수용과 응답**, 보호가 없는 브랜치의 `GET`이 404인지, **무료 요금제 비공개 저장소의 보호가 403으로 거부되는지**(가정), `GET`이 `allow_deletions.enabled`를 주는지. ③ **배포 키 `POST`**의 성공 응답(201 가정), **같은 공개 키 중복 시 응답**(422 가정, 어떤 4xx든 거부로 매핑), `read_only` 응답 echo. ④ **토큰 범위:** 클래식 `repo`·`public_repo`가 생성·콘텐츠·보호·배포 키를 덮는지, **세분화 토큰의 권한 이름과 개인 계정에 새 저장소를 만들 수 있는지**(문서 페이지가 명시하지 않음). ⑤ 한도 응답(403 + `x-ratelimit-remaining: 0`, 헤더 없는 이차 한도는 `github_refused`로 분류됨). ⑥ `POST /user/repos` 응답의 `private`·`full_name` 모양, 이름이 바뀐 저장소의 301(안 따름). ⑦ GitHub Enterprise Server의 `/api/v3` 접두사(관례, 문서에서 확인 못함).
- **orchd가 실제 유닛에서 `/home/orch/orchd/ssh/keys`·`keys-staging`을 만들 수 있는지**(`ReadWritePaths=/home/orch/orchd`, 소유자 `orch`, 0700이 `umask`를 넘는지, `ProtectHome=read-only`와의 상호작용), 키 승격의 `RENAME_NOREPLACE`(리눅스에서는 컴파일만 확인).
- **OpenSSH 9.6이 `ssh.MarshalPrivateKey`가 쓴 개인 키 파일을 받아들이는지**(시험은 같은 Go 라이브러리로 다시 파싱했을 뿐이다), **GitHub가 공개 키와 주석(`orchd-deploy`)을 받아들이는지**, `IdentitiesOnly`와 첫 실제 GitHub `git clone`·`push`.
- **`github.com`(또는 `ssh.github.com:443`)의 `known_hosts` 항목과 ssh 도달:** 22는 닫혀 있다고 알려져 있고 방법(규칙 / 443 ssh)은 사용자 결정이다. **`api.github.com:443`은 저장소의 nft 파일(36행)이 공인 주소 443을 허용하므로 이미 열려 있을 가능성이 높으나 실서버의 규칙으로 확인하지 못했다.** `ssh.github.com:443` 형태의 ssh 주소는 코드가 받고 시험했으나 GitHub에서 실제로 되는지는 미확인.
- **등록부 v2→v3 마이그레이션을 실서버 파일(제공자 `example-group-gitlab`)·uid·샌드박스로** 하는 것, `registry.json.v2.bak` 쓰기(폴더 fsync 포함), 롤백(등록부 복원)의 실제 영향, 샌드박스 안 `-check`.
- **broker·orchd 버전 차이 구간의 실제 동작**(런북은 그 구간을 없애는 순서를 쓰지만, 옛 orchd가 새 broker 응답에서 실제로 기본 원격을 쓰는지는 W3c 코드 읽기(`56dfe3e`의 해석기가 `GitLab == nil`이면 건너뜀)로 알 뿐 서버에서 보지 않았다).
- **GitLab 쪽 W3c 미확인의 이월:** **공용 배포 키가 두 번째 GitLab 프로젝트에 합류하는지**(허용 목록에 "키를 프로젝트에 켜기"가 없다), **GitLab 기본 브랜치 보호 설정**(더 약하면 모든 생성이 `protect`), GitLab link의 `GET projects/:path`와 `path_with_namespace` 대소문자·리다이렉트 동작, 깊이 4 그룹.
- **실제 패스키 서명**을 broker가 `set_provider`(GitHub)·`create_project`(GitHub)·`link_project`·`attach_remote`에 대해 검증하는지.
- **키보드만·스크린 리더**로의 새 화면 사용, 실제 Chrome에서 선택·공개 여부 칸이 다시 그려질 때 포커스 유지, 130초 대기와 앞단 제한시간. E5의 ego-browser 확인은 1280px과 390px의 일부 화면이고 **서명은 대역 패스키**였다(보지 못한 화면: 390px GitHub 생성 폼 이미지, 390px 복제 상태 표, `unknown outcome`·`key_expired` 화면, 제공자 수정·삭제 확인).
- **리눅스에서의 파일·프로세스 동작**은 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- W2b·W3a·W3b·W3c에서 이월된 미확인(실제 MR, 한도·예산 초과 실동작, 실제 OpenAI 키 교체, 브라우저의 비밀번호 관리자 반응, 실제 GitLab 19.0.8 동작 등)은 각 결과 문서 그대로다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **이 도구는 GitHub의 저장소와 저장소에 붙인 배포 키를 지우지 못하고 등록부에서 프로젝트를 지우는 op도 없다.** 시험용 리소스는 사람이 GitHub에서 지우고 등록부 항목·폴더·`ssh/keys/<id>/`는 남는다.
2. **프로젝트 id는 원격 저장소 이름과 같아야 한다**(GitHub는 대소문자 무시, id는 소문자). **GitHub 저장소는 이름을 소문자로 바꾼 것이 id 규칙에 맞으면 연결할 수 있고**(`MyRepo` → `myrepo`), 이름에 `_`·`.`가 있거나(`my_repo`, `Repo.js`) id 규칙 밖이면 연결할 수 없다. **GitLab에서 경로에 `_`·대문자·`.`가 있는 프로젝트는 연결할 수 없다.** **새 저장소는 소문자 이름만 만든다.**
3. **무료 요금제의 비공개 GitHub 저장소는 항상 `create_partial`(`protect`)로 끝난다**(보호 브랜치가 유료). 저장소는 파일만 있는 채로 남는다. 공개 저장소는 누구나 읽는다.
4. **연결한 기존 저장소의 보호·설정은 이 도구가 보장하지 않는다**(배포 키만 붙인다). 쓰기 권한을 준 키로는 보호가 없는 브랜치에 푸시할 수 있다(기본은 읽기).
5. **GitHub 토큰은 사용자 권한 전체를 가진 토큰일 수 있다**(클래식 `repo`): 코드는 9개 호출만 보내지만 유출되면 GitHub에서 그 이상이 가능하다. 서명된 변경과 만료일·폐기가 방어다.
6. **orchd 재시작은 서명 대기 중인 제안의 키를 잊는다**(`key_expired`, 다시 제안). 보관 중이던 키(알 수 없는 결과 뒤)도 재시작이 잊는다: 운영자 복구 절차(런북 4의 E)가 있다. 승격 실패(드묾)는 등록된 프로젝트가 키 없이 남을 수 있다(`key_unavailable`).
7. **GitHub 호스트 키는 사용자가 공식 지문과 대조해 직접 넣어야 한다**(자동 신뢰 없음).
8. **정적 `--git-remote-for` 프로젝트에는 원격을 붙일 수 없다**(`static_remote_wins`).
9. **GitHub 조직 생성은 없다**(API 없음): 소유자는 내 계정 또는 이미 있는 조직. 소유자 목록은 5쪽까지(`truncated`).
10. **`--git-remote`가 없는 orchd에는 게시 해석기가 없다**(GitHub 프로젝트 게시도 없다).
11. **"알 수 없음 = 거부"**(W3c-4)는 GitHub 프로젝트에도 그대로다.
12. W3c의 7장(받아들인 한계)이 그대로 적용된다.

## 8. 사용자 결정 목록

컷오버 때 사용자가 정하거나 해야 하는 것(런북 15.0, 3단계):
1. **GitHub 토큰:** 만들지 여부, **종류(세분화 / 클래식 `public_repo` / 클래식 `repo`)와 만료일**. 세분화 토큰이 이 도구의 호출(개인 계정에 저장소 만들기 포함)을 덮는지는 확인하지 못했다: 공식 문서를 사용자가 직접 읽고 정한다. 값은 **웹 토큰 화면에만** 입력하고 채팅·터미널에 붙이지 않는다.
2. **소유자:** 개인 계정(2026-11-12 결정). 조직은 쓰지 않는다.
3. **공개/비공개와 요금제:** 공개는 누구나 읽는다, **무료 요금제에서는 비공개가 `protect`에서 막힌다**. 저장소마다 서명 화면에서 직접 고른다(기본값 없음).
4. **ssh 경로(사용자 결정, 이 도구와 컨트롤러는 nft를 바꾸지 않는다):** `github.com:22`를 여는 규칙(GitHub `meta`의 `git` IP 대역 등)을 정할지, 규칙 없이 `ssh://git@ssh.github.com:443`을 쓸지. `api.github.com:443`은 이미 열려 있을 가능성이 높다(런북 15.0 (e)에서 확인).
5. **`github.com` 호스트 키를 GitHub 공식 SSH 지문과 사용자가 대조해 `known_hosts_gitlab`에 넣기**(`ssh-keyscan` 출력은 그것만으로 믿지 않는다. 절차: 런북 15.0 사용자 사전 작업 5).
6. **정책 `registered` 클래스:** W3c에서 이미 필요하고 넣었다(14장). 없으면 제안이 `policy_class_missing`.
7. **실제 저장소를 만들지 여부와 이름, 기존 저장소를 연결할지·어느 저장소인지:** **GitHub 저장소와 배포 키는 이 도구가 지울 수 없다**(사용자가 GitHub에서 직접 지운다, 등록부·폴더·`ssh/keys/<id>/`는 남는다). 시험용 이름으로.
8. **한계를 받아들일지:** 프로젝트 id = 원격 저장소 이름(GitHub는 대문자만 다른 이름은 가능, `_`·`.`는 불가), GitLab에서 `_`·대문자·`.` 경로는 연결 불가, 새 저장소는 소문자 이름만, `sample-app` 같은 정적 원격 프로젝트에는 연결 불가.
9. **게시 권한(읽기/쓰기)과 작은 시험 작업으로 게시를 확인할지**(3-9).
10. **컷오버가 안정되면 `/root/backup-w3e`를 언제 지울지**(토큰·키 사본이 들어 있어 `shred`), GitHub 토큰의 권한을 시험 뒤 줄일지·폐기할지, 롤백 뒤 `ssh/keys/`의 개인 키를 지울지(GitHub의 배포 키를 먼저 제거한 뒤).
11. **W3d(PR·MR, 조건부 자동 병합)로 갈지.** 그때 빈 `checks`를 통과로 세지 않는 규칙을 넣는다(W3c 이월).
12. **앞단 프록시의 제한시간**이 130초보다 짧다면 올릴지.

## 9. 컷오버 기록
2026-11-12 22:26 KST에 broker·orchd를 갱신했다(백업 `/root/backup-w3e`). 서버에서 본 것만 적는다(자세한 항목별 판정은 6.0).
- 등록부 v2 → v3 마이그레이션 성공, `registry.json.v2.bak`은 원본과 같다. 정책 `registered` 추가.
- 실제 패스키로 제공자 `swlee3306-github`(`ssh://git@ssh.github.com:443`) 등록. `[ssh.github.com]:443` 호스트 키는 GitHub 공식 문서·`api.github.com/meta`의 지문(`SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`)과 일치한 것을 확인한 뒤 고정.
- 첫 `create_project`는 `github_refused`(토큰 범위)로 실패, 사용자가 토큰을 바꾼 뒤 성공. `swlee3306/w3e-smoke-1112`(`basic`)·`w3e-smoke-1113`(`go`)가 공개로 만들어졌고 `main` 보호, 배포 키 등록, 복제 커밋 3개·6개, 저장소별 키 폴더 700/600.
- 작업 #47(`basic`): `verification_incomplete`·`no_required_checks`(빈 검사는 게이트가 거부: 예상). 작업 #48(`go`): 통과, `orch/48`을 `ssh.github.com:443`로 게시.
- 알려진 제약: `api.github.com:443` 열림, `github.com:22` 닫힘. `sulee_portfolio` 연결은 F1b 전에는 불가능했다.
- 하지 않은 것: 롤백, `link_project`·`attach_remote` 실제 실행, 비공개 저장소 생성, GitLab 두 번째 프로젝트.

