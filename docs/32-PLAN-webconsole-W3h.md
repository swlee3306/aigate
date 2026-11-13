# W3h 구현 계획: 배포 키 떼기를 등록 때 서명된 키 해시에 묶기 (등록부 v5), 거부 진단 로그, 멈추는 테스트 수정

상위: `31-RESULTS-webconsole-W3g.md`(8.1: GitLab 키 식별 잔여 위험, 열린 결정), `30-PLAN-webconsole-W3g.md`. 사용자 승인: 2026-11-14("(a)로 구현 시작해줘").

## 배경
W3g는 GitLab 배포 키를 **본문**으로만 찾는다(공용 키는 첫 프로젝트의 제목으로 남기 때문). 그런데 어느 본문을 찾을지(`deploy_key` 키 줄)는 서명되지 않은 orchd 입력이라, orchd가 손상되면 서명한 프로젝트 안의 **다른 키 하나**가 떼어질 수 있다(피해 범위: 그 프로젝트의 키 1개, 사용자 서명 필요, 시험 `TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g`가 현재 동작을 고정). 프로젝트를 만들거나 연결할 때는 `deploy_key_sha256`이 **이미 서명되고 broker가 검증한다**(`create_project.go:185~188`, `link_project.go`). 이 값을 등록부 행에 저장해 두면, 떼기 때 서버가 이미 가진 서명된 값과 대조할 수 있다.

## 전역 제약
- 서명 필드·변경 해시·`REGISTRY_MESSAGE_RE`·웹 벡터는 **바꾸지 않는다**(W3g 상태 그대로). 새 연산·새 허용 목록 행·새 오류 코드 없음.
- 등록부 파일만 v4 → v5. 마이그레이션은 원자적이고 백업(`registry.json.v4.bak`)은 한 번만 쓰고 덮어쓰지 않는다(v3→v4와 같은 규칙).
- 비밀 취급은 W3g와 같다: 키 줄·해시는 공개 정보지만 로그·원장에 본문을 싣지 않는다(해시 앞 8자리까지만 허용).
- 게이트: gofmt, go vet(+linux/darwin), `go test ./internal/... ./cmd/... -count=1 -race` 완주, `node --test "internal/orch/web/test/*.test.js"`, `TestConstantsAgreeWithWeb` PASS(SKIP 없음), 적대 시험 변이는 복사본에서만.

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **H-D1 등록부 v5.** `RemoteLink`에 선택 필드 `key_sha256`(소문자 16진 64자)을 더한다. 등록부 검증: 값이 있으면 `Remote`와 `Provider`가 모두 있어야 하고 64자 소문자 16진이어야 한다(그 외 거부). v4 파일을 열면 행은 그대로 두고 version만 5로 올려 다시 쓴다(해시 없음). **옛 broker(W3g)는 v5를 거부한다**(롤백은 `.v4.bak`에서 등록부 복원).
- **H-D2 저장 지점 3곳.** `create_project`(`create_project.go:329`), `link_project`(`link_project.go:334`, 새 행 / `:348`, 기존 행에 연결하는 `attach_remote`)에서 원격을 등록할 때 이미 검증한 서명된 `deploy_key_sha256`을 `key_sha256`으로 같이 저장한다(소문자 16진 SHA-256, 입력한 `deploy_key` 줄 전체의 해시).
- **H-D3 대조(떼기 때).** `remove_project`(`detach_key=true`)와 `remove_deploy_key`의 **모양 단계(서명 검증과 요청 ID 소모 전)**에서, 행에 `key_sha256`이 있으면 orchd가 준 `deploy_key` 줄의 SHA-256이 같아야 한다(상수 시간 비교). 다르면 `deploy_key_mismatch`(ID 소모 없음). 행에 해시가 **없으면**: GitHub는 W3g 규칙 그대로(제목+본문), **GitLab은 `deploy_key_mismatch`로 거부**(닫힌 실패: 사용자는 떼기를 끄고 지운 뒤 GitLab 화면에서 키를 직접 지운다). 대조를 통과한 뒤의 `classifyKeys`(제목·본문 규칙)는 그대로 둔다.
- **H-D4 키 교체.** GitLab 공용 `.pub`을 나중에 바꾸면 그 전에 만든 행은 해시가 달라져 떼기가 거부된다(안전한 쪽의 실패). 런북에 적는다.
- **H-D5 orchd.** `projects` 읽기의 `remote`에 `key_sha256`이 오면 받는다(모르는 필드를 막지 않는 읽기이므로 W3g orchd와도 안전). 동작 변경 없음. 웹은 바꾸지 않는다(`deploy_key_mismatch`는 키 떼기 거부 문구로 이미 있다: 없으면 추가).
- **H-D6 거부 진단 로그.** orchd가 HTTP 403으로 거부할 때(서명·세션·소켓 사용자 거부) 사유 코드와 메서드·경로를 고정 문구 한 줄로 로그에 남긴다(`orchd: refused 403 <code> <METHOD> <path>`). 쿼리·본문·헤더·서명·토큰은 남기지 않는다. 원장은 바꾸지 않는다.
- **H-D7 멈추는 시험.** `TestConcurrentDeleteOfOneProjectSendsOneDelete`의 기다림에 시간 제한을 둔다(운영 코드 변경 없음).

## 검토 초점 (개별 시험이 놓치기 쉬운 것, 가능성 순)
1. 해시가 있는 GitHub·GitLab 행에서 orchd가 **다른 키 줄**을 보내면(본문이 달라도, 같은 프로젝트의 다른 키여도) 서명 전에 거부되고 DELETE가 0번인가.
2. 해시가 없는 GitLab 행(옛 행·마이그레이션된 행)은 거부되고 GitHub 행은 W3g처럼 동작하는가.
3. 마이그레이션 중단·재시도·백업 덮어쓰기 금지·옛 broker의 v5 거부.
4. 3곳(create/link/attach) 모두 해시를 저장하는가(하나라도 빠지면 그 경로의 행이 GitLab에서 영영 떼기 거부).
5. 해시가 `remote: null`이 되는 경로(원격 삭제 뒤 `remote_deleted_at` 행, `clear_orphan`)에서 값이 남지 않는가.
6. 403 진단 로그에 비밀·본문이 없는가, 모든 403 경로가 코드를 갖는가.

## Task 1 (H1): broker (등록부 v5, 저장, 대조)
**파일:** 수정 `internal/broker/registry.go`(+필요 시 `registry_v5.go`), `create_project.go`, `link_project.go`, `delete_ops.go`, 시험.
- 시험(먼저 실패하는 것부터): `TestRegistryV4MigratesToV5Atomically`, `TestV4ToV5WritesBackupOnceNeverOverwritten`, `TestOldBrokerRefusesV5`, `TestRegistryV5Rules`(해시 형식·Remote/Provider 필요·없는 행 허용), `TestCreateLinkAttachStoreTheSignedKeyHash`(3곳), `TestDetachRefusedWhenKeyLineHashDiffers`, `TestRemoveDeployKeyRefusedWhenKeyLineHashDiffers`, `TestGitLabRowWithoutHashRefusesDetach`, `TestGitHubRowWithoutHashKeepsW3gRule`, `TestRemoteClearedDropsHash`, `TestUnsignedKeyLineWideningNowRefusedW3h`(W3g의 `TestUnsignedKeyLineNeverWidensBeyondTheSignedProjectW3g`가 고정한 동작을 거부로 바꾼다: 기존 시험을 새 기대로 고치고 이유를 보고서에 적는다).
- 커밋 `feat(broker): bind key detach to the signed deploy key hash (registry v5)`.

## Task 2 (H2): orchd와 웹, 진단 로그, 멈추는 시험
**파일:** 수정 `internal/orch/broker/registry.go`(RemoteLink 필드), `internal/orch/api/*`(403 로그), `internal/orch/web/static/views/delete.js`(문구가 없을 때만), `internal/orch/web/dev_test.go`(필요 시), 시험.
- 시험: `TestProjectsViewAcceptsKeySHA256`, `TestRefused403LogsReasonAndPathOnly`(코드별: bad_signature, stale_signature, unknown_signer, csrf_failed, forbidden_uid), `TestRefusalLogHasNoSecrets`, 웹 `TestDeployKeyMismatchTextForDetach`(없으면), 멈추는 시험의 시간 제한(H-D7).
- 커밋 `feat(orchd): log the reason of 403 refusals; accept key_sha256; bound a hanging test`.

## Task 3 (H3): 적대 시험, 결과(`docs/33`), 런북(`docs/10` 18장)
- 적대 시험과 변이 표(저장 3곳 중 하나 빼기, 대조 빼기, 상수 시간 비교를 `==`로, 해시 없는 GitLab 허용, 해시 없는 GitHub 거부, 마이그레이션 백업 덮어쓰기, `remote: null`에서 해시 남기기 등).
- 결과 문서 `docs/33-RESULTS-webconsole-W3h.md`(서버에서 본 것·못 본 것 구분), 런북 `docs/10` 18장(W3g→W3h 순서: orchd 정지 → 백업 → broker(v5 마이그레이션, `.v4.bak`) → orchd, 시험 절차, 롤백: broker 바이너리 + 등록부를 `.v4.bak`에서 복원, 키 교체 때의 영향), `docs/19` 상태 줄.
- 게이트 전체 실행, 커밋 `test: adversarial suite for W3h`, `docs: W3h results and cutover runbook`.

## 자체 검토 기록
- 서명 필드·벡터·허용 목록·오류 코드를 건드리지 않으므로 W3g 웹·orchd와 섞여도 안전하다(W3h broker + W3g orchd: orchd는 `deploy_key`를 전과 같이 보내고 broker가 대조한다; W3g broker는 v5 등록부를 열지 못하므로 롤백 때 등록부 복원이 필요하다).
- 현재 서버에는 제공자에 연결된 행이 없다(`toy`·`sample-app`는 옛 행). 그래서 마이그레이션이 해시를 채울 필요가 없고, 새로 만드는 행부터 해시를 가진다.
