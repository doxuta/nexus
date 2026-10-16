// lib/ruleSentence.ts — a stored rule read as three plain sentences: Khi (when) / Nếu (if) / Thì (then).
// Pure and total: every trigger and action key the engine accepts has a template, and an unknown key
// reads as a neutral sentence — never an empty string and never a raw code (specs/074 FR-021, lát f2-rule-list).
// Every template goes through the dictionary (studio.build.automation.list.*).
import type { EntityMeta, FieldMeta, WorkflowDetail } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { isNullOp, OP_MAP } from "@/lib/workflowConds";
import { timerFromCondition, timerPreview, TIMER_CREATED_AT, TIMER_UNITS, TIMER_UPDATED_AT } from "@/lib/workflowTimer";

export interface RuleSentence { when: string; cond: string; then: string[] }

const K = "studio.build.automation.list.";
const key = (suffix: string) => (K + suffix) as I18nKey;
const KNOWN_WHEN: readonly string[] = ["after_create", "after_update", "after_delete", "scheduled", "date_offset", "before_save", "before_delete"];
const KNOWN_THEN: readonly string[] = ["field_update", "in_app_notification", "email_alert", "outbound_webhook", "assign", "create_record", "set_deadline", "set_elapsed", "normalize", "veto", "start_approval"];

/** Seconds → "2 giờ" (largest whole unit). */
function every(seconds: number): string {
  if (seconds % 86400 === 0) return tf(key("every.days"), { n: seconds / 86400 });
  if (seconds % 3600 === 0) return tf(key("every.hours"), { n: seconds / 3600 });
  if (seconds % 60 === 0) return tf(key("every.minutes"), { n: seconds / 60 });
  return tf(key("every.seconds"), { n: seconds });
}

const str = (v: unknown) => (typeof v === "string" ? v : "");

export function ruleSentence(wf: WorkflowDetail, entities: readonly EntityMeta[], fields: readonly FieldMeta[]): RuleSentence {
  const entityLabel = (name: string) => entities.find((e) => e.name === name)?.label || name;
  const fieldLabel = (name: string) => (name === TIMER_CREATED_AT || name === TIMER_UPDATED_AT ? name : fields.find((f) => f.name === name)?.label || name);
  const entity = entityLabel(str(wf.entityName));
  const raw = wf as Record<string, unknown>;
  const cond = (wf.condition ?? null) as { match?: string; saveOn?: string; rules?: Array<{ field: string; op: string; value?: unknown }> } | null;
  const trigger = str(wf.trigger);

  let when: string;
  if (trigger === "scheduled") {
    const secs = typeof raw.intervalSeconds === "number" ? raw.intervalSeconds : 0;
    when = secs > 0 ? tf(key("when.scheduled"), { every: every(secs), entity }) : tf(key("when.scheduledOnce"), { entity });
  } else if (trigger === "date_offset") {
    const timer = timerFromCondition(cond);
    when = tf(key("when.date_offset"), { timer: timer ? timerPreview(timer, fieldLabel(timer.dateField)) : "…", entity });
  } else if (trigger === "before_save") {
    const suffix = cond?.saveOn === "create" ? ".create" : cond?.saveOn === "update" ? ".update" : "";
    when = tf(key(`when.before_save${suffix}`), { entity });
  } else if (KNOWN_WHEN.includes(trigger)) {
    when = tf(key(`when.${trigger}`), { entity });
  } else {
    when = entity;
  }

  const rules = cond?.rules ?? [];
  const ifText = rules.length === 0
    ? t(key("ifNone"))
    : rules.map((r) => {
        const op = Object.entries(OP_MAP).find(([, v]) => v === r.op)?.[0] ?? r.op;
        const field = fieldLabel(r.field);
        if (isNullOp(r.op)) return tf(key("cond.noValue"), { field, op });
        const v = r.value;
        const value = v && typeof v === "object" && typeof (v as { field?: unknown }).field === "string"
          ? fieldLabel((v as { field: string }).field)
          : String(v ?? "") || t(key("blank"));
        return tf(key("cond"), { field, op, value });
      }).join(t(key(cond?.match === "any" ? "or" : "and")));

  const then = (wf.actions ?? []).map((a) => {
    const type = str(a.type);
    if (!KNOWN_THEN.includes(type)) return t(key("then.unknown"));
    const field = fieldLabel(str(a.field));
    switch (type) {
      case "field_update": {
        const value = String(a.value ?? "") || t(key("blank"));
        if (a.childEntity) return tf(key("then.field_update.child"), { field, value, child: entityLabel(str(a.childEntity)) });
        if (a.via) return tf(key("then.field_update.via"), { field, value, via: fieldLabel(str(a.via)) });
        return tf(key("then.field_update"), { field, value });
      }
      case "in_app_notification": return tf(key("then.in_app_notification"), { title: str(a.title) || str(a.message) || t(key("blank")) });
      case "email_alert": return a.to ? tf(key("then.email_alert"), { to: str(a.to) }) : t(key("then.email_alert.noTo"));
      case "create_record": return tf(key("then.create_record"), { entity: entityLabel(str(a.entityName)) });
      case "set_deadline": {
        const off = a.offset as { amount?: number; unit?: string } | undefined;
        const unit = TIMER_UNITS.find((u) => u.key === off?.unit)?.label ?? str(off?.unit);
        const offset = `${off?.amount ?? "…"} ${unit}`.trim();
        return tf(key(a.calendarId ? "then.set_deadline.calendar" : "then.set_deadline"), { field, offset });
      }
      case "veto": return tf(key("then.veto"), { message: str(a.message) || t(key("blank")) });
      default: return tf(key(`then.${type}`), { field });
    }
  });
  return { when, cond: ifText, then };
}
