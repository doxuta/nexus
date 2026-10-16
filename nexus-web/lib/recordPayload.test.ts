import { describe, it, expect } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { buildPayload, reverseValue, seedValues, emptyDefault, cloneRecordData, toWireDateTime } from "./recordPayload";

// Field factory.
const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({
  name, fieldType, ...extra,
});

describe("emptyDefault", () => {
  it("gives type-appropriate empties (no undefined for controlled widgets)", () => {
    expect(emptyDefault(f("a", "boolean"))).toBe(false);
    expect(emptyDefault(f("a", "number"))).toBe("");
    expect(emptyDefault(f("a", "rating"))).toBe(0);
    expect(emptyDefault(f("a", "multi_select"))).toEqual([]);
    expect(emptyDefault(f("a", "currency"))).toBe("");
    expect(emptyDefault(f("a", "composite"))).toEqual({});
    expect(emptyDefault(f("a", "text"))).toBe("");
  });
});

describe("reverseValue", () => {
  it("currency {amount,currencyCode} → decimal string", () => {
    expect(reverseValue(f("c", "currency"), { amount: 2100, currencyCode: "VND" })).toBe("2100");
  });
  it("currency plain number → string", () => {
    expect(reverseValue(f("c", "currency"), 500)).toBe("500");
  });
  it("number passes through as number", () => {
    expect(reverseValue(f("n", "number"), 42)).toBe(42);
  });
  it("relation {id,label} → id; bare string → string", () => {
    expect(reverseValue(f("r", "relation"), { id: "abc", label: "Acme" })).toBe("abc");
    expect(reverseValue(f("r", "relation"), "abc")).toBe("abc");
  });
  it("null → emptyDefault (flip guard)", () => {
    expect(reverseValue(f("b", "boolean"), null)).toBe(false);
    expect(reverseValue(f("m", "multi_select"), undefined)).toEqual([]);
  });
  it("formula plain string passes through", () => {
    expect(reverseValue(f("fx", "formula"), "already text")).toBe("already text");
  });
  it("formula currency-resultType {amount,currencyCode} → formatted money, NOT '[object Object]' (W5)", () => {
    expect(reverseValue(f("fx", "formula"), { amount: 8000, currencyCode: "USD" })).not.toMatch(/object Object/);
    expect(reverseValue(f("fx", "formula"), { amount: 8000, currencyCode: "USD" })).toContain("8.000");
    expect(reverseValue(f("fx", "formula"), { amount: 3500000000, currencyCode: "VND" })).toBe("3.500.000.000 VNĐ");
  });
});

describe("seedValues", () => {
  const fields = [f("name", "text"), f("revenue", "number"), f("active", "boolean")];
  it("create mode → empty defaults keyed by name", () => {
    expect(seedValues(fields)).toEqual({ name: "", revenue: "", active: false });
  });
  it("edit mode → reversed record values", () => {
    expect(seedValues(fields, { name: "Acme", revenue: 100, active: true })).toEqual({
      name: "Acme", revenue: 100, active: true,
    });
  });
});

describe("seedValues — mặc định khi tạo (#275)", () => {
  const fields = [
    f("trang_thai", "picklist", { isRequired: true, defaultValue: "Chờ xác nhận", options: { values: ["Chờ xác nhận", "Xong"] } }),
    f("dang_ap_dung", "boolean", { defaultValue: "true" }),
    f("si_so", "number", { isRequired: true, defaultValue: "15" }),
    f("diem", "rating", { defaultValue: "3" }),
    f("nhan", "multi_select", { defaultValue: '["a","b"]' }),
    f("ghi_chu", "text"),
    f("hong", "number", { defaultValue: "abc" }),
  ];
  it("create: điền mặc định theo kiểu; không có mặc định → rỗng; mặc định hỏng → rỗng", () => {
    expect(seedValues(fields, undefined, true)).toEqual({
      trang_thai: "Chờ xác nhận", dang_ap_dung: true, si_so: 15, diem: 3, nhan: ["a", "b"], ghi_chu: "", hong: "",
    });
  });
  it("lưu ngay không chạm gì ⇒ payload mang đúng mặc định", () => {
    const out = buildPayload(fields, seedValues(fields, undefined, true));
    expect(out).toMatchObject({ trang_thai: "Chờ xác nhận", dang_ap_dung: true, si_so: 15 });
  });
  it("boolean mặc định Tắt/'false' vẫn là false", () => {
    expect(seedValues([f("b", "boolean", { defaultValue: "false" })], undefined, true)).toEqual({ b: false });
  });
  it("create có prefill: khoá thiếu lấy mặc định, khoá có giữ giá trị prefill", () => {
    expect(seedValues(fields, { si_so: 20 }, true).si_so).toBe(20);
    expect(seedValues(fields, { si_so: 20 }, true).trang_thai).toBe("Chờ xác nhận");
  });
  it("không bật cờ (sửa hồ sơ / mặc định cũ): vẫn rỗng", () => {
    expect(seedValues(fields).trang_thai).toBe("");
    expect(seedValues(fields, { ghi_chu: "x" }).si_so).toBe("");
  });
});

describe("buildPayload", () => {
  it("date/datetime: wire values pass through; legacy display DMY converts at the boundary", () => {
    const fields = [f("ngay", "date"), f("hen", "datetime")];
    // Wire (what DateInput emits since the 2026-07-15 fix) — untouched.
    expect(buildPayload(fields, { ngay: "2026-08-01", hen: "2026-08-01T09:30:00+07:00" }))
      .toEqual({ ngay: "2026-08-01", hen: "2026-08-01T09:30:00+07:00" });
    // Legacy display form from an old-build draft — converted, never 422s the save.
    expect(buildPayload(fields, { ngay: "01/08/2026", hen: "01/08/2026 \u00b7 09:30" }))
      .toEqual({ ngay: "2026-08-01", hen: "2026-08-01T09:30:00+07:00" }); // Vietnam clock (#278)
    // Optional empties drop.
    expect(buildPayload(fields, { ngay: "", hen: "" })).toEqual({});
  });

  it("drops empty optionals, keeps required + 0/false", () => {
    const fields = [
      f("name", "text", { isRequired: true }),
      f("phone", "text"),
      f("revenue", "number"),
    ];
    const out = buildPayload(fields, { name: "Acme", phone: "", revenue: "" });
    expect(out).toEqual({ name: "Acme" }); // phone dropped (optional empty), revenue dropped
  });

  it("coerces number string → number; required empty number is NOT sent as 0 (#275)", () => {
    const fields = [f("revenue", "number"), f("score", "number", { isRequired: true })];
    const out = buildPayload(fields, { revenue: "2100", score: "" });
    // the server applies the field default / answers RECORD_REQUIRED_MISSING — never a silent 0
    expect(out).toEqual({ revenue: 2100 });
  });

  it("currency string → {amount, currencyCode} with default VND", () => {
    const fields = [f("deal", "currency", { isRequired: true })];
    expect(buildPayload(fields, { deal: "999" })).toEqual({
      deal: { amount: 999, currencyCode: "VND" },
    });
  });

  it("currency honours options.currencyCode", () => {
    const fields = [f("deal", "currency", { isRequired: true, options: { currencyCode: "USD" } })];
    expect(buildPayload(fields, { deal: "10" })).toEqual({
      deal: { amount: 10, currencyCode: "USD" },
    });
  });

  it("drops empty optional currency", () => {
    const fields = [f("deal", "currency")];
    expect(buildPayload(fields, { deal: "" })).toEqual({});
  });

  it("keeps boolean false and empty multi_select only when required", () => {
    const fields = [f("active", "boolean"), f("tags", "multi_select"), f("must", "multi_select", { isRequired: true })];
    const out = buildPayload(fields, { active: false, tags: [], must: [] });
    expect(out).toEqual({ active: false, must: [] }); // tags (optional empty array) dropped
  });

  it("polymorphic_relation sends {type,id} only when BOTH present; never a bare {type}", () => {
    const fields = [f("related_to", "polymorphic_relation")];
    // Complete → sent verbatim (stripped to exactly type+id).
    expect(buildPayload(fields, { related_to: { type: "crm_accounts", id: "a1" } }))
      .toEqual({ related_to: { type: "crm_accounts", id: "a1" } });
    // Bare {type} (no id) → dropped, NOT sent (would 422 on the server).
    expect(buildPayload(fields, { related_to: { type: "crm_accounts" } })).toEqual({});
    // Unset (null / empty) → dropped.
    expect(buildPayload(fields, { related_to: null })).toEqual({});
    expect(buildPayload(fields, { related_to: "" })).toEqual({});
  });

  it("polymorphic_relation strips stray keys to exactly {type,id}", () => {
    const fields = [f("related_to", "polymorphic_relation")];
    expect(buildPayload(fields, { related_to: { type: "crm_contacts", id: "c9", label: "hack" } }))
      .toEqual({ related_to: { type: "crm_contacts", id: "c9" } });
  });
});

describe("polymorphic_relation seed/reverse", () => {
  it("emptyDefault is null (picker reads unset until both set)", () => {
    expect(emptyDefault(f("r", "polymorphic_relation"))).toBeNull();
  });
  it("reverseValue keeps a stored {type,id} verbatim; null-ish → null", () => {
    expect(reverseValue(f("r", "polymorphic_relation"), { type: "crm_accounts", id: "a1" }))
      .toEqual({ type: "crm_accounts", id: "a1" });
    expect(reverseValue(f("r", "polymorphic_relation"), null)).toBeNull();
    expect(reverseValue(f("r", "polymorphic_relation"), "garbage")).toBeNull();
  });
});

describe("cloneRecordData (?cloneFrom=<id>, §7.1)", () => {
  const fields = [
    f("name", "text"),
    f("revenue", "number"),
    f("account", "relation", { options: { relatedEntity: "crm_accounts" } }),
    f("invoice_no", "auto_number"),                 // server-assigned → must NOT carry
    f("total", "formula"),                          // computed → must NOT carry
    f("owner_email", "lookup"),                     // computed → must NOT carry
    f("opp_count", "rollup"),                       // computed → must NOT carry
  ];

  it("copies user-entered fields verbatim", () => {
    const out = cloneRecordData(fields, { name: "Acme", revenue: 100, account: "acc-1" });
    expect(out).toEqual({ name: "Acme", revenue: 100, account: "acc-1" });
  });

  it("strips system / audit columns", () => {
    const out = cloneRecordData(fields, {
      id: "rec-1", tenant_id: "t-1", created_by: "u-1", created_at: "2026-01-01",
      updated_at: "2026-02-02", team_set_id: "ts-1", name: "Acme",
    });
    expect(out).toEqual({ name: "Acme" });
    expect(out).not.toHaveProperty("id");
    expect(out).not.toHaveProperty("created_by");
  });

  it("strips server-computed / server-assigned field types (formula/lookup/rollup/auto_number)", () => {
    const out = cloneRecordData(fields, {
      name: "Acme", invoice_no: "INV-00042", total: { amount: 9, currencyCode: "USD" },
      owner_email: "x@y.z", opp_count: 7,
    });
    expect(out).toEqual({ name: "Acme" });
  });

  it("drops keys not in the entity's field set", () => {
    const out = cloneRecordData(fields, { name: "Acme", ghost_field: "leftover" });
    expect(out).toEqual({ name: "Acme" }); // unknown key dropped — a clone carries only real user fields
  });

  it("undefined source → empty object", () => {
    expect(cloneRecordData(fields, undefined)).toEqual({});
  });
});

// ── MỘT XƯỞNG 2026-07-29, hai lỗi tìm được khi dùng thật ─────────────────────────────────────
//
// (1) Server ĐỌC và GHI hai phương ngữ khác nhau cho cùng một field: `GET` trả
//     "2026-07-14 09:00:00" còn `PUT` đòi RFC3339 và **từ chối chính đầu ra của mình** bằng
//     422 RECORD_INVALID_DATE. Tái hiện trên trình duyệt: mở BẤT KỲ ticket nào → Sửa → "Lưu thay
//     đổi" mà KHÔNG động vào gì → 422. Mọi bản ghi có field datetime đều không lưu nổi trừ khi
//     người dùng tình cờ chọn lại ngày.
//
// (2) `buildPayload` bỏ hẳn key khi field tuỳ chọn rỗng. Đúng cho POST, SAI cho PUT (partial
//     update: thiếu key = "đừng đụng"). Người dùng ĐỔI được giá trị nhưng KHÔNG BAO GIỜ gỡ được —
//     xoá trắng ô rồi Lưu, báo thành công, dữ liệu cũ còn nguyên. Đây là họ accept-then-swallow.
describe("toWireDateTime — vá chỗ server không đọc nổi chữ của chính nó", () => {
  // #278: the server's space-form echo is UTC (D1) — it must keep saying so on the way back, else
  // the same instant is re-read as a different wall clock.
  it("đổi phương ngữ có dấu cách (UTC) sang RFC3339 có Z", () => {
    expect(toWireDateTime("2026-07-14 09:00:00")).toBe("2026-07-14T09:00:00Z");
    expect(toWireDateTime("2026-07-14 09:00")).toBe("2026-07-14T09:00:00Z");
  });

  it("giá trị không múi giờ (draft cũ) là giờ Việt Nam, gắn +07:00", () => {
    expect(toWireDateTime("2026-07-14T09:00:00")).toBe("2026-07-14T09:00:00+07:00");
    expect(toWireDateTime("2026-07-14T09:00")).toBe("2026-07-14T09:00:00+07:00");
  });

  it("KHÔNG đụng vào giá trị vốn đã hợp lệ (không được viết lại múi giờ)", () => {
    for (const v of ["2026-07-14T09:00:00Z", "2026-07-14T09:00:00+07:00", "2026-07-14", ""]) {
      expect(toWireDateTime(v)).toBe(v);
    }
  });

  it("reverseValue chuẩn hoá ngay lúc giá trị VÀO form, nên draft/clone cũng sạch", () => {
    expect(reverseValue(f("due_date", "datetime"), "2026-07-14 09:00:00")).toBe("2026-07-14T09:00:00Z");
    expect(reverseValue(f("d", "date"), "2026-07-14")).toBe("2026-07-14");
  });

  it("buildPayload chặn lần hai: giá trị lọt thẳng từ server vẫn ra đúng dạng ghi", () => {
    const out = buildPayload([f("due_date", "datetime")], { due_date: "2026-07-14 09:00:00" });
    expect(out.due_date).toBe("2026-07-14T09:00:00Z");
  });
});

describe("buildPayload — chế độ update phải XOÁ TRỐNG được", () => {
  const fields = [
    f("company", "text"),
    f("source", "picklist"),
    f("account", "relation"),
    f("close_date", "date"),
    f("amount", "currency", { options: { currencyCode: "VND" } }),
    f("probability", "number"),
    f("tags", "multi_select"),
    f("assigned_to", "user"),
    f("related_to", "polymorphic_relation"),
  ];
  const empties = {
    company: "", source: "", account: "", close_date: "", amount: "",
    probability: "", tags: [], assigned_to: "", related_to: null,
  };

  it('mặc định ("create") vẫn BỎ key — MassEditModal dựa vào đúng điều này để khoá nút gửi', () => {
    expect(buildPayload(fields, empties)).toEqual({});
  });

  it('"update" gửi null tường minh cho MỌI kiểu — null là lệnh xoá của JSON_MERGE_PATCH', () => {
    const out = buildPayload(fields, empties, "update");
    for (const k of Object.keys(empties)) {
      expect(out, `thiếu khoá ${k}`).toHaveProperty(k);
      expect(out[k], `${k} phải là null`).toBeNull();
    }
  });

  it("field BẮT BUỘC không bị biến thành null (để validation nói đúng chỗ sai)", () => {
    const req = [f("subject", "text", { isRequired: true })];
    expect(buildPayload(req, { subject: "" }, "update")).toEqual({ subject: "" });
  });

  it("giá trị THẬT vẫn đi qua nguyên vẹn ở cả hai chế độ; false và 0 KHÔNG phải là rỗng", () => {
    const fs = [f("active", "boolean"), f("score", "number"), f("note", "text")];
    const vals = { active: false, score: 0, note: "x" };
    for (const mode of ["create", "update"] as const) {
      expect(buildPayload(fs, vals, mode)).toEqual({ active: false, score: 0, note: "x" });
    }
  });
});

// specs/060 (#270): file/image values are [{id,name}] refs owned by the file API (the record's "Tệp"
// tab); the form only shows a static note. Echoing them back sent "[object Object]" for file (422
// "phải là mảng tham chiếu file") and null for image (the save silently dropped the photo refs).
describe("file/image — tab Tệp quản lý, form KHÔNG BAO GIỜ ghi (specs/060 #270)", () => {
  const refs = [{ id: "f1", name: "anh-nghiem-thu.png" }];
  const fs = [
    f("tieu_de", "text"),
    f("chu_ky_khach", "file"),
    f("anh", "image"),
    f("anh_bb", "image", { isRequired: true }),
  ];
  const record = { tieu_de: "Phiếu 1", chu_ky_khach: refs, anh: refs, anh_bb: refs };

  it("form Sửa hồ sơ đã có ảnh + chữ ký: payload không mang khoá file/image nào", () => {
    const out = buildPayload(fs, { ...seedValues(fs, record), tieu_de: "Đã xong" }, "update");
    expect(out).toEqual({ tieu_de: "Đã xong" });
  });

  it("nhân bản / tạo từ hồ sơ có tệp: không mang tham chiếu tệp của hồ sơ nguồn", () => {
    expect(buildPayload(fs, seedValues(fs, cloneRecordData(fs, record)))).toEqual({ tieu_de: "Phiếu 1" });
  });

  it("reverseValue giữ nguyên mảng tham chiếu, không ép thành chuỗi", () => {
    expect(reverseValue(f("x", "file"), refs)).toEqual(refs);
    expect(reverseValue(f("x", "image"), refs)).toEqual(refs);
  });
});

describe("required number left empty (#275)", () => {
  const req = { name: "qty", fieldType: "number", isRequired: true } as unknown as FieldMeta;
  it("update: omitted key means keep — so layoutSchema must be the one that rejects (see layoutSchema.test)", () => {
    expect(buildPayload([req], { qty: "" }, "update")).toEqual({});
  });
});

describe("seedValues — default của picklist con (#255)", () => {
  const dep = (name: string, parent: string, map: Record<string, string[]>, values: string[], def: string) =>
    f(name, "picklist", { defaultValue: def, options: { values, controllingField: parent, dependentValues: map } });
  const tinh = f("tinh", "picklist", { options: { values: ["hn", "hcm"] } });
  const huyen = dep("huyen", "tinh", { hn: ["badinh"], hcm: ["q1"] }, ["badinh", "q1"], "q1");

  it("cha trống: default con bị bỏ", () => {
    expect(seedValues([tinh, huyen], undefined, true).huyen).toBe("");
  });
  it("cha có default không cho phép default con: bỏ; cho phép: giữ", () => {
    expect(seedValues([{ ...tinh, defaultValue: "hn" }, huyen], undefined, true).huyen).toBe("");
    expect(seedValues([{ ...tinh, defaultValue: "hcm" }, huyen], undefined, true).huyen).toBe("q1");
  });
  it("chuỗi cha-con chỉ bỏ tới khi ổn định, bất kể thứ tự field", () => {
    const c = f("c", "picklist", { defaultValue: "y", options: { values: ["x", "y"] } });
    const b = dep("b", "c", { x: ["b1"] }, ["b1"], "b1");
    const a = dep("a", "b", { b1: ["a1"] }, ["a1"], "a1");
    const s = seedValues([c, a, b], undefined, true);
    expect(s.b).toBe("");
    expect(s.a).toBe("");
  });
  it("sửa bản ghi (không applyDefaults) hoặc giá trị có sẵn: không đụng", () => {
    expect(seedValues([tinh, huyen], { huyen: "q1" }, true).huyen).toBe("q1");
    expect(seedValues([tinh, huyen], { tinh: "hn", huyen: "q1" }).huyen).toBe("q1");
  });
});
