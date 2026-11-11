# 웹 콘솔 W3c — 결과 기록 (2026-11-11)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w3c`)에서 끝났고, 실제 서버(orch-vm)와 실제 GitLab에는 아직 아무것도 하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 14장, 설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `24-PLAN-webconsole-W3c.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 GitLab 서버·가짜 broker·임시 폴더·가짜 `git`/`ssh` 경로·로컬 bare 저장소·명백한 더미 값**)과 ego-browser로 개발용 고정 서버를 본 것(C5 보고서)만 가리킨다. **실제 GitLab 19.0.8, 실제 관리자 토큰, 실제 패스키 서명, 리눅스 systemd 샌드박스 안의 broker·orchd, 실제 `uid`의 파일 소유·모드, 실제 `ssh`로의 `git clone`/`push`는 한 번도 쓰지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다. 시험 값은 `canary-NOT-A-REAL-TOKEN-…` 꼴의 더미다.
> **W3a·W3b의 교훈(임시 폴더 시험이 실서버의 권한 문제를 숨겼음)을 구조에 넣었다:** 새 코드가 실행 사용자·유닛 샌드박스 아래에서 만지는 경로·소유자·모드는 4장의 표로 모았고, 컨트롤러가 컷오버 전에 서버에서 읽기 전용으로 보는 목록은 런북 14.0에 있다. 이 저장소의 시험은 소유자·모드를 흉내 낼 뿐 **실제 uid와 실제 샌드박스를 거친 적이 없다.**

## 1. 만든 것

**C1. 등록부 버전 2와 순수 검증기.** `providers`(id·종류 `gitlab`·`base_url`·`ssh_url`·`token_ref`)·`orphans`(최대 100, 값 없음)·프로젝트의 `provider`/`gitlab.project_id`. 버전 1 파일은 첫 시작에서 원자적으로 2로 올라가고 내용은 그대로다. 서명 동작 4개(`set_provider`·`delete_provider`·`create_group`·`create_project`)와 이름·그룹 경로·설명·URL 검증기(`internal/registrymsg`).
**C2. GitLab 클라이언트.** 코드에 고정된 9개 (메서드, 경로) 허용 목록, 삭제·변경·병합·토큰 호출 없음, 제공자 주소 규칙(SSRF: 루프백·링크 로컬·예약·메타데이터 주소 거부, 연결 시점 재검사, 리다이렉트 안 따름), 토큰은 `PRIVATE-TOKEN` 헤더로만, GitLab 응답 문장은 읽지도 반영하지도 않음.
**C3. broker op.** `providers`·`list_groups`(읽기)와 서명 필요한 `set_provider`·`delete_provider`·`create_group`·`create_project`. `create_project`는 그룹 확인 → 이름 충돌 확인(인수 금지) → 프로젝트 → 첫 파일들(파일 API) → 기본 브랜치 확인 → 보호 브랜치 → 배포 키 → **마지막에** 등록부. 중간 실패는 지우지 않고 고아 기록 + `create_partial`.
**C4. orchd.** 제공자·그룹 읽기, 제안·적용 엔드포인트(서명 메시지·미리보기·배포 키 해시·한도·원장), 등록부 항목만 보고 하는 복제(`internal/orch/clone`: 고정 안전 옵션, 임시 폴더 후 이름 바꾸기), 게시 원격 해석기(`publish.RemoteResolver`).
**C5. 웹.** `#/providers`, `#/projects`의 "새 프로젝트 만들기"(그룹 선택·생성, 확인 화면에서 브라우저가 `change`를 다시 계산), 단계별 결과·`create_partial` 안내·"다시 복제"·고아 배지.
**C6(이 작업).** 적대 시험 24개(broker 14 + orchd 9 + clone 1), 변이 13종 확인, 이 문서, 런북 14장, 문서 19·`infra/README.md` 갱신.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-11, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0. 세는 방식은 W3a·W3b와 같다: 최상위 시험 함수와 하위 시험을 따로 센다(`go test -json` 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**, 표준 오류 출력 없음. 최상위 시험 함수 1470개 중 **1468 통과, 0 실패, 2 건너뜀**(하위 시험 935개 모두 통과). 건너뜀 2개는 W1~W3b와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollectionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사).
- `node --test "internal/orch/web/test/*.test.js"`: **229개 전부 통과**(W3b 194개 + C5 35개).
- **게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, 하위 `tokens`·`providers` 모두 PASS, **`SKIP` 없음**.
- W3b(23번 문서) 대비 최상위 통과 1297 → 1468(+171), 하위 시험 833 → 935(+102). C6가 더한 것은 최상위 24개(broker 14, orchd api 9, clone 1).

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 13 | 0 | 0 | 0 |
| `cmd/orchd` | 33 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 67 | 0 | 0 | 76 |
| `internal/broker` | 278 | 0 | 0 | 231 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 45 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 328 | 0 | 0 | 33 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 10 | 0 | 0 | 0 |
| `internal/orch/clone` | 24 | 0 | 0 | 0 |
| `internal/orch/knownhosts` | 4 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 23 | 0 | 0 | 0 |
| `internal/orch/refine` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 43 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 11 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/registrymsg` | 23 | 0 | 0 | 4 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

### C6가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3c_test.go`(14개), orchd `internal/orch/api/adversarial_w3c_test.go`(9개: 이름이 겹친 것은 `…W3c` 접미사), clone `internal/orch/clone/adversarial_w3c_test.go`(1개). 이미 같은 일을 하는 W3c 시험(C2~C5)은 복사하지 않고 **전체 흐름 위의 시험**으로 새로 썼다(아래 "하는 일").

| 브리프의 시험 | 위치 | 하는 일 |
|---|---|---|
| `TestAdminTokenNeverLeavesBroker` | broker | 읽기·쓰기 흐름 20가지(성공, 모든 단계 실패, GitLab이 토큰을 모든 오류 본문에 되풀이하는 경우, 거부 등)를 **헤더를 기록하는 앞단**을 거쳐 실행. 응답(원본 바이트)·로그·상태 폴더의 모든 파일(토큰 저장소 제외)·GitLab이 받은 URL과 본문·`PRIVATE-TOKEN` 아닌 모든 헤더에 토큰이 없음, `PRIVATE-TOKEN`은 정확히 토큰, 실제 경로의 패닉(값이 토큰), 깨진 요청 |
| `TestEveryGitLabRequestIsAllowlisted` | broker | 모든 흐름의 GitLab 요청 전체가 허용 목록 부분집합(메서드·경로·쿼리 키), `DELETE/PUT/PATCH/HEAD` 없음, 금지 경로 조각(merge·access_tokens·user·commits·enable …) 없음, **POST 본문 키가 문서화된 것뿐**(설명에 `"},"namespace_id":999…`를 넣어도 구조 불변), 허용 목록 9행이 모두 쓰임. 발송 기본 동작에 파괴적·토큰 모양 요청 9개를 직접 줘도 네트워크 0. 적대적 그룹 경로·이름·템플릿은 GitLab 요청 0. 코드에 `http.MethodDelete/Put/Patch`·`"DELETE"` 문자열 없음 |
| `TestSSRFProviderAddressesRefused` | broker | **운영 주소 정책**(시임 없음)에서 `set_provider`가 루프백·`[::1]`·`0.0.0.0`·`localhost`·메타데이터·링크 로컬·IPv4 매핑·NAT64·6to4·10진/16진/8진·예약·http 공개 주소·`userinfo`·경로·쿼리 등 약 55개 `base_url`과 19개 `ssh_url`을 거부(서명 가능한 25개 이상은 주소 정책이 거부), 이름이 금지 주소로 풀리면 거부, 한 번 통과한 이름이 **연결 시점**에 바뀌어도(재바인딩) 거부, 루프백 함정 서버 접속 0 |
| `TestRedirectToOtherHostNotFollowedAndTokenNotSent` | broker | 301·302·303·307·308을 모든 요청 또는 쓰기에만 → 다른 호스트가 받은 요청 0(토큰 헤더 0), 성공으로 끝난 리다이렉트 없음, 프로젝트 등록 안 됨 |
| `TestProviderBaseURLChangeNeedsSignatureAndShowsInChange` | broker | 같은 제공자 id의 주소를 바꾸는 요청: 서명 없음·옛 주소 서명·`change`만 다시 계산·ssh 주소·토큰 이름·종류 변경 모두 거부, 변경 전 토큰은 옛 GitLab에만 감, `change` 해시와 정규 변경문에 새 주소가 들어 있음, 정직한 서명은 한 번만 통과 |
| `TestSignatureOverDifferentProjectFieldsRefused` | broker | `create_project` 서명을 필드 하나만 바꿔 재사용(7개 필드 + 이름) → 거부(`change`를 다시 계산하면 `bad_signature`), GitLab 요청 0·등록부 불변·ID 미소모 |
| `TestCrossActionSignaturesRefused` | broker | **8개 서명 동작의 모든 순서쌍 56개** × (서명만 / 서명+`change`+요청 ID+시각) → 전부 거부, 등록부·토큰 저장소·사용한 ID·GitLab 불변 |
| `TestDeployKeySwapRefused` | broker | 다른 키·주석 변경·공백·개행·두 번째 키 주입·옵션 접두·빈 값·대문자·보이지 않는 문자·매우 긴 값·다른 키의 해시 → 서명 전에 거부, GitLab 요청 0, ID 미소모. 정직한 쌍은 GitLab이 정확히 그 키를 받음 |
| `TestExistingGitLabProjectNeverAdopted` | broker | 최상위·하위 그룹에 이미 있음, 확인과 POST 사이에 생김(409), 확인 호출 자체가 401·403·500·502, 등록부 충돌 → 어느 경우에도 파일·보호·배포 키 호출 없음, 등록 없음 |
| `TestPartialFailureNeverDeletes` | broker | 5개 단계 실패 + 패닉 + 등록부 쓰기 실패: `DELETE/PUT/PATCH` 없음, 모든 POST가 정확히 한 번(재시도 없음), 고아 기록에 실제 단계, 등록 안 됨 |
| `TestConcurrentSameNameNoDoubleCreate` | broker | 12개 동시 요청(요청 ID 다름·그룹 둘·같은 서명 두 번) → 성공 ≤ 1, `POST /projects` ≤ 1, 등록부 항목 ≤ 1 |
| `TestRegistryV2BrokenFailsClosedAndDoesNotWidenAIContext` | broker | 깨진 버전 2 파일 15종(잘림·쓰레기·빈 파일·문자열 버전·`version`/`Version` 중복·알 수 없는 키·제공자 중복·모르는 제공자·자동 병합 켜짐·나쁜 고아·제공자 없는 `project_id`·버전 3·뒤따르는 데이터 …): 모든 읽기·쓰기·`refine`이 `registry_unavailable`, AI 전송 경로 0(옛 `context_repos`로 폴백하지 않음), 가짜 OpenAI 호출 0, 파일 불변. 유효한 버전 2 대조 |
| `TestStatusAndProjectsNoSecrets` | broker | `status`·`projects`·`providers`·`tokens`·`list_groups`와 쓰기 오류 응답·로그에 토큰·OpenAI 키·파일 경로·서명자 줄·요청 ID·`change`·배포 키 줄이 없음, 고아는 값만 |
| `TestCreateProjectBrokerDoesNotStatPaths` | broker | 새 프로젝트 경로에 심볼릭 링크·일반 파일이 있어도, 저장소 루트가 통째로 없어도 `create_project`가 똑같이 성공하고 파일 시스템 불변(루트를 만들지 않음). W3c broker 파일이 `os`·`path/filepath`·`os/exec`를 import하지 않음 |
| `TestCloneNeverUsesRequestSuppliedURL` | orchd | 본문(10종)·쿼리(4종)·헤더(2종)의 `url`/`remote`/`ssh_url`/옵션/git 설정 80요청: 복제 옵션이 항상 등록부에서 만든 값, broker의 `create_project` 응답이 evil `ssh_url`을 말해도 자동 복제는 제공자 값, 등록 경로가 루트 밖이면 복제 안 함, 생성 요청의 위조 필드 8종은 복제·broker 쓰기 0 |
| `TestCloneOptionInjectionRefused` | orchd | **실제 `clone.Clone`**에 `-o…`·`--upload-pack=`·`ext::`·`file://`·scp 형식·https·다른 사용자·옵션 모양 호스트·경로 등 ~35개 원격, 호스트 고정 위반 4개, 17개 대상 경로, 키·known_hosts·ssh 경로 14개 변형 → 모두 거부, 기록용 `git`이 한 번도 실행되지 않음·루트 아래 아무것도 안 생김. API: 위조 등록부 행 25개(프로젝트 경로·제공자 ssh 주소)는 복제 도우미까지 가지 못함 |
| `TestCloneHookNotExecuted` | **clone 패키지** | 로컬 파일 전송 시임이 필요해 clone 패키지에 있다. **실제 `git`**으로 훅을 여러 모양으로 담은 저장소(추적되는 `hooks/`·`.githooks/`·`.husky/`, `.gitattributes`의 필터, `.gitmodules`, bare 원격의 서버 훅)와 적대적 템플릿 디렉터리(훅·`reference-transaction`·`core.fsmonitor`/`core.hooksPath`를 스크립트로 가리키는 `config`)를 복제 → 실행된 스크립트 0, 서브모듈 안 가져옴. **대조:** `core.hooksPath` 차단만 빼면 훅이 실행됨 |
| `TestResolverNeverTrustsRequestData` | orchd | 해석기가 아는 것은 등록부와 고정 호스트 키 파일뿐: 저장소 문자열(URL·옵션·`..`·대소문자·NUL·공백·부모 폴더·하위 폴더)은 원격이 되지 않음, 호스트 미고정·이름 불일치 행은 "거부", broker가 없고 표가 낡으면 아는 저장소도 모르는 저장소도 "거부" |
| `TestApplyForgedFieldsRefusedBeforeBrokerW3c` | orchd | 4개 동작 × 위조 9종(여분 키, 여분 필드, 대소문자 주체, 시각·요청 ID 모양, 다른 동작, 서명 여분 필드, 다른 `change`): 거부, **broker 호출 0**. 클라이언트가 보낸 `deploy_key_sha256`은 제안에서 거부, 제안의 해시는 `.pub`의 해시, `.pub` 교체·정책 클래스 없음은 broker 호출 없이 거부 |
| `TestProposeNeverWritesBrokerW3c` | orchd | 4개 동작 × 4회 × (정상 + 위조 + 깨진 본문): broker 호출 0, 원장 0, 복제 0, 등록부 변경 알림 0, 저장소 루트·키·고정 파일 불변 |
| `TestProvidersLedgerNoSecrets` | orchd | 성공 + 적대적 broker 오류 문장 6종 × 4개 동작 + 복제 실패 후: 원장과 프로세스 로그에 설명 글·`.pub`·해시·요청 ID·`change`·메시지·서명·키 경로·주소·토큰 이름·그룹 경로·broker 문장이 없음 |
| `TestOrchdWorksWithBrokerDownW3c` | orchd | 소켓 없음·아무도 안 듣는 소켓: W3c 읽기·적용·복제는 고정 `broker_unavailable`(소켓 이름·연결 문장 없음), 제안은 동작, 복제 0, 정책·프로젝트·작업·제출은 그대로 |
| `TestNoBrokerImportInOrchdW3c` | orchd | `go list -deps ./cmd/orchd`의 모든 `aigate/` 패키지: `internal/broker` 없음, 그룹·프로젝트·배포 키·보호 브랜치 REST 경로·주소 정책·메타데이터 주소가 소스에 없음, 토큰 헤더는 옛 `publish`의 초안 MR 코드(W3c 이전, 자체 플래그)에만 있음 |
| (추가) `TestProviderBaseURLChangeShownInPreview` | orchd | 제공자 주소 변경 제안의 `fields`·미리보기에 새 주소, `change`가 옛 제안과 다름, 서명할 메시지가 `change`를 담음 |

**이름 충돌 처리.** `TestNoBrokerImportInOrchd`·`TestOrchdWorksWithBrokerDown`(W3a·W2b)·`TestProposeNeverWritesBroker`·`TestApplyForgedFieldsRefusedBeforeBroker`(W3a)는 같은 패키지에 같은 이름이 이미 있어서 W3c 범위는 `…W3c` 접미사로 새로 썼다(기존 시험은 그대로). `TestCloneHookNotExecuted`는 clone 패키지의 시임이 필요해 그쪽 파일에 있다.

**시험을 새로 쓰는 중 발견한 것.** **제품 코드의 결함은 발견하지 못했다**(적대 시험이 첫 실행에서 시험 작성 실수 외에 실패한 것이 없다). 관찰 몇 가지: ① 등록부를 읽을 때(`validateRegistry`)는 제공자 주소의 **모양**만 보고 주소 정책(루프백 등)과 프로젝트 경로의 `repo_roots` 소속은 다시 보지 않는다. 파일은 broker만 쓰고(0600, 서명 뒤) 연결 시점에 주소가 다시 검사되므로 받아들였다. ② 복제하는 호스트의 템플릿 디렉터리 `config`가 필터 드라이버를 정의하면 그 필터는 실행된다(`core.hooksPath`·`core.fsmonitor`는 명령줄이 이긴다). 운영에서는 `GIT_TEMPLATE_DIR`가 환경에서 지워지고 전역·시스템 설정이 꺼져 있으며 시스템 템플릿은 root 소유라 저장소 내용으로는 닿지 않는다. 방어를 더 두려면 `git clone --template=`(빈 값)을 쓸 수 있으나 고치지 않았다. ③ `orchd`의 `internal/orch/publish`에는 W3c 이전의 초안 MR 호출(`PRIVATE-TOKEN`, 자체 `--gitlab-*` 플래그)이 남아 있다(W3d에서 broker로 옮긴다). ④ orchd는 서명의 `issued`·요청 ID를 판단하지 않고 broker가 거부하게 한다(W3b 설계).

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖(`/tmp/aigate-mutation/copy-N`)으로 복사**(`rsync`, `.git`·`.superpowers`·`graphify-out` 제외)해 한 곳만 망가뜨리고 관련 패키지(`go test`, 웹은 `node --test`)를 돌렸다. 컴파일되게 `false &&`·`error(nil)` 형태로 바꿨다. 변이마다 복사본을 새로 만들었고 작업 트리는 건드리지 않았다. 전부 구현자 실행이다(검토자가 아직 재현하지 않음).

| # | 망가뜨린 곳 | 실패한 시험 (굵게 = C6의 새 시험) |
|---|---|---|
| ① | 서명 검증 제거(`VerifyRegistry` 결과를 항상 성공으로) | TestRegistryNeverWrittenWithoutValidSignature, TestSignatureOverDifferentChangeRefused, TestSSHNamespaceMismatchRefused, TestApprovalMessageCannotAuthorizeRegistryChange, TestSignerRemovedTakesEffectImmediately, TestSetTokenForOtherRefOrKindRefused, TestApprovalAndRegistryMessagesStillSeparate, **TestProviderBaseURLChangeNeedsSignatureAndShowsInChange**, **TestSignatureOverDifferentProjectFieldsRefused**, **TestCrossActionSignaturesRefused**, TestRegisterRequiresValidSignature, TestSignerRemovalTakesEffectImmediately, TestInvalidSignatureDoesNotConsumeRequestID, TestSetFlagAIContextOnlyAndSigned … (전부 21개) (+하위 시험 6개) |
| ② | `change` 일치 검사 제거(`signed != want`) | TestRegistryNeverWrittenWithoutValidSignature, TestSignatureOverDifferentChangeRefused, TestValueSwapAfterSignatureRefused, TestSetTokenForOtherRefOrKindRefused, **TestProviderBaseURLChangeNeedsSignatureAndShowsInChange**, **TestSignatureOverDifferentProjectFieldsRefused**, TestRegisterChangeMismatchRefused, TestInvalidSignatureDoesNotConsumeRequestID, TestWriteOpsNeverChangeRegistryOnAnyFailure, TestChangeRequiredAndMismatchBurnsNoID, TestSetProviderRules, TestSetTokenFieldsMustMatchSignedChange (+하위 시험 8개) |
| ③ | 허용 목록 검사 제거(`do`가 아무 경로나 보냄) | **TestEveryGitLabRequestIsAllowlisted**, TestDoRefusesOffListRequests |
| ④ | 주소 검증의 IP 거부 제거(`ipForbidden` 항상 false) | **TestSSRFProviderAddressesRefused**, TestIPRulesFix1, TestDialPolicyRefusalIsBadURL, TestProviderURLRules, TestIPRules, TestSSHURLRules, TestDialTimeIPRecheck, TestCheckProviderResolves, TestProviderStoreRoundTrip, TestShapeErrorsBurnNoID, TestSetProviderHostNameMustResolveToAllowedAddress |
| ⑤ | 리다이렉트 허용(`CheckRedirect` 제거) | **TestRedirectToOtherHostNotFollowedAndTokenNotSent**, TestNoRedirectFollowedProviderAPI, TestStatusMapping |
| ⑥ | 기존 GitLab 프로젝트 인수(이름 충돌 `project_name_taken` 무시) | **TestExistingGitLabProjectNeverAdopted**, TestCreateProjectNameTaken, TestPartialFailureRecordsOrphan, TestQuotaSpentOnlyBeforeFirstWrite (+하위 시험 6개) |
| ⑦ | 부분 실패 때 고아 기록 안 함(`AddOrphan`이 아무것도 안 함) | **TestPartialFailureNeverDeletes**, **TestStatusAndProjectsNoSecrets**, TestPartialFailureRecordsOrphan, TestUncertainAfterWriteAlwaysOrphan, TestRegistryWriteFailureAfterGitLabIsOrphan, TestCanaryTokenNeverInAnyOutput, TestTimeoutIsUnknownOutcome, TestPanicAfterProjectWriteLeavesOrphan, TestProjectsReadListsOrphans, TestStoreAddOrphanLogsFixedLineOnDrop (+하위 시험 13개) |
| ⑧ | `deploy_key_sha256` 비교 제거 | **TestDeployKeySwapRefused**, TestCreateProjectBindsDeployKey, TestPathAndKeyMismatchBurnNoID |
| ⑨ | 복제 옵션에서 `protocol.allow=never` 제거 | TestCloneUsesFixedSafeOptions, TestCloneProductionRefusesFileTransport |
| ⑩ | `core.hooksPath=/dev/null` 제거(훅 비활성 제거) | **TestCloneHookNotExecuted**, TestCloneUsesFixedSafeOptions, TestCloneRealGitLocalFixture |
| ⑪ | 해석기가 호스트 고정 검사를 건너뜀(`RemotePinned`) | TestResolverRequiresPinnedHost, **TestResolverNeverTrustsRequestData** |
| ⑫ | 요청 본문의 URL을 복제에 사용 | **TestCloneNeverUsesRequestSuppliedURL**, TestCloneRemoteOnlyFromRegistry |
| ⑬ | 웹 `change` 비교 제거(`providers.js`) | TestSignOnlyWhenChangeMatches (node, `providers.test.js`; the first runner parsed TAP wrongly, re-run by hand) |

**13개 모두 잡혔다.** 살아남은 변이는 없다. 참고: ③은 처음 구현에서 C2의 `TestDoRefusesOffListRequests` 하나만 잡았다(공개 메서드는 허용 목록 밖 요청을 만들 수 없어서 `TestEveryGitLabRequestIsAllowlisted`가 못 잡음) → 발송 기본 동작에 파괴적·토큰 모양 요청을 직접 주는 단계를 그 시험에 더하고 다시 돌려 이제 두 시험이 잡는다(위 표는 다시 돌린 결과). ⑬은 처음 실행기가 node의 출력 형식을 잘못 읽어 "안 잡힘"으로 기록했고 손으로 다시 돌려 확인했다. ⑨·⑩·⑪·⑫는 clone·api(⑪은 publish도) 패키지를 돌렸고 이 표의 시험은 그 안에서 실패한 것이다. 솔직히: ⑨(`protocol.allow=never`)는 **C6의 새 시험이 잡지 못하고** C4의 `TestCloneUsesFixedSafeOptions`·`TestCloneProductionRefusesFileTransport`만 잡는다(운영 `clone.Clone`은 로컬 경로를 어차피 거부하는 조합이라 새 시험의 입력 거부와 겹치지 않는다). 변이는 구현자가 정한 한 곳씩이라 같은 결함의 다른 모양은 확인하지 않았다.

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

C1~C5가 각자 적은 표를 모았다. **이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험에 기초한 것이고 실서버에서 본 것이 아니다.** 컷오버 전 확인 목록은 런북 14.0. **실행 사용자와 샌드박스(변경 없음).** broker: `orch-broker`(uid 999), `ProtectSystem=strict`, **`ProtectHome=yes`(`/home`을 보지 못한다)**, `PrivateTmp`, `NoNewPrivileges`, `CapabilityBoundingSet=` 비어 있음, `MemoryDenyWriteExecute`, `SystemCallFilter=@system-service`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`, 쓰기는 `/var/lib/orch-broker`·`/run/orch-broker`만. orchd: `orch`, `ProtectSystem=strict`, `ProtectHome=read-only`, `ReadWritePaths=/home/orch/orchd /home/orch/repos`, `PrivateTmp=yes`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`(`infra/systemd/*.service`, 바뀌지 않음).

| 경로·대상 | 소유자 / 모드 | 접근하는 쪽 | 하는 일 | 샌드박스·보호 규칙 |
|---|---|---|---|---|
| `/var/lib/orch-broker/registry.json` | `orch-broker` 0600(폴더 0700) | broker만 | **버전 1 → 2 마이그레이션(첫 시작에서 원자적 쓰기)**, 제공자·고아·프로젝트 추가(임시 파일 → rename) | `ReadWritePaths`. **옛 broker는 버전 2를 깨짐으로 봄 → 롤백은 등록부 복원** |
| `/var/lib/orch-broker/{tokens.json, secrets/<이름>}` | 0600 / 0400(폴더 0700) | broker | **호출마다 읽기만**(GitLab 호출 때 `gitlab` 종류 토큰). 값은 `PRIVATE-TOKEN` 헤더로만 나감 | 변경 없음 |
| `/var/lib/orch-broker/used-requests.json` | 0600 | broker | W3c 서명 요청 ID도 기록 | 변경 없음 |
| 제공자 주소 `http://100.64.0.10:18082` | — | **broker**(uid `orch-broker`) | HTTP 호출(그룹·프로젝트·파일·보호·배포 키), 연결 시점 주소 재검사, 리다이렉트 안 따름, 연결 10초·전체 15초·본문 1 MiB | nft가 허용해야 함(**다른 주소는 새 규칙, 방화벽은 바꾸지 않음**) |
| `/home/orch/repos/<이름>` | 예상 `orch:orch 0700`, 일반 폴더 | **orchd**(`orch`): `os.Mkdir` 뒤 git. **broker는 보지도 않는다**(`/home`이 안 보임) | 새 프로젝트의 복제 위치(`repo_roots[0]` 바로 아래 한 단계) | `ReadWritePaths=/home/orch/repos`. **broker `repo_roots[0]`이 `/home/orch/repos`여야 함**, 경로에 심볼릭 링크 없음. **실서버 쓰기 가능성 미확인** |
| `/home/orch/repos/.clone-<16자 hex>`(일시적) | `orch` 0700 | orchd | 복제 도중의 임시 폴더, 끝나면 이름을 바꿔 들임(대상이 생겼으면 덮지 않음). **시작 때 남은 것을 지움** | 같음 |
| `/home/orch/orchd/ssh/id_gitlab_deploy` | `orch` 0600 | `ssh` 자식 프로세스만 | `-i`로 지정. **orchd 코드는 열지 않는다**(시험) | 변경 없음 |
| `/home/orch/orchd/ssh/id_gitlab_deploy.pub` | `orch` 0644, 일반 파일(링크 아님), 한 줄, 주석 ≤ 100자·공백 없음 | orchd | 제안 때 해시, 적용 때 다시 읽어 해시 비교, 한 줄을 broker에 전달 | 읽기 전용 접근 |
| `/home/orch/orchd/ssh/known_hosts_gitlab` | `orch`, 그룹·기타 쓰기 불가 | orchd(읽기), `ssh`(읽기, `UpdateHostKeys=no`) | **고정 호스트**: 평문·`[host]:port`·해시 항목만 인정(와일드카드·`@cert-authority` 아님) | 변경 없음 |
| `git`·`ssh` 실행 파일 | `/usr/bin` 등 | orchd | 시작 때 `/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin`에서만 찾아 절대 경로 고정(환경 변수 안 봄). 없으면 `git_missing`·`ssh_missing`(생성은 성공) | — |
| 임시 폴더 `orch-clone-*` | `orch` 0700 | orchd | 복제 자식의 `HOME`, 끝나면 지움 | `PrivateTmp=yes` |
| orchd 원장 | 기존 | orchd | 새 이벤트 `provider_change`·`project_create`·`project_clone`(키 `action,actor,code,ok,subject` / `actor,code,ok,subject`) | 기존과 같음 |
| `policy.yml` | root/관리자 | orchd(작업 시작 때 읽음) | **클래스 `registered`가 있어야** 제안 가능(없으면 `policy_class_missing`). **추가하면 정책 해시가 바뀜** | 사용자 결정 |
| `/root/backup-w3c` | root 0700 | 사람(root) | 롤백용 바이너리·`registry.json`(버전 1)·정책 복사 | — |
| 웹 정적 파일 | orchd 바이너리에 내장 | orchd | 새 `views/providers.js` 등(경로·모드 변경 없음) | — |

새 유닛·새 플래그·새 설정 키·새 포트·새 방화벽 규칙·DB 스키마 변경(v8 그대로)은 없다. **실서버에서만 알 수 있는 것은 6장.**

## 5. 설계서와 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| W3c-1 | **첫 커밋은 커밋 API가 아니라 파일 API로 파일마다**(`README.md`가 먼저 `main`을 만든다). 빈 프로젝트에 커밋 API가 404라는 GitLab 이슈 #577329 때문. 원자적이지 않고 파일 수만큼 커밋이 생긴다. 실서버 확인은 컷오버뿐 | 중간 실패 시 일부 파일만 있는 프로젝트(`create_partial` 단계 `first_commit`) |
| W3c-2 | **`default_branch`를 보내지 않는다**(문서상 `initialize_with_readme`가 필요). 첫 파일 뒤 프로젝트를 읽어 기본 브랜치가 `main`이 아니면 `create_partial`(단계 `default_branch`) | 가정이 틀리면 모든 생성이 이 단계에서 끝남 |
| W3c-3 | **복제 상태는 등록부가 아니라 orchd가 파일 시스템을 보고 계산**(`cloned`). 등록부의 모든 쓰기를 서명으로 유지하려고 서명 없는 `mark_ready` op를 만들지 않음 | 폴더를 사람이 지우면 "복제 안 됨"이 됨(다시 복제 가능) |
| W3c-4 | **게시 원격 해석기**(`publish.RemoteResolver`): 정적 `--git-remote-for`가 우선, 그 밖은 등록부(30초 캐시, 변경 직후 무효화)의 제공자 ssh 주소 + 프로젝트로 만들고 **고정 호스트일 때만**. `("", true)`는 "거부". **등록부를 읽지 못하고 표가 낡으면 모르는 저장소도 "거부"**(기본 원격으로 다른 프로젝트의 커밋을 밀어 넣는 일을 막으려는 fail closed) | broker가 꺼진 동안·변경 직후 `--git-remote`만 쓰는 저장소의 게시도 실패(재시도 가능) |
| W3c-5 | **등록부 버전 2**(롤백 때 등록부 복원 필요, 런북 ⚠ 2). 옛 broker가 버전 2를 깨짐으로 봄은 의도적 | 롤백 절차가 늘어남 |
| W3c-6 | **부분 실패는 재개하지 않는다**: 고아 기록(최대 100개, 값 없음) + `create_partial`, 같은 이름은 GitLab에 있는 동안 `project_name_taken`. **GitLab 쪽 삭제는 이 도구에 없다** | 사용자가 GitLab에서 정리 |
| W3c-7 | **시간 제한 사슬:** broker 60초 < orchd→broker 70초 < 웹 130초. 생성 적용(+자동 복제)은 최대 약 90초 | 앞단 프록시 제한이 더 짧으면 서버는 끝냈는데 화면은 "결과를 모름" |
| W3c-8 | **주체별 한도**(orchd): 생성 5/h, 그룹 5/h, 제공자 쓰기 10/h(한 통), 그룹 목록 60/h, 복제 10/h, 제안 60/h. broker에도 전체 상한(생성 10/h). 서명이 유효하면 요청 ID는 소모되고 한도는 **첫 GitLab 쓰기 직전**에만 소모 | 조정 한 곳 |
| W3c-9 | **제공자 주소 규칙:** `http`는 사설·tailnet IP 리터럴에만, 그 밖은 `https`, 소문자 정규형 호스트만(10진·16진 IP 표기 거부), 이름은 설정 시와 **연결 시점 모두** 확인(재바인딩), 이미 프로젝트가 참조하는 제공자는 주소·ssh 주소 변경 불가 | 정당한 별칭 주소를 쓸 수 없음 |
| W3c-10 | **이름 규칙에 GitLab 예약어 목록을 두지 않는다**(검토에서 제거): GitLab이 권한이고 거부는 `gitlab_refused` | 예약어 이름은 서명 뒤에야 거부됨 |
| W3c-11 | **웹이 Go보다 엄격**한 곳(설명의 보이지 않는 글자 등): 화면이 `findHostile`까지 거부 | 화면은 받지 않는 글을 broker는 받음(해롭지 않음) |
| W3c-12 | **복제 도구 고정:** `UpdateHostKeys=no`, 임시 폴더 후 이름 바꾸기, `git`·`ssh` 절대 경로 시작 때 고정(`git_missing`·`ssh_missing`), 복제 한도·원장 | — |
| W3c-13 | **템플릿(`basic`)의 `checks`는 빈 목록**(파서 통과). 빈 목록이 "통과"로 읽히는지는 확인하지 않음. W3c에는 자동 병합이 없다(항상 꺼짐) → **W3d는 빈 `checks`를 통과로 세지 않아야 한다**(이월) | W3d에서 잘못 읽으면 검사 없는 프로젝트가 자동 병합될 수 있음 |
| W3c-14 | **배포 키 재사용 미확인:** 같은 `.pub`을 여러 프로젝트에 등록할 수 있는지 GitLab에 달려 있다. 안 되면 허용 목록 밖의 호출(키를 프로젝트에 켜기)이 필요 → 컷오버 결정 | 두 번째 이후 프로젝트가 `create_partial`(단계 `deploy_key`) |
| W3c-15 | **`known_hosts` 고정은 정확한 항목만**(평문·`[host]:port`·해시). 와일드카드·`@cert-authority` 줄만 있으면 `host_not_pinned` | 파일을 고쳐야 함 |

## 6. 실서버에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 14장)에서 처음 확인한다.**
- **실제 GitLab 19.0.8에서:** `POST /groups`·`POST /projects`, **빈 저장소에 파일 API로 첫 파일이 `main`을 만들고 기본 브랜치가 되는지**, 보호 브랜치(`main`의 기존 규칙 유무, `POST`의 상태 코드, Maintainer 푸시·병합·강제 푸시 금지), 배포 키 `POST`(201 가정)와 **같은 공개 키를 두 번째 프로젝트에 등록할 수 있는지**, `min_access_level=40` 그룹 목록, 예약어 이름의 거부 방식, **관리자 토큰의 권한**(범위 `api`, 그룹·프로젝트 생성 권한). 시험은 가짜 서버로 요청 모양만 확인했다.
- **orchd가 `/home/orch/repos`에 새 폴더를 만들 수 있는지**(소유자 `orch`, `ReadWritePaths`, 임시 폴더, `.clone-*` 이름 바꾸기), `git`·`ssh` 실행 파일과 `known_hosts_gitlab`의 실제 내용(정확한 호스트 항목), 샌드박스 안에서의 `git clone`.
- **배포 키로 새 프로젝트를 실제 `git clone`할 수 있는지(읽기 키·쓰기 키), 쓰기 키로 `push`가 되는지**, 게시 해석기가 만든 원격으로의 실제 push. 시험은 가짜 `git`과 로컬 bare 저장소(파일 전송 시임)였다.
- **broker가 GitLab HTTP에 닿는지**(nft의 `100.64.0.10:18082`), 실서버에서 연결 시점 주소 검사가 사설·tailnet 주소를 허용하는지.
- **등록부 v1→v2 마이그레이션**을 실서버 파일·uid·샌드박스로(임시 폴더에서만 확인), 롤백(등록부 복원)의 실제 영향, 샌드박스 안 `-check`의 제공자 점검.
- **`basic`·`go` 템플릿이 실제 게이트를 통과하는지**(시험은 설정 파서 통과와 `go` 템플릿의 임시 폴더 `go vet && go test`까지), 빈 `checks`의 판정.
- **실제 패스키 서명**을 broker가 `set_provider`·`create_group`·`create_project`에 대해 검증하는지(W3a는 `set_flag`만, W3b의 `set_token`은 컷오버에서 하지 않았다). 관리자 토큰 등록이 그 첫 실제 `set_token`이다.
- **키보드만·스크린 리더**로의 `#/providers`·새 프로젝트 화면 사용(DOM 대역 시험만), 브라우저에서 제공자 화면에 비밀 입력칸이 없음을 눈으로 보는 것(코드·시험은 `type=password`·값 칸 없음을 고정). C5의 ego-browser 확인은 1280px과 일부 390px 화면이고 서명은 대역 패스키였다.
- **웹 응답 대기 130초와 앞단(역방향 프록시·tailnet)의 제한시간**이 실제로 맞는지.
- **리눅스에서의 파일·프로세스 동작**(`renameat2 RENAME_NOREPLACE`, 프로세스 그룹 종료, `O_NOFOLLOW`)은 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- W2b·W3a·W3b에서 이월된 미확인(실제 MR, 한도·예산 초과 실동작, 실제 OpenAI 키 교체, 브라우저의 비밀번호 관리자 반응)은 각 결과 문서 그대로다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **이 도구는 GitLab의 그룹·프로젝트를 지우지 못하고 등록부에서 프로젝트를 지우는 op도 없다.** 시험용 리소스는 사람이 GitLab에서 지우고 등록부 항목·폴더는 남는다.
2. **첫 커밋이 원자적이지 않다**(W3c-1). 부분 실패 프로젝트는 사람이 정리한다.
3. **고아 기록은 실패를 본 뒤에야 쓰인다.** POST와 기록 사이에 broker가 죽으면 기록 없는 GitLab 프로젝트가 남는다(이름은 GitLab 확인으로 계속 `project_name_taken`). `project` 단계의 고아는 실제로 없는 프로젝트를 가리킬 수 있다("불확실").
4. **관리자 토큰은 `api` 범위 전권이다**: 코드는 9개 호출만 보내지만 유출되면 GitLab에서 그 이상이 가능하다. 서명된 변경과 만료일·폐기가 방어다. 사설·tailnet 주소가 허용되므로 서명을 받는 사람이 제공자를 tailnet의 다른 노드로 돌릴 수는 있다(주소가 서명된 변경에 들어 있어 확인 화면에 보인다).
5. **등록부 읽기는 제공자 주소의 모양만 다시 본다**(주소 정책은 연결 시점에 다시 적용). 고아 기록 정리 op가 없다.
6. **게시 해석기의 fail closed**: broker가 안 되거나 변경 직후에는 `--git-remote`만 쓰는 저장소의 게시도 실패한다(W3c-4).
7. **같은 배포 키 하나로 모든 새 프로젝트**(W3c-14). 읽기 키로 만든 프로젝트는 이 서버가 push하지 못한다(게시 권한 "읽기"의 의도).
8. **`basic` 템플릿은 검사가 없다**(W3c-13). W3c에서는 자동 병합이 꺼져 있어 해롭지 않다.
9. **orchd의 `publish`에 W3c 이전의 초안 MR 코드(`PRIVATE-TOKEN`)가 남아 있다**(옵트인, W3d에서 broker로).
10. **웹이 Go보다 엄격한 글자 규칙**(W3c-11), **세 곳 중복 규칙**(broker·orchd·브라우저)은 `TestConstantsAgreeWithWeb`와 벡터 시험이 지킨다.
11. **복제 템플릿 디렉터리는 신뢰한다**(2장 관찰 ②).

## 8. 사용자 결정 목록

컷오버 때 사용자가 정하거나 해야 하는 것(런북 14.0, 3단계):
1. **정책에 `registered: {level: L1}`를 넣을지**(정책 해시가 바뀌므로 실행 중 작업이 없을 때). L0로 두면 생성을 제안할 수 없다.
2. **GitLab 관리자 토큰**: 어느 사용자의 토큰으로, `api` 범위, **만료일**. 값은 **웹 토큰 화면에 직접 입력하고 채팅·터미널에 붙이지 않는다.** 먼저 13장 3-3의 입력칸 점검을 항목별로 한다.
3. **새 프로젝트를 만들 그룹**(새 전용 그룹 권장, 이름).
4. **실제 GitLab 리소스를 만들지 여부와 이름**(3-4): 만들면 사내 GitLab에 남고 도구는 지우지 못한다 → 시험용 이름을 쓰고 끝난 뒤 **사용자가 GitLab에서 직접 지운다**(등록부·폴더는 남는다). 게시 권한(읽기/쓰기)과 두 번째 프로젝트로 배포 키 재사용을 확인할지.
5. **작은 시험 작업을 새 프로젝트에 제출할지**(3-6, push 확인, 빈 `checks` 판정).
6. **배포 키를 GitLab이 프로젝트마다 거부하면** 어떻게 할지: 허용 목록에 "키를 프로젝트에 켜기" 한 줄을 더하는 후속 변경 / 프로젝트마다 다른 키(orchd가 키 하나뿐이라 큰 변경).
7. **컷오버가 안정되면 `/root/backup-w3c`를 언제 지울지**(토큰·키 사본이 들어 있어 `shred`), GitLab 관리자 토큰의 권한을 시험 뒤 줄일지·폐기할지.
8. **W3d(MR·조건부 자동 병합)로 갈지.** 그때 빈 `checks`를 통과로 세지 않는 규칙을 넣는다.
9. 앞단 프록시의 제한시간이 130초보다 짧다면 올릴지.

## 9. 컷오버 기록
아직 없음(컷오버 전). 컷오버를 하면 이 장에 서버에서 본 것만 적는다(21·23번 문서와 같은 방식).
