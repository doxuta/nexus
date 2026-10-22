"""Unit checks for scripts/check-orphan-endpoints.py (#54).

Run: python3 scripts/test_check_orphan_endpoints.py   (from nexus-platform/)

The gate passed routes nothing in nexus-web calls: a comment naming the path counted as a caller, and a route that
starts with a parameter was probed by its last static segment only. Each case below builds a throwaway tree (one Go
file of routes, a few web files), runs the real script on it with --strict and reads the verdict.
"""

import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-orphan-endpoints.py")
_spec = importlib.util.spec_from_file_location("check_orphan_endpoints", _path)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


def run_gate(routes, web):
    """Exit code and stdout of `check-orphan-endpoints.py --strict` on a tree holding these routes and web files."""
    with tempfile.TemporaryDirectory() as tmp:
        scripts = os.path.join(tmp, "nexus-platform", "scripts")
        handlers = os.path.join(tmp, "nexus-platform", "internal", "handlers")
        os.makedirs(scripts)
        os.makedirs(handlers)
        shutil.copy(_path, scripts)
        with open(os.path.join(handlers, "routes.go"), "w", encoding="utf-8") as fh:
            fh.write("package handlers\n\nfunc register() {\n")
            fh.writelines(f'\trg.{m}("{p}", h.X)\n' for m, p in routes)
            fh.write("}\n")
        for name, src in web.items():
            dest = os.path.join(tmp, "nexus-web", name)
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            with open(dest, "w", encoding="utf-8") as fh:
                fh.write(src)
        r = subprocess.run([sys.executable, os.path.join(scripts, "check-orphan-endpoints.py"), "--strict"],
                           capture_output=True, text=True)
        return r.returncode, r.stdout


class Callers(unittest.TestCase):
    def assertOrphan(self, route, web):
        code, out = run_gate([route], web)
        self.assertEqual(code, 1, out)
        self.assertIn(f"{route[0]:6} {route[1]}", out)

    def assertCalled(self, route, web):
        code, out = run_gate([route], web)
        self.assertEqual(code, 0, out)
        self.assertIn("MỒ CÔI: 0", out)

    def test_a_comment_naming_the_route_is_not_a_caller(self):
        # #54 repro 1: one comment line was enough. Line, block and JSX comments all name it here.
        self.assertOrphan(("GET", "/:entity/:id/notes/newest"), {
            "lib/api.ts": "// GET /x/notes/newest\n/* GET /:entity/:id/notes/newest */\nexport const a = 1;\n",
            "components/Notes.tsx": "export const N = () => <div>{/* `/${e}/${id}/notes/newest` */}</div>;\n",
        })

    def test_a_route_is_probed_by_all_its_static_segments(self):
        # #54 repro 2: `/:entity/:id/notes/latest` was reduced to `/latest`, which code says elsewhere.
        self.assertOrphan(("GET", "/:entity/:id/notes/latest"), {
            "components/CalendarView.tsx": "// hold the earliest/latest event of the span\n"
                                           "export const LATEST = \"/latest\";\n"
                                           "export const notes = (e: string, id: string) => `/${e}/${id}/notes`;\n",
        })

    def test_a_static_prefix_alone_does_not_call_a_deeper_route(self):
        self.assertOrphan(("PUT", "/admin/apps/:id/nav"), {
            "lib/api.ts": 'export const list = () => req("GET", "/admin/apps");\n'
                          "export const one = (id: string) => req(\"GET\", `/admin/apps/${id}`);\n",
        })

    def test_a_template_literal_caller_is_found(self):
        self.assertCalled(("GET", "/:entity/:id/notes/latest"), {
            "lib/api.ts": "export const latest = (entity: string, id: string, n: number) =>\n"
                          "  req(\"GET\", `/${entity}/${encodeURIComponent(id)}/notes/latest?limit=${n}`);\n",
        })

    def test_param_segments_match_placeholders_or_literal_segments(self):
        self.assertCalled(("PUT", "/admin/apps/:id/nav"), {
            "lib/api.ts": "export const nav = (a: { id: string }) => req(\"PUT\", `/admin/apps/${a.id}/nav`, {});\n"})
        self.assertCalled(("PUT", "/admin/apps/:id/nav"), {
            "lib/api.ts": 'export const nav = () => req("PUT", "/admin/apps/crm/nav", {});\n'})
        self.assertCalled(("GET", "/:entity"), {
            "lib/api.ts": "export const list = (e: string, q: string) => req(\"GET\", `/${e}${q ? `?${q}` : \"\"}`);\n"})

    def test_a_route_of_params_only_needs_placeholders_and_its_own_end(self):
        # `/:entity/:id` has no static segment: a literal `/admin/apps` or a deeper `/${e}/${id}/notes` is no caller.
        self.assertOrphan(("GET", "/:entity/:id"), {
            "lib/api.ts": 'export const a = () => req("GET", "/admin/apps");\n'
                          "export const n = (e: string, id: string) => req(\"GET\", `/${e}/${id}/notes`);\n"})
        self.assertCalled(("GET", "/:entity/:id"), {
            "lib/api.ts": "export const one = (e: string, id: string) => req(\"GET\", `/${e}/${id}`);\n"})

    def test_double_slashes_inside_strings_are_not_comments(self):
        # The caller sits on the same line, after a URL in a string and a template; stripping at `//` would drop it.
        self.assertCalled(("GET", "/:entity/:id/notes/latest"), {
            "lib/api.ts": "const BASE = \"http://localhost:8080/api/v1\"; const u = `https://x.test//${BASE}`; "
                          "export const f = (e: string, id: string) => fetch(`${BASE}/${e}/${id}/notes/latest`);\n"})
        self.assertCalled(("GET", "/:entity/:id/notes/latest"), {
            "lib/api.ts": "const SEP = '//'; const RE = /[\"'`]\\/\\//; "
                          "export const f = (e: string, id: string) => req(\"GET\", `/${e}/${id}/notes/latest`);\n"})

    def test_a_path_helper_in_the_same_file_is_followed(self):
        # lib/api.ts builds backup routes as `${tenantBackupsPath(tenantId)}/${backupId}`.
        self.assertCalled(("GET", "/admin/tenants/:id/backups/:backupId/file"), {
            "lib/api.ts": "const backups = (tenantId: string) => `/admin/tenants/${encodeURIComponent(tenantId)}/backups`;\n"
                          "export const file = (t: string, b: string) => reqBlob(`${backups(t)}/${b}/file`);\n"})

    def test_an_allowed_orphan_is_excused_with_its_reason(self):
        code, out = run_gate([("GET", "/health")], {"lib/api.ts": "export const a = 1;\n"})
        self.assertEqual(code, 0, out)
        self.assertIn(gate.ALLOWED_ORPHANS["/health"], out)


class StripComments(unittest.TestCase):
    def test_comments_go_strings_and_templates_stay(self):
        src = ('const u = "https://a.test/x"; // c1\n'
               "const t = `//${a /* c2 */}/*`; /* c3 */\n"
               "const j = <div>{/* c4 */}</div>;\n")
        out = gate.strip_comments(src)
        for c in ("c1", "c2", "c3", "c4"):
            self.assertNotIn(c, out)
        self.assertIn('"https://a.test/x"', out)
        self.assertIn("`//${a  }/*`", out)

    def test_regex_literals_and_division(self):
        out = gate.strip_comments("const r = /\\/\\/[`'\"]/g; const d = a / b; // gone\nconst k = `kept`;\n")
        self.assertIn("/\\/\\/[`'\"]/g", out)
        self.assertNotIn("gone", out)
        self.assertIn("`kept`", out)


if __name__ == "__main__":
    unittest.main()
