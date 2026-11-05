# 오케스트레이터(②) 구현 계획 (v0.1, 2026-11-05)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 홈서버 VM 3개(에이전트·검사·오케스트레이터) 위에서 AI 작업을 큐로 받아 에이전트 실행 → 커밋 회수 → 샌드박스 밖 판정 → 승인 대기·허용 목록 실행 → 자율 단계 관리까지 하는 `orchd`/`orch`를 만든다.

**Architecture:** 판정하는 `aigate`는 오케스트레이터 VM에서 돌고, 검사 명령 실행만 검사 VM에서 한다(`gate.Executor` 추상화 + ssh 구현). `orchd`(Go, SQLite 상태 머신)가 에이전트 VM·검사 VM을 오케스트레이터가 먼저 거는 제한 ssh로만 다룬다. 사람은 Tailscale ssh로 접속해 터미널에서 승인한다.

**Tech Stack:** Go 1.27(기존 모듈 `aigate`에 `cmd/orchd`, `cmd/orch`, `internal/orch/...` 추가), SQLite(`modernc.org/sqlite`, cgo 없음), 시스템 `ssh`/`git`/`openstack` CLI, cloud-init, nftables, systemd.

**Spec:** `docs/07-DESIGN-orchestrator.md`(승인됨), `docs/06-REQUIREMENTS-orchestrator.md`(OR1~OR11), 게이트 `docs/02-DESIGN-gate.md`. 코드 위치: `lab/code/aigate/`(브랜치 `impl/orch`를 `impl/gate`에서 새로 만든다).

## Global Constraints
- 판정은 오케스트레이터 VM에서만 한다. 검사 VM이 돌려준 값(JUnit, `go test -json`, 종료 코드, 출력)은 **신뢰하지 않는 입력**이다: 크기 상한(JUnit/JSON 각 8 MiB, 출력 꼬리 1500자), 엄격한 파싱, 타임아웃.
- 오케스트레이터가 항상 먼저 연결한다. 에이전트·검사 VM에서 오케스트레이터로 거는 경로를 만들지 않는다.
- 에이전트 VM에는 구독 로그인만, 검사 VM에는 자격증명이 전혀 없다. 저장소 쓰기 토큰, 승인(`~/.aigate/approvals.json`)·이력은 오케스트레이터 VM에만 둔다.
- 방화벽은 VM 안쪽(nftables, 기본 차단 + 허용 목록). 홈서버 보안그룹은 적용되지 않으므로 의존하지 않는다.
- 인프라 오류는 `error`(재시도), 판정 결과와 분리한다. **오류를 `ok`로 처리하지 않는다.**
- 파괴적 행위(삭제, 운영 서버·계정 권한 변경)는 코드에서 하드 차단한다. 정책 파일로 풀 수 없다.
- 승인은 사람이 터미널에서 직접 한다(TTY 요구는 `aigate approve` 그대로). 에이전트·검사 VM은 승인·이력 파일에 접근할 수 없다.
- 홈서버에 실제 변경을 가하는 단계(VM 생성, 방화벽 적용, 스냅샷)는 스크립트 기본이 **dry-run**이고, `--apply`는 사용자 확인 후에만 실행한다. 기존 VM·서비스는 건드리지 않는다.
- 모든 커밋 메시지 끝에 `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- 리뷰 방침(사용자 요청): 작업별 리뷰 1회 + 수정 1회까지, 전체 리뷰는 마지막에 1회. 새 변형을 라운드로 계속 막는 방식은 하지 않는다. 남는 위험은 문서화한다.

## Review Focus (스펙이 말하지 않지만 부딪힐 수 있는 상황 5개)
1. **검사 VM이 거짓·거대·깨진 결과를 돌려줄 때:** 위조 JUnit, 8 MiB 초과 출력, 잘린 XML, 끝나지 않는 명령 → 판정은 `ok`가 되면 안 되고 `verification_incomplete`가 된다 → Task 4·5.
2. **`orchd`가 단계 중간에 죽을 때:** 재시작 후 같은 단계를 다시 실행해도 결과가 같다(멱등), 중복 승인 항목이 생기지 않는다 → Task 7.
3. **스냅샷 복원이 느리거나 실패할 때:** 작업은 `error`로 재시도 상한 후 `failed`, VM은 사용 중지 표시 → Task 9.
4. **에이전트가 커밋 없이 끝나거나 거대한 커밋·수천 개 브랜치를 만들 때:** 수집 단계가 상한으로 거부하고 `failed`로 기록 → Task 9.
5. **같은 작업의 승인 대기가 중복·만료·취소될 때:** 승인 해시가 달라지면(새 커밋) 이전 승인이 무효이고 항목이 갱신된다 → Task 11.

## 파일 구조 (모듈 `aigate`)
- `infra/` — VM 생성·방화벽·스냅샷 스크립트와 cloud-init 템플릿, 검증 스크립트, `discovery.md`
- `internal/gate/executor.go` — `Executor` 인터페이스와 로컬 구현(기존 동작 이동)
- `internal/remote/` — ssh 실행기, 입력 상한, 번들 전송
- `internal/orch/store/` — SQLite 스키마와 접근 함수
- `internal/orch/machine/` — 상태 머신, 단계 러너, 복구
- `internal/orch/vm/` — 에이전트 VM·검사 VM 드라이버(번들 전달, 커밋 회수, 스냅샷 복원)
- `internal/orch/policy/` — `policy.yml`, 자율 단계, 허용 목록, 하한선
- `internal/orch/approvals/` — 승인 대기 목록, 승인 연동
- `internal/orch/publish/` — 허용 목록 행위(브랜치 push, Draft PR, 알림)
- `cmd/orchd/`, `cmd/orch/` — 데몬과 CLI
- `e2e/` — 기존 e2e를 백엔드 파라미터로 재사용 + 적대 시나리오

---

### Task 0: 브랜치와 현장 조사 (읽기 전용)

**Files:** Create `infra/discovery.md`

**Interfaces:** Produces: 이후 Task 1이 쓸 값(이미지 이름, flavor, 네트워크, 키페어, 볼륨 타입)을 `infra/discovery.md`에 기록한다.

- [ ] **Step 1:** `lab/code/aigate/`에서 `git checkout -b impl/orch`(기점 `impl/gate`).
- [ ] **Step 2:** 홈서버(`ssh openstack-host`)에서 **읽기 전용** 명령으로 조사한다: `openstack image list`, `flavor list`, `network list`, `subnet list`, `keypair list`, `volume type list`, `server list`, `quota show`(관리자 인증 정보가 어디 있는지 찾는다. 값은 출력하지 않는다). 우분투 클라우드 이미지가 있는지, 없으면 업로드해야 하는지를 기록한다.
- [ ] **Step 3:** 기존 VM과 네트워크(어떤 IP 대역, floating IP 유무, Tailscale 연결 방식)를 `discovery.md`에 요약하고, VM 3개의 후보 사양(오케스트레이터 1vCPU/2GB/10GB, 에이전트 2/4/20, 검사 4/6/30, 모두 Cinder 볼륨 부팅)과 필요한 IP·이름을 제안한다.
- [ ] **Step 4:** 검증: 조사 명령이 아무것도 변경하지 않았음을 `server list`·`volume list` 전후 비교로 확인한다. Commit `docs(infra): homeserver discovery for orchestrator VMs`.

### Task 1: VM 3개 생성 스크립트 (기본 dry-run)

**Files:** Create `infra/create-vms.sh`, `infra/cloud-init/{orchestrator,agent,test}.yaml`, `infra/lib.sh`, `infra/README.md`

**Interfaces:**
- Consumes: Task 0의 `discovery.md` 값
- Produces: `infra/create-vms.sh [--apply] [--only orchestrator|agent|test]`; 이름은 `orch-vm`, `agent-vm`, `test-vm`; 각 VM에 ssh 키 로그인만 허용되는 사용자 `orch`/`agent`/`runner`.

- [ ] **Step 1: 실패하는 점검 작성** `infra/test/check-create-dryrun.sh`: `create-vms.sh`(인자 없음)가 어떤 `openstack ... create` 명령도 **실행하지 않고** 계획만 출력하는지(`--apply` 없이 `openstack server list` 개수 불변), 이미 있는 VM 이름이면 건너뛰는지(멱등)를 검사. Run: `sh infra/test/check-create-dryrun.sh` → FAIL(스크립트 없음).
- [ ] **Step 2:** `create-vms.sh`를 구현한다(`openstack` CLI, cloud-init 사용자 데이터, Cinder 부팅 볼륨, 기존 이름 있으면 건너뜀, `--apply` 없으면 명령 출력만). cloud-init에서 하는 일: 사용자·ssh 공개키, 패키지(공통 `git`, `ca-certificates`; 검사 VM `golang`, `nodejs`, `npm`, `python3-venv`; 에이전트 VM `git`, `nodejs`/`claude`·`codex` 설치 절차는 Task 10), nftables 규칙 파일 배치(Task 2).
- [ ] **Step 3:** 점검 재실행 → PASS. `shellcheck`가 있으면 통과.
- [ ] **Step 4 (사용자 확인 필요):** 사용자에게 계획 출력(dry-run)을 보여 주고 `--apply` 승인을 받은 뒤 VM을 **하나씩** 만든다(오케스트레이터 → 에이전트 → 검사). 한 VM마다 `ssh`로 접속되는지, 홈서버 메모리·CPU 변화가 기존 서비스에 영향이 없는지 확인한다. 이상이 있으면 중단한다.
- [ ] **Step 5:** Commit `feat(infra): create orchestrator VMs script with dry-run default`.

### Task 2: 방화벽·스냅샷 스크립트와 검증

**Files:** Create `infra/nftables/{orchestrator,agent,test}.nft`, `infra/verify-firewall.sh`, `infra/snapshot.sh`, `infra/test/check-nft-syntax.sh`

**Interfaces:** Produces: `infra/snapshot.sh create|restore|time <vm>`(Cinder 스냅샷, 복원 시간을 초 단위로 출력), `infra/verify-firewall.sh`(각 VM에서 시도하는 연결 목록과 기대 결과 표 출력, 기대와 다르면 비0 종료).

- [ ] **Step 1: 실패하는 점검** `check-nft-syntax.sh`: 세 `.nft` 파일이 `nft -c -f`(문법 검사)를 통과하는지, 기본 정책이 `drop`인지, 에이전트·검사 VM에 오케스트레이터 방향 outbound 허용 규칙이 **없는지** 검사.
- [ ] **Step 2:** 규칙 작성. 오케스트레이터: 사용자 Tailscale ssh 허용, 에이전트·검사 VM ssh 나가기 허용, GitLab 읽기·쓰기, 그 외 차단. 에이전트: 오케스트레이터의 ssh 들어오기만, 나가기는 LLM API 도메인 IP 집합(주기적 갱신 스크립트로 `nft set` 갱신)과 DNS만. 검사: 오케스트레이터 ssh 들어오기만, 나가기는 패키지 저장소(Go 프록시, npm, PyPI)만. LAN 대역(Mac mini, gpu-server, 관리망)은 모두 차단.
- [ ] **Step 3:** `verify-firewall.sh` 작성: 에이전트·검사 VM에서 Mac mini·gpu-server·오케스트레이터·상대 VM으로 접속 시도(전부 실패 기대), 허용 목록 밖 도메인 접속 시도(실패 기대), 허용 도메인 접속(성공 기대). 결과 표를 `infra/firewall-report.md`로 저장.
- [ ] **Step 4 (사용자 확인 필요):** `--apply`로 규칙을 적용하고 `verify-firewall.sh` 실행. 실패(LAN 접근 성공 등)가 있으면 규칙을 고쳐 재시도. 이어서 `snapshot.sh`로 검사 VM의 깨끗한 상태 스냅샷을 만들고 복원 시간을 3회 측정해 `infra/firewall-report.md`에 기록(설계서 가정 검증).
- [ ] **Step 5:** Commit `feat(infra): nftables rules, firewall verification and snapshot restore timing`.

### Task 3: `gate.Executor` 추출 (동작 불변 리팩터링)

**Files:** Create `internal/gate/executor.go`, `internal/gate/executor_local.go`; Modify `internal/gate/exec.go`, `internal/gate/gate.go`; Test `internal/gate/executor_test.go`

**Interfaces:**
- Produces:
  - `type CheckRequest struct{ SHA string; CheckID string; Argv, Setup, Env []string; Adapter string; Timeout time.Duration }`
  - `type CheckOutput struct{ RC int; TimedOut bool; Tail string; Seconds float64; JUnit []byte; GoTestJSON []byte }`
  - `type Executor interface{ Prepare(ctx context.Context, repo string, shas []string) error; RunCheck(ctx context.Context, req CheckRequest) (CheckOutput, error); Close() error }`
  - `gate.Options.Executor Executor`(nil이면 로컬)

- [ ] **Step 1:** 기존 `go test ./... -count=1`이 통과하는 상태에서 시작해 결과를 기록한다. 로컬 실행기 동작을 고정하는 테스트를 먼저 쓴다: `TestLocalExecutorMatchesLegacy`(같은 입력에 대해 리팩터링 전 결과와 같은 `RC`·`Tail`·파싱 결과), `TestRunCheckBoundsOutput`(JUnit 8 MiB 초과 시 오류).
- [ ] **Step 2:** `exec.go`의 worktree 생성·명령 실행·결과 파일 읽기를 `LocalExecutor`로 옮기고 `gate.Run`이 `Executor`를 통해서만 검사를 실행하게 한다. 어댑터 파싱은 바이트 입력(`JUnit`, `GoTestJSON`)으로 받게 바꾼다.
- [ ] **Step 3:** 전체 `go test ./... -count=1`과 `go test ./e2e -count=1`이 리팩터링 전과 **같은 결과**(132/132)로 통과하는지 확인한다. 하나라도 달라지면 원인을 고친다(기대값을 바꾸지 않는다).
- [ ] **Step 4:** Commit `refactor(gate): extract Executor interface with local implementation`.

### Task 4: ssh 원격 실행기

**Files:** Create `internal/remote/ssh.go`, `internal/remote/limits.go`, `internal/remote/bundle.go`, `internal/remote/ssh_test.go`

**Interfaces:**
- Consumes: Task 3의 `gate.Executor`, `CheckRequest`, `CheckOutput`
- Produces: `func NewSSH(cfg Config) (*SSH, error)`(`Config{Host, User, KeyPath, RepoDir, Limits}`), `*SSH`가 `gate.Executor`를 구현. 검사 VM에서 `git worktree add`로 커밋을 꺼내 `setup`→검사 명령 실행→결과 파일을 `cat`으로 읽어 반환. 시스템 `ssh`를 `exec.Command`로 호출(`BatchMode=yes`, `StrictHostKeyChecking=yes`, `ConnectTimeout`, 알려진 호스트 파일 지정).

- [ ] **Step 1: 실패하는 테스트 작성(가짜 ssh)** 환경 변수 `AIGATE_SSH_BIN`으로 `ssh` 대신 **로컬에서 명령을 실행하는 가짜 ssh 스크립트**(`internal/remote/testdata/fakessh.sh`)를 쓴다. 테스트: `TestPrepareSendsBundle`(커밋이 VM의 베어 저장소에 도착), `TestRunCheckReturnsParsedInputs`(JUnit·JSON이 돌아옴), `TestTimeoutKillsRemote`(타임아웃 시 원격 프로세스 그룹 종료), `TestHostKeyUnknownFails`(알려지지 않은 호스트는 연결 실패, 우회 옵션 없음).
- [ ] **Step 2: 적대 테스트 작성** 가짜 ssh가 VM 역할로 **나쁜 출력**을 돌려주게 한다: `TestForgedJUnitRejected`(스키마·크기 위반), `TestOversizedOutputRejected`(8 MiB 초과는 오류, 메모리 상한), `TestTruncatedXMLRejected`, `TestEndlessOutputBounded`(타임아웃 내 종료, 출력 꼬리만 보관), `TestRCLieDoesNotPass`(VM이 `rc=0`을 말해도 JUnit이 없으면 결과 없음으로 처리). 모두 `gate`에서 `verification_incomplete`가 되는지 `gate` 수준 테스트도 하나 둔다.
- [ ] **Step 3:** `SSH`를 구현한다. 환경 변수는 `CheckRequest.Env` 허용 목록 값만 전달한다(원격 명령 문자열은 인자 이스케이프를 한 곳에서 처리하고 셸 보간을 쓰지 않는다: `ssh host -- <고정 래퍼> <인자…>` 형태). 결과 읽기는 `head -c LIMIT+1`로 상한을 원격에서 먼저 건다.
- [ ] **Step 4:** `go test ./internal/remote ./internal/gate -count=1` PASS. Commit `feat(remote): ssh executor with untrusted-input limits`.

### Task 5: 원격 실행기 동등성 e2e와 적대 시나리오

**Files:** Modify `e2e/builder.go`; Create `e2e/scenarios_remote_test.go`

**Interfaces:** Produces: 환경 변수 `AIGATE_E2E_BACKEND=local|fakessh|vm`(기본 `local`). `vm`이면 Task 1에서 만든 실제 검사 VM을 쓴다(없으면 건너뛰지 않고 실패 메시지로 알림).

- [ ] **Step 1:** 기존 e2e(132개)를 `fakessh` 백엔드로도 돌릴 수 있게 빌더가 `Options.Executor`를 주입하도록 한다. 시나리오 기대값은 **바꾸지 않는다**.
- [ ] **Step 2:** `AIGATE_E2E_BACKEND=fakessh go test ./e2e -count=1` 결과가 `local`과 **판정 단위로 동일**한지 비교하는 스크립트(`e2e/compare-backends.sh`)를 만든다. 다른 시나리오는 원인을 고친다.
- [ ] **Step 3:** 적대 시나리오(위조 JUnit, 거대 출력, 무한 출력, 형식 깨짐, 원격 `rc` 거짓말)를 `scenarios_remote_test.go`에 넣는다. 기대: 전부 `verification_incomplete`, `ok` 없음.
- [ ] **Step 4 (사용자 확인 필요, 검사 VM 준비 후):** `AIGATE_E2E_BACKEND=vm`으로 대표 시나리오(각 스택 3개씩)를 실제 VM에서 실행해 판정이 같은지, 시간이 얼마나 늘었는지 기록한다(`docs/09-RESULTS-orchestrator.md`).
- [ ] **Step 5:** Commit `test(e2e): remote backend parity and hostile VM scenarios`.

### Task 6: 상태 저장소 (SQLite)

**Files:** Create `internal/orch/store/store.go`, `schema.sql`, `store_test.go`

**Interfaces:**
- Produces: `type Store`, `func Open(path string) (*Store, error)`; `Task{ID, Repo, Base, Prompt, Class, Agent, State, Attempts, CreatedAt, UpdatedAt, ErrorText}`; `func (s *Store) CreateTask(ctx, Task) (int64, error)`, `Transition(ctx, id int64, from, to State, note string) error`(현재 상태가 `from`일 때만 성공, 한 트랜잭션에서 `task_events`에 기록), `NextRunnable(ctx) (Task, bool, error)`, `AddApproval(ctx, Approval) (int64, error)`, `PendingApprovals(ctx)`.

- [ ] **Step 1: 실패하는 테스트:** `TestTransitionRequiresExpectedState`(다른 상태에서의 전이는 거부), `TestTransitionIsAtomicWithEvent`(전이와 이벤트가 함께 기록되거나 둘 다 안 됨), `TestConcurrentTransitionsOneWins`(동시 전이 시 하나만 성공), `TestReopenKeepsState`(닫았다 다시 열어도 상태 유지), `TestIdempotentApprovalPerTaskHash`(같은 작업·같은 해시의 승인 항목은 중복 생성되지 않음).
- [ ] **Step 2:** WAL 모드, 외래 키, 스키마 버전 테이블, 마이그레이션 함수를 구현한다. 파일 권한 0600.
- [ ] **Step 3:** 테스트 PASS. Commit `feat(orch): sqlite state store with atomic transitions`.

### Task 7: 상태 머신과 복구

**Files:** Create `internal/orch/machine/machine.go`, `transitions.go`, `machine_test.go`

**Interfaces:**
- Consumes: Task 6의 `store`
- Produces: `type State string`(`queued preparing agent_running collecting verifying needs_approval passed publishing done failed cancelled error`), `var Allowed map[State][]State`, `type Step interface{ Name() State; Run(ctx context.Context, t store.Task) (Outcome, error) }`, `type Outcome struct{ Next State; Note string }`, `func (m *Machine) Tick(ctx) error`(실행할 작업 하나를 한 단계 진행), `func (m *Machine) Recover(ctx) error`(재시작 시 실행 중이던 단계를 `error`가 아닌 **다시 실행 가능** 상태로 되돌림).

- [ ] **Step 1: 실패하는 테스트:** `TestAllowedTransitionsTable`(허용·금지 전이를 표로 검사, 금지 전이 시도는 오류), `TestCrashDuringStepResumesSameStep`(가짜 스텝이 중간에 패닉/종료한 뒤 `Recover` → 같은 단계를 다시 실행, 결과가 한 번만 반영됨), `TestInfraErrorNeverBecomesPassed`(스텝이 인프라 오류를 반환하면 `error`/재시도, 상한 후 `failed`이고 `passed`가 되지 않음), `TestCancelStopsFurtherSteps`, `TestAttemptLimit`.
- [ ] **Step 2:** 구현. 단계 실행 전 `Transition(from→running 표시)`와 실행 후 `Transition(→next)` 사이에 죽으면 `Recover`가 마지막 이벤트로 복구한다.
- [ ] **Step 3:** PASS. Commit `feat(orch): task state machine with crash recovery`.

### Task 8: `orch` CLI와 로컬 API

**Files:** Create `cmd/orchd/main.go`, `cmd/orch/main.go`, `internal/orch/api/api.go`, `api_test.go`

**Interfaces:**
- Produces: `orchd`가 유닉스 소켓(`/run/orchd/orchd.sock`, 0600)에서 JSON API 제공: `POST /tasks`, `GET /tasks`, `GET /tasks/{id}`, `POST /tasks/{id}/cancel`, `GET /approvals`. `orch submit --repo R --base B --prompt P --class C --agent claude|codex`, `orch list`, `orch show ID`, `orch cancel ID`. 승인 명령은 Task 11.

- [ ] **Step 1: 실패하는 테스트:** API 라운드트립(제출→조회), 잘못된 입력 거부(빈 프롬프트, 알 수 없는 에이전트, 허용되지 않은 저장소 경로), 소켓 권한 0600, 동시 제출.
- [ ] **Step 2:** 구현. `orchd`는 `Machine.Tick` 루프(동시 실행 수는 `policy`에서, 기본 1)와 API를 같이 띄운다. systemd 유닛 파일을 `infra/systemd/orchd.service`로 둔다.
- [ ] **Step 3:** PASS. Commit `feat(orch): orchd daemon and orch CLI over local socket`.

### Task 9: VM 드라이버 (번들 전달, 에이전트 실행, 커밋 회수, 스냅샷 복원)

**Files:** Create `internal/orch/vm/agent.go`, `test.go`, `snapshot.go`, `vm_test.go`; Create `infra/agent-runner/agent-runner`(에이전트 VM에 배치할 래퍼), `infra/agent-runner/authorized_keys.example`

**Interfaces:**
- Produces: `AgentVM{Host,KeyPath}`: `Prepare(ctx, task, repo, baseSHA string) error`(`git bundle` 전달·작업 폴더 초기화), `Run(ctx, task, agent, prompt string, limit time.Duration) (RunResult, error)`(래퍼 호출), `Collect(ctx, task, localRepo string) (headSHA string, commits int, err error)`(읽기 전용 fetch, 커밋 수·크기 상한). `TestVM{...}`: `Restore(ctx) error`(Cinder 스냅샷 복원, 시간 기록), `Executor() gate.Executor`. 래퍼는 `agent-runner prepare|run|collect-info`만 받고 그 밖의 명령은 거부한다(authorized_keys `command=`).

- [ ] **Step 1: 실패하는 테스트(가짜 ssh)** `TestCollectRejectsNoCommit`, `TestCollectRejectsHugeHistory`(커밋 수·객체 크기 상한), `TestCollectRejectsExtraRefs`(수천 브랜치/태그는 무시하고 지정 ref만), `TestWrapperRejectsUnknownSubcommand`, `TestRunTimeoutKillsAgent`, `TestPrepareResetsWorkspace`, `TestRestoreFailureMarksVMUnusable`, `TestRestoreSlowIsRecorded`.
- [ ] **Step 2:** 구현. `Collect`는 에이전트 VM에서 오케스트레이터로 거는 경로가 아니라 **오케스트레이터가 `git fetch`(ssh upload-pack 읽기 전용)**로 가져오며, 가져온 객체는 별도 네임스페이스 ref(`refs/orch/<task>/head`)에만 둔다. 스냅샷 복원은 `openstack` CLI를 호출하고 `Restore`가 끝나 ssh가 응답할 때까지 상한 안에서 대기한다.
- [ ] **Step 3:** PASS. Commit `feat(orch): agent and test VM drivers with bounded collection`.

### Task 10: 에이전트 러너와 로그인 (실제 VM, 사용자 참여)

**Files:** Create `infra/agent-runner/README.md`, `infra/agent-runner/install.sh`

**Interfaces:** Produces: 에이전트 VM에서 `agent-runner run <task> <claude|codex>`가 프롬프트 파일을 읽어 `claude -p` 또는 `codex exec`를 작업 폴더에서 실행하고 종료 코드·로그 꼬리를 낸다.

- [ ] **Step 1:** `install.sh`가 에이전트 VM에 Node, `claude`, `codex` CLI와 래퍼, `authorized_keys`의 `command=` 제한 키를 설치한다(`--apply` 없으면 dry-run).
- [ ] **Step 2 (사용자 직접):** 로그인은 사용자가 한 번 한다: Claude는 `claude setup-token`으로 만든 장기 토큰, Codex는 `codex login --device-auth` 또는 `--with-access-token`. **토큰을 이 대화나 저장소에 붙이지 않는다.** 에이전트 VM의 `~/.config/...`에 600 권한으로만 저장한다. 절차를 `README.md`에 단계별로 적는다.
- [ ] **Step 3 (사용자 확인 필요):** 로그인 후 에이전트 VM에서 작은 작업(예: 임시 저장소에 파일 하나 만들고 커밋)을 `agent-runner run`으로 실행해 동작하는지, 로그인 정보가 허용 목록 밖으로 나가지 않는지(`verify-firewall.sh` 재실행) 확인한다.
- [ ] **Step 4:** Commit `feat(infra): agent runner install and login procedure`.

### Task 11: 승인 대기 목록, 허용 목록 실행, 하한선

**Files:** Create `internal/orch/approvals/approvals.go`, `internal/orch/publish/publish.go`, `internal/orch/policy/floor.go`, 테스트 각각; Modify `cmd/orch/main.go`

**Interfaces:**
- Produces: `approvals.Enqueue(ctx, taskID int64, kind string, base, head, hash, reason string) error`(같은 작업·해시면 갱신, 새 해시면 이전 항목 무효 표시), `orch approvals`, `orch approve ID`(내부적으로 `aigate approve`와 같은 화면·안전 검사를 호출, 사람이 터미널에서 직접), `publish.Do(ctx, action Action) error`(`Action{Kind: "push_branch"|"draft_pr"|"notify"|...}`는 허용 목록에 있을 때만, 없으면 승인 대기로), `policy.Floor(action) error`(삭제, 운영 서버·권한 변경 계열은 **항상 거부**).

- [ ] **Step 1: 실패하는 테스트:** `TestFloorAlwaysDenies`(정책에서 허용해도 거부), `TestUnlistedActionGoesToApproval`, `TestAllowlistedActionRuns`(가짜 GitLab), `TestNewHeadInvalidatesPreviousApproval`, `TestApprovalRequiresTTY`, `TestAgentVMCannotReadApprovals`(파일 권한 0600과 경로 확인), `TestDuplicateEnqueueIsIdempotent`.
- [ ] **Step 2:** 구현. 저장소 쓰기 토큰은 오케스트레이터 VM의 0600 파일에서만 읽고 로그·출력에 남기지 않는다. Draft PR 생성은 GitLab API(`draft: true`). push는 `refs/heads/orch/<task>`만 허용한다.
- [ ] **Step 3:** PASS. Commit `feat(orch): approval queue, allowlisted publishing, hard floor`.

### Task 12: 정책 파일과 자율 단계

**Files:** Create `internal/orch/policy/policy.go`, `autonomy.go`, `propose.go`, 테스트; Modify `cmd/orch/main.go`

**Interfaces:**
- Produces: `policy.Load(path) (Policy, error)`(`classes: {버그수정: {level: L1}, ...}`, `allowlist: [...]`, `limits: {...}`), `policy.Hash()`(작업마다 기록), `autonomy.Decide(level Level, verdict Category, action Action) Decision`, `propose.FromLedger(ledgerPath string, days int) []Proposal`(종류별 연속 `ok` 횟수, 승인 필요율, 회귀 발생), `autonomy.Demote(...)`(위반·사고 시 한 단계 하향을 정책 상태 파일과 이력에 기록). 승격은 정책 파일을 사람이 바꿔야 적용된다.

- [ ] **Step 1: 실패하는 테스트:** 단계×판정×행위 표 검사(L0~L3), 하한선은 모든 단계에서 거부, 승격 제안은 파일을 **수정하지 않음**, 강등은 자동이고 이력에 기록됨, 잘못된 정책 파일(알 수 없는 키, 중복 종류, 없는 단계) 거부.
- [ ] **Step 2:** 구현. 정책 파일 변경은 `orchd`가 다음 작업 시작 시 다시 읽어 해시를 기록한다.
- [ ] **Step 3:** PASS. Commit `feat(orch): policy file, autonomy levels, proposals and demotion`.

### Task 13: 통합·보안·장애 시험과 결과 문서

**Files:** Create `docs/09-RESULTS-orchestrator.md`, `infra/test/security-checks.sh`, `internal/orch/machine/fault_test.go`

- [ ] **Step 1:** 통합 시험(사용자 확인 후, 실제 VM): `sample-collector`로 작업 제출 → 에이전트(작은 작업) → 커밋 회수 → 검사 VM 판정 → 승인 대기/통과 → (허용 목록) 브랜치 push는 **로컬 전용 모드**로 대체 가능(원격 push는 사용자가 허용할 때만). 소요 시간과 단계별 시간을 기록한다.
- [ ] **Step 2: 보안 시험(`security-checks.sh`):** 에이전트 VM에서 LAN·오케스트레이터·검사 VM 접속 시도(전부 실패), 허용 목록 밖 도메인(실패), 승인 파일 읽기 시도(실패), 검사 VM에서 보고서 위조 시도(판정 `ok` 아님), 에이전트가 만든 거대 커밋·많은 ref(수집 거부).
- [ ] **Step 3: 장애 주입:** 단계 중간 VM 중지, `orchd` 강제 종료 후 재개, 스냅샷 복원 실패, 승인 중 새 커밋 → 모두 설계서 9장 표대로 동작하는지 기록.
- [ ] **Step 4:** 결과를 `docs/09-RESULTS-orchestrator.md`(통과·실패·한계, 측정값)와 `전체_계획서.md`에 반영한다. 사람 없이 쓸 수 있는지(무인 적합성)를 **솔직하게** 판정한다. 막힌 항목이 남으면 적는다.
- [ ] **Step 5:** `go test ./... -count=1`과 `go test ./e2e -count=1` 전체 통과 확인. Commit `docs(orch): integration, security and fault-injection results`.

---

## Self-Review
- **설계서 커버리지:** 1장 구조(Task 0~2, 8), 2장 구성 요소(Task 6~8), 3장 상태 머신(Task 7), 4장 원격 실행기(Task 3~5), 5장 에이전트 경계(Task 9~10), 6장 승인(Task 11), 7장 정책·자율(Task 12), 8장 네트워크·보안(Task 2, 13), 9장 장애(Task 7, 9, 13), 10장 시험(Task 5, 13), 11장 순서(M0=Task 0~2, M1=Task 3~5, M2=Task 6~8, M3=Task 9~10, M4=Task 11, M5=Task 12). 구현 계획이 설계서에 없는 것을 추가하지 않았다(`gate.Executor` 추상화는 4장의 구체화).
- **타입 일관성:** `gate.Executor`/`CheckRequest`/`CheckOutput`(Task 3)을 Task 4·5·9가 같은 이름으로 쓴다. `store.Task`/`State`(Task 6)를 Task 7·8·11이 쓴다. `policy.Floor`(Task 11)는 Task 12 `autonomy.Decide`가 호출한다.
- **비율:** 코드 본문 없이 시그니처·테스트 이름·검증 방법만 적었다.
- **위험 표시:** 홈서버·VM 변경(Task 1·2·5 Step 4·10·13 Step 1)은 사용자 확인이 필요한 단계로 표시했다.
