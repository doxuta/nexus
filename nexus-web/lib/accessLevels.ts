// lib/accessLevels.ts — the three-tier view of a role's grants on ONE table (specs/074 FR-031, lát f2-access-matrix).
// Pure, and it works on the RAW RoleGrant[] of getRole: lib/admin's grantsToMatrix/matrixToGrants drop every "none"
// cell, and PUT /admin/roles/:id replaces the whole set, so going through them would silently lose an explicit
// "none" on another table (= "blocked by name" turns into "follows *" = a quiet privilege raise).
// Effective grant of one action on a table follows ACLService.Authorize: exact table > "*" > nothing.
import type { RoleAccessLevel, RoleActionVerb, RoleGrant } from "@/lib/api";

/** Not named AccessLevel: lib/admin already uses that for the row scope (none | owner | team | all). */
export type MatrixTier = "none" | "view" | "edit" | "custom";

export interface TableLevel {
  tier: MatrixTier;
  /** At least one of view/create/edit comes from the "every table" (*) row: not quick-editable. */
  inherited: boolean;
}

const WRITES: RoleActionVerb[] = ["view", "create", "edit"];
const find = (grants: readonly RoleGrant[], entity: string, action: RoleActionVerb) =>
  grants.find((x) => x.entityName === entity && x.action === action);

export function levelOf(grants: readonly RoleGrant[], entity: string): TableLevel {
  let inherited = false;
  const on = [...WRITES, "delete" as const].map((action) => {
    const exact = find(grants, entity, action);
    if (exact) return exact.accessLevel !== "none";
    const star = entity === "*" ? undefined : find(grants, "*", action);
    if (star && star.accessLevel !== "none") inherited = true;
    return !!star && star.accessLevel !== "none";
  });
  const [v, c, e, d] = on;
  // delete is independent of view on the server (DELETE/restore only check the "delete" action), so a role that can
  // still delete must never read "Không thấy": it is "custom" and the role page shows the real grants.
  const tier: MatrixTier = !v && !c && !e ? (d ? "custom" : "none") : v && !c && !e ? "view" : v && c && e ? "edit" : "custom";
  return { tier, inherited };
}

/**
 * Set a table to a tier: only the view/create/edit grants of THIS table change; delete (except at "none"), scopes already set and every
 * other table's grants (explicit "none" included) leave exactly as they came. Raising from nothing uses scope "all";
 * create/edit take the view scope. Lowering drops the grants ("none" also this table's delete), except an explicit "none" that already blocks the table
 * (dropping it would hand the table back to "*"). `custom` is not a target.
 */
export function applyLevel(grants: readonly RoleGrant[], entity: string, tier: MatrixTier): RoleGrant[] {
  if (tier === "custom") return [...grants];
  const want: Record<RoleActionVerb, boolean> = {
    view: tier !== "none",
    create: tier === "edit",
    edit: tier === "edit",
    delete: false,
  };
  const out: RoleGrant[] = [];
  const viewScope = (): RoleAccessLevel => {
    const v = find(grants, entity, "view");
    return v && v.accessLevel !== "none" ? v.accessLevel : "all";
  };
  for (const x of grants) {
    // "Không thấy" also takes this table's delete away, otherwise the role keeps a way to remove records.
    const mine = x.entityName === entity && (WRITES.includes(x.action) || (tier === "none" && x.action === "delete"));
    if (!mine) out.push(x);
    else if (x.accessLevel === "none" && !want[x.action]) out.push(x); // keep the explicit block
    else if (want[x.action] && x.accessLevel !== "none") out.push(x); // keep the scope already there
    // else: lowered, or an explicit none that is being replaced below
  }
  for (const action of WRITES) {
    if (want[action] && !out.some((x) => x.entityName === entity && x.action === action)) {
      out.push({ entityName: entity, action, accessLevel: viewScope() });
    }
  }
  return out;
}
