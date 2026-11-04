# Fixtures

All six fixtures were captured from REAL runner executions in throwaway
projects under a temp dir (none hand-written). Temp paths were rewritten to
`/tmp/toy` with sed; timestamps/hostnames are as captured.

Versions: pytest from verify-harness venv, go (1.27.x), vitest/jest/jest-junit
from `npm install vitest jest jest-junit` (node 26, npm 11).

## pytest.xml
Project: test_a.py (pass, fail, skip, xfail, parametrized `["a b","한글",1]`,
a class method) and test_broken.py (imports a missing module = collection error).
```
python -m pytest -q --continue-on-collection-errors --junitxml=pytest.xml test_a.py test_broken.py
```
Note: pytest writes non-ASCII param ids escaped (`test_param[한글]`).
xfail is `<skipped type="pytest.xfail">`.

## gotest.jsonl / gotest_buildfail.jsonl / gotest_all.jsonl
Module with pkg `ok` (TestA with subtests `sub case`/`fails`, TestSkip, TestPass)
and pkg `bad` (test file calling an undefined function).
```
go test -json -count=1 ./ok   > gotest.jsonl
go test -json -count=1 ./bad  > gotest_buildfail.jsonl
go test -json -count=1 ./...  > gotest_all.jsonl
```

## vitest.xml
src/a.test.js: pass, fail, `test.skip`, `test.each(['a b','한글'])`.
```
npx vitest run --reporter=junit --outputFile=vitest.xml
```

## jest.xml
a.test.js: pass, fail, `test.skip`, describe + `test.each(['a b','한글'])`.
```
JEST_JUNIT_OUTPUT_FILE=jest.xml npx jest --ci --reporters=jest-junit
```
jest-junit quirks: classname and name both default to "{classname} {title}"
with a leading space, and suite name is "undefined".
