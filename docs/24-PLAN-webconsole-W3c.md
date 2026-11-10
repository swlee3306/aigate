# W3c 구현 계획: 제공자(GitLab) 설정, 새 그룹·새 프로젝트 생성, 복제와 게시 연결

> **에이전트 실행자용:** 필수 하위 스킬은 `superpowers:subagent-driven-development`(권장) 또는 `superpowers:executing-plans`이다. 단계는 체크박스(`- [ ]`)로 추적한다.

**목표:** 웹에서 GitLab 서버를 "제공자"로 등록하고(토큰은 W3b의 저장소에 있는 관리자 토큰을 `token_ref`로 가리킴), 그 서버에 **새 그룹**과 **새 프로젝트**(빈 프로젝트 + `.aigate.yml`이 든 첫 커밋 + 배포 키 + `main` 보호)를 서명 승인으로 만들고, orchd가 그 저장소를 `/home/orch/repos/<이름>`에 복제하며, 게시(`orch/<번호>` 브랜치 push)가 등록부의 제공자 정보로 동작하게 한다. 자동 병합, MR 생성 이전, GitHub, 기존 프로젝트의 GitLab 연결은 이 계획의 범위가 아니다(W3d·W3e).

**구조:** broker가 제공자(`providers`)를 등록부에 소유하고, GitLab 호출 전체를 **고정된 (메서드, 경로 모양) 허용 목록**으로만 한다(삭제 호출 없음, 주소는 제공자 설정으로 고정, 리다이렉트 안 따름). 변경 op는 W3a·W3b와 같은 서명 검증(`aigate-registry-v1`에 action 더함)을 거친다. 프로젝트 생성은 broker가 GitLab 쪽 4~5단계를 하고 **마지막에** 등록부에 등록한다. 복제와 게시는 orchd 몫이다(저장소 소유자는 `orch`): 복제는 고정된 안전 옵션으로 하고, 게시 원격은 등록부의 제공자 정보에서 **고정된 SSH 호스트 키 파일에 있는 호스트만** 허용해 계산한다.

**기술:** Go(broker, orchd, registrymsg, signing, publish), 바닐라 JS(웹), 가짜 GitLab 서버(httptest)·가짜 broker·임시 git 저장소로 시험.

**설계서:** `docs/19-DESIGN-webconsole-W3-projects.md`(v0.4)의 4.6, 5.3, 6, 7장 중 W3c 몫. 앞 단계: `docs/20`~`23`(W3a·W3b, 컷오버 완료).

## 전역 제약 (모든 작업에 적용)
- **비밀은 어디에도 새지 않는다.** 관리자 토큰은 broker 밖으로 나가지 않는다: 응답·로그·원장·오류·GitLab에 보내는 본문(헤더 `PRIVATE-TOKEN` 외)·테스트 출력에 없다. 오류는 고정 코드와 고정 문장뿐이다(GitLab이 준 응답 본문·오류 문장은 **읽지도, 반영하지도 않는다**). 시험의 가짜 토큰은 `canary-NOT-A-REAL-TOKEN-…`.
- **배포 현실(W3a·W3b 교훈).** 각 작업 보고서에 **경로·소유자·모드·실행 사용자·샌드박스 규칙 표**를 넣는다. 실서버 사실: broker는 `orch-broker`(uid 999, `ProtectSystem=strict`, `ProtectHome=yes`, 쓰기 `/var/lib/orch-broker`·`/run/orch-broker`만)이며 **`/home`을 보지 못한다**(그래서 broker는 저장소 경로의 파일 시스템에 접근하지 않는다); orchd는 `orch`(`ProtectHome=read-only`, `ReadWritePaths=/home/orch/orchd /home/orch/repos`)이고 `/home/orch/repos`는 `orch:orch 0700`, 배포 키는 `/home/orch/orchd/ssh/id_gitlab_deploy`(+`.pub`, 서버에 있음)와 고정 호스트 키 파일 `known_hosts_gitlab`; GitLab(사내 `http://100.64.0.10:18082`, ssh 22)은 nft가 허용하는 주소다(다른 주소는 새 방화벽 규칙이 필요하며 **방화벽을 임의로 바꾸지 않는다**). 시험은 임시 폴더에서만 돌므로 **시험이 통과해도 서버 권한을 보증하지 않는다**: 컨트롤러가 서버에서 읽기 전용으로 직접 확인할 항목을 보고서·런북에 목록으로 남긴다.
- **GitLab 호출 허용 목록(broker, 정확히 이것만, 시험이 가짜 서버에서 모든 요청을 가로채 확인):** `GET /api/v4/groups`(목록, 쿼리 `min_access_level` 등 고정 몇 개), `GET /api/v4/groups/{path}`, `POST /api/v4/groups`, `GET /api/v4/projects/{path}`(존재 확인), `POST /api/v4/projects`, `POST /api/v4/projects/{id}/repository/files/{path}`(첫 커밋: 파일마다 한 번, C2 결정으로 `/repository/commits` 대신), `GET /api/v4/projects/{id}/protected_branches/{name}`, `POST /api/v4/projects/{id}/protected_branches`, `PUT`(해당 보호 규칙 갱신이 문서상 필요하면 구현자가 공식 문서로 확인해 한정), `POST /api/v4/projects/{id}/deploy_keys`. **삭제(`DELETE`)·`PUT`/`PATCH`로 프로젝트·그룹 변경·병합·토큰 관련 호출은 코드에 없다.**
- **제공자 주소 검증(broker):** `http`/`https`만, 사용자 정보·쿼리·조각·경로(`/api/v4`는 코드가 붙임) 금지, 호스트 정규화, **리다이렉트 안 따름**, 연결 대상 IP가 루프백·링크 로컬(`169.254.0.0/16` 포함)이면 거부(사설망·tailnet IP는 허용), 연결·전체 제한시간 고정. OpenAI 주소는 코드 상수이며 이 목록으로 바뀌지 않는다. `ssh_url`은 `ssh://git@호스트[:포트]` 형태만(경로·자격 증명 금지).
- **모든 변경에 서명**(W3a와 같은 `verifySigned` 순서: 엄격 키 → 상태 사용 가능 → `not_signed` → `change` 필수 → 서명자 매 호출 로드 → 검증된 구성 요소로 메시지 재구성 → `VerifyRegistry` → `change` 일치 → **그 뒤에만** 요청 ID 소모). 서명 전에 거부되는 모양 오류는 ID를 소모하지 않고, 서명이 유효하면 이후 단계에서 실패해도 소모된다.
- orchd 바이너리는 `internal/broker`와 OpenAI 코드를 import하지 않는다. 공유 순수 코드는 `internal/registrymsg`(및 새 `internal/gitlabnames` 같은 순수 패키지는 허용).
- 새 쓰기 엔드포인트는 `requireWrite`(CSRF) 뒤, 서명 필요. 웹: W1~W3b 규칙(인라인 스크립트·스타일 금지, 서버 문자열은 텍스트 노드, 위험 API 금지, `confirm/alert` 금지, 저장소 API 금지, **`type=password`·`text-security` 금지**). 기존 테스트의 의미를 약화하지 않는다. 커밋 메시지 끝: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. `rg`가 없으니 `grep -rn`(글롭은 따옴표).
- 검증 기본: `go vet ./...`, `gofmt -l .`(비어야 함), `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`, `go test ./internal/... ./cmd/... -count=1 -race`(요약 줄을 걸러 내지 않고 본다), 웹 작업은 `node --test "internal/orch/web/test/*.test.js"`와 `go test -run TestConstantsAgreeWithWeb -v ./internal/orch/api`(PASS, SKIP 없음).

## 정해 둔 결정 (구현자가 다시 정하지 않는다)
- **D1 등록부 확장(버전 2):** `providers: [{"id","kind":"gitlab","base_url","ssh_url","token_ref"}]`(id·token_ref는 `^[a-z][a-z0-9-]{0,39}$`, `token_ref`는 저장소에 있는 `gitlab` 종류 토큰 이름), 프로젝트 항목의 `provider`(제공자 id 또는 null)와 `gitlab:{"project":"그룹/이름[/…]","project_id":정수|null}`(W3a가 자리만 둔 항목을 쓴다). `auto_merge`는 계속 항상 꺼짐(켜려는 요청은 `not_supported`). 등록부 파일 버전을 2로 올리고 W3b 이하 broker는 이 파일을 "깨짐"으로 보고 fail closed 한다(롤백 때 등록부를 W3b 백업으로 되돌려야 함: 런북에 경고). 마이그레이션은 버전 1 파일을 읽어 2로 올려 쓴다(원자적, 서명 불필요: 내용 변화 없음). 고아 기록 `orphans:[{"provider","project","project_id","step","at"}]`(최대 100개, 값 없음).
- **D2 서명 action 추가(`aigate-registry-v1`, 정의는 `registrymsg` 스키마 표에 더함):** `set_provider`(subject=제공자 id; fields `kind`,`base_url`,`ssh_url`,`token_ref`), `delete_provider`(fields 없음; **참조하는 프로젝트가 있으면 `provider_in_use`**), `create_group`(subject=`<제공자 id>`; fields `name`,`path`,`parent_path`(""=최상위)), `create_project`(subject=새 프로젝트 id=이름; fields `provider`,`group_path`,`template`(`basic`|`go`),`description`(최대 200자, 제어·방향 전환 문자 없음),`ai_context`(bool),`publish`(`write`|`read`),`deploy_key_sha256`(64 hex: 서명하는 공개 키 한 줄의 SHA-256)). 이름 규칙 `^[a-z][a-z0-9-]{1,39}$`(프로젝트 id = GitLab 프로젝트 이름 = 폴더 이름). 기존 action(`register_project`, `set_flag`, `set_token`, `delete_token`)은 불변.
- **D3 broker op와 오류 코드(고정 문장, `fixedMessages` 등록):** 읽기 `providers`(서명 불필요, 비밀 없음), `list_groups`(args `provider`; GitLab에 실제로 물어 그룹 경로·이름만 줌, 서명 불필요), 쓰기 `set_provider`, `delete_provider`, `create_group`, `create_project`. 새 코드: `provider_unknown`, `provider_exists`, `provider_in_use`, `bad_url`, `token_ref_unknown`(저장소에 그 이름의 `gitlab` 토큰이 없거나 쓸 수 없음), `provider_unreachable`(연결·시간 초과·TLS 등: GitLab 문장 미반영), `gitlab_refused`(4xx/5xx 응답: 상태 코드 범주만), `group_unknown`, `group_exists`, `project_name_taken`(GitLab에 이미 있거나 등록부 id/경로 충돌), `create_partial`(GitLab 쪽 일부만 만들어짐 → **고아 기록 후 응답에 `{provider, project, project_id, step}`**), `deploy_key_mismatch`(받은 공개 키의 SHA-256이 서명된 `deploy_key_sha256`과 다름), `template_unknown`. W3a·W3b 코드 재사용.
- **D4 `create_project` 순서와 멱등:** (1) 서명·모양 검증(이름·그룹 경로·템플릿·`deploy_key` 공개 키 형식·해시 일치), 등록부에 같은 id·경로(`/home/orch/repos/<이름>`)가 있으면 `project_name_taken`; (2) `GET /groups/{group_path}`로 그룹 존재 확인(`group_unknown`); (3) `GET /projects/{group_path}%2F{이름}`로 이미 있는지 확인(있으면 `project_name_taken`, **기존 프로젝트를 인수하지 않는다**); (4) `POST /projects`(`name`, `path`=이름, `namespace_id`, `visibility: private`, `initialize_with_readme: false`, `default_branch: main`, `description`); (5) 첫 커밋: **`POST /projects/{id}/repository/files/{path}`를 파일마다 한 번**(`README.md`가 먼저이고 `branch: main`으로 `main`을 만든다; GitLab 이슈 #577329가 빈 프로젝트에서 `/repository/commits`가 404라고 기록해서 C2가 이 방식으로 정했다: **파일 수만큼의 요청이고 원자적이지 않다**, 중간 실패는 `create_partial`(단계 `first_commit`). 서버(GitLab 19.0.8)에서의 실제 동작은 컷오버에서 처음 확인). 첫 커밋 뒤 `GET /projects/{id}`로 `default_branch`를 읽고 `main`이 아니면 등록하지 않고 `create_partial`(단계 `default_branch`); (6) `GET …/protected_branches/main`: 푸시·병합 허용 수준이 Maintainer(40) 이상이고 강제 push 금지인지 확인하고 아니면 `POST …/protected_branches`로 그렇게 설정(허용 목록의 한도 안에서); (7) `POST …/deploy_keys`(`title: "orchd <이름>"`, `key`: 공개 키 한 줄, `can_push`: `publish == "write"`); (8) **마지막에** 등록부에 항목 추가(`kind: "registered"`, `path: /home/orch/repos/<이름>`, `ai_context`, `provider`, `gitlab:{project:"그룹경로/이름", project_id}`). 4~7 중 하나가 실패하면 **삭제하지 않고** 고아 기록(`step`=실패 단계)을 남기고 `create_partial`로 답한다: 이미 만든 GitLab 프로젝트는 사용자가 GitLab에서 정리하고(우리는 삭제 기능이 없다), 같은 이름으로 다시 만들려면 그 정리가 먼저다(`project_name_taken`). 재개(resume)는 만들지 않는다. 한 번의 호출 전체 제한 60초, GitLab 호출당 15초.
- **D5 복제(orchd, `POST /api/v1/projects/{id}/clone`과 `create_project` 응답 직후 자동 1회):** 서명은 필요 없다(이미 서명된 등록부 항목을 따라 복제만 하고 등록부를 바꾸지 않는다). 대상: 등록부 항목의 `path`(`repo_roots` 바로 아래 한 단계, 아직 없을 때만; 이미 있으면 `already_cloned`), 원격: 제공자 `ssh_url` + `/` + `gitlab.project` + `.git`. **고정 안전 옵션**: `GIT_SSH_COMMAND`는 코드가 만든 값(`-i <--git-ssh-key> -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=<--git-ssh-known-hosts> -o BatchMode=yes`), `git -c protocol.allow=never -c protocol.ssh.allow=always -c core.hooksPath=/dev/null -c core.fsmonitor=false clone --no-recurse-submodules --no-local`, 환경에서 `GIT_*`·`SSH_*` 정리, 시간 제한 120초, 실패 시 만들다 만 폴더를 지운다(자기가 만든 폴더만). 호스트는 **고정 호스트 키 파일에 있는 호스트만** 허용(없으면 `host_not_pinned`, 복제 시도 안 함). 복제 상태는 등록부가 아니라 **orchd가 파일 시스템을 보고 계산**한다(`cloned: bool`).
- **D6 게시 원격 해석(orchd, `publish` 패키지):** `publish.Config`에 선택 필드 `RemoteResolver func(repo string) (string, bool)`를 더한다. 정적 `RepoRemotes`에 없는 저장소에 대해서만 쓰이고, 등록부(broker `projects`를 짧게 캐시: 30초 또는 변경 직후 무효화)의 `provider`·`gitlab.project`로 `ssh_url`+`/`+프로젝트+`.git`을 만들며 **호스트가 고정 호스트 키 파일에 있을 때만** 반환한다(요청·작업 본문에서 온 값은 쓰지 않는다). 기존 정적 원격 동작과 시험은 불변. 게시가 쓰기 권한 배포 키(`publish: write`)가 필요한 프로젝트에서 push가 거부되면 기존 고정 오류로 실패한다.
- **D7 정책 클래스:** 새 프로젝트의 `kind`는 `registered`다. `policy.yml`에 `registered: {level: L1}`가 있어야 L1로 돈다(없으면 `default_level` L0). orchd의 propose는 정책에 `registered`가 **없으면** `policy_class_missing`으로 서명 전에 거부한다(관리자가 정책에 한 번 넣어야 함: 정책 해시가 바뀌므로 실행 중 작업이 없을 때, 런북 0단계).
- **D8 템플릿(코드에 고정, 사용자 글이 파일로 들어가지 않음):** `basic`: `README.md`, `.gitignore`, `.aigate.yml`(`version: 1`, `protected: [".aigate.yml"]`, 검사 최소 하나 — **`checks`가 비어도 되는지 구현자가 `internal/config`로 확인**하고 안 되면 항상 통과하는 `exit-code` 검사 하나(`true`)를 넣되 README에 "자동 병합 대상이 아님"을 적는다); `go`: `go.mod`(`module <이름>`), `main.go`, `main_test.go`, `README.md`, `.gitignore`, `.aigate.yml`(보호 파일 `**/*_test.go`·`go.mod`·`.aigate.yml`, 검사 `unit`(preset go)·`vet`·`build` 모두 `required: true`). 시험: 두 템플릿의 `.aigate.yml`이 **게이트가 쓰는 `config` 파서를 통과**하고 `go` 템플릿은 임시 폴더에서 `go vet ./... && go test ./...`가 통과한다.
- **D9 한도(주체별):** `create_project` 시간당 5, `create_group` 시간당 5, `set_provider`·`delete_provider` 시간당 10, `list_groups` 시간당 60. broker 전체에도 같은 종류의 상한(시간당 `create_project` 10, GitLab 쓰기 호출 합계). 환급은 W3a·W3b 규칙(Unsent·busy·forbidden만).
- **D10 웹:** (a) `#/providers`: 제공자 목록(id, 주소, ssh 주소, 토큰 이름·**마지막 4자·만료**(W3b `tokens`에서), 도달 여부는 표시하지 않음), "제공자 추가"(저장소의 `gitlab` 토큰 중 선택), 삭제(사용 중이면 단추 비활성). (b) `#/projects`에 "새 프로젝트 만들기": 제공자 선택 → `list_groups`로 받은 그룹 선택 또는 "새 그룹 만들기"(별도 서명) → 이름·설명·템플릿·AI 전송(기본 꺼짐)·게시 권한(쓰기/읽기) → **확인 화면에 변경 전체**(제공자, 그룹 경로, 이름, 템플릿, AI 전송, 게시 권한, 배포 키 지문 `ssh-keygen -l` 형식 SHA-256과 `.pub` 한 줄의 앞부분)를 보여 주고 브라우저가 `change`를 다시 계산해 맞을 때만 서명 단추 → 서명 → 진행 결과(단계별: 그룹 확인, 프로젝트, 첫 커밋, 보호, 배포 키, 등록, 복제) → 실패 시 `create_partial` 안내(고아 프로젝트 경로·ID를 텍스트로 보여 주고 "GitLab에서 직접 지우세요, 이 도구는 삭제하지 않습니다"), 복제만 실패하면 "다시 복제" 단추(서명 없음). 목록에 `cloned` 여부와 고아 알림 배지. 서버가 준 문장은 모두 텍스트 노드.

## 검토 초점 (설계서가 암시하지만 개별 작업 시험이 안 다루기 쉬운 것, 가능성 순)
1. **관리자 토큰의 영향 범위**: broker가 GitLab으로 낼 수 있는 모든 (메서드, 경로, 본문 필드)가 허용 목록 안에 있는지(삭제·변경·병합 호출이 코드 경로에 없음), 요청 본문에 사용자 글이 어떻게 들어가는지(이름·설명·그룹 경로 → URL 경로 인코딩과 JSON 본문 구분, `../`·`%2F` 주입으로 다른 그룹·프로젝트에 닿기), 응답 본문 반영 없음, 제공자 주소 SSRF(사설·메타데이터 주소), 리다이렉트, 토큰이 다른 제공자 주소로 새는지(제공자 `base_url` 변경 + 같은 `token_ref` 재사용: 주소가 바뀌면 서명 대상에 들어 있어 사용자가 보게 되는가).
2. **부분 실패·고아·멱등**: 4~7 각 단계 실패 뒤 상태(고아 기록, 등록부 미등록), 같은 요청 재전송, 동시 `create_project` 같은 이름, GitLab에 이미 있는 이름 인수 금지, 등록 단계(8) 실패(등록부 쓰기 실패) 때의 고아 처리, 호출 시간 초과 시 "결과 불확실" 취급(쓰기 op의 `timeout` = 불확실).
3. **복제 안전**: 원격 URL이 등록부에서만 오는지, 호스트 고정, 옵션 주입(`-o`, `--upload-pack`, `ext::`, `file://` 프로토콜), 이미 존재하는 경로·심볼릭 링크 대상, 폴더 이름과 등록부 id의 일치, 실패 시 폴더 정리가 남의 폴더를 지우지 않는지, 복제된 저장소의 훅 비활성.
4. **게시 원격 해석의 신뢰**: 해석기가 등록부(서명으로만 바뀜)와 고정 호스트 키 파일만 믿는지, 정적 원격과의 우선순위, 캐시 무효화, broker가 꺼졌을 때 게시 동작(fail closed 또는 정적 원격만).
5. **배포 현실**: orchd가 `/home/orch/repos`에 새 폴더를 만들 수 있는지(`ReadWritePaths`, 소유자), 배포 키 파일 읽기, SSH 실행 파일·`known_hosts_gitlab`, broker가 GitLab(HTTP)에 도달하는지(nft), 새 GitLab 주소면 방화벽 필요, 등록부 버전 2와 롤백(옛 broker가 등록부를 깨짐으로 봄).

---

### Task 1 (C1): 서명 메시지 action 확장과 등록부 버전 2

**파일:**
- 수정: `internal/registrymsg/registrymsg.go`(스키마 표·정규식·도우미), `internal/approval/signing/registry_message.go`(action 상수), `internal/broker/registry.go`(버전 2, providers, 프로젝트의 provider·gitlab 필드, 고아 기록, 마이그레이션 1→2, 검증 완화), 각 `_test.go`
- 만들기(선택): `internal/gitlabnames/…`(그룹·프로젝트 경로 검증·인코딩 순수 함수)

**인터페이스:**
- 생산: action 상수 `set_provider`, `delete_provider`, `create_group`, `create_project`와 필드 스키마(D2), 이름·그룹 경로·`deploy_key_sha256` 검증, 정규식과 상수가 웹과 같음을 비교하는 드리프트 시험 확장, `Registry` v2(`providers []Provider`, `Project.Provider *string`, `Orphans []Orphan`), `OpenRegistry`의 1→2 마이그레이션, `validateRegistry` 완화(제공자·gitlab 허용, 자동 병합은 계속 불가).

- [ ] **Step 1: 실패하는 시험(이름 그대로)** — `TestNewActionsInSchema`(필드 종류·알 수 없는 필드·빈 값 규칙 거부), `TestGroupAndProjectNameRules`(정규식 경계, `.`·`..`·`/`·`%2f`·유니코드 거부, 예약 이름), `TestDeployKeyHashField`(64 hex 아님 거부), `TestTemplateAndPublishEnums`, `TestDescriptionRules`(200자, 제어·방향 전환·개행 거부), `TestExistingActionsUnchanged`, `TestSubjectRulePerAction`(제공자 id·프로젝트 id·토큰 ref 규칙이 서로 섞이지 않음), `TestSigningDefinitionsDoNotDrift`(새 action·정규식까지), `TestRegistryV1MigratesToV2Atomically`(내용 불변, 모드 0600, 실패 시 v1 그대로), `TestRegistryV2ValidationRules`(제공자 id·URL 모양, `token_ref` 정규식, 프로젝트가 등록부에 없는 제공자 id를 참조하면 거부, 중복 제공자, `auto_merge` 켜짐 거부, 최대 개수·크기), `TestOrphansBounded`(100개 상한, 오래된 것 정리 방식 고정), `TestOldBrokerVersionRefusesV2`(문서화: 버전 값 2는 W3b 코드가 깨짐으로 취급함을 시험이 `version` 상수로 고정).
- [ ] **Step 2: 실패 확인** — `go test ./internal/registrymsg/ ./internal/approval/signing/ ./internal/broker/ -count=1 -run 'NewActions|GroupAndProject|DeployKey|Template|Description|Existing|SubjectRule|Drift|V1MigratesToV2|V2Validation|Orphans|OldBroker'`.
- [ ] **Step 3: 구현** — W3a·W3b의 스키마 표에 항목을 더하는 방식. 등록부 쓰기 코드는 `registry.go` 한 곳. 기존 시험은 의미 변경 없이 통과(바꾼 것은 이유와 함께 보고서에).
- [ ] **Step 4: 통과 확인** — 같은 명령 + 전체 `./internal/... ./cmd/...`.
- [ ] **Step 5: 커밋** — `feat(registry): providers, project gitlab links, orphans and four new signed actions (registry v2)`.

---

### Task 2 (C2): broker GitLab 클라이언트(허용 목록·주소 검증)와 제공자 저장

**파일:**
- 만들기: `internal/broker/gitlabapi.go`, `internal/broker/gitlabapi_test.go`, `internal/broker/provider.go`, `internal/broker/provider_test.go`
- 수정: `internal/broker/server.go`(와이어링), `internal/broker/config.go`(필요 시), `cmd/orch-broker/main.go`(`-check`가 제공자 설정·토큰 참조를 읽기 전용으로 확인)

**인터페이스:**
- 소비: C1의 레지스트리·이름 검증, W3b의 `TokenStore.Value(ref)`.
- 생산: `type GitLabAPI struct`와 메서드 `Groups(ctx)`, `Group(ctx, path)`, `CreateGroup(ctx, name, path, parentPath)`, `Project(ctx, path)`, `CreateProject(ctx, …)`, `CommitFiles(ctx, projectID, files)`, `ProtectedMain(ctx, projectID)`/`EnsureProtectedMain`, `AddDeployKey(ctx, projectID, title, key, canPush)`: **모든 요청이 이 파일의 한 `do` 함수를 거치고**, `do`는 허용 (메서드, 경로 모양) 표와 대조한 뒤에만 보낸다. 주소 검증기 `ValidateProviderURL`, `ValidateSSHURL`, IP 거부 규칙.

- [ ] **Step 1: 실패하는 시험** — `TestOnlyAllowlistedRequestsReachGitLab`(가짜 서버가 **모든** 요청의 메서드·경로를 기록: 각 공개 메서드를 정상·오류 경로로 호출한 뒤 기록된 집합이 허용 목록의 부분집합이고 삭제·변경·병합·토큰 경로가 없음), `TestDoRefusesOffListRequests`(허용 표 밖 (메서드, 경로)를 `do`에 직접 주면 거부), `TestPathEncodingNoInjection`(그룹 경로·프로젝트 이름에 `/`·`%2f`·`..`·`?`·`#`·개행·유니코드: 모양 검사에서 거부되고, 통과한 값은 한 번만 인코딩되어 다른 경로로 새지 않음), `TestTokenOnlyInHeader`(가짜 서버가 본문·URL·쿼리에서 토큰을 찾지 못함), `TestNoRedirectFollowed`, `TestProviderURLRules`(스킴, 사용자 정보, 경로, 쿼리, 루프백, 링크 로컬·메타데이터, 사설·tailnet 허용, 호스트 대소문자·끝 점, IPv6, 숫자 호스트 변형 `0x7f.1` 등), `TestSSHURLRules`, `TestGitLabTextNeverReflected`(가짜 서버가 오류 본문에 canary를 넣어도 오류·로그에 없음), `TestStatusMapping`(401/403/404/409/422/5xx → 고정 코드 범주), `TestTimeoutsAndBodyCap`(응답 크기 상한, 연결·전체 제한시간), `TestTokenRefResolution`(없거나 쓸 수 없는 토큰 → `token_ref_unknown`, 요청 안 나감), `TestProviderStoreRoundTrip`(등록부에 제공자 추가·삭제·참조 중 삭제 거부), `TestCheckValidatesProviders`(`-check`: 제공자·토큰 참조 확인, 아무것도 쓰지 않음, 네트워크 접속 안 함).
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ ./cmd/orch-broker/ -count=1 -run 'GitLab|Allowlist|Provider|Redirect|TokenOnly|PathEncoding|SSHURL'`.
- [ ] **Step 3: 구현** — GitLab REST 사양(엔드포인트·필드·응답 코드)은 **구현 시점에 공식 문서로 확인**하고 출처를 보고서에 쓴다(특히 첫 커밋 API의 빈 저장소 동작, 보호 브랜치 필드). 응답은 필요한 필드만 엄격히 디코딩하고 나머지는 버린다.
- [ ] **Step 4: 통과 확인** — `-race` 포함, 기존 broker 시험 의미 변경 없음.
- [ ] **Step 5: 커밋** — `feat(broker): allowlisted GitLab API client and provider storage with URL validation`.

---

### Task 3 (C3): broker op — 제공자, 그룹, 프로젝트 생성

**파일:**
- 만들기: `internal/broker/providers_ops.go`, `internal/broker/create_project.go`, `internal/broker/templates.go`(고정 템플릿) 와 각 `_test.go`
- 수정: `internal/broker/server.go`(op 등록, `fixedMessages`, 제한 시간: `create_project` 60 s, 나머지 15~20 s), `internal/broker/projects.go`(공통 `verifySigned` 재사용 필요한 만큼)

**인터페이스:**
- 소비: C1 action·스키마, C2 `GitLabAPI`·제공자 저장, W3a의 `verifySigned` 순서, W3b의 토큰 저장소.
- 생산: op `providers`, `list_groups`, `set_provider`, `delete_provider`, `create_group`, `create_project`(D2~D4, D9), 템플릿 파일 집합 `TemplateFiles(template, name) []File`, 고아 기록 쓰기, `create_project` 응답 `{project:{id,path,kind,ai_context,provider,gitlab,auto_merge}, ssh_url, clone_url, cloned_hint}`.

- [ ] **Step 1: 실패하는 시험** — 서명(W3a·W3b 표 그대로): `TestProviderWritesRequireSignature`, `TestChangeRequiredAndMismatchBurnsNoID`, `TestRequestIDOnceAndBurnedAfterValidSignature`, `TestShapeErrorsBurnNoID`, 제공자: `TestSetProviderRules`(주소 규칙·토큰 참조 확인·같은 id 재설정은 서명된 변경으로만, `base_url` 변경 시 `token_ref`가 그대로여도 서명 대상에 새 주소가 들어 있음), `TestDeleteProviderInUse`, 그룹: `TestListGroupsReadOnlyNoSecrets`, `TestCreateGroupFlow`(가짜 GitLab: 비공개 고정, 부모 경로, 이미 있으면 `group_exists`, 이름·경로 규칙), 프로젝트: `TestCreateProjectHappyPath`(가짜 GitLab이 받은 요청 순서 (3)(4)(5)(6)(7)과 본문 필드가 D4와 일치, 마지막에 등록부 항목, 응답 모양), `TestCreateProjectBindsDeployKey`(서명된 `deploy_key_sha256`와 받은 공개 키가 다르면 `deploy_key_mismatch`이고 GitLab에 아무 요청도 안 나감), `TestCreateProjectNameTaken`(GitLab에 이미 있는 이름·등록부 충돌 → `project_name_taken`, **기존 프로젝트를 인수하지 않음**, 쓰기 요청 없음), `TestPartialFailureRecordsOrphan`(4·5·6·7 각 단계 실패 주입 → `create_partial` + 고아 기록(step), 등록부에는 프로젝트 없음, 삭제 호출 없음), `TestRegistryWriteFailureAfterGitLabIsOrphan`, `TestConcurrentCreateSameNameOne`(`-race`), `TestTemplatesParseWithGateConfig`(두 템플릿의 `.aigate.yml`이 `internal/config` 파서를 통과, `go` 템플릿은 임시 폴더에서 `go vet`·`go test` 통과), `TestTemplateContentFixed`(사용자 글이 파일에 들어가지 않음: 이름만 치환, 이름 규칙으로 안전), `TestCreateProjectNeverTouchesFilesystem`(broker가 저장소 경로를 stat하지 않음), `TestCanaryTokenNeverInAnyOutput`(모든 성공·실패·패닉 경로), `TestTimeoutIsUnknownOutcome`(60 s·호출 시간 초과 → 쓰기 `timeout`은 불확실로 취급), `TestFixedMessagesRegisteredForNewCodes`, `TestOpAllowlist`(삭제 계열·`set_*`가 아닌 임의 op는 `unknown_op`).
- [ ] **Step 2: 실패 확인** — `go test ./internal/broker/ -count=1 -run 'Provider|Group|CreateProject|Template|Partial|Orphan'`.
- [ ] **Step 3: 구현** — W3a `verifySigned`의 순서를 그대로 따른다. GitLab 호출은 C2의 `GitLabAPI`만 쓴다. 템플릿은 코드에 상수로 두고 이름만 치환한다. 첫 커밋 API 동작이 공식 문서와 다르면 대체안을 보고서에 쓴다.
- [ ] **Step 4: 통과 확인** — `-race`.
- [ ] **Step 5: 커밋** — `feat(broker): providers, groups and signed project creation with orphan records`.

---

### Task 4 (C4): orchd — 제안·적용 엔드포인트, 복제, 게시 원격 해석

**파일:**
- 만들기: `internal/orch/api/providers.go`, `internal/orch/api/create_project.go`, `internal/orch/clone/clone.go`(+시험), 각 `_test.go`
- 수정: `internal/orch/broker/client.go`(새 op), `internal/orch/api/{v1.go,api.go,daemon.go}`(경로·설정·한도), `internal/orch/publish/publish.go`(`RemoteResolver`), `cmd/orchd/{main.go,vm.go}`(해석기 연결: 새 플래그 없음)

**인터페이스:**
- 소비: C3 op, W3a 프로젝트 엔드포인트 패턴(`propose`/`apply`), W3b의 배포 키 공개 키 조회, 정책 클래스.
- 생산: `GET /providers`, `GET /providers/{id}/groups`(broker `list_groups` 전달), `POST /providers/propose`·`/providers/apply`(set_provider/delete_provider/create_group), `POST /projects/create/propose`·`/projects/create/apply`(propose는 `policy_class_missing`·배포 키 지문 계산·`deploy_key_sha256` 포함 메시지), `POST /projects/{id}/clone`, 목록 응답에 `cloned`·`orphans`, 복제 도우미 `clone.Clone(ctx, opts) error`, `publish.Config.RemoteResolver`.

- [ ] **Step 1: 실패하는 시험** — 엔드포인트(W3a 패턴): `TestProvidersListAndGroupsViaBroker`, `TestProposeBuildsMessageStoresNothingSendsNoWrite`(set_provider·delete_provider·create_group·create_project 모두), `TestCreateProposeIncludesDeployKeyHash`(공개 키를 `<--git-ssh-key>.pub`에서 읽고 SHA-256이 `fields.deploy_key_sha256`에 들어감, 개인 키는 열지 않음), `TestCreateProposePolicyClassMissing`(서명 전 거부), `TestApplyRecomputesChangeAndRefusesMismatch`, `TestApplyForwardsDeployKeyAndSignature`(공개 키 한 줄을 broker 인자로 그대로), `TestApplyMapsAllNewBrokerCodes`(고정 문장·상태, GitLab·broker 문장 미전달, `create_partial`은 고아 정보(공개 값만)를 응답에 실음), `TestWriteTimeoutIsOutcomeUnknown`, `TestLimitsAndRefundRules`(D9), `TestLedgerEventsNoSecrets`(`provider_change`·`project_create` 이벤트: 행위자·action·subject·ok·code만), `TestRequireWriteAndCSRF`, `TestOrchdWorksWithBrokerDown`(확장), `TestNoBrokerImportInOrchd`; 복제: `TestCloneUsesFixedSafeOptions`(실행되는 명령줄과 환경을 검사하는 이음새: `-c protocol.allow=never`, `protocol.ssh.allow=always`, `core.hooksPath=/dev/null`, `--no-recurse-submodules`, `--no-local`, 고정 `GIT_SSH_COMMAND`, `GIT_*` 정리), `TestCloneRefusesUnpinnedHost`(호스트 키 파일에 없으면 시도 안 함), `TestCloneRefusesExistingPathAndSymlink`, `TestCloneOnlyUnderRepoRootsOneLevel`, `TestCloneCleansOnlyItsOwnPartialDir`, `TestCloneRemoteOnlyFromRegistry`(요청 본문의 URL은 무시), `TestCloneRealGitLocalFixture`(로컬 베어 저장소를 `file://`이 아닌 **허용된 경로**로 흉내 낼 수 없으므로, 이음새로 원격을 로컬 경로로 바꿔 실제 `git clone` 결과(HEAD, 파일)와 훅 비활성을 확인: 이 시험 전용 이음새는 프로덕션 코드 경로에서 접근 불가임을 정적 검사로 확인), 게시: `TestPublishResolverOnlyForUnlistedRepos`, `TestResolverRequiresPinnedHost`, `TestResolverBuildsFromRegistryOnly`, `TestResolverCacheInvalidation`, `TestStaticRemotesUnchanged`(W2·W3 게시 시험 그대로), `TestPublishFailsClosedWhenBrokerDown`(해석기가 없으면 정적 원격만), 정적 `TestResolverNotWiredFromRequestData`.
- [ ] **Step 2: 실패 확인** — `go test ./internal/orch/... -count=1 -run 'Provider|CreateProject|Clone|Resolver'`(api 패키지는 약 3분).
- [ ] **Step 3: 구현** — 복제는 `os/exec`로 `git`을 직접 실행하고(셸 없음), 인자를 코드가 조립한다. 게시 원격 해석은 `publish`에 선택 필드로 더하고 기존 `remoteFor`는 정적 → 해석기 순으로 본다.
- [ ] **Step 4: 통과 확인** — `-race`, 기존 orchd·publish 시험 의미 변경 없음.
- [ ] **Step 5: 커밋** — `feat(api): provider and project-creation endpoints, hardened clone and registry-driven publish remotes`.

---

### Task 5 (C5): 웹 — 제공자 화면과 새 프로젝트 만들기

**파일:**
- 만들기: `internal/orch/web/static/views/providers.js`, 시험 `internal/orch/web/test/providers.test.js`
- 수정: `static/views/projects.js`("새 프로젝트 만들기" 흐름), `static/shell.js`·`router.js`·`app.js`(메뉴), `static/webauthn.js`(필요 시 메시지 정규식), `style.css`, `internal/orch/api/web_fixtures_test.go`(실제 Go 구조체에서 픽스처 생성), `test/fake-ctx.js`

**인터페이스:**
- 소비: C4 엔드포인트, W3a·W3b의 `computeChange`·`signRegistryChange`·`guard.js`·`findHostile`·`noRetry429`.
- 생산: `#/providers` 화면, 프로젝트 화면의 생성 흐름, 변경 해시 브라우저 재계산(새 action 4개, Go 고정 벡터), 배포 키 지문 계산(`SHA-256` of the public key blob, Go와 같은 규칙).

- [ ] **Step 1: 실패하는 시험(JS, 이름 그대로)** — `TestChangeVectorsMatchGoForNewActions`, `TestProvidersListRendersAsText`(악성 값 inert), `TestAddProviderFlow`(선택 가능한 토큰은 W3b `tokens`의 `gitlab` 종류 `usable` 행만, 주소 사전 검증), `TestDeleteProviderDisabledWhenInUse`, `TestCreateGroupConfirmationShowsEveryField`, `TestCreateProjectConfirmationShowsEveryField`(제공자·그룹 경로·이름·템플릿·AI 전송·게시 권한·배포 키 지문과 `.pub` 앞부분), `TestSignOnlyWhenChangeMatches`(서버 `change`·메시지 본문·`deploy_key_sha256`이 브라우저 재계산과 같을 때만), `TestNamePrevalidation`(이름·그룹 경로·설명 규칙을 서명 전에: 서명 낭비 방지), `TestProgressStepsRenderedFromFixedStatuses`(고정 단계 문구만, 서버 문장 미표시), `TestCreatePartialShowsOrphanWithManualCleanupNotice`(프로젝트 경로·ID는 텍스트, "이 도구는 삭제하지 않습니다"), `TestCloneRetryButtonNoSignature`, `TestAllNewErrorCodesFixedTexts`(C3·C4의 모든 새 코드), `TestOutcomeUnknownReloadsListsNoAutoRetry`, `TestSubmitOnceNoDoubleSign`, `TestNoStorageNoNavigateTrap`(fake-ctx에 `navigate` 없음, 사용한 `ctx.*`가 실제 `app.js`에 있음), `TestNoPasswordLikeMarkup`(W3b 정적 금지 규칙이 새 파일도 덮음), `TestTokenSelectDoesNotShowValues`. `TestConstantsAgreeWithWeb`가 새 상수(이름·경로 정규식, 템플릿·게시 enum, 길이)를 비교하도록 확장되고 **웹 파일이 있어야 통과**.
- [ ] **Step 2: 실패 확인** — `node --test "internal/orch/web/test/providers.test.js"`.
- [ ] **Step 3: 구현** — W3a `projects.js`·W3b `tokens.js`의 패턴을 따른다. 서버가 준 문장·오류는 모두 `el()` 텍스트 노드.
- [ ] **Step 4: 시각 확인** — `ego-browser skill`을 먼저 읽고 개발용 픽스처 서버(`web/dev_test.go`, `ORCH_WEB_DEV`)에 제공자·그룹·생성 성공·`create_partial`·복제 재시도 픽스처를 추가해 1280px·390px을 보고 **본 것만** 쓴다. **실제 앱 경로(`app.js`)**로 끝까지 걷는다. 패스키는 대역이라고 쓴다. 브라우저의 비밀번호·자동 완성 팝업은 이 화면에 비밀 입력칸이 없으므로 해당 없음(제공자 화면은 토큰 값을 입력하지 않는다)이라고 쓴다.
- [ ] **Step 5: 변이 확인(복사본)** — 확인 없이 `apply` 전송, `change` 비교 제거, 배포 키 해시 비교 제거, 서버 문장을 속성에 넣음, 이름 사전 검증 제거 → 각각 시험 실패.
- [ ] **Step 6: 통과 확인 + 커밋** — `go test ./internal/orch/web/... ./internal/orch/api/... -race`, node 전체, `TestConstantsAgreeWithWeb` PASS(SKIP 없음); `feat(web): providers and new project creation screens`.

---

### Task 6 (C6): 적대 시험, 결과 문서, 컷오버 런북

**파일:** 만들기 `internal/broker/adversarial_w3c_test.go`, `internal/orch/api/adversarial_w3c_test.go`, `docs/25-RESULTS-webconsole-W3c.md`; 수정 `docs/10-OPERATIONS-orchestrator.md`(W3c 컷오버 절 14장), `docs/19-DESIGN-webconsole-W3-projects.md`(상태·W3c 반영), `infra/README.md`

- [ ] **적대 시험(모두 거부/무효):** broker: `TestAdminTokenNeverLeavesBroker`(모든 op·오류·패닉·로그에서 canary 검색), `TestEveryGitLabRequestIsAllowlisted`(모든 흐름의 요청 집합이 허용 목록의 부분집합, 삭제·병합 없음), `TestSSRFProviderAddressesRefused`, `TestRedirectToOtherHostNotFollowedAndTokenNotSent`, `TestProviderBaseURLChangeNeedsSignatureAndShowsInChange`, `TestSignatureOverDifferentProjectFieldsRefused`(필드 하나만 바뀐 `create_project` 서명 재사용 → 거부, GitLab 요청 없음), `TestCrossActionSignaturesRefused`(`set_provider`↔`create_project`↔`set_token`↔`register_project` 등 모든 쌍), `TestDeployKeySwapRefused`, `TestExistingGitLabProjectNeverAdopted`, `TestPartialFailureNeverDeletes`, `TestConcurrentSameNameNoDoubleCreate`, `TestRegistryV2BrokenFailsClosedAndDoesNotWidenAIContext`, `TestStatusAndProjectsNoSecrets`, `TestCreateProjectBrokerDoesNotStatPaths`. orchd: `TestCloneNeverUsesRequestSuppliedURL`, `TestCloneOptionInjectionRefused`, `TestCloneHookNotExecuted`(훅이 든 원격 저장소를 복제해도 훅이 실행되지 않음), `TestResolverNeverTrustsRequestData`, `TestApplyForgedFieldsRefusedBeforeBroker`, `TestProposeNeverWritesBroker`, `TestProvidersLedgerNoSecrets`, `TestOrchdWorksWithBrokerDown`, `TestNoBrokerImportInOrchd`.
- [ ] **변이 확인(저장소 복사본에서만):** ① 서명 검증 제거 ② `change` 일치 제거 ③ 허용 목록 검사 제거(`do`가 아무 경로나 보냄) ④ 주소 검증의 IP 거부 제거 ⑤ 리다이렉트 허용 ⑥ 기존 프로젝트 인수(이름 충돌 무시) ⑦ 부분 실패 때 고아 기록 안 함 ⑧ `deploy_key_sha256` 비교 제거 ⑨ 복제 옵션에서 `protocol.allow` 제거 ⑩ 훅 비활성 제거 ⑪ 해석기가 호스트 고정 검사를 건너뜀 ⑫ 요청 본문의 URL을 복제에 사용 ⑬ 웹 `change` 비교 제거. 각각 어느 시험이 실패했는지 표로 기록(안 걸리면 시험을 추가하고 다시).
- [ ] **결과 문서(`docs/25`):** `docs/23` 구조: 만든 것, 시험 목록(실제 개수), 변이 표, 설계서와 달라진 곳, **배포 현실 표**(경로·모드·소유자·실행 사용자·샌드박스), **실서버에서 확인하지 못한 것**(전부: 실제 GitLab 19.0.8에서 `POST /groups`·`POST /projects`·빈 저장소 첫 커밋 API·보호 브랜치·배포 키 API의 실제 동작과 관리자 토큰 권한, orchd가 `/home/orch/repos`에 복제할 수 있는지(권한), 배포 키로 `git clone`/`push`가 새 프로젝트에 되는지(쓰기 키·읽기 키), `ssh`·`git` 실행 파일과 `known_hosts_gitlab`, broker가 GitLab HTTP에 도달하는지(nft), 등록부 v2 마이그레이션과 롤백 영향, 템플릿이 실제 게이트에서 통과하는지, 키보드·스크린 리더), 사용자 결정 목록.
- [ ] **컷오버 런북(W3c, `docs/10` 14장):** W3b 런북 양식(순서 고정, 단계마다 백업(`/root/backup-w3c`, root 0700), `[사용자]`/`[컨트롤러]`/`[주의]`/`[되돌림 가능]`). 0단계 사전 점검(컨트롤러가 서버에서 읽기 전용으로): 정책에 `registered` 클래스(없으면 관리자 한 번 추가 — 사용자 결정, 정책 해시 변경, 실행 중 작업 없음 확인), `/home/orch/repos` 쓰기 가능(`orch` 계정, orchd 샌드박스 `ReadWritePaths`), 배포 키·`.pub`·`known_hosts_gitlab`의 호스트(GitLab ssh 주소가 거기 있는지), `git`·`ssh` 실행 파일, GitLab HTTP·SSH 도달(nft 허용 주소인지), 샌드박스 안 `-check`(제공자 설정 점검 포함). **사용자 사전 작업**: GitLab **관리자 토큰**을 만들어 W3b 토큰 화면에서 `gitlab` 종류로 등록(값은 사용자가 직접 입력, 대화에 붙이지 않음, 만료일 지정), 새 프로젝트를 만들 그룹 결정(새 전용 그룹 권장). 1단계 broker 갱신(정지 → 바이너리 → 샌드박스 `-check` → 시작, **등록부가 v2로 마이그레이션됨, 옛 broker는 v2를 깨짐으로 봄 → 롤백 시 등록부를 `/root/backup-w3c`에서 복원해야 함**), 2단계 orchd 갱신, 3단계 웹 확인: 제공자 등록(실제 서명), 그룹 목록(읽기, GitLab 호출), **실제 그룹·프로젝트를 만드는 것은 사용자 결정**(만들어지면 사내 GitLab에 실제 리소스가 남고 우리 도구는 지우지 못한다: 시험용 이름을 쓰고 확인 뒤 사용자가 GitLab에서 직접 지움), 복제·게시 확인(작은 시험 작업 제출은 사용자 결정), 4단계 롤백(바이너리 + 등록부 복원, GitLab에 만든 것은 남음). 브라우저 확인 항목(해당 시): 제공자 화면에는 비밀 입력칸이 없다.
- [ ] **`go test ./... -count=1 -race` 전체 + node 테스트 + `TestConstantsAgreeWithWeb -v`(PASS, SKIP 없음)**, 커밋 `test: adversarial suite for W3c`, `docs: W3c results and cutover runbook`.

---

## 자체 검토 기록
- **설계서 대비 범위:** 4.6(제공자)·5.3(새 프로젝트 생성: 그룹 선택·생성, 첫 커밋을 broker가 커밋 API로, 배포 키, 고아 처리)·6장(관리자 토큰 위협: API 허용 목록, 삭제 코드 없음, 호출 한도, 원장)·7장(서명 메시지에 배포 키 해시) → C1~C6. **의도적으로 뺀 것:** 기존 프로젝트(`sample-app`)를 제공자에 연결하는 op(W3d에서 MR·병합과 함께), MR 생성 경로의 제공자 전환(W3d), 조건부 자동 병합(W3d), GitHub(W3e), GitLab 프로젝트·그룹 삭제·정리(사용자가 GitLab에서).
- **설계서와 달라진 점(이 계획이 정한 것):** 복제 상태를 등록부가 아니라 orchd가 파일 시스템으로 계산(등록부의 모든 쓰기를 서명으로 유지하기 위해 서명 없는 `mark_ready` op를 만들지 않음); 게시 원격을 등록부에서 동적으로 해석하는 `publish.RemoteResolver`(기존엔 시작 플래그로만 설정 가능했음: 새 프로젝트마다 플래그를 고칠 수 없으므로); 등록부 버전 2(롤백 시 옛 broker는 v2를 깨짐으로 취급: 의도적, 런북에 경고); 부분 실패 재개 없음(고아 기록 후 사용자가 정리).
- **이름 일관성:** op `providers`/`list_groups`/`set_provider`/`delete_provider`/`create_group`/`create_project`, 새 오류 코드(`provider_unknown`, `provider_exists`, `provider_in_use`, `bad_url`, `token_ref_unknown`, `provider_unreachable`, `gitlab_refused`, `group_unknown`, `group_exists`, `project_name_taken`, `create_partial`, `deploy_key_mismatch`, `template_unknown`, `policy_class_missing`, `host_not_pinned`, `already_cloned`), 필드 `deploy_key_sha256`·`template`·`publish`, 등록부 `providers`·`orphans`·`provider`·`gitlab`이 C1~C6에서 같다.
- **미결(구현 중 구현자가 정하고 보고서에 기록):** 첫 커밋 API(빈 저장소)가 안 될 때의 대체(파일 API), `checks`가 빈 템플릿이 허용되는지, 보호 브랜치 갱신 호출의 정확한 모양(공식 문서 확인), 그룹 목록 호출의 쿼리 파라미터, 고아 기록 정리 방식, `create_project` 응답 모양의 세부, 복제 시험의 로컬 원격 이음새 방식.
- **사용자 확인이 필요한 값:** 컷오버에서만: GitLab **관리자 토큰**(값은 사용자가 웹으로 직접), 새 프로젝트를 만들 **그룹**(새 전용 그룹 권장), **실제 리소스를 만들지 여부와 이름**, 정책에 `registered` 클래스를 추가하는 것. 구현·시험에는 필요 없다.
