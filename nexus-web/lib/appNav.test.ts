import { describe, it, expect, vi, beforeEach } from "vitest";

const adminApp = vi.fn();
const replaceAppNav = vi.fn();
vi.mock("@/lib/api", () => ({
  adminApp: (...a: unknown[]) => adminApp(...a),
  replaceAppNav: (...a: unknown[]) => replaceAppNav(...a),
}));

import { readFileSync } from "node:fs";
import { addEntityToAppNav, mergeAppNavKeys } from "@/lib/appNav";

// Shared arbiter with Go mergeAppNav (internal/ai/app_nav_merge_test.go).
const CASES = JSON.parse(
  readFileSync("../nexus-platform/internal/ai/testdata/app_nav_merge_cases.json", "utf8"),
) as Array<{
  name: string;
  current: Array<{ itemType: "entity" | "dashboard"; key: string; label?: string }>;
  add: string[];
  want: Array<{ itemType: "entity" | "dashboard"; key: string; label?: string }>;
}>;
const toItem = (i: { itemType: "entity" | "dashboard"; key: string; label?: string }) => ({
  itemType: i.itemType, refId: i.key, ...(i.label ? { label: i.label } : {}),
});

describe("mergeAppNavKeys (shared cases)", () => {
  for (const c of CASES) {
    it(c.name, () => {
      const add = c.add.map((k) => ({ itemType: "entity" as const, refId: k }));
      expect(mergeAppNavKeys(c.current.map(toItem), add)).toEqual(c.want.map(toItem));
    });
  }
});

const items = [
  { id: "n1", itemType: "entity", refId: "e-patients", label: "Hồ sơ", sortOrder: 0 },
  { id: "n2", itemType: "dashboard", refId: "d-kpi", label: null, sortOrder: 1 },
];

describe("addEntityToAppNav", () => {
  beforeEach(() => {
    adminApp.mockReset();
    replaceAppNav.mockReset();
  });

  it("keeps every row (table, dashboard, custom label) and appends the new table last", async () => {
    adminApp.mockResolvedValue({ items });
    await addEntityToAppNav("app-1", "e-new");
    expect(replaceAppNav).toHaveBeenCalledWith("app-1", [
      { itemType: "entity", refId: "e-patients", label: "Hồ sơ" },
      { itemType: "dashboard", refId: "d-kpi" },
      { itemType: "entity", refId: "e-new" },
    ]);
  });

  it("does not PUT when the table is already in the menu", async () => {
    adminApp.mockResolvedValue({ items });
    await addEntityToAppNav("app-1", "e-patients");
    expect(replaceAppNav).not.toHaveBeenCalled();
  });

  it("surfaces a failed PUT", async () => {
    adminApp.mockResolvedValue({ items });
    replaceAppNav.mockRejectedValue(new Error("boom"));
    await expect(addEntityToAppNav("app-1", "e-new")).rejects.toThrow("boom");
  });
});
