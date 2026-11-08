# orch 승인 흐름 데모 (Stageshow)

orch 콘솔의 승인 흐름("승인 대기 → 터미널에서 승인 → 승인됨")을 짧은 반복 애니메이션으로 만든 **가짜 UI 데모**다. 실제 콘솔 화면이 아니라 블로그·포트폴리오용 일러스트이고, 숫자와 문구는 설명용이다.

| 파일 | 내용 |
|---|---|
| `media/orch-approve.gif` | 약 11초 반복 GIF(760x540, 2.5MB). 블로그에 바로 올릴 수 있다 |
| `media/poster.png` | 모션 줄임 설정/썸네일용 한 장(이야기가 한 장으로 읽힌다) |
| `media/loop-sheet.png` | 루프 전체 컨택트 시트(검토용) |
| `orch-approve/` | React 컴포넌트(`index.tsx`, `orch.css`). Stageshow 킷 위에서 동작 |
| `REHEARSAL.md` | 촬영·게이트·독립 검토 기록, 미확인 항목 |

## 다시 만들거나 고치려면
Stageshow 스킬(<https://benji.org/stageshow>, `benjitaylor/skills`)이 필요하다. 설치 폴더를 건드리지 않으려고 복사본에서 작업했다.

```sh
cp -R ~/.claude/skills/stageshow /tmp/stageshow-work && cd /tmp/stageshow-work
cp -R <이 저장소>/docs/web-demo/orch-approve examples/orch-approve
npm install && npm run dev                       # 미리보기 서버
node scripts/film.mjs "http://127.0.0.1:<포트>/?capture=orch-approve" --out shots/loop.png --every 800 --count 18 --gate
```
`npm run dev`는 선택 의존성 `portless`가 로컬 프록시(포트 1355)를 백그라운드로 띄우므로, 끝나면 프록시도 종료한다.

## 한계
- 넓은 화면(760x540) 하나만 있다. 좁은 폭(compact) 설계는 하지 않았다.
- MP4는 이 Mac에서 인코더(arm64 `ffmpeg`)가 없어 만들지 못했다. GIF는 헤드리스 크롬 화면 스트림(약 26fps)을 15fps로 다시 샘플링한 것이다.
