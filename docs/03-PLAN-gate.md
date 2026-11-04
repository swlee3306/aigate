# 검증 게이트(aigate) 구현 계획 (v0.1, 2026-11-04)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 기준(base)·대상(head) 커밋을 직접 실행·비교해 AI 변경을 판정하고 증거 보고서·이력을 남기는 Go 단일 바이너리 `aigate`를 만든다.

**Architecture:** 순수 함수 `rules`(정규화된 결과 → 위반·판정)를 중심에 두고, `gitx`(체크아웃·diff)·`runner`(타임아웃 실행)·`adapter`(러너 결과 정규화)·`approval`·`ledger`·`report`를 작은 패키지로 분리한다. `gate.Run`이 이들을 엮고 `cmd/aigate`가 CLI를 제공한다. 검증은 규칙 단위 테스트 → 어댑터 픽스처 → 실제 git 저장소 종단 시나리오 → 적대 검토 순서.

**Tech Stack:** Go 1.27(설치됨), `gopkg.in/yaml.v3`, 표준 라이브러리, git 2.54. 종단 시나리오: pytest(Python 3.14), `go test`, vitest(Node 26/npm 11).

**Spec:** `docs/02-DESIGN-gate.md`(승인됨), 요구사항 `docs/01-REQUIREMENTS.md`.

## Global Constraints
- 판정 우선순위 `verification_incomplete > check_weakened > behavior_failure > ok`, 종료 코드 0/1/2/3, 게이트 자체 오류 **64**(오류 시 절대 ok 금지).
- 불변식: base에서 `passed`이던 id는 head에서 같은 id로 실행되어 `passed`여야 한다. base에서 이미 실패하던 id는 `waived`.
- 재시도 없음. 컴파일·수집·설정 오류는 모두 `verification_incomplete`.
- `.aigate.yml`은 **base 커밋에서 읽는다**. 변경은 승인 대상(`protected`에 포함).
- 승인 단위는 보호 파일 diff의 해시(sha256, 앞 16자). 정확히 같을 때만 유효. `approve`는 TTY 없으면 거부. 승인은 `protected_modified` 위반만 해소.
- `approval_on_add` 경로에 새 파일이 추가되면 승인 대상. 일반 새 테스트 파일은 자유.
- 검사 실행 시 저장소 설정의 선택 범위 축소를 무력화(예: pytest `-o addopts=`).
- 보고서 `.aigate/reports/<head10>.json|.md`(git 미추적), 이력 `~/.aigate/ledger/<repo-id>.jsonl`(해시 체인), 승인 `~/.aigate/approvals.json`.
- 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus
1. **테스트 id 정규화**: 공백·유니코드·`[param]`·Go 서브테스트 이름이 base/head에서 같은 규칙으로 id가 되어야 한다. 다르면 가짜 `test_missing`이 쏟아진다 → Task 2.
2. **이름만 바꾼 테스트**: 삭제+추가로 보이므로 `check_weakened`(승인 필요). 상세에 "이름 변경 의심" 힌트를 준다 → Task 1.
3. **base == head / head가 base의 후손이 아님**: 같은 커밋이면 ok, 후손이 아니면 `64`가 아니라 명확한 위반(`base_not_ancestor`, incomplete) → Task 9.
4. **의존성이 설치되지 않은 깨끗한 worktree**(node_modules, .env 없음): 크래시 없이 `verification_incomplete`와 원인 문구 → Task 3·11.
5. **대량 테스트(1만 개)·큰 출력**: 출력은 꼬리 1500자만 보관, 메모리·시간이 선형이어야 한다 → Task 3.

## 파일 구조 (Go 모듈 `aigate`, 위치 `lab/code/aigate/`)
- `cmd/aigate/main.go` — CLI 분기(`run|check|approve|ledger`), 종료 코드
- `internal/rules/` — 순수 판정(`rules.go`, `rules_test.go`)
- `internal/adapter/` — `junit.go`, `gotestjson.go`, `exitcode.go`, `preset.go`, `testdata/`
- `internal/runner/` — `runner.go`(타임아웃·프로세스 그룹)
- `internal/config/` — `.aigate.yml` 파싱·검증
- `internal/gitx/` — git 호출 래퍼(해석, worktree, diff, dirty)
- `internal/approval/` — 해시·저장·TTY
- `internal/ledger/` — 추가 전용 해시 체인
- `internal/report/` — 스키마 v1, JSON·MD, 신선도 `Check`
- `internal/gate/` — `Run` 오케스트레이션
- `e2e/` — 시나리오 빌더와 스위트(`scenarios_py_test.go`, `_go_`, `_ts_`)
- `reference-py/` — 기존 Python 초안(참고용 이동)

---

### Task 0: 프로젝트 골격

**Files:** Create `go.mod`, `.gitignore`, `Makefile`, `cmd/aigate/main.go`(빈 main); Move `aigate/*.py` → `reference-py/`

- [ ] **Step 1:** `lab/code/aigate/`에서 `git init`, `go mod init aigate`, `go get gopkg.in/yaml.v3`. 기존 `aigate/` Python 패키지 디렉터리를 `reference-py/`로 이동(내용 변경 없음). `docs/`는 그대로.
- [ ] **Step 2:** `.gitignore`에 `.aigate/`, `bin/`, `*.test` 추가. `Makefile` 타깃: `build`(→`bin/aigate`), `test`(`go test ./...`), `e2e`(`go test ./e2e -count=1`).
- [ ] **Step 3:** 검증: `go build ./... && go vet ./...` 가 오류 없이 끝난다.
- [ ] **Step 4:** Commit `chore: scaffold aigate go module, move python draft to reference-py`.

### Task 1: 판정 규칙 (순수 함수)

**Files:** Create `internal/rules/rules.go`, `internal/rules/rules_test.go`

**Interfaces:**
- Produces:
  - `type Status string` (`passed|failed|error|skipped`), `type Results map[string]Status`
  - `type Category string` (`ok|behavior_failure|verification_incomplete|check_weakened`), `func (c Category) ExitCode() int` (0/1/2/3)
  - `type RunInfo struct{ Results Results; RC int; TimedOut bool; ParseFailed bool }`
  - `type Input struct{ CheckID string; Required bool; Base, Head RunInfo; Dirty bool; ProtectedRemoved int; ProtectedFiles []string; AddedGuardedFiles []string; ApprovalHashValid bool }`
  - `type Violation struct{ Rule string; Category Category; Detail string; IDs []string }`
  - `func Evaluate(in Input) (violations []Violation, waived []string)`
  - `func Verdict(vs []Violation) Category` (우선순위 적용)

- [ ] **Step 1:** 실패하는 표 기반 테스트 `TestEvaluate`: 사례별 기대 `Rule`과 `Category`. 사례: base 통과 id가 head에서 `failed`→`regression`(behavior); id 없음→`test_missing`(weakened); `skipped`→`test_skipped`(weakened); head `error`→`setup_or_collection_error`(incomplete); `TimedOut`→`timeout`(incomplete); 결과 0건→`no_tests_ran`(incomplete); base `failed`이던 id가 head에서도 실패→위반 없음+`waived`에 포함; `Dirty`→`dirty_tree`(incomplete); `ProtectedRemoved>0`이고 `ApprovalHashValid=false`→`protected_modified`(weakened), `true`이면 위반 없음; `AddedGuardedFiles` 비어있지 않고 승인 없음→`protected_modified`; 새 테스트 `failed`→`new_test_fails`; 새 테스트 `skipped`→`new_test_skipped`.
- [ ] **Step 2:** `TestVerdict`: 세 범주가 섞이면 `verification_incomplete`, weakened+behavior면 `check_weakened`, 위반 없으면 `ok`. `TestExitCode`: 0/1/2/3.
- [ ] **Step 3:** `TestRenameHint`: base 통과 id 하나가 사라지고 head에 같은 접미사의 새 id가 하나 생기면 `test_missing`의 `Detail`에 "이름 변경 의심"이 포함된다(Review Focus 2).
- [ ] **Step 4:** `go test ./internal/rules -v` 가 FAIL(미정의)임을 확인 후, 위 시그니처대로 구현. base가 불안정(`TimedOut`/결과 0건/`error` 포함)이면 `base_unreliable`, base가 `RC!=0`이면 `base_not_green`(둘 다 incomplete)을 낸다. 구현 후 `go test ./internal/rules -v` 전부 PASS.
- [ ] **Step 5:** Commit `feat(rules): invariant-based verdict rules`.

### Task 2: 어댑터와 id 정규화

**Files:** Create `internal/adapter/{junit.go,gotestjson.go,exitcode.go,preset.go}`, `internal/adapter/adapter_test.go`, `internal/adapter/testdata/{pytest.xml,vitest.xml,jest.xml,gotest.jsonl,gotest_buildfail.jsonl}`

**Interfaces:**
- Consumes: `rules.Results`, `rules.Status`
- Produces:
  - `type Parsed struct{ Results rules.Results; ParseFailed bool }`
  - `func ParseJUnit(path string) Parsed`
  - `func ParseGoTestJSON(r io.Reader) Parsed` (id = `패키지/Test/서브`, 빌드 실패 패키지는 `패키지[build]`=`error`)
  - `func ExitCode(id string, rc int) Parsed` (rc==0→passed)
  - `type Preset struct{ Command []string; Neutralize []string; Adapter string }`, `func PresetFor(name string) (Preset, bool)` (`pytest|go|vitest|jest`)
  - `func NormalizeID(s string) string` (앞뒤 공백 제거, 연속 공백 1칸, 유니코드 NFC)

- [ ] **Step 1:** 실제 러너 출력으로 픽스처 생성(각 러너를 toy 프로젝트에서 한 번 실행해 저장: 통과·실패·skip·xfail·수집 오류가 섞인 것). 스크립트는 `internal/adapter/testdata/README.md`에 명령으로 기록.
- [ ] **Step 2:** 실패 테스트: `TestParseJUnitPytest`는 픽스처에서 `passed=…, failed=…, skipped=…`(픽스처 값 그대로) 개수 단언; xfail→`skipped`; 수집 오류→`error`. `TestParseGoTestJSON`: 서브테스트 id가 `pkg/TestA/sub_case`; `gotest_buildfail.jsonl`→`error`. `TestParseFailed`: 깨진 XML→`ParseFailed=true`. `TestNormalizeID`: `"test  a "`→`"test a"`, NFC/NFD 한글이 같은 id가 됨(Review Focus 1).
- [ ] **Step 3:** 구현. 프리셋: pytest `python -m pytest -q`+`-o addopts=`, go `go test -json ./...`, vitest/jest는 `--reporter=junit`·jest-junit 대신 JUnit 출력 옵션(`$AIGATE_JUNIT` 경로 사용). 기본 `go test`는 `-count=1`을 포함한다.
- [ ] **Step 4:** `go test ./internal/adapter -v` PASS. Commit `feat(adapter): junit, go test json, exit-code adapters with id normalization`.

### Task 3: 러너 (타임아웃·프로세스 그룹)

**Files:** Create `internal/runner/runner.go`, `internal/runner/runner_test.go`

**Interfaces:**
- Produces: `type Spec struct{ Dir string; Argv []string; Env []string; Timeout time.Duration }`, `type Out struct{ RC int; TimedOut bool; Seconds float64; Tail string }`, `func Run(ctx context.Context, s Spec) Out` (타임아웃 시 프로세스 그룹 전체를 종료, `RC=124`)

- [ ] **Step 1:** 실패 테스트: `TestTimeoutKillsChildren`: `sh -c 'sleep 30 & wait'`를 1초 타임아웃으로 실행→`TimedOut`, 1.5초 안에 반환, 이후 `sleep` 프로세스가 남지 않음. `TestTailLimited`: 5MB 출력→`len(Tail)<=1500`. `TestEnvPassed`: `AIGATE_JUNIT`가 자식에 전달됨. `TestMissingBinary`: 없는 명령→`RC!=0`이고 패닉 없음(Review Focus 4·5).
- [ ] **Step 2:** 구현(`Setpgid`, 타임아웃 시 `-pgid`에 SIGKILL, 출력은 링 버퍼로 꼬리만 보관). `go test ./internal/runner -v` PASS.
- [ ] **Step 3:** Commit `feat(runner): process-group timeout execution`.

### Task 4: 설정 로드 (`.aigate.yml`, base에서 읽기)

**Files:** Create `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `type Check struct{ ID, Preset, Adapter string; Command []string; Timeout time.Duration; Required bool; Setup []string }`, `type Config struct{ Version int; Protected, ApprovalOnAdd []string; Checks []Check }`, `func Parse(data []byte) (Config, error)`, `func (c Config) Hash() string`
- **설계서 3.1에 없는 추가(검토 요청):** `Check.Setup`(선택). 각 worktree에서 검사 전에 한 번 실행(예: `npm ci`, 의존성 링크). 깨끗한 worktree에는 의존성이 없어 TS 검사가 불가능하므로 필요하다. 승인하지 않으면 TS는 `verification_incomplete`로만 나온다.

- [ ] **Step 1:** 실패 테스트: 설계서 3.1 예시 YAML이 파싱되어 `Checks[0].Preset=="pytest"`, `ApprovalOnAdd`에 `**/conftest.py` 포함; `version` 누락·알 수 없는 키·중복 `id`·`timeout` 형식 오류→에러; `preset`과 `adapter+command` 동시 미지정→에러; `Hash()`는 키 순서와 무관하게 같은 값.
- [ ] **Step 2:** 구현(`yaml.v3` KnownFields). 기본 `required: true`, 기본 `timeout: 300s`. `go test ./internal/config -v` PASS. Commit `feat(config): parse and validate .aigate.yml`.

### Task 5: git 래퍼

**Files:** Create `internal/gitx/gitx.go`, `internal/gitx/gitx_test.go`

**Interfaces:**
- Produces: `func Resolve(repo, ref string) (sha string, err error)`, `func MergeBase(repo, a, b string) (string, error)`, `func IsAncestor(repo, anc, desc string) (bool, error)`, `func Dirty(repo string) (string, error)`, `func Show(repo, sha, path string) ([]byte, error)`(base에서 설정 읽기), `func WithWorktree(repo, sha string, fn func(dir string) error) error`(항상 정리), `type ProtDiff struct{ Hash string; RemovedLines []string; TouchedFiles, FilesWithRemovals, AddedFiles []string }`, `func ProtectedDiff(repo, base, head string, protected []string) (ProtDiff, error)`, `func MatchGlob(pattern, path string) bool`(`**` 지원)

- [ ] **Step 1:** 실패 테스트(임시 git 저장소 헬퍼 사용): 파일 수정→`RemovedLines`에 지워진 줄, `FilesWithRemovals`; 파일 추가만→`RemovedLines` 비어있고 `AddedFiles`에 포함; 삭제된 파일→`RemovedLines` 포함; `Hash`는 같은 diff에서 같고 줄 하나 바뀌면 달라진다; `WithWorktree`는 fn이 에러/패닉이어도 디렉터리를 제거; `Dirty`는 추적 안 된 새 파일도 감지; `MatchGlob("tests/**","tests/a/b.py")` true, `("**/conftest.py","conftest.py")` true.
- [ ] **Step 2:** 구현(`git diff --no-renames -U0`, `:(glob)` 경로 스펙). `go test ./internal/gitx -v` PASS. Commit `feat(gitx): worktree checkout, protected diff and hashing`.

### Task 6: 승인

**Files:** Create `internal/approval/approval.go`, `internal/approval/approval_test.go`

**Interfaces:**
- Produces: `type Store struct{ Path string }`, `func (s Store) Grant(repoID, baseSHA, hash string, now time.Time) error`, `func (s Store) Valid(repoID, baseSHA, hash string) bool`, `func RequireTTY(stdin *os.File) error`

- [ ] **Step 1:** 실패 테스트: Grant 후 같은 `(repo,base,hash)`만 `Valid`; 해시 하나만 달라도 false; 파일이 없으면 false(에러 아님); 손상된 JSON→`Valid` false이고 `Grant`가 기존 내용을 덮어쓰지 않고 에러; `RequireTTY`는 파이프 stdin이면 에러.
- [ ] **Step 2:** 구현(원자적 쓰기: 임시 파일→rename, 권한 0600). PASS 후 Commit `feat(approval): hash-bound approvals with tty requirement`.

### Task 7: 이력(ledger)

**Files:** Create `internal/ledger/ledger.go`, `internal/ledger/ledger_test.go`

**Interfaces:**
- Produces: `type Event struct{ Seq int; TS string; PrevHash, Hash, Type, Repo string; Payload map[string]any }`, `func Append(path string, typ, repo string, payload map[string]any, now time.Time) error`, `func Verify(path string) (n int, err error)`

- [ ] **Step 1:** 실패 테스트: 3건 Append 후 `Verify`→n=3; 중간 줄의 payload를 바꾸면 에러(변조 위치 `seq` 포함); 줄을 삭제하면 에러; 동시에 두 프로세스가 Append해도 `seq`가 연속(파일 잠금); 빈/없는 파일 `Verify`→0.
- [ ] **Step 2:** 구현(`Hash=sha256(Seq|TS|PrevHash|Type|Repo|정렬된 payload JSON)`, `flock`). PASS 후 Commit `feat(ledger): append-only hash-chained event log`.

### Task 8: 보고서와 신선도

**Files:** Create `internal/report/report.go`, `internal/report/report_test.go`

**Interfaces:**
- Consumes: `rules.Violation`, `rules.Category`
- Produces: `type Report struct{ SchemaVersion int; Category rules.Category; ExitCode int; Repo, Base, Head, ConfigHash, ToolVersion, StartedAt, FinishedAt string; Checks []CheckResult; Violations []rules.Violation; Waived []string; Approvals []ApprovalState; Initial bool }`, `func (r Report) JSON() []byte`, `func (r Report) Markdown() string`, `func Check(r Report, headNow string, dirty bool, approvalValid func(hash string) bool) []string`(문제 목록, 비면 신선)

- [ ] **Step 1:** 실패 테스트: JSON 왕복 동일; 마크다운에 판정·위반 규칙 이름 포함; `Check`: head 불일치→"증거는 …"; dirty→문구; 판정 ok 아님→문구; 승인 필요 보고서의 승인이 무효→문구; 모두 정상→빈 목록.
- [ ] **Step 2:** 구현, PASS. Commit `feat(report): schema v1, markdown and freshness check`.

### Task 9: 오케스트레이션과 CLI

**Files:** Create `internal/gate/gate.go`, `internal/gate/gate_test.go`, `cmd/aigate/main.go`(채움)

**Interfaces:**
- Consumes: Task 1~8 전부
- Produces: `type Options struct{ Repo, Base, Head, TaskClass, ApproveHash, HomeDir string }`, `func Run(ctx context.Context, o Options) (report.Report, error)`(오류는 게이트 자체 오류만)
- CLI: `aigate run --repo --base [--head HEAD] [--task-class] [--approve HASH]`, `aigate check <report.json> --repo`, `aigate approve --repo --base --head`, `aigate ledger verify [--repo]`. 종료 코드는 판정(0~3), 자체 오류 64.

- [ ] **Step 1:** 실패 테스트(임시 git 저장소, 가짜 검사 `exit-code` 어댑터 사용): base==head→`ok`, 검사 base/head 각각 한 번씩 실행; head가 base의 후손이 아님→`base_not_ancestor`(incomplete); `base` 생략 시 `merge-base(HEAD, origin 기본 브랜치)`, 해석 불가면 **초기 모드**(회귀 비교 없이 head 녹색 여부, `Report.Initial=true`); `.aigate.yml`이 base에 없으면 `64`; `.aigate.yml`을 head에서만 바꿔도 base의 설정이 적용됨; `Run`이 이력에 `run_result` 한 줄을 추가; `ApproveHash`가 실제 diff 해시와 일치하고 승인 저장소에 있을 때만 `protected_modified`가 사라짐; 오류 경로에서 보고서 `Category`가 `ok`인 적이 없음.
- [ ] **Step 2:** `Run` 구현: 설정은 `gitx.Show(base, ".aigate.yml")`, 검사별 `WithWorktree`로 base·head 각각 실행(`Setup` 선행), 어댑터 결과→`rules.Evaluate`, 위반을 합쳐 `Verdict`, 보고서 저장(`.aigate/reports/`), 이력 Append. CLI 연결, `main`은 종료 코드를 반환.
- [ ] **Step 3:** `go test ./internal/gate -v` PASS, `make build && ./bin/aigate run --help`가 사용법을 출력. Commit `feat(gate): orchestration and CLI`.

### Task 10: 종단 시나리오 — Python(pytest)

**Files:** Create `e2e/builder.go`, `e2e/scenarios_py_test.go`, `e2e/testdata/py/{ingest.py,test_ingest.py,mutants.json}`(`verify-harness`의 `src/ingest.py`, `tests/test_ingest.py`와 변이 8종을 복사)

**Interfaces:**
- Produces: `type Scenario struct{ Name string; Mutate func(w *Workspace); Want rules.Category; WantRules []string; Approve bool }`, `func RunScenario(t *testing.T, s Scenario)`(base 커밋 생성→변경→head 커밋→`gate.Run`)

- [ ] **Step 1:** 시나리오 표 작성(기대 판정을 먼저 적는다): 변이 M1~M8 각각 `behavior_failure`; `hang`(무한 루프, 타임아웃 5초)·수집 오류(잘못된 import)·fixture 연결 오류→`verification_incomplete`; 테스트 삭제·`skip` 추가·`xfail` 추가·기대값 변경·단언 약화·허용오차 확대·`pytest.skip()` 호출→`check_weakened`(모두 변이본과 함께 통과시키도록 구성); conftest로 `pytest_collection_modifyitems` deselect→`check_weakened`; **새 `conftest.py` 추가**(변이본을 통과시키는 훅)→`check_weakened`(`approval_on_add`); `pytest.ini`에 `addopts=-k boundary` 추가→선택 무력화되어 `ok`; dirty 작업 트리→`verification_incomplete`; 정상: 새 테스트 추가, 리팩터링, 기준 변경 + 승인 해시→`ok`, 같은 변경 + 잘못된 해시→`check_weakened`.
- [ ] **Step 2:** `go test ./e2e -run Py -count=1 -v`로 실행. 실패하는 시나리오는 **규칙·구현을 고치고**(시나리오 기대를 맞추려 바꾸지 않는다) 재실행. 약화·불완전 시나리오에서 `ok`가 하나라도 나오면 합격 불가.
- [ ] **Step 3:** 모든 시나리오 PASS 후 시나리오별 실행 시간과 오경보(정상 시나리오가 ok가 아닌 횟수)를 `docs/04-RESULTS-gate.md`에 표로 기록. Commit `test(e2e): python scenario suite`.

### Task 11: 종단 시나리오 — Go와 TS/Vue

**Files:** Create `e2e/scenarios_go_test.go`, `e2e/scenarios_ts_test.go`, `e2e/testdata/{go,ts}/`

- [ ] **Step 1:** Go toy 모듈(함수 + `_test.go`)로 시나리오 약 8개: 변이→behavior, 테스트 삭제·`t.Skip()` 추가→weakened, 컴파일 오류→incomplete, 새 `_test.go` 추가(정상)→ok, 서브테스트 이름 변경→weakened(이름 변경 의심 힌트), 빌드 실패(`go build`)→behavior(base는 빌드됨).
- [ ] **Step 2:** TS toy(vitest) 시나리오 약 8개: 같은 종류 + `node_modules` 없는 깨끗한 worktree→`verification_incomplete`와 원인 문구(Review Focus 4), `setup: npm ci`를 둔 설정→정상 실행. 네트워크가 막혀 `npm ci`가 실패하면 그 시나리오는 `skip`이 아니라 "환경 미충족"으로 표에 명시한다.
- [ ] **Step 3:** `go test ./e2e -count=1 -v` 전체 PASS. 결과를 `docs/04-RESULTS-gate.md`에 추가. Commit `test(e2e): go and ts scenario suites`.

### Task 12: 적대 검토와 마감

**Files:** Create `docs/04-RESULTS-gate.md`(Task 10에서 시작), Modify `docs/serve.py`(문서 목록에 03·04 추가)

- [ ] **Step 1:** Codex에게 읽기 전용으로 설계서와 구현 요약을 주고 "게이트를 통과시키는 우회 시나리오 10개"를 요청(`--sandbox read-only`, 범위 명시). 제안된 우회를 e2e 시나리오로 추가해 실행하고, 뚫린 것은 규칙을 고친다. 못 고치는 것은 설계서 4장 한계에 추가한다.
- [ ] **Step 2:** 합격 3지표 중 게이트 범위 항목(불완전·무력화를 ok로 통과 = 0)을 시나리오 전체에서 확인하고, 참고 지표(오경보율, 실행 시간)를 기록한다.
- [ ] **Step 3:** `make test && make e2e` 전체 PASS를 확인하고 결과를 사용자에게 보고(합격 여부, 뚫린 우회, 한계). Commit `docs: gate results and adversarial review`.

---

## Self-Review
- **설계서 커버리지:** 3.1 설정(Task 4, 9), 3.2 어댑터(2), 3.3 규칙(1), 3.4 승인(6, 9), 3.5 신선도(8), 3.6 스키마(8), 3.7 종료 코드(1, 9), 3.8 실행 안전(3), 3.9 이력(7, 9), 초기 모드(9), 테스트 전략 1~5(1, 2, 10~12), 성공 지표 중 게이트 범위(10~12). 설계서 밖 추가는 `Setup` 하나이며 Task 4에 검토 요청으로 표시했다.
- **타입 일관성:** `rules.Status/Results/Category/Violation`을 어댑터·보고서·게이트가 같은 이름으로 쓴다. `gitx.ProtDiff` 필드가 `rules.Input`의 `ProtectedRemoved`, `ProtectedFiles`, `AddedGuardedFiles`로 채워진다(Task 9에서 매핑).
- **비율:** 코드 본문은 시그니처와 테스트 단언만 두었고 알고리즘 본문은 쓰지 않았다.
