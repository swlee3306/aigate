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

실행: `go test ./e2e -run Go -count=1 -v`. 장난감 모듈 `toy`(`calc` 패키지의 `Sum`·`Dedupe`·`Window`, 표 기반 테스트와 서브테스트 16개 통과, `cmd/toy`의 main 패키지). 검사 두 개: `unit`(프리셋 go)와 `build`(`go build ./...`, exit-code). 기대 판정은 실행 전에 작성했고 수정하지 않았다. head 통과/실패는 `unit` 기준.

### 시나리오별 결과

| 시나리오 | 기대 | 실제 | 일치 | 위반 규칙 | head 통과/실패 | 초 |
|---|---|---|---|---|---|---|
| behavior/sum_skips_last_element | behavior_failure | behavior_failure | 예 | regression | 11/5 | 1.83 |
| weakened/killing_subtest_deleted | check_weakened | check_weakened | 예 | protected_modified, test_missing | 15/0 | 1.78 |
| weakened/t_skip_added_to_killing_subtest | check_weakened | check_weakened | 예 | test_skipped | 15/0 | 1.87 |
| incomplete/compile_error_in_source | verification_incomplete | verification_incomplete | 예 | regression, setup_or_collection_error, test_missing | 0/0 | 1.33 |
| incomplete/syntax_error_in_test_file | verification_incomplete | verification_incomplete | 예 | setup_or_collection_error, test_missing | 0/0 | 1.40 |
| weakened/subtest_renamed_with_mutant | check_weakened | check_weakened | **아니오** | new_test_fails, protected_modified, regression, test_missing | 14/2 | 1.87 |
| weakened/test_function_renamed_hint | check_weakened | check_weakened | 예 | protected_modified, test_missing | 16/0 | 1.85 |
| weakened/expected_value_changed_with_mutant | check_weakened | check_weakened | 예 | protected_modified | 16/0 | 1.84 |
| incomplete/untested_main_build_break | verification_incomplete | verification_incomplete | 예 | regression, setup_or_collection_error | 16/0 | 1.85 |
| ok/new_test_file | ok | ok | 예 | - | 17/0 | 1.86 |
| ok/refactor_rename_local | ok | ok | 예 | - | 16/0 | 1.86 |

### 집계

- 시나리오 11개, 기대와 일치 10개, 불일치 1개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건. 오경보(정상 `ok` 시나리오 중 ok가 아닌 것): 0/2
- gate 실행 시간(시나리오당 base+head, 검사 2개): 평균 1.76초, 최대 1.87초

### 불일치 (findings)

1. **`weakened/subtest_renamed_with_mutant`**: 범주(check_weakened)·종료 코드·규칙은 일치했지만 `test_missing` detail에 `이름 변경 의심` 힌트가 **없다**. 원인: 힌트는 사라진 id와 새 id의 마지막 세그먼트(`/` 뒤)가 같을 때만 붙는데, 서브테스트 이름 자체를 바꾸면 마지막 세그먼트가 바뀌어 매칭되지 않는다(`.../TestWindow/full_span` → `.../TestWindow/entire_span`). 판정은 맞고(약화로 잡힘) 사람을 위한 힌트만 빠진다. 같은 이유로 이름이 바뀐 id가 head에서 실패하는 경우 `regression`(부모 테스트)·`new_test_fails`도 함께 나온다. 기대는 수정하지 않았다.
2. 시나리오 변경 없이 확인한 대조군 `weakened/test_function_renamed_hint`: 테스트 함수 이름만 바꾸면(서브테스트 이름 유지) 힌트가 붙는다. 즉 힌트는 "상위 이름이 바뀌고 마지막 세그먼트가 같은" 경우에만 동작한다.

### 관찰

- **빌드 검사만 볼 수 있는 변이는 이 Go 버전에서 만들 수 없다.** 브리프의 "`cmd/` main 패키지를 깨뜨리면 build 검사만 실패" 시나리오는 불가능했다. `go test ./...`는 테스트 파일이 없는 패키지도 컴파일하고 컴파일 실패를 `FAIL toy/cmd/toy [build failed]`(`go test -json`의 `FailedBuild`)로 보고하기 때문이다(go1.27.1에서 직접 확인). `go build ./...`가 실패하는 소스 변경은 항상 unit 검사에서도 빌드 오류(`error`)가 된다. 그래서 `behavior_failure`(빌드만 실패) 경로는 Go 스택에서 재현되지 않고, 대신 같은 변이를 `incomplete/untested_main_build_break`로 두고 실제 동작(`verification_incomplete`: unit의 빌드 오류 + build 검사 `regression`)을 기대로 고정했다(unit이 빌드 오류로 incomplete가 되므로 build 검사의 `regression`은 부수적이다). 즉 "base에서 되던 빌드가 head에서 실패 → behavior_failure" 규칙은 Go에서는 exit-code 어댑터 단위 테스트로만 검증되고 종단에서는 닿지 않는다.
- 소스 컴파일 오류(`incomplete/compile_error_in_source`)는 `setup_or_collection_error`(unit의 `[build]` error)와 `regression`(build 검사), `test_missing`이 함께 나오고 incomplete가 이긴다.
- `t.Skip()`을 서브테스트에 넣으면 보호 파일에서 줄만 추가되므로 `protected_modified` 없이 `test_skipped`만 나온다(Python 마커와 같은 구조).
- 서브테스트 삭제와 기대값 변경은 `protected_modified`로도 잡힌다(삭제·수정된 기존 줄).

## TypeScript (vitest)

실행: `go test ./e2e -run TS -count=1 -v`. 장난감 프로젝트(`src/calc.ts`의 `sum`·`dedupe`·`clamp`, `src/calc.test.ts`의 `describe`/`it` 9개). 의존성은 테스트 프로세스당 한 번 `e2e/.cache/ts/node_modules`에 `npm ci`로 설치(lock 해시가 같으면 재사용, 네트워크 필요)하고 각 worktree에는 `setup: ln -s <절대경로>/node_modules node_modules`로 연결한다. npm 설치가 불가능하면 TS 시나리오는 실패하며 해당 행은 "환경 미충족"으로 기록된다(이번 실행은 설치 성공, 환경 미충족 0건). vitest 5.1.0, node v26.5.0, npm 11.17.0.

### 시나리오별 결과

| 시나리오 | 기대 | 실제 | 일치 | 위반 규칙 | head 통과/실패 | 초 |
|---|---|---|---|---|---|---|
| behavior/sum_skips_last_element | behavior_failure | behavior_failure | 예 | regression | 7/2 | 1.57 |
| weakened/it_skip_on_killing_test | check_weakened | check_weakened | 예 | protected_modified, test_skipped | 8/0 | 1.32 |
| weakened/killing_it_deleted | check_weakened | check_weakened | 예 | protected_modified, test_missing | 8/0 | 1.34 |
| weakened/it_title_renamed_with_mutant | check_weakened | check_weakened | **아니오** | new_test_fails, protected_modified, test_missing | 8/1 | 1.33 |
| weakened/test_file_renamed_hint | check_weakened | check_weakened | 예 | protected_modified, test_missing | 9/0 | 1.32 |
| weakened/assertion_weakened_to_truthy | check_weakened | check_weakened | 예 | protected_modified | 9/0 | 1.34 |
| incomplete/syntax_error_in_source | verification_incomplete | check_weakened | **아니오** | new_test_fails, test_missing | 0/1 | 1.34 |
| ok/new_test_file | ok | ok | 예 | - | 10/0 | 1.34 |
| ok/refactor_sum_with_reduce | ok | ok | 예 | - | 9/0 | 1.35 |
| incomplete/no_node_modules_no_setup | verification_incomplete | verification_incomplete | **아니오** | base_unreliable, parse_failed | 0/0 | 1.29 |
| ok/real_npm_ci_setup_SLOW | ok | ok | 예 | - | 9/0 | 4.12 |

`ok/real_npm_ci_setup_SLOW`는 base와 head worktree 각각에서 실제 `npm ci`(커밋된 lockfile)를 실행한다(npm 캐시가 따뜻한 상태의 시간이다. 캐시가 비어 있으면 훨씬 길다).

### 집계

- 시나리오 11개, 기대와 일치 8개, 불일치 3개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건. 오경보: 0/3
- gate 실행 시간: 평균 1.60초, 최대 4.12초 (최대는 실제 `npm ci` 시나리오)

### 불일치 (findings)

1. **`incomplete/syntax_error_in_source`: 기대 verification_incomplete, 실제 check_weakened (종료 코드 3 대 2).** 설계서 3.3은 "수집·컴파일·설정 오류"를 `verification_incomplete`로 정하지만, vitest의 JUnit 보고서는 `src/calc.ts`의 구문 오류를 오류(`<error>`)가 아니라 파일 이름을 id로 한 **실패한 테스트케이스 1개**(`src/calc.test.ts::src/calc.test.ts`, `<failure>`)로 내보내고 나머지 테스트는 보고하지 않는다. 어댑터는 이를 `failed`로 읽어 base의 통과 id 9개가 모두 사라진 것(`test_missing`, check_weakened)과 새 실패 id(`new_test_fails`, behavior)로 판정했고 weakened가 behavior를 이긴다. 결과는 "ok가 아님"이라 안전한 방향이지만 범주가 틀렸고(원인은 코드 문제인데 "검사를 약화했다"로 보고됨) 사람에게 잘못된 사유가 간다. 기대는 수정하지 않았다. 개선 후보: junit 어댑터에서 id가 파일 경로와 같은 케이스(스위트 수준 실패)를 `error`로 매핑.
2. **`incomplete/no_node_modules_no_setup`: 범주는 일치(verification_incomplete)하지만 원인이 보이지 않는다.** `npx vitest run`이 `ERR_MODULE_NOT_FOUND`(vitest 패키지를 찾지 못함)로 종료되고 JUnit 파일이 만들어지지 않아 위반은 `base_unreliable`과 `parse_failed`(detail: "base 실행이 불안정합니다", "head 결과를 해석하지 못했습니다")뿐이다. 러너가 보관한 출력 꼬리(tail)는 보고서에 들어가지 않으므로 "node_modules가 없다"는 원인 문구가 위반에 나타나지 않는다. 시나리오는 위반 detail에 `node_modules`가 포함되어야 한다고 기대했고 이는 충족되지 않았다. 브리프 Step 2(원인 문구)가 요구한 동작이 구현에 없다.
3. **`weakened/it_title_renamed_with_mutant`**: Go finding 1과 같은 원인으로 `이름 변경 의심` 힌트가 없다. TS id는 `파일::describe > 제목`이고 마지막 세그먼트가 `describe > 제목` 전체라서 제목이나 describe 이름을 바꾸면 힌트가 불가능하다. 대조군 `weakened/test_file_renamed_hint`(파일 이름만 변경)는 힌트가 붙었다. 판정(check_weakened)은 맞다.

### 관찰

- 시나리오 `ok/real_npm_ci_setup_SLOW`로 `setup: npm ci`가 base·head 양쪽에서 실제로 동작하고 lockfile만으로 재현 가능한 실행이 됨을 확인했다.
- `it.skip`은 `it(` 줄을 바꾸므로 `test_skipped`와 `protected_modified`가 함께 나온다(Go의 `t.Skip()` 추가와 다름).
- 시나리오 작성 중 실제 `npm ci` 시나리오의 첫 실행이 behavior_failure였던 것은 게이트 문제가 아니라 시나리오 편집 오류(`Replace`가 첫 `total`만 바꿔 코드가 깨짐)였고 `ReplaceAll`로 고쳤다(기대는 처음부터 `ok`). 이 외에 기대를 바꾼 시나리오는 없다.
