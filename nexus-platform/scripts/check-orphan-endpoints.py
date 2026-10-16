#!/usr/bin/env python3
"""Bắt "ĐOẠN NỐI KHÔNG AI VIẾT" — endpoint backend không frontend nào gọi.

Vì sao cần cổng này: chẩn đoán 2026-07-29 (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b) tìm thấy MỘT
mẫu lặp lại — backend đúng, frontend đúng, **đoạn nối ở giữa không ai viết** — và mọi cổng hiện
có vẫn xanh vì mỗi cổng chỉ kiểm MỘT ĐẦU:

  • cột `sys_notifications.type` thêm bằng cả một migration riêng, FE dựng 5 nhánh icon,
    nhưng 2 câu INSERT không liệt kê cột  →  mọi hàng NULL, không nhánh nào từng khớp
  • `GET /ai/health` viết ra ĐÚNG để tránh 503, rồi 0 caller  →  người dùng vẫn gõ xong mới ăn 503
  • `PATCH /admin/relationships/:id` tồn tại  →  tạo được, xoá được, SỬA thì không có đường
  • `POST /admin/entities` trả `aclSeed.warning`  →  FE không đọc, seed hỏng mà người dựng không biết

Script này bắt lớp con dễ đo nhất: **route đã đăng ký mà không file nào trong nexus-web gọi tới**.

⚠️ GIỚI HẠN ĐÃ BIẾT — nói thẳng, vì một cổng tự nhận phủ-hết mà thực ra mù chính là thứ tạo ra
"test xanh mà sản phẩm vẫn mù mờ":
  • MÙ THEO METHOD. So khớp theo ĐƯỜNG DẪN, nên `PATCH /admin/relationships/:id` KHÔNG bị bắt: FE
    có gọi `/admin/relationships` (tạo, xoá) nên đường dẫn coi như "có người dùng", dù động từ PATCH
    thì không ai gọi — quan hệ tạo được, xoá được, **SỬA thì không có đường vào**. Bản method-aware
    đã thử và cho 28 dòng nghi ngờ trong đó ~20 là nhiễu họ `/:entity/*`; một cổng ồn là cổng bị bỏ
    qua, nên giữ bản chính xác-mà-hẹp và ghi giới hạn ở đây.
  • MÙ VỚI KEY TRONG RESPONSE. `POST /admin/entities` trả `aclSeed.warning` — endpoint có caller nên
    sạch, nhưng KEY đó không FE nào đọc. Loại này phải soi tay.

Dùng:
    python3 scripts/check-orphan-endpoints.py            # báo cáo, exit 0
    python3 scripts/check-orphan-endpoints.py --strict   # exit 1 khi có endpoint mồ côi MỚI

`--strict` so với danh sách miễn trừ ALLOWED_ORPHANS bên dưới: một endpoint chỉ được phép mồ côi
khi có LÝ DO viết ra ở đây. Thêm route mới mà không có FE caller ⇒ hoặc nối dây, hoặc ghi lý do.

ponytail: stdlib thuần, không parse Go AST — regex trên lời gọi router là đủ và đọc được.
"""

import argparse
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WEB = os.path.join(os.path.dirname(ROOT), "nexus-web")

# Route registrations: rg.GET("/path", …) / rg.POST(…) / r.DELETE(…) …
ROUTE_RE = re.compile(r'\b\w+\.(GET|POST|PUT|PATCH|DELETE)\(\s*"([^"]+)"')

# Endpoint được phép không có FE caller — mỗi dòng PHẢI có lý do.
ALLOWED_ORPHANS = {
    "/health": "liveness probe hạ tầng, không phải bề mặt sản phẩm",
    "/ready": "readiness probe hạ tầng",
    "/metrics": "Prometheus scrape, không phải FE",
    "/auth/sso/callback": "IdP gọi trực tiếp qua trình duyệt, không phải fetch từ FE",
    "/auth/sso/start": "điều hướng trình duyệt (href), không phải fetch",
}


def go_routes():
    """Mọi (method, path) đăng ký trong internal/handlers."""
    out = set()
    for dirpath, _dirs, files in os.walk(os.path.join(ROOT, "internal", "handlers")):
        for f in files:
            if not f.endswith(".go") or f.endswith("_test.go"):
                continue
            with open(os.path.join(dirpath, f), encoding="utf-8") as fh:
                for m in ROUTE_RE.finditer(fh.read()):
                    out.add((m.group(1), m.group(2)))
    return out


# FE gọi API qua một helper dạng `req<T>("METHOD", `/path/${x}`, body)` (lib/api.ts, lib/admin.ts…).
# Bắt CẢ method LẪN path cho phép phát hiện "mồ côi theo method": path có người gọi nhưng đúng động
# từ đó thì không — chính là ca `PATCH /admin/relationships/:id` (tạo được, xoá được, SỬA thì không).
FE_CALL_RE = re.compile(r'"(GET|POST|PUT|PATCH|DELETE)"\s*,\s*[`"\']([^`"\']*)')


def web_calls():
    """Tập (method, tiền-tố-tĩnh-của-path) mà FE thực sự gọi + toàn văn FE để dò dự phòng."""
    calls, chunks = set(), []
    for dirpath, dirs, files in os.walk(WEB):
        dirs[:] = [d for d in dirs if d not in {"node_modules", ".next", "_archive"}]
        for f in files:
            if not f.endswith((".ts", ".tsx")) or ".test." in f:
                continue
            with open(os.path.join(dirpath, f), encoding="utf-8", errors="ignore") as fh:
                txt = fh.read()
            chunks.append(txt)
            for m in FE_CALL_RE.finditer(txt):
                calls.add((m.group(1), m.group(2).split("${")[0].rstrip("/")))
    return calls, "\n".join(chunks)


def probe(path):
    """Chuỗi FE cần chứa để coi là 'có gọi'.

    FE nối URL bằng template literal, nên chỉ phần TĨNH mới xuất hiện nguyên văn. Hai dạng:
      • "/admin/apps/:id/nav"  → tiền tố tĩnh "/admin/apps" (đủ đặc trưng)
      • "/:entity/:id/aggregate" → KHÔNG có tiền tố tĩnh (bắt đầu bằng tham số) → dùng đoạn tĩnh
        CUỐI ("aggregate"). Bỏ qua bước này là lý do bản đầu báo 3 false-positive: mọi route
        `/:entity/...` đều "mồ côi" vì chuỗi đem đi dò còn nguyên dấu ":".
    """
    static = path.split("/:")[0]
    if len(static) > 1:
        return static
    tail = [seg for seg in path.split("/") if seg and not seg.startswith(":")]
    # Giữ dấu "/" đầu: dò "/notes" chứ không phải "notes". FALSE NEGATIVE đã cắn thật (2026-07-29,
    # do chính agent viết lát ghi-chú phát hiện): 3 route notes gộp về chuỗi trần "notes", khớp nhầm
    # comment "Honesty notes" và một field tên `notes` trong fixture test ⇒ cổng báo "0 mồ côi"
    # trong khi có 3 mồ côi THẬT. Một cổng báo xanh sai còn tệ hơn không có cổng.
    return "/" + tail[-1] if tail else path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--strict", action="store_true",
                    help="exit 1 khi có endpoint mồ côi ngoài danh sách miễn trừ")
    a = ap.parse_args()

    if not os.path.isdir(WEB):
        print(f"LỖI MÔI TRƯỜNG: không thấy {WEB}", file=sys.stderr)
        return 2

    calls, web = web_calls()
    routes = sorted(go_routes(), key=lambda r: (r[1], r[0]))
    orphans, excused = [], []
    for method, path in routes:
        # Lý do ghi tay thắng dò chuỗi: một route đã khai là nợ FE luôn hiện ở "Miễn trừ", kể cả khi
        # tiền tố tĩnh của nó trùng một route anh em có caller (cổng mù ⇒ nợ sẽ xanh im lặng).
        if path in ALLOWED_ORPHANS:
            excused.append((method, path))
            continue
        pfx = probe(path)
        # Ưu tiên bằng chứng MẠNH: FE gọi đúng (method, path). Nếu không có, chấp nhận bằng chứng
        # YẾU (chuỗi path xuất hiện đâu đó) — có helper gói lại nên không phải chỗ nào cũng thấy
        # cặp method+path nguyên văn; nhưng nếu path KHÔNG xuất hiện ở đâu cả thì chắc chắn mồ côi.
        strong = any(m == method and (p.startswith(pfx) or pfx.startswith(p)) for m, p in calls if p)
        if strong:
            continue
        if pfx in web:
            continue  # đường dẫn có người gọi — xem GIỚI HẠN ở docstring về mồ-côi-theo-method
        orphans.append((method, path))

    print(f"Route đã đăng ký: {len(routes)} · có FE gọi: {len(routes) - len(orphans) - len(excused)}"
          f" · miễn trừ: {len(excused)} · MỒ CÔI: {len(orphans)}")
    if excused:
        print("\nMiễn trừ (có lý do):")
        for m, p in excused:
            print(f"  {m:6} {p:44} — {ALLOWED_ORPHANS[p]}")
    if orphans:
        print("\n❌ MỒ CÔI — backend có, không FE nào gọi:")
        for m, p in orphans:
            print(f"  {m:6} {p}")
        print("\nMỗi dòng trên là một 'đoạn nối không ai viết': năng lực đã trả tiền để xây,")
        print("người dùng không với tới được. Hoặc NỐI DÂY, hoặc ghi lý do vào ALLOWED_ORPHANS.")
        return 1 if a.strict else 0
    print("\n✅ Không có endpoint mồ côi.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
