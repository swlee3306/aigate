# 웹 콘솔 W1 설계서: API와 인증 기반 (초안 v0.1, 2026-11-09)

> 상태: **구현됨 (W1, 2026-11-09), 컷오버 대기.** 결과·가정/실험 판정·설계와 달라진 곳은 `13-RESULTS-webconsole-W1.md`, 배포·롤백 절차는 `10-OPERATIONS-orchestrator.md` 9장. 이 문서는 설계 당시 그대로 둔다.

상위: `06-REQUIREMENTS-orchestrator.md`(OR1~OR11), `01-REQUIREMENTS.md`(FR15), `07-DESIGN-orchestrator.md`. 양식은 `00-TEMPLATE.md`. 화면 목업은 `web-mockup/index-v4.html`(README 참고), 데모는 `web-demo/`.
**확정**은 사용자가 정했거나 코드로 확인한 것, **제안**은 이 문서의 기본안(승인 필요).

## 1. 배경: 사용자 시나리오 (2026-11-09 사용자 작성)

웹 콘솔은 모니터링 화면이 아니라 **개발의 주 조작 화면**이다. 사용자 시나리오 전체:

1. 웹 접속 → 2. 신규 프로젝트 생성 또는 기존 프로젝트 연결(git, 폴더 업로드) → 3. 요구사항 간단 입력("~을 만들고 싶어") → 4. 요구사항을 분석해 설계서 작성, 사용자가 수정·제외·체크 방식으로 검토 → 5. 구현 계획 정의 → 6. 구현 계획별로 기능 개발 조각으로 쪼개기
[기능이 완성될 때까지 반복] 7. 제출 → 8. 준비 → 9. 에이전트 구현 → 10. 수집 → 11. 검증(ok) → 12. 게시 → 13. 검토·병합
14. 기능 최종 통합 테스트 → 15. 개발 완료

| 단계 | 현재 상태 (2026-11-09 확인) |
|---|---|
| 7~12 | 있음. 오케스트레이터 ②가 실제 VM에서 38건 실행됨 (`09-RESULTS-orchestrator.md`) |
| 13 | 수동. MR 자동 생성은 안 됨(게시 코드가 https GitLab만 지원, 이 GitLab은 http) |
| 1~6, 14~15 | 없음 |

메뉴는 프로젝트 안에서 위 단계를 따라가는 **단계 기준**으로 한다(사용자 확정).

## 2. 서브프로젝트 분해

| # | 이름 | 범위 | 시나리오 단계 |
|---|---|---|---|
| **W1** | **API와 인증 기반 (이 문서)** | 공개 API v1, 신원·세션, 서명 승인, HTTPS, CLI 클라이언트 전환, 웹 뼈대(단계 메뉴), 프로젝트 자원의 최소 틀 | 1 |
| W2 | 구현 루프 화면 | 7~13을 웹에서 보기·제출·재시도, 병합 보조 | 7~13 |
| W3 | 프로젝트 연결 | git 연결, 폴더 업로드, 게이트 설정(`.aigate.yml`) 부트스트랩(FR9) | 2 |
| W4 | 기획 | 요구사항 입력 → 분석 → 설계서 생성, 검토 UI(수정·제외·체크) | 3~4 |
| W5 | 계획과 분해 | 구현 계획, 작업 조각 생성·순서·의존, 검토 | 5~6 |
| W6 | 통합과 완료 | 기능 최종 통합 테스트, 완료 처리 | 14~15 |

순서: W1 → W2 → W3 → W4 → W5 → W6. 근거: 검증된 ②를 가장 먼저 사용자 화면에 올리고, LLM 호출과 업로드 보안이 필요한 앞 단계는 뒤에 쌓는다. 각 서브프로젝트는 자기 요구사항 → 설계서 → 계획 → 구현을 따로 거친다.

## 3. W1 요구사항

### 확정 (사용자 결정)
| # | 내용 | 날짜 |
|---|---|---|
| W1-R1 | 메뉴는 워크플로우의 큰 축(단계)별로 둔다. 아직 안 만든 축도 처음부터 자리를 둔다 | 2026-11-09 |
| W1-R2 | 웹과 터미널을 기능으로 구분하지 않는다. **모든 기능을 API로 제공하고 CLI와 웹은 같은 API의 두 클라이언트**다. 터미널은 계속 지원하고 사용성은 웹이 맡는다 | 2026-11-09 |
| W1-R3 | 승인은 **사용자 키의 서명**이다. 웹은 패스키, CLI는 SSH 키. 서버는 검증만 한다 | 2026-11-09 |
| W1-R4 | CLI는 orch-vm이 아니라 **이 Mac에서 실행되는 원격 클라이언트**가 된다(SSH 키가 Mac 밖으로 나가면 안 되므로 서명은 Mac에서만 가능) | 2026-11-09 |
| W1-R5 | Tailscale 안에서만 접속한다. 디자인은 목업 v4 | 2026-11-08~09 |

### 범위
- 포함: 공개 API v1, 신원·세션, 서명 승인(데몬·게이트 검증), HTTPS 노출, CLI의 API 클라이언트 전환, 웹 뼈대와 읽기 화면, 프로젝트 자원의 최소 틀(읽기)
- 제외: 프로젝트 연결·업로드(W3), 기획·계획 화면(W4·W5), 통합 테스트(W6), 알림(⑤), 모델 게이트웨이(④)

## 4. 현재 코드 사실 (확인함, 2026-11-09)

- API는 유닉스 소켓에 8개 경로: `POST/GET /tasks`, `GET /tasks/{id}`, `POST /tasks/{id}/cancel|retry`, `GET /approvals`, `GET /approvals/{id}`, `POST /approvals/{id}/reverify` (`internal/orch/api/api.go`).
- 승인 저장: `<GateHome>/.aigate/approvals.json`(0600)에 `(repo, base, hash, granted_at)`. 쓰는 곳은 TTY를 확인하는 `orch approve`와 `aigate approve`. 읽는 곳은 데몬과 **게이트(`aigate`) 둘 다**(`internal/approval/approval.go`: `Store.Valid`, `Store.Grant`).
- diff 해시는 `hashOf(d.HashedText())`이다(`internal/gitx/gitx.go`). 해시가 표시 가능한 텍스트의 함수라서, 클라이언트가 텍스트에서 해시를 다시 계산하는 것이 원리상 가능하다.
- `go.mod`에는 암호·WebAuthn 라이브러리가 없다(`x/term`, `x/text`, yaml, sqlite).
- orch-vm 방화벽은 입력 기본 drop, 허용은 LAN 3대의 22번과 `tailscale0`의 22번뿐이다. 에이전트·검사 VM(10.70.0.0/24)은 이 입력 규칙에 해당하지 않아 웹 포트에 닿을 수 없다(`infra/nftables/orchestrator.nft`).
- 승인 종류: `protected_modified`(보호 파일 수정), `publish_outside_allowlist`(허용 목록 밖 게시, 정책 변경 의사 종류 포함). 승인 항목마다 서버가 계산한 해시가 있다.

## 5. 구조

```
 [사용자 기기]                      [orch-vm]
  브라우저(패스키) ──HTTPS(tailscale serve)──▶ 127.0.0.1:PORT ┐
  CLI(Mac, SSH 키) ──HTTPS(tailscale serve)──▶ 127.0.0.1:PORT ├─▶ orchd: API v1 핸들러 ─▶ 상태 머신, SQLite, 이력
  로컬 소켓(관리자 ssh) ───────────────────────────────────────┘          │
                                                                          └─▶ 승인 파일(서명 항목) ─▶ aigate가 읽을 때 재검증
```

- **API v1은 하나**다. 유닉스 소켓과 HTTPS 리스너가 같은 핸들러를 쓴다. HTTPS 리스너는 `127.0.0.1`에만 바인딩하고, TLS는 `tailscale serve`가 종단한다.
- 화면은 의존성 없는 정적 파일(HTML, CSS, 바닐라 JS)을 `go:embed`로 바이너리에 넣는다.
- 기존 8개 경로는 소켓에서 별칭으로 유지하다가 CLI 전환이 끝나면 폐기한다.

## 6. API v1

모든 경로는 `/api/v1` 아래. 요청과 응답은 JSON. 오류는 `{ "error": { "code", "message" } }`.

| 그룹 | 경로 | 보증 수준 |
|---|---|---|
| 단계 메뉴 | `GET /stages` (단계 목록, 계획서 상태 ✅🔄⬜, 담당 구성요소, 다음에 사람이 할 일) | 읽기 |
| 개요 | `GET /overview` (상태별 건수, 단계별 평균 시간, 사람이 볼 것) | 읽기 |
| 프로젝트 | `GET /projects`, `GET /projects/{p}` (W1은 읽기만, 생성은 W3) | 읽기 |
| 작업 | `GET /tasks`, `GET /tasks/{id}`, `GET /tasks/{id}/events`, `/report`, `/diff` | 읽기 |
| 작업 조작 | `POST /tasks`, `/tasks/{id}/cancel`, `/tasks/{id}/retry` | 일반 쓰기 |
| 승인 | `GET /approvals`, `GET /approvals/{id}`(서버가 만든 서명 대상 `message` 포함) | 읽기 |
| 승인 확정 | `POST /approvals/{id}/grant` (서명 제출) | **보안 승인** |
| 재판정 | `POST /approvals/{id}/reverify` (유효한 서명이 있을 때만 재판정, 없으면 아무것도 바꾸지 않음) | 일반 쓰기 |
| 정책·운영 | `GET /policy`, `GET /ops` (VM, 쿼터, 스냅샷, base 캐시) | 읽기 |
| 인증 | `POST /auth/challenge`, `POST /auth/token`(SSH 키), `POST /auth/webauthn/login/begin|finish`, `POST /auth/webauthn/register/begin|finish`, `GET /auth/session`, `POST /auth/logout` | — |

규칙:
- **읽기 전용 응답은 서버가 계산한 값만** 낸다. 프롬프트, diff, 에이전트 출력은 신뢰할 수 없는 글이다.
- diff는 승인 해시와 같은 분류가 되도록 게이트의 기존 코드(`gate.Prepare`)를 재사용하고, git에는 `--no-ext-diff --no-textconv`를 준다. 크기 상한을 둔다. 보고서는 보고서 폴더 안의 파일만 읽는다.
- 쓰기 요청은 `Idempotency-Key`를 받는다.

## 7. 신원과 세션

| 경로 | 방법 | 결과 |
|---|---|---|
| 웹 | 패스키(WebAuthn) 로그인 | 서버 쪽 세션(무작위 256비트, SQLite 저장). 쿠키는 `HttpOnly; Secure; SameSite=Strict` |
| CLI(원격) | SSH 키 챌린지: 서버가 무작위 값을 주면 키로 `aigate-login-v1` 메시지에 서명해 제출 | 짧은 수명의 베어러 토큰 |
| 로컬 소켓 | 소켓 상대 프로세스의 uid(SO_PEERCRED)가 허용 목록에 있음 | 읽기와 일반 쓰기 |

- 일반 쓰기에는 CSRF 방어를 건다: `Origin` 검사, 세션에 묶인 `X-Orch-CSRF` 헤더, JSON 전용.
- 어떤 경로든 **보안 승인은 세션이 아니라 매번의 서명**이다(8장).
- 인증 경로에는 분당 횟수 제한을 둔다.

## 8. 서명 승인

### 8.1 서명 대상(정규 메시지)
UTF-8, 줄바꿈 `\n`, 항목 순서 고정:

```
aigate-approval-v1
kind: <protected_modified | publish_outside_allowlist>
repo: <repo id>
task: <작업 번호>
base: <sha>
hash: <diff 해시, 또는 게시 해시(작업+head+행위/정책 해시)>
issued: <RFC 3339 UTC>
```
**(구현 후 정정, 2026-11-09 R23: `hash`는 두 종류 모두 정확히 64자리 소문자 hex(SHA-256 전체)다. diff 해시는 원래 SHA-256 앞 16자리였으나 전체로 넓혔고, 16자리 해시가 든 메시지는 Go `signing.Message`와 웹 `MESSAGE_RE` 모두 거부한다. docs/13 §8의 3번)**

서버가 승인 항목에서 이 메시지를 만들어 `GET /approvals/{id}`에 담는다. 서명 제출 시 서버는 메시지를 다시 만들어 제출된 것과 같은지 확인한다(해시가 달라졌으면 거부).

### 8.2 서명 방법
| 클라이언트 | 서명 | 검증 |
|---|---|---|
| 웹 | WebAuthn assertion. challenge는 메시지의 SHA-256. 사용자 확인(UV) 필수 | 서명, origin, rpIdHash, UP·UV 플래그, 서명 카운터 |
| CLI | SSH 서명(네임스페이스 `aigate-approval`)을 메시지에 | 공개 키로 검증 |

### 8.3 검증 위치와 승인 파일 v2
검증은 **두 곳**에서 한다: 서명을 받는 데몬, 그리고 승인 파일을 읽는 **게이트**. 승인 항목(v2)에는 서명 원문을 함께 저장한다:

```json
{ "repo": "...", "base": "...", "hash": "...", "granted_at": "...",
  "message": "...", "signer": "<서명자 ID>", "alg": "ssh-ed25519 | webauthn-es256", "sig": "...",
  "webauthn": { "authenticator_data": "...", "client_data_json": "..." } }
```
`Store.Valid`는 항목이 있는지에 더해 서명이 서명자 목록의 공개 키로 검증되고 메시지가 `(repo, base, hash)`와 일치하는지 본다.

### 8.4 서명자 목록(신뢰의 뿌리)
- 파일: **`/etc/aigate/signers.json`**(경로는 `--signers-file`/`AIGATE_SIGNERS`로 바꿀 수 있고 기본값이 이것). 항목은 `(id, 종류, 공개 키, 라벨, 등록 시각)`(웹 패스키는 `credential_id` 추가).
- **파일과 그 폴더(`/etc/aigate`)가 모두 root 소유, 그룹·기타 쓰기 불가.** orch 사용자는 파일을 수정할 수 없고, 폴더에도 쓸 수 없어 **지우거나 바꿔치기할 수도 없다.** 로더는 파일과 상위 폴더의 소유자·모드를 검사해 어긋나면 서명 승인을 모두 거부한다(fail closed).
- 이유(2026-11-09 계획 작성 중 발견한 보강): 첫 초안은 `<GateHome>/.aigate/`에 두었으나 그 폴더는 orch 소유라서, 파일이 root 소유여도 orch가 폴더에서 파일을 지우거나 교체할 수 있었다.
- 등록과 복구는 **관리자 SSH + sudo 명령**으로만 한다. 웹에서 패스키를 만들면 서버에 "대기 중 자격"으로만 남고, 관리자가 지문을 확인한 뒤 `sudo orch-admin signers add`로 활성화한다.

### 8.5 전환
- `/etc/aigate/signers.json`이 없으면 기존 방식(TTY 승인)이 그대로 동작한다(코드를 먼저 넣고 모드는 꺼 둠). orch 사용자는 `/etc/aigate`에 쓸 수 없어 이 파일을 만들거나 지워서 모드를 바꿀 수 없다.
- 파일이 생긴 순간부터 **서명 필수 모드**: 서명 없는 v1 항목은 거부한다. 이미 `approved` 상태인 이력은 보존한다.
- 서명 필수는 **orchd의 gate-home**에만 적용한다. 사용자의 개인 `aigate` 단독 사용은 영향 없다. **(구현 후 정정: 서명 필수의 범위는 프로세스 전역이다. docs/13 §7)**

### 8.6 이력(ledger)
승인과 모든 쓰기 동작에 "누가(서명자 ID 또는 세션 주체), 무엇을, 언제, 어느 경로(웹/CLI/소켓)"를 남긴다. 기존 `approval_granted` 이벤트에 서명자와 알고리즘을 추가한다.

## 9. HTTPS와 네트워크
- `tailscale serve`로 orch-vm의 로컬 포트에 HTTPS를 붙인다(패스키가 보안 연결을 요구하므로 필수). 인증서는 Tailscale 관리 화면에서 HTTPS 기능을 켜야 한다(사용자 작업).
- 방화벽: `tailscale0`에서 들어오는 443을 허용하는 규칙 1줄 추가(적용은 사용자가 `apply-firewall.sh`로, 180초 자동 롤백 있음).
- 응답 헤더: 엄격한 CSP(`default-src 'self'`; `frame-ancestors 'none'`), `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`(API). CORS 없음. Host 헤더 허용 목록. 요청 본문 1 MiB 제한.
- 화면은 **`textContent`로만** 출력한다. 에이전트가 만든 글을 HTML로 해석하지 않는다.

## 10. 신뢰 모델과 위협

| 위협 | 대응 | 남는 위험 |
|---|---|---|
| 승인 파일 위조 | 서명자 목록(root 소유 폴더 안의 root 소유 파일)의 키로 서명을 검증. 게이트도 재검증 | 없음(키 없이는 위조 불가) |
| 웹 서버·`orchd` 침해 | 승인 서명 위조·재사용 불가(API 경유 또는 `approvals.json` 쓰기), 서명자 목록 변경 불가 | 작업 취소·열람은 가능 **(구현 후 정정: `orch` 사용자로 임의 코드가 실행되면 해당 없음. docs/13 §4)** |
| 서명 재사용 | 메시지가 `(repo, base, hash, task)`에 묶임. 다른 항목에는 무효. `issued`는 서명 시점에 서버가 ±5분 확인 | 같은 항목의 재승인은 같은 효과(의도된 동작). **(구현 후 정정: 승인은 `head`에 묶이지 않으므로 해시의 충돌 저항이 곧 이 대응의 강도다. 64비트 diff 해시에서는 약 2^32번 작업으로 다른 head의 충돌 diff가 승인을 재사용할 수 있었다. R23에서 SHA-256 전체 64자리로 넓혀 막았다. docs/13 §8의 3번)** |
| 세션 탈취·CSRF | `HttpOnly; Secure; SameSite=Strict`, Origin 검사, CSRF 헤더. 보안 승인은 세션과 무관 | — |
| 에이전트 출력의 XSS | `textContent` 출력, CSP | — |
| 에이전트가 API 호출 | 에이전트·검사 VM은 `tailscale0`에 닿지 못함. 로컬 소켓은 0600 | — |
| **침해된 서버가 사용자를 속임**(다른 diff를 보여 주고 다른 해시에 서명시킴) | 서명은 해시에 대한 것이라 그 해시의 diff에만 적용된다. 그러나 사용자가 본 내용과 해시의 일치는 서버가 보장한다 | **남음.** 8.7 후속 과제 |
| 서명자 키 분실 | 관리자 SSH + sudo로 복구. 서명자는 여러 개 등록 가능(웹 패스키, Mac SSH 키, 예비 키) | — |

### 8.7 표시와 해시의 일치 (후속)
diff 해시가 `HashedText()`의 함수이므로, CLI(같은 Go 모듈)는 서버가 준 해시 대상 텍스트로 해시를 **다시 계산**해 일치를 확인한 뒤에야 서명하도록 할 수 있다. 웹은 같은 계산을 JS로 해야 한다. 이번 W1에서는 CLI에만 넣고 웹은 후속으로 한다(가정 A3).

## 11. 단계와 시험 계획

| 단계 | 내용 | 끝났다고 볼 기준 |
|---|---|---|
| P1 | API v1 + CLI 클라이언트 전환(인증은 소켓 그대로), `/stages`·`/overview`·단계별 시간·보고서·diff 읽기 API | 기존 `orch` 명령이 API v1로 동일하게 동작, 단위·e2e 통과 |
| P2 | 서명 검증을 승인 패키지·게이트에 추가(모드 꺼 둠), 서명자 목록, v2 승인 파일 | 서명 있는 승인으로 게이트가 `ok` 판정, 서명 없음·변조·다른 해시는 거부 |
| P3 | HTTPS, 패스키·SSH 챌린지 로그인, 세션·CSRF | 웹 로그인과 CLI 로그인이 실제 장비에서 동작 |
| P4 | 웹 뼈대(단계 메뉴)와 읽기 화면 | 목업 v4와 같은 화면이 실제 데이터로 표시 |
| P5 | 보안 승인 전 과정 e2e와 적대 시험 | 위조 파일, 서명 재사용, 잘못된 해시, 만료, 다른 Origin, CSRF, 서명자 목록 변조가 모두 거부됨을 시험으로 확인 |

시험은 기존 방식대로 변이 확인(시험이 실제로 잡는지)을 포함한다. 배포는 단계마다 DB와 바이너리를 백업한 뒤 한다.

## 확정
위 3장 확정 표(W1-R1~R5)와 4장 코드 사실. 서명 승인의 구조(8장)는 사용자가 방향을 확정했고(2026-11-09), 세부 형식은 이 문서의 제안이다.

## 가정
| # | 가정 | 확인 방법 |
|---|---|---|
| A1 | `tailscale serve`로 HTTPS를 붙일 수 있고 인증서가 발급된다(`bon-openstack`이 443을 tailscaled로 쓰는 것으로 보이나 미확인) | orch-vm에서 `tailscale serve` 시험, Tailscale 관리 화면에서 HTTPS 기능 확인 |
| A2 | 서명 `issued` ±5분이 CLI·웹 모두에 충분하다 | P3 실사용에서 서명 실패 사례 확인 |
| A3 | CLI가 서버가 준 해시 대상 텍스트로 해시를 다시 계산할 수 있다(`hashOf`가 순수 해시) | `internal/gitx/gitx.go`의 `hashOf` 확인, 단위 시험 |
| A4 | Mac의 SSH 키(ed25519)로 `ssh-keygen -Y sign` 방식 서명이 Go에서 검증된다 | `x/crypto/ssh`로 시험 |
| A5 | 세션 수명(유휴 8시간, 절대 24시간)이 개인 사용에 적절하다 | 사용해 보고 조정 |

## 미결정
| # | 항목 | 결정자 | 기본값 |
|---|---|---|---|
| D1 | WebAuthn 검증을 직접 최소 구현(ES256, 증명 `none`만)할지 라이브러리를 쓸지 | 사용자(보안 코드 의존성 판단) | 라이브러리가 검증된 코드이므로 `go-webauthn` 계열을 먼저 검토하고, 의존성 크기를 본 뒤 결정 |
| D2 | SSH 서명 검증에 `golang.org/x/crypto/ssh` 추가 | 사용자 | 추가한다(Go 팀 모듈) |
| D3 | 서명 필수 모드로 전환하는 시점 | 사용자 | P5 시험이 모두 통과한 뒤 |
| D4 | 서명자 등록 도구 이름과 위치(`orch-admin` 등) | 설계 | `orch admin signers add|list|remove`(root 권한에서만 쓰기) |
| D5 | 웹의 해시 재계산(JS) 시점 | 사용자 | W2 이후 |

## 실험할 것
| # | 무엇을 | 끝났다는 기준 |
|---|---|---|
| X1 | orch-vm에서 `tailscale serve` HTTPS와 패스키 등록·로그인 | 브라우저에서 패스키 로그인이 성공 |
| X2 | `ssh-keygen -Y sign` 서명을 Go로 검증 | 정상 서명 통과, 변조·다른 네임스페이스 거부 |
| X3 | 게이트가 서명 항목을 읽어 판정하는 경로의 비용 | 서명 검증이 게이트 시간에 눈에 띄는 영향(1초 이상)을 주지 않음 |

## 부록: 설계 요약 대비 보강한 것
1. 서명자 목록을 **root 소유 파일**로 두고 등록을 관리자 명령으로만 하게 했다(요약에서는 "일회용 링크"만 언급). 이유: `orchd` 침해 시 공격자가 자기 키를 등록할 수 없어야 서명 위조 불가 성질이 유지된다.
2. 위협 표에 **침해된 서버가 사용자를 속이는 경우**를 정직하게 남겼다. 서명 방식은 승인 위조를 막지만, 표시와 해시의 일치까지는 보장하지 않는다. CLI의 해시 재계산으로 완화하고 웹은 후속이다.
