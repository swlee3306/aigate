# 웹 콘솔 W1 — 결과 기록 (2026-11-09)

> 상태: **구현 완료, 컷오버 대기.** 코드와 시험은 이 저장소에서 끝났고, 실제 서버(orch-vm)에는 아직 아무것도 배포하지 않았다. 배포·되돌리기 절차는 `10-OPERATIONS-orchestrator.md` 9장, 설계는 `11-DESIGN-webconsole-W1.md`.
> 이 문서의 "확인함"은 이 저장소에서 실제로 실행한 시험(macOS, 합성 인증기, 실제 `ssh-keygen`)만 가리킨다. `tailscale serve`, 실제 브라우저 패스키, nft 적재, systemd 시작, 게이트 비용 측정, 키보드·스크린 리더 확인은 **한 번도 하지 않았다**(6장에 컷오버 때 확인할 목록).

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
- **`orchd` 침해의 범위(전체 브랜치 검토에서 정정).** 서명 승인이 막는 것은 위조·재사용이다: API를 통한 가짜 승인, `approvals.json`에 직접 쓴 항목은 루트 소유 서명자 목록으로 검증되지 않아 통과하지 못한다. 그러나 **`orch` 사용자로 임의 코드가 실행되는 경우는 막지 못한다.** 그 코드는 (a) 보호되지 않은 바이너리를 바꿔치기할 수 있고(그래서 런북은 바이너리를 root 소유 `/usr/local/lib/aigate`에 둔다. 배포 전 필수), (b) 읽기·쓰기 가능한 배포 키 `/home/orch/orchd/ssh/id_gitlab_deploy`로 **승인 없이 원격에 push**할 수 있고(바이너리 소유권과 무관), (c) `approvals.json.lock`을 쥐고 모든 승인을 멈출 수 있다. 설계서 10장의 "작업 취소·열람만 가능"은 이 범위에서 과장이었다(해당 표를 정정함). 또 **게시(publish) 종류 승인은 `(task, head, action)`을 서명하고 `head`에 대한 서버의 판단을 믿는다.** 설계서 8.7(표시와 해시의 일치는 서버를 믿음)은 diff 승인뿐 아니라 게시 승인에도 적용된다.
- 침해된 서버가 사용자에게 다른 diff를 보여 주는 위험은 남는다(설계서 10장). CLI와 웹 모두 서버가 준 텍스트로 해시를 다시 계산하지만, 그 텍스트가 실제 저장소의 diff인지는 서버를 믿는다.
- diff 응답의 파일 목록(`files`, numstat)에는 상한이 없다(시험: 1,800개 파일 변경이 그대로 나옴). 해시 대상 텍스트는 256 KiB에서 잘린다.
- 같은 서명이 여러 armored 표현을 가진다. 승인 항목은 `(repo, base, hash)`로만 구분하고 서명 문자열을 식별자로 쓰지 않는다(CF3).
- CF21 면제는 `GET /auth/session`, `GET /auth/signers`, `POST /auth/logout`에만 있다(`HEAD /auth/session`은 면제 아님). 그 밖의 인증 경로에서는 로그인한 사용자도 전역 한도에 막힐 수 있다.
- Host 검사는 포트를 무시한다(`tailscale serve`가 브라우저의 Host를 `:443` 유무와 관계없이 넘기기 때문). 허용된 이름이면 어떤 포트든 통과한다.
- 단일 사용자 시험 환경의 한계: 서명자 목록의 "폴더는 orch 소유, 파일은 root 소유" 조합은 소유자를 바꿀 수 없어 직접 만들지 못했다. 소유자 규칙(uid 0만)을 시험 사용자 소유 배치에 적용하는 방식과 폴더 쓰기 권한 비트로 확인했다.
- 서명 필수 모드는 프로세스 전역(R7)이라, 같은 기계에서 혼자 쓰는 개인 `aigate`도 목록 파일이 있으면 서명 필수가 된다(설계서 8.5의 "orchd의 gate-home에만"과 다름).

## 5. 설계서의 가정(A)과 실험(X) 결과

판정은 셋 중 하나: **검증됨**(어떻게), **일부 검증**(무엇을, 무엇은 안 함), **검증 못 함**(이유). 근거는 이 저장소의 시험, 검토자의 소스 읽기, 구현 보고서뿐이다.

| # | 설계서의 가정/실험 | 판정 | 근거와 한계 |
|---|---|---|---|
| A1 | `tailscale serve`로 HTTPS를 붙일 수 있고 인증서가 발급된다 | **검증 못 함** | orch-vm에서 `tailscale serve`를 한 번도 실행하지 않았고 Tailscale 관리 콘솔의 HTTPS 설정도 보지 못했다(이 환경에서 접근하지 않음). 스크립트는 드라이런·잘못된 인자 거부만 시험했다(`infra/test-web-scripts.sh` 17개). 설계서가 적은 "`bon-openstack`이 443을 tailscaled로 쓰는 것으로 보인다"도 미확인. 컷오버 3단계에서 확인한다. |
| A2 | 서명 `issued` ±5분이 CLI·웹 모두에 충분하다 | **일부 검증** | ±5분을 서버가 강제하는 것(`TestStaleIssuedRejected`, `api.TestGrantStaleIssued403`)과 CLI가 로그인 메시지의 시각을 ±5분으로 확인하는 것은 시험했다. "충분한가"는 실사용(시계 오차, 서명 화면에서 머무는 시간)을 봐야 하며 **하지 않았다**. 서명 화면을 5분 넘게 열어 두면 거부될 수 있다(코드 읽기에 따른 추정). |
| A3 | CLI가 서버가 준 해시 대상 텍스트로 해시를 다시 계산할 수 있다(`hashOf`가 순수 해시) | **검증됨** | 코드를 읽었다: `hashOf`는 입력 문자열의 SHA-256뿐이다(`internal/gitx/gitx.go`, 외부 상태 없음. 2026-11-09 결정 R23 이후 전체 64 hex, 그 전에는 앞 16 hex). 시험: `gitx.TestHashOfMatchesDiffHash`(공개 `HashOf` = 게이트의 diff 해시), `cmd/orch` `TestApproveRefusesHashMismatch`, `TestApproveSignedFlow`, `remote_e2e_test.go`(원격 CLI가 서버 텍스트로 재계산 후 서명). 한계: 재계산은 "보인 글자 = 해시"만 보증한다. 그 글자가 실제 저장소 diff인지는 서버를 믿는다(설계서 10장). |
| A4 | Mac의 SSH 키(ed25519)로 `ssh-keygen -Y sign` 서명이 Go에서 검증된다 | **검증됨** | 실제 `ssh-keygen -Y sign`(이 Mac의 OpenSSH 10.3p1)이 만든 서명을 `x/crypto/ssh` 기반 검증기가 통과시킨다(`signing.TestVerifyOK`: sha512 기본과 `-O hashalg=sha256`, `cmd/orch` `TestDefaultSignCmdSSHKeygen`, `TestCLILoginRealSSHKeygen`, 원격 e2e). 한계: ed25519만 지원, 하드웨어 키(`sk-`)와 ssh-agent 경로는 시험하지 않았다. 다른 OpenSSH 버전은 Mac 쪽 서명 도구로만 의미가 있고 검증은 Go에서 하므로 VM의 OpenSSH와 무관하다. |
| A5 | 세션 수명(유휴 8시간, 절대 24시간)이 개인 사용에 적절하다 | **검증 못 함** | 값이 구현된 것(만료·절대 상한 시험)만 확인했다. 적절한지는 써 봐야 안다. |
| X1 | orch-vm에서 `tailscale serve` HTTPS와 패스키 등록·로그인 | **검증 못 함** | 실제 노드·실제 브라우저에서 한 번도 못 했다. 간접 근거만 있다: (1) Task 21 검토자가 **tailscale 소스를 읽어** `serve`가 클라이언트가 보낸 `Tailscale-User-*` 헤더를 지우고 Host를 보존한다(`r.Out.Host = r.In.Host`)고 확인했다. 이것은 **소스 읽기이며 실행이 아니다.** (2) 패스키 등록·로그인·서명은 합성(소프트웨어) 인증기로만 시험했다. 실제 기기의 `navigator.credentials`, 서명 카운터 0 보고, rpId 생략 동작은 미확인. 컷오버 단계 3·5에서 확인한다(6장). |
| X2 | `ssh-keygen -Y sign` 서명을 Go로 검증(정상 통과, 변조·다른 네임스페이스 거부) | **검증됨** | 정상 서명 통과(위 A4). 변조: 실제 서명을 모든 오프셋에서 잘라 보고 바이트마다 뒤집어도 통과하지 않는다(`signing.TestMalformedArmorNoPanic` 등). 다른 네임스페이스: 로그인(`aigate-login`)과 승인(`aigate-approval`) 서명이 같은 바이트에 대해 서로 통과하지 않는다(`TestLoginVsApprovalNamespacesDisjoint`, `signing.TestLoginSignatureNotAcceptedAsApproval`, `api.TestApprovalSignatureNotAcceptedAsLogin`). |
| X3 | 게이트의 서명 검증 비용이 1초 이상 영향을 주지 않는다 | **검증 못 함** | **측정하지 않았다.** VM에서도 이 Mac에서도 서명 검증 경로의 시간을 재지 않았고 벤치마크도 없다. 보고서 어디에도 수치가 없다. 추정(측정 아님): 승인 검사(`Valid`)마다 서명자 목록 파일을 읽고 항목당 ed25519 검증 1번이라 파일 읽기와 수십 µs 수준일 것으로 보이나, 확인 전에는 결론이 아니다. 컷오버 4단계 뒤 `orch show`의 판정 시간을 단계 1(레거시)과 비교한다. |

## 6. 컷오버 때 확인할 것

아래는 이 저장소에서 할 수 없었던 것이고, `10-OPERATIONS-orchestrator.md` 9장의 각 단계 검증 체크리스트에 들어 있다. 하나라도 어긋나면 그 단계에서 멈춘다.

| 항목 | 단계 | 무엇을 보나 |
|---|---|---|
| `tailscale serve` HTTPS(A1, X1) | 3 | 관리 콘솔 HTTPS 켜기 → `serve --apply` → 브라우저에서 인증서 경고 없이 `https://<FQDN>/` |
| Host·`Tailscale-User-*` 헤더 처리(X1) | 3 | 소스를 읽어 확인한 동작이 실제로도 맞는지: `--allowed-host`가 맞으면 421이 나지 않는다. 클라이언트가 `Tailscale-User-Login`을 보내도 서버 키로 쓰이지 않는지는 별도 시험이 필요하다 |
| 방화벽 443 규칙·nft 적재 | 3-d | `apply-firewall.sh`의 `nft -c` 통과, 22번 접속 유지, 규칙이 `serve`에 필요한지(필요 없을 수 있다) |
| systemd 유닛 시작 | 3-b | 새 플래그로 시작, `ProtectSystem=strict`에서 대기 목록 파일 쓰기, `socket uids allowed: [0 <orch uid>]` 로그 |
| 실제 브라우저 패스키(X1) | 5 | 등록 → 지문 대조 → 활성화 → 로그인 → 서명 승인. 서명 카운터 0, rpId, 동기화 패스키(iCloud) 동작 |
| 서명 검증 비용(X3) | 4 | 게이트 시간이 레거시 대비 늘지 않는지(재측정) |
| 시계·`issued` ±5분(A2) | 4, 5 | 서명 실패 사례가 없는지 |
| 키보드·스크린 리더 | 5 | 화면은 ego-browser로 1280/390px 모습만 봤다. 키보드만으로 전체 흐름, 스크린 리더 낭독은 하지 않았다 |
| 옛 빌드의 대기 승인(R23) | 1 | 배포 전부터 대기 중이던 승인은 16자리 해시라 새 빌드에서 무효. `orch approvals`로 남은 항목을 확인하고 다시 승인 |
| 롤백이 실제로 되는지 | 6 | `signers.json` 삭제 후 재시작 없이 레거시로 돌아가는지(코드 읽기 기준일 뿐). 옛 바이너리 복원 시 DB 스키마 v6 문제 |

## 7. 설계서와 달라진 곳

설계서는 고치지 않았다(상태 줄만 바꿈). 구현이 설계서·계획서와 다르게 정한 것:
- **CSRF 헤더**는 `X-Orch-CSRF`(설계서 7장 문구 그대로; 계획서의 `X-CSRF-Token`이 아님, R17-1). `GET /auth/session`이 호출할 때마다 CSRF를 새로 발급한다(R20-1). 다른 탭이 열리면 앞 탭의 CSRF가 무효가 되어 웹 셸이 한 번 다시 받아 재시도한다.
- **세션 수명** 유휴 8시간·절대 24시간(설계서와 같음), 베어러 토큰 15분. `--session-ttl`로 유휴 시간만 5분~24시간으로 바꿀 수 있고 절대 상한 24시간은 고정이다. 쿠키 이름은 `__Host-orch_session`(Secure, HttpOnly, SameSite=Strict).
- **서명자 목록 위치**: 설계서 8.4의 `/etc/aigate/signers.json`과 같다. macOS 개발용 기본값만 `/private/etc/aigate/signers.json`(`/etc`가 심볼릭 링크라 fail closed가 되는 것을 피함, 운영 Linux 경로는 그대로).
- **대기 패스키는 파일**(`/home/orch/orchd/state/signers.pending.json`, `--pending-file`)에 있다. 설계서는 위치를 정하지 않았다. 처음 계획은 `/var/lib/orchd`였으나 `ProtectSystem=strict`에서 쓰기 불가라 유닛 위치로 바꿨고 CLI와 orchd 기본값을 맞췄다(R18, Task 22).
- **관리 명령 이름**은 설계서 8.4의 `orch-admin`이 아니라 `orch admin signers add|list|remove|pending`(R2).
- **엔드포인트**: 설계서 6장 표에 없는 `GET /healthz`(무인증), `GET /auth/signers`, `POST /admin/enroll`(소켓 주체만)이 추가됐고, `/ops`에는 VM·쿼터 수치가 없다(R4, W2로). 무인증 `GET /api/v1/…` 없는 경로는 404가 아니라 401이다. 웹 화면이 HTTP 리스너의 `/`를 맡으면서 **옛 평면 경로(`/tasks`, `/approvals`)는 HTTP로는 제공하지 않고 소켓에서만** 쓸 수 있다(기존 시험 3개의 단언을 고쳤다).
- **웹의 diff 해시 재계산**(설계서 D5/8.7)은 W1에서 하지 않았다. 웹은 메시지 문법, 메시지 = 상세의 필드, `sha256(message)` = 다이제스트, 저장소 id, 게시 행위 도출만 확인한다. 대신 잘렸거나 다 보이지 않은 승인은 서명 자체를 거부한다(R22).
- **서명 필수 모드의 범위**는 프로세스 전역(R7)이다. 설계서 8.5는 orchd의 gate-home에만 적용한다고 썼다.
- **`--socket-uid`** 플래그가 추가됐다(설계서에 없음). 기본은 orchd 자신의 uid만 허용한다.

전체 브랜치 검토에서 문서에 빠져 있다고 확인된 조용한 차이 네 가지:
- **WebAuthn 서명 카운터를 검증하지 않는다**(R17, 설계서 8.2). 감수한 위험: 동기화되는 패스키는 카운터 0을 보고하고, 사용자는 한 명이며, 사용자 확인(UV)을 필수로 요구한다.
- **원장에 주체·경로가 남는 것은 `approval_granted`뿐이다**(signer, via). 제출·취소·재시도 이벤트는 누가 했는지 기록하지 않는다(설계서 8.6의 "모든 쓰기 동작에 누가·경로"보다 좁음). W2 후보.
- **`Idempotency-Key`는 `POST /tasks`만 따른다**, `GET /projects/{p}`는 구현하지 않았고 `/projects` 목록만 있다(설계서 6장).
- **게이트의 `--signers-file`/`AIGATE_SIGNERS`(설계서 8.4)는 일부러 구현하지 않았다.** `orch`가 정할 수 있는 모드 스위치는 서명 필수 모드를 끄는 구멍을 다시 연다. 서명자 목록 경로는 프로세스 전역이며 orchd의 `--signers-file`과 root의 `orch admin`으로만 정한다. 설계서 8.5 문구를 이에 맞춰 고친다: 서명자 목록의 범위는 프로세스 전역이다.

## 8. 사용자가 정할 것

1. **잘린 diff 정책(R22).** 지금은 웹과 CLI 모두 256 KiB를 넘는 보호 파일 diff는 서명을 거부하고, 저장소에서 `git diff`로 보고 커밋을 나누라고 안내한다. 처음 구현(웹에서 "잘림 확인"을 입력하면 서명 허용)은 철회했다. 정말 큰 보호 파일 변경을 승인할 다른 길(예: 서버 밖에서 해시를 계산하는 도구)을 둘지.
2. **소켓 uid 기본값.** 기본은 orchd 자신의 uid만이다. `sudo orch …`는 `--socket-uid 0`을 더해야 하고, 지금 유닛 예시는 `--socket-uid 0`을 넣었다. 운영자 uid를 기본에 넣을지, `0`을 빼고 `sudo -u orch`로만 운영할지.
3. **diff 해시 16자리(관찰 O1) — 해결됨: 전체 64자리로 확장 (2026-11-09 사용자 결정, R23).** 원래 문제: 승인과 서명이 묶이는 diff 해시가 SHA-256의 앞 16 hex(64비트)였다. 승인은 `(repo, base, hash)`에 묶이고 `head`에는 묶이지 않으므로, 악의적 에이전트가 약 2^32번의 작업으로 같은 해시의 두 diff(다른 head)를 만들어 하나를 승인받고 다른 것을 재시도에 쓸 수 있었다. 이제 `gitx.HashOf`는 SHA-256 전체(64 hex)를 돌려주고, 서명 메시지(`signing.Message.Validate`/`ParseMessage`)와 웹 `MESSAGE_RE`는 모든 종류에서 정확히 64자리 소문자 hex만 받는다(16자리 메시지는 거부, 짧은 해시로 낮추는 길이 막힘). 게시 해시(`approvals.PublishHash`)는 원래 64자리라 그대로다. 설정 해시(`config.Hash`, 16자리)는 보고서·원장의 `config_hash`에만 쓰이고 승인·메시지에 들어가지 않아 바꾸지 않았다. **운영 영향:** 마이그레이션은 없다. 옛 빌드가 만든 승인(`approvals.json`의 16자리 항목, SQLite `approvals_pending`의 16자리 대기 행)은 새 해시와 맞지 않아 **승인이 없는 것으로 판정된다(fail closed)**. 전환 때 옛 빌드로 대기 중이던 승인은 새 빌드에서 **다시 승인해야 한다**. 옛 행은 목록·상세에 그대로 보이지만(오류·패닉 없음) 서명할 메시지가 나오지 않고 grant는 `hash_changed`로 거부된다. 시험: `gitx.TestHashOfIsFullSHA256`, `signing.TestHashMustBeFull64HexForEveryKind`, `approval.TestOldShortHashEntryNeverValidates{Legacy,Signed}Mode`, `approval.TestGrantSignedRefusesShortHash`, `gate.TestOldShortHashApprovalNotHonored`, `api.TestOldShortHashEntryDetailAndGrant`, `orch.TestOldShortHashPendingEntry`, 웹 `message fields must equal…`(16·63자리·대문자 거부). 변이(`hashOf`를 `[:16]`으로 되돌림)는 5개 패키지 시험 37개가 잡았다.
4. **서명자 목록의 범위(R7).** 목록 경로는 프로세스 전역이라, **같은 기계의 개인용 `aigate` 단독 실행도 `signers.json`이 생기면 서명 필수가 된다**(설계서 8.5는 orchd의 gate-home만이라고 썼다). 범위를 gate-home으로 좁힐지, 설계서 문구를 고칠지.
5. **대기 패스키를 파일에 둔 것(R18).** orchd는 `/etc/aigate`에 쓸 수 없고(R3) 데몬이 꺼져 있어도 root 활성화가 되어야 해서 JSON 파일로 했다. root가 `pending remove`를 하는 것과 orchd의 갱신이 동시에 일어나면 한쪽이 덮어쓸 수 있다(파일 잠금 없음, 알려진 한계). SQLite + 소켓 전용 관리 엔드포인트로 옮길지.
6. **macOS 기본 서명자 목록 경로.** `/private/etc/aigate/signers.json`(개발용). 운영은 Linux라 영향 없음. 그대로 둘지.
7. **컷오버 시점.** 설계서 D3("P5 시험이 모두 통과한 뒤")는 충족했다(적대 시험과 변이 시험 통과). 실제 서버에서의 단계 1~5는 사용자가 정한다.

## 9. 결정(Ruling) 목록

원장(`.superpowers/sdd/12-PLAN-webconsole-W1/progress.md`)의 `Ruling:` 줄과 계획서 표 R1~R6을 옮겼다. "틀렸을 때 비용"은 원장에 적힌 문구이고, 적혀 있지 않은 것은 "미기재"로 둔다(추정하지 않음). 번호 R17~R20은 서로 다른 두 결정에 중복으로 쓰였다.

| id | 결정 | 틀렸을 때 비용 |
|---|---|---|
| R1 | WebAuthn은 직접 최소 구현(ES256, 증명 `none`, 검증만) | `internal/webauthn`의 두 함수 뒤에 있어 라이브러리로 교체가 쉬움 |
| R2 | 관리 도구는 `orch admin signers add\|list\|remove\|pending`(별도 바이너리 없음), 쓰기는 euid 0만 | 이름만 바뀜 |
| R3 | 서명자 목록은 `/etc/aigate/signers.json`, 폴더와 파일 모두 root 소유 | 경로는 플래그라 바꾸기 쉬움 |
| R4 | `/ops`는 정책·빌드·base 캐시·마지막 복원만, VM·쿼터 수치는 W2 | W2 첫 작업으로 추가 |
| R5 | 프로젝트는 테이블 없이 작업의 repo별 집계(생성은 W3) | W3에서 테이블 추가 |
| R6 | `orchd`·`orch`·`aigate` 세 바이너리를 같이 배포 | 컷오버 순서로 해결(옛 게이트는 서명 없는 항목도 통과시킴) |
| R7 | 서명자 목록 경로·옵션은 프로세스 전역(`signing.RegistryPath/SetRegistryPath/DefaultOptions`), `Store{Path}`는 그대로 | 시험이 전역을 바꿈(`t.Cleanup`으로 복원), 경미. 부수 효과: 같은 기계의 단독 `aigate`도 서명 필수가 됨(8장 4번) |
| R8 | 패스키 서명자 항목이 RPID·Origin을 고정, `VerifyOptions`에서는 뺌(게이트는 서버 설정을 모름) | 서명자 목록 스키마 변경 |
| R9 | `signCmd(keyPath, namespace, message)` 하나로 로그인·승인 서명 | 시그니처 변경 |
| R10 | peercred는 빌드 태그 파일로 분리(linux `SO_PEERCRED`, darwin `LOCAL_PEERCRED`) | darwin 태그 파일 |
| R11 | 승인 상세 응답에 `truncated` 포함(CLI 거부에 필요) | 필드 이름 변경 |
| R12 | `TestKindConstantsMatchStore`로 중복 상수를 지킴 | 없음 |
| 미번호(Task 4) | `ProtectedFiles` = `Changed()`와 `AddedGuarded`의 정렬·중복 제거 합집합(브리프는 `Changed()`) | 필드 의미(B7/B8/D2가 읽음) |
| R13 | 정규 승인 메시지는 마지막에 `\n`, 필드는 소문자 hex(repo 12, base 40/64, hash 16/64 → **R23에서 64만**), issued는 UTC 초 단위, task > 0 | 기대 문자열(golden) 변경 |
| R14 | 모든 서명·검증 경로는 먼저 `msg.Validate()`를 부르고 거부(빈 `Text()`나 0 다이제스트는 서명·수용 불가) | 미기재 |
| R15 | `go get golang.org/x/crypto`를 Task 10에서 9로 당김(레지스트리가 ssh 공개 키를 검증) | 없음 |
| R16 | `ErrRegistryAbsent`는 신뢰 검사를 통과한 폴더 안에서 ENOENT일 때만(없는 폴더는 가장 가까운 존재하는 상위가 같은 검사를 통과해야 함) | 미기재(이 결정이 없으면 orch 소유 폴더에서 파일을 지워 서명 모드를 끌 수 있었다) |
| 미번호(Task 12) | 수정 1회차를 별도 재검토 없이 수용(운영 코드 2줄, 나머지 시험) | 검증 시험의 빈틈은 최종 전체 검토에서 드러남 |
| 미번호(darwin) | macOS 기본 서명자 목록 경로 `/private/etc/aigate/signers.json`(Linux는 그대로) | 개발 Mac에서의 동작만 다름 |
| R17(webauthn) | `ParseAttestationNone(attestationObject, rpID)`가 rpIdHash·플래그(UP, UV, AT)도 검증, 공용 `CheckClientData`, 카운터는 검증 안 함 | 시그니처 변경, 경미 |
| R17-1..4 → R19 | 구현자 선택을 설계서 기준으로 수용: CSRF 헤더 `X-Orch-CSRF`, 토큰도 쓰기 가능, 세션 유휴 8시간·절대 24시간·토큰 15분, 승인 grant는 쓰기 티어(세션은 CSRF) | 미기재 |
| R18 | 대기 패스키는 JSON 파일에만 둠(orchd가 원자적으로 쓰고 root CLI가 읽음), 테이블 없음 | 대기 목록을 SQLite + 소켓 전용 관리 엔드포인트로 옮기는 재작업(admin.go, Task 19) |
| R19 | Task 17 구현자의 선택 수용(위 R17-1..4) | 위와 같음 |
| R19-1 | 등록 코드는 `begin`에서 소비(일회용), 의식을 못 끝내면 다시 발급 | 미기재 |
| R20 | `failLimiter` 제거(DoS 면, 이득 없음), 로그인 챌린지 1000개 상한(초과 503), `/auth/*` 속도 제한은 Task 21로 | 무차별 대입은 256비트 nonce + ed25519라 여전히 불가능, 요청 폭주는 Task 21 전까지 무제한 |
| R20-1 | `GET /auth/session`이 세션 CSRF를 회전 | 다른 탭의 CSRF가 무효가 되어 웹 셸이 재조회·1회 재시도해야 함 |
| R21-1 | Origin도 Sec-Fetch-Site도 없으면 **세션 쿠키가 없을 때만** 통과(브리프 규칙대로면 원격 `orch login`이 깨짐) | 검토자가 건전하다고 판단, 잔여 위험은 구현자 보고보다 낮음 |
| R22 | 웹도 잘린·다 보이지 않은 승인에는 서명하지 않음(CLI와 동일), `orch approve` 안내 | 아주 큰 보호 diff는 CLI로만 승인 |
| 미번호(수정 회차) | Task 18, 19, 22, 23, 24, 25, 26의 수정 1회차를 별도 재검토 없이 수용(각각 시험·변이 확인, vet 통과) | 빈틈은 최종 전체 검토(fable)에서 드러남 |
| R23 | (2026-11-09 사용자 결정) 승인 diff 해시를 SHA-256 앞 16 hex에서 전체 64 hex로 넓힘. 서명 메시지·웹 문법의 hash는 모든 종류에서 64자리만. 마이그레이션 없음(옛 16자리 승인은 fail closed, 재승인) | 전환 때 옛 빌드로 대기 중이던 승인을 다시 해야 함. 롤백하면 새 빌드로 만든 64자리 승인을 옛 바이너리가 쓰지 못함(10장 롤백 메모) |
