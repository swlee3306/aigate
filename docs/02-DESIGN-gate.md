# 서브프로젝트 1 — 검증 게이트(aigate) 설계서 (v0.9, 2026-11-05 — 8차 수정: 새 경로의 접은 별칭(folded alias)을 보호 파일로 취급·기존 충돌 그룹은 멤버·blob·mode 비교·충돌 목록 코드 포인트 표시; v0.8, 2026-11-05 — 7차 수정: head 트리 경로 접기 충돌 검사(`path_case_collision`)·내장 보호 glob 대소문자 무시; v0.7 2026-11-05 — 6차 수정: 로드되는 코드 디렉터리 아래 바이너리 요약 승인 금지; v0.6 2026-11-05 — 5차 수정: 로드 가능한 코드 바이너리 거부·확장자-매직 일치·심볼릭 링크 표시; v0.5 2026-11-05 — 4차 수정: 바이너리 요약 허용목록·stats 측정 보정·규칙별 횟수·가정/실험; v0.4 2026-11-05 — 3차 수정: NUL 가리기·CRLF·서브모듈·stats; v0.3 2026-11-04 최종 리뷰 + 사용자 결정 반영)

상위 문서: `01-REQUIREMENTS.md`. 이 문서는 서브프로젝트 ①(검증 게이트)만 다룬다. 오케스트레이터·워커 어댑터·모델 게이트웨이·알림은 별도 설계.

## 1. 목적과 범위
AI가 만든 변경이 "정말 됐는지"를 **모델 출력을 읽지 않고** 기준 커밋(base)과 대상 커밋(head)을 직접 실행·비교해 판정하고, 증거 보고서를 남기는 CLI.

관련 요구사항: FR2(증거 없이 완료 불인정), FR3(판정 범주), FR4(직접 실행·불변식), FR8(증거 보고서), FR10(기준 변경은 승인 시 인정), FR11(Python·Go·TS/Vue).

**비범위(v1)**: 로컬 LLM 자문(→ 서브프로젝트 ④), 변이 테스트 "검사의 검사"(v2), 샌드박스 격리(→ ②), 실패 자동 재시도, 검사 병렬 실행, 다중 저장소.

## 2. 결정 사항 (사용자 확정)
| 항목 | 결정 |
|---|---|
| 판정 방식 | 불변식 기반(diff 패턴 휴리스틱 아님) |
| 검사 범위 | 설정 가능한 검사 목록, v1 기본 어댑터는 테스트+빌드 |
| 구현 언어 | Go 단일 바이너리 |
| base 정의 | 작업 시작 시 고정한 커밋, 없으면 부모 브랜치의 merge-base. base가 없는 새 프로젝트는 "초기 모드" |
| 기준 변경 | 허용하되 승인 해시 없으면 `check_weakened`(FR10) |

## 3. 구조
- 바이너리 `aigate`. 명령: `run`(판정), `check`(증거 신선도 확인), `approve`(사람 승인 기록), `ledger verify`(이력 무결성 확인).
- 저장소의 `.aigate.yml`에 검사 목록과 보호 파일 glob을 둔다. **설정은 base 커밋에서 읽는다**(에이전트가 검사 명령·보호 목록을 바꿔 판정을 피하지 못하게). `.aigate.yml` 자체의 변경도 승인 대상이다.
- 흐름: base/head 해석 → 각각 격리 worktree 체크아웃 → 검사 실행(base, head 모두) → 어댑터가 결과를 `테스트 id → 상태`로 공통화 → 규칙 적용 → 위반 목록 → 우선순위로 판정 → 보고서(JSON+MD) 저장.
- **초기 모드**(base 없음): 회귀 비교 없이 "head에서 검사가 모두 녹색인가"와 보호 파일 스냅샷 기록만 수행. 이후 커밋은 이 스냅샷 커밋을 base로 삼는다.

### 3.1 설정 예 (`.aigate.yml`)
```yaml
version: 1
protected:            # (v0.3) 이미 있는 파일은 어떤 변경이든(줄 추가 포함) 승인 필요. 새 파일 추가는 허용(approval_on_add·내장 보호 경로 제외). v0.2까지는 "새 줄 추가만은 허용"
  - "tests/**"
  - "**/*_test.go"
  - "**/*.test.ts"
  - "pytest.ini"
  - "pyproject.toml"
  - "go.mod"
  - ".aigate.yml"
approval_on_add:      # 이 경로에 "새 파일이 추가"되어도 승인 필요(테스트 동작을 바꾸는 파일). 일반 새 테스트 파일은 해당 없음
  - "**/conftest.py"
  - "**/testutils/**"
  - "**/fixtures/**"
  - "vitest.config.*"
  - "jest.config.*"
checks:
  - id: unit
    preset: pytest            # pytest | go | vitest | jest | (직접 지정)
    timeout: 300s
    required: true
  - id: build
    adapter: exit-code
    command: "go build ./..."
    timeout: 120s
    required: true
```

**v0.2 추가 — 최종 리뷰 반영(2026-11-04)**
- `protected_modified`는 보호 파일(보호 glob과 `approval_on_add`·내장 보호 경로의 합집합)의 **기존 줄 변경·삭제**뿐 아니라 **파일 삭제(빈 파일 포함)·형식 변경(T, 예: 파일→심볼릭 링크)·모드만 변경·바이너리 내용 변경**에도 발동한다. 바이너리 판정은 속성과 무관하게 blob 내용(앞 8000바이트의 NUL, git과 같은 기준)으로 한다.
- 보호 diff(해시·삭제 줄·추가 파일·approve 화면)는 모두 `git diff --text -U0 --full-index`로 만들고 textconv·외부 diff·속성 파일·diff 배치 설정(`indentHeuristic`, `interHunkContext`, `noprefix`, `mnemonicPrefix`, `orderFile`, `submodule`, `ignoreSubmodules`, `relative`)을 고정한다. 그래서 head의 `.gitattributes`나 `.git/info/attributes`의 `-diff`/`binary`가 지운 줄을 가릴 수 없다. `--numstat`은 `--text`를 무시하므로 판정에 쓰지 않는다(`--raw`로 상태·모드·blob).
- 기존 보호 파일에 **텍스트 줄만 추가**하는 것은 여전히 승인 대상이 아니다(4.1 결정 대기). → **v0.3에서 바뀜(아래)**.
- **내장 보호 경로**(설정과 무관하게 `approval_on_add`로 취급, 따라서 보호 diff에도 포함): `**/node_modules/**`, `.gitattributes`, `**/.gitattributes`, `.gitmodules`, `.npmrc`, `**/.npmrc`, `pytest.ini`, `**/pytest.ini`, `tox.ini`, `**/tox.ini`, `setup.cfg`, `**/setup.cfg`, `go.work`, `go.work.sum`, `vendor/**`, `**/sitecustomize.py`, `**/usercustomize.py`, `requirements*.txt`, `**/requirements*.txt`, `package-lock.json`, `**/package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`. 러너 설정·의존성 설치·인터프리터 시작 코드를 바꿀 수 있는 파일들이다. 새로 추가하면 승인이 필요하고, 이미 있는 파일에서 줄을 지우거나 바꾸는 것도(보호 diff에 들어가므로) 승인이 필요하다.

**v0.3 추가 — 사용자 결정 반영(2026-11-04, 8장 [9])**
- **기존 보호 파일의 모든 변경은 승인 대상이다(옵션 1).** 보호 파일(보호 glob·`approval_on_add`·내장 보호 경로의 합집합) 중 base와 head에 **모두 있고 내용·모드·형식이 조금이라도 다른 파일**은 줄을 지우지 않고 **줄만 추가해도** `protected_modified`(규칙 이름 그대로, check_weakened)다. 위반 설명에 해당 파일 이름과 실제 승인 해시가 나온다. 바뀌지 않은 파일, base에 없고 head에만 있는 파일은 여기에 들지 않는다(이름 변경은 `--no-renames`라 삭제 + 추가).
- **새 파일은 그대로다(8장 [6]).** `protected`에만 맞는 새 파일(예: 새 `tests/test_extra.py`)은 승인 없이 허용, `approval_on_add`·내장 보호 경로에 맞는 새 파일(예: 새 `tests/conftest.py`, `web/yarn.lock`)은 전처럼 승인 필요.
- 승인 해시·결속은 바뀌지 않았다(보호 glob 합집합에 대한 diff 텍스트 그대로의 해시).
- 내장 보호 경로에 하위 경로 변형 추가: `**/yarn.lock`, `**/pnpm-lock.yaml`, `**/go.work`, `**/go.work.sum`, `**/vendor/**`, `requirements/*.txt`, `**/requirements/*.txt`.
- 실무 규칙: **새 테스트는 새 파일에.** 기존 테스트 파일에 테스트를 더하면 매번 사람 승인이 필요하다.

**v0.8 추가 — 7차 수정(2026-11-05, 8장 [16], 사용자 결정 2026-11-05)**
- **내장 보호 경로는 대소문자를 무시해서 매칭한다.** `gate.BuiltinGuarded`의 모든 glob은 git `:(glob,icase)`로 보호 diff(`ProtectedDiff`: 해시·approve 화면)와 새 guarded 파일(`AddedMatching`) 양쪽에 쓴다. 그래서 새 `Node_Modules/x/…`, `Vendor/…`, `.NPMRC`, `Package-Lock.json`, `Pytest.ini`, `.Gitattributes`도 소문자 이름과 똑같이 `approval_on_add`(새 파일이면 `protected_modified`, check_weakened)이고 승인 해시가 그 파일을 덮는다. 코드에서는 `gitx.Pathspec{Glob, ICase}`(`gitx.Globs(...)`/`gitx.ICaseGlobs(...)`)로 표시한다.
- **저장소 설정의 glob(`protected`·`approval_on_add`)은 지금처럼 대소문자를 구분한다.** 기존 사용자 glob의 승인 해시가 조용히 바뀌지 않게 하려는 것이다(예: `tests/**`는 `Tests/x.py`에 맞지 않음). 대소문자 변형이 없는 diff의 해시는 icase 전환 전과 바이트 단위로 같다(단위 테스트 `TestICaseKeepsHashWithoutVariants`).
- git의 `icase`는 **ASCII만** 접는다. 유니코드 접기(`ſ`→s, `K`(켈빈)→k)와 NFC/NFD 차이는 이 glob이 아니라 3.3 v0.8의 `path_case_collision`이 잡는다 — **트리에 다른 철자가 함께 있을 때만**이다. 충돌 상대 없이 새로 넣은 `pyteſt.ini` 같은 이름은 잡지 못한다(4장 v0.8 (6), 남은 거짓 `ok`).

**v0.9 추가 — 8차 수정(2026-11-05, 8장 [17], 사용자 결정 "수정 라운드 8 진행해줘")**
- **새 경로의 접은 별칭(folded alias)도 보호 파일이다.** base..head에서 새로 추가된 모든 경로(`git diff --diff-filter=A --no-renames`, `AddedMatching`과 같은 집합; `gitx.AddedPaths`)의 접기 키(`pathfold.KeyPath`: 성분마다 `NFD(Fold(NFD(c)))`를 `/`로 이음, 3.3 v0.8과 같은 키)를 내장 보호 glob과 저장소의 `approval_on_add` glob을 **같은 키로 접은 glob**(`pathfold.FoldGlob`)에 Go 쪽 매처(`gitx.MatchGlob`)로 맞춰 본다. 접은 키는 맞는데 git 자체 매칭(내장은 `:(glob,icase)`, 사용자 glob은 대소문자 구분)이 그 경로를 내지 않았다면 **접은 별칭**이다: `go.worK`(켈빈 기호 U+212A), `pyteſt.ini`, `ſetup.cfg`, `pyteﬆ.ini`(합자 U+FB06), `requirementſ.txt`, `package-locK.json`, 그리고 사용자 glob `**/conftest.py`에 대한 새 `Conftest.py`·`a/b/Conftest.py`. 접은 별칭은 새 guarded 파일과 똑같이 다룬다 — `AddedGuarded`에 들어가 `protected_modified`(승인 없으면 check_weakened)가 되고, `approve` 화면에 나오며, **정확한 원래 경로를 `:(literal)` pathspec으로 보호 diff에 더해** 승인 해시(diff 텍스트와 `A <path>` 줄)가 그 파일의 경로·내용을 덮는다(`run`·`approve`가 공유하는 `Prepare`). literal이라 경로의 이상한 바이트가 glob 문법이 되지 않고, literal pathspec은 정렬해 넘긴다(pathspec 순서는 git 출력에 영향이 없음).
- **glob을 접는 방법**: `/`로 성분을 나누고 `**` 성분은 그대로 둔다. 나머지 성분에서 `*`·`?`는 그대로, 그 사이의 리터럴 문자열은 경로와 같은 키로 접는다. `\x` 이스케이프는 x를 접고 이스케이프를 유지한다. `[...]` 문자 클래스는 ASCII 글자만 소문자로 바꾸고(접은 경로에는 ASCII 대문자가 없음), 비ASCII 문자는 접은 결과가 한 글자일 때만 바꾼다(`ß`→`ss`처럼 여러 글자가 되면 그대로 — 클래스는 한 글자에 맞음). 근사: `?`와 클래스는 접은 키의 한 글자에 맞으므로 여러 글자로 접히거나 분해되는 문자(`é`→`e`+U+0301)에는 맞지 않는다(덜 맞는 쪽의 근사). 내장 glob은 모두 ASCII라 접기는 소문자화와 같다.
- **비ASCII라서가 아니라 guard glob에 맞아서다.** `Straße.txt`·`docs/Überblick.md`·한글 이름처럼 ASCII로 접히는 이름도 어떤 guard glob에도 맞지 않으면 그대로 `ok`다. 또 Go 매처와 git의 구조적 차이(디렉터리 접두 매칭, `[!x]` 등)가 별칭을 만들지 않도록, **접지 않은 경로**(내장 glob은 git icase처럼 ASCII만 소문자)로도 Go 매처가 맞는 경로는 별칭으로 치지 않는다 — 별칭은 접기 때문에만 생긴다. 이 규칙은 보호 대상을 **더하기만** 한다.
- **해시**: 접은 별칭이 없는 저장소의 pathspec 집합·보호 diff·해시는 바이트 단위로 이전과 같다(단위 테스트 `TestHashUnchangedWithoutFoldedAlias`가 7차 코드 cb35145에서 기록한 해시 `8082594eac4a51b7`을 고정). base에 이미 있던 파일은 새 파일이 아니므로 대상이 아니다. `protected` 목록은 접지 않는다(새 파일 허용, 8장 [6]).
- 3.1 v0.8의 "저장소 설정 glob은 대소문자를 구분한다"는 git pathspec으로서는 그대로다. 접은 매칭은 **새 파일에 한해** `approval_on_add`에 별칭을 더하는 별도 단계다(사용자 결정 2026-11-05 "수정 라운드 8", 재리뷰 잔여 (3)).

### 3.2 어댑터
공통 인터페이스: `Run(check, workdir) → {rc, timedOut, results[id→status], tail}`. 상태는 `passed | failed | error | skipped`.
- `junit`: 명령이 `$AIGATE_JUNIT` 경로에 JUnit XML을 쓰게 한다(pytest, vitest, jest).
- `gotest-json`: `go test -json`을 직접 파싱. id는 `패키지/Test/서브테스트`. 컴파일 실패는 `error`.
- `exit-code`: 통과/실패만(빌드). id는 검사 id.
- 프리셋은 명령과 "선택 범위 축소 무력화 옵션"을 채운다(예: pytest `-o addopts=`).
- (적대 검토 반영) pytest 프리셋은 `python -P -m pytest`로 실행한다. `-P`가 없으면 `python -m`이 작업 디렉터리를 `sys.path` 맨 앞에 넣어 저장소 루트의 `pytest.py`가 진짜 pytest 대신 실행된다. 루트에서 import하는 저장소는 pytest 설정에 `pythonpath = .`이 필요하고, 3.11 미만 Python은 `-P`를 몰라 실행이 실패하고 JUnit이 없어 `verification_incomplete`로 닫힐 것으로 추정한다(3.14에서만 확인, 미검증).

### 3.3 판정 규칙
위반은 여러 개 동시에 나올 수 있고 모두 보고서에 남긴다. 판정은 가장 높은 우선순위: `verification_incomplete > check_weakened > behavior_failure > ok`.

| 범주 | 규칙 |
|---|---|
| verification_incomplete | 타임아웃 / 결과 파싱 불가·0건 / 러너 자체 오류(head가 실패 코드로 끝났는데 실패·오류 결과가 없음 = `runner_error`) / 수집·컴파일·설정 오류(`error`) / 필수 검사 미실행 / 커밋되지 않은 변경(dirty) / base 불안정 또는 base 필수 검사 실패 |
| check_weakened | base에서 통과하던 id가 head에서 없음(삭제·이름 변경·deselect) / skip·xfail 처리 / 새 테스트가 skip / 보호 파일 기존 줄 변경·삭제(승인 없음; v0.3부터 기존 보호 파일의 **모든 변경, 줄 추가 포함**) / `approval_on_add` 경로에 새 파일 추가(승인 없음) |
| behavior_failure | base에서 통과하던 id가 head에서 실패 / 새 테스트 실패 / base에서 되던 빌드가 head에서 실패 |

- **불변식**: base에서 `passed`이던 모든 id는 head에서 같은 id로 실행되어 `passed`여야 한다.
- base에서 이미 실패하던 id는 비교에서 제외하고 보고서에 `waived`로 표시한다.
- 컴파일·수집 오류는 코드 문제와 환경 문제를 구분하지 않고 보수적으로 `verification_incomplete`로 둔다.
- 재시도 없음(재시도가 실제 실패를 가릴 수 있음).
- (v0.3, 8장 [9]) `protected_modified`는 "base에 있던 보호 파일이 어떤 식으로든 바뀜(줄 추가·삭제·변경, 파일 삭제, 형식·모드·바이너리 변경)" 또는 "`approval_on_add`·내장 보호 경로에 새 파일"일 때 발동한다. `protected`에만 맞는 새 파일은 발동하지 않는다.
- (적대 검토 반영) **내장 보호 경로**: `**/node_modules/**`는 설정과 무관하게 항상 `approval_on_add`로 취급한다(커밋된 `node_modules/.bin/vitest`가 러너 자체를 바꿔 결과를 위조할 수 있으므로). 이 경로를 건드리지 않는 diff의 승인 해시는 구성상 바뀌지 않는다(pathspec이 diff 텍스트에 아무것도 더하지 않음).

- **(v0.8, 7차 수정, 8장 [16]) `path_case_collision`(verification_incomplete).** `run`(비교 모드·초기 모드)과 `approve`가 공유하는 `Prepare`는 **checkout 전에** head 트리의 모든 경로(`git ls-tree -r -z --name-only <head>`)와 base 트리의 모든 경로를 읽어, 대소문자·정규화를 구분하지 않는 파일 시스템(macOS APFS, Windows NTFS)이 같은 항목으로 만드는 경로를 찾는다. 경로 **성분마다** 접기 키 `NFD(Fold(NFD(c)))`(유니코드 완전 대소문자 접기 `golang.org/x/text/cases` — `ſ`→s, `K`→k, `ß`→ss — 와 정준 분해; NFC `é`와 NFD `e`+U+0301이 같음)를 만들고, 성분 트라이로 (접은 부모 경로, 접은 성분)마다 원래 철자(원래 전체 경로 접두어)를 모은다. 서로 다른 철자가 둘 이상이면 충돌 그룹이다: **파일끼리**(`tests/Run.js`·`tests/run.js`: checkout 때 뒤에 쓴 파일이 앞 파일을 덮음), **디렉터리끼리**(`Node_Modules/x/a.js`·`node_modules/y/b.js`: 같은 파일 경로가 없어도 디렉터리가 합쳐짐), **파일과 디렉터리**(`tests/run`·`tests/Run/x.js`). base 트리에 **똑같은 철자 집합으로** 있던 그룹은 이미 있던 충돌이라 무시하고, head에만 있는 그룹이나 새 철자가 더해진 그룹은 모두 위반이다(초기 모드는 base가 없으므로 head의 모든 그룹). 부모 그룹이 새로 생겼고 철자가 부모 성분에서만 다른 하위 그룹은 부모 그룹에 포함된 것으로 보고 따로 세지 않는다. 위반은 실행당 **하나**, 최대 5개 그룹을 `{철자, 철자}`로(각 경로는 기존 경로 이스케이프 `textsafe.Escape`, 제어 바이트는 `\xNN`으로, 원문은 출력하지 않음) 나열하고 나머지는 "외 N건"으로 센다. 설명은 "그런 파일 시스템에서는 이 경로들이 서로 덮어쓰거나 가려 checkout 결과가 커밋된 트리와 같지 않으므로 검사를 실행하지 않았습니다. 커밋에서 충돌하는 이름을 고치세요." **충돌이 있으면 head도 base도 checkout하지 않고 어떤 검사도 실행하지 않는다**(`base_not_ancestor`처럼 이 위반 하나만 담은 보고서); 보고서·`.md` 저장과 ledger `run_result`(`rules: ["path_case_collision"]`)는 그대로 한다. 판정이 ok가 아니므로 `aigate check`는 그 보고서를 신선한 증거로 받지 않는다. 경로 성분 트라이라 비용은 전체 경로 길이에 비례한다(20만 경로 head+base 약 0.4초, 단위 테스트가 1초 상한을 고정). 대소문자를 구분하는 파일 시스템(Linux)에서도 같은 규칙을 적용한다(같은 커밋이 macOS·Windows에서 다르게 checkout되므로; fail-closed).

- **(v0.9, 8차 수정, 8장 [17]) 기존 충돌 그룹은 멤버·blob·mode가 모두 같을 때만 "기존"으로 인정한다.** v0.8은 base에 같은 키·같은 멤버 이름의 그룹이 있으면 기존으로 보고 무시했으므로, base에 똑같이 있던 `src/sum.js`·`src/Sum.js` 중 head가 `Sum.js` 내용을 바꿔도 `ok`였다. 이제 `git ls-tree -r -t -z`(`gitx.TreeSigs`)로 head·base의 모든 항목(디렉터리 포함)의 `mode 객체id`를 읽어, 그룹의 **모든 멤버가 양쪽에서 같은 서명일 때만** 기존 그룹이다(`pathfold.New(head, base, headSig, baseSig)`). 멤버 하나라도 blob·mode가 다르거나, 한쪽에 서명이 없으면(실패 시 닫힘) 새 그룹이고 `path_case_collision`이다. 디렉터리 멤버는 tree id를 비교하므로 그 아래 무엇이 바뀌어도 새 그룹이다. 대가: 기존 충돌 디렉터리(`Foo/`와 `foo/`) 아래의 어떤 변경도 막힌다 — 충돌 이름을 고치면 풀린다.
- **(v0.9) 충돌 목록의 코드 포인트 표시.** 그룹에 비ASCII 멤버가 하나라도 있으면 각 멤버 뒤에 비ASCII 문자의 코드 포인트를 붙인다(예: NFC `문서 [U+BB38 U+C11C]`와 NFD `문서 [U+1106 U+116E U+11AB U+1109 U+1165]`; ASCII만인 멤버는 `[ASCII]`, 잘못된 UTF-8 바이트는 `\xNN`). 그래서 화면에서 똑같이 보이는 NFC·NFD 철자를 구별할 수 있다. 덧붙이는 문자열은 ASCII이고 경로 자체는 기존 이스케이프 규칙(`textsafe.Escape`)을 따르므로 원시 제어 바이트가 없다. 순수 ASCII 그룹은 전과 같다. `run`의 위반 설명과 `approve` 거부 메시지가 같은 함수(`gate.CollisionList`)를 쓴다.

### 3.4 승인
- 단위: 보호 파일 diff의 해시(정규화한 diff 텍스트의 sha256). `aigate approve`가 diff를 보여주고, 사람이 확인하면 `(저장소, base, 해시, 시각)`을 `~/.aigate/approvals.json`(저장소 밖)에 기록. **TTY 없는 실행은 거부**한다.
- 해시가 정확히 같을 때만 유효. 승인 후 보호 파일이 더 바뀌면 재승인.
- 승인은 `protected_modified` 위반만 해소한다. 다른 위반은 그대로 남는다.
- **v0.2 추가 — 최종 리뷰 반영(2026-11-04)**: `approve`는 해시를 계산하는 텍스트 **전체**(diff 텍스트와 정렬된 `A <경로>` 줄)를 자르지 않고 `----- BEGIN HASHED DIFF -----` / `----- END HASHED DIFF -----` 사이에 출력한 뒤 `yes`를 묻는다. 표시된 텍스트의 sha256 앞 16자가 곧 표시된 해시다. 새 보호 파일(conftest.py, node_modules 항목 등)도 이름만이 아니라 내용이 보인다. 이전에는 삭제 줄 40개와 파일 이름만 보여 줘서 보이지 않는 부분에 위조를 숨길 수 있었다.
- **v0.3 추가 — 제어 문자 거부(fail-closed, 8장 [10])**: `approve`는 **아무것도 출력하기 전에** 보여 줄 diff 텍스트와 출력할 모든 경로(`--raw -z`에서 온 따옴표 없는 경로)를 검사한다. `\n`·`\t` 이외의 C0 제어 문자(ESC 0x1b, `\r`, NUL 등), DEL 0x7f, 잘못된 UTF-8, C1 제어 문자(U+0080–U+009F), 양방향 재배치 문자(U+202A–U+202E, U+2066–U+2069), 경로 안의 줄바꿈이 하나라도 있으면 **거부**한다: 종료 64, 표준 출력에는 아무것도 쓰지 않고, 표준 오류에 첫 위치(경로 목록의 몇 번째 경로 또는 diff 몇 번째 줄과 그 파일 머리)와 문자를 `\xNN`/`U+XXXX`로만 적는다(원문 바이트는 출력하지 않음). 아무것도 승인·기록하지 않는다. 해시는 그대로 원래 바이트로 계산한다. 이유: 에이전트가 보호 파일의 `+` 줄에 커서 이동·지우기 시퀀스를 넣으면 사람이 보는 화면에서 승인할 삭제 줄을 숨길 수 있다(최종 재리뷰).
- **v0.3 추가 — 바이너리 요약**: base 또는 head blob의 앞 8000바이트에 NUL이 있는 보호 파일(git과 같은 기준, 속성 무관)은 원래 바이트 대신 `binary file changed: <경로>, N bytes (base M bytes)` 한 줄로 보여 준다(경로도 같은 검사 대상). 해시에는 전처럼 원래 바이트가 들어간다. 그래서 바이너리 파일이 있으면 표시된 텍스트의 sha256은 해시와 다르고, 화면에 그 사실을 한 줄로 알린다.
- **v0.4 추가 — NUL 가리기 방지(3차 수정, 8장 [11])**: 바이너리 판단을 base·head **쪽마다** 따로 한다(그 쪽 blob 어디든 NUL이 있으면 바이너리, git의 "앞 8000바이트" 기준을 포함하는 상위 집합, 속성 무관). `approve`는 출력하기 전에 다음이면 **거부**한다(종료 64, 표준 출력 없음, 승인 없음, 표준 오류에 이스케이프한 경로와 이유, head의 첫 NUL 줄 번호): (a) 기존 파일(M/T)이 **텍스트에서 바이너리로** 바뀜(`text_became_binary`), (b) head에서 바이너리이고 추가(A)·변경(M/T)된 파일의 이름이 소스·설정·텍스트(대소문자 무시 확장자 `.ts .tsx .js .jsx .mjs .cjs .go .py .rb .java .kt .c .h .cc .cpp .cs .rs .php .sh .bash .zsh .ini .cfg .conf .toml .yml .yaml .json .xml .html .css .md .txt .lock .mod .sum .gradle .properties .env`, 또는 확장자 없는 `Makefile Dockerfile Jenkinsfile Rakefile .gitattributes .gitmodules .npmrc conftest`)(`binary_source_file`). 이유: 텍스트 보호 파일에 NUL 하나만 넣으면 예전에는 `binary file changed` 한 줄로만 보여 사람이 내용을 못 보고 승인했다(TS/JS 주석 속 NUL은 vitest를 통과). 요약 한 줄은 **존재하는 모든 쪽이 바이너리인 진짜 바이너리 파일**(변경: 양쪽, 추가: head, 삭제: base)에만 쓰고, 형식은 `binary file changed: <이스케이프한 경로>, <head 크기> bytes (base <base 크기> bytes), head blob <40자 id>`(삭제는 head 0 bytes, blob id 0…0). 바이너리에서 텍스트로 바뀐 파일은 요약하지 않고 diff 그대로 보여 주는데, `-U0` diff의 `-` 줄에 base의 NUL이 남으므로 실제로는 아래 제어 문자 규칙으로 거부된다(fail-closed, 사람은 내용을 가린 채 승인하지 않음). 게이트 판정은 그대로다(바이너리 변경은 전처럼 `protected_modified`).
- **v0.4 추가 — CRLF 허용(8장 [12])**: `\r` **바로 뒤가 `\n`** 이면(CRLF 줄 끝) 안전한 것으로 보고 그대로 출력한다(해시는 원래 바이트, 표시된 블록의 sha256 앞 16자 = 해시). **단독 `\r`**(뒤가 `\n`이 아님: 텍스트 끝, 다른 바이트 앞, `\r\r\n`의 첫 `\r`)은 여전히 거부한다. 파일 마지막 바이트가 `\r`이면 git이 `\ No newline at end of file` 앞에 자기 `\n`을 붙이므로 그 경우도 단독 `\r`로 보고 거부한다. 경로 속 `\r`, Markdown 보고서의 한 줄 이스케이프(`\x0d`)는 바뀌지 않았다.
- **v0.4 — 의도적으로 승인하지 않는 fixture**: ESC/ANSI 시퀀스, 잘못된 UTF-8(Latin-1 등), C1, 양방향 문자를 담은 **텍스트** fixture는 계속 거부한다. 우회 플래그는 **의도적으로 없다**. 그런 변경은 사람이 직접 `git diff --text <base> <head> -- <경로>`(필요하면 `| cat -v`)로 확인한 뒤, fixture를 진짜 바이너리(NUL 포함, 소스·텍스트 확장자가 아닌 이름)로 두거나 제어 문자를 없앤 커밋을 만든다.
- **v0.4 — 거부 사건 기록**: 위 거부(안전 검사 거부)는 출력 전에 이력에 `approval_refused`(이유 코드만)를 남긴다(3.10). 사람이 `yes`가 아닌 답을 한 경우는 사건으로 남기지 않는다.
- **v0.5 추가 — 바이너리 요약은 허용목록에만(4차 수정, 8장 [13])**: 3차 재리뷰에서 **새로 추가한** guarded 바이너리가 요약 한 줄로 승인되는 것을 재현했다: `#!/usr/bin/env node` + NUL인 확장자 없는 `node_modules/.bin/vitest`(실제로 실행되어 "all tests passed"를 출력), 텍스트 확장자 목록에 없던 `.mts`, 실행 권한 100755인 `vendor/sub/run`(sh + NUL). 원인은 거부 목록(텍스트 확장자 denylist) 방식이었다. 이제 **허용목록**이다. 요약 한 줄은 (i) 삭제된 파일, 또는 (ii) head에서 바이너리인 추가·변경·형식 변경 파일 중 **다음을 모두 만족하는 것**에만 쓴다: 소문자 확장자가 `png jpg jpeg gif webp ico icns bmp tif tiff heic avif svgz pdf zip gz tgz bz2 xz zst 7z tar woff woff2 ttf otf eot wasm node so dylib dll exe bin dat db sqlite sqlite3 mp3 mp4 m4a mov wav ogg flac webm psd class jar pb npy npz parquet` 중 하나, head의 git 모드가 `100644`(실행 권한·심볼릭 링크·gitlink 아님), 내용이 `#!`로 시작하지 않음, 경로 성분에 `.bin`이 없음(`node_modules/.bin/…`, `**/.bin/**`; `bin/`은 해당 없음, `x.bin` 파일 이름도 해당 없음; 이름이 정확히 `.bin`인 파일은 해당). 그 밖에 head에서 바이너리인 파일은 모두 출력 전에 **거부**한다(종료 64, 표준 출력 없음, 승인 없음, 파일 내용은 출력하지 않고 이스케이프한 경로·이유·이유 코드만): `binary_executable`(실행 권한 또는 100644가 아닌 모드, `#!` 시작, `.bin` 경로), `binary_source_file`(v0.4의 텍스트 확장자·이름 규칙에 맞음 — 예전 사건의 의미를 지키려고 분류용으로만 남김; 이 목록의 어느 것도 허용목록에 없다는 것을 단위 테스트로 확인), `binary_unknown_type`(그 밖: 확장자 없음, `.mts .cts .vue .svelte .snap .sql .dat2` 등 목록에 없는 확장자). 판단 순서는 `binary_became_text` → `text_became_binary` → `binary_executable` → `binary_source_file` → `binary_unknown_type`. 양쪽이 모두 바이너리인 허용목록 fixture(예: 변경된 `.png`, `.bin`)는 전처럼 요약으로 승인할 수 있다.
- **v0.5 추가 — 바이너리→텍스트 전용 거부**: 기존 파일(M/T)이 base에서 바이너리, head에서 텍스트이면 일반 바이트 검사 **전에** `binary_became_text`로 거부한다. 예전에는 일반 제어 문자 메시지가 나와 head를 탓하고 "새 커밋에서 문자를 없애라"고 했지만 NUL은 base 쪽 `-` 줄에 있다. 새 메시지는 "base는 바이너리, head는 텍스트"라고 말하고, 변경을 "이 파일 삭제 + 텍스트를 새 이름의 파일로 추가"로 나누라고 안내한다(삭제는 요약 한 줄, 추가한 텍스트는 그대로 보여 검토 가능).
- **v0.5 — 남는 판단**: 허용목록에는 `.node .so .dylib .dll .exe .wasm .class .jar`처럼 **불러와 실행될 수 있는 형식**도 들어 있다(사용자 결정 목록 그대로). 이런 파일의 변경은 요약 한 줄(경로·크기·blob id)로만 보이므로 사람이 경로를 보고 판단해야 한다(4장 v0.5).
- **v0.6 추가 — 로드 가능한 코드·매직 바이트·심볼릭 링크(5차 수정, 8장 [14], [13]을 고침)**: 4차 재리뷰의 남은 위험 세 가지를 고쳤다. (1) **로드 가능한 코드 바이너리는 요약 승인하지 않는다.** `.node .so .pyd .dylib .dll .exe .wasm .class .jar .a .o .obj .lib .bundle .jnilib`(대소문자 무시, `.cpython-312-darwin.so`·`.abi3.so` 같은 CPython 태그와 `libx.so.1`처럼 `.so.<숫자>`로 끝나는 이름 포함)을 허용목록에서 뺐다. 추가·변경·형식 변경이면 `binary_loadable_code`로 거부한다(종료 64, 승인 없음, 우회 플래그는 의도적으로 없음). 러너가 코드로 불러올 수 있는 바이너리는 요약으로 검토할 수 없으므로 사람이 저장소 밖에서 직접 검토(산출물 확인)하고 **사람이 직접 커밋해** 다음 base에 들어가게 한다. 삭제는 전처럼 요약 한 줄. 재현: CPython은 같은 디렉터리의 `mod.py`보다 확장 모듈 `mod.cpython-314-darwin.so`(모드 100644)를 먼저 불러와 텍스트 변경 없이 `mod.add(2,2)`가 4에서 42가 되었다. (2) **확장자와 매직 바이트가 맞아야 요약한다.** 매직 넘버가 있는 허용목록 형식(png jpg jpeg gif webp bmp ico icns tif tiff pdf zip gz tgz bz2 xz zst 7z woff woff2 ttf otf eot sqlite sqlite3 mp3 mp4 m4a mov heic avif wav ogg flac webm psd tar npy npz parquet svgz)은 head blob 앞부분(최대 512바이트)이 그 형식의 매직과 맞을 때만 요약하고, 아니면 `binary_magic_mismatch`로 거부한다(메시지는 확장자를 말하고 "내용이 그 형식이 아님"이라고만 하며 내용은 출력하지 않음). 매직이 없는 `.bin .dat .db .pb`(`.db`는 SQLite가 아닐 수 있어 매직 없음으로 취급)는 **base에 이미 있던 바이너리 파일을 바꾼 경우(M/T)에만, 경로 성분에 `node_modules`·`vendor`(대소문자 무시)가 없을 때만** 요약하고, 새로 추가했거나 그 디렉터리 아래면 `binary_unknown_type`으로 거부한다. 재현: `node_modules/helper/package.json`의 `"main": "logo.png"` + JS에 NUL을 넣은 `logo.png`가 요약 한 줄로 승인되었고 Node `require()`가 그것을 JS로 실행했다. 진짜 PNG 헤더로 시작하면 Node에서 구문 오류가 난다. (3) **심볼릭 링크 표시(거부 규칙 추가 없음).** `approve` 화면의 변경·새 guarded 파일 목록과 `run` 보고서의 `protected_modified` 설명에 링크를 `<경로> (symlink → <대상>)`로 표시하고, 링크 대상 파일의 내용은 검토한 diff에 들어 있지 않다는 한 줄을 붙인다. 대상은 경로와 같은 검사를 받는다(`approve`: 줄바꿈·제어 문자가 있으면 `unsafe_bytes`로 거부, 보고서: `\xNN`으로 이스케이프). 바이너리→심볼릭 링크 변경의 메시지는 "head는 심볼릭 링크/텍스트"라고 말한다(이유 코드는 그대로 `binary_became_text`). 판단 순서: `binary_became_text` → `text_became_binary` → `binary_executable` → `binary_loadable_code` → `binary_source_file` → `binary_unknown_type`(허용목록 밖, 또는 매직 없는 형식의 조건 불충족) → `binary_magic_mismatch`. 위 "v0.5 — 남는 판단"의 로드 가능한 형식 허용은 이것으로 대체된다.
- **v0.7 추가 — 로드되는 코드 디렉터리 아래 바이너리는 요약 승인하지 않음(6차 수정, 8장 [15], 사용자 결정 2026-11-05)**: 5차 재리뷰에서 형식 매직과 JS가 동시에 성립하는 **폴리글랏** 파일(`.gif` `GIF89a=0;…`, `.ttf` `true;…`, `.bmp` `BM=0;…`, `.mp3` `ID3=0;…`, `.mp4` `//\x00\x18ftypmp42\n<JS>`, `.tar` `/*`…`ustar`@257…`*/<JS>`)이 요약 한 줄로 승인되고 기본 Node `require()`로 실행되는 것이 재현됐다(4장 정정). 확장자+매직으로는 이 부류를 닫을 수 없으므로, **러너가 코드를 불러오는 디렉터리** `node_modules` `vendor` `site-packages` `dist-packages` `.venv` `venv` `__pypackages__` `bower_components` `third_party`(경로의 **디렉터리 성분** 전체 일치, 깊이 무관, 대소문자 무시; 파일 이름 자체와 `my_node_modules_backup`·`vendors`처럼 이름을 포함만 하는 성분은 해당 없음) 아래에서 **추가·변경·형식 변경(A/M/T)된 바이너리는 형식과 관계없이 요약 승인하지 않는다**. 이유 코드 `binary_in_loaded_dir`(종료 64, 표준 출력 없음, 파일 내용 미출력, 이스케이프한 경로, 이력에 `approval_refused`(`reason`, `needed_approval`), 승인 없음). 메시지는 "형식에 맞는 파일이 동시에 올바른 코드일 수 있어 요약으로 검토할 수 없다, 사람이 저장소 밖에서 산출물을 직접 검토하고 직접 커밋해 다음 base에 들어가게 하라, 우회 플래그는 의도적으로 없다"고 말한다. **삭제(D)는 그대로 요약 한 줄로 승인할 수 있다.** 그 디렉터리 밖의 바이너리(예: `tests/**` fixture)는 v0.6 규칙(허용목록 + 매직 + 실행 불가·`#!` 없음·`.bin` 성분 없음 + 매직 없는 형식 제한) 그대로 요약한다. 텍스트 파일은 영향이 없다(전처럼 텍스트로 보여 검토). 디렉터리 목록은 코드 한 곳(`cmd/aigate/cli.go`의 `loadedCodeDirs`)에만 있고 단위 테스트가 내용을 고정한다. 매직 없는 형식의 "`node_modules/`·`vendor/` 밖에서만" 조건(v0.6)은 이 규칙에 흡수되었다(그 경우 이유 코드는 이제 `binary_in_loaded_dir`). **판단 순서(파일마다, diff 순서에서 처음 걸린 파일의 이유를 보고)**: `binary_became_text` → `text_became_binary` → `binary_executable` → `binary_in_loaded_dir` → `binary_loadable_code` → `binary_source_file` → `binary_unknown_type` → `binary_magic_mismatch`; 이 바이너리 검사 전체가 `unsafe_bytes` 검사(경로·diff 텍스트) **앞**이다(v0.6까지의 순서 유지). `binary_executable`을 앞에 둔 이유: 실행 권한·`#!`·`.bin` 성분은 "실행된다"는 가장 구체적인 증거이고 디렉터리와 무관하게 거부된다(로드 디렉터리 아래면 메시지에 "형식을 바꿔도 요약 승인하지 않음"을 덧붙임). 그 밖의 형식 이유보다 `binary_in_loaded_dir`를 앞에 둔 이유: 그 디렉터리 아래에서는 형식을 고쳐도 승인할 수 없으므로 형식 이유의 안내("진짜 그 형식으로 바꿔라" 등)는 성공할 수 없는 조언이 된다.
- `run`이 출력하는 Markdown 보고서의 검사 id·규칙·위반 설명은 같은 문자들을 `\xNN`/`\uXXXX`로 이스케이프한다(이 텍스트로는 아무것도 승인하지 않으므로 거부 대신 이스케이프). JSON 보고서는 그대로다.

- **v0.8 추가 — 경로 접기 충돌이 있는 head는 승인하지 않음(7차 수정, 8장 [16])**: `approve`는 `Prepare`가 `path_case_collision` 그룹(3.3 v0.8)을 보고하면 다른 어떤 검사·출력보다 **먼저** 거부한다: 종료 64, 표준 출력 없음, 표준 오류에 이스케이프한 충돌 경로(최대 5그룹 + 나머지 수)와 이유 코드 `path_case_collision`, 이력 `approval_refused`(`reason: path_case_collision`, `needed_approval`), 승인 없음. 그런 head는 커밋된 대로 checkout되지 않아 `run`이 검사를 아예 실행하지 않으므로, 그 diff를 승인해도 `ok`가 될 수 없다; 다른 안전 거부(`unsafe_bytes`, `binary_*`)와 같은 방식으로 처리했다. 해결은 충돌하는 이름을 고친 커밋이다.

- **v0.9 추가 — 접은 별칭의 승인(8차 수정, 8장 [17])**: `approve`는 `Prepare`의 `AddedGuarded`·보호 diff를 그대로 보여 주므로 접은 별칭(3.1 v0.9)도 새 보호 파일(`+ <path>`)로 나오고 그 diff 전체가 화면에 나온다. 승인한 해시는 `run`이 계산하는 해시와 같아 같은 head는 승인 후 `ok`이고, 별칭 파일 내용이 다르면 해시가 달라 승인이 적용되지 않는다. 바이너리·로드 디렉터리·제어 문자 규칙도 다른 보호 파일과 똑같이 적용된다. 충돌 목록의 코드 포인트 표시(3.3 v0.9)는 `path_case_collision` 거부 메시지에도 나온다.

### 3.5 증거 신선도
보고서는 head SHA, base SHA, 설정 해시, 도구 버전, 실행 명령, 시각을 묶는다. `aigate check`는 (a) 보고서 head == 현재 HEAD (b) 작업 트리 clean (c) 판정 ok (d) 승인 유효를 확인한다. 오케스트레이터는 완료 선언·push 전에 반드시 호출한다.

**v0.2 추가 — 최종 리뷰 반영(2026-11-04)**: 사용법은 `aigate check REPORT.json --repo DIR --base REF`이고 `--base`는 필수다. 위 (a)~(d)에 더해 다음 중 하나라도 맞지 않으면 신선하지 않다(종료 1, 문제 목록 출력).
- (e) base 고정: 보고서 base == `--base`가 가리키는 커밋. 초기(initial) 모드 보고서와 base == head 보고서(자기 비교)는 증거로 인정하지 않는다.
- (f) 저장소: 보고서 repo == 현재 git 최상위 경로.
- (g) 설정: 보고서 config_hash == **base 커밋**의 `.aigate.yml` 해시.
- (h) 내부 일관성: 판정 ok인데 위반이 있거나 종료 코드가 0이 아니거나 무효 승인이 있으면 모순으로 거부.
- (i) 이력 대조: ledger가 `ledger verify`를 통과하고, 이 저장소의 (head, base)에 대한 **마지막** `run_result`가 판정 ok, 같은 config_hash, 그리고 검사하는 보고서 파일 바이트의 sha256(`report_sha256`)과 같아야 한다. 보고서 파일만 손으로 쓰거나 실행 뒤 고친 경우는 여기서 걸린다.
- `run`은 보고서를 먼저 저장하고 그 바이트의 sha256을 ledger `run_result`에 기록한다. ledger 쓰기가 실패하면 게이트 오류(64)이고, 그때 남은 보고서는 (i)에 걸려 `check`를 통과하지 못한다.
- `run`은 base == head(해석 후 같은 커밋)이면 게이트 오류다. CLI `run`은 `--base REF`가 필수이고, `--initial`을 명시할 때만 base 없이 초기 모드로 돈다(보고서에 initial로 표시, 위 (e)에 따라 `check` 증거가 아님). 둘 다 주거나 둘 다 없으면 사용법 오류(64).
- 읽을 수 없는 보고서·풀 수 없는 base·base에 설정 없음·홈 디렉터리 오류는 게이트 오류(64)이며 절대 fresh가 아니다.
- 한계: ledger는 키 없는 해시 체인이라, 같은 사용자 권한의 에이전트가 ledger 전체를 다시 계산해 쓰면 (i)도 우회된다(3.9·4장, 근본 차단은 ②).

### 3.6 보고서 스키마 v1
```json
{
  "schema_version": 1, "category": "check_weakened", "exit_code": 3,
  "repo": "...", "base": "sha", "head": "sha", "config_hash": "...", "tool_version": "...",
  "started_at": "...", "finished_at": "...",
  "checks": [{"id": "unit", "base": {"rc": 0, "counts": {}, "seconds": 1.2}, "head": {"rc": 0, "counts": {}, "seconds": 1.1}}],
  "violations": [{"rule": "test_skipped", "category": "check_weakened", "detail": "...", "ids": ["..."]}],
  "waived": ["..."],
  "approvals": [{"hash": "...", "valid": false}]
}
```
저장: `.aigate/reports/<head>.json`, `.md` (git 미추적).

### 3.7 종료 코드
0 ok / 1 behavior_failure / 2 verification_incomplete / 3 check_weakened / **64 게이트 자체 오류**(git 실패, 설정 오류 등). 자체 오류에서는 절대 ok를 내지 않는다.
- v0.2 추가: SIGINT(Ctrl-C)·SIGTERM으로 중단된 `run`도 64("interrupted; nothing recorded")다. 러너가 검사의 프로세스 그룹을 죽이고, worktree는 컨텍스트와 무관하게 지우며, 보고서·ledger는 쓰지 않는다. 검사 실행 순서는 모든 검사의 base 실행을 먼저, 그다음 모든 head 실행이다(head 코드가 공유 상태를 더럽혀 base 결과를 바꾸지 못하게).

### 3.8 실행 안전
검사는 프로세스 그룹 단위로 타임아웃 시 종료한다. 에이전트가 쓴 코드를 실행하므로 샌드박스가 이상적이나 v1에는 없고 서브프로젝트 ②에서 다룬다.

### 3.9 이력(ledger) — FR14 대응 (사용자 검토 대기)
점진적 자율성의 근거 데이터를 쌓기 위해 게이트는 사건을 **추가 전용 이력**으로 남긴다.
- 위치: `~/.aigate/ledger/<repo-id>.jsonl` (저장소 밖). 한 줄 = 한 사건, 각 줄에 직전 줄의 해시(`prev_hash`)를 넣어 변조를 감지한다(`aigate ledger verify`).
- 게이트가 직접 기록하는 사건: `run_result`(보고서 id, 판정, base/head, 검사별 개수, `task_class`; v0.2부터 `config_hash`, `report_sha256` 추가), `approval_granted`, `approval_rejected`.
- 게이트 밖(서브프로젝트 ②)이 기록할 사건: `revert_observed`(사람이 되돌림), `human_override`, `promotion_proposed/approved`, `demotion`.
- `task_class`는 `aigate run --task-class <이름>`으로 받는다(기본 `unclassified`). 분류 체계는 ②에서 정한다.
- 한계: 같은 사용자 권한의 에이전트가 파일 전체를 다시 쓰면 해시 체인도 위조할 수 있다. 3.4와 같은 이유로 근본 차단은 ②의 샌드박스에서 한다.
- 무결성 범위(정확히): 해시 체인은 **중간 줄의 수정·삽입·삭제·순서 변경과 깨진 마지막 줄**을 `ledger verify`로 드러낸다(tamper-evident). 그러나 **마지막 N줄을 통째로 지우거나 파일을 비우는 것은 감지하지 못한다**(남은 부분도 유효한 체인이다). 또 체인은 키 없는 sha256이라 파일에 쓸 수 있는 쪽은 전체를 다시 계산할 수 있다. 꼬리 절단 감지는 외부에 마지막 해시·줄 수를 따로 두는 방식(②)이 필요하다.
- 게이트는 이력을 읽어 승격을 판단하지 않는다(판정은 항상 현재 커밋의 증거만으로).
- **v0.4 추가(3차 수정)**: 실제 구현된 승인 사건 이름은 `approval_granted`(payload: `base`, `hash`, `files_changed` = 승인한 diff의 보호 파일 수; `approvals.json` 기록 **뒤에** 추가)와 `approval_refused`(payload: `base`, `reason` = `unsafe_bytes`·`text_became_binary`·`binary_source_file`; 원래 바이트·경로 없음; 거부를 알리기 **전에** 추가)다. 위의 `approval_rejected`는 `approval_refused`로 이름이 바뀌었고 의미는 "안전 검사 거부"로 좁다(사람의 `no`는 기록하지 않음). `run_result`에는 `rules`(위반 규칙 이름의 정렬·중복 제거 목록, 설명·경로 없음)가 더해졌다. 승인 기록 뒤 이력 추가가 실패하면 "승인은 approvals.json에 기록됨(유효)"이라고 알리고 종료 64. `aigate check`는 여전히 `run_result`만 본다.
- **v0.5 추가(4차 수정)**: `approval_refused` payload에 `needed_approval`(bool: 그 diff가 실제로 승인이 필요한가, 게이트의 `NeedsApproval()`과 같은 조건)이 더해졌다. `reason`에 `binary_became_text`·`binary_executable`·`binary_unknown_type`이 더해졌다. 사건은 전처럼 모두 기록한다.
- **v0.6 추가(5차 수정)**: `reason`에 `binary_loadable_code`·`binary_magic_mismatch`가 더해졌다(3.4 v0.6).
- **v0.7 추가(6차 수정)**: `reason`에 `binary_in_loaded_dir`가 더해졌다(3.4 v0.7). 매직 없는 형식이 로드 디렉터리 아래에서 거부되는 경우의 이유 코드는 `binary_unknown_type`에서 `binary_in_loaded_dir`로 바뀌었다.
- **v0.8 추가(7차 수정)**: `reason`에 `path_case_collision`이 더해졌다(3.4 v0.8). `run_result`의 `rules`에도 같은 이름이 나온다(3.3 v0.8).

### 3.10 `aigate stats` — 승인 부담 측정 (v0.4 추가)
- `aigate stats [--repo DIR] [--days N(기본 14)] [--json]`: 저장소의 이력을 먼저 `ledger verify`와 같은 방식으로 검증하고(실패하면 문제를 출력하고 종료 1, 이력 없음은 `no events` 종료 0), 오늘(UTC)과 그 앞 N-1일 동안의 사건을 UTC 날짜별로 센다.
- 열: `날짜 runs ok behavior incomplete weakened 승인필요 승인수 거절수`. `승인필요` = payload `rules`에 `protected_modified`가 있는 `run_result`(승인 없이 보호 파일이 바뀐 실행), `승인수` = `approval_granted`, `거절수` = `approval_refused`. 합계 줄과 `approvals_granted / runs` 비율(run이 0이면 `-`, JSON은 `null`). `--json`은 같은 내용을 `per_day`·`totals`·`approval_ratio`로 낸다.
- 목적: 실제 사용 1–2주 뒤 사람이 "승인 요구가 얼마나 귀찮은가"를 숫자로 본다. 판정에는 쓰지 않는다. `rules` 필드가 없는 이전 `run_result`는 `승인필요`에 세지 않는다.
- **v0.5 변경(4차 수정, 측정 보정)**:
  - 열 이름 `거절수` → **`안전검사 거부`**. `needed_approval`이 true인 `approval_refused`만 센다(승인이 필요 없는 diff의 거부는 부담이 아니므로). 이 필드가 없는 이전 사건은 true로 센다.
  - 비율 하나 추가: **`필요율`**(`needed_approval / runs`, JSON `approval_needed_ratio`, run이 0이면 `-`/`null`)을 `approvals_granted / runs` 옆에 낸다. 승인수 비율은 사람이 승인을 미루거나 거부되면 낮아지므로, 부담은 필요율로 보는 것이 맞다.
  - 해석 주의(출력에도 한 줄로 냄): `runs`는 **모든 실행**을 센다(서로 다른 head 수가 아님 — 같은 head를 여러 번 돌리면 여러 번). 사람이 `yes`가 아닌 답(예: `no`)을 한 것은 **기록되지 않는다**. 이력은 키 없는 해시 체인이라 숫자는 참고용이다(3.9).
  - **규칙별 횟수**: 텍스트 출력에 두 번째 표 `규칙별 횟수`(규칙, 창 안에서 그 규칙이 `rules`에 든 `run_result` 수; 횟수 내림차순, 같으면 이름순), JSON에 `rules` 객체(`{규칙: 횟수}`). 목적: **같은 규칙이 계속 나오면 에이전트의 실수만이 아니라 도구·문서·구조 검사가 빠져 있다는 신호**로 본다(예: `protected_modified`가 잦으면 "새 테스트는 새 파일에" 안내나 저장소 구조를 손볼 때).
  - 테스트 `TestStatsFromRealRuns`는 실제 실행의 마지막 사건 시각으로 시계를 고정하고 합계 줄만 확인한다(UTC 자정을 넘겨도 실패하지 않음). `statsNow`는 그대로 주입 가능한 시계다.

## 4. 알려진 한계
- 같은 사용자 권한의 에이전트는 이론상 `~/.aigate/approvals.json`을 직접 쓸 수 있다. v1은 `approve`의 TTY 요구만 있고, 근본 차단은 ②가 에이전트를 해당 경로에 접근 불가한 샌드박스에서 실행할 때 가능.
- 새 파일 추가로 검사를 무력화하는 우회는 `approval_on_add` 경로(conftest 등)에 한해 승인 대상으로 줄였다. 그 밖의 새 테스트 파일 안에 심은 무력화 코드(예: assert를 무시하는 헬퍼 호출)는 불변식으로 못 잡을 수 있다(변이 테스트 v2 대상). 적대 검토에서 확인한다.
- 테스트 id가 실행마다 바뀌는 러너(무작위 이름 등)는 지원하지 않는다.
- base 자체가 불안정(flaky)하면 `verification_incomplete`가 늘어난다.
- 이력(ledger)은 꼬리 절단·파일 비우기를 감지하지 못하고 키가 없다(3.9 "무결성 범위").
- **(v0.2, 무인 사용 차단 요인) 검사 대상 코드에 넘기는 환경(I4).** 검사·`setup` 명령은 aigate와 같은 사용자 권한과 전체 환경 변수(예: `GH_TOKEN`, 클라우드 자격 증명)로 실행된다. 에이전트가 쓴 테스트 코드가 토큰을 읽고 `~/.aigate`(승인·ledger)를 직접 쓸 수 있다. 출력 꼬리는 가리지만 실행 자체는 막지 않는다. 환경 허용 목록과 샌드박스(서브프로젝트 ②)가 생기기 전에는 **사람 없이 판정을 믿는 무인 사용을 하면 안 된다.**
- **(v0.2) pytest `-P`.** pytest 프리셋은 `python -P -m pytest`라 Python 3.11 이상이 필요하다. 루트에서 import하는 저장소는 `pythonpath = .` 설정이 필요하다. 3.11 미만에서는 실행이 실패해 `verification_incomplete`로 닫힐 것으로 추정한다(미검증).
- **(v0.2) `--base`는 항상 명시.** CLI `run`·`check`는 `--base` 없이는 돌지 않는다(초기 모드는 `--initial` 명시). 오케스트레이터는 작업을 시작할 때 고정한 base를 두 명령에 같은 값으로 넘긴다. 라이브러리 `gate.Run`은 Base가 비면 예전처럼 merge-base를 자동으로 정한다(테스트용). → v0.3: merge-base를 못 찾거나 merge-base == head이면 **게이트 오류**다(예전에는 조용히 초기 모드). 초기 모드는 `Options.Initial`을 명시할 때만.
- **(v0.2) 결정 대기: 보호 파일에 줄만 추가.** 기존 보호 파일에 텍스트 줄만 추가하는 변경은 여전히 승인 없이 통과한다(3.1). 이것으로 테스트를 무력화하는 우회 10개가 `ok`로 남아 있다(4.1 첫 항목, 사용자 결정 필요). → **v0.3에서 해소**(사용자 결정 옵션 1, 8장 [9]): 이제 기존 보호 파일의 모든 변경이 승인 대상이고 10개(+`.aigate.yml`의 `required: false` 1개) 모두 `check_weakened`로 잡힌다.
- **(v0.3) 대가: 기존 테스트 파일에 테스트를 더해도 승인 필요.** 기존 보호 파일(테스트 파일, `pytest.ini`, lock 파일 등)을 고치는 정상 변경도 사람이 `aigate approve`로 승인해야 `ok`가 된다(예: `pytest.ini`에 `addopts` 한 줄 → 승인 전 `check_weakened`, 승인 후 `ok`). 실무 규칙은 **"새 테스트는 새 파일에"**: `protected`에만 맞는 새 테스트 파일은 승인 없이 허용된다. 새 파일 안의 무력화(실행 중 위조·monkeypatch)는 여전히 4.1의 원리적 한계다.
- **(v0.3) `approve` 거부의 대가.** CRLF 줄 끝(`\r`)을 쓰는 보호 파일, 제어 문자를 일부러 담은 테스트 데이터는 바이너리(NUL)가 아니면 `approve`가 거부한다(fail-closed). 그런 파일은 줄 끝을 바꾸거나 NUL이 있는 바이너리 fixture로 두어 요약 표시를 받아야 한다. 또 텍스트 파일 앞부분에 NUL 하나를 넣으면 그 파일은 요약 한 줄로만 보인다(사람은 이름·크기와 "binary"를 보고 판단해야 함).
- **(v0.2) `npx --no-install`.** vitest/jest 프리셋은 새 다운로드를 하지 않지만, `~/.npm/_npx` 캐시에 예전에 받은 러너가 있으면 그것을 실행한다(고정되지 않은 버전). 실제 저장소는 `setup: npm ci`로 로컬 설치를 쓴다.
- `runner_error`는 모든 테스트가 통과해도 러너가 0이 아닌 코드로 끝나는 경우(예: vitest의 unhandled error·커버리지 임계 실패)를 `verification_incomplete`로 만든다. 보수적인 판정이지만 오경보 원인이 될 수 있고, 장난감 시나리오의 오경보율 0%에는 이런 경우가 없다.

- **(v0.4, 3차 수정) `approve`가 받아들이는 것과 거부하는 것.** CRLF는 `\r\n` 쌍일 때만 허용한다(단독 `\r`은 거부). ESC/ANSI·잘못된 UTF-8(Latin-1)·C1·양방향 문자를 담은 텍스트 fixture, 텍스트에서 바이너리로 바뀐 파일, 소스·설정·텍스트 이름의 바이너리 파일, 바이너리에서 텍스트로 바뀐 파일(base의 NUL이 `-` 줄에 보이므로)은 승인할 수 없다. 우회 플래그는 의도적으로 없다(3.4 v0.4: 직접 `git diff --text`로 검토). 위의 "v0.3 `approve` 거부의 대가" 중 CRLF와 NUL 요약 부분은 이것으로 대체된다.
- **(v0.4) 닮은꼴·보이지 않는 유니코드는 표시하지 않는다.** 폭 0 문자(U+200B 등), 전각·NFKC 닮은꼴 식별자(같은 이름처럼 보이는 다른 테스트 재정의)는 거부도 표시도 하지 않는다. 사람이 화면에서 구별하지 못할 수 있다.
- **(v0.5, 4차 수정) 바이너리 요약.** 요약 한 줄은 허용목록 형식(3.4 v0.5)에만 쓰고 나머지 바이너리는 승인할 수 없다(우회 플래그 없음: 사람이 `git diff --text`로 검토하고 텍스트로 두거나 허용목록 형식으로 바꾼다). 허용목록 형식이라도 `.node .so .dylib .dll .exe .wasm .class .jar`는 불러와 실행될 수 있는 코드이며, 사람은 경로·크기·blob id만 보고 판단한다. 파일 내용 기반 형식 검사(매직 바이트)는 하지 않는다(확장자만 본다).
- **(v0.6, 5차 수정) 로드 가능한 코드·매직 없는 바이너리·형식 판단의 한계.** 네이티브 애드온(`.node`), 확장 모듈(`.so`·`.pyd`), 라이브러리·목적 파일·실행 파일·`.wasm .class .jar` 같은 **로드 가능한 코드는 `approve`로 승인할 수 없고 사람이 저장소 밖에서 검토한 뒤 직접 커밋해야 한다**(다음 base에 들어간 뒤부터는 변경이 아님). 매직 없는 `.bin .dat .db .pb`는 base에 있던 파일의 변경이면서 `node_modules/`·`vendor/` 밖일 때만 요약한다. **확장자+매직 검사는 여전히 휴리스틱일 뿐이다**: 올바른 PNG 헤더 뒤에 JS를 덧붙인 파일도 매직 검사는 통과하며, 그 파일을 코드로 불러오도록 설정된 러너(예: 그런 내용을 받아들이는 로더나 플러그인)라면 실행될 수 있다. 사람은 요약 줄의 경로·크기·blob id를 보고 판단해야 한다. 심볼릭 링크는 대상 경로만 보이고 대상 파일의 내용은 보호 diff 밖이다(표시만 하고 거부하지 않음). 위의 v0.5 항목 중 "로드 가능한 형식도 요약 한 줄"과 "매직 바이트 검사는 하지 않는다"는 이것으로 대체된다.
- **(정정 2026-11-05, 5차 재리뷰) 매직 바이트 검사는 "진짜 PNG 헤더면 Node가 구문 오류"라는 설명보다 훨씬 약하다.** 재리뷰어가 `.gif`(`GIF89a=0;…`), `.ttf`(`true;…`), `.bmp`(`BM=0;…`), `.mp3`(`ID3=0;…`), `.mp4`(`//\x00\x18ftyp…`), `.tar`(`/*`…`ustar`@257…`*/`)로 **JS와 형식 매직이 동시에 성립하는 파일**을 만들어 요약 한 줄로 승인받고 기본 Node `require()`로 실행시켰다. 매직이 JS 식별자·표현식으로 읽히거나 offset 뒤에 있으면 앞을 주석으로 채울 수 있어서, 41개 형식의 약 절반이 같은 원리에 해당한다(PNG는 `\x89`로 시작해 해당하지 않음). 앞서 적은 "그런 내용을 받아들이는 로더가 있어야 실행된다"는 문장은 틀렸다. **운영 규칙:** 요약 한 줄로 보이는 바이너리가 diff 안의 텍스트(`main`, `require`, 설정 등)에서 참조되면 그 바이너리는 검토하지 않은 코드로 취급하고 승인하지 않는다. 구조적 해결안(guarded 경로 `node_modules`·`vendor`의 추가·수정 바이너리는 요약 승인 전면 금지, 삭제만 요약)은 사용자 결정 대기 중이다.
- **(v0.7, 6차 수정) 로드되는 코드 디렉터리 규칙과 남은 한계.** 위 정정의 구조적 해결안을 사용자 결정(2026-11-05)으로 적용했다: `node_modules` `vendor` `site-packages` `dist-packages` `.venv` `venv` `__pypackages__` `bower_components` `third_party` 아래의 추가·변경·형식 변경 바이너리는 `approve`로 승인할 수 없다(`binary_in_loaded_dir`, 3.4 v0.7; 삭제만 요약). 위 운영 규칙은 그 디렉터리 안에서는 코드가 대신하고, 밖에서는 계속 유효하다. **남은 한계:** (1) **`tests/**` fixture와 그 밖의 데이터 디렉터리는 범위 밖이다.** 폴리글랏 파일도 `tests/fixtures/` 아래면 v0.6 규칙대로 요약 한 줄로 승인된다(fixture는 데이터로 취급). **그런 fixture를 코드로 불러오는 테스트(예: `require('./fixtures/logo.gif')`)나, 이 목록에 없는 디렉터리를 코드로 읽는 러너·설정(예: `NODE_PATH`·`pythonpath`·사용자 정의 로더가 가리키는 `lib/`·`packages/` 같은 디렉터리)은 이 규칙이 보호하지 않는다**: 바이너리를 참조하는 텍스트(`main`·`require`·설정)가 diff에 보이면 사람이 그 바이너리를 검토하지 않은 코드로 취급해야 한다(위 운영 규칙). (2) **목록에 디렉터리를 더하려면 코드를 바꿔야 한다**(`loadedCodeDirs`, 설정 키 없음, 의도적). (3) 이 규칙은 `approve` 화면에 오는 파일, 즉 보호 diff에 들어간 파일에만 적용된다. 내장 guarded 경로는 `**/node_modules/**`·`vendor/**`·`**/vendor/**`(대소문자 구분 glob)이고 `site-packages`·`.venv` 등은 내장 guarded가 아니므로, 보호 경로(`protected`·`approval_on_add`) 밖의 새 파일은 처음부터 승인 대상이 아니다(8장 [6]); 그 경로를 지키는 것은 저장소 설정의 몫이다. 대소문자가 다른 `Node_Modules/`도 보호 경로 안이면 거부되지만, 보호 경로 밖이면 내장 glob이 대소문자를 구분해 diff에 들어오지 않는다(확인함: 보호 경로 밖 새 `Node_Modules/h/index.js`에 `run` 종료 0). 대소문자 무시 파일시스템의 macOS에서는 Node가 그 디렉터리를 찾는다(확인함: `require("h")`가 `Node_Modules/h`를 불러옴) — 기존 guarded 목록의 한계, 이번 범위 밖.
- **(정정 2026-11-05, 6차 재리뷰) 대소문자·유니코드 접기 충돌은 "범위 밖"이 아니라 거짓 `ok`다(Critical, 대소문자를 구분하지 않는 파일 시스템 — macOS APFS, Windows NTFS).** 내장 보호 glob(`node_modules/**`, `vendor/**`, `.npmrc`, `pytest.ini`, `package-lock.json`, `.gitattributes` 등)은 대소문자를 구분해서 매칭한다. 재현: base에 `node_modules/tinyassert/index.js`가 있을 때 head에서 `add`를 망가뜨리고 새 경로 `Node_Modules/tinyassert/package.json`(`{"main":"lax.js"}`)과 `Node_Modules/tinyassert/lax.js`(빈 파일)를 추가하면 APFS에서는 두 디렉터리가 합쳐져 `require.resolve`가 `lax.js`로 가고, `run`은 exit 0 `ok`, `approve`는 "files touched (0)", `check`는 `fresh`였다(대조군은 exit 1). Go `vendor/`도 `Vendor/example.com/dep/zz.go`(init에서 `os.Exit(0)`)로 같은 방식으로 통과했다. 같은 부류 두 가지가 더 있다: 보호 디렉터리 안의 대소문자 충돌(base의 `tests/Run.js`, head에서 새 `tests/run.js` 추가 → checkout 시 원본을 덮어씀; 지금 막는 것은 메인 트리의 `dirty_tree` 하나뿐이고 `git update-index --skip-worktree`로 숨기면 통과), 유니코드 접기(`ſ`→s, `K`→k; `node_moduleſ/…`가 같은 shadow를 만들고 `:(glob,icase)`로도 잡히지 않음). **운영 규칙(수정 전까지):** 대소문자를 구분하지 않는 파일 시스템(macOS, Windows)에서는 `ok`가 대소문자·유니코드 변형 경로를 통한 의존성·러너 shadow를 배제하지 못한다고 보고, head의 파일 목록(`git ls-tree -r --name-only HEAD`)에 `node_modules`·`vendor`·테스트 파일과 이름만 대소문자가 다른 항목이 있는지 사람이 확인한다. **해결안(수정 라운드 7 후보, 사용자 결정 대기):** (A) 내장 보호 glob에만 `:(glob,icase)`를 적용 — ASCII 변형만 막음, (B) 근본 수정 — `run`이 checkout 전에 base∪head 트리의 모든 경로를 유니코드 대소문자 접기와 NFC/NFD 정규화로 비교해 충돌이 하나라도 있으면 `verification_incomplete`로 처리하고, guard 매칭도 접은 경로로 수행.
- **(v0.8, 7차 수정 2026-11-05) 위 정정에서 재현한 형태의 거짓 `ok`는 해결됐다 — 위 "운영 규칙(수정 전까지)"은 그 형태(트리 안에 두 철자가 함께 있는 경우)에는 더 이상 필요 없다(기록으로 남김). 충돌 상대 없는 접기 철자는 아래 (6)에 남은 한계와 새 운영 규칙이 있다.** 사용자 결정으로 해결안 (A)와 (B)를 둘 다 적용했다: (A) 내장 보호 glob은 `:(glob,icase)`(3.1 v0.8), (B) checkout 전 head 트리 경로 접기 충돌 검사 `path_case_collision`(3.3 v0.8; 해결안 원문의 "guard 매칭도 접은 경로로" 대신, 충돌이 있으면 checkout 자체를 하지 않으므로 접은 경로 매칭이 필요 없다). 재현한 세 형태(`Node_Modules/` shadow, Go `Vendor/` shadow, `tests/Run.js` vs `tests/run.js`, `node_moduleſ/`)는 이제 모두 `verification_incomplete`(검사 미실행)이고 `approve`는 거부한다(04 "수정 라운드 7"). **여전히 처리하지 않는 것:** (1) **Windows에만 있는 동등성** — 예약 장치 이름(`CON`·`NUL`·`COM1` 등), 이름 끝의 점·공백(`a.` = `a`), 8.3 짧은 이름(`PROGRA~1`), NTFS 대체 데이터 스트림(`a:stream`)은 접기 키에 넣지 않았다(Windows에서 aigate를 쓰려면 별도 작업). (2) **base 철자와 head 철자 사이의 차이는 대상이 아니다**: base와 head는 각자 다른 worktree에 따로 checkout되므로, base의 `tests/Run.js`가 head에서 `tests/run.js`로 이름만 바뀐 것은 충돌이 아니다(같은 트리 안의 두 철자만 충돌). (3) **base에 이미 똑같이 있던 충돌은 무시한다** — 그런 base는 지금도 합쳐진 채 checkout된다(새 철자가 더해지면 위반). (4) **메인 작업 트리의 `dirty_tree` 규칙은 그대로다**(재현에서 우연히 막던 것; 이제는 그것에 기대지 않음). (5) 접기 키는 유니코드 완전 접기라 `ß`와 `ss`도 같게 본다 — 이 기계의 APFS에서 확인함(`Straße`를 만들고 `Strasse`로 열림; `pyteſt.ini`는 `pytest.ini`로, `K`ey는 `key`로 열림). (6) **(남은 거짓 `ok`, 재현함) 충돌 상대가 없는 유니코드 접기 철자의 새 보호 파일.** git `icase`는 ASCII만 접고 (B)는 철자가 하나뿐이면 충돌이 아니므로, base에 같은 이름이 없을 때 head에 새로 넣은 `pyteſt.ini`·`ſetup.cfg`·`go.worK`·`package-locK.json`(그리고 base에 `node_modules/`가 없을 때의 `node_moduleſ/…`)은 내장 guard에도 충돌 검사에도 걸리지 않는다(재현: 새 `pytest.ini`·`package-lock.json`은 `check_weakened`, 새 `pyteſt.ini`·`go.worK`·`ſetup.cfg`·`package-locK.json`은 `ok`). APFS는 그 파일을 원래 이름으로 열어 주므로(위 (5)) pytest·go·npm이 설정으로 읽을 수 있다(러너가 실제로 읽는지는 pytest로 시험하지 않음, APFS `test -f`로만 확인). **해결안(수정 라운드 8 후보, 사용자 결정 대기):** head 트리 경로를 `pathfold.Key`로 접은 결과가 ASCII이면 그것을 `BuiltinGuarded`와 맞춰 보고, 맞는 파일은 리터럴 pathspec으로 보호 diff(해시)와 새 guarded 목록에 넣는다(git 매칭 의미와 어긋나지 않도록 glob 매칭 방법을 함께 정해야 함). **운영 규칙(그때까지):** 대소문자 무시 파일 시스템에서는 head의 새 파일 이름에 ASCII가 아닌 글자(특히 `ſ`·`K`·합자·`ß`)가 있으면 사람이 확인한다. 또 `inLoadedDir`(`strings.EqualFold`)는 `ſ`·`K`를 접지만 내장 guard는 ASCII만 접으므로, 둘이 일치한다는 단위 테스트(`TestLoadedDirsAndBuiltinGuardsAgreeCaseInsensitively`)는 **ASCII 대소문자 변형에 대해서만** 일치를 고정한다.
- **(v0.9, 8차 수정 2026-11-05) 위 v0.8 (6)의 남은 거짓 `ok`(충돌 상대 없는 접기 철자의 새 보호 파일)는 해결됐다. v0.8 (6)의 2026-11-05 운영 규칙(새 비ASCII 이름을 사람이 확인)은 guard glob(내장·`approval_on_add`)에 맞는 이름에 대해서는 접은 별칭 규칙(3.1 v0.9)으로 대체된다(위 문장은 기록으로 남김).** 재현한 형태 — `go.worK`(켈빈)가 `go.mod`의 `replace`를 no-op 모듈로 돌림, `pyteſt.ini`·`ſetup.cfg`의 `pythonpath` shadow, importlib 모드에서 사용자 glob을 피한 새 `tests/Conftest.py`, 기존 충돌 그룹 안의 내용 변경 — 은 모두 수정 전 거짓 `ok`였고 이제 `check_weakened`(앞의 넷) 또는 `verification_incomplete`(`path_case_collision`)다(04 "수정 라운드 8"). **여전히 남는 것:** (1) **근본 해결은 대소문자를 구분하는 파일 시스템에서 검사를 실행하는 것이다**(서브프로젝트 ②의 리눅스 컨테이너·샌드박스 요구사항). 접은 별칭 규칙은 guard glob에 맞는 이름만 잡으므로, guard 목록에 없는 경로를 러너가 대소문자·유니코드를 무시하고 찾는 경우(예: 보호 경로 밖에서 `NODE_PATH`·`pythonpath`가 가리키는 디렉터리의 변형 철자)는 여전히 이 규칙 밖이다. (2) Windows에만 있는 동등성(v0.8 (1))은 처리하지 않는다. (3) `protected`만 맞는 경로의 새 파일은 결정 [6]대로 여전히 승인 없이 허용한다(접은 매칭은 `approval_on_add`·내장 guard에만 적용). (4) glob 접기는 근사다(3.1 v0.9: `?`·클래스는 접은 한 글자에만 맞음). (5) 기존 충돌 디렉터리 아래의 어떤 변경도 이제 `path_case_collision`이다(3.3 v0.9의 대가). (6) pytest 기본(prepend) import 모드는 대소문자가 다른 `Conftest.py`를 불러오지 못해 수집 오류가 난다(확인함 — 그 경우는 원래도 `ok`가 아니었음). pytest 프리셋은 `addopts`를 무력화하므로 importlib 모드는 검사 명령에 직접 쓴 경우에만 해당하며, 그때는 `Conftest.py`를 불러오므로 별칭 규칙이 필요하다.
- **(v0.4) 무인 사용 차단 요인은 그대로**: I4(검사 코드에 전체 환경·`~/.aigate` 쓰기 권한), 키 없는 ledger(같은 사용자 에이전트가 체인 전체를 다시 쓰거나 꼬리를 자를 수 있음 — `aigate stats` 숫자도 같은 한계). 둘 다 서브프로젝트 ②의 샌드박스 전에는 해소되지 않는다.

### 4.1 적대 검토(Task 12)에서 확인한 한계
결과 표는 `04-RESULTS-gate.md` "적대 검토". 아래는 v1 불변식 게이트로는 원리상 못 잡는 것(재현 시나리오는 `KnownGap`으로 남김)과 사용자 결정이 필요한 것이다.
- **결정 대기: 보호 파일에 줄만 추가하는 무력화.** (→ **v0.3에서 해소**: 사용자 결정 옵션 1, 3.1 v0.3 추가·8장 [9]. 아래 원문은 기록으로 남김.) 3.1은 "새 줄 추가만은 허용"이므로, 기존 테스트 파일에 줄만 끼워 넣는 무력화(본문 맨 앞 `return`, 같은 이름 테스트 재정의, 테스트 모듈 끝에서 대상 함수 재정의, 모든 테스트를 no-op으로 감싸기, `vi.mock` 추가, `//go:build ignore` 한 줄 + 같은 id의 대리 테스트, 보호된 `vitest.config.ts`에 위조 코드 추가, `.aigate.yml`에 `required: false` 추가)가 `protected_modified`에 안 걸린다. 규칙으로 잡을 수 있으나 기존 동작(테스트 추가는 승인 불필요)을 바꾸므로 결정이 필요하다. 선택지: (1) 기존 보호 파일의 모든 hunk(추가 포함)를 승인 대상으로(모두 막힘, 기존 파일에 테스트를 추가하는 정상 변경과 `ok/pytest_ini_addopts_narrowing`이 승인 필요로 바뀜) (2) 파일 끝 추가만 허용(본문 삽입은 막지만 끝에 붙이는 재정의·no-op 래핑·위조는 통과) (3) 현재대로 두고 한계로 문서화.
- **실행 중 위조(in-process).** 검사 대상 코드가 러너와 같은 프로세스에서 돌기 때문에, 새 일반 테스트 파일(승인 불필요)이 대상 모듈을 monkeypatch하거나, `pytest_plugins`로 보고서를 바꾸는 훅을 싣거나, 종료 시 JUnit 파일을 다시 쓰고 `os._exit(0)`으로 종료 코드까지 맞추거나, Go `TestMain`이 test2json 스트림을 고쳐 쓰면 결과를 구별할 수 없다. `runner_error`는 종료 코드를 안 맞춘 위조만 잡는다. 완화: 승인 대상 경로 확대(설정), head의 소스에 **base의 테스트 파일**만 얹어 한 번 더 돌리는 검사(새 파일을 무력화), 결과 수집을 러너 밖으로 빼는 격리(②).
- **테스트 환경 감지.** 소스가 `"pytest" in sys.modules`, `AIGATE_JUNIT` 환경 변수, Go `testing.Testing()`이나 `os.Args[0]`의 `.test`로 테스트 중임을 알아채고 그때만 올바르게 동작하면 게이트는 볼 수 없다(테스트 중의 동작만 관찰).
- **모듈 가리기(shadowing).** 새 `src/calc.js`가 `calc.ts`를 가리거나(vite가 `.js`를 `.ts`보다 먼저 찾음), 새 `tests/ingest.py`가 `src/ingest.py`를 가린다(pytest가 테스트 디렉터리를 `pythonpath`보다 앞에 넣음). 완화: 저장소 설정의 `approval_on_add`에 TS 저장소의 `src/**/*.js`, Python 저장소의 `tests/` 아래 테스트가 아닌 모듈 같은 경로 추가(glob은 저장소별로 정하고 시험할 것).
- **테스트하지 않는 입력.** 어떤 테스트도 쓰지 않는 입력에서만 깨지는 변경은 통과한다(변이 테스트 v2가 이 빈틈을 측정).
- **exit-code 검사의 단위.** `go build ./...` 같은 exit-code 검사는 대상별 id가 없어서, 깨진 패키지를 build tag로 빼면 "테스트 없는 패키지 삭제"와 구별되지 않는다.
- **비필수 검사.** base 설정이 테스트 검사를 `required: false`로 두면 그 검사의 실패는 판정에 안 들어간다(설계상 동작, 설정 선택). 완화: 테스트 검사가 하나도 필수가 아니면 경고.
- **증거 파일 위조 → v0.2에서 수정(최종 리뷰 C2).** 예전에는 `check`가 저장소 안 보고서 파일만 믿어 손으로 쓴 `category: ok` 파일이 통과했다. 이제 3.5 (e)~(i)로 base·설정·ledger의 `run_result`(보고서 sha256)와 대조한다. 남은 한계: ledger 체인 전체를 다시 쓰는 같은 사용자 에이전트(키 없음).
- **base 자동 결정 → v0.2에서 CLI는 `--base` 필수.** 예전에는 `--base`를 생략하면 merge-base로 정했고, 에이전트가 `main`을 head로 옮기면 초기 모드가 되었으며 `--base HEAD`(자기 비교)도 ok를 냈다. 이제 자기 비교는 오류, 초기 모드는 `--initial` 명시, 초기 모드 보고서는 `check` 증거가 아니다.
- **의존성 디렉터리.** `node_modules/`는 내장 보호 경로로 막았다(v0.2: `vendor/**`, `go.work`, lock 파일, `requirements*.txt`, `.npmrc`도 내장 보호, 3.1). Go `vendor/`(수정된 vendored 의존성, 예: 항상 통과하는 assert 라이브러리)와 go.mod의 새 `replace` 줄은 같은 계열이지만 장난감 모듈에 의존성이 없어 재현하지 않았다. 의존성이 있는 저장소는 `vendor/**`를 `approval_on_add`에 넣는 것을 권한다.

## 5. 테스트 전략
1. 규칙: Go 표 기반 단위 테스트(합성 결과 입력).
2. 어댑터: pytest / go test / vitest·jest 실제 출력 픽스처.
3. 종단 시나리오: 실제 git 저장소를 만들어 Python·Go·TS 각각 약 25개. 기존 `verify-harness` 24케이스를 재구성하고 처음 보는 변형을 추가.
4. 적대 검토: 구현 후 Codex에게 우회 시나리오를 설계하게 해 실제로 뚫리는지 확인.
5. 합격선: 약화·불완전 시나리오 `ok` 오판 **0건**. 정상 변경(테스트 추가, 리팩터링, 승인된 기준 변경)의 오경보 수와 실행 시간은 측정해 기록.

## 6. 기존 프로토타입 처리
`lab/code/aigate/aigate/*.py`(Python 초안)는 미검증 참고용이다. 규칙의 동작 참고 구현으로만 쓰고 Go로 다시 작성한다. 시나리오 생성기는 `verify-harness`의 변이·테스트 자산을 재사용한다.

## 7. 미결 사항
- (해결) 성공 지표는 8장 [7]에서 확정.
- 첫 적용 저장소 이름(설계에는 영향 작음, 종단 시나리오 이후 정함).

## 8. 검토 기록 (사용자와 항목별 확정)
- [1] 대표 판정 우선순위: **verification_incomplete > check_weakened > behavior_failure > ok** 확정 (2026-11-04).
- [2] 승인 단위: **변경 내용(diff 해시) 그대로에만 유효, 바뀔 때마다 재승인** 확정. 파일 단위 사전 허용은 v1에서 지원하지 않음.
- [3] 설정 위치: **`.aigate.yml`은 base 커밋에서 읽는다**, 설정 변경은 승인·커밋 후 다음 작업부터 적용 확정.
- [4] 재시도: **v1은 재시도 없음**, 기준(base)에서 이미 실패하던 테스트만 waived 확정. 반복 실행(--repeat)은 후속 옵션.
- [5] 컴파일·수집·설정 오류: **전부 verification_incomplete** 확정(원인 구분은 사람이 보고서로 판단).
- [6] 새 파일 우회: **(B) 동작을 바꾸는 파일(conftest·설정·헬퍼)만 승인 대상**, 일반 새 테스트 파일은 허용 확정. 설정 키 `approval_on_add` 추가(3.1·3.3 반영).
- [7] 성공 지표: **합격 3개**(불완전·무력화를 ok로 통과 = 0 / 승인 경계 위반 자동 실행 = 0 / 보고서 없는 완료 주장 = 0, 뒤 둘은 서브프로젝트 ②에서 강제) + **참고 2개**(정상 변경 오경보율, 실행 시간: 측정·기록만) 확정.
- [8] 점진적 자율성(FR14): 승격은 **시스템이 제안하고 사람이 승인**, 강등은 자동, 삭제·운영 변경 승인은 하한선 확정. 게이트에는 이력(ledger, 3.9)만 추가하고 자율 단계 관리는 서브프로젝트 ②로 둔다.
- [9] 보호 파일의 기존 파일 변경은 줄 추가도 승인 대상(옵션 1, 사용자 결정 2026-11-04). 새 파일은 [6] 그대로(`approval_on_add`·내장 보호 경로만 승인 대상). 3.1·3.3 v0.3 추가.
- [10] approve는 제어/비정상 문자가 있으면 거부(fail-closed)(사용자 결정 2026-11-04). 바이너리 보호 파일은 요약 한 줄로 표시, 해시는 원래 바이트. 3.4 v0.3 추가.
- [11] approve는 텍스트→바이너리 전환·소스 확장자 바이너리를 거부(NUL 가리기 방지)(사용자 승인 3차 수정 2026-11-05). 요약 한 줄은 진짜 바이너리에만, head blob id 포함. 3.4 v0.4 추가.
- [12] \r\n 허용, 단독 \r 거부(사용자 승인 3차 수정 2026-11-05). ESC/ANSI·Latin-1 fixture는 계속 거부(우회 플래그 없음). 3.4 v0.4 추가.
- [13] approve 화면 요약은 허용목록 바이너리에만(새 guarded 바이너리 요약 우회 차단)(4차 수정 2026-11-05, 이미 승인된 [11]의 결함 수정). 이유 코드 `binary_executable`·`binary_source_file`·`binary_unknown_type`, 바이너리→텍스트는 `binary_became_text`. 3.4 v0.5 추가.
- [14] approve는 로드 가능한 코드 바이너리를 요약 승인하지 않으며 확장자-매직 불일치를 거부(5차 수정 2026-11-05, 사용자 결정 "남은 위험 고쳐주고", [13]을 고침). 이유 코드 `binary_loadable_code`·`binary_magic_mismatch`, 매직 없는 형식은 기존 파일 변경·`node_modules/`·`vendor/` 밖에서만(아니면 `binary_unknown_type`). 심볼릭 링크는 `(symlink → 대상)` 표시만. 3.4 v0.6 추가.
- [15] approve는 로드되는 코드 디렉터리(node_modules·vendor 등) 아래 추가·수정 바이너리를 요약 승인하지 않음(폴리글랏 우회 차단). 6차 수정 2026-11-05, 사용자 결정 "수정 라운드 6(바이너리 요약 승인 구조적 금지) 진행 — 승인". 이유 코드 `binary_in_loaded_dir`, 삭제는 요약, `tests/**` fixture는 v0.6 규칙 그대로. 3.4·3.9·4장 v0.7 추가.
- [16] head 트리의 대소문자·유니코드 접기 충돌은 verification_incomplete로 처리(체크아웃 전 검사), 내장 보호 glob은 대소문자 무시. 7차 수정 2026-11-05, 사용자 결정 "1번 진행"(해결안 A+B). 규칙 `path_case_collision`(검사 미실행, 보고서·ledger 기록, `approve` 거부), 사용자 glob은 대소문자 구분 유지, base에 똑같이 있던 충돌은 무시. 3.1·3.3·3.4·3.9·4장 v0.8 추가.
- [17] 새로 추가된 경로의 접은 키가 내장·사용자 approval_on_add glob에 맞으면 보호 파일로 취급(승인 대상·해시 포함), 기존 충돌 그룹은 멤버·blob·mode가 같을 때만 기존으로 인정. 8차 수정 2026-11-05, 사용자 결정 "수정 라운드 8 진행해줘"(설계 선택: 컨트롤러 — 비ASCII→ASCII 접기 전체 금지 대신 guard glob 매칭 기준). 충돌 목록은 비ASCII 멤버의 코드 포인트 표시. 3.1·3.3·3.4·4장 v0.9.

## 9. 가정 (사실로 확인하지 않은 전제, v0.5 추가 — 문서 규칙 `00-TEMPLATE.md`)
네 칸 중 **확정**은 2장·8장, **미결정**은 7장이 맡는다.

| 가정 | 근거·현재 상태 | 확인 방법 |
|---|---|---|
| pytest 프리셋의 `python -P`는 Python 3.11 이상이 필요하고, 그보다 낮으면 실행이 실패해 `verification_incomplete`로 닫힌다 | 3.14에서만 확인(3.2, 4장). 3.11 미만은 미검증 | 3.10 이하 인터프리터로 e2e pytest 시나리오를 돌려 판정을 기록 |
| 호출자는 `run`·`check`에 `--base`를 항상 명시하고 같은 값을 넘긴다 | CLI는 `--base` 없으면 거부(3.5). 같은 값인지는 호출자(②) 책임. 라이브러리 `gate.Run`은 Base가 비면 merge-base를 쓴다(4장) | ② 통합 테스트에서 base 고정 확인 |
| jest의 파일 로드 실패(스위트 이름 `undefined`)는 `verification_incomplete`가 아니라 `check_weakened`로 분류되어도 안전하다(둘 다 ok 아님) | Task 11에서 남긴 소항목, 수정하지 않음(진행 기록). vitest는 `error`로 고침 | jest 구문 오류 fixture로 시나리오를 만들어 범주를 기록하고, ok로 새지 않는지 확인 |
| `aigate stats` 숫자는 참고용이다(측정 기간 동안 에이전트가 이력을 고치지 않는다) | 이력은 키 없는 해시 체인이고 꼬리 절단을 감지하지 못한다(3.9) | 측정 중 `ledger verify` 통과, `approvals.json`의 항목 수와 `approval_granted` 수 대조 |
| 승인 빈도는 견딜 만하다 | [9]의 대가(4장 v0.3). 측정 전 | 10장 첫 실험 |
| `npx --no-install`은 새로 받지 않지만 로컬 npx 캐시의 고정되지 않은 러너를 실행할 수 있다; 실제 저장소는 `setup: npm ci`로 로컬 설치를 쓴다 | 4장 v0.2 | 첫 적용 저장소 설정에 `setup: npm ci`가 있는지, 보고서의 러너 버전이 lock과 같은지 확인 |
| 에이전트는 새 테스트를 새 파일에 쓴다 | 3.1 v0.3 실무 규칙 | `aigate stats` 규칙별 횟수의 `protected_modified`와 그 보고서의 파일 목록 |
| 바이너리 형식은 확장자로 충분히 가려진다(매직 바이트 검사 없음) | 3.4 v0.5. `#!`·실행 권한·`.bin`만 내용·모드로 본다 | 실제 저장소의 바이너리 보호 파일 목록을 뽑아 허용목록 밖·안을 확인 |

## 10. 실험할 것 (대화로 답할 수 없어 실행·프로토타입이 필요한 것, v0.5 추가)
| 실험 | 무엇을 | 끝났다고 볼 기준 |
|---|---|---|
| 하루 승인 부담 | 실제 저장소 하나에서 1–2주 동안 `aigate run`·`approve`를 쓰고 `aigate stats --days 14`(`--json`)로 하루 승인필요·승인수·안전검사 거부·필요율·규칙별 횟수를 본다 | 숫자가 쌓이고 사람이 [9](옵션 1)를 유지할지 판단, 결과는 `04-RESULTS-gate.md` |
| 변이 검사(v2)로 테스트 추가 자동 승인 | 기존 테스트 파일에 테스트만 더한 변경을 변이 테스트로 검사해 승인 없이 통과시킬 수 있는지, 실행 시간·비용 | 4.1 우회 시나리오에서 `ok` 오판 0을 유지하면서 승인 수가 얼마나 줄어드는지 측정 |
| 신뢰할 수 없는 테스트 코드용 환경 허용 목록·샌드박스(②) | 검사 명령에 넘기는 환경 변수를 허용 목록으로 줄이고 `~/.aigate` 쓰기를 막는다 | 토큰 읽기·`approvals.json`·이력 직접 쓰기 시나리오가 막힘(I4 해소) |
| CRLF 저장소·Windows 출신 저장소 | 줄 끝이 CRLF인 실제 저장소, Windows에서 만든 저장소(모드·경로)에서 `run`·`approve` | 오경보·거부 건수 기록(지금은 단위 테스트로만 확인) |
