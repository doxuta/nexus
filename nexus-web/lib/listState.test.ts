// lib/listState.test.ts — F7 list-amnesia helper contract:
//   1. URL round-trip: buildListSearch → parseListSearch reproduces q/sort/view/filters, defaults
//      are omitted (clean bare URLs), unknown params survive.
//   2. Precedence: explicit URL beats persisted prefs; prefs beat defaults.
//   3. localStorage prefs + last-applied view id: junk tolerance, match-guarded clear.

import { describe, it, expect, beforeEach } from "vitest";
import {
  parseListSearch,
  buildListSearch,
  hasExplicitListState,
  urlStateIsExplicit,
  resolveInitialModes,
  listPrefsKey,
  loadListPrefs,
  saveListPrefs,
  lastAppliedViewKey,
  loadLastAppliedViewId,
  saveLastAppliedViewId,
  clearLastAppliedViewId,
} from "@/lib/listState";
import type { FilterRow } from "@/lib/filterModel";

const row = (field: string, op: string, value: string): FilterRow => ({ id: "t", field, op, value });

describe("buildListSearch ↔ parseListSearch round-trip", () => {
  it("round-trips q + non-default sort + view + filters", () => {
    const search = buildListSearch({
      current: "",
      filters: [row("stage", "eq", "won"), row("phone", "notnull", "")],
      q: "acme",
      sort: { field: "amount", order: "desc" },
      defaultSort: { field: "created_at", order: "desc" },
      viewMode: "kanban",
    });
    const parsed = parseListSearch(`?${search}`);
    expect(parsed.q).toBe("acme");
    expect(parsed.sort).toEqual({ field: "amount", order: "desc" });
    expect(parsed.viewMode).toBe("kanban");
    expect(parsed.filters.map((f) => [f.field, f.op, f.value])).toEqual([
      ["stage", "eq", "won"],
      ["phone", "notnull", "1"], // null-ops encode as "1" — the filterModel dialect, unchanged
    ]);
  });

  it("omits every default: table view, empty q, sort equal to the layout default", () => {
    expect(
      buildListSearch({
        current: "",
        filters: [],
        q: "  ",
        sort: { field: "created_at", order: "desc" },
        defaultSort: { field: "created_at", order: "desc" },
        viewMode: "table",
      }),
    ).toBe("");
  });

  it("keeps sort when it differs from the default (field OR order)", () => {
    const byOrder = buildListSearch({
      current: "",
      filters: [],
      q: "",
      sort: { field: "created_at", order: "asc" },
      defaultSort: { field: "created_at", order: "desc" },
      viewMode: "table",
    });
    expect(new URLSearchParams(byOrder).get("sort")).toBe("created_at");
    expect(new URLSearchParams(byOrder).get("order")).toBe("asc");
  });

  it("preserves unknown bare params, drops stale list/fetch params from `current`", () => {
    const search = buildListSearch({
      current: "?utm_source=mail&q=old&stage[eq]=lost&page=3&view=calendar",
      filters: [],
      q: "",
      sort: null,
      defaultSort: null,
      viewMode: "table",
    });
    const sp = new URLSearchParams(search);
    expect(sp.get("utm_source")).toBe("mail"); // unknown → preserved
    expect(sp.get("q")).toBeNull();            // list-owned → re-emitted from live state (empty)
    expect(sp.get("stage[eq]")).toBeNull();    // stale filter → dropped
    expect(sp.get("page")).toBeNull();         // fetch-internal reserved → dropped
    expect(sp.get("view")).toBeNull();         // table = default → omitted
  });

  it("skips half-built filter rows (no value on a non-null op) — filterModel semantics", () => {
    const search = buildListSearch({
      current: "",
      filters: [row("stage", "eq", "  "), row("name", "contains", "an")],
      q: "",
      sort: null,
      viewMode: "table",
    });
    expect(search).toBe(new URLSearchParams({ "name[contains]": "an" }).toString());
  });

  it("parse: order defaults to asc unless explicitly desc; junk view is null", () => {
    expect(parseListSearch("?sort=name").sort).toEqual({ field: "name", order: "asc" });
    expect(parseListSearch("?sort=name&order=DESC").sort).toEqual({ field: "name", order: "desc" });
    expect(parseListSearch("?view=weird").viewMode).toBeNull();
  });
});

describe("explicitness (deep links stay authoritative)", () => {
  it.each([
    ["?q=acme", true],
    ["?view=kanban", true],
    ["?sort=amount&order=desc", true],
    ["?stage[eq]=won", true],
    ["", false],
    ["?utm_source=mail", false], // unknown params are not list state
    ["?page=2", false],          // fetch-internal, never restored → must not block auto-apply
  ])("%s → %s", (search, want) => {
    expect(hasExplicitListState(search)).toBe(want);
    expect(urlStateIsExplicit(parseListSearch(search))).toBe(want);
  });
});

describe("resolveInitialModes precedence", () => {
  it("URL view wins over prefs; density comes from prefs (no URL param)", () => {
    expect(resolveInitialModes({ viewMode: "kanban" }, { viewMode: "calendar", density: "compact" }))
      .toEqual({ viewMode: "kanban", density: "compact" });
  });
  it("prefs win over defaults when URL is silent", () => {
    expect(resolveInitialModes({ viewMode: null }, { viewMode: "calendar", density: "relaxed" }))
      .toEqual({ viewMode: "calendar", density: "relaxed" });
  });
  it("falls back to table/normal", () => {
    expect(resolveInitialModes({ viewMode: null }, {})).toEqual({ viewMode: "table", density: "normal" });
  });
});

describe("localStorage prefs", () => {
  beforeEach(() => window.localStorage.clear());

  it("save/load round-trip per entity", () => {
    saveListPrefs("accounts", { viewMode: "kanban", density: "compact" });
    expect(loadListPrefs("accounts")).toEqual({ viewMode: "kanban", density: "compact" });
    expect(loadListPrefs("contacts")).toEqual({}); // isolated per entity
  });

  it("tolerates junk and drops invalid values", () => {
    window.localStorage.setItem(listPrefsKey("accounts"), "not-json{");
    expect(loadListPrefs("accounts")).toEqual({});
    window.localStorage.setItem(listPrefsKey("accounts"), JSON.stringify({ viewMode: "grid", density: 7 }));
    expect(loadListPrefs("accounts")).toEqual({});
    window.localStorage.setItem(listPrefsKey("accounts"), JSON.stringify({ viewMode: "calendar" }));
    expect(loadListPrefs("accounts")).toEqual({ viewMode: "calendar" });
  });
});

describe("last-applied view id", () => {
  beforeEach(() => window.localStorage.clear());

  it("save/load/clear per entity", () => {
    saveLastAppliedViewId("accounts", "v1");
    expect(loadLastAppliedViewId("accounts")).toBe("v1");
    expect(loadLastAppliedViewId("contacts")).toBeNull();
    clearLastAppliedViewId("accounts");
    expect(loadLastAppliedViewId("accounts")).toBeNull();
  });

  it("match-guarded clear only removes the given id", () => {
    saveLastAppliedViewId("accounts", "v2");
    clearLastAppliedViewId("accounts", "v1"); // deleting view v1 must not forget v2
    expect(loadLastAppliedViewId("accounts")).toBe("v2");
    clearLastAppliedViewId("accounts", "v2");
    expect(loadLastAppliedViewId("accounts")).toBeNull();
  });

  it("empty stored value reads as null", () => {
    window.localStorage.setItem(lastAppliedViewKey("accounts"), "");
    expect(loadLastAppliedViewId("accounts")).toBeNull();
  });
});

// keepSort — MỘT XƯỞNG 2026-07-30. Thẻ trang chủ "Lead mới chưa xử lý" trỏ
// `?order=desc&sort=created_at&status[eq]=New`; giá trị trùng defaultSort của crm_leads nên bộ
// canonicalizer bỏ luôn, link chia sẻ còn `?status[eq]=New`.
//
// Trung thực về tầm cỡ: THỨ TỰ người dùng thấy hôm nay KHÔNG đổi (layout default và server default
// đều là created_at desc). Mất là Ý ĐỊNH đã ghi trong link — ngày ai đó sửa defaultSort của layout,
// mọi link kiểu đó âm thầm mang nghĩa khác. Đây là lý do vá, và cũng là lý do KHÔNG được viết commit
// kiểu "sửa danh sách không giữ thứ tự".
describe("buildListSearch — keepSort giữ ý định tường minh của URL", () => {
  const base = { current: "", filters: [], q: "", viewMode: "table" as const };
  const sort = { field: "created_at", order: "desc" as const };

  it("mặc định vẫn bỏ sort trùng defaultSort (URL sạch — hành vi cũ không đổi)", () => {
    const s = buildListSearch({ ...base, sort, defaultSort: sort });
    expect(s).not.toContain("sort=");
    expect(s).not.toContain("order=");
  });

  it("keepSort → giữ nguyên cả khi trùng defaultSort", () => {
    const s = new URLSearchParams(buildListSearch({ ...base, sort, defaultSort: sort, keepSort: true }));
    expect(s.get("sort")).toBe("created_at");
    expect(s.get("order")).toBe("desc");
  });

  it("sort KHÁC defaultSort thì luôn được ghi, bất kể keepSort", () => {
    const other = { field: "updated_at", order: "asc" as const };
    for (const keepSort of [false, true]) {
      const s = new URLSearchParams(buildListSearch({ ...base, sort: other, defaultSort: sort, keepSort }));
      expect(s.get("sort")).toBe("updated_at");
      expect(s.get("order")).toBe("asc");
    }
  });

  it("keepSort không tự bịa ra sort khi chưa có sort nào", () => {
    const s = buildListSearch({ ...base, sort: null, defaultSort: sort, keepSort: true });
    expect(s).not.toContain("sort=");
  });
});
