# W3a 구현 계획: 프로젝트 등록부와 서명된 변경 (기존 저장소 등록)

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다.

**목표:** broker가 프로젝트 등록부를 소유하고 서명 승인을 직접 검증하며, 웹에서 서버에 이미 있는 저장소를 등록(AI 전송 허용 설정 포함)할 수 있게 한다. 토큰 메뉴·프로젝트 생성·자동 병합·GitHub는 이 계획의 범위가 아니다(W3b~W3e).

**구조:** `orch-broker`에 등록부 파일(`/var/lib/orch-broker/registry.json`, broker만 쓴다)을 두고, 변경 op(`register_project`, `set_flag`)는 **서명 원문을 함께 받아 broker가 검증**한다(`internal/approval/signing`에 새 메시지 형식 `aigate-registry-v1`을 더한다). orchd는 읽기 조회와 "변경 내용 → 서명 대상 메시지" 변환, 서명 전달만 한다. 웹은 프로젝트 목록과 등록 화면을 더하고 기존 패스키 서명 코드를 재사용한다.

**기술:** Go(broker, orchd, signing), 바닐라 JS(웹), 가짜 broker·가짜 서명자로 시험.

**설계서:** `docs/19-DESIGN-webconsole-W3-projects.md`(v0.4). 이 계획은 그 4.1~4.3, 4.5(일부), 5.2, 7, 8장의 W3a 몫이다.

## 전역 제약 (모든 작업에 적용)
- **비밀은 어디에도 새지 않는다**(W2b 규칙 그대로): 키·토큰·서명 원문 안의 비밀 없음, 오류는 고정 코드와 고정 문장뿐, 로그·원장·응답에 값 없음. W3a에는 토큰 값을 다루는 op가 없다(`set_token`은 W3b).
- **실제 서버·실제 GitLab·실제 OpenAI에 접속하지 않는다.** 시험은 임시 폴더, 가짜 서명자(시험용 ed25519 키와 WebAuthn 모의), 가짜 broker 소켓만. 실서버 적용은 B 이후 사용자 승인으로 별도(컷오버).
- **등록부와 서명 목록의 신뢰**: 등록부 파일은 `orch-broker` 소유 0600이고 폴더 `/var/lib/orch-broker`는 0700. 파일이 깨졌거나 소유·모드가 어긋나면 **변경과 조회 모두 fail closed**(등록부 없음과 깨짐을 구별: 파일이 아예 없을 때만 "처음 시작"으로 마이그레이션). 서명자 목록은 `/etc/aigate/signers.json`을 W1의 신뢰 검사(`signing.LoadRegistry`)로 읽는다(root 소유, 폴더 포함 쓰기 금지). 목록이 없거나 깨지면 모든 쓰기 op를 거부한다.
- **broker의 쓰기 op는 서명 없이는 아무것도 바꾸지 않는다.** 서명이 유효해도 메시지의 `change` 해시가 요청 내용에서 broker가 다시 계산한 값과 다르면 거부한다. 요청 ID는 한 번만 쓸 수 있다(1시간 보관, 재시작에도 유지).
- **orchd 바이너리는 `internal/broker`와 OpenAI 클라이언트 코드를 import하지 않는다**(W2b와 같음). 등록부 형식·메시지 조립에 필요한 순수 코드는 새 작은 패키지 `internal/registrymsg`로 두고 broker와 orchd가 함께 쓴다(HTTP·OpenAI 의존 없음).
- 새 쓰기 엔드포인트는 `requireWrite`(세션이면 CSRF) 뒤에 둔다. 서명은 요구한다. 웹: W1·W2a·W2b 규칙(인라인 스크립트·스타일 금지, 서버 문자열은 텍스트 노드, 위험 API 금지, `confirm/alert` 금지, 저장소(localStorage 등)에 상태 저장 금지).
- 기존 테스트의 의미를 약화하지 않는다. 바꿔야 하면 이유를 보고서에 쓴다. 커밋 메시지 끝: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. `rg`가 없으니 `grep -rn`(글롭은 따옴표).
- 검증 기본: `go vet ./...`, `gofmt -l .`(비어야 함), `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`, `go test ./internal/... ./cmd/... -count=1 -race`, 웹 작업은 `node --test "internal/orch/web/test/*.test.js"`. 전체 `go test ./... -count=1 -race`는 A6에서.

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **D1 등록부 v1(JSON, broker가 원자적으로 씀):** `{"version":1,"providers":[],"projects":[{"id","path","kind","ai_context","provider":null,"gitlab":null,"auto_merge":{"enabled":false}}],"auto_merge_global":{"enabled":false,"max_per_day":5}}`. W3a는 `providers`를 항상 빈 배열로, `provider`·`gitlab`을 `null`로 두고(자리만 있음), `auto_merge`는 항상 꺼짐으로 둔다(**W3a의 `set_flag`는 `ai_context`만 바꿀 수 있고 `auto_merge`·전역 스위치를 켜려는 요청은 `not_supported`로 거부**). `id`는 정규식 `^[a-z][a-z0-9-]{0,39}$`, `path`는 정리된 절대 경로, `kind`는 정책 클래스 이름(`^[a-z][a-z0-9_-]{0,31}$`). 알 수 없는 키는 거부한다.
- **D2 마이그레이션(결정 5, 기본안):** 등록부 파일이 **없을 때** broker가 시작하면 `broker.json`의 `context_repos`와 `gitlab.projects`를 한 번 읽어 등록부를 만든다(`id`는 경로의 마지막 요소를 정규식에 맞게 정리, 충돌하면 시작 거부). 그 뒤로는 **등록부가 우선**이고 옛 항목은 무시된다. `orch-broker -check`는 등록부가 있는데 옛 항목이 남아 있으면 **경고만**(종료 코드 0, 시작은 막지 않음). `gitlab.projects`의 매핑은 W3a에서는 등록부 `gitlab: {project, project_id: null}`로 옮기기만 하고 MR 생성(W2b)은 **등록부의 `gitlab.project`**를 읽어 계속 동작해야 한다(회귀 시험).
- **D3 서명 메시지(`aigate-registry-v1`)**, 줄 순서 고정, 끝에 개행:
  ```
  aigate-registry-v1
  action: <register_project | set_flag>
  subject: <프로젝트 id>
  change: <변경 내용 정규 JSON의 SHA-256 64자리 소문자 hex>
  request: <무작위 128비트 = 32자리 소문자 hex>
  issued: <UTC 초 단위 RFC 3339, 예 2026-11-09T12:00:00Z>
  ```
  `change`의 입력은 **정규 JSON**: 키를 사전순으로 정렬하고 공백 없이, 사람이 읽는 화면 문구가 아니라 `{"action":…,"subject":…,"fields":{…}}` 구조의 변경 내용(`register_project`: `path`, `kind`, `ai_context`; `set_flag`: `flag`, `value`). 접두 `aigate-registry-v1`은 기존 `aigate-approval-v1`과 달라서 두 파서가 서로의 메시지를 거부한다. 검증은 `signing.Verify`와 같은 의미(서명자 목록, 알고리즘 일치, 5분 이내, ssh-ed25519는 `NamespaceApproval`이 아닌 **새 네임스페이스 `aigate-registry`**, WebAuthn은 `Digest()`를 challenge로)를 따르고 **기존 `Verify`의 동작과 시험은 바꾸지 않는다**.
- **D4 재사용 방지:** `signing.Verify`에는 WebAuthn 서명 카운터 검사가 **없다**(코드 확인: `verify.go`에 카운터 처리 없음). 따라서 W3a의 재전송 방어는 **(a) 5분 신선도 + (b) 요청 ID 1회성**이다. broker가 쓴 요청 ID를 `/var/lib/orch-broker/used-requests.json`에 시각과 함께 저장(1시간 지난 항목 정리, 파일이 깨졌으면 쓰기 op 거부)한다. 카운터 검사는 도입하지 않고 이 한계를 결과 문서에 적는다.
- **D5 오류 코드(전부 고정 문장, broker `fixedMessages`에 등록):** `not_signed`(서명 없음), `bad_signature`, `unknown_signer`, `stale_signature`, `signers_unavailable`(서명자 목록 없음·깨짐), `request_reused`, `change_mismatch`, `project_exists`, `project_unknown`, `path_not_allowed`(경로가 `repo_roots` 밖·정리되지 않음·심볼릭 링크·저장소 아님), `registry_unavailable`(등록부 깨짐), `not_supported`. orchd는 이를 같은 이름의 고정 코드·문장과 HTTP 상태(400/403/404/409/503)로 바꾸고 broker 문장을 그대로 전달하지 않는다.
- **D6 `repo_roots`의 출처와 경로 검사의 분담(정정됨, 컨트롤러 판정):** broker는 `repo_roots`를 broker 설정(`broker.json`의 새 선택 키 `repo_roots: ["/home/orch/repos"]`)에서 읽는다. 값이 없으면 `register_project`는 `path_not_allowed`로 거부한다(안전한 기본). **broker의 경로 검사는 문법뿐이다**: 4096바이트 이하, 절대 경로, `filepath.Clean(p)==p`, 부모가 `repo_roots` 항목과 **글자 그대로 같음**(= 바로 아래 한 단계, 루트 자체는 거부). **broker는 등록 경로에 대해 파일 시스템을 전혀 건드리지 않는다**(`lstat`·`.git`·폴더 여부 검사 없음). 이유: 실서버에서 `/home/orch/repos`는 `orch:orch 0700`이라 broker 계정(`orch-broker`)이 `SupplementaryGroups=orch`가 있어도 설계상 읽지 못한다. broker가 지키는 성질은 "AI 전송 허용이 서명자가 승인한 그 경로에만 적용된다"는 것이다. **파일 시스템 검증은 orchd만 한다**(`propose`와 `apply` 모두, broker를 부르기 전에): 심볼릭 링크 아님, 폴더, `.git`은 폴더이거나 일반 파일, 정책 `repo_roots` 바로 아래, `ownRepo`(git 천장·gitdir이 루트 아래), HEAD 있음. 사용 시점(미리보기·`refine`)에는 등록 뒤에 링크로 바꿔치기된 경로를 거부한다. 남는 위험: 등록 경로가 존재하지 않거나 이상한 폴더일 수 있으나 orchd가 사용 시점에 거부하므로 무해하다.
- **D7 등록부 조회:** broker `projects` op(읽기, 서명 불필요)는 `{"projects":[…],"providers":[],"auto_merge_global":{…}}`를 준다(비밀 없음). 기존 `status`의 `context_repos`는 **등록부에서 `ai_context:true`인 프로젝트의 경로**로 계산해 계속 준다(orchd 쪽 W2b 코드 호환). `refine`의 허용 목록 검사(`repoAllowed`)도 등록부를 쓴다.
- **D8 orchd 엔드포인트(`/api/v1`):** `GET /projects/registry`(등록부 목록, 읽기. **`GET /projects`는 이미 저장소별 작업 요약으로 쓰이므로 경로를 바꿨다**, A4 결정), `GET /projects/candidates`(`repo_roots` 아래 아직 등록되지 않은 저장소, 읽기), `POST /projects/propose`(본문 `{action, subject, fields}` → `{message, change, request, issued, preview}`: 서버가 정규 JSON·해시·요청 ID·`issued`를 만들고 **화면에 보여 줄 한국어 설명 문장 배열**을 함께 줌, **아무것도 저장하지 않음**), `POST /projects/apply`(본문 `{action, subject, fields, request, issued, signature}` → broker로 전달, 성공하면 갱신된 프로젝트 반환). `apply`가 받은 `fields`로 **서버가 다시 `change`를 계산**해 서명 메시지와 맞는지 확인한 뒤 broker에 넘기고 broker도 한 번 더 확인한다(이중 확인). 원장에 `project_change` 이벤트 `{actor, action, subject, ok, code}`(내용 없음).
- **D9 웹:** 새 화면 `#/projects`(목록: id, 경로, AI 전송 허용 여부, GitLab 연결 이름·자동 병합 표시는 읽기 전용 자리표시만) + "프로젝트 추가"(후보 저장소 고르기 → 설정 `ai_context`(기본 꺼짐) → **변경 내용 전체를 보여 주는 확인 화면**(propose의 `preview`) → 패스키 서명(`webauthn.js` 재사용, challenge는 메시지의 SHA-256) → 완료) + 프로젝트 행에서 `ai_context` 켜기/끄기(같은 서명 흐름). 확인 화면은 서버가 준 `change`를 화면의 `fields`로 **브라우저가 다시 계산**한 값과 비교해 같을 때만 서명 단추를 연다. CLI 서명(SSH)은 W3a에서 웹 UI를 만들지 않고, API가 `alg: ssh-ed25519`도 받아들이는 것만 시험한다.
- **D10 새 프로젝트의 정책 연결:** W3a는 `kind`를 사용자가 정책 파일의 **기존 클래스 이름** 중에서만 고른다(`GET /projects/candidates` 응답이 정책의 클래스 이름 목록도 준다: 로드된 `policy.Policy`의 `Classes` 맵의 키, 정렬해서). `registered` 클래스를 정책에 넣는 일은 W3c(프로젝트 생성) 몫이다.

## 검토 초점 (설계서가 암시하지만 개별 작업 시험이 안 다루기 쉬운 것, 가능성 순)
1. **서명 없는 쓰기 / 변조된 쓰기**: `orch` 계정(orchd)이 서명 없이, 또는 서명은 유효하지만 다른 내용으로 broker에 변경을 시도하는 경우(→ A3 `change_mismatch`, A6 적대 시험).
2. **재전송과 재사용**: 같은 서명·요청 ID 다시 보내기, 만료 직전·직후, broker 재시작 뒤 재전송(→ A2·A3).
3. **등록부를 통한 AI 전송 허용 확대**: 경로 조작(`..`, 심볼릭 링크, 정리되지 않은 경로, `repo_roots` 밖, 중첩된 하위 폴더), 같은 저장소 중복 등록, `id` 충돌로 다른 프로젝트 덮어쓰기(→ A3).
4. **마이그레이션 사고**: 등록부가 없을 때만 옛 설정을 옮기는지, 깨진 등록부를 "없음"으로 오해해 옛 설정으로 되돌려 AI 허용을 넓히지 않는지, W2b의 `refine`·`create_mr`가 등록부 전환 뒤에도 동작하는지(→ A2).
5. **화면과 서명의 일치**: 서명 화면의 변경 설명과 실제로 서명되는 `change`가 다른 경우, 서버가 준 설명 문장이 HTML로 해석되는 경우(→ A5).

---

### Task 1 (A1): 서명 메시지 `aigate-registry-v1`과 검증

**파일:**
- 만들기: `internal/approval/signing/registry_message.go`, `internal/approval/signing/registry_message_test.go`, `internal/registrymsg/registrymsg.go`, `internal/registrymsg/registrymsg_test.go`
- 수정: `internal/approval/signing/verify.go`(공통 검증 보조 함수를 **추가**, 기존 `Verify` 동작은 그대로)

**인터페이스:**
- 소비: 기존 `signing.Registry`, `signing.Signature`, `signing.VerifyOptions`, `VerifySSHSig`, `verifyWebAuthn`.
- 생산: `signing.RegistryMessage{Action, Subject, Change, Request string; Issued time.Time}`와 `Validate()`, `Text()`, `Digest()`, `ParseRegistryMessage(text)`, `signing.VerifyRegistry(reg Registry, msg RegistryMessage, sig Signature, o VerifyOptions) error`(같은 `ErrBadSignature` 등 센티널 오류), 상수 `NamespaceRegistry = "aigate-registry"`. `registrymsg.CanonicalChange(action, subject string, fields map[string]any) ([]byte, error)`(키 사전순·공백 없는 JSON, 알 수 없는 필드·부동소수점 거부)와 `registrymsg.ChangeHash(...) string`(64자리 소문자 hex), `registrymsg.NewRequestID() string`(128비트 무작위).

- [ ] **Step 1: 실패하는 시험(이름 그대로)** — `TestRegistryMessageTextRoundTrip`, `TestRegistryMessageRejectsBadFields`(action 모르는 값, subject가 정규식 위반, change가 64자리 hex 아님, request가 32자리 hex 아님, issued가 UTC 초 단위 아님), `TestApprovalAndRegistryMessagesRefuseEachOther`(`ParseMessage`가 registry 텍스트를, `ParseRegistryMessage`가 approval 텍스트를 거부), `TestVerifyRegistrySSH`(새 네임스페이스로 서명한 것만 통과, 기존 `aigate-approval` 네임스페이스 서명은 거부), `TestVerifyRegistryWebAuthn`(기존 `verify_test.go`의 모의 서명자 도우미를 재사용, challenge가 `Digest()`가 아니면 거부), `TestVerifyRegistryFreshness`(±5분 밖 거부), `TestVerifyRegistryUnknownSignerAndAlgMismatch`, `TestExistingVerifyUnchanged`(기존 `Verify` 시험이 전부 그대로 통과), `TestCanonicalChangeStableAndStrict`(키 순서가 달라도 같은 해시, 알 수 없는 필드·부동소수점·중복 키 거부), `TestChangeHashChangesWithAnyField`.
- [ ] **Step 2: 시험이 실패하는지 확인** — `go test ./internal/approval/signing/ ./internal/registrymsg/ -count=1`, 기대: 정의되지 않은 심볼로 컴파일 실패.
- [ ] **Step 3: 구현** — `RegistryMessage`는 기존 `Message`와 같은 스타일(엄격한 `ParseRegistryMessage`는 바이트 단위로 정확한 정규 텍스트만 받음). `VerifyRegistry`는 `Verify`와 같은 단계를 거치되 SSH 네임스페이스만 `NamespaceRegistry`. 중복을 줄이기 위해 서명 종류별 검증 코어를 `verifyCore(signer, textBytes, digest, sig)`로 뽑아 두 함수가 쓰게 하되 **기존 `Verify`의 공개 동작·오류 값은 불변**. `registrymsg`는 HTTP·파일·시간 의존이 없는 순수 패키지.
- [ ] **Step 4: 통과 확인** — 같은 명령 + `go vet`/`gofmt`, 기존 `./internal/approval/...` 전체 시험이 그대로 통과.
- [ ] **Step 5: 커밋** — `feat(signing): registry change messages (aigate-registry-v1) and verification`.

---

### Task 2 (A2): broker 등록부와 마이그레이션

**파일:**
- 만들기: `internal/broker/registry.go`, `internal/broker/registry_test.go`, `internal/broker/usedreq.go`, `internal/broker/usedreq_test.go`
- 수정: `internal/broker/config.go`(`repo_roots` 선택 키, 시작 순서), `internal/broker/server.go`(`New`가 등록부를 로드, `statusHandler`·`repoAllowed`가 등록부 사용), `internal/broker/mr.go`(프로젝트의 GitLab 이름을 등록부에서 읽음), `cmd/orch-broker/main.go`(`-check`의 옛 항목 경고)

**인터페이스:**
- 소비: A1의 `registrymsg`(타입이 필요하면), 기존 `Config`, `trust` 검사 도우미.
- 생산: `type Registry struct`(D1 모양), `type Project struct`, `OpenRegistry(dir string, legacy LegacySource, o RegistryOptions) (*RegistryStore, error)`(없으면 마이그레이션, 있으면 로드, 깨졌으면 `ErrRegistryBroken`), `(*RegistryStore).Snapshot() Registry`, `(*RegistryStore).Update(func(*Registry) error) error`(잠금 안에서 변경 → 검증 → 임시 파일에 쓰고 fsync → rename, 0600, **쓰기 실패 시 메모리 상태를 되돌림**), `(*RegistryStore).AIContextPaths() []string`, `(*RegistryStore).GitLabProject(path string) (string, bool)`, `UsedRequests{Mark(id string, now time.Time) error}`(`ErrRequestReused`).

- [ ] **Step 1: 실패하는 시험** — `TestRegistryFreshStartMigratesLegacyConfig`(없는 등록부 + `context_repos`·`gitlab.projects` → 등록부 생성, 모든 `ai_context`가 옛 `context_repos`와 같고 GitLab 이름이 옮겨짐), `TestRegistryExistingWinsOverLegacy`(등록부가 있으면 옛 항목 변경은 무시), `TestRegistryBrokenIsFailClosedNotMigrated`(깨진 JSON·알 수 없는 키·버전 불일치·소유자/모드 불량 → 시작이 조회·쓰기 모두 `registry_unavailable`, **옛 설정으로 폴백하지 않음**), `TestRegistryAtomicWriteSurvivesCrash`(쓰기 중간 실패 주입 → 이전 내용 유지), `TestRegistryRejectsInvalidProjects`(id 정규식, 중복 id, 중복 path, 상대·정리 안 된 path, `auto_merge.enabled:true`는 W3a에서 불가), `TestRegistryFileMode0600`, `TestUsedRequestsOnceAndPersistAcrossRestart`, `TestUsedRequestsExpireAfterOneHour`(시계 주입), `TestUsedRequestsBrokenFileRefusesWrites`, `TestRefineUsesRegistryAllowList`(등록부에서 `ai_context:false`로 바꾸면 W2b `refine`이 `repo_not_allowed`), `TestCreateMRUsesRegistryGitLabProject`(W2b `create_mr`이 등록부의 GitLab 이름으로 동작, 매핑 없으면 `repo_not_mapped`), `TestStatusContextReposFromRegistry`, `TestCheckWarnsAboutLegacyEntries`(`-check`가 경고 문장 출력하고 종료 코드 0).
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ -count=1`.
- [ ] **Step 3: 구현** — 등록부는 D1 모양과 검증을 지키고 **쓰는 코드는 이 파일 하나에만** 둔다. `New`의 시작 순서: 설정 신뢰 검사 → 등록부 열기(마이그레이션) → 사용된 요청 ID 열기 → 핸들러 등록. 시작 실패는 고정 문장으로 종료(값 인용 금지). `repo_roots` 키는 선택(없으면 빈 목록), 값은 정리된 절대 경로만.
- [ ] **Step 4: 통과 확인** — `-race` 포함, 기존 broker 시험이 의미 변경 없이 통과(바꾼 것은 보고서에 이유 기록).
- [ ] **Step 5: 커밋** — `feat(broker): project registry owned by the broker with fail-closed load and legacy migration`.

---

### Task 3 (A3): broker 읽기·쓰기 op와 서명 검증

**파일:**
- 만들기: `internal/broker/projects.go`, `internal/broker/projects_test.go`
- 수정: `internal/broker/server.go`(`projects`, `register_project`, `set_flag` 핸들러 등록, `fixedMessages`에 D5 코드)

**인터페이스:**
- 소비: A1의 `signing.VerifyRegistry`·`registrymsg`, A2의 `RegistryStore`·`UsedRequests`, `signing.LoadRegistry(signing.RegistryPath(), signing.DefaultOptions)`.
- 생산: 요청 `{"v":1,"op":"register_project","args":{"id","path","kind","ai_context","request","issued","signature":{signer,alg,sig,webauthn?}}}`, `set_flag` args `{"id","flag":"ai_context","value":bool,"request","issued","signature"}`, 읽기 `projects` args 없음. 응답은 갱신된 프로젝트(D1 모양). 허용 op 이름 목록에 세 개 추가.

- [ ] **Step 1: 실패하는 시험** — `TestProjectsReadIsOpenAndSecretFree`, `TestRegisterRequiresValidSignature`(서명 없음 `not_signed`, 모르는 서명자 `unknown_signer`, 잘못된 서명 `bad_signature`, 5분 밖 `stale_signature`, 서명자 목록 없음/깨짐 `signers_unavailable`), `TestRegisterChangeMismatchRefused`(서명은 유효하나 args의 path/kind/ai_context 중 하나가 서명된 `change`와 다름 → `change_mismatch`, **등록부 불변**), `TestRegisterRequestIDOnce`(같은 요청 ID 두 번 → `request_reused`, 재시작 뒤에도), `TestRegisterPathRules`(`..`, 정리 안 된 경로, 절대 아님, `repo_roots` 밖, `repo_roots` 자체, 두 단계 이상 아래, 심볼릭 링크, `.git` 없음 → `path_not_allowed`), `TestRegisterDuplicateIDOrPath`(`project_exists`, 기존 항목 불변), `TestSetFlagAIContextOnlyAndSigned`(서명 확인, `auto_merge`·전역 스위치는 `not_supported`), `TestSetFlagUnknownProject`(`project_unknown`), `TestWriteOpsNeverChangeRegistryOnAnyFailure`(모든 실패 경로 뒤 등록부 바이트가 변경 전과 같음), `TestFixedMessagesRegisteredForNewCodes`, `TestOpAllowlistIncludesOnlyNewOps`(삭제·토큰 op 이름은 여전히 `unknown_op`), `TestWriteOpsRefuseWhenRegistryBrokenOrUsedFileBroken`, `TestConcurrentRegisterSameIDCreatesOnce`(`-race`, N개 동시, 하나만 성공).
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ -count=1 -run 'Register|Projects|SetFlag'`.
- [ ] **Step 3: 구현** — 핸들러 순서: 인자 엄격 파싱(알 수 없는 필드 거부) → 서명자 목록 로드(**호출마다** 읽어 root가 서명자를 지우면 즉시 반영) → 메시지 조립(`action`, `subject=id`, `change=ChangeHash(…)`, 인자의 `request`·`issued`) → `VerifyRegistry` → 요청 ID 소모(`Mark`) → 경로·중복 검사 → `Update`. **검증이 모두 끝나기 전에는 요청 ID를 소모하지 않지만, 서명이 유효한 요청은 이후 단계(경로 거부 등)에서 실패해도 소모한다**(같은 서명을 계속 재시도하지 못하게). 등록부 쓰기는 `RegistryStore.Update` 한 곳.
- [ ] **Step 4: 통과 확인** — `-race`.
- [ ] **Step 5: 커밋** — `feat(broker): projects, register_project and set_flag with signature verification`.

---

### Task 4 (A4): orchd 엔드포인트와 broker 클라이언트 확장

**파일:**
- 만들기: `internal/orch/api/projects.go`, `internal/orch/api/projects_test.go`
- 수정: `internal/orch/broker/client.go`(새 op 호출 도우미와 오류 코드 매핑), `internal/orch/api/{v1.go,api.go,daemon.go}`(경로, 설정), `internal/orch/api/refine.go`(허용 여부를 등록부 기반 `status`에서 읽는 기존 동작 확인만)

**인터페이스:**
- 소비: A1의 `registrymsg`, A3의 broker op들, W2a의 `/repos`(`repos.go`)의 저장소 후보 계산, `requireWrite`, 정책의 클래스 이름 목록.
- 생산: D8의 네 엔드포인트와 응답 JSON, 원장 `project_change` 이벤트, `TaskView`·기존 응답은 변경 없음.

- [ ] **Step 1: 실패하는 시험** — `TestProjectsListFromBroker`(가짜 broker), `TestCandidatesExcludeRegisteredAndOutsideRoots`(이미 등록된 저장소·`repo_roots` 밖·심볼릭 링크 제외, 정책 클래스 이름 목록 포함), `TestProposeBuildsMessageAndStoresNothing`(응답의 `change`가 `registrymsg`로 다시 계산한 값과 같음, `preview` 문장이 비어 있지 않음, 가짜 broker가 **쓰기 호출을 받지 않음**), `TestProposeRejectsBadFieldsAndHostileText`(제어·방향 전환 문자, 알 수 없는 필드, 너무 긴 값), `TestApplyRecomputesChangeAndRefusesMismatch`(본문의 `fields`가 서명된 `change`와 다르면 400, broker 호출 없음), `TestApplyForwardsSignatureAndMapsErrors`(broker의 모든 D5 코드가 고정 문장·상태로 매핑, broker 문장 미전달), `TestApplyRequiresWriteTierAndCSRF`(401/403), `TestApplyLedgerEventHasNoContent`(원장 줄에 경로·내용 없음, 행위자·action·subject·ok·code만), `TestProjectsWorkWithBrokerDown`(소켓 없음 → 읽기·쓰기 `broker_unavailable`, **기존 orchd 기능은 그대로**), `TestAPIAcceptsSSHSignature`(`alg: ssh-ed25519` 서명도 broker로 그대로 전달), `TestConstantsAgreeWithWeb`(새 정규식·상수가 웹과 같음: 기존 `constants_agree_test.go`의 방식 재사용).
- [ ] **Step 2: 실패 확인** — `go test ./internal/orch/api/ -count=1 -run 'Project|Candidates|Propose|Apply'`.
- [ ] **Step 3: 구현** — `propose`는 읽기 전용(저장·broker 쓰기 없음). `apply`는 서버가 `fields`에서 `change`를 다시 계산하고 서명 메시지(`request`, `issued`, `change`)를 구성해 **broker에 서명과 함께 그대로** 넘긴다. orchd는 서명 유효성을 직접 판단하지 않는다(판단은 broker). 후보 조회는 W2a의 `/repos` 방어(git 천장, 허용 루트)를 그대로 재사용한다.
- [ ] **Step 4: 통과 확인** — `-race`, 기존 api 시험 의미 변경 없이 통과.
- [ ] **Step 5: 커밋** — `feat(api): project list, candidates, propose and apply endpoints backed by the broker registry`.

---

### Task 5 (A5): 웹 — 프로젝트 목록과 서명된 변경

**파일:**
- 만들기: `internal/orch/web/static/views/projects.js`, 시험 `internal/orch/web/test/projects.test.js`
- 수정: `static/shell.js`(메뉴), `static/router.js`·`app.js`(필요 시), `static/render.js`(변경 없으면 그대로), `style.css`, `internal/orch/api/web_fixtures_test.go`(프로젝트·후보·propose 응답을 **실제 Go 구조체**에서 생성), 시험 도우미 `test/fake-ctx.js`

**인터페이스:**
- 소비: A4의 엔드포인트, `webauthn.js`(기존 `signApproval(detail, signer)`는 **승인 항목 `detail`용이라 그대로 못 쓴다**: 같은 방식(challenge = 메시지 SHA-256, UV 필수, `activeSigners`·`rememberedPasskey` 재사용)으로 **새 함수 `signRegistryChange(message, signer)`를 이 작업이 추가**하고 `signApproval`과 겹치는 부분만 공통 보조 함수로 뽑는다. `signApproval`의 동작·시험은 불변), W2b의 `findHostile`·`SAFE_TOKEN_RE`·`submitErrorText`·`noRetry429` 옵션.
- 생산: `#/projects` 화면, `registerFlow`(후보 → 설정 → 확인 → 서명 → 결과), `computeChange(action, subject, fields) → hex`(브라우저가 `registrymsg`와 같은 정규 JSON·SHA-256을 계산하는 순수 함수).

- [ ] **Step 1: 실패하는 시험(JS, 이름 그대로)** — `TestComputeChangeMatchesGoVector`(Go가 만든 고정 벡터 JSON을 읽어 같은 해시), `TestProjectsListRendersAsText`(경로·id·종류가 텍스트 노드, 악성 글 `<img onerror>`·`javascript:`가 속성·`href`로 안 감), `TestRegisterNeedsConfirmationBeforeSigning`(확인 화면 전에는 서명 요청이 나가지 않음, `propose`가 먼저 호출되고 `apply`는 서명 뒤에만), `TestSignButtonDisabledUntilChangeMatches`(서버가 준 `change`가 브라우저 계산값과 다르면 서명 단추 없음과 고정 경고), `TestConfirmShowsEveryFieldBeingChanged`(경로, 종류, AI 전송 허용 여부를 모두 표시, "AI 전송 허용: 예"일 때 강조 문구), `TestAIContextDefaultsOff`, `TestToggleAIContextGoesThroughSignature`, `TestApplyErrorsFixedTexts`(D5의 모든 코드 → 고정 문구, 서버 원문 미표시), `TestSubmitOnceNoDoubleSign`(더블 클릭에도 서명 요청 1회), `TestNoStorageAndNoNavigateTrap`(저장소 API 미사용 정적 검사, 계획서 화면과 같은 이탈 규칙 재사용 여부는 작성 중인 입력이 없으면 가드 없음), `TestNoRetry429ForProjectWrites`. 기존 정적 위험 API 검사(`TestNoForbiddenSinksInStaticJS`)와 `TestWebAssetsEmbedded`가 새 파일을 포함해 통과.
- [ ] **Step 2: 실패 확인** — `node --test "internal/orch/web/test/projects.test.js"`.
- [ ] **Step 3: 구현** — 서버 문자열(경로·id·설명 문장·오류)은 모두 `el()` 텍스트 노드. 정규 JSON 계산은 Go `registrymsg`와 같은 규칙(키 정렬, 공백 없음, 정수·불리언·문자열만). 서명은 `webauthn.js`에 새로 추가한 `signRegistryChange`가 메시지 텍스트의 SHA-256을 challenge로 하여 수행한다(`signApproval`과 같은 오류 처리·`WA_TEXT` 문구 재사용). 화면은 W2b와 같은 톤(한국어, 짧은 문구).
- [ ] **Step 4: 시각 확인** — `ego-browser skill`을 먼저 읽고 W2b의 개발용 픽스처 서버(`web/dev_test.go`, `ORCH_WEB_DEV`)에 프로젝트 목록·후보·propose·apply 성공·실패 픽스처를 추가해 1280px·390px 화면을 보고 **본 것만** 보고서에 쓴다(키보드·스크린리더 여부도 사실대로). **실제 앱 경로**(`app.js`)로 목록 → 등록 → 서명 → 결과까지 한 번 걷는다(W2b에서 `ctx.navigate`가 시험에만 있던 결함 재발 방지).
- [ ] **Step 5: 변이 확인(복사본)** — 확인 화면 없이 `apply` 전송 → 시험 실패; `change` 비교 제거 → 시험 실패; 서버 문장을 속성에 넣음 → 시험·정적 검사 실패.
- [ ] **Step 6: 통과 확인 + 커밋** — `go test ./internal/orch/web/... ./internal/orch/api/... -race`, node 전체; `feat(web): projects list and signed project registration`.

---

### Task 6 (A6): 적대 시험, 결과 문서, 컷오버 런북

**파일:** 만들기 `internal/broker/adversarial_w3a_test.go`, `internal/orch/api/adversarial_w3a_test.go`, `docs/21-RESULTS-webconsole-W3a.md`; 수정 `docs/10-OPERATIONS-orchestrator.md`(W3a 컷오버 절), `docs/19-DESIGN-webconsole-W3-projects.md`(상태 한 줄), `infra/README.md`

- [ ] **적대 시험(모두 거부/무효):** broker: `TestRegistryNeverWrittenWithoutValidSignature`(모든 op·모든 실패 경로 뒤 등록부 바이트 불변), `TestForeignUIDCannotWrite`(허용 uid 밖), `TestOrchdIdentityCannotReadOrWriteRegistryFile`(파일 모드·소유 단언), `TestReplayAfterRestartRefused`, `TestSignatureOverDifferentChangeRefused`(다른 `change`에 대한 유효 서명 재사용), `TestSSHNamespaceMismatchRefused`(`aigate-approval` 네임스페이스 서명으로 registry 변경 시도), `TestApprovalMessageCannotAuthorizeRegistryChange`(승인용 서명 메시지를 registry 변경에 재사용 불가, 반대도), `TestSignerRemovedTakesEffectImmediately`(서명자 목록에서 서명자를 지우면 다음 요청부터 거부), `TestLegacyMigrationOnlyWhenRegistryAbsent`, `TestBrokenRegistryDoesNotWidenAIContext`(손상 → `context_repos` 비어 있음 + `refine` 거부), `TestStatusAndProjectsNeverContainSecrets`. orchd: `TestProposeNeverWritesBroker`, `TestApplyForgedFieldsRefusedBeforeBroker`, `TestProjectsLedgerNoPathsOrContent`, `TestOrchdWorksWithBrokerDown`(기존 기능 그대로), `TestNoBrokerImportInOrchd`(`go list -deps ./cmd/orchd`에 `internal/broker` 없음).
- [ ] **변이 확인(저장소 복사본에서만):** ① 서명 검증 제거 ② `change` 일치 검사 제거 ③ 요청 ID 1회성 제거 ④ 신선도 검사 제거 ⑤ 경로 규칙 제거 ⑥ 깨진 등록부를 "없음"으로 취급 ⑦ 마이그레이션이 항상 실행 ⑧ 서명자 목록 캐시(호출마다 읽지 않음) ⑨ 웹에서 `change` 비교 제거 ⑩ orchd가 서명을 직접 판정하게 함. 각각 어느 시험이 실패했는지 표로 기록(안 걸리면 시험을 추가하고 다시).
- [ ] **결과 문서(`docs/21`):** W2b의 `docs/18` 구조: 만든 것, 시험 목록(실제 개수), 변이 표, 설계서와 달라진 곳, **실서버에서 확인하지 못한 것**(broker가 `/etc/aigate/signers.json`을 실제로 읽는지(읽기 권한은 확인함), 실제 패스키 서명이 broker에서 검증되는지, 마이그레이션이 실제 `broker.json`으로 올바른 등록부를 만드는지, 서명 카운터 부재의 한계, 등록부 경로 규칙이 실제 `/home/orch/repos`에서 동작하는지, 웹 서명 흐름 키보드·스크린 리더), 사용자 결정 목록.
- [ ] **컷오버 런북(W3a, `docs/10`의 새 절):** W2b 런북 양식(순서 고정, 단계마다 백업, `[사용자]`/`[주의]`/`[되돌림 가능]`, 검증 체크리스트와 "그래도 안 되면" 줄). 0단계 시작 전 조건(W2b 컷오버 상태, 실행 중 작업 없음). 1단계 백업(`broker.json`, `/var/lib/orch-broker`, 바이너리 `*.prev-w3a`, 유닛)과 broker·orchd 새 바이너리 배포, `broker.json`에 `repo_roots` 추가, `orch-broker -check`(옛 항목 경고 확인), broker 시작 → **등록부가 마이그레이션되었는지**(`projects` 호출 결과가 현재 `context_repos`와 같은지 `python3` 소켓 호출로 확인), 2단계 orchd 재시작(DB 스키마 변경 없음), 3단계 웹 확인(프로젝트 목록 → AI 계획서가 그대로 동작하는지(회귀) → 후보 저장소가 있으면 등록 흐름은 **사용자 결정**: 실제로 서명해 등록할지, 이미 등록된 두 프로젝트의 `ai_context`를 껐다 켜 볼지) → 서명 거부 확인(가능하면 잘못된 서명은 자동 시험만), 4단계 롤백(등록부 파일을 지우고 이전 바이너리·`broker.json`으로 되돌리면 옛 방식이 다시 동작함을 확인: 마이그레이션은 등록부가 없을 때만 일어나므로 롤백 뒤 다시 올리면 **그 사이 등록부 변경이 사라진다**고 경고).
- [ ] **`go test ./... -count=1 -race` 전체 + node 테스트**, 커밋 `test: adversarial suite for W3a`, `docs: W3a results and cutover runbook`.

---

## 자체 검토 기록
- **설계서 대비 범위:** 4.1(구조)·4.2(서명 검증)·4.3(등록부 모양) → A1·A2·A3; 4.5의 `projects`/`register_project`/`set_flag`(W3a 몫) → A3; 5.2(기존 저장소 추가) → A4·A5; 7장(메시지) → A1; 8장(API) → A4; 6장 위협 → A6 적대 시험. **의도적으로 뺀 것:** 토큰(`set_token`, W3b), 제공자·그룹·프로젝트 생성·첫 커밋(W3c), 자동 병합(W3d), GitHub(W3e), 정책 `registered` 클래스.
- **설계서와 달라진 점(이 계획이 정한 것):** 서명 메시지의 `change`는 화면 문구가 아니라 정규 JSON의 해시(설계서 7장의 구체화), SSH 네임스페이스 `aigate-registry` 신설, WebAuthn 카운터는 **기존 `Verify`에 없는 것을 확인해** 도입하지 않고 요청 ID 1회성으로 대신함(설계서 11장 결정 3의 "broker가 카운터를 따로 기록" 안은 **실현 가능성 조사 결과 불필요**: 카운터 상태 자체가 없다. 이 변경을 사용자에게 알린다), `repo_roots`를 broker 설정에서 읽음(D6), `set_flag`는 `ai_context`만.
- **이름 일관성:** op `projects`/`register_project`/`set_flag`, 오류 코드(`not_signed`, `bad_signature`, `unknown_signer`, `stale_signature`, `signers_unavailable`, `request_reused`, `change_mismatch`, `project_exists`, `project_unknown`, `path_not_allowed`, `registry_unavailable`, `not_supported`), `RegistryMessage`, `VerifyRegistry`, `NamespaceRegistry`, `registrymsg.ChangeHash`, 등록부 필드(`ai_context`, `auto_merge`, `kind`)가 A1~A6에서 같다.
- **미결(구현 중 구현자가 정하고 보고서에 기록):** `registrymsg`의 정규 JSON에서 허용하는 값의 종류(문자열·불리언·정수만으로 시작), 서명 화면의 설명 문장 문구, `-check` 경고 문장.
- **사용자 확인이 필요한 값:** 없음(구현·시험에는 필요 없음). 컷오버에서 등록 흐름을 실제로 서명해 볼지만 사용자 결정(A6 런북).
