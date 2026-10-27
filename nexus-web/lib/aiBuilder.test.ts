import { describe, it, expect } from "vitest";
import {
  draftFieldToInput,
  draftEntityToPayload,
  draftRelationshipToPayload,
  draftLayoutToPayload,
  draftWorkflowToPayload,
  draftReportToPayload,
  draftAppToPayload,
  resolveAppNavEntities,
  serverMessagesToTurns,
  hasDrafts,
  formatTokens,
  aiLimitMessage,
  layoutViewLabel,
  summarizeLayoutDraft,
  workflowTriggerPhrase,
  workflowRuleText,
  conditionJoiner,
  workflowActionText,
  changePathText,
  reportTypeLabel,
  reportSummaryText,
  getEditFieldDraft,
  getDeleteFieldDraft,
  getDeleteEntityDraft,
  getRecordsDraft,
  draftEditFieldToUpdatePayload,
  draftRecordsRows,
  editFieldDiffRows,
  recordsPreviewModel,
  recordCellText,
  recordsApplyMessage,
  draftApplyRank,
  isDestructiveDraft,
  getApprovalDraft,
  getDashboardDraft,
  getWorkQueueDraft,
  draftApprovalToPayload,
  approvalDraftRefs,
  resolveApprovalApprovers,
  draftDashboardToPayload,
  draftWorkQueueToPayload,
  resolveRoleByName,
  slaText,
  approvalStepText,
  approvalCriteriaCount,
  approvalCriteriaRules,
  dashboardWidgetLines,
  workQueueSummaryText,
  type AiDraftField,
  type AiDraftEntity,
  type AiDraftRelationship,
  type AiDraftLayout,
  type AiDraftWorkflow,
  type AiDraftReport,
  type AiDraftApp,
  type AiDraftEditField,
  type AiDraftItem,
  type AiServerMessage,
  type AiDraftApproval,
  type AiDraftDashboard,
  type AiDraftWorkQueue,
} from "./aiBuilder";

describe("draftFieldToInput", () => {
  it("renames required → isRequired (the load-bearing flag must not be dropped)", () => {
    const f: AiDraftField = { name: "email", label: "Email", fieldType: "email", required: true };
    const out = draftFieldToInput(f);
    expect(out.isRequired).toBe(true);
    // NewFieldInput has no `required` key — the rename must be complete
    expect("required" in out).toBe(false);
  });

  it("coerces a missing/undefined required to isRequired:false", () => {
    const f: AiDraftField = { name: "note", label: "Ghi chú", fieldType: "text" };
    expect(draftFieldToInput(f).isRequired).toBe(false);
  });

  it("passes structured options straight through (no re-derivation)", () => {
    const options = { relatedEntity: "benh_nhan", cardinality: "1-N" };
    const f: AiDraftField = { name: "patient", label: "Bệnh nhân", fieldType: "relation", options };
    const out = draftFieldToInput(f);
    // same object content — passthrough, not rebuilt
    expect(out.options).toEqual(options);
  });

  it("preserves picklist values verbatim", () => {
    const f: AiDraftField = {
      name: "status", label: "Trạng thái", fieldType: "picklist",
      options: { values: ["cho_kham", "da_kham", "huy"] },
    };
    expect(draftFieldToInput(f).options).toEqual({ values: ["cho_kham", "da_kham", "huy"] });
  });

  it("omits options entirely when empty (no empty {} on the wire)", () => {
    expect(draftFieldToInput({ name: "a", label: "A", fieldType: "text", options: {} }).options).toBeUndefined();
    expect(draftFieldToInput({ name: "a", label: "A", fieldType: "text" }).options).toBeUndefined();
  });

  it("carries name/label/fieldType through unchanged", () => {
    const out = draftFieldToInput({ name: "amount", label: "Số tiền", fieldType: "currency", required: false });
    expect(out).toMatchObject({ name: "amount", label: "Số tiền", fieldType: "currency", isRequired: false });
  });
});

describe("draftEntityToPayload", () => {
  const ent: AiDraftEntity = {
    name: "lich_hen",
    label: "Lịch hẹn",
    fields: [
      { name: "code", label: "Mã hẹn", fieldType: "text", required: true },
      { name: "patient", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan" }, required: true },
      { name: "note", label: "Ghi chú", fieldType: "text" },
    ],
  };

  it("maps name/label and defaults description to empty string", () => {
    const p = draftEntityToPayload(ent);
    expect(p.name).toBe("lich_hen");
    expect(p.label).toBe("Lịch hẹn");
    expect(p.description).toBe("");
  });

  it("maps every field through draftFieldToInput (rename + passthrough)", () => {
    const p = draftEntityToPayload(ent);
    expect(p.fields).toHaveLength(3);
    expect(p.fields[0]).toMatchObject({ name: "code", fieldType: "text", isRequired: true });
    expect(p.fields[1]).toMatchObject({ name: "patient", isRequired: true, options: { relatedEntity: "benh_nhan" } });
    expect(p.fields[2].isRequired).toBe(false);
    // no leaked `required` key anywhere
    expect(p.fields.every((f) => !("required" in f))).toBe(true);
  });

  it("tolerates an entity proposed with no fields", () => {
    const p = draftEntityToPayload({ name: "empty", label: "Trống", fields: [] });
    expect(p.fields).toEqual([]);
  });
});

// E9 M5 — the N-N draft → POST /admin/relationships body mapper. Identity-SHAPED but load-bearing:
// it pins the exact 3-key payload and strips anything else the wire might carry.
describe("draftRelationshipToPayload", () => {
  it("maps name/entityA/entityB 1:1 onto the createRelationship payload", () => {
    const r: AiDraftRelationship = { name: "hoc_vien_khoa_hoc", entityA: "hoc_vien", entityB: "khoa_hoc" };
    expect(draftRelationshipToPayload(r)).toEqual({
      name: "hoc_vien_khoa_hoc",
      entityA: "hoc_vien",
      entityB: "khoa_hoc",
    });
  });

  it("strips extra keys a newer backend might add to the wire draft (exact 3-key body)", () => {
    const wire = {
      name: "bac_si_benh_nhan", entityA: "bac_si", entityB: "benh_nhan",
      joinTableName: "rel_bac_si_benh_nhan", relationshipType: "N-N",
    } as AiDraftRelationship;
    const payload = draftRelationshipToPayload(wire);
    expect(Object.keys(payload).sort()).toEqual(["entityA", "entityB", "name"]);
    expect(payload).not.toHaveProperty("joinTableName");
  });
});

// E9 M6 FE — the layout draft → POST /admin/layouts body mapper. Pins the entity→entityName rename
// and the exact 3-key upsert body; layoutJson goes through OPAQUE (server-validated at propose
// time with the same validator SaveLayout runs — the FE must never rewrite it).
describe("draftLayoutToPayload", () => {
  const LAYOUT_JSON = {
    version: 1,
    sections: [{ title: "Thông tin chung", columns: 2, fields: ["full_name", "email"] }],
  };

  it("renames entity → entityName and pins the exact {entityName, viewType, layoutJson} body", () => {
    const l: AiDraftLayout = { entity: "crm_contacts", viewType: "edit", layoutJson: LAYOUT_JSON };
    const payload = draftLayoutToPayload(l);
    expect(payload).toEqual({ entityName: "crm_contacts", viewType: "edit", layoutJson: LAYOUT_JSON });
    expect(Object.keys(payload).sort()).toEqual(["entityName", "layoutJson", "viewType"]);
  });

  it("passes layoutJson through untouched (same reference — no clone, no re-derivation)", () => {
    const l: AiDraftLayout = { entity: "crm_contacts", viewType: "list", layoutJson: LAYOUT_JSON };
    expect(draftLayoutToPayload(l).layoutJson).toBe(LAYOUT_JSON);
  });

  it("strips extra keys a future wire draft might carry and never sends isDefault", () => {
    const wire = {
      entity: "crm_contacts", viewType: "detail", layoutJson: LAYOUT_JSON,
      isDefault: true, id: "lay-1",
    } as unknown as AiDraftLayout;
    const payload = draftLayoutToPayload(wire);
    expect(payload).not.toHaveProperty("isDefault"); // default-promotion is a human decision
    expect(payload).not.toHaveProperty("id");
    expect(Object.keys(payload).sort()).toEqual(["entityName", "layoutJson", "viewType"]);
  });
});

// E9 M7 FE — the workflow draft → POST /admin/workflows body mapper. Identity-shaped but rebuilt
// key-by-key: condition rules/actions can never leak unknown keys, and ABSENT keys stay absent
// (null-op rules value-free; field_update NIT-1: absent value is NOT fabricated into null).
describe("draftWorkflowToPayload", () => {
  const FULL: AiDraftWorkflow = {
    name: "Đóng cơ hội thắng",
    entityName: "crm_opportunities",
    trigger: "after_update",
    condition: { match: "and", rules: [{ field: "stage", op: "eq", value: "won" }] },
    actions: [
      { type: "field_update", field: "status", value: "closed" },
      { type: "in_app_notification", userId: "$owner", title: "Cơ hội thắng", message: "Cơ hội vừa chuyển sang thắng." },
    ],
    isActive: true,
  };

  it("maps the full body {name, entityName, trigger, condition, actions, isActive} 1:1", () => {
    expect(draftWorkflowToPayload(FULL)).toEqual({
      name: "Đóng cơ hội thắng",
      entityName: "crm_opportunities",
      trigger: "after_update",
      condition: { match: "and", rules: [{ field: "stage", op: "eq", value: "won" }] },
      actions: [
        { type: "field_update", field: "status", value: "closed" },
        { type: "in_app_notification", userId: "$owner", title: "Cơ hội thắng", message: "Cơ hội vừa chuyển sang thắng." },
      ],
      isActive: true,
    });
  });

  it("omits condition entirely when absent or rule-less (match-all — nothing invented)", () => {
    expect(draftWorkflowToPayload({ ...FULL, condition: undefined })).not.toHaveProperty("condition");
    expect(draftWorkflowToPayload({ ...FULL, condition: { match: "and", rules: [] } })).not.toHaveProperty("condition");
  });

  it("keeps a null-operator rule value-free (backend 422s a value on isnull/notnull)", () => {
    const p = draftWorkflowToPayload({
      ...FULL,
      condition: { match: "and", rules: [{ field: "note", op: "isnull" }] },
    });
    expect((p.condition as { rules: Record<string, unknown>[] }).rules[0]).toEqual({ field: "note", op: "isnull" });
    expect("value" in (p.condition as { rules: Record<string, unknown>[] }).rules[0]).toBe(false);
  });

  it("keeps a field_update WITHOUT value value-free (NIT-1: absent ≠ fabricate null)", () => {
    const p = draftWorkflowToPayload({ ...FULL, actions: [{ type: "field_update", field: "status" }] });
    expect((p.actions as Record<string, unknown>[])[0]).toEqual({ type: "field_update", field: "status" });
  });

  it("strips unknown action keys (structural pin: url/secret/headers can never leak)", () => {
    const wire = {
      ...FULL,
      actions: [{ type: "field_update", field: "status", value: "x", url: "https://evil", secret: "s" }],
    } as unknown as AiDraftWorkflow;
    const p = draftWorkflowToPayload(wire);
    expect((p.actions as Record<string, unknown>[])[0]).toEqual({ type: "field_update", field: "status", value: "x" });
  });

  it("strips extra top-level keys and defaults isActive true only when the wire omitted it", () => {
    const wire = { ...FULL, isActive: undefined, id: "wf-9" } as unknown as AiDraftWorkflow;
    const p = draftWorkflowToPayload(wire);
    expect(p.isActive).toBe(true);
    expect(p).not.toHaveProperty("id");
    expect(Object.keys(p).sort()).toEqual(["actions", "condition", "entityName", "isActive", "name", "trigger"]);
  });
});

// E9 M8 FE — the report draft → POST /admin/reports body mapper. Pins the exact
// {name, entityName, type, config, isActive} body (wire key `type`, NOT reportType — verified
// against DraftReport json tags + the report handler's reportRequest) and rebuilds config +
// filters key-by-key so nothing else — notably `sort`, a human decision in the Report Builder —
// ever leaks into the gated admin API.
describe("draftReportToPayload", () => {
  const SUMMARY: AiDraftReport = {
    name: "Cơ hội theo giai đoạn",
    entityName: "crm_opportunities",
    type: "summary",
    config: {
      groupBy: "stage",
      metric: "count",
      filters: [{ field: "stage", op: "neq", values: ["lost"] }],
    },
    isActive: true,
  };

  it("maps the full summary body {name, entityName, type, config, isActive} 1:1", () => {
    expect(draftReportToPayload(SUMMARY)).toEqual({
      name: "Cơ hội theo giai đoạn",
      entityName: "crm_opportunities",
      type: "summary",
      config: { groupBy: "stage", metric: "count", filters: [{ field: "stage", op: "neq", values: ["lost"] }] },
      isActive: true,
    });
  });

  it("keeps absent config keys ABSENT (mirrors omitempty — no empty columns/field/bucket/filters)", () => {
    const p = draftReportToPayload({ ...SUMMARY, config: { groupBy: "stage", metric: "count" } });
    expect(p.config).toEqual({ groupBy: "stage", metric: "count" });
    expect(Object.keys(p.config as object).sort()).toEqual(["groupBy", "metric"]);
  });

  it("carries the numeric-metric trio metric+field+bucket for a time-bucketed sum", () => {
    const p = draftReportToPayload({
      ...SUMMARY,
      name: "Doanh thu theo tháng",
      config: { groupBy: "created_at", metric: "sum", field: "amount", bucket: "month" },
    });
    expect(p.config).toEqual({ groupBy: "created_at", metric: "sum", field: "amount", bucket: "month" });
  });

  it("passes tabular columns through and rebuilds filter predicates key-by-key (absent op/values stay absent)", () => {
    const p = draftReportToPayload({
      name: "Danh sách cơ hội",
      entityName: "crm_opportunities",
      type: "tabular",
      config: { columns: ["name", "stage", "amount"], filters: [{ field: "stage" }] },
      isActive: true,
    });
    expect(p.config).toEqual({ columns: ["name", "stage", "amount"], filters: [{ field: "stage" }] });
    const filter = (p.config as { filters: Record<string, unknown>[] }).filters[0];
    expect("op" in filter).toBe(false);
    expect("values" in filter).toBe(false);
  });

  it("strips extra keys — config.sort and unknown filter keys can never leak (structural pin)", () => {
    const wire = {
      ...SUMMARY,
      id: "rep-9",
      config: {
        groupBy: "stage", metric: "count",
        sort: { field: "created_at", dir: "desc" },
        filters: [{ field: "stage", op: "eq", values: ["won"], caseSensitive: true }],
      },
    } as unknown as AiDraftReport;
    const p = draftReportToPayload(wire);
    expect(p).not.toHaveProperty("id");
    expect(p.config).not.toHaveProperty("sort"); // ordering is a human decision (spec §15)
    expect((p.config as { filters: Record<string, unknown>[] }).filters[0]).toEqual({
      field: "stage", op: "eq", values: ["won"],
    });
    expect(Object.keys(p).sort()).toEqual(["config", "entityName", "isActive", "name", "type"]);
  });

  it("carries the grid keys subGroups/columnGroup/having (specs/049 S7a — was dropped silently)", () => {
    const p = draftReportToPayload({
      ...SUMMARY,
      type: "matrix",
      config: {
        groupBy: "stage", metric: "sum", field: "amount",
        columnGroup: { field: "close_date", bucket: "month" },
        subGroups: [{ field: "owner_id" }],
        having: { on: "value", op: "gt", values: ["10000"] },
      },
    });
    expect(p.config).toEqual({
      groupBy: "stage", metric: "sum", field: "amount",
      columnGroup: { field: "close_date", bucket: "month" },
      subGroups: [{ field: "owner_id" }],
      having: { on: "value", op: "gt", values: ["10000"] },
    });
  });

  it("defaults isActive to true only when the wire omitted it (malformed persisted draft guard)", () => {
    const p = draftReportToPayload({ ...SUMMARY, isActive: undefined });
    expect(p.isActive).toBe(true);
  });
});

// Apps Phase 3 — the app draft → POST /admin/apps body mapper. Pins the exact CreateAppPayload
// key set: navEntities NEVER goes here (the nav is a second call after name→id resolution) and
// isDefault/sortOrder are never sent (human decisions in the Apps Manager).
describe("draftAppToPayload", () => {
  const FULL: AiDraftApp = {
    name: "phong_kham",
    label: "Phòng khám",
    icon: "stethoscope",
    color: "cy",
    description: "Quản lý bệnh nhân và lịch hẹn",
    navEntities: ["benh_nhan", "lich_hen"],
  };

  it("maps name/label and carries icon/color/description when present", () => {
    expect(draftAppToPayload(FULL)).toEqual({
      name: "phong_kham",
      label: "Phòng khám",
      icon: "stethoscope",
      color: "cy",
      description: "Quản lý bệnh nhân và lịch hẹn",
    });
  });

  it("keeps absent optionals ABSENT (no empty icon/color/description keys on the wire)", () => {
    const p = draftAppToPayload({ name: "sales", label: "Bán hàng", navEntities: ["accounts"] });
    expect(p).toEqual({ name: "sales", label: "Bán hàng" });
    expect(Object.keys(p).sort()).toEqual(["label", "name"]);
  });

  it("never leaks navEntities/isDefault/sortOrder or extra wire keys (structural pin)", () => {
    const wire = { ...FULL, isDefault: true, sortOrder: 3, id: "app-9" } as unknown as AiDraftApp;
    const p = draftAppToPayload(wire);
    expect(p).not.toHaveProperty("navEntities"); // nav attaches via replaceAppNav, not create
    expect(p).not.toHaveProperty("isDefault");
    expect(p).not.toHaveProperty("sortOrder");
    expect(p).not.toHaveProperty("id");
    expect(Object.keys(p).sort()).toEqual(["color", "description", "icon", "label", "name"]);
  });
});

// Apps Phase 3 — the pure name→id resolution step of applying an app draft. The apply path calls
// this with the live /metadata/entities list; missing names surface as the card's honest partial
// warning ("Apply các entity này trước").
describe("resolveAppNavEntities", () => {
  const ENTITIES = [
    { id: "id-bn", name: "benh_nhan" },
    { id: "id-lh", name: "lich_hen" },
    { name: "no_id_yet" }, // registry row without an id — cannot be referenced
  ];

  it("resolves names to ReplaceNav items in nav order (order IS the sidebar order)", () => {
    const r = resolveAppNavEntities(["lich_hen", "benh_nhan"], ENTITIES);
    expect(r.items).toEqual([
      { itemType: "entity", refId: "id-lh" },
      { itemType: "entity", refId: "id-bn" },
    ]);
    expect(r.missing).toEqual([]);
  });

  it("matches case-insensitively and trims whitespace (canonical stored names are lowercase)", () => {
    const r = resolveAppNavEntities([" Benh_Nhan "], ENTITIES);
    expect(r.items).toEqual([{ itemType: "entity", refId: "id-bn" }]);
  });

  it("collects unresolved names in nav order, preserving the raw spelling", () => {
    const r = resolveAppNavEntities(["benh_nhan", "hoa_don", "lich_hen", "kho_thuoc"], ENTITIES);
    expect(r.items.map((i) => i.refId)).toEqual(["id-bn", "id-lh"]);
    expect(r.missing).toEqual(["hoa_don", "kho_thuoc"]);
  });

  it("treats an entity row without an id as missing (no refId to send)", () => {
    const r = resolveAppNavEntities(["no_id_yet"], ENTITIES);
    expect(r.items).toEqual([]);
    expect(r.missing).toEqual(["no_id_yet"]);
  });

  it("dedupes repeats (first occurrence wins) so ReplaceNav never 422s on a duplicate pair", () => {
    const r = resolveAppNavEntities(["benh_nhan", "BENH_NHAN", "hoa_don", "hoa_don"], ENTITIES);
    expect(r.items).toEqual([{ itemType: "entity", refId: "id-bn" }]);
    expect(r.missing).toEqual(["hoa_don"]);
  });

  it("tolerates null/empty/blank input", () => {
    expect(resolveAppNavEntities(null, ENTITIES)).toEqual({ items: [], missing: [] });
    expect(resolveAppNavEntities([], ENTITIES)).toEqual({ items: [], missing: [] });
    expect(resolveAppNavEntities(["  "], ENTITIES)).toEqual({ items: [], missing: [] });
    expect(resolveAppNavEntities(["benh_nhan"], [])).toEqual({ items: [], missing: ["benh_nhan"] });
  });
});

// E9 M8 — the report card's Vietnamese digest helpers.
describe("report draft display helpers", () => {
  it("reportTypeLabel maps the three types to Vietnamese and leaves unknown tokens raw", () => {
    expect(reportTypeLabel("summary")).toBe("Tổng hợp");
    expect(reportTypeLabel("matrix")).toBe("Ma trận");
    expect(reportTypeLabel("tabular")).toBe("Bảng");
    expect(reportTypeLabel("pivot")).toBe("pivot");
  });

  it("reportSummaryText: having without values (malformed persisted draft) does not throw", () => {
    const cfg = { metric: "sum", field: "amount", groupBy: "stage", having: { on: "value", op: "between" } } as never;
    expect(() => reportSummaryText("summary", cfg)).not.toThrow();
  });

  it("reportSummaryText: count + groupBy reads as one compact sentence", () => {
    expect(reportSummaryText("summary", { metric: "count", groupBy: "stage" }))
      .toBe("Đếm số bản ghi · nhóm theo stage");
  });

  it("reportSummaryText: numeric metric names its field; bucket appends the time unit", () => {
    expect(reportSummaryText("summary", { metric: "sum", field: "amount", groupBy: "created_at", bucket: "month" }))
      .toBe("Tổng amount · nhóm theo created_at · gộp theo tháng");
    expect(reportSummaryText("matrix", { metric: "avg", field: "amount", groupBy: "stage" }))
      .toBe("Trung bình amount · nhóm theo stage");
  });

  it("reportSummaryText: count never names a field (count takes no field)", () => {
    expect(reportSummaryText("summary", { metric: "count", field: "amount", groupBy: "stage" }))
      .toBe("Đếm số bản ghi · nhóm theo stage");
  });

  it("reportSummaryText: tabular counts its columns; empty → '' (the card states opaque honestly)", () => {
    expect(reportSummaryText("tabular", { columns: ["name", "stage", "amount"] })).toBe("Bảng dữ liệu 3 cột");
    expect(reportSummaryText("tabular", {})).toBe("");
    expect(reportSummaryText("summary", {})).toBe("");
    expect(reportSummaryText("summary", null)).toBe("");
  });

  it("reportSummaryText: mentions column axis / sub-levels / threshold (ô for matrix, nhóm for summary)", () => {
    expect(reportSummaryText("matrix", {
      metric: "sum", field: "amount", groupBy: "stage",
      columnGroup: { field: "close_date", bucket: "month" },
      having: { on: "value", op: "gt", values: ["10000"] },
    })).toBe("Tổng amount · nhóm theo stage · cột theo close_date (tháng) · chỉ giữ ô có giá trị lớn hơn 10000");
    expect(reportSummaryText("summary", {
      metric: "count", groupBy: "stage", subGroups: [{ field: "owner_id" }],
      having: { on: "count", op: "between", values: ["2", "5"] },
    })).toBe("Đếm số bản ghi · nhóm theo stage · cấp phụ owner_id · chỉ giữ nhóm có số bản ghi trong khoảng 2 – 5");
  });

  it("reportSummaryText: unknown metric/bucket tokens render raw (honest — never invented)", () => {
    expect(reportSummaryText("summary", { metric: "median", field: "amount", groupBy: "stage" }))
      .toBe("median amount · nhóm theo stage");
    expect(reportSummaryText("summary", { metric: "count", groupBy: "created_at", bucket: "decade" }))
      .toBe("Đếm số bản ghi · nhóm theo created_at · gộp theo decade");
  });
});

// E9 M6 — the card's readable digest of layoutJson (unit-tested so the card JSX stays dumb).
describe("summarizeLayoutDraft", () => {
  it("edit/detail → sections with title, columns and field names (spacers + junk cells skipped)", () => {
    const s = summarizeLayoutDraft("edit", {
      version: 1,
      sections: [
        { title: "Thông tin chung", columns: 2, fields: ["full_name", { field: "email" }, { spacer: true }] },
        { columns: 1, fields: [42, { notAField: true }] },
      ],
    });
    expect(s).toEqual({
      kind: "sections",
      sections: [
        { title: "Thông tin chung", columns: 2, fields: ["full_name", "email"] },
        { title: "Khu vực 2", columns: 1, fields: [] }, // untitled section gets an honest positional name
      ],
    });
  });

  it("list → column names + defaultSort when present", () => {
    const s = summarizeLayoutDraft("list", {
      version: 1,
      columns: [{ field: "full_name", width: 200 }, { field: "status" }],
      defaultSort: { field: "created_at", order: "desc" },
    });
    expect(s).toEqual({
      kind: "columns",
      columns: ["full_name", "status"],
      defaultSort: { field: "created_at", order: "desc" },
    });
  });

  it("degrades to opaque for an unknown viewType or an unrecognizable structure", () => {
    expect(summarizeLayoutDraft("kanban", { version: 1 })).toEqual({ kind: "opaque" });
    expect(summarizeLayoutDraft("edit", { version: 1 })).toEqual({ kind: "opaque" }); // no sections array
    expect(summarizeLayoutDraft("list", null)).toEqual({ kind: "opaque" });
  });
});

// E9 M7 — the KHI/NẾU/THÌ sentence pieces.
describe("workflow draft display helpers", () => {
  it("workflowTriggerPhrase names the three after-commit events (no 'Khi' — the row label says it); unknown token renders raw", () => {
    expect(workflowTriggerPhrase("after_create")).toBe("Tạo mới bản ghi");
    expect(workflowTriggerPhrase("after_update")).toBe("Cập nhật bản ghi");
    expect(workflowTriggerPhrase("after_delete")).toBe("Xoá bản ghi");
    expect(workflowTriggerPhrase("some_future_event")).toBe("some_future_event");
  });

  it("workflowRuleText renders op symbols and keeps null-ops / absent values value-free", () => {
    expect(workflowRuleText({ field: "stage", op: "eq", value: "won" })).toBe("stage = won");
    expect(workflowRuleText({ field: "amount", op: "gte", value: 1000 })).toBe("amount ≥ 1000");
    expect(workflowRuleText({ field: "note", op: "isnull" })).toBe("note rỗng");
    expect(workflowRuleText({ field: "x", op: "eq" })).toBe("x ="); // absent value: render what's present
    // queue #162: the AI may now propose the edge operators — the card speaks them, not the raw token.
    expect(workflowRuleText({ field: "stage", op: "changed" })).toBe("stage vừa đổi");
    expect(workflowRuleText({ field: "stage", op: "changedTo", value: "won" })).toBe("stage vừa đổi thành won");
  });

  // The engine reads match case-insensitively (EvaluateWithBefore: EqualFold "or"), so the card must
  // too: a strict === "or" showed a draft buffered as "OR" joined by "và" while Apply created OR.
  it("conditionJoiner reads match the way the engine does", () => {
    for (const m of ["or", "OR", " Or "]) expect(conditionJoiner(m), m).toBe("hoặc");
    for (const m of ["and", "AND", "", undefined, null, 7]) expect(conditionJoiner(m), String(m)).toBe("và");
  });

  it("workflowActionText renders each safe action readably ($owner → chủ bản ghi)", () => {
    expect(workflowActionText({ type: "field_update", field: "status", value: "closed" }))
      .toBe("Cập nhật status = closed");
    expect(workflowActionText({ type: "field_update", field: "status" }))
      .toBe("Cập nhật status (không kèm giá trị)"); // NIT-1 — honest about the absent value
    expect(workflowActionText({ type: "in_app_notification", userId: "$owner", title: "Cơ hội thắng" }))
      .toBe("Thông báo cho chủ bản ghi: “Cơ hội thắng”");
    expect(workflowActionText({ type: "email_alert", to: "sales@example.com", subject: "Deal won" }))
      .toBe("Gửi email tới sales@example.com: “Deal won”");
    expect(workflowActionText({ type: "mystery_action" })).toBe("Hành động không nhận diện"); // no machine token (S13)
  });

  // DAILY-QUEUE #164: set_elapsed is off the propose_workflow schema but not on its denylist, so a
  // model can buffer one and Apply sends its `field` — the number the rule will overwrite on every
  // firing. The card is the human's only look before Apply, so it must name that field; the raw
  // token alone hid it. Any other off-schema action carrying a field shows it too.
  it("workflowActionText names the field an off-schema action will write (set_elapsed)", () => {
    expect(workflowActionText({ type: "set_elapsed", field: "resolution_minutes" }))
      .toBe("Ghi thời gian xử lý (phút giờ làm việc) vào resolution_minutes");
    expect(workflowActionText({ type: "mystery_action", field: "score" })).toBe("Hành động không nhận diện (trường score)");
  });

  it("layoutViewLabel maps the three viewTypes to Vietnamese and leaves unknown tokens raw", () => {
    expect(layoutViewLabel("edit")).toBe("Form nhập liệu");
    expect(layoutViewLabel("detail")).toBe("Trang chi tiết");
    expect(layoutViewLabel("list")).toBe("Danh sách");
    expect(layoutViewLabel("kanban")).toBe("kanban");
  });
});

describe("hasDrafts", () => {
  it("treats null / undefined / [] all as no-draft", () => {
    expect(hasDrafts(null)).toBe(false);
    expect(hasDrafts(undefined)).toBe(false);
    expect(hasDrafts([])).toBe(false);
  });
  it("is true only for a non-empty proposal array", () => {
    expect(hasDrafts([{ kind: "field", field: { name: "x", label: "X", fieldType: "text" } }])).toBe(true);
  });
});

describe("serverMessagesToTurns", () => {
  const rows: AiServerMessage[] = [
    { seq: 0, role: "user", content: "Tôi mở phòng khám nha khoa", draft: null, createdAt: "2026-07-04T10:00:00Z" },
    {
      seq: 1, role: "assistant", content: "Tôi đề xuất đối tượng **Bệnh nhân**.",
      draft: [{ kind: "entity", entity: { name: "benh_nhan", label: "Bệnh nhân", fields: [{ name: "ho_ten", label: "Họ tên", fieldType: "text", required: true }] } }],
      createdAt: "2026-07-04T10:00:05Z",
    },
  ];

  it("maps persisted rows to render turns, deriving a stable seq-based key", () => {
    const turns = serverMessagesToTurns(rows);
    expect(turns).toHaveLength(2);
    expect(turns[0]).toMatchObject({ id: "m_0", role: "user", content: "Tôi mở phòng khám nha khoa" });
    expect(turns[1]).toMatchObject({ id: "m_1", role: "assistant", content: "Tôi đề xuất đối tượng **Bệnh nhân**." });
  });

  it("carries an assistant row's decoded draft through so draft-cards re-render on resume", () => {
    const turns = serverMessagesToTurns(rows);
    expect(hasDrafts(turns[1].draft)).toBe(true);
    expect(turns[1].draft?.[0]).toMatchObject({ kind: "entity", entity: { name: "benh_nhan" } });
  });

  it("normalises a user row's draft to undefined (a user turn never carries a draft)", () => {
    const turns = serverMessagesToTurns(rows);
    expect(turns[0].draft).toBeUndefined();
  });

  it("sets draft:null for an assistant row with no persisted draft (a plain question)", () => {
    const turns = serverMessagesToTurns([
      { seq: 0, role: "assistant", content: "Anh cần quản lý gì?", draft: null, createdAt: "2026-07-04T10:00:00Z" },
    ]);
    expect(turns[0].draft).toBeNull();
  });

  it("preserves an assistant max_steps turn: empty content + a partial draft still renders", () => {
    const turns = serverMessagesToTurns([
      { seq: 0, role: "assistant", content: "", draft: [{ kind: "field", field: { entity: "benh_nhan", name: "sdt", label: "SĐT", fieldType: "text" } }], createdAt: "2026-07-04T10:00:00Z" },
    ]);
    expect(turns[0].content).toBe("");
    expect(hasDrafts(turns[0].draft)).toBe(true);
  });

  it("carries the outcome an assistant row was stored with, so a reopened turn reads as the live one (#30 item 5)", () => {
    const turns = serverMessagesToTurns([
      { seq: 0, role: "user", content: "chép tài khoản của không gian khác", draft: null, createdAt: "" },
      { seq: 1, role: "assistant", content: "", draft: null, createdAt: "", outcome: "refused" },
      { seq: 2, role: "user", content: "thêm bảng phiếu", draft: null, createdAt: "", outcome: "ok" },
      { seq: 3, role: "assistant", content: "", draft: [{ kind: "field", field: { entity: "benh_nhan", name: "sdt", label: "SĐT", fieldType: "text" } }], createdAt: "", outcome: "max_steps" },
      { seq: 4, role: "assistant", content: "Xong.", draft: null, createdAt: "" },
    ]);
    expect(turns.map((x) => x.outcome)).toEqual([undefined, "refused", undefined, "max_steps", undefined]);
  });

  it("coerces an unexpected role to 'user' and tolerates null/empty input", () => {
    const turns = serverMessagesToTurns([
      { seq: 0, role: "system", content: "x", draft: null, createdAt: "2026-07-04T10:00:00Z" },
    ]);
    expect(turns[0].role).toBe("user");
    expect(serverMessagesToTurns(null as unknown as AiServerMessage[])).toEqual([]);
    expect(serverMessagesToTurns([])).toEqual([]);
  });

  // E9 M5 — a persisted relationship draft resumes through the SAME path as entity/field drafts.
  it("carries a persisted kind:relationship draft through resume intact", () => {
    const turns = serverMessagesToTurns([
      {
        seq: 3, role: "assistant", content: "Đề xuất quan hệ N-N.",
        draft: [{ kind: "relationship", relationship: { name: "hoc_vien_khoa_hoc", entityA: "hoc_vien", entityB: "khoa_hoc" } }],
        createdAt: "2026-07-08T09:00:00Z",
      },
    ]);
    expect(hasDrafts(turns[0].draft)).toBe(true);
    expect(turns[0].draft?.[0]).toEqual({
      kind: "relationship",
      relationship: { name: "hoc_vien_khoa_hoc", entityA: "hoc_vien", entityB: "khoa_hoc" },
    });
  });

  // Review F3 pin at the mapper level: a persisted UNKNOWN kind must survive the mapper untouched
  // (rendering degrades in DraftCard) — the mapper must never filter or throw on it. ("workflow"
  // became a KNOWN kind in E9 M7, so the pin now uses a kind no build knows.)
  it("passes a persisted UNKNOWN-kind draft through without filtering or throwing", () => {
    const turns = serverMessagesToTurns([
      {
        seq: 4, role: "assistant", content: "Đề xuất tương lai.",
        draft: [{ kind: "dashboard" }],
        createdAt: "2026-07-08T09:00:05Z",
      },
    ]);
    expect(hasDrafts(turns[0].draft)).toBe(true);
    expect(turns[0].draft?.[0].kind).toBe("dashboard");
  });

  // E9 M6/M7 — persisted layout + workflow drafts resume through the same opaque path.
  it("carries persisted kind:layout and kind:workflow drafts through resume intact", () => {
    const layoutDraft = {
      kind: "layout",
      layout: { entity: "crm_contacts", viewType: "edit", layoutJson: { version: 1, sections: [] } },
    };
    const workflowDraft = {
      kind: "workflow",
      workflow: {
        name: "Đóng cơ hội thắng", entityName: "crm_opportunities", trigger: "after_update",
        actions: [{ type: "field_update", field: "status", value: "closed" }], isActive: true,
      },
    };
    const turns = serverMessagesToTurns([
      { seq: 5, role: "assistant", content: "Đề xuất bố cục + workflow.", draft: [layoutDraft, workflowDraft], createdAt: "2026-07-08T10:00:00Z" },
    ]);
    expect(turns[0].draft?.[0]).toEqual(layoutDraft);
    expect(turns[0].draft?.[1]).toEqual(workflowDraft);
  });

  // Apps Phase 3 — a persisted app draft resumes through the same opaque path.
  it("carries a persisted kind:app draft through resume intact", () => {
    const appDraft = {
      kind: "app",
      app: {
        name: "phong_kham", label: "Phòng khám", icon: "stethoscope",
        navEntities: ["benh_nhan", "lich_hen"],
      },
    };
    const turns = serverMessagesToTurns([
      { seq: 7, role: "assistant", content: "Đề xuất app.", draft: [appDraft], createdAt: "2026-07-10T09:00:00Z" },
    ]);
    expect(turns[0].draft?.[0]).toEqual(appDraft);
  });

  // E9 M8 — a persisted report draft resumes through the same opaque path.
  it("carries a persisted kind:report draft through resume intact", () => {
    const reportDraft = {
      kind: "report",
      report: {
        name: "Cơ hội theo giai đoạn", entityName: "crm_opportunities", type: "summary",
        config: { groupBy: "stage", metric: "count" }, isActive: true,
      },
    };
    const turns = serverMessagesToTurns([
      { seq: 6, role: "assistant", content: "Đề xuất báo cáo.", draft: [reportDraft], createdAt: "2026-07-08T11:00:00Z" },
    ]);
    expect(turns[0].draft?.[0]).toEqual(reportDraft);
  });
});

// E9 M4 (spec §12) — the 429 code → honest Vietnamese notice mapping. Retry-After seconds are
// best-effort (CORS): every code MUST read honestly both with and without them.
describe("aiLimitMessage", () => {
  it("AI_RATE_LIMITED with Retry-After seconds → includes '~Ns'", () => {
    expect(aiLimitMessage("AI_RATE_LIMITED", 42)).toBe("Đang gửi quá nhanh — thử lại sau ~42s.");
  });

  it("AI_RATE_LIMITED without seconds → honest seconds-free wording (no fake countdown)", () => {
    const msg = aiLimitMessage("AI_RATE_LIMITED");
    expect(msg).toBe("Đang gửi quá nhanh — chờ một chút rồi gửi lại.");
    expect(msg).not.toContain("~"); // never invent a number we don't have
  });

  it("rounds fractional seconds UP and rejects junk (0 / negative / NaN → seconds-free)", () => {
    expect(aiLimitMessage("AI_RATE_LIMITED", 2.3)).toContain("~3s");
    expect(aiLimitMessage("AI_RATE_LIMITED", 0)).not.toContain("~");
    expect(aiLimitMessage("AI_RATE_LIMITED", -7)).not.toContain("~");
    expect(aiLimitMessage("AI_RATE_LIMITED", Number.NaN)).not.toContain("~");
  });

  it("AI_CONCURRENCY_LIMIT → another-turn-running notice (fixed wording, Retry-After ignored)", () => {
    const msg = aiLimitMessage("AI_CONCURRENCY_LIMIT", 5);
    expect(msg).toBe("Đang có lượt AI Builder khác của tổ chức chạy — chờ xíu rồi gửi lại.");
    expect(aiLimitMessage("AI_CONCURRENCY_LIMIT")).toBe(msg);
  });

  it("AI_BUDGET_EXCEEDED → names the daily tenant budget + the 00:00 UTC reset", () => {
    const msg = aiLimitMessage("AI_BUDGET_EXCEEDED", 14400);
    expect(msg).toContain("ngân sách token AI");
    expect(msg).toContain("00:00 UTC");
    expect(msg).not.toContain("14400"); // hours-long waits are said as a reset time, not raw seconds
  });

  it("returns null for any non-limit code so callers fall back to errorMessage()", () => {
    expect(aiLimitMessage("AI_DISABLED")).toBeNull();
    expect(aiLimitMessage("AI_GATEWAY_ERROR", 30)).toBeNull();
    expect(aiLimitMessage("")).toBeNull();
  });
});

describe("formatTokens", () => {
  it("renders sub-1k as an integer, ≥1k as x.yk", () => {
    expect(formatTokens(449)).toBe("449");
    expect(formatTokens(2788)).toBe("2.8k");
    expect(formatTokens(1000)).toBe("1.0k");
  });
  it("guards 0 / negative / NaN", () => {
    expect(formatTokens(0)).toBe("0");
    expect(formatTokens(-5)).toBe("0");
    expect(formatTokens(Number.NaN)).toBe("0");
  });
});

// ── Nước 7 POWER — edit_field / delete_field / delete_entity / records ──────────────────────────

// The edit_field draft → PUT /admin/fields/:id patch mapper. Rebuilt key-by-key from `after` so
// nothing else leaks into the gated admin API, and ABSENT keys stay absent (apply-when-non-nil).
describe("draftEditFieldToUpdatePayload", () => {
  const base: AiDraftEditField = {
    entity: "benh_nhan",
    field: "phone",
    before: { label: "SĐT", isRequired: false },
    after: { label: "Số điện thoại", isRequired: true },
  };

  it("builds the patch from `after` only — changed keys, nothing from `before`", () => {
    expect(draftEditFieldToUpdatePayload(base)).toEqual({ label: "Số điện thoại", isRequired: true });
  });

  it("strips unknown keys a future wire might carry (exact known-key rebuild)", () => {
    const wire = {
      ...base,
      after: { label: "X", fieldOrder: 99, name: "hack", fieldType: "text" } as unknown as AiDraftEditField["after"],
    };
    expect(draftEditFieldToUpdatePayload(wire)).toEqual({ label: "X" });
  });

  it("passes defaultValue null through (explicit clear-the-default IS a value)", () => {
    const e: AiDraftEditField = { ...base, after: { defaultValue: null } };
    expect(draftEditFieldToUpdatePayload(e)).toEqual({ defaultValue: null });
  });

  it("tolerates a malformed draft with a missing after (empty no-op patch)", () => {
    const e = { entity: "a", field: "b", before: {} } as AiDraftEditField;
    expect(draftEditFieldToUpdatePayload(e)).toEqual({});
  });

  it("carries options through as the structured object (passthrough, not rebuilt)", () => {
    const options = { values: ["a", "b"] };
    const e: AiDraftEditField = { ...base, after: { options } };
    expect(draftEditFieldToUpdatePayload(e).options).toEqual(options);
  });
});

// The diff table model: one row per key present in `after` (the contract's changed-key set); a
// key only in `before` is NOT a row (rendering it would claim a change the draft doesn't propose).
describe("editFieldDiffRows", () => {
  it("emits one row per changed key with the VN label and trước → sau text", () => {
    const rows = editFieldDiffRows({
      entity: "benh_nhan", field: "phone",
      before: { label: "SĐT", isRequired: false },
      after: { label: "Số điện thoại", isRequired: true },
    });
    expect(rows).toEqual([
      { key: "label", label: "Nhãn", before: "SĐT", after: "Số điện thoại" },
      { key: "isRequired", label: "Bắt buộc", before: "không", after: "có" },
    ]);
  });

  it("a before-only key is NOT a row; a missing before-value renders undefined (— in the card)", () => {
    const rows = editFieldDiffRows({
      entity: "e", field: "f",
      before: { description: "cũ" }, // description NOT in after → no row
      after: { label: "Mới" },       // label not in before → before undefined
    });
    expect(rows).toEqual([{ key: "label", label: "Nhãn", before: undefined, after: "Mới" }]);
  });

  it("options render as compact JSON, never [object Object]", () => {
    const rows = editFieldDiffRows({
      entity: "e", field: "f",
      before: { options: { values: ["a"] } },
      after: { options: { values: ["a", "b"] } },
    });
    expect(rows[0].before).toBe('{"values":["a"]}');
    expect(rows[0].after).toBe('{"values":["a","b"]}');
  });

  it("tolerates missing before/after objects entirely", () => {
    expect(editFieldDiffRows({ entity: "e", field: "f" } as AiDraftEditField)).toEqual([]);
  });
});

// Tolerant container accessors — camelCase is the wire contract (project camelCase-JSON rule),
// the snake_case twin is accepted because drafts persist in draft_json across parallel builds.
describe("draft payload accessors (camelCase + snake_case twin)", () => {
  const ef = { entity: "e", field: "f", before: {}, after: { label: "X" } };

  it("reads the camelCase container key", () => {
    expect(getEditFieldDraft({ kind: "edit_field", editField: ef })).toEqual(ef);
  });

  it("reads the snake_case twin key", () => {
    const item = { kind: "edit_field", edit_field: ef } as unknown as AiDraftItem;
    expect(getEditFieldDraft(item)).toEqual(ef);
  });

  it("returns undefined for a non-object payload (malformed persisted draft → fallback card)", () => {
    expect(getEditFieldDraft({ kind: "edit_field", editField: "junk" as unknown as AiDraftEditField })).toBeUndefined();
    expect(getRecordsDraft({ kind: "records", records: [] as unknown as never })).toBeUndefined();
  });

  it("delete accessors mirror the same tolerance", () => {
    const df = { entity: "e", field: "f", label: "F", fieldType: "text" };
    const de = { entity: "e", label: "E", fieldCount: 3 };
    expect(getDeleteFieldDraft({ kind: "delete_field", deleteField: df })).toEqual(df);
    expect(getDeleteFieldDraft({ kind: "delete_field", delete_field: df } as unknown as AiDraftItem)).toEqual(df);
    expect(getDeleteEntityDraft({ kind: "delete_entity", deleteEntity: de })).toEqual(de);
    expect(getDeleteEntityDraft({ kind: "delete_entity", delete_entity: de } as unknown as AiDraftItem)).toEqual(de);
  });
});

// The rows a records draft submits + the preview table model — both share the same object-row
// filter so "N dòng" on the card is exactly the N the apply sends.
describe("records draft helpers", () => {
  const rows = [
    { name: "Anh", phone: "090" },
    { name: "Bình", email: "b@x.vn" },
    { name: "Chi" }, { name: "Dũng" }, { name: "Én" }, { name: "Phú" }, { name: "Giang" },
  ];

  it("draftRecordsRows keeps object rows only (malformed entries never reach the API)", () => {
    const dirty = [rows[0], "junk", null, 7, [1, 2], rows[1]] as unknown as Array<Record<string, unknown>>;
    expect(draftRecordsRows({ entity: "benh_nhan", rows: dirty })).toEqual([rows[0], rows[1]]);
  });

  it("recordsPreviewModel: first 5 rows, honest moreCount, column union in first-seen order", () => {
    const m = recordsPreviewModel(rows);
    expect(m.rows).toHaveLength(5);
    expect(m.moreCount).toBe(2);
    expect(m.total).toBe(7);
    expect(m.columns).toEqual(["name", "phone", "email"]); // union of PREVIEW rows, first-seen order
  });

  it("recordsPreviewModel tolerates null/undefined/empty", () => {
    expect(recordsPreviewModel(null)).toEqual({ columns: [], rows: [], moreCount: 0, total: 0 });
    expect(recordsPreviewModel(undefined).total).toBe(0);
  });

  it("recordCellText: absent key → —, structures → JSON, null spelled out", () => {
    expect(recordCellText(undefined)).toBe("—");
    expect(recordCellText("Anh")).toBe("Anh");
    expect(recordCellText(42)).toBe("42");
    expect(recordCellText(null)).toBe("null");
    expect(recordCellText({ amount: 5 })).toBe('{"amount":5}');
  });
});

// The honest per-row bulk-apply summary — null on full success, named failing rows otherwise.
describe("recordsApplyMessage", () => {
  it("returns null when every row was created (caller shows plain success)", () => {
    expect(recordsApplyMessage(3, 3, [
      { index: 0, status: "created" }, { index: 1, status: "created" }, { index: 2, status: "created" },
    ])).toBeNull();
  });

  it("names failing rows 1-based with the server's own message", () => {
    expect(recordsApplyMessage(1, 3, [
      { index: 0, status: "created" },
      { index: 1, status: "failed", error: "Thiếu thông tin bắt buộc", code: "RECORD_REQUIRED_MISSING" },
      { index: 2, status: "failed", code: "RECORD_DUPLICATE" }, // no error text → code shown
    ])).toBe("Đã tạo 1/3 dòng — 2 dòng lỗi: dòng 2: Thiếu thông tin bắt buộc · dòng 3: RECORD_DUPLICATE.");
  });

  it("caps the detail at 3 rows with an honest +N lỗi khác", () => {
    const results = [0, 1, 2, 3, 4].map((i) => ({ index: i, status: "failed", error: `lỗi ${i}` }));
    const msg = recordsApplyMessage(0, 5, results);
    expect(msg).toContain("5 dòng lỗi");
    expect(msg).toContain("dòng 1: lỗi 0");
    expect(msg).toContain("dòng 3: lỗi 2");
    expect(msg).not.toContain("dòng 4");
    expect(msg).toContain("(+2 lỗi khác)");
  });
});

// applyAll's dependency order + the destructive-skip predicate.
describe("draftApplyRank / isDestructiveDraft", () => {
  it("orders entity < schema kinds (incl. edit/delete) < records < app", () => {
    expect(draftApplyRank("entity")).toBeLessThan(draftApplyRank("field"));
    expect(draftApplyRank("field")).toBe(draftApplyRank("edit_field"));
    expect(draftApplyRank("delete_field")).toBe(1);
    expect(draftApplyRank("field")).toBeLessThan(draftApplyRank("records"));
    expect(draftApplyRank("records")).toBeLessThan(draftApplyRank("app"));
  });

  it("flags ONLY delete kinds WITH a payload as destructive", () => {
    expect(isDestructiveDraft({ kind: "delete_field", deleteField: { entity: "e", field: "f", label: "F", fieldType: "text" } })).toBe(true);
    expect(isDestructiveDraft({ kind: "delete_entity", deleteEntity: { entity: "e", label: "E", fieldCount: 2 } })).toBe(true);
    expect(isDestructiveDraft({ kind: "delete_field" })).toBe(false); // no payload → honest apply error, not a dialog
    expect(isDestructiveDraft({ kind: "edit_field", editField: { entity: "e", field: "f", before: {}, after: {} } })).toBe(false);
    expect(isDestructiveDraft({ kind: "records", records: { entity: "e", rows: [] } })).toBe(false);
  });
});

// ── AI-MODULES — approval / dashboard / workqueue draft kinds (parallel-BE contract) ────────────

// The approval draft → POST /admin/approval-processes body mapper. The load-bearing conversion
// is the step key RENAME approverRef → approverId; steps are rebuilt key-by-key so nothing else
// leaks into the gated admin API.
describe("draftApprovalToPayload", () => {
  const base: AiDraftApproval = {
    name: "Duyệt giảm giá",
    entityName: "crm_opportunities",
    steps: [
      { approverType: "user", approverRef: "u-1", slaSeconds: 3600 },
      { approverType: "role", approverRef: "r-9" },
    ],
  };

  it("maps the full body, renaming approverRef → approverId and defaulting trigger/isActive", () => {
    expect(draftApprovalToPayload(base)).toEqual({
      name: "Duyệt giảm giá",
      entityName: "crm_opportunities",
      trigger: "after_create", // the manual builder's own default when the wire omits it
      steps: [
        { approverType: "user", approverId: "u-1", slaSeconds: 3600 },
        { approverType: "role", approverId: "r-9" },
      ],
      isActive: true,
    });
  });

  it("passes a PRESENT trigger through raw (server validates — no mirrored rules)", () => {
    expect(draftApprovalToPayload({ ...base, trigger: "after_update" }).trigger).toBe("after_update");
  });

  it("includes criteria only when non-empty; empty/absent stays absent", () => {
    const criteria = { match: "and", rules: [{ field: "amount", op: "gt", value: "1000" }] };
    expect(draftApprovalToPayload({ ...base, criteria }).criteria).toEqual(criteria);
    expect(draftApprovalToPayload(base).criteria).toBeUndefined();
    expect(draftApprovalToPayload({ ...base, criteria: {} }).criteria).toBeUndefined();
  });

  it("carries a step's onReject to the apply body, single-approver and council steps alike (specs/043 S8)", () => {
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { id: "tp", approverType: "role", approverRef: "r-1" },
        { approverType: "role", approverRef: "r-2", onReject: { to: "@submitter", resumeAt: "start" } },
        { approvers: [{ type: "role", ref: "r-3" }, { type: "user", ref: "u-4" }], onReject: { to: " tp " } } as never,
        { approverType: "role", approverRef: "r-5", onReject: { to: " " } },
      ],
    });
    expect(p.steps[0]).not.toHaveProperty("onReject");
    expect(p.steps[1]).toMatchObject({ onReject: { to: "@submitter", resumeAt: "start" } });
    expect(p.steps[2]).toMatchObject({ onReject: { to: "tp" } });
    expect(p.steps[3]).not.toHaveProperty("onReject"); // a blank target is dropped, not sent
  });

  it("tolerates the approverId twin key and strips junk SLA values", () => {
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { approverType: "user", approverId: "u-7" },              // twin key, no approverRef
        { approverType: "user", approverRef: "u-8", slaSeconds: -5 }, // junk SLA → dropped
      ],
    });
    expect(p.steps).toEqual([
      { approverType: "user", approverId: "u-7" },
      { approverType: "user", approverId: "u-8" },
    ]);
  });

  it("sends a many-approver step as approvers[] + completion with NO single approver (specs/043 S4)", () => {
    // The server refuses approverType/approverId beside approvers (one source of approver); each
    // ref renames to `id` exactly like approverRef does. Junk modes stay out — the server validates.
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { approverType: "role", approverRef: "r-1" },
        {
          id: "hoi-dong", name: " Hội đồng ", approverType: "", slaSeconds: 7200, slaClock: "wall",
          approvers: [{ type: "role", ref: "r-2" }, { type: "user", ref: "u-3" }, { type: "weird", id: "x-4" }],
          completion: "any",
        },
        { approverType: "", approvers: [{ type: "role", ref: "r-5" }, { type: "role", ref: "r-6" }], completion: "quorum" },
      ],
    });
    expect(p.steps).toEqual([
      { approverType: "role", approverId: "r-1" },
      {
        id: "hoi-dong", name: "Hội đồng", slaSeconds: 7200, slaClock: "wall",
        approvers: [{ type: "role", id: "r-2" }, { type: "user", id: "u-3" }, { type: "user", id: "x-4" }],
        completion: { mode: "any" },
      },
      // a quorum with no percent still goes out as a quorum — the server refuses it (422)
      { approvers: [{ type: "role", id: "r-5" }, { type: "role", id: "r-6" }], completion: { mode: "quorum" } },
    ]);
  });

  it("sends a quorum council with its percent, and passes an unusable percent through for the server to refuse (specs/043 S6b)", () => {
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { approverType: "", approvers: [{ type: "role", ref: "r-5" }, { type: "role", ref: "r-6" }, { type: "role", ref: "r-7" }], completion: "quorum", completionPercent: 60 },
        { approverType: "", approvers: [{ type: "role", ref: "r-5" }, { type: "role", ref: "r-6" }], completion: "quorum", completionPercent: 60.5 },
      ],
    });
    expect(p.steps[0]).toMatchObject({ completion: { mode: "quorum", percent: 60 } });
    expect(p.steps[1]).toMatchObject({ completion: { mode: "quorum", percent: 60.5 } });
    expect(approvalStepText({ approverType: "", approvers: [{ type: "role", ref: "r-2" }, { type: "role", ref: "r-3" }], completion: "quorum", completionPercent: 60 }, 0))
      .toBe("1. [role:r-2, role:r-3] · đủ 60% số người duyệt");
  });

  it("describes a council step with its approvers and policy", () => {
    expect(approvalStepText({ approverType: "", approvers: [{ type: "role", ref: "r-2" }, { type: "user", ref: "u-3" }], completion: "all", slaSeconds: 3600 }, 1))
      .toBe("2. [role:r-2, user:u-3] · tất cả phải duyệt · SLA 1 giờ");
    expect(approvalStepText({ approverType: "", approvers: [{ type: "role", ref: "r-2" }], completion: "any" }, 0))
      .toBe("1. [role:r-2] · người đầu tiên quyết");
  });

  it("carries the step's stable id and name through to Apply (specs/043 S0)", () => {
    // The server assigns/validates ids and the AI draft buffers them; rebuilding the step from
    // approverType/approverId/slaSeconds alone lost them at Apply (and lost the step name too).
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { id: "truong-phong", name: "Trưởng phòng", approverType: "role", approverRef: "r-1" },
        { id: "", approverType: "user", approverRef: "u-2" }, // blank id → omitted, server fills
      ],
    });
    expect(p.steps).toEqual([
      { id: "truong-phong", name: "Trưởng phòng", approverType: "role", approverId: "r-1" },
      { approverType: "user", approverId: "u-2" },
    ]);
  });

  it("carries a step's slaClock to Apply (specs/043 S1) — junk stays out", () => {
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { approverType: "user", approverRef: "u-1", slaSeconds: 14400, slaClock: "wall" },
        { approverType: "user", approverRef: "u-2", slaClock: "gio" },
      ],
    });
    expect(p.steps).toEqual([
      { approverType: "user", approverId: "u-1", slaSeconds: 14400, slaClock: "wall" },
      { approverType: "user", approverId: "u-2" },
    ]);
  });

  it("strips extra step keys a future wire might carry (structural pin)", () => {
    const p = draftApprovalToPayload({
      ...base,
      steps: [{ approverType: "user", approverRef: "u-1", webhookUrl: "https://evil" } as never],
    });
    expect(p.steps).toEqual([{ approverType: "user", approverId: "u-1" }]);
  });

  it("keeps a proposed step name (#69) — trimmed, and absent when blank", () => {
    // propose_approval's schema offers `name` per step ("Trưởng phòng duyệt"); rebuilding steps
    // key-by-key used to drop it, so an AI-built process reached the approval screen nameless.
    const p = draftApprovalToPayload({
      ...base,
      steps: [
        { name: "  Trưởng phòng duyệt ", approverType: "role", approverRef: "r-1" },
        { name: "   ", approverType: "user", approverRef: "u-1" },
      ],
    });
    expect(p.steps).toEqual([
      { name: "Trưởng phòng duyệt", approverType: "role", approverId: "r-1" },
      { approverType: "user", approverId: "u-1" },
    ]);
  });

  it("honours an explicit isActive:false and tolerates a step-less draft", () => {
    const p = draftApprovalToPayload({ ...base, steps: undefined as never, isActive: false });
    expect(p.steps).toEqual([]);
    expect(p.isActive).toBe(false);
  });
});

describe("approval display helpers", () => {
  it("slaText speaks phút/giờ/ngày and returns '' for junk (nothing invented)", () => {
    expect(slaText(1800)).toBe("30 phút");
    expect(slaText(3600)).toBe("1 giờ");
    expect(slaText(7200)).toBe("2 giờ");
    expect(slaText(172800)).toBe("2 ngày");
    expect(slaText(undefined)).toBe("");
    expect(slaText(0)).toBe("");
    expect(slaText(-60)).toBe("");
    expect(slaText(Number.NaN)).toBe("");
  });

  it("approvalStepText renders 'n. type:ref · SLA …' and omits an absent SLA", () => {
    expect(approvalStepText({ approverType: "user", approverRef: "u-1", slaSeconds: 3600 }, 0))
      .toBe("1. user:u-1 · SLA 1 giờ");
    expect(approvalStepText({ approverType: "role", approverRef: "r-2" }, 1)).toBe("2. role:r-2");
    expect(approvalStepText({ approverType: "user", approverId: "u-9" }, 2)).toBe("3. user:u-9"); // twin key
    expect(approvalStepText({ approverType: "user" }, 0)).toBe("1. user:?"); // honest, never invented
  });

  it("approvalCriteriaRules reads the well-formed rules of the opaque criteria, skipping junk", () => {
    expect(approvalCriteriaRules({ match: "or", rules: [{ field: "stage", op: "changedTo", value: "won" }, { field: "note", op: "isnull" }] }))
      .toEqual([{ field: "stage", op: "changedTo", value: "won" }, { field: "note", op: "isnull", value: undefined }]);
    expect(approvalCriteriaRules({ rules: [{ f: 1 }, null, "x", { field: "a", op: 3 }] })).toEqual([]);
    expect(approvalCriteriaRules({ rules: "junk" })).toEqual([]);
    expect(approvalCriteriaRules(undefined)).toEqual([]);
  });

  it("approvalCriteriaCount counts rules and treats absent/opaque as 0 (mọi bản ghi)", () => {
    expect(approvalCriteriaCount({ match: "and", rules: [{ f: 1 }, { f: 2 }] })).toBe(2);
    expect(approvalCriteriaCount({})).toBe(0);
    expect(approvalCriteriaCount(undefined)).toBe(0);
    expect(approvalCriteriaCount({ rules: "junk" })).toBe(0);
  });
});

// The dashboard draft → createDashboard() argument mapper. Widgets rebuilt key-by-key; a widget
// without an id gets a client id (idGen injected here for determinism).
describe("draftDashboardToPayload", () => {
  const seq = () => {
    let n = 0;
    return () => `wid-${++n}`;
  };

  it("maps name/description and rebuilds widgets, backfilling missing ids only", () => {
    const d: AiDraftDashboard = {
      name: "Tổng quan bán hàng",
      description: "Doanh thu theo tháng",
      widgets: [
        { id: "keep-1", title: "Doanh thu", type: "kpi", entity: "crm_opportunities", metric: "sum", field: "amount" },
        { title: "Theo giai đoạn", type: "bar", entity: "crm_opportunities", metric: "count", groupBy: "stage" },
      ],
    };
    expect(draftDashboardToPayload(d, seq())).toEqual({
      name: "Tổng quan bán hàng",
      description: "Doanh thu theo tháng",
      widgets: [
        { id: "keep-1", title: "Doanh thu", type: "kpi", entity: "crm_opportunities", metric: "sum", field: "amount" },
        { id: "wid-1", title: "Theo giai đoạn", type: "bar", entity: "crm_opportunities", metric: "count", groupBy: "stage" },
      ],
    });
  });

  it("keeps absent widget keys ABSENT and passes bucket/x/y/w/h/filters through when present", () => {
    const d: AiDraftDashboard = {
      name: "B",
      widgets: [{
        title: "T", type: "line", entity: "e", metric: "count",
        groupBy: "created_at", bucket: "month", x: 0, y: 0, w: 6, h: 4,
        filters: [{ field: "stage", op: "eq", values: ["won"] }],
      }],
    };
    const w = draftDashboardToPayload(d, seq()).widgets[0] as unknown as Record<string, unknown>;
    expect(w.bucket).toBe("month");
    expect(w.w).toBe(6);
    expect(w.filters).toEqual([{ field: "stage", op: "eq", values: ["won"] }]);
    expect("field" in w).toBe(false); // absent stays absent
  });

  it("strips unknown widget keys (structural pin) and tolerates junk entries + empty widgets", () => {
    const d = {
      name: "X",
      widgets: [{ title: "T", type: "kpi", entity: "e", metric: "count", url: "https://evil" }, "junk"],
    } as unknown as AiDraftDashboard;
    const p = draftDashboardToPayload(d, seq());
    expect(p.widgets[0]).toEqual({ id: "wid-1", title: "T", type: "kpi", entity: "e", metric: "count" });
    expect(p.widgets[1]).toEqual({ id: "wid-2" }); // junk entry degrades to an id-only stub, server 422s honestly
    expect(draftDashboardToPayload({ name: "Y", widgets: undefined as never }, seq()).widgets).toEqual([]);
    expect(draftDashboardToPayload({ name: "Y", widgets: [] }, seq()).description).toBeUndefined();
  });

  it("dashboardWidgetLines: titles/types for the card, positional fallback title, '?' type", () => {
    expect(dashboardWidgetLines({
      name: "D",
      widgets: [{ title: "Doanh thu", type: "kpi" }, { type: "bar" }, {}],
    })).toEqual([
      { title: "Doanh thu", type: "kpi" },
      { title: "Widget 2", type: "bar" },
      { title: "Widget 3", type: "?" },
    ]);
    expect(dashboardWidgetLines({ name: "D", widgets: undefined as never })).toEqual([]);
  });
});

// The workqueue draft → WorkQueueInput mapper + the apply-time role resolver.
describe("draftWorkQueueToPayload / resolveRoleByName", () => {
  const base: AiDraftWorkQueue = {
    name: "hop_dong_cho_duyet",
    entityName: "crm_contracts",
    filters: [{ field: "status", op: "eq", value: "pending" }],
    roleName: "sales",
  };

  it("maps the body with label→name fallback, rebuilt filters, open verb and isActive default", () => {
    expect(draftWorkQueueToPayload(base, "role-1")).toEqual({
      name: "hop_dong_cho_duyet",
      label: "hop_dong_cho_duyet", // no wire label → machine name doubles as display
      entityName: "crm_contracts",
      filters: [{ field: "status", op: "eq", value: "pending" }],
      actionVerb: "open",
      isActive: true,
      roleId: "role-1",
    });
  });

  it("keeps an explicit label, omits roleId when unresolved-less, and maps showOnHome→isActive", () => {
    const p = draftWorkQueueToPayload({ ...base, label: "Hợp đồng chờ duyệt", roleName: undefined, showOnHome: false });
    expect(p.label).toBe("Hợp đồng chờ duyệt");
    expect(p.isActive).toBe(false);
    expect("roleId" in p).toBe(false);
  });

  it("null-op filters carry value '' (the queue dialect's required string) and junk filters degrade", () => {
    const p = draftWorkQueueToPayload({
      ...base,
      filters: [{ field: "note", op: "isnull" }, { field: "x", op: "eq", value: 7 as never }],
    });
    expect(p.filters).toEqual([
      { field: "note", op: "isnull", value: "" },
      { field: "x", op: "eq", value: "" }, // non-string value never leaks a wrong type
    ]);
    expect(draftWorkQueueToPayload({ ...base, filters: undefined }).filters).toEqual([]);
  });

  it("resolveRoleByName matches NAME then LABEL case-insensitively; no match → undefined", () => {
    const roles = [
      { id: "r-1", name: "sales", label: "Kinh doanh" },
      { id: "r-2", name: "support" },
    ];
    expect(resolveRoleByName("Sales", roles)).toBe("r-1");
    expect(resolveRoleByName("kinh doanh", roles)).toBe("r-1");
    expect(resolveRoleByName("support", roles)).toBe("r-2");
    expect(resolveRoleByName("marketing", roles)).toBeUndefined();
    expect(resolveRoleByName("  ", roles)).toBeUndefined();
  });

  it("workQueueSummaryText digests filters/role/home visibility honestly", () => {
    expect(workQueueSummaryText(base)).toBe("1 điều kiện lọc · vai sales · hiện trên Trang chủ");
    expect(workQueueSummaryText({ name: "q", entityName: "e", showOnHome: false }))
      .toBe("mọi bản ghi · mọi người thấy · ẩn khỏi Trang chủ");
  });
});

// Tolerant container accessors + applyAll rank for the three module kinds.
describe("AI-MODULES accessors + rank", () => {
  it("accessors read the container key and reject non-object payloads (fallback card)", () => {
    const ap = { name: "A", entityName: "e", steps: [] };
    const db = { name: "D", widgets: [] };
    const wq = { name: "Q", entityName: "e" };
    expect(getApprovalDraft({ kind: "approval", approval: ap })).toEqual(ap);
    expect(getDashboardDraft({ kind: "dashboard", dashboard: db })).toEqual(db);
    expect(getWorkQueueDraft({ kind: "workqueue", workqueue: wq })).toEqual(wq);
    // camelCase twin for the one-word kind (persisted parallel-build tolerance)
    expect(getWorkQueueDraft({ kind: "workqueue", workQueue: wq } as unknown as AiDraftItem)).toEqual(wq);
    expect(getApprovalDraft({ kind: "approval" })).toBeUndefined();
    expect(getDashboardDraft({ kind: "dashboard", dashboard: "junk" as unknown as AiDraftDashboard })).toBeUndefined();
  });

  it("approval/dashboard/workqueue are rank-1 (after entities, before records/app) and NOT destructive", () => {
    for (const kind of ["approval", "dashboard", "workqueue"]) {
      expect(draftApplyRank("entity")).toBeLessThan(draftApplyRank(kind));
      expect(draftApplyRank(kind)).toBe(1);
      expect(draftApplyRank(kind)).toBeLessThan(draftApplyRank("records"));
      expect(draftApplyRank(kind)).toBeLessThan(draftApplyRank("app"));
    }
    expect(isDestructiveDraft({ kind: "approval", approval: { name: "A", entityName: "e", steps: [] } })).toBe(false);
    expect(isDestructiveDraft({ kind: "workqueue", workqueue: { name: "Q", entityName: "e" } })).toBe(false);
  });
});

// The model names approvers by role NAME / user EMAIL; Apply turns them into ids — storing the name
// itself would make a step (a whole council) that nobody holds.
describe("resolveApprovalApprovers", () => {
  const roles = [{ id: "r-mgr", name: "manager", label: "Quản lý" }, { id: "r-dir", name: "director" }];
  const users = [{ id: "u-1", email: "Duyet1@example.com" }];
  const draft = {
    name: "Duyệt báo giá", entityName: "crm_quotes",
    steps: [
      { approverType: "role", approverRef: "Manager" },
      { id: "hoi-dong", approverType: "", approvers: [{ type: "role", ref: "director" }, { type: "user", ref: "duyet1@example.com" }], completion: "all" },
    ],
  };

  it("turns names and emails into ids, for single and council steps", () => {
    const r = resolveApprovalApprovers(draft, roles, users);
    if ("error" in r) throw new Error(r.error);
    const body = draftApprovalToPayload(r.draft);
    expect(body.steps[0]).toMatchObject({ approverType: "role", approverId: "r-mgr" });
    expect(body.steps[1]).toMatchObject({ approvers: [{ type: "role", id: "r-dir" }, { type: "user", id: "u-1" }], completion: { mode: "all" } });
    expect(approvalDraftRefs(draft).map((x) => x.ref)).toEqual(["Manager", "director", "duyet1@example.com"]);
  });

  it("a ref that is already a known id passes through", () => {
    const r = resolveApprovalApprovers({ ...draft, steps: [{ approverType: "user", approverRef: "u-1" }] }, roles, users);
    expect("draft" in r && r.draft.steps[0].approverId).toBe("u-1");
  });

  it("refuses an unknown role or user by name instead of storing it", () => {
    const noRole = resolveApprovalApprovers({ ...draft, steps: [{ approverType: "role", approverRef: "ghost" }] }, roles, users);
    expect(noRole).toEqual({ error: expect.stringContaining('vai "ghost"') });
    const noUser = resolveApprovalApprovers(draft, roles, []);
    expect(noUser).toEqual({ error: expect.stringContaining('người dùng "duyet1@example.com"') });
  });
});

describe("change preview (L10-4a)", () => {
  it("changePathText reads array length and element paths", () => {
    expect(changePathText("actions[1]")).toBe("hành động 2");
    expect(changePathText("actions")).toBe("các hành động (số lượng đổi)");
    expect(changePathText("condition.rules[0].value")).toBe("điều kiện 1 · giá trị");
    expect(changePathText("(root)")).toBe("toàn bộ");
  });
  it("workflowActionText describes a webhook without exposing a key", () => {
    expect(workflowActionText({ type: "outbound_webhook", url: "https://api.partner.vn" })).toBe("Gọi webhook tới https://api.partner.vn");
    expect(workflowActionText({ type: "outbound_webhook", credentialId: "c1", url: "https://x" })).toBe("Gọi webhook qua khoá kết nối");
  });
  it("serverMessagesToTurns flags reopened assistant turns", () => {
    const t = serverMessagesToTurns([{ seq: 1, role: "assistant", content: "x", draft: null } as never]);
    expect(t[0].reopened).toBe(true);
  });
});

describe("changeSideLines — app menu", () => {
  it("After marks rows the Before side lacks with '+ '", async () => {
    const { changeSideLines } = await import("@/lib/aiBuilder");
    const before = { nav: [{ type: "dashboard", name: "d", label: "Tổng quan" }, { type: "entity", name: "patients", label: "Hồ sơ" }] };
    const after = { nav: [...before.nav, { type: "entity", name: "visits", label: "" }] };
    expect(changeSideLines("app", before)).toEqual(["Tổng quan", "Hồ sơ"]);
    expect(changeSideLines("app", after, before)).toEqual(["Tổng quan", "Hồ sơ", "+ visits"]);
  });
});
