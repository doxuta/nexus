// lib/workflowTemplateVars.ts — the fields a workflow email can name (specs/059, DAILY-QUEUE #271).
//
// The engine renders three placeholder shapes in an email_alert's To/Subject/Body:
//   {{field}}                 a field of the record the rule runs on
//   {{relation.field}}        a field of the record a relation points at (the patient of an appointment)
//   {{lookup}}                a lookup field, as the record page shows it
//   {{user.email}} / .name    the person a user field points at (DAILY-QUEUE #336)
// Before this the builder typed machine names into free text; "Gửi tới" is now picked. Pure — the
// picker component loads the related entities' fields and hands them in.
import type { FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";

export interface TemplateVar {
  token: string;
  label: string;
}

export interface TemplateVarGroup {
  key: string;
  label: string;
  vars: TemplateVar[];
}

// Values a mail never prints usefully — never offered. relation/user render a raw id (noise); rollup is
// computed at READ time and is not in the data the engine renders from; currency, multi_select,
// composite, polymorphic_relation and file/image are stored as objects/arrays that workflow.toString
// renders as "". Same list as the save-time check services.templateRendersEmpty (which refuses the
// empty ones in {{a.b}}), plus a lookup ON the linked record (one hop, filtered below).
const NOT_RENDERABLE = new Set(["relation", "user", "rollup", "multi_select", "currency", "composite", "file", "image", "polymorphic_relation"]);

const renders = (f: FieldMeta) =>
  !NOT_RENDERABLE.has(f.fieldType) && !(f.fieldType === "formula" && f.options?.resultType === "currency");

// An address: the Email type, or a text field named or labelled "email" — every built-in pack (CRM,
// LMS, Ticketing) declares its address as text + unique, and a field's type cannot change afterwards
// (FIELD_TYPE_IMMUTABLE), so the Email type alone would hide the address those apps already have.
const EMAILISH = /e-?mail/i;
const isAddress = (f: FieldMeta) =>
  f.fieldType === "email" || (f.fieldType === "text" && (EMAILISH.test(f.name) || EMAILISH.test(f.label ?? "")));

const fieldLabel = (f: FieldMeta) => f.label || f.name;

/** The entity a to-one relation field points at ("" for anything else, polymorphic included). */
export function relatedEntityOf(f: FieldMeta): string {
  const target = f.fieldType === "relation" ? f.options?.relatedEntity : undefined;
  return typeof target === "string" ? target.trim() : "";
}

/** Distinct entities the rule's relation fields point at — the ones whose fields the picker loads. */
export function relationTargets(fields: FieldMeta[]): string[] {
  return [...new Set(fields.map(relatedEntityOf).filter(Boolean))];
}

/**
 * The placeholder groups for a picker: the record's own fields, then one group per relation.
 * emailOnly narrows every group to addresses — isAddress fields, and lookups that mirror one — which
 * is what "Gửi tới" needs. related maps an entity name to its fields (missing = not loaded yet).
 */
export function templateVarGroups(fields: FieldMeta[], related: Record<string, FieldMeta[]>, emailOnly: boolean): TemplateVarGroup[] {
  // A lookup prints the field it mirrors (undefined until that entity's fields are loaded).
  const sourceOf = (f: FieldMeta) => {
    const via = fields.find((x) => x.name === f.options?.via);
    return via ? related[relatedEntityOf(via)]?.find((x) => x.name === f.options?.sourceField) : undefined;
  };
  const offer = (f: FieldMeta): boolean => {
    if (f.fieldType !== "lookup") return emailOnly ? isAddress(f) : renders(f);
    const source = sourceOf(f);
    return emailOnly ? !!source && isAddress(source) : !source || renders(source);
  };

  const groups: TemplateVarGroup[] = [{
    key: "",
    label: t("page.workflow.tpl.ownGroup"),
    vars: fields.filter(offer).map((f) => ({ token: `{{${f.name}}}`, label: fieldLabel(f) })),
  }];
  for (const rel of fields) {
    // A user field points at a row of the system users table: the engine reads only its email and
    // name (services.userTemplateFields), so those two are what is offered.
    if (rel.fieldType === "user") {
      const userVars = [{ f: "email", label: t("page.workflow.tpl.userEmail") }, ...(emailOnly ? [] : [{ f: "name", label: t("page.workflow.tpl.userName") }])];
      groups.push({
        key: rel.name,
        label: fieldLabel(rel),
        vars: userVars.map((v) => ({ token: `{{${rel.name}.${v.f}}}`, label: `${fieldLabel(rel)} › ${v.label}` })),
      });
      continue;
    }
    const target = relatedEntityOf(rel);
    const targetFields = target ? related[target] : undefined;
    if (!targetFields) continue;
    groups.push({
      key: rel.name,
      label: fieldLabel(rel),
      // One hop only, like the engine: a lookup on the linked record is not followed.
      vars: targetFields
        .filter((f) => f.fieldType !== "lookup" && offer(f))
        .map((f) => ({ token: `{{${rel.name}.${f.name}}}`, label: `${fieldLabel(rel)} › ${fieldLabel(f)}` })),
    });
  }
  return groups.filter((g) => g.vars.length > 0);
}

/** Subject/body: a picked placeholder goes at the end, one space from what is already there. */
export function appendToken(current: string, token: string): string {
  if (!current) return token;
  return /\s$/.test(current) ? current + token : `${current} ${token}`;
}
