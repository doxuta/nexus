// lib/dependentPicklist.ts — dependent picklists on the client (specs/052-dependent-picklist).
//
// A CHILD picklist carries `options.controllingField` (a sibling single picklist of the same entity)
// and `options.dependentValues` (parent value → the child values allowed under it). The server
// judges every write (422 RECORD_DEPENDENT_PICKLIST_INVALID, details.field = the child); these
// helpers keep the record form from offering a pair it would refuse, and give the Studio inspector
// what it warns about while the pair is being configured. Pure — no IO.

import type { FieldMeta } from "@/lib/api";
import type { FieldAclGrant } from "@/lib/fieldAcl";
import { stateFromGrant } from "@/lib/fieldAcl";

export type DependentValues = Record<string, string[]>;

export interface Dependency {
  controllingField: string;
  dependentValues: DependentValues;
}

type Options = { values?: unknown; controllingField?: unknown; dependentValues?: unknown } | undefined;

/** The child's link to its controlling field, or null for a plain picklist. */
export function dependencyOf(options: Options): Dependency | null {
  const cf = options?.controllingField;
  const dv = options?.dependentValues;
  if (typeof cf !== "string" || !cf || !dv || typeof dv !== "object" || Array.isArray(dv)) return null;
  return { controllingField: cf, dependentValues: dv as DependentValues };
}

export type DependentChoice =
  /** Not dependent, or its controlling field is not on this form: the whole list. */
  | { kind: "flat" }
  /** The controlling field is empty: nothing to choose until it is. */
  | { kind: "blocked"; controllingField: string }
  /** The children the chosen parent value allows, in the child's own value order. */
  | { kind: "filtered"; values: string[] };

/**
 * What a picklist may list given its sibling values (the form state keyed by field name).
 * A controlling field absent from `siblings` — hidden from the caller's role, or left off the
 * layout — gives nothing to filter by: the list stays flat and the server judges the pair.
 */
export function dependentChoice(options: Options, siblings: Record<string, unknown> | undefined): DependentChoice {
  const dep = dependencyOf(options);
  if (!dep || !siblings || !(dep.controllingField in siblings)) return { kind: "flat" };
  const parent = siblings[dep.controllingField];
  if (parent == null || parent === "") return { kind: "blocked", controllingField: dep.controllingField };
  const allowed = new Set(dep.dependentValues[String(parent)] ?? []);
  const values = Array.isArray(options?.values) ? (options.values as string[]) : [];
  return { kind: "filtered", values: values.filter((v) => allowed.has(v)) };
}

/**
 * The children a change of `changed` blanks: every picklist on the form (present in `values`)
 * controlled by it whose value the new parent value no longer allows — and, down the chain, their
 * own children. `values` already holds the new value of `changed`. A child locked for the caller's
 * role (`readOnly`) is left as it is: it is neither editable nor sent, so blanking it on screen
 * would show a value the record will not have.
 */
/** A relation picker's link to a sibling relation (queue #304): list target records where
 *  `field` (a relation on the target entity) equals the id the form holds in `parentField`. */
export interface RelationFilter { field: string; parentField: string }

export function relationFilterOf(options: object | undefined): RelationFilter | null {
  const fb = (options as { filterBy?: unknown } | undefined)?.filterBy as { field?: unknown; parentField?: unknown } | undefined;
  if (!fb || typeof fb.field !== "string" || !fb.field || typeof fb.parentField !== "string" || !fb.parentField) return null;
  return { field: fb.field, parentField: fb.parentField };
}

export type RelationFilterChoice =
  | { kind: "flat" }
  | { kind: "blocked"; parentField: string }
  | { kind: "filtered"; field: string; value: string };

/** What a relation picker may list given its sibling values; a parent absent from the form leaves
 *  it flat (hidden from the role / off the layout — nothing to filter by). */
export function relationFilterChoice(options: object | undefined, siblings: Record<string, unknown> | undefined): RelationFilterChoice {
  const rf = relationFilterOf(options);
  if (!rf || !siblings || !(rf.parentField in siblings)) return { kind: "flat" };
  const v = siblings[rf.parentField];
  const id = v && typeof v === "object" ? (v as { id?: unknown }).id : v;
  if (id == null || id === "") return { kind: "blocked", parentField: rf.parentField };
  return { kind: "filtered", field: rf.field, value: String(id) };
}

export function clearedDependents(
  fieldsByName: Record<string, FieldMeta>,
  values: Record<string, unknown>,
  changed: string,
): Record<string, string> {
  const out: Record<string, string> = {};
  const next = { ...values };
  const queue = [changed];
  while (queue.length > 0) {
    const parent = queue.shift()!;
    for (const f of Object.values(fieldsByName)) {
      if (f.readOnly || !(f.name in next)) continue;
      const cur = next[f.name];
      if (cur == null || cur === "") continue;
      // queue #304: a relation picker filtered by `parent` forgets its choice (it may not belong
      // to the new parent; the picker re-lists under it).
      if (f.fieldType === "relation") {
        if (relationFilterOf(f.options)?.parentField !== parent) continue;
        out[f.name] = "";
        next[f.name] = "";
        queue.push(f.name);
        continue;
      }
      if (f.fieldType !== "picklist" || dependencyOf(f.options)?.controllingField !== parent) continue;
      const choice = dependentChoice(f.options, next);
      if (choice.kind === "filtered" && choice.values.includes(String(cur))) continue;
      out[f.name] = "";
      next[f.name] = "";
      queue.push(f.name);
    }
  }
  return out;
}

/** The map with every child value the child no longer lists removed (else 422 FR-2 on save). */
export function pruneDependentValues(map: DependentValues, childValues: string[]): DependentValues {
  const keep = new Set(childValues);
  return Object.fromEntries(Object.entries(map).map(([k, vs]) => [k, vs.filter((v) => keep.has(v))]));
}

/**
 * Fields that may control `child`: other single picklists, minus any whose own controlling chain
 * already reaches `child` (choosing one would close a cycle — the server refuses it anyway).
 */
export function controllingCandidates(fields: FieldMeta[], child: string): FieldMeta[] {
  const byName = new Map(fields.map((f) => [f.name, f]));
  const reaches = (name: string) => {
    const seen = new Set<string>();
    for (let cur: string | undefined = name; cur && !seen.has(cur); cur = dependencyOf(byName.get(cur)?.options)?.controllingField) {
      if (cur === child) return true;
      seen.add(cur);
    }
    return false;
  };
  return fields.filter((f) => f.fieldType === "picklist" && !reaches(f.name));
}

export interface DependencyWarnings {
  /** Child values no live parent value allows — nobody can ever pick them. */
  orphanChildren: string[];
  /** Map keys the controlling field no longer lists (tolerated by the server, dead weight). */
  orphanKeys: string[];
}

export function dependencyWarnings(p: {
  childValues: string[];
  dependentValues: DependentValues;
  parentValues: string[];
}): DependencyWarnings {
  const live = new Set(p.parentValues);
  const reachable = new Set(
    Object.entries(p.dependentValues).flatMap(([k, vs]) => (live.has(k) ? vs : [])),
  );
  return {
    orphanChildren: p.childValues.filter((v) => !reachable.has(v)),
    orphanKeys: Object.keys(p.dependentValues).filter((k) => !live.has(k)),
  };
}

/**
 * Roles for which the parent and the child sit at different field-ACL levels (default / read-only /
 * hidden). Such a role can be stuck (parent editable, child not: a parent change leaves a child it
 * cannot fix) or can probe a hidden parent through the child's 422s — specs/052 §6.
 */
export function visibilityMismatch(grants: FieldAclGrant[], parent: string, child: string): string[] {
  const state = new Map<string, ReturnType<typeof stateFromGrant>>();
  const roles: string[] = [];
  for (const g of grants) {
    if (g.fieldName !== parent && g.fieldName !== child) continue;
    state.set(`${g.roleId}::${g.fieldName}`, stateFromGrant(g.canView, g.canEdit));
    if (!roles.includes(g.roleId)) roles.push(g.roleId);
  }
  const of = (role: string, field: string) => state.get(`${role}::${field}`) ?? "default";
  return roles.filter((r) => of(r, parent) !== of(r, child));
}

export interface CsvImport {
  /** parent code -> child codes (duplicates merged, first-seen order). */
  map: DependentValues;
  /** Every distinct child code, first-seen order. */
  children: string[];
  /** One entry per rejected line (1-based); the caller applies nothing while this is non-empty. */
  errors: { line: number; kind: "format" | "parent"; text: string }[];
}

/** Parse the "parent,child" CSV of the mapping dialog (queue #254). The parent may be its code or
 *  its display label; the split is on the FIRST comma/tab so a quoted child may contain commas. */
export function parseDependentCsv(text: string, parentValues: string[], parentLabel: (v: string) => string): CsvImport {
  const byKey = new Map<string, string>();
  for (const v of parentValues) { byKey.set(v, v); byKey.set(parentLabel(v), v); }
  const unq = (s: string) => s.trim().replace(/^"(.*)"$/, "$1").trim();
  const map: DependentValues = {};
  const seenChild = new Set<string>();
  const children: string[] = [];
  const errors: CsvImport["errors"] = [];
  text.split(/\r?\n/).forEach((raw, i) => {
    if (!raw.trim()) return;
    const cut = raw.search(/[,\t]/);
    const p = cut < 0 ? "" : unq(raw.slice(0, cut));
    const c = cut < 0 ? "" : unq(raw.slice(cut + 1));
    if (!p || !c) { errors.push({ line: i + 1, kind: "format", text: raw.trim() }); return; }
    const code = byKey.get(p);
    if (code === undefined) { errors.push({ line: i + 1, kind: "parent", text: p }); return; }
    const list = (map[code] ??= []);
    if (!list.includes(c)) list.push(c);
    if (!seenChild.has(c)) { seenChild.add(c); children.push(c); }
  });
  return { map, children, errors };
}
