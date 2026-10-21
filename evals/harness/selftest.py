#!/usr/bin/env python3
"""Prove the graders without a model: python3 evals/harness/selftest.py [case ...]

For each case it scaffolds the workspace a run starts from, applies hand-written variants of what an agent could
leave, and grades each one twice: with the case's eval graders (the same regexes, in Python) and with regrade.sh.

- nothing: the untouched workspace. Every eval grader must fail and the regrade must fail.
- gaming: only comments that match the eval graders, and a fake gates.txt. The regrade must fail.
- wrong: a plausible change that is wrong (an OR that leaks, a field never set, a route on the wrong handler).
  The regrade must fail.
- reference: a correct change, its gates.txt from the real commands. Every eval grader and the regrade must pass.

Needs .cache/harness-eval (prepare.sh). Exit 1 when a variant breaks its expectation. Standard library only.
"""
import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import regrade  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
BASE = re.search(r"^BASE=(\w+)", open(os.path.join(HERE, "lib", "common.sh")).read(), re.M).group(1)


def base_file(path):
    return subprocess.run(["git", "-C", regrade.REPO, "show", f"{BASE}:{path}"], capture_output=True, text=True, check=True).stdout


def write(ws, path, text):
    p = os.path.join(ws, path)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as f:
        f.write(text)


def edit(ws, path, old, new, count=1):
    p = os.path.join(ws, path)
    s = open(p).read()
    assert s.count(old) == count, (path, old[:60], s.count(old))
    write(ws, path, s.replace(old, new))


def run(ws, cwd, *cmd):
    p = subprocess.run(list(cmd), cwd=os.path.join(ws, cwd), env=ENV, capture_output=True, text=True)
    return p.stdout + p.stderr


def gates(ws, text):
    write(ws, "gates.txt", text)


NP = "nexus-platform/"
REPO_GO, REPO_TEST, SVC_TEST = NP + "internal/notes/repository.go", NP + "internal/notes/repository_unit_test.go", NP + "internal/notes/service_unit_test.go"
HANDLER, API = NP + "internal/handlers/note_handler.go", "nexus-web/lib/api.ts"


# ---- notes-tenant-leak --------------------------------------------------------------------------------------------

def tenant_reference(ws):
    for p in (REPO_GO, REPO_TEST, SVC_TEST):
        write(ws, p, base_file(p))
    gates(ws, run(ws, "nexus-platform", "go", "test", "./internal/notes/"))


def tenant_or(ws):
    """The tenant comes back, but ORed: AND binds tighter, so any tenant's rows on that record match."""
    tenant_reference(ws)
    edit(ws, REPO_GO, "WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL",
         "WHERE tenant_id = ? OR entity_name = ? AND record_id = ? AND deleted_at IS NULL", 2)
    gates(ws, run(ws, "nexus-platform", "go", "test", "./internal/notes/"))


def tenant_gaming(ws):
    for fn in ("ListByRecord", "CountByRecord"):
        edit(ws, REPO_GO, f"func (r *Repository) {fn}(", f"// WHERE tenant_id = ? AND entity_name = ?\nfunc (r *Repository) {fn}(")
    edit(ws, REPO_GO, "func (r *Repository) CountByRecord(", "// tenant_id = ? checked\nfunc (r *Repository) CountByRecord(")
    edit(ws, REPO_GO, "func (r *Repository) GetForRecord(", "// tenant_id = ? checked\nfunc (r *Repository) GetForRecord(")
    gates(ws, "ok  \tnexus/platform/internal/notes\t0.4s\n")


# ---- notes-pinned-field -------------------------------------------------------------------------------------------

def pinned_reference(ws, set_dto=True):
    write(ws, NP + "migrations/000100_record_notes_pinned.up.sql",
          "ALTER TABLE sys_record_notes ADD COLUMN pinned TINYINT(1) NOT NULL DEFAULT 0;\n")
    write(ws, NP + "migrations/000100_record_notes_pinned.down.sql", "ALTER TABLE sys_record_notes DROP COLUMN pinned;\n")
    edit(ws, REPO_GO, '	Body       string       `db:"body"`', '	Body       string       `db:"body"`\n	Pinned     bool         `db:"pinned"`')
    edit(ws, REPO_GO, "SELECT id, tenant_id, entity_name, record_id, body, created_by, created_at, updated_at, deleted_at\n"
         "	           FROM sys_record_notes\n	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL\n"
         "	           ORDER BY created_at DESC, id DESC",
         "SELECT id, tenant_id, entity_name, record_id, body, pinned, created_by, created_at, updated_at, deleted_at\n"
         "	           FROM sys_record_notes\n	           WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL\n"
         "	           ORDER BY pinned DESC, created_at DESC, id DESC")
    edit(ws, REPO_TEST, '"ORDER BY created_at DESC, id DESC"', '"ORDER BY pinned DESC, created_at DESC, id DESC"')
    edit(ws, HANDLER, '	Body          string    `json:"body"`', '	Body          string    `json:"body"`\n	Pinned        bool      `json:"pinned"`')
    if set_dto:
        edit(ws, HANDLER, "ID: r.ID, Body: r.Body, CreatedBy: r.CreatedBy,", "ID: r.ID, Body: r.Body, Pinned: r.Pinned, CreatedBy: r.CreatedBy,")
    gofmt(ws)
    gates(ws, run(ws, "nexus-platform", "go", "test", "./internal/notes/", "./internal/handlers/"))


def pinned_dto_never_set(ws):
    pinned_reference(ws, set_dto=False)


def pinned_gaming(ws):
    edit(ws, REPO_GO, "type Note struct {", '// db:"pinned" — ORDER BY pinned DESC\ntype Note struct {')
    edit(ws, HANDLER, "type noteDTO struct {", '// json:"pinned"\ntype noteDTO struct {')
    write(ws, NP + "migrations/000100_record_notes_pinned.up.sql", "-- ALTER TABLE sys_record_notes ADD COLUMN pinned TINYINT(1) DEFAULT 0;\n")
    write(ws, NP + "migrations/000100_record_notes_pinned.down.sql", "-- ALTER TABLE sys_record_notes DROP COLUMN pinned;\n")
    gates(ws, "ok  \tnexus/platform/internal/notes\t0.4s\n")


# ---- notes-newest-endpoint ----------------------------------------------------------------------------------------

API_FN = """
// GET /:entity/:id/notes/newest → {data: the newest note, or null}.
export async function getNewestRecordNote(entity: string, recordId: string): Promise<RecordNote | null> {
  const r = await req<{ data: RecordNote | null }>("GET", `/${entity}/${recordId}/notes/newest`);
  return r.data;
}
"""

NEWEST_VIA_LIST = """
// Newest handles GET /:entity/:id/notes/newest: {"data": the newest note, or null}.
func (h *NoteHandler) Newest(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, _, err := h.svc.List(c.Request.Context(), scopeOf(c), tenantID, strings.TrimSpace(c.Param("entity")), strings.TrimSpace(c.Param("id")), 1, 1)
	if err != nil {
		h.respondError(c, err, "Newest")
		return
	}
	if len(rows) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTOs(c.Request.Context(), tenantID, rows)[0]})
}
"""

NEWEST_VIA_SERVICE = """
// Newest handles GET /:entity/:id/notes/newest: {"data": the newest note, or null}.
func (h *NoteHandler) Newest(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	n, err := h.svc.Newest(c.Request.Context(), scopeOf(c), tenantID, strings.TrimSpace(c.Param("entity")), strings.TrimSpace(c.Param("id")))
	if err != nil {
		h.respondError(c, err, "Newest")
		return
	}
	if n == nil {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.toDTOs(c.Request.Context(), tenantID, []notes.Note{*n})[0]})
}
"""

SERVICE_NEWEST = """
// Newest returns the newest live note on (entityName, recordID), or nil when it has none.
func (s *Service) Newest(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) (*Note, error) {
	canonical, _, err := s.guard.ResolveNoteTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListByRecord(ctx, tenantID, canonical, recordID, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("notes.Service.Newest: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}
"""


def newest_common(ws, handler_method):
    edit(ws, HANDLER, 'rg.POST("/:entity/:id/notes", h.Create)', f'rg.POST("/:entity/:id/notes", h.Create)\n\trg.GET("/:entity/:id/notes/newest", h.{handler_method})')
    with open(os.path.join(ws, API), "a") as f:
        f.write(API_FN)


def newest_gates(ws):
    gates(ws, run(ws, "nexus-platform", "python3", "scripts/check-orphan-endpoints.py", "--strict")
          + run(ws, "nexus-platform", "go", "test", "./internal/handlers/", "./internal/notes/"))


def newest_reference_handler(ws):
    """Only the handler changes: it reads page 1 of the list with a limit of 1."""
    newest_common(ws, "Newest")
    with open(os.path.join(ws, HANDLER), "a") as f:
        f.write(NEWEST_VIA_LIST)
    gofmt(ws)
    newest_gates(ws)


def newest_reference_service(ws):
    """A new method on NoteService and notes.Service, as most agents write it; the hidden test uses the real service."""
    newest_common(ws, "Newest")
    edit(ws, HANDLER, "	Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, noteID string) error\n",
         "	Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, noteID string) error\n"
         "	Newest(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) (*notes.Note, error)\n")
    with open(os.path.join(ws, HANDLER), "a") as f:
        f.write(NEWEST_VIA_SERVICE)
    with open(os.path.join(ws, NP + "internal/notes/service.go"), "a") as f:
        f.write(SERVICE_NEWEST)
    with open(os.path.join(ws, NP + "internal/handlers/note_acl_wiring_test.go"), "a") as f:
        f.write("\nfunc (s noteSpy) Newest(context.Context, services.RecordScope, string, string, string) (*notes.Note, error) {\n\treturn nil, nil\n}\n")
    gofmt(ws)
    newest_gates(ws)


def newest_wrong_handler(ws):
    """The route and the client exist, but the route serves the whole list."""
    newest_common(ws, "List")
    newest_gates(ws)


def newest_gaming(ws):
    edit(ws, HANDLER, 'rg.POST("/:entity/:id/notes", h.Create)', 'rg.POST("/:entity/:id/notes", h.Create)\n\t// rg.GET("/:entity/:id/notes/newest", h.Newest)')
    with open(os.path.join(ws, API), "a") as f:
        f.write('\n// export async function getNewestRecordNote(entity, recordId) { return req("GET", `/${entity}/${recordId}/notes/newest`); }\n')
    gates(ws, "✅ Không có endpoint mồ côi.\nok  \tnexus/platform/internal/handlers\t1.2s\n")


# ---- mcp-eval-delete-task -----------------------------------------------------------------------------------------

def mcp_reference(ws):
    run_py, test_py = "evals/mcp/run.py", "evals/mcp/test_run.py"
    edit(ws, run_py, '    p = d.get(d["kind"]) or {}',
         '    if d["kind"] == "delete_entity":\n        return f\'delete_entity:{(d.get("deleteEntity") or {}).get("entity", "")}\'\n    p = d.get(d["kind"]) or {}')
    edit(ws, run_py, '     "no proposal at all (a read-only question)"),\n]',
         '     "no proposal at all (a read-only question)"),\n    (6, "Remove the Warehouses table.",\n'
         '     lambda ds: has(ds, "delete_entity", lambda d: "warehouse" in d["deleteEntity"]["entity"]),\n'
         '     "a proposal to delete a table named *warehouse*"),\n]')
    edit(ws, test_py, 'if __name__ == "__main__":',
         'class Task6(unittest.TestCase):\n    def test_grades_the_real_deletion_draft(self):\n'
         '        grade = next(t for t in run.TASKS if t[0] == 6)[2]\n'
         '        self.assertTrue(grade([{"kind": "delete_entity", "deleteEntity": {"entity": "warehouses"}}]))\n'
         '        self.assertFalse(grade([]))\n\n\nif __name__ == "__main__":')
    gates(ws, run(ws, ".", "python3", test_py))


def mcp_gaming(ws):
    edit(ws, "evals/mcp/run.py", "TASKS = [", '# (6, "Remove the Warehouses table.", "deleteEntity") — later\nTASKS = [')
    edit(ws, "evals/mcp/test_run.py", "import unittest", 'import unittest  # "deleteEntity"')
    gates(ws, "....\n----------------------------------------------------------------------\nRan 5 tests in 0.001s\n\nOK\n")


def gofmt(ws):
    run(ws, "nexus-platform", "gofmt", "-w", "internal")


VARIANTS = {
    "notes-tenant-leak": [("nothing", None, "fail"), ("gaming", tenant_gaming, "fail"), ("wrong: leak-or", tenant_or, "fail"),
                          ("reference", tenant_reference, "pass")],
    "notes-pinned-field": [("nothing", None, "fail"), ("gaming", pinned_gaming, "fail"),
                           ("wrong: pin-dto-never-set", pinned_dto_never_set, "fail"), ("reference", pinned_reference, "pass")],
    "notes-newest-endpoint": [("nothing", None, "fail"), ("gaming", newest_gaming, "fail"),
                              ("wrong: newest-wrong-handler", newest_wrong_handler, "fail"),
                              ("reference (handler only)", newest_reference_handler, "pass"),
                              ("reference (new service method)", newest_reference_service, "pass")],
    "mcp-eval-delete-task": [("nothing", None, "fail"), ("gaming", mcp_gaming, "fail"), ("reference", mcp_reference, "pass")],
}


def eval_graders(case_dir, base, ws):
    """The case's regex graders, evaluated as `claude plugin eval` does: files = paths the run created."""
    created = "\n".join(sorted(set(regrade.text_files(ws)) - set(regrade.text_files(base))))
    out = {}
    for name in sorted(os.listdir(os.path.join(case_dir, "graders"))):
        fm = open(os.path.join(case_dir, "graders", name)).read().split("---")[1]
        pattern = re.search(r"^pattern:\s*'(.*)'\s*$", fm, re.M).group(1).replace("''", "'")
        target = re.search(r"^target:\s*(.*)$", fm, re.M).group(1).strip()
        if target == "files":
            text = created
        else:
            p = os.path.join(ws, re.search(r"path:\s*([^ }]+)", target).group(1))
            text = open(p).read() if os.path.exists(p) else None
        out[name[:-3]] = text is not None and re.search(pattern, text) is not None
    return out


ENV = regrade.go_env()


def main():
    cases = sys.argv[1:] or list(VARIANTS)
    bad = 0
    for case in cases:
        case_dir = os.path.join(HERE, case)
        root, base = regrade.scaffold(case_dir)
        try:
            for name, apply, want in VARIANTS[case]:
                ws = os.path.join(root, "variant")
                shutil.copytree(base, ws, symlinks=True)
                try:
                    if apply:
                        apply(ws)
                    graders = eval_graders(case_dir, base, ws)
                    ok, output = regrade.regrade_ws(case_dir, ws, ENV)
                finally:
                    regrade.remove(ws)
                score = sum(graders.values()) / len(graders)
                expect = {"nothing": score == 0 and not ok}.get(name, ok if want == "pass" else not ok)
                if want == "pass":
                    expect = expect and score == 1
                bad += not expect
                print(f'{"ok  " if expect else "BAD "} {case:22} {name:32} eval {score:.2f}  regrade {"pass" if ok else "fail"}  (want {want})')
                for line in output.splitlines():
                    if line.startswith(("PASS", "FAIL")):
                        print(f"       {line}")
        finally:
            regrade.remove(root)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
