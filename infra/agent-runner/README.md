# agent-runner: 에이전트 VM의 제한된 실행 경로

오케스트레이터(orch-vm)가 에이전트 VM(`agent-vm`, 10.70.0.12)에서 Claude/Codex를 돌리는 **유일한 길**이다. 오케스트레이터가 항상 먼저 연결하고, 에이전트 VM에서 나가는 연결 경로는 없다(LLM API 도메인 제외, VM 방화벽).

## 구성

| 파일 | 설치 위치 | 역할 |
|---|---|---|
| `agent-runner` | `/usr/local/sbin/agent-runner` (root 0755) | 강제 명령(bash). `$SSH_ORIGINAL_COMMAND`를 엄격히 검사 |
| `agent-runner.conf.example` | `/etc/agent-runner.conf` (root 0644, 없을 때만) | 호출자가 바꿀 수 없는 설정: 시간 제한, 로그 꼬리 크기, `claude`/`codex` 인자 |
| `authorized_keys.example` | `/etc/ssh/aigate/agent_authorized_keys` (root 0644) | `command=...,no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty` 한 줄 |
| `sshd-agent.conf` | `/etc/ssh/sshd_config.d/aigate-agent.conf` | `Match User agent`만 root 소유 authorized_keys를 사용 |
| `install.sh` | (맥에서 실행) | 위를 설치. 기본 dry-run, `--apply` 필요 |

authorized_keys를 `agent` 홈이 아니라 root 소유 파일에 두는 이유: 에이전트(와 그가 실행하는 코드)가 `agent` 사용자로 돌기 때문에 홈의 `~/.ssh/authorized_keys`는 스스로 고칠 수 있다. 그러면 `command=` 제한을 지울 수 있다.

## 허용 명령 (그 밖은 종료 코드 2, 부작용 없음)
- `prepare <task>`: stdin의 git bundle(최대 200 MiB, `refs/aigate/<sha>` 하나)로 `/srv/aigate/work/<task>/repo`를 지우고 다시 만든다. 기준 커밋에서 브랜치 `task-<task>`, 훅 비활성(`core.hooksPath=/dev/null`), 서브모듈 갱신 없음.
- `run <task> <claude|codex>`: stdin의 프롬프트(최대 64 KiB)로 작업 폴더에서 에이전트 실행. `RUN_TIMEOUT`(기본 3600초) 후 프로세스 그룹을 SIGKILL, 로그는 마지막 64 KiB만. 에이전트가 커밋 안 한 변경은 래퍼가 `git add -A && git commit`(고정 신원 `agent-runner`, 훅 없음). 응답은 `rc`, `timedout`, `committed`, `tail`이 든 고정 형식.
- `collect-info <task>`: `head`, `base`, `commits`(base..head), `bytes`(객체 크기 추정)만 출력. 파일 내용은 출력하지 않는다.
- `upload-pack '/<task>'`: 그 작업 폴더의 저장소에 대한 `git-upload-pack`만 실행(읽기 전용). `refs/heads/task-<task>` 외 모든 ref(다른 브랜치, 태그)는 숨긴다. 오케스트레이터의 `git fetch --upload-pack=upload-pack`이 쓴다. 다른 경로는 거부.

`<task>`는 `[0-9]{1,18}`. 인자 추가, 셸 문자, 줄바꿈은 모두 거부한다.

에이전트 인자는 `/etc/agent-runner.conf`에서만 바꾼다(기본: `claude -p --permission-mode acceptEdits --allowedTools "Bash,Edit,Write,Read,Glob,Grep"`, `codex exec --skip-git-repo-check --sandbox workspace-write -`). 도구를 줄이려면 이 파일의 `CLAUDE_ARGS`를 고친다. 호출자(오케스트레이터)는 못 바꾼다.

## 절차

0. 전제(이미 완료): agent-vm에 Node, `claude`, `codex` CLI가 설치되어 있고, 사용자가 `infra/agent-login.sh`로 로그인했다. 확인:
   ```sh
   sh infra/agent-login.sh status      # claude/codex 모두 "credentials file present" 이어야 함
   ```
   로그인이 없으면 **사용자가 자기 터미널에서** `sh infra/agent-login.sh claude`, `sh infra/agent-login.sh codex`를 실행한다. 토큰은 이 대화, 저장소, 로그에 붙이지 않는다(에이전트 홈에 CLI가 600 권한으로 저장).

1. 오케스트레이터 전용 **새** 키를 orch-vm에서 만든다(개인 키는 orch-vm을 떠나지 않는다. 관리자 키와 `aigate_ed25519`를 재사용하지 않는다):
   ```sh
   ssh ubuntu@192.168.1.200 'ssh-keygen -q -t ed25519 -N "" -C aigate-orchestrator -f ~/.ssh/aigate_agent_runner_ed25519'
   ```
   공개키(`.pub`)만 맥으로 가져온다: `scp ubuntu@192.168.1.200:.ssh/aigate_agent_runner_ed25519.pub /tmp/agent_runner.pub`.
   개인 키는 orchd를 돌리는 사용자(orch)만 읽을 수 있게 둔다(0600).

2. 계획 확인(dry-run, 아무것도 바꾸지 않음):
   ```sh
   sh infra/agent-runner/install.sh /tmp/agent_runner.pub
   ```
   지문과 authorized_keys 줄, 설치 경로가 맞는지 본다.

3. 사용자 확인 후 적용:
   ```sh
   sh infra/agent-runner/install.sh --apply /tmp/agent_runner.pub
   ```
   스크립트는 바뀌는 파일을 `*.pre-agent-runner`로 한 번 백업하고, `sshd -t`가 실패하면 sshd 설정을 되돌리고 멈춘다.

4. 강제 키 검증(선택이지만 권장). 개인 키는 orch-vm에 있으므로 orch-vm에서 직접 확인하거나, 키 파일을 맥에 두었다면:
   ```sh
   sh infra/agent-runner/install.sh --apply --verify-key <개인 키 경로> /tmp/agent_runner.pub
   ```
   `id`, `cat /etc/passwd`, `prepare abc`, `run 1 evil`, `upload-pack '/etc'`가 모두 거부되는지 확인한다. orch-vm에서의 같은 확인:
   `ssh -i ~/.ssh/aigate_agent_runner_ed25519 -o IdentitiesOnly=yes agent@10.70.0.12 id` 는 `agent-runner: command not allowed`로 끝나야 한다.

5. 로그인 상태 재확인: `sh infra/agent-login.sh status`. 방화벽 허용 목록 밖으로 나가지 않는지 `infra/verify-firewall.sh`를 다시 돌린다.

6. (사용자 참여) 작은 작업으로 끝에서 끝까지 확인한다. orch-vm에서:
   ```sh
   git -C /tmp/t init -q && ... && git -C /tmp/t commit -q --allow-empty -m base
   # 오케스트레이터의 vm.AgentVM{Host: "10.70.0.12", KeyPath: ".../aigate_agent_runner_ed25519", KnownHosts: ...}로
   # Prepare, Run("파일 hello.txt를 만들어라"), Collect 순서로 실행
   ```
   결과로 `refs/orch/<task>/head`가 생기고 `hello.txt`가 든 커밋 한 개가 있어야 한다.

## 되돌리기
agent-vm에서(관리자 키로): `sudo rm /etc/ssh/sshd_config.d/aigate-agent.conf && sudo sshd -t && sudo systemctl reload ssh` 하면 `agent`가 다시 홈의 authorized_keys를 쓴다. 래퍼와 설정은 `*.pre-agent-runner`가 있으면 그것으로 복원한다.

## 알려진 한계
- 호스트 키: 래퍼 경로는 `StrictHostKeyChecking=yes`로 고정된 known_hosts를 쓴다. agent-vm은 복원 대상이 아니므로 키가 바뀌지 않지만, VM을 다시 만들면 known_hosts를 다시 고정해야 한다.
- 타임아웃은 에이전트의 프로세스 그룹만 죽인다. `setsid`로 빠져나간 자손은 남을 수 있다(VM 단위 격리가 최종 방어).
- `agent` 사용자와 에이전트가 같은 계정이므로 에이전트는 작업 폴더·`.git`을 자유롭게 바꾼다. 그래서 오케스트레이터는 가져온 객체를 격리 저장소에서 fsck·재계산한 뒤에만 받아들인다.

## 실제 VM 검증 결과 (2026-11-06)
- 설치: `install.sh --apply <orch 전용 공개키>` 적용됨. `sshd -T -C user=agent` 에서 `authorizedkeysfile /etc/ssh/aigate/agent_authorized_keys`.
  (첫 적용은 `Match` 안에 `PermitUserEnvironment`가 있어 `sshd -t`가 거부했고 스크립트가 drop-in을 지우고 중단했다. 지시어를 빼고 재적용.)
- orch-vm(`orch` 계정, 전용 키 `~/.ssh/id_aigate`, 호스트키 고정 `~/.ssh/known_hosts_aigate`)에서:
  - 허용되지 않은 명령(`ls /`, `collect-info 1; id`, `upload-pack /etc`)은 `command not allowed`(rc 2).
  - `prepare 5001`(번들) → `run 5001 claude`(8 s) → 에이전트가 커밋 → `collect-info`가 commits=1 보고.
  - 오케스트레이터 쪽 `git fetch --upload-pack=upload-pack ssh://agent@10.70.0.12/<task>`가 작업 브랜치만 가져옴(fsck 켬). 다른 ref·경로 밖 요청은 거부.
  - 호스트키 미등록이면 `StrictHostKeyChecking=yes`로 연결 거부.
- **Codex (2026-11-06 해결):** `codex exec --sandbox workspace-write`가 `bwrap: loopback: Failed RTM_NEWADDR`로 실패했다(Ubuntu 24.04의
  `kernel.apparmor_restrict_unprivileged_userns=1`). 전역 sysctl을 푸는 대신 **Codex가 자체 포함한 bwrap 바이너리 하나에만** 사용자 네임스페이스를 허용하는
  AppArmor 프로파일을 `infra/agent-codex-sandbox.sh --apply`로 설치(사용자가 직접 실행). 전역 sysctl은 1 유지, 일반 `unshare`는 계속 차단.
  확인: `--check`, 그리고 오케스트레이터 작업 8(`--agent codex`): prepare → codex 실행 → 커밋 → 수집 → 판정 ok → push, 34 s.
  롤백 `--rollback`. Codex 업그레이드로 바이너리 경로가 바뀌면 `--apply`를 다시 실행해야 한다. 샌드박스는 켜져 있다(해제 플래그 미사용).
