# The planted bug: the notes list and count lose their tenant filter, and the unit tests that pinned it are
# loosened to match, so the package stays green and the leak is silent.
source "$(dirname "$0")/../lib/scaffold.sh"
python3 - "$ws/nexus-platform/internal/notes" <<'PY'
import sys
d = sys.argv[1]
def plant(name, *edits):
    p = f"{d}/{name}"
    s = open(p).read()
    for old, new, n in edits:
        assert s.count(old) == n, (name, old, s.count(old))
        s = s.replace(old, new)
    open(p, "w").write(s)
plant("repository.go",
      ("WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND deleted_at IS NULL",
       "WHERE entity_name = ? AND record_id = ? AND deleted_at IS NULL", 2),
      ("q, tenantID, entityName, recordID, limit, offset)", "q, entityName, recordID, limit, offset)", 1),
      ("q, tenantID, entityName, recordID); err != nil", "q, entityName, recordID); err != nil", 1))
plant("repository_unit_test.go",
      ('"tenant_id = ?", "entity_name = ?", "record_id = ?",\n', '"entity_name = ?", "record_id = ?",\n', 1),
      ('[]driver.Value{"tenant-a", "crm_accounts", "rec-1", int64(20), int64(0)}',
       '[]driver.Value{"crm_accounts", "rec-1", int64(20), int64(0)}', 1),
      ('"COUNT(*)", "tenant_id = ?", "entity_name = ?"', '"COUNT(*)", "entity_name = ?"', 1))
plant("service_unit_test.go",
      ("if len(call.args) < 4 || call.args[3] != int64(100) {", "if len(call.args) < 3 || call.args[2] != int64(100) {", 1),
      ("call.args[3], call.args)", "call.args[2], call.args)", 1))
PY
commit_base
