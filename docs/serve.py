"""문서 열람용 읽기 전용 웹서버. 허용 목록의 .md 파일만 렌더링해서 보여준다. 사용: serve.py <bind-ip> <port>"""
import html, pathlib, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import markdown

BASE = pathlib.Path(__file__).resolve().parent.parent.parent  # lab/code
DOCS = {  # URL 이름 -> 파일 (여기에 없는 파일은 절대 제공하지 않는다)
    "requirements": ("01 요구사항", BASE / "aigate/docs/01-REQUIREMENTS.md"),
    "design-gate": ("02 검증 게이트 설계서", BASE / "aigate/docs/02-DESIGN-gate.md"),
    "plan-gate": ("03 검증 게이트 구현 계획", BASE / "aigate/docs/03-PLAN-gate.md"),
    "results-gate": ("04 검증 게이트 결과", BASE / "aigate/docs/04-RESULTS-gate.md"),
    "req-orchestrator": ("06 ② 오케스트레이터 요구사항", BASE / "aigate/docs/06-REQUIREMENTS-orchestrator.md"),
    "design-orchestrator": ("07 ② 오케스트레이터 설계서", BASE / "aigate/docs/07-DESIGN-orchestrator.md"),
    "plan-orchestrator": ("08 ② 오케스트레이터 구현 계획", BASE / "aigate/docs/08-PLAN-orchestrator.md"),
    "master-plan": ("00 전체 계획서", BASE.parent.parent / "전체_계획서.md"),
    "build-ledger": ("05 구축 기록과 판정", BASE / "aigate/docs/05-BUILD-LEDGER.md"),
    "harness-results": ("verify-harness 평가 결과", BASE / "verify-harness/RESULTS.md"),
    "harness-spec": ("verify-harness 대상 스펙", BASE / "verify-harness/spec.md"),
}
CSS = "body{font:16px/1.65 -apple-system,'Apple SD Gothic Neo',sans-serif;max-width:920px;margin:0 auto;padding:16px;color:#1d1d1f;background:#fff}@media(prefers-color-scheme:dark){body{background:#1c1c1e;color:#e5e5e7}a{color:#6cb2ff}th,td{border-color:#444!important}code,pre{background:#2c2c2e!important}}table{border-collapse:collapse;display:block;overflow-x:auto}th,td{border:1px solid #ccc;padding:6px 10px}pre{overflow-x:auto;padding:12px;background:#f4f4f5;border-radius:6px}code{background:#f4f4f5;padding:1px 4px;border-radius:4px}nav a{margin-right:12px}"


def page(title, body):
    nav = '<nav><a href="/">목록</a>' + "".join(f'<a href="/{k}">{html.escape(v[0])}</a>' for k, v in DOCS.items()) + "</nav><hr>"
    return f'<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{html.escape(title)}</title><style>{CSS}</style>{nav}{body}'.encode()


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        key = self.path.strip("/").split("?")[0]
        if key == "":
            body = "<h1>문서 목록</h1><ul>" + "".join(f'<li><a href="/{k}">{html.escape(v[0])}</a></li>' for k, v in DOCS.items()) + "</ul>"
            return self.send(200, page("문서 목록", body))
        if key in DOCS and DOCS[key][1].exists():  # 파일은 요청 시점에 다시 읽는다(수정 즉시 반영)
            text = DOCS[key][1].read_text(encoding="utf-8")
            return self.send(200, page(DOCS[key][0], markdown.markdown(text, extensions=["tables", "fenced_code"])))
        self.send(404, page("404", "<h1>404</h1>"))

    def send(self, code, data):
        self.send_response(code); self.send_header("Content-Type", "text/html; charset=utf-8"); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)

    def log_message(self, *a): pass


ThreadingHTTPServer((sys.argv[1], int(sys.argv[2])), H).serve_forever()
