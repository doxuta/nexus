#!/usr/bin/env python3
"""Nghiệm thu người-thứ-ba cho specs/049-report-grouping-pivot (báo cáo nhiều trục), theo từng lát.

Chạy (server trên DB bản sao, KHÔNG trên nexus_platform dùng chung):

    python3 scripts/acceptance-report-grid.py --base http://localhost:8167/api/v1 --section S2b

Chấm bằng tài khoản KHÔNG-admin `test@example.com` (vai crm_sales_rep: xem cơ hội CỦA MÌNH, tạo mức tất
cả). `admin@example.com` chỉ cấu hình báo cáo, tạo dữ liệu mốc và dọn dẹp — không bao giờ để chấm (admin bỏ
qua mọi ACL, nên "admin thấy chạy" đúng theo cấu tạo kể cả khi quyền hỏng).

Dữ liệu cô lập bằng MÃ LƯỢT CHẠY: mọi cơ hội tạo ra mang `QS-<8 hex>` trong tên và mọi báo cáo thử mang
bộ lọc `name contains <mã lượt>` — môi trường dev đã có cơ hội của rep và admin, không có bộ lọc thì
mọi khẳng định "2 trong 5" sai từ đầu. Dọn trong `finally` bằng token ADMIN (vai rep không có quyền
xoá cơ hội). `--keep` giữ dữ liệu (in mã lượt) cho lượt bấm thật trên màn; `--cleanup <mã lượt>` dọn sau.

Mục:
  S2b  Ma trận "giai đoạn × tháng chốt" (config.columnGroup): rep chỉ thấy 2 cơ hội của mình ở MỌI ô/tổng;
       tổng hàng == báo cáo Tổng hợp theo giai đoạn chạy bằng CÙNG token; tổng chung khác của admin;
       khoá columnGroup đi qua API quản trị; xuất CSV lưới ⇒ 200 text/csv (trước lát S4 là 422
       REPORT_EXPORT_GRID_UNSUPPORTED); Ma trận chưa có trục cột trả y hệt báo cáo Tổng hợp cùng cấu hình.
  S3   Tổng hợp "giai đoạn → tháng chốt" (config.subGroups): rep chỉ thấy nhánh chứa 4 cơ hội của mình, tổng phụ
       + tổng cộng đúng, tháng theo lịch; với CẢ 5 phép tính, tổng phụ cấp 1 == báo cáo Tổng hợp một cấp theo
       giai đoạn chạy bằng CÙNG token (cùng chuỗi thập phân; AVG là của dòng gốc, không phải của các tháng);
       admin thấy nhánh rep không thấy; subGroups rỗng lưu như không có khoá; xuất CSV cây ⇒ 200 text/csv.
  S4   CSV đúng lưới/cây đang thấy: rep tải export.csv của Ma trận "giai đoạn × tháng chốt" và của cây "giai đoạn
       → tháng chốt" — BOM, tiêu đề trục là nhãn trường, cột/hàng Tổng, dòng "Tổng <giai đoạn>" + "Tổng cộng";
       TỪNG ô/tổng/tổng phụ == /run chạy bằng CÙNG token (cùng chuỗi thập phân); admin (mốc) file == run của
       admin, tổng chung khác; Tổng hợp một cấp vẫn key,value,count; lưới vượt 1000 ô ⇒ 422 phong bì JSON, không
       file rỗng/cắt.
  S5   Ngưỡng trên nhóm lá (config.having) — kịch bản "nhóm A" của US3: Qualification có 3 cơ hội với admin (900.5)
       nhưng rep chỉ sở hữu 1 (300); ngưỡng "giá trị > 500" ⇒ admin thấy Qualification, rep KHÔNG; tổng cộng của rep
       == tổng các nhóm còn lại của rep; Qualification vẫn có với rep khi không ngưỡng; Ma trận có ngưỡng chỉ giữ ô
       đạt; ngưỡng loại hết ⇒ 200 rỗng; CSV = cây một cấp có "Tổng cộng"; ngưỡng sai ⇒ 422 REPORT_HAVING_INVALID.
  S6   Tên thay UUID trên trục: admin tạo 2 khách hàng mang mã lượt; rep tạo 3 cơ hội gắn khách hàng (admin 1). Rep
       chạy Tổng hợp theo khách hàng / theo người phụ trách, Ma trận người phụ trách × khách hàng, cây khách hàng →
       giai đoạn ⇒ mọi khoá mang `label` = tên khách hàng / họ tên rep (không email) / nhãn giai đoạn tiếng Việt, 0
       UUID làm nhãn; file Ma trận/cây in ĐÚNG các tên đó (== /run cùng token, 0 UUID trong file); admin (mốc) thấy 2
       người phụ trách đều có tên; Tổng hợp một cấp vẫn xuất key,value,count (in id — FR-016). Từ S6 các mục S4/S5 so
       file với nhãn giai đoạn tiếng Việt (đọc valueLabels từ GET /metadata/fields như màn).
  S7a  AI Builder: bản nháp mang đủ columnGroup/having/subGroups, Áp dụng lưu đủ, rep chạy được.
  S7b  Gói + ⌘K: JSON gói xuất mang đủ khoá lưới, xem trước cài liệt kê báo cáo; ⌘K trả số của riêng rep.

Tiền điều kiện của S2b/S3/S4: trường `close_date` của crm_opportunities có index (gói CRM ≥ 1.8.0 khai sẵn).
Môi trường cài gói cũ hơn ⇒ lỗi AGGREGATE_REQUIRES_INDEXED trên trục cột ⇒ script dừng exit 2 và chỉ
cách bật (PUT /admin/fields/<id của close_date> {"options":{"indexed":true}}).

Exit: 0 = đạt · 1 = có kỳ vọng lệch · 2 = môi trường không dùng được.
ponytail: stdlib thuần (urllib/argparse), cùng khuôn scripts/acceptance-before-save-hooks.py.
"""

import argparse
import csv
import io
import json
import re
import sys
import urllib.error
import urllib.request
import uuid
from decimal import Decimal

TENANT = "northwind"
ADMIN = ("admin@example.com", "Demo-Admin-2026!")  # same demo constants scripts/acceptance-acl.py uses
STAFF = ("test@example.com", "Demo-Rep-2026!")
ENTITY = "crm_opportunities"
ACCOUNTS = "crm_accounts"


class EnvError(Exception):
    """The environment is not usable (server down, login refused, precondition missing) — exit 2."""


def req(base, method, path, token=None, body=None, timeout=20):
    """JSON request → (status, parsed body, content-type)."""
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    if token:
        headers["Authorization"] = "Bearer " + token
    r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read()
            ctype = resp.headers.get("Content-Type", "")
            return resp.status, (json.loads(raw) if raw and ctype.startswith("application/json") else raw), ctype
    except urllib.error.HTTPError as e:
        raw = e.read()
        ctype = e.headers.get("Content-Type", "")
        try:
            return e.code, (json.loads(raw) if raw else {}), ctype
        except ValueError:
            return e.code, raw, ctype
    except Exception as e:  # connection refused, timeout…
        raise EnvError(f"không gọi được {base + path} — {e}") from e


def login(base, email, password):
    code, body, _ = req(base, "POST", "/auth/login", body={"tenant": TENANT, "email": email, "password": password})
    data = (body or {}).get("data") or {}
    if code != 200 or not data.get("accessToken"):
        raise EnvError(f"đăng nhập {email} thất bại ({code})")
    return data["accessToken"], data.get("user") or {}


fails = []


def check(label, cond, detail=""):
    print(("  ĐẠT   " if cond else "  LỆCH  ") + label + (f"   [{detail}]" if detail else ""))
    if not cond:
        fails.append(label)


def dec(v):
    return None if v is None else Decimal(str(v))


def headers_kv(lst):
    """rows/columns/groups → {key: (Decimal value, count)} in response order."""
    return [(h.get("key"), dec(h.get("value")), h.get("count")) for h in (lst or [])]


class Run:
    """One acceptance run: the run tag, the tokens, and everything to clean up."""

    def __init__(self, base, tag):
        self.base, self.tag = base, tag
        self.opps, self.reports, self.accounts = [], [], []
        self.adm = self.staff = None
        self.staff_user = {}
        self.require_ai = False

    def opp(self, token, stage, close_date, amount, account=None):
        body = {"name": f"{self.tag} {stage} {close_date}", "stage": stage, "close_date": close_date,
                "amount": {"amount": amount, "currencyCode": "VND"}}
        if account:
            body["account"] = account
        code, resp, _ = req(self.base, "POST", f"/{ENTITY}", token, body)
        if code != 201:
            raise EnvError(f"tạo cơ hội thử thất bại ({code} {json.dumps(resp, ensure_ascii=False)[:200]})")
        self.opps.append(resp["data"]["id"])

    def report(self, name, typ, config):
        config = dict(config, filters=[{"field": "name", "op": "contains", "values": [self.tag]}])
        code, resp, _ = req(self.base, "POST", "/admin/reports", self.adm,
                            {"name": f"{self.tag} {name}", "entityName": ENTITY, "type": typ, "config": config})
        if code != 201:
            raise EnvError(f"admin tạo báo cáo thử thất bại ({code} {json.dumps(resp, ensure_ascii=False)[:200]})")
        rid = resp["data"]["report"]["id"]
        self.reports.append(rid)
        return rid

    def account(self, name):
        """Admin creates a customer tagged with the run (the rep may only view customers)."""
        code, resp, _ = req(self.base, "POST", f"/{ACCOUNTS}", self.adm, {"name": f"{self.tag} {name}"})
        if code != 201:
            raise EnvError(f"admin tạo khách hàng thử thất bại ({code} {json.dumps(resp, ensure_ascii=False)[:200]})")
        self.accounts.append(resp["data"]["id"])
        return resp["data"]["id"], f"{self.tag} {name}"

    def run(self, token, rid):
        return req(self.base, "GET", f"/reports/{rid}/run", token)

    def cleanup(self):
        for rid in self.reports:
            req(self.base, "DELETE", f"/admin/reports/{rid}", self.adm)
        for oid in self.opps:
            req(self.base, "DELETE", f"/{ENTITY}/{oid}", self.adm)
        for aid in self.accounts:
            req(self.base, "DELETE", f"/{ACCOUNTS}/{aid}", self.adm)


# Seed: 5 opportunities, 3 stages × 2 close months, decimal amounts; the rep owns 2.
REP_ROWS = [("Proposal", "2026-01-15", 1000.25), ("Negotiation", "2026-02-10", 2000.5)]
ADMIN_ROWS = [("Proposal", "2026-02-20", 300.75), ("Closed Won", "2026-01-05", 4000), ("Negotiation", "2026-01-25", 50.1)]


def section_s2b(r):
    for row in REP_ROWS:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS:
        r.opp(r.adm, *row)
    matrix_cfg = {"groupBy": "stage", "metric": "sum", "field": "amount",
                  "columnGroup": {"field": "close_date", "bucket": "month"}}
    matrix = r.report("Giá trị cơ hội theo giai đoạn × tháng chốt", "matrix", matrix_cfg)
    summary = r.report("Giá trị cơ hội theo giai đoạn", "summary",
                       {"groupBy": "stage", "metric": "sum", "field": "amount"})
    legacy = r.report("Ma trận chưa có trục cột", "matrix", {"groupBy": "stage", "metric": "sum", "field": "amount"})

    code, body, _ = req(r.base, "GET", f"/admin/reports/{matrix}", r.adm)
    cg = (((body or {}).get("data") or {}).get("report") or {}).get("config", {}).get("columnGroup")
    check("khoá columnGroup đi qua API quản trị và được lưu", cg == {"field": "close_date", "bucket": "month"}, json.dumps(cg))

    code, body, _ = r.run(r.staff, matrix)
    if code == 422 and (body or {}).get("code") == "AGGREGATE_REQUIRES_INDEXED" and \
            ((body or {}).get("details") or {}).get("axis") == "columnGroup":
        raise EnvError("trường close_date chưa có index trên môi trường này (gói CRM < 1.8.0) — admin bật: "
                       "PUT /admin/fields/<id close_date> {\"options\":{\"indexed\":true}} rồi chạy lại")
    d = (body or {}).get("data") or {}
    check("rep chạy Ma trận ⇒ 200 type=matrix", code == 200 and d.get("type") == "matrix", f"{code} {d.get('type')}")
    if code != 200 or d.get("type") != "matrix":
        return
    check("trục hàng/cột đúng cấu hình",
          d.get("rowAxis") == {"field": "stage", "bucket": ""} and d.get("columnAxis") == {"field": "close_date", "bucket": "month"},
          f"{d.get('rowAxis')} × {d.get('columnAxis')}")
    rows, cols = headers_kv(d.get("rows")), headers_kv(d.get("columns"))
    want_rows = [("Negotiation", Decimal("2000.5"), 1), ("Proposal", Decimal("1000.25"), 1)]
    want_cols = [("2026-01", Decimal("1000.25"), 1), ("2026-02", Decimal("2000.5"), 1)]
    check("hàng = đúng 2 giai đoạn của rep, tổng hàng giảm dần", rows == want_rows, str(rows))
    check("cột = tháng chốt của rep theo lịch", cols == want_cols, str(cols))
    cells = sorted((c["r"], c["c"], dec(c["value"]), c["count"]) for c in d.get("cells") or [])
    check("ô chỉ có 2 cơ hội của rep (ô trống không có mặt)",
          cells == [(0, 1, Decimal("2000.5"), 1), (1, 0, Decimal("1000.25"), 1)], str(cells))
    total = d.get("total") or {}
    check("tổng chung = 2 cơ hội của rep (3000.75)", dec(total.get("value")) == Decimal("3000.75") and total.get("count") == 2,
          json.dumps(total))

    code, sbody, _ = r.run(r.staff, summary)
    sgroups = headers_kv(((sbody or {}).get("data") or {}).get("groups"))
    check("tổng hàng == báo cáo Tổng hợp theo giai đoạn chạy bằng CÙNG token", code == 200 and sgroups == rows, str(sgroups))

    code, abody, _ = r.run(r.adm, matrix)
    atotal = ((abody or {}).get("data") or {}).get("total") or {}
    check("admin thấy cả 5 cơ hội (mốc so sánh), tổng chung khác của rep",
          dec(atotal.get("value")) == Decimal("7351.6") and atotal.get("count") == 5 and dec(atotal.get("value")) != dec(total.get("value")),
          json.dumps(atotal))

    code, _, ctype = req(r.base, "GET", f"/reports/{matrix}/export.csv", r.staff)
    check("rep xuất CSV Ma trận ⇒ 200 text/csv (từ lát S4; so từng ô ở mục S4)",
          code == 200 and ctype.startswith("text/csv"), f"{code} {ctype}")

    code, lbody, _ = r.run(r.staff, legacy)
    code2, sbody2, _ = r.run(r.staff, summary)
    ld, sd = (lbody or {}).get("data") or {}, (sbody2 or {}).get("data") or {}
    check("Ma trận chưa có trục cột trả y hệt Tổng hợp cùng cấu hình (type summary)",
          code == 200 and code2 == 200 and ld == sd and ld.get("type") == "summary", json.dumps(ld, ensure_ascii=False)[:160])


# S3 adds two more rep rows so the rep's Proposal spans 3 rows over 2 months — an average of the
# month averages (525) would differ from the true average (533.33…).
REP_EXTRA_S3 = [("Proposal", "2026-01-28", 99.75), ("Proposal", "2026-02-12", 500)]


def tree_kv(groups):
    """groups → ["key=value/count[children…]"], values exact without trailing zeros (nesting + order pinned)."""
    out = []
    for g in groups or []:
        v = dec(g.get("value"))
        s = f"{g.get('key')}={'null' if v is None else format(v.normalize(), 'f')}/{g.get('count')}"
        if "children" in g:
            s += "[" + " ".join(tree_kv(g["children"])) + "]"
        out.append(s)
    return out


def section_s3(r):
    for row in REP_ROWS + REP_EXTRA_S3:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS:
        r.opp(r.adm, *row)
    month = {"field": "close_date", "bucket": "month"}
    tree = r.report("Cơ hội theo giai đoạn → tháng chốt", "summary",
                    {"groupBy": "stage", "metric": "sum", "field": "amount", "subGroups": [month]})
    flat = r.report("Tổng hợp một cấp (subGroups rỗng)", "summary",
                    {"groupBy": "stage", "metric": "sum", "field": "amount", "subGroups": []})

    code, body, _ = req(r.base, "GET", f"/admin/reports/{tree}", r.adm)
    sg = (((body or {}).get("data") or {}).get("report") or {}).get("config", {}).get("subGroups")
    check("khoá subGroups đi qua API quản trị và được lưu", sg == [month], json.dumps(sg))
    code, body, _ = req(r.base, "GET", f"/admin/reports/{flat}", r.adm)
    fcfg = (((body or {}).get("data") or {}).get("report") or {}).get("config", {})
    check("subGroups rỗng lưu như không có khoá (Tổng hợp một cấp giữ nguyên)", "subGroups" not in fcfg, json.dumps(fcfg))

    code, body, _ = r.run(r.staff, tree)
    if code == 422 and (body or {}).get("code") == "AGGREGATE_REQUIRES_INDEXED" and \
            ((body or {}).get("details") or {}).get("axis") == "subGroups[0]":
        raise EnvError("trường close_date chưa có index trên môi trường này (gói CRM < 1.8.0) — admin bật: "
                       "PUT /admin/fields/<id close_date> {\"options\":{\"indexed\":true}} rồi chạy lại")
    d = (body or {}).get("data") or {}
    check("rep chạy Tổng hợp nhiều cấp ⇒ 200 type=summary có levels",
          code == 200 and d.get("type") == "summary"
          and d.get("levels") == [{"field": "stage", "bucket": ""}, {"field": "close_date", "bucket": "month"}],
          f"{code} {d.get('type')} {d.get('levels')}")
    if code != 200 or not d.get("levels"):
        return
    want = ["Negotiation=2000.5/1[2026-02=2000.5/1]",
            "Proposal=1600/3[2026-01=1100/2 2026-02=500/1]"]
    got = tree_kv(d.get("groups"))
    check("cây chỉ gồm nhánh chứa cơ hội của rep; tổng phụ đúng; tháng theo lịch", got == want, str(got))
    total = d.get("total") or {}
    check("tổng cộng = 4 cơ hội của rep (3600.5)", dec(total.get("value")) == Decimal("3600.5") and total.get("count") == 4,
          json.dumps(total))

    # Every metric: level-1 sub-totals == the one-level summary, run with the SAME (rep) token.
    for metric in ("count", "sum", "avg", "min", "max"):
        base = {"groupBy": "stage", "metric": metric}
        if metric != "count":
            base["field"] = "amount"
        t_id = r.report(f"Cây {metric}", "summary", dict(base, subGroups=[month]))
        s_id = r.report(f"Một cấp {metric}", "summary", base)
        _, tb, _ = r.run(r.staff, t_id)
        _, sb, _ = r.run(r.staff, s_id)
        lvl1 = [(g.get("key"), g.get("value"), g.get("count")) for g in ((tb or {}).get("data") or {}).get("groups") or []]
        one = [(g.get("key"), g.get("value"), g.get("count")) for g in ((sb or {}).get("data") or {}).get("groups") or []]
        check(f"[{metric}] tổng phụ cấp 1 == Tổng hợp một cấp theo giai đoạn (cùng token, cùng chuỗi thập phân)",
              bool(lvl1) and lvl1 == one, f"{lvl1} vs {one}")
        if metric == "avg":
            prop = next((g for g in ((tb or {}).get("data") or {}).get("groups") or [] if g.get("key") == "Proposal"), {})
            check("[avg] Proposal = trung bình 3 dòng gốc (533.33…), không phải trung bình của 2 tháng (525)",
                  str(dec(prop.get("value")))[:6] == "533.33", str(prop.get("value")))

    code, abody, _ = r.run(r.adm, tree)
    ad = (abody or {}).get("data") or {}
    atotal = ad.get("total") or {}
    akeys = [g.get("key") for g in ad.get("groups") or []]
    check("admin thấy cả 7 cơ hội (mốc so sánh) và nhánh rep không thấy (Closed Won)",
          dec(atotal.get("value")) == Decimal("7951.35") and atotal.get("count") == 7 and "Closed Won" in akeys,
          f"{json.dumps(atotal)} {akeys}")

    code, _, ctype = req(r.base, "GET", f"/reports/{tree}/export.csv", r.staff)
    check("rep xuất CSV cây ⇒ 200 text/csv (từ lát S4; so từng tổng phụ ở mục S4)",
          code == 200 and ctype.startswith("text/csv"), f"{code} {ctype}")


def download(r, token, rid):
    """GET export.csv → (status, content-type, parsed rows | JSON body, starts-with-BOM)."""
    code, body, ctype = req(r.base, "GET", f"/reports/{rid}/export.csv", token)
    if not isinstance(body, (bytes, bytearray)):
        return code, ctype, body, False
    text = body.decode("utf-8")
    bom = text.startswith("﻿")
    return code, ctype, list(csv.reader(io.StringIO(text.lstrip("﻿")))), bom


def numbers_plain(rows):
    """Heading row and first column as they are; every other cell's decimal without trailing zeros."""
    return [line if i == 0 else [line[0]] + [format(Decimal(x).normalize(), "f") if x else "" for x in line[1:]]
            for i, line in enumerate(rows)]


def csv_key(g):
    """How the file prints a /run group: its label when the run named it (S6), else the key; NULL = (trống)."""
    if g.get("label") is not None:
        return g["label"]
    return "(trống)" if g.get("key") is None else g["key"]


def field_meta(r):
    """(field name → label, stage code → Vietnamese label) as the screen reads them (GET /metadata/fields)."""
    code, body, _ = req(r.base, "GET", f"/metadata/fields/{ENTITY}", r.staff)
    fields = (body or {}).get("data") or []
    labels = {f.get("name"): f.get("label") or f.get("name") for f in fields}
    stage = next((f for f in fields if f.get("name") == "stage"), {})
    return labels, ((stage.get("options") or {}).get("valueLabels") or {})


def csv_val(g):
    return "" if g.get("value") is None else g["value"]


def matrix_csv_from_run(d, row_title, col_title):
    """The file contract of a matrix (FR-020), stated from its /run payload."""
    cols = d.get("columns") or []
    out = [[f"{row_title} \\ {col_title}"] + [csv_key(c) for c in cols] + ["Tổng"]]
    lines = [[csv_key(row)] + [""] * len(cols) + [csv_val(row)] for row in d.get("rows") or []]
    for c in d.get("cells") or []:
        lines[c["r"]][1 + c["c"]] = csv_val(c)
    return out + lines + [["Tổng"] + [csv_val(c) for c in cols] + [csv_val(d.get("total") or {})]]


def tree_csv_from_run(d, titles):
    """The file contract of a tree, stated from its /run payload."""
    n = len(titles)
    out = [list(titles) + ["Giá trị", "Số lượng"]]

    def line(path, label, g):
        row = list(path) + [label] + [""] * (n - len(path) - 1)
        return row + [csv_val(g), str(g.get("count"))]

    def walk(groups, path):
        for g in groups or []:
            if "children" in g:
                walk(g["children"], path + [csv_key(g)])
                out.append(line(path, "Tổng " + csv_key(g), g))
            else:
                out.append(line(path, csv_key(g), g))

    walk(d.get("groups"), [])
    return out + [line([], "Tổng cộng", d.get("total") or {})]


def section_s4(r):
    for row in REP_ROWS:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS:
        r.opp(r.adm, *row)
    month = {"field": "close_date", "bucket": "month"}
    matrix = r.report("Giá trị cơ hội theo giai đoạn × tháng chốt", "matrix",
                      {"groupBy": "stage", "metric": "sum", "field": "amount", "columnGroup": month})
    tree = r.report("Cơ hội theo giai đoạn → tháng chốt", "summary",
                    {"groupBy": "stage", "metric": "sum", "field": "amount", "subGroups": [month]})
    legacy = r.report("Tổng hợp một cấp", "summary", {"groupBy": "stage", "metric": "sum", "field": "amount"})

    # Axis headings = the field labels the screen shows (GET /metadata/fields, same call as nexus-web);
    # since S6 a stage prints as its Vietnamese label ("Đàm phán"), the code where the tenant has none.
    labels, stage_vl = field_meta(r)
    stage_t, close_t = labels.get("stage", "stage"), labels.get("close_date", "close_date")
    neg, prop = stage_vl.get("Negotiation", "Negotiation"), stage_vl.get("Proposal", "Proposal")

    code, body, _ = r.run(r.staff, matrix)
    if code == 422 and (body or {}).get("code") == "AGGREGATE_REQUIRES_INDEXED":
        raise EnvError("trường close_date chưa có index trên môi trường này (gói CRM < 1.8.0) — admin bật: "
                       "PUT /admin/fields/<id close_date> {\"options\":{\"indexed\":true}} rồi chạy lại")
    run_m = (body or {}).get("data") or {}
    code, ctype, rows, bom = download(r, r.staff, matrix)
    check("rep tải CSV Ma trận ⇒ 200 text/csv có BOM UTF-8", code == 200 and ctype.startswith("text/csv") and bom,
          f"{code} {ctype} bom={bom}")
    want = [[f"{stage_t} \\ {close_t}", "2026-01", "2026-02", "Tổng"],
            [neg, "", "2000.5", "2000.5"],
            [prop, "1000.25", "", "1000.25"],
            ["Tổng", "1000.25", "2000.5", "3000.75"]]
    check("file Ma trận = đúng lưới của rep: 2 giai đoạn × 2 tháng, ô trống để trống, cột/hàng Tổng",
          isinstance(rows, list) and numbers_plain(rows) == want, str(rows)[:300])
    check("file Ma trận == /run cùng token, TỪNG ô và từng tổng (cùng chuỗi thập phân)",
          isinstance(rows, list) and rows == matrix_csv_from_run(run_m, stage_t, close_t), str(rows)[:200])

    code, body, _ = r.run(r.staff, tree)
    run_t = (body or {}).get("data") or {}
    code, ctype, trows, bom = download(r, r.staff, tree)
    check("rep tải CSV cây ⇒ 200 text/csv có BOM UTF-8", code == 200 and ctype.startswith("text/csv") and bom,
          f"{code} {ctype} bom={bom}")
    check("file cây: mỗi cấp một cột, 'Tổng <giai đoạn>' sau các tháng, 'Tổng cộng' cuối",
          isinstance(trows, list) and [t[:2] for t in trows] == [
              [stage_t, close_t], [neg, "2026-02"], ["Tổng " + neg, ""],
              [prop, "2026-01"], ["Tổng " + prop, ""], ["Tổng cộng", ""]], str(trows)[:300])
    check("file cây == /run cùng token, từng lá, tổng phụ và tổng cộng",
          isinstance(trows, list) and trows == tree_csv_from_run(run_t, [stage_t, close_t]), str(trows)[:200])

    code, body, _ = r.run(r.adm, matrix)
    run_a = (body or {}).get("data") or {}
    code, _, arows, _ = download(r, r.adm, matrix)
    check("admin (mốc): file == /run của admin, tổng chung 7351.6 khác của rep",
          isinstance(arows, list) and arows == matrix_csv_from_run(run_a, stage_t, close_t)
          and Decimal(arows[-1][-1]) == Decimal("7351.6"), str(arows[-1:] if isinstance(arows, list) else arows))

    code, ctype, lrows, _ = download(r, r.staff, legacy)
    check("Tổng hợp một cấp vẫn xuất key,value,count như cũ",
          code == 200 and isinstance(lrows, list) and lrows and lrows[0] == ["key", "value", "count"], str(lrows)[:120])

    # Over the cap: the rep's rows by id × 32+ distinct close days > 1000 dense cells ⇒ 422 JSON, no file.
    for i in range(32):
        r.opp(r.staff, "Proposal", f"2026-03-{i + 1:02d}" if i < 31 else "2026-04-01", 1)
    big = r.report("Quá trần ô", "matrix", {"groupBy": "id", "metric": "count",
                                             "columnGroup": {"field": "close_date", "bucket": "day"}})
    code, ctype, ebody, bom = download(r, r.staff, big)
    check("vượt trần ⇒ 422 REPORT_MATRIX_TOO_MANY_CELLS phong bì JSON, không file rỗng/cắt",
          code == 422 and ctype.startswith("application/json") and isinstance(ebody, dict)
          and ebody.get("code") == "REPORT_MATRIX_TOO_MANY_CELLS" and not bom,
          f"{code} {ctype} {json.dumps(ebody, ensure_ascii=False)[:160] if isinstance(ebody, dict) else ebody}")


# S5 — the spec's "group A" (US3): Qualification holds 3 rows for the admin (900.5) of which the rep
# owns 1 (300); Negotiation is the rep's 600.25; Prospecting the admin's 50.
REP_ROWS_S5 = [("Qualification", "2026-01-10", 300), ("Negotiation", "2026-02-10", 600.25)]
ADMIN_ROWS_S5 = [("Qualification", "2026-01-20", 300), ("Qualification", "2026-02-05", 300.5), ("Prospecting", "2026-01-15", 50)]


def section_s5(r):
    for row in REP_ROWS_S5:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS_S5:
        r.opp(r.adm, *row)
    over500 = {"on": "value", "op": "gt", "values": ["500"]}
    base = {"groupBy": "stage", "metric": "sum", "field": "amount"}
    kept = r.report("Giai đoạn có giá trị > 500", "summary", dict(base, having=over500))
    plain = r.report("Giai đoạn (không ngưỡng)", "summary", base)

    code, body, _ = req(r.base, "GET", f"/admin/reports/{kept}", r.adm)
    hv = (((body or {}).get("data") or {}).get("report") or {}).get("config", {}).get("having")
    check("khoá having đi qua API quản trị và được lưu", hv == over500, json.dumps(hv))

    code, body, _ = r.run(r.staff, kept)
    d = (body or {}).get("data") or {}
    check("rep chạy Tổng hợp có ngưỡng ⇒ 200 cây MỘT cấp (levels = giai đoạn) + echo ngưỡng",
          code == 200 and d.get("type") == "summary" and d.get("levels") == [{"field": "stage", "bucket": ""}]
          and d.get("having") == over500, f"{code} {d.get('type')} {d.get('levels')} {d.get('having')}")
    if code != 200:
        return
    rep_groups = headers_kv(d.get("groups"))
    check("rep: chỉ Negotiation (600.25) đạt ngưỡng — Qualification của rep chỉ 300 nên bị loại",
          rep_groups == [("Negotiation", Decimal("600.25"), 1)], str(rep_groups))
    total = d.get("total") or {}
    check("tổng cộng của rep == tổng các nhóm còn lại của rep (600.25 / 1)",
          dec(total.get("value")) == sum((g[1] for g in rep_groups), Decimal(0)) == Decimal("600.25") and total.get("count") == 1,
          json.dumps(total))

    code, pbody, _ = r.run(r.staff, plain)
    pgroups = headers_kv(((pbody or {}).get("data") or {}).get("groups"))
    check("Qualification CÓ với rep (300 / 1) khi không ngưỡng — ngưỡng loại nó, không phải phạm vi",
          ("Qualification", Decimal("300"), 1) in pgroups, str(pgroups))

    code, abody, _ = r.run(r.adm, kept)
    ad = (abody or {}).get("data") or {}
    agroups, atotal = headers_kv(ad.get("groups")), ad.get("total") or {}
    check("admin (mốc): Qualification 900.5 / 3 và Negotiation 600.25 đạt ngưỡng; tổng 1500.75 / 4",
          agroups == [("Qualification", Decimal("900.5"), 3), ("Negotiation", Decimal("600.25"), 1)]
          and dec(atotal.get("value")) == Decimal("1500.75") and atotal.get("count") == 4, f"{agroups} {json.dumps(atotal)}")

    matrix = r.report("Ô giai đoạn × tháng chốt > 500", "matrix",
                      dict(base, columnGroup={"field": "close_date", "bucket": "month"}, having=over500))
    code, mbody, _ = r.run(r.staff, matrix)
    if code == 422 and (mbody or {}).get("code") == "AGGREGATE_REQUIRES_INDEXED":
        raise EnvError("trường close_date chưa có index trên môi trường này (gói CRM < 1.8.0) — admin bật: "
                       "PUT /admin/fields/<id close_date> {\"options\":{\"indexed\":true}} rồi chạy lại")
    md = (mbody or {}).get("data") or {}
    cells = [(c["r"], c["c"], dec(c["value"]), c["count"]) for c in md.get("cells") or []]
    check("Ma trận có ngưỡng: rep chỉ còn ô Negotiation × 2026-02 (600.25); hàng/cột/tổng chung chỉ cộng ô đó",
          code == 200 and md.get("type") == "matrix" and md.get("having") == over500
          and headers_kv(md.get("rows")) == [("Negotiation", Decimal("600.25"), 1)]
          and headers_kv(md.get("columns")) == [("2026-02", Decimal("600.25"), 1)]
          and cells == [(0, 0, Decimal("600.25"), 1)] and dec((md.get("total") or {}).get("value")) == Decimal("600.25"),
          f"{code} {headers_kv(md.get('rows'))} {cells} {md.get('total')}")

    none = r.report("Không nhóm nào đạt", "summary", dict(base, having={"on": "value", "op": "gt", "values": ["1000000000"]}))
    code, nbody, _ = r.run(r.staff, none)
    nd = (nbody or {}).get("data") or {}
    check("ngưỡng loại hết ⇒ 200, groups [], total {value:null, count:0} (không lỗi)",
          code == 200 and nd.get("groups") == [] and nd.get("total") == {"value": None, "count": 0}, json.dumps(nd)[:160])

    _, stage_vl = field_meta(r)  # since S6 the tree file prints the stage's Vietnamese label
    code, ctype, rows, bom = download(r, r.staff, kept)
    check("rep xuất CSV Tổng hợp có ngưỡng ⇒ cây một cấp: nhóm còn lại + 'Tổng cộng' (không phải key,value,count)",
          code == 200 and bom and isinstance(rows, list) and len(rows) == 3 and rows[0][1:] == ["Giá trị", "Số lượng"]
          and rows[1][0] == stage_vl.get("Negotiation", "Negotiation") and Decimal(rows[1][1]) == Decimal("600.25")
          and rows[2][0] == "Tổng cộng" and Decimal(rows[2][1]) == Decimal("600.25"), str(rows)[:200])

    code, ebody, _ = req(r.base, "POST", "/admin/reports", r.adm, {
        "name": f"{r.tag} ngưỡng sai", "entityName": ENTITY, "type": "summary",
        "config": dict(base, having={"on": "value", "op": "between", "values": ["10", "9.5"]})})
    if code == 201:  # an unfixed server stores it — clean up anyway
        r.reports.append(((ebody or {}).get("data") or {}).get("report", {}).get("id"))
    check("ngưỡng sai hình dạng (cận dưới > cận trên) ⇒ 422 REPORT_HAVING_INVALID {field:having, reason:range}",
          code == 422 and (ebody or {}).get("code") == "REPORT_HAVING_INVALID"
          and (ebody or {}).get("details") == {"field": "having", "reason": "range"}, f"{code} {json.dumps(ebody, ensure_ascii=False)[:160]}")


UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", re.I)


def all_groups(d):
    """Every group of a /run payload: groups (+ children), rows, columns."""
    out = []

    def walk(gs):
        for g in gs or []:
            out.append(g)
            walk(g.get("children"))

    walk(d.get("groups"))
    return out + list(d.get("rows") or []) + list(d.get("columns") or [])


def uuid_cells(rows):
    return [c for line in rows for c in line if UUID_RE.match(c or "")] if isinstance(rows, list) else ["<no file>"]


def section_s6(r):
    rep_name = r.staff_user.get("fullName") or ""
    if not rep_name:
        raise EnvError(f"{STAFF[0]} chưa có họ tên — trục người phụ trách không có tên để hiện (S6 không lấy email)")
    labels, stage_vl = field_meta(r)
    account_t, stage_t = labels.get("account", "account"), labels.get("stage", "stage")
    a_id, a_name = r.account("Công ty An Phát")
    b_id, b_name = r.account("Công ty Bình Minh")
    names = {a_id: a_name, b_id: b_name}
    for stage, day, amount, acc in [("Proposal", "2026-01-15", 1000.25, a_id), ("Negotiation", "2026-02-10", 2000.5, a_id),
                                    ("Proposal", "2026-02-20", 500, b_id)]:
        r.opp(r.staff, stage, day, amount, acc)
    r.opp(r.adm, "Closed Won", "2026-01-05", 4000, b_id)
    base = {"metric": "sum", "field": "amount"}
    by_account = r.report("Giá trị theo khách hàng", "summary", dict(base, groupBy="account"))
    by_owner = r.report("Giá trị theo người phụ trách", "summary", dict(base, groupBy="ownerId"))
    matrix = r.report("Người phụ trách × khách hàng", "matrix", dict(base, groupBy="ownerId", columnGroup={"field": "account"}))
    tree = r.report("Khách hàng → giai đoạn", "summary", dict(base, groupBy="account", subGroups=[{"field": "stage"}]))

    code, body, _ = r.run(r.staff, by_account)
    d = (body or {}).get("data") or {}
    got = [(g.get("key"), g.get("label")) for g in d.get("groups") or []]
    check("rep: Tổng hợp theo khách hàng ⇒ mỗi nhóm mang label = tên khách hàng (không phải UUID)",
          code == 200 and got == [(a_id, a_name), (b_id, b_name)], str(got))
    code, body, _ = r.run(r.staff, by_owner)
    d = (body or {}).get("data") or {}
    got = [(g.get("key"), g.get("label")) for g in d.get("groups") or []]
    check(f"rep: Tổng hợp theo người phụ trách ⇒ label = họ tên của rep ({rep_name}), không email",
          code == 200 and got == [(r.staff_user.get("id"), rep_name)], str(got))

    code, body, _ = r.run(r.staff, matrix)
    run_m = (body or {}).get("data") or {}
    check("rep: Ma trận người phụ trách × khách hàng ⇒ hàng = tên rep, cột = tên khách hàng",
          code == 200 and [g.get("label") for g in run_m.get("rows") or []] == [rep_name]
          and [(g.get("key"), g.get("label")) for g in run_m.get("columns") or []] == [(a_id, a_name), (b_id, b_name)],
          f"{code} {[(g.get('key'), g.get('label')) for g in all_groups(run_m)]}")
    code, ctype, rows, bom = download(r, r.staff, matrix)
    check("rep: file Ma trận in đúng tên (tiêu đề cột = khách hàng, cột đầu = rep), == /run cùng token",
          code == 200 and isinstance(rows, list) and rows[0][1:3] == [a_name, b_name] and rows[1][0] == rep_name
          and rows == matrix_csv_from_run(run_m, "ownerId", account_t), str(rows)[:300])
    check("rep: 0 UUID trong file Ma trận", not uuid_cells(rows), str(uuid_cells(rows)))

    code, body, _ = r.run(r.staff, tree)
    run_t = (body or {}).get("data") or {}
    lvl = [(g.get("label"), [c.get("label") for c in g.get("children") or []]) for g in run_t.get("groups") or []]
    # A code the tenant gave no Vietnamese label carries no label (None).
    want_lvl = [(a_name, [stage_vl.get("Negotiation"), stage_vl.get("Proposal")]), (b_name, [stage_vl.get("Proposal")])]
    check("rep: cây khách hàng → giai đoạn ⇒ nhánh mang tên khách hàng, cấp 2 mang nhãn giai đoạn tiếng Việt",
          code == 200 and lvl == want_lvl, f"{lvl} want {want_lvl}")
    code, ctype, trows, bom = download(r, r.staff, tree)
    subtotals = [t[0] for t in trows if t and t[0].startswith("Tổng ")] if isinstance(trows, list) else []
    check("rep: file cây == /run cùng token; dòng tổng phụ 'Tổng <tên khách hàng>'",
          code == 200 and isinstance(trows, list) and trows == tree_csv_from_run(run_t, [account_t, stage_t])
          and subtotals == [f"Tổng {a_name}", f"Tổng {b_name}", "Tổng cộng"], f"{subtotals} {str(trows)[:200]}")
    check("rep: 0 UUID trong file cây", not uuid_cells(trows), str(uuid_cells(trows)))

    leaked = [g.get("label") for d in (run_m, run_t) for g in all_groups(d) if UUID_RE.match(g.get("label") or "")]
    unnamed = [g.get("key") for d in (run_m, run_t) for g in all_groups(d) if UUID_RE.match(g.get("key") or "") and not g.get("label")]
    check("rep: không nhãn nào là UUID, không khoá UUID nào thiếu nhãn", not leaked and not unnamed, f"{leaked} {unnamed}")
    check("tên khách hàng trên /run đúng bản ghi (label khớp id)",
          all(names.get(g.get("key"), g.get("label")) == g.get("label") for d in (run_m, run_t) for g in all_groups(d)), "")

    code, body, _ = r.run(r.adm, matrix)
    run_a = (body or {}).get("data") or {}
    arows = [g.get("label") for g in run_a.get("rows") or []]
    check("admin (mốc): Ma trận có 2 người phụ trách (rep + admin), cả hai mang họ tên",
          code == 200 and len(arows) == 2 and rep_name in arows and all(arows) and not any(UUID_RE.match(x) for x in arows), str(arows))

    code, ctype, lrows, _ = download(r, r.staff, by_account)
    check("Tổng hợp một cấp vẫn xuất key,value,count như cũ (in id — FR-016, 'Nợ còn mở')",
          code == 200 and isinstance(lrows, list) and lrows[0] == ["key", "value", "count"] and {l[0] for l in lrows[1:]} == {a_id, b_id},
          str(lrows)[:200])


def ai_report_draft(r, message):
    """Ask the AI Builder (admin). Returns (status, first report draft or None); 503 = AI off."""
    code, body, _ = req(r.base, "POST", "/admin/ai/builder/chat", r.adm, {"message": message}, timeout=180)  # real model call: slow
    drafts = ((body or {}).get("data") or {}).get("draft") or []
    return code, next((d.get("report") for d in drafts if d.get("kind") == "report" and d.get("report")), None)


def section_s7a(r):
    """S7a: the AI draft carries all 3 grid keys and Apply saves them (the pair that must travel together)."""
    for row in REP_ROWS:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS:
        r.opp(r.adm, *row)
    code, draft = ai_report_draft(
        r, f"Tạo báo cáo bảng chéo cơ hội: giai đoạn (hàng) × tháng chốt (cột), tổng số tiền amount, "
           f"chỉ giữ những ô trên 10.000 — đối tượng {ENTITY}")
    if code == 200 and draft:
        cfg = draft.get("config") or {}
        print(f"  [AI kịch bản 1] type={draft.get('type')} columnGroup={json.dumps(cfg.get('columnGroup'))} having={json.dumps(cfg.get('having'))}")
        check("AI đề xuất Ma trận mang columnGroup + having (ngưỡng theo ô)",
              draft.get("type") == "matrix" and bool(cfg.get("columnGroup")) and bool(cfg.get("having")), json.dumps(cfg))
    else:
        if r.require_ai:
            raise EnvError(f"--require-ai: AI không trả bản nháp báo cáo (HTTP {code}) — đường AI không được chứng minh")
        print(f"  (AI không trả bản nháp báo cáo — HTTP {code}; gateway/AI tắt? Dùng bản nháp dựng tay có đúng hình dạng "
              f"AI — đường Áp dụng vẫn được chứng minh, đường AI thì KHÔNG: ghi rõ trong STATE.)")
        cfg = {"groupBy": "stage", "metric": "sum", "field": "amount",
               "columnGroup": {"field": "close_date", "bucket": "month"},
               "having": {"on": "value", "op": "gt", "values": ["10000"]}}
        draft = {"type": "matrix", "config": cfg}
    # Apply = POST /admin/reports with the draft's config (FE draftReportToPayload does exactly this).
    rid = r.report("AI nháp lưới", draft["type"], cfg)
    code, body, _ = req(r.base, "GET", f"/admin/reports/{rid}", r.adm)
    saved = (((body or {}).get("data") or {}).get("report") or {}).get("config", {})
    for k in ("columnGroup", "having"):
        check(f"Áp dụng lưu đủ khoá {k}", saved.get(k) == cfg.get(k), json.dumps(saved.get(k)))
    code, body, _ = r.run(r.staff, rid)
    d = (body or {}).get("data") or {}
    check("rep (không-admin) mở báo cáo vừa áp ⇒ 200 Ma trận", code == 200 and d.get("type") == "matrix",
          f"{code} {d.get('type')} {json.dumps(body, ensure_ascii=False)[:200]}")
    # US7 kịch bản 4: "chỉ giữ giai đoạn có tổng > X" must NOT become a matrix + having.
    code, draft2 = ai_report_draft(
        r, f"Tạo báo cáo cơ hội {ENTITY} theo giai đoạn × tháng chốt, tổng amount, chỉ giữ giai đoạn có tổng trên 10.000")
    if code == 200 and draft2:
        c2 = draft2.get("config") or {}
        print(f"  [AI kịch bản 4] type={draft2.get('type')} groupBy={c2.get('groupBy')} columnGroup={json.dumps(c2.get('columnGroup'))} "
              f"subGroups={json.dumps(c2.get('subGroups'))} having={json.dumps(c2.get('having'))}")
        # FR-012: having filters cells / deepest-level groups, never a parent total — so a draft with
        # having may only be a ONE-level summary (no columnGroup, no subGroups).
        check("'giai đoạn có tổng > X' KHÔNG ra having trên Ma trận / Tổng hợp nhiều cấp",
              not (c2.get("having") and (c2.get("columnGroup") or c2.get("subGroups") or draft2.get("type") == "matrix")),
              json.dumps(c2))
    elif code == 200:
        # No draft (refusal / limitation text / index-first proposal) proves nothing about scenario 4: not a check.
        if r.require_ai:
            raise EnvError("--require-ai: kịch bản 4 không ra bản nháp báo cáo — chưa chứng minh được; chạy lại hoặc đọc phản hồi AI")
        print("  (kịch bản 4: AI không trả bản nháp — từ chối/đề xuất bật index trước; KHÔNG tính là đạt, ghi rõ trong STATE.)")
    else:
        if r.require_ai:
            raise EnvError(f"--require-ai: kịch bản 4 AI không chạy (HTTP {code})")
        print(f"  (kịch bản 4: AI không chạy — HTTP {code}; đường AI chưa được chứng minh.)")


def section_s7b(r):
    """S7b: the pack carries every grid key out (export + dry-run install on `northwind`), and the Ctrl-K assistant
    answers a tree report with the asker's OWN numbers. Install-then-run as non-admin is pinned by the
    integration test TestPackReport_GridKeysInstallThenRunAsNonAdmin (no throwaway tenant on a dev machine)."""
    for row in REP_ROWS:
        r.opp(r.staff, *row)
    for row in ADMIN_ROWS:
        r.opp(r.adm, *row)
    month = {"field": "close_date", "bucket": "month"}
    tree_name = "Giá trị cơ hội theo giai đoạn rồi theo tháng"
    r.report(tree_name, "summary", {"groupBy": "stage", "metric": "sum", "field": "amount", "subGroups": [month]})
    mat_name = "Giai đoạn × tháng có ngưỡng"
    r.report(mat_name, "matrix", {"groupBy": "stage", "metric": "sum", "field": "amount", "columnGroup": month,
                                  "having": {"on": "value", "op": "gt", "values": ["100"]}})

    # Pack: the exported JSON carries columnGroup/having/subGroups, and the install preview lists both reports.
    code, body, _ = req(r.base, "GET", f"/admin/verticals/export?entities={ENTITY}", r.adm, timeout=60)
    pack = ((body or {}).get("data") or {}).get("pack") or {}
    by_name = {x.get("name"): x.get("config") or {} for x in pack.get("reports") or []}
    tn, mn = f"{r.tag} {tree_name}", f"{r.tag} {mat_name}"
    check("export 200 và có cả hai báo cáo lưới", code == 200 and tn in by_name and mn in by_name, str(code))
    t, m = by_name.get(tn, {}), by_name.get(mn, {})
    check("JSON gói mang subGroups của báo cáo cây", t.get("subGroups") == [month], json.dumps(t.get("subGroups")))
    check("JSON gói mang columnGroup + having của Ma trận",
          m.get("columnGroup") == month and (m.get("having") or {}).get("op") == "gt", json.dumps(m))
    code, body, _ = req(r.base, "POST", "/admin/verticals/install?dryRun=true", r.adm, {"pack": pack}, timeout=60)
    plan = (((body or {}).get("data") or {}).get("plan")) or {}
    listed = set(plan.get("reportsToCreate") or []) | set(plan.get("reportsToSkip") or [])
    check("xem trước cài (dry-run) trên tổ chức northwind liệt kê cả hai báo cáo lưới",
          code == 200 and {tn, mn} <= listed, f"{code} {sorted(listed)[:4]}")

    # Ctrl-K: the rep asks (reply-only check: /assistant/ask returns no tool trace, so this does NOT prove the tree branch; the unit test does); the answer must carry the rep's own total (3000.75), never the admin-wide one.
    code, body, _ = req(r.base, "POST", "/assistant/ask", r.staff,
                        {"message": f'Chạy báo cáo "{tn}" và cho tôi biết tổng giá trị theo từng giai đoạn'}, timeout=180)
    data = (body or {}).get("data") if isinstance(body, dict) else None
    reply = str((data or body or {}).get("reply") or "") if isinstance(body, dict) else ""
    if code == 200 and reply:
        print(f"  [⌘K trả lời] {reply[:300]!r}")
        digits = re.sub(r"[^0-9]", "", reply)
        check("⌘K trả lời theo số của riêng rep (3000,75), không lộ tổng của admin (7351,6)",
              re.search(r"(?<!\d)300075(?!\d)", digits) is not None and "73516" not in digits, reply[:200])
    else:
        if r.require_ai:
            raise EnvError(f"--require-ai: ⌘K không trả lời (HTTP {code}) — đường AI không được chứng minh")
        print(f"  (⌘K không chạy — HTTP {code}; gateway/AI tắt? Đường ⌘K chỉ được chứng minh bằng TestRunReportTool_TreeAndGridWithLabels.)")


SECTIONS = {"S2b": section_s2b, "S3": section_s3, "S4": section_s4, "S5": section_s5, "S6": section_s6, "S7a": section_s7a, "S7b": section_s7b}


def main():
    ap = argparse.ArgumentParser(description="Nghiệm thu không-admin cho specs/049 báo cáo nhiều trục.")
    ap.add_argument("--base", required=True, help="vd http://localhost:8167/api/v1 (server chạy trên DB bản sao)")
    ap.add_argument("--section", choices=sorted(SECTIONS))
    ap.add_argument("--require-ai", action="store_true", help="S7a: AI tắt/không ra bản nháp ⇒ exit 2 thay vì rơi sang bản nháp dựng tay")
    ap.add_argument("--keep", action="store_true", help="giữ dữ liệu thử (in mã lượt) cho lượt bấm thật trên màn")
    ap.add_argument("--cleanup", metavar="MA_LUOT", help="dọn dữ liệu của một lượt --keep trước đó rồi thoát")
    args = ap.parse_args()
    base = args.base.rstrip("/")
    if not args.section and not args.cleanup:
        ap.error("cần --section hoặc --cleanup")

    r = Run(base, args.cleanup or "QS-" + uuid.uuid4().hex[:8])
    try:
        r.adm, _ = login(base, *ADMIN)
        if args.cleanup:
            find_leftovers(r)
            return 0
        r.staff, user = login(base, *STAFF)
        r.staff_user = user
        if user.get("isAdmin"):
            raise EnvError(f"{STAFF[0]} đang là admin — nghiệm thu bằng admin tự chấm điểm cho mình")
        r.require_ai = args.require_ai
        print(f"Mã lượt chạy: {r.tag}")
        SECTIONS[args.section](r)
    except EnvError as e:
        print(f"LỖI MÔI TRƯỜNG: {e}")
        return 2
    finally:
        if r.adm and not args.keep:
            r.cleanup()
        elif args.keep:
            print(f"(giữ dữ liệu — dọn sau: --cleanup {r.tag})")

    print(f"\n{'ĐẠT' if not fails else 'LỆCH'} — mục {args.section}: {len(fails)} kỳ vọng lệch")
    return 1 if fails else 0


def find_leftovers(r):
    """--cleanup: collect the run's reports and opportunities by tag (admin sees all), then delete."""
    code, body, _ = req(r.base, "GET", "/admin/reports", r.adm)
    for rep in ((body or {}).get("data") or {}).get("reports") or []:
        if rep.get("name", "").startswith(r.tag):
            r.reports.append(rep["id"])
    code, body, _ = req(r.base, "GET", f"/{ENTITY}?limit=100&name[contains]={r.tag}", r.adm)
    for rec in (body or {}).get("data") or []:
        r.opps.append(rec["id"])
    code, body, _ = req(r.base, "GET", f"/{ACCOUNTS}?limit=100&name[contains]={r.tag}", r.adm)
    for rec in (body or {}).get("data") or []:
        r.accounts.append(rec["id"])
    print(f"dọn {len(r.reports)} báo cáo, {len(r.opps)} cơ hội, {len(r.accounts)} khách hàng của lượt {r.tag}")


if __name__ == "__main__":
    sys.exit(main())
