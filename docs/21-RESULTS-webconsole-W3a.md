# 웹 콘솔 W3a — 결과 기록 (2026-11-10)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소(브랜치 `impl/webconsole-w3a`)에서 끝났고, 실제 서버(orch-vm)에는 아직 아무것도 배포하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 12장, 설계는 `19-DESIGN-webconsole-W3-projects.md`, 계획은 `20-PLAN-webconsole-W3a.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, **시험용 서명자·가짜 broker 클라이언트·임시 폴더**, 실제 `git`)만 가리킨다. **실제 패스키 서명, 리눅스 systemd 샌드박스 안의 broker, 실제 `broker.json`·`signers.json`·`/home/orch/repos`는 한 번도 쓰지 않았다**(5장). 이 작업에서 실제 키를 만들거나 요청하지 않았고 어떤 서버에도 접속하지 않았다.
> **컷오버 전에 사용자가 알아야 할 것 하나:** `orch-broker.service`의 `ProtectHome=yes` 때문에 **실서버에서는 모든 프로젝트 등록이 `path_not_allowed`로 실패할 것으로 추정**한다(코드·유닛을 읽은 추정, 미확인). 유닛을 바꿔야 하는지는 사용자 결정이다(런북 12.0 (d), 이 문서 5·6장).

## 1. 만든 것

**A1. 서명 메시지 `aigate-registry-v1`.** 줄 순서가 고정된 정규 텍스트(`action`, `subject`, `change`, `request`, `issued`). `change`는 화면 문구가 아니라 **정규 JSON(키 사전순·공백 없음)의 SHA-256**이다(`internal/registrymsg`: broker와 orchd가 같이 쓰는 입출력 없는 작은 패키지). 알려진 필드만 받고(`register_project`: `path`·`kind`·`ai_context`, `set_flag`: `flag`·`value`), 중복 키·실수·null·중첩·뒤 데이터·제어/방향 전환 문자·잘못된 UTF-8을 거부한다. `signing.VerifyRegistry`는 `Verify`와 같은 의미(서명자 목록, 알고리즘 일치, 5분 신선도, 신선도 생략 불가)에 SSH 네임스페이스만 **`aigate-registry`**(승인은 `aigate-approval`)로 하고 WebAuthn은 메시지 SHA-256을 challenge로 쓴다. 기존 `Verify`의 동작과 시험은 바뀌지 않았다.
**A2. broker의 프로젝트 등록부.** `<state-dir>/registry.json`(v1, `orch-broker` 소유 0600, 폴더 0700, 원자적 쓰기: 임시 파일 0600 + fsync + rename + 폴더 fsync), `used-requests.json`(쓴 요청 ID, 1시간, 최대 5000, 재시작에도 유지). 파일이 **없을 때만** `broker.json`의 `context_repos`·`gitlab.projects`를 한 번 옮기고(`link()`로 배타적 생성: 늦게 생긴 파일을 덮어쓰지 않음), 그 뒤 옛 항목은 무시한다. 깨진·소유자/모드가 틀린·심볼릭 링크·1 MiB 초과 파일은 "없음"이 아니라 **깨짐**으로 보고 모든 op를 `registry_unavailable`로 거부한다(옛 설정으로 되돌아가지 않음). `refine`의 허용 목록, `create_mr`의 GitLab 프로젝트, `status`의 `context_repos`는 등록부에서 읽는다. `orch-broker -check`는 읽기 전용으로 등록부를 검사한다(깨졌으면 거부, 등록부가 있는데 옛 항목이 남았으면 경고만).
**A3. broker의 `projects`·`register_project`·`set_flag`.** `projects`는 읽기·서명 불필요·비밀 없음. 쓰기 op의 순서: 엄격한 인자 파싱(정확한 대소문자의 유일한 키) → `set_flag`의 flag가 `ai_context`가 아니면 `not_supported`(서명 전) → 등록부 사용 가능 → 서명 있음(`not_signed`) → **`change` 필수**(없으면 `change_mismatch`, ID 소비 없음) → 서명자 목록을 **호출마다 다시 읽음**(`signers_unavailable`) → `VerifyRegistry`(broker 시계 기준) → 서명된 change == 요청에서 다시 계산한 change(`change_mismatch`) → **그제야** 요청 ID 소비(`request_reused`) → 경로 규칙 → 등록부 갱신. 경로 규칙은 broker가 직접 판정한다: 4096바이트 이하, 절대·정리된 경로, **`repo_roots`의 바로 한 단계 아래**, `lstat`로 심볼릭 링크가 아닌 폴더, `.git`은 폴더이거나 일반 파일. 파일 내용은 읽지 않는다. 오류는 전부 고정 코드·고정 문장이고 서명 검증 오류 문구는 전달하지 않는다. 쓰기 op의 시간 제한은 10초이며 `timeout`은 "결과 모름"이다.
**A4. orchd 엔드포인트.** `GET /api/v1/projects/registry`(등록부 목록, 한 항목이라도 이상하면 전체가 `broker_error`), `GET /projects/candidates`(허용 루트 바로 아래의 등록되지 않은 저장소와 정책 클래스 이름 `kinds`), `POST /projects/propose`(정규 메시지·`change`·`request`·`issued`·화면용 한국어 문장 `preview`를 만들 뿐 **broker를 부르지 않고 아무것도 저장하지 않는다**), `POST /projects/apply`(서버가 `fields`에서 `change`를 다시 계산해 맞을 때만, 서명은 모양만 검사하고 그대로 broker로 전달: **서명이 유효한지는 broker의 몫**). 쓰기 실패 중 실제로 적용됐을 수 있는 것(`timeout`, 요청을 다 쓴 뒤 끊김, 알 수 없는 코드)은 `outcome_unknown` 504로 알리고 다시 보내지 않는다. 주체별 한도 `propose` 60/h, `apply` 30/h. 경로 글자는 인쇄 가능한 ASCII와 한글 음절만. 원장 `project_change` 이벤트는 `{actor, action, subject, ok, code}`뿐(경로·서명·내용 없음). `GET /api/v1/projects`(저장소별 작업 요약)는 그대로다.
**A5. 웹.** `#/projects`: 목록(id, 경로, 종류, AI 전송 허용 여부, GitLab 이름·자동 병합은 읽기 전용 표시), "프로젝트 추가"(후보 → 설정 → **변경 내용 전체 확인 화면** → 패스키 서명 → 결과), 행별 AI 전송 켜기/끄기(같은 흐름). 확인 화면의 서명 단추는 **브라우저가 자기 값으로 다시 계산한 정규 JSON·SHA-256·메시지가 서버가 준 것과 모두 같을 때만** 열리고, 서버 문장은 텍스트 노드로만 넣으며 AI 문장 하나가 브라우저의 AI 값과 일치해야 한다. 서명 뒤에는 어떤 결과든 결과 화면으로 가고 같은 제안을 다시 서명하지 못한다. 저장소(localStorage 등)·`confirm/alert`·인라인 스크립트·스타일 없음.
**A6. 이번 작업.** 적대 시험(2·3장), 변이 시험(3장), 이 문서, 컷오버 런북(`10-OPERATIONS-orchestrator.md` 12장), `infra/README.md`·`19번` 문서의 상태 줄. **코드(제품) 변경은 없다**: 시험 파일과 문서만 바꿨다(시험 보조 함수 `newWEnvMod`와 로그 버퍼 필드를 `projects_test.go`에 더했다).

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-10, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0, 코드·시험의 마지막 커밋 `55672a7`(`test: adversarial suite for W3a`; 이 문서 커밋은 코드를 바꾸지 않음). 세는 방식은 W2b(18번 문서 2장)와 같다: 최상위 시험 함수와 하위 시험(`t.Run`)을 따로 센다(`go test -json` 결과 줄에서 집계).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l .`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**, 표준 오류 출력 없음. 최상위 시험 함수 1194개 중 **1192 통과, 0 실패, 2 건너뜀**(하위 시험 808개 모두 통과). 건너뜀 2개는 W1~W2b와 같다: `internal/orch/web` `TestDevServer`(수동 개발 서버), `internal/pathfold` `TestCollectionsPerformance`(-race 빌드에서만 건너뛰는 시간 검사).
- `node --test "internal/orch/web/test/*.test.js"`: **165개 전부 통과**(W2b 137개 + W3a 28개).
- **A5 게이트:** `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api` → `--- PASS: TestConstantsAgreeWithWeb`, **`SKIP` 없음**(웹 파일이 없거나 상수가 어긋나면 `t.Fatal`이므로 건너뛰는 경로가 없다). 이 명령은 런북 12장 1-0(빌드 전 확인)에도 들어 있다.
- W2b(18번 문서) 대비 최상위 통과 1084 → 1192(+108). 새 패키지: `internal/registrymsg`(4개). 늘어난 곳: `internal/broker` 74 → 138, `internal/orch/api` 238 → 264, `internal/approval/signing` 55 → 63, `cmd/orch-broker` 5 → 7, `internal/orch/refine` 13 → 17.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 53 | 0 | 0 | 44 |
| `cmd/orch-broker` | 7 | 0 | 0 | 0 |
| `cmd/orchd` | 31 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 33 | 0 | 0 | 80 |
| `internal/approval/signing` | 63 | 0 | 0 | 76 |
| `internal/broker` | 138 | 0 | 0 | 114 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 60 | 0 | 0 | 63 |
| `internal/gitx` | 45 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 264 | 0 | 0 | 27 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/broker` | 5 | 0 | 0 | 0 |
| `internal/orch/machine` | 34 | 0 | 0 | 14 |
| `internal/orch/policy` | 9 | 0 | 0 | 25 |
| `internal/orch/policy/autonomy` | 9 | 0 | 0 | 0 |
| `internal/orch/policy/propose` | 9 | 0 | 0 | 0 |
| `internal/orch/publish` | 17 | 0 | 0 | 0 |
| `internal/orch/refine` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 66 | 0 | 0 | 24 |
| `internal/orch/store` | 43 | 0 | 0 | 4 |
| `internal/orch/vm` | 88 | 0 | 0 | 5 |
| `internal/orch/web` | 9 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 | 12 |
| `internal/registrymsg` | 4 | 0 | 0 | 0 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report` | 8 | 0 | 0 | 0 |
| `internal/rules` | 7 | 0 | 0 | 33 |
| `internal/runner` | 14 | 0 | 0 | 2 |
| `internal/stats` | 1 | 0 | 0 | 0 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

테스트 파일이 없는 패키지(`internal/approval/ui`, `internal/gittest`, `internal/refinesize`)는 표에서 뺐다.

이번(A6)에 더한 시험(이름 그대로, 실제 개수 18개 + 기존 1개 확장):
- `internal/broker/adversarial_w3a_test.go` **12개**: `TestRegistryNeverWrittenWithoutValidSignature`, `TestForeignUIDCannotWrite`, `TestOrchdIdentityCannotReadOrWriteRegistryFile`, `TestReplayAfterRestartRefused`, `TestSignatureOverDifferentChangeRefused`, `TestSSHNamespaceMismatchRefused`, `TestApprovalMessageCannotAuthorizeRegistryChange`, `TestSignerRemovedTakesEffectImmediately`, `TestLegacyMigrationOnlyWhenRegistryAbsent`, `TestBrokenRegistryDoesNotWidenAIContext`, `TestStatusAndProjectsNeverContainSecrets`, `TestWriteOpContractChangeRequiredIDKeptAndExactKeys`(A3 계약: `change` 필수, 대소문자·중복 키, 거부가 ID를 소비하지 않음).
- `internal/orch/api/adversarial_w3a_test.go` **6개**: `TestProposeNeverWritesBroker`, `TestApplyForgedFieldsRefusedBeforeBroker`(위조 본문 51가지), `TestProjectsLedgerNoPathsOrContent`, `TestProjectWritesBoundedAndTimeoutIsUnknown`(A4 계약: `timeout`·끊김은 `outcome_unknown`·재시도 없음·주체별 한도), `TestOrchdForwardsSignatureAndNeverJudgesIt`, `TestNoBrokerImportInOrchd`(`go list -deps ./cmd/orchd`에 `internal/broker` 없음, 대조군으로 `registrymsg`·`orch/broker`가 있음).
- 기존 `TestOrchdWorksWithBrokerDown`(`adversarial_w2b_test.go`, 이름이 같아 새로 만들 수 없었다)에 W3a를 더했다: broker가 없을 때 `registry`·`candidates`는 고정 `broker_unavailable`(소켓 경로 노출 없음), `propose`는 200, `apply`는 503, 옛 `GET /projects`는 200.

적대 시험이 확인하는 것(요약. 모든 시험에는 같은 요청을 정직하게 만든 **대조군**이 붙어 있어 전부 거부하는 것만으로는 통과하지 못한다):
| 시험 | 공격 | 기대(확인함) |
|---|---|---|
| `TestRegistryNeverWrittenWithoutValidSignature` | 소켓으로 19가지 실패(서명 없음·null, 뒤집은 서명, 승인 네임스페이스, 모르는 서명자, 다른 키의 서명자 ID, 6분 전·6분 뒤, `change` 없음·틀림, 내용 바꿈, 다른 요청의 서명, 여분 필드, 지원하지 않는 flag, 서명자 파일 없음·깨짐·그룹 쓰기 가능·비어 있음)와 쓰레기·`null`·큰 요청 | 모든 경우 정해진 고정 코드, **등록부와 `used-requests.json`의 바이트가 그대로**, `.state-*` 임시 파일 없음. 대조군: 같은 요청이 그 뒤 성공(ID가 소비되지 않았음) |
| `TestForeignUIDCannotWrite` | 허용 uid 밖(그리고 자격 확인 오류) 호출자가 완전히 유효한 서명 요청과 읽기 op 전부를 보냄 | 모두 `forbidden`, 상태 바이트 불변. 대조군: 허용된 호출자가 **같은 요청**을 보내면 성공 |
| `TestOrchdIdentityCannotReadOrWriteRegistryFile` | orchd 계정이 상태 파일을 읽거나 쓰는 상황. macOS에서는 uid를 바꿀 수 없어 **소유자·모드 단언**으로 확인: 폴더 0700, 등록부·사용 ID 파일 0600, 소유자는 broker 계정. 폴더 0750/0755, 파일 0640/0644/0602/0400, 심볼릭 링크, 사용 ID 파일 0664, 소유자가 다른 파일(시임) | 전부 broker가 "신뢰할 수 없음"으로 보고 읽지 않고 쓰기를 거부 |
| `TestReplayAfterRestartRefused` | 같은 요청을 실행 중에, broker 재시작 뒤에, 다른 내용으로 | `request_reused`, 상태 불변. 대조군: 새 ID는 성공 |
| `TestSignatureOverDifferentChangeRefused` | ssh·WebAuthn 서명자: AI 꺼짐으로 서명받은 등록을 켜짐으로 바꿈(`change`는 그대로 → `change_mismatch`, 새 내용의 `change`로 재계산 → `bad_signature`), 다른 프로젝트의 서명을 재지정, `set_flag` 값 뒤집기 | 모두 거부, 상태 불변. 대조군: 서명받은 그대로는 성공하고 AI는 꺼진 채 등록됨 |
| `TestSSHNamespaceMismatchRefused` | 네임스페이스 `aigate-approval`·`aigate-login`·`file`·`git`·공백/대문자 변형·빈 값으로 서명 | 모두 `bad_signature`, 상태 불변 |
| `TestApprovalMessageCannotAuthorizeRegistryChange` | 정상적이고 신선한 **승인 서명**(SSH `aigate-approval`, WebAuthn)을 등록 변경의 서명으로 제출. 반대로 등록 서명을 `signing.Verify`(승인)에 제출 | 앞은 `bad_signature`, 뒤는 검증 실패. 대조군: 각 서명은 자기 종류에서는 검증됨 |
| `TestSignerRemovedTakesEffectImmediately` | broker가 도는 중에 서명자 목록에서 서명자를 지움, 목록을 비움 | 지운 서명자의 다음 요청부터 `unknown_signer`(재시작 없이), 남은 서명자는 영향 없음, 다시 넣으면 **같은 요청**이 성공(거부가 ID를 쓰지 않음) |
| `TestLegacyMigrationOnlyWhenRegistryAbsent` | 등록부가 있는 채로 `context_repos`·`gitlab.projects`를 늘려 재시작, AI를 끈 뒤 재시작, 파일을 깨뜨림, 파일을 지움 | 있을 때는 옛 항목이 무시되고(바이트 불변, AI 끈 것이 켜지지 않음), 깨졌을 때는 이전하지 않고 건드리지 않으며, 지웠을 때만 다시 이전 |
| `TestBrokenRegistryDoesNotWidenAIContext` | 깨진 등록부 4종(쓰레기, 빈 파일, 모르는 키, 자동 병합 켜짐) + 옛 설정은 `repo-a`를 허용 | `AIContextPaths`가 비어 있고(`context_repos` 없음), `status`·`projects`·`refine` 모두 `registry_unavailable`, 가짜 OpenAI 호출 0, 파일 불변. **브리프의 "`context_repos` 비어 있음"은 실제로는 `status`가 `context_repos` 없이 오류로 답하는 것이다** |
| `TestStatusAndProjectsNeverContainSecrets` | 등록·변경 뒤 `status`·`projects`와 실패 응답·로그를 훑음 | 키·토큰·`Bearer`·`PRIVATE-TOKEN`·서명자 공개키·키 파일 경로·`signature` 없음, 프로젝트 객체의 키가 정확히 7개 |
| `TestWriteOpContract…` | `change` 없음·빈 값, 키 8개의 대문자 변형, 최상위·서명 객체 안의 중복 키 | 모두 `change_mismatch`/`bad_request`, 상태 불변, **같은 요청 ID가 그 뒤 성공** |
| `TestProposeNeverWritesBroker` | 12가지 제안(정상·비정상·유효하지 않은 `set_flag`·알 수 없는 동작·서명이 섞인 본문·쓰레기) | broker 호출 0(읽기도 0), 원장 `project_change` 없음. 대조군: apply가 오면 쓰기 정확히 1 |
| `TestApplyForgedFieldsRefusedBeforeBroker` | 위조 본문 51가지: 여분·대소문자·중복 키, 필드 타입, `change`·요청·시각 형식, 서명 모양, 경로 규칙(상대·`..`·정리 안 됨·루트 밖·하위 폴더·링크·저장소 아님·없음·방향 전환 문자·개행), 정책에 없는 종류, 허용되지 않는 flag, 뒤 데이터 | 전부 4xx, **broker 호출 0**. 대조군: 정직한 본문은 통과해 쓰기 1 |
| `TestProjectsLedgerNoPathsOrContent` | 경로·broker 문장에 표지 문자열을 심고 broker 응답 7종(성공, 이미 있음, 서명 오류, 시간 초과, 끊김, 쓰레기, 메시지 속 표지) | 원장에 표지·경로·종류·서명·서명자 ID 없음, 이벤트 payload가 정확히 5개 키, 이벤트 수 = 적용 수, 해시 사슬 검증 통과, 응답에 broker 문장 없음 |
| `TestProjectWritesBoundedAndTimeoutIsUnknown` | broker가 `timeout`/끊김으로 답함, 한 주체가 31번째 적용 | 504 `outcome_unknown`, 전달 정확히 1번(재시도 없음), 31번째는 429이고 broker 쓰기는 30에서 멈춤 |
| `TestOrchdForwardsSignatureAndNeverJudgesIt` | 이상한 알고리즘·서명자 ID·ssh에 WebAuthn 자료·WebAuthn에 자료 없음 | orchd는 모양만 보고 **그대로 전달**, 사용자가 보는 결과는 broker의 `bad_signature` 403(서명을 orchd가 판정하지 않음) |
| `TestNoBrokerImportInOrchd` | `go list -deps ./cmd/orchd` | `internal/broker`·`cmd/orch-broker`·OpenAI 클라이언트 없음, 대조군 의존성 있음 |

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 저장소를 **작업 트리 밖의 새 임시 폴더(`$TMPDIR/w3a-mut-a6`)로 복사**(`rsync`, `.git`·`.superpowers` 제외)해 한 곳만 망가뜨리고 관련 패키지(`go test`, 웹은 `node --test`)를 돌렸다. 변이마다 원본에서 파일을 다시 복사해 되돌렸고, 끝난 뒤 복사본을 지웠다. 작업 트리는 건드리지 않았다. 컴파일되지 않는 변이는 잡힌 것이 아니라서(W2b에서 겪은 일) `false && (…)`·`error(nil)` 형태로 컴파일되게 만들었다(①과 ⑩b는 첫 시도가 "선언만 하고 쓰지 않음" 빌드 실패라 고쳐서 다시 돌렸다). 전부 구현자 실행이다(검토자가 아직 재현하지 않음). **브리프의 10개 행 모두 한 번에 잡혔고 시험을 새로 더할 필요가 없었다**(잡히지 않은 변이 0). 굵은 글씨는 이번(A6)에 더한 시험.

| # | 망가뜨린 곳 | 잡은 시험(굵게 = A6) |
|---|---|---|
| ① | broker가 `signing.VerifyRegistry` 결과를 항상 성공으로 취급 | **TestRegistryNeverWrittenWithoutValidSignature**, **TestApprovalMessageCannotAuthorizeRegistryChange**, **TestSSHNamespaceMismatchRefused**, **TestSignatureOverDifferentChangeRefused**, **TestSignerRemovedTakesEffectImmediately**, TestRegisterRequiresValidSignature, TestSignerRemovalTakesEffectImmediately, TestInvalidSignatureDoesNotConsumeRequestID, TestFailedRequestsDoNotConsumeRequestID, TestSetFlagAIContextOnlyAndSigned, TestSignatureNotReusableForOtherActionOrSubject, TestWriteOpsNeverChangeRegistryOnAnyFailure |
| ② | `signed != want`(서명된 `change`와 다시 계산한 `change`의 비교) 제거 | **TestRegistryNeverWrittenWithoutValidSignature**, **TestSignatureOverDifferentChangeRefused**, TestRegisterChangeMismatchRefused, TestInvalidSignatureDoesNotConsumeRequestID, TestWriteOpsNeverChangeRegistryOnAnyFailure |
| ③ | 요청 ID 1회성(`Used.Mark`) 제거 | **TestReplayAfterRestartRefused**, **TestOrchdIdentityCannotReadOrWriteRegistryFile**(사용 ID 파일이 없어서), TestRegisterRequestIDOnce, TestConcurrentIdenticalRequestsRegisterOnce, TestRegisterUsesBrokerClockForUsedRequests, TestValidSignatureConsumesRequestIDEvenWhenLaterCheckFails, TestWriteOpsNeverChangeRegistryOnAnyFailure, TestWriteOpsRefuseWhenRegistryBrokenOrUsedFileBroken |
| ④ | `VerifyRegistry`가 신선도를 건너뜀(`SkipFreshness = true`) | **TestRegistryNeverWrittenWithoutValidSignature**(6분 전·뒤), TestVerifyRegistryFreshness(signing), TestRegisterRequiresValidSignature, TestFailedRequestsDoNotConsumeRequestID, TestSetFlagAIContextOnlyAndSigned |
| ⑤ | 등록 경로 규칙(`pathAllowed`) 전체 제거 | TestRegisterPathRules, TestRegisterGitEntryRules, TestRegisterPathRefusedWithoutRepoRoots, TestRegisterDuplicateIDOrPath, TestValidSignatureConsumesRequestIDEvenWhenLaterCheckFails (**새 적대 시험은 잡지 못함**: 경로 규칙은 A3 시험이 지키고, A6의 broker 시험은 경로 규칙을 공격하지 않는다. orchd 쪽 경로 위조는 `TestApplyForgedFieldsRefusedBeforeBroker`가 본다) |
| ⑥ | 깨진 등록부를 "없음"으로 취급(메모리에서 옛 설정으로 대체) | **TestBrokenRegistryDoesNotWidenAIContext**, **TestLegacyMigrationOnlyWhenRegistryAbsent**, TestBrokenRegistryMakesOpsUnavailableNotLegacy, TestProjectsReadIsOpenAndSecretFree, TestRegistryBrokenIsFailClosedNotMigrated, TestRegistryMigrationDoesNotClobberLateFile, TestWriteOpsRefuseWhenRegistryBrokenOrUsedFileBroken |
| ⑦ | 마이그레이션이 항상 실행(등록부가 있어도 옛 설정이 이김) | **TestLegacyMigrationOnlyWhenRegistryAbsent**, **TestReplayAfterRestartRefused**, TestRegistryAtomicWriteSurvivesCrash, TestRegistryExistingWinsOverLegacy, TestRegistryFreshStartMigratesLegacyConfig, TestRegistryMigrationDoesNotClobberLateFile, TestWriteAtomicDirSyncFailureKeepsMemoryEqualToDisk |
| ⑧ | 서명자 목록을 캐시(서버별로 처음 읽은 것을 계속 씀) | **TestRegistryNeverWrittenWithoutValidSignature**, **TestSignerRemovedTakesEffectImmediately**, TestFailedRequestsDoNotConsumeRequestID, TestRegisterRequiresValidSignature, TestSignerRemovalTakesEffectImmediately |
| ⑨ | 웹: 서버의 `change`와 브라우저가 다시 계산한 `change`의 비교(`resp.change !== change`) 제거 — **node 시험** | TestSignButtonDisabledUntilChangeMatches(A5. 165개 중 이것 하나만 실패) |
| ⑩ | orchd가 서명을 직접 판정하게 함: (a) orchd가 `internal/broker`를 import | **TestNoBrokerImportInOrchd** |
| ⑩ | (b) orchd가 서명을 broker에 넘기지 않음(`signature`를 인자에서 뺌) | **TestOrchdForwardsSignatureAndNeverJudgesIt**, TestAPIAcceptsSSHSignature, TestApplyForwardsSignatureAndMapsErrors |

내가 더한 보조 변이(같은 방법, 전부 잡힘):
| # | 망가뜨린 곳 | 잡은 시험 |
|---|---|---|
| 11 | orchd `apply`의 `change` 다시 계산 비교 제거 | **TestApplyForgedFieldsRefusedBeforeBroker**, TestApplyRecomputesChangeAndRefusesMismatch |
| 12 | broker가 허용 uid 검사를 건너뜀 | **TestForeignUIDCannotWrite**, TestBrokerForeignUidAndOversizeFlood, TestForeignUIDRefused, TestAllowedUIDServedWithRealPeerCred |
| 13 | 상태 폴더의 그룹·기타 권한 검사 제거 | **TestOrchdIdentityCannotReadOrWriteRegistryFile**, TestRegistryBadStateDirIsBroken |
| 14 | 등록부 파일의 정확한 0600 모드 검사 제거 | **TestOrchdIdentityCannotReadOrWriteRegistryFile**, TestRegistryBrokenIsFailClosedNotMigrated, TestUsedRequestsBrokenFileRefusesWrites |
| 15 | 정확한 대소문자의 유일한 키 검사(`exactObjectKeys`) 제거 | **TestWriteOpContractChangeRequiredIDKeptAndExactKeys**, TestArgsKeysAreExactCaseAndUnique |
| 16 | `set_flag`가 `ai_context` 외의 flag도 받음 | **TestRegistryNeverWrittenWithoutValidSignature**, TestSetFlagAIContextOnlyAndSigned, TestWriteOpsNeverChangeRegistryOnAnyFailure |

정직하게 적는다: ⑤·⑨와 ①~④·⑥~⑧ 중 일부는 **A6의 새 시험이 단독으로가 아니라 A1~A5의 기존 시험과 함께** 잡았다(위 표에서 굵지 않은 이름). A5 보고서가 이미 밝힌 대로 **웹의 "제안·목록 카운터를 합치는" 변이와 "적용 때 목록 무효화 제거" 변이는 어떤 시험도 잡지 못한다**(화면에서 도달할 수 없는 경로를 지키는 이중 방어이고, 6장에서 사용자에게 알린다).

## 3b. 단계별 검토에서 나온 수정 (A1~A5, 요약)
각 작업은 검토를 거쳤고 수정 라운드의 항목은 `.superpowers/sdd/20-PLAN-webconsole-W3a/progress.md`에 있다. 큰 것만: A2 마이그레이션 생성이 경쟁에 안전하지 않았음(배타적 `link()` 생성으로 수정), A3 `change` 인자가 선택이라 변조 감지가 모호했음(필수로 수정), A3 시험 한 곳만 "검증 전에 ID를 쓰지 않음"을 지켰음(표 시험 추가), A4 주체별 한도 없음과 쓰기 실패의 `outcome_unknown` 분류 없음(수정), A5 제안·목록 카운터 공유로 화면이 잠길 수 있었음(수정).

## 4. 설계서와 달라진 곳 (결정)

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| W3a-1 | 서명 메시지의 `change`는 **정규 JSON의 SHA-256**(설계서 7장의 화면 문구가 아님). SSH 네임스페이스 **`aigate-registry`** 신설 | 메시지 형식은 `v1` 접두로 구분되어 바꾸려면 `v2` |
| W3a-2 | **WebAuthn 서명 카운터를 도입하지 않았다**: 코드를 확인하니 `signing.Verify`에 카운터 처리가 없고 카운터 상태 자체가 없다(설계서 11장 결정 3의 "broker가 카운터를 따로 기록" 안은 전제가 없었다). 대신 **5분 신선도 + 요청 ID 1회성(1시간 보관, 파일에 영속)** | 복제된 패스키는 카운터로 걸러지지 않는다(5장) |
| W3a-3 | `change`는 broker와 orchd 모두에서 **필수**(없으면 `change_mismatch`). 두 방식을 허용하면 변조 감지가 모호해진다(A3 판정) | 없음 |
| W3a-4 | `repo_roots`는 **broker 설정**(`broker.json`)에서 읽는다(D6). 값이 없으면 모든 등록이 `path_not_allowed`. orchd 정책의 `repo_roots`와 **철자가 같아야** 한다 | 철자가 다르면 모든 등록이 거부됨(런북 12.0 (c)) |
| W3a-5 | 등록부 목록은 **`GET /api/v1/projects/registry`**(`GET /projects`는 이미 저장소별 작업 요약) | 없음 |
| W3a-6 | `set_flag`는 **`ai_context`만**. `auto_merge`·전역 스위치는 `not_supported`(서명 전에 거부되어 ID를 쓰지 않음) | W3d에서 완화 |
| W3a-7 | 마이그레이션된 프로젝트의 `kind`는 **`legacy`**(옛 설정에 정책 클래스가 없음). 화면에 표시만 하고 **작업 제출 때의 정책 클래스는 orchd가 정한다** | 나중에 정책 클래스와 이어 주는 작업 필요 |
| W3a-8 | 깨진 등록부로도 broker는 **시작하고** op마다 `registry_unavailable`로 답한다(옛 설정으로 되돌아가지 않음). `-check`는 **거부**한다 | 깨졌을 때 서비스는 "AI 꺼짐"으로 보임 |
| W3a-9 | 요청 ID는 서명·`change` 검증이 **모두 끝난 뒤에만** 소비하고, 시각은 **broker 시계**를 쓴다(서명자가 쓴 `issued` 아님). 경로·중복 검사에서 거부돼도 ID는 소비된다 | 없음 |
| W3a-10 | 쓰기 실패 중 적용됐을 수 있는 것(`timeout`·요청을 다 쓴 뒤 끊김·알 수 없는 코드·이상한 성공 응답)은 **`outcome_unknown` 504**로 알리고 재시도하지 않는다 | 사용자가 목록을 다시 읽어야 함 |
| W3a-11 | 종류(`kind`)는 **ASCII 정책 클래스 이름만**, 경로 글자는 **인쇄 가능한 ASCII와 한글 음절만** | 한글 클래스 이름(`버그수정` 등)이나 이상한 글자의 폴더는 등록할 수 없음 |
| W3a-12 | 주체별 한도 `propose` 60/h, `apply` 30/h(한도 계산은 broker에 닿기 전에 실패했을 때만 슬롯을 돌려줌) | 조정 한 곳 |
| W3a-13 | 등록부 목록은 **전부 아니면 전무**: 한 항목이라도 프로젝트로 보이지 않으면 목록 전체가 `broker_error`(부분 목록을 주지 않음) | 한 항목 때문에 화면이 비어 보임 |
| W3a-14 | 등록 경로 검사는 **등록하는 순간의 검사**이고 `repo_roots` 상위 경로의 심볼릭 링크는 풀지 않는다. 등록 뒤의 링크 바꿔치기는 **사용 시점에 거부**된다(미리보기 `allowed:false`/403, 시험 있음) | 상위 경로가 링크이면 거부가 아니라 통과할 수 있음(루트 자체는 root 소유 설정이 정함) |
| W3a-15 | orchd는 서명의 **모양만** 검사한다(알고리즘·서명자 ID의 값은 판정하지 않음). 판정은 broker의 몫(`TestOrchdForwardsSignatureAndNeverJudgesIt`) | 이상한 값이 broker까지 갔다가 거부됨 |
| W3a-16 | broker 상태 폴더 검사(소유 + 그룹·기타 비트 없음)는 등록부·사용 ID 파일에 적용된다. **`budget.json`은 이 폴더 검사와 별개로 읽는다**(브리프·보고의 "폴더 검사가 `budget.json`도 포함"은 코드와 맞지 않아 이 문서·런북은 코드대로 적었다) | 없음 |
| 로 | `-check`는 등록부가 **아직 없는 첫 점검에서는 경고하지 않는다**(아무것도 쓰지 않으므로). 경고는 마이그레이션 뒤에 나온다 | 없음 |

## 5. 실서버에서 확인하지 못한 것

**아래 전부는 이 저장소에서 한 번도 확인하지 않았다. 컷오버(런북 12장)에서 처음 확인한다.**
- **실제 패스키 서명을 broker가 검증하는지.** 시험은 시험용 ed25519 키(SSHSIG)와 ES256 모의 WebAuthn 인증기만 썼다. 화면의 시각 확인도 **대역 패스키**(`navigator.credentials.get`을 바꿔치기)로 했으므로 실제 인증기·실제 서명·서버 검증은 한 번도 지나가지 않았다. 실제 브라우저의 `clientDataJSON`·`authenticatorData` 모양이 W1의 승인 서명과 같은 경로로 검증되는지는 W1 컷오버에서 승인에 대해 확인했을 뿐 **등록 메시지에 대해서는 미확인**이다.
- **broker가 실제 `/etc/aigate/signers.json`을 읽는지.** 읽기 권한(root 소유 0644, 폴더 0755)은 확인했지만 broker 프로세스(`orch-broker` 계정, `ProtectSystem=strict`)가 실제로 열어 검증에 쓰는지는 미확인(사전 점검 (b)).
- **마이그레이션이 실제 `broker.json`으로 올바른 등록부를 만드는지**(`toy`·`sample-app`의 `id`·경로·`legacy`, `gitlab` 절이 있으면 그 매핑). 시험은 임시 설정으로만 했다.
- **`ProtectHome=yes`가 등록 경로 검사를 막을 것으로 추정**(5장 맨 위 알림). `/home/orch/repos`가 broker의 샌드박스에서 보이는지는 미확인이고 **유닛은 바꾸지 않았다.** 런북의 `TemporaryFileSystem=/home:ro` + `BindReadOnlyPaths=/home/orch/repos`는 **미검증 제안**이다. 등록부 경로 규칙이 실제 `/home/orch/repos`에서 동작하는지도 같은 이유로 미확인이다.
- **서명 카운터 부재의 한계**(W3a-2). 복제된 패스키가 5분 안에 쓰이면 카운터가 없어 걸러지지 않는다. 방어는 신선도와 요청 ID 1회성뿐이다.
- **키보드만, 스크린 리더로의 `#/projects` 화면 사용.** DOM 대역 시험(라벨, 네이티브 컨트롤, 텍스트 노드)만 했다. ego-browser 시각 확인은 A5에서 1280px과 390px로 했고, 390px 긴 경로(300자) 확인은 확인 화면의 위쪽 절반만 찍어 **서명 단추 근처는 보지 않았다.**
- **`repo_roots` 철자 일치.** 시험은 임시 폴더의 같은 철자로만 했다. 실제 `policy.yml`(`repo_roots: [/home/orch/repos]`)과 broker 설정이 글자 그대로 같은지는 컷오버에서 본다(사전 점검 (c)).
- **리눅스에서의 상태 파일 동작.** 소유자 확인(`syscall.Stat_t`)·`link()` 배타 생성·폴더 fsync는 macOS에서만 실행했고 리눅스는 `GOOS=linux go vet`으로 컴파일만 확인했다.
- **orchd가 실제 저장소로 계산하는 후보 목록**(실제 `/home/orch/repos` 아래의 폴더들, 실제 `policy.yml`의 클래스 이름). 시험 저장소로만 했다.
- **실부하·장시간에서의 한도와 `used-requests.json` 크기**(5000개 상한, 1시간): 시험의 주입 시계로만.
- 기존 W2b에서 이월된 미확인(실제 GitLab·MR, 한도·예산 초과 실동작 등)은 18번 문서 5·7장 그대로이고 이번 변경은 그 경로를 건드리지 않았다(회귀 시험은 통과: `TestCreateMRUsesRegistryGitLabProject`, `TestRefineUsesRegistryAllowList`).

## 6. 사용자 결정 목록

컷오버 때 사용자가 정하거나 해야 하는 것(런북 12.0):
1. **`orch-broker.service`의 `ProtectHome=yes`를 어떻게 할지**(등록이 막힐 것으로 추정, 5장). 선택지: 유닛을 그대로 두고 이번에는 이미 등록된 두 프로젝트의 AI 전송 켜기/끄기만 시험, `TemporaryFileSystem=/home:ro` + `BindReadOnlyPaths=/home/orch/repos` 드롭인(미검증), `ProtectHome=read-only`(더 넓게 열림, `SupplementaryGroups=orch`와의 조합 주의). **임의로 바꾸지 않았다.**
2. **컷오버에서 실제로 서명해 프로젝트를 등록할지**(실제 broker 등록부가 바뀜), 아니면 `toy`·`sample-app` 중 하나의 AI 전송을 껐다 켜 볼지(런북 3-3·3-5). 어느 쪽이든 실제 패스키 서명이 broker에서 처음 검증된다.
3. 등록해 본다면 **어느 후보 저장소를 어떤 `kind`(정책 클래스)로** 등록할지, AI 전송은 켜지 않고 시작하는 것을 권한다(확인 화면의 기본값도 꺼짐).
4. 마이그레이션 뒤 `broker.json`의 옛 `context_repos`·`gitlab.projects`를 **언제 지울지**(롤백을 쉽게 하려면 지우지 않는다. 지워도 동작에는 영향 없음, `-check` 경고만 남는다).
5. **WebAuthn 카운터 없이 가는 것**(신선도 + 요청 ID 1회성)을 받아들일지, 카운터 상태를 따로 만드는 작업을 할지(W3a-2).
6. 마이그레이션된 프로젝트의 `kind: legacy`를 이후에 **정책 클래스와 어떻게 이을지**(W3c의 `registered` 클래스와 함께 정할지).
7. 컷오버가 안정되면 백업(`/home/orch/orchd/backup-w3a`, `*.prev-w3a`)을 **언제 지울지**(`backup-w3a`에 `openai.key` 사본이 들어 있으므로 `shred`로 지운다).
8. 다음 단계(W3b 토큰 등)로 갈지.

## 7. 컷오버 기록
아직 없음(컷오버 전). 컷오버를 하면 이 장에 서버에서 본 것만 적는다(W2b 18번 문서 7장과 같은 방식).
