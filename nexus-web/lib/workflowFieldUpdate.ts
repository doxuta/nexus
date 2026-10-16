// field_update editor helpers (specs/058) — "Cập nhật trường" on the triggering record OR on the record
// one of its relation fields points at ("hoá đơn Đã thanh toán ⇒ lịch hẹn đổi trạng thái").
//
// Wire shape: { type:"field_update", via?, field, value }. `via` = a `relation` field of the rule's
// entity; `field` then names a field of the RELATED entity (options.relatedEntity).
// specs/084: { type:"field_update", childEntity, childVia, field, value } writes EVERY child record —
// the rows of `childEntity` whose relation field `childVia` points at the triggering record. `field`
// then names a field of the CHILD entity. Exclusive with `via`.
//
// The editor state lives in the action's flat string params (like every other action card):
//   via    — relation field name, "" / absent = the triggering record
//   field  — target field name
//   values — JSON [[field, widgetValue]] once the value was edited here (reuses create_record's
//            pair machinery, so the value is WIDGET-NATIVE and typed on emit — #80)
//   raw    — JSON of the value exactly as it was STORED, while nobody touched it. Opening a rule and
//            saving it back re-sends that value untouched: the old editor stringified everything
//            (`String(100)` → "100"), silently turning a number rule into a text one on re-save.
//   onlyIfEmpty / valueSource — the before_save keys (specs/048 S4), carried through so an opened rule
//            saves back as it was: dropped, the server reads a {{template}} or a field name as a
//            constant and "fill only when empty" as "overwrite on every save". The editor for them is
//            FE-before-save-hooks-3 (MutateActionEditor); until then they only travel.
// React-free so vitest exercises it directly.

import type { FieldMeta } from "@/lib/api";
import { READONLY_FIELD_TYPES, SYSTEM_FIELDS, emptyDefault } from "@/lib/recordPayload";
import { CR_SUPPORTED_TYPES, crEmitValues, crHydrateRows, crPairsToString, crValuePairs } from "@/lib/workflowCreateRecord";

/** A relation field of the rule's entity the action can follow, with the entity it points at. */
export interface RelationChoice { name: string; label: string; target: string }

/** Every `relation` field that names its target entity (polymorphic/N-N never qualify — the backend refuses them). */
export function relationChoices(entityFields: FieldMeta[]): RelationChoice[] {
  const out: RelationChoice[] = [];
  for (const f of entityFields) {
    if (f.fieldType !== "relation") continue;
    const target = typeof f.options?.relatedEntity === "string" ? f.options.relatedEntity.trim() : "";
    const card = typeof f.options?.cardinality === "string" ? f.options.cardinality : "";
    if (!target || card === "N-N" || card === "N-1-inverse") continue;
    out.push({ name: f.name, label: f.label || f.name, target });
  }
  return out;
}

/** A relation of ANOTHER entity that points back at the rule's entity — "every `entity` row whose `name` is this record". */
export interface ChildChoice { entity: string; name: string; label: string }

/** Child relations of the rule's entity found in the loaded fields of other entities (to-one only,
 *  like the backend: N-N / N-1-inverse never qualify). Sorted so the picker order is stable. */
export function childChoices(ruleEntity: string, fieldsByEntity: Record<string, FieldMeta[]>): ChildChoice[] {
  const out: ChildChoice[] = [];
  for (const [entity, fields] of Object.entries(fieldsByEntity)) {
    for (const f of fields) {
      if (f.fieldType !== "relation") continue;
      const target = typeof f.options?.relatedEntity === "string" ? f.options.relatedEntity.trim() : "";
      const card = typeof f.options?.cardinality === "string" ? f.options.cardinality : "";
      if (target !== ruleEntity || card === "N-N" || card === "N-1-inverse") continue;
      out.push({ entity, name: f.name, label: f.label || f.name });
    }
  }
  return out.sort((a, b) => a.entity.localeCompare(b.entity) || a.name.localeCompare(b.name));
}

/** The record picker's option value for a child relation (entity and field names are [a-z0-9_]). */
export const childOptionValue = (entity: string, name: string) => `child:${entity}:${name}`;

/** The entity the action writes to: the child entity for a children action, the related entity for a
 *  via, else the rule's own entity. */
export function fuTargetEntity(ruleEntity: string, entityFields: FieldMeta[], via: string | undefined, childEntity?: string): string | undefined {
  if (childEntity) return childEntity;
  if (!via) return ruleEntity;
  return relationChoices(entityFields).find((c) => c.name === via)?.target;
}

/** The fields the action's value is typed against: the RELATED entity's for a via ([] while they load
 *  or when the via is not a to-one relation), else the rule's own. One source for the editor, the
 *  payload and the save-time check, so the three can never type the value against different fields. */
export function fuTargetFields(ruleEntity: string, ruleFields: FieldMeta[], fieldsByEntity: Record<string, FieldMeta[]>, via: string | undefined, childEntity?: string): FieldMeta[] {
  if (!via && !childEntity) return ruleFields;
  const target = fuTargetEntity(ruleEntity, ruleFields, via, childEntity);
  return target ? fieldsByEntity[target] ?? [] : [];
}

/** Fields the value editor can write: not system, not computed/assigned, a type it can emit. A `user`
 *  field is writable here (FieldInput has a user picker, the value is a plain id) — the old free-text
 *  editor accepted it, so dropping it would strand every rule that sets one. */
export function fuWritableFields(fields: FieldMeta[]): FieldMeta[] {
  return fields.filter((f) => !SYSTEM_FIELDS.has(f.name) && !READONLY_FIELD_TYPES.has(f.fieldType)
    && (CR_SUPPORTED_TYPES.has(f.fieldType) || f.fieldType === "user"));
}

/** valueSource "field" (copy another field) or "template" ({{field}} sentence): the value is text, never typed. */
export const isDynamicSource = (src: string | undefined) => src === "field" || src === "template";

/** A stored field_update action → editor params (value kept verbatim in `raw`). */
export function fuParamsFromAction(a: Record<string, unknown>): Record<string, string> {
  const p: Record<string, string> = { field: typeof a.field === "string" ? a.field : "" };
  if (typeof a.via === "string" && a.via) p.via = a.via;
  if (typeof a.childEntity === "string" && a.childEntity) p.childEntity = a.childEntity;
  if (typeof a.childVia === "string" && a.childVia) p.childVia = a.childVia;
  if (a.onlyIfEmpty === true) p.onlyIfEmpty = "true";
  if (typeof a.valueSource === "string" && a.valueSource) p.valueSource = a.valueSource;
  // A copied field / a {{template}} is a plain sentence, not a typed value (specs/048 S4): it lives in srcValue.
  if (isDynamicSource(p.valueSource)) p.srcValue = typeof a.value === "string" ? a.value : "";
  else if ("value" in a && a.value !== undefined) p.raw = JSON.stringify(a.value);
  return p;
}

function parseRaw(raw: string | undefined): unknown {
  if (raw === undefined) return undefined;
  try { return JSON.parse(raw); } catch { return raw; }
}

/** The widget-native value to show: the edited one, else the stored one reversed once, else empty. */
export function fuWidgetValue(params: Record<string, string>, meta: FieldMeta | undefined): unknown {
  if (isDynamicSource(params.valueSource)) return params.srcValue ?? "";
  if (params.values !== undefined) return crValuePairs(params.values)[0]?.[1];
  const stored = parseRaw(params.raw);
  if (!meta) return stored ?? "";
  if (stored === undefined) return emptyDefault(meta);
  return crHydrateRows({ [meta.name]: stored }, [meta])[0][1];
}

/** Params after the value was edited in the widget (the stored value is dropped). */
export function fuWithValue(params: Record<string, string>, value: unknown): Record<string, string> {
  if (isDynamicSource(params.valueSource)) return { ...params, srcValue: String(value ?? "") };
  const next: Record<string, string> = { ...params, values: crPairsToString([[params.field ?? "", value]]) };
  delete next.raw;
  return next;
}

/** Params after the target record or field changed: the old value — and where it was read from — means
 *  nothing for the new field; onlyIfEmpty stays (it is about the target, whichever it is). */
export function fuRetarget(params: Record<string, string>, patch: { via?: string; childEntity?: string; childVia?: string; field?: string }, meta: FieldMeta | undefined): Record<string, string> {
  const next: Record<string, string> = { ...params, ...patch };
  // Choosing a record kind sets all three keys at once: a via clears the children pair and back.
  if (patch.via !== undefined || patch.childEntity !== undefined) {
    next.field = patch.field ?? "";
    next.via = patch.via ?? "";
    next.childEntity = patch.childEntity ?? "";
    next.childVia = patch.childVia ?? "";
  }
  if (!next.via) delete next.via;
  if (!next.childEntity) delete next.childEntity;
  if (!next.childVia) delete next.childVia;
  delete next.raw;
  delete next.values;
  delete next.valueSource;
  delete next.srcValue;
  if (meta && next.field === meta.name) next.values = crPairsToString([[meta.name, emptyDefault(meta)]]);
  return next;
}

/** Editor params → the backend action. An untouched stored value travels back exactly as stored. */
export function fuPayload(params: Record<string, string>, targetFields: FieldMeta[]): Record<string, unknown> {
  const field = params.field ?? "";
  const out: Record<string, unknown> = {
    type: "field_update",
    ...(params.via ? { via: params.via } : {}),
    ...(params.childEntity ? { childEntity: params.childEntity, childVia: params.childVia ?? "" } : {}),
    field,
  };
  if (isDynamicSource(params.valueSource)) {
    out.value = params.srcValue ?? "";
  } else if (params.values !== undefined) {
    const emitted = crEmitValues(crValuePairs(params.values), targetFields);
    if (field in emitted) out.value = emitted[field];
  } else if (params.raw !== undefined) {
    out.value = parseRaw(params.raw);
  }
  if (params.onlyIfEmpty === "true") out.onlyIfEmpty = true;
  if (params.valueSource) out.valueSource = params.valueSource;
  return out;
}

/** A field is chosen but the action would carry no value. The backend reads a missing value as null
 *  and every run would silently CLEAR the target field (on a related record the trigger's user may not
 *  even be allowed to edit), so the builder refuses to save it. A value stored as-is (`raw`, including
 *  an explicit null written through the API) is not missing: opening and saving a rule changes nothing. */
export function fuValueMissing(params: Record<string, string>, targetFields: FieldMeta[]): boolean {
  if (!params.field) return false;
  if (isDynamicSource(params.valueSource)) return (params.srcValue ?? "").trim() === "";
  if (params.values === undefined) return params.raw === undefined;
  const v = fuPayload(params, targetFields).value;
  return v === undefined || v === null || (typeof v === "string" && v.trim() === "");
}
