import { describe, it, expect } from "vitest";
import { pickScopedEntityIndex, pickScopedRuleId,
  entityNamesFromNav, splitByScope, scopeEntityOptions, dashboardInScope,
} from "./appScope";
import type { AppNavEntry } from "./api";

function entityItem(name: string): AppNavEntry {
  return { type: "entity", refId: `id-${name}`, label: name, href: `/${name}` };
}
function dashItem(id: string): AppNavEntry {
  return { type: "dashboard", refId: id, label: "Dash", href: `/dashboards/${id}` };
}

describe("entityNamesFromNav — app nav → entity slug set", () => {
  it("null nav (All Items / nav loading) → null = unscoped", () => {
    expect(entityNamesFromNav(null)).toBeNull();
  });

  it("collects entity slugs from href, ignores dashboard items", () => {
    const names = entityNamesFromNav([entityItem("accounts"), dashItem("d1"), entityItem("tickets")]);
    expect(names).toEqual(new Set(["accounts", "tickets"]));
  });

  it("empty nav → empty set (app with no entities scopes everything out, honestly)", () => {
    expect(entityNamesFromNav([])).toEqual(new Set());
  });
});

describe("splitByScope — soft row filter with honest hidden count", () => {
  const rows = [
    { id: 1, entityName: "accounts" },
    { id: 2, entityName: "tickets" },
    { id: 3, entityName: null },
    { id: 4, entityName: "students" },
  ];

  it("unscoped (null) → everything visible, hidden 0", () => {
    const { visible, hidden } = splitByScope(rows, null, (r) => r.entityName);
    expect(visible).toHaveLength(4);
    expect(hidden).toBe(0);
  });

  it("filters by entity ∈ scope and counts the hidden rest", () => {
    const { visible, hidden } = splitByScope(rows, new Set(["accounts"]), (r) => r.entityName);
    expect(visible.map((r) => r.id)).toEqual([1, 3]); // row 3 has no entity → stays visible
    expect(hidden).toBe(2);
  });

  it("rows without entity attribution are never hidden", () => {
    const { visible } = splitByScope(rows, new Set<string>(), (r) => r.entityName);
    expect(visible.map((r) => r.id)).toEqual([3]);
  });
});

describe("scopeEntityOptions — select options keep the stored out-of-scope value", () => {
  const ents = [{ name: "accounts" }, { name: "tickets" }, { name: "students" }];

  it("unscoped → all entities", () => {
    expect(scopeEntityOptions(ents, null)).toHaveLength(3);
  });

  it("scoped → only in-scope entities", () => {
    expect(scopeEntityOptions(ents, new Set(["tickets"])).map((e) => e.name)).toEqual(["tickets"]);
  });

  it("current out-of-scope selection stays rendered (no silent blank select)", () => {
    const opts = scopeEntityOptions(ents, new Set(["tickets"]), "students");
    expect(opts.map((e) => e.name)).toEqual(["tickets", "students"]);
  });
});

describe("dashboardInScope — any widget entity in scope", () => {
  it("unscoped → visible", () => {
    expect(dashboardInScope(["accounts"], null)).toBe(true);
  });

  it("some widget reads an in-scope entity → visible", () => {
    expect(dashboardInScope(["students", "accounts"], new Set(["accounts"]))).toBe(true);
  });

  it("no widget touches the scope → hidden", () => {
    expect(dashboardInScope(["students"], new Set(["accounts"]))).toBe(false);
  });

  it("no widgets / no entity info → visible (never hide blind)", () => {
    expect(dashboardInScope([], new Set(["accounts"]))).toBe(true);
    expect(dashboardInScope([undefined, null], new Set(["accounts"]))).toBe(true);
  });
});

// MỘT XƯỞNG — quy tắc này đã sai HAI lần theo hai cách khác nhau (xem docstring của hàm).
// Lần 2 phát hiện khi đóng vai TAY MƠ dựng app từ số 0 (Founder yêu cầu 2026-07-29): app vừa tạo,
// danh sách bảng rỗng vì bộ lọc app ẩn hết, nhưng khung phải vẫn hiện "Ứng viên · obj_candidates"
// của app KHÁC — và nút xanh "Thêm trường" khi đó ghi trường vào bảng của người khác.
describe("pickScopedEntityIndex", () => {
  const NAMES = ["candidates", "crm_accounts", "pm_projects"];

  it("app RỖNG → -1 (không chọn gì), KHÔNG rơi về bảng đầu tiên của tenant", () => {
    expect(pickScopedEntityIndex(NAMES, new Set())).toBe(-1);
  });

  it("chọn đúng bảng ĐẦU TIÊN thuộc app, không phải index 0 của tenant", () => {
    expect(pickScopedEntityIndex(NAMES, new Set(["pm_projects"]))).toBe(2);
  });

  it("không scope (xem toàn tenant) → bảng đầu tiên", () => {
    expect(pickScopedEntityIndex(NAMES, null)).toBe(0);
  });

  it("tenant chưa có bảng nào → -1 dù có scope hay không", () => {
    expect(pickScopedEntityIndex([], new Set(["x"]))).toBe(-1);
    expect(pickScopedEntityIndex([], null)).toBe(-1);
  });
});

// Queue #297: khung Tự động hoá mở luật ĐẦU TIÊN CỦA TENANT (SLA của Ticketing) dưới một app 0 luật —
// nút Xoá/Cập nhật sống trên luật của app khác. Cùng họ với pickScopedEntityIndex.
describe("pickScopedRuleId", () => {
  const TICKET = { id: "wf-ticket" };
  const CLASS_A = { id: "wf-class-a" };
  const CLASS_B = { id: "wf-class-b" };
  const ALL = [TICKET, CLASS_A, CLASS_B];

  it("app 0 luật → null, KHÔNG rơi về luật đầu tiên của tenant", () => {
    expect(pickScopedRuleId([TICKET], [], null)).toBeNull();
  });

  it("chưa mở gì → luật đầu tiên của danh sách ĐÃ LỌC", () => {
    expect(pickScopedRuleId(ALL, [CLASS_A, CLASS_B], null)).toBe("wf-class-a");
  });

  it("luật đang mở còn trong danh sách đã lọc → giữ", () => {
    expect(pickScopedRuleId(ALL, [CLASS_A, CLASS_B], "wf-class-b")).toBe("wf-class-b");
  });

  it("luật đang mở bị scope ẩn → đổi sang luật đầu tiên đã lọc, hoặc không gì", () => {
    expect(pickScopedRuleId(ALL, [CLASS_A, CLASS_B], "wf-ticket")).toBe("wf-class-a");
    expect(pickScopedRuleId(ALL, [], "wf-ticket")).toBeNull();
  });

  it("luật chưa có trong danh sách (vừa tạo, chưa tải lại) → giữ", () => {
    expect(pickScopedRuleId(ALL, [CLASS_A], "wf-new")).toBe("wf-new");
  });
});
