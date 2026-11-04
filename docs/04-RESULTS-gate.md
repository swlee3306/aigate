# 04 결과: aigate 게이트 종단 시나리오 (Python/pytest)

실행: `go test ./e2e -run Py -count=1 -v` (시나리오마다 실제 git 저장소를 만들고 `gate.Run` 실행). 기대 판정은 결과를 보기 전에 작성했고, 이후 수정하지 않았다.

## 시나리오별 결과

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

## 집계

- 시나리오 32개, 기대와 일치 32개, 불일치 0개
- 약화·불완전 시나리오에서 `ok`가 나온 경우(놓침): 0건
- 오경보율(정상 `ok` 시나리오 중 ok가 아닌 비율): 0/5 = 0%
- gate 실행 시간: 평균 0.75초, 최대 5.41초 (최대는 hang 시나리오의 5초 타임아웃 포함)
- 타임아웃 시나리오를 제외한 평균: 0.60초

## 불일치 (findings)

없음. 모든 시나리오가 기대 범주와 종료 코드, 지정한 규칙 이름과 일치했다.

## 관찰

- `skip` 마커·`xfail` 마커·`pytest.skip()` 호출은 보호 파일에서 기존 줄을 지우지 않고 줄만 추가하므로 `protected_modified`는 발생하지 않고, `test_skipped`(base에서 통과하던 테스트가 head에서 skip)로만 잡힌다. 이 경로가 유일한 방어선이다.
- `incomplete/import_error`는 `setup_or_collection_error`와 `test_missing`이 함께 나온다.
- `pytest.ini`의 `addopts = -k boundary`는 게이트가 `-o addopts=`로 무력화해 head에서도 9개가 모두 실행되었고 `ok`였다.
- 파일 이름 변경은 `test_missing`의 detail에 `이름 변경 의심` 힌트가 포함되었다.
- 한계: 시나리오는 pytest 한 종류의 장난감 프로젝트(9개 테스트) 한 개에 대한 것이며, 승인 흐름의 `ok`는 해시를 테스트가 직접 승인소에 넣어 검증했다(TTY 승인 경로는 이 스위트의 범위 밖).
