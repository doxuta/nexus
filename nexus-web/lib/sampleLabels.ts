// Labels for where a sample row came from (specs/077 f5-trial-sample-view). Order is fixed: Trợ lý · Tự sinh · Gói.
import type { SampleWipeKept } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export interface SampleBySource { ai?: number; generator?: number; pack?: number }

const ORDER = ["ai", "generator", "pack"] as const;

/** "Trợ lý 3 · Tự sinh 5 · Gói 2"; sources with 0 rows (or missing keys) are left out. */
export function sampleSourceText(by: SampleBySource | undefined): string {
  return ORDER.filter((k) => (by?.[k] ?? 0) > 0)
    .map((k) => `${t(`studio.build.trial.sample.source.${k}` as const)} ${by?.[k]}`)
    .join(" · ");
}

// Words for the sample-data generator (specs/077 T095). The server answers with SAMPLE_* codes; the builder reads
// a Vietnamese sentence, and an unknown code never leaks through as text.

const SKIP = [
  "SAMPLE_FILE_REQUIRED",
  "SAMPLE_NO_USER",
  "SAMPLE_PARENT_EMPTY",
  "SAMPLE_VETOED",
  "SAMPLE_PARENT_SKIPPED",
  "SAMPLE_ALREADY_PRESENT",
  "SAMPLE_PACK_UNAVAILABLE",
] as const;

export type SkipKey = `studio.build.trial.generate.skip.${(typeof SKIP)[number] | "other"}`;

export function skipReasonKey(code: string): SkipKey {
  return (SKIP as readonly string[]).includes(code)
    ? (`studio.build.trial.generate.skip.${code}` as SkipKey)
    : "studio.build.trial.generate.skip.other";
}

export const skipReason = (code: string) => t(skipReasonKey(code));

// Labels and links for the sample-data wipe UI (specs/077 f5-sample-wipe).

// ponytail: van-hanh/du-lieu/<bảng>/<id> (F6) is not mounted yet, so the link goes to the record page; switch to
// studioHref(slug, "van-hanh/du-lieu/:bang/:id", …) once that route is mounted.
export const recordHref = (entity: string, id: string) => `/${encodeURIComponent(entity)}/${encodeURIComponent(id)}`;

/** "{Bảng} · …{6 last characters of the id}" — enough to tell kept rows apart without printing a uuid. */
export const shortRecord = (table: string, id: string) => `${table} · …${id.slice(-6)}`;

const REASON_KEY: Record<SampleWipeKept["reason"], I18nKey> = {
  referenced_by_real: "studio.build.trial.wipe.kept.referenced",
  field_default: "studio.build.trial.wipe.kept.fieldDefault",
  too_large: "studio.build.trial.wipe.kept.tooLarge",
  busy: "studio.build.trial.wipe.kept.busy",
};

/** Why a row was kept; `field_default` names "{bảng} › {trường}" from `blockedBy`. */
export function keptReason(k: SampleWipeKept, label: (entity: string) => string): string {
  const b = k.blockedBy[0];
  return tf(REASON_KEY[k.reason], { table: b ? label(b.entity) : "", field: b?.field ?? "" });
}
