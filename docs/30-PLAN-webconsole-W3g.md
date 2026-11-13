# W3g 구현 계획: 삭제를 서명 한 번으로 끝내기 (배포 키 함께 떼기, 원격 삭제와 복제본·등록 함께 지우기)

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다. **상태: 초안 v0.1(2026-11-13), 구현 승인 전. 승인 전에는 구현하지 않는다.**

**배경(W3f 서버 시험, 2026-11-13, `docs/29` §9):** (1) `remove_project`로 복제본·등록을 지울 때 배포 키를 같이 떼지 않아 GitHub 저장소(`sulee_portfolio`)에 `orchd sulee-portfolio` 키가 남았다. 키는 따로 "배포 키만 떼기"를 먼저 해야 하는데 순서를 놓치기 쉽다. (2) 원격 저장소를 지우면서 "복제본도 함께"를 골라도 등록은 남아, 등록을 지우는 두 번째 서명이 필요했다. 사용자 결정(2026-11-13): 둘 다 서명 한 번에 끝나도록 한다. **되돌릴 수 없는 일을 자동으로 하지는 않는다**: 복제본을 지우는 것은 지금처럼 사용자가 고를 때만, 키 떼기는 서명에 명시된 선택으로만 한다.

## 전역 제약
- **`docs/28`(W3f)과 `docs/26`·`docs/24`의 전역 제약을 그대로 적용한다**(서명 순서·요청 ID 규칙, 비밀 비유출, 고정 문장, 삭제는 서명 없이 일어나지 않음, 허용 목록의 DELETE 행은 W3f가 정한 것뿐, 웹 규칙, 검증 명령). 아래는 W3g에서 달라지는 것만 쓴다.
- **새 DELETE 호출 행은 만들지 않는다.** W3g는 W3f의 기존 호출(키 목록 읽기·키 삭제·저장소 삭제)의 **순서와 서명 범위**만 바꾼다.
- **등록부 버전은 올리지 않는다(v4 그대로).** 서명 항목의 필드 하나가 늘어난다(`remove_project.detach_key`). 서버(W3f)에서 W3g로 올리는 순서는 W3f와 같다(orchd 정지 → broker → orchd). 새 orchd와 옛 broker가 섞이면 변경 해시가 달라 서명이 거부된다(안전하게 실패).
- **실패는 닫힌 쪽으로:** 키 떼기가 실패하면 등록과 폴더를 지우지 않는다. 결과가 불확실하면 자동 재시도하지 않는다.

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **G-D1 `remove_project`에 서명 필드 `detach_key`(bool)를 더한다.** 서명 항목의 필드는 `scope`(`local`), `legacy_ack`, `detach_key` 셋이다(모두 필수, 정확한 키 집합). 서명 정의·변경 해시 벡터(Go 고정, 웹 일치 시험)·`REGISTRY_MESSAGE_RE`·웹 상수 일치 시험을 함께 바꾼다. W3f의 `remove_project` 변경 해시는 달라진다(서버에 보류 중인 서명은 없다).
- **G-D2 키 떼기 동작:** `detach_key=true`인 `remove_project`는 broker가 (1) shape 단계에서 확인: 프로젝트에 제공자와 원격이 있고 `remote_deleted_at`이 없으며 orchd가 보낸 공개 키 줄(`deploy_key`)이 있음(없으면 `bad_request`, 서명 전), (2) 서명 검증·요청 ID 소모, (3) 키를 찾음: **GitHub: 제목 `orchd <id>`와 키 본문이 둘 다 맞는 정확히 하나**(W3f 규칙). **GitLab: 키 본문이 맞는 정확히 하나(제목은 무시: 공용 키는 첫 프로젝트의 제목 `orchd <첫 id>`로 남고 다른 프로젝트에는 연결만 되므로, 본문이 우리 키의 신원이다. 이 호출은 그 프로젝트에서의 연결만 끊는다).** 둘 이상이면 `remove_refused`로 멈춤. 일치가 0개일 때: **목록에 우리 제목 또는 우리 본문을 가진 키가 하나라도 있으면 `remove_refused`로 멈춘다**("찾지 못함"을 "이미 없음"으로 오해하지 않는다: 키가 남은 채 등록이 지워지는 것을 막는다), 둘 다 없을 때만 "이미 없음"으로 보고 계속. `remove_deploy_key`(W3f)도 같은 식별 규칙을 쓴다(GitLab의 공용 키를 쓰는 두 번째 이후 프로젝트에서 키를 뗄 수 있게 한다), (4) `DELETE`, (5) 성공이면 등록 행을 지움. **키 떼기가 거부·불확실이면 등록 행을 지우지 않는다**(`delete_partial`이면 불확실: 자동 재시도 없음, 사용자는 상태를 읽고 다시 서명). `detach_key=false`이면 W3f와 같다(등록만 지움: 확인 화면에 "배포 키가 원격에 남습니다"). GitLab은 그 프로젝트에서만 키를 비활성화한다(W3f 규칙).
- **G-D3 `detach_key`가 의미 없는 경우:** 원격이 없거나(`remote == nil`), 원격이 이미 지워졌거나(`remote_deleted_at`), 제공자가 없는 레거시 행: `detach_key`는 `false`여야 하고 `true`이면 `bad_request`(서명 전). 웹은 이 경우 선택지를 보이지 않는다.
- **G-D4 `delete_remote_repo` + `also_local=true`는 등록까지 같은 서명으로 지운다.** broker가 `DELETE` 성공 뒤 등록 행을 **지운다**(`remote: 없음` 상태로 남기지 않음). `also_local=false`이면 W3f 그대로(행은 `remote: 없음`+`remote_deleted_at`로 남고 `Provider`가 유지된다). orchd는 broker가 성공을 돌려준 뒤에만 복제본·키 폴더를 지운다(W3f의 `removeLocal`과 같은 안전 규칙). **로컬 삭제가 실패하면 등록 없는 폴더가 남는다**(W3f의 단독 `remove_project`와 같은 실패 모양, 같은 복구: 경로를 다시 등록한 뒤 서명된 `remove_project`). 이 실패 모양은 알려진 위험이다.
- **G-D5 orchd:** `remove_project`의 propose는 `detach_key`가 켜져 있으면 프로젝트의 키 줄(`keys/<id>/id_ed25519.pub` 또는 GitLab 공용 `.pub`)을 읽어 broker에 줄 수 있는지 서명 전에 확인한다(없으면 `key_unavailable_for_removal`: 키를 남기려면 사용자가 `detach_key`를 끄고 명시적으로 진행). 키 줄은 요청 데이터에서 받지 않는다. 미리보기 응답에 `detach_key_available`(bool)과 키 제목·지문(공개 정보)을 더하고 W3f의 `remote_key_may_remain`은 `detach_key_available`과 함께 유지한다. `delete_remote_repo`+`also_local`의 apply 순서: broker(원격 삭제+등록 지움) → 로컬 삭제. 한도: `remove_project`(키 떼기 포함)는 키 제거 한도(시간당 10)와 로컬 정리 한도(시간당 10)를 둘 다 따른다.
- **G-D6 오류 코드:** 새 코드는 만들지 않는다(재사용: `bad_request`, `remove_refused`, `delete_partial`, `key_unavailable_for_removal`, `key_not_found`는 "이미 없음"으로 흡수). 필요하면 고정 문장만 손본다(예: 키 떼기가 실패해 등록을 남겼다는 안내).
- **G-D7 웹:** "복제본과 등록 지우기" 화면에 체크 "배포 키도 함께 떼기"(원격이 연결되어 있고 키 줄을 쓸 수 있을 때만 보이고, **기본 체크**). 확인 화면에 어느 저장소(이름·주소)의 어떤 키(제목, 지문)를 떼는지와, 끄면 "키가 원격에 남습니다. 직접 지우세요"를 보인다. "원격 저장소까지 지우기"의 "복제본도 함께" 선택은 W3f의 경고와 확인 체크를 유지하되 **결과 화면의 "등록 지우기" 후속 서명 안내를 `also_local=true` 경로에서는 보이지 않는다**(등록까지 지워졌으므로). `also_local=false`인 경우와 W3f 때 만들어진 `remote_deleted_at` 행(이미 서버에 있을 수 있음)을 위해 후속 서명 화면은 남긴다. 변경 해시는 브라우저가 자기 값(`detach_key` 포함)으로 다시 계산해 비교하고, 서버 문장은 정확히 일치해야 한다.

## 검토 초점 (개별 시험이 놓치기 쉬운 것, 가능성 순)
1. **키 떼기와 등록 해제의 순서와 실패 모양:** 키 떼기가 거부·불확실이면 등록 행이 그대로인지, 키는 떼였는데 등록 해제가 실패한 경우(등록부 쓰기 실패)의 상태, 같은 서명 재사용 불가, `key_not_found`를 성공으로 보는 것이 잘못된 키 삭제로 이어지지 않는지(여러 개·본문 불일치).
2. **서명 범위:** `detach_key`가 서명에 들어가고 브라우저·orchd·broker가 같은 값으로 다시 계산하는지, `detach_key=true`인데 원격이 없는 행, 레거시, `remote_deleted_at`이 있는 행의 거부(서명 전), 다른 action 서명으로 키가 떼이지 않는지(240쌍 시험 확장).
3. **`also_local`로 등록이 지워지는 새 경로:** 원격 삭제 성공 직후 broker가 행을 지우는 것이 W3f의 안전 규칙(출처 `created`, 번호·이름 확인, `remote_deleted_at` 기록 의미)을 약하게 하지 않는지, 지운 뒤 등록 없는 폴더·키 폴더의 복구 절차가 런북에 있는지, `also_local=false` 경로가 W3f 그대로인지.
4. **혼합 버전:** W3f orchd + W3g broker, W3g orchd + W3f broker(변경 해시 불일치로 서명 거부 = 안전, 화면에 어떻게 보이는가), 롤백 순서.
5. **웹 오도 위험:** 키를 남기는 선택이 눈에 띄는지, `detach_key` 기본 체크가 원격이 없는 행에 잘못 보이지 않는지, 후속 서명 안내가 `also_local=true`에서 사라진 뒤에도 W3f 상태 행에 보이는지, 결과 화면의 사실이 서버 상태와 일치하는지.
6. **배포 현실:** 서버는 W3f(main 263d0e1)이고 등록부 v4 그대로, 설정·유닛 변경 없음, 키 줄은 orchd가 `ssh/keys/<id>/`에서 읽음(폴더 삭제 전에 읽어야 함: 순서).

---

### Task 1 (G1): 서명 정의 + broker
**파일:** 수정 `internal/registrymsg/*`(필드·검증), `internal/approval/signing/registry_message.go`, `internal/broker/delete_ops.go`(`remove_project`의 키 떼기+등록 해제, `delete_remote_repo`+`also_local`의 등록 해제), 시험·벡터.
- [ ] **시험(이름 그대로):** `TestRemoveProjectDetachKeyFieldInSchema`, `TestRemoveProjectDetachKeyShapeRules`(원격 없음·레거시·`remote_deleted_at`·키 줄 없음 → 서명 전 거부, ID 미소모), `TestRemoveProjectDetachesThenUnregisters`(요청 순서: 목록 GET → 키 DELETE → 등록 지움), `TestDetachFailureKeepsRegistration`(거부·불확실 모두 행 유지), `TestDetachKeyNotFoundContinues`, `TestDetachMultipleOrMismatchedKeyNeverDeleted`, `TestRemoveProjectWithoutDetachUnchanged`(W3f 동작), `TestDeleteRemoteAlsoLocalRemovesRow`, `TestDeleteRemoteWithoutAlsoLocalKeepsRowAndProvider`, `TestAlsoLocalRegistryWriteFailureIsPartial`, `TestChangeHashVectorsForRemoveProjectW3g`(Go 고정 벡터), `TestCrossActionSignaturesStillRefusedW3g`(모든 쌍), `TestNoNewDeleteRowsInAllowlist`(W3f 집합 그대로), `TestCanaryTokenNeverInAnyOutputW3g`. 기존 시험의 의미 변경 금지(바꾸면 이유와 함께 보고).
- [ ] **커밋:** `feat(broker): detach the deploy key and unregister in one signed remove_project; also_local removes the row`.

### Task 2 (G2): orchd
**파일:** 수정 `internal/orch/api/delete_project.go`(propose/apply, 미리보기 필드, `also_local` 순서), 시험.
- [ ] **시험:** `TestRemoveProposeReadsKeyLineOnlyFromKeyFolder`, `TestDetachKeyUnavailableRefusedBeforeSignature`, `TestKeyLineReadBeforeLocalRemoval`(순서), `TestApplyForwardsDetachKeyAndKeyLine`, `TestPreviewDetachKeyAvailableAndFingerprint`, `TestAlsoLocalRemovesLocalAfterBrokerRowRemoval`, `TestAlsoLocalFailureLeavesUnregisteredFolderWithExactItems`, `TestLimitsDetachKey`, `TestLedgerNoSecretsW3g`, `TestOrchdWorksWithBrokerDownW3g`, 혼합 버전 시험(옛 broker 응답 `unknown_op` → 불가 안내).
- [ ] **커밋:** `feat(api): detach-key option for remove_project and one-signature also_local`.

### Task 3 (G3): 웹
**파일:** 수정 `views/delete.js`, `providers.js`(상수·문장), `webauthn.js`(필요 시), 웹 시험·벡터 소비.
- [ ] **시험:** `TestDetachKeyCheckboxShownOnlyWhenAvailableDefaultChecked`, `TestConfirmationNamesRepoAndKeyTitleAndFingerprint`, `TestUncheckedShowsKeyRemainsWarning`, `TestSignGateRecomputesChangeWithDetachKey`, `TestAlsoLocalResultHasNoFollowUpSignature`, `TestFollowUpStillOfferedForRemoteDeletedRows`, `TestChangeVectorsMatchGoW3g`, `TestNoStorageNoNavigateTrap`, 오류 문구 시험 확장. 시각 확인(1280·390px, 실제 `app.js` 경로, 패스키는 대역, 본 것만).
- [ ] **커밋:** `feat(web): detach the deploy key with the removal, one signature for remote deletion`.

### Task 4 (G4): 적대 시험, 결과(`docs/31`), 런북(`docs/10` 17장)
- [ ] 적대 시험과 변이 표(서명 범위에서 `detach_key` 제외, 키 떼기 실패 때 등록 지움, 본문 비교 제거, `also_local`에서 등록을 안 지움/`also_local=false`에서 지움, 순서 바꿈(키 폴더를 먼저 지우고 키 줄 읽기), 웹 기본 체크 제거 등), 결과 문서(서버에서 확인하지 못한 것: 실제 키 떼기 실행 등), 런북(W3f에서 W3g로 올리는 순서, 시험 절차: 새 시험 저장소로 키 떼기+복제본·등록 정리, 원격 삭제+복제본+등록 한 번에, `detach_key` 끄기 확인, 롤백(W3f 바이너리 복원, 등록부는 v4 그대로)). W3f 런북 16장에 "키를 남기지 않으려면 키 떼기를 먼저" 함정을 추가한다.
- [ ] 전체 `go test ./... -race`, node, `TestConstantsAgreeWithWeb`; 커밋 `test: adversarial suite for W3g`, `docs: W3g results and cutover runbook`.

---

## 자체 검토 기록
- **범위:** 사용자의 두 요청(키 함께 떼기, 원격 삭제와 복제본·등록을 서명 한 번으로)을 G1~G4가 덮는다. **의도적으로 뺀 것:** 복제본을 기본으로 지우는 것(되돌릴 수 없는 일을 자동으로 하지 않음), 등록부 버전 변경, 새 DELETE 호출, GitLab 동작 변경, 이미 있는 `remote_deleted_at` 행의 마이그레이션(후속 서명 화면 유지).
- **위험:** 서명 범위가 넓어진다(원격 삭제 서명이 등록 해제까지, 제거 서명이 키 떼기까지). 서명 확인 화면이 그 범위를 평문으로 보여 주고 브라우저가 해시를 다시 계산하므로 서명된 것과 보인 것이 같다. 키 떼기 실패 때 등록을 남기는 것은 닫힌 쪽 선택이다.
- **사용자 확인이 필요한 값(승인 시):** ① `detach_key` 기본 체크(권장), ② 키를 뗄 수 없을 때 등록과 폴더를 지우지 않고 멈춤(권장), ③ `also_local=true`는 등록까지 지움(로컬 삭제 실패 시 등록 없는 폴더가 남는 위험을 받아들임), ④ 복제본을 지우는 것은 계속 사용자가 고를 때만.
