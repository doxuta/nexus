"use client";

// CompareTable — three versions of one upgrade item side by side: the old pack version, the builder's own and the new
// pack version (specs/079 f7-nang-cap-ke-hoach, plan.md 6 "So sánh ba cột"). One row per changed part; the mark in a
// column says what happened to that part since the old pack version: + added, - removed, ~ changed, = unchanged.
// When the server gave no part list (an added or removed item) the whole item is shown, one JSON block per column.
import type { UpgradeItem } from "@/lib/api";
import { t } from "@/lib/i18n";

/** "actions[0].stampField" -> ["actions", "0", "stampField"]. */
export const pathTokens = (p: string) => p.match(/[^.[\]]+/g) ?? [];

/** Display label of a part path: "actions[0].stampField" -> "actions #1 › stampField" (never the raw path). */
export const pathLabel = (p: string) => p.replace(/\[(\d+)\]/g, (_, n) => ` #${Number(n) + 1}`).replace(/\./g, " › ");

export function valueAt(root: unknown, path: string): unknown {
  let cur: unknown = root;
  for (const k of pathTokens(path)) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string, unknown>)[k];
  }
  return cur;
}

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/** + / - / ~ / = of `value` against the old pack version `base`. */
export function markOf(base: unknown, value: unknown): "+" | "-" | "~" | "=" {
  if (same(base, value)) return "=";
  if (base === undefined) return "+";
  if (value === undefined) return "-";
  return "~";
}

function Cell({ value }: { value: unknown }) {
  if (value === undefined) return <span className="slds-text-color_weak">{t("studio.maintain.upgrade.absent")}</span>;
  if (typeof value === "string") return <span className="break-words">{value}</span>;
  return <code className="slds-text-body_small break-words">{JSON.stringify(value)}</code>;
}

export function CompareTable({ item }: { item: UpgradeItem }) {
  const paths = [...new Set([...item.theirsPaths, ...item.oursPaths, ...item.overlap])];
  const hasMerged = item.merged !== undefined;
  const head = (
    <tr className="slds-line-height_reset">
      <th scope="col"><div className="slds-truncate">{t("studio.maintain.upgrade.col.part")}</div></th>
      <th scope="col"><div className="slds-truncate">{t("studio.maintain.upgrade.col.base")}</div></th>
      <th scope="col"><div className="slds-truncate">{t("studio.maintain.upgrade.col.live")}</div></th>
      <th scope="col"><div className="slds-truncate">{t("studio.maintain.upgrade.col.theirs")}</div></th>
      {hasMerged && <th scope="col"><div className="slds-truncate">{t("studio.maintain.upgrade.col.merged")}</div></th>}
    </tr>
  );
  const rows = paths.length > 0
    ? paths.map((p) => {
        const base = valueAt(item.base, p), live = valueAt(item.live, p), theirs = valueAt(item.theirs, p);
        return { key: p, label: pathLabel(p), base, live, theirs, merged: valueAt(item.merged, p), liveMark: markOf(base, live), theirsMark: markOf(base, theirs) };
      })
    : [{ key: "*", label: "", base: item.base, live: item.live, theirs: item.theirs, merged: item.merged, liveMark: markOf(item.base, item.live), theirsMark: markOf(item.base, item.theirs) }];
  return (
    <div className="slds-scrollable_x">
      <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={item.label || item.itemKey}>
        <thead>{head}</thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.key} className="slds-hint-parent">
              <th scope="row" className="break-all">{r.label}</th>
              <td><Cell value={r.base} /></td>
              <td><span aria-hidden="true" className="slds-m-right_x-small slds-text-color_weak">{r.liveMark}</span><Cell value={r.live} /></td>
              <td><span aria-hidden="true" className="slds-m-right_x-small slds-text-color_weak">{r.theirsMark}</span><Cell value={r.theirs} /></td>
              {hasMerged && <td><Cell value={r.merged} /></td>}
            </tr>
          ))}
        </tbody>
      </table>
      {paths.length === 0 && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.maintain.upgrade.noPaths")}</p>}
    </div>
  );
}
