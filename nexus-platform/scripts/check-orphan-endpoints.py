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
  • A caller is non-comment code that spells the whole route (#54): comments are stripped first, and the path
    must start a string or follow a `${…}`, then every segment in order — a leading `:param` as a `${…}`
    placeholder, a later one as a placeholder or one literal segment. Any string counts, not only an argument
    of req()/fetch() — a page href equal to an API path still passes. nexus-web/test/ (fetch mocks) is skipped.
  • A route built in pieces is followed only through a path helper of the same file whose value is one literal
    (`const p = (id) => `/admin/x/${id}`` then `${p(id)}/y`); other compositions read as orphans.

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
# Keyed by (method, path): an excuse covers that verb only, so a later PUT on an excused GET path is still caught.
ALLOWED_ORPHANS = {
    ("GET", "/health"): "liveness probe hạ tầng, không phải bề mặt sản phẩm",
    ("GET", "/ready"): "readiness probe hạ tầng",
    ("GET", "/metrics"): "Prometheus scrape, không phải FE",
    ("GET", "/auth/sso/callback"): "IdP gọi trực tiếp qua trình duyệt, không phải fetch từ FE",
    ("GET", "/auth/sso/start"): "điều hướng trình duyệt (href), không phải fetch",
    ("GET", "/admin/team-sets/:id"): "no single-set screen: TeamSetsPanel lists every set with its teams (#54)",
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


# A `/` right after one of these (or after `return`/`typeof`/`case`) opens a regex literal, not a division.
_REGEX_AFTER = set("(,=:[!&|?{;+-*%~^")


def _regex_can_start(out):
    tail = "".join(out[-16:]).rstrip()
    return not tail or tail[-1] in _REGEX_AFTER or re.search(r"\b(?:return|typeof|case)$", tail) is not None


def strip_comments(src):
    """TS/TSX source without `//` and `/* */` comments (JSX `{/* */}` included).

    String, template and regex literals are copied as they are, so `"https://…"` or `` `${a}//b` `` keep their `//`;
    `${…}` inside a template is code again, nested templates included. A quote in JSX text ("Don't") opens a
    string that ends at the newline, as a JS string would. Agrees with the TypeScript compiler's comment ranges on
    every non-test .ts/.tsx file of nexus-web (checked when written, #54).
    """
    out, i, n = [], 0, len(src)
    depth = []  # one brace counter per open `${`
    while i < n:
        c = src[i]
        if c == "`" or (c == "}" and depth and depth[-1] == 0):  # a template starts, or resumes after `${…}`
            if c == "}":
                depth.pop()
            j = i + 1
            while j < n and src[j] != "`" and not src.startswith("${", j):
                j += 2 if src[j] == "\\" else 1
            if src.startswith("${", j):
                depth.append(0)
                j += 1
            out.append(src[i:j + 1])
            i = j + 1
        elif c in "\"'":
            j = i + 1
            while j < n and src[j] not in (c, "\n"):
                j += 2 if src[j] == "\\" else 1
            out.append(src[i:j + 1])
            i = j + 1
        elif src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
        elif src.startswith("/*", i):
            j = src.find("*/", i + 2)
            i = n if j < 0 else j + 2
            out.append(" ")
        elif c == "/" and _regex_can_start(out):
            j, in_class = i + 1, False
            while j < n and src[j] != "\n":
                if src[j] == "\\":
                    j += 2
                    continue
                if src[j] == "[":
                    in_class = True
                elif src[j] == "]":
                    in_class = False
                elif src[j] == "/" and not in_class:
                    break
                j += 1
            out.append(src[i:j + 1])
            i = j + 1
        else:
            if depth and c in "{}":
                depth[-1] += 1 if c == "{" else -1
            out.append(c)
            i += 1
    return "".join(out)


# `const p = (id: string) => `/admin/x/${id}`` or `const p = "/admin/x"`: a path helper with one literal value.
_PATH_HELPER_RE = re.compile(r"""\bconst\s+(\w+)\s*=\s*(?:\([^)]*\)\s*(?::\s*\w+\s*)?=>\s*)?([`"'])(/[^`"'\n]*)\2""")


def inline_path_helpers(code):
    """`${p(id)}/y` → `/admin/x/${id}/y` for every path helper `p` defined in the same file."""
    for name, _quote, value in _PATH_HELPER_RE.findall(code):
        code = re.sub(r"\$\{" + name + r"(?:\([^()]*\))?\}", lambda _m, v=value: v, code)
    return code


def web_code():
    """Non-comment source of every non-test .ts/.tsx file in nexus-web, path helpers inlined."""
    chunks = []
    for dirpath, dirs, files in os.walk(WEB):
        # test/: vitest helpers whose fake fetch routes name API paths without calling them.
        dirs[:] = [d for d in dirs if d not in {"node_modules", ".next", "_archive", "test"}]
        for f in files:
            if not f.endswith((".ts", ".tsx")) or ".test." in f:
                continue
            with open(os.path.join(dirpath, f), encoding="utf-8", errors="ignore") as fh:
                chunks.append(inline_path_helpers(strip_comments(fh.read())))
    return "\n".join(chunks)


_PLACEHOLDER = r"\$\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}"  # ${id}, ${encodeURIComponent(id)}, ${q ? `?${q}` : ""}
_SEGMENT = r"[^/\s\"'`?#{}$]+"                        # one literal segment: /admin/apps/crm/nav


def route_pattern(path):
    """Regex for code that builds a request to `path`: the whole path, from its start to its end.

    `/:entity/:id/notes/latest` needs `/${…}/${…}/notes/latest` — not `/latest` alone (#54). The path starts where
    a string or template opens or right after a `${…}` (`${BASE}/…`), never inside a longer path. A `:param` before
    the first static segment is a `${…}` placeholder only — as a literal segment `/:entity/:id/members` would read
    `/admin/teams/${id}/members` as its caller (#61 verifier); a later `:param` may also be one literal segment
    (`/admin/apps/crm/nav`). After the last segment comes no further path: `?query`, a closing quote or `${qs}`
    may follow, `/more` may not.
    """
    body, static_seen = "", False
    for seg in (s for s in path.split("/") if s):
        is_param = seg.startswith(":")
        static_seen = static_seen or not is_param
        param = f"(?:{_PLACEHOLDER}|{_SEGMENT})" if static_seen else _PLACEHOLDER
        body += "/" + (param if is_param else re.escape(seg))
    return re.compile(r"(?<=[\"'`}])" + body + r"(?![\w./-])")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--strict", action="store_true",
                    help="exit 1 khi có endpoint mồ côi ngoài danh sách miễn trừ")
    a = ap.parse_args()

    if not os.path.isdir(WEB):
        print(f"LỖI MÔI TRƯỜNG: không thấy {WEB}", file=sys.stderr)
        return 2

    code = web_code()
    routes = sorted(go_routes(), key=lambda r: (r[1], r[0]))
    orphans, excused = [], []
    for method, path in routes:
        # Lý do ghi tay thắng dò chuỗi: một route đã khai là nợ FE luôn hiện ở "Miễn trừ", kể cả khi
        # tiền tố tĩnh của nó trùng một route anh em có caller (cổng mù ⇒ nợ sẽ xanh im lặng).
        if (method, path) in ALLOWED_ORPHANS:
            excused.append((method, path))
            continue
        # Path only, not method: see the method-blind limit in the docstring.
        if not route_pattern(path).search(code):
            orphans.append((method, path))

    print(f"Route đã đăng ký: {len(routes)} · có FE gọi: {len(routes) - len(orphans) - len(excused)}"
          f" · miễn trừ: {len(excused)} · MỒ CÔI: {len(orphans)}")
    if excused:
        print("\nMiễn trừ (có lý do):")
        for m, p in excused:
            print(f"  {m:6} {p:44} — {ALLOWED_ORPHANS[(m, p)]}")
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
