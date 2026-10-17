import { afterEach, describe, it, expect } from "vitest";
import { accessTablesHidden, proposalView, proposalDone, proposalWarnLink } from "./aiProposalView";
import type { AiDraftItem } from "./aiBuilder";

const link = (id: string) => `/studio/${id}`;
const texts = (v: ReturnType<typeof proposalView>) => v?.changes.map((c) => `${c.op}:${c.text}`);

const ITEMS: Record<string, AiDraftItem> = {
  entity: { kind: "entity", entity: { name: "benh_nhan", label: "Bệnh nhân", fields: [{ name: "ten", label: "Tên", fieldType: "text", required: true }] } },
  field: { kind: "field", field: { entity: "benh_nhan", name: "ngay_tai_kham", label: "Ngày tái khám", fieldType: "date" } },
  relationship: { kind: "relationship", relationship: { name: "bn_thuoc", entityA: "benh_nhan", entityB: "thuoc" } },
  layout: { kind: "layout", layout: { entity: "benh_nhan", viewType: "edit", layoutJson: { sections: [{ title: "Chung", columns: 2, rows: [["ten", "sdt"]] }] } } },
  workflow: { kind: "workflow", workflow: { name: "Nhắc lịch", entityName: "lich_hen", trigger: "after_create", actions: [{ type: "field_update", field: "status", value: "moi" }], isActive: true } },
  report: { kind: "report", report: { name: "Theo giai đoạn", entityName: "co_hoi", type: "summary", config: { filters: [{ field: "stage", op: "eq", values: ["won"] }] } } },
  app: { kind: "app", app: { name: "phong_kham", label: "Phòng khám", navEntities: ["benh_nhan"] } },
  edit_field: { kind: "edit_field", editField: { entity: "benh_nhan", field: "sdt", before: { label: "SĐT" }, after: { label: "Số điện thoại" } } },
  delete_field: { kind: "delete_field", deleteField: { entity: "benh_nhan", field: "fax", label: "Số fax", fieldType: "text" } },
  delete_entity: { kind: "delete_entity", deleteEntity: { entity: "khuyen_mai", label: "Khuyến mãi", fieldCount: 7 } },
  records: { kind: "records", records: { entity: "benh_nhan", rows: [{ ten: "An" }, { ten: "Bình" }] } },
  approval: { kind: "approval", approval: { name: "Duyệt giảm giá", entityName: "bao_gia", steps: [{ approverRef: "role:sales_manager" } as never] } },
  dashboard: { kind: "dashboard", dashboard: { name: "Tổng quan", widgets: [{ title: "Doanh thu", type: "metric" }] } },
  workqueue: { kind: "workqueue", workqueue: { name: "cho_duyet", label: "Chờ duyệt", entityName: "bao_gia", filters: [{ field: "status", op: "eq", value: "moi" }] } },
  access: { kind: "access", access: { role: "crm_sales_rep", roleLabel: "Nhân viên kinh doanh", labels: { tickets: "Phiếu hỗ trợ" },
    grants: [{ entityName: "tickets", action: "view", accessLevel: "all" }, { entityName: "tickets", action: "create", accessLevel: "all" }, { entityName: "tickets", action: "edit", accessLevel: "owner" }] } },
};

describe("proposalView", () => {
  it.each(Object.keys(ITEMS))("gives %s a title, a where line and at least one change", (k) => {
    const v = proposalView(ITEMS[k]);
    expect(v).not.toBeNull();
    expect(v!.title).not.toBe("");
    expect(v!.where).not.toBe("");
    expect(v!.changes.length).toBeGreaterThan(0);
    expect(v!.op).toBeUndefined(); // no server `change` ⇒ no label
    expect(v!.before).toBeUndefined();
    expect(v!.after).toBeUndefined();
  });

  it("field reads as 'Thêm trường' with a + row", () => {
    const v = proposalView(ITEMS.field)!;
    expect(v.title).toBe("Thêm trường “Ngày tái khám”");
    expect(v.where).toBe("Bảng benh_nhan");
    expect(texts(v)).toEqual(["add:Ngày tái khám (Ngày)"]);
  });

  it("edit_field uses ~ rows carrying Trước → Sau", () => {
    const v = proposalView(ITEMS.edit_field)!;
    expect(v.title).toBe("Sửa trường “Số điện thoại”");
    expect(v.changes[0]).toMatchObject({ op: "change", text: "Nhãn: SĐT → Số điện thoại" });
  });

  it("delete_entity names the field count and is destructive", () => {
    const v = proposalView(ITEMS.delete_entity)!;
    expect(v.destructive).toBe(true);
    expect(texts(v)).toEqual(["remove:Bảng cùng 7 trường", "remove:Toàn bộ bản ghi của bảng"]);
    expect(proposalView(ITEMS.delete_field)!.destructive).toBe(true);
    expect(proposalView(ITEMS.field)!.destructive).toBe(false);
  });

  it("adds 'khách chưa thấy' only when the project is known undelivered", () => {
    expect(proposalView(ITEMS.field, { delivered: false })!.where).toBe("Bảng benh_nhan · khách chưa thấy");
    expect(proposalView(ITEMS.field, { delivered: true })!.where).toBe("Bảng benh_nhan");
  });

  it("records carries a preview table", () => {
    const v = proposalView(ITEMS.records)!;
    expect(v.table).toMatchObject({ columns: ["ten"], rows: [["An"], ["Bình"]], moreCount: 0, total: 2 });
  });

  it("with `change`: label, ≤5 changed places + the rest, Trước/Sau, masked string kept verbatim", () => {
    const wf = ITEMS.workflow;
    const v = proposalView({
      ...wf,
      change: {
        docType: "workflow", op: "update", label: "Luật", changedPaths: ["a", "b", "c", "d", "e", "f"], moreChanged: 3,
        before: { trigger: "after_create", actions: [{ type: "outbound_webhook", url: "[đã che]" }] },
        after: { trigger: "after_create", actions: [] },
      },
    })!;
    expect(v.op).toBe("update");
    expect(v.changes).toHaveLength(5);
    expect(v.moreCount).toBe(3);
    expect(v.before!.join("|")).toContain("[đã che]");
    expect(v.after).toBeDefined();
  });

  it("create/delete with `change` keep the per-kind rows; delete has Trước only", () => {
    const c = (op: string) => ({ docType: "field", op, label: "x", before: { label: "Fax" } });
    const created = proposalView({ ...ITEMS.field, change: c("create") })!;
    expect(created.op).toBe("create");
    expect(created.before).toBeUndefined();
    const del = proposalView({ ...ITEMS.delete_field, change: c("delete") })!;
    expect(del.op).toBe("delete");
    expect(del.before).toBeDefined();
    expect(del.after).toBeUndefined();
  });

  it("unknown kind, or a known kind without its payload ⇒ null", () => {
    expect(proposalView({ kind: "hologram" })).toBeNull();
    expect(proposalView({ kind: "layout" })).toBeNull();
    expect(proposalView({ kind: "delete_field", deleteField: null as never })).toBeNull();
  });
});

describe("proposalDone / proposalWarnLink", () => {
  it("routes each applied kind to the builder that owns it", () => {
    expect(proposalDone(ITEMS.layout, undefined, link)).toMatchObject({ href: "/studio/dung/man-hinh", text: "Đã lưu bố cục." });
    expect(proposalDone(ITEMS.dashboard, "d1", link).href).toBe("/dashboards/d1");
    expect(proposalDone(ITEMS.dashboard, undefined, link).href).toBe("/dashboards");
    expect(proposalDone(ITEMS.records, undefined, link).href).toBe("/benh_nhan");
    expect(proposalDone(ITEMS.app, undefined, link).href).toBe("/home?app=phong_kham");
  });
  it("warn link exists for app/entity/records only", () => {
    expect(proposalWarnLink(ITEMS.app, link)?.label).toBe("Mở App và menu");
    expect(proposalWarnLink(ITEMS.records, link)?.href).toBe("/benh_nhan");
    expect(proposalWarnLink(ITEMS.workflow, link)).toBeNull();
  });
});

// issue #2: the access card says, in human words, what the role may do on the table.
describe("proposalView — access", () => {
  const withChange: AiDraftItem = {
    ...ITEMS.access,
    change: { docType: "role", docKey: "r1", op: "update", label: "Vai", target: { name: "crm_sales_rep", entity: "tickets" },
      before: { grants: [], labels: { tickets: "Phiếu hỗ trợ" } },
      after: { grants: ITEMS.access.access!.grants, labels: { tickets: "Phiếu hỗ trợ" } } },
  };
  afterEach(() => localStorage.removeItem("nexus.prefs.v1"));

  const LINES = ["add:Phiếu hỗ trợ: xem tất cả", "add:Phiếu hỗ trợ: tạo", "add:Phiếu hỗ trợ: sửa của mình"];

  it("the title is the sentence: <role> được xem tất cả · tạo · sửa của mình trên <table>; one + line per grant", () => {
    const v = proposalView(ITEMS.access)!;
    expect(v.title).toBe("Nhân viên kinh doanh được xem tất cả · tạo · sửa của mình trên Phiếu hỗ trợ");
    expect(texts(v)).toEqual(LINES);
    expect(v.where).toBe("Người dùng & quyền");
  });

  it("in English: <Role label> can view all · create · edit their own <Table>", () => {
    localStorage.setItem("nexus.prefs.v1", JSON.stringify({ locale: "en" }));
    expect(proposalView(ITEMS.access)!.title).toBe("Nhân viên kinh doanh can view all · create · edit their own Phiếu hỗ trợ");
  });

  it("two tables: one clause each", () => {
    const two: AiDraftItem = { kind: "access", access: { role: "r", roleLabel: "Lễ tân", labels: { a: "A", b: "B" },
      grants: [{ entityName: "a", action: "view", accessLevel: "all" }, { entityName: "b", action: "edit", accessLevel: "owner" }] } };
    expect(proposalView(two)!.title).toBe("Lễ tân được xem tất cả trên A; Lễ tân được sửa của mình trên B");
  });

  it("with the server's role change: Sửa, the lines stay, Trước/Sau name the table's access", () => {
    const v = proposalView(withChange)!;
    expect(v.op).toBe("update");
    expect(texts(v)).toEqual(LINES);
    expect(v.before).toEqual(["Phiếu hỗ trợ: không có quyền"]);
    expect(v.after).toEqual(["Phiếu hỗ trợ: xem tất cả · tạo · sửa của mình"]);
  });

  it("applied, it points at Người dùng & quyền", () => {
    expect(proposalDone(ITEMS.access, undefined, link)).toMatchObject({ href: "/studio/dung/nguoi-dung-quyen", label: "Mở Người dùng & quyền" });
  });
});

// Verifier round 2: "the table stays hidden until Xuất bản" only while every table of the access card still is.
describe("accessTablesHidden", () => {
  const two: AiDraftItem = { kind: "access", access: { role: "r", grants: [
    { entityName: "a", action: "view", accessLevel: "all" }, { entityName: "b", action: "view", accessLevel: "all" }] } };
  const list = (b?: "new") => [{ name: "a", label: "A", tableName: "obj_a", draftState: "new" as const }, { name: "b", label: "B", tableName: "obj_b", draftState: b }];
  it("every table still new: hidden", () => expect(accessTablesHidden(two, list("new"))).toBe(true));
  it("one table published already: not hidden", () => expect(accessTablesHidden(two, list())).toBe(false));
  it("a table not in the list yet, or no list at all: not claimed hidden", () => {
    expect(accessTablesHidden(two, list("new").slice(0, 1))).toBe(false);
    expect(accessTablesHidden(two, undefined)).toBe(false);
  });
  it("not an access card: false", () => expect(accessTablesHidden(ITEMS.entity, list("new"))).toBe(false));
});
