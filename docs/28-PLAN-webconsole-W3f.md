# W3f 구현 계획: 프로젝트 삭제 (서버의 복제본 지우기, 원격 저장소 지우기, 배포 키 떼기)

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다. **상태: 초안 v0.1(2026-11-12), 사용자 승인 전. 승인 전에는 구현하지 않는다.**

**배경(사용자 요청, 2026-11-12):** "클론이든 원격이든 지우는 기능을 만들어줬으면 좋겠다." W3a~W3e의 **가장 큰 설계 원칙은 "삭제 코드가 아예 없다"였다**(관리자 토큰이 새도 삭제는 못 하게). 이 계획은 그 원칙을 **의도적으로 부분 해제**한다: 삭제를 넣되 서명·허용 목록·범위 제한·출처 확인으로 가둔다. 이 변경은 토큰의 최악의 피해 범위를 넓힌다(저장소 삭제 가능). 사용자가 삭제용 토큰 분리를 하지 않기로 했으므로(F-D1) 그 위험은 코드의 범위 제한(출처·번호·서명)으로만 줄인다.

**목표:** 웹에서 서명 한 번으로 (a) **서버의 복제본과 등록·키를 지우고**(로컬 정리: 되돌릴 수 있음, 원격 저장소는 그대로), (b) **이 도구가 만든 원격 저장소를 지우고**(GitHub·GitLab, 되돌릴 수 없음), (c) **연결해 둔 기존 저장소에서 이 도구가 붙인 배포 키만 떼며**(저장소는 그대로), (d) 고아 기록을 지운다.

## 전역 제약
- **`docs/24`·`docs/26`의 전역 제약을 그대로 적용한다**(비밀 비유출, 서명 순서와 요청 ID 규칙, 고정 문장, 웹 규칙, 검증 명령 등). 아래는 삭제에서 새로 생기는 것만 쓴다.
- **삭제는 모두 서명이 필요하고 확인 화면에서 사람이 대상을 눈으로 확인한다.** 브라우저는 서명할 `change`를 자기 값으로 다시 계산한다. 원격 삭제는 **저장소 이름을 직접 입력**해야 서명 단추가 켜진다(GitHub의 삭제 확인과 같은 방식: 복사 붙여넣기 방지를 위해 입력칸 붙여넣기는 막지 않되 대소문자까지 같아야 함).
- **레거시 프로젝트(`toy`, `sample-app` 등 등록부 `kind: legacy`)도 서버의 복제본·등록부 항목은 지울 수 있다(사용자 결정 2026-11-12). 단 한 번 더 묻는다:** 일반 삭제의 확인 화면과 서명 **앞에 별도의 두 번째 확인 화면**("이것은 처음부터 있던 프로젝트입니다. 서버의 복제본과 등록이 지워지고, 이 서버의 작업 기록은 남지만 이 저장소로는 더 작업을 낼 수 없습니다" + 원격에 안 올라간 커밋·변경 개수 + 프로젝트 이름을 한 번 더 직접 입력)을 거친다. 이 확인은 서명되는 필드 `legacy_ack`(`true`)로 들어가 서버가 서명 없이는 레거시를 지우지 못한다(`legacy_ack_required`). **레거시 프로젝트의 원격 저장소(예: `sample-app`의 사내 GitLab `example-group/sample-collector`)는 이 기능으로 지우지 못한다**(출처가 `legacy`/`unknown`: `origin_unknown`): 사용자가 요청한 것은 서버 쪽 삭제이고, 이 도구가 만들지 않은 원격 저장소를 지울 수 있게 하면 회사 저장소를 지울 위험이 생기기 때문이다. 정적 `--git-remote-for`가 있는 프로젝트(`sample-app`)를 지우면 orchd 유닛의 그 설정은 남으므로 런북에 "설정을 사람이 정리" 단계를 적는다(없는 폴더를 가리키는 정적 원격은 해롭지 않지만 지저분하다).
- **실행 중·대기 중·게시 대기·승인 대기 작업이 있는 프로젝트는 지우지 못한다**(`project_busy`, orchd가 DB로 확인한 뒤 서명 전에 거부).
- **원격 저장소 삭제는 이 도구가 만든 저장소만 대상**이다(등록부의 `origin: created`). 연결(`linked`)한 기존 저장소는 **절대 지우지 못하고** 이 도구가 붙인 배포 키만 뗄 수 있다. 출처를 모르는 프로젝트(이 기능 이전에 만든 것)는 원격 삭제를 거부한다(`origin_unknown`): 필요하면 사용자가 GitHub·GitLab 화면에서 지운다.
- **대상 확인 이중화:** 원격 삭제 직전에 broker가 저장소를 읽어 **등록부에 적힌 저장소 번호(`remote.id`)와 같은지**, 이름이 같은지 확인한다(저장소가 이름이 바뀌었거나 소유가 이전되었거나 같은 이름으로 다시 만들어진 경우 거부: `remote_changed`).
- **삭제 순서와 부분 실패(되돌릴 수 없는 일부터 하지 않는다):** 원격 삭제를 요청받아도 **먼저 로컬 정리를 하지 않는다.** 순서는 (1) 서명·모양 검증과 orchd의 `project_busy` 확인, (2) broker가 원격 저장소 삭제(성공하면 등록부에 `remote: null`, `remote_deleted_at` 기록), (3) orchd가 복제본·키 폴더 삭제, (4) broker가 등록부에서 프로젝트 제거. (2) 성공 뒤 (3)·(4)가 실패하면 프로젝트는 "원격 없음" 상태로 남고 **같은 로컬 정리를 다시 서명 없이가 아니라 새 서명으로** 재시도한다(원격 삭제는 이미 끝났으니 되돌릴 일이 없다). 어느 단계든 오류가 "결과 불확실"이면 자동 재시도하지 않고 상태를 다시 읽게 한다.
- **로컬 정리의 파일 시스템 안전(orchd):** 대상은 `repo_roots` 바로 아래 한 단계의 폴더이고, 이름은 등록부의 프로젝트 id와 같고, `Lstat`으로 심볼릭 링크가 아니며, `.git`이 있는 디렉터리이며, **`RemoveAll`은 `SameFile`로 같은 폴더임을 다시 확인한 직후에만** 한다(교체 경쟁 방지). 키 폴더 `ssh/keys/<id>/`도 같은 규칙. 지우기 전에 이동 후 삭제(`<root>/.deleting-<무작위>`로 rename 뒤 삭제, 시작 때 남은 것 청소)로 중간에 죽어도 반쯤 지워진 폴더가 프로젝트 이름으로 남지 않게 한다.
- **로컬 정리 전에 확인할 것(데이터 손실 방지):** 복제본에 **원격에 올라가지 않은 커밋이나 수정 안 된 파일**이 있으면(`git status`·`git log @{u}..`) 확인 화면에 보여 주고(개수만, 내용 아님) 사용자가 그래도 지우겠다고 체크해야 한다. 원격이 이미 지워졌으면 그 복제본이 유일한 사본일 수 있으니 같은 경고를 강하게 한다.

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **F-D1 삭제 토큰(사용자 결정 2026-11-12, 초안의 "삭제용 토큰 분리"를 철회):** 사용자가 이미 **일반 토큰에 삭제 권한(GitHub `delete_repo` 등)까지 모두 주어 두었으므로 별도 삭제 토큰을 만들지 않는다.** 삭제 호출도 제공자의 기존 `token_ref` 토큰을 쓴다(제공자 스키마·`set_provider`·토큰 화면은 바뀌지 않는다). **그 결과 이 토큰이 유출되면 토큰 소유 계정의 저장소를 지울 수 있다는 점은 사용자가 감수한 위험이다**(코드는 그 위험을 키우지 않도록 삭제 호출을 정확히 F-D4의 행들로만, `origin: created`인 저장소에만, 서명·번호 확인 뒤에만 한다). 만료일을 짧게 두고 시험이 끝나면 권한을 줄이는 것을 권한다.
- **F-D2 등록부 버전 4:** 프로젝트에 `origin`(`created`|`linked`|`unknown`)과 `remote_deleted_at`(선택) 추가. v3→v4 마이그레이션은 W3e의 방식(원자적, `registry.json.v3.bak` 한 번만, W3d 등과 충돌 없음)이고, **기존 프로젝트의 `origin`은 `unknown`**(레거시 `legacy` 종류는 `legacy`). 새로 만든 것은 `created`, 연결한 것은 `linked`. W3e 이하 broker는 v4를 깨짐으로 보고 fail closed(롤백은 백업 복원).
- **F-D3 서명 action(`aigate-registry-v1`에 추가):** `remove_project`(subject=프로젝트 id; fields `scope`=`local`만 허용: 서버의 복제본·키·등록부 항목 제거, 원격은 그대로; `legacy_ack`(bool): 레거시 프로젝트는 `true`여야 하고 그 밖의 프로젝트는 `false`/없음), `delete_remote_repo`(subject=프로젝트 id; fields `remote_path`, `remote_id`(숫자 문자열), `confirm_name`(저장소 이름과 같아야 함), `also_local`(bool: 성공 뒤 로컬 정리까지 이어서 함)), `remove_deploy_key`(subject=프로젝트 id; fields `remote_path`: **연결(`linked`)한 프로젝트에서 이 도구가 붙인 배포 키만 떼고** 프로젝트는 등록부에 남김), `clear_orphan`(subject=제공자 id; fields `project`: 고아 기록 하나 제거, 외부 호출 없음). 기존 action 불변.
- **F-D4 broker 외부 호출 허용 목록 추가(정확히 이것만, 시험이 모든 요청을 가로채 확인):** GitHub `DELETE /repos/{owner}/{repo}`(제공자의 `token_ref` 토큰으로, 그 프로젝트의 `origin == created`이고 `remote.id`가 맞을 때만), GitHub `DELETE /repos/{owner}/{repo}/keys/{id}`와 `GET …/keys`(키 찾기: 제목 `orchd <id>`와 공개 키 지문이 둘 다 맞는 것만 떼며 **다른 키는 절대 건드리지 않는다**), GitLab `DELETE /projects/{id}`(제공자의 `token_ref` 토큰으로, `origin == created`, `project_id` 일치), GitLab 배포 키는 **공용 키 하나를 여러 프로젝트가 같이 쓰므로 GitLab 쪽 키 떼기는 `DELETE /projects/{id}/deploy_keys/{key_id}`(그 프로젝트에서만 비활성)로 한정**하고 키 자체를 지우는 호출(`DELETE /deploy_keys/…`)은 코드에 없다. 그룹·조직·토큰·협업자·다른 저장소를 지우는 호출은 코드에 없다. GitLab 서버 설정에 따라 지연 삭제(복구 가능 기간)가 될 수 있으며 응답은 그대로 보고하지 않고 "삭제 요청됨" 고정 문장만 쓴다.
- **F-D5 한도:** 원격 저장소 삭제는 **하루 3회**(broker 전체), 주체별 시간당 3회; 로컬 정리·키 떼기·고아 지우기는 시간당 10회. 속도 제한은 첫 외부 쓰기 직전에만 소모(W3c 규칙).
- **F-D6 오류 코드(고정 문장):** `legacy_ack_required`(레거시를 두 번째 확인 없이 지우려 함), `project_busy`, `origin_unknown`, `not_created_by_tool`(연결한 저장소를 원격 삭제하려 함), `remote_changed`, `confirm_mismatch`, `delete_partial`(원격은 지워졌는데 로컬 정리가 덜 됨 → 상태를 읽고 다시 정리), `key_not_found`, `remove_refused`(제공자가 거부: 토큰 범위 부족 가능). 모두 provider·broker 원문을 반영하지 않는다.
- **F-D7 웹:** 프로젝트 행에 "삭제…" 메뉴(레거시·바쁜 프로젝트는 비활성). 대화상자 없이 화면 전환으로: (1) 무엇을 지울지 고르기(서버 복제본만 / 복제본+원격 저장소 / 배포 키만 떼기), (2) **지울 대상 전체를 평문으로 나열**(원격 이름과 주소, 공개 여부, 복제본 경로, 키 폴더, 원격에 안 올라간 커밋 수·변경 파일 수, **되돌릴 수 없음** 굵게), (3) 원격 삭제는 저장소 이름 직접 입력, (4) 확인 화면에서 브라우저가 `change`를 다시 계산해 맞을 때만 서명, (5) 결과: 단계별 완료 표시와 `delete_partial` 안내(남은 일과 다시 하기). 고아 목록에 "기록 지우기"(서명).

## 검토 초점 (개별 시험이 놓치기 쉬운 것, 가능성 순)
1. **잘못된 대상 삭제:** 이름 변경·소유 이전·같은 이름 재생성(`remote_changed`), `linked`·`unknown`·`legacy` 출처의 원격 삭제 거부(레거시는 로컬 삭제만, 두 번째 확인과 `legacy_ack` 필요), 대소문자 이름, id와 번호 일치, 다른 제공자 id로의 혼동, 서명된 값과 실제 요청의 불일치.
2. **되돌릴 수 없는 순서:** 원격 삭제 후 로컬 정리 실패, 로컬 정리 후 원격 실패가 일어나지 않는 순서인지, 결과 불확실 때 자동 재시도·재서명 유도 없음, 같은 서명 재사용 불가.
3. **로컬 삭제의 파일 시스템 안전:** 심볼릭 링크·`..`·다른 폴더로의 교체 경쟁, `repo_roots` 밖, 레거시 프로젝트 폴더(`sample-app`, `toy`)가 `legacy_ack` 서명 없이는 어떤 경로로도 지워지지 않는지, 진행 중 작업, 시작 때 `.deleting-*` 청소가 남의 것을 지우지 않는지.
4. **토큰 사용 범위:** 삭제 호출이 제공자의 토큰만 쓰는지(OpenAI 키나 다른 종류 토큰으로 나가지 않음), 토큰 종류가 제공자 종류와 맞는지, 삭제 호출이 서명된 대상 밖으로 나가지 않는지, 삭제 권한이 이미 있다는 가정 하에서도 출처·번호·서명 확인이 토큰 권한에 기대지 않고 코드에서 먼저 강제되는지.
5. **허용 목록 확장의 한계:** 새 `DELETE` 행이 정확히 위 셋뿐인지(GitHub 저장소, GitHub 배포 키, GitLab 프로젝트·프로젝트 배포 키 비활성), 경로 인코딩·주입, 응답 본문 비반영.
6. **배포 현실:** orchd가 폴더를 지울 수 있는지(`ReadWritePaths`, 소유자), 등록부 v4와 롤백(`registry.json.v3.bak`), 서버의 기존 프로젝트(`w3e-smoke-1112`·`1113`은 `origin: unknown`이 되어 원격 삭제 대상이 아님: 사용자가 직접 지우거나 아래 메모 참조), 토큰 권한(`delete_repo`)은 사용자 몫.

> **기존 시험 저장소에 대한 메모:** 이미 만든 `w3e-smoke-1112`·`w3e-smoke-1113`은 출처가 `unknown`이라 이 기능으로 원격을 지우지 못한다. 로컬 정리(`remove_project`)는 가능하다. 원격은 사용자가 GitHub에서 직접 지운다. 정말 이 기능으로 지우고 싶다면 사용자가 그 저장소의 출처를 `created`로 바꾸는 **서명된 `claim_origin` 작업**을 추가할 수 있으나, 이 계획에는 넣지 않는다(잘못 주장하면 연결한 저장소를 지울 수 있기 때문).

---

### Task 1 (F1): 등록부 버전 4, 새 action·필드·오류 코드
**파일:** 수정 `internal/registrymsg/*`, `internal/approval/signing/registry_message.go`, `internal/broker/registry.go`(+시험), 웹 상수 드리프트 시험.
- [ ] **시험(이름 그대로):** `TestRegistryV3MigratesToV4Atomically`(내용 불변, `origin`은 `unknown`/`legacy`, `.v3.bak` 한 번만), `TestOldBrokerRefusesV4`, `TestNewProjectsRecordOrigin`(created/linked), `TestDeleteActionsInSchema`(필드·subject 규칙·교차 action 서명 거부), `TestConfirmNameField`, `TestExistingActionsUnchangedByW3f`.
- [ ] **커밋:** `feat(registry): registry v4 with project origin and delete actions`.

### Task 1b (F1b): 기존 저장소 연결의 프로젝트 이름 제약 해제 (W3e 후속, 사용자 요청 2026-11-12)
**배경:** W3e의 `link_project`·`attach_remote`는 프로젝트 id가 원격 저장소 이름과 같아야 했다(`remote_name_mismatch`). 그래서 `swlee3306/sulee_portfolio`처럼 `_`가 든 이름은 연결할 수 없었다. 사용자가 이 제약을 풀기를 원한다.
**결정:** 연결·부착의 **프로젝트 id(= 서버의 폴더 이름 = 키 폴더 이름)는 사용자가 정하고 저장소 이름과 무관**하다. id 규칙은 그대로(`^[a-z][a-z0-9-]{1,39}$`). 저장소 이름·경로는 제공자 규칙대로 `_`·`.`·대문자(GitHub)를 허용한다(GitLab 경로의 소문자 제한은 broker의 GitLab 클라이언트 규칙이라 이번에는 그대로: 문서화). **새 저장소 만들기(`create_project`)는 계속 id = 저장소 이름이다**(우리가 이름을 정해 만들기 때문).
**바꿀 곳:** orchd `link_project.go`의 `remote_name_mismatch` 검사와 `errNameMismatch` 제거, `create_project.go`의 복제 대상 검사에서 `NameMatches` 제거(`filepath.Base(entry.Path) == id`는 유지), `clone/remote.go`의 `NameMatches`를 연결된 프로젝트의 해석기·복제 경로에서 제거하되 **`created` 출처(F1의 `origin`)의 프로젝트는 계속 id와 이름이 같은지 확인**(등록부가 비정상일 때의 방어), 웹: 연결·부착 폼에 **편집 가능한 "프로젝트 이름" 칸**을 두고 저장소 이름에서 제안값(소문자, `_`·`.`→`-`, 규칙에 맞게 자름)을 채우되 사용자가 고칠 수 있다, 확인 화면에 "저장소 이름과 프로젝트 이름이 다름"을 알린다, `remote_name_mismatch`는 코드·웹 문구에서 제거. 서명되는 값은 그대로(subject = 프로젝트 id, `remote_path`). **안전 불변식은 유지:** 원격 URL은 등록부(제공자 `ssh_url` + `remote_path`)에서만 만들고, 복제 대상은 `repo_roots` 바로 아래 `<id>` 하나, 키는 `keys/<id>/`.
- [ ] **시험:** `TestLinkAllowsProjectIdDifferentFromRepoName`(GitHub `Sulee_Portfolio`/`sulee_portfolio` → id `sulee-portfolio`), `TestLinkRepoNameWithUnderscoreAndDotAccepted`, `TestResolverBuildsRemoteFromRegistryPathNotId`(키는 id 폴더, 원격은 저장소 경로), `TestCloneDestIsProjectIdNotRepoName`, `TestCreateProjectStillRequiresIdEqualsName`, `TestCreatedOriginRowWithMismatchedNameRefusedByResolver`, 웹: `TestLinkFormSuggestsProjectNameAndAllowsEdit`, `TestLinkConfirmationShowsBothNames`, `TestRemoteNameMismatchCodeGone`, 이름 제안 함수 벡터(`sulee_portfolio`→`sulee-portfolio`, `Repo.js`→`repo-js`, 한 글자·숫자 시작·40자 초과 처리), 서명 변경 해시 벡터 불변. 기존 시험의 의미 변경 금지(바꾸면 이유).
- [ ] **커밋:** `feat: link existing repositories with a project name independent of the repository name`.

---

### Task 2 (F2): broker 삭제 클라이언트와 op
**파일:** 수정 `internal/broker/githubapi.go`·`gitlabapi.go`(허용 표에 DELETE 행만 추가), `provider_iface.go`; 만들기 `delete_ops.go`.
- [ ] **시험:** `TestOnlyDeleteRowsAddedToAllowlist`(정확히 F-D4의 행), `TestRemoteDeleteRefusedForLinkedUnknownLegacy`, `TestDeleteChecksRemoteIdAndName`(`remote_changed`), `TestDeleteKeyOnlyMatchingTitleAndFingerprint`, `TestGitLabSharedKeyNeverDeleted`, `TestNoGroupOrOrgOrTokenDeletion`, `TestSignedFieldsCoverEveryDeleteInput`, `TestDeleteUsesOnlyTheProvidersOwnToken`, `TestDeletePartialAndUnknownOutcomeNoAutoRetry`, `TestDailyLimit`, `TestCanaryTokenNeverInAnyOutput`.
- [ ] **커밋:** `feat(broker): signed project deletion with a separate delete token`.

### Task 3 (F3): orchd 로컬 정리와 엔드포인트
**파일:** 만들기 `internal/orch/api/delete_project.go`, `internal/orch/remove/remove.go`(+시험); 수정 `internal/orch/api/{v1,api,daemon}.go`.
- [ ] **시험:** `TestRemoveOnlyUnderRepoRootsOneLevel`, `TestRemoveRefusesSymlinkAndSwap`, `TestRemoveLegacyOnlyWithLegacyAck`, `TestRemoveRefusesBusy`, `TestRemoveRenamesThenDeletes`, `TestStartupSweepOnlyOwnDeleting`, `TestUnpushedWorkWarning`(개수만), `TestKeyDirRemoved`, `TestProposeStoresNothingNoSideEffects`, `TestApplyRecomputesChange`, `TestOrderRemoteBeforeLocal`, `TestDeletePartialShowsStateAndRetry`, `TestLedgerNoSecrets`, `TestRequireWriteAndCSRF`, `TestOrchdWorksWithBrokerDown`.
- [ ] **커밋:** `feat(api): local removal and delete endpoints`.

### Task 4 (F4): 웹
- [ ] **시험:** `TestLegacyDeleteNeedsSecondConfirmationAndLegacyAck`(두 번째 화면 없이는 제안 단추 없음, 서명에 `legacy_ack`가 들어감), `TestLegacyRemoteNeverDeletable`, `TestDeleteMenuDisabledForBusy`, `TestDeleteConfirmationListsEveryTarget`, `TestTypedNameRequired`, `TestSignOnlyWhenChangeMatchesW3f`, `TestUnpushedWarningShown`, `TestDeletePartialScreen`, `TestOutcomeUnknownNoAutoRetry`, `TestAllNewErrorCodesFixedTexts`, `TestNoStorageNoNavigateTrap`, 변경 해시 Go 벡터. 시각 확인(`ego-browser skill`, 1280·390px, 실제 `app.js` 경로, 패스키는 대역, 본 것만).
- [ ] **커밋:** `feat(web): project deletion screens`.

### Task 5 (F5): 적대 시험, 결과(`docs/29`), 컷오버 런북(`docs/10` 16장)
- [ ] 적대 시험과 변이 표(서명 검증 제거, `remote_id` 확인 제거, `origin` 확인 제거, 삭제 토큰 대신 일반 토큰 사용, 레거시 거부 제거, `SameFile` 확인 제거, 허용 목록 확장 한계 제거 등), 결과 문서(서버에서 확인하지 못한 것: 실제 GitHub `DELETE`와 `delete_repo` 범위, GitLab 지연 삭제, orchd의 실제 폴더 삭제 권한 등), 런북(등록부 v4 백업·마이그레이션·롤백, 삭제 권한은 일반 토큰에 이미 있음(별도 토큰 없음), 되돌릴 수 없는 단계는 사용자 결정).
- [ ] 전체 `go test ./... -race`, node, `TestConstantsAgreeWithWeb`; 커밋 `test: adversarial suite for W3f`, `docs: W3f results and cutover runbook`.

---

## 자체 검토 기록
- **레거시(사용자 결정 2026-11-12):** 레거시 프로젝트의 서버 쪽 삭제는 허용하되 두 번째 확인(`legacy_ack` 서명)을 거친다. 레거시의 원격은 지우지 못한다.
- **범위:** 요청의 "클론"(F3 로컬 정리)과 "원격"(F2 원격 저장소 삭제)을 덮는다. 추가한 것: 배포 키만 떼기(연결한 저장소를 지우지 않고 정리), 고아 기록 지우기(삭제 기능이 없어서 쌓이던 것). **의도적으로 뺀 것:** 그룹·조직 삭제, 출처 주장(`claim_origin`), 서명 없는 삭제, 일괄 삭제, W3d(자동 병합).
- **원칙 변경의 근거와 한계:** 사용자가 일반 토큰에 삭제 권한까지 주었다(F-D1). 토큰이 유출되면 그 계정의 저장소가 위험하다는 점은 사용자가 감수했다.
- **사용자 확인이 필요한 값(계획 승인 시):** ① (철회됨) 삭제 토큰 분리는 하지 않음: 일반 토큰에 삭제 권한이 이미 있음, ② 레거시 프로젝트는 서버 쪽 삭제만 두 번째 확인과 함께 허용(원격 삭제는 불가), ③ 이미 만든 시험 저장소의 원격은 사용자가 직접 지움, ④ 원격 삭제는 이 도구가 만든 것만, ⑤ GitHub 토큰의 `delete_repo` 범위는 사용자가 이미 부여함(확인은 사용자 몫).
