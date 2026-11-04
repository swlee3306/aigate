import argparse, json, pathlib, sys
from .gate import run_gate, check_fresh, to_markdown

ap = argparse.ArgumentParser(prog="aigate")
sub = ap.add_subparsers(dest="mode", required=True)
r = sub.add_parser("run"); r.add_argument("--repo", default="."); r.add_argument("--base", required=True); r.add_argument("--head", default="HEAD")
r.add_argument("--cmd", default="python -m pytest -q"); r.add_argument("--timeout", type=int, default=300)
r.add_argument("--protected", action="append"); r.add_argument("--approve"); r.add_argument("--out", default=".aigate")
c = sub.add_parser("check"); c.add_argument("report"); c.add_argument("--repo", default=".")
a = ap.parse_args()
if a.mode == "run":
    rep = run_gate(a.repo, a.base, a.head, a.cmd, a.timeout, a.protected, a.approve)
    out = pathlib.Path(a.repo) / a.out; out.mkdir(exist_ok=True)
    (out / f"report-{rep['evidence']['head'][:10]}.json").write_text(json.dumps(rep, ensure_ascii=False, indent=1), encoding="utf-8")
    (out / f"report-{rep['evidence']['head'][:10]}.md").write_text(to_markdown(rep), encoding="utf-8")
    print(to_markdown(rep)); sys.exit(rep["exit_code"])
issues = check_fresh(json.load(open(a.report, encoding="utf-8")), a.repo)
print("\n".join(issues) or "fresh"); sys.exit(1 if issues else 0)
