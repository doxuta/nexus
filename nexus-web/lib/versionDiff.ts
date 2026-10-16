// Pure helpers of the publish-history version page (specs/079 f7-lich-su-dong-thoi-gian, FR-064).
// The server already masks secrets in before/after; maskedValue is the second line of defence by key name.
import type { PublishVersionDoc } from "./api";
import { t } from "./i18n";
import type { I18nKey } from "./i18n/vi";

export const MASK = "[đã che]";
const SECRET_KEY = /secret|token|password|authorization|api[-_]?key|signing/i;

export type Lookup = { found: true; value: unknown } | { found: false };

/** `actions[0].stampField` / `options.values[1]` → the value in `doc`, or found:false. */
export function valueAtPath(doc: unknown, path: string): Lookup {
  let cur = doc;
  for (const m of path.matchAll(/([^.[\]]+)|\[(\d+)\]/g)) {
    if (cur === null || typeof cur !== "object") return { found: false };
    const key = m[2] !== undefined ? Number(m[2]) : m[1];
    if (!Object.prototype.hasOwnProperty.call(cur, key)) return { found: false };
    cur = (cur as Record<string | number, unknown>)[key];
  }
  return { found: true, value: cur };
}

export interface ChangeRow {
  path: string;
  label: string | null;
  before: unknown;
  after: unknown;
  mark: "+" | "~" | "−" | "=";
  /** false = neither side can look the path up (shown in "Chi tiết", never dropped). */
  found: boolean;
}

const lastKey = (path: string) => path.split(/[.[\]]/).filter(Boolean).pop() ?? "";
const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

export function docChangeRows(doc: PublishVersionDoc): ChangeRow[] {
  return doc.changedPaths.map((path) => {
    const b = valueAtPath(doc.before, path);
    const a = valueAtPath(doc.after, path);
    const secret = SECRET_KEY.test(lastKey(path));
    const show = (l: Lookup) => (!l.found ? undefined : secret ? MASK : l.value);
    const mark: ChangeRow["mark"] = a.found && !b.found ? "+" : !a.found && b.found ? "−" : a.found && b.found && same(a.value, b.value) ? "=" : "~";
    return { path, label: null, before: show(b), after: show(a), mark, found: a.found || b.found };
  });
}

type DocWithTable = PublishVersionDoc & { entityName?: string };

export function filterDocs<T extends PublishVersionDoc>(docs: T[], f: { loai?: string; bang?: string }): T[] {
  const hasTable = (docs as DocWithTable[]).some((d) => d.entityName);
  return docs.filter((d) => (!f.loai || d.docType === f.loai) && (!f.bang || !hasTable || (d as DocWithTable).entityName === f.bang));
}

const TYPE: Record<string, I18nKey> = {
  workflow: "draftBar.type.workflow", report: "draftBar.type.report", dashboard: "draftBar.type.dashboard",
  app: "draftBar.type.app", layout: "draftBar.type.layout", branding: "draftBar.type.branding",
  field: "draftBar.type.field", entity: "draftBar.type.entity", relationship: "draftBar.type.relationship",
  role: "studio.maintain.version.type.role", field_acl: "studio.maintain.version.type.field_acl",
};
export const docTypeLabel = (docType: string) => (TYPE[docType] ? t(TYPE[docType]) : docType);
