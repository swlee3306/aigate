# 웹 콘솔 W3h — 결과 기록 (2026-11-14)

> 상태: **구현 완료(브랜치 `impl/webconsole-w3h`), 배포하지 않음.** 코드와 시험은 이 저장소에서 끝났고, 실제 서버(orch-vm)와 실제 GitHub·GitLab에는 아무것도 하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 18장, 계획은 `32-PLAN-webconsole-W3h.md`. 출발점은 W3g 컷오버가 끝난 서버(main `6bc81b4`, 등록부 v4, 제공자 `example-group-gitlab`·`swlee3306-github`, 프로젝트 `toy`·`sample-app`(legacy)만: `31-RESULTS-webconsole-W3g.md` 9장)다.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더·명백한 더미 값**)만 가리킨다. **실제 등록부의 v5 마이그레이션, 실제 GitHub·GitLab에서 저장된 해시로 하는 키 떼기, 운영 리스너의 403 진단 로그, 리눅스 systemd 샌드박스 안의 동작은 한 번도 하지 않았다**(6장). 이 작업에서 실제 키·토큰을 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다.
> **W3h는 W3g의 열린 결정(31번 8장 1, GitLab 키 식별의 남은 위험)을 사용자가 고른 (a)로 닫는다**: 프로젝트를 만들거나 연결할 때 이미 서명·검증된 `deploy_key_sha256`을 등록 행에 저장하고, 키를 뗄 때 orchd가 준 키 줄이 그 해시와 같아야 한다. 서명 필드·변경 해시·`REGISTRY_MESSAGE_RE`·웹 벡터·허용 목록·연산·오류 코드 목록은 **바뀌지 않는다**. 바뀌는 것은 등록부 파일 버전(v4 → v5)과 broker·orchd의 대조, orchd의 403 진단 로그다.

## 1. 만든 것

**H1. broker (등록부 v5, 저장, 대조).** `RemoteLink`에 선택 필드 `key_sha256`(소문자 16진 64자, 없으면 키 자체가 파일·응답에 나오지 않음)을 더하고 등록부를 v5로 올린다. 검증: 값이 있으면 `provider`가 있어야 하고 `^[0-9a-f]{64}$`(그 밖은 등록부 거부, 쓰기도 같은 규칙). v4 파일을 열면 행은 그대로 두고 version만 5로 다시 쓰며 그 전에 `registry.json.v4.bak`(0600)을 **한 번만** 쓴다(이미 있으면 덮어쓰지 않음, v3→v4와 같은 규칙). v3·v4 파일 안의 `key_sha256`은 모르는 키로 거부. **저장 지점 3곳**: `create_project`(행 생성), `link_project`(새 행), `attach_remote`(기존 행에 원격 연결)가 모양 단계에서 이미 `sha256(deploy_key) == 서명된 deploy_key_sha256`을 상수 시간으로 확인한 값을 저장한다(키 줄 **전체**, 주석 포함의 해시). **대조**: `remove_project`(`detach_key=true`)와 `remove_deploy_key`의 모양 단계(서명 검증·요청 ID 소모 전)와 잠금 뒤 재확인 둘 다에서 새 순수 함수 `keyLineCode`가 판단한다: 행에 해시가 있으면 `sha256(키 줄)`과 상수 시간 비교, 다르면 `deploy_key_mismatch`; 해시가 없으면 GitHub만 통과(W3g 규칙: 제목+본문이 뒤에서 결정), GitLab과 모르는 종류는 `deploy_key_mismatch`(닫힌 실패). 기존 거부 코드가 먼저이고 새 규칙은 맨 끝이다(그래서 `remote_changed`가 `deploy_key_mismatch`보다 앞선다: 3장 MC). `classifyKeys`(제목·본문 규칙)는 바뀌지 않는다. `sameRowCore`가 해시도 비교한다. 원격이 지워지면(`remote: null`) 해시는 `remote` 안에 있으므로 함께 사라진다.
**H2. orchd·웹, 진단 로그, 멈추는 시험.** orchd의 `RemoteLink`가 `key_sha256`을 읽는다(관대한 읽기, 콘솔 등록부 뷰에는 싣지 않음). broker와 같은 규칙의 `keyLineMatchesRow`를 **서명 전에** 본다: 미리보기의 `detach_key_available`(해시가 있으면 같을 때만, 해시 없는 GitLab은 false, 해시 없는 GitHub은 W3g처럼), propose·apply의 409 `deploy_key_mismatch`(`remove_project` 떼기는 "떼기를 끄고 진행" 문장 `errKeyMismatchDetach`, `remove_deploy_key`는 "제공자 화면에서 직접 제거" 문장 `errKeyMismatchRemoval`). broker가 `deploy_key_mismatch`를 돌려주면 "결과 알 수 없음"이 아닌 **확정 거부(409, 변경 없음)**. **H-D6 진단 로그**: 두 리스너의 가장 바깥 층 `logRefusals`가 HTTP 403마다 한 줄 `orchd: refused 403 <코드> <메서드> <경로>`를 남긴다(코드는 응답의 v1 `error.code` 또는 평면 `code`, 없으면 `forbidden`; 메서드가 이상하면 `OTHER`; 경로는 쿼리 없는 `EscapedPath`, 256바이트를 넘으면 잘라 `...`). 쿼리·헤더·본문·서명·토큰·키 줄은 남기지 않고 원장은 바뀌지 않는다. **H-D7**: `TestConcurrentDeleteOfOneProjectSendsOneDelete`의 기다림에 시간 제한(운영 코드 변경 없음). broker `registry.go`의 중복 nil 검사 정리(동작 같음). 웹: `DEL_FAIL.deploy_key_mismatch`(remove_deploy_key 흐름).
**H3(이 작업).** 적대 시험 10개(broker 5, orchd api 5)와 공유 벡터 표 `internal/broker/testdata/keyline_vectors_w3h.json`, 변이 시험 26개(3장), H2 검토의 작은 지적 4개 반영(아래), 이 문서, 런북 18장, 31번 8장 1의 "해결됨" 표시, 문서 19 상태 줄.
- **H2 검토 지적 반영(운영 동작 변경 없음, 웹 문장 한 곳만).** (a) 300바이트 경로가 256바이트에서 `...`와 함께 잘리는 시험, (b) 두 `deploy_key_mismatch` 문장이 서로 다르고 `remove_deploy_key` 쪽은 "떼기를 끄라"고 하지 않는 시험, (c) `refusal_log.go`에 주석: ResponseWriter를 감싸면 `http.MaxBytesReader`가 쓰는 net/http의 비공개 "요청이 너무 큼" 신호(작성기에 대한 타입 단언, `Unwrap` 사슬 없음)가 가려져, 너무 큰 본문이면 연결을 곧바로 닫지 않고 처리 뒤 최대 256KB를 읽어 버린 다음 닫는다(응답은 같다), (d) 웹 `detachRefused` 문장의 원인 목록에 "이 서버의 배포 키가 등록할 때의 키와 다름"을 하나 더함(원인별 분기 없음), 그 문장을 고정하는 node 시험 `TestDetachRefusedAfterSignatureSaysNothingChangedR1`에 단언 추가. `TestConstantsAgreeWithWeb`는 그대로 PASS(SKIP 없음).

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-14, macOS(Apple Silicon), `go version` 1.27.1, Node v26.5.0. 세는 방식은 W3a~W3g와 같다(최상위 시험 함수와 하위 시험을 따로, `go test -json` 결과 줄에서 집계). 이 숫자는 **H3의 시험을 더한 작업 트리 위에서 처음부터 다시 돌린 전체 실행**의 것이고 변이 실행과 겹치지 않게 돌렸다(문서만 그 뒤에 바뀐다).
- `gofmt -l .`: 출력 없음. `go vet ./...`, `GOOS=linux go vet ./...`, `GOOS=darwin go vet ./...`: 통과.
- `go test ./internal/... ./cmd/... -count=1 -race -timeout 30m -json`(31번 문서와 같은 명령, `e2e` 미포함): **종료 코드 0**, 오류 출력 없음, `DATA RACE` 0건. 시험이 있는 38개 패키지 모두 `ok`. 최상위 시험 함수 **1813 통과, 0 실패, 2 건너뜀**(하위 시험 869개 모두 통과). 건너뜀 2개는 이전과 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollisionsPerformance`(-race에서 건너뛰는 시간 검사). `TestRunbookRollbackEditMakesAW3gRegistryW3h`는 건너뛰지 않고 돌았다(python3 있음). 시험 파일이 없는 패키지 4개는 표에서 뺐다.
- `node --test "internal/orch/web/test/*.test.js"`: **318개 전부 통과**(0 실패, 0 건너뜀: H2의 318개 그대로, H3는 기존 시험에 단언만 더함).
- **게이트:** `go test ./internal/... -run TestConstantsAgreeWithWeb -v` → `--- PASS: TestConstantsAgreeWithWeb`, 하위 `tokens`·`providers`·`w3e`·`w3f` 모두 PASS, **`SKIP` 없음**.
- 31번 문서(W3g, 같은 명령)와 비교: 최상위 1787 → 1813(+26 = broker +16(H1 11, H3 5) + orchd api +10(H2 5, H3 5)), 하위 858 → 869. broker 436 → 452, orchd api 426 → 436.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 14 | 0 | 0 | 0 |
| `cmd/orchd` | 34 | 0 | 0 | 0 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 70 | 0 | 0 | 76 |
| `internal/broker` | 452 | 0 | 0 | 303 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 52 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 436 | 0 | 0 | 40 |
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

### H3가 더한 적대 시험 (모두 "거부되거나 무해해야 한다")
broker `internal/broker/adversarial_w3h_test.go`(5개), orchd `internal/orch/api/adversarial_w3h_test.go`(5개, 그중 2개는 H2 검토 지적). 새 node 시험은 없다(기존 `TestDetachRefusedAfterSignatureSaysNothingChangedR1`에 단언 한 줄). H1·H2가 이미 같은 일을 하는 시험은 복사하지 않았다.

| 시험 | 하는 일 |
|---|---|
| `TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h` (broker) | **경쟁 seam(MF)**: 모양 단계 대조를 통과한 뒤, 잠금 뒤 재확인 전에 행의 해시가 다른 키 줄의 해시로 바뀐다(seam은 broker 시계 `s.Now`: `verifySignedShape` 안에서 `shape()` 뒤·요청 ID 기록 전에 꼭 한 번 읽힌다). `remove_project`(떼기)·`remove_deploy_key` × GitHub·GitLab 해시 행 4경우 모두 잠금 뒤 재확인이 `deploy_key_mismatch`로 거부하고 **제공자 요청 0(키 목록도 `DELETE`도 없음)**, 행은 바뀐 그대로 남는다. seam이 실제로 돌았는지 확인한다. **요청 ID는 이때 이미 소모되어 있다**(아래 "브리프와 다른 점" 1): 해시를 되돌리고 같은 서명을 다시 보내면 `request_reused`(여전히 요청 0). 대조 확인: 새 서명이면 키 하나가 떼어진다 |
| `TestRemoteChangedOutranksKeyMismatchW3h` (broker) | **거부 코드 우선순위(MC)**: 원격 번호가 없는 행(해시 있음·없음, GitHub·GitLab) + 다른 키 줄(다른 키, 주석만 붙인 같은 본문)에서 두 op 모두 `remote_changed`가 이긴다. `remove_deploy_key`에 행과 다른 `remote_path` + 다른 키 줄도 `remote_changed`. 모두 서명 전, ID 미소모, 제공자 요청 0 |
| `TestKeyLineRuleVectorsW3h` (broker) | broker의 `keyLineCode`를 공유 벡터 표(27행: GitHub·GitLab·모르는 종류 × 해시 없음·같음·다름, 주석만 다른 줄, 대문자 해시, 빈 줄)에 고정한다. 표가 9가지 조합을 다 덮는지도 확인 |
| `TestRowMigratedFromV4CannotDetachOnGitLabW3h` (broker) | W3g broker가 쓴 v4 파일(해시 없음)을 W3h broker가 열면 v5로 올라가고 `.v4.bak`이 그 v4 바이트와 같다. 그 GitLab 행은 바른 줄이든 다른 줄이든 두 op 모두 서명 전 `deploy_key_mismatch`(ID 미소모, 요청 0), GitHub 행은 W3g 규칙으로 떼어지고(키 `DELETE` 1), GitLab 행은 떼기 없이 지울 수 있다(요청 0). (런북 18장 ⚠ 3·3-3) |
| `TestRunbookRollbackEditMakesAW3gRegistryW3h` (broker) | **런북 18장 단계 4의 `python3` 스크립트를 글자 그대로 실행**(시험의 `rollbackScript` 상수 = 런북의 스크립트, 이 문서를 쓸 때 기계적으로 같음을 확인): 해시 행 2개를 가진 v5 등록부 → `key_sha256` 0개·`"version":4`·모드 600, **W3g 엄격 로더 복제가 받아들임**, 그 밖의 모든 값이 같음(디코드 비교), 두 번째 실행은 "nothing changed"로 멈추고 파일 불변, W3h broker가 다시 열면 v5로 올라가고 해시 없음·`.v4.bak`이 편집된 파일. 해시 없는 v5 파일은 버전만 바뀐다. (python3가 없으면 건너뜀: 이번 실행에서는 돌았다) |
| `TestKeyLineRuleVectorsAgreeWithBrokerW3h` (orchd) | orchd의 `keyLineMatchesRow`를 **같은 벡터 파일**(`../../broker/testdata/keyline_vectors_w3h.json`)에 고정한다. 두 쪽이 같은 기대값에 묶이므로 서로 어긋날 수 없다: 화면이 broker가 거부할 떼기를 제안하지 않고, broker가 받을 떼기를 숨기지 않는다(W3e `deploykey_vectors.json`과 같은 방식) |
| `TestRemoteChangedOutranksKeyMismatchW3h` (orchd) | 같은 우선순위를 orchd 쪽에서: 번호 없는 GitLab 행(해시 다름·없음)의 떼기 propose가 409 `remote_changed`, 미리보기 선택지 없음; `remove_deploy_key`에 다른 경로면 `remote_changed`, 같은 경로면 `deploy_key_mismatch`. broker 호출 0. **알려진 차이 하나를 고정**(7장 7) |
| `TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h` (orchd) | **W3g의 "서명되지 않는 키 줄로 다른 키 떼기" 장면을 orchd 쪽에서**: 등록 때와 다른 키 줄(GitLab 공용 `.pub`, GitHub 키 폴더 `.pub`을 다른 키로 바꿈)이면 미리보기 선택지 없음(지문도 없음), propose(떼기·`remove_deploy_key`)는 서명 전 409 `deploy_key_mismatch`(변경 해시 없음), **줄이 맞을 때 만든 제안도 apply에서 broker 호출 전에 409**, 각 op가 자기 문장으로 답한다(떼기 문장에만 "…를 끄고"). broker 호출 0, 복제본·행 그대로, 떼기를 끄면 진행된다. (broker 쪽 절반 — 유효한 서명 + 다른 줄 → 서명 전 거부·제공자 요청 0 — 은 H1의 `TestUnsignedKeyLineWideningNowRefusedW3h`와 위 MF 시험) |
| `TestRefusalLogTruncatesLongPathW3h` (orchd) | H2 지적 (a): 300바이트 경로가 앞 256바이트 + `...`로 한 줄, 정확히 256바이트면 표식 없음 |
| `TestKeyMismatchTextsDifferW3h` (orchd) | H2 지적 (b): 두 문장이 다르고 둘 다 409·`deploy_key_mismatch`·"변경 없음"으로 끝남, 떼기 문장에만 "\"배포 키도 함께 떼기\"를 끄고", `remove_deploy_key` 문장에는 "함께 떼기"·"끄고"가 없고 "직접 제거"가 있다 |

**브리프와 다른 점(솔직히).**
1. **MF 시험의 "요청 ID를 소모하지 않음"은 이 코드에서 성립할 수 없다.** `verifySignedShape`는 `shape()` → 서명 검증 → **요청 ID 기록** 순서이고, 핸들러는 그 뒤에 잠금을 잡고 재확인한다. 그래서 모양 단계 뒤에 행이 바뀌어 잠금 뒤 재확인이 거부하면 ID는 이미 쓰였다("From here on the request ID is consumed", W3f 이래의 설계). 시험은 이것을 **있는 그대로 고정**했다: 거부·요청 0·행 유지, 그리고 해시를 되돌린 재전송이 `request_reused`. 기록 순서를 바꾸는 것은 전역 제약 밖의 동작 변경이라 하지 않았다. 사용자에게는 아무것도 지워지지 않았고 새로 서명하면 된다는 뜻이다.
2. **"orchd apply에 위조한 줄 → broker 거부, DELETE 0"을 한 프로세스의 종단 시험으로 잇지 않았다.** orchd 시험은 broker 패키지를 쓰지 않는 가짜 broker로 돌고, 위조한 줄은 orchd가 손상된 경우에만 broker에 간다(정상 orchd는 요청에서 키 줄을 받지 않고 자기 파일만 읽는다: W3g G-D5). 그래서 두 절반의 합성이다: orchd는 다른 줄이면 서명 전·broker 호출 전에 멈추고(`TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h`), 손상된 orchd를 흉내 낸 broker 직접 호출은 유효한 서명이어도 제공자 요청 0으로 거부된다(`TestUnsignedKeyLineWideningNowRefusedW3h`, MF 시험).

**이미 H1·H2의 시험이 증명하는 것은 복사하지 않는다.** 계획의 검토 초점이 어느 시험에 있는지의 지도:

| 속성 | 시험 |
|---|---|
| 해시가 있는 GitHub·GitLab 행에서 다른 키 줄(같은 프로젝트의 다른 키, 목록에 없는 키, 주석만 다른 같은 본문)은 서명 전 거부, `DELETE` 0, ID 미소모(서명이 틀려도 같은 답 = 서명 검증 전) | `TestDetachRefusedWhenKeyLineHashDiffers`, `TestRemoveDeployKeyRefusedWhenKeyLineHashDiffers`, `TestUnsignedKeyLineWideningNowRefusedW3h`, W3g `TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g`(H1이 기대를 "거부"로 바꿈), 새 MF 시험 |
| 해시 없는 GitLab 행은 거부, GitHub 행은 W3g 규칙 | `TestGitLabRowWithoutHashRefusesDetach`, `TestGitHubRowWithoutHashKeepsW3gRule`, 새 broker 시험 `TestRowMigratedFromV4CannotDetachOnGitLabW3h`, 벡터 표 |
| 마이그레이션: 원자적, 중단·재시도, 백업 한 번·덮어쓰지 않음, 옛 broker의 v5 거부 | `TestRegistryV4MigratesToV5Atomically`, `TestV4ToV5WritesBackupOnceNeverOverwritten`, `TestOldBrokerRefusesV5`, 갱신된 v1~v4 마이그레이션 시험들, 새 `TestRunbookRollbackEditMakesAW3gRegistryW3h` |
| v5 파일 규칙(대문자·길이·빈 값·`provider` 없는 행·행 수준의 키·`remote` 안 모르는 키), 쓰기에도 같은 규칙, 스냅숏 깊은 복사 | `TestRegistryV5Rules` |
| 저장 3곳(create/link/attach)이 모두 해시를 저장(행·응답·파일·`projects` 읽기·다시 열기) | `TestCreateLinkAttachStoreTheSignedKeyHash` |
| `remote: null` 경로(원격 삭제 뒤 `remote_deleted_at` 행, `also_local`, `clear_orphan`)에서 해시가 남지 않음 | `TestRemoteClearedDropsHash` |
| `sameRowCore`가 해시를 비교 | `TestSameRowCoreComparesOnlyMeaningfulFields` |
| orchd: 미리보기·propose·apply가 broker 규칙을 서명 전에, broker의 `deploy_key_mismatch`는 확정 409 | `TestDetachKeyAvailableNeedsMatchingHash`, `TestDeployKeyMismatchIsDefiniteRefusalNotUnknownOutcome`, 새 orchd 시험 3개 |
| orchd가 `key_sha256`을 읽되 콘솔 등록부 뷰에는 싣지 않음 | `TestProjectsViewAcceptsKeySHA256` |
| 403 진단 로그: 모든 403 경로의 코드, 두 리스너 끝단, 비밀 없음, 줄바꿈 주입 불가, 경로 길이 제한 | `TestRefused403LogsReasonAndPathOnly`, `TestRefusalLogHasNoSecrets`, 새 `TestRefusalLogTruncatesLongPathW3h` |
| 웹: `remove_deploy_key`의 `deploy_key_mismatch` 문장, 떼기 거부 문장 | `TestDeployKeyMismatchTextForDetach`, `TestDetachRefusedAfterSignatureSaysNothingChangedR1`(새 원인 단언) |
| 멈추는 시험(H-D7) | `TestConcurrentDeleteOfOneProjectSendsOneDelete`(H2 보고서: 붙잡은 동안 단언을 틀리게 하면 0.22초에 실패, 옛 시험은 40초 시간 초과까지 멈춤) |

**시험을 새로 쓰는 중 발견한 것.** (a) **제품 코드의 결함은 발견하지 못했다.** 제품 코드는 주석 한 곳(`refusal_log.go`)과 웹 문장 한 곳만 바꿨다. (b) `remove_deploy_key`에서 원격 번호가 없는 행(제공자는 있음)의 거부 코드가 orchd(`deploy_key_mismatch`)와 broker(`remote_changed`)에서 다르다(7장 7). 둘 다 서명 전 확정 거부이고 만들기·연결 경로는 그런 행을 쓰지 않는다. 시험으로 고정만 했다. (c) 처음 쓴 롤백 시험은 등록부 폴더를 `t.TempDir()`로 만들어 W3h broker가 폴더 권한 때문에 다시 열지 못했다(시험의 실수, 기존 도우미 `stateDirFor`로 고침).

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖(`/tmp/w3h-h3-mut/aigate`)으로 복사**(`rsync`, `.git`·`.superpowers`·`graphify-out` 제외)해 한 곳만 망가뜨리고 관련 시험을 돌렸다. 도구는 `/tmp/w3h-h3/mutate.py`(순차 실행, 정확한 바꿈 문자열은 `mutants.py`, 바꿀 문자열이 파일에서 정확히 한 번 나와야 실행). 변이마다 복사본의 원래 파일로 되돌리고 **작업 트리의 파일과 같은지 확인**했다(작업 트리는 건드리지 않았다). "모양 단계만/잠금 뒤만 생략"(MS·MF)은 대조 줄을 뺀 복제 함수(`…NoKey`)를 파일 끝에 붙이고 한 호출 지점만 그것으로 바꿨다. 먼저 같은 명령을 변이 없이 돌려 모두 통과함을 확인했다(broker `ok` 95초, api `ok` 65초, node `delete.test.js` 56개 통과). 명령(시간 제한 600초): broker `go test ./internal/broker -run 'W3h|W3g|W3f|Detach|Remove|Delete|Key|Registry|Create|Link|Attach|SameRow|Concurrent|Unsigned'`, B07·B10은 패키지 전체(`-run` 없음, 900초), orchd `go test ./internal/orch/api -run 'W3h|W3g|W3f|Remove|Delete|Detach|Key|Preview|Propose|Apply|Refus|Constants|Mismatch'`, 웹 `node --test internal/orch/web/test/delete.test.js`. 굵게 = H3의 새 시험. 실패한 시험이 6개를 넘으면 처음 6개와 나머지 개수만 적었다.

| # | 망가뜨린 곳 | 패키지 | 결과 | 실패한 시험 (개수) |
|---|---|---|---|---|
| B01 | `create_project`가 해시를 저장하지 않음(저장 지점 1) | broker | 잡힘 | TestCreateLinkAttachStoreTheSignedKeyHash (1) |
| B02 | `link_project` 새 행이 해시를 저장하지 않음(저장 지점 2) | broker | 잡힘 | TestCreateLinkAttachStoreTheSignedKeyHash (1) |
| B03 | `attach_remote` 기존 행이 해시를 저장하지 않음(저장 지점 3) | broker | 잡힘 | TestCreateLinkAttachStoreTheSignedKeyHash (1) |
| B04 | 해시 비교를 모든 곳에서 생략(모양 단계와 잠금 뒤 둘 다) | broker | 잡힘 | TestDetachRefusedWhenKeyLineHashDiffers, TestGitLabKeyRemovalOnlyThisProjectsKeyId, **TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h**, **TestKeyLineRuleVectorsW3h**, TestRemoveDeployKeyRefusedWhenKeyLineHashDiffers, TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g … (+1) (7) |
| MS1 | `remove_project`: 모양 단계에서만 비교 생략(잠금 뒤는 남음) | broker | 잡힘 | TestDetachRefusedWhenKeyLineHashDiffers, TestGitLabRowWithoutHashRefusesDetach, **TestRowMigratedFromV4CannotDetachOnGitLabW3h**, TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g, TestUnsignedKeyLineWideningNowRefusedW3h (5) |
| MS2 | `remove_deploy_key`: 모양 단계에서만 비교 생략 | broker | 잡힘 | TestGitLabKeyRemovalOnlyThisProjectsKeyId, TestGitLabRowWithoutHashRefusesDetach, TestRemoveDeployKeyRefusedWhenKeyLineHashDiffers, **TestRowMigratedFromV4CannotDetachOnGitLabW3h**, TestUnsignedKeyLineWideningNowRefusedW3h (5) |
| **MF1** | `remove_project`: **잠금 뒤 재확인에서만** 비교 생략(H1 검토의 MF) | broker | 잡힘 | **TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h** (1) |
| **MF2** | `remove_deploy_key`: 잠금 뒤 재확인에서만 비교 생략 | broker | 잡힘 | **TestKeyHashChangedBetweenShapeAndLockIsRefusedW3h** (1) |
| **MC1** | `remove_project`: `deploy_key_mismatch`를 `remote_changed`보다 먼저(H1 검토의 MC) | broker | 잡힘 | **TestRemoteChangedOutranksKeyMismatchW3h** (1) |
| **MC2** | `remove_deploy_key`: 같은 순서 바꿈 | broker | 잡힘 | **TestRemoteChangedOutranksKeyMismatchW3h** (1) |
| B05 | 해시 없는 GitLab 행 허용 | broker | 잡힘 | TestGitLabRowWithoutHashRefusesDetach, **TestKeyLineRuleVectorsW3h**, **TestRowMigratedFromV4CannotDetachOnGitLabW3h** (3) |
| B06 | 해시 없는 GitHub 행 거부 | broker | 잡힘 | TestCanaryTokenNeverInAnyOutputW3g, TestConcurrentDeleteOfOneProjectSendsOneDelete, TestConcurrentDetachSendsOneKeyDelete, TestCrossActionSignaturesStillRefusedW3g, TestDeleteKeyOnlyMatchingTitleAndFingerprint, TestDeleteUsesOnlyTheProvidersOwnToken … (+19) (25) |
| B07 | 마이그레이션이 이미 있는 `registry.json.v4.bak`을 덮어씀 | broker(전체) | 잡힘 | TestV4ToV5WritesBackupOnceNeverOverwritten (1) |
| B08 | v5 파일: 대문자 16진 해시 허용 | broker | 잡힘 | TestRegistryV5Rules (1) |
| B09 | v5 파일: `provider` 없는 행의 `key_sha256` 허용 | broker | 잡힘 | TestRegistryV5Rules (1) |
| B10 | 상수 시간 비교를 문자열 `!=`로 | broker(전체) | **살아남음(동등 변이)** | — (0) |
| O01 | orchd 미리보기: 해시 없는 GitLab 행에 선택지 | orchd api | 잡힘 | TestDetachKeyAvailableNeedsMatchingHash, **TestKeyLineRuleVectorsAgreeWithBrokerW3h** (2) |
| O02 | orchd 미리보기: 해시 없는 GitHub 행에 선택지 없음 | orchd api | 잡힘 | TestApplyForwardsDetachKeyAndKeyLine, TestBadRequestOnOtherDeleteActionsUnchanged, TestDeployKeyMismatchIsDefiniteRefusalNotUnknownOutcome, TestDetachKeyAvailableNeedsMatchingHash, TestDetachKeyUnavailableRefusedBeforeSignature, TestDetachSlotsComeBackOnlyWhenNothingHappenedW3g … (+18) (24) |
| O03 | orchd: 저장된 해시를 무시(항상 같음) | orchd api | 잡힘 | TestDetachKeyAvailableNeedsMatchingHash, **TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h**, **TestKeyLineRuleVectorsAgreeWithBrokerW3h**, **TestRemoteChangedOutranksKeyMismatchW3h** (4) |
| O04 | orchd `remove_deploy_key`: 키 불일치를 `remote_changed`보다 먼저 | orchd api | 잡힘 | **TestRemoteChangedOutranksKeyMismatchW3h** (1) |
| O05 | 403 로그: 경로 길이 제한 없음 | orchd api | 잡힘 | **TestRefusalLogTruncatesLongPathW3h** (1) |
| O06 | 403 로그: 자르되 `...` 표식 없음 | orchd api | 잡힘 | **TestRefusalLogTruncatesLongPathW3h** (1) |
| O07 | `remove_deploy_key` 불일치 문장이 "떼기를 끄라"고 함 | orchd api | 잡힘 | **TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h**, **TestKeyMismatchTextsDifferW3h** (2) |
| O08 | `remove_deploy_key` 불일치에 떼기용 문장(`errKeyMismatchDetach`)으로 답함 | orchd api | 잡힘(시험 보강 뒤) | **TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h** (1) |
| W01 | 웹 `detachRefused`에서 새 원인을 뺌 | node | 잡힘 | TestDetachRefusedAfterSignatureSaysNothingChangedR1 (1) |

**26개 중 25개가 잡혔고 1개(B10)는 동등 변이로 살아남았다.** 솔직히 기록할 것:
1. **B10은 동등 변이다**(H1 보고서 M5와 같은 것): 상수 시간 비교와 `!=`는 결과가 같고, 차이는 시간뿐이라 기능 시험으로는 구별할 수 없다. 비교 대상(키 줄과 그 해시)은 비밀이 아니므로 시간 차이로 새는 것도 없다(심층 방어로 둔다).
2. **O08은 처음에 살아남았다**: 두 op의 `deploy_key_mismatch`가 같은 코드·같은 상태라, `remove_deploy_key`가 떼기용 문장으로 답해도 기존 시험은 코드만 봤다. `TestForgedKeyLineRefusedBeforeSignatureAndBrokerW3h`에 "각 op가 자기 문장으로 답한다(`remove_deploy_key`에는 '를 끄고'가 없고 떼기에는 있다)"를 더한 뒤 다시 돌려 잡혔다(O07도 함께 다시 돌려 잡힘 확인).
3. **B07은 처음 실행에서 "살아남음"으로 나왔지만 시험 선택의 빈틈이었다**: `-run` 정규식에 `TestV4ToV5WritesBackupOnceNeverOverwritten`이 걸리지 않았다(`Registry`·`W3h` 어느 것도 이름에 없다). 패키지 전체로 다시 돌려 잡혔다. B10도 같은 이유로 패키지 전체로 다시 돌렸고 여전히 살아남았다.
4. **MF1·MF2·MC1·MC2·O04·O05·O06은 H3의 새 시험만 잡는다**: 시험은 변이 실행 전에 썼으므로 "변이로 찾은 빈틈"은 아니지만, 이 시험들이 없으면 살아남는 변이다(H1 보고서의 우려 "잠금 뒤 재대조만 빼는 변이는 따로 시험하지 않았다"가 닫혔다).
5. 처음 실행에서 O07의 바꿀 문자열이 파일에 두 번 나와 도구가 멈췄다(실행 전 검사). 더 좁은 문자열로 다시 써서 O07·O08·W01·B07·B10을 이어서 돌렸다.
6. "`remote: null`에서 해시 남기기"는 H1 보고서대로 **표현할 수 없는 변이**다(해시가 `remote` 안에 있어 `Remote = nil`이 함께 지운다). H1(M1~M18)·H2(M1~M15)의 변이는 이번에 다시 돌리지 않았다(각 보고서에 기록).
7. **제품 코드는 하나도 바꾸지 않았다**(주석과 웹 문장 한 곳 말고).

## 4. 배포 현실 표 (경로·소유자·모드·실행 사용자·샌드박스)

**이 표의 모든 "예상"은 코드·유닛 파일 읽기와 임시 폴더 시험에 기초한 것이고 실서버에서 본 것이 아니다.** 실행 사용자와 샌드박스는 W3g와 같다(29번 4장). **새 경로·새 유닛·새 플래그·새 설정 키·새 포트·DB 스키마 변경(v8)은 없다. 등록부 파일 버전만 v4 → v5.**

| 경로·대상 | 소유자 / 모드 | 접근하는 쪽 | W3h에서 달라지는 것 | 비고 |
|---|---|---|---|---|
| `/var/lib/orch-broker/registry.json` | `orch-broker` 0600 | broker만 | **v4 → v5**(시작할 때 한 번, 행 불변, 해시 없음). 그 뒤 만들기·연결한 행의 `remote`에 `key_sha256` | W3g broker는 v5를 읽지 못한다(`-check` 거부, 실행 중이면 `registry_unavailable`): 롤백은 등록부 편집(런북 18장 단계 4) |
| `/var/lib/orch-broker/registry.json.v4.bak` | `orch-broker` 0600 | broker(한 번 씀) | **새 파일**: 마이그레이션 직전의 v4 바이트. 이미 있으면 덮어쓰지 않는다 | 전환 직전 상태이므로 되살리면 전환 뒤 변경 전부가 되돌아간다(7장 4) |
| `/home/orch/orchd/ssh/id_gitlab_deploy.pub`, `…/keys/<id>/id_ed25519.pub` | `orch:orch` | orchd(읽기만) | 읽은 줄의 sha256을 행의 해시와 서명 전에 비교 | 줄을 바꾸면(주석만이라도) 그 전 행의 떼기가 거부된다(7장 1) |
| orchd 로그(journald) | — | orchd | **HTTP 403마다 한 줄** `orchd: refused 403 <코드> <메서드> <경로>` | 쿼리·헤더·본문·서명·토큰·키 줄 없음. 인증 없이도 만들 수 있다(7장 2) |
| broker 로그 | journald | broker | 변화 없음(성공 줄은 id만) | — |
| 웹 정적 파일 | orchd 바이너리 안 | orchd | `views/delete.js`: `DEL_FAIL.deploy_key_mismatch`(H2), `detachRefused` 원인 하나 추가(H3) | orchd를 갱신해야 보인다 |
| 원장 | 기존 | orchd | 변화 없음 | — |

## 5. 계획의 결정(H-D1~H-D7)과 작업 중 판정

| id | 결정 / 판정 | 틀렸을 때 비용 |
|---|---|---|
| H-D1 | 등록부 v5: `remote.key_sha256`(선택, 소문자 16진 64자, `provider` 필요). v4 → v5는 행 그대로·버전만, `.v4.bak` 한 번. 옛 broker는 v5 거부 | 롤백 때 등록부 편집이 필요 |
| H-D2 | 저장 3곳(create, link 새 행, attach 기존 행)에 서명·검증된 `deploy_key_sha256` | 하나라도 빠지면 그 경로의 GitLab 행은 영영 떼기 거부(시험·변이 B01~B03) |
| H-D3 | 떼기 때 모양 단계(서명 검증·ID 소모 전)에서 상수 시간 대조, 다르면 `deploy_key_mismatch`. 해시 없음: GitHub W3g 규칙, GitLab 거부. 대조 뒤 `classifyKeys` 그대로 | GitLab 옛 행은 이 도구로 떼지 못함(사람이 GitLab 화면에서) |
| H-D4 | 키 교체(공용 `.pub` 변경, 주석만이라도)는 그 전 행의 떼기를 거부하게 한다(안전한 실패), 런북에 수동 절차 | 사람이 떼기 없이 지우고 키를 화면에서 뗌 |
| H-D5 | orchd가 `key_sha256`을 읽는다(관대한 읽기). **H1 검토 확장**: `deploy_key_mismatch` → 확정 409(두 op), 미리보기 `detach_key_available`을 broker 규칙으로 서명 전에 | — |
| H-D6 | orchd HTTP 403마다 고정 한 줄 로그, 비밀 없음, 원장 불변 | 로그 양(7장 2) |
| H-D7 | 멈추는 시험에 시간 제한(운영 코드 불변) | — |
| H1 검토 판정 | (1) H-D5 확장(위), (2) `registry.go` `case "4"`의 중복 nil 검사 정리(H2에서), (3) 런북의 롤백 서술 정정: `.v4.bak` 복원은 전환 뒤 등록부 변경 전부를 되돌리므로 편집(해시 없으면 버전만, 있으면 `key_sha256` 삭제 + 버전 4)을 권하고, 재전환 전 낡은 `.v4.bak`을 치운다, (4) 변이 표에 MF(잠금 뒤 재대조만 생략)·MC(거부 코드 우선순위), MF는 경쟁 seam 시험으로 | — |
| H2 검토 판정 | 작은 지적 6개: #1 `MaxBytesReader` 신호 손실 주석, #4 경로 길이 시험, #5 두 문장 구분 시험, #6 웹 `detachRefused` 원인 추가 → H3에서 반영(1장). #2 인증 없는 403의 로그 양, #3 `forbidden_uid`의 두 줄 → 알려진 한계로 기록(7장 2·3) | — |

## 6. 실서버·실제 GitHub·GitLab에서 확인하지 못한 것 (NOT verified on the real server)

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 18장)에서 처음 확인한다.** 시험은 가짜 GitHub·가짜 GitLab·가짜 broker·임시 폴더로 요청 모양과 흐름만 확인했다.
- **실제 등록부의 v4 → v5 마이그레이션**: 운영 등록부(`toy`·`sample-app`)가 행 그대로 v5가 되는지, `registry.json.v4.bak`의 소유자·모드·sha256, 샌드박스 안에서의 쓰기(`ProtectSystem=strict`, `ReadWritePaths=/var/lib/orch-broker`).
- **실제 GitHub(및 GitLab)에서 저장된 해시로 하는 키 떼기**: 실제 패스키로 만든 `create_project` 서명의 `deploy_key_sha256`이 행에 저장되고, 그 프로젝트의 키 폴더 `.pub` 줄의 sha256과 같으며(런북 18장 3-2 (b)), 그 해시로 `remove_project`(떼기)가 실제로 키를 떼는지.
- **운영 리스너의 403 진단 로그**: HTTPS 앞단 뒤에서 일부러 만든 403(런북 2-1)이 한 줄만 남는지, 앞단이 `Origin`을 그대로 넘기는지, journald의 실제 모양.
- **GitLab 키 떼기(17장 3-4)는 W3g 컷오버에서도 하지 않았고 여전히 실서버에서 해 본 적이 없다.** 공용 키의 두 번째 프로젝트 제목(17.0 (c))도 아직 보지 않았다.
- **롤백 편집 스크립트를 실제 서버의 root·`orch-broker` 소유 파일에 돌린 적이 없다**(시험은 같은 스크립트를 사용자 권한의 임시 폴더에서 돌렸다: `os.chown`이 같은 소유자로만 확인됨).
- **혼합 버전 화면**(W3h broker + W3g orchd의 "결과를 알 수 없음")은 브라우저로 본 적이 없다.
- **리눅스의 동작**은 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- W3g(31번 6장·9장 "아직 보지 못한 것")에서 이월된 미확인은 그대로다.

## 7. 받아들인 한계 (알고서 둔 것)
1. **해시는 키 줄 전체(종류·본문·주석)를 덮는다.** 그래서 공용 GitLab `.pub`의 **주석만 바꿔도** 그 전에 만든 GitLab 행은 떼기가 거부된다(GitHub은 그 프로젝트의 키 폴더 `.pub`을 바꾼 경우). 안전한 쪽의 실패이고 해시를 다시 채우는 길은 없다: 떼기 없이 지우고 키는 제공자 화면에서 사람이 뗀다(런북 18장 "키 교체"). 본문만 해시했다면 주석 변경에 견디지만, 서명된 값(`deploy_key_sha256`)이 이미 줄 전체의 해시라 그 값을 그대로 쓰는 쪽을 골랐다(변이 H1 M17이 고정).
2. **인증 없는 403의 로그 양(H2 검토 #2, 받아들임).** 콘솔 주소에 닿을 수 있는 누구나 `Origin`이 다른 요청으로 `bad_origin` 403과 그 로그 줄을 만들 수 있다. 줄은 고정 모양·256바이트 경로 제한이고 비밀이 없으며, orchd 자체에는 로그 속도 제한이 없다(journald의 기본 속도 제한이 적용될 것으로 예상: 서버에서 확인하지 않음). 모든 403을 남기는 것이 W3g 컷오버의 원인 미확정 403(31번 9장)을 다시 보지 않기 위한 목적이다.
3. **`forbidden_uid`는 두 줄(H2 검토 #3, 받아들임).** 소켓 사용자 거부는 기존 진단 줄 `socket peer uid N refused`와 새 줄 `orchd: refused 403 forbidden_uid …`가 함께 남는다.
4. **`.v4.bak`의 의미.** 마이그레이션 직전의 v4 바이트 그대로이고 한 번만 쓰며 덮어쓰지 않는다. 그래서 (a) 되살리면 전환 뒤의 등록부 변경 **전부**(만들거나 연결한 행, 지운 행)가 되돌아가고(권장 롤백은 편집: 런북 18장 단계 4), (b) 롤백한 뒤 그대로 두면 다음 전환은 낡은 파일을 그대로 두고 새 백업을 쓰지 않는다(런북 18.0 (b)·단계 4 A-5에서 옮긴다).
5. **전환 전의 GitLab 행은 이 도구로 키를 뗄 수 없다**(해시가 없다). 롤백했다가 다시 올리면 그사이 만든 행도 그렇게 된다. 지금 서버에는 그런 행이 없다고 예상한다.
6. **MF 경쟁(모양 단계와 잠금 사이에 행의 해시가 바뀜)에서는 요청 ID가 쓰인다**(2장 "브리프와 다른 점" 1). 아무것도 지워지지 않고 새 서명이 필요하다. 이 경쟁은 같은 프로젝트의 등록부 변경이 그 몇 밀리초 안에 끼어들 때만 생긴다.
7. **`remove_deploy_key`에서 원격 번호 없는 행의 거부 코드 차이**: orchd는 키 줄이 해시와 다르면 `deploy_key_mismatch`, broker는 `remote_changed`(broker는 번호 없음을 먼저 본다). 둘 다 서명 전의 확정 거부이고 만들기·연결 경로는 번호 없는 원격을 쓰지 않는다. 시험 `TestRemoteChangedOutranksKeyMismatchW3h`(orchd)가 이 차이를 고정한다.
8. **`"key_sha256":null`은 "없음"으로 읽힌다**(Go 디코더, `remote_deleted_at`과 같은 기존 한계, H1 보고서). 없음은 더 엄격한 쪽(GitLab 거부)이다. 프로젝트 행 안쪽 키의 대소문자 차이(`Key_SHA256`)도 Go가 같은 키로 읽는다(기존 v4 필드와 같은 한계).
9. **혼합 버전(W3h broker + W3g orchd)**에서는 해시 없는 GitLab 행에도 선택지가 보이고 서명 뒤 거부가 "결과 알 수 없음"으로 보이며 한도 칸을 쓴다. 갱신 순서로 피한다.
10. W3g의 한계(31번 7장)는 그대로 적용된다. 단 7장 1(GitLab에서 서명되지 않는 키 줄이 키를 고름)은 **해시를 가진 행에서는 닫혔다**(다른 줄은 서명 전 거부). 해시 없는 GitHub 행은 W3g 규칙(제목+본문)이 지킨다. 7장 3(공용 `.pub`을 바꾼 경우의 잘못된 "이미 없음")도 해시 있는 행에서는 "이미 없음" 대신 서명 전 거부가 된다.

## 8. 사용자 결정 목록

이미 정해진 것(2026-11-14): 31번 8장 1의 열린 결정 → **(a) 등록부에 해시 저장**("(a)로 구현 시작해줘"), 판정 H1·H2 검토(5장).

컷오버 때 사용자가 정하거나 해야 하는 것(런북 18장):
1. **배포 여부와 시점**(이 브랜치는 병합·푸시·배포하지 않았다).
2. **시험용 새 저장소의 이름**(3-2의 GitHub 시험 저장소 A), GitLab 키 떼기 시험(3-4)을 할지(17장 3-4의 조건을 먼저 만족해야 한다).
3. 롤백이 필요해질 때 **편집(권장)과 `.v4.bak` 복원 중 무엇을 쓸지**(런북 18장 단계 4).
4. `/root/backup-w3h`를 언제 지울지.

## 9. 컷오버 기록
2026-11-14 15:13~15:15 KST, 사용자 요청("1": 병합 + 서버 반영)으로 main(`ac2c1ec`)에 fast-forward 병합(push 안 함)하고 서버(orch-vm)에 반영했다. 아래는 컨트롤러가 서버에서 읽기 전용으로 **실제로 본 것**이다. 단계 3(웹 시험)은 아직 하지 않았다.

**본 것**
- 사전 점검(18.0 a~d): broker·orchd active(W3g), 등록부 `"version":4`(sha256 `cb9a3f67…`, W3g 반영 직후와 같음), `registry.json.v4.bak` 없음, 공용 `.pub` 필드 3개, 키 폴더 비어 있음, 실행·대기·승인 대기 작업 없음(마지막 작업 `48 done`).
- 새 바이너리 4개의 해시가 Mac 빌드와 일치(orchd `d5e5abc6…`, orch-broker `07e0ed24…`).
- 1-2·1-3: `/root/backup-w3h`(root 0700)에 옛 바이너리 4개(해시 기록)·`tree-before.txt`·`registry.json.w3g`(v4 사본, 원본과 해시 같음). **일어난 일:** 처음 실행에서 `tree-before.txt`를 쓰는 한 줄이 리다이렉트 권한 오류로 멈췄다(`set -e`: orchd만 멈추고 바이너리 백업만 끝난 상태, broker·등록부는 건드리지 않음). `sudo tee`로 다시 만들고 이어서 진행했다. 런북의 해당 명령 모양은 이 서버에서 그대로는 안 된다(아래 "런북 보완").
- 1-4 샌드박스 `-check`: 통과, 종료 후에도 `"version":4`, `.v4.bak` 없음, 등록부 sha256 불변(= `-check`가 등록부를 쓰지 않음).
- 1-5 시작: `active`, 등록부 `"version":5`, `registry.json`·`registry.json.v4.bak` 모두 `orch-broker:orch-broker 600`, **`.v4.bak`의 sha256이 전환 직전 사본과 같음**(`cb9a3f67…`), `key_sha256` 0개, `.state-*` 임시 파일 0개.
- 1-6 읽기 op: 제공자 `example-group-gitlab`·`swlee3306-github`, 프로젝트 `toy`·`sample-app`(둘 다 `origin: legacy`, `provider`·`remote` 없음), orphan 0개, `status`·`tokens` ok. W3g와 같다.
- 단계 2: orchd `active`, 로그에 `approvals: mode signed (3 signers)`·HTTP 리스너·소켓 uid, 오류 없음, `orch list` 마지막 번호 48 그대로, 해시 일치. 로그인 없이 `removal-preview`·`remove/propose`·`remove-key/propose`가 **401**(서버 안에서 프록시가 보내는 `Host`·`Origin`을 붙여 호출).
- **2-1 403 진단 로그**: 일부러 만든 `bad_origin` 403 하나에서 로그에 `orchd: refused 403 bad_origin GET /api/v1/auth/session`이 **정확히 한 줄**, 쿼리 표식 `w3hmarker`는 **0건**, 시작 뒤 오류 줄 0건. (서버 안에서 직접 호출했으므로 HTTPS 앞단 프록시를 거친 경로는 보지 않았다.)

**런북 보완(다음 개정에서 고칠 것)**: 1-2의 `sudo sh -c "find … -printf '…\n' > $B/tree-before.txt"` 모양은 원격 셸 인용 때문에 리다이렉트가 바깥 셸에서 실행되어 권한 오류가 났다. `sudo sh -c "find …" | sudo tee $B/tree-before.txt`로 쓰면 된다.

**아직 보지 못한 것**
- 단계 3 전체: 새로 만든 행이 해시를 갖는지(3-2 (b)), 그 해시로 키 떼기가 되는지(실제 GitHub), `toy`·`sample-app`의 "삭제…" 화면에 떼기 선택지가 없는지(3-1), 3-5 누출 점검.
- 로그인한 정상 사용 중 403 로그, GitLab 키 떼기(3-4), HTTPS 앞단 프록시를 거친 403 로그 모양.
