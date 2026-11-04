<!-- Codex CLI 0.164.0, run 2026-11-04: codex exec --sandbox read-only --skip-git-repo-check -C <repo> -o <out> "<prompt>". Prompt: /Users/sampleuser/.claude/jobs/7017a766/tmp/aigate_adv_prompt.txt (pre-authorized, read-only, report only). One run; no question asked. Saved verbatim below. -->

# Read-only bypass review

Ranked by practical plausibility. I treated the BASE commit and `.aigate.yml` as fixed; `gate.loadConfig` reads the configuration from BASE. The edits below target the HEAD checkout or the test environment. I did not execute any repros.

Code anchors: [`exec.go` — `runSide`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/gate/exec.go>), [`rules.go` — `Evaluate`, `compare`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/rules/rules.go>), [`gitx.go` — `ProtectedDiff`, `removedLines`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/gitx/gitx.go>), [`junit.go` — `ParseJUnit`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/adapter/junit.go>), [`preset.go` — `PresetFor`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/adapter/preset.go>), [`gate.go` — `runCompare`](</Users/sampleuser/Desktop/개인/커리어/study/GPU/lab/code/aigate/internal/gate/gate.go>), and [`scenarios_ts_test.go` — `tsLinkSetup`](</Users/sampleuser/Desktop/개인/ᄏᥲ리어/study/GPU/lab/code/aigate/e2e/scenarios_ts_test.go>).

1. **Return correct results only under the gate’s JUnit environment** — pytest, vitest.  
   **Edit:** In `src/ingest.py`, branch on `os.environ.get("AIGATE_JUNIT")`; in `src/calc.ts`, branch on `process.env.AIGATE_JUNIT`. Use the correct implementation in that branch and a broken one otherwise. `runSide` explicitly sets `AIGATE_JUNIT` for JUnit checks.  
   **Miss:** `rules.compare` sees the same test IDs passing at BASE and HEAD.  
   **Fix:** No small sound fix; code can detect test environments. Additional contexts and tests reduce risk but cannot establish behavior generally.

2. **Return correct results only in Go’s test binary** — go test.  
   **Edit:** In `calc/calc.go`, have `Sum` check `strings.HasSuffix(os.Args[0], ".test")`; call the correct implementation for that case and the broken implementation otherwise.  
   **Miss:** `PresetFor("go")` runs `go test -json`; test results pass while normal program execution is wrong. `compare` checks only those results.  
   **Fix:** No small sound fix; the program can identify that it is running as a test binary.

3. **Make pytest exit nonzero after writing an all-passing JUnit report** — pytest.  
   **Edit:** Append to `tests/test_ingest.py`:
   ```python
   import atexit, os
   atexit.register(lambda: os._exit(1))
   ```
   Pytest writes its report during session finish; the exit handler then forces status 1.  
   **Miss:** `runSide` retains the nonzero `RC` but still parses JUnit. `rules.Evaluate`/`compare` do not reject a nonzero head `RC` when parsed results are nonempty and passing. `check` then accepts the saved `ok` report.  
   **Fix:** Yes. Treat a nonzero JUnit exit with no parsed failure explaining it as `verification_incomplete`.

4. **Append wrappers that turn every pytest test into a no-op** — pytest.  
   **Edit:** Append to `tests/test_ingest.py` a loop that replaces each `test_*` function with a `functools.wraps` wrapper returning `None`. `wraps` preserves names, markers, and signatures, so IDs remain.  
   **Miss:** `ProtectedDiff`/`removedLines` detect removed lines, not added lines; `compare` sees the BASE IDs as passing.  
   **Fix:** A limited fix is to treat additions to protected test files as protected changes too, requiring approval. It would also require approval for benign test additions.

5. **Exclude Go’s real tests and add passing tests with the same IDs** — go test.  
   **Edit:** Add `//go:build ignore` and a blank line at the start of `calc/calc_test.go`. Add `calc/aigate_surrogate_test.go` with no-op `TestSum`, `TestDedupe`, and `TestWindow`, reproducing their existing subtest names such as `empty`, `scattered`, and `full_span`. Break `Sum` in `calc/calc.go`.  
   **Miss:** The build-tag insertion adds lines but removes none. Go emits the same package/test/subtest IDs as passed, and the build check still succeeds.  
   **Fix:** Treat additions to protected test files as protected changes, or require approval for those diffs.

6. **Mock the production module while keeping Vitest test IDs** — vitest.  
   **Edit:** Append a hoisted `vi.mock("./calc", ...)` to `src/calc.test.ts`, supplying correct mock implementations for `sum`, `dedupe`, and `clamp`; break the real implementations in `src/calc.ts`.  
   **Miss:** The existing tests still execute under the same IDs, but against the mock. The file edit is additions-only, so `ProtectedDiff` reports no removed lines.  
   **Fix:** The narrow fix is to require approval for any changes to protected test files. The broader issue is that test code controls what implementation is exercised.

7. **Short-circuit Vitest config after writing a passing report** — vitest.  
   **Edit:** Append to `vitest.config.ts` code that writes JUnit XML containing the BASE `classname`/`name` pairs, then calls `process.exit(0)`. The config runs before test collection.  
   **Miss:** `runSide` trusts the JUnit file and `ParseJUnit` maps its cases directly to passing IDs. Config additions are not counted by `removedLines`.  
   **Fix:** Requiring approval for additions to config files catches this instance. Preventing arbitrary code from producing trusted results needs a stronger runner boundary.

8. **Overwrite pytest’s JUnit file during interpreter shutdown** — pytest.  
   **Edit:** Append an `atexit` handler to `tests/test_ingest.py` that writes `<testcase>` entries for every BASE ID to the path in `AIGATE_JUNIT`. Keep the BASE classname, `tests.test_ingest`, and the existing names (`test_sum_basic`, `test_missing_is_none_not_zero`, etc.).  
   **Miss:** `runSide` parses the file after pytest exits; `ParseJUnit` trusts its contents. If real tests fail and pytest exits nonzero, this also relies on bypass 3, which ignores that exit code.  
   **Fix:** No small sound change while the code under test can write the report file. A stronger isolation and result-collection boundary is needed.

9. **Shadow pytest itself with a repository-root module** — pytest.  
   **Edit:** Add root-level `pytest.py`. Since the preset runs `python -m pytest` from the checkout, the module can write a fabricated JUnit file with all BASE IDs passing and exit 0.  
   **Miss:** The file is outside the stated protected and `approval_on_add` patterns; `ParseJUnit` has no way to authenticate the runner that produced the XML.  
   **Fix:** For this specific shadowing case, launch a verified pytest entry point outside the checkout and validate its import path. That does not solve general report forgery by test code.

10. **Replace the local Vitest executable reached through a shared `node_modules` link** — vitest, conditional on setup.  
    **Edit:** Where setup symlinks an editable `node_modules`, replace `node_modules/.bin/vitest` or its target with a script that writes passing JUnit for the BASE IDs and exits 0. The E2E `tsLinkSetup` uses a shared cache link, and the preset invokes `npx vitest`.  
    **Miss:** `npx` can select the project-local executable; `runSide` does not pin or verify it. If the linked directory is ignored, the change may not make the checkout dirty.  
    **Fix:** Use an isolated, verified tool installation rather than a mutable shared link. This is conditional on the target’s setup using such a link.

11. **Let an optional test failure disappear beside a required build check** — pytest, go test, vitest; only if BASE config marks tests optional.  
    **Edit:** No HEAD config edit is needed. If BASE `.aigate.yml` has `unit: required: false` and a separate required check that passes, break a production function in HEAD.  
    **Miss:** `runCompare` discards all violations from non-required checks; `hasRequired` only verifies that some check is required. This does not apply to the E2E defaults shown, where unit checks are required.  
    **Fix:** Make the policy explicit: require test checks to be required, or prevent `ok` when any configured test check fails.

12. **Break behavior for a valid input absent from the tests** — pytest, go test, vitest.  
    **Edit:** For example, in `src/ingest.py` return `None` when a record’s source is `"customer-42"`; in `calc/calc.go` return `0` when `Sum` receives `999`; in `src/calc.ts` return `0` when `sum` receives `9999`. Choose values not present in the BASE tests.  
    **Miss:** All existing IDs still pass, so `compare` has no regression to report.  
    **Fix:** No sound small gate change; broaden tests with edge cases and properties, while recognizing finite tests cannot cover every valid input.

Static read-only review only; none of these repros were executed.

Weakest point: The gate trusts test IDs and statuses produced by code from HEAD.
Root cause: Additions to protected files are allowed, and the test process can influence its own evidence.
Limit: Even an unmodified suite cannot prove behavior on inputs it never exercises.