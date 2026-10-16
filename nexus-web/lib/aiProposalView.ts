// proposalView — one assistant draft as plain card data (specs/076 ai2): a title, a "where" line and a short list of
// "+ ~ − =" changes, for all 14 draft kinds. Pure text, no React: the ProposalCard only lays it out. Business words
// come from the dictionary (S13); a kind this build cannot read yields null (the card then shows the dashed fallback).
import type { ChangeItem } from "@/components/studio/ChangeList";
import { FIELD_TYPE_META, type FieldType } from "@/lib/metadata";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import type { RouteId } from "@/lib/studioRoutes";
import {
  approvalCriteriaCount, approvalCriteriaRules, approvalStepText, changePathText, changeSideLines, conditionJoiner,
  dashboardWidgetLines, draftRecordsRows, editFieldDiffRows, getApprovalDraft, getDashboardDraft, getDeleteEntityDraft,
  getDeleteFieldDraft, getEditFieldDraft, getRecordsDraft, getWorkQueueDraft, layoutViewLabel, recordCellText,
  recordsPreviewModel, reportSummaryText, reportTypeLabel, summarizeLayoutDraft, workflowActionText, workflowRuleText,
  workflowTriggerPhrase, workQueueSummaryText, type AiDraftItem,
} from "@/lib/aiBuilder";

export type ProposalOp = "create" | "update" | "delete";

export interface ProposalView {
  kind: string;
  title: string;
  where: string;
  /** At most five "changed place" rows when the server sent `change` for an update; else the per-kind rows. */
  changes: ChangeItem[];
  /** "+{n} chỗ khác" count past the five shown (update only). */
  moreCount: number;
  /** Set only when the server sent `change`: the Tạo mới / Sửa / Xoá label. Absent ⇒ no label, no Trước/Sau. */
  op?: ProposalOp;
  before?: string[];
  after?: string[];
  /** records: first rows as text, for the small preview table. */
  table?: { columns: string[]; rows: string[][]; moreCount: number; total: number };
  destructive: boolean;
}

const typeLabel = (ft: string) => FIELD_TYPE_META[ft as FieldType]?.label ?? ft;
const V = (k: string, vars: Record<string, string | number> = {}) => tf(`studio.assistant.view.${k}` as I18nKey, vars);
const joiner = (match: unknown) => t(conditionJoiner(match) === "hoặc" ? "studio.assistant.view.or" : "studio.assistant.view.and");
const bound = (label: string, type: string, required?: boolean) => V(required ? "fieldRequired" : "field", { label, type });

interface Ctx { delivered?: boolean }

export function proposalView(item: AiDraftItem, ctx: Ctx = {}): ProposalView | null {
  const where = (w: string) => (ctx.delivered === false ? tf("studio.assistant.where.hidden", { where: w }) : w);
  const table = (x: string) => where(tf("studio.assistant.where.table", { x }));
  const base = (v: Omit<ProposalView, "kind" | "moreCount" | "destructive"> & { destructive?: boolean }): ProposalView =>
    withChange(item, { ...v, kind: item.kind, moreCount: 0, destructive: v.destructive ?? false });

  if (item.kind === "entity" && item.entity) {
    const e = item.entity;
    return base({
      title: V("entity", { name: e.label }),
      where: e.appName ? where(`App ${e.appName}`) : table(e.label),
      changes: e.fields.map((f) => ({ op: "add" as const, text: bound(f.label, typeLabel(f.fieldType), f.required) })),
    });
  }
  if (item.kind === "field" && item.field) {
    const f = item.field;
    return base({
      title: V("field.title", { name: f.label }),
      where: table(f.entity ?? ""),
      changes: [{ op: "add", text: bound(f.label, typeLabel(f.fieldType), f.required) }],
    });
  }
  if (item.kind === "relationship" && item.relationship) {
    const r = item.relationship;
    return base({
      title: V("relationship", { a: r.entityA, b: r.entityB }),
      where: where(tf("studio.assistant.where.tables", { a: r.entityA, b: r.entityB })),
      changes: [
        { op: "add", text: V("relationship.many", { name: r.name }) },
        { op: "add", text: V("relationship.join") },
      ],
    });
  }
  if (item.kind === "layout" && item.layout) {
    const l = item.layout;
    const sum = summarizeLayoutDraft(l.viewType, l.layoutJson);
    const changes: ChangeItem[] =
      sum.kind === "sections"
        ? sum.sections.map((s) => ({ op: "change" as const, text: V("layout.section", { title: s.title, cols: s.columns, n: s.fields.length }) }))
        : sum.kind === "columns"
          ? [{ op: "change", text: V("layout.columns", { cols: sum.columns.join(", ") }) }]
          : [{ op: "same", text: V("layout.opaque") }];
    return base({ title: V("layout.title", { view: layoutViewLabel(l.viewType).toLowerCase() }), where: table(l.entity), changes });
  }
  if (item.kind === "workflow" && item.workflow) {
    const w = item.workflow;
    const rules = w.condition?.rules ?? [];
    const join = ` ${joiner(w.condition?.match)} `;
    return base({
      title: V("workflow", { name: w.name }),
      where: table(w.entityName),
      changes: [
        { op: "add", text: V("when", { x: workflowTriggerPhrase(w.trigger) }) },
        ...(rules.length > 0 ? [{ op: "add" as const, text: V("if", { x: rules.map(workflowRuleText).join(join) }) }] : []),
        ...(w.actions ?? []).map((a) => ({ op: "add" as const, text: V("then", { x: workflowActionText(a) }) })),
      ],
    });
  }
  if (item.kind === "report" && item.report) {
    const r = item.report;
    const cfg = r.config ?? {};
    const lead = reportSummaryText(r.type, cfg);
    const n = Array.isArray(cfg.filters) ? cfg.filters.length : 0;
    return base({
      title: V("report", { name: r.name }),
      where: table(r.entityName),
      changes: [
        { op: "add", text: lead ? V("report.lead", { type: reportTypeLabel(r.type), lead }) : reportTypeLabel(r.type) },
        ...(n > 0 ? [{ op: "add" as const, text: V("filters", { n }) }] : []),
      ],
    });
  }
  if (item.kind === "app" && item.app) {
    const a = item.app;
    const nav = a.navEntities ?? [];
    return base({
      title: V("app", { name: a.label }),
      where: t("studio.assistant.where.menu"),
      changes: [
        ...(a.description ? [{ op: "add" as const, text: a.description }] : []),
        { op: "add", text: nav.length > 0 ? V("app.menu", { list: nav.join(" · ") }) : V("app.noMenu") },
      ],
    });
  }
  const ef = item.kind === "edit_field" ? getEditFieldDraft(item) : undefined;
  if (ef) {
    const rows = editFieldDiffRows(ef);
    return base({
      title: V("editField", { name: ef.after?.label ?? ef.before?.label ?? ef.field }),
      where: table(ef.entity),
      changes: rows.length === 0
        ? [{ op: "same", text: V("editField.none") }]
        : rows.map((r) => ({ op: "change" as const, text: V("editField.row", { label: r.label, before: r.before ?? "—", after: r.after }), key: r.key })),
    });
  }
  const df = item.kind === "delete_field" ? getDeleteFieldDraft(item) : undefined;
  if (df) {
    return base({
      title: V("deleteField", { name: df.label || df.field }),
      where: table(df.entity),
      changes: [{ op: "remove", text: bound(df.label || df.field, typeLabel(df.fieldType)) }],
      destructive: true,
    });
  }
  const de = item.kind === "delete_entity" ? getDeleteEntityDraft(item) : undefined;
  if (de) {
    return base({
      title: V("deleteEntity", { name: de.label || de.entity }),
      where: table(de.label || de.entity),
      changes: [
        { op: "remove", text: V("deleteEntity.fields", { n: de.fieldCount }) },
        { op: "remove", text: V("deleteEntity.rows") },
      ],
      destructive: true,
    });
  }
  const rc = item.kind === "records" ? getRecordsDraft(item) : undefined;
  if (rc) {
    const n = draftRecordsRows(rc).length;
    const pv = recordsPreviewModel(rc.rows);
    return base({
      title: V("records", { n }),
      where: table(rc.entity),
      changes: [{ op: "add", text: n === 0 ? V("records.none") : V("records.count", { n }) }],
      table: n === 0 ? undefined : {
        columns: pv.columns,
        rows: pv.rows.map((r) => pv.columns.map((c) => recordCellText(r[c]))),
        moreCount: pv.moreCount,
        total: pv.total,
      },
    });
  }
  const ap = item.kind === "approval" ? getApprovalDraft(item) : undefined;
  if (ap) {
    const steps = Array.isArray(ap.steps) ? ap.steps : [];
    const rules = approvalCriteriaRules(ap.criteria);
    const crit = approvalCriteriaCount(ap.criteria);
    const join = ` ${joiner(ap.criteria?.match)} `;
    return base({
      title: V("approval", { name: ap.name }),
      where: table(ap.entityName),
      changes: [
        { op: "add", text: crit > 0 ? V("if", { x: rules.map(workflowRuleText).join(join) }) : V("approval.all") },
        ...(steps.length === 0
          ? [{ op: "same" as const, text: V("approval.noSteps") }]
          : steps.map((s, i) => ({ op: "add" as const, text: approvalStepText(s, i) }))),
      ],
    });
  }
  const db = item.kind === "dashboard" ? getDashboardDraft(item) : undefined;
  if (db) {
    const lines = dashboardWidgetLines(db);
    return base({
      title: V("dashboard", { name: db.name }),
      where: t("studio.assistant.where.dashboards"),
      changes: [
        ...(db.description ? [{ op: "same" as const, text: db.description }] : []),
        ...(lines.length === 0
          ? [{ op: "same" as const, text: V("dashboard.none") }]
          : lines.map((l) => ({ op: "add" as const, text: V("dashboard.widget", { title: l.title, type: l.type }) }))),
      ],
    });
  }
  const wq = item.kind === "workqueue" ? getWorkQueueDraft(item) : undefined;
  if (wq) {
    return base({
      title: V("workqueue", { name: wq.label || wq.name }),
      where: table(wq.entityName),
      changes: [
        ...(Array.isArray(wq.filters) ? wq.filters : []).map((f) => ({ op: "add" as const, text: workflowRuleText({ field: f.field, op: f.op, value: f.value }) })),
        { op: "add", text: workQueueSummaryText(wq) },
      ],
    });
  }
  return null;
}

// The server's own `change` (L10-4a) decides the label and the Trước/Sau; for an update it also narrows the rows to the
// changed places (≤5 + "+n chỗ khác"). Without it the card still reads fine, just unlabelled.
function withChange(item: AiDraftItem, v: ProposalView): ProposalView {
  const c = item.change;
  if (!c) return v;
  const op: ProposalOp = c.op === "update" ? "update" : c.op === "delete" ? "delete" : "create";
  const out: ProposalView = { ...v, op };
  const paths = (c.changedPaths ?? []).slice(0, 5);
  if (op === "update" && paths.length > 0) {
    out.changes = paths.map((p) => ({ op: "change" as const, text: changePathText(p), key: p }));
    out.moreCount = c.moreChanged ?? 0;
  }
  if (op !== "create") out.before = changeSideLines(c.docType, c.before);
  if (op === "update") out.after = changeSideLines(c.docType, c.after, c.before);
  return out;
}

// Where the applied card points (moved from the old page's DraftCard; same destinations and wording).
export interface ProposalLink { text: string; href: string; label: string }
type Link = (id: RouteId) => string;

export function proposalDone(item: AiDraftItem, createdId: string | undefined, link: Link): ProposalLink {
  const studio = (text: string, id: RouteId, label: string): ProposalLink => ({ text, href: link(id), label });
  switch (item.kind) {
    case "layout": return studio("Đã lưu bố cục.", "dung/man-hinh", "Mở Màn hình");
    case "workflow": return studio("Đã tạo thành công.", "dung/tu-dong", "Mở Tự động");
    case "report": return { text: "Đã tạo thành công.", href: "/reports", label: "Mở Báo cáo" };
    case "app":
      // Grants were seeded onto two NEW app roles, not the tenant's existing ones; until someone is put in one of them
      // the app is empty for everyone but an admin (and an admin testing it cannot see that), so the line says it.
      return {
        text: "Đã tạo app. Còn một bước: gán người vào vai của app (Quản trị → Người dùng), nếu không nhân viên mở ra sẽ trống.",
        href: `/home?app=${encodeURIComponent(item.app?.name ?? "")}`,
        label: "Mở app vừa dựng",
      };
    case "edit_field": return studio("Đã cập nhật trường.", "dung/du-lieu", "Mở Dữ liệu");
    case "delete_field": return studio("Đã xoá trường.", "dung/du-lieu", "Mở Dữ liệu");
    case "delete_entity": return studio("Đã xoá đối tượng.", "dung/du-lieu", "Mở Dữ liệu");
    case "records": return { text: "Đã tạo dữ liệu.", href: `/${item.records?.entity ?? ""}`, label: "Mở danh sách" };
    case "approval": return studio("Đã tạo quy trình duyệt.", "dung/tu-dong/duyet", "Mở Quy trình duyệt");
    case "dashboard":
      return { text: "Đã tạo bảng điều khiển.", href: createdId ? `/dashboards/${createdId}` : "/dashboards", label: "Mở bảng điều khiển" };
    case "workqueue": return studio("Đã tạo hàng đợi.", "dung/tu-dong/hang-doi", "Mở Hàng đợi việc");
    default: return studio("Đã tạo thành công.", "dung/du-lieu", "Mở Dữ liệu");
  }
}

/** The fix-it link of the partial-success (warn) line: apps/tables get fixed in "App và menu", rows in their own list. */
export function proposalWarnLink(item: AiDraftItem, link: Link): Omit<ProposalLink, "text"> | null {
  if (item.kind === "app" || item.kind === "entity") return { href: link("dung/man-hinh/app"), label: "Mở App và menu" };
  if (item.kind === "records" && item.records) return { href: `/${item.records.entity}`, label: "Mở danh sách" };
  return null;
}
