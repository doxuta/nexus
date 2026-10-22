"""Held out of the run (evals/harness/mcp-eval-delete-task/hidden); regrade.py runs it on the finished workspace:
python3 check_task6.py <workspace>. Task 6 must grade the draft the engine really stores for a deletion
(internal/ai/tools.go: DraftItem{Kind: "delete_entity", DeleteEntity: {entity, label, fieldCount}}, JSON key
"deleteEntity"), and the report must name the table."""
import sys

sys.path.insert(0, sys.argv[1] + "/evals/mcp")
import run  # noqa: E402

task = next((t for t in run.TASKS if t[0] == 6), None)
assert task is not None, "no task 6"
assert "warehouse" in task[1].lower(), task[1]
grade = task[2]


def delete(entity):
    return {"kind": "delete_entity", "deleteEntity": {"entity": entity, "label": entity, "fieldCount": 3}}


assert grade([delete("warehouses")]), "rejects the real deletion draft"
assert grade([delete("crm_contacts"), delete("warehouses")]), "rejects a deletion among others"
assert not grade([]), "passes with no proposal"
assert not grade([delete("crm_accounts")]), "passes a deletion of another table"
assert not grade([{"kind": "entity", "entity": {"name": "warehouses", "fields": []}}]), "passes a creation"
assert "warehouses" in run.label(delete("warehouses")), "the report does not name the table: " + run.label(delete("warehouses"))
print("task 6 grades the real deletion draft")
