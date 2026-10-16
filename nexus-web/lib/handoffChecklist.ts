// lib/handoffChecklist.ts — the rows of "Giao cho khách" (specs/077 f5-handoff-checklist). Pure: settled data in,
// rows out. Rows carry i18n keys and route ids, never text or URLs. "error" is never turned into "ok".
import type { AdminUser, AppReadiness, RoleSummary } from "@/lib/api";
import { nextStep, STEP_ROUTE, type BuildStepKey, type BuildStepState } from "@/lib/buildSteps";
import type { I18nKey } from "@/lib/i18n/vi";
import type { RouteId } from "@/lib/studioRoutes";

// ponytail: đổi hằng này nếu Founder muốn Báo cáo/Thương hiệu chỉ nhắc, và đổi một dòng ở kiến trúc UI 6.3
export const BLOCKING_STEP_KEYS: readonly BuildStepKey[] = ["data", "access", "reports", "branding"];

export type HandoffAction =
  | { kind: "link"; labelKey: I18nKey; labelParams?: Record<string, string>; routeId: RouteId; params?: Record<string, string>; query?: Record<string, string> }
  | { kind: "retry"; labelKey: I18nKey; source: string }
  | { kind: "wipe"; labelKey: I18nKey };

export interface HandoffRow {
  id: string;
  tone: "ok" | "todo" | "unknown";
  blocking: boolean;
  msg: { key: I18nKey; params?: Record<string, string | number> };
  note?: { key: I18nKey };
  /** samples row only: every remaining sample row is one the last wipe kept (the builder may tick "I know") */
  keptHeld?: boolean;
  action?: HandoffAction;
  /** owner rows only: the account the row is about (OwnerAccountRow renders it) */
  owner?: AdminUser;
}

export interface HandoffAppInput {
  id: string;
  name: string;
  label: string;
  /** undefined / "error" = the detail call failed or has not answered */
  readiness?: AppReadiness | "error";
}

export interface HandoffInput {
  /** null = no project row to derive from (nothing known); "error" = the source exists but failed */
  steps: BuildStepState[] | null | "error";
  apps: HandoffAppInput[] | "error";
  /** `kept` = rows the last wipe left alone (page memory); undefined = no wipe happened on this page */
  samples: { total: number; kept?: number } | "error";
  /** the builder ticked "keep these sample rows"; only lowers the row when ALL remaining samples are kept ones */
  keptSampleAck?: boolean;
  /** machine name → label, for the "table X has no field" sentence */
  entityLabels: Record<string, string>;
  /** undefined = no source (no owner row); "error" = the users/roles read failed */
  owner?: { users: AdminUser[]; roles: RoleSummary[] } | "error";
}

/** Customer owners that can actually log in and manage users: recognised by canManageUsers, never by role name. */
export const usableOwners = (users: AdminUser[]) => users.filter((u) => u.canManageUsers === true && u.status === "active");

const K = (tail: string) => `studio.build.handover.${tail}` as I18nKey;
const NON_BLOCKING = { key: K("row.nonBlocking") };
const retry = (source: string): HandoffAction => ({ kind: "retry", labelKey: K("action.retry"), source });

function make(r: Omit<HandoffRow, "note">): HandoffRow {
  return r.tone !== "ok" && !r.blocking ? { ...r, note: NON_BLOCKING } : r;
}

function stepsRow(steps: HandoffInput["steps"]): HandoffRow {
  if (steps === null) return make({ id: "steps", tone: "unknown", blocking: false, msg: { key: K("row.steps.unknown") } });
  if (steps === "error") return make({ id: "steps", tone: "unknown", blocking: true, msg: { key: K("row.steps.unknown") }, action: retry("steps") });
  const missing = steps.filter((s) => BLOCKING_STEP_KEYS.includes(s.key) && s.complete === false);
  if (missing.length === 0) return make({ id: "steps", tone: "ok", blocking: false, msg: { key: K("row.steps.ok") } });
  // Open the first missing blocking step; nextStep() could answer Chạy thử, which must not be pointed at.
  const first = nextStep(missing.map((s) => ({ ...s, optional: false }))) ?? missing[0].key;
  return make({
    id: "steps",
    tone: "todo",
    blocking: true,
    msg: { key: K("row.steps.todo"), params: { n: missing.length, steps: missing.map((s) => s.key).join(",") } },
    action: { kind: "link", labelKey: K("action.openStep"), labelParams: { step: first }, routeId: STEP_ROUTE[first] },
  });
}

function appRow(a: HandoffAppInput, labels: Record<string, string>): HandoffRow {
  const id = `app:${a.id}`;
  const app = a.label || a.name;
  const r = a.readiness;
  if (!r || r === "error") {
    return make({ id, tone: "unknown", blocking: true, msg: { key: K("row.app.unknown"), params: { app } }, action: retry(id) });
  }
  if (r.ready) return make({ id, tone: "ok", blocking: false, msg: { key: K("row.app.ok"), params: { app } } });
  const missing = r.entitiesMissingFields ?? [];
  if (!r.allEntitiesHaveFields || missing.length > 0) {
    return make({
      id, tone: "todo", blocking: true,
      msg: { key: K("row.app.todo.fields"), params: { app, entities: missing.map((n) => labels[n] || n).join(", ") } },
      action: missing[0]
        ? { kind: "link", labelKey: K("action.addField"), routeId: "dung/du-lieu/:bang", params: { bang: missing[0] }, query: { app: a.name } }
        : { kind: "link", labelKey: K("action.addField"), routeId: "dung/du-lieu", query: { app: a.name } },
    });
  }
  if (!r.hasNonAdminViewRole) {
    return make({ id, tone: "todo", blocking: true, msg: { key: K("row.app.todo.viewRole"), params: { app } }, action: { kind: "link", labelKey: K("action.grantView"), routeId: "dung/nguoi-dung-quyen" } });
  }
  return make({ id, tone: "todo", blocking: true, msg: { key: K("row.app.todo.noStaff"), params: { app } }, action: { kind: "link", labelKey: K("action.addStaff"), routeId: "van-hanh/nguoi-dung" } });
}

// The owner row never blocks. A failed read or a server that does not send canManageUsers is "unknown",
// never "no owner" (that sentence would be a claim we could not check).
function ownerRows(owner: HandoffInput["owner"]): HandoffRow[] {
  if (owner === undefined) return [];
  const unknown = make({ id: "owner", tone: "unknown", blocking: false, msg: { key: K("row.owner.unknown") }, action: retry("owner") });
  if (owner === "error") return [unknown];
  if (!owner.users.some((u) => typeof u.canManageUsers === "boolean")) return [unknown];
  const usable = usableOwners(owner.users);
  if (usable.length === 0) return [make({ id: "owner", tone: "todo", blocking: false, msg: { key: K("row.owner.none") } })];
  return usable.map((u): HandoffRow => {
    const role = owner.roles.find((r) => r.id === u.roleId);
    return { id: `owner:${u.id}`, tone: "ok", blocking: false, owner: u, msg: role ? { key: K("row.owner.ok"), params: { email: u.email, role: role.label || role.name } } : { key: K("row.owner.noRole"), params: { email: u.email } } };
  });
}

// Sample data blocks the handover (specs/077 f5-handoff-sample-gate). Unreadable = blocking unknown, never "no samples".
function samplesRow(samples: HandoffInput["samples"], ack: boolean): HandoffRow {
  if (samples === "error") return make({ id: "samples", tone: "unknown", blocking: true, msg: { key: K("row.samples.unknown") }, action: retry("samples") });
  if (samples.total === 0) return make({ id: "samples", tone: "ok", blocking: false, msg: { key: K("row.samples.ok") } });
  const keptHeld = (samples.kept ?? 0) > 0 && (samples.kept ?? 0) >= samples.total;
  return make({
    id: "samples", tone: "todo", blocking: !(keptHeld && ack), keptHeld,
    msg: { key: K(keptHeld ? "row.samples.kept" : "row.samples.todo"), params: { n: samples.total } },
    action: { kind: "wipe", labelKey: K("action.wipeNow") },
  });
}

export function buildHandoffRows(input: HandoffInput): HandoffRow[] {
  const rows: HandoffRow[] = [stepsRow(input.steps)];
  if (input.apps === "error") rows.push(make({ id: "apps", tone: "unknown", blocking: true, msg: { key: K("row.apps.unknown") }, action: retry("apps") }));
  else if (input.apps.length === 0) {
    rows.push(make({ id: "apps", tone: "todo", blocking: true, msg: { key: K("row.apps.none") }, action: { kind: "link", labelKey: K("action.openApps"), routeId: "dung/man-hinh/app" } }));
  } else rows.push(...input.apps.map((a) => appRow(a, input.entityLabels)));
  rows.push(...ownerRows(input.owner));
  rows.push(samplesRow(input.samples, input.keptSampleAck === true));
  return rows;
}

export const blockingCount = (rows: HandoffRow[]) => rows.filter((r) => r.blocking).length;
export const canDeliver = (rows: HandoffRow[], opts: { keptSampleAck?: boolean } = {}) =>
  rows.every((r) => !r.blocking || (r.id === "samples" && r.keptHeld === true && opts.keptSampleAck === true));
