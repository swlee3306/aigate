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

### 3.3 판정 규칙
위반은 여러 개 동시에 나올 수 있고 모두 보고서에 남긴다. 판정은 가장 높은 우선순위: `verification_incomplete > check_weakened > behavior_failure > ok`.

| 범주 | 규칙 |
|---|---|
| verification_incomplete | 타임아웃 / 결과 파싱 불가·0건 / 러너 자체 오류 / 수집·컴파일·설정 오류(`error`) / 필수 검사 미실행 / 커밋되지 않은 변경(dirty) / base 불안정 또는 base 필수 검사 실패 |
| check_weakened | base에서 통과하던 id가 head에서 없음(삭제·이름 변경·deselect) / skip·xfail 처리 / 새 테스트가 skip / 보호 파일 기존 줄 변경·삭제(승인 없음) / `approval_on_add` 경로에 새 파일 추가(승인 없음) |
| behavior_failure | base에서 통과하던 id가 head에서 실패 / 새 테스트 실패 / base에서 되던 빌드가 head에서 실패 |

- **불변식**: base에서 `passed`이던 모든 id는 head에서 같은 id로 실행되어 `passed`여야 한다.
- base에서 이미 실패하던 id는 비교에서 제외하고 보고서에 `waived`로 표시한다.
- 컴파일·수집 오류는 코드 문제와 환경 문제를 구분하지 않고 보수적으로 `verification_incomplete`로 둔다.
- 재시도 없음(재시도가 실제 실패를 가릴 수 있음).

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
- 게이트는 이력을 읽어 승격을 판단하지 않는다(판정은 항상 현재 커밋의 증거만으로).

## 4. 알려진 한계
- 같은 사용자 권한의 에이전트는 이론상 `~/.aigate/approvals.json`을 직접 쓸 수 있다. v1은 `approve`의 TTY 요구만 있고, 근본 차단은 ②가 에이전트를 해당 경로에 접근 불가한 샌드박스에서 실행할 때 가능.
- 새 파일 추가로 검사를 무력화하는 우회는 `approval_on_add` 경로(conftest 등)에 한해 승인 대상으로 줄였다. 그 밖의 새 테스트 파일 안에 심은 무력화 코드(예: assert를 무시하는 헬퍼 호출)는 불변식으로 못 잡을 수 있다(변이 테스트 v2 대상). 적대 검토에서 확인한다.
- 테스트 id가 실행마다 바뀌는 러너(무작위 이름 등)는 지원하지 않는다.
- base 자체가 불안정(flaky)하면 `verification_incomplete`가 늘어난다.

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
