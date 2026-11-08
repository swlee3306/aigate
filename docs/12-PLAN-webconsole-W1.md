# 웹 콘솔 W1 구현 계획 (API와 인증 기반)

> **실행하는 에이전트에게:** 필수 서브스킬은 superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans다. 단계는 체크박스(`- [ ]`)로 추적한다.

**목표:** 공개 API v1, 서명 승인(웹 패스키·CLI SSH 키), 세션·HTTPS 접속, 웹 뼈대를 만들어, 웹과 CLI가 같은 API로 모든 기능(보안 승인 포함)을 쓰게 한다.
**구조:** `orchd` 하나가 API v1 핸들러를 유닉스 소켓과 `127.0.0.1` HTTP 리스너에 같이 붙이고(TLS는 `tailscale serve`), 승인은 root 소유 폴더의 서명자 목록으로 검증하는 서명만 인정한다. 검증은 데몬과 게이트(승인 파일을 읽는 쪽) 두 곳에서 한다.
**기술:** Go 1.27.1(모듈 `aigate`), net/http, modernc sqlite, `golang.org/x/crypto/ssh`(신규), 직접 구현한 최소 WebAuthn 검증기, 바닐라 JS + `node --test`.
**설계서:** `docs/11-DESIGN-webconsole-W1.md` (승인됨, 2026-11-09). 화면 기준은 `docs/web-mockup/index-v4.html`.
**커밋 트레일러:** 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## 계획 단계 결정 (rulings, 틀리면 비용이 작은 순)

| # | 결정 | 이유 | 틀렸을 때 비용 |
|---|---|---|---|
| R1 | 설계서 D1: WebAuthn은 **직접 최소 구현**(ES256, 증명 `none`, 검증만) | 라이브러리는 의존성이 많고, 우리는 단일 사용자·ES256만 필요. 합성 인증기로 전부 시험 가능 | `internal/webauthn`의 두 함수 뒤에 있어 라이브러리로 교체가 쉬움 |
| R2 | 설계서 D4: 관리 도구는 `orch admin signers add|list|remove|pending`(별도 바이너리 없음). 쓰기는 euid 0에서만 | 배포물 하나 유지 | 이름만 바뀜 |
| R3 | **서명자 목록 위치는 `/etc/aigate/signers.json`**(설계서 8.4를 이 계획 작성 중 정정). 폴더와 파일 모두 root 소유 | orch 소유 폴더 안의 root 소유 파일은 교체될 수 있다 | 경로 설정은 플래그라 바꾸기 쉬움 |
| R4 | `/ops`는 W1에서 정책·빌드·base 캐시·마지막 복원 기록만. **VM·쿼터 수치는 W2로** (OpenStack 호출 필요, 설계서 6장 표에서 줄임) | 읽기 API 범위를 줄여 W1 완료를 앞당김 | W2 첫 작업으로 추가 |
| R5 | 프로젝트는 W1에서 테이블 없이 **작업의 repo별 집계**로 보여 줌(생성은 W3) | 설계서 범위(읽기만) | W3에서 테이블 추가 |
| R6 | 배포는 `orchd`·`orch`·`aigate` 세 바이너리를 **같이** 한다 | 옛 게이트는 서명 없는 항목도 통과시킨다(서명 필드를 모름) | 컷오버 순서로 해결(E2) |

### 실행 전 점검에서 추가한 결정 (2026-11-09, 계획 충돌 해소)
| # | 결정 | 이유 |
|---|---|---|
| R7 | 서명자 목록 경로와 로드 옵션은 **프로세스 전역**: `signing.RegistryPath()`, `signing.SetRegistryPath(string)`, `var signing.DefaultOptions RegistryOptions`. `approval.Store`는 `Store{Path}` 그대로이고 `SignersPath` 필드가 없다. `api.Config`에 `Signers`·`SignersPath`를 두지 않는다 | `Store{Path}`를 만드는 호출자 6곳(게이트, 게시, 승인 확인 등)이 `--signers-file`을 모르면 데몬과 게이트가 다른 모드가 된다 |
| R8 | WebAuthn 서명자는 목록 항목에 `RPID`, `Origin`을 함께 고정한다. `VerifyOptions`에는 RPID/Origin이 없다. `signers add --pending`은 대기 자격의 값(`Config.RPID/Origin`으로 등록 시 기록)을 복사한다 | 승인 파일을 읽는 게이트에는 서버 설정이 없다. 목록(root 소유)이 신뢰의 뿌리다 |
| R9 | CLI 서명 이음새는 `var signCmd func(keyPath, namespace string, message []byte) (string, error)` | 로그인(`aigate-login`)과 승인(`aigate-approval`)이 같은 이음새를 쓴다 |
| R10 | 소켓 상대 uid는 빌드 태그 파일로 읽는다: Linux는 `syscall.GetsockoptUcred`, macOS는 `golang.org/x/sys/unix.GetsockoptXucred`. `x/sys`는 이미 `go.mod`에 있어 새 모듈이 아니다 | 개발은 Mac, 운영은 Linux |
| R11 | B7의 승인 상세 응답에 `truncated bool`(hashed_text가 잘렸는지)을 포함한다 | B8의 `TestApproveRefusesTruncatedDiff`가 필요로 함 |
| R12 | B7에 `TestKindConstantsMatchStore`(signing의 종류 상수 == `store.Kind*`)를 추가한다 | 두 패키지가 같은 문자열을 따로 정의함 |

## 전역 제약 (모든 작업에 적용)

- API 경로는 모두 `/api/v1` 아래. 오류는 `{"error":{"code","message"}}`. 사용자에게 보이는 메시지는 한국어.
- 읽기(GET) 엔드포인트는 부작용이 없다.
- 새 의존성은 `golang.org/x/crypto`뿐(WebAuthn 라이브러리 금지).
- 서명 메시지 형식은 설계서 8.1 그대로(`aigate-approval-v1`, 키 순서 고정). 서명 `issued` 허용 오차는 ±5분. 승인 파일 항목은 서명 시점 이후 만료되지 않는다(지금처럼).
- 서명자 목록(`/etc/aigate/signers.json`)은 파일과 상위 폴더가 모두 root 소유이고 그룹·기타 쓰기 불가일 때만 읽는다. 어긋나면 서명 승인을 모두 거부(fail closed)한다.
- 서명 필수 모드는 목록 파일의 존재로 켜진다. 모드에서 서명 없는 승인 항목은 `Store.Valid`가 false다.
- HTTP 리스너는 `127.0.0.1`에만 바인딩한다. 응답 헤더: CSP `default-src 'self'; frame-ancestors 'none'`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, API는 `Cache-Control: no-store`. 요청 본문 상한 1 MiB, diff 응답 상한 256 KiB, 보고서 응답 상한 2 MiB.
- 화면은 에이전트가 만든 글을 `textContent`로만 출력한다. `innerHTML`·`insertAdjacentHTML`·`document.write` 금지.
- 승인의 의미는 그대로다: 승인은 `(repo, base, hash)`에 묶인다.
- 시험 실행: `go vet ./...`, `go test ./... -count=1`, 동시성 관련은 `-race`.

## 검토 포인트 (스펙이 암시하지만 작업별 시험이 놓치기 쉬운 것)

1. **항목 A의 서명을 항목 B에 재사용** → 거부 (B1, B6, B7, E1).
2. **서명 필수 모드에서 `approvals.json`을 손으로 고쳐 서명 없는/위조 항목 추가** → 데몬과 게이트 모두 "승인 없음" (B6, E1).
3. **서명자 목록 변조·권한 오류·orch 소유 폴더** → 모두 거부, 승인 전부 무효 (B2, E1).
4. **프롬프트·diff·보고서에 `<img onerror=…>`, `</script>`, 제어 문자** → 화면에 글자 그대로 (D1~D3, E1).
5. **교차 출처 POST, Host 위조(DNS 리바인딩), CSRF 헤더 없음, 로그인 서명을 승인에 재사용(그 반대도)** → 거부 (C3, C4, C5, E1).

## 파일 구조

| 경로 | 책임 |
|---|---|
| `internal/orch/api/v1.go`, `timeline.go`, `diff.go`, `stages.go`, `info.go` | API v1 라우터와 읽기 엔드포인트 |
| `internal/orch/api/auth.go`, `secure.go` | 신원·세션·티어, HTTP 보안 미들웨어 |
| `internal/approval/signing/` | 정규 메시지, 서명자 목록, SSH 서명·통합 검증 |
| `internal/webauthn/` | 최소 CBOR, 증명 파싱, assertion 검증 |
| `internal/approval/approval.go` | 승인 파일 v2(서명 항목), 서명 필수 모드 |
| `internal/orch/store/auth.go` | 세션·챌린지·대기 자격·멱등 키 테이블 |
| `internal/orch/web/` | `go:embed` 정적 파일(HTML, CSS, JS)과 JS 시험 |
| `cmd/orch/` | 원격·v1 클라이언트, 서명 승인, `admin` 명령 |
| `cmd/orchd/` | 새 플래그와 연결 |
| `infra/` | 배포 스크립트, 방화벽 규칙, systemd 단위 |

---

# A. API v1 (설계서 P1)

### Task A1: 라우터와 오류 봉투
**Files:** Create `internal/orch/api/v1.go`, `v1_test.go`; Modify `internal/orch/api/api.go`(`newHandler`가 v1을 붙임)
**Interfaces:** Produces `func (h *handler) mountV1(mux *http.ServeMux)`, `func writeV1Err(w http.ResponseWriter, status int, code, msg string)`. 기존 경로는 그대로 두고 `/api/v1/tasks` 등 같은 핸들러를 봉투 오류로 감싸 노출.
- [ ] **시험 작성** `v1_test.go`(`newEnv`, `e.do` 사용): `TestV1UnknownRouteEnvelope`(404, JSON `error.code=="not_found"`), `TestV1WrongMethodEnvelope`(405 `method_not_allowed`), `TestLegacyRoutesStillWork`(`GET /tasks` 200), `TestV1TasksListMatchesLegacy`(두 응답의 작업 ID 목록 동일), `TestV1ErrorCodes`(400 `bad_request`, 404 `not_found`, 409 `conflict`)
- [ ] 실패 확인: `go test ./internal/orch/api -run 'V1|Legacy' -count=1` → 컴파일/실패
- [ ] `mountV1` 구현: 기존 `h.submit/list/get/cancel/retry/approvals/approvalGet/reverify`를 `v1Wrap(fn)`으로 감싸 응답의 `{"error":"msg"}`를 `{"error":{"code":codeOf(status),"message":msg}}`로 바꾼다.
- [ ] 통과 확인 + `go vet ./internal/orch/api`
- [ ] 커밋 `feat(api): /api/v1 router with error envelope`

### Task A2: 단계별 시간(타임라인)과 이벤트 엔드포인트
**Files:** Create `internal/orch/api/timeline.go`, `timeline_test.go`; Modify `v1.go`
**Interfaces:** Produces `type StepTime struct{State string; Seconds float64; Note string; At time.Time}`, `type VerifyBreakdown struct{RestoreSec, GateSec, BaseSec, HeadSec float64; BaseCacheHits, BaseCacheRuns int}`, `func Timeline(evs []store.Event) []StepTime`, `func ParseVerifyNote(note string) (VerifyBreakdown, bool)`; 경로 `GET /api/v1/tasks/{id}/events` → `{task, steps, verify}`.
- [ ] **시험:** `TestTimelineDurations`(이벤트 3개 → 초 = 다음 이벤트 시각 − 이번, 마지막은 0), `TestParseVerifyNote`(실제 노트 두 줄: `verdict ok | restore 55s (delete_server 11s, ...) | gate 153s (base 79s, head 71s)`과 캐시 적중 `... | gate 84s (base 0.0s, head 80s) | base cache 1/1` → 값 일치), `TestParseVerifyNoteMissing`(타이밍 없는 노트는 false), `TestEventsEndpoint`(작업 하나 만들고 전이 후 steps 길이)
- [ ] 구현: `Timeline`은 순수 함수. `ParseVerifyNote`는 정규식 하나로 `restore Ns`, `gate Ns (base X, head Y)`, `base cache h/n`을 읽는다.
- [ ] 통과 + vet, 커밋 `feat(api): task timeline and events endpoint`

### Task A3: 보고서 엔드포인트
**Files:** Create `internal/orch/api/report.go`, `report_test.go`; Modify `daemon.go`(`Config.ReportDir string`), `v1.go`, `cmd/orchd/vm.go`(`cfg.ReportDir` 전달)
**Interfaces:** Produces `GET /api/v1/tasks/{id}/report` → 보고서 JSON 원문(2 MiB 상한).
- [ ] **시험:** `TestReportServedFromReportDir`, `TestReportOutsideDirRefused`(DB의 `report_path`를 `/etc/passwd`로 바꿔도 404), `TestReportSymlinkEscapeRefused`(보고서 폴더 안 심볼릭 링크가 밖을 가리키면 거부), `TestReportTooLarge`(413), `TestReportNotYet`(판정 전 404)
- [ ] 구현: `filepath.EvalSymlinks`로 풀고 `ReportDir`(풀린 경로)의 접두 + 경로 구분자 검사. 열 때 `O_NOFOLLOW`.
- [ ] 통과 + vet, 커밋 `feat(api): task report endpoint under the report dir`

### Task A4: diff 엔드포인트와 `gitx.HashOf`
**Files:** Modify `internal/gitx/gitx.go`(+`gitx_test.go`); Create `internal/orch/api/diff.go`, `diff_test.go`; Modify `v1.go`
**Interfaces:** Produces `func HashOf(s string) string`(기존 `hashOf`의 공개 래퍼), `type DiffView struct{Hash string; HashedText string; Truncated bool; ProtectedFiles []string; Files []FileStat}`, `type FileStat struct{Path string; Added, Deleted int}`; `GET /api/v1/tasks/{id}/diff`. Consumes `gate.Prepare(gate.Options{Repo, Base, Head})`(`p.Diff.Hash`, `p.Diff.HashedText()`, `p.Diff.Changed()`).
- [ ] **시험:** `TestHashOfMatchesDiffHash`(`gitx.HashOf(p.Diff.HashedText()) == p.Diff.Hash`), `TestDiffReturnsHashAndProtectedFiles`(임시 git 저장소에 보호 파일 수정 커밋 → 해시가 `gate.Prepare`와 동일, 보호 목록 일치), `TestDiffTruncation`(256 KiB 초과 → `Truncated=true`, `len(HashedText)<=256KiB`, 해시는 전체 기준), `TestDiffNoHead409`, `TestDiffNoTextconvNoExtDiff`(`.gitattributes`로 textconv 드라이버를 지정한 저장소에서 마커 파일이 만들어지지 않음)
- [ ] 구현: `Files`는 `git -c core.hooksPath=/dev/null diff --numstat --no-ext-diff --no-textconv <base> <head>`(publish.go의 최소 환경과 같은 env). 잘림 시 `HashedText`는 앞 256 KiB를 UTF-8 경계에서 자름.
- [ ] 통과 + vet, 커밋 `feat(api): diff endpoint reusing the gate's diff and hash`

### Task A5: 단계 메뉴와 개요
**Files:** Create `internal/orch/api/stages.go`, `stages_test.go`; Modify `v1.go`
**Interfaces:** Produces `type Stage struct{ID, Name, Status string; Components []string; Next string}`, `GET /api/v1/stages`, `type Overview struct{Total int; ByState map[string]int; PendingApprovals int; StageAvg map[string]float64; Todo []TodoItem}`, `type TodoItem struct{Kind string; Task int64; Approval int64; Text string}`, `GET /api/v1/overview`.
- [ ] **시험:** `TestStagesStatic`(6개: design, implement, verify, approve, publish, operate. 상태값은 ✅🔄⬜ 중 하나), `TestOverviewCounts`(상태가 다른 작업 4개 → `ByState`, `Total`), `TestOverviewTodoListsPendingApprovals`, `TestOverviewStageAvgFromEvents`(최근 완료 작업의 이벤트로 단계 평균, 완료 작업 없으면 키 없음)
- [ ] 구현: 단계 표는 코드 상수(출처 주석: `전체_계획서.md` 2026-11-09). 개요는 `ListTasks(ctx, 500)`와 최근 완료 20건의 `Events`로 계산(상한을 둬 비용 제한).
- [ ] 통과 + vet, 커밋 `feat(api): stages and overview endpoints`

### Task A6: 프로젝트, 정책, 운영 현황
**Files:** Create `internal/orch/api/info.go`, `info_test.go`; Modify `daemon.go`(`Config.Policy func() PolicyView`, `Config.Ops func() OpsView`), `v1.go`, `cmd/orchd/main.go`·`vm.go`(공급 함수 연결)
**Interfaces:** Produces `type PolicyView struct{Hash string; Classes map[string]string; Allowlist, Floor []string; BaseCache bool}`, `type OpsView struct{Build string; Started time.Time; LastRestore *RestoreView; BaseCacheEntries int}`, `GET /api/v1/projects`(작업의 repo별 `{repo, class, tasks, last_task}` 집계), `/policy`, `/ops`.
- [ ] **시험:** `TestProjectsAggregatedFromTasks`, `TestPolicyEndpoint`(스텁 공급 함수), `TestOpsNilProvidersGiveEmptyObject`(공급 함수가 nil이어도 500이 아니라 `{}`), `TestOpsNoSecrets`(응답에 토큰·키 경로가 없음)
- [ ] 구현: `orchd`는 `policy.Live.Current()`로 `PolicyView`, `steps`의 복원 기록과 base 캐시 디렉터리 항목 수로 `OpsView`를 채운다.
- [ ] 통과 + vet, 커밋 `feat(api): projects, policy and ops read endpoints`

### Task A7: CLI를 API v1 클라이언트로 전환
**Files:** Modify `cmd/orch/cli.go`, `approve.go`; Create `cmd/orch/transport.go`, `transport_test.go`; Test `cmd/orch/cli_test.go`
**Interfaces:** Produces `func newClient(sock string) *client`(유지), `func newRemoteClient(server string, token func() (string, error)) *client`, 플래그 `--server URL`, `--token-file PATH`. `client.call`이 `/api/v1`을 붙이고 `{"error":{"message"}}`를 읽는다. Consumes A1의 v1 경로.
- [ ] **시험:** 기존 `cli_test.go`·`approve_test.go`가 v1 경로로 그대로 통과, `TestCLIAgainstHTTPServer`(`httptest.NewServer`, `Authorization: Bearer` 전달), `TestCLIErrorEnvelopeMessage`(서버 메시지가 한국어 그대로 출력)
- [ ] 구현: 경로 접두사와 오류 파싱만 바꾸고 명령 동작은 유지. 원격 모드에서는 소켓 대신 HTTPS 전송. 인증 토큰 공급은 C3에서 채우고 이 작업에서는 파일에서 읽는 함수만.
- [ ] `go test ./cmd/... -count=1`, 커밋 `feat(cli): talk to API v1, add --server remote mode`

---

# B. 서명 승인 (설계서 P2)

### Task B1: 정규 메시지
**Files:** Create `internal/approval/signing/message.go`, `message_test.go`
**Interfaces:** Produces `type Message struct{Kind, Repo string; Task int64; Base, Hash string; Issued time.Time}`, `func (m Message) Text() string`, `func (m Message) Digest() [32]byte`, `func ParseMessage(text string) (Message, error)`, `const NamespaceApproval = "aigate-approval"`, `Kind*` 상수 2개.
- [ ] **시험:** `TestMessageTextGolden`(고정 입력 → 설계서 8.1 형식 그대로의 문자열), `TestDigestGolden`(SHA-256 16진 고정값), `TestParseRoundTrip`, `TestParseRejects`(CRLF, 앞뒤 공백, 줄 추가·누락·순서 바뀜, 알 수 없는 kind, 비-UTC 시각, 빈 해시)
- [ ] 구현: `Text()`는 `fmt` 템플릿 한 곳. `ParseMessage`는 `Text()`와 정확히 같은 줄 구조만 받아들이고, 파싱 후 `Text()`를 다시 만들어 입력과 바이트 단위로 같은지 확인한다.
- [ ] 통과, 커밋 `feat(signing): canonical approval message`

### Task B2: 서명자 목록
**Files:** Create `internal/approval/signing/registry.go`, `registry_test.go`
**Interfaces:** Produces `type Signer struct{ID, Type, Label, PublicKey, CredentialID, RPID, Origin string; AddedAt time.Time}`(Type: `ssh-ed25519`, `webauthn-es256`; 웹 서명자는 `CredentialID`, `RPID`, `Origin` 필수, R8), `func RegistryPath() string`, `func SetRegistryPath(path string)`, `var DefaultOptions RegistryOptions`(R7), `type Registry struct{Signers []Signer}`, `type RegistryOptions struct{OwnerOK func(uid uint32) bool}`, `const DefaultRegistryPath = "/etc/aigate/signers.json"`, `func LoadRegistry(path string, o RegistryOptions) (Registry, error)`, `func (r Registry) Find(id string) (Signer, bool)`, `var ErrRegistryAbsent`.
- [ ] **시험:** `TestAbsentIsErrRegistryAbsent`, `TestLoadOK`, `TestRefusesGroupOrWorldWritableFile`, `TestRefusesSymlink`, `TestRefusesNonRootOwnerFile`(`OwnerOK` 기본은 uid 0), `TestRefusesWritableParentDir`(상위 폴더가 그룹·기타 쓰기 가능), `TestRefusesNonRootOwnedParentDir`, `TestRejectsDuplicateIDsAndUnknownType`, `TestRejectsBadPublicKey`
- [ ] 구현: `os.Lstat`으로 파일과 `filepath.Dir`을 검사. 시험에서는 `OwnerOK`를 현재 uid 허용으로 바꿔 쓴다. 공개 키는 `ssh.ParseAuthorizedKey`(ed25519만), 웹 키는 base64url 비압축 P-256 점(65바이트, 0x04 시작).
- [ ] 통과, 커밋 `feat(signing): root-owned signers registry loader`

### Task B3: SSH 서명 검증
**Files:** Modify `go.mod`/`go.sum`(`go get golang.org/x/crypto`); Create `internal/approval/signing/sshsig.go`, `sshsig_test.go`
**Interfaces:** Produces `func VerifySSHSig(authorizedKey, namespace string, message []byte, armored string) error`.
- [ ] **시험(시험 중 `ssh-keygen -t ed25519`와 `ssh-keygen -Y sign`으로 키와 서명을 만든다, 없으면 `t.Skip`):** `TestVerifyOK`, `TestWrongNamespace`, `TestTamperedMessage`, `TestWrongKey`, `TestRSAKeyRefused`, `TestMalformedArmorNoPanic`(빈 문자열, 잘린 base64, 잘못된 매직, 긴 입력)
- [ ] 구현: PROTOCOL.sshsig 형식 파싱(`SSHSIG` 매직, 버전 1, 공개 키, 네임스페이스, 예약, 해시 알고리즘 sha256/sha512, 서명). 서명 대상은 `"SSHSIG" + string(ns) + string("") + string(hashalg) + string(H(message))`. 서명 안의 공개 키가 등록 키와 바이트 단위로 같아야 하고, `ssh.PublicKey.Verify`로 검증한다.
- [ ] 통과 + `go mod tidy`로 의존성 추가분만 늘었는지 확인, 커밋 `feat(signing): verify ssh-keygen -Y sign signatures`

### Task B4: 최소 WebAuthn 검증기
**Files:** Create `internal/webauthn/cbor.go`, `attestation.go`, `assertion.go`, `webauthn_test.go`
**Interfaces:** Produces `type Credential struct{ID []byte; PublicKey *ecdsa.PublicKey}`, `func ParseAttestationNone(attestationObject []byte) (Credential, error)`, `type AssertionInput struct{PublicKey *ecdsa.PublicKey; AuthenticatorData, ClientDataJSON, Signature, Challenge []byte; RPID, Origin string}`, `func VerifyAssertion(in AssertionInput) error`.
- [ ] **시험(시험용 합성 인증기: P-256 키로 증명 객체와 assertion을 직접 만든다):** `TestRegisterNoneOK`, `TestAssertOK`, `TestWrongOrigin`, `TestWrongRPID`, `TestMissingUserVerification`, `TestMissingUserPresence`, `TestWrongChallenge`, `TestBadSignature`, `TestTypeCreateRefused`(assertion에 `webauthn.create`), `TestCrossOriginTrueRefused`, `TestAttestationFormatNotNoneRefused`, `TestCBORMalformed`(잘림·무한 길이·깊이 9 초과·중복 키), `TestOversizedInputRefused`
- [ ] 구현: 최소 CBOR(부호 없는/음수 정수, 바이트열, 텍스트, 배열, 맵만, 깊이 8, 불확정 길이 거부). 서명 카운터는 검증하지 않는다(패스키는 동기화로 카운터가 0일 수 있고, 서명이 메시지 해시에 묶여 재사용은 다른 항목에 무효). 검증은 `clientData.type=="webauthn.get"`, 챌린지 base64url 일치, origin 일치, rpIdHash=SHA-256(rpID), UP·UV 플래그, ES256 DER 서명을 `authData ‖ SHA-256(clientDataJSON)`에 대해 `ecdsa.VerifyASN1`.
- [ ] 통과 + `go vet`, 커밋 `feat(webauthn): minimal ES256 assertion verifier`

### Task B5: 통합 검증 `signing.Verify`
**Files:** Create `internal/approval/signing/verify.go`, `verify_test.go`
**Interfaces:** Produces `type Signature struct{Signer, Alg, Sig string; WebAuthn *WebAuthnData}`, `type WebAuthnData struct{AuthenticatorData, ClientDataJSON string}`(base64url), `type VerifyOptions struct{Now time.Time; MaxAge time.Duration; SkipFreshness bool}`(웹 서명의 RPID·Origin은 서명자 항목에서 읽는다, R8), `func Verify(reg Registry, msg Message, sig Signature, o VerifyOptions) error`. Consumes B1~B4.
- [ ] **시험:** `TestVerifySSH`, `TestVerifyWebAuthn`(챌린지 = `msg.Digest()`), `TestUnknownSigner`, `TestAlgMismatchWithSignerType`, `TestIssuedTooOldAndTooNew`(±5분 경계), `TestSkipFreshnessIgnoresIssued`, `TestLoginSignatureNotAcceptedAsApproval`(네임스페이스·메시지 접두가 다른 서명 거부)
- [ ] 구현: 서명자 조회 → 종류 확인 → 신선도(건너뛰기 옵션 없을 때) → 방식별 검증. 오류는 정해진 `ErrBadSignature`, `ErrUnknownSigner`, `ErrStale`.
- [ ] 통과, 커밋 `feat(signing): unified signature verification`

### Task B6: 승인 파일 v2와 서명 필수 모드
**Files:** Modify `internal/approval/approval.go`, `approval_test.go`; Test `internal/gate` 한 건
**Interfaces:** Produces `Store{Path string}`(변경 없음. 서명자 목록 경로는 `signing.RegistryPath()`, 옵션은 `signing.DefaultOptions`, R7), 항목에 `message, signer, alg, sig, webauthn` 필드(`omitempty`, 파일 version은 1 유지), `func (s Store) GrantSigned(msg signing.Message, sig signing.Signature, now time.Time, o signing.VerifyOptions) error`, `var ErrSignatureRequired`; 기존 `Grant`는 서명 필수 모드에서 `ErrSignatureRequired`. `Valid` 시그니처는 그대로다(호출자 6곳 변경 없음). Consumes B5.
- [ ] **시험:** `TestLegacyModeUnchanged`(목록 파일 없음 → 기존 동작), `TestSignedModeRejectsUnsignedEntry`, `TestSignedModeAcceptsValidEntry`, `TestSignedModeRejectsTamperedHash`, `TestSignedModeRejectsTamperedBaseOrRepo`, `TestSignedModeRejectsEditedMessage`, `TestSignatureForItemANotValidForItemB`(검토 포인트 1), `TestLoosePermsRegistryFailsClosed`(시험은 `signing.SetRegistryPath`와 `t.Cleanup`으로 전역 복원)(검토 포인트 3: 목록이 있으나 권한이 틀리면 `Valid` false이고 `GrantSigned` 오류), `TestGrantRefusedInSignedMode`, `TestGrantSignedIdempotentAndConcurrent`(두 고루틴 동시 → 항목 1개, flock), `TestGateRunsHonorSignedMode`(`internal/gate`: 서명 없는 승인 파일 + 목록 파일 → `ApprovalHashValid` false)
- [ ] 구현: `Valid`는 목록 파일이 있으면(없으면 `ErrRegistryAbsent` → 기존 경로) 항목의 `message`를 `ParseMessage`로 읽고 `(repo, base, hash)`가 인자와 같은지 확인한 뒤 `SkipFreshness: true`로 검증한다. 목록 로드 오류는 false. `GrantSigned`는 신선도를 강제하고 같은 flock 구간에서 쓴다.
- [ ] `go test ./internal/approval/... ./internal/gate/... ./internal/orch/... -count=1 -race`, 커밋 `feat(approval): signed entries and signed-only mode`

### Task B7: 승인 상세의 서명 메시지와 `grant` 엔드포인트
**Files:** Modify `internal/orch/api/v1.go`, `api.go`; Create `internal/orch/api/grant.go`, `grant_test.go`
**Interfaces:** Produces `GET /api/v1/approvals/{id}` 응답에 `message`, `digest`, `hashed_text`(보호 파일 항목, A4 재사용), `truncated`(R11), `meaning`(한국어 "무엇을 허용하나요?"), `requires_signature bool`, `issued`; `POST /api/v1/approvals/{id}/grant` 본문 `{signer, alg, sig, issued, webauthn?}` → 서명 확인 → `GrantSigned` → 재판정 → 작업 뷰. 이력(ledger) `approval_granted`에 `signer`, `alg`, `via`(socket/session/token) 추가.
- [ ] **시험(시험용 서명자 목록 + `ssh-keygen -Y sign`):** `TestGrantHappyPathSSH`(작업이 `passed`/다음 상태로 진행), `TestGrantWrongHashConflict409`, `TestGrantStaleIssued403`, `TestGrantUnknownSigner403`, `TestGrantNotPending409`, `TestGrantDestructiveBlockedRefused`, `TestGrantSignatureForOtherApprovalRefused`(검토 포인트 1), `TestGrantMessageRebuiltServerSide`(클라이언트가 보낸 `issued`로만 메시지를 다시 만든다, 본문의 다른 필드는 무시), `TestGrantLedgerRecordsSigner`, `TestDetailMessageMatchesGrantVerification`, `TestKindConstantsMatchStore`(R12)
- [ ] 구현: 서버는 항목과 작업에서 `Message`를 다시 만든다(`Issued`만 클라이언트 값, 허용 오차 안). 서명 필수 모드가 아니면 `requires_signature=false`이고 `grant`는 400 `not_signed_mode`.
- [ ] 통과 + `-race`, 커밋 `feat(api): signed approval grant endpoint`

### Task B8: CLI 서명 승인과 `orch admin signers`
**Files:** Modify `cmd/orch/approve.go`, `cli.go`; Create `cmd/orch/admin.go`, `admin_test.go`, `approve_signed_test.go`
**Interfaces:** Produces `orch approve ITEM [--sign-key PATH]`(서명 필수면 서명 흐름), `var signCmd func(keyPath, namespace string, message []byte) (armored string, err error)`(R9. 시험용 이음새, 기본은 `ssh-keygen -Y sign -n <namespace> -f <키>`에 메시지를 표준 입력으로. 승인은 `aigate-approval`), `orch admin signers add|list|remove|pending`(`--signers-file`, 쓰기는 euid 0만), `var geteuid = os.Geteuid`.
- [ ] **시험:** `TestApproveSignedFlow`(상세 → TTY 확인 → 서명 → `grant`), `TestApproveRefusesHashMismatch`(서버가 준 `hashed_text`의 `gitx.HashOf`가 `hash`와 다르면 서명하지 않고 이유 출력), `TestApproveRefusesTruncatedDiff`(`Truncated`면 서명 거부, 안내 출력), `TestApproveStillNeedsTTY`, `TestLegacyApproveUnchangedWhenNotSigned`, `TestSignersAddRequiresRoot`, `TestSignersAddWritesAtomicallyAndIdempotent`, `TestSignersListShowsFingerprints`, `TestSignersRemoveRequiresRoot`
- [ ] 구현: 정책 변경 같은 `hashed_text`가 없는 항목은 서버가 계산한 `meaning`과 해시를 보여 주고 서명한다(해시 재계산은 diff가 있는 항목에만). `admin signers add`는 `ssh-ed25519` 공개 키 파일과 웹 대기 자격(`pending`) 둘 다 받는다. 목록은 임시 파일에 쓰고 `rename`.
- [ ] 통과, 커밋 `feat(cli): signed approvals and admin signers`

---

# C. 신원과 HTTPS (설계서 P3)

### Task C1: 저장소 마이그레이션 6 (세션, 챌린지, 대기 자격, 멱등 키)
**Files:** Modify `internal/orch/store/store.go`(마이그레이션 6 추가); Create `internal/orch/store/auth.go`, `auth_test.go`
**Interfaces:** Produces `type Session struct{ID, Principal, Kind, CSRF string; CreatedAt, LastSeen, ExpiresAt time.Time}`, `CreateSession/GetSession/TouchSession/DeleteSession`, `type Challenge struct{ID, Purpose, Data string; ExpiresAt time.Time}`, `SaveChallenge/TakeChallenge`(한 번만), `AddPendingCredential/ListPendingCredentials/DeletePendingCredential`, `PutIdempotent/GetIdempotent(key) (taskID int64, ok bool)`, `PurgeExpired(ctx)`. 비밀(세션·토큰)은 SHA-256만 저장.
- [ ] **시험:** `TestMigrateFromV5KeepsTasks`(v5 DB를 열면 작업·승인 보존, 버전 6), `TestSessionExpiry`, `TestChallengeSingleUse`, `TestChallengeExpiry`, `TestPendingCredentialRoundTrip`, `TestIdempotencyKeyTTL`, `TestPurgeExpired`, `TestSessionStoresHashNotSecret`(DB에서 원문 토큰이 검색되지 않음)
- [ ] 구현·통과·`-race`, 커밋 `feat(store): sessions, challenges, pending credentials, idempotency (schema v6)`

### Task C2: 신원 주체, 티어 미들웨어, 소켓 uid
**Files:** Create `internal/orch/api/auth.go`, `auth_test.go`; Modify `daemon.go`(`Config.SocketUIDs []uint32`, `ConnContext`)
**Interfaces:** Produces `type Principal struct{ID, Kind string}`(Kind: `socket`, `session`, `token`), `func PrincipalFrom(ctx context.Context) (Principal, bool)`, 미들웨어 `authenticate(next)`(소켓: 상대 uid가 허용 목록(R10: `peercred_linux.go`, `peercred_darwin.go` 빌드 태그 파일), HTTP: 베어러 토큰 또는 세션 쿠키), `requireWrite(next)`, `GET /api/v1/healthz`(무인증), `POST /tasks`의 `Idempotency-Key` 처리.
- [ ] **시험:** `TestSocketPrincipalAllowedUID`, `TestSocketForeignUIDRefused`(`peerUID` 이음새), `TestHTTPNoCredentials401`, `TestHealthzOpen`, `TestIdempotentSubmitReturnsSameTask`, `TestIdempotencyKeyDifferentBodyConflict409`
- [ ] 구현·통과·`-race`, 커밋 `feat(api): principals, write tier, socket uid allowlist, idempotency`

### Task C3: SSH 챌린지 로그인 (서버와 CLI)
**Files:** Create `internal/orch/api/login_ssh.go`, `login_ssh_test.go`, `cmd/orch/login.go`, `login_test.go`; Modify `v1.go`, `cmd/orch/transport.go`
**Interfaces:** Produces `POST /api/v1/auth/challenge {signer}` → `{challenge_id, message, expires}`, `POST /api/v1/auth/token {challenge_id, sig}` → `{token, expires}`(15분). 로그인 메시지는 `aigate-login-v1\nsigner: …\nnonce: …\nissued: …`, 네임스페이스 `aigate-login`.
- [ ] **시험:** `TestLoginHappyPath`, `TestChallengeSingleUse`(재사용 거부), `TestWrongSigner`, `TestExpiredChallenge`, `TestApprovalSignatureNotAcceptedAsLogin`, `TestLoginSignatureNotAcceptedAsApproval`(검토 포인트 5), `TestTokenExpires`, `TestTokenUnlocksWriteNotApproval`(토큰만으로는 `grant` 불가, 서명이 필요), **CLI:** `TestCLILoginStoresTokenMode0600`(`orch login`이 토큰을 `--token-file`에 0600으로 저장), `TestRemoteClientAutoLoginWhenTokenExpired`(만료된 토큰이면 `signCmd` 이음새로 챌린지에 서명해 새 토큰을 받고 요청을 재시도), `TestRemoteClientNeverSendsKeyMaterial`(요청 본문에 개인 키 경로·내용이 없음)
- [ ] 구현: CLI는 A7의 `token func() (string, error)`를 채운다(파일의 토큰이 유효하면 사용, 아니면 B8의 `signCmd`로 `aigate-login` 네임스페이스 서명).
- [ ] 통과, 커밋 `feat: ssh key challenge login (api and cli)`

### Task C4a: 패스키 등록 (대기 자격)
**Files:** Create `internal/orch/api/webauthn_register.go`, `webauthn_register_test.go`; Modify `cmd/orch/admin.go`(`orch admin enroll`), `v1.go`
**Interfaces:** Produces `orch admin enroll`(소켓 전용, 일회용 코드 발급·출력, 10분), `POST /api/v1/auth/webauthn/register/begin {code}` → 등록 옵션(챌린지), `POST /api/v1/auth/webauthn/register/finish` → 대기 자격 저장(`orch admin signers pending`으로 조회, `signers add --pending <id>`로 root가 활성화). 대기 자격에는 등록 시점의 `Config.RPID`·`Origin`을 기록하고 활성화 때 서명자 항목에 복사한다(R8).
- [ ] **시험:** `TestEnrollCodeSingleUseAndExpiry`, `TestRegisterBeginNeedsValidCode`, `TestRegisterFinishStoresPendingOnly`(목록 파일은 그대로, 로그인 불가), `TestRegisterRejectsWrongOriginOrRPID`, `TestPendingNotUsableForLoginOrApproval`
- [ ] 구현·통과, 커밋 `feat(api): passkey enrollment into a pending list`

### Task C4b: 패스키 로그인, 세션, CSRF
**Files:** Create `internal/orch/api/webauthn_login.go`, `webauthn_login_test.go`; Modify `auth.go`, `v1.go`
**Interfaces:** Produces `GET /api/v1/auth/signers`(활성 웹 자격의 공개 목록: id, credential_id, label), `POST /auth/webauthn/login/begin {signer}`·`/finish`(assertion 검증 → 세션 쿠키 `HttpOnly; Secure; SameSite=Strict`, 응답에 CSRF 토큰), `GET /auth/session`, `POST /auth/logout`. 세션 수명: 유휴 8시간, 절대 24시간. 로그인 챌린지와 승인 챌린지는 목적(purpose)이 달라 서로 대체될 수 없다.
- [ ] **시험:** `TestLoginOKSetsCookieFlags`, `TestLoginOnlyForActiveRegistrySigners`, `TestCSRFHeaderRequiredOnWrite`, `TestSessionIdleAndAbsoluteExpiry`, `TestLogoutInvalidates`, `TestApprovalAssertionNotAcceptedAsLogin`(검토 포인트 5), `TestSessionNeverUnlocksApprovalWithoutSignature`
- [ ] 구현·통과, 커밋 `feat(api): passkey login, sessions, csrf`

### Task C5: HTTP 리스너와 보안 미들웨어, orchd 플래그
**Files:** Create `internal/orch/api/secure.go`, `secure_test.go`; Modify `daemon.go`(`Config.HTTPListen, AllowedHosts []string, SessionTTL`, 두 번째 `http.Server`), `cmd/orchd/main.go`
**Interfaces:** Produces 미들웨어 `secureHeaders`, `hostCheck(allowed)`, `originCheck(origin)`, `limitBody`, `rateLimit`(인증 경로 분당 10회/주소), 플래그 `--http-listen`, `--allowed-host`(반복), `--origin`, `--rp-id`, `--signers-file`(시작 시 `signing.SetRegistryPath`, R7), `--session-ttl`. 루프백이 아닌 주소 바인딩은 거부.
- [ ] **시험:** `TestSecurityHeadersPresent`, `TestHostMismatchRefused`(검토 포인트 5), `TestCrossOriginPostRefused`, `TestBodyTooLarge413`, `TestRateLimit429`, `TestNonLoopbackBindRefused`, `TestShutdownClosesBothListeners`, `TestAPIResponsesNoStore`
- [ ] 구현·통과·`-race`, 커밋 `feat(api): loopback http listener with security middleware`

### Task C6: 배포 산출물
**Files:** Create `infra/orch-web-serve.sh`, `infra/signers-init.sh`; Modify `infra/nftables/orchestrator.nft`, `infra/systemd/orchd-orchvm.service`, `infra/README.md`
- [ ] `infra/orch-web-serve.sh`: 기본은 드라이런으로 `tailscale serve --bg --https=443 http://127.0.0.1:<PORT>` 계획만 출력, `--apply`일 때 실행. 포트는 인자.
- [ ] `infra/signers-init.sh`(root에서 실행): `/etc/aigate`를 `root:root 0755`로 만들고 빈 `signers.json`은 만들지 **않는다**(만들면 서명 필수 모드가 켜지므로 컷오버 단계에서 첫 키와 함께 만듦). 드라이런 기본.
- [ ] nft: `iifname "tailscale0" tcp dport 443 accept`를 입력 체인에 추가. 단위 파일에 새 플래그 반영(값은 예시, 실제 값은 E2에서 확정).
- [ ] 검증: `sh -n`으로 문법, `nft -c -f infra/nftables/orchestrator.nft`(가능한 환경에서). 커밋 `infra: web serve, signers init, firewall 443, unit flags`

---

# D. 웹 뼈대 (설계서 P4)

### Task D1: 임베드 뼈대, 단계 메뉴, 개요
**Files:** Create `internal/orch/web/web.go`, `web_test.go`, `static/index.html`, `static/app.js`, `static/style.css`, `static/render.js`, `static/api.js`, `test/render.test.js`; Modify `daemon.go`(HTTP 리스너에서 `/`와 정적 파일 제공)
**Interfaces:** Produces `//go:embed static`, `func web.Handler() http.Handler`(SPA: `/api/`를 제외한 경로는 `index.html`), JS `el(tag, attrs, children)`(텍스트는 항상 `textContent`), `renderStageMenu(stages)`, `renderOverview(overview)`, `poll(fn, ms)`(일시정지·마지막 갱신 시각). 스타일은 `web-mockup/index-v4.html`에서 가져온다.
- [ ] **시험(Go):** `TestStaticServedWithCSPAndTypes`, `TestSPAFallbackNotForAPI`(`/api/v1/nope`는 JSON 404), `TestWebAssetsEmbedded`. **시험(JS, `node --test`):** `TestElNeverCreatesHTML`(`<img src=x onerror=…>`·`</script>`·제어 문자가 텍스트 노드로만 생김, 자식 요소 없음), `TestStageMenuFromStages`, `TestOverviewShowsTodo`, `TestPollPauseStopsFetch`. Go 쪽에 `TestWebJS`가 `node --test`를 실행(node 없으면 skip).
- [ ] 구현·통과, 커밋 `feat(web): embedded shell, stage menu and overview`

### Task D2: 작업 목록·상세, diff 보기
**Files:** Create `static/views/tasks.js`, `static/views/task.js`, `static/views/diff.js`, `test/views.test.js`
- [ ] **시험(JS):** `TestTasksTableRendersHostileText`(프롬프트에 HTML이 있어도 글자 그대로), `TestTaskTimelineBars`(A2 응답으로 막대 폭 계산), `TestDiffViewEscapesAndMarksProtected`, `TestDiffTruncatedBanner`
- [ ] 구현: 데이터는 A2/A4/A1 응답. 목록 행은 네이티브 `<button aria-pressed>`.
- [ ] 통과, 커밋 `feat(web): tasks, task detail and diff views`

### Task D3: 승인 화면, 패스키 서명, 등록 UI
**Files:** Create `static/views/approvals.js`, `static/views/login.js`, `static/webauthn.js`, `test/approvals.test.js`
**Interfaces:** Produces `signApproval(detail)`(`navigator.credentials.get`의 challenge = `detail.digest`), `register(code)`, 로그인 화면. 보안 승인은 승인 직전 화면에서 서버가 준 `message`를 그대로 표시한다.
- [ ] **시험(JS, WebAuthn은 스텁):** `TestSignApprovalUsesDigestAsChallenge`, `TestApprovalsShowMeaningAndMessage`, `TestGrantSentOnlyAfterConfirm`, `TestHashedTextHashRecomputeIsNotClaimed`(웹은 해시 재계산을 하지 않으므로 화면에 "확인되지 않음" 표시. 설계서 8.7 후속), `TestLoginViewNoSecretsInDOM`
- [ ] 구현·통과, 커밋 `feat(web): approvals with passkey signing and enrollment UI`

---

# E. 검증과 컷오버 (설계서 P5)

### Task E1: 적대 시험 모음
**Files:** Create `internal/orch/api/adversarial_test.go`, `internal/approval/adversarial_test.go`
- [ ] **시험(모두 거부/무효를 확인):** `TestForgedApprovalsFileIgnored`(서명 필수 모드에서 `approvals.json`에 항목을 직접 추가), `TestEditedMessageOrHashRejected`, `TestReplayAcrossItemsRejected`, `TestRegistryInOrchOwnedDirRefused`(목록 파일이 orch 소유 폴더에 있음), `TestRegistryMode0666Refused`, `TestRegistryDeletedFallsBackOnlyWhenAbsentNotWhenBroken`(깨진 파일은 레거시로 되돌아가지 않고 거부), `TestLoginVsApprovalNamespacesDisjoint`, `TestCSRFCrossOriginRejected`, `TestHostRebindingRejected`, `TestBodyAndDiffCaps`, `TestStaleIssuedRejected`, `TestUnsignedLegacyEntryAfterSignedModeRejected`, `TestConcurrentGrantsOneEntry`
- [ ] **변이 확인(시험이 실제로 잡는지):** 아래 5가지를 임시로 망가뜨려 해당 시험이 실패하는지 본다. ① `Valid`가 서명 검증을 건너뜀 ② 레지스트리 상위 폴더 검사 제거 ③ 메시지의 `hash` 줄 비교 제거 ④ `Origin` 검사 제거 ⑤ 로그인 네임스페이스를 승인과 같게 함. 되돌린 뒤 전체 통과를 확인하고 결과를 `docs/13-RESULTS-webconsole-W1.md` 초안에 적는다.
- [ ] `go test ./... -count=1 -race`(관련 패키지), 커밋 `test: adversarial suite for signed approvals and web security`

### Task E2: 문서와 컷오버 런북
**Files:** Modify `docs/10-OPERATIONS-orchestrator.md`, `docs/11-DESIGN-webconsole-W1.md`(상태), `전체_계획서.md`, `.superpowers/sdd/…/progress.md`; Create `docs/13-RESULTS-webconsole-W1.md`
- [ ] 런북(순서 고정, 각 단계 전 백업):
  1. 세 바이너리(`orchd`, `orch`, `aigate`)를 `.prev-w1`로 백업 후 **함께** 배포. `signers.json` 없음 → 레거시 동작 확인(작업 제출, 기존 TTY 승인 한 건).
  2. `sudo sh infra/signers-init.sh --apply`로 `/etc/aigate` 생성.
  3. Tailscale 관리 화면에서 HTTPS 인증서 기능을 켠다(**사용자 작업**). `infra/orch-web-serve.sh --apply`. `apply-firewall.sh --apply orchestrator`로 443 규칙 적용(180초 자동 롤백).
  4. `orch admin signers add`로 이 Mac의 SSH 공개 키를 등록 → 서명 필수 모드가 켜진다. toy 작업 하나로 서명 승인을 끝까지 시험(승인 → 재검증 → done).
  5. 웹에서 `orch admin enroll` 코드로 패스키 등록 → `signers add --pending` 활성화 → 웹 로그인과 웹 서명 승인 시험.
  6. **롤백:** `sudo rm /etc/aigate/signers.json`이면 레거시 모드로 돌아간다(orch 사용자는 못 함). 바이너리는 `.prev-w1`로 복원.
- [ ] 설계서의 가정 A1~A4와 실험 X1~X3 결과를 `13-RESULTS`에 기록(`tailscale serve` HTTPS, `ssh-keygen -Y sign` Go 검증, 게이트 비용 1초 미만, `hashOf` 순수성).
- [ ] 계획서(B-7)와 메모리 갱신, 커밋 `docs: web console W1 results and cutover runbook`

---

## 자체 검토 기록
- **설계서 대비 범위:** 6장 API(단계·개요·프로젝트·작업·이벤트·보고서·diff·승인·정책·운영·인증) → A1~A6, B7, C3~C4b. 7장 신원 → C2~C4b. 8장 서명 승인(메시지, 방법, 검증 위치, 파일 v2, 서명자 목록, 전환, 이력, 8.7 CLI 해시 재계산) → B1~B8. 9장 HTTPS·네트워크 → C5, C6. 10장 위협 → E1. 11장 단계 P1~P5 → A, B, C, D, E.
- **의도적 축소:** `/ops`의 VM·쿼터 수치(R4)와 웹의 해시 재계산(설계서 D5)은 W1에서 하지 않는다.
- **이름 일관성:** `signing.Message`·`Signature`·`Registry`·`VerifyOptions`, `approval.Store.GrantSigned`, `gitx.HashOf`, API 응답 `message`·`digest`·`hashed_text`가 B·C·D에서 같다.
