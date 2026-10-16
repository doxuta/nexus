// "Lấy từ hồ sơ kích hoạt" (queue #305/#333): a rule value taken from the record that fired the rule
// instead of typed in. Wire form is the engine's own — "$record.<field>" (typed copy) and "$record" (the
// record's id, for a relation that points back at it) — the same strings create_record and field_update
// resolve at fire time (workflow.ResolveFieldUpdateValue / resolveCreateValues). React-free for vitest.

import type { FieldMeta } from "@/lib/api";
import { SYSTEM_FIELDS } from "@/lib/recordPayload";

export const RECORD_SENTINEL = "$record";
export const RECORD_FIELD_PREFIX = "$record.";

export interface SourceOption { value: string; label: string }

function relatedEntity(f: FieldMeta): string {
  return typeof f.options?.relatedEntity === "string" ? f.options.relatedEntity : "";
}

/** What `target` may be filled from on the triggering record. The engine refuses a copy whose type differs
 *  from the target's (RECORD_INVALID_TYPE at save), so only same-type fields are offered — and a relation
 *  only from a relation to the same entity. `$record` (the record itself) goes to a relation that points
 *  back at the rule's own entity. Empty ⇒ nothing fits and the picker is not shown. */
export function triggerSourceOptions(target: FieldMeta, ruleFields: FieldMeta[], ruleEntity: string): SourceOption[] {
  const out: SourceOption[] = [];
  const targetRel = target.fieldType === "relation" ? relatedEntity(target) : "";
  for (const f of ruleFields) {
    if (SYSTEM_FIELDS.has(f.name) || f.fieldType !== target.fieldType) continue;
    if (targetRel && relatedEntity(f) !== targetRel) continue;
    out.push({ value: RECORD_FIELD_PREFIX + f.name, label: f.label || f.name });
  }
  if (targetRel && targetRel === ruleEntity) out.push({ value: RECORD_SENTINEL, label: "" });
  return out;
}

/** The source field of a "$record.<field>" value; undefined for a typed value or "$record". */
export function sourceFieldName(v: unknown): string | undefined {
  return typeof v === "string" && v.startsWith(RECORD_FIELD_PREFIX) ? v.slice(RECORD_FIELD_PREFIX.length) : undefined;
}

/** A value that is read from the triggering record (either form). */
export function isFromTrigger(v: unknown): boolean {
  return typeof v === "string" && (v === RECORD_SENTINEL || v.startsWith(RECORD_FIELD_PREFIX));
}

const FREE_TEXT = new Set(["text", "email", "rich_text"]);

/** isFromTrigger for a row whose target may be free text: there "$record.<f>" is also what a person types
 *  letter by letter, so it only counts once it names a real field of the rule's record (else the input
 *  would vanish mid-typing). Other targets have no typed form that starts with "$record". */
export function isFromTriggerFor(v: unknown, target: FieldMeta, ruleFields: FieldMeta[]): boolean {
  if (!isFromTrigger(v)) return false;
  if (!FREE_TEXT.has(target.fieldType) || v === RECORD_SENTINEL) return true;
  const src = sourceFieldName(v);
  return ruleFields.some((f) => f.name === src);
}
