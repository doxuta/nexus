source "$(dirname "$0")/../lib/regrade.sh"
cd "$ws"
for f in test_run.py testdata/recorded-task-5.jsonl testdata/failed-connection.jsonl; do from_base "evals/mcp/$f"; done
check "python3 evals/mcp/test_run.py (BASE's tests on the agent's run.py)" python3 evals/mcp/test_run.py
check "task 6 grades the real deletion draft (hidden)" python3 "$case_dir/hidden/check_task6.py" "$ws"
finish
