# orch 웹 콘솔 목업

읽기 전용 v1 화면(FR15, OR11)의 정적 목업. 실제 코드가 아니라 설계 입력이다.

| 파일 | 내용 |
|---|---|
| `index.html` | v1: GitLab식 목록 + 개요만 Qolly식 흐름도 |
| `index-v2.html` | v2: 전체를 Qolly 스타일 3단(메뉴 / 점 격자 캔버스 / 인스펙터)으로 통일 |
| `index-v3.html` | v3: v2 + ui-ux-pro-max 검토 반영(아래). **현재 기준안** |

브라우저에서 파일을 바로 연다(`#/overview`, `#/tasks`, `#/tasks/38`, `#/approvals`, `#/diff`).
작업 목록·시간·상태 수는 2026-11-08 실제 값, 승인 대기 1건과 변경 내용은 설명용 예시다.

## 참고한 것
- 시각 언어: Dribbble "Qolly - AI Outreach Automation Workflow UI UX Design" (구조·스타일만 참고, 로고·일러스트·문구는 쓰지 않음).
- 목록·사이드바 문법: 사내 GitLab 화면.
- 검토 도구: <https://github.com/nextlevelbuilder/ui-ux-pro-max-skill> (커밋 4f2dbb6, 2026-11-08). 로컬 CSV 검색 도구이며 이 저장소에는 설치하지 않고 임시 폴더에서 실행했다.

## v2 → v3에서 바뀐 것과 이유
| 변경 | 근거 |
|---|---|
| 결과 노드 보조 글자 `#aebdba` → `#d6e4e1` | 가장 밝은 그라디언트 지점(#1f6a3d)에서 3.38:1로 4.5:1 미달이었음 → 5.03:1 |
| "실시간" 표시에 마지막 갱신 시각 + **일시정지** 버튼, 일시정지 상태는 색·모양·글자로 표시 | 스킬 Real-Time 패턴: live 표시는 갱신 시각과 stale 상태를 보이고 갱신을 멈출 수 있어야 함 |
| 갱신 상태를 `role="status" aria-atomic` 한 곳에서 안내 | UX 지침 "Contextual Live Badge Updates" |
| 파이프라인 노드에 `aria-pressed`(선택 상태) | "Compact Control Semantics" |
| 작업 행 선택을 `<tr>` 클릭이 아니라 네이티브 `<button aria-pressed>`로 | 같은 지침(클릭 가능한 비버튼 요소 금지) |
| 표를 `overflow-x:auto`로 감쌈 | "Table Handling" |
| 유리 패널에 `backdrop-filter: blur(14px)`, 호버 전환 180ms | 스킬 추천 스타일(Glassmorphism)의 핵심 효과 |
| 터치(coarse pointer)에서 버튼·메뉴 최소 44px | 스킬 접근성 우선순위 2 |

## 스킬 추천 중 따르지 않은 것
- 색상: 스킬은 슬레이트 네이비 `#0F172A` 계열을 추천했지만, 사용자가 고른 Qolly 참고안의 초록·청록 팔레트(`#020404 #03344E #2E8A4E #115E5C`)를 유지.
- 패턴 "Real-Time / Operations Landing"은 랜딩 페이지용이라 앱 콘솔에는 맞지 않아 쓰지 않고 live 표시 규칙만 가져옴.
- 글꼴 Fira Code/Fira Sans: 한글 글리프가 없고 Google Fonts는 외부 요청이라 쓰지 않음(시스템 한글 글꼴 + 등폭은 시스템 mono). 구현 때 쓰려면 자체 호스팅.

## 대비 계산(WCAG 2.x, v3 기준)
패널 위 본문 15.86 · 보조 글자 9.34 · 약한 글자 6.03 · 상태 칩 6.65~7.43 · 링크 9.22 · 노드 보조(유리 8.56, 결과 노드 가장 밝은 지점 5.03) · diff 추가 11.63 · 연결선(비텍스트) 6.04~8.59(기준 3:1).

## 아직 확인하지 못한 것
- 모바일 폭(375/768px)을 화면으로 확인하지 못함(CSS만 있음).
- 스크린리더 실제 낭독, 키보드 전체 순회.
