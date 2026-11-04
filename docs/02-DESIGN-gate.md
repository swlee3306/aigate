# 서브프로젝트 1 — 검증 게이트(aigate) 설계서 (v0.1, 2026-11-04)

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
protected:            # 기존 줄이 바뀌거나 지워지면 승인 필요. 새 줄/새 파일 추가만은 허용
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

### 3.2 어댑터
공통 인터페이스: `Run(check, workdir) → {rc, timedOut, results[id→status], tail}`. 상태는 `passed | failed | error | skipped`.
- `junit`: 명령이 `$AIGATE_JUNIT` 경로에 JUnit XML을 쓰게 한다(pytest, vitest, jest).
- `gotest-json`: `go test -json`을 직접 파싱. id는 `패키지/Test/서브테스트`. 컴파일 실패는 `error`.
- `exit-code`: 통과/실패만(빌드). id는 검사 id.
- 프리셋은 명령과 "선택 범위 축소 무력화 옵션"을 채운다(예: pytest `-o addopts=`).
- (적대 검토 반영) pytest 프리셋은 `python -P -m pytest`로 실행한다. `-P`가 없으면 `python -m`이 작업 디렉터리를 `sys.path` 맨 앞에 넣어 저장소 루트의 `pytest.py`가 진짜 pytest 대신 실행된다. 루트에서 import하는 저장소는 pytest 설정에 `pythonpath = .`이 필요하고, 3.11 미만 Python은 실행이 실패해 `verification_incomplete`로 닫힌다.

### 3.3 판정 규칙
위반은 여러 개 동시에 나올 수 있고 모두 보고서에 남긴다. 판정은 가장 높은 우선순위: `verification_incomplete > check_weakened > behavior_failure > ok`.

| 범주 | 규칙 |
|---|---|
| verification_incomplete | 타임아웃 / 결과 파싱 불가·0건 / 러너 자체 오류(head가 실패 코드로 끝났는데 실패·오류 결과가 없음 = `runner_error`) / 수집·컴파일·설정 오류(`error`) / 필수 검사 미실행 / 커밋되지 않은 변경(dirty) / base 불안정 또는 base 필수 검사 실패 |
| check_weakened | base에서 통과하던 id가 head에서 없음(삭제·이름 변경·deselect) / skip·xfail 처리 / 새 테스트가 skip / 보호 파일 기존 줄 변경·삭제(승인 없음) / `approval_on_add` 경로에 새 파일 추가(승인 없음) |
| behavior_failure | base에서 통과하던 id가 head에서 실패 / 새 테스트 실패 / base에서 되던 빌드가 head에서 실패 |

- **불변식**: base에서 `passed`이던 모든 id는 head에서 같은 id로 실행되어 `passed`여야 한다.
- base에서 이미 실패하던 id는 비교에서 제외하고 보고서에 `waived`로 표시한다.
- 컴파일·수집 오류는 코드 문제와 환경 문제를 구분하지 않고 보수적으로 `verification_incomplete`로 둔다.
- 재시도 없음(재시도가 실제 실패를 가릴 수 있음).
- (적대 검토 반영) **내장 보호 경로**: `**/node_modules/**`는 설정과 무관하게 항상 `approval_on_add`로 취급한다(커밋된 `node_modules/.bin/vitest`가 러너 자체를 바꿔 결과를 위조할 수 있으므로). 이 경로를 건드리지 않는 diff의 승인 해시는 바뀌지 않는다.

### 3.4 승인
- 단위: 보호 파일 diff의 해시(정규화한 diff 텍스트의 sha256). `aigate approve`가 diff를 보여주고, 사람이 확인하면 `(저장소, base, 해시, 시각)`을 `~/.aigate/approvals.json`(저장소 밖)에 기록. **TTY 없는 실행은 거부**한다.
- 해시가 정확히 같을 때만 유효. 승인 후 보호 파일이 더 바뀌면 재승인.
- 승인은 `protected_modified` 위반만 해소한다. 다른 위반은 그대로 남는다.

### 3.5 증거 신선도
보고서는 head SHA, base SHA, 설정 해시, 도구 버전, 실행 명령, 시각을 묶는다. `aigate check`는 (a) 보고서 head == 현재 HEAD (b) 작업 트리 clean (c) 판정 ok (d) 승인 유효를 확인한다. 오케스트레이터는 완료 선언·push 전에 반드시 호출한다.

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

### 3.8 실행 안전
검사는 프로세스 그룹 단위로 타임아웃 시 종료한다. 에이전트가 쓴 코드를 실행하므로 샌드박스가 이상적이나 v1에는 없고 서브프로젝트 ②에서 다룬다.

### 3.9 이력(ledger) — FR14 대응 (사용자 검토 대기)
점진적 자율성의 근거 데이터를 쌓기 위해 게이트는 사건을 **추가 전용 이력**으로 남긴다.
- 위치: `~/.aigate/ledger/<repo-id>.jsonl` (저장소 밖). 한 줄 = 한 사건, 각 줄에 직전 줄의 해시(`prev_hash`)를 넣어 변조를 감지한다(`aigate ledger verify`).
- 게이트가 직접 기록하는 사건: `run_result`(보고서 id, 판정, base/head, 검사별 개수, `task_class`), `approval_granted`, `approval_rejected`.
- 게이트 밖(서브프로젝트 ②)이 기록할 사건: `revert_observed`(사람이 되돌림), `human_override`, `promotion_proposed/approved`, `demotion`.
- `task_class`는 `aigate run --task-class <이름>`으로 받는다(기본 `unclassified`). 분류 체계는 ②에서 정한다.
- 한계: 같은 사용자 권한의 에이전트가 파일 전체를 다시 쓰면 해시 체인도 위조할 수 있다. 3.4와 같은 이유로 근본 차단은 ②의 샌드박스에서 한다.
- 무결성 범위(정확히): 해시 체인은 **중간 줄의 수정·삽입·삭제·순서 변경과 깨진 마지막 줄**을 `ledger verify`로 드러낸다(tamper-evident). 그러나 **마지막 N줄을 통째로 지우거나 파일을 비우는 것은 감지하지 못한다**(남은 부분도 유효한 체인이다). 또 체인은 키 없는 sha256이라 파일에 쓸 수 있는 쪽은 전체를 다시 계산할 수 있다. 꼬리 절단 감지는 외부에 마지막 해시·줄 수를 따로 두는 방식(②)이 필요하다.
- 게이트는 이력을 읽어 승격을 판단하지 않는다(판정은 항상 현재 커밋의 증거만으로).

## 4. 알려진 한계
- 같은 사용자 권한의 에이전트는 이론상 `~/.aigate/approvals.json`을 직접 쓸 수 있다. v1은 `approve`의 TTY 요구만 있고, 근본 차단은 ②가 에이전트를 해당 경로에 접근 불가한 샌드박스에서 실행할 때 가능.
- 새 파일 추가로 검사를 무력화하는 우회는 `approval_on_add` 경로(conftest 등)에 한해 승인 대상으로 줄였다. 그 밖의 새 테스트 파일 안에 심은 무력화 코드(예: assert를 무시하는 헬퍼 호출)는 불변식으로 못 잡을 수 있다(변이 테스트 v2 대상). 적대 검토에서 확인한다.
- 테스트 id가 실행마다 바뀌는 러너(무작위 이름 등)는 지원하지 않는다.
- base 자체가 불안정(flaky)하면 `verification_incomplete`가 늘어난다.
- 이력(ledger)은 꼬리 절단·파일 비우기를 감지하지 못하고 키가 없다(3.9 "무결성 범위").

### 4.1 적대 검토(Task 12)에서 확인한 한계
결과 표는 `04-RESULTS-gate.md` "적대 검토". 아래는 v1 불변식 게이트로는 원리상 못 잡는 것(재현 시나리오는 `KnownGap`으로 남김)과 사용자 결정이 필요한 것이다.
- **결정 대기: 보호 파일에 줄만 추가하는 무력화.** 3.1은 "새 줄 추가만은 허용"이므로, 기존 테스트 파일에 줄만 끼워 넣는 무력화(본문 맨 앞 `return`, 같은 이름 테스트 재정의, 테스트 모듈 끝에서 대상 함수 재정의, 모든 테스트를 no-op으로 감싸기, `vi.mock` 추가, `//go:build ignore` 한 줄 + 같은 id의 대리 테스트, 보호된 `vitest.config.ts`에 위조 코드 추가, `.aigate.yml`에 `required: false` 추가)가 `protected_modified`에 안 걸린다. 규칙으로 잡을 수 있으나 기존 동작(테스트 추가는 승인 불필요)을 바꾸므로 결정이 필요하다. 선택지: (1) 기존 보호 파일의 모든 hunk(추가 포함)를 승인 대상으로(모두 막힘, 기존 파일에 테스트를 추가하는 정상 변경과 `ok/pytest_ini_addopts_narrowing`이 승인 필요로 바뀜) (2) 파일 끝 추가만 허용(본문 삽입은 막지만 끝에 붙이는 재정의·no-op 래핑·위조는 통과) (3) 현재대로 두고 한계로 문서화.
- **실행 중 위조(in-process).** 검사 대상 코드가 러너와 같은 프로세스에서 돌기 때문에, 새 일반 테스트 파일(승인 불필요)이 대상 모듈을 monkeypatch하거나, `pytest_plugins`로 보고서를 바꾸는 훅을 싣거나, 종료 시 JUnit 파일을 다시 쓰고 `os._exit(0)`으로 종료 코드까지 맞추거나, Go `TestMain`이 test2json 스트림을 고쳐 쓰면 결과를 구별할 수 없다. `runner_error`는 종료 코드를 안 맞춘 위조만 잡는다. 완화: 승인 대상 경로 확대(설정), head의 소스에 **base의 테스트 파일**만 얹어 한 번 더 돌리는 검사(새 파일을 무력화), 결과 수집을 러너 밖으로 빼는 격리(②).
- **테스트 환경 감지.** 소스가 `"pytest" in sys.modules`, `AIGATE_JUNIT` 환경 변수, Go `testing.Testing()`이나 `os.Args[0]`의 `.test`로 테스트 중임을 알아채고 그때만 올바르게 동작하면 게이트는 볼 수 없다(테스트 중의 동작만 관찰).
- **모듈 가리기(shadowing).** 새 `src/calc.js`가 `calc.ts`를 가리거나(vite가 `.js`를 `.ts`보다 먼저 찾음), 새 `tests/ingest.py`가 `src/ingest.py`를 가린다(pytest가 테스트 디렉터리를 `pythonpath`보다 앞에 넣음). 완화: 저장소 설정의 `approval_on_add`에 TS 저장소의 `src/**/*.js`, Python 저장소의 `tests/` 아래 테스트가 아닌 모듈 같은 경로 추가(glob은 저장소별로 정하고 시험할 것).
- **테스트하지 않는 입력.** 어떤 테스트도 쓰지 않는 입력에서만 깨지는 변경은 통과한다(변이 테스트 v2가 이 빈틈을 측정).
- **exit-code 검사의 단위.** `go build ./...` 같은 exit-code 검사는 대상별 id가 없어서, 깨진 패키지를 build tag로 빼면 "테스트 없는 패키지 삭제"와 구별되지 않는다.
- **비필수 검사.** base 설정이 테스트 검사를 `required: false`로 두면 그 검사의 실패는 판정에 안 들어간다(설계상 동작, 설정 선택). 완화: 테스트 검사가 하나도 필수가 아니면 경고.
- **증거 파일 위조(제안, 미구현).** `aigate check`는 저장소 안 `.aigate/reports/<head>.json`(git 미추적, 에이전트가 쓸 수 있음)을 믿는다. `run` 없이 `category: ok` 파일을 써 두면 `check`가 통과한다. 3.5가 check의 단계를 정하므로 바꾸지 않았다. 제안: `check`가 `~/.aigate/ledger`의 마지막 `run_result`(같은 head·base·판정 ok)와 대조.
- **base 자동 결정.** `--base`를 생략하면 merge-base로 정한다. 에이전트가 `main`을 head로 옮길 수 있으면 merge-base == head가 되어 초기 모드(회귀 비교 없음)가 된다. 오케스트레이터는 항상 `--base`를 고정해 넘긴다는 전제다.
- **의존성 디렉터리.** `node_modules/`는 내장 보호 경로로 막았다. Go `vendor/`(수정된 vendored 의존성, 예: 항상 통과하는 assert 라이브러리)와 go.mod의 새 `replace` 줄은 같은 계열이지만 장난감 모듈에 의존성이 없어 재현하지 않았다. 의존성이 있는 저장소는 `vendor/**`를 `approval_on_add`에 넣는 것을 권한다.

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
