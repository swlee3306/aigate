# 웹 콘솔 W3b — 결과 기록 (2026-11-10)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w3b`)에서 끝났고, 실제 서버(orch-vm)에는 아직 아무것도 배포하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 13장, 설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `22-PLAN-webconsole-W3b.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 broker 클라이언트·가짜 OpenAI 서버·임시 폴더·명백한 더미 값**)과 ego-browser로 개발용 고정 서버를 본 것만 가리킨다. **실제 패스키 서명, 리눅스 systemd 샌드박스 안의 broker, 실제 `uid`(`orch-broker`)의 파일 소유·모드, 실제 OpenAI 키, 실제 브라우저의 "비밀번호 저장" 동작은 한 번도 쓰지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다. 시험 값은 `canary-NOT-A-REAL-TOKEN-…` 꼴의 더미다.
> **W3a의 교훈(임시 폴더 시험이 실서버의 권한 문제를 숨겼음, 21번 문서 8장)을 이번에 구조에 넣었다:** 새 코드가 실행 사용자·유닛 샌드박스 아래에서 만지는 모든 경로·소유자·모드를 **4장의 배포 현실 표**로 모았고, 컨트롤러가 컷오버 전에 서버에서 읽기 전용으로 보는 사전 점검 목록을 런북 13.0에 따로 뒀다. 이 저장소의 시험은 소유자·모드 비트를 흉내 낼 뿐(`fileOwnerOK`·`OwnerOK` 시임, `chmod`) **실제 uid 999와 실제 샌드박스를 거친 적이 없다.**

## 1. 만든 것

**B1. 서명 메시지에 두 동작 추가.** `aigate-registry-v1`에 `set_token`(`subject` = 토큰 이름, `fields` = `{kind, expires, value_hash}`)과 `delete_token`(`fields` = `{}`)을 더했다(`internal/registrymsg`, `internal/approval/signing`). `value_hash` = `SHA-256(request ‖ ":" ‖ 값)`의 hex: 값이 아니라 **요청 ID를 솔트로 섞은 해시**를 서명하므로 서명과 값이 같은 요청 ID에 묶인다. `ValueHash`는 `request`가 소문자 hex 32자가 아니면 빈 문자열을 돌려주고(호출자가 거부), 만료일은 `""` 또는 실제 달력 날짜(연도 2000~9999)다. 기존 동작(`register_project`, `set_flag`)의 의미와 시험은 그대로.
**B2. broker의 비밀 저장소.** `secrets/<이름>`(0400, 폴더 0700, `orch-broker` 소유, 일반 파일·링크 수 1·심볼릭 링크 아님)와 `tokens.json`(0600, 값 없음, 엄격한 문법·한도)을 `TokenStore`가 함께 다룬다. 쓰기는 임시 파일(0600→0400) → `fsync` → `rename` → 폴더 `fsync`이고, 어느 단계에서 실패해도 **이전 값이 그대로 남고**(rename 뒤 실패면 이전 값을 다시 쓴다) 임시 파일은 시작 때 무조건 치운다(0으로 덮은 뒤 삭제). 소유자·모드·링크가 어긋난 파일은 **그 토큰만** 없는 것으로 보고(다른 토큰·등록부는 영향 없음), `secrets/` 폴더나 `tokens.json`이 어긋나면 저장소 전체가 `tokens_unavailable`이다. `openai` 키는 저장소(`secrets/openai`)가 있으면 그것, 없으면 설정의 키 파일을 **호출마다** 읽는다(핫 리로드: 진행 중인 호출은 시작할 때 읽은 키로 끝남). 키가 하나도 없으면 `refine`은 호출·예약 없이 고정 코드 `ai_key_missing`으로 실패한다. `status`에 `tokens_available`·`warn_days`·`expiring`(이름과 남은 일수만, `last4` 없음), `-check`에 `secrets/`·`tokens.json` 읽기 전용 검사.
**B3. broker의 `tokens`·`set_token`·`delete_token`.** W3a의 검증 순서(엄격한 인자 → 서명 있음 → `change` 필수 → 서명자 목록 호출마다 다시 읽음 → `VerifyRegistry` → 서명된 `change` == 다시 계산한 `change` → **그제야** 요청 ID 1회성)에 값 검사를 더했다: 서명된 `value_hash`와 받은 값으로 다시 계산한 해시가 같아야 하고(`value_mismatch`), 값은 32~512자의 인쇄 가능한 ASCII(0x21~0x7e, 공백·제어·비ASCII·앞뒤 공백 거부, 자동으로 자르지 않음)이며, 이미 지난 만료일은 `bad_request`. 오류는 전부 고정 코드·고정 문장(`token_unknown`, `bad_kind`, `token_too_short`, `token_bad_format`, `value_mismatch`, `token_exists_other_kind`, `tokens_unavailable`, `openai_singleton`, `ai_key_missing`). `set_token`은 최상위 인자 `value_hash`를 더 가진다(D5 보정, 5장). 핸들러가 패닉하면 로그에는 패닉 값이 아니라 **형식(`%T`)만** 남긴다(저장되지 않은 값이 새는 길을 막음).
**B4. orchd 엔드포인트.** `GET /api/v1/settings/tokens`(broker 목록 전달, 행을 다시 한도 안으로 검사), `POST /api/v1/tokens/begin`(요청 ID·발급 시각만 만듦), `POST /api/v1/tokens/message`(정규 메시지·`change`·고정 한국어 문장), `POST /api/v1/tokens/apply`(값이 들어오는 **유일한** 요청: 본문 4 KiB 상한, 값은 문자열이 아니라 바이트로, 모든 형식 출력에서 `[redacted]`, 처리 뒤 버퍼를 0으로 덮음, 서버가 `fields`로 `change`를 다시 계산해 비교, 값과 `value_hash`·D3 규칙을 broker 호출 **전에** 확인해 한 번뿐인 요청 ID가 헛되이 소모되지 않게 함), `GET /api/v1/settings/deploy-key`(`--git-ssh-key` 경로 + `.pub`만 읽어 공개 키와 지문을 줌, 개인 키는 열지 않음, W3c용). 주체별 한도: `begin`+`message` 합쳐 시간당 60, `apply` 시간당 20. 원장 `token_change`는 `{actor, action, subject, kind, ok, code}`뿐(값·해시·`last4`·요청 ID·서명 없음). 쓰기의 `timeout`·`tokens_unavailable`·`registry_unavailable`·`internal_error`·이상한 성공 응답은 **`outcome_unknown`(504)**이다.
**B5. 웹.** `#/tokens` 화면(목록, 추가/교체, 이름 확인 뒤 삭제)과 만료 배너(`days_left ≤ warn_days`, 토큰·프로젝트 화면 상단). 흐름은 `begin` → `message` → 확인 화면(종류·이름·만료일·값의 마지막 4자·**값 해시 앞 8자**, 값 전체는 다시 안 보임; 서명 단추는 서버가 준 `change`와 브라우저가 다시 계산한 값이 같을 때만 열림) → 패스키 서명 → `apply`. 값은 `begin`·`message`에 실리지 않고 `apply` 본문에만 있다. 입력 칸은 서명 전에 D3 규칙을 브라우저에서 미리 검사한다. 값 보관은 **한 변수**이고 확인 화면에서 입력 칸을 비우며 `apply` 본문을 만들 때·취소·오류·이탈에서 덮어쓴다. 입력 중 이탈은 `guard.js`로 막는다(값을 한 글자라도 쳤을 때만).
**B6. 적대 시험, 이 문서, 컷오버 런북.** 2장.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-10, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0. 세는 방식은 W3a(21번 문서 2장)와 같다: 최상위 시험 함수와 하위 시험(`t.Run`)을 따로 센다(`go test -json` 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**, 표준 오류 출력 없음. 최상위 시험 함수 1294개 중 **1292 통과, 0 실패, 2 건너뜀**(하위 시험 833개 모두 통과). 건너뜀 2개는 W1~W3a와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollectionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사). 이 숫자는 B6 시험을 더한 **최종 코드**에서 한 번에 돌린 결과다(커밋 `test: adversarial suite for W3b` 시점의 트리).
- `node --test "internal/orch/web/test/*.test.js"`: **193개 전부 통과**(W3a 165개 + W3b 28개: `tokens.test.js`와 `guard.test.js`의 새 시험)
- **B5 게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, `--- PASS: TestConstantsAgreeWithWeb/tokens`, **`SKIP` 없음**(웹 파일이 없거나 상수가 어긋나면 `t.Fatal`이므로 건너뛰는 경로가 없다. 토큰 하위 시험 `tokens`도 PASS). 이 명령은 런북 13장 1-0에 들어 있다.
- W3a(21번 문서) 대비 최상위 통과 1198 → 1292(+94), 하위 시험 804 → 833(+29). 늘어난 곳: `internal/broker` 139 → 191(+52), `internal/orch/api` 265 → 292(+27), `internal/registrymsg` 4 → 13(+9), `internal/approval/signing` 63 → 65, `internal/orch/broker` 5 → 6, `cmd/orch-broker` 11 → 12, `cmd/orchd` 31 → 32, `internal/orch/web` 9 → 10. 새 패키지는 없다. B6의 새 시험은 broker 9개 + orchd 4개(`TestBeginAndMessageNeverWriteBroker`, `TestTokensApplyForgedFieldsRefusedBeforeBroker`, `TestTokensApplyValueRulesBeforeBroker`(변이 ⑪ 때 추가), `TestDeployKeyNeverReadsPrivateKey`)다.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 12 | 0 | 0 | 0 |
| `cmd/orchd` | 32 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 65 | 0 | 0 | 76 |
| `internal/broker` | 191 | 0 | 0 | 136 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 45 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 292 | 0 | 0 | 30 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 6 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 17 | 0 | 0 | 0 |
| `internal/orch/refine` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 43 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 10 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/registrymsg` | 13 | 0 | 0 | 0 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

### B6가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3b_test.go`(9개)와 orchd `internal/orch/api/adversarial_w3b_test.go`(4개, 변이 ⑪ 뒤 하나 추가). 브리프에 이름이 있는 것 중 B2~B5가 이미 같은 일을 하는 것은 **복사하지 않고 아래 표에 교차 참조**했다.

| 브리프의 시험 | 상태 | 하는 일 |
|---|---|---|
| `TestSecretsNeverReadableByOtherUIDs` | **새로 씀**(broker) | 서명된 `set_token`·교체·삭제·실패한 쓰기를 거친 뒤 상태 폴더 **전체**를 걸으며 모든 항목의 소유자 = broker uid, 그룹·기타 비트 없음, 심볼릭 링크 없음, `secrets/` 0700, 비밀 파일 정확히 0400·링크 수 1, `tokens.json` 0600, 삭제·실패한 토큰의 파일 없음(임시 파일 포함)을 단언. 이어서 다른 사용자가 읽을 수 있게 모드를 넓히면(0440·0404·0444·0640·0600·0500·0700) 그 토큰만 안 쓰이고(`openai`는 설정 키로 폴백, 이웃 토큰은 영향 없음), 소유자가 다른 파일(소유자 검사가 거부하는 시임)은 안 쓰이며, `secrets/`가 읽을 수 있는 모드(0750·0705·0770·0755)면 저장소 전체가 꺼져 **쓰기가 거부되고 파일이 생기지 않으며**, 상태 폴더가 0755면 열기를 거부함을 단언 |
| `TestValueSwapAfterSignatureRefused` | **새로 씀**(broker) | 서명한 값과 다른 값(다른 값, 첫·가운데·마지막 글자, 한 글자 추가, 대소문자, 비ASCII 모양 글자, 빈 값, 끝 공백) 9가지 → 전부 `value_mismatch`, 저장소 불변. 다른 요청 ID의 `value_hash`를 끼우면 `change_mismatch`(서명된 `change`가 해시를 묶음). 바꿔치기 시도가 서명된 요청 ID를 **소모**하므로 정직한 값으로 같은 서명을 다시 보내도 `request_reused`. 새 서명은 통과 |
| `TestSignatureReplayAcrossRestartRefused` | **새로 씀**(broker) | `set_token`과 `delete_token` 서명을 쓴 뒤(토큰은 이미 지워짐) 실행 중과 **재시작 2번 뒤** 같은 바이트·다른 값·같은 서명으로 다시 보내면 전부 `request_reused`(`token_unknown`이 아님: 요청 ID 파일이 막은 것), 삭제된 토큰이 되살아나지 않음. 기존 `TestRequestIDOnceAcrossRestart`의 확장 |
| `TestSetTokenForOtherRefOrKindRefused` | **새로 씀**(broker) | `gitlab-a`/`gitlab`로 서명한 요청을 다른 이름·다른 종류·`openai`로 바꿔 보냄(공격자가 `change`·해시를 다시 계산해도 서명은 못 바꿈) → `bad_signature` 또는 `change_mismatch` 또는 `openai_singleton`, 저장소 불변이고 **요청 ID도 소모되지 않음**, 이름이 다른 `delete_token`도 `bad_signature`. 정직한 요청은 이후에도 통과 |
| `TestApprovalAndRegistryMessagesStillSeparate` | **새로 씀**(broker, ssh와 WebAuthn 서명자 둘 다) | 작업 승인 서명, `register_project`·`set_flag`·`delete_token` 서명(같은 요청 ID로)으로 `set_token`·`delete_token`을 승인할 수 없고(`bad_signature`), 반대로 `set_token`·`delete_token` 서명으로 `set_flag`·`register_project`를 승인할 수 없으며 승인으로도 검증되지 않음. 등록부·토큰 저장소 불변 |
| `TestTokenFilesNeverInRegistryOrBudget` | **새로 씀**(broker) | 토큰 설정·교체·삭제가 `registry.json`·`budget.json`을 한 바이트도 바꾸지 않고, 프로젝트 변경이 토큰 파일을 바꾸지 않으며, `registry.json`·`used-requests.json`·`budget.json`·`tokens.json`에 값·해시·`last4`·토큰 이름·프로젝트 경로가 서로 섞여 들어가지 않고, `projects`·`status` 출력에 값·`last4`가 없음 |
| `TestBrokenTokenMetaDoesNotDisableOtherFeatures` | **새로 씀**(broker) | 저장소에 `openai` 키가 있는 상태에서 `tokens.json`을 깨뜨리고 재시작: 토큰 op 셋(`tokens`·`set_token`·`delete_token`)은 `tokens_unavailable`(ID 소모 없음, 깨진 파일은 건드리지 않음), `projects`·`status`·`register_project`·`set_flag`는 그대로 동작, `refine`은 **설정 키**로 호출되고(저장소 키는 메타데이터를 믿을 수 없으므로 쓰지 않음) 설정 키도 없으면 `ai_key_missing`이며 upstream 호출 0 |
| `TestDeleteThenRefineFailsClosedNoBudget` | **새로 씀**(broker) | 설정 키가 없고 저장소 키만 있는 상태: 저장소 키로 호출됨 → `delete_token` → `refine` 5번 모두 `ai_key_missing`, 예산·시간당 슬롯·예산 파일·upstream 호출 불변, 삭제된 키는 어디서도 안 나옴(`secrets/` 비어 있음, 저장소를 다시 열어도 없음), 소켓 응답은 고정 문장이고 값 없음. 기존 `TestOpenAIKeyViaSignedHandlersReachesRefine`의 반대 방향 |
| `TestHotReloadRaceNoTornKey` | **새로 씀**(broker, `-race`) | 한 고루틴이 `openai` 키를 둘(긴 공통 접두사를 가진 두 값) 사이로 계속 교체·삭제하고, 세 고루틴이 `currentOpenAIKey`를 읽고, 실제 `refine`을 20번 호출: 가짜 OpenAI가 본 `Authorization` 헤더는 항상 설정 키·키 A·키 B 셋 중 하나(섞인 키 없음), 경합이 끝난 뒤 마지막으로 교체한 키가 **다음 호출부터** 쓰임 |
| `TestNoValueInLogsPanicsStatusTokens` | **교차 참조** | broker: `TestNoValueInAnyResponseOrLog`(성공·실패 14경로와 패닉, 응답·로그·`tokens`·`status`·상태 폴더의 비밀 아닌 파일 전부), `TestNoValueInAnyOutput`(B2, `%v/%+v/%#v`), `TestPanicLogsTypeNotPayload`(저장 안 된 값을 실은 패닉) |
| `TestTokenValueNeverInLedgerLogsErrorsAnyPath` | **교차 참조** | orchd: `TestApplyForwardsValueExactlyOnceAndNeverLogsIt`(성공, JSON 이스케이프된 값, broker 코드 24개 × set/delete, 연결 끊김, 쓰레기 응답, 로컬 거부 15가지, 큰·깨진 본문, 한도, 패닉, broker 없음; 모든 응답·`log` 출력·원장·gate home 아래 모든 파일을 검색), `TestLedgerTokenChangeHasNoValueHashOrLast4`, `TestPanicAfterBrokerWriteIsRecorded`, `TestRefusedBodyWipesTheValueCopy`, `TestRequestBodyNotRetainedAfterApply` |
| `TestApplyForgedFieldsRefusedBeforeBroker` | **새로 씀**(orchd, 이름은 `TestTokensApplyForgedFieldsRefusedBeforeBroker`: W3a의 같은 이름이 이미 있음) + 기존 `TestTokensApplyRecomputesChangeAndRefusesMismatch` | 위조·기형 `apply` 본문 26가지(여분 키, 여분 `fields` 키, `value_hash` 누락·대문자·다른 요청의 해시, 다른 흐름의 요청 ID·짧은·대문자 요청 ID, 깨진 발급 시각, 서명 없음·문자열·여분 필드, 다른 동작, 대문자·공백 있는 이름, `openai` 종류에 다른 이름, 값 없음·null·숫자·공백·개행·짧음·다른 값, 다른 형식의 만료일)과 **중복 키**: 전부 4xx, broker 호출 0, 값 반향·로그 없음, 정직한 요청은 이후 정확히 1번 broker에 감 |
| (변이 ⑪ 때 추가) | **새로 씀**(orchd) | `TestTokensApplyValueRulesBeforeBroker`: 해시는 맞지만 D3 규칙을 어기는 값 12가지(31자, 1자, 513자, 앞·뒤·가운데 공백, 개행, 탭, NUL, DEL, 비ASCII, 짧으면서 공백)가 broker 호출 없이 브로커와 같은 코드로 거부되고, 경계 32자·512자는 통과 |
| `TestBeginAndMessageNeverWriteBroker` | **새로 씀**(orchd) | 4종류 흐름 × 6회와 거부되는 요청 후에도 `begin`·`message`는 broker를 **한 번도 부르지 않고**(읽기도 없음) 원장 줄·파일을 만들지 않고 로그에 값이 없음. 이어지는 `apply` 한 번이 broker 쓰기 정확히 한 번 |
| `TestOrchdWorksWithBrokerDown` | **교차 참조** | `TestOrchdWorksWithBrokerDown`(W2b)와 `TestTokensWorkWithBrokerDown`(B4: 소켓 없음·아무도 안 듣는 소켓 둘 다 — 목록·`apply`는 고정 `broker_unavailable`, `begin`·`message`는 동작, 기존 기능 그대로) |
| `TestNoBrokerImportInOrchd` | **교차 참조** | W3a의 `TestNoBrokerImportInOrchd`(`go list -deps ./cmd/orchd`에 `internal/broker`가 없음, 양성 대조 포함). 비밀 저장소·OpenAI 클라이언트는 `internal/broker` 안이므로 같은 시험이 이번에도 지킨다 |
| `TestDeployKeyNeverReadsPrivateKey` | **새로 씀**(orchd) + 기존 `TestDeployKeyEndpoint` | 개인 키만 있고 `.pub`이 없음, `.pub`이 개인 키의 **하드 링크**·**심볼릭 링크**·폴더, 플래그가 `.pub` 이름을 가리킴 → 전부 고정 `deploy_key_unavailable`이고 개인 키 바이트 없음. 대조: 모드 000인 개인 키 + 올바른 `.pub` → 200과 두 키뿐. (코드가 개인 키를 열지 않는 것은 B4의 `TestDeployKeyEndpoint` (b)(c)와 변이로도 확인) |

**시험을 새로 쓰는 중 발견한 것:** ① 값 규칙을 orchd에서 지우는 변이(11b·11d)가 처음에는 **어떤 시험도 실패시키지 않았다**(B4의 시험은 "값에 canary가 안 샌다"를 보았을 뿐 코드를 단언하지 않았음) → `TestTokensApplyValueRulesBeforeBroker`를 더하고 다시 돌려 잡았다. ② 비밀 파일 모드 검사는 `Perm()`만 보므로 setuid 비트가 붙은 0400 파일도 신뢰한다. 데이터 파일이라 읽기 권한에는 영향이 없어서 고치지 않고 7장에 적었다. 그 밖에 구현의 결함은 발견하지 못했다(적대 시험 13개가 첫 실행에서 시험 작성 실수 3건 — 서명한 요청 ID 재사용, 설정 키 없이는 재시작 불가, 모드 상수 — 외에는 모두 통과).

### 확인한 방법 (무엇을 어떻게)
1. **자동 시험(위 표, 전체 실행).** 시험용 서명자(ssh-ed25519 SSHSIG와 ES256 모의 WebAuthn 인증기), 가짜 OpenAI·가짜 broker 서버, 임시 폴더, 더미 값. 값이 새는지는 모든 성공·실패 경로 뒤 응답·로그·원장·상태 파일을 통째로 검색한다.
2. **변이 시험(3장).** 12행 전부 저장소 복사본에서만 하고 시험이 실제로 실패하는지 확인했다.
3. **눈으로 본 확인(B5, ego-browser, 개발용 고정 서버).** 1280px: 목록과 배너(만료 임박 4개 중 하나는 지난 것)·이름 글자 그대로 표시(적대적 `<img …>` 이름)·추가 폼·확인 화면·서명 → 저장·삭제(이름 확인) 흐름, 이탈 경고와 "취소", `outcome_unknown` 문장. 개발 서버의 `fetch` 감시로 `begin`·`message` 본문에 값이 없고 `apply`에만 있음, `localStorage`·`sessionStorage` 비어 있음, 흐름 뒤 페이지 HTML에 값 없음, 히스토리 상태 null. **390px(CDP 장치 재정의): 목록(카드)과 확인 화면(40자 이름 줄바꿈), `scrollWidth == 390`만 봤다. 390px의 삭제 확인·오류 화면, 프로젝트 화면의 배너는 못 봤다**(1280px로만 봄). **서명은 대역 패스키**(`navigator.credentials.get` 바꿔치기)다: 실제 인증기·실제 서명·서버 검증은 거치지 않았다.
4. **JS 시험(node).** DOM 대역 위에서 흐름 순서, 값 보관과 덮어쓰기, 입력 칸 속성, 서버 문장이 텍스트 노드인지, 실제 Go가 만든 메시지 고정값을 화면 코드가 받아들이는지(`TestRealGoMessageFixturesAreAccepted`), 상수가 Go와 같은지.

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖의 새 임시 폴더(`$TMPDIR/w3b-mut-b6`)로 복사**(`rsync`, `.git`·`.superpowers`·`node_modules` 제외)해 한 곳만 망가뜨리고 관련 패키지(`go test`, 웹은 `node --test`)를 돌렸다. 변이마다 한 번에 한 파일만 바꾸고 직후에 원본으로 되돌렸다(복사본의 모든 변경 파일을 원본과 `cmp`로 비교). 컴파일되지 않는 변이는 잡힌 것이 아니라서 `&& false`·`error(nil)` 같은 형태로 컴파일되게 만들었다(⑫의 첫 시도는 `err` 재선언으로 빌드 실패라 `:=` 형태로 고쳐 다시 돌렸고, ⑩의 첫 결과는 시험 보고서 파서 오류라 고쳐 다시 돌렸다). 전부 구현자 실행이다(검토자가 아직 재현하지 않음). "굵게" = B6가 새로 쓴 시험. **브리프의 12행 모두 잡혔다. 한 가지(⑪의 orchd 쪽)는 처음에 안 잡혀 시험을 더하고 다시 돌렸다.**

| # | 망가뜨린 곳 | 잡은 시험 |
|---|---|---|
| ① | 서명 검증 제거(`VerifyRegistry` 결과를 항상 성공으로) | **TestSetTokenForOtherRefOrKindRefused**, **TestApprovalAndRegistryMessagesStillSeparate**, TestSetTokenRequiresValidSignature, TestSetTokenChangeRequired, TestSignatureNotReusableForOtherActionOrSubject, 그리고 W3a의 서명 시험 다수(TestRegistryNeverWrittenWithoutValidSignature, TestApprovalMessageCannotAuthorizeRegistryChange …) |
| ② | `value_hash` 일치 검사 제거 | **TestValueSwapAfterSignatureRefused**, TestValueMismatchRefused, TestPanicLogsTypeNotPayload |
| ③ | 서명된 `change`와 다시 계산한 `change` 비교 제거 | **TestValueSwapAfterSignatureRefused**, **TestSetTokenForOtherRefOrKindRefused**, TestSetTokenFieldsMustMatchSignedChange(4개 하위 시험 모두), W3a의 TestSignatureOverDifferentChangeRefused 등 |
| ④ | 요청 ID 1회성(`Used.Mark`) 제거 | **TestSignatureReplayAcrossRestartRefused**, **TestValueSwapAfterSignatureRefused**, TestRequestIDOnceAcrossRestart, TestConcurrentSetSameRefOneWins, TestValueRules, TestKindAndNameRules, TestSetTokenExpiresInPastRefused, TestDeleteTokenSignedOnlyAndUnknownRef, TestBadExpiresFormatBurnsNoIDPastDateDoes, W3a의 TestReplayAfterRestartRefused 등 |
| ⑤ | 비밀 파일 모드 0400 → 0644 | **TestSecretsNeverReadableByOtherUIDs**, TestSecretStorePutGetDeleteModes, TestSecretStorePutRestoresPreviousOnPostRenameFailure, TestMetaAndFileMismatchHandled, TestOpenAIKeyPrecedenceStoreOverConfig, TestTokensHandlerShowsBrokenStoredAndConfigRows, TestSetTokenReplacesAtomically |
| ⑥ | 쓰기 원자성 제거(옛 파일을 지우고 절반을 쓴 뒤 나머지를 씀) | **TestSecretsNeverReadableByOtherUIDs**, **TestHotReloadRaceNoTornKey** 와 B6의 broker 시험 대부분, TestSecretStoreAtomicOnFailure(`temp-written`·`synced`), TestSecretStorePutGetDeleteModes, TestRegistryAtomicWriteSurvivesCrash, TestWriteAtomicDirSyncFailureKeepsMemoryEqualToDisk 외 다수. **첫 실행에서 시험 하나가 멈춰 10분을 넘겨 내가 중단했다**(멈춤 자체도 "잡힘"이지만 기록을 위해 `-timeout 90s`로 다시 돌렸고 위와 같은 목록이 나왔다) |
| ⑦ | 핫 리로드 대신 서버별로 처음 읽은 키만 계속 씀 | **TestHotReloadRaceNoTornKey**, **TestDeleteThenRefineFailsClosedNoBudget**, **TestBrokenTokenMetaDoesNotDisableOtherFeatures**, TestOpenAIKeyPrecedenceStoreOverConfig, TestOpenAIKeyHotReloadNextCall, TestAIKeyMissingIsFixedErrorAndNoBudgetUse, TestDeleteOpenAIFallsBackToConfigKey, TestOpenAIKeyViaSignedHandlersReachesRefine |
| ⑧ | 삭제 때 파일을 남김(unlink 생략) | **TestSecretsNeverReadableByOtherUIDs**, **TestDeleteThenRefineFailsClosedNoBudget**, TestSecretStoreDeleteOverwritesThenRemoves, TestTokenMetaMissingFileMeansEmptyOnlyWhenAbsent, TestSetMetaFailureRestoresPreviousValue, TestSetFailureKeepsPreviousLiveAndMetaAgree(하위 시험 여럿), TestConcurrentPutDeleteGetConsistent, TestDeleteTokenSignedOnlyAndUnknownRef. (⑥과 같은 이유로 처음 실행이 멈춰 중단했고 `-timeout 90s`로 다시 돌렸다) |
| ⑨ | orchd `apply`가 값을 로그에 찍음(`log.Printf`) | TestApplyForwardsValueExactlyOnceAndNeverLogsIt, TestTokensWorkWithBrokerDown(두 하위 시험), TestPanicAfterBrokerWriteIsRecorded, **TestTokensApplyForgedFieldsRefusedBeforeBroker** |
| ⑩ | 웹이 값을 `begin`에 실음 / `message`에 실음 | (a) begin: TestFlowOrderBeginMessageSignApply, TestDeleteNeedsNameConfirmAndSignature. (b) message: TestFlowOrderBeginMessageSignApply (node, B5의 시험) |
| ⑪ | 길이·문자 규칙 제거: broker(`valueRuleCode`) / orchd 로컬 길이 검사 / orchd 로컬 문자·크기 검사 / 브라우저 `checkValue` | broker: **TestSecretsNeverReadableByOtherUIDs**, TestValueRules, TestTokenFixedMessages. **orchd 길이·문자: 처음에는 아무 시험도 실패하지 않았다(11b) → `TestTokensApplyValueRulesBeforeBroker`를 더해 다시 돌리니 두 변이(11b·11d) 모두 이것이 실패시킨다.** 브라우저: TestD3ValuePreValidationBeforeHashAndSign, TestValueNeverInDOMOrStateAfterSubmit (node) |
| ⑫ | `-check`의 `secrets/`·`tokens.json` 검사 제거(`TokenStoreStatus` 호출 삭제) | TestCheckValidatesSecretsDirAndMeta(`cmd/orch-broker`, B2의 시험) |

⑥의 변이는 거칠다: 옛 파일을 지운 뒤 절반을 쓰고 나머지를 쓰는 형태라 0400 파일에 두 번째로 쓰는 단계가 권한 오류로 실패해 기본 쓰기 시험까지 대부분 깨진다(그래서 목록이 길고 처음 실행이 멈췄다). **원자성만 정확히 가리키는 증거는 `TestSecretStoreAtomicOnFailure`(`temp-written`·`synced` 하위 시험: 쓰기 중 실패해도 이전 값이 남음)** 하나다.

솔직히 적는다: ②·③·④는 W3a·B2~B3의 기존 시험과 B6의 새 시험이 **함께** 잡는다(굵은 것이 B6). ⑨·⑫·⑩은 **B6의 새 시험이 단독으로 잡은 것이 아니라** B4·B5·B2의 기존 시험이 잡는다(⑨는 새 `TestTokensApplyForgedFieldsRefusedBeforeBroker`도 잡음). 변이는 구현자가 정한 한 곳씩이라 같은 결함의 다른 모양(예: 값을 `begin`이 아니라 `Content-Type`·쿠키에 실음)은 시험이 잡는지 확인하지 않았다.

## 3b. 단계별 검토에서 나온 수정 (B1~B5, 요약)
각 작업은 검토를 거쳤고 수정 라운드의 항목은 `.superpowers/sdd/22-PLAN-webconsole-W3b/progress.md`에 있다. 큰 것만: B2 `Put`이 rename 뒤 폴더 `fsync` 실패에서 새 키는 살아 있는데 메타데이터는 옛것인 상태를 남김(이전 값을 다시 쓰도록 수정), 시작 때 비밀 임시 파일 청소가 1분 나이 제한이라 값이 든 임시 파일이 남을 수 있었음(제한 없이·0으로 덮은 뒤 삭제), 로그 가림이 줄마다 모든 비밀 파일을 다시 읽음(캐시); B3 `change` 인자를 바꿔도 요청 ID가 소모되지 않음을 시험이 고정하지 못했음(인자별 단언 추가), 핸들러 패닉 로그가 저장되지 않은 값을 그대로 찍음(형식만 찍도록 수정), `refine`까지 이어지는 통합 시험 부재(추가); B4 `ai_key_missing`이 orchd의 시간당 `refine` 슬롯을 돌려주지 않아 키를 등록한 뒤에도 사용자가 잠길 수 있었음(환급), 본문 검사에서 거부될 때 값 사본을 안 지움(수정), 죽은 코드·`apply` 도중 패닉이 원장에 안 남음(수정); B5 목록 다시 읽기 실패가 **옛 목록을 현재인 것처럼** 보여 줌(`outcome_unknown` 뒤 변경이 일어났는지 알 수 없게 됨 → 실패 화면으로 수정), 비밀번호 관리자 저장 제안 위험(`type=password` → `type=text` + CSS 가림 + 힌트, 5장), 입력 칸·보관 변수 덮어쓰기 구멍들(시험 추가).

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

B2~B4가 각자 적은 표를 한 곳에 모았다. **이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험(소유자·모드 흉내)에 기초한 것이고 실서버에서 본 것이 아니다.** 컷오버 전 확인 목록은 런북 13.0.

**실행 사용자와 샌드박스.** broker: 계정 `orch-broker`(uid 999), `Group=orch-broker`, `SupplementaryGroups=orch`, `UMask=0077`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `NoNewPrivileges=yes`, `CapabilityBoundingSet=`(비어 있음), `MemoryDenyWriteExecute=yes`, `SystemCallFilter=@system-service`, `StateDirectory=orch-broker`(`StateDirectoryMode=0700`), **쓰기는 `ReadWritePaths=/var/lib/orch-broker /run/orch-broker`만**, `/etc`는 읽기 전용, `/home`은 보이지 않음(`infra/systemd/orch-broker.service`). orchd: 계정 `orch`, `ProtectSystem=strict`, `ProtectHome=read-only`, `ReadWritePaths=/home/orch/orchd /home/orch/repos`(`infra/systemd/orchd-orchvm.service`), **`/var/lib/orch-broker`는 0700이라 접근 불가**. 유닛은 바뀌지 않는다.

| 경로 | 소유자 / 모드 | 접근하는 쪽 | 하는 일 | 샌드박스·보호 규칙 |
|---|---|---|---|---|
| `/var/lib/orch-broker` | `orch-broker:orch-broker` **0700** | broker | 신뢰 검사(`stat`), 읽기. **orchd는 접근 불가**(0700) | `ReadWritePaths`, `StateDirectory`. 소유·모드가 어긋나면 등록부·토큰 저장소 모두 꺼짐 |
| `/var/lib/orch-broker/secrets/`(새) | `orch-broker`, **0700** | broker | 시작 때 한 번 `mkdir`(상태 폴더가 신뢰될 때만), 매 호출 `stat`·`readdir`·`fsync`. **`-check`는 만들지 않는다** | `ReadWritePaths`의 하위. `UMask=0077`이 같은 모드를 줌 |
| `/var/lib/orch-broker/secrets/<이름>`(새) | `orch-broker`, **0400**, 일반 파일, 링크 수 1 | broker | 읽기(`O_NOFOLLOW`), 쓰기는 임시 파일 → `rename`, 삭제는 `chmod 0600` + 0으로 덮기 + `unlink` | `chmod`·`rename`·`unlink`·`fsync`는 `@system-service`. `link(2)`는 쓰지 않음(rename만) |
| `/var/lib/orch-broker/secrets/.state-*`(임시, 일시적) | `orch-broker`, 0600 → 0400 | broker | 쓰기 중에만 존재. **시작 때 나이와 상관없이 0으로 덮고 삭제** | 같음 |
| `/var/lib/orch-broker/tokens.json`(새) | `orch-broker`, **0600**(값 없음) | broker | 읽기, 원자적 다시 쓰기(상태 폴더의 `.state-*` 임시 파일 → rename) | `ReadWritePaths` |
| `/var/lib/orch-broker/{registry.json, used-requests.json, budget.json}` | `orch-broker`, 0600 | broker | 변경 없음. 단 `used-requests.json`은 **`set_token`·`delete_token`의 서명 요청 ID도** 기록 | 같음 |
| `/etc/orch-broker/openai.key`(설정의 `key_file`) | `orch-broker` 0400(root가 둠) | broker | **이제 호출마다 읽음**(W2b의 `readSecretFile` 규칙 그대로), **절대 쓰지 않음**. 저장소 키가 없을 때의 폴백이자 롤백의 마지막 보루 | `/etc`는 읽기 전용. **broker 시작은 여전히 이 파일이 신뢰할 수 있게 있어야 함** |
| `/etc/orch-broker/broker.json`, `/etc/aigate/signers.json` | root, 읽기 전용 | broker | 변경 없음(서명자 목록은 호출마다 읽음) | 새 설정 키 없음 |
| `/run/orch-broker/broker.sock` | `orch-broker`, 그룹 `orch` | orchd(연결), broker | 새 op: `tokens`(읽기), `set_token`, `delete_token`(값은 이 소켓으로만 이동) | 변경 없음 |
| `<--git-ssh-key>.pub`, 실서버는 `/home/orch/orchd/ssh/id_gitlab_deploy.pub` | 예상 `orch`(또는 root) 0644, 일반 파일(심볼릭 링크·폴더 아님), 4096바이트 이하, 한 줄 `ssh-ed25519`/`ssh-rsa` | orchd | **읽기만**(`Lstat` + `O_NOFOLLOW`로 열기 + `SameFile` 확인) | `/home`은 `ProtectHome=read-only`로 읽을 수 있고 `/home/orch/orchd`는 쓰기 가능. **실서버에 이 파일이 있는지는 미확인** |
| `<--git-ssh-key>`(개인 키), 실서버 `/home/orch/orchd/ssh/id_gitlab_deploy` | `orch`, 0600 | 게시 코드(기존) | **이 엔드포인트는 절대 열지 않음**(시험: 모드 000·키 모양 canary·하드/심볼릭 링크) | 변경 없음 |
| orchd 원장(`policy.OrchLedgerPath`) | 기존(orchd가 이미 씀) | orchd | `token_change` 한 줄을 `apply`마다 추가(값·해시·`last4` 없음) | 기존 `project_change`와 같은 파일·권한 |
| `/root/backup-w3b`(새, 컷오버용) | root 0700 | 사람(root) | 롤백용 바이너리·설정 복사. **`orch`가 쓸 수 없는 폴더** | — |
| 웹 정적 파일 | orchd 바이너리에 내장 | orchd | 변경: 새 파일 `views/tokens.js`, `expiry.js`(경로·모드·소유자 변경 없음) | — |

새 시스템 호출 종류·새 유닛·새 플래그·새 설정 키·새 포트는 없다(`fsync`·`rename`·`chmod`·`unlink`·`O_NOFOLLOW` 열기는 W3a가 이미 쓰는 종류이고 `link(2)`와 폴더 `fsync`는 W3a 컷오버에서 서버가 동작함을 확인했다, 21번 문서 10장). **실서버에서만 알 수 있는 것은 6장.**

## 5. 설계서와 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| W3b-1 | **서명 흐름이 3요청**(`begin` → `message` → 서명 → `apply`, D8). 값의 솔트 해시가 요청 ID에 묶여 브라우저가 요청 ID를 먼저 알아야 한다. 불변식: 값은 `apply`에만, `begin`·`message`는 저장도 broker 쓰기도 없음 | 요청이 하나 더 감 |
| W3b-2 | **`openai` 키 우선순위: broker 상태 폴더의 저장소 > 설정 파일**(D2). 설정 파일은 `/etc`라 broker가 쓸 수 없다. 웹에서 교체하면 `secrets/openai`가 생기고 설정 파일은 그대로, 삭제하면 설정 키로 폴백. 키를 호출마다 읽는다(핫 리로드, D10) | 설정 파일의 키가 있으면 웹으로 "지워도" 키가 남는 것처럼 보임(설정 키만 있는 행은 삭제 불가, `token_unknown`) |
| W3b-3 | **`set_token`은 최상위 인자 `value_hash`를 가진다**(D5 보정, B3 판정). `fields`에 들어 있는 값이므로 `kind`·`expires`처럼 인자로 와야 broker가 서명된 메시지를 검증된 구성요소로 다시 만들면서도 `value_mismatch`와 `change_mismatch`를 구분한다. orchd `apply`는 `fields.value_hash`를 그대로 넘긴다 | 없음 |
| W3b-4 | **`tokens` 읽기 op는 B2에서 등록**(상태·만료 시험이 둘 다 필요). 행에 D5에 없던 `usable` 필드가 있다(메타데이터만 있는 행·신뢰할 수 없는 파일 표시용). 설정의 OpenAI 키는 `ref:openai, source:config, last4:""`로 나오고, 깨진 저장소 `openai` 행이 있으면 그 행과 **실제로 쓰이는 설정 행이 함께** 나온다 | 클라이언트가 `usable`을 알아야 함(orchd·웹은 처리) |
| W3b-5 | **메타데이터와 파일의 불일치 정책(B2 판정):** 저장 비밀은 **메타데이터와 신뢰할 수 있는 파일이 둘 다** 있을 때만 쓴다. 파일만 있고 메타데이터가 없으면(고아) 쓰지도 목록에 올리지도 않고 `Orphans()`와 `-check` 경고만 낸다; 메타데이터만 있으면 `usable:false`로 목록에 있고 지울 수 있다. **결과: 운영자가 `secrets/`에 파일을 직접 떨어뜨려 토큰을 만들 수는 없다**(`set_token`만 만든다) | 운영자의 수작업 등록 불가(의도적) |
| W3b-6 | **요청 ID 정책(B3, 시험으로 고정):** 서명 검증 전 또는 중에 거부되는 입력(모양 규칙 포함: 이름·종류·만료일 형식, 서명 모양, 서명자 없음, `change` 불일치)은 ID를 **소모하지 않는다.** 유효한 서명이 한 번 통과하면 ID는 **소모된다**(값 해시 불일치, 값 규칙, 지난 만료일, 저장소 거부 포함). 그래서 고친 값도 **새로 서명해야 한다.** 브라우저가 D3 규칙을 서명 전에 미리 검사하고 orchd가 값↔`value_hash`·D3를 broker 호출 전에 확인한다 | 사용자가 값을 잘못 치면 한 번 더 서명 |
| W3b-7 | **`delete_token`:** 저장소 키가 있으면 삭제(설정 키가 있으면 폴백), 설정 키만 있는 `openai`는 `token_unknown`, 설정 키도 없으면 삭제는 허용되고 그 뒤 `refine`은 `ai_key_missing`(503 고정 문장). `ai_key_missing`은 broker의 예산·시간당 슬롯·upstream을 쓰지 않고 orchd의 시간당 `refine` 슬롯도 **돌려준다**(검토 뒤 수정) | 키를 지운 사용자가 `refine`에서 막힘(의도적: 고정 문장이 등록 화면을 가리킴) |
| W3b-8 | **orchd의 로컬 사전 검사**(값 ↔ `fields.value_hash`, D3 규칙)가 broker 호출 전에 있다. broker도 다시 검사한다. **같은 규칙이 broker·orchd·브라우저 세 곳에 있어** 서로 어긋나면 안 되고 `TestConstantsAgreeWithWeb`가 상수·경계값을 바이트 단위로 고정한다 | 세 곳을 같이 고쳐야 함 |
| W3b-9 | **주체별 한도:** `begin`+`message`는 **하나의 한도 시간당 60**, `apply` 시간당 20(D7). W3a의 `ProjectApplyPerActorHour`(30)는 그대로. 슬롯 환급은 W3a와 같은 규칙(보내지 못함·`busy`·`forbidden`만) | 조정 한 곳 |
| W3b-10 | **쓰기의 `tokens_unavailable`은 `outcome_unknown`(504)**(W3a-17과 같은 이유: 폴더 `fsync` 실패는 변경이 적용된 채로 올 수 있음). 읽기의 `tokens_unavailable`은 503. 원장 `token_change`의 `kind`는 `delete_token`에서 빈 문자열(서명된 `fields`에 종류가 없고 추가 조회를 하지 않음) | 목록을 다시 읽어 확인해야 함 |
| W3b-11 | **목록은 전부 아니면 전무**(W3a-13과 같음): broker 행 하나라도 이상하면 `GET /settings/tokens`가 502 `broker_error`. `expiring`은 웹에 전달하지 않고 **브라우저가 `days_left`와 `warn_days`로 배너를 계산**한다 | 한 행 때문에 화면이 비어 보임(런북 알아둘 것 4) |
| W3b-12 | **값 입력 칸이 `type=password`가 아니라 `type=text` + CSS 가림**(D9를 의도적으로 바꿈). Chrome은 비밀번호 칸의 `autocomplete=off`를 무시하고 폼 없는 입력도 저장을 제안할 수 있다. `new-password`는 오히려 "강력한 비밀번호 생성·저장"을 부른다. 신호를 줄이는 속성 묶음(`autocomplete/autocapitalize/autocorrect=off`, `spellcheck=false`, 중립 이름, `data-lpignore`·`data-1p-ignore`·`data-bwignore`·`data-form-type=other`, `<form>` 없음)을 시험으로 고정. **트레이드오프: 가려진 칸에서 복사하면 실제 글자가 복사될 수 있고, 일부 비밀번호 관리자는 이름·레이블로 칸을 판단한다.** 휴리스틱이지 보장이 아니다 | 저장 제안이 뜨면 런북 3-3에서 기록하고 그 값을 거절 |
| W3b-13 | **값 보관(D9의 "확인 화면 진입 시 덮어쓰기"는 불가능했다):** 서명 뒤 `apply` 본문에 값이 필요하므로 **입력 칸은 값이 보관 변수로 옮겨지는 순간 비우고**, 폼은 확인 화면에서 DOM을 떠나며, 보관 변수는 `apply` 본문을 만들 때·취소·오류·거부·이탈에서 덮어쓴다. 확인 화면과 서명 사이에는 값이 보관 변수에 있고 이 구간의 이탈은 막는다(안내문에 "취소"). 해시용 바이트 배열은 0으로 지운다 | JS 문자열은 지울 수 없다(7장) |
| W3b-14 | **만료일:** UTC 날짜로 센다(`DaysLeft`: 만료일 당일이 0, 14일은 경고, 15일은 아님, 지난 것은 음수). `expires == 오늘`은 등록 가능, 이미 지난 날짜는 `bad_request`, 연도는 2000~9999. 브라우저의 UTC 시계는 지난 날짜 검사에만 쓰고 서명 경로에는 안 쓴다(`TestTokensViewClockOnlyForExpiry`) | KST 새벽 9시간 동안 하루 차이 |
| W3b-15 | **공유 웹 파일 두 개를 바꿨다:** `webauthn.js`의 `REGISTRY_MESSAGE_RE`가 네 동작 모두를 받도록 넓힘(W3a의 상수 단언을 네 Go 상수로 갱신, 의미 유지), `guard.js`가 화면별 고정 안내문 문자열을 돌려줄 수 있게 함(`true`는 옛 문구 그대로, 기존 시험 불변) | W3a 화면 회귀(시험이 지킴) |
| W3b-16 | **핸들러 패닉 로그는 형식(`%T`)만**: W3a까지는 패닉 값을 찍었고 B3 검토에서 저장되지 않은 값이 그대로 로그에 남음을 재현했다 | 없음 |

## 6. 실서버에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 13장)에서 처음 확인한다.**
- **broker가 실제 유닛 샌드박스 안에서 `secrets/`를 만들고 `0400` 파일을 쓰는지.** `orch-broker` 계정(uid 999)·`ProtectSystem=strict`·`ReadWritePaths`·`UMask=0077`·`SystemCallFilter` 아래에서 `mkdir`, 임시 파일 `chmod 0400`, `rename`, 폴더 `fsync`, 삭제의 `chmod`·`unlink`가 서버에서 되는지는 미확인(런북 1-4, 3-5). 이 저장소의 시험은 시험 사용자의 임시 폴더에서 소유자 검사를 시임으로 흉내 냈다. `-check`는 읽기 전용이라 이것을 보지 못한다.
- **실서버의 `/var/lib/orch-broker` 소유·모드, `/etc/orch-broker/openai.key` 소유·모드와 broker의 읽기, `systemctl show`의 `ReadWritePaths`·`ProtectSystem`·`UMask`.** 사전 점검 (a)(b)(c)로 컨트롤러가 컷오버 전에 본다.
- **실제 패스키 서명을 broker가 `set_token`에 대해 검증하는지.** 시험은 시험용 ed25519(SSHSIG)와 ES256 모의 인증기만 썼다. 화면의 눈으로 본 확인도 **대역 패스키**였으므로 실제 인증기·실제 서명·서버 검증은 한 번도 지나가지 않았다. W3a 컷오버는 `set_flag`에 대해서만 확인했다(21번 문서 10장).
- **실제 OpenAI 키를 웹에서 교체한 뒤 `refine`이 새 키를 쓰는지.** 시험은 가짜 OpenAI 서버가 본 `Authorization` 헤더로 확인했다(`TestOpenAIKeyHotReloadNextCall`, `TestOpenAIKeyViaSignedHandlersReachesRefine`, `TestHotReloadRaceNoTornKey`). 실제 키 교체는 사용자 결정이고 확인에는 AI 호출 한 번의 비용이 든다.
- **삭제 뒤 설정 키 폴백**의 실서버 동작(시험은 임시 폴더의 가짜 설정 키).
- **브라우저의 "비밀번호 저장/업데이트/제안" 프롬프트가 뜨지 않는지**, 가림(`text-security`) 표시, 붙여넣기. 코드가 정한 속성 묶음(W3b-12)은 시험이 고정하지만 **실제 Chrome·Safari의 동작은 한 번도 보지 못했다**(런북 3-3, 사용자). 비밀번호 관리자 확장은 더 알 수 없다.
- **JS 문자열을 메모리에서 지울 수 없다.** 보관 변수를 덮어써도 문자 데이터는 GC가 재사용할 때까지 남고, 입력 위젯·JSON 본문·네트워크 스택의 사본은 지울 수 없다(바이트 배열만 지움).
- **`last4`가 목록에 보인다.** 값의 마지막 4자(식별용, 알려진 정보 노출). 화면은 값을 다시 보이지 않지만 `last4`와 확인 화면의 해시 앞 8자가 있다.
- **토큰이 orchd 메모리를 거친다.** orchd는 바이트로 받고 처리 뒤 0으로 덮지만 JSON 디코더 내부 버퍼·인코더의 풀 상태·커널 소켓 버퍼·GC가 옮긴 사본은 지울 수 없다(`encoding/json` 한계). **`apply` 도중의 코어 덤프에는 값이 들어 있을 수 있다.** 같은 uid의 메모리 읽기는 막지 못한다(설계서 6장의 알려진 잔존 위험).
- **키보드만, 스크린 리더로의 `#/tokens` 화면 사용.** DOM 대역 시험(레이블, 네이티브 컨트롤, 텍스트 노드)만 했다. ego-browser 확인은 1280px과 390px로 했지만 **390px에서는 목록과 확인 화면만 보았고(삭제 확인·오류 화면, 프로젝트 화면의 배너는 보지 않음), 서명은 대역 패스키였다.**
- **배포 키 `.pub`이 실서버에 있는지·형식·모드**(미확인, 사전 점검 d). 없어도 W3b는 영향이 없다(소비자는 W3c).
- **실서버에서의 `-check`의 새 검사**(`secrets/`·`tokens.json`): 시험은 임시 폴더로만, 실서버 샌드박스에서는 컷오버 1-3에서 처음 본다.
- **리눅스에서의 파일 동작.** 소유자 확인(`syscall.Stat_t`)·링크 수 검사·`O_NOFOLLOW|O_NONBLOCK` 열기·폴더 `fsync`는 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다(`openflags_unix.go`는 `//go:build unix`).
- **실부하·장시간에서의 한도와 `used-requests.json` 크기**(5000개 상한, 1시간): 주입한 시계로만.
- **WebAuthn 카운터 부재**(W3a-2)는 그대로다. 복제된 패스키가 5분 안에 쓰이면 걸러지지 않는다(방어는 신선도와 요청 ID 1회성뿐).
- 기존 W2b·W3a에서 이월된 미확인(실제 GitLab·MR, 한도·예산 초과 실동작 등)은 18·21번 문서 그대로이고 이번 변경은 그 경로를 건드리지 않았다(회귀 시험은 통과: 기존 `refine`·`create_mr`·프로젝트 시험).
- **`gitlab`·`github` 토큰의 소비자는 없다**(W3c). 저장·목록·삭제만 검증되었고 이 값으로 어떤 외부 호출이 성공하는지는 W3b의 범위가 아니다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **교체 도중 비밀 파일 rename과 메타데이터 쓰기 사이의 중단(전원·강제 종료)은 원자적이지 않다**(B2 판정, 코드 주석). 어느 쓰기 순서도 중단에 원자적이지 않고 메타데이터를 먼저 쓰면 옛 값에 새 메타데이터가 붙는다. **교체 중 중단되면 새 값은 쓰이고 목록의 `last4`·만료일이 옛것으로 남을 수 있다**(다음 `set_token`까지). 처음 등록(교체 아님)의 중단은 고아 파일로 남고 무시된다(W3b-5).
2. **목록은 전부 아니면 전무**(W3b-11, 런북 알아둘 것 4): 이상한 행 하나가 화면 전체를 가린다. 서버에서 `tokens.json`을 고쳐야 한다.
3. **값이 거치는 메모리**(6장): 브라우저 JS, orchd, broker Go 문자열(값은 문자열로 저장소에 들어가 GC까지 남음. 삭제 때만 파일을 0으로 덮음, SSD·COW 파일 시스템에서는 최선의 노력).
4. **`last4` 노출과 가려진 칸의 복사**(W3b-12): 5.1과 6장.
5. **만료일은 사용자가 적은 값**이고 GitLab·GitHub에 확인하지 않는다. UTC로 센다.
6. **서명이 유효하면 요청 ID가 소모된다**(W3b-6): 값 규칙을 어겨도 다시 서명해야 한다(브라우저·orchd의 사전 검사가 이를 줄임).
7. **설정 키 파일이 broker 시작에 필수다**(W2b 규칙 유지): "저장소 키만 쓰는" 배치는 이 규칙을 푸는 후속 작업이 필요하다. 그래서 설정 키 파일을 지우지 않는 것을 권한다.
8. **비밀 파일의 모드 검사는 `Perm()`(하위 9비트)만 본다**: setuid·setgid·sticky 비트가 붙은 0400 파일도 신뢰한다. 데이터 파일이고 다른 사용자의 읽기 권한과 무관해 고치지 않았다(B6 시험 작성 중 확인).
9. **orchd의 세 곳 중복 규칙**(W3b-8)과 **공유 웹 파일 변경**(W3b-15).
10. **`gitlab`·`github` 토큰은 W3c까지 소비자가 없다.** 지금 등록해도 아무것도 바뀌지 않는다.

## 8. 사용자 결정 목록

컷오버 때 사용자가 정하거나 해야 하는 것(런북 13.0, 3단계):
1. **컷오버에서 실제 OpenAI 키를 웹으로 교체해 볼지**(런북 3-5): 선택지는 **교체 없음**(권장 기본) 또는 **사용자가 이미 가진 같은 키 값으로 교체**(확인에 AI 호출 한 번의 비용). 어느 쪽이든 키 값은 **사용자가 직접 입력하고 이 대화·채팅·터미널에 붙이지 않는다.** 가짜 토큰을 진짜처럼 등록하는 선택지는 없다. 교체했다면 3-6의 삭제로 "원래 상태"로 되돌릴지.
2. **브라우저의 비밀번호 저장 제안**(3-3): 뜨면 어떤 브라우저인지 기록하고 이 방식(`type=text` + 가림)을 계속 쓸지, 아니면 다른 방식을 택할지.
3. **GitLab 관리자 토큰을 언제 줄지**(W3c 준비: W3b의 토큰 메뉴가 필요). 사용자가 직접 입력한다. 관리자 토큰의 영향 범위는 설계서 6장.
4. **배포 키 `.pub`을 만들지**(런북 사전 점검 d, 알아둘 것 12): W3c 전에는 쓸 곳이 없다. 만들면 `ssh-keygen -y`가 개인 키를 읽는다.
5. **컷오버가 안정되면 `/root/backup-w3b`를 언제 지울지**(`openai.key` 사본이 들어 있으므로 `shred`), 그리고 **`/etc/orch-broker/openai.key`를 언제 정리할지**(롤백을 쉽게 하려면 지우지 않는다. 웹 저장소 키만 쓰려면 broker 시작 규칙 완화 작업이 먼저 필요, 7장 7).
6. 다음 단계(W3c 제공자·프로젝트 생성)로 갈지, 그 전에 W3a의 "후보 등록" 흐름(21번 문서 10장의 미확인)을 확인할지.
7. WebAuthn 카운터 없이 가는 것(W3a-2)을 계속 받아들일지.

## 9. 컷오버 기록
아직 없음(컷오버 전). 컷오버를 하면 이 장에 서버에서 본 것만 적는다(21번 문서 10장과 같은 방식).
