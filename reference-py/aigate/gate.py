"""AI 구현 결과의 결정적 검증 게이트. 모델 출력은 읽지 않고, 기준 커밋(base)과 대상 커밋(head)을 직접 실행해 비교한다.

판정 불변식: base에서 통과하던 모든 테스트는 head에서도 같은 id로 실행되어 통과해야 한다.
 - head에서 실패 → behavior_failure
 - head에서 사라짐/skip/xfail 처리 → check_weakened
 - 실행 자체가 불완전(타임아웃, 수집·설정 오류, 미커밋 변경, base 불안정) → verification_incomplete
보호 대상 파일(테스트·설정)에서 기존 줄이 지워지거나 바뀌면 사람의 승인(diff 해시)이 없는 한 check_weakened.
우선순위: incomplete > weakened > behavior. 모든 위반을 보고서에 남긴다.
"""
import datetime, hashlib, json, os, shlex, subprocess, sys, tempfile, time
import xml.etree.ElementTree as ET

PROTECTED = ["tests/**", "**/conftest.py", "conftest.py", "pytest.ini", "pyproject.toml", "setup.cfg", "tox.ini"]
EXIT = {"ok": 0, "behavior_failure": 1, "verification_incomplete": 2, "check_weakened": 3}


def git(repo, *args, check=True):
    return subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True, check=check).stdout


def run_suite(path, cmd, timeout):
    """path에서 고정된 명령으로 전체 스위트를 실행한다. 저장소 설정의 addopts는 무력화해 선택 범위를 줄이지 못하게 한다."""
    xml = tempfile.NamedTemporaryFile(suffix=".xml", delete=False).name
    argv = cmd + ["-o", "addopts=", "-p", "no:cacheprovider", "--junitxml", xml]
    t = time.time(); timed_out = False
    try:
        p = subprocess.run(argv, cwd=path, capture_output=True, text=True, timeout=timeout)
        rc, tail = p.returncode, (p.stdout + p.stderr)[-1500:]
    except subprocess.TimeoutExpired as e:
        rc, timed_out, tail = 124, True, ((e.stdout or b"").decode() if isinstance(e.stdout, bytes) else (e.stdout or ""))[-1500:]
    statuses = {}
    try:
        if not timed_out:
            root = ET.parse(xml).getroot()
            for tc in root.iter("testcase"):
                tid = f'{tc.get("classname")}::{tc.get("name")}'
                st = "passed"
                if tc.find("failure") is not None: st = "failed"
                elif tc.find("error") is not None: st = "error"
                elif tc.find("skipped") is not None: st = "skipped"
                statuses[tid] = st
    except Exception:
        pass
    finally:
        if os.path.exists(xml): os.remove(xml)
    return {"rc": rc, "timed_out": timed_out, "statuses": statuses, "seconds": round(time.time() - t, 2), "tail": tail}


class Checkout:
    """커밋을 격리된 임시 worktree로 꺼낸다(작업 트리 상태가 증거에 섞이지 않게)."""
    def __init__(self, repo, ref): self.repo, self.ref = repo, ref
    def __enter__(self):
        self.dir = tempfile.mkdtemp(prefix="aigate-")
        git(self.repo, "worktree", "add", "--detach", self.dir, self.ref)
        return self.dir
    def __exit__(self, *a):
        git(self.repo, "worktree", "remove", "--force", self.dir, check=False)


def protected_diff(repo, base, head, globs):
    spec = [f":(glob){g}" for g in globs]
    text = git(repo, "diff", "--no-renames", "-U0", base, head, "--", *spec)
    stat = git(repo, "diff", "--no-renames", "--numstat", base, head, "--", *spec)
    removed = [l[1:] for l in text.splitlines() if l.startswith("-") and not l.startswith("---")]
    touched = [l.split("\t")[2] for l in stat.splitlines() if l.strip()]
    modified = [l.split("\t")[2] for l in stat.splitlines() if l.strip() and l.split("\t")[1] not in ("0", "-")]
    return {"hash": hashlib.sha256(text.encode()).hexdigest()[:16], "removed_lines": removed, "touched_files": touched, "files_with_removals": modified}


def counts(statuses):
    c = {"passed": 0, "failed": 0, "error": 0, "skipped": 0}
    for s in statuses.values(): c[s] += 1
    return c


def run_gate(repo, base, head="HEAD", cmd="python -m pytest -q", timeout=300, protected=None, approve=None):
    repo = os.path.abspath(repo); cmd_l = shlex.split(cmd).copy()
    if cmd_l[:1] == ["python"]: cmd_l[0] = sys.executable
    head_sha, base_sha = git(repo, "rev-parse", head).strip(), git(repo, "rev-parse", base).strip()
    v = []  # (category, rule, detail)
    dirty = git(repo, "status", "--porcelain").strip()
    if dirty: v.append(("verification_incomplete", "dirty_tree", "커밋되지 않은 변경이 있다(증거는 커밋 기준이라 이 변경은 검증되지 않음): " + "; ".join(dirty.splitlines()[:5])))
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    with Checkout(repo, base_sha) as d: rb = run_suite(d, cmd_l, timeout)
    with Checkout(repo, head_sha) as d: rh = run_suite(d, cmd_l, timeout)
    pd = protected_diff(repo, base_sha, head_sha, protected or PROTECTED)

    base_pass = {t for t, s in rb["statuses"].items() if s == "passed"}
    if rb["timed_out"] or not rb["statuses"] or any(s == "error" for s in rb["statuses"].values()):
        v.append(("verification_incomplete", "base_unreliable", f"기준 커밋 실행이 불안정하다(rc={rb['rc']}, 타임아웃={rb['timed_out']}, 수집={len(rb['statuses'])})"))
    elif rb["rc"] != 0:
        v.append(("verification_incomplete", "base_not_green", f"기준 커밋이 이미 실패한다(rc={rb['rc']}) — 비교 기준으로 쓸 수 없음"))
    if rh["timed_out"]: v.append(("verification_incomplete", "timeout", f"대상 실행이 {timeout}초 안에 끝나지 않았다"))
    elif not rh["statuses"]: v.append(("verification_incomplete", "no_tests_ran", f"실행된 테스트가 없다(rc={rh['rc']}): " + rh["tail"][-200:].replace("\n", " ")))
    elif rh["rc"] in (2, 4, 5): v.append(("verification_incomplete", "run_error", f"pytest 종료 코드 {rh['rc']}"))
    errs = sorted(t for t, s in rh["statuses"].items() if s == "error")
    if errs: v.append(("verification_incomplete", "setup_or_collection_error", f"오류 {len(errs)}건: {errs[:3]}"))

    if rh["statuses"] and not rh["timed_out"]:
        for t in sorted(base_pass):
            s = rh["statuses"].get(t)
            if s == "passed": continue
            if s == "failed": v.append(("behavior_failure", "regression", f"기준에서 통과하던 {t} 가 실패한다"))
            elif s is None: v.append(("check_weakened", "test_missing", f"기준에서 통과하던 {t} 가 실행되지 않는다(삭제·이름 변경·deselect)"))
            elif s == "skipped": v.append(("check_weakened", "test_skipped", f"기준에서 통과하던 {t} 가 skip/xfail 처리됐다"))
        for t, s in sorted(rh["statuses"].items()):
            if t not in base_pass and s == "failed": v.append(("behavior_failure", "new_test_fails", f"새 테스트 {t} 가 실패한다"))
            if t not in rb["statuses"] and s == "skipped": v.append(("check_weakened", "new_test_skipped", f"새 테스트 {t} 가 skip 상태다"))
    approved = bool(approve) and approve == pd["hash"]
    if pd["removed_lines"] or any(rule == "test_missing" for _, rule, _ in v):
        if not approved:
            v.append(("check_weakened", "protected_modified", f"보호 대상 파일의 기존 줄이 {len(pd['removed_lines'])}개 지워지거나 바뀌었다({pd['files_with_removals']}). 사람이 확인했다면 --approve {pd['hash']}"))
    cats = {c for c, _, _ in v}
    category = next((c for c in ("verification_incomplete", "check_weakened", "behavior_failure") if c in cats), "ok")
    return {"category": category, "exit_code": EXIT[category], "started_at": started,
            "evidence": {"head": head_sha, "base": base_sha, "command": cmd, "dirty": bool(dirty), "approved_protected_diff": approved,
                         "head_run": {"rc": rh["rc"], "seconds": rh["seconds"], "timed_out": rh["timed_out"], "counts": counts(rh["statuses"])},
                         "base_run": {"rc": rb["rc"], "seconds": rb["seconds"], "counts": counts(rb["statuses"])},
                         "protected_diff": {k: pd[k] for k in ("hash", "touched_files", "files_with_removals")}},
            "violations": [{"category": c, "rule": r, "detail": d} for c, r, d in v]}


def check_fresh(report, repo):
    """증거 보고서가 현재 저장소 상태에 대한 것인지 확인한다(오래된 증거 재사용 방지). 문제 목록을 반환."""
    head = git(repo, "rev-parse", "HEAD").strip(); issues = []
    if report["evidence"]["head"] != head: issues.append(f"증거는 {report['evidence']['head'][:8]} 기준인데 현재 HEAD는 {head[:8]}")
    if git(repo, "status", "--porcelain").strip(): issues.append("작업 트리에 커밋되지 않은 변경이 있다")
    if report["category"] != "ok": issues.append(f"증거 판정이 ok가 아니다({report['category']})")
    return issues


def to_markdown(r):
    e = r["evidence"]; h = e["head_run"]
    lines = [f"# 검증 보고서: {r['category']} (exit {r['exit_code']})", "",
             f"- head `{e['head'][:10]}` / base `{e['base'][:10]}` / 명령 `{e['command']}`",
             f"- 실행: 통과 {h['counts']['passed']}, 실패 {h['counts']['failed']}, 오류 {h['counts']['error']}, skip {h['counts']['skipped']} ({h['seconds']}s)",
             f"- 보호 대상 diff 해시 `{e['protected_diff']['hash']}` (승인됨: {e['approved_protected_diff']})", "", "## 위반"]
    lines += [f"- [{v['category']}] **{v['rule']}**: {v['detail']}" for v in r["violations"]] or ["- 없음"]
    return "\n".join(lines) + "\n"
