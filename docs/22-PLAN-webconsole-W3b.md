# W3b 구현 계획: 토큰 메뉴 (웹 폼 → broker만 저장) 와 만료 경고

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다.

**목표:** 웹의 "토큰" 화면에서 OpenAI 키와 GitLab·GitHub 토큰을 **서명 승인으로 등록·교체·삭제**하고, 값은 orchd를 거치되 저장·기록하지 않고 broker만 저장하며, 만료가 다가오면 경고한다. OpenAI 키는 교체하면 **broker 재시작 없이** 바로 쓰인다. 새 프로젝트 생성·제공자 설정·자동 병합·GitHub 어댑터는 이 계획의 범위가 아니다(W3c~W3e).

**구조:** broker에 비밀 저장소(`/var/lib/orch-broker/secrets/<ref>`, 0400)와 메타데이터(`tokens.json`, 값 없음)를 두고, 변경 op(`set_token`, `delete_token`)는 W3a와 같은 서명 검증(`aigate-registry-v1`, 새 action 둘)을 거친다. 서명되는 `change`에는 값이 아니라 **솔트를 섞은 해시** `SHA-256(request ‖ ":" ‖ 값)`이 들어가 서명 뒤 값을 바꿔치기할 수 없다. orchd는 값을 받아 **저장·로깅 없이 즉시 broker로 전달**한다. 웹은 토큰 화면과 확인·서명 흐름을 더한다.

**기술:** Go(broker, orchd, registrymsg, signing), 바닐라 JS(웹), 가짜 broker·가짜 서명자·임시 폴더로 시험.

**설계서:** `docs/19-DESIGN-webconsole-W3-projects.md`(v0.4)의 4.2, 4.5, 5.1, 6, 7장 중 W3b 몫. 앞 단계: `docs/20`·`21`(W3a, 컷오버 완료).

## 전역 제약 (모든 작업에 적용)
- **비밀은 어디에도 새지 않는다.** 토큰 값은 응답, 로그, 원장, 오류 문장, 테스트 출력, 문서, 커밋, 이 대화, **orchd의 접근 로그·패닉 출력·메모리 덤프 가능한 큰 버퍼**에 남지 않는다. 오류는 고정 코드와 고정 문장뿐이다. 시험의 가짜 값은 `canary-NOT-A-REAL-TOKEN-…` 같은 명백한 더미로, **모든 성공·실패 경로 뒤에 응답·로그·원장·파일(저장 파일 제외)을 통째로 검색**하는 시험을 갖춘다.
- **이 계획의 모든 작업은 "배포 현실"을 점검한다**(W3a에서 임시 폴더 시험이 서버의 권한 구조를 못 잡은 교훈). 새 코드가 실행 사용자·유닛 샌드박스 아래에서 만지는 **모든 경로, 모드, 소유자**를 작업 보고서에 표로 적는다(예: broker는 `orch-broker`(uid 999), `ProtectSystem=strict`, 쓰기는 `ReadWritePaths=/var/lib/orch-broker /run/orch-broker`만 가능, `/etc`는 읽기 전용, `/home`은 안 보임; orchd는 `orch`, `ProtectHome=read-only`, `/var/lib/orch-broker`는 0700이라 접근 불가). 시험은 가능하면 소유자가 다른 uid·모드 비트를 흉내 낸다.
- **토큰 파일의 신뢰:** `secrets/` 폴더는 `orch-broker` 소유 0700, 파일은 `orch-broker` 소유 0400. 소유자·모드가 어긋나거나 심볼릭 링크면 그 토큰은 **없는 것으로 취급하고 fail closed**(그 토큰을 쓰는 기능만 꺼짐, 다른 토큰·등록부는 영향 없음). 값은 한 번에 원자적으로 쓴다(임시 파일 0600 → fsync → rename → 폴더 fsync). 쓰기 실패 시 **이전 값이 그대로 남는다**.
- **orchd 바이너리는 `internal/broker`와 OpenAI 클라이언트 코드를 import하지 않는다**(W2b·W3a와 같음). 공유 코드는 순수 패키지 `internal/registrymsg`.
- 새 쓰기 엔드포인트는 `requireWrite`(세션이면 CSRF) 뒤에 둔다. 서명을 요구한다. 웹: W1~W3a 규칙(인라인 스크립트·스타일 금지, 서버 문자열은 텍스트 노드, 위험 API 금지, `confirm/alert` 금지, 저장소(localStorage 등)에 값·상태 저장 금지, 이벤트 로그에 값 금지).
- 기존 테스트의 의미를 약화하지 않는다. 바꿔야 하면 이유를 보고서에 쓴다. 커밋 메시지 끝: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. `rg`가 없으니 `grep -rn`(글롭은 따옴표).
- 검증 기본: `go vet ./...`, `gofmt -l .`(비어야 함), `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`, `go test ./internal/... ./cmd/... -count=1 -race`, 웹 작업은 `node --test "internal/orch/web/test/*.test.js"`와 `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api`(PASS, SKIP 없음). 전체 `go test ./... -count=1 -race`는 B6에서.

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **D1 토큰 종류와 이름:** 이름(`ref`)은 정규식 `^[a-z][a-z0-9-]{0,39}$`, 종류(`kind`)는 `openai`, `gitlab`, `github` 셋. **`openai` 종류는 이름이 `openai`인 것 하나만** 허용한다. `gitlab`·`github`는 여러 개(여러 서버·계정). W3b에서 **실제로 쓰이는 것은 `openai` 하나뿐**이다(broker의 OpenAI 호출). `gitlab`·`github` 토큰은 **저장·목록·삭제만 되고** W3c의 제공자가 `token_ref`로 가져다 쓴다(지금은 쓰는 코드가 없다. 기존 W2b의 `create_mr`은 계속 설정의 `gitlab.token_file`을 쓴다: 바꾸지 않는다).
- **D2 저장 위치와 우선순위:** 값은 `/var/lib/orch-broker/secrets/<ref>`(0400), 메타데이터는 `/var/lib/orch-broker/tokens.json`(0600, 값 없음): `{"version":1,"tokens":[{"ref","kind","last4","expires","set_at"}]}`. `last4`는 값의 **마지막 4자**(식별용, 알려진 정보 노출 한계는 6장에 적음), `expires`는 사용자가 적은 날짜 `YYYY-MM-DD` 또는 `""`, `set_at`은 broker 시계 UTC. **`openai` 키의 우선순위:** `secrets/openai`가 있으면 그것, 없으면 지금처럼 설정의 `openai.key_file`. 설정의 키 파일은 `/etc`(읽기 전용)라서 broker가 바꿀 수 없으므로, 웹에서 교체하면 `secrets/openai`가 생기고 설정의 파일은 그대로 남는다(사용자가 원하면 나중에 지움). 삭제하면 `secrets/openai`만 지워지고 설정의 키가 다시 쓰인다.
- **D3 값 규칙:** 길이 **32자 이상 512자 이하**, 문자는 **인쇄 가능한 ASCII(0x21~0x7e)만**(공백·개행·제어·비ASCII 거부), 앞뒤 공백은 **거부**(자동으로 잘라 주지 않는다: 서명한 해시와 달라지므로). 값은 **`set_token`에서만** 들어오고 broker가 길이·문자를 검사한다.
- **D4 서명 메시지:** W3a의 `aigate-registry-v1`에 action 두 개를 **더한다**(`set_token`, `delete_token`): `subject`는 토큰 `ref`(정규식 위), `change`의 `fields`: `set_token` = `{"kind":<문자열>,"expires":<문자열 "" 또는 YYYY-MM-DD>,"value_hash":<64자리 소문자 hex>}`, `delete_token` = `{}`(빈 객체). `value_hash = SHA-256(request ‖ ":" ‖ 값)`의 hex(`request`는 메시지의 128비트 요청 ID, 값은 UTF-8 바이트). **broker는 받은 값으로 이 해시를 다시 계산해 `fields.value_hash`와 같을 때만** 진행한다(다르면 `value_mismatch`). 기존 action(`register_project`, `set_flag`)의 의미·시험은 바꾸지 않는다.
- **D5 broker op와 오류 코드(전부 고정 문장, `fixedMessages`에 등록):** 읽기 `tokens`(서명 불필요: `[{ref, kind, last4, expires, set_at, source: "store"|"config", days_left: 정수|null}]` + `warn_days: 14`), 쓰기 `set_token` args `{"ref","kind","expires","value","value_hash","request","issued","change","signature"}`(`value_hash`는 서명된 `fields.value_hash`와 같은 값을 그대로 전달: broker가 서명 메시지를 검증된 구성 요소로 다시 만들 때 필요하고, 값만 바뀐 경우(`value_mismatch`)와 해시를 바꾼 경우(`change_mismatch`)를 구별한다), `delete_token` args `{"ref","request","issued","change","signature"}`(W3a와 같은 순서·규칙: `change` 필수, 정확한 대소문자 키, 서명 검증 후에 요청 ID 소모, 서명자 목록 호출마다 로드). 새 코드: `token_unknown`(삭제·교체 대상 없음), `bad_kind`, `token_too_short`, `token_bad_format`(문자·길이 상한·공백), `value_mismatch`, `token_exists_other_kind`(같은 ref가 다른 종류), `tokens_unavailable`(비밀 저장소·메타데이터가 깨짐), `openai_singleton`(`openai` 종류에 다른 이름). W3a의 코드는 재사용. **`delete_token`은 `openai`가 설정 키로 대체 가능하면 허용, 아무 키도 남지 않아도 허용**(그 뒤 `refine`은 `upstream_error`가 아니라 고정 코드 `ai_key_missing`으로 실패하고 예산·한도는 소모하지 않는다).
- **D6 만료 경고:** broker가 `tokens`와 `status`에 `days_left`(오늘까지 남은 일수, `expires`가 비면 null)와 `expiring`(≤14일 또는 이미 지남) 목록을 준다. 웹은 **프로젝트·토큰 화면 상단**에 고정 문구 배너("토큰 N개가 14일 안에 만료됩니다")를 보인다. 만료일은 사용자가 입력한 값이라 **기계가 검증하지 않는다**(GitLab에 물어보지 않음)고 화면에 쓴다. 이미 지난 만료일로 `set_token`은 `bad_request`.
- **D7 orchd 엔드포인트(`/api/v1`):** `GET /settings/tokens`(broker `tokens` 전달), `POST /tokens/begin`(본문 `{action, subject, fields:{kind, expires}}` — `delete_token`은 `fields:{}` → `{request, issued}`: 서버가 **요청 ID와 발급 시각만** 만들어 돌려줌, 값 없음, 저장·broker 호출 없음), `POST /tokens/message`(본문 `{action, subject, fields, request, issued}` → `{message, change, request, issued, preview}`: 서버가 정규 메시지·`change`·화면용 고정 한국어 문장을 만들어 줌, 값 없음, 저장·broker 호출 없음), `POST /tokens/apply`(본문 `{action, subject, fields, request, issued, signature, value?}`: **값이 들어오는 유일한 요청**, 크기 상한 4 KiB, 접근 로그·원장·오류에 본문 없음, 처리 뒤 가능한 범위에서 버퍼를 0으로 덮어씀(한계는 보고서에 적음), 서버가 `fields`로 `change`를 다시 계산해 맞는지 확인한 뒤 broker로 그대로 전달, 서명 유효성은 broker가 판단), `GET /settings/deploy-key`(orchd가 게시에 쓰는 SSH 키의 **공개 키**: `--git-ssh-key` 경로 + `.pub`를 읽고 형식 검사 `ssh-ed25519 …`/`ssh-rsa …` 한 줄, 없으면 `deploy_key_unavailable`, W3c가 쓴다). 사용자별 한도: `begin`+`message` 합쳐 시간당 60회, `apply` 시간당 20회(W3a와 같은 환급 규칙: Unsent·busy·forbidden만). 원장 `token_change` 이벤트 `{actor, action, subject, kind, ok, code}`(**값·해시·last4 없음**).
- **D8 흐름(값 해시가 요청 ID에 묶이므로 3단계):** (1) 브라우저가 `begin`으로 `request`·`issued`를 받는다. (2) 브라우저가 값과 `request`로 `value_hash`를 계산해 `fields`에 넣는다. (3) `message`로 서버가 만든 정규 메시지·`change`·문장을 받아 확인 화면을 만들고, 브라우저가 `fields`로 다시 계산한 `change`와 같을 때만 서명 단추를 연다. (4) 서명 후 `apply`에 값과 서명을 보낸다. **불변식(바꿀 수 없음):** 값은 `begin`·`message`에 실려 가지 않고 `apply` 본문에만 있다; 서명되는 `value_hash`는 같은 `request`에 묶인다; `begin`·`message`는 아무것도 저장하지 않고 broker에 쓰기 op를 보내지 않는다.
- **D9 웹:** 새 화면 `#/tokens`(메뉴 "토큰"): 표(이름, 종류, 마지막 4자, 만료일·남은 일수·만료 임박 표시, 출처 `저장소/설정 파일`), "추가/교체"(종류 선택, 이름, 값을 **가린 입력**(`type="password"`, `autocomplete="off"`, 붙여넣기 허용), 만료일(`gitlab`·`github`는 필수 권장 표시), 값은 **JS 변수에 최소 시간만** 보관하고 확인 화면 진입·서명 후·취소·이탈 시 `''`로 덮어쓰고 입력 칸도 비움), **확인 화면**: 종류·이름·만료일·값의 마지막 4자·**값 해시 앞 8자**를 보여 주고(값 전체는 다시 보이지 않음) 서명 단추는 서버가 준 `change`와 브라우저가 다시 계산한 값이 같을 때만 열림, 패스키 서명(`signRegistryChange` 재사용), 결과, 삭제(이름 확인 후 서명). 상단 만료 배너. **값이 DOM의 다른 곳·로그·저장소·URL·히스토리에 남지 않고**, 입력 중 화면을 떠나려 하면 W2b `guard.js`로 경고한다(값을 한 글자라도 쳤을 때만).
- **D10 `openai` 핫 리로드:** broker의 OpenAI 클라이언트는 호출마다 `SecretStore.Get("openai")`에서 현재 키를 읽는다(우선순위 D2). `set_token`/`delete_token`이 성공하면 **다음 호출부터** 새 키를 쓴다(재시작 불필요). 진행 중인 호출은 시작할 때 읽은 키로 끝난다. 키가 없으면(`ai_key_missing`) 호출하지 않고 예약도 하지 않는다.

## 검토 초점 (설계서가 암시하지만 개별 작업 시험이 안 다루기 쉬운 것, 가능성 순)
1. **값 유출 경로**: orchd의 접근 로그·패닉·오류·원장, 브라우저의 상태·히스토리·클립보드·URL, broker의 로그·응답·`tokens` 출력(`last4`가 새 정보를 주는지), 서명 메시지·`change`에 값이 섞여 들어가는지(→ B4·B5·B6의 canary 시험).
2. **서명 뒤 값 바꿔치기**: orchd 또는 중간 계층이 서명한 해시와 다른 값을 broker에 넘기는 경우, 같은 서명으로 다른 값 재전송, 요청 ID 재사용(→ B3).
3. **교체 도중 부분 쓰기와 핫 리로드 경합**: 쓰기 중 장애로 반쯤 쓴 키, 호출 진행 중 교체, 교체 실패 시 이전 키 유지(→ B2).
4. **삭제 의미**: 사용 중인 `openai` 삭제 뒤 `refine` 동작(고정 코드, 예산 불소모), 메타데이터와 파일 불일치(파일만 있고 메타 없음 / 메타만 있고 파일 없음)(→ B2·B3).
5. **배포 현실**: broker 샌드박스에서 `secrets/` 쓰기·0400 파일 생성·`rename`·소유자 검사, orchd가 `/var/lib/orch-broker`를 못 읽는 것, `-check`가 새 상태를 확인하는지(→ 각 작업의 "배포 현실" 표, B6).

---

### Task 1 (B1): 서명 메시지 action 확장 (`set_token`, `delete_token`)

**파일:**
- 수정: `internal/registrymsg/registrymsg.go`(스키마·정규식·정규 JSON·`ValueHash` 도우미), `internal/approval/signing/registry_message.go`(action 상수·검증), 각 `_test.go`

**인터페이스:**
- 생산: `registrymsg.ActionSetToken`, `ActionDeleteToken`, 토큰 `ref` 정규식(`subject` 검증을 action별로: 토큰 action은 토큰 ref 규칙, 기존 action은 프로젝트 id 규칙 그대로), 스키마 `set_token`: `kind`(문자열, `openai|gitlab|github`), `expires`(문자열, `""` 또는 `YYYY-MM-DD`), `value_hash`(64자리 소문자 hex); `delete_token`: 필드 없음. `registrymsg.ValueHash(request string, value string) string`. `signing.RegistryMessage`가 새 action을 받는다(기존 action 불변).

- [ ] **Step 1: 실패하는 시험(이름 그대로)** — `TestTokenActionsInSchema`(필드 종류·알 수 없는 필드 거부), `TestTokenRefRegexAndKindSet`, `TestExpiresFormat`(빈 문자열 허용, `2026-13-40`·`2026-1-1`·`20261109`·공백 포함 거부), `TestValueHashIsSaltedAndStable`(요청 ID가 다르면 같은 값도 다른 해시, 값이 한 바이트만 달라도 다름, 64자리 소문자 hex), `TestSigningAcceptsTokenActionsAndStillRejectsUnknown`, `TestExistingActionsUnchanged`(기존 `register_project`·`set_flag` 시험 그대로 통과), `TestDeleteTokenHasNoFields`, `TestSubjectRuleIsPerAction`(토큰 ref가 프로젝트 id 규칙과 다른 점이 있으면 서로 섞이지 않음), `TestSigningDefinitionsDoNotDrift`가 새 action·정규식까지 비교하도록 확장.
- [ ] **Step 2: 시험이 실패하는지 확인** — `go test ./internal/registrymsg/ ./internal/approval/signing/ -count=1`, 기대: 정의되지 않은 심볼.
- [ ] **Step 3: 구현** — W3a의 스키마 표에 항목을 더하는 방식(코드 변경은 표·정규식·도우미). `ValueHash`는 `registrymsg`(순수 패키지)에 둔다. 값이 `change`에 **들어가지 않음**을 시험이 단언한다(canonical JSON에 값 부분 문자열 없음).
- [ ] **Step 4: 통과 확인** — 같은 명령 + 기존 `./internal/approval/...`, `./internal/broker/...` 시험이 그대로 통과.
- [ ] **Step 5: 커밋** — `feat(signing): token actions (set_token, delete_token) with salted value hash`.

---

### Task 2 (B2): broker 비밀 저장소, 메타데이터, OpenAI 핫 리로드

**파일:**
- 만들기: `internal/broker/secretstore.go`, `internal/broker/secretstore_test.go`, `internal/broker/tokenmeta.go`, `internal/broker/tokenmeta_test.go`
- 수정: `internal/broker/server.go`(OpenAI 클라이언트가 호출마다 키를 가져옴, `status`에 만료 요약), `internal/broker/openai.go`, `internal/broker/refine.go`(`ai_key_missing`), `cmd/orch-broker/main.go`(`-check` 확장), `internal/broker/config.go`(필요 시)

**인터페이스:**
- 소비: W3a의 상태 폴더 신뢰·원자 쓰기 도우미(`readStateFile`, `writeAtomic`, `checkStateDir`), 기존 `Secrets`/키 파일 로더.
- 생산: `SecretStore{Dir string}`의 `Get(ref) (value string, ok bool)`, `Put(ref, value) error`(원자적, 0400, 실패 시 이전 값 유지), `Delete(ref) error`(덮어쓰기 후 제거), `Refs() []string`, `TokenMeta`(`tokens.json` 읽기/쓰기, W3a 등록부와 같은 fail-closed 규칙: 없으면 빈 목록, 깨지면 `ErrTokensBroken`), `(*Server).currentOpenAIKey() (string, bool)`(우선순위 D2), 만료 계산 `DaysLeft(expires string, now time.Time) *int`.

- [ ] **Step 1: 실패하는 시험** — `TestSecretStorePutGetDeleteModes`(파일 0400, 폴더 0700, 값이 정확히 그대로), `TestSecretStoreAtomicOnFailure`(쓰기 중간 실패 주입 → 이전 값 유지, 새 값이 반쯤 남지 않음, 임시 파일 정리), `TestSecretStoreRefusesSymlinkAndWrongOwnerOrMode`(그 토큰만 없음으로 취급 + fail closed, 다른 토큰은 영향 없음), `TestSecretStoreDeleteOverwritesThenRemoves`(삭제 뒤 파일 없음, 같은 이름 재생성 가능), `TestTokenMetaBrokenIsFailClosed`(깨진 `tokens.json` → 비밀 쓰기·읽기 거부 `tokens_unavailable`, 파일 불변), `TestTokenMetaMissingFileMeansEmptyOnlyWhenAbsent`, `TestMetaAndFileMismatchHandled`(파일만 있고 메타 없음 → 목록에 `last4` 없이 `source: store`로 보이지 않고 **쓰지 않음**으로 표시? 구현자가 정하고 보고서에 기록하되 값이 새지 않고 fail-closed일 것; 메타만 있고 파일 없음 → 사용 불가), `TestOpenAIKeyPrecedenceStoreOverConfig`, `TestOpenAIKeyHotReloadNextCall`(교체 직후 다음 `refine`이 새 키를 `Authorization`에 실음: 가짜 OpenAI 서버가 받은 헤더 비교, 진행 중이던 호출은 옛 키로 끝남), `TestAIKeyMissingIsFixedErrorAndNoBudgetUse`(`secrets/openai` 삭제 + 설정 키 파일 없음 → `ai_key_missing`, 예약·시간당 슬롯 소모 없음), `TestStatusAndTokensExpiry`(만료 14일 이내·지남·빈 만료일·경계), `TestCheckValidatesSecretsDirAndMeta`(`-check`가 `secrets/` 모드·`tokens.json`을 확인하고 아무것도 쓰지 않음), `TestNoValueInAnyOutput`(canary 값을 `Put`한 뒤 `Get` 외 모든 출력·로그·오류에서 검색 → 없음).
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ ./cmd/orch-broker/ -count=1`.
- [ ] **Step 3: 구현** — 저장·메타 쓰기 코드는 이 두 파일에만 둔다. `secrets/`는 시작 때 없으면 `0700`으로 만든다(상태 폴더 신뢰 검사 통과 시에만). `openai` 키는 `Server`가 `SecretStore`와 설정의 키 파일을 모두 알고 `currentOpenAIKey()`가 D2 순서로 읽는다(설정 키 파일은 W2b의 신뢰 검사를 그대로). OpenAI 클라이언트는 `Key` 고정 필드 대신 키 제공 함수를 쓴다(W2b 시험은 같은 의미로 통과해야 함). `-check`는 읽기 전용.
- [ ] **Step 4: 통과 확인** — `-race` 포함, W2b·W3a broker 시험이 의미 변경 없이 통과(바꾼 것은 이유와 함께 보고서에).
- [ ] **Step 5: 커밋** — `feat(broker): secret store with token metadata, openai hot reload and expiry summary`.

---

### Task 3 (B3): broker 토큰 op와 서명 검증

**파일:**
- 만들기: `internal/broker/tokens.go`, `internal/broker/tokens_test.go`
- 수정: `internal/broker/server.go`(op 등록, `fixedMessages`, 제한 시간), `internal/broker/projects.go`(공통 `verifySigned` 재사용 위해 필요한 만큼만)

**인터페이스:**
- 소비: B1의 메시지·`ValueHash`, B2의 `SecretStore`·`TokenMeta`, W3a의 `verifySigned`(검증 순서, 요청 ID 소모 규칙, 서명자 매 호출 로드).
- 생산: D5의 op 세 개(`tokens`, `set_token`, `delete_token`), 응답 `tokens` 항목, 고정 코드.

- [ ] **Step 1: 실패하는 시험** — `TestTokensReadSecretFree`(값·해시 없음, `last4`·만료·출처만), `TestSetTokenRequiresValidSignature`(W3a와 같은 표: not_signed, unknown_signer, bad_signature, stale_signature, signers_unavailable), `TestSetTokenChangeRequired`(없으면 `change_mismatch`, 요청 ID 불소모), `TestValueMismatchRefused`(서명은 유효한데 `value`가 서명된 `value_hash`와 다름 → `value_mismatch`, 저장소·메타 불변, **요청 ID 불소모가 아니라**: 서명 검증을 통과했으므로 소모한다 — 이 규칙을 표로 고정), `TestValueRules`(31자·513자·공백·개행·비ASCII·앞뒤 공백 → 각각 `token_too_short`/`token_bad_format`), `TestKindAndNameRules`(`bad_kind`, `openai_singleton`, `token_exists_other_kind`), `TestSetTokenReplacesAtomically`(성공 후 `Get`이 새 값, 메타의 `last4`·`set_at` 갱신, 이전 `last4`가 응답에 없음), `TestSetTokenExpiresInPastRefused`, `TestDeleteTokenSignedOnlyAndUnknownRef`, `TestDeleteOpenAIFallsBackToConfigKey`, `TestRequestIDOnceAcrossRestart`(W3a의 사용된 ID 저장소 재사용), `TestExactCaseKeysAndNoUnknownFields`(`Value`, 중복 키 거부), `TestConcurrentSetSameRefOneWins`(`-race`: 같은 요청 ID N개 → 하나 성공 나머지 `request_reused`), `TestNoValueInAnyResponseOrLog`(canary 값으로 모든 성공·실패 경로 → 응답·로그·`tokens` 출력·메타 파일(`last4` 외)에 값 부분 문자열 없음), `TestWriteOpsRefuseWhenTokensBroken`, `TestOpAllowlistDeleteProjectStillUnknown`(프로젝트·토큰 삭제 외의 삭제 계열은 여전히 `unknown_op`), `TestHandlerTimeoutsExplicit`.
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ -count=1 -run 'Token'`.
- [ ] **Step 3: 구현** — W3a `verifySigned`의 순서를 그대로 따른다(엄격 키 → 등록 상태 → `not_signed` → `change` 필수 → 서명자 로드 → 서명 검증 → `change` 일치 → 요청 ID 소모). **값 해시 검사는 서명 검증과 `change` 일치 뒤, 요청 ID 소모 뒤가 아니라 앞에서도 가능하지만 어느 쪽이든 위 시험이 정한 표를 따른다.** 값은 메모리에서만 다루고 오류 문장에 반영하지 않는다. 삭제는 덮어쓰기 후 제거.
- [ ] **Step 4: 통과 확인** — `-race`.
- [ ] **Step 5: 커밋** — `feat(broker): tokens, set_token and delete_token with signature and value-hash verification`.

---

### Task 4 (B4): orchd 엔드포인트, 값 비로깅, 배포 키 공개 키

**파일:**
- 만들기: `internal/orch/api/tokens.go`, `internal/orch/api/tokens_test.go`
- 수정: `internal/orch/broker/client.go`(새 op 호출), `internal/orch/api/{v1.go,api.go,daemon.go}`(경로, 설정, 한도), `cmd/orchd/vm.go`(공개 키 경로는 기존 `--git-ssh-key`에서 유도, 새 플래그 없음)

**인터페이스:**
- 소비: B3의 broker op, W3a의 `apply` 패턴(`projectsApply`: 변경 재계산, 서명 전달, 오류 매핑, 한도, 결과 불확실), `requireWrite`.
- 생산: D7의 엔드포인트 4~5개(`GET /settings/tokens`, `POST /tokens/begin`, `POST /tokens/message`, `POST /tokens/apply`, `GET /settings/deploy-key`), 원장 `token_change`.

- [ ] **Step 1: 실패하는 시험** — `TestTokensListMapsBrokerAndHidesNothingExtra`, `TestBeginAndMessageNeverReceiveValue`(두 엔드포인트의 본문에 `value`가 있으면 거부 `bad_request`), `TestBeginReturnsOnlyRequestAndIssued`(저장·broker 호출 없음, 요청 ID는 `crypto/rand`, `issued`는 초 단위 UTC), `TestMessageBuildsCanonicalAndStoresNothing`(`change`가 `registrymsg`로 다시 계산한 값과 같음, 가짜 broker가 쓰기 호출을 받지 않음), `TestApplyRecomputesChangeAndRefusesMismatch`(`fields`·`value_hash`와 다른 `change`, broker 호출 없음), `TestApplyForwardsValueExactlyOnceAndNeverLogsIt`(canary 값: 가짜 broker가 받은 바이트는 같고, orchd 로그·원장·응답·오류·패닉 복구 출력에 값 없음, 성공·모든 오류 경로), `TestApplyBodyCap4KiB`, `TestApplyRequiresWriteTierAndCSRF`, `TestErrorMappingAllNewCodesFixed`(broker 새 코드 모두 고정 문장·상태, broker 문장 미전달), `TestWriteTimeoutIsOutcomeUnknown`(재시도 없음), `TestRegistryUnavailableOnWriteIsOutcomeUnknown`(W3a와 같은 규칙), `TestTokenRateLimits`(propose 30/h, apply 20/h, 주체별 독립, 환급은 Unsent·busy·forbidden만), `TestLedgerTokenChangeHasNoValueHashOrLast4`, `TestDeployKeyEndpoint`(`.pub` 읽기, 형식 검사, 없음 → 고정 코드, 개인 키 파일은 **열지 않음**: 시험이 개인 키 경로를 만들어 두고 읽히지 않았음을 확인), `TestTokensWorkWithBrokerDown`(기존 기능 그대로, 읽기·쓰기 `broker_unavailable`), `TestNoBrokerImportInOrchd`, `TestRequestBodyNotRetainedAfterApply`(핸들러가 본문 버퍼를 보관하지 않는지: 구현이 허용하는 범위에서 값이 든 슬라이스를 덮어씀을 확인), `TestConstantsAgreeWithWeb`가 토큰 상수(정규식·길이·종류·만료 형식)를 비교하도록 확장하고 **웹 파일이 있어야 통과**(`t.Fatal`).
- [ ] **Step 2: 실패 확인** — `go test ./internal/orch/api/ -count=1 -run 'Token|DeployKey'`.
- [ ] **Step 3: 구현** — 값은 `apply`의 JSON 본문에서 읽어 **지역 슬라이스로만** 다루고, 요청 구조체를 로그에 찍는 코드가 없는지 확인한다(기본 `%v` 출력 포함: 값 필드는 `String()`이 `[redacted]`를 주는 타입으로 감싼다). 패닉 복구 로그에 요청 본문이 들어가지 않게 한다. 서명 유효성은 판단하지 않고 broker가 한다(W3a 규칙). 공개 키는 `<--git-ssh-key>.pub`만 읽고 형식을 검사한다.
- [ ] **Step 4: 통과 확인** — `-race`, 기존 api 시험 의미 변경 없이 통과.
- [ ] **Step 5: 커밋** — `feat(api): token list, begin, message and apply endpoints and the deploy key public half`.

---

### Task 5 (B5): 웹 — 토큰 화면

**파일:**
- 만들기: `internal/orch/web/static/views/tokens.js`, 시험 `internal/orch/web/test/tokens.test.js`
- 수정: `static/shell.js`(메뉴), `static/router.js`·`app.js`(필요 시), `static/webauthn.js`(필요 시 `signRegistryChange`가 새 action 메시지를 받음 확인만), `style.css`, `internal/orch/api/web_fixtures_test.go`(토큰 목록·propose·message·apply 응답을 **실제 Go 구조체**에서 생성), 시험 도우미 `test/fake-ctx.js`

**인터페이스:**
- 소비: B4의 엔드포인트, W3a의 `computeChange`·`signRegistryChange`·`guard.js`, W2b·W3a의 `findHostile`·`submitErrorText`·`noRetry429`.
- 생산: `#/tokens` 화면, `computeValueHash(request, value) → hex`(브라우저 SHA-256, Go `registrymsg.ValueHash`와 같은 입력 바이트 규칙), 만료 배너 컴포넌트(프로젝트 화면 상단에도 사용).

- [ ] **Step 1: 실패하는 시험(JS, 이름 그대로)** — `TestValueHashMatchesGoVector`(Go가 만든 고정 벡터 JSON을 읽음), `TestTokensListRendersAsText`(악성 ref·종류·last4: 텍스트 노드만, `<img onerror>`·`javascript:` 속성에 안 감), `TestValueNeverInDOMOrStateAfterSubmit`(제출·취소·이탈·오류 뒤 값이 입력 칸·변수·`document` 어디에도 없음: 정적 검사 + 동작 시험), `TestPasswordInputAttributes`(`type=password`, `autocomplete=off`, 저장소 API 미사용), `TestConfirmShowsKindRefExpiryLast4AndHashPrefixOnly`(값 전체는 확인 화면에 없음), `TestSignButtonOnlyWhenChangeMatches`(서버 `change`·`message`가 브라우저 재계산과 같을 때만), `TestFlowOrderBeginMessageSignApply`(`begin`→`message`→서명→`apply`, 값은 `apply` 본문에만, `begin`·`message` 본문에 값 없음, `value_hash`가 `begin`이 준 `request`로 계산됨), `TestDeleteNeedsNameConfirmAndSignature`, `TestExpiryBannerWithin14DaysAndPast`(고정 문구, 만료일은 사용자 입력이라 검증되지 않는다는 안내 포함), `TestExpiresRequiredHintForGitHostTokens`, `TestApplyErrorsFixedTexts`(B3·B4의 모든 새 코드 → 고정 문구, 서버 원문 미표시), `TestOutcomeUnknownReloadsList`, `TestSubmitOnceNoDoubleSign`, `TestLeaveGuardOnlyWhenValueTyped`(값을 한 글자라도 쳤을 때만 경고, 제출·취소 뒤엔 해제, 가두지 않음), `TestNoStorageNoNavigateTrap`(저장소 API 미사용 정적 검사, `ctx.navigate` 의존 없음, fake-ctx는 navigate 미제공), `TestNoRetry429ForTokenWrites`. 기존 정적 위험 API 검사와 `TestWebAssetsEmbedded`가 새 파일을 포함해 통과.
- [ ] **Step 2: 실패 확인** — `node --test "internal/orch/web/test/tokens.test.js"`.
- [ ] **Step 3: 구현** — 값은 입력 이벤트에서만 읽어 지역 변수로 두고, 해시 계산·서명 직전에 필요한 만큼만 쓰고, 단계가 끝나면 입력 칸을 비우고 변수를 `''`로 덮는다(JS 문자열은 불변이라 메모리에서 지울 수 없다는 한계를 보고서·6장에 적는다). 확인 화면은 값 전체를 다시 그리지 않는다. 서버가 준 문장·오류는 모두 `el()` 텍스트 노드.
- [ ] **Step 4: 시각 확인** — `ego-browser skill`을 먼저 읽고, 개발용 픽스처 서버(`web/dev_test.go`, `ORCH_WEB_DEV`)에 토큰 목록(만료 임박 포함)·begin/message/apply 성공·실패 픽스처를 추가해 1280px·390px 화면을 보고 **본 것만** 보고서에 쓴다. **실제 앱 경로(`app.js`)**로 목록 → 추가 → 확인 → 서명 → 결과, 교체, 삭제까지 걷는다. 값이 네트워크 요청 본문 외 어디에도 안 나가는지 브라우저 개발 도구 수준(요청 목록, 히스토리)으로 확인한 것만 쓴다. 패스키는 W3a와 같이 대역이므로 실제 인증기 검증은 아니라고 쓴다.
- [ ] **Step 5: 변이 확인(복사본)** — 값을 `begin`/`message` 본문에 넣음 → 시험 실패; `change` 비교 제거 → 실패; 확인 화면에 값 전체를 그림 → 실패; 서버 문장을 속성에 넣음 → 시험·정적 검사 실패.
- [ ] **Step 6: 통과 확인 + 커밋** — `go test ./internal/orch/web/... ./internal/orch/api/... -race`, node 전체, `TestConstantsAgreeWithWeb` PASS(SKIP 없음); `feat(web): token menu with signed changes and expiry banner`.

---

### Task 6 (B6): 적대 시험, 결과 문서, 컷오버 런북

**파일:** 만들기 `internal/broker/adversarial_w3b_test.go`, `internal/orch/api/adversarial_w3b_test.go`, `docs/23-RESULTS-webconsole-W3b.md`; 수정 `docs/10-OPERATIONS-orchestrator.md`(W3b 컷오버 절 13장), `docs/19-DESIGN-webconsole-W3-projects.md`(상태 한 줄과 W3b 변경 반영), `infra/README.md`

- [ ] **적대 시험(모두 거부/무효):** broker: `TestSecretsNeverReadableByOtherUIDs`(파일 모드·소유자 단언), `TestValueSwapAfterSignatureRefused`(서명한 해시와 다른 값 → `value_mismatch`, 저장소 불변), `TestSignatureReplayAcrossRestartRefused`, `TestSetTokenForOtherRefOrKindRefused`(서명한 `ref`·`kind`와 다른 요청 → `bad_signature`/`change_mismatch`), `TestApprovalAndRegistryMessagesStillSeparate`(approval 서명으로 토큰 변경 불가, 프로젝트 변경 서명으로 토큰 변경 불가), `TestTokenFilesNeverInRegistryOrBudget`, `TestBrokenTokenMetaDoesNotDisableOtherFeatures`(토큰 메타가 깨져도 프로젝트 조회·`refine`의 설정 키 경로는 안전하게 동작하거나 명확히 거부 — 구현이 정한 규칙대로 단언), `TestDeleteThenRefineFailsClosedNoBudget`, `TestHotReloadRaceNoTornKey`(`-race`: 교체와 `refine` 동시, 헤더의 키는 옛 키나 새 키 중 하나이고 섞이지 않음), `TestNoValueInLogsPanicsStatusTokens`. orchd: `TestTokenValueNeverInLedgerLogsErrorsAnyPath`, `TestApplyForgedFieldsRefusedBeforeBroker`, `TestBeginAndMessageNeverWriteBroker`, `TestOrchdWorksWithBrokerDown`(확장), `TestNoBrokerImportInOrchd`, `TestDeployKeyNeverReadsPrivateKey`.
- [ ] **변이 확인(저장소 복사본에서만, 작업 트리에 남기지 않음):** ① 서명 검증 제거 ② `value_hash` 일치 검사 제거 ③ `change` 일치 제거 ④ 요청 ID 1회성 제거 ⑤ 파일 모드 0400→0644 ⑥ 쓰기 원자성 제거(직접 덮어쓰기) ⑦ 핫 리로드 대신 시작 때 한 번만 읽기 ⑧ 삭제 때 파일 남김 ⑨ orchd가 값을 로그에 찍음 ⑩ 웹에서 값을 `begin`/`message`에 실음 ⑪ 길이·문자 규칙 제거 ⑫ `-check`의 `secrets/` 검사 제거. 각각 어느 시험이 실패했는지 표로 기록(안 걸리면 시험을 추가하고 다시).
- [ ] **결과 문서(`docs/23`):** W3a의 `docs/21` 구조: 만든 것, 시험 목록(실제 개수), 변이 표, 설계서와 달라진 곳, **배포 현실 표**(경로·모드·소유자·실행 사용자·샌드박스 조건), **실서버에서 확인하지 못한 것**(broker가 샌드박스 안에서 `secrets/`에 쓰는지, 실제 패스키로 `set_token` 서명이 검증되는지, 실제 OpenAI 키를 웹에서 교체한 뒤 `refine`이 새 키를 쓰는지, 삭제 뒤 설정 키 폴백, JS 문자열을 메모리에서 지울 수 없는 한계, `last4` 노출, 토큰이 orchd 메모리를 거치는 한계, 키보드·스크린 리더), 사용자 결정 목록.
- [ ] **컷오버 런북(W3b, `docs/10` 13장):** W3a 런북 양식(순서 고정, 단계마다 백업, `[사용자]`/`[주의]`/`[되돌림 가능]`, 검증 체크리스트와 "그래도 안 되면" 줄, **컨트롤러가 서버에서 직접 읽기 전용으로 확인하는 사전 점검 목록**). 0단계: W3a 컷오버 상태가 출발점, 실행 중 작업 없음, **`/var/lib/orch-broker`가 `orch-broker` 0700**(`secrets/`가 여기 생긴다), 백업은 `/root/backup-w3b`(0700, `orch`가 쓸 수 없는 곳). 1단계 broker 갱신(정지 → 바이너리 → 샌드박스 `-check`(secrets 폴더 생성 가능 여부를 `ReadWritePaths` 하에서 봄) → 시작): 새 `secrets/`가 `orch-broker:orch-broker 700`으로 생기는지, `tokens`가 **`openai` 한 항목만 `source: config`**로 보이는지, 기존 `refine`이 그대로 도는지(회귀). 2단계 orchd 갱신. 3단계 웹 확인: 토큰 화면, **실제 OpenAI 키 교체는 사용자 결정**(비용: 이후 한 번의 AI 호출 필요, 키는 사용자가 직접 입력하고 이 대화에 붙이지 않음), GitLab 토큰 등록은 W3c 준비용(사용자가 토큰을 줄 때). 4단계 롤백(바이너리와 `broker.json` 짝, `secrets/`와 `tokens.json` 삭제 — 삭제하면 웹에서 등록한 토큰이 사라지고 `openai`는 설정 키로 돌아감을 경고).
- [ ] **`go test ./... -count=1 -race` 전체 + node 테스트 + `TestConstantsAgreeWithWeb -v`(PASS, SKIP 없음)**, 커밋 `test: adversarial suite for W3b`, `docs: W3b results and cutover runbook`.

---

## 자체 검토 기록
- **설계서 대비 범위:** 4.2(서명 검증)·4.5(`set_token`/`delete_token`)·5.1(토큰 메뉴)·6장(토큰 위협: 값이 orchd를 거침, `last4` 노출)·7장(해시를 솔트와 함께 서명)·10장 W3b 행(토큰 메뉴, 만료 경고, 배포 키 공개 키 조회) → B1~B6. **의도적으로 뺀 것:** 제공자 설정과 `token_ref` 사용(W3c), 그룹·프로젝트 생성, GitLab에 만료일을 묻는 일, 자동 병합, GitHub 어댑터, 기존 `create_mr`의 토큰 파일 대체.
- **설계서와 달라진 점(이 계획이 정한 것):** 값의 해시를 서명하기 위해 `request`를 브라우저가 먼저 알아야 해서 `begin` → `message` → 서명 → `apply` 3요청 흐름으로 정했다(D8; 불변식은 바꿀 수 없음); `openai` 키는 설정 파일 대신 broker 상태 폴더의 저장소가 우선(설정 파일은 `/etc`라 broker가 쓸 수 없음, D2); `gitlab`·`github` 토큰은 W3b에서 **소비자가 없음**(저장만).
- **이름 일관성:** op `tokens`/`set_token`/`delete_token`, 오류 코드(`token_unknown`, `bad_kind`, `token_too_short`, `token_bad_format`, `value_mismatch`, `token_exists_other_kind`, `tokens_unavailable`, `openai_singleton`, `ai_key_missing`), 값 해시 `SHA-256(request ‖ ":" ‖ value)`, `ValueHash`/`computeValueHash`, 파일 `secrets/<ref>`·`tokens.json`이 B1~B6에서 같다.
- **미결(구현 중 구현자가 정하고 보고서에 기록):** D8 `begin`/`message` 응답 필드의 세부, 메타만 있고 파일이 없는 상태(또는 반대)의 표시·처리, `last4`를 비밀 노출로 볼지에 대한 화면 문구, 만료 배너 문구, `ai_key_missing`의 HTTP 상태.
- **사용자 확인이 필요한 값:** 없음(구현·시험에는 필요 없음). 컷오버에서 **실제 OpenAI 키를 웹으로 교체해 볼지**와 **GitLab 관리자 토큰을 언제 줄지**는 사용자 결정(런북 3단계).
