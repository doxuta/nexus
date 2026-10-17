// applyProposal — the 14 proposal kinds (specs/076 ai1). The UI-level behaviour is pinned by
// components/studio/assistant/*.test.tsx; this file pins the module contract directly.
import { describe, it, expect, vi, beforeEach } from "vitest";
import * as api from "@/lib/api";
import { applyProposal, proposalKey, needsLiveConfirm } from "./aiApply";
import type { AiDraftItem } from "./aiBuilder";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  const fns = [
    "createEntityWithAcl", "addField", "createRelationship", "createLayout", "createWorkflow", "updateWorkflow",
    "listWorkflows", "createReport", "listReports", "updateReport", "createApp", "replaceAppNav", "appAclBackfill",
    "adminApps", "adminApp", "listEntities", "listFields", "updateField", "deleteField", "deleteEntity",
    "createRecordsBulk", "createSampleRecords", "createApprovalProcess", "createDashboard", "createWorkQueue",
    "listRoles", "listUserDirectory", "getRole", "updateRolePermissions",
  ];
  return { ...actual, ...Object.fromEntries(fns.map((f) => [f, vi.fn()])) };
});

const item = (x: unknown) => x as AiDraftItem;
const NONE = { appScoped: false, seeded: false };
const DEPS = { asSample: true };

const ENTITY = item({ kind: "entity", entity: { name: "benh_nhan", label: "Bệnh nhân", fields: [{ name: "name", label: "Họ tên", fieldType: "text", required: true }] } });
const FIELD = item({ kind: "field", field: { entity: "benh_nhan", name: "ngay", label: "Ngày", fieldType: "date" } });
const REL = item({ kind: "relationship", relationship: { name: "hv_kh", entityA: "hoc_vien", entityB: "khoa_hoc" } });
const LAYOUT = item({ kind: "layout", layout: { entity: "crm_contacts", viewType: "edit", layoutJson: { version: 1, sections: [] } } });
const WORKFLOW = item({ kind: "workflow", workflow: { name: "Đóng cơ hội", entityName: "crm_opportunities", trigger: "after_update", actions: [], isActive: true } });
const REPORT = item({ kind: "report", report: { name: "Theo giai đoạn", entityName: "crm_opportunities", type: "summary", config: { groupBy: "stage", metric: "count" }, isActive: true } });
const APP = item({ kind: "app", app: { name: "phong_kham", label: "Phòng khám", icon: "stethoscope", navEntities: ["benh_nhan"] } });
const EDIT = item({ kind: "edit_field", editField: { entity: "benh_nhan", field: "phone", before: { label: "SĐT" }, after: { label: "Số điện thoại" } } });
const DEL_FIELD = item({ kind: "delete_field", deleteField: { entity: "benh_nhan", field: "fax", label: "Fax", fieldType: "text" } });
const DEL_ENTITY = item({ kind: "delete_entity", deleteEntity: { entity: "khuyen_mai", label: "Khuyến mãi", fieldCount: 7 } });
const ROWS = [{ name: "Anh" }, { name: "Bình" }, { name: "Chi" }];
const RECORDS = item({ kind: "records", records: { entity: "benh_nhan", rows: ROWS } });
const APPROVAL = item({ kind: "approval", approval: { name: "Duyệt", entityName: "crm_opportunities", steps: [{ approverType: "role", approverRef: "director", slaSeconds: 60 }] } });
const DASHBOARD = item({ kind: "dashboard", dashboard: { name: "Tổng quan", widgets: [{ id: "w1", title: "KPI", type: "kpi", entity: "x", metric: "count" }] } });
const WORKQUEUE = item({ kind: "workqueue", workqueue: { name: "q", label: "Q", entityName: "crm_contracts", filters: [] } });

const bulk = (created: number, failed = 0) => ({
  mode: "partial" as const, created, failed,
  results: [
    ...Array.from({ length: created }, (_, i) => ({ index: i, status: "created" as const, id: `r${i}` })),
    ...Array.from({ length: failed }, (_, i) => ({ index: created + i, status: "failed" as const, code: "X", error: "lỗi dòng" })),
  ],
});

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(api.createEntityWithAcl).mockResolvedValue({ entity: { id: "e1", name: "benh_nhan" }, aclSeed: NONE } as never);
  vi.mocked(api.createApp).mockResolvedValue({ id: "app-1" } as never);
  vi.mocked(api.listEntities).mockResolvedValue([{ id: "e1", name: "benh_nhan" }] as never);
  vi.mocked(api.appAclBackfill).mockResolvedValue({ failed: [] } as never);
  vi.mocked(api.listFields).mockResolvedValue([{ id: "f1", name: "phone" }, { id: "f2", name: "fax" }] as never);
  vi.mocked(api.listRoles).mockResolvedValue([{ id: "r1", name: "director" }] as never);
  vi.mocked(api.listUserDirectory).mockResolvedValue([]);
  vi.mocked(api.createDashboard).mockResolvedValue({ id: "d1" } as never);
  vi.mocked(api.createSampleRecords).mockResolvedValue(bulk(3));
  vi.mocked(api.createRecordsBulk).mockResolvedValue(bulk(3));
});

describe("applyProposal — success per kind", () => {
  const cases: [string, AiDraftItem, () => unknown][] = [
    ["entity", ENTITY, () => api.createEntityWithAcl],
    ["field", FIELD, () => api.addField],
    ["relationship", REL, () => api.createRelationship],
    ["layout", LAYOUT, () => api.createLayout],
    ["workflow", WORKFLOW, () => api.createWorkflow],
    ["report", REPORT, () => api.createReport],
    ["app", APP, () => api.createApp],
    ["edit_field", EDIT, () => api.updateField],
    ["delete_field", DEL_FIELD, () => api.deleteField],
    ["delete_entity", DEL_ENTITY, () => api.deleteEntity],
    ["records", RECORDS, () => api.createSampleRecords],
    ["approval", APPROVAL, () => api.createApprovalProcess],
    ["dashboard", DASHBOARD, () => api.createDashboard],
    ["workqueue", WORKQUEUE, () => api.createWorkQueue],
  ];
  it.each(cases)("%s calls its gated admin API once and lands as done", async (_k, it_, fn) => {
    const out = await applyProposal(it_, DEPS);
    expect(fn()).toHaveBeenCalledTimes(1);
    expect(out.status).toBe("done");
  });

  it("dashboard returns the created id for the footer deep-link", async () => {
    expect((await applyProposal(DASHBOARD, DEPS)).createdId).toBe("d1");
  });
});

describe("applyProposal — errors", () => {
  it.each([[400], [409], [422]])("a %i is an error the model may read back (reportable)", async (status) => {
    vi.mocked(api.createRelationship).mockRejectedValue(new api.ApiError(status, "REL_BAD", "quan hệ sai"));
    const out = await applyProposal(REL, DEPS);
    expect(out).toMatchObject({ status: "error", reportable: true });
    expect(out.message).toContain("quan hệ sai");
  });

  it.each([[403], [500]])("a %i is shown on the card but NOT reported to the model", async (status) => {
    vi.mocked(api.createLayout).mockRejectedValue(new api.ApiError(status, "X", "không được"));
    expect(await applyProposal(LAYOUT, DEPS)).toMatchObject({ status: "error", reportable: false });
  });

  it("a network failure (not ApiError) is an error and not reportable", async () => {
    vi.mocked(api.addField).mockRejectedValue(new Error("offline"));
    expect(await applyProposal(FIELD, DEPS)).toMatchObject({ status: "error", reportable: false });
  });

  it("edit_field / delete_field whose field vanished is an error naming the field, no write", async () => {
    vi.mocked(api.listFields).mockResolvedValue([]);
    const a = await applyProposal(EDIT, DEPS);
    const b = await applyProposal(DEL_FIELD, DEPS);
    expect(a.message).toMatch(/Không tìm thấy trường phone trong benh_nhan/);
    expect(b.message).toMatch(/Không tìm thấy trường fax trong benh_nhan/);
    expect(api.updateField).not.toHaveBeenCalled();
    expect(api.deleteField).not.toHaveBeenCalled();
  });

  it("an unknown kind or a missing payload is an honest error, never a throw", async () => {
    expect((await applyProposal(item({ kind: "future" }), DEPS)).status).toBe("error");
    expect((await applyProposal(item({ kind: "layout" }), DEPS)).status).toBe("error");
  });

  it("approval with an unresolvable role is an error and creates nothing", async () => {
    vi.mocked(api.listRoles).mockResolvedValue([]);
    expect((await applyProposal(APPROVAL, DEPS)).status).toBe("error");
    expect(api.createApprovalProcess).not.toHaveBeenCalled();
  });

  it("workqueue naming an unknown role is an error and does NOT fall back to visible-to-everyone", async () => {
    const wq = item({ kind: "workqueue", workqueue: { name: "q", label: "Q", entityName: "x", filters: [], roleName: "ghost" } });
    expect((await applyProposal(wq, DEPS)).message).toMatch(/Không tìm thấy vai "ghost"/);
    expect(api.createWorkQueue).not.toHaveBeenCalled();
  });
});

// propose_access (issue #2): read the role, add the proposal with the one additive rule, write the whole set back.
describe("applyProposal — access", () => {
  const g = (entityName: string, action: "view" | "create" | "edit" | "delete", accessLevel: "all" | "owner" = "all") => ({ entityName, action, accessLevel });
  const CRM = [g("crm_accounts", "view"), g("crm_accounts", "edit", "owner"), g("crm_accounts", "delete", "owner")];
  const ADD = [g("tickets", "view"), g("tickets", "create"), g("tickets", "edit", "owner")];
  // The card's before-image is what the server read when it proposed: the role's grants on the card's tables (+ "*" rows).
  const card = (before: ReturnType<typeof g>[]) => item({ kind: "access", access: { role: "crm_sales_rep", roleLabel: "Nhân viên kinh doanh", grants: ADD, labels: { tickets: "Phiếu hỗ trợ" } },
    change: { docType: "role", docKey: "r1", op: "update", label: "Vai", target: { name: "crm_sales_rep", entity: "tickets" },
      before: { grants: before, labels: { tickets: "Phiếu hỗ trợ" } }, after: { grants: ADD, labels: { tickets: "Phiếu hỗ trợ" } } } });
  const ACCESS = card([]);
  beforeEach(() => {
    vi.mocked(api.listRoles).mockResolvedValue([{ id: "r1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" }]);
    vi.mocked(api.listEntities).mockResolvedValue([{ name: "crm_accounts" }, { name: "tickets" }] as never);
    vi.mocked(api.getRole).mockResolvedValue({ id: "r1", name: "crm_sales_rep", label: "Nhân viên kinh doanh", tenantId: "t", actions: CRM });
    vi.mocked(api.updateRolePermissions).mockResolvedValue({} as never);
  });

  it("writes the role's grants back with the proposal added: the other tables untouched", async () => {
    expect(await applyProposal(ACCESS, DEPS)).toMatchObject({ status: "done" });
    expect(api.updateRolePermissions).toHaveBeenCalledTimes(1);
    expect(api.updateRolePermissions).toHaveBeenCalledWith("r1", { name: "crm_sales_rep", label: "Nhân viên kinh doanh",
      actions: [...CRM, g("tickets", "view"), g("tickets", "create"), g("tickets", "edit", "owner")] });
  });

  it("a role that is gone is an honest error, nothing written", async () => {
    vi.mocked(api.listRoles).mockResolvedValue([]);
    expect((await applyProposal(ACCESS, DEPS)).message).toMatch(/Không tìm thấy vai "Nhân viên kinh doanh"/);
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });

  it("a table not built yet is an error naming it: apply the table first, nothing written", async () => {
    vi.mocked(api.listEntities).mockResolvedValue([{ name: "crm_accounts" }] as never);
    expect((await applyProposal(ACCESS, DEPS)).message).toMatch(/Phiếu hỗ trợ/);
    expect(api.getRole).not.toHaveBeenCalled();
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });

  it("nothing to add: done, says so, no write", async () => {
    const all = [g("*", "view"), g("*", "create"), g("*", "edit")];
    vi.mocked(api.getRole).mockResolvedValue({ id: "r1", name: "crm_sales_rep", tenantId: "t", actions: all });
    expect(await applyProposal(card(all), DEPS)).toMatchObject({ status: "done", message: expect.stringMatching(/đã có đủ/) });
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });

  // Verifier round 1, LOW: read-merge-PUT has no concurrency token, so a change made since the proposal would be lost
  // and its Trước/Sau would lie. The role's grants on the card's tables are compared with the card's before-image first.
  it("the role changed on the card's tables since the proposal: refused, nothing written", async () => {
    vi.mocked(api.getRole).mockResolvedValue({ id: "r1", name: "crm_sales_rep", tenantId: "t", actions: [...CRM, g("tickets", "view", "owner")] });
    const out = await applyProposal(ACCESS, DEPS);
    expect(out).toMatchObject({ status: "error", message: expect.stringMatching(/đã đổi/) });
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });

  it("a change on the role's OTHER tables is not the card's business: written, kept", async () => {
    const moved = [g("crm_accounts", "view"), g("crm_leads", "view")];
    vi.mocked(api.getRole).mockResolvedValue({ id: "r1", name: "crm_sales_rep", tenantId: "t", actions: moved });
    expect(await applyProposal(ACCESS, DEPS)).toMatchObject({ status: "done" });
    expect(vi.mocked(api.updateRolePermissions).mock.calls[0][1].actions).toEqual([...moved, ...ADD]);
  });

  it("no before-image to compare with: refused (fail closed)", async () => {
    const bare = item({ kind: "access", access: { role: "crm_sales_rep", grants: ADD, labels: { tickets: "Phiếu hỗ trợ" } } });
    expect((await applyProposal(bare, DEPS)).status).toBe("error");
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });

  it("a project switch before the write stops it", async () => {
    let calls = 0;
    expect((await applyProposal(ACCESS, { asSample: true, assertScope: () => calls++ < 0 })).status).toBe("warn");
    expect(api.updateRolePermissions).not.toHaveBeenCalled();
  });
});

describe("applyProposal — 409 means update", () => {
  it("workflow: updates the same-named after rule in place", async () => {
    vi.mocked(api.createWorkflow).mockRejectedValue(new api.ApiError(409, "WORKFLOW_ALREADY_EXISTS", "có rồi"));
    vi.mocked(api.listWorkflows).mockResolvedValue([{ id: "w1", name: "Đóng cơ hội", trigger: "after_update" }] as never);
    expect((await applyProposal(WORKFLOW, DEPS)).status).toBe("done");
    expect(api.updateWorkflow).toHaveBeenCalledWith("w1", expect.anything());
  });

  it("workflow: never overwrites a same-named before_* rule", async () => {
    vi.mocked(api.createWorkflow).mockRejectedValue(new api.ApiError(409, "WORKFLOW_ALREADY_EXISTS", "có rồi"));
    vi.mocked(api.listWorkflows).mockResolvedValue([{ id: "w1", name: "Đóng cơ hội", trigger: "before_save" }] as never);
    expect((await applyProposal(WORKFLOW, DEPS)).status).toBe("error");
    expect(api.updateWorkflow).not.toHaveBeenCalled();
  });

  it("report: updates the same-named report in place", async () => {
    vi.mocked(api.createReport).mockRejectedValue(new api.ApiError(409, "REPORT_ALREADY_EXISTS", "có rồi"));
    vi.mocked(api.listReports).mockResolvedValue([{ id: "rp1", name: "Theo giai đoạn" }] as never);
    expect((await applyProposal(REPORT, DEPS)).status).toBe("done");
    expect(api.updateReport).toHaveBeenCalledWith("rp1", expect.anything());
  });

  it("app: an existing slug merges the menu (keeps current items) and says so", async () => {
    vi.mocked(api.createApp).mockRejectedValue(new api.ApiError(409, "APP_ALREADY_EXISTS", "có rồi"));
    vi.mocked(api.adminApps).mockResolvedValue([{ id: "app-9", name: "phong_kham" }] as never);
    vi.mocked(api.adminApp).mockResolvedValue({ items: [{ id: "i1", itemType: "entity", refId: "old", sortOrder: 0 }] } as never);
    const out = await applyProposal(APP, DEPS);
    expect(out.status).toBe("warn");
    expect(out.message).toMatch(/đã có sẵn/);
    const nav = vi.mocked(api.replaceAppNav).mock.calls[0][1] as unknown[];
    expect(nav.length).toBeGreaterThanOrEqual(2);
  });

  it("app: a failed grants backfill is stated, not claimed", async () => {
    vi.mocked(api.appAclBackfill).mockRejectedValue(new Error("boom"));
    const out = await applyProposal(APP, DEPS);
    expect(out.status).toBe("warn");
    expect(out.message).toMatch(/CHƯA cấp được quyền/);
  });

  it("app: a missing nav table is a warn naming it", async () => {
    vi.mocked(api.listEntities).mockResolvedValue([]);
    const out = await applyProposal(APP, DEPS);
    expect(out.status).toBe("warn");
    expect(out.message).toMatch(/còn thiếu 1 entity chưa Apply: benh_nhan/);
  });
});

describe("applyProposal — records", () => {
  it("asSample ⇒ createSampleRecords, not the bulk route", async () => {
    await applyProposal(RECORDS, { asSample: true });
    expect(api.createSampleRecords).toHaveBeenCalledWith("benh_nhan", ROWS, "partial");
    expect(api.createRecordsBulk).not.toHaveBeenCalled();
  });

  it("not asSample ⇒ createRecordsBulk", async () => {
    await applyProposal(RECORDS, { asSample: false });
    expect(api.createRecordsBulk).toHaveBeenCalledWith("benh_nhan", ROWS, "partial");
    expect(api.createSampleRecords).not.toHaveBeenCalled();
  });

  it("some rows failing ⇒ terminal warn (re-apply would duplicate); none created ⇒ retryable error", async () => {
    vi.mocked(api.createSampleRecords).mockResolvedValue(bulk(2, 1));
    expect((await applyProposal(RECORDS, DEPS)).status).toBe("warn");
    vi.mocked(api.createSampleRecords).mockResolvedValue(bulk(0, 3));
    expect((await applyProposal(RECORDS, DEPS)).status).toBe("error");
  });

  it("zero rows is an error and writes nothing", async () => {
    const empty = item({ kind: "records", records: { entity: "benh_nhan", rows: [] } });
    expect((await applyProposal(empty, DEPS)).status).toBe("error");
    expect(api.createSampleRecords).not.toHaveBeenCalled();
  });
});

describe("applyProposal — scope guard (FR-108)", () => {
  it("assertScope is asked before EVERY write: flipping it mid-app stops the later steps with a warn, no error report", async () => {
    let calls = 0;
    const out = await applyProposal(APP, { asSample: true, assertScope: () => ++calls < 2 }); // createApp ok, replaceAppNav refused
    expect(api.createApp).toHaveBeenCalledTimes(1);
    expect(api.replaceAppNav).not.toHaveBeenCalled();
    expect(api.appAclBackfill).not.toHaveBeenCalled();
    expect(out.status).toBe("warn");
    expect(out.reportable).toBeUndefined();
    expect(out.error).toBeUndefined();
  });

  it("out of scope before the first write ⇒ nothing is written", async () => {
    const out = await applyProposal(ENTITY, { asSample: true, assertScope: () => false });
    expect(api.createEntityWithAcl).not.toHaveBeenCalled();
    expect(out.status).toBe("warn");
  });
});

describe("helpers", () => {
  it("proposalKey is turn:index", () => expect(proposalKey("t1", 2)).toBe("t1:2"));
  it("needsLiveConfirm: only a delivered project asks, for records / app-scoped table / app", () => {
    expect(needsLiveConfirm(RECORDS, false)).toBe(false);
    expect(needsLiveConfirm(RECORDS, true)).toBe(false); // sample rows (default) are not live — specs/076 ai10
    expect(needsLiveConfirm(RECORDS, true, false)).toBe(true);
    expect(needsLiveConfirm(FIELD, true)).toBe(false);
  });
});

// Words the maker must never read (S13 table). 
describe("outcome messages carry no engine vocabulary", () => {
  const BANNED = /\b(tenant|entity|layout|pack|vertical|workflow|metadata|webhook|dashboard|record|field|role|draft|publish|builder|schema|endpoint|AI|Apply)\b/i;
  const LEGACY = /entity chưa Apply/;
  it("every message the module can produce", async () => {
    vi.mocked(api.listFields).mockResolvedValue([]);
    vi.mocked(api.listRoles).mockResolvedValue([]);
    vi.mocked(api.createSampleRecords).mockResolvedValue(bulk(1, 1));
    vi.mocked(api.listEntities).mockResolvedValue([]);
    vi.mocked(api.appAclBackfill).mockRejectedValue(new Error("x"));
    const msgs = [
      (await applyProposal(EDIT, DEPS)).message, (await applyProposal(DEL_FIELD, DEPS)).message,
      (await applyProposal(APPROVAL, DEPS)).message,
      (await applyProposal(item({ kind: "workqueue", workqueue: { name: "q", label: "Q", entityName: "x", filters: [], roleName: "g" } }), DEPS)).message,
      (await applyProposal(item({ kind: "layout" }), DEPS)).message,
      (await applyProposal(item({ kind: "records", records: { entity: "x", rows: [] } }), DEPS)).message,
      (await applyProposal(APP, DEPS)).message,
      (await applyProposal(ENTITY, { asSample: true, assertScope: () => false })).message,
    ];
    for (const m of msgs) {
      expect(m).toBeTruthy();
      expect((m as string).replace(LEGACY, "")).not.toMatch(BANNED);
    }
  });
});
