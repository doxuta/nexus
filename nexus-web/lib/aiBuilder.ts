// lib/aiBuilder.ts — pure model + transforms for the E9 AI Builder chat composer (spec
// ../specs/020-e9-ai-builder/spec.md §4-§7 + §11 M3 sessions). NO fetch here (that lives in lib/api.ts
// so it can reuse the private req() auth/refresh path) — this module is side-effect-free so it stays
// unit-testable.
//
// M3 contract change (§11): the server now OWNS the conversation history (persisted per session). The
// chat request sends a SINGLE new user message + an optional sessionId — no client-held messages[]
// anymore. On resume, a session's persisted rows are mapped back to ChatTurn[] via
// serverMessagesToTurns (each assistant row carries its decoded draft so draft-cards re-render).
//
// Contract mirror (backend internal/ai/tools.go DraftItem/DraftEntity/DraftField/DraftRelationship/
// DraftLayout/DraftWorkflow/DraftReport/DraftApp + the AI-MODULES kinds approval/dashboard/
// workqueue): the chat endpoint returns PROPOSALS only. Applying a draft goes through the SAME
// gated admin APIs the manual builder uses (createEntity / addField / createRelationship /
// createLayout / createWorkflow / createReport / createApp + replaceAppNav /
// createApprovalProcess / createDashboard / createWorkQueue) — the AI never writes schema
// (HITL, spec §7).

import type {
  NewFieldInput, AdminRelationshipInput, AdminLayoutInput, CreateAppPayload, AppNavReplaceItem,
  ApprovalStep, ApprovalParallelStep, ApprovalOnReject, DashboardWidgetDef, WorkQueueInput, RoleGrant,
} from "@/lib/api";
import { newWidgetId } from "@/lib/widgetAuthoring";
import { accessText } from "@/lib/accessLevels";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

// Only user/assistant roles ever exist in a conversation — the backend injects its own system persona
// + grounding and persists only server-written user/assistant rows.
export type AiChatRole = "user" | "assistant";

// A proposed field. `entity` is set only for a STANDALONE field proposal (kind:"field" → maps to
// POST /admin/fields); it is absent for a field nested inside a proposed entity.
export interface AiDraftField {
  entity?: string;
  name: string;
  label: string;
  fieldType: string;
  options?: Record<string, unknown>;
  required?: boolean;
}

// A proposed new entity plus its initial fields (kind:"entity" → maps to POST /admin/entities,
// which registers the entity AND its fields in one call).
export interface AiDraftEntity {
  name: string;
  label: string;
  fields: AiDraftField[];
  /** Stamped by the server (never the model) when the turn ran inside an app: Áp dụng creates the
   * table with that app's id and adds it to the app menu (specs/067 L10-2). */
  appName?: string;
}

// A proposed N-N relationship between two EXISTING entities (E9 M5 — backend DraftRelationship,
// internal/ai/tools.go). Maps 1:1 onto the body of POST /admin/relationships {name, entityA,
// entityB}. 1-N/1-1 relations are NOT this kind — they stay kind:"field" with fieldType "relation"
// (a relation FIELD on the child entity) and render through the existing field card.
export interface AiDraftRelationship {
  name: string;
  entityA: string;
  entityB: string;
}

// A proposed layout for one (entity, viewType) — E9 M6 (backend DraftLayout, internal/ai/tools.go).
// Maps 1:1 onto the body of POST /admin/layouts {entityName: entity, viewType, layoutJson} (upsert
// on the (tenant, entity, view) key — no duplicate conflict exists at Apply). `entity` carries the
// CANONICAL stored entity name (resolved server-side before buffering). layoutJson is carried
// OPAQUE: it was validated at propose time by the exact validator SaveLayout runs (parity), so the
// FE summarizes it for the card and passes it through untouched — never rewrites it.
export interface AiDraftLayout {
  entity: string;
  viewType: "edit" | "detail" | "list" | (string & {});
  layoutJson: Record<string, unknown>;
}

// One field-op-value predicate of a proposed workflow condition (backend DraftWorkflowRule).
// `value` is ABSENT (omitempty) for the null-operators (isnull/notnull) — the backend rejects a
// value on those — and also when the model proposed an explicit JSON null: absence just means
// "no value on the wire", render/forward what's present.
export interface AiDraftWorkflowRule {
  field: string;
  op: string;
  value?: unknown;
}

// The condition block of a proposed workflow. Absent entirely → match-all.
export interface AiDraftWorkflowCondition {
  match?: string; // "and" | "or"
  rules?: AiDraftWorkflowRule[];
}

// One proposed automation step (backend DraftWorkflowAction). Mirrors its shape deliberately:
// there are NO url/secret/headers keys — outbound_webhook is hard-rejected at propose time, so a
// buffered draft is structurally incapable of carrying a webhook target.
export interface AiDraftWorkflowAction {
  type: string; // "field_update" | "in_app_notification" | "email_alert" (safe-action policy §14)
  // field_update. `value` absent can mean "clear to null" OR "no value proposed" (backend
  // omitempty, NIT-1) — the card renders only what's present, never fabricates a value.
  field?: string;
  value?: unknown;
  // in_app_notification ("$owner" = record owner, resolved by the engine at fire time)
  userId?: string;
  title?: string;
  message?: string;
  // email_alert
  to?: string;
  subject?: string;
  body?: string;
  // outbound_webhook: only appears in a BEFORE preview of an existing rule (the AI never proposes one).
  // url arrives cut to scheme://host; credentialId is an id, never a key.
  url?: string;
  credentialId?: string;
}

// A proposed automation rule — E9 M7 (backend DraftWorkflow). Maps 1:1 onto the body of
// POST /admin/workflows {name, entityName, trigger, condition?, actions, isActive}. entityName is
// the canonical stored name; trigger is one of the three after-commit events (scheduled is not
// proposable). isActive is ALWAYS true on the wire (the tool takes no isActive param — an inactive
// or scheduled rule is a human decision in the manual builder); it stays optional here only so a
// malformed hand-written draft doesn't type-error.
export interface AiDraftWorkflow {
  name: string;
  entityName: string;
  trigger: string;
  condition?: AiDraftWorkflowCondition;
  actions: AiDraftWorkflowAction[];
  isActive?: boolean;
}

// One filter predicate of a proposed report config (backend DraftReportFilter) — the same
// field/op/values vocabulary as a live list/aggregate query. op/values are ABSENT (omitempty)
// when the model didn't set them; render/forward only what's present.
export interface AiDraftReportFilter {
  field: string;
  op?: string;
  values?: string[];
}

// The FLAT config block of a proposed report (backend DraftReportConfig, mirroring
// services.ReportConfig): columns for tabular, groupBy/metric/field/bucket for summary/matrix,
// filters for both. There is deliberately NO sort key — a tabular report's default ordering is
// cosmetic polish a human sets in the Report Builder, not something the AI proposes (spec §15).
export interface AiDraftReportConfig {
  columns?: string[];
  filters?: AiDraftReportFilter[];
  groupBy?: string;
  metric?: string; // count | sum | avg | min | max (sum/avg/min/max need a numeric `field`)
  field?: string;
  bucket?: string; // day | week | month | quarter | year — summary/matrix with a date groupBy only
  // Grid keys (specs/049 S7a) — backend DraftReportConfig, same shapes as services.ReportConfig.
  subGroups?: AiDraftReportAxis[]; // summary only: sub-levels after groupBy (≤ 2)
  columnGroup?: AiDraftReportAxis; // matrix only: the column axis (groupBy = rows)
  having?: AiDraftReportHaving; // threshold on each cell / deepest-level group (never a row total)
}

export interface AiDraftReportAxis {
  field: string;
  bucket?: string;
}

export interface AiDraftReportHaving {
  on: string; // value | count
  op: string; // gt | gte | lt | lte | eq | neq | between
  values: string[];
}

// A proposed saved report — E9 M8 (backend DraftReport, internal/ai/tools.go). Maps 1:1 onto the
// body of POST /admin/reports {name, entityName, type, config, isActive}. NOTE the wire key is
// `type` (NOT reportType) — pinned by both DraftReport's json tags and the report handler's
// reportRequest. entityName carries the CANONICAL stored entity name (resolved server-side before
// buffering). isActive is ALWAYS true on an AI draft (an inactive report is a human decision in
// the manual builder); optional here only so a malformed persisted draft doesn't type-error.
export interface AiDraftReport {
  name: string;
  entityName: string;
  type: "summary" | "matrix" | "tabular" | (string & {});
  config: AiDraftReportConfig;
  isActive?: boolean;
}

// A proposed App container — Apps Phase 3 (backend DraftApp, internal/ai/tools.go). Applying is
// TWO calls: POST /admin/apps {name, label, icon?, color?, description?} creates the shell, then
// PUT /admin/apps/:id/nav attaches the sidebar. navEntities carries entity NAMES (canonical
// lowercase, display order, deduped server-side) — the FE resolves each name to its sys_entities
// id via /metadata/entities at Apply time (resolveAppNavEntities). Apply order contract: entities
// FIRST, the app LAST — a nav name whose entity draft hasn't been applied yet has no id to
// resolve and surfaces as an honest per-card "missing" warning, never a silent drop.
export interface AiDraftApp {
  name: string;
  label: string;
  icon?: string;
  color?: string;
  description?: string;
  navEntities: string[];
}

// ── AI-EDIT + FINISH-THE-BUILD draft kinds (Nước 7 POWER — NEXUS-EASE-DIRECTION §Nước-7) ────────
// Same HITL invariant as every kind above: these are PROPOSALS ONLY. Applying routes through the
// SAME gated admin APIs the manual Studio uses (PUT /admin/fields/:id, DELETE /admin/fields/:id,
// DELETE /admin/entities/:name, POST /:entity/bulk) — the AI never writes schema or data itself.

// The editable key set of an edit_field proposal — the exact PUT /admin/fields/:id patch surface
// minus fieldOrder (display order is a human decision in the Studio, not an AI proposal).
export interface AiDraftEditFieldPatch {
  label?: string;
  description?: string;
  isRequired?: boolean;
  defaultValue?: unknown;
  options?: Record<string, unknown>;
}

// A proposed change to ONE existing field (kind:"edit_field"). `before` carries the current stored
// values, `after` carries ONLY the keys that change (the diff set — the card renders one row per
// `after` key). NAMES are the wire contract: the field ID is resolved against the live registry
// (listFields) AT APPLY TIME, because a propose-time id could be stale after a rename/delete.
export interface AiDraftEditField {
  entity: string;
  field: string;
  before: AiDraftEditFieldPatch;
  after: AiDraftEditFieldPatch;
}

// A proposed field deletion (kind:"delete_field") — label/fieldType are carried so the confirm
// card can show WHAT dies without a lookup. Destructive: Apply demands a ConfirmDialog.
export interface AiDraftDeleteField {
  entity: string;
  field: string;
  label: string;
  fieldType: string;
}

// A proposed entity deletion (kind:"delete_entity") — fieldCount makes the impact honest on the
// card AND in the type-name confirm dialog. Destructive: Apply demands a type-name ConfirmDialog.
export interface AiDraftDeleteEntity {
  entity: string;
  label: string;
  fieldCount: number;
  /** The packs that ship the table (issue #30 item 4); absent = built here, or not known. */
  packs?: string[];
  /** The tenant's records the delete hides, counted when proposed; absent = not counted. */
  recordCount?: number;
}

// Proposed sample/seed rows for an EXISTING entity (kind:"records", ≤20 rows server-side). Applied
// via POST /:entity/bulk?mode=partial so one bad row never blocks the rest — per-row failures are
// surfaced honestly (recordsApplyMessage), never silently dropped.
export interface AiDraftRecords {
  entity: string;
  rows: Array<Record<string, unknown>>;
}

// ── AI-MODULES draft kinds (approval / dashboard / workqueue — Nước 7, parallel-BE contract) ────
// Same HITL invariant as every kind above: PROPOSALS ONLY, applied through the SAME gated admin
// APIs the manual builders use (POST /admin/approval-processes, POST /admin/dashboards,
// POST /admin/work-queues). Parsed TOLERANTLY: the BE lands in parallel, so absent/extra keys
// must degrade to honest fallbacks, never crash a persisted session.

// One approver step of a proposed approval process. The wire key is `approverRef` (a resolved
// user/role id, validated at propose time); `approverId` is tolerated as a twin in case a
// persisted draft mirrors the manual API's own key. Renamed to approverId at apply.
export interface AiDraftApprovalStep {
  id?: string; // stable step id (specs/043 S0) — validated/filled server-side at propose time
  name?: string; // optional step label ("Trưởng phòng duyệt") — carried to the apply body
  approverType: string; // "user" | "role" (server-validated — never re-mirrored here)
  approverRef?: string;
  approverId?: string;
  slaSeconds?: number;
  slaClock?: string; // "business" | "wall" (specs/043 S1); absent = working hours
  /** A step held by several people (specs/043 S4) INSTEAD of approverType/approverRef: each ref is
   *  a resolved id like approverRef (renamed to `id` at Apply). */
  approvers?: { type: string; ref?: string; id?: string }[];
  completion?: string; // "all" | "any" | "quorum" — only beside approvers
  completionPercent?: number; // 1..100, with quorum only (specs/043 S6b)
  /** Where a rejection of this step goes (specs/043 S8); carried to the apply body as-is. */
  onReject?: { to: string; resumeAt?: string };
}

// A proposed approval process (kind:"approval") — maps onto the body of
// POST /admin/approval-processes {name, entityName, trigger, criteria?, steps, isActive}.
// `trigger` may be absent on the wire → defaults to "after_create" at apply (the manual
// builder's own default). criteria is carried OPAQUE (validated at propose time — parity).
export interface AiDraftApproval {
  name: string;
  entityName: string;
  trigger?: string; // "after_create" | "after_update"
  criteria?: Record<string, unknown>;
  steps: AiDraftApprovalStep[];
  isActive?: boolean;
}

// One widget of a proposed dashboard — the DashboardWidgetDef surface, every key optional
// because the wire draft was server-validated (parity) and the FE never re-derives it. A widget
// without an id gets a client id at apply (same affordance as the manual widget composer).
export interface AiDraftDashboardWidget {
  id?: string;
  title?: string;
  type?: string;
  entity?: string;
  metric?: string;
  field?: string;
  groupBy?: string;
  bucket?: string;
  x?: number;
  y?: number;
  w?: number;
  h?: number;
  filters?: unknown; // opaque — same filter dialect as report/list, passed through untouched
}

// A proposed dashboard (kind:"dashboard") — maps onto createDashboard(name, widgets,
// description?). The created id feeds the card's deep-link (/dashboards/<id>).
export interface AiDraftDashboard {
  name: string;
  description?: string;
  widgets: AiDraftDashboardWidget[];
}

// One filter predicate of a proposed work queue — the FilterRow/queue dialect {field, op, value}.
export interface AiDraftWorkQueueFilter {
  field: string;
  op: string;
  value?: string;
}

// A proposed home-screen work queue (kind:"workqueue") — maps onto the body of
// POST /admin/work-queues (WorkQueueInput). `roleName` is a role NAME resolved to roleId against
// the live registry AT APPLY TIME (same pattern as app-nav name→id); an unresolvable role is an
// HONEST per-card error, never a silent visible-to-everyone widening. showOnHome → isActive.
export interface AiDraftWorkQueue {
  name: string;
  label?: string;
  entityName: string;
  filters?: AiDraftWorkQueueFilter[];
  roleName?: string;
  showOnHome?: boolean;
}

// Who may use what the assistant built (kind:"access", propose_access): grants to ADD to ONE existing role, by NAME
// (resolved to an id at apply, like a work queue's roleName). Only view/create/edit at all | owner. Applied by
// lib/aiApply.ts: read the role, mergeAccessGrants (lib/accessLevels.ts), write the whole set back.
export interface AiDraftAccess {
  role: string;
  roleLabel?: string;
  grants: RoleGrant[];
  /** table name → label, for the card */
  labels?: Record<string, string>;
}

// One proposal in a turn. Exactly one payload key (entity/field/relationship/layout/workflow/
// report/app/editField/deleteField/deleteEntity/records) is set, per kind. `draft` on the wire can
// be null (the agent asked a question instead of proposing) — callers must treat null and [] alike.
// kind is kept open ((string & {}) like AiOutcome) because drafts are PERSISTED in draft_json: a
// session written by a NEWER backend can resume on an older FE carrying a kind this build doesn't
// know — renderers/appliers MUST degrade to an honest fallback, never deref by kind.
// Create-or-Update preview of one proposal (backend DraftChange, specs/067 L10-4a). before/after
// are already secret-redacted; absent on a draft saved before the feature or on an app card.
export interface AiDraftChange {
  docType: string;
  docKey?: string;
  target?: { entity?: string; field?: string; view?: string; name?: string };
  op: "create" | "update" | "delete" | (string & {});
  label: string;
  before?: Record<string, unknown> | null;
  after?: Record<string, unknown> | null;
  changedPaths?: string[];
  moreChanged?: number;
}

export interface AiDraftItem {
  change?: AiDraftChange;
  kind:
    | "entity" | "field" | "relationship" | "layout" | "workflow" | "report" | "app"
    | "edit_field" | "delete_field" | "delete_entity" | "records"
    | "approval" | "dashboard" | "workqueue" | "access"
    | (string & {});
  entity?: AiDraftEntity;
  field?: AiDraftField;
  relationship?: AiDraftRelationship;
  layout?: AiDraftLayout;
  workflow?: AiDraftWorkflow;
  report?: AiDraftReport;
  app?: AiDraftApp;
  editField?: AiDraftEditField;
  deleteField?: AiDraftDeleteField;
  deleteEntity?: AiDraftDeleteEntity;
  records?: AiDraftRecords;
  approval?: AiDraftApproval;
  dashboard?: AiDraftDashboard;
  workqueue?: AiDraftWorkQueue;
  access?: AiDraftAccess;
}

export interface AiUsage {
  promptTokens: number;
  outputTokens: number;
  totalTokens: number;
}

// outcome: "ok" (assistant replied / proposed) | "max_steps" (tool-loop hit AI_MAX_STEPS, draft may
// be partial — badge "đề xuất chưa hoàn tất, hỏi tiếp") | "refused" (the model declined, reply empty — the
// panel says so instead of showing nothing). Kept as string so an unknown future value
// degrades gracefully instead of type-erroring.
export type AiOutcome = "ok" | "max_steps" | "refused" | (string & {});

export interface AiChatResult {
  // sessionId is ALWAYS present in the response — for a new thread (request sent no sessionId) it is
  // the freshly-minted id the caller must keep for subsequent turns.
  sessionId: string;
  reply: string;
  draft: AiDraftItem[] | null;
  runId: string;
  usage: AiUsage;
  outcome: AiOutcome;
  // specs/067: the context the SERVER resolved and used (null/absent = the turn had none). The
  // "Đang dựng" chip is drawn from this, never from the URL the client sent.
  context?: AiTurnContext | null;
}

// ── specs/067-ai-context: which app/screen the maker is in ───────────────────────────────────────
// Lookup KEYS only (the server re-resolves them inside the project of the session's token).
export interface AiTurnContextInput {
  app?: string;
  panel?: string;
  entity?: string;
  recordId?: string;
}
export interface AiTurnContext {
  project: { name: string };
  app: { name: string; label: string } | null;
  panel: string | null;
  entity?: { name: string; label: string; inApp: boolean } | null;
  recordId?: string | null;
}

export interface AiChatBody {
  message: string;
  sessionId?: string;
  context?: AiTurnContextInput;
}

/**
 * THE one place a chat request body is built — the SSE client, its three fallbacks to the JSON
 * route, and (through runTurn) Thử lại and the apply-failure report all call it, so no path can
 * drop the context. Only non-empty context keys are sent; an all-empty context has no `context` key.
 */
export function buildAiChatBody(message: string, sessionId?: string, context?: AiTurnContextInput): AiChatBody {
  const body: AiChatBody = { message };
  if (sessionId) body.sessionId = sessionId;
  const ctx: AiTurnContextInput = {};
  const app = context?.app?.trim();
  const panel = context?.panel?.trim();
  const entity = context?.entity?.trim();
  const recordId = context?.recordId?.trim();
  if (app) ctx.app = app;
  if (panel) ctx.panel = panel;
  if (entity) ctx.entity = entity;
  if (recordId) ctx.recordId = recordId;
  if (Object.keys(ctx).length > 0) body.context = ctx;
  return body;
}

// ── M3 session persistence (spec §11) ────────────────────────────────────────────────────────────
// A session summary as returned by GET /admin/ai/builder/sessions (the caller's own, newest first).
export interface AiSessionSummary {
  id: string;
  title: string;
  updatedAt: string; // ISO 8601
}

// One persisted message row (GET /admin/ai/builder/sessions/:id → messages[]). The backend already
// DECODES draft_json back to a draft[] (or null) on the way out, so it renders exactly like a live
// chat turn's draft. An assistant max_steps turn can have empty content + a non-null draft. outcome is
// how an assistant turn ended, as the live turn answered it; absent when none was stored.
export interface AiServerMessage {
  seq: number;
  role: string; // server-written "user" | "assistant"; coerced on the way in (serverMessagesToTurns)
  content: string;
  draft: AiDraftItem[] | null;
  createdAt: string;
  outcome?: AiOutcome;
}

// Full session detail (GET /admin/ai/builder/sessions/:id).
export interface AiSessionDetail {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  messages: AiServerMessage[];
}

// ── Render model ─────────────────────────────────────────────────────────────────────────────────
// A turn as held in component state. The user turn is just role+content; an assistant turn also
// carries the draft/outcome/usage so the bubble can render draft cards + the token/max_steps badge.
// The server owns history now (M3) — the client never resends turns; it sends only the new message.
export interface ChatTurn {
  id: string;
  role: AiChatRole;
  content: string;
  draft?: AiDraftItem[] | null;
  outcome?: AiOutcome;
  usage?: AiUsage;
  // true for a turn rebuilt from a stored session: its Before was read when it was proposed.
  reopened?: boolean;
}

// serverMessagesToTurns: the pure mapper that rebuilds the rendered conversation from a session's
// PERSISTED rows on resume (spec §11 — "click a session → render its history, bubbles + draft-cards
// from persisted drafts"). Each assistant row's decoded draft is carried onto the turn so a resumed
// draft-card is still applicable, and its stored outcome so a declined turn says so and a max_steps one keeps
// its note (#30 item 5). usage is not stored per message, so a resumed turn omits the token badge.
// Role is coerced to the user/assistant union (rows are always server-written as one of those). The
// key is derived from the monotonic seq so resume keys are stable (no random churn across reloads).
export function serverMessagesToTurns(messages: AiServerMessage[]): ChatTurn[] {
  return (messages ?? []).map((m) => {
    const role: AiChatRole = m.role === "assistant" ? "assistant" : "user";
    const turn: ChatTurn = { id: `m_${m.seq}`, role, content: m.content ?? "" };
    if (role === "assistant") turn.draft = hasDrafts(m.draft) ? m.draft : null;
    if (role === "assistant") turn.reopened = true;
    if (role === "assistant" && m.outcome) turn.outcome = m.outcome;
    return turn;
  });
}

// hasDrafts: true only for a non-null, non-empty proposal array. The single predicate both the
// render path and the apply-all button use so null and [] are handled identically.
export function hasDrafts(draft: AiDraftItem[] | null | undefined): draft is AiDraftItem[] {
  return Array.isArray(draft) && draft.length > 0;
}

// Tolerant payload accessor for the snake-kind drafts. The kind VALUE is snake_case on the wire
// ("edit_field" — mirroring trigger tokens like "after_create"), while the container KEY follows
// the project's camelCase-JSON rule ("editField"). Because drafts persist in draft_json and the
// BE lands in parallel, accept the snake_case twin key too — a key-spelling mismatch must degrade
// to nothing worse than reading the same object, never to an UnknownDraftCard. Non-object payloads
// (malformed persisted draft) return undefined so render/apply fall back honestly.
function draftPayload<T extends object>(item: AiDraftItem, camelKey: string, snakeKey: string): T | undefined {
  const rec = item as unknown as Record<string, unknown>;
  const v = rec[camelKey] ?? rec[snakeKey];
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as T) : undefined;
}

export function getEditFieldDraft(item: AiDraftItem): AiDraftEditField | undefined {
  return draftPayload<AiDraftEditField>(item, "editField", "edit_field");
}
export function getDeleteFieldDraft(item: AiDraftItem): AiDraftDeleteField | undefined {
  return draftPayload<AiDraftDeleteField>(item, "deleteField", "delete_field");
}
export function getDeleteEntityDraft(item: AiDraftItem): AiDraftDeleteEntity | undefined {
  return draftPayload<AiDraftDeleteEntity>(item, "deleteEntity", "delete_entity");
}
export function getRecordsDraft(item: AiDraftItem): AiDraftRecords | undefined {
  return draftPayload<AiDraftRecords>(item, "records", "records");
}
export function getApprovalDraft(item: AiDraftItem): AiDraftApproval | undefined {
  return draftPayload<AiDraftApproval>(item, "approval", "approval");
}
export function getDashboardDraft(item: AiDraftItem): AiDraftDashboard | undefined {
  return draftPayload<AiDraftDashboard>(item, "dashboard", "dashboard");
}
export function getAccessDraft(item: AiDraftItem): AiDraftAccess | undefined {
  const a = draftPayload<AiDraftAccess>(item, "access", "access");
  return a && typeof a.role === "string" && Array.isArray(a.grants) ? a : undefined;
}
export function getWorkQueueDraft(item: AiDraftItem): AiDraftWorkQueue | undefined {
  // camelCase twin "workQueue" tolerated — the kind token itself is one word ("workqueue").
  return draftPayload<AiDraftWorkQueue>(item, "workqueue", "workQueue");
}

// draftApplyRank: the applyAll dependency order. Entities FIRST (0) — everything references them;
// fields/relationships/layouts/workflows/reports/edits/deletes AND the module kinds
// approval/dashboard/workqueue next (1, draft order preserved — they reference entities/fields,
// nothing references them); records at 1.5 — sample rows can only insert after their entity AND
// fields exist; the app LAST (2) — its nav resolves entity NAMES → ids. Stable sort keeps
// original indexes within a rank.
export function draftApplyRank(kind: string): number {
  if (kind === "entity") return 0;
  if (kind === "records") return 1.5;
  if (kind === "app") return 2;
  return 1;
}

// isDestructiveDraft: the two kinds whose Apply demands a ConfirmDialog interstitial — and which
// "Áp dụng tất cả" deliberately SKIPS (each deletion needs its own human confirm; bulk-firing a
// delete behind a generic button would gut the HITL guarantee). Payload presence is part of the
// predicate: a delete kind with no payload isn't confirmable — it falls through to the honest
// "thiếu thông tin" apply error instead of opening an empty dialog.
export function isDestructiveDraft(item: AiDraftItem): boolean {
  return (
    (item.kind === "delete_field" && getDeleteFieldDraft(item) !== undefined) ||
    (item.kind === "delete_entity" && getDeleteEntityDraft(item) !== undefined)
  );
}

/**
 * specs/067 T038a + T088 (FR-034 of specs/063-draft-publish): once the project is delivered, sample rows
 * (they write REAL data), a table that lands INSIDE an app (it seeds grants on real staff) and any app
 * card take effect for real users at once — they need an explicit "ÁP NGAY" confirm and never ride
 * "Áp dụng tất cả".
 */
export function needsLiveConfirm(item: AiDraftItem, delivered: boolean, asSample = true): boolean {
  if (!delivered) return false;
  // specs/076 ai10: sample rows are marked and fire no automation, so only the "real data" choice is live.
  return (item.kind === "records" && !asSample) || (item.kind === "entity" && !!item.entity?.appName) || (item.kind === "app" && !!item.app);
}

/** The slice of GET /admin/publish/state the card footers need; null/undefined = unknown (403, backend off). */
export interface AiPublishInfo { delivered: boolean; draftableTypes: string[] }

// Which metadata document type an AI card writes (sys_meta class registry on the server).
export function draftDocType(kind: string): string {
  if (kind === "field" || kind === "edit_field" || kind === "delete_field") return "field";
  if (kind === "entity" || kind === "delete_entity") return "entity";
  return kind;
}

/**
 * A new table, field or N-N relation on a delivered workspace is created live but hidden from the customer
 * until the next publish (specs/063 FR-060, metaversion.ClassOf NewHidden) — no draft row, nothing to undo.
 * The one predicate for that, so the card label, its class and its footer cannot disagree again.
 */
export function bornHidden(kind: string, draftableTypes: string[]): boolean {
  return (kind === "entity" || kind === "field" || kind === "relationship") && draftableTypes.includes("field");
}

/**
 * specs/067 T088 (plan §9): the i18n key of the line under an APPLIED card telling the builder where
 * the change went on a delivered project. Order matters — first match wins. null = say nothing
 * (not delivered / unknown state: every change is live and the old footer already says so).
 */
export function appliedFootKey(item: AiDraftItem, info: AiPublishInfo | null | undefined): I18nKey | null {
  if (!info?.delivered) return null;
  if (item.kind === "records") return "page.studio.ai.foot.records";
  if (bornHidden(item.kind, info.draftableTypes)) {
    return item.kind === "entity" && item.entity?.appName ? "page.studio.ai.foot.createdHiddenApp" : "page.studio.ai.foot.createdHidden";
  }
  if (item.kind === "app") return "page.studio.ai.foot.appLive";
  // delete_field is live-immediate until S7-9 turns it into a draft (draft_repo_field.go:44).
  if (item.kind === "delete_field") return "page.studio.ai.foot.live";
  return info.draftableTypes.includes(draftDocType(item.kind)) ? "page.studio.ai.foot.draft" : "page.studio.ai.foot.live";
}

// ── Draft → apply payload transforms (the unit-tested core) ────────────────────────────────────
// A proposed field is already VALIDATED server-side (the propose_* tool ran the same
// ValidateCreateEntity / ValidateAddField the manual builder hits) and `options` is already the exact
// sys_fields.options JSON shape NewFieldInput.options wants — so pass it straight through, do NOT
// re-run DataModelBuilder's buildOptions (that derives options from raw form state and would corrupt
// an already-structured object). The load-bearing conversion is the key RENAME required → isRequired.
export function draftFieldToInput(f: AiDraftField): NewFieldInput {
  const input: NewFieldInput = {
    name: f.name,
    label: f.label,
    fieldType: f.fieldType,
    isRequired: !!f.required, // DraftField.required → NewFieldInput.isRequired (do not drop this flag)
  };
  if (f.options && Object.keys(f.options).length > 0) {
    input.options = f.options; // passthrough — already structured + validated
  }
  return input;
}

// draftEntityToPayload: shape a proposed entity into the createEntity() argument set. description is
// empty ("" is what createEntity defaults to) — the AI proposes name/label/fields, not a blurb.
export function draftEntityToPayload(e: AiDraftEntity): {
  name: string;
  label: string;
  description: string;
  fields: NewFieldInput[];
} {
  return {
    name: e.name,
    label: e.label,
    description: "",
    fields: (e.fields ?? []).map(draftFieldToInput),
  };
}

// draftRelationshipToPayload: shape a proposed N-N relationship into the createRelationship()
// argument (E9 M5). Identity-shaped on purpose — it PINS the exact POST /admin/relationships body
// {name, entityA, entityB} and strips any extra keys a future wire draft might carry, so the apply
// path never leaks unknown fields into the gated admin API.
export function draftRelationshipToPayload(r: AiDraftRelationship): AdminRelationshipInput {
  return { name: r.name, entityA: r.entityA, entityB: r.entityB };
}

// draftLayoutToPayload: shape a proposed layout into the createLayout() argument (E9 M6).
// Identity-shaped but load-bearing — it PINS the exact POST /admin/layouts body {entityName,
// viewType, layoutJson} (the wire's `entity` key RENAMES to `entityName`) and strips any extra
// keys a future wire draft might carry. isDefault is deliberately NOT sent: promoting a layout to
// default is a human decision in the Layout Builder, not part of the AI proposal. layoutJson goes
// through untouched — it is already the exact validated structure the upsert wants (parity, §13).
export function draftLayoutToPayload(l: AiDraftLayout): AdminLayoutInput {
  return {
    entityName: l.entity,
    viewType: l.viewType as AdminLayoutInput["viewType"], // enum-validated at propose time (parity)
    layoutJson: l.layoutJson,
  };
}

// The complete key set of a proposed workflow action (backend DraftWorkflowAction omitempty
// fields). draftWorkflowToPayload rebuilds each action from THIS list so nothing else — not even
// a key a future wire might invent — leaks into the gated admin API.
const WORKFLOW_ACTION_KEYS = ["field", "value", "userId", "title", "message", "to", "subject", "body"] as const;

function draftWorkflowActionToPayload(a: AiDraftWorkflowAction): Record<string, unknown> {
  const out: Record<string, unknown> = { type: a.type };
  for (const k of WORKFLOW_ACTION_KEYS) {
    if (a[k] !== undefined) out[k] = a[k];
  }
  return out;
}

// draftWorkflowToPayload: shape a proposed automation rule into the createWorkflow() body (E9 M7)
// — {name, entityName, trigger, condition?, actions, isActive}. Identity-shaped: condition rules
// and actions are rebuilt key-by-key so extra keys never leak, and ABSENT keys stay absent (a
// null-operator rule carries no `value` — the backend 422s one; a field_update without `value`
// stays without it, NIT-1: we cannot distinguish "clear to null" from "unset" and must not
// fabricate either). condition is omitted entirely when it has no rules (match-all), mirroring the
// manual WorkflowBuilder. isActive is always present on the wire (backend serializes the bool
// unconditionally, always true for an AI draft) — `?? true` only guards a malformed draft.
export function draftWorkflowToPayload(w: AiDraftWorkflow): Record<string, unknown> {
  const payload: Record<string, unknown> = {
    name: w.name,
    entityName: w.entityName,
    trigger: w.trigger,
    actions: (w.actions ?? []).map(draftWorkflowActionToPayload),
    isActive: w.isActive ?? true,
  };
  const rules = w.condition?.rules;
  if (Array.isArray(rules) && rules.length > 0) {
    const cond: Record<string, unknown> = {
      rules: rules.map((r) =>
        r.value !== undefined
          ? { field: r.field, op: r.op, value: r.value }
          : { field: r.field, op: r.op },
      ),
    };
    if (w.condition?.match) cond.match = w.condition.match;
    payload.condition = cond;
  }
  return payload;
}

// draftReportToPayload: shape a proposed report into the createReport() body (E9 M8) —
// {name, entityName, type, config, isActive}, the exact POST /admin/reports shape. Identity-shaped
// but rebuilt key-by-key: the config and each filter predicate are reconstructed from the known
// key set so nothing else — not even a `sort` a future wire might carry (ordering is a human
// decision in the Report Builder, spec §15) — leaks into the gated admin API. Empty/absent config
// keys stay absent (mirrors the backend's omitempty); isActive is always present on the wire
// (backend serializes true unconditionally) — `?? true` only guards a malformed persisted draft.
export function draftReportToPayload(r: AiDraftReport): Record<string, unknown> {
  const c = r.config ?? {};
  const config: Record<string, unknown> = {};
  if (Array.isArray(c.columns) && c.columns.length > 0) config.columns = c.columns;
  if (Array.isArray(c.filters) && c.filters.length > 0) {
    config.filters = c.filters.map((f) => {
      const out: Record<string, unknown> = { field: f.field };
      if (f.op !== undefined) out.op = f.op;
      if (f.values !== undefined) out.values = f.values;
      return out;
    });
  }
  if (c.groupBy) config.groupBy = c.groupBy;
  if (c.metric) config.metric = c.metric;
  if (c.field) config.field = c.field;
  if (c.bucket) config.bucket = c.bucket;
  const axis = (a: AiDraftReportAxis): Record<string, unknown> =>
    a.bucket ? { field: a.field, bucket: a.bucket } : { field: a.field };
  if (Array.isArray(c.subGroups) && c.subGroups.length > 0) config.subGroups = c.subGroups.map(axis);
  if (c.columnGroup?.field) config.columnGroup = axis(c.columnGroup);
  if (c.having) config.having = { on: c.having.on, op: c.having.op, values: c.having.values };
  return {
    name: r.name,
    entityName: r.entityName,
    type: r.type,
    config,
    isActive: r.isActive ?? true,
  };
}

// draftAppToPayload: shape a proposed app into the createApp() body (Apps Phase 3) — the exact
// POST /admin/apps shape, rebuilt key-by-key. navEntities deliberately does NOT go here (the nav
// is a SECOND call, PUT /admin/apps/:id/nav, after name→id resolution); isDefault/sortOrder are
// never sent — promoting a default app / ordering the switcher is a human decision in the Apps
// Manager, not part of the AI proposal. Empty optionals stay absent (mirrors CreateAppDialog).
export function draftAppToPayload(a: AiDraftApp): CreateAppPayload {
  const p: CreateAppPayload = { name: a.name, label: a.label };
  if (a.icon) p.icon = a.icon;
  if (a.color) p.color = a.color;
  if (a.description) p.description = a.description;
  return p;
}

// The name→id resolution step of applying an app draft (Apps Phase 3).
export interface AppNavResolveResult {
  /** Ready for PUT /admin/apps/:id/nav — array order IS the sidebar order; label omitted = inherit. */
  items: AppNavReplaceItem[];
  /** Nav names with no live entity (their drafts weren't applied yet) — raw spelling, nav order. */
  missing: string[];
}

// resolveAppNavEntities: resolve a draft's nav entity NAMES against the live registry
// (/metadata/entities) into ReplaceNav items + the honest list of names that DON'T resolve.
// Matching is case-insensitive (canonical stored names are lowercase; a persisted/hand-written
// draft may differ in case). Repeats are deduped (first occurrence wins) because ReplaceNav 422s
// on a duplicate (type, ref) pair; an entity row without an id can't be referenced → missing.
export function resolveAppNavEntities(
  navEntities: string[] | null | undefined,
  entities: Array<{ id?: string; name: string }>,
): AppNavResolveResult {
  const byName = new Map<string, string>();
  for (const e of entities) {
    if (e.id) byName.set(e.name.toLowerCase(), e.id);
  }
  const items: AppNavReplaceItem[] = [];
  const missing: string[] = [];
  const seen = new Set<string>();
  for (const raw of navEntities ?? []) {
    const key = raw.trim().toLowerCase();
    if (key === "" || seen.has(key)) continue;
    seen.add(key);
    const id = byName.get(key);
    if (id !== undefined) items.push({ itemType: "entity", refId: id });
    else missing.push(raw);
  }
  return { items, missing };
}

// The complete editable key set of an edit_field patch (the PUT /admin/fields/:id surface minus
// fieldOrder). Both the apply payload AND the diff table are rebuilt from THIS list so nothing
// else — not a key a future wire might invent — leaks into the gated admin API or the card.
const EDIT_FIELD_PATCH_KEYS = ["label", "description", "isRequired", "defaultValue", "options"] as const;
type EditFieldPatchKey = (typeof EDIT_FIELD_PATCH_KEYS)[number];

// draftEditFieldToUpdatePayload: shape an edit_field proposal into the updateField() body — ONLY
// the changed keys (`after`), rebuilt key-by-key. Absent keys stay absent (PUT /admin/fields/:id
// is apply-when-non-nil: omitted keys keep their stored value — sending an unchanged key would
// widen the blast radius of a stale draft). `defaultValue: null` passes through (an explicit
// clear-the-default proposal is a value, not an absence).
export function draftEditFieldToUpdatePayload(e: AiDraftEditField): AiDraftEditFieldPatch {
  const after = e.after ?? {};
  const out: Record<string, unknown> = {};
  for (const k of EDIT_FIELD_PATCH_KEYS) {
    if (after[k] !== undefined) out[k] = after[k];
  }
  return out as AiDraftEditFieldPatch;
}

// draftRecordsRows: the rows a records draft will actually submit — object rows only. A malformed
// persisted draft can carry non-object entries; both the preview and the bulk POST use THIS list
// so the "N dòng" the card shows is exactly the N the apply sends.
export function draftRecordsRows(r: AiDraftRecords): Array<Record<string, unknown>> {
  return objectRows(r.rows);
}

function objectRows(rows: unknown): Array<Record<string, unknown>> {
  return (Array.isArray(rows) ? rows : []).filter(
    (row): row is Record<string, unknown> => row !== null && typeof row === "object" && !Array.isArray(row),
  );
}

// onRejectOf copies a draft step's onReject (specs/043 S8) keeping only the two server keys; a blank
// target is dropped (the server validates the rest — no mirrored rules here).
function onRejectOf(o: { to?: string; resumeAt?: string } | undefined): ApprovalOnReject | undefined {
  if (!o || typeof o.to !== "string" || o.to.trim() === "") return undefined;
  const out: ApprovalOnReject = { to: o.to.trim() };
  if (o.resumeAt === "rejector" || o.resumeAt === "start") out.resumeAt = o.resumeAt;
  return out;
}

// draftApprovalToPayload: shape a proposed approval process into the createApprovalProcess()
// body — the exact POST /admin/approval-processes shape the manual Approval Builder sends.
// Steps are rebuilt key-by-key (the wire's `approverRef` RENAMES to `approverId`; the twin key
// is tolerated) so nothing else leaks into the gated admin API. trigger defaults to
// "after_create" (the manual builder's own default) only when the wire omitted it — a present
// token passes through raw and the server validates it (parity — no mirrored rules here).
// criteria is opaque passthrough, included only when it is a non-empty object (the manual
// builder omits it the same way). isActive `?? true` mirrors the workflow/report convention:
// an AI draft applies ACTIVE — the card badges that before the click.
export function draftApprovalToPayload(a: AiDraftApproval): {
  name: string;
  entityName: string;
  trigger: string;
  criteria?: Record<string, unknown>;
  steps: (ApprovalStep | ApprovalParallelStep)[];
  isActive: boolean;
} {
  const steps: (ApprovalStep | ApprovalParallelStep)[] = (Array.isArray(a.steps) ? a.steps : []).map((s) => {
    // A many-approver step (S4) is sent as approvers[] + completion and NO approverType/approverId
    // (the server refuses both sources on one step). Each ref renames to `id`, like approverRef.
    if (Array.isArray(s.approvers) && s.approvers.length > 0) {
      const par: ApprovalParallelStep = {
        approvers: s.approvers.map((ap) => ({ type: ap.type === "role" ? "role" : "user", id: ap.ref ?? ap.id ?? "" })),
      };
      if (s.completion === "all" || s.completion === "any") par.completion = { mode: s.completion };
      // A quorum keeps whatever percent the draft carried (even a junk one): the server answers 422,
      // rather than the council silently degrading to the stricter `all` at Apply.
      else if (s.completion === "quorum") {
        par.completion = { mode: "quorum", percent: s.completionPercent };
      }
      if (typeof s.id === "string" && s.id !== "") par.id = s.id;
      const pname = typeof s.name === "string" ? s.name.trim() : "";
      if (pname) par.name = pname;
      if (typeof s.slaSeconds === "number" && Number.isFinite(s.slaSeconds) && s.slaSeconds > 0) par.slaSeconds = s.slaSeconds;
      if (s.slaClock === "business" || s.slaClock === "wall") par.slaClock = s.slaClock;
      const por = onRejectOf(s.onReject);
      if (por) par.onReject = por;
      return par;
    }
    const step: ApprovalStep = {
      approverType: s.approverType === "role" ? "role" : "user",
      approverId: s.approverRef ?? s.approverId ?? "",
    };
    // The stable id rides along (specs/043 S0): rebuilding the step from the approver alone
    // dropped it at Apply. Blank → omitted (the server assigns the id).
    if (typeof s.id === "string" && s.id !== "") step.id = s.id;
    const name = typeof s.name === "string" ? s.name.trim() : "";
    if (name) step.name = name; // the approval screen's only label once the process is gone (#69)
    if (typeof s.slaSeconds === "number" && Number.isFinite(s.slaSeconds) && s.slaSeconds > 0) {
      step.slaSeconds = s.slaSeconds;
    }
    // The step's clock rides along (specs/043 S1) — dropped, a "wall clock" step would run in
    // working hours after Apply. Only the two server tokens pass; anything else stays out.
    if (s.slaClock === "business" || s.slaClock === "wall") step.slaClock = s.slaClock;
    // The step's answer to a rejection rides along (specs/043 S8) — dropped, "reject → back to the
    // submitter" would silently become a final reject after Apply.
    const or = onRejectOf(s.onReject);
    if (or) step.onReject = or;
    return step;
  });
  const payload: ReturnType<typeof draftApprovalToPayload> = {
    name: a.name,
    entityName: a.entityName,
    trigger: a.trigger || "after_create",
    steps,
    isActive: a.isActive ?? true,
  };
  if (a.criteria && typeof a.criteria === "object" && Object.keys(a.criteria).length > 0) {
    payload.criteria = a.criteria;
  }
  return payload;
}

// The DashboardWidgetDef key set (minus id, handled separately) — each widget of a dashboard
// draft is rebuilt from THIS list so nothing else leaks into the gated admin API.
const DASHBOARD_WIDGET_KEYS = [
  "title", "type", "entity", "metric", "field", "groupBy", "bucket", "x", "y", "w", "h", "filters",
] as const;

// draftDashboardToPayload: shape a proposed dashboard into the createDashboard() argument set.
// Widgets are rebuilt key-by-key; absent keys stay absent (a missing type/metric surfaces as the
// server's own 422 at apply — honest, never invented). A widget without an id gets a client id
// (idGen injectable for tests; default = the manual composer's own newWidgetId).
export function draftDashboardToPayload(
  d: AiDraftDashboard,
  idGen: () => string = newWidgetId,
): { name: string; description?: string; widgets: DashboardWidgetDef[] } {
  const widgets = (Array.isArray(d.widgets) ? d.widgets : []).map((w) => {
    const src = (w && typeof w === "object" ? w : {}) as Record<string, unknown>;
    const out: Record<string, unknown> = {
      id: typeof src.id === "string" && src.id !== "" ? src.id : idGen(),
    };
    for (const k of DASHBOARD_WIDGET_KEYS) {
      if (src[k] !== undefined) out[k] = src[k];
    }
    // Server-validated at propose time (parity) — the FE never re-derives widget config.
    return out as unknown as DashboardWidgetDef;
  });
  const payload: { name: string; description?: string; widgets: DashboardWidgetDef[] } = {
    name: d.name,
    widgets,
  };
  if (typeof d.description === "string" && d.description !== "") payload.description = d.description;
  return payload;
}

// draftWorkQueueToPayload: shape a proposed work queue into the createWorkQueue() body
// (WorkQueueInput). label falls back to name (the wire may omit a display label); filters are
// rebuilt key-by-key with the queue dialect's required `value` string (null-ops carry "");
// roleId comes from the CALLER (resolved from roleName at apply time — this module stays
// fetch-free); showOnHome maps to isActive (`?? true` — a proposed queue lands on the home
// screen unless the draft says otherwise); actionVerb defaults to "open" like the manual EMPTY.
export function draftWorkQueueToPayload(q: AiDraftWorkQueue, roleId?: string): WorkQueueInput {
  const input: WorkQueueInput = {
    name: q.name,
    label: q.label || q.name,
    entityName: q.entityName,
    filters: (Array.isArray(q.filters) ? q.filters : []).map((f) => ({
      field: f.field,
      op: f.op,
      value: typeof f.value === "string" ? f.value : "",
    })),
    actionVerb: "open",
    isActive: q.showOnHome ?? true,
  };
  if (roleId) input.roleId = roleId;
  return input;
}

// resolveRoleByName: case-insensitive role NAME (then label) → id against the live registry
// (listRoles) — the workqueue apply-time twin of resolveAppNavEntities. undefined = no match:
// the caller must surface an honest error, NEVER fall back to visible-to-everyone (that would
// silently widen who sees the queue).
export function resolveRoleByName(
  roleName: string,
  roles: Array<{ id: string; name: string; label?: string }>,
): string | undefined {
  const key = roleName.trim().toLowerCase();
  if (key === "") return undefined;
  const byName = roles.find((r) => r.name.toLowerCase() === key);
  if (byName) return byName.id;
  return roles.find((r) => (r.label ?? "").toLowerCase() === key)?.id;
}

// approvalDraftRefs: every approver a proposed approval names, as {type, ref}. The model can never
// know tenant UUIDs, so each ref is a role NAME or a user EMAIL (tools.go: "Hệ thống đổi ra id lúc
// Apply") — this is the list the Apply step has to turn into ids.
export function approvalDraftRefs(a: AiDraftApproval): Array<{ type: "role" | "user"; ref: string }> {
  const out: Array<{ type: "role" | "user"; ref: string }> = [];
  for (const s of Array.isArray(a.steps) ? a.steps : []) {
    if (Array.isArray(s.approvers) && s.approvers.length > 0) {
      for (const ap of s.approvers) out.push({ type: ap.type === "role" ? "role" : "user", ref: ap.ref ?? ap.id ?? "" });
    } else {
      out.push({ type: s.approverType === "role" ? "role" : "user", ref: s.approverRef ?? s.approverId ?? "" });
    }
  }
  return out;
}

// resolveApprovalApprovers: role NAME / user EMAIL → id against the live registries, at Apply time
// (the twin of resolveRoleByName for work queues). An unresolvable ref is an HONEST error naming
// it — never stored as-is: an approver id nobody holds makes a step (or, on a council, the whole
// step) that no one can ever decide. A ref that already IS a known id passes through.
export function resolveApprovalApprovers(
  a: AiDraftApproval,
  roles: Array<{ id: string; name: string; label?: string }>,
  users: Array<{ id: string; email?: string }>,
): { draft: AiDraftApproval } | { error: string } {
  const idOf = (type: "role" | "user", ref: string): string | undefined => {
    const key = ref.trim();
    if (key === "") return undefined;
    if (type === "role") return roles.find((r) => r.id === key)?.id ?? resolveRoleByName(key, roles);
    const lower = key.toLowerCase();
    return users.find((u) => u.id === key || (u.email ?? "").toLowerCase() === lower)?.id;
  };
  let bad: string | undefined;
  const one = (type: string | undefined, ref: string | undefined): string => {
    const kind = type === "role" ? "role" : "user";
    const id = idOf(kind, ref ?? "");
    if (!id && bad === undefined) bad = `${t(kind === "role" ? "aiCard.approver.role" : "aiCard.approver.user")} "${ref ?? ""}"`;
    return id ?? "";
  };
  const steps = (Array.isArray(a.steps) ? a.steps : []).map((s) => {
    if (Array.isArray(s.approvers) && s.approvers.length > 0) {
      return { ...s, approvers: s.approvers.map((ap) => ({ type: ap.type, id: one(ap.type, ap.ref ?? ap.id) })) };
    }
    return { ...s, approverRef: undefined, approverId: one(s.approverType, s.approverRef ?? s.approverId) };
  });
  if (bad !== undefined) return { error: tf("aiCard.approver.missing", { who: bad }) };
  return { draft: { ...a, steps } };
}

// ── Draft-card display helpers (pure — the card JSX stays dumb) ─────────────────────────────────

// layoutViewLabel: viewType → the label the layout card header shows. An unknown token renders raw (honest — no
// guessing).
export function layoutViewLabel(viewType: string): string {
  return viewType === "edit" || viewType === "detail" || viewType === "list" ? t(`aiCard.layoutView.${viewType}`) : viewType;
}

// One edit/detail section reduced for the card: title + the field names its cells reference.
export interface AiLayoutSectionSummary { title: string; columns: number; fields: string[] }

// The card's readable digest of a layoutJson. "sections" for edit/detail, "columns" for list,
// "opaque" when the structure isn't recognizable — the card then says so honestly (Apply still
// works: the payload passes the ORIGINAL layoutJson through, which the server re-validates).
export type AiLayoutSummary =
  | { kind: "sections"; sections: AiLayoutSectionSummary[] }
  | { kind: "columns"; columns: string[]; defaultSort?: { field: string; order: string } }
  | { kind: "opaque" };

// A layout cell is string | {field,...} | {spacer:true} (lib/api LayoutCell). Reduce to the field
// name; spacers and unrecognizable cells yield null (skipped from the summary, never invented).
function cellFieldName(cell: unknown): string | null {
  if (typeof cell === "string") return cell;
  if (cell && typeof cell === "object") {
    const f = (cell as { field?: unknown }).field;
    if (typeof f === "string" && f !== "") return f;
  }
  return null;
}

export function summarizeLayoutDraft(
  viewType: string,
  layoutJson: Record<string, unknown> | null | undefined,
): AiLayoutSummary {
  const json = layoutJson ?? {};
  if (viewType === "list") {
    const rawCols = json.columns;
    if (!Array.isArray(rawCols)) return { kind: "opaque" };
    const columns = rawCols.map(cellFieldName).filter((f): f is string => f !== null);
    const ds = json.defaultSort as { field?: unknown; order?: unknown } | undefined;
    if (ds && typeof ds === "object" && typeof ds.field === "string") {
      return { kind: "columns", columns, defaultSort: { field: ds.field, order: String(ds.order ?? "") } };
    }
    return { kind: "columns", columns };
  }
  if (viewType === "edit" || viewType === "detail") {
    const rawSections = json.sections;
    if (!Array.isArray(rawSections)) return { kind: "opaque" };
    const sections: AiLayoutSectionSummary[] = rawSections.map((s, i) => {
      const o = (s && typeof s === "object" ? s : {}) as { title?: unknown; columns?: unknown; fields?: unknown };
      return {
        title: typeof o.title === "string" && o.title !== "" ? o.title : tf("aiCard.layout.section", { n: i + 1 }),
        columns: typeof o.columns === "number" && Number.isFinite(o.columns) ? o.columns : 1,
        fields: (Array.isArray(o.fields) ? o.fields : []).map(cellFieldName).filter((f): f is string => f !== null),
      };
    });
    return { kind: "sections", sections };
  }
  return { kind: "opaque" };
}

// workflowTriggerPhrase: the clause AFTER the card's "KHI" row label (no leading "Khi" — the label
// already says it, "KHI | Khi cập nhật…" would stutter). Only the three after-commit events are
// proposable; an unknown token renders raw.
export function workflowTriggerPhrase(trigger: string): string {
  return trigger === "after_create" || trigger === "after_update" || trigger === "after_delete" ? t(`aiCard.trigger.${trigger}`) : trigger;
}

// The 11 backend condition operators (workflow.go validOperators) → display tokens, same vocabulary
// as the run-history trace (lib/workflowRuns OP_DISPLAY).
const WORKFLOW_OP_TEXT: Record<string, string> = { eq: "=", neq: "≠", gt: ">", gte: "≥", lt: "<", lte: "≤" };
const WORKFLOW_OP_WORD: Record<string, I18nKey> = {
  contains: "aiCard.op.contains", isnull: "aiCard.op.isnull", notnull: "aiCard.op.notnull",
  changed: "aiCard.op.changed", changedTo: "aiCard.op.changedTo",
};

// A draft value as compact display text. Strings render bare (chips already isolate them); null is
// spelled out; structures fall back to JSON so nothing renders as "[object Object]".
function draftValueText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null) return "null";
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// workflowRuleText: one condition predicate as compact text — "stage = won", "note rỗng".
// Null-operators carry no value; for any other op only a PRESENT value renders (never fabricated).
export function workflowRuleText(r: AiDraftWorkflowRule): string {
  const op = WORKFLOW_OP_TEXT[r.op] ?? (WORKFLOW_OP_WORD[r.op] ? t(WORKFLOW_OP_WORD[r.op]) : r.op);
  if (r.op === "isnull" || r.op === "notnull" || r.value === undefined) return `${r.field} ${op}`;
  return `${r.field} ${op} ${draftValueText(r.value)}`;
}

// conditionJoiner: the word between two condition chips. The engine reads match case-insensitively
// (workflow.EvaluateWithBefore: EqualFold "or"), so the card does too — a strict === "or" showed a
// draft buffered as "OR" joined by "và" while Apply created an OR rule.
export function conditionJoiner(match: unknown): "hoặc" | "và" {
  return typeof match === "string" && match.trim().toLowerCase() === "or" ? "hoặc" : "và";
}

// workflowActionText: one action as a readable Vietnamese sentence for the card's "THÌ" row.
// field_update with an absent value stays honest about NIT-1 (absent ≠ null): the text says only
// what is present. An unknown action type renders its raw token.
export function workflowActionText(a: AiDraftWorkflowAction): string {
  switch (a.type) {
    case "field_update":
      return a.value !== undefined
        ? tf("aiCard.action.fieldUpdate", { field: a.field ?? "?", value: draftValueText(a.value) })
        : tf("aiCard.action.fieldUpdateNoValue", { field: a.field ?? "?" });
    case "in_app_notification": {
      const who = a.userId === "$owner" ? t("aiCard.action.owner") : (a.userId || "?");
      return a.title ? tf("aiCard.action.notify", { who, title: a.title }) : tf("aiCard.action.notifyNoTitle", { who });
    }
    case "email_alert":
      return a.subject ? tf("aiCard.action.email", { to: a.to ?? "?", subject: a.subject }) : tf("aiCard.action.emailNoSubject", { to: a.to ?? "?" });
    // Off the propose schema but not denied (specs/046 §3): a model can still buffer one, and Apply
    // sends its field — so the card must name the number it will overwrite (DAILY-QUEUE #164).
    case "set_elapsed":
      return tf("aiCard.action.setElapsed", { field: a.field ?? "?" });
    // Only seen in the BEFORE side of an existing rule (the AI never proposes one): the url is
    // already cut to scheme://host, a vault connection shows as such — never a key.
    case "outbound_webhook":
      return a.credentialId ? t("aiCard.action.webhookVault") : tf("aiCard.action.webhook", { url: a.url ?? "?" });
    default:
      // Apply sends `field` for any type, so an unknown action still shows the field it targets.
      // The machine token itself is not shown (S13) — a person cannot read it.
      return a.field ? tf("aiCard.action.unknownField", { field: a.field }) : t("aiCard.action.unknown");
  }
}

// reportTypeLabel: report type → the badge label the report card shows. An unknown token renders raw (honest — no
// guessing).
export function reportTypeLabel(type: string): string {
  return type === "summary" || type === "matrix" || type === "tabular" ? t(`aiCard.reportType.${type}`) : type;
}

// The 5 aggregate metrics (services validateReportConfig vocabulary), the 5 time buckets (W4 vocabulary) and the 7
// having operators → their words in lib/i18n (aiCard.metric.*, aiCard.bucket.*, aiCard.having.*). An unknown token
// renders raw.
const REPORT_METRICS = new Set(["count", "sum", "avg", "min", "max"]);
const REPORT_BUCKETS = new Set(["day", "week", "month", "quarter", "year"]);
const HAVING_OPS = new Set(["gt", "gte", "lt", "lte", "eq", "neq", "between"]);
const metricText = (m: string) => (REPORT_METRICS.has(m) ? t(`aiCard.metric.${m}` as I18nKey) : m);
const bucketText = (b: string) => (REPORT_BUCKETS.has(b) ? t(`aiCard.bucket.${b}` as I18nKey) : b);
const havingOpText = (op: string) => (HAVING_OPS.has(op) ? t(`aiCard.having.${op}` as I18nKey) : op);

// reportSummaryText: the aggregation as ONE compact readable Vietnamese sentence for the report
// card body — never a raw JSON dump. "Đếm số bản ghi · nhóm theo stage", "Tổng amount · nhóm theo
// created_at · gộp theo tháng"; tabular → "Bảng dữ liệu N cột". Renders only what's PRESENT
// (a malformed persisted draft must not get an invented metric/field); unknown tokens render raw;
// an unsummarizable config yields "" so the card can state that honestly.
export function reportSummaryText(type: string, cfg: AiDraftReportConfig | null | undefined): string {
  const c = cfg ?? {};
  if (type === "tabular") {
    const n = Array.isArray(c.columns) ? c.columns.length : 0;
    return n > 0 ? tf("aiCard.report.tabular", { n }) : "";
  }
  const parts: string[] = [];
  if (c.metric) {
    const base = metricText(c.metric);
    // count takes no field; the numeric metrics read naturally as "Tổng amount" / "Trung bình amount"
    parts.push(c.metric !== "count" && c.field ? `${base} ${c.field}` : base);
  }
  if (c.groupBy) parts.push(tf("aiCard.report.groupBy", { x: c.groupBy }));
  if (c.bucket) parts.push(tf("aiCard.report.bucket", { x: bucketText(c.bucket) }));
  const axisText = (a: AiDraftReportAxis) =>
    a.bucket ? `${a.field} (${bucketText(a.bucket)})` : a.field;
  if (c.columnGroup?.field) parts.push(tf("aiCard.report.columnGroup", { x: axisText(c.columnGroup) }));
  if (Array.isArray(c.subGroups) && c.subGroups.length > 0) {
    parts.push(tf("aiCard.report.subGroups", { x: c.subGroups.map(axisText).join(", ") }));
  }
  if (c.having) {
    const h = c.having;
    const what = t(h.on === "count" ? "aiCard.report.havingCount" : "aiCard.report.havingValue");
    // cells for a matrix, the deepest-level groups for a multi-level summary.
    const unit = t(type === "matrix" ? "aiCard.report.unitCell" : "aiCard.report.unitGroup");
    const op = havingOpText(h.op);
    const hv = Array.isArray(h.values) ? h.values : [];
    const vals = h.op === "between" ? hv.join(" – ") : (hv[0] ?? "");
    parts.push(tf("aiCard.report.having", { unit, what, op, vals }).trim());
  }
  return parts.join(" · ");
}

// Row labels of the edit_field diff table (aiCard.editField.*) — one entry per editable key, fixed display order.
const EDIT_FIELD_KEY_LABEL: Record<EditFieldPatchKey, I18nKey> = {
  label: "aiCard.editField.label",
  description: "aiCard.editField.description",
  isRequired: "aiCard.editField.isRequired",
  defaultValue: "aiCard.editField.defaultValue",
  options: "aiCard.editField.options",
};

// One row of the edit_field diff table. before/after are ALREADY display text (mono cell values);
// `before` is undefined when the wire draft didn't carry the old value — rendered "—", never
// invented.
export interface AiEditFieldDiffRow { key: string; label: string; before?: string; after: string }

// A patch value as compact display text for the diff table. Booleans read as Vietnamese (the
// isRequired toggle is có/không everywhere else in the Studio); the rest reuses draftValueText
// (strings bare, null spelled out, structures as JSON — never "[object Object]").
function editValueText(v: unknown): string {
  if (typeof v === "boolean") return t(v ? "aiCard.yes" : "aiCard.no");
  return draftValueText(v);
}

// editFieldDiffRows: the diff set of an edit_field proposal — ONE row per key present in `after`
// (the contract: after carries only changed keys). A key present only in `before` is deliberately
// NOT a row: rendering it would claim a change the draft doesn't propose.
export function editFieldDiffRows(e: AiDraftEditField): AiEditFieldDiffRow[] {
  const before = e.before ?? {};
  const after = e.after ?? {};
  const rows: AiEditFieldDiffRow[] = [];
  for (const k of EDIT_FIELD_PATCH_KEYS) {
    if (after[k] === undefined) continue;
    rows.push({
      key: k,
      label: t(EDIT_FIELD_KEY_LABEL[k]),
      before: before[k] !== undefined ? editValueText(before[k]) : undefined,
      after: editValueText(after[k]),
    });
  }
  return rows;
}

// The records card's table model: first `limit` rows + the honest "+N nữa" count. Columns are the
// union of the PREVIEW rows' keys in first-seen order (what the visible rows can actually show).
export interface AiRecordsPreview {
  columns: string[];
  rows: Array<Record<string, unknown>>;
  moreCount: number;
  total: number;
}

export function recordsPreviewModel(
  rows: Array<Record<string, unknown>> | null | undefined,
  limit = 5,
): AiRecordsPreview {
  const all = objectRows(rows);
  const preview = all.slice(0, limit);
  const columns: string[] = [];
  for (const row of preview) {
    for (const k of Object.keys(row)) {
      if (!columns.includes(k)) columns.push(k);
    }
  }
  return { columns, rows: preview, moreCount: Math.max(0, all.length - preview.length), total: all.length };
}

// recordCellText: one preview cell as display text. An absent key renders "—" (the row simply
// doesn't set that column); everything else goes through draftValueText.
export function recordCellText(v: unknown): string {
  return v === undefined ? "—" : draftValueText(v);
}

// slaText: an approval step's SLA seconds as compact Vietnamese (fmtSLA's phút/giờ/ngày
// vocabulary). "" for absent/junk — the card then simply omits the SLA suffix, never invents one.
export function slaText(seconds: number | undefined): string {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))} phút`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)} giờ`;
  return `${Math.round(seconds / 86400)} ngày`;
}

// approvalStepText: one approver step as the card's numbered line — "1. user:<ref> · SLA 1 giờ".
// Raw machine tokens on purpose (approverRef is a resolved id, honest as-is); only a PRESENT SLA
// renders.
export function approvalStepText(s: AiDraftApprovalStep, index: number): string {
  const sla = slaText(s.slaSeconds);
  if (Array.isArray(s.approvers) && s.approvers.length > 0) {
    // A council (S4): every approver, then the policy that ends the step.
    const who = s.approvers.map((ap) => `${ap.type}:${ap.ref ?? ap.id ?? "?"}`).join(", ");
    const policy = s.completion === "any" ? "người đầu tiên quyết"
      : s.completion === "quorum" ? `đủ ${s.completionPercent ?? "?"}% số người duyệt`
      : "tất cả phải duyệt";
    return `${index + 1}. [${who}] · ${policy}${sla ? ` · SLA ${sla}${s.slaClock === "wall" ? " (giờ đồng hồ)" : ""}` : ""}`;
  }
  const ref = s.approverRef ?? s.approverId ?? "?";
  return `${index + 1}. ${s.approverType}:${ref}${sla ? ` · SLA ${sla}${s.slaClock === "wall" ? " (giờ đồng hồ)" : ""}` : ""}`;
}

// approvalCriteriaCount: how many entry-criteria rules a draft's opaque criteria document
// carries ({match, rules:[...]}) — 0 for absent/unrecognizable (the card then says "mọi bản
// ghi", the BE's own NULL-criteria semantics).
export function approvalCriteriaCount(criteria: Record<string, unknown> | null | undefined): number {
  const rules = criteria?.rules;
  return Array.isArray(rules) ? rules.length : 0;
}

// approvalCriteriaRules: the well-formed rules of a draft's opaque criteria document, for the card's
// chips (the same workflowRuleText vocabulary as the workflow card). Malformed entries are skipped —
// the document was server-validated at propose time; this only guards a hand-edited session.
export function approvalCriteriaRules(criteria: Record<string, unknown> | null | undefined): AiDraftWorkflowRule[] {
  const rules = criteria?.rules;
  if (!Array.isArray(rules)) return [];
  return rules.flatMap((r): AiDraftWorkflowRule[] => {
    const { field, op, value } = (r && typeof r === "object" ? r : {}) as { field?: unknown; op?: unknown; value?: unknown };
    return typeof field === "string" && typeof op === "string" ? [{ field, op, value }] : [];
  });
}

// One widget of the dashboard card body: title + type token. A missing title falls back to the
// positional "Widget N" (the viewer's own convention); a missing type renders "?" honestly.
export interface AiDashboardWidgetLine { title: string; type: string }

export function dashboardWidgetLines(d: AiDraftDashboard): AiDashboardWidgetLine[] {
  return (Array.isArray(d.widgets) ? d.widgets : []).map((w, i) => {
    const o = (w && typeof w === "object" ? w : {}) as { title?: unknown; type?: unknown };
    return {
      title: typeof o.title === "string" && o.title !== "" ? o.title : `Widget ${i + 1}`,
      type: typeof o.type === "string" && o.type !== "" ? o.type : "?",
    };
  });
}

// workQueueSummaryText: the queue card's one-line digest — filter count (or the honest
// every-record wording), who sees it, and whether it lands on the home screen.
export function workQueueSummaryText(q: AiDraftWorkQueue): string {
  const n = Array.isArray(q.filters) ? q.filters.length : 0;
  const parts = [
    n > 0 ? `${n} điều kiện lọc` : "mọi bản ghi",
    q.roleName ? `vai ${q.roleName}` : "mọi người thấy",
    (q.showOnHome ?? true) ? "hiện trên Trang chủ" : "ẩn khỏi Trang chủ",
  ];
  return parts.join(" · ");
}

// The per-row outcome subset recordsApplyMessage needs — mirrors lib/api's BulkRowResult without
// importing it (this module stays fetch-free/type-light). `index` is the 0-based submit position.
export interface AiBulkRowOutcome { index: number; status: string; error?: string; code?: string }

// recordsApplyMessage: the honest per-row failure summary of a bulk apply — null when every row
// was created (caller shows plain success). Names the first 3 failing rows (1-based, matching what
// a human counts in the preview) with the server's own message, then an honest "+N lỗi khác".
export function recordsApplyMessage(created: number, total: number, results: AiBulkRowOutcome[]): string | null {
  const failed = (results ?? []).filter((r) => r.status !== "created");
  if (failed.length === 0) return null;
  const detail = failed
    .slice(0, 3)
    .map((r) => `dòng ${r.index + 1}: ${r.error || r.code || "lỗi không rõ"}`)
    .join(" · ");
  const more = failed.length > 3 ? ` (+${failed.length - 3} lỗi khác)` : "";
  return `Đã tạo ${created}/${total} dòng — ${failed.length} dòng lỗi: ${detail}${more}.`;
}

// ── 429 throttle notices (E9 M4 — spec §12, MED-1) ───────────────────────────────────────────────
// The chat endpoint can refuse a turn with 429 + one of three codes (backend BudgetGuard). Each maps
// to an HONEST transient notice: the refused turn was never persisted server-side (lazy-persist), so
// the composer stays usable and "Thử lại" simply re-sends. `retryAfterSec` comes from the Retry-After
// response header WHEN the browser can read it (same-origin, or CORS Expose-Headers); when it can't
// — the current cross-origin dev setup — the seconds-free wording is used. Never a fake countdown.
// Returns null for any non-limit code so callers fall back to the central errorMessage() map.
export function aiLimitMessage(code: string, retryAfterSec?: number): string | null {
  // Only trust a finite positive number; round UP so "thử lại sau ~Ns" never understates the wait.
  const secs =
    typeof retryAfterSec === "number" && Number.isFinite(retryAfterSec) && retryAfterSec > 0
      ? Math.ceil(retryAfterSec)
      : undefined;
  switch (code) {
    case "AI_RATE_LIMITED":
      return secs !== undefined ? tf("aiCard.limit.rateRetryIn", { s: secs }) : t("aiCard.limit.rate");
    case "AI_CONCURRENCY_LIMIT":
      return t("aiCard.limit.concurrency");
    case "AI_BUDGET_EXCEEDED":
      // Daily budget is per tenant per UTC day (spec §12) → reset 00:00 UTC = 07:00 giờ Việt Nam.
      return t("aiCard.limit.budget");
    default:
      return null;
  }
}

// ── Display helpers ────────────────────────────────────────────────────────────────────────────
// formatTokens: subtle per-turn cost hint. 2788 → "2.8k", 449 → "449", 0/negatives → "0".
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n < 1000) return String(Math.round(n));
  return `${(n / 1000).toFixed(1)}k`;
}

// A stable-ish id for a turn (client-only, for React keys). crypto.randomUUID when available.
export function newTurnId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `t_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
}

const CHANGE_WORD: Record<string, string> = {
  actions: "hành động", rules: "điều kiện", sections: "nhóm", fields: "trường", columns: "cột", nav: "mục menu",
  value: "giá trị", field: "trường", op: "phép so sánh", match: "cách ghép", trigger: "thời điểm chạy",
  isActive: "bật/tắt", entityName: "bảng", name: "tên", label: "nhãn", isRequired: "bắt buộc",
  defaultValue: "giá trị mặc định", options: "tuỳ chọn", description: "mô tả", type: "loại", message: "nội dung",
  title: "tiêu đề", subject: "tiêu đề email", to: "người nhận", url: "địa chỉ", intervalSeconds: "chu kỳ chạy",
  condition: "điều kiện", config: "cấu hình", layout: "bố cục", viewType: "loại màn",
};
const CHANGE_PLURAL = new Set(["actions", "rules", "sections", "fields", "columns", "nav"]);
const CHANGE_SKIP = new Set(["condition", "layout", "config"]);

// changePathText: a metadiff path as words. An array that changed LENGTH is reported as the array
// itself ("actions" → "các hành động (số lượng đổi)"); an element path is 1-based ("actions[1]" →
// "hành động 2"); wrapper keys are dropped ("condition.rules[0].value" → "điều kiện 1 · giá trị").
export function changePathText(path: string): string {
  if (path === "(root)") return "toàn bộ";
  const segs = path.split(".");
  const last = segs[segs.length - 1];
  const word = (k: string) => CHANGE_WORD[k] ?? k;
  if (CHANGE_PLURAL.has(last)) return `các ${word(last)} (số lượng đổi)`;
  const parts = segs
    .filter((sg, i) => i === segs.length - 1 || !CHANGE_SKIP.has(sg))
    .map((sg) => {
      const m = /^([A-Za-z_]+)\[(\d+)\]$/.exec(sg);
      return m ? `${word(m[1])} ${Number(m[2]) + 1}` : word(sg);
    });
  return parts.join(" · ");
}

export const CHANGE_OP_LABEL: Record<string, string> = {
  create: "Tạo mới", update: "Sửa cái đang có", delete: "Xoá",
};

// changeSideLines: one side (before/after) of a change as a few short readable lines, reusing the
// card vocabulary (layout summary, rule/action text, report sentence). Never a raw JSON dump.
export function changeSideLines(
  docType: string,
  side: Record<string, unknown> | null | undefined,
  other?: Record<string, unknown> | null,
): string[] {
  if (!side) return [];
  const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : JSON.stringify(v));
  if (docType === "app") {
    // App menu: one line per row; on the After side a row the Before side lacks reads "+ name".
    type Row = { type?: string; name?: string; label?: string };
    const rows = (side.nav as Row[] | undefined) ?? [];
    const had = new Set(((other?.nav as Row[] | undefined) ?? []).map((r) => `${r.type}:${r.name}`));
    return rows.map((r) => `${other && !had.has(`${r.type}:${r.name}`) ? "+ " : ""}${r.label || r.name}`);
  }
  if (docType === "layout") {
    const sum = summarizeLayoutDraft(str(side.viewType), side.layout as Record<string, unknown> | undefined);
    if (sum.kind === "sections") {
      return sum.sections.map((s) => `${s.title} · ${s.columns} cột · ${s.fields.join(", ")}`);
    }
    if (sum.kind === "columns") return [sum.columns.join(", ")];
    return [];
  }
  if (docType === "workflow") {
    const cond = side.condition as AiDraftWorkflowCondition | null | undefined;
    const rules = cond?.rules ?? [];
    const acts = Array.isArray(side.actions) ? (side.actions as AiDraftWorkflowAction[]) : [];
    return [
      workflowTriggerPhrase(str(side.trigger)),
      ...(rules.length > 0 ? [`Nếu ${rules.map(workflowRuleText).join(conditionJoiner(cond?.match) === "hoặc" ? " hoặc " : " và ")}`] : []),
      ...acts.map(workflowActionText),
    ];
  }
  if (docType === "report") {
    const line = reportSummaryText(str(side.type), side.config as AiDraftReportConfig | undefined);
    return line ? [line] : [];
  }
  if (docType === "role") {
    // propose_access: the role's grants on the tables it touches (and its "*" rows), one line per table.
    const grants = Array.isArray(side.grants) ? (side.grants as RoleGrant[]) : [];
    const labels = (side.labels ?? {}) as Record<string, string>;
    return Object.keys(labels).map((n) => tf("studio.assistant.access.side", { table: labels[n] || n, actions: accessText(grants, n) }));
  }
  return Object.entries(side)
    .filter(([, v]) => v !== "" && v != null && !(Array.isArray(v) && v.length === 0))
    .slice(0, 6)
    .map(([k, v]) => `${CHANGE_WORD[k] ?? k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`);
}
