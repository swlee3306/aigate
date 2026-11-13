# W3 설계: 프로젝트 등록·생성, 토큰 관리, 조건부 자동 병합

상태: **W3a(프로젝트 등록부와 서명된 변경)와 W3b(토큰 메뉴·만료 경고·OpenAI 키 교체·배포 키 공개 키 조회)는 컷오버 완료(2026-11-10, 실서버 orch-vm; W3b에서 실제 패스키 `set_token`·키 교체는 하지 않았다, 23번 문서 11장). W3c(제공자 설정·새 그룹·새 프로젝트 생성·복제·게시 원격 해석)도 컷오버 완료, 실서버에서 운영 중이다.** 결과는 `25-RESULTS-webconsole-W3c.md`, 컷오버 절차는 `10-OPERATIONS-orchestrator.md` 14장, 계획은 `24-PLAN-webconsole-W3c.md`. W3b는 `23`·같은 문서 13장·`22`, W3a는 `21`·12장·`20`. **W3e(GitHub 제공자·새 GitHub 저장소 생성·기존 저장소 연결·원격 연결·저장소별 배포 키)도 2026-11-12 실서버에 컷오버되어 운영 중이다**(main `3830546`, 등록부 v3, 제공자 `example-group-gitlab`·`swlee3306-github`). 실서버에서 본 것과 못 본 것은 `27-RESULTS-webconsole-W3e.md` 6장·9장, 절차는 `10-OPERATIONS-orchestrator.md` 15장, 계획은 `26-PLAN-webconsole-W3e.md`. **W3f(프로젝트 삭제: 서버의 복제본·등록·키 지우기, 이 도구가 만든 원격 저장소 지우기, 연결한 저장소에서 배포 키만 떼기, 고아 기록 지우기)는 2026-11-13 실서버에 컷오버되어 운영 중이다**(main `263d0e1`, 등록부 v4; 실서버에서 본 것과 못 본 것은 `29-RESULTS-webconsole-W3f.md` 9장). 컷오버 절차는 `10-OPERATIONS-orchestrator.md` 16장, 계획은 `28-PLAN-webconsole-W3f.md`. **W3g(삭제를 서명 한 번으로: 복제본·등록을 지울 때 배포 키도 함께 떼기(`remove_project.detach_key`), 원격 삭제와 복제본·등록을 같은 서명으로(`also_local`))는 브랜치 `impl/webconsole-w3g`에 구현 완료, 배포하지 않음(2026-11-13).** 실제 서버·실제 GitHub·GitLab에는 아무것도 하지 않았다. 결과는 `31-RESULTS-webconsole-W3g.md`, 컷오버 절차는 `10-OPERATIONS-orchestrator.md` 17장, 계획은 `30-PLAN-webconsole-W3g.md`. 새 DELETE 호출 행과 등록부 버전 변경은 없다(W3g는 이후 2026-11-13 실서버에 컷오버됐다: 31번 9장). **W3h(배포 키 떼기를 등록 때 서명된 키 해시에 묶기: 등록부 v5의 `remote.key_sha256`, 해시 없는 GitLab 행의 떼기 거부, orchd의 HTTP 403 거부 진단 로그)는 브랜치 `impl/webconsole-w3h`에 구현 완료, 배포하지 않음(2026-11-14).** 실제 서버·실제 GitHub·GitLab에는 아무것도 하지 않았다. 결과는 `33-RESULTS-webconsole-W3h.md`, 컷오버 절차는 `10-OPERATIONS-orchestrator.md` 18장, 계획은 `32-PLAN-webconsole-W3h.md`. 서명 필드·변경 해시·허용 목록은 바뀌지 않는다. **W3f는 이 설계의 "삭제 코드가 아예 없다"는 원칙(아래 비범위·4.5·위협 표의 "삭제 코드 부재")을 의도적으로 부분 해제한다**: 서명·허용 목록·출처(`origin`)·저장소 번호 확인으로 가둔 삭제만 있고(그룹·조직·토큰·협업자 삭제는 여전히 코드에 없다), 사용자가 삭제 권한을 이미 가진 일반 토큰을 그대로 쓰기로 했다(별도 삭제 토큰 없음). 아래 본문은 W3f 이전의 설계 기록이다. W3d(MR·조건부 자동 병합)는 아직 계획·구현이 없다. 아래 본문은 초안 v0.4(2026-11-09, 사용자 1~3차 검토 반영, GitLab 서버 설정·GitHub 공식 문서 확인 반영)의 설계이고, W3a가 설계와 달라진 곳은 21번 문서 4장에, W3b가 달라진 곳은 23번 문서 5장에 있다. 이 문서의 "확인함"은 이 저장소와 orch-vm에서 직접 읽은 것이고, "가정"과 "추정"은 확인하지 못한 것이다.

> **경고(W3b 이후 설계에 필수):** W3a의 `set_flag` 서명은 **프로젝트 id를 묶고 경로는 묶지 않는다.** 삭제·이름 변경·재등록 op가 없는 지금은 안전하지만, 그런 op를 만들 때는 **경로 또는 등록부 세대를 `change`에 넣어야** 한다(그러지 않으면 예전에 서명된 AI 켜기가 다른 경로를 가진 같은 id에 재사용될 수 있다). 5분 신선도와 요청 ID 1회성은 이를 줄여 줄 뿐 설계의 대체물이 아니다.
> **W3b가 설계와 달라진 곳(요약, 상세는 23번 문서 5장):** ① 값의 솔트 해시가 요청 ID에 묶이므로 서명 흐름이 `begin` → `message` → 서명 → `apply` 3요청이다(값은 `apply`에만 실린다). ② `set_token`은 최상위 인자 `value_hash`를 더 가진다. ③ `openai` 키는 설정 파일 대신 broker 상태 폴더의 저장소(`secrets/openai`)가 우선하고, 설정 파일 키는 폴백으로 남는다(`/etc`는 broker가 쓸 수 없다). ④ 값 입력칸은 `type=password`가 아니라 `type=text` + CSS 가림이다(브라우저의 "비밀번호 저장" 제안을 피하려는 선택, 복사하면 실제 글자가 나올 수 있다). ⑤ `gitlab`·`github` 토큰은 W3c 전까지 소비자가 없다(저장만). ⑥ 만료일은 UTC로 세고 사용자가 적은 값이며 GitLab·GitHub에 확인하지 않는다.
> **W3c가 설계와 달라진 곳(요약, 상세는 25번 문서 5장):** ① 빈 저장소의 첫 커밋은 커밋 API가 아니라 파일 API로 파일마다 올린다(원자적이지 않음). ② 복제 상태는 등록부가 아니라 orchd가 파일 시스템으로 계산한다(서명 없는 쓰기 op를 만들지 않으려고). ③ 게시 원격은 등록부에서 동적으로 해석한다(`publish.RemoteResolver`, 읽지 못하면 "알 수 없음 = 거부"). ④ 등록부 버전 2: 옛 broker는 이를 깨짐으로 보므로 롤백은 등록부 복원이 필요하다. ⑤ 부분 실패는 재개하지 않고 고아 기록만 남긴다. 이 도구는 GitLab의 그룹·프로젝트를 지우지 못한다.
> **W3e가 설계와 달라진 곳(요약, 상세는 27번 문서 5장):** ① 등록부 버전 3: 프로젝트의 연결이 `remote:{path,id}`로 일반화되고 broker→orchd `projects` 응답의 필드 이름이 `gitlab`에서 `remote`로 바뀐다. W3c broker는 버전 3을 깨짐으로 보므로 롤백은 등록부 복원이 필요하고, **옛 orchd + 새 broker는 W3c 프로젝트를 기본 원격으로 게시할 수 있어서 런북은 orchd를 먼저 멈추고 broker → orchd 순서로 갱신한다.** ② GitHub는 저장소마다 새 배포 키(orchd가 Go 표준 라이브러리로 생성, 제안 때는 임시 폴더에만, 서명된 해시가 맞을 때만 `ssh/keys/<id>/`로 승격). ③ 공개/비공개는 서명되는 필드이고 기본값이 없다. 보호 브랜치는 방금 만든 저장소에만 쓰고, 무료 요금제의 비공개는 항상 `create_partial`(`protect`). ④ 기존 저장소 연결·원격 연결은 저장소를 만들거나 지우거나 설정을 바꾸지 않고 배포 키 하나만 붙이며, 고아 기록에 `origin`(`created`|`linked`)이 있다. 프로젝트 id는 원격 저장소 이름과 같아야 하고 정적 `--git-remote-for` 프로젝트에는 연결할 수 없다. 이 도구는 GitHub 저장소와 배포 키를 지우지 못한다.
앞선 문서: `11`(W1 인증·서명), `14`(W2·`orch-broker`), `17`·`18`(W2b 계획·결과·컷오버).

## 1. 배경과 사용자 결정 (2026-11-09)
W2b 컷오버 중 MR을 켜려다 사용자가 이렇게 정했다: 프로젝트를 만들 때마다 설정 파일을 손으로 고치고 토큰을 스크립트로 넣는 대신, **웹 메뉴로 프로젝트를 추가·생성하고 토큰을 넣고, GitLab 게시 여부를 묻고, 조건이 맞으면 자동으로 병합**하게 하자. Orca의 "신규 프로젝트 추가"와 같은 개념(저장소 하나를 시스템에 등록하는 메뉴)이라고 했다. 이 비교는 사용자의 설명에 근거한 이해이고 Orca의 실제 동작은 확인하지 않았다.

문답으로 확정한 것:
| 항목 | 결정 |
|---|---|
| 범위 | **빈 새 프로젝트 생성 + GitLab 프로젝트 생성**까지(서버에 이미 있는 저장소 등록과 GitLab 주소 clone 등록은 그 안에 포함된다고 본다, 5.2) |
| 자동 병합 | **조건부**: 판정 통과 + 프로젝트별로 켜 둔 경우만. 기본은 꺼짐 |
| 토큰 입력 | **웹 폼 → broker만 저장**(orchd는 값을 저장·기록하지 않는다) |
| GitLab 프로젝트 생성 권한 | 큰 권한 토큰 **하나를 broker에 저장**. 새 프로젝트는 **기존 그룹 안에 추가하거나 새 그룹을 만들 수 있게** 한다. 사용자가 **전체 권한을 줄 수 있는 토큰**을 주기로 했다(2026-11-09). 이 위험은 사용자가 받아들였고 6장의 완화책을 건다 |
| 변경 승인 | 등록부를 바꾸는 **모든 변경에 서명 승인**(웹 패스키 또는 SSH 서명) |
| 등록부 소유 | **A안: broker가 등록부를 소유**하고 서명 검증도 broker가 한다 |
| 지금 MR | 이 기능이 생긴 뒤 한 번에 켠다(현재 서버는 W2b 상태 그대로, MR 꺼짐) |
| 연결 대상 | **GitLab과 GitHub를 자유롭게 설정**: 여러 서버·여러 계정을 "제공자(provider)"로 등록한다(2026-11-09 2차 검토). 처음 구현은 GitLab만이고 GitHub는 같은 구조에 어댑터를 더한다(10장) |
| 큰 권한 토큰 종류 | 사용자가 **관리자 토큰**을 주기로 했다(내부 서버에서만 쓰므로). 이 위험은 사용자가 받아들였다(6장) |

## 2. 범위와 비범위
**범위(W3):** 프로젝트 등록부, 프로젝트 추가(기존 저장소)·생성(새 저장소 + GitLab 프로젝트), 토큰 등록·교체·삭제 메뉴, 프로젝트별 설정(AI 전송 허용, GitLab 연결, 자동 병합), 조건부 자동 병합, 모든 변경의 서명 승인과 이력.
**비범위(W3f가 일부를 뒤집음):** 프로젝트·브랜치·MR **삭제**(W3f 전까지 broker에 삭제 기능을 아예 두지 않았다. W3f는 프로젝트·원격 저장소·배포 키·고아 기록 삭제만 넣고 브랜치·MR·그룹 삭제는 여전히 없다), 정책 등급(L0~L2) 편집, 사람이 아닌 서명자, 계획서·분해(W4·W5). (여러 서버·GitLab과 GitHub 지원은 2차 검토에서 **범위에 넣었다**, 4.6)

## 3. 현재 코드·서버 사실 (확인함)
- **GitLab 서버(19.0.8) 설정(2026-11-09 서버 읽기 전용 조회로 확인)**: 최상위 그룹은 `example-group` 하나, **관리자 계정 9명**, 새 사용자도 최상위 그룹을 만들 수 있음(`can_create_group: true`, 관리자는 항상 가능), 새 그룹 기본 공개 범위 비공개, **개인 액세스 토큰은 만료일 지정 필수**(최대 수명 제한 없음), **새 프로젝트 기본 브랜치 보호: push·병합 Maintainer 이상, 강제 push 금지, 개발자의 최초 push 불가**.
- `/etc/aigate/signers.json`은 `root:root 0644`, 폴더 `/etc/aigate`는 `root:root 0755`라서 **broker 계정(`orch-broker`)이 읽을 수 있다**(2026-11-09 서버에서 `test -r`/`test -x` 확인). `orch`도 읽지만 쓰지는 못한다.
- 작업 판정 값은 `rules.Category`(`ok`, `behavior_failure`, `check_weakened`, `verification_incomplete`)이고, **`ok`일 때만 작업이 게시 단계로 간다**(steps.go). 그래서 자동 병합의 "판정 통과"는 `verdict == ok`다.
- 저장소 하나를 추가하려면 지금은 이 파일들을 손으로 고쳐야 한다: 정책 `policy.yml`의 `classes`, `orchd` 유닛의 `--git-remote-for`, broker 설정의 `context_repos`와 `gitlab.projects`, 토큰 파일, 서비스 재시작(docs/10 11장).
- **`policy.yml`은 root 소유가 아니다**: `/home/orch/orchd/policy.yml`, `orch:orch 0600`(2026-11-09 서버 확인). 내용: `classes: {toy: L1, sample-app: L1}`, `allowlist: [push_branch]`, `repo_roots: [/home/orch/repos]`, `default_level: L0`. 정책이 작업 중간에 바뀌면 게시 시점에 정책 해시가 달라져 사람 승인으로 넘어간다(docs/10, 구현됨). 즉 `orch`가 정책을 바꿔도 게시 때 서명 승인이 걸린다.
- 반면 **"AI로 코드를 보내도 되는 저장소"(`context_repos`)와 GitLab 토큰은 root 소유 broker 설정/파일**이라 `orch`가 넓힐 수 없다. 이것이 W2b의 핵심 안전장치다.
- 서명 승인(W1): 서명자 목록 `/etc/aigate/signers.json`(root 소유 폴더·파일), 웹 패스키(WebAuthn ES256), CLI SSH 서명(네임스페이스 `aigate-approval`), 서명 메시지는 고정 형식(`aigate-approval-v1`, 정확히 64자리 hex 해시). 서명 검증 코드는 `internal/approval/signing`에 있다.
- GitLab `example-group/sample-collector`(id 1): 기본 브랜치 `main`, 보호됨(push·병합 모두 Maintainer, 강제 push 금지)(2026-11-09 서버 읽기로 확인). sample-app만 GitLab에 게시한다(Deploy Key `id_gitlab_deploy`). `toy`는 로컬 `remote.git`에만 게시한다.
- W2b 기능: broker(`refine`, `create_mr`, `status`), MR은 항상 Draft이고 병합 코드가 없다(그 보장을 이 W3가 **일부러 바꾼다**, 4.4).

## 4. 구조
### 4.1 한눈에
```
브라우저 ──(세션, 서명)──▶ orchd ──(unix 소켓)──▶ orch-broker ──▶ GitLab API / OpenAI
                           │  읽기 전용 조회          │  쓰기 전용: 등록부, 토큰
                           └─ 저장소(git) 작업         └─ /var/lib/orch-broker/registry.json
```
- **등록부(registry)**: broker 전용 폴더의 파일. 쓰는 것은 broker 프로세스뿐(`orch-broker` 소유 0600, `orch`는 읽지도 못한다). orchd는 broker의 `projects` 조회(읽기 전용, 비밀 없음)로 목록을 받는다.
- **토큰**: `/etc/orch-broker/` 아래가 아니라 broker 상태 폴더(`/var/lib/orch-broker/secrets/`, `orch-broker` 소유 0700, 파일 0400)에 broker가 쓴다. `/etc/orch-broker`는 계속 root 소유(설정 신뢰 뿌리)로 남는다. 기존 `openai.key`와 `gitlab.token` 파일 경로는 호환을 위해 읽는다.
- **orchd의 역할**: 웹 요청을 받아 검증·표시하고, 저장소(git) 작업(`init`, 첫 커밋, push)을 하고, 서명 승인 요청을 만들어 broker에 넘긴다. orchd는 등록부와 토큰을 쓸 수 없다.

### 4.2 broker가 서명을 직접 검증한다
orchd가 "서명 확인했다"고 말하는 것을 broker가 믿으면 `orch` 계정이 뚫렸을 때 등록부를 넓힐 수 있다. 그래서 변경 요청에는 **서명 원문이 따라오고 broker가 검증한다**.
- 서명 메시지: `aigate-approval-v1` 형식을 확장한 `aigate-registry-v1`(7장). 서명자 목록은 `/etc/aigate/signers.json`(broker가 읽기 전용으로 접근, **가정**: broker 계정이 읽을 수 있는 모드. 구현 전에 서버에서 모드 확인).
- 검증 코드는 `internal/approval/signing`을 가져다 쓴다(WebAuthn 검증은 origin·rpID·UP/UV·카운터). broker가 `internal/approval/signing`을 import하는 것은 orchd와 달리 허용한다(OpenAI 호출 코드가 아니라 순수 검증 코드이므로). 의존성 방향 시험을 둔다.
- **재사용 방지**: 메시지에 `issued`(5분 안)와 요청 ID(무작위 128비트)가 들어가고 broker가 사용한 요청 ID를 보관(최근 1시간)해 같은 서명을 다시 쓰지 못하게 한다. WebAuthn 서명 카운터는 broker가 자격별로 따로 기록한다(**미결정**: orchd의 카운터 상태와 어떻게 합칠지, 11장).
- 서명 없는 요청, 서명자가 목록에 없는 요청, 메시지와 요청 내용이 다른 요청은 모두 고정 코드로 거부한다.

### 4.3 등록부의 모양 (v1, broker가 관리)
```json
{ "version": 1,
  "gitlab": { "base_url": "http://100.64.0.10:18082", "group": "<그룹 경로>", "target_branch": "main" },
  "projects": [
    { "id": "toy", "path": "/home/orch/repos/toy", "kind": "toy",
      "ai_context": true,
      "gitlab": null,
      "auto_merge": { "enabled": false } },
    { "id": "sample-app", "path": "/home/orch/repos/sample-app", "kind": "sample-app",
      "ai_context": true,
      "gitlab": { "project": "example-group/sample-collector", "project_id": 1 },
      "auto_merge": { "enabled": false } } ],
  "auto_merge_global": { "enabled": false, "max_per_day": 10 } }
```
- `kind`는 orchd 정책의 `classes` 이름과 같다. **새로 등록하는 프로젝트의 `kind`는 정책에 있는 `registered`**(`level: L1`, 아래 5.3)로 시작한다. 등급을 올리는 것은 이 메뉴의 범위가 아니다.
- 지금의 broker 설정 `context_repos`와 `gitlab.projects`는 이 등록부로 **이전**한다(마이그레이션은 W3a에서 `broker.json`의 옛 항목을 한 번 읽어 등록부로 옮기고, 이후 등록부가 우선한다. 구현 시 남기는 옛 항목의 처리는 미결정).

### 4.4 자동 병합(조건부)과 "broker는 병합하지 않는다" 규칙의 변경
W2b의 broker는 병합 코드가 아예 없었다. W3는 이를 **좁게** 바꾼다.
- 병합은 **MR을 방금 만든 같은 작업에 대해서만, 아래 조건이 모두 참일 때만** 한다:
  1. 전역 `auto_merge_global.enabled`와 그 프로젝트의 `auto_merge.enabled`가 둘 다 켜져 있고(둘 다 서명 승인으로 켠 것), 오늘 병합 수가 `max_per_day` 미만.
  2. 작업이 `done`이고 판정이 통과이며(orchd의 verdict), 게시된 `head` 커밋이 MR의 head SHA와 같다.
  3. MR이 Draft가 아니게 된 뒤(broker가 "Draft 해제"를 하고) 병합 요청의 `sha` 인자를 `head`로 지정해 GitLab이 **다른 커밋이 올라가 있으면 병합을 거부**하게 한다.
  4. 원본 브랜치는 지우지 않는다(`should_remove_source_branch: false`).
- **새 권한**: 병합과 프로젝트 생성이 필요하므로 그룹 단위 큰 권한 토큰(Maintainer, 범위 `api`, 6장)을 쓴다. `main`의 보호 규칙(push·병합 Maintainer)은 그대로 두고 **그 토큰의 Maintainer 권한으로만** 병합이 된다. 즉 GitLab 쪽 보호 규칙을 우회하는 것이 아니다.
- broker의 `merge_mr`는 위 조건 중 **등록부로 확인할 수 있는 것**(스위치 둘, 일일 상한, 프로젝트·브랜치 형식, `sha` 고정)을 직접 검사한다. 판정 통과 여부만은 broker가 알 수 없어 orchd가 알려 준 `verdict_ok`를 믿어야 하므로, 이 한 가지는 orchd를 신뢰한다. `orch`가 뚫렸을 때의 방어선은 서명으로 켠 프로젝트 스위치, 일일 상한, `sha` 고정이다.
- **위험(솔직히)**: 판정(verdict)이 틀렸거나 에이전트가 판정을 속였다면 사람 검토 없이 `main`에 들어간다. 그래서 기본은 꺼짐이고, 켜는 것은 서명이 필요하며, 하루 상한이 있고, 언제든 전역 스위치로 끈다(꺼도 이미 병합된 것은 못 되돌린다).

### 4.6 제공자(provider): 연결할 GitLab·GitHub를 자유롭게 설정
등록부에 제공자 목록을 두고 프로젝트가 하나를 가리킨다.
```json
"providers": [
  { "id": "gl-office", "kind": "gitlab", "base_url": "http://100.64.0.10:18082", "token_ref": "gl-office" },
  { "id": "gh-main",   "kind": "github", "api_url": "https://api.github.com", "web_url": "https://github.com", "token_ref": "gh-main" } ]
```
- 제공자 추가·변경·삭제·토큰 교체는 **모두 서명 승인**이다(`aigate-registry-v1`의 `action`에 `set_provider` 추가). 서버 주소를 사용자가 자유롭게 정할 수 있게 하는 대신, **주소는 서명된 변경으로만 바뀌고 요청 하나로 바뀌지 않는다.**
- **주소 검증**(broker): `http`/`https`만, 사용자 정보·쿼리·조각 금지, 호스트 이름은 정규화해 비교, **리다이렉트를 따르지 않음**, 연결 대상 IP가 **루프백·링크 로컬(`169.254.0.0/16`, 메타데이터 주소 포함)**이면 거부(사설망 IP와 tailnet은 허용: 사내 GitLab이 그렇다). OpenAI 주소는 계속 코드 상수로 고정하고 이 목록에서 바꿀 수 없다.
- **제공자별 API 호출 허용 목록**을 따로 둔다(4.5의 목록은 GitLab 몫). 허용 목록 밖 호출을 만드는 코드 경로는 없고 시험이 가짜 서버에서 모든 요청을 확인한다.
- **GitHub 쪽(공식 문서 `docs.github.com`로 2026-11-09 확인함. 요약 모델이 읽은 것이라 정확한 문구는 구현 때 다시 읽는다. API를 직접 호출해 본 것은 아니다)**:
  - 저장소 생성은 `POST /user/repos`, `POST /orgs/{org}/repos`(조직 구성원이어야 함). 클래식 토큰은 공개는 `public_repo`나 `repo`, 비공개는 `repo` 범위. 빈 저장소가 기본(`auto_init` 기본값 거짓).
  - **github.com에는 조직 생성 REST 엔드포인트가 문서에 없다**(`/admin/organizations`는 Enterprise Server 전용). 따라서 GitHub는 "새 그룹 만들기"를 지원하지 않고 **기존 조직·사용자 안에만** 저장소를 만든다.
  - PR은 `POST /repos/{o}/{r}/pulls`의 `draft` 인자로 Draft 생성이 된다. **그러나 Draft를 "준비됨"으로 바꾸는 REST 엔드포인트는 문서에서 찾지 못했다**(PR 수정 API에 `draft` 필드가 없다). 그래서 GitHub에서는 **자동 병합을 켠 프로젝트는 Draft 없이 일반 PR로 만들고**, 자동 병합을 끈 프로젝트만 Draft로 만든다(GraphQL 변이 `markPullRequestReadyForReview`는 이번 설계에서 쓰지 않는다. 존재와 동작은 이 조사에서 확인하지 못했다). GitHub가 Draft PR 병합을 막는 것은 추정이다.
  - 병합은 `PUT …/pulls/{n}/merge`: `sha`는 "PR head가 일치해야 병합 허용"이고 **불일치면 409**, `merge_method`는 `merge|squash|rebase`. 필요한 세분화 권한은 Contents(write).
  - 배포 키는 `POST …/keys`의 `read_only: false`로 쓰기 가능(문서의 경고: 쓰기 배포 키는 admin 협업자와 같은 행동이 가능). 세분화 권한 Administration(write).
  - 브랜치 보호 `PUT …/branches/{b}/protection`은 저장소 admin 권한 필요. **공개 저장소는 무료 요금제에서도 가능, 비공개는 유료 요금제 필요**(문서 기준). 룰셋(`/repos/{o}/{r}/rulesets`)은 대안이지만 무료 요금제 가능 여부는 문서에서 확인하지 못했다.
  - **빈 저장소의 첫 커밋**: 콘텐츠 API(`PUT …/contents/{path}`)와 Git 데이터 API가 빈 저장소에서 동작하는지는 **문서에서 찾지 못했다**(Git 커밋 생성 API는 `parents`를 비우면 루트 커밋을 만든다고 문서에 있다). W3e 구현 전에 실험한다.
  - 토큰: 세분화 토큰은 "선택한 저장소만"으로 제한할 수 있고(문서), 한 리소스 소유자(사용자 또는 조직 하나)에만 묶이며 **새 저장소를 만드는 데 쓸 수 있는지는 문서로 확인하지 못했다**. 클래식 토큰이 위 동작 전체를 덮는다는 명시적 문서는 찾지 못했다(일부 엔드포인트의 클래식 범위가 문서에 없음). → **GitHub 토큰 종류와 필요한 범위는 W3e에서 실제로 시험해 정한다.**
  - 사용자가 "GitHub 쪽은 비공개 저장소가 아니다"라고 했다(2026-11-09). 공개 저장소를 가정하면 브랜치 보호가 무료 요금제에서 가능하고 AI 전송의 기밀 부담이 작지만, **AI 전송 허용(`ai_context`)은 여전히 프로젝트별 명시 설정**으로 둔다(공개 여부를 자동으로 믿지 않는다).
- 구현 순서: 등록부 구조는 처음부터 `providers`를 갖고(W3a), **GitLab 어댑터만 먼저** 만들며, GitHub 어댑터는 W3e로 따로 한다(10장).

### 4.5 broker의 새 작업(op)
| op | 하는 일 | 서명 | 비밀 |
|---|---|---|---|
| `projects` | 등록부 읽기(경로·종류·AI 전송·GitLab 프로젝트 이름·자동 병합 여부) | 불필요 | 없음 |
| `register_project` | 기존 저장소 등록(AI 전송, GitLab 연결 설정) | 필요 | — |
| `list_groups` | 토큰이 볼 수 있는 GitLab 그룹 목록(경로·이름만) | 불필요(읽기, 비밀 없음) | 토큰 사용 |
| `create_group` | GitLab 그룹 새로 만들기(이름·경로·상위 그룹 선택) | 필요 | 토큰 사용 |
| `create_project` | 선택한 기존 그룹 또는 방금 만든 그룹 안에 GitLab 프로젝트 생성 + 등록 | 필요 | 토큰 사용 |
| `set_token` | 토큰 저장·교체(OpenAI 키, GitLab 그룹 토큰) | 필요 | 값은 요청에 들어가고 broker만 저장 |
| `delete_token` | 토큰 파일 삭제(shred) | 필요 | — |
| `set_flag` | 프로젝트별 `ai_context`, `auto_merge`, 전역 스위치 | 필요 | — |
| `create_mr` | (W2b, 유지) | 불필요(작업 경로) | — |
| `merge_mr` | 조건 충족 시 Draft 해제 후 병합(4.4) | 불필요(조건이 서명으로 켜진 등록부) | 그룹 토큰 사용 |
- 삭제 계열(`delete_project`, `delete_group`, 브랜치·MR 삭제)은 **만들지 않는다**. (W3f 갱신: `remove_project`·`delete_remote_repo`·`remove_deploy_key`·`clear_orphan` 네 동작이 추가되었다: `28-PLAN-webconsole-W3f.md`. `delete_group`과 브랜치·MR 삭제는 지금도 없다.)
- **GitLab API 호출 허용 목록**: broker 코드는 아래 (메서드, 경로 모양)만 호출할 수 있고 그 밖의 호출을 만드는 코드 경로가 없다 — `GET /groups`(목록), `POST /groups`, `POST /projects`, `POST /projects/:id/repository/commits`(첫 커밋, 5.3), `POST /projects/:id/deploy_keys`, `POST|PUT /projects/:id/protected_branches`, `GET|POST /projects/:id/merge_requests`, `PUT /projects/:id/merge_requests/:iid`(Draft 해제), `PUT /projects/:id/merge_requests/:iid/merge`. 시험이 모든 HTTP 요청을 가짜 서버에서 가로채 이 목록에 있는 것만 나가는지 확인한다(W2b의 `TestMRClientNeverMerges`와 같은 방식).
- `status`는 그대로 두고 `weekly_budget` 등에 더해 토큰 만료 임박 여부를 알려 준다(7장 위협과 6장).
- 제공자 관련 op: `set_provider`(서명), `list_providers`(읽기, 비밀 없음). 위 표의 GitLab 전용 이름(`list_groups` 등)은 제공자 id를 인자로 받는 일반 op가 된다.

## 5. 흐름
### 5.1 토큰 메뉴 (웹 폼 → broker)
1. 웹 "설정 → 토큰": 목록에는 종류(OpenAI 키, GitLab 그룹 토큰), **마지막 4자리**, 만료일(GitLab), 등록 시각만 보인다. 값은 어디에도 다시 보이지 않는다.
2. "교체" → 값 입력 폼(가린 입력, 자동 완성 끔) → 서명 승인(패스키) → orchd는 **값을 읽지 않고** 서명 요청과 함께 broker로 그대로 넘긴다.
   - **미결정(핵심)**: 값이 orchd를 거치는 것은 불가피하다(웹 → orchd → broker). 그래서 orchd는 요청 본문을 로깅하지 않고 즉시 broker로 전달하고 메모리에서 지우는 경로를 따로 만든다. 더 강한 방법(웹이 broker에 직접 접속하게 하려면 별도 HTTPS 노출이 필요해 W1 구조를 크게 바꾼다)은 이번 범위에서 제외하고, orchd 프로세스 메모리에 값이 잠깐 머무는 위험을 6장에 적는다.
3. broker는 파일(0400)에 쓰고 응답에는 마지막 4자리만 준다.

### 5.2 프로젝트 추가 (서버에 이미 있는 저장소)
`GET /api/v1/repos`(W2a)가 `/home/orch/repos` 아래 저장소를 보여 준다. 사용자는 저장소를 고르고 **AI 전송 허용 여부**, **GitLab에 올릴지**(올릴 거면 기존 GitLab 프로젝트 이름 또는 새로 만들기), **자동 병합 여부**(기본 꺼짐)를 정한다 → 서명 승인 → broker가 등록부에 쓴다. 이미 있는 GitLab 프로젝트를 연결하는 경우 그 프로젝트가 broker의 **그룹 토큰 범위(그룹)** 안에 있어야 한다.

### 5.3 새 프로젝트 생성 (빈 프로젝트 + GitLab 프로젝트)
0. **그룹 선택**: 화면에서 `list_groups`로 받은 기존 그룹 중 하나를 고르거나 "새 그룹 만들기"(이름·경로(정규식 `^[a-z][a-z0-9-]{1,40}$`)·상위 그룹 선택)를 고른다. 새 그룹은 **비공개**로만 만들고, 만들기도 서명 승인(`create_group`)을 거친다.
1. 이름·설명 입력 → 서명 승인(메시지에 이름, 그룹, AI 전송·자동 병합 설정이 모두 들어간다).
2. broker가 GitLab에 프로젝트를 만든다(`POST /projects`, 선택한 그룹의 `namespace_id`, 이름은 정규식 `^[a-z][a-z0-9-]{1,40}$`, 빈 프로젝트, `main` 보호를 **broker가 명시적으로 설정**: push·병합 Maintainer만).
3. **첫 커밋은 broker가 GitLab의 커밋 API로 만든다**(`POST /projects/:id/repository/commits`, `branch: main`, 파일은 선택한 템플릿의 `.aigate.yml`, `.gitignore`, README). 이유: GitLab의 기본 보호(3장)는 개발자·배포 키의 최초 push를 막으므로 orchd가 배포 키로 `main`을 push하는 방식은 새 프로젝트에서 막힐 가능성이 크다. 관리자 토큰(Maintainer 이상)의 커밋 API는 보호 규칙을 풀지 않고 쓴다. **빈 저장소에 이 API로 첫 커밋을 만드는 동작은 추정이고 확인하지 못했다**(11장 구현 전 실험).
4. broker가 `orchd`가 알려 준 **배포 키 공개 키**를 프로젝트에 **읽기 전용** 배포 키로 등록한다(clone만 필요하므로 `can_push: false`. 작업 브랜치 `orch/<번호>` 게시에는 쓰기가 필요하므로 **게시를 켠 프로젝트는 쓰기 배포 키**로 등록하되, 이 선택은 생성 화면에서 묻고 서명에 포함한다). 공개 키는 비밀이 아니다.
5. orchd가 `/home/orch/repos/<name>`에 `git clone`(배포 키)한다. 실패하면 **GitLab에 만든 프로젝트는 지우지 않고**(삭제 기능이 없다) 등록을 `failed`로 표시해 사용자에게 알린다(고아 프로젝트는 사용자가 GitLab에서 정리).
6. 성공하면 broker가 등록부에 쓰고 orchd의 정책은 `kind: registered`로 인식한다. **전제(한 번만)**: `policy.yml`에 `registered: {level: L1}` 클래스를 사람이 한 번 넣는다(5.4).

### 5.4 정책과의 연결
- 웹에서 등록한 프로젝트는 모두 `kind: registered`(L1, 허용 동작은 기존 `allowlist`인 `push_branch`만)다. L1보다 높은 등급이나 다른 허용 동작은 이 메뉴가 주지 않는다.
- `policy.yml`에 `registered` 클래스를 넣는 일은 이 설계의 **일회성 관리자 작업**이다(정책 해시가 바뀌므로 실행 중인 작업은 없어야 한다, W2a·W2b와 같은 컷오버 절차).

## 6. 위협과 완화
| 위협 | 완화 |
|---|---|
| **전체 권한 토큰 유출**(사용자 결정: 전체 권한을 줄 수 있는 토큰) | **솔직히: GitLab `api` 범위 토큰은 그룹 하나로 범위를 줄일 수 없다.** 개인 액세스 토큰이면 그 사용자가 접근할 수 있는 **모든 것**이 영향권이고, 관리자(admin) 계정 토큰이면 GitLab 전체(삭제 포함)다. broker 코드가 삭제를 못 하게 막아도 **토큰이 유출되면 유출한 쪽은 무엇이든 할 수 있다**. 완화: ① **사용자가 관리자 토큰을 주기로 했다**(내부 서버에서만 쓴다는 이유). "내부에서만 쓴다"는 외부 노출은 줄이지만 **유출됐을 때의 영향 범위는 그대로**다(orch-vm 침해, orchd 메모리, 백업 같은 경로로 새면 GitLab 전체가 영향권). 사용자가 이 점을 알고 받아들였다. 권장하는 대안(관리자가 아닌 전용 봇 사용자)은 그대로 남겨 두고, 관리자 토큰이면 새 최상위 그룹 생성이 가능한 대신 **다른 보호 장치를 더 엄격히** 한다(②~⑥, 특히 API 허용 목록과 삭제 코드 부재). **GitLab 인스턴스 설정에 따라 최상위 그룹 생성 가능 여부가 다르다**(확인하지 못함), ② 만료일 지정과 만료 임박 경고(`status`), ③ broker의 **API 호출 허용 목록**(4.5)과 삭제 코드 부재, ④ 생성·병합·토큰 변경은 전부 원장과 호출 한도, ⑤ 파일은 0400 broker 전용, `orch`는 읽기 불가, 값은 응답·로그·원장·오류에 절대 나오지 않음(W2b 규칙 재사용), ⑥ 의심되면 GitLab에서 토큰 폐기가 먼저 |
| **웹 세션 탈취로 허용 목록·자동 병합 확대** | 모든 변경이 서명(패스키·SSH 키) 승인, 서명 없이는 broker가 거부, 재사용 방지(요청 ID·발급 시각), 서명 대상에 변경 내용 전체를 넣어 승인 화면과 같게 |
| **`orch` 계정 탈취** | 등록부·토큰은 `orch`가 읽거나 쓸 수 없고 서명 검증은 broker가 한다. 단 `orch`는 정책 파일과 저장소를 바꿀 수 있으므로 (게시 시) 정책 해시 변경 승인이 마지막 방어선이다(기존) |
| **토큰 값이 orchd 메모리를 거침** | orchd는 값을 저장·로깅하지 않고 즉시 전달·소거. 프로세스 메모리 덤프가 가능한 공격자(같은 uid)는 막지 못한다 — **알려진 잔존 위험**으로 문서화 |
| **자동 병합으로 잘못된 코드가 `main`에** | 기본 꺼짐, 프로젝트별·전역 서명 스위치, 하루 상한, `sha` 고정으로 다른 커밋 병합 거부, 판정 통과 필수, 되돌리기는 사용자가 GitLab에서(broker는 revert도 하지 않음) |
| **생성 도중 실패로 고아 GitLab 프로젝트** | 삭제하지 않고 `failed`로 기록·알림, 같은 이름의 재시도는 "이미 있음"으로 안내 |
| **프로젝트 이름·경로 조작** | 이름 정규식. 경로: **broker는 문법만 검사**(정리된 절대 경로, `repo_roots` 항목의 바로 아래 한 단계, 파일 시스템 접근 없음: broker 계정은 `/home/orch/repos`(`orch:orch 0700`)를 읽지 못한다), **파일 시스템 검증(심볼릭 링크·폴더·git 저장소·HEAD·정책 루트 아래)은 orchd만**(propose·apply 모두 broker 호출 전, W3a 정정, 21번 문서 4장). 지키는 성질은 AI 전송 허용이 서명자가 승인한 경로에만 적용된다는 것 |
| **선택 비밀 보호(AI 전송)** | 프로젝트별 `ai_context`가 꺼져 있으면 미리보기가 `allowed:false`(W2b와 같음) |

## 7. 서명 메시지 (`aigate-registry-v1`)
```
aigate-registry-v1
action: <register_project | create_project | set_token | delete_token | set_flag>
subject: <프로젝트 id 또는 토큰 종류>
change: <정규화된 변경 내용의 SHA-256 64자리 hex>
request: <무작위 128비트 hex>
issued: <RFC 3339 UTC>
```
- 화면은 서명 전에 `change`가 가리키는 **전체 변경 내용을 사람이 읽을 수 있게 보여 주고**, 그 내용으로 `change`를 다시 계산해 일치할 때만 서명 버튼을 연다(W2a의 "보이는 값 = 보내는 값"과 같은 규칙).
- 토큰 값 자체는 `change`에 **넣지 않는다**(서명 메시지에 비밀을 넣지 않는다). 대신 **솔트를 섞은 해시** `SHA-256(request ‖ ":" ‖ 값)`을 넣는다(결정 2, 권장안 채택). 이렇게 하면 서명 승인과 적용 사이에 orchd가 **다른 토큰으로 바꿔치기하는 것**을 broker가 잡는다. `request`가 솔트라 사전 계산이 안 되고, broker는 값이 **32자 이상**이 아니면 거부해 짧은 비밀의 무차별 대입을 막는다.
- 기존 `aigate-approval-v1`(작업 승인)과 섞이지 않도록 첫 줄(접두)이 다르고 `Message`/`MESSAGE_RE`가 서로 거부한다.

## 8. API 초안 (orchd, `/api/v1`)
| 경로 | 설명 | 등급 |
|---|---|---|
| `GET /projects` | 등록부 목록(비밀 없음) | 읽기 |
| `GET /projects/candidates` | `repo_roots` 아래 아직 등록되지 않은 저장소 | 읽기 |
| `POST /projects/propose` | 변경 내용을 받아 서명 대상 메시지·화면 문구 생성(저장 안 함) | 쓰기 |
| `POST /projects/apply` | 서명과 함께 변경 적용(broker로 전달) | 쓰기+서명 |
| `GET /settings/tokens` | 토큰 목록(종류·마지막 4자리·만료) | 읽기 |
| 기존 `/tasks/{id}/mr` | 유지, 자동 병합 결과 필드(`merge_state` 등)를 `TaskView`에 추가 | — |
- 서버가 토큰 값을 응답에 넣는 경로는 어디에도 없다(응답 스키마 시험).

## 9. 시험 계획
- broker 단위: 서명 없음·잘못된 서명자·메시지 불일치·재사용·만료 모두 거부, 등록부 변경 원자성(파일 손상 시 거부=fail closed), 토큰 쓰기가 값을 응답·로그에 안 남김, 삭제 op 부재(허용 op 이름 시험), 병합 조건 표(조건 하나씩 빼면 거부), `sha` 불일치 시 GitLab 쪽 거부를 가짜 서버로, 일일 상한.
- 적대 시험: `orch` uid가 등록부·토큰·서명자 목록을 못 읽고 쓰기, orchd가 서명 없이 broker 쓰기 op 호출 시 거부, 서명 재전송, 요청 본문에 토큰이 orchd 로그·원장에 없는지.
- 웹: 서명 전 변경 내용 전체 표시, `change` 재계산 일치, 토큰 폼이 값을 DOM·저장소에 남기지 않음, 오류 문구 고정.
- 가짜 GitLab 서버(프로젝트 생성, 배포 키, 보호 브랜치 설정, MR 목록·병합)로 흐름 전체를 시험하고 **실제 GitLab은 컷오버에서 처음 만난다**(미확인으로 기록).

## 10. 쪼개기와 순서
| 단계 | 내용 | 비고 |
|---|---|---|
| **W3a** | 등록부, `projects`/`register_project`/`set_flag`(AI 전송·읽기), `broker`가 서명 검증, 웹 프로젝트 목록·기존 저장소 등록, 옛 `broker.json` 항목 이전 | 토큰·생성·병합 없음. 가장 먼저 컷오버 가능 |
| **W3b** | 토큰 메뉴(`set_token`/`delete_token`), 만료 경고, 배포 키 공개 키 조회 | W3a 위 |
| **W3c** | 제공자 설정(GitLab), 새 그룹·새 프로젝트 생성(배포 키·`.aigate.yml` 첫 커밋·보호 브랜치), 고아 처리 | 관리자 토큰 필요(W3b) |
| **W3d** | 조건부 자동 병합(`merge_mr`, 전역·프로젝트 스위치, 상한, 필수 검사 확인), `TaskView` 병합 필드 | GitLab만. 가장 위험 |
| **W3e** | GitHub 어댑터(저장소·PR 생성·병합, 배포 키, 브랜치 보호) | 같은 등록부·서명·허용 목록 구조 위에 제공자만 추가. 구현 때 GitHub 공식 문서로 API 확인 |
각 단계가 따로 계획·구현·리뷰·컷오버를 거친다. **MR 켜기는 W3a~W3b 이후 sample-app를 등록부로 옮기면서** 자연스럽게 한다.

## 11. 결정과 남은 미결정
**결정함(2026-11-09, 사용자 1차 검토):**
1. **GitLab 그룹**: 새 그룹 만들기와 기존 그룹 안에 추가하기를 **둘 다 지원**한다(5.3의 0단계, `list_groups`·`create_group`). 사용자가 전체 권한을 줄 수 있는 토큰을 주기로 했다. 위험은 6장에 적었다(권장: 관리자가 아닌 전용 봇 사용자). 기존 sample-app는 그 토큰이 접근할 수 있으면 같은 토큰을 쓰고, 그렇지 않으면 지금의 프로젝트 토큰을 계속 쓴다(등록부 항목별 `token` 필드로 구분, W3a에서 상세).
2. **토큰 해시를 서명 메시지에 넣는다**: 솔트(`request`)를 섞은 해시, 토큰 길이 32자 이상 요구(7장).
3. **`signers.json`을 broker가 읽을 수 있다**: 서버에서 확인함(3장). WebAuthn 카운터는 broker가 자격별로 따로 기록하는 것으로 시작한다(orchd 상태와 합치지 않음. 두 곳의 카운터가 어긋나 정상 서명이 거부되지 않는지는 W3a 시험 항목).
4. **자동 병합**: "판정 통과" = `verdict == ok`(3장)이고 작업이 `done`이며 게시된 `head`가 MR의 head와 같음. 이 밖의 추가 비교는 하지 않는다. `max_per_day` 기본값은 **5**로 시작한다(10이 아님, 처음에는 보수적으로. 바꾸는 것도 서명).

**남은 미결정(W3a 계획 전에 정하거나 계획에서 정한다):**
5. **옛 `broker.json`의 `context_repos`·`gitlab.projects`: 기본안으로 한다(결정함)** — 등록부로 옮긴 뒤 옛 항목은 **무시하고**, `orch-broker -check`가 "옛 항목이 남아 있습니다" 경고를 낸다(시작은 막지 않음).
6. **`.aigate.yml`(확인함, 결정함)**: 게이트는 base 커밋의 `.aigate.yml`을 **필수로 읽고 없으면 오류**를 낸다(`internal/gate/gate.go` `ConfigHashAt`). 그래서 **새 프로젝트의 첫 커밋에 유효한 `.aigate.yml`이 반드시 들어가야** 한다(없으면 그 프로젝트의 모든 작업이 검증 단계에서 실패). 첫 커밋 템플릿은 생성 화면에서 고른다(`go` 템플릿: 단위 테스트·`go vet`·빌드 확인, 보호 파일에 `.aigate.yml`·`go.mod`·테스트 파일 포함; `기본` 템플릿: 보호 파일에 `.aigate.yml`만, 검사 없음). **자동 병합은 `.aigate.yml`에 `required: true` 검사가 하나 이상 있는 프로젝트에서만 켤 수 있다**(검사가 없는 프로젝트에서는 판정이 의미 없이 `ok`가 되므로 broker가 `set_flag auto_merge`를 거부). 검사 내용이 사소한지(항상 통과하는 명령인지)는 기계가 판단할 수 없어 **켜는 서명 화면에 검사 목록을 그대로 보여 준다.**
7. **토큰 종류(결정함)**: 관리자 토큰. 6장의 위험 표 참고. **제공자별로 토큰을 따로 둘 수 있다**(4.6).

## 12. 아직 모르는 것(구현 전 실험)
- 실제 GitLab(19.0.8)의 `POST /projects/:id/repository/commits`가 **빈 저장소**에 첫 커밋을 만드는지, 그 뒤 보호된 `main`과 배포 키 clone이 문제없는지, `POST /groups`·`POST /projects`·`deploy_keys`·`protected_branches`·`merge` 동작(최상위 그룹 생성 권한은 서버 설정으로 확인됨: 가능)과 Draft 해제(제목의 `Draft:` 접두 제거로 가능한지)를 W3c·W3d 구현 전에 실험(가짜 서버로는 형식만 시험).
- WebAuthn 검증을 broker 프로세스(`MemoryDenyWriteExecute`, 소켓 외 샌드박스 등)에서 문제없이 도는지.
- GitHub API의 **실제 호출** 동작(공식 문서 확인은 4.6에 반영, 문서로 못 찾은 항목: Draft 해제, 빈 저장소 첫 커밋, 토큰 범위)과 필요한 토큰 종류(W3e에서 시험).
- orchd의 `repo_roots` 아래 새 디렉터리를 만들 때 `ProtectSystem=strict`·`ReadWritePaths` 규칙이 충분한지(`ReadWritePaths=/home/orch/repos`로 가능해 보이나 시험 안 함).
- 웹 → orchd → broker 토큰 경로가 W1 CSRF·세션 규칙과 충돌하지 않는지.
