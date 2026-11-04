# 04 결과: aigate 게이트 종단 시나리오 (Python/pytest, Go, TypeScript/vitest)

실행: `go test ./e2e -run Py -count=1 -v` (시나리오마다 실제 git 저장소를 만들고 `gate.Run` 실행). 기대 판정은 결과를 보기 전에 작성했고, 이후 수정하지 않았다.

## Python/pytest

### 시나리오별 결과

| 시나리오 | 기대 | 실제 | 위반 규칙 | head 통과 수 | 초 |
|---|---|---|---|---|---|
| behavior/M1_end_inclusive | behavior_failure | behavior_failure | regression | 7 | 0.67 |
| behavior/M2_no_dedupe | behavior_failure | behavior_failure | regression | 8 | 0.59 |
| behavior/M3_missing_zero | behavior_failure | behavior_failure | regression | 7 | 0.59 |
| behavior/M4_start_exclusive | behavior_failure | behavior_failure | regression | 7 | 0.59 |
| behavior/M5_no_rollback | behavior_failure | behavior_failure | regression | 8 | 0.59 |
| behavior/M6_sum_plus_one | behavior_failure | behavior_failure | regression | 4 | 0.59 |
| behavior/M7_dedupe_ignores_source | behavior_failure | behavior_failure | regression | 8 | 0.59 |
| behavior/M8_swallow_failure | behavior_failure | behavior_failure | regression | 8 | 0.60 |
| incomplete/hang | verification_incomplete | verification_incomplete | timeout | 0 | 5.41 |
| incomplete/import_error | verification_incomplete | verification_incomplete | setup_or_collection_error, test_missing | 0 | 0.63 |
| incomplete/fixture_raises_conftest | verification_incomplete | verification_incomplete | protected_modified, setup_or_collection_error | 0 | 0.61 |
| incomplete/dirty_tree | verification_incomplete | verification_incomplete | dirty_tree | 9 | 0.58 |
| incomplete/base_not_ancestor | verification_incomplete | verification_incomplete | base_not_ancestor | 0 | 0.06 |
| incomplete/no_required_checks_optional | verification_incomplete | verification_incomplete | no_required_checks | 9 | 0.57 |
| incomplete/no_required_checks_zero | verification_incomplete | verification_incomplete | no_required_checks | 0 | 0.14 |
| weakened/skip_marker | check_weakened | check_weakened | test_skipped | 8 | 0.58 |
| weakened/xfail_marker | check_weakened | check_weakened | test_skipped | 8 | 0.60 |
| weakened/expected_value_changed | check_weakened | check_weakened | protected_modified | 9 | 0.59 |
| weakened/assert_is_not_none | check_weakened | check_weakened | protected_modified | 9 | 0.58 |
| weakened/killing_test_deleted | check_weakened | check_weakened | protected_modified, test_missing | 8 | 0.58 |
| weakened/tolerance_widened | check_weakened | check_weakened | protected_modified | 9 | 0.60 |
| weakened/pytest_skip_call | check_weakened | check_weakened | test_skipped | 8 | 0.58 |
| weakened/conftest_deselects | check_weakened | check_weakened | protected_modified, test_missing | 8 | 0.58 |
| weakened/conftest_exists_only | check_weakened | check_weakened | protected_modified | 9 | 0.59 |
| weakened/testutils_helper_added | check_weakened | check_weakened | protected_modified | 9 | 0.58 |
| weakened/test_file_renamed | check_weakened | check_weakened | protected_modified, test_missing | 9 | 0.58 |
| ok/new_test_added | ok | ok | - | 10 | 0.59 |
| ok/refactor_rename_local | ok | ok | - | 9 | 0.58 |
| ok/docstring_only | ok | ok | - | 9 | 0.60 |
| ok/pytest_ini_addopts_narrowing | ok | ok | - | 9 | 0.59 |
| ok/requirement_change_approved | ok | ok | - | 9 | 1.16 |
| weakened/requirement_change_wrong_hash | check_weakened | check_weakened | protected_modified | 9 | 1.17 |

승인 시나리오(`ok/requirement_change_approved`, `weakened/requirement_change_wrong_hash`)의 시간은 gate 실행 2회 합계다.

### 집계

- 시나리오 32개, 기대와 일치 32개, 불일치 0개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건
- 오경보율(정상 `ok` 시나리오 중 ok가 아닌 비율): 0/5 = 0%
- gate 실행 시간: 평균 0.75초, 최대 5.41초 (최대는 hang 시나리오의 5초 타임아웃 포함)
- 타임아웃 시나리오를 제외한 평균: 0.60초

### 불일치 (findings)

없음. 모든 시나리오가 기대 범주와 종료 코드, 지정한 규칙 이름과 일치했다.

### 관찰

- `skip` 마커·`xfail` 마커·`pytest.skip()` 호출은 보호 파일에서 기존 줄을 지우지 않고 줄만 추가하므로 `protected_modified`는 발생하지 않고, `test_skipped`(base에서 통과하던 테스트가 head에서 skip)로만 잡힌다. 이 경로가 유일한 방어선이다.
- `incomplete/import_error`는 `setup_or_collection_error`와 `test_missing`이 함께 나온다.
- `pytest.ini`의 `addopts = -k boundary`는 게이트가 `-o addopts=`로 무력화해 head에서도 9개가 모두 실행되었고 `ok`였다.
- 파일 이름 변경은 `test_missing`의 detail에 `이름 변경 의심` 힌트가 포함되었다.
- 한계: 시나리오는 pytest 한 종류의 장난감 프로젝트(9개 테스트) 한 개에 대한 것이며, 승인 흐름의 `ok`는 해시를 테스트가 직접 승인소에 넣어 검증했다(TTY 승인 경로는 이 스위트의 범위 밖).
- 하드닝(Task 11): `check_weakened` 시나리오는 첫 검사의 head 실패 수가 0이어야 한다(약화로 깨진 변경이 통과해야 하므로). 공유 러너에 넣었고 Python 32개 모두 그대로 통과했다(예외 옵트아웃 `HeadMayFail`은 Python에서 쓰지 않음). 위 시간 표는 하드닝 이전 실행 기록이며 하드닝 후 재실행에서도 32/32 일치했다.

## Go (`go test -json` + `go build` 검사)

실행: `go test ./e2e -run Go -count=1 -v`. 장난감 모듈 `toy`(`calc` 패키지의 `Sum`·`Dedupe`·`Window`, 표 기반 테스트와 서브테스트 16개 통과, `cmd/toy`의 main 패키지). 검사 두 개: `unit`(프리셋 go)와 `build`(`go build ./...`, exit-code). 기대 판정은 실행 전에 작성했다. 아래 표는 게이트 수정(F1~F3) 이후 최종 실행이다. head 통과/실패는 `unit` 기준.

### 시나리오별 결과

| 시나리오 | 기대 | 실제 | 일치 | 위반 규칙 | head 통과/실패 | 초 |
|---|---|---|---|---|---|---|
| behavior/sum_skips_last_element | behavior_failure | behavior_failure | 예 | regression | 11/5 | 1.84 |
| weakened/killing_subtest_deleted | check_weakened | check_weakened | 예 | protected_modified, test_missing | 15/0 | 1.84 |
| weakened/t_skip_added_to_killing_subtest | check_weakened | check_weakened | 예 | test_skipped | 15/0 | 1.84 |
| incomplete/compile_error_in_source | verification_incomplete | verification_incomplete | 예 | regression, setup_or_collection_error, test_missing | 0/0 | 1.31 |
| incomplete/syntax_error_in_test_file | verification_incomplete | verification_incomplete | 예 | setup_or_collection_error, test_missing | 0/0 | 1.38 |
| weakened/subtest_renamed_with_mutant | check_weakened | check_weakened | 예 | new_test_fails, protected_modified, regression, test_missing | 14/2 | 1.86 |
| weakened/test_function_renamed_hint | check_weakened | check_weakened | 예 | protected_modified, test_missing | 16/0 | 2.20 |
| weakened/expected_value_changed_with_mutant | check_weakened | check_weakened | 예 | protected_modified | 16/0 | 1.85 |
| incomplete/untested_main_build_break | verification_incomplete | verification_incomplete | 예 | regression, setup_or_collection_error | 16/0 | 1.77 |
| ok/new_test_file | ok | ok | 예 | - | 17/0 | 1.85 |
| ok/refactor_rename_local | ok | ok | 예 | - | 16/0 | 1.83 |

### 집계

- 시나리오 11개, 기대와 일치 11개, 불일치 0개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건. 오경보: 0/2
- gate 실행 시간(base+head, 검사 2개): 평균 1.78초, 최대 2.20초

### 관찰

- `go test ./...`는 테스트 파일이 없는 패키지도 컴파일하고 실패를 `FAIL toy/cmd/toy [build failed]`로 보고한다(go1.27.1). 그래서 `go build ./...`만 실패하고 unit은 통과하는 변이는 만들 수 없다(F4 참조). 같은 변경을 `incomplete/untested_main_build_break`로 두고 실제 동작을 기대로 고정했다.
- 소스 컴파일 오류는 `setup_or_collection_error`(unit의 `[build]` error)와 build 검사의 `regression`, `test_missing`이 함께 나오고 incomplete가 이긴다.
- `t.Skip()` 추가는 줄만 추가하므로 `protected_modified` 없이 `test_skipped`만 나온다.

## TypeScript (vitest)

실행: `go test ./e2e -run TS -count=1 -v`. 장난감 프로젝트(`src/calc.ts`의 `sum`·`dedupe`·`clamp`, `src/calc.test.ts`의 `describe`/`it` 9개). 의존성은 테스트 프로세스당 한 번 `e2e/.cache/ts/node_modules`에 `npm ci`로 설치(lock 해시가 같으면 재사용, 네트워크 필요)하고 각 worktree에는 `setup: ln -s <절대경로>/node_modules node_modules`로 연결한다. npm 설치가 불가능하면 TS 시나리오는 실패하며 해당 행은 "환경 미충족"으로 기록된다(이번 실행은 설치 성공, 환경 미충족 0건). vitest 5.1.0, node v26.5.0, npm 11.17.0.

### 시나리오별 결과

| 시나리오 | 기대 | 실제 | 일치 | 위반 규칙 | head 통과/실패 | 초 |
|---|---|---|---|---|---|---|
| behavior/sum_skips_last_element | behavior_failure | behavior_failure | 예 | regression | 7/2 | 1.34 |
| weakened/it_skip_on_killing_test | check_weakened | check_weakened | 예 | protected_modified, test_skipped | 8/0 | 1.33 |
| weakened/killing_it_deleted | check_weakened | check_weakened | 예 | protected_modified, test_missing | 8/0 | 1.34 |
| weakened/it_title_renamed_with_mutant | check_weakened | check_weakened | 예 | new_test_fails, protected_modified, test_missing | 8/1 | 1.33 |
| weakened/test_file_renamed_hint | check_weakened | check_weakened | 예 | protected_modified, test_missing | 9/0 | 1.34 |
| weakened/assertion_weakened_to_truthy | check_weakened | check_weakened | 예 | protected_modified | 9/0 | 1.34 |
| incomplete/syntax_error_in_source | verification_incomplete | verification_incomplete | 예 | setup_or_collection_error, test_missing | 0/0 | 1.33 |
| ok/new_test_file | ok | ok | 예 | - | 10/0 | 1.36 |
| ok/refactor_sum_with_reduce | ok | ok | 예 | - | 9/0 | 1.33 |
| incomplete/no_node_modules_no_setup | verification_incomplete | verification_incomplete | 예 | base_unreliable, parse_failed | 0/0 | 1.25 |
| ok/real_npm_ci_setup_SLOW | ok | ok | 예 | - | 9/0 | 3.11 |

`ok/real_npm_ci_setup_SLOW`는 base와 head worktree 각각에서 실제 `npm ci`(커밋된 lockfile)를 실행한다(npm 캐시가 따뜻한 상태의 시간).

### 집계

- 시나리오 11개, 기대와 일치 11개, 불일치 0개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건. 오경보: 0/3
- gate 실행 시간: 평균 1.49초, 최대 3.11초 (최대는 실제 `npm ci` 시나리오)

### 관찰

- `ok/real_npm_ci_setup_SLOW`로 `setup: npm ci`가 base·head 양쪽에서 동작함을 확인했다.
- `it.skip`은 `it(` 줄을 바꾸므로 `test_skipped`와 `protected_modified`가 함께 나온다(Go의 `t.Skip()` 추가와 다름).
- 시나리오 작성 중 실제 `npm ci` 시나리오의 첫 실행이 behavior_failure였던 것은 시나리오 편집 오류(`Replace`가 첫 `total`만 바꿔 코드가 깨짐)였고 `ReplaceAll`로 고쳤다.

## 발견과 수정

Go·TS 최초 실행에서 54개 중 4개가 기대와 달랐다. 기대는 수정하지 않고 게이트를 고쳤다(단위 테스트를 먼저 작성해 RED를 확인한 뒤 수정).

- **F1 vitest 구문 오류가 incomplete가 아니라 check_weakened로 판정됨.** vitest JUnit은 파일 로드 실패를 `name == classname == testsuite name`(파일 경로)인 실패 testcase 1개로 낸다. 수정: `internal/adapter/junit.go`에서 `<failure>` testcase의 name·classname이 둘러싼 testsuite 이름과 모두 같으면 `error`로 매핑(스위트 수준 로드 실패 = 수집 오류). 지시서의 "name == classname"에 "testsuite 이름과도 같음" 조건을 더했다. jest-junit은 일반 테스트도 name == classname이라 지시서 그대로면 실패한 jest 테스트가 전부 `error`가 되기 때문이다(jest 스위트 이름은 `undefined`). 실제 vitest 출력을 `testdata/vitest_syntax_error.xml`에 고정하고 README에 명령을 기록했다. 기존 pytest/jest/vitest 픽스처 결과는 그대로이고 테스트로 확인한다. 결과: `incomplete/syntax_error_in_source`가 verification_incomplete(`setup_or_collection_error`)로 일치.
- **F2 실패 원인이 보고서에 안 보임.** 수정: 러너 출력의 끝 300자(공백 정리, 유효 UTF-8, rune 경계 절단)를 각 측 `RunInfo.Tail`로 보관하고, 결과가 없거나 오류·타임아웃인 측에 한해 규칙 `no_tests_ran`·`parse_failed`·`base_unreliable`·`timeout`·`setup_or_collection_error`·`base_not_green`의 detail 끝에 ` | 출력 끝: <tail>`을 붙인다(규칙 이름 불변, 규칙 판정은 Tail을 읽지 않음). 초기 모드와 setup 실패에도 적용. `incomplete/no_node_modules_no_setup`은 detail에 `ERR_MODULE_NOT_FOUND`가 보인다. 이 시나리오의 WantDetail은 코디네이터 지시에 따라 `node_modules`에서 실제 원인 문구 `ERR_MODULE_NOT_FOUND`로 바꿨다(출력 꼬리에는 `node_modules` 문자열이 없다). 이 시나리오는 이 변경만이 기대 수정이고, 그 외 기대는 모두 최초 그대로다.
- **F3 이름 변경 힌트 누락.** 수정: `internal/rules`의 `missingDetail`이 마지막 세그먼트 일치에 더해 "부모 경로(마지막 `::`/`/` 앞)가 같고 사라진 id와 새 id가 각각 정확히 1개"일 때도 힌트를 낸다(1:1이 아니거나 부모가 다르면 안 냄). Go 서브테스트(`pkg/TestA/old` → `new`)·vitest `describe > it` 제목 변경·모호한(2:1, 1:2)·무관한 경우를 규칙 단위 테스트로 추가. 결과: `weakened/subtest_renamed_with_mutant`(Go)와 `weakened/it_title_renamed_with_mutant`(TS)가 힌트를 포함해 일치.
- **F4 Go에서 build 검사만 실패하는 시나리오: 제외(수용).** `go test ./...`가 테스트 없는 패키지도 컴파일해 build 실패를 unit에서도 `[build failed]`(error)로 만들기 때문에 "빌드만 실패"를 만들 수 없다. 같은 변경을 `incomplete/untested_main_build_break`로 두었다. "base에서 되던 빌드가 head에서 실패 → behavior_failure" 규칙은 Go 종단 시나리오로는 닿지 않는다.

최종: Python 32/32, Go 11/11, TS 11/11 일치(전체 54개, 불일치 0개). `go test ./... -count=1` 전 패키지 통과.
