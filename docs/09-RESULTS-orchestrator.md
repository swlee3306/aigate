# 서브프로젝트 ② 오케스트레이터 — 결과 기록 (작성 중)

## Task 5: 원격 실행기 동등성
- fakessh 백엔드: 기존 e2e를 그대로 실행, local과 fakessh 모두 156 pass / 0 fail, 판정 148개 동일(`e2e/compare-backends.sh`).
- 적대 시나리오 17개(`e2e/scenarios_remote_test.go`): 위조·거대·끝없는 출력, 깨진 XML, 전송 위조. ok로 끝난 것 없음.
  - 과대 결과(9 MiB JUnit/JSON, 끝없는 JSON)와 전송 위조(flood, 잘림, 거대 선언 길이, unreachable)는 `verification_incomplete`
    **판정이 아니라 gate 오류**(종료 코드 64, 판정·보고서 없음)로 끝난다. 로컬 실행기도 같다. 설계(인프라 오류는 `error`, 판정과 분리,
    오류를 ok로 처리하지 않음)와 일치하므로 유지한다. 오케스트레이터(T7)는 gate 오류를 `error` 상태로 두고 재시도 횟수를 제한해야 한다
    (결정적 과대 출력이 무한 재시도가 되지 않게).
  - exit-code 어댑터 검사는 VM이 말하는 종료 코드를 그대로 믿으므로 rc 거짓말을 탐지할 수 없다(설계상 한계). junit/go-json 검사는
    결과 파일이 없으면 incomplete.
- 실제 VM(test-vm 10.70.0.13, orch-vm 경유 점프, 호스트키 엄격 검사, 역할 키): 대표 시나리오 9개(py·go·ts 각 3개: ok / weakened / behavior 또는 weakened)
  **전부 로컬과 같은 판정으로 통과**. 소요 시간: VM 54 s, 로컬 15 s (약 3.6배; 시나리오당 py 3~4 s, go 6~7 s, ts 8 s).
- 첫 실행에서 TS 3개가 실패한 원인은 하네스: 로컬 `node_modules` 심볼릭 링크 setup이 VM에서 깨진 경로. VM 백엔드에서는 `npm ci`로 바꿨다(결과는 verification_incomplete로 끝나 ok로 오인되지 않았다).
- VM 준비(기록): test-vm에 `python3-pytest python-is-python3`(apt), Node 22(npm `node@22`; apt의 Node 18은 vitest 5.1 요구 미달),
  runner 계정 `go env -w GOTOOLCHAIN=go1.27.1`(go.mod `go 1.27`을 Go 1.22가 `go1.27`로 해석해 실패). 이 변경은 `test-vm-clean` 스냅샷에 **포함되지 않았으므로** Task 9에서 스냅샷을 다시 만들어야 한다.
