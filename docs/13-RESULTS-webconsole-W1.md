# 웹 콘솔 W1 — 결과 기록 (초안, E1 시점)

> 상태: **초안**(Task 26/E1에서 작성, 2026-11-09). E2(Task 27)에서 가정 A1~A4·실험 X1~X3 결과와 배포 기록을 더해 완성한다.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험만 가리킨다. 실제 서버(orch-vm), 실제 브라우저 패스키, `tailscale serve`는 아직 한 번도 실행하지 않았다.

## 1. 만든 것

**A. API v1(Task 1~7).** `orchd`의 모든 기능을 `/api/v1` 아래에 다시 묶었다. 오류는 `{"error":{"code","message"}}` 한 가지 모양이고 메시지는 한국어 고정 문장이다. 읽기 API로 단계(`/stages`), 개요(`/overview`: 상태별 수, 승인 대기, 단계별 평균 시간), 작업 타임라인, 보고서(경로 탈출 차단, 2 MiB 상한), diff(게이트와 같은 해시 대상 텍스트, 256 KiB에서 자르고 `truncated` 표시, 해시는 항상 전체 텍스트의 것), 운영 정보(`/ops`: 정책·빌드·base 캐시·마지막 복원), 프로젝트(작업의 저장소별 집계)를 추가했다. CLI `orch`는 같은 API를 소켓(로컬) 또는 `--server https://…`(원격, 토큰)로 쓴다.

**B. 서명 승인(Task 8~15).** 승인은 이제 "누가 어떤 `(repo, base, hash)`를 허용했는지"에 대한 서명이다. 정규 메시지 `aigate-approval-v1`(키 순서·줄바꿈 고정, 엄격한 역파싱), root 소유 폴더 안의 root 소유 서명자 목록(`/etc/aigate/signers.json`, macOS 개발용은 `/private/etc/...`), SSH ed25519(`ssh-keygen -Y sign`, 네임스페이스 `aigate-approval`)와 WebAuthn ES256(직접 구현한 최소 검증기, 증명 `none`) 두 종류의 서명자, 승인 파일 항목에 서명 필드 추가(파일 버전은 그대로 1). 목록 파일이 있으면 **서명 필수 모드**: 서명이 검증되는 항목만 유효하고, 목록이 "없음"이 아닌 다른 어떤 문제(권한, 소유자, 형식, 심볼릭 링크, 신뢰할 수 없는 폴더 안의 부재)도 모든 승인을 무효로 만든다(fail closed). 데몬의 승인 API(`POST /approvals/{id}/grant`)는 메시지를 서버가 다시 만들고 `issued` ±5분을 확인한다. CLI `orch approve`는 서버가 준 diff로 해시를 다시 계산해 일치할 때만 서명한다. 관리 명령 `orch admin signers add|list|remove|pending`(쓰기는 root만).

**C. 인증과 HTTP 보안(Task 16~22).** 세션·토큰·챌린지·멱등 키 테이블, 소켓 상대 uid 확인(Linux `SO_PEERCRED`, macOS `LOCAL_PEERCRED`), SSH 챌린지 로그인(네임스페이스 `aigate-login`, 토큰 15분), 패스키 로그인과 등록(대기 자격 → 관리자가 root로 활성화), 세션 쿠키 `__Host-orch_session`(Secure, HttpOnly, SameSite=Strict, 유휴 8시간·절대 24시간), 쓰기마다 `X-Orch-CSRF`. `127.0.0.1` HTTP 리스너의 미들웨어: 보안 헤더(CSP 등) → Host 허용 목록(421) → CORS 없음 → Origin/Sec-Fetch-Site 검사(403) → 본문 1 MiB(413) → JSON만(415) → `/auth/*` 속도 제한(429). 배포 스크립트·nft 규칙 1줄·systemd 단위 예시와 운영 절차(README).

**D. 웹 콘솔(Task 23~25).** `go:embed`로 내장한 바닐라 JS 화면: 단계 메뉴, 작업 목록·상세·타임라인, diff 보기(보이지 않는 문자·제어 문자 표시, 잘림 표시), 패스키 로그인·등록, 승인 화면. 에이전트가 만든 글은 `textContent`로만 넣는다. 승인 화면은 메시지·저장소 id·게시 행위·다이제스트를 브라우저에서 다시 확인하고, 잘렸거나 다 보이지 않은 승인은 서명하지 않고 `orch approve`를 안내한다(R22).

**E1. 적대 시험(Task 26).** 아래 3장. 공격자가 승인 파일을 마음대로 쓸 수 있거나(orch 사용자), 서명자 목록 배치가 잘못됐거나, 브라우저가 다른 출처에서 요청을 보낼 때 승인이 생기지 않는지를 확인하는 시험과, 그 시험이 실제로 잡는지 확인하는 변이 시험을 추가했다. 함께 갚은 빚: 불안정한 시험 2개, 유효한 세션·토큰의 전역 속도 제한 면제(CF21: `GET /auth/session`, `GET /auth/signers`, `POST /auth/logout`에만 적용. 챌린지·토큰·패스키 로그인·등록은 자격이 있어도 키별·전역 한도를 모두 받는다), CLI 서명 화면의 게시 설명 표시.

## 2. 시험 목록 (실제 실행 결과)

실행: 2026-11-09, macOS(Apple Silicon), Go 1.27.1, Node v26.5.0, 커밋 `2c05dbb`(수정 1회차 후 다시 실행).

- `go vet ./...`, `GOOS=darwin go vet ./cmd/... ./internal/...`, `GOOS=linux go vet ./cmd/... ./internal/...`: 통과. `gofmt -l`: 출력 없음.
- `go test ./... -count=1 -race`: **종료 코드 0**. 최상위 시험 함수 851개 중 849 통과, 0 실패, 2 건너뜀(하위 시험 659개 모두 통과).
  - 건너뜀 2개: `internal/orch/web` `TestDevServer`(수동 개발 서버, 환경 변수로만 켬), `internal/pathfold` `TestCollisionsPerformance`(-race 빌드에서만 건너뜀. 20만 경로를 1초 안에 처리하는지 재는 시간 검사라 -race 부담에서는 의미가 없다. 이 건너뜀은 **-race에서만 시간 검사를 숨긴다**. 일반 빌드 `go test ./internal/pathfold -count=1`에서는 그대로 돌고 통과(0.53초)).
- `node --test "internal/orch/web/test/*.test.js"`: **76개 전부 통과**.
- `internal/orch/api` 패키지 전체를 `-race -count=3`으로 부하(변이 시험과 동시 실행) 중에 돌려 통과. 고친 두 시험(`TestOverviewCounts`, `TestRetryAPIConcurrentDouble`)은 각각 `-race -count=30` 통과.

| 패키지 | 통과 | 실패 | 건너뜀 | 하위 시험 |
|---|---|---|---|---|
| `cmd/aigate` | 56 | 0 | 0 | 103 |
| `cmd/orch` | 51 | 0 | 0 | 44 |
| `cmd/orchd` | 27 | 0 | 0 | 0 |
| `e2e` | 9 | 0 | 0 | 147 |
| `internal/adapter` | 13 | 0 | 0 | 0 |
| `internal/approval` | 30 | 0 | 0 | 80 |
| `internal/approval/signing` | 54 | 0 | 0 | 73 |
| `internal/config` | 8 | 0 | 0 | 0 |
| `internal/gate` | 59 | 0 | 0 | 63 |
| `internal/gitx` | 37 | 0 | 0 | 3 |
| `internal/ledger` | 17 | 0 | 0 | 0 |
| `internal/orch/api` | 160 | 0 | 0 | 2 |
| `internal/orch/approvals` | 6 | 0 | 0 | 0 |
| `internal/orch/machine` | 33 | 0 | 0 | 14 |
| `internal/orch/policy` (+autonomy, propose) | 27 | 0 | 0 | 25 |
| `internal/orch/publish` | 17 | 0 | 0 | 0 |
| `internal/orch/steps` | 51 | 0 | 0 | 17 |
| `internal/orch/store` | 31 | 0 | 0 | 4 |
| `internal/orch/vm` | 75 | 0 | 0 | 5 |
| `internal/orch/web` (Go) | 9 | 0 | 1 | 0 |
| `internal/pathfold` | 5 | 0 | 1 (-race만) | 12 |
| `internal/remote` | 18 | 0 | 0 | 7 |
| `internal/report`, `rules`, `runner`, `stats` | 30 | 0 | 0 | 35 |
| `internal/textsafe` | 4 | 0 | 0 | 25 |
| `internal/webauthn` | 22 | 0 | 0 | 0 |

이번에 더한 적대 시험: `internal/approval/adversarial_test.go` 10개(하위 시험 포함 통과 항목 79개), `internal/orch/api/adversarial_test.go` 6개, `cmd/orch/remote_e2e_test.go` 1개(원격 CLI → 실제 데몬 HTTP 리스너 → SSH 서명 승인 → 게이트 통과까지), `secure_test.go`에 CF21 시험 4개(면제 경로 한정 포함).

## 3. 변이 시험 (시험이 실제로 잡는지)

방법: 커밋한 코드를 `git worktree`로 따로 꺼내 한 곳만 망가뜨리고 `go test ./internal/approval/... ./internal/orch/api/... ./cmd/orch/... -count=1`을 돌렸다(작업 트리는 건드리지 않음, 끝나면 사본 삭제). `e2e` 패키지는 변이마다 돌리지 않았다(실제 트리에서 한 번만).

| # | 망가뜨린 곳 | 잡은 시험(이번에 추가한 것은 굵게) |
|---|---|---|
| ① | `Valid`가 서명 검증을 건너뜀(`signedValid`가 항상 참) | **TestForgedApprovalsFileIgnored**, **TestEditedMessageOrHashRejected**, **TestReplayAcrossItemsRejected**, **TestLoginVsApprovalNamespacesDisjoint**, TestSignatureForItemANotValidForItemB 외 3 |
| ② | 서명자 목록 상위 폴더 신뢰 검사 제거 | **TestRegistryInOrchOwnedDirRefused**, **TestRegistryDeletedFallsBackOnlyWhenAbsentNotWhenBroken**, TestLoosePermsRegistryFailsClosed, signing.TestAbsentRequiresTrustedParent 외 3, orch.TestSignersAddRefusesUntrustedRegistry |
| ③ | 저장된 메시지의 `hash` 줄과 조회 해시 비교 제거 | **TestEditedMessageOrHashRejected**, **TestReplayAcrossItemsRejected**, TestSignatureForItemANotValidForItemB, TestSignedModeRejectsTamperedHash |
| ④ | `Origin` 검사 제거(어떤 Origin이든 통과) | **TestCSRFCrossOriginRejected**, TestCrossOriginPostRefused, TestOriginCheckedOnAuthSession, TestAPIResponsesNoStore, TestStaticServedWithCSPAndTypes |
| ⑤ | 로그인 네임스페이스 = 승인 네임스페이스 | **TestLoginVsApprovalNamespacesDisjoint**, **TestForgedApprovalsFileIgnored**, api.TestLoginSignatureNotAcceptedAsApproval, api.TestApprovalSignatureNotAcceptedAsLogin, api.TestTokenUnlocksWriteNotApproval, orch.TestCLILoginRealSSHKeygen |
| ⑥ | 서명자 목록의 모든 오류를 "없음"(레거시)으로 처리 | **TestRegistryDeletedFallsBackOnlyWhenAbsentNotWhenBroken**, **TestRegistryInOrchOwnedDirRefused**, **TestRegistryMode0666Refused**, TestLoosePermsRegistryFailsClosed, TestSignedModeAndNotSignedModeSentinel, api.TestGrantSignersUnavailable503 |
| ⑦ | `GrantSigned`가 `issued` 신선도 검사를 건너뜀 | **TestStaleIssuedRejected**, TestGrantSignedIdempotentAndConcurrent, api.TestGrantStaleIssued403 |
| ⑧ | 쓰기의 CSRF 헤더 검사 제거 | **TestCSRFCrossOriginRejected**, TestWriteTierSessionCSRF, TestCSRFHeaderRequiredOnWrite, TestWriteTierCoversEveryMutatingRoute, TestGrantLedgerViaSession, TestOriginCheckedOnAuthSession |
| ⑨ | Host 허용 목록 검사 제거 | **TestHostRebindingRejected**, TestHostMismatchRefused, TestSecurityHeadersPresent, TestAPIResponsesNoStore, TestStaticServedWithCSPAndTypes |
| ⑩ | (CF21) 유효한 자격의 전역 한도 면제 제거 | **TestValidCredentialBypassesGlobalAuthCeiling**, **TestCredentialExemptionOnlyOnSessionRoutes** |
| ⑩b | (수정 1회차) 면제를 모든 `/auth/*` 경로로 넓힘 | **TestCredentialExemptionOnlyOnSessionRoutes** |
| ⑪ | `limitBody`의 `MaxBytesReader` 감싸기 제거 | **TestLimitBodyWrapsChunkedBody** |
| ⑫ | `Sec-Fetch-Site` 값 1개 조건 제거 | **TestSecFetchSiteSingleValue** |
| ⑬ | `activePasskey`의 종류 재확인 제거 | **TestActivePasskeyTypeRecheck** |
| ⑭ | CLI 게시 승인의 "서버 설명(검증 안 됨)" 표시 되돌림 | TestApproveSignedFlow(이번에 단언 추가) |
| ⑮ | 승인 파일 갱신의 `flock` 제거 | **TestConcurrentGrantsOneEntry**, TestConcurrentGrantsNoLostEntries, TestGrantSignedIdempotentAndConcurrent |

결과: 16개 모두 잡혔다(잡히지 않은 변이 없음). 브리프의 9개(①~⑨) 중 ①, ②(폴더 검사, 두 형태 m2a·m2b), ③, ④, ⑤, ⑥, ⑧, ⑨와 ⑩은 **검토자가 따로 재현해 같은 결과를 확인**했다(m1, m2a, m2b, m3, m4, m5, m6, m8, m9, m10). ⑦과 ⑪~⑮, ⑩b는 구현자 실행만이다. 각 변이는 `go vet`도 통과한 상태로(컴파일 오류로 실패한 것이 아님) 확인했다.

## 4. 알려진 한계와 확인하지 않은 것

확인하지 않은 것(실제 환경 필요):
- **`tailscale serve`의 실제 동작.** Host 헤더 보존과 `Tailscale-User-Login` 헤더 처리(클라이언트가 보낸 값 제거)는 Task 21 검토에서 tailscale 소스를 읽어 확인했을 뿐 실제로 실행하지 않았다. HTTPS 인증서 발급(가정 A1)도 미확인.
- **실제 브라우저의 패스키 의식.** 등록·로그인·승인 서명은 합성(소프트웨어) 인증기로만 시험했다. 실제 기기(iCloud 키체인 등)의 `navigator.credentials` 동작, 서명 카운터 0 보고, rpId 생략 시 동작은 미확인.
- **키보드·스크린 리더 사용.** 화면은 ego-browser로 1280/390px 모습만 봤다. 키보드만으로의 전체 흐름과 스크린 리더 낭독은 하지 않았다.
- **nft 규칙.** `infra/nftables`에 443 규칙 1줄을 추가했지만 커널에 올리지도, `nft -c`로 검사하지도 않았다(이 Mac에 nft 없음).
- **systemd 단위.** 플래그 예시를 넣은 단위 파일을 만들었을 뿐 systemd로 시작해 보지 않았다.

알려진 한계(코드에 남아 있음):
- 침해된 서버가 사용자에게 다른 diff를 보여 주는 위험은 남는다(설계서 10장). CLI와 웹 모두 서버가 준 텍스트로 해시를 다시 계산하지만, 그 텍스트가 실제 저장소의 diff인지는 서버를 믿는다.
- diff 응답의 파일 목록(`files`, numstat)에는 상한이 없다(시험: 1,800개 파일 변경이 그대로 나옴). 해시 대상 텍스트는 256 KiB에서 잘린다.
- 같은 서명이 여러 armored 표현을 가진다. 승인 항목은 `(repo, base, hash)`로만 구분하고 서명 문자열을 식별자로 쓰지 않는다(CF3).
- CF21 면제는 `GET /auth/session`, `GET /auth/signers`, `POST /auth/logout`에만 있다(`HEAD /auth/session`은 면제 아님). 그 밖의 인증 경로에서는 로그인한 사용자도 전역 한도에 막힐 수 있다.
- Host 검사는 포트를 무시한다(`tailscale serve`가 브라우저의 Host를 `:443` 유무와 관계없이 넘기기 때문). 허용된 이름이면 어떤 포트든 통과한다.
- 단일 사용자 시험 환경의 한계: 서명자 목록의 "폴더는 orch 소유, 파일은 root 소유" 조합은 소유자를 바꿀 수 없어 직접 만들지 못했다. 소유자 규칙(uid 0만)을 시험 사용자 소유 배치에 적용하는 방식과 폴더 쓰기 권한 비트로 확인했다.
- 서명 필수 모드는 프로세스 전역(R7)이라, 같은 기계에서 혼자 쓰는 개인 `aigate`도 목록 파일이 있으면 서명 필수가 된다(설계서 8.5의 "orchd의 gate-home에만"과 다름).

## 5. 남은 결정 (사용자)

1. **잘린 diff 정책(R22).** 지금은 웹과 CLI 모두 256 KiB를 넘는 보호 파일 diff는 서명을 거부하고, 저장소에서 `git diff`로 보고 커밋을 나누라고 안내한다. 정말 큰 보호 파일 변경을 승인할 다른 길(예: 서버 밖에서 해시를 계산하는 도구)을 둘지.
2. **소켓 uid 기본값.** 기본은 orchd 자신의 uid만 허용한다. `sudo orch …`를 쓰려면 `--socket-uid 0`을 더해야 한다. 운영자 uid를 기본에 넣을지.
3. **diff 해시 16자리(관찰 O1).** 승인과 서명이 묶이는 diff 해시는 SHA-256의 앞 16 hex(64비트)다. 약 2^32번 시도로 같은 해시의 두 diff를 만드는 생일 공격이 이론상 가능하다. 전체 64자리로 넓히면 저장된 승인을 옮겨야 한다(게시 해시는 이미 64자리).
4. **서명자 목록의 범위.** 목록은 기계 전체(프로세스 전역 경로)에 적용된다. orchd의 gate-home에만 적용하도록 범위를 좁힐지(설계서 8.5 문구와 맞추기).
