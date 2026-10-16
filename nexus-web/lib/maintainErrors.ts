import { ApiError } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { formatBytes } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

const DOC_TYPE_KEY: Record<string, I18nKey> = {
  workflow: "draftBar.type.workflow", report: "draftBar.type.report", dashboard: "draftBar.type.dashboard",
  app: "draftBar.type.app", layout: "draftBar.type.layout", branding: "draftBar.type.branding",
  field: "draftBar.type.field", entity: "draftBar.type.entity",
};

/** Turn the raw `details` of a maintenance error into flat display strings (sizes, times, business labels). */
function flatten(d: Record<string, unknown>): Record<string, string | number> {
  const out: Record<string, string | number> = {};
  for (const [k, v] of Object.entries(d)) {
    if (typeof v === "number") out[k] = k.endsWith("Bytes") ? formatBytes(v) : v;
    else if (typeof v === "string") {
      const when = k.endsWith("At") ? new Date(v) : null;
      out[k] = when && !isNaN(when.getTime())
        ? when.toLocaleString("vi-VN", { hour: "2-digit", minute: "2-digit", day: "2-digit", month: "2-digit" })
        : v;
    } else if (Array.isArray(v) && k === "docTypes") {
      out[k] = v.map((x) => (DOC_TYPE_KEY[String(x)] ? t(DOC_TYPE_KEY[String(x)]) : String(x))).join(", ");
    }
  }
  return out;
}

/**
 * Sentence for a maintenance-zone error. Looks up `studio.maintain.err.<CODE>` and fills `{vars}` from details;
 * when a var is missing the clause is dropped (`.plain` variant) so `{…}` never reaches the screen; no key ⇒ the
 * server's own sentence via errorMessage(). Never prints the machine code.
 */
export function maintainErrorText(e: unknown): string {
  if (!(e instanceof ApiError)) return errorMessage(e);
  const key = `studio.maintain.err.${e.code}` as I18nKey;
  const text = t(key);
  if (text === key) return errorMessage(e);
  const vars = flatten((e.details ?? {}) as Record<string, unknown>);
  const need = [...text.matchAll(/\{(\w+)\}/g)].map((m) => m[1]);
  if (need.every((n) => n in vars)) return tf(key, vars);
  const plain = `${key}.plain` as I18nKey;
  return t(plain) === plain ? errorMessage(e) : t(plain);
}
