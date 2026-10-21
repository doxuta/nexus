// Applies one assistant proposal (15 kinds) through the SAME gated admin APIs the manual builders
// use — the AI never writes schema itself (HITL). Pure async: no React, no window. Moved verbatim
// out of the old AI Builder page (specs/076 ai1) so the card UI and later the side panel
// share one implementation. Every apply is idempotent: a second click either 409s into an update
// (workflow, report, app) or is blocked by the persisted card state (lib/aiApplyState.ts).
import {
  createEntityWithAcl, addField, createRelationship, createLayout, createWorkflow,
  createReport, createApp, replaceAppNav, appAclBackfill, adminApps, adminApp, listEntities, ApiError,
  listWorkflows, updateWorkflow, listReports, updateReport,
  listFields, updateField, deleteField, deleteEntity, createRecordsBulk, createSampleRecords,
  createApprovalProcess, createDashboard, createWorkQueue, listRoles, listUserDirectory,
  listDraftChanges, discardDraft, getRole, updateRolePermissions, type DraftChange, type PublishState, type RoleGrant,
} from "@/lib/api";
import { mergeAccessGrants } from "@/lib/accessLevels";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { isBeforeTrigger } from "@/lib/workflowBefore";
import { addEntityToAppNav, mergeAppNavKeys, navRow } from "@/lib/appNav";
import {
  draftEntityToPayload, draftFieldToInput, draftRelationshipToPayload,
  draftLayoutToPayload, draftWorkflowToPayload, draftReportToPayload,
  draftAppToPayload, resolveAppNavEntities,
  getEditFieldDraft, getDeleteFieldDraft, getDeleteEntityDraft, getRecordsDraft,
  draftEditFieldToUpdatePayload, draftRecordsRows, recordsApplyMessage,
  getApprovalDraft, getDashboardDraft, getWorkQueueDraft,
  approvalDraftRefs, draftApprovalToPayload, draftDashboardToPayload, draftWorkQueueToPayload,
  resolveApprovalApprovers, resolveRoleByName, getAccessDraft,
  needsLiveConfirm, appliedFootKey, bornHidden, draftDocType, isDestructiveDraft,
  type AiDraftAccess, type AiDraftItem,
} from "@/lib/aiBuilder";
import { classOf } from "@/lib/draftClass";
import type { I18nKey } from "@/lib/i18n/vi";
import type { ApplyState } from "@/lib/aiApplyState";

export { needsLiveConfirm };


export interface ApplyDeps {
  /** Proposed rows go to the sample-data route (marked, no automation) when true, /:entity/bulk when false. */
  asSample: boolean;
  /**
   * Asked before EVERY write: false ⇒ the project changed under the apply (FR-108), so the remaining
   * steps must not run against the wrong customer. Omitted ⇒ always in scope.
   */
  assertScope?: () => boolean;
}

/** Terminal (or retryable-error) result of one apply. `error` is set only for a thrown failure. */
export interface ApplyOutcome extends ApplyState {
  error?: unknown;
  /** The failure is a 400/409/422 the model can correct: only those are reported back to it (FR-112). */
  reportable?: boolean;
}

/** Key of a card's apply state: one per (assistant turn, draft position). */
export function proposalKey(turnId: string, index: number): string {
  return `${turnId}:${index}`;
}

/** Where a card is saved: its turn's server seq (a read-back turn is `m_<seq>`, serverMessagesToTurns) and its index. null for a turn of this tab. */
export function proposalPlace(key: string): { seq: number; index: number } | null {
  const m = /^m_(\d+):(\d+)$/.exec(key);
  return m ? { seq: Number(m[1]), index: Number(m[2]) } : null;
}

class ScopeChanged extends Error {}

// Every write goes through `w`, which re-checks scope first.
function makeWrites(deps: ApplyDeps) {
  const w = <A extends unknown[], R>(fn: (...a: A) => Promise<R>) => async (...a: A): Promise<R> => {
    if (deps.assertScope && !deps.assertScope()) throw new ScopeChanged();
    return fn(...a);
  };
  return {
    createEntityWithAcl: w(createEntityWithAcl), addField: w(addField), createRelationship: w(createRelationship),
    createLayout: w(createLayout), createWorkflow: w(createWorkflow), updateWorkflow: w(updateWorkflow),
    createReport: w(createReport), updateReport: w(updateReport), createApp: w(createApp),
    replaceAppNav: w(replaceAppNav), appAclBackfill: w(appAclBackfill), updateField: w(updateField),
    deleteField: w(deleteField), deleteEntity: w(deleteEntity), createRecordsBulk: w(createRecordsBulk),
    createSampleRecords: w(createSampleRecords), createApprovalProcess: w((p: ReturnType<typeof draftApprovalToPayload>) => createApprovalProcess(p)),
    createDashboard: w(createDashboard), createWorkQueue: w(createWorkQueue), addEntityToAppNav: w(addEntityToAppNav),
    updateRolePermissions: w(updateRolePermissions),
  };
}
type Writes = ReturnType<typeof makeWrites>;

export async function applyProposal(item: AiDraftItem, deps: ApplyDeps): Promise<ApplyOutcome> {
  try {
    return await run(item, deps, makeWrites(deps));
  } catch (e) {
    if (e instanceof ScopeChanged) return { status: "warn", message: t("studio.assistant.apply.scopeChanged") };
    // 4xx surfaces AT apply-time (table exists, relation target missing…): friendly VN, conversation continues.
    const message = e instanceof ApiError && e.code === "APP_ALREADY_EXISTS"
      ? t("studio.assistant.apply.appExists")
      : errorMessage(e);
    const reportable = e instanceof ApiError && [400, 409, 422].includes(e.status);
    return { status: "error", message, error: e, reportable };
  }
}

const err = (message: string): ApplyOutcome => ({ status: "error", message });

async function run(item: AiDraftItem, deps: ApplyDeps, api: Writes): Promise<ApplyOutcome> {
  if (item.kind === "entity" && item.entity) {
    const p = draftEntityToPayload(item.entity);
    // specs/067 L10-2: a table built INSIDE an app (server-stamped appName) is created WITH that app's
    // id so default grants are seeded, then added to the menu. Without this staff could not see it.
    const wantApp = item.entity.appName;
    const app = wantApp ? (await adminApps()).find((a) => a.name === wantApp) : undefined;
    const created = await api.createEntityWithAcl(p.name, p.label, p.description, p.fields, app?.id);
    const base = { entityName: p.name };
    if (!wantApp) return { status: "done", ...base, message: t("page.studio.ai.apply.noApp") };
    if (!app) return { status: "warn", ...base, message: t("page.studio.ai.apply.appGone") };
    let navOk = true;
    try {
      const entId = created.entity.id ?? (await listEntities()).find((e) => e.name === p.name)?.id;
      if (!entId) throw new Error("entity id unresolved");
      await api.addEntityToAppNav(app.id, entId);
    } catch (e) {
      if (e instanceof ScopeChanged) throw e;
      navOk = false;
    }
    const acl = created.aclSeed;
    if (!navOk) return { status: "warn", ...base, message: tf("page.studio.ai.apply.navFailed", { app: app.label }) };
    if (acl.appScoped && !acl.seeded) return { status: "warn", ...base, message: acl.warning || t("page.dataModel.aclWarn") };
    return { status: "done", ...base, message: tf("page.studio.ai.apply.seeded", { app: app.label }) };
  }
  if (item.kind === "field" && item.field?.entity) {
    await api.addField(item.field.entity, draftFieldToInput(item.field));
    return { status: "done", entityName: item.field.entity };
  }
  if (item.kind === "relationship" && item.relationship) {
    await api.createRelationship(draftRelationshipToPayload(item.relationship));
    return { status: "done" };
  }
  if (item.kind === "layout" && item.layout) {
    // POST /admin/layouts is an UPSERT on (tenant, entity, view): re-applying overwrites, no 409.
    await api.createLayout(draftLayoutToPayload(item.layout));
    return { status: "done" };
  }
  if (item.kind === "workflow" && item.workflow) {
    // A second Apply 409s instead of minting a double-firing twin — resolve the existing rule by name and UPDATE it.
    try {
      await api.createWorkflow(draftWorkflowToPayload(item.workflow));
    } catch (e) {
      if (!(e instanceof ApiError) || e.code !== "WORKFLOW_ALREADY_EXISTS") throw e;
      const existing = (await listWorkflows()).find((x) => x.name === item.workflow!.name);
      // specs/048: never overwrite a same-named before rule (a refusal built by hand) with an AI after_* draft.
      if (!existing || isBeforeTrigger(existing.trigger)) throw e;
      await api.updateWorkflow(existing.id, draftWorkflowToPayload(item.workflow));
    }
    return { status: "done" };
  }
  if (item.kind === "report" && item.report) {
    try {
      await api.createReport(draftReportToPayload(item.report));
    } catch (e) {
      if (!(e instanceof ApiError) || e.code !== "REPORT_ALREADY_EXISTS") throw e;
      const existing = (await listReports()).find((r) => r.name === item.report!.name);
      if (!existing) throw e;
      await api.updateReport(existing.id, draftReportToPayload(item.report));
    }
    return { status: "done" };
  }
  if (item.kind === "app" && item.app) return applyApp(item.app, api);
  const ef = item.kind === "edit_field" ? getEditFieldDraft(item) : undefined;
  if (ef) {
    // The field id is resolved against the live registry AT APPLY TIME (a propose-time id could be stale).
    const live = (await listFields(ef.entity)).find((f) => f.name === ef.field);
    if (!live?.id) return err(tf("studio.assistant.apply.fieldMissing", { field: ef.field, table: ef.entity }));
    await api.updateField(live.id, draftEditFieldToUpdatePayload(ef));
    return { status: "done", entityName: ef.entity };
  }
  const df = item.kind === "delete_field" ? getDeleteFieldDraft(item) : undefined;
  if (df) {
    // Destructive: the caller only gets here after the confirm step.
    const live = (await listFields(df.entity)).find((f) => f.name === df.field);
    if (!live?.id) return err(tf("studio.assistant.apply.fieldMissingDelete", { field: df.field, table: df.entity }));
    await api.deleteField(live.id);
    return { status: "done", entityName: df.entity };
  }
  const de = item.kind === "delete_entity" ? getDeleteEntityDraft(item) : undefined;
  if (de) {
    await api.deleteEntity(de.entity);
    return { status: "done" };
  }
  const rc = item.kind === "records" ? getRecordsDraft(item) : undefined;
  if (rc) {
    // mode=partial: one bad row never blocks the rest. all created ⇒ done; some ⇒ terminal warn
    // (re-apply would duplicate the created rows); none ⇒ retryable error.
    const rows = draftRecordsRows(rc);
    if (rows.length === 0) return err(t("studio.assistant.apply.noRows"));
    const res = deps.asSample
      ? await api.createSampleRecords(rc.entity, rows, "partial")
      : await api.createRecordsBulk(rc.entity, rows, "partial");
    const failMsg = recordsApplyMessage(res.created, rows.length, res.results);
    return failMsg === null
      ? { status: "done", entityName: rc.entity }
      : { status: res.created > 0 ? "warn" : "error", message: failMsg, entityName: rc.entity };
  }
  const ap = item.kind === "approval" ? getApprovalDraft(item) : undefined;
  if (ap) {
    // Approvers arrive as a role NAME / user EMAIL (the model never knows ids): resolve against the live
    // registries NOW; an unresolvable one is an honest error, never stored.
    const refs = approvalDraftRefs(ap);
    const roles = refs.some((r) => r.type === "role") ? await listRoles() : [];
    const emails = [...new Set(refs.filter((r) => r.type === "user" && r.ref.trim() !== "").map((r) => r.ref.trim()))];
    const users = (await Promise.all(emails.map((e) => listUserDirectory(e, 5)))).flat();
    const resolved = resolveApprovalApprovers(ap, roles, users);
    if ("error" in resolved) return err(resolved.error);
    await api.createApprovalProcess(draftApprovalToPayload(resolved.draft));
    return { status: "done" };
  }
  const db = item.kind === "dashboard" ? getDashboardDraft(item) : undefined;
  if (db) {
    const p = draftDashboardToPayload(db);
    const created = await api.createDashboard(p.name, p.widgets, p.description);
    return { status: "done", createdId: created.id };
  }
  const wq = item.kind === "workqueue" ? getWorkQueueDraft(item) : undefined;
  if (wq) {
    // An unresolvable role is an HONEST error — never a silent fall-back to visible-to-everyone.
    let roleId: string | undefined;
    if (wq.roleName) {
      roleId = resolveRoleByName(wq.roleName, await listRoles());
      if (!roleId) return err(tf("studio.assistant.apply.roleMissing", { role: wq.roleName }));
    }
    await api.createWorkQueue(draftWorkQueueToPayload(wq, roleId));
    return { status: "done" };
  }
  const ac = item.kind === "access" ? getAccessDraft(item) : undefined;
  if (ac) return applyAccess(ac, Array.isArray(item.change?.before?.grants) ? (item.change.before.grants as RoleGrant[]) : undefined, api);
  // Unknown kind, or a known kind whose payload is missing (a persisted draft from a newer backend).
  return err(t("studio.assistant.apply.incomplete"));
}

// propose_access: the role is named (the model never knows ids) and PUT /admin/roles/:id replaces the whole grant set,
// so the role is read NOW, the proposal is added by the one additive rule and the set is written back (a traced role
// entry: Bảo trì › Lịch sử can undo it). A table that does not exist yet is an honest error: a grant on nothing would wait
// for whatever table gets that name later.
// The role's grants on the card's tables and its "*" rows (what the server projected as the card's before-image), order-free.
function sameAccess(live: readonly RoleGrant[], before: readonly RoleGrant[] | undefined, ac: AiDraftAccess): boolean {
  if (!before) return false; // no image to compare with: never write blind
  const tables = new Set(Object.keys(ac.labels ?? {}));
  const sig = (gs: readonly RoleGrant[]) => gs.filter((g) => g.entityName === "*" || tables.has(g.entityName))
    .map((g) => `${g.entityName}|${g.action}|${g.accessLevel}`).sort().join(",");
  return sig(live) === sig(before);
}

async function applyAccess(ac: AiDraftAccess, before: RoleGrant[] | undefined, api: Writes): Promise<ApplyOutcome> {
  const id = resolveRoleByName(ac.role, await listRoles());
  if (!id) return err(tf("studio.assistant.apply.accessRoleMissing", { role: ac.roleLabel || ac.role }));
  const have = new Set((await listEntities()).map((e) => e.name));
  const missing = [...new Set(ac.grants.map((g) => g.entityName))].filter((n) => !have.has(n));
  if (missing.length > 0) return err(tf("studio.assistant.apply.accessTableMissing", { list: missing.map((n) => ac.labels?.[n] || n).join(", ") }));
  const role = await getRole(id);
  // PUT replaces the whole set and carries no version: if the role moved on the card's tables since the proposal, writing
  // would drop that change and the card's Trước/Sau would no longer be true. Compare with the card's before-image first.
  if (!sameAccess(role.actions ?? [], before, ac)) return err(t("studio.assistant.apply.accessStale"));
  const actions = mergeAccessGrants(role.actions ?? [], ac.grants);
  if (JSON.stringify(actions) === JSON.stringify(role.actions ?? [])) return { status: "done", message: t("studio.assistant.apply.accessNothing") };
  await api.updateRolePermissions(id, { name: role.name, label: role.label, actions });
  return { status: "done" };
}

// Creates the app shell (409 ⇒ MERGE into the existing one: "thêm bảng X vào app Y" is the ordinary
// request), resolves nav table names to ids and attaches them, then backfills grants.
async function applyApp(app: NonNullable<AiDraftItem["app"]>, api: Writes): Promise<ApplyOutcome> {
  let created: { id: string };
  let merged = false;
  try {
    created = await api.createApp(draftAppToPayload(app));
  } catch (e) {
    if (!(e instanceof ApiError) || e.code !== "APP_ALREADY_EXISTS") throw e;
    const existing = (await adminApps()).find((a) => a.name === app.name);
    if (!existing) throw e;
    created = { id: existing.id };
    merged = true;
  }
  try {
    const entities = await listEntities();
    const { items: navItems, missing } = resolveAppNavEntities(app.navEntities, entities);
    // replaceAppNav is replace-ALL: on an existing app the current nav is read and prepended, or the
    // app would silently lose every screen it had. Order preserved, duplicates dropped by refId.
    let finalNav = navItems;
    if (merged) {
      const current = await adminApp(created.id);
      finalNav = mergeAppNavKeys((current.items ?? []).map(navRow), navItems);
    }
    if (finalNav.length > 0) await api.replaceAppNav(created.id, finalNav);

    // A brand-new table carries ZERO grants, and the AI applies tables FIRST and the app LAST, so each
    // table could not be scoped to the app at creation. The backfill is the catch-up; without it the
    // customer's staff open a workspace with nothing in it (invisible to admin: is_admin bypasses ACL).
    let aclNote = "";
    if (navItems.length > 0) {
      try {
        const acl = await api.appAclBackfill(created.id, false);
        if (acl.failed.length > 0) aclNote += tf("studio.assistant.apply.appAclFailed", { n: acl.failed.length });
      } catch (e) {
        if (e instanceof ScopeChanged) throw e;
        aclNote = t("studio.assistant.apply.appAclAll"); // never claim grants that were not written
      }
    }
    const partial = missing.length > 0
      ? tf("page.studio.ai.apply.appMissing", { n: missing.length, list: missing.join(", ") })
      : "";
    const mergedNote = merged ? t("studio.assistant.apply.appMerged") : "";
    return partial || aclNote || mergedNote
      ? { status: "warn", message: (partial + aclNote + mergedNote).trim() }
      : { status: "done" };
  } catch (e) {
    if (e instanceof ScopeChanged) throw e;
    // The app EXISTS but the nav could not be attached: partial success, fixable by hand.
    return { status: "warn", message: t("studio.assistant.apply.appNavFailed") };
  }
}

// ── specs/076 ai9: where an applied card lands (draft / live), Hoàn tác, sealed after Publish ──

/** What we know of the project's delivery. Only a clean "not delivered" answer is `undelivered`; every other failure is `unknown` (FR-130). */
export type Delivery =
  | { kind: "loading" }
  | { kind: "unknown" }
  | { kind: "undelivered" }
  | { kind: "delivered"; state: PublishState };

/** Folds a usePublishState() result into the tri-state. Fail closed: an error, a missing field or a half-loaded body is never "undelivered". */
export function deliveryOf(q: { data?: unknown; error?: unknown; isPending?: boolean }): Delivery {
  if (q.error) {
    const e = q.error;
    // A bare 404 (no body code) is a backend that predates publish/state: nothing was ever delivered there.
    return e instanceof ApiError && e.status === 404 && (!e.code || e.code === "UNKNOWN") ? { kind: "undelivered" } : { kind: "unknown" };
  }
  const d = q.data as Partial<PublishState> | undefined | null;
  if (d == null) return q.isPending ? { kind: "loading" } : { kind: "unknown" };
  if (typeof d.delivered !== "boolean" || !Array.isArray(d.draftableTypes)) return { kind: "unknown" };
  if (!d.delivered) return { kind: "undelivered" };
  if (!Array.isArray(d.directTypes)) return { kind: "unknown" };
  return { kind: "delivered", state: d as PublishState };
}

export interface CardMode {
  label: I18nKey;
  disabled: boolean;
  /** Ask in place ("Áp ngay cho người dùng thật") before writing. */
  confirm: boolean;
  /** Part of "Áp dụng tất cả". */
  inApplyAll: boolean;
  note: "checking" | "unknown" | null;
  recheck: boolean;
  /** Frozen into the card at apply time. */
  foot: I18nKey | null;
  /** The write goes to the draft, so Hoàn tác may be offered. */
  canUndo: boolean;
  headVersionNo: number;
}

const APPLY: I18nKey = "studio.assistant.card.apply";
const APPLY_DRAFT: I18nKey = "studio.assistant.draft.applyDraft";
const APPLY_NOW: I18nKey = "studio.assistant.draft.applyNow";

// The document type whose class decides the card. A new table / field / N-N relation is hidden from the customer when "field" is draftable (#390).
function classDocType(item: AiDraftItem, draftable: string[]): string {
  if (bornHidden(item.kind, draftable)) return "field";
  return item.change?.docType || draftDocType(item.kind);
}

/** Label, confirmation, apply-all membership and footer of one card, from the item and the delivery. The class comes from classOf (F1): no table here. */
export function cardMode(item: AiDraftItem, d: Delivery, asSample = true): CardMode {
  const base: CardMode = { label: APPLY, disabled: false, confirm: false, inApplyAll: true, note: null, recheck: false, foot: null, canUndo: false, headVersionNo: 0 };
  // Access applies at once to real users whether or not the project was ever published (staff may exist before the first
  // publish): always asked in place, never part of "Áp dụng tất cả".
  if (d.kind === "undelivered" && item.kind === "access") return { ...base, label: APPLY_NOW, confirm: true, inApplyAll: false };
  if (d.kind === "undelivered") return base;
  if (d.kind === "loading") return { ...base, disabled: true, inApplyAll: false, note: "checking" };
  if (d.kind === "unknown") {
    return { ...base, label: APPLY_NOW, confirm: true, inApplyAll: false, note: "unknown", recheck: true, foot: "studio.assistant.draft.footer.unknown" };
  }
  const st = d.state;
  const live = needsLiveConfirm(item, true, asSample);
  const cls = classOf(classDocType(item, st.draftableTypes), st);
  // Sample rows are marked and fire no automation (ai10): only the "real data" choice is live, so they apply at once.
  const sample = item.kind === "records" && !live;
  const asked = live || (!sample && (cls === "direct" || cls === "untraced"));
  const created = bornHidden(item.kind, st.draftableTypes);
  // delete_* ask on their own (retype / InlineConfirm in the card) and are never in apply-all.
  const own = isDestructiveDraft(item);
  const foot = !live && !sample && cls === "untraced" && !own ? "studio.assistant.draft.footer.untraced" : appliedFootKey(item, { delivered: true, draftableTypes: st.draftableTypes });
  return {
    ...base,
    // A new table / field is created hidden, not saved as a draft row (#390): its foot says so, so the button does not claim "bản nháp".
    label: asked ? APPLY_NOW : created || sample ? APPLY : APPLY_DRAFT,
    confirm: asked && !own,
    inApplyAll: !asked && !own,
    foot,
    // A new table / field writes no draft row (#390): nothing to bring back.
    canUndo: cls === "draft" && !live && !own && !created && item.kind !== "records" && item.kind !== "app",
    headVersionNo: st.headVersion?.versionNo ?? 0,
  };
}

// ── Hoàn tác: bring back exactly the draft rows this card wrote (FR-082) ──

interface Target { docType: string; docKey?: string; entity?: string; label?: string; max: number }

function targetOf(item: AiDraftItem): Target {
  const entity = item.change?.target?.entity ?? item.entity?.name ?? item.field?.entity ?? item.editField?.entity ?? item.workflow?.entityName
    ?? item.report?.entityName ?? item.layout?.entity ?? item.approval?.entityName ?? item.workqueue?.entityName;
  return {
    docType: item.change?.docType || draftDocType(item.kind),
    docKey: item.change?.docKey || undefined,
    entity,
    // The server words change.label as `Luật "X"` / `Báo cáo "X"` but the draft row carries the bare name: match the row against target.name.
    label: item.change?.target?.name || item.change?.label || undefined,
    max: 1 + (item.kind === "entity" ? item.entity?.fields?.length ?? 0 : 0),
  };
}

export type UndoCandidates = { ok: true; rows: DraftChange[] } | { ok: false; why: "seen" | "nomatch" };

/**
 * `before`/`after`: listDraftChanges() around the apply. The Hoàn tác button exists only when every candidate row is NEW (its id
 * was not in `before`) and matches the change; a row that was already there and moved means the builder had a draft of the same
 * document, so the card must not discard it (it would throw away their work too).
 */
export function resolveUndo(item: AiDraftItem, before: DraftChange[], after: DraftChange[]): UndoCandidates {
  const t = targetOf(item);
  const types = item.kind === "entity" ? ["entity", "field"] : [t.docType];
  const was = new Map(before.map((r) => [r.id, r]));
  const scoped = after.filter((r) => types.includes(r.docType));
  const matches = (r: DraftChange) =>
    t.docKey ? r.docType === t.docType && r.docKey === t.docKey
      : (!t.entity || r.entityName === t.entity) && (r.docType !== t.docType || !t.label || r.label === t.label);
  const moved = scoped.filter((r) => { const b = was.get(r.id); return !!b && (b.updatedAt !== r.updatedAt || b.updatedBy !== r.updatedBy) && matches(r); });
  if (moved.length > 0) return { ok: false, why: "seen" };
  const fresh = scoped.filter((r) => !was.has(r.id));
  if (fresh.length === 0 || fresh.length > t.max || !fresh.every(matches)) return { ok: false, why: "nomatch" };
  return { ok: true, rows: fresh };
}

async function readRows(deps: Pick<ApplyDeps, "assertScope">): Promise<DraftChange[] | null> {
  if (deps.assertScope && !deps.assertScope()) return null;
  try {
    return (await listDraftChanges()).changes;
  } catch {
    return null; // can't prove which rows are ours ⇒ no Hoàn tác (never guess)
  }
}

/** applyProposal + the before/after capture that makes Hoàn tác possible. The outcome carries the foot and, when provable, the draft rows. */
export async function applyTracked(item: AiDraftItem, deps: ApplyDeps, mode: CardMode): Promise<ApplyOutcome> {
  const before = mode.canUndo ? await readRows(deps) : null;
  const out = await applyProposal(item, deps);
  if (out.status !== "done" && out.status !== "warn") return out;
  if (mode.foot) out.foot = mode.foot;
  if (!before) return out;
  const after = await readRows(deps);
  const found = after ? resolveUndo(item, before, after) : ({ ok: false, why: "nomatch" } as const);
  if (!found.ok) return { ...out, noUndo: found.why };
  return {
    ...out,
    changeIds: found.rows.map((r) => r.id),
    headVersionNo: mode.headVersionNo,
    updatedAt: found.rows.map((r) => r.updatedAt).sort().at(-1),
    stamps: Object.fromEntries(found.rows.map((r) => [r.id, { at: r.updatedAt, by: r.updatedBy }])),
  };
}

// After a reload only the newest stamp survives: any later write is not ours.
function edited(r: DraftChange, s: ApplyState): boolean {
  const mine = s.stamps?.[r.id];
  if (mine) return r.updatedAt !== mine.at || r.updatedBy !== mine.by;
  return !!s.updatedAt && Date.parse(r.updatedAt) > Date.parse(s.updatedAt);
}

export type UndoPlan = { ok: true; rows: DraftChange[]; labels: string[] } | { ok: false; reason: "stale" | "error" | "scope"; error?: unknown };

/** Reads the drafts NOW and says which of this card's rows would go; stale when somebody kept editing one. */
export async function planUndo(state: ApplyState, deps: Pick<ApplyDeps, "assertScope">): Promise<UndoPlan> {
  const ids = state.changeIds ?? [];
  if (ids.length === 0) return { ok: false, reason: "stale" };
  if (deps.assertScope && !deps.assertScope()) return { ok: false, reason: "scope" };
  let list: DraftChange[];
  try {
    list = (await listDraftChanges()).changes;
  } catch (error) {
    return { ok: false, reason: "error", error };
  }
  const rows = list.filter((r) => ids.includes(r.id));
  // A row that is gone (discarded in the DraftBar, published from another tab) is not "undone by this card": say so instead of claiming success.
  if (rows.length < ids.length) return { ok: false, reason: "stale" };
  if (rows.some((r) => edited(r, state))) return { ok: false, reason: "stale" };
  return { ok: true, rows, labels: rows.map((r) => r.label) };
}

export type UndoResult = { status: "undone" } | { status: "stale" } | { status: "scope" } | { status: "error"; error: unknown };

/** Discards only the rows in `changeIds`. 404 DRAFT_NOT_FOUND = already gone. Any other failure stops at once: nothing else is touched. */
export async function undoProposal(state: ApplyState, deps: Pick<ApplyDeps, "assertScope">): Promise<UndoResult> {
  const plan = await planUndo(state, deps);
  if (!plan.ok) return plan.reason === "error" ? { status: "error", error: plan.error } : { status: plan.reason };
  for (const r of plan.rows) {
    if (deps.assertScope && !deps.assertScope()) return { status: "scope" };
    try {
      await discardDraft(r.id);
    } catch (error) {
      if (error instanceof ApiError && error.code === "DRAFT_NOT_FOUND") continue;
      return { status: "error", error };
    }
  }
  return { status: "undone" };
}

/** The head version number when a Publish went out after this card was applied; null otherwise. */
export function sealedVersion(state: ApplyState, d: Delivery): number | null {
  if (d.kind !== "delivered" || !state.changeIds?.length || typeof state.headVersionNo !== "number") return null;
  const head = d.state.headVersion?.versionNo ?? 0;
  return head > state.headVersionNo ? head : null;
}
