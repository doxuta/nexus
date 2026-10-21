source "$(dirname "$0")/../lib/regrade.sh"
cd "$ws"
check "python3 evals/mcp/test_run.py" python3 evals/mcp/test_run.py
check "task 6 grades the real deletion draft (hidden)" python3 "$case_dir/hidden/check_task6.py" "$ws"
finish
