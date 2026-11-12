# W3e 구현 계획: GitHub 제공자(GitLab과 같은 기능) + 기존 저장소 연결

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다. **상태: 초안 v0.1(2026-11-12), 사용자 승인 전. 승인 전에는 구현하지 않는다.**

**목표:** 사용자의 요청("GitLab과 완전 동일하게": 새 저장소 만들기, 기존 저장소 연결, 코드를 올리는 것(게시))에 따라, W3c의 제공자 구조에 **GitHub 제공자**를 더한다. 같은 서명 승인·허용 목록·고아 처리·복제·게시 흐름이 GitHub에서도 동작하고, GitLab·GitHub **둘 다**에 대해 **기존 저장소 연결**(원격에 이미 있는 저장소를 이 서버에 가져와 등록, 이미 등록된 로컬 프로젝트에 원격 붙이기)을 추가한다. MR/PR 생성·조건부 자동 병합은 이 계획의 범위가 아니다(W3d: 그 때 두 제공자를 함께 다룬다).

**구조:** broker의 GitLab 클라이언트를 **제공자 인터페이스** 뒤로 옮기고(동작 불변, W3c 시험 그대로 통과) GitHub 구현을 하나 더한다. 각 구현은 자기 (메서드, 경로 모양) **허용 목록**만 보낸다. 서명 메시지·등록부(버전 3)·고아 기록·한도는 W3c 방식을 그대로 확장한다. GitHub 배포 키는 **저장소마다 달라야 하므로**(같은 키를 다른 저장소에 못 붙임) orchd가 **저장소별 키를 만들고**(`orch` 계정), 복제·게시가 그 키를 쓴다.

**기술:** Go(broker, orchd, registrymsg, signing, publish), 바닐라 JS(웹), 가짜 GitHub 서버(httptest)·가짜 broker·임시 git 저장소로 시험.

**설계서:** `docs/19`의 4.6(GitHub 쪽 조사), 앞 단계: `docs/24`(W3c 계획), `docs/25`(W3c 결과, 컷오버 진행 중).

## 전역 제약
- **`docs/24`의 "전역 제약" 전부를 그대로 적용한다**(비밀 비유출, 배포 현실 표, 허용 목록, 주소 검증, 서명 검증 순서, orchd의 broker import 금지, 웹 규칙, 검증 명령, 커밋 꼬리말). 아래는 GitHub에서 달라지는 것만 쓴다.
- **GitHub 호출 허용 목록(정확히 이것만, 가짜 서버가 모든 요청을 가로채 확인):** `GET /user`, `GET /user/orgs`(소유자 목록), `GET /repos/{owner}/{repo}`(존재·기본 브랜치 확인), `POST /user/repos`, `POST /orgs/{org}/repos`, `PUT /repos/{owner}/{repo}/contents/{path}`(첫 커밋: 파일마다 한 번), `GET /repos/{owner}/{repo}/branches/{branch}/protection`, `PUT /repos/{owner}/{repo}/branches/{branch}/protection`(**이번 흐름이 방금 만든 저장소의 `main`에만**), `POST /repos/{owner}/{repo}/keys`(배포 키). 헤더 `Authorization: Bearer <토큰>`, `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`(공식 문서로 구현 때 재확인). **삭제(`DELETE`)·저장소 설정 변경(`PATCH`)·병합·토큰 관련 호출은 코드에 없다.** 오류 본문은 읽지도 반영하지도 않는다(상태 코드 범주만; 한도 초과 403/429는 `provider_rate_limited` 고정 코드).
- **GitHub 제공자 주소:** `base_url`은 API 주소(기본 `https://api.github.com`)이고 **`https`만**(사설·tailnet IP 리터럴의 `http`도 GitHub Enterprise Server용으로만 허용하는 W3c 규칙 유지), `ssh_url`은 `ssh://git@github.com`(또는 Enterprise 호스트). 기존 주소 검증(루프백·링크 로컬·메타데이터·리다이렉트 거부·연결 시점 IP 재확인)을 그대로 쓴다. **api.github.com에는 공개 IP가 붙으므로 연결 시점 검사는 공개 주소를 허용**해야 한다(W3c는 이미 허용).
- **정직한 한계(GitHub API가 GitLab과 다른 부분, 설계에 이미 반영):** (1) github.com에는 **조직 생성 API가 없다**: GitHub에서는 `create_group`이 `not_supported_for_provider`이고 "그룹" 자리는 **기존 조직 또는 내 계정**을 고른다. (2) **배포 키는 저장소마다 달라야** 한다. (3) **브랜치 보호는 공개 저장소만 무료 요금제에서 가능**하고 비공개는 유료 요금제가 필요하다(없으면 `create_partial`(단계 `protect`)). (4) **빈 저장소에 콘텐츠 API로 첫 파일이 되는지, 토큰 범위(세분화·클래식)가 새 저장소 생성·배포 키 등록을 덮는지는 문서로 확인하지 못했다**: 구현자는 공식 문서를 읽어 근거를 보고서에 쓰고, 실제 동작은 **컷오버에서만** 확인된다.
- **공개/비공개는 서명되는 필드 `visibility`(`public`|`private`)이며 기본값이 없다**(사용자가 반드시 고른다). GitLab은 계속 `private` 고정이고 이 필드를 받지 않는다. 확인 화면에 굵게 표시한다(공개 저장소는 누구나 읽는다). `ai_context`는 공개 여부와 무관하게 별도 명시 설정이다.
- **저장소별 배포 키(orchd):** 키 쌍은 orchd가 `ssh-keygen`이 아니라 **Go 표준 라이브러리(ed25519)** 로 만들고(외부 프로그램 없음), 개인 키는 `/home/orch/orchd/ssh/keys/<이름>/id_ed25519`(`orch:orch 0600`, 폴더 0700), 공개 키는 같은 이름 `.pub`. **제안 단계는 등록부와 GitHub에 아무것도 쓰지 않으므로** 키는 제안에서 `ssh/keys-staging/<무작위>/`에 만들고(서명하는 메시지에 공개 키 해시), 적용이 성공 경로에 들어가면 `keys/<이름>/`으로 옮기며(원자적 rename), **적용되지 않은 임시 키는 15분 뒤와 orchd 시작 때 지운다**(자기가 만든 것만). GitLab 프로젝트는 계속 공용 키(W3c)를 쓴다.
- **정책·등급:** 새 프로젝트의 `kind`는 `registered`(W3c와 같음, `policy.yml`의 클래스 필요).
- **네트워크는 사용자 결정이다(이 계획은 방화벽을 바꾸지 않는다).** 서버의 nft는 현재 GitLab(`100.64.0.10`의 22·18082)만 허용한다. `api.github.com`·`github.com:22`는 IP가 바뀌므로 **허용 규칙을 어떻게 둘지(GitHub `meta` IP 대역, 중계 프록시 등)는 컷오버 전에 사용자가 정한다**. 규칙이 없으면 broker·orchd의 GitHub 호출은 `provider_unreachable`·복제 실패로 끝난다(안전하게 실패).

## 정해 둔 결정
- **E-D1 등록부 버전 3:** `providers[].kind`에 `github` 추가, 프로젝트의 `provider`·원격 필드는 `gitlab:{project,project_id}`를 `remote:{path,id}`로 일반화(읽을 때 W3c의 `gitlab` 필드를 `remote`로 이전). **W3c broker는 버전 3 파일을 깨짐으로 보고 fail closed**: 롤백은 바이너리와 함께 `registry.json`을 W3c 백업으로 복원(런북에 경고). 마이그레이션 2→3은 원자적이고 서명 불필요(내용 불변).
- **E-D2 서명 action:** 기존 `set_provider`에 `kind`가 `github`인 경우의 필드 규칙을 더한다. `create_project` fields에 `visibility`(GitHub일 때만 필수, GitLab이면 금지), `owner`는 `group_path` 필드를 그대로 쓴다(의미: GitHub 소유자). 새 action: `link_project`(subject=새 로컬 프로젝트 id; fields `provider`,`remote_path`(`그룹/이름` 또는 `소유자/저장소`),`ai_context`,`publish`,`deploy_key_sha256`: 원격에 **이미 있는 저장소**를 가져와 등록하고 배포 키를 붙인다. 저장소를 만들거나 지우거나 설정을 바꾸지 않는다), `attach_remote`(subject=이미 등록된 프로젝트 id; fields `provider`,`remote_path`,`publish`,`deploy_key_sha256`: 등록된 로컬 프로젝트에 원격을 연결해 게시 해석기가 그 원격을 쓰게 한다. **정적 `--git-remote-for` 항목이 있는 프로젝트(예: sample-app)는 정적이 우선이라 효과가 없으므로 거부 `static_remote_wins`**).
- **E-D3 추가 오류 코드(고정 문장):** `not_supported_for_provider`, `provider_rate_limited`, `visibility_required`, `visibility_not_allowed`, `repo_not_found`(link/attach 대상이 원격에 없음), `already_linked`, `static_remote_wins`, `key_staging_failed`, `key_expired`(제안 뒤 15분이 지나 임시 키가 지워짐 → 다시 제안).
- **E-D4 GitHub `create_project` 순서(GitLab의 D4와 같은 모양, 단계 이름도 같음):** (1) 서명·모양·키 해시 검증, (2) 소유자 확인(`GET /user`·`GET /user/orgs`에서 서명된 소유자가 있는지), (3) `GET /repos/{owner}/{repo}`가 404일 때만 진행(있으면 `project_name_taken`, 인수 안 함), (4) `POST /user/repos` 또는 `POST /orgs/{org}/repos`(`name`, `private`=`visibility=="private"`, `auto_init:false`, `has_issues`·`has_wiki` 기본), (5) 첫 파일들을 `PUT …/contents/{path}`로 하나씩(`README.md` 먼저, `branch:main`) 후 `GET /repos`로 기본 브랜치가 `main`인지 확인(아니면 `create_partial`(`default_branch`)), (6) 보호: `GET …/protection`이 404면 `PUT …/protection`(PR 필수 없음·강제 push 금지·삭제 금지·관리자 포함 설정은 구현자가 공식 문서로 정하고 근거 기록), 이미 있으면 읽어 약하면 거부, (7) `POST …/keys`(`title: "orchd <이름>"`, `read_only: publish != "write"`), (8) **마지막에** 등록부 등록. 4~7 실패는 W3c와 같이 지우지 않고 고아 기록 + `create_partial`.
- **E-D5 `link_project`·`attach_remote` 순서:** (1) 서명 검증, (2) `GET`으로 원격 저장소가 있는지(없으면 `repo_not_found`), 등록부에 같은 id·같은 원격이 있으면 `already_linked`, (3) 배포 키 등록(`POST …/keys`): 저장소에 이미 같은 키가 있으면 성공으로 보지 않고 `gitlab_refused`/`github` 거부 코드(GitLab은 공용 키 합류 규칙이 W3c와 같음), (4) 등록부 기록. `link_project`는 마지막에 orchd가 복제(기존 `POST /projects/{id}/clone`)한다. 어느 단계 실패든 지우지 않고 고아 기록 + `create_partial`(키만 붙고 등록 실패 등).
- **E-D6 한도:** GitHub 쓰기 호출도 W3c의 `create_project`·`create_group` 한도 계열에 합산하고(주체별 시간당 5), `link_project`·`attach_remote` 시간당 10. GitHub 응답의 한도 헤더는 읽지 않는다(고정 한도만).
- **E-D7 웹:** 제공자 추가 화면에 종류 선택(`gitlab`|`github`; GitHub는 주소 기본값 `https://api.github.com`, ssh `ssh://git@github.com`), 새 프로젝트에서 **소유자 선택**(GitHub는 "새 그룹 만들기" 숨김), **공개/비공개 선택(기본 없음, 굵은 확인 문구)**, 확인 화면에 저장소별 키 지문, "기존 저장소 연결"(원격 경로 입력 → 확인 → 서명)과 프로젝트 행의 "원격 연결". 서버 문장은 모두 텍스트 노드, 새 코드는 고정 문구.

## 검토 초점 (개별 작업 시험이 놓치기 쉬운 것, 가능성 순)
1. **GitLab 회귀 없음:** 제공자 인터페이스로 옮긴 뒤 W3c의 모든 시험·적대 시험이 **의미 변경 없이** 통과하고 GitLab 허용 목록이 그대로인가(실서버에서 이미 도는 경로다).
2. **저장소별 키의 수명과 권한:** 임시 키의 지연 삭제가 남의 폴더를 지우지 않는가, 키가 GitHub·로그·원장·오류에 새지 않는가, 개인 키 모드, orchd 재시작 중 임시 키, 제안만 하고 적용 안 한 경우 청소, 같은 이름으로 두 번 제안.
3. **공개 저장소가 잘못 만들어질 위험:** `visibility` 기본값이 없는지, 서명·확인 화면·등록부에 일관되게 들어가는지, 확인 없이 공개되는 경로가 없는지.
4. **GitHub 허용 목록과 SSRF:** 소유자·저장소 이름이 경로에 한 번만 인코딩되어 다른 경로로 새지 않는가(`../`·`%2f`·대소문자: GitHub 이름은 대소문자를 구분하지 않으므로 중복 판정에 주의), 리다이렉트, 토큰이 다른 호스트로 새는지, `PUT protection`이 이번에 만든 저장소에만 가는지.
5. **`attach_remote`가 기존 프로젝트를 건드리는 범위:** 정적 원격이 있는 프로젝트(sample-app) 거부, 등록부 쓰기만 하고 원격 저장소 설정은 안 바꾸는지, AI 문맥 설정이 바뀌지 않는지.
6. **배포 현실:** 새 키 폴더 `ssh/keys/`가 orchd의 `ReadWritePaths`(`/home/orch/orchd`) 안인지, `known_hosts`에 `github.com` 고정 항목(사용자가 GitHub 공식 지문과 대조해 넣음)이 있어야 복제가 되는지, 방화벽(사용자 결정), 등록부 v3와 롤백.

---

### Task 1 (E1): 등록부 버전 3, 새 action·필드·오류 코드
**파일:** 수정 `internal/registrymsg/*`, `internal/approval/signing/registry_message.go`, `internal/broker/registry.go`, 각 `_test.go`.
**생산:** 제공자 `kind` 확장, `remote:{path,id}` 일반화와 2→3 마이그레이션, 필드 `visibility`, action `link_project`·`attach_remote`, E-D3 코드 목록, 웹과의 상수 드리프트 시험 확장.
- [ ] **Step 1: 실패하는 시험(이름 그대로)** — `TestGitHubProviderShape`, `TestVisibilityFieldRules`(GitHub 필수·GitLab 금지·값 enum), `TestLinkAndAttachActionsInSchema`, `TestRemotePathRules`(`소유자/저장소`·`그룹/하위/이름` 경계, `.`·`..`·`%2f`·유니코드·`.git` 접미사 거부), `TestRegistryV2MigratesToV3Atomically`(내용 불변, 모드 0600, 실패 시 v2 그대로), `TestOldBrokerRefusesV3`, `TestExistingActionsUnchanged`, `TestSigningDefinitionsDoNotDriftW3e`.
- [ ] **Step 2: 실패 확인 → Step 3: 구현(W3c 방식으로 표에 항목 추가) → Step 4: 통과 확인(`-race`, 기존 시험 불변) → Step 5: 커밋** — `feat(registry): github provider kind, remote links and visibility (registry v3)`.

### Task 2 (E2): 제공자 인터페이스와 GitHub 클라이언트
**파일:** 만들기 `internal/broker/provider_iface.go`, `internal/broker/githubapi.go`(+시험); 수정 `internal/broker/gitlabapi.go`(인터페이스 구현으로, **동작 불변**), `provider.go`.
**생산:** 인터페이스(소유자 목록, 소유자 조회, 저장소 존재, 저장소 만들기, 파일 올리기, 보호 확인·설정, 배포 키 등록, 기본 브랜치 읽기), GitHub 구현(`do` 하나·허용 표·`Authorization` 헤더만에 토큰·리다이렉트 거부·연결 시점 IP 재확인).
- [ ] **Step 1: 실패하는 시험** — `TestGitLabBehaviourUnchangedViaInterface`(W3c 시험 그대로 통과 확인), `TestGitHubOnlyAllowlistedRequests`(가짜 서버가 모든 요청 기록, 삭제·PATCH·병합 없음), `TestGitHubDoRefusesOffList`, `TestGitHubPathEncodingNoInjection`, `TestGitHubTokenOnlyInAuthHeader`, `TestGitHubNoRedirect`, `TestGitHubTextNeverReflected`, `TestGitHubStatusMapping`(401/403/404/409/422/429/5xx → 고정 코드, 한도 초과 → `provider_rate_limited`), `TestGitHubCaseInsensitiveNameCollision`, `TestProtectionPutOnlyForJustCreatedRepo`, `TestGitHubProviderURLRules`, `TestSeamsNotReachableFromProduction`(W3c 정적 시험 확장).
- [ ] **Step 3: 구현** — GitHub REST 사양은 **구현 시점에 공식 문서로 확인하고 출처를 보고서에 쓴다**(특히 콘텐츠 API 빈 저장소 동작, 브랜치 보호 필드, 배포 키 응답 코드). **Step 4–5:** `-race`, 커밋 `feat(broker): provider interface and allowlisted GitHub client`.

### Task 3 (E3): broker op 일반화, `link_project`, `attach_remote`
**파일:** 수정 `internal/broker/providers_ops.go`, `create_project.go`; 만들기 `link_project.go`(+시험).
- [ ] **Step 1: 실패하는 시험** — `TestCreateProjectGitHubHappyPath`(요청 순서·본문이 E-D4와 일치, 마지막에 등록), `TestCreateProjectGitHubPartialFailures`(4·5·6·7 실패 주입 → 고아 기록, 삭제 호출 없음), `TestCreateGroupNotSupportedForGitHub`, `TestListOwnersGitHub`(`truncated` 의미 유지), `TestVisibilityMustBeSigned`, `TestPrivateProtectionNeedsPaidPlanBecomesPartial`, `TestLinkProjectExistingOnly`(없으면 `repo_not_found`, 쓰기 없음), `TestLinkProjectNeverCreatesOrDeletes`, `TestAttachRemoteStaticWins`(정적 원격이 있는 프로젝트 거부는 orchd가 알려 주므로 broker는 등록부 규칙만), `TestAlreadyLinked`, `TestLinkKeyAlreadyInUseNotSuccess`, W3c 서명 시험 표를 두 새 action에 반복(`TestLinkAttachRequireSignature`, `TestChangeMismatchBurnsNoID`, `TestCrossActionSignaturesRefusedW3e`), `TestCanaryTokenNeverInAnyOutputW3e`.
- [ ] **Step 5: 커밋** — `feat(broker): github create_project, link_project and attach_remote`.

### Task 4 (E4): orchd — 저장소별 키, 엔드포인트, 복제·게시 연결
**파일:** 만들기 `internal/orch/keys/keys.go`(+시험), `internal/orch/api/link_project.go`; 수정 `internal/orch/api/{providers,create_project}.go`, `internal/orch/clone/*`, `internal/orch/publish/publish.go`(해석기가 **키 경로도** 돌려줌), `cmd/orchd/*`.
- [ ] **Step 1: 실패하는 시험** — 키: `TestKeyGenerationEd25519NoExternalProgram`, `TestStagingKeyModesAndOwner`, `TestStagingKeyExpiresAndSweepsOnlyOwn`, `TestPromoteIsAtomicRename`, `TestPrivateKeyNeverInAnyOutput`, `TestProposeCreatesNoGitHubSideEffect`, `TestKeyExpiredAtApply`; 엔드포인트: `TestGitHubProviderEndpoints`, `TestCreateProposeIncludesPerRepoKeyHash`(GitLab은 공용 키 해시 그대로), `TestLinkAttachProposeApply`(`static_remote_wins` 포함), `TestApplyMapsNewCodes`, `TestLimitsW3e`, `TestLedgerNoSecretsW3e`; 복제·게시: `TestCloneUsesPerProjectKeyForGitHub`(명령줄의 `-i`가 그 프로젝트 키), `TestCloneGitHubHostMustBePinned`, `TestResolverReturnsKeyPathFromRegistryOnly`, `TestResolverGitLabUnchanged`, `TestStaticRemotesUnchangedW3e`, `TestPublishUsesPerProjectKey`.
- [ ] **Step 5: 커밋** — `feat(api): per-repo deploy keys, github and link endpoints, key-aware clone and publish`.

### Task 5 (E5): 웹
**파일:** 수정 `providers.js`, `projects.js`, `style.css`, 웹 시험·픽스처(Go에서 생성), `web_test.go`의 상수 비교.
- [ ] **Step 1: 실패하는 시험** — `TestProviderKindGitHubFormDefaults`, `TestOwnerSelectNoGroupCreationForGitHub`, `TestVisibilityRequiredNoDefault`(선택 전에는 제안 단추 비활성), `TestPublicConfirmationBold`, `TestCreateConfirmationShowsPerRepoKeyFingerprint`, `TestSignOnlyWhenChangeMatchesW3e`(새 필드까지), `TestLinkProjectFlow`, `TestAttachRemoteFlow`(정적 원격 거부 문구), `TestAllNewErrorCodesFixedTextsW3e`, `TestOutcomeUnknownNoAutoRetryW3e`, `TestChangeVectorsMatchGoW3e`, `TestNoPasswordLikeMarkupW3e`. **시각 확인(`ego-browser skill` 먼저, 1280·390px, 실제 `app.js` 경로, 패스키는 대역, 본 것만 기록)**, 변이: 비교 제거·visibility 기본값 주입·서버 문장 속성 삽입.
- [ ] **커밋** — `feat(web): github provider, owner select, visibility choice and link flows`.

### Task 6 (E6): 적대 시험, 결과 문서(`docs/27`), 컷오버 런북(`docs/10` 15장)
- [ ] 적대 시험: GitHub 허용 목록 전수, 대소문자·유니코드 이름 충돌, 공개 저장소 우발 생성 불가, 키 누수 불가, `attach_remote` 범위, 크로스 action 서명 거부, 임시 키 청소, W3c 적대 시험 전부 재통과. 변이 표(서명 검증·change 일치·허용 목록·visibility 필수 제거·키 해시 비교 제거·정적 원격 우선 제거·`PUT protection` 대상 제한 제거 등).
- [ ] 런북 15장(W3c 14장 양식): 사전 점검(등록부 v2, `ssh/keys/` 쓰기, GitHub 도달성은 **사용자가 정한 방화벽 규칙** 확인, `known_hosts`의 `github.com` 항목을 **GitHub 공식 SSH 지문과 사용자가 대조**), 사용자 작업(GitHub 토큰 만들기·웹 토큰 화면에 `github` 종류로 입력, 소유자 결정, **공개/비공개와 실제 저장소 생성은 사용자 결정(GitHub 저장소는 이 도구가 지우지 못한다)**), 롤백(바이너리 + 등록부 v2 복원, `keys/` 폴더 처리), 서버에서 확인하지 못한 것 전부.
- [ ] 전체 `go test ./... -race`, node 전체, `TestConstantsAgreeWithWeb` PASS; 커밋 `test: adversarial suite for W3e`, `docs: W3e results and cutover runbook`.

---

## 자체 검토 기록
- **범위:** 사용자의 세 요구(새 저장소·기존 연결·게시)를 E3~E5가 덮는다. 새 저장소 = `create_project`(GitHub), 기존 연결 = `link_project`(원격→로컬) + `attach_remote`(로컬→원격), 게시 = 키 인식 해석기. "기존 저장소 연결"의 GitLab 쪽이 W3c에 없었으므로(`docs/24` 자체 검토 기록에서 W3d로 미뤘음) 이 계획이 **두 제공자 모두**에 더한다(한 번에 만들기 위함). **의도적으로 뺀 것:** PR/MR 생성·조건부 자동 병합(W3d), 저장소·조직 삭제·정리(사용자가 직접), GitHub 조직 생성(API 없음).
- **W3d와의 순서:** 이 계획이 먼저든 W3d가 먼저든 독립적이다. 단 W3d의 병합·MR 호출이 제공자 인터페이스를 쓰게 하려면 **W3e의 E2를 먼저 하는 편이 낫다**(W3d 계획에서 이 점을 반영).
- **미결(구현자가 공식 문서로 정하고 보고서에 기록):** 콘텐츠 API 빈 저장소 첫 파일, 보호 `PUT`의 정확한 본문, 배포 키 `POST`의 응답 코드와 같은 키 중복 거부 응답, 세분화 토큰 권한 목록, 소유자 목록의 페이지네이션(링크 헤더를 따르지 않고 쪽 번호만).
- **사용자 확인이 필요한 값:** (계획 승인 시) 기본 방침 확인: ① GitHub "그룹 만들기" 제외, ② 저장소별 키를 orchd가 생성, ③ 공개/비공개 기본값 없음(항상 선택), ④ 방화벽 규칙은 사용자 결정. 구현·시험에는 필요 없고 컷오버에서만 GitHub 토큰·소유자·실제 생성 여부가 필요하다.
