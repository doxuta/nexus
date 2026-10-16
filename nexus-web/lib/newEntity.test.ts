// lib/newEntity (specs/074 T012): slug + "create a table inside an app" ported from DataModelBuilder.
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary, EntityMeta } from "@/lib/api";

const createEntityWithAcl = vi.fn();
const adminApp = vi.fn();
const replaceAppNav = vi.fn();
const listEntities = vi.fn();
const emitDraftsChanged = vi.fn();
vi.mock("@/lib/draftEvents", () => ({ emitDraftsChanged: (...a: unknown[]) => emitDraftsChanged(...a) }));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  createEntityWithAcl: (...a: unknown[]) => createEntityWithAcl(...a),
  adminApp: (...a: unknown[]) => adminApp(...a),
  replaceAppNav: (...a: unknown[]) => replaceAppNav(...a),
  listEntities: (...a: unknown[]) => listEntities(...a),
}));

import { ApiError } from "@/lib/api";
import { createEntityInApp, createErrorField, slugify } from "./newEntity";

const app: AppSummary = { id: "app-crm", name: "crm", label: "CRM", icon: null, color: null, isDefault: false, sortOrder: 0 };
const made: EntityMeta = { id: "e9", name: "don_thuoc", label: "Đơn thuốc", tableName: "obj_don_thuoc" };
const seeded = { appScoped: true, seeded: true };

describe("slugify", () => {
  it.each([
    ["Đơn thuốc", "don_thuoc"],
    ["  Khách hàng  VIP ", "khach_hang_vip"],
    ["Hoá-đơn/2024", "hoa_don_2024"],
  ])("%s -> %s", (input, out) => expect(slugify(input)).toBe(out));

  it("starts with a letter: a leading digit gets a prefix, empty stays empty", () => {
    expect(slugify("2024 Kế hoạch")).toBe("b_2024_ke_hoach");
    expect(slugify("!!!")).toBe("");
  });
});

describe("createEntityInApp", () => {
  beforeEach(() => {
    for (const m of [createEntityWithAcl, adminApp, replaceAppNav, listEntities]) m.mockReset();
    createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: seeded });
    adminApp.mockResolvedValue({ items: [{ itemType: "entity", refId: "e1", label: "Khách" }] });
    replaceAppNav.mockResolvedValue(undefined);
  });

  it("with an app: sends appId, reads the menu, then replaces it keeping every existing row", async () => {
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(createEntityWithAcl).toHaveBeenCalledWith("don_thuoc", "Đơn thuốc", "", [], "app-crm");
    expect(adminApp).toHaveBeenCalledWith("app-crm");
    expect(replaceAppNav).toHaveBeenCalledWith("app-crm", [
      { itemType: "entity", refId: "e1", label: "Khách" },
      { itemType: "entity", refId: "e9" },
    ]);
    expect(r).toMatchObject({ entity: made, addedToNav: true, navError: null, aclSeed: seeded });
  });

  it("a new table is a pending change on a delivered workspace: the header recounts once it exists", async () => {
    emitDraftsChanged.mockReset();
    await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(emitDraftsChanged).toHaveBeenCalledWith("entity-create");
    emitDraftsChanged.mockReset();
    createEntityWithAcl.mockRejectedValue(new Error("down"));
    await expect(createEntityInApp({ name: "x", label: "X", app: null })).rejects.toThrow("down");
    expect(emitDraftsChanged).not.toHaveBeenCalled();
  });

  it("a table already in the menu is not PUT again", async () => {
    adminApp.mockResolvedValue({ items: [{ itemType: "entity", refId: "e9" }] });
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(replaceAppNav).not.toHaveBeenCalled();
    expect(r.addedToNav).toBe(true);
  });

  it("without an app: no appId, no menu calls, and the seed signal is still returned", async () => {
    createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: { appScoped: false, seeded: false } });
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app: null });
    expect(createEntityWithAcl).toHaveBeenCalledWith("don_thuoc", "Đơn thuốc", "", [], undefined);
    expect(adminApp).not.toHaveBeenCalled();
    expect(r).toMatchObject({ addedToNav: false, navError: null, aclSeed: { appScoped: false, seeded: false } });
  });

  it("aclSeed.warning is passed through, never swallowed", async () => {
    createEntityWithAcl.mockResolvedValue({ entity: made, aclSeed: { appScoped: true, seeded: false, warning: "chưa cấp được" } });
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(r.aclSeed.warning).toBe("chưa cấp được");
  });

  it("the table exists but the menu write fails: no throw, navError says so (the table must not be lost)", async () => {
    replaceAppNav.mockRejectedValue(new Error("nav down"));
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(r.entity).toEqual(made);
    expect(r.addedToNav).toBe(false);
    expect(r.navError).toBe("nav down");
  });

  it("resolves the id from the entity list when the create reply has none", async () => {
    createEntityWithAcl.mockResolvedValue({ entity: { ...made, id: undefined }, aclSeed: seeded });
    listEntities.mockResolvedValue([made]);
    await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    expect(replaceAppNav).toHaveBeenCalledWith("app-crm", expect.arrayContaining([{ itemType: "entity", refId: "e9" }]));
  });

  it("undo removes the table from the menu only: the table is not deleted", async () => {
    const r = await createEntityInApp({ name: "don_thuoc", label: "Đơn thuốc", app });
    adminApp.mockResolvedValue({ items: [{ itemType: "entity", refId: "e1" }, { itemType: "entity", refId: "e9" }] });
    replaceAppNav.mockClear();
    await r.undo();
    expect(replaceAppNav).toHaveBeenCalledWith("app-crm", [{ itemType: "entity", refId: "e1" }]);
  });

  it("a failed create throws the API error unchanged", async () => {
    createEntityWithAcl.mockRejectedValue(new ApiError(409, "ENTITY_EXISTS", "đã có"));
    await expect(createEntityInApp({ name: "x", label: "X", app })).rejects.toMatchObject({ status: 409 });
  });
});

describe("createErrorField: which box an error belongs to", () => {
  it("409 = the code is taken", () => expect(createErrorField(new ApiError(409, "ENTITY_EXISTS", "m"))).toBe("name"));
  it("422 with details.field names that box", () => {
    expect(createErrorField(new ApiError(422, "VALIDATION", "m", { field: "label" }))).toBe("label");
    expect(createErrorField(new ApiError(422, "VALIDATION", "m", { field: "name" }))).toBe("name");
  });
  it("anything else belongs to the form", () => {
    expect(createErrorField(new ApiError(422, "VALIDATION", "m"))).toBeNull();
    expect(createErrorField(new Error("x"))).toBeNull();
  });
});
