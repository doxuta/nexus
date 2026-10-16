#!/bin/bash
# check-plan-sync.sh — two independent jobs (decoupled 2026-09-01, Spec Kit adoption):
#
#  1. PLAN SYNC — for EVERY specs/*/tasks.md and legacy docs/*-plan.md that
#     carries the progress format (header bars "M2 [✅⬜] 1/4" + table rows
#     "| ✅ | M2-T1 |"), verify the header matches the table. A legacy plan
#     absorbed into specs/NNN-*/tasks.md (the core engine plan moved to
#     specs/040 on 2026-09-30) keeps being checked there; a deleted file simply
#     drops out of the glob, so the gate is never orphaned by a doc deletion.
#     Files without the format (most Spec Kit tasks.md use "- [x]" lists) are
#     skipped.
#  2. BLUEPRINT — ARCHITECTURE.md must exist, must not still mark a milestone ⬜
#     that a plan reports complete (overstate), and §6 must not understate code
#     that exists on disk (GATE-01 sentinel check). Runs even with zero plans.
#
# Exit 1 on any mismatch. Run from nexus-platform/:  bash scripts/check-plan-sync.sh
set -euo pipefail
DOCS="$(cd "$(dirname "$0")/.." && pwd)/docs"
SPECS="$(cd "$(dirname "$0")/../.." && pwd)/specs"

python3 - "$DOCS" "$SPECS" <<'PY'
import glob, os, re, sys

docs_dir, specs_dir = sys.argv[1], sys.argv[2]
problems = []
summary = []

# ---- Job 1: per-plan header↔table sync (auto-discovered) ----
all_done = {}   # plan label -> {milestone: done}
all_total = {}  # plan label -> {milestone: total}
plan_paths = sorted(glob.glob(os.path.join(docs_dir, "*-plan.md"))) + \
    sorted(glob.glob(os.path.join(specs_dir, "*", "tasks.md")))
for plan_path in plan_paths:
    text = open(plan_path, encoding="utf-8").read()
    # Every spec's file is named tasks.md, so label those by their specs/ path, not basename.
    if plan_path.startswith(specs_dir + os.sep):
        name = "specs/" + os.path.relpath(plan_path, specs_dir)
    else:
        name = os.path.basename(plan_path)

    done, total = {}, {}
    for status, task in re.findall(r"^\|\s*([✅⬜])\s*\|\s*(M\d-T\d+)", text, re.MULTILINE):
        m = "M" + task[1]
        total[m] = total.get(m, 0) + 1
        if status == "✅":
            done[m] = done.get(m, 0) + 1
    if not total:
        continue  # plan without the progress format — not covered by this gate
    all_done[name], all_total[name] = done, total

    header = {}
    for m, count, tot in re.findall(r"^(M\d)\s*\[[^\]]*\]\s*(\d+)/(\d+)", text, re.MULTILINE):
        header[m] = (int(count), int(tot))

    for m in sorted(set(total) | set(header)):
        d, t = done.get(m, 0), total.get(m, 0)
        if m not in header:
            problems.append(f"{name} {m}: bảng có {d}/{t} nhưng KHÔNG có thanh tiến độ ở header")
            continue
        if header[m] != (d, t):
            problems.append(f"{name} {m}: header ghi {header[m][0]}/{header[m][1]} nhưng bảng đếm {d}/{t}  ← LỆCH")

    gt_done, gt_total = sum(done.values()), sum(total.values())
    mt = re.search(r"Tổng:\s*\*?\*?\s*(\d+)/(\d+)", text)
    if mt and (int(mt.group(1)), int(mt.group(2))) != (gt_done, gt_total):
        problems.append(f"{name} Tổng: header ghi {mt.group(1)}/{mt.group(2)} nhưng bảng đếm {gt_done}/{gt_total}  ← LỆCH")
    summary.append(f"{name} {gt_done}/{gt_total} (" +
                   "; ".join(f"{m} {done.get(m,0)}/{total.get(m,0)}" for m in sorted(total)) + ")")

# ---- Job 2: MASTER BLUEPRINT checks — unconditional, plan-independent ----
arch_path = os.path.join(docs_dir, "ARCHITECTURE.md")
if not os.path.exists(arch_path):
    problems.append("KHÔNG thấy docs/ARCHITECTURE.md để đối chiếu blueprint")
else:
    arch = open(arch_path, encoding="utf-8").read()

    # Overstate: a plan reports a milestone fully done but the blueprint still marks it ⬜.
    for name in sorted(all_total):
        done, total = all_done[name], all_total[name]
        for m in sorted(total):
            if total[m] > 0 and done.get(m, 0) == total[m] and re.search(rf"\b{m}\b\s*⬜", arch):
                problems.append(f"ARCHITECTURE.md còn ghi {m} ⬜ nhưng {name} đã xong {done[m]}/{total[m]}  ← BLUEPRINT LỆCH (sync §6/§7)")

    # GATE-01 understate: sentinel package exists on disk but the §6 EPIC cell is still pure ⬜.
    platform_root = os.path.dirname(docs_dir)
    epic_sentinels = {
        "4": "internal/workflow",                # Workflow Platform
        "5": "internal/bpm",                     # BPM Platform
        "6": "internal/report",                  # Reporting Platform
        "8": "internal/services/vertical_pack",  # Reference Verticals
    }
    for num, sub in sorted(epic_sentinels.items()):
        if not os.path.isdir(os.path.join(platform_root, sub)):
            continue  # not built yet → ⬜ is honest, skip
        row = re.search(rf"^\|\s*\*\*{num}\.\s[^|]*\|[^|]*\|([^|]*)\|", arch, re.MULTILINE)
        if not row:
            problems.append(f"§6: KHÔNG thấy dòng EPIC {num} trong ARCHITECTURE.md để đối chiếu")
            continue
        if "✅" not in row.group(1) and "🟡" not in row.group(1):
            problems.append(
                f"§6 EPIC {num}: code `{sub}/` tồn tại nhưng ô trạng thái vẫn ⬜ (understate)  ← DRIFT, cập nhật §6")

if problems:
    print("❌ PLAN LỆCH ĐỒNG BỘ — sửa header cho khớp bảng:")
    for p in problems:
        print("   •", p)
    sys.exit(1)
print("✅ Plan đồng bộ — " + ("; ".join(summary) if summary else "0 plan theo format (blueprint checks vẫn chạy)"))
PY
