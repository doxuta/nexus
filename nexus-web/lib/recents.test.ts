import { describe, it, expect, beforeEach } from "vitest";
import {
  pushRecentRoute,
  pushRecentRecord,
  loadRecentRoutes,
  loadRecentRecords,
  pruneRecentRecords,
  filterKnownRecords,
  recordRouteVisit,
  recordRecordVisit,
  RECENT_ROUTES_CAP,
  RECENT_RECORDS_CAP,
  type RecentRoute,
  type RecentRecord,
} from "./recents";

beforeEach(() => {
  window.localStorage.clear();
});

describe("pushRecentRoute — most-recent-first, dedup, cap 5", () => {
  it("prepends the newest visit", () => {
    const l1 = pushRecentRoute([], { href: "/crm_accounts", label: "Khách hàng" }, 1);
    const l2 = pushRecentRoute(l1, { href: "/reports", label: "Báo cáo" }, 2);
    expect(l2.map((r) => r.href)).toEqual(["/reports", "/crm_accounts"]);
  });

  it("re-visiting an existing href moves it to the front (no duplicates)", () => {
    let list: RecentRoute[] = [];
    list = pushRecentRoute(list, { href: "/a", label: "A" }, 1);
    list = pushRecentRoute(list, { href: "/b", label: "B" }, 2);
    list = pushRecentRoute(list, { href: "/a", label: "A" }, 3);
    expect(list.map((r) => r.href)).toEqual(["/a", "/b"]);
    expect(list[0].at).toBe(3);
  });

  it(`caps at ${RECENT_ROUTES_CAP}`, () => {
    let list: RecentRoute[] = [];
    for (let i = 0; i < 9; i++) list = pushRecentRoute(list, { href: `/r${i}`, label: `R${i}` }, i);
    expect(list).toHaveLength(RECENT_ROUTES_CAP);
    expect(list[0].href).toBe("/r8"); // newest kept, oldest dropped
  });
});

describe("pushRecentRecord — dedup by entity+id, cap 8", () => {
  it("dedups on the (entity, id) pair, not just id", () => {
    let list: RecentRecord[] = [];
    list = pushRecentRecord(list, { entity: "a", id: "1", label: "x" }, 1);
    list = pushRecentRecord(list, { entity: "b", id: "1", label: "y" }, 2);
    expect(list).toHaveLength(2); // same id, different entity → both kept
    list = pushRecentRecord(list, { entity: "a", id: "1", label: "x2" }, 3);
    expect(list).toHaveLength(2);
    expect(list[0]).toMatchObject({ entity: "a", id: "1", label: "x2" });
  });

  it(`caps at ${RECENT_RECORDS_CAP}`, () => {
    let list: RecentRecord[] = [];
    for (let i = 0; i < 12; i++) {
      list = pushRecentRecord(list, { entity: "e", id: String(i), label: `L${i}` }, i);
    }
    expect(list).toHaveLength(RECENT_RECORDS_CAP);
    expect(list[0].id).toBe("11");
  });
});

describe("storage round-trips (anon user key — no cached /auth/me in tests)", () => {
  it("route visits persist and reload", () => {
    recordRouteVisit({ href: "/approvals", label: "Phê duyệt" });
    expect(loadRecentRoutes().map((r) => r.href)).toEqual(["/approvals"]);
  });

  it("record visits persist and reload", () => {
    recordRecordVisit({ entity: "crm_accounts", id: "abc", label: "ACME" });
    expect(loadRecentRecords()[0]).toMatchObject({ entity: "crm_accounts", id: "abc", label: "ACME" });
  });
});

describe("recents outlive what they point at", () => {
  it("drops entries whose entity is gone — a dead link is not a recent view", () => {
    const list = [
      { entity: "crm_leads", id: "a", label: "Eve", at: 3 },
      { entity: "xe_cho_thue", id: "b", label: "51K-123.45", at: 2 },
      { entity: "crm_accounts", id: "c", label: "ACME", at: 1 },
    ];
    expect(filterKnownRecords(list, ["crm_leads", "crm_accounts"]).map((r) => r.entity)).toEqual([
      "crm_leads",
      "crm_accounts",
    ]);
  });

  it("an empty entity list means NOT LOADED — it must never wipe the store", () => {
    recordRecordVisit({ entity: "crm_accounts", id: "abc", label: "ACME" });
    expect(pruneRecentRecords([])).toHaveLength(1);
    expect(loadRecentRecords()).toHaveLength(1);
  });

  it("pruning persists, so the ghost does not come back on the next render", () => {
    recordRecordVisit({ entity: "crm_accounts", id: "abc", label: "ACME" });
    recordRecordVisit({ entity: "gone_entity", id: "x", label: "Bóng ma" });
    expect(pruneRecentRecords(["crm_accounts"]).map((r) => r.entity)).toEqual(["crm_accounts"]);
    expect(loadRecentRecords().map((r) => r.entity)).toEqual(["crm_accounts"]);
  });
});

describe("recent routes outlive the routes they point at", () => {
  it("drops hrefs of routes deleted in the Studio IA clean-up", () => {
    window.localStorage.clear();
    let l = pushRecentRoute([], { href: "/studio/apps/crm", label: "a" }, 1);
    l = pushRecentRoute(l, { href: "/admin/doctor", label: "b" }, 2);
    l = pushRecentRoute(l, { href: "/admin/users", label: "c" }, 3);
    l = pushRecentRoute(l, { href: "/studio/du-an/northwind/dung/du-lieu", label: "d" }, 4);
    window.localStorage.setItem("nexus.recent-routes.v1:x", JSON.stringify(l));
    expect(loadRecentRoutes("x").map((r) => r.label).sort()).toEqual(["c", "d"]);
  });
});
