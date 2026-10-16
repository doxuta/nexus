// lib/fieldAcl.ts — pure matrix state model for the Field-ACL editor (W7, api-conventions §11.1).
// NO IO here — the tri-state semantics + upsert-by-key payload builder live in pure functions so
// they can be unit-tested without a backend. Network calls live in lib/admin.ts (getFieldAcl…).
//
// Mental model (one with backend `loadRestrictions`):
//   • default  (visible + editable)      → NO row stored. Absent key = default.
//   • readonly (visible, write → 403)    → row { canView:true,  canEdit:false }
//   • hidden   (stripped from all reads) → row { canView:false, canEdit:false }
// Grant (true,true) is never persisted — sending it on PUT DELETES the row (clears the exception).

import type { FieldMeta } from "@/lib/api";
import type { FieldType } from "@/lib/metadata";
import { supportsDefaultValue } from "@/lib/fieldOptions";
import { READONLY_FIELD_TYPES } from "@/lib/recordPayload";
import { t, tf } from "@/lib/i18n";

export type CellState = "default" | "readonly" | "hidden";

export interface FieldAclGrant {
  roleId: string;
  entityName: string;
  fieldName: string;
  canView: boolean;
  canEdit: boolean;
}

/** Sparse matrix: only NON-default cells are stored. Absent key ⇒ "default". */
export type AclMatrix = Record<string, CellState>;

// Composite key: roleId (uuid, no "::") + fieldName (identifier, no "::") — safe to split on "::".
export const cellKey = (roleId: string, fieldName: string): string => `${roleId}::${fieldName}`;
export function splitCellKey(key: string): { roleId: string; fieldName: string } {
  const i = key.indexOf("::");
  return { roleId: key.slice(0, i), fieldName: key.slice(i + 2) };
}

/** Map a stored grant's (canView,canEdit) to a tri-state. */
export function stateFromGrant(canView: boolean, canEdit: boolean): CellState {
  if (!canView) return "hidden"; // (false,*) — backend normalizes canEdit=false when hidden
  if (!canEdit) return "readonly"; // (true,false)
  return "default"; // (true,true)
}

/** Map a tri-state back to (canView,canEdit). "default" ⇒ (true,true) which the backend clears. */
export function grantBoolsFromState(state: CellState): { canView: boolean; canEdit: boolean } {
  switch (state) {
    case "hidden":
      return { canView: false, canEdit: false };
    case "readonly":
      return { canView: true, canEdit: false };
    default:
      return { canView: true, canEdit: true };
  }
}

// Click a cell to cycle through the three states in this order.
export const CELL_CYCLE: CellState[] = ["default", "readonly", "hidden"];
export function cycleCellState(s: CellState): CellState {
  const i = CELL_CYCLE.indexOf(s);
  return CELL_CYCLE[(i + 1) % CELL_CYCLE.length];
}

/** Build the sparse matrix from GET grants (GET returns only cells with a live effect). */
export function buildMatrix(grants: FieldAclGrant[]): AclMatrix {
  const m: AclMatrix = {};
  for (const g of grants) {
    const s = stateFromGrant(g.canView, g.canEdit);
    if (s !== "default") m[cellKey(g.roleId, g.fieldName)] = s;
  }
  return m;
}

export function getCellState(m: AclMatrix, roleId: string, fieldName: string): CellState {
  return m[cellKey(roleId, fieldName)] ?? "default";
}

/** Set (or clear when "default") a cell in a COPY of the matrix. */
export function setCellState(
  m: AclMatrix,
  roleId: string,
  fieldName: string,
  state: CellState,
): AclMatrix {
  const next = { ...m };
  const k = cellKey(roleId, fieldName);
  if (state === "default") delete next[k];
  else next[k] = state;
  return next;
}

/** Keys whose effective state differs between draft and saved — the cells that must be PUT. */
export function changedKeys(draft: AclMatrix, saved: AclMatrix): string[] {
  const keys = new Set([...Object.keys(draft), ...Object.keys(saved)]);
  const out: string[] = [];
  for (const k of keys) {
    if ((draft[k] ?? "default") !== (saved[k] ?? "default")) out.push(k);
  }
  return out.sort();
}

export function isDirty(draft: AclMatrix, saved: AclMatrix): boolean {
  return changedKeys(draft, saved).length > 0;
}

/**
 * Build the upsert-by-key PUT payload from the diff (§11.1 warning). EVERY changed cell is
 * emitted EXPLICITLY — including a cell toggled BACK to default, which sends (true,true) so the
 * backend deletes its row. Unchanged cells are absent ⇒ the backend keeps their stored value.
 * The array is ordered by changedKeys() so a 422 `details.index` maps to payload[index].
 */
export function buildPutPayload(
  draft: AclMatrix,
  saved: AclMatrix,
  entityName: string,
): FieldAclGrant[] {
  return changedKeys(draft, saved).map((k) => {
    const { roleId, fieldName } = splitCellKey(k);
    const state = draft[k] ?? "default";
    const { canView, canEdit } = grantBoolsFromState(state);
    return { roleId, entityName, fieldName, canView, canEdit };
  });
}

/** Count restricted (non-default) cells per field — drives the "N ẩn/khoá" row summary. */
export function restrictionCountForField(m: AclMatrix, fieldName: string): number {
  return Object.keys(m).filter((k) => splitCellKey(k).fieldName === fieldName).length;
}

// ── Dead-end locks (queue #146) ─────────────────────────────────────────────────────────────────
// A lock (read-only OR hidden — both are can_edit=0) on a field that is required, written by
// clients and NOT filled by the engine at create leaves that role unable to create any record:
// the create guard refuses the locked key, leaving it out is RECORD_REQUIRED_MISSING, and the
// server says RECORD_REQUIRED_LOCKED. PutGrants refuses to WRITE such a lock, but rows saved
// before that guard (or a field made required after the lock) still exist. These helpers let the
// matrix and the create forms say so before anyone types.

// The field types whose stored default the record engine WRITES into a create that omits the field
// — the fill branches of services.defaultValue (record_service.go; every `case` but "formula").
// Every other type drops a stored default (legacy currency/user/composite defaults included).
// Pinned against the Go source by lib/fieldAcl.test.ts.
export const DEFAULT_FILL_TYPES = new Set([
  "number", "rating", "boolean", "text", "email", "picklist", "date", "datetime", "relation", "rich_text", "multi_select",
]);

/**
 * Mirror of services.requiredEngineCannotFill: required, not server-assigned (formula/lookup/
 * rollup/auto_number), and no default the engine materializes at create.
 */
// ponytail: a stored default the engine fails to coerce (e.g. a picklist value since removed from
// the list) counts as "fills" here, while Create 422s on it. The admin matrix no longer uses this:
// it reads the server's own answer (GET /admin/field-acl `deadEndIfLocked`, queue #306). What still
// uses the mirror is the create form's up-front notice for non-admin callers (lockedRequiredNotice),
// which has no admin endpoint to ask — and Studio re-validates a default on every save, so only
// API/pack-written rows can drift. Upgrade path: a per-field flag on GET /metadata/fields.
export function requiredEngineCannotFill(f: Pick<FieldMeta, "isRequired" | "fieldType" | "defaultValue">): boolean {
  if (!f.isRequired || READONLY_FIELD_TYPES.has(f.fieldType)) return false;
  return !(DEFAULT_FILL_TYPES.has(f.fieldType) && (f.defaultValue ?? "").trim() !== "");
}

/**
 * Mirror of services.studioCanSetDefault (queue #145): "đặt giá trị mặc định" is advice an admin
 * can follow only for types the field builder has a default input for — relation has none (the
 * engine accepts a record id, but only through the API).
 */
export const studioCanSetDefault = (fieldType: string): boolean => supportsDefaultValue(fieldType as FieldType);

/** The caller's fields (GET /metadata/fields — `readOnly` is per caller) that strand a create. */
export function lockedDeadEnds<F extends Pick<FieldMeta, "isRequired" | "fieldType" | "defaultValue" | "readOnly">>(fields: F[]): F[] {
  return fields.filter((f) => f.readOnly && requiredEngineCannotFill(f));
}

/**
 * The create forms' up-front warning (LayoutForm create, "Tạo nhiều dòng"), or null when no field
 * the caller may see strands a create. Hidden fields are not served to the caller at all, so they
 * cannot be named here (anti-oracle) — the server's answer on save stays the only signal for them.
 */
export function lockedRequiredNotice(fields: Pick<FieldMeta, "name" | "label" | "isRequired" | "fieldType" | "defaultValue" | "readOnly">[]): string | null {
  const dead = lockedDeadEnds(fields);
  if (dead.length === 0) return null;
  // Advise a default only when EVERY stranded field can take one from Studio (same rule as the
  // server's lockedRequiredRemedy — never send the admin into a second 422).
  const remedy = dead.every((f) => studioCanSetDefault(f.fieldType))
    ? "page.recordForm.lockedRequired.remedyDefault"
    : "page.recordForm.lockedRequired.remedyNoDefault";
  return tf("page.recordForm.lockedRequired", {
    fields: dead.map((f) => `“${f.label || f.name}”`).join(", "),
    remedy: t(remedy),
  });
}

/** The roles (in `roleIds` order) whose cell locks `fieldName` — read-only or hidden. */
export function deadEndRoleIds(m: AclMatrix, fieldName: string, roleIds: string[]): string[] {
  return roleIds.filter((r) => getCellState(m, r, fieldName) !== "default");
}
