# Rehearsal: orch-approve

Gold: `dark-hero` (the shell, the dark look, the row that opens at the top and changes status on its own pill, the reset while closed) and `hero-demo` (a terminal satellite that comes forward at the beat it pays off).
Kept: stage 760 wide, window inset top-left and bled off the right and bottom, one lit color (green, once), the pill that eases from its left edge.
Changed: the product (orch approvals), Korean strings, a terminal that rides in from below the stage, a pending-count in the sidebar, no cursor.

Beats (loop about 11.2 s): 1 a new approval row opens at the top with an amber "승인 대기" pill and the sidebar count rolls to 1. 2 a terminal rides in. 3 `orch approve 3` is typed, its short output appears, `yes` is typed, `approved.`. 4 the pill turns to "승인됨" on itself and the count rolls to 0 (payoff, held 2.3 s). 5 the terminal leaves, the row closes, the story's state is reset while the row is closed.

Captured: wide 760x540 loop (18 frames at 800 ms, the arrival beat at 40 and 50 ms, the sidebar count zoomed at 60 ms), opening still, reduced-motion poster, a real-time recording (the GIF).
Gate: `rehearsal passed (necessary, not sufficient: ...)` on the wide loop; motion coverage is `inconclusive` (sampling gaps up to 48 ms).
Typecheck: `npx tsc --noEmit -p playground` clean. Class clash scan: none.

Film lines and what was done:
- framing: terminal 24px / 30px from the edge, a sliver past the bottom, window only 6px past the bottom -> moved the terminal to the one gutter (48px), made the stage taller (540) and the window body taller so it runs well past the bottom. Cleared.
- lands on a transform / steps in its box (terminal, the "작업 #2" tag) -> `will-change: transform` on both. Cleared.
- jump / settle snap on the typed command and the `yes` line -> `data-typing` and `data-snap` on exactly those two lines (typing is meant to step). Cleared.
- appears with no entrance: the sidebar count -> filmed at 60 ms zoomed: the pill did fade and the digit did roll, but in about 0.1 s. Slowed the fade to 0.45 s and the roll to 700 ms; the line no longer appears.
- manual lines about the terminal body's bottom geometry moving -> deferred by the tool while the terminal is sliding; the settled poster and the loop frames show no overflow. Left as the tool's own note.

Review (fresh eye, a separate agent given only review.md, verify.md, the frames, the gold still and a one-line brief) -> response:
1. Void under the list in the opening still -> agreed. Five filler rows now run off the bottom (`shots/still.png` re-filmed).
2. Terminal looks like a floating card covering the lower rows -> kept on purpose: it covers only filler bars and the sidebar's quiet half, every real row stays whole, and it sits on the one gutter the gate asks for. Not changed.
3. Gray bars fill the lower list -> same fix as 1; real words stay on the rows the story is about.
4. Sidebar half bars with a void -> kept, same as dark-hero.
5. Dim terminal lines too faint -> agreed. `--ss-term-dim` raised to `#a3adb8` on the terminal.
6. Exit and seam -> reviewer found it reads as a deliberate reset; no change.
7. Row entrance not verified by the reviewer -> filmed at 40 ms (`shots/beat-arrive.png`): the list opens about 36px, the row drops in and is whole by about 0.3 s.
8–9. Small badge, count fade at the seam -> no change on a desktop hero; the count fade is now slower (see above).

Assumptions: the Korean strings are product copy; the sample numbers (항목 3, 작업 #5) are illustrative. The demo does not show the real console data.
Unresolved: no `compact` (phone) design. A 390 px host would show the wide design scaled down (small text). It needs its own composition (terminal included), not a squeeze.
Not checked: `--states --gate`, a 390 px film, the light look, a 4x zoom of the pill and terminal strokes, reduced-motion toggled during playback, a real device, a frame-rate log in a real browser (the GIF is a headless Chrome screencast at about 26 fps, not a rate measurement). The reviewer did not check the terminal open beat at 40 ms either; the frames at 800 ms show it arriving whole.
