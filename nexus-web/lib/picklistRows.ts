// picklistRows — pure state model for the W5 picklist/multi_select ROW editor (plan §3.2:
// one option per row with reorder/rename/delete, replacing the comma-string textarea).
//
// The stored contract stays `options.values: string[]` (order = display order). The editor works
// on Row objects with STABLE local ids so React keys / dnd-kit sortable ids survive renames —
// keying rows by their value would remount the input on every keystroke and break drag state.
//
// Serialization rule (rowsToValues): trim each value, drop empties. Duplicates are NOT silently
// dropped here — the UI blocks save and marks them via duplicateIds() so the admin resolves them
// consciously (silently deduping could orphan stored record values).

export interface PicklistRow {
  id: string; // editor-local stable id — NEVER persisted
  value: string;
  // Display label for this CODE (options.valueLabels layer, "New" → "Mới"). Optional and
  // independent of `value`: renaming the LABEL must never touch the code — the code is what
  // records store and what pack queues/reports/workflows compare against. Blank = no label
  // (the code shows as-is).
  label?: string;
}

let seq = 0;
function nextId(): string {
  seq += 1;
  return `plr-${seq}`;
}

/** Build editor rows from the stored values array (one row per value, stable ids).
 * `labels` (options.valueLabels) fills each row's display label when present. */
export function rowsFromValues(values: string[], labels?: Record<string, string>): PicklistRow[] {
  return values.map((v) => ({ id: nextId(), value: v, label: labels?.[v] ?? "" }));
}

/** Serialize back to the options.values contract: trimmed, empties dropped, order preserved. */
export function rowsToValues(rows: PicklistRow[]): string[] {
  return rows.map((r) => r.value.trim()).filter((v) => v.length > 0);
}

/**
 * Serialize the label layer back to options.valueLabels: one entry per row whose TRIMMED label is
 * non-empty, keyed by the trimmed CODE. Returns undefined when no row carries a label, so the
 * caller can omit the key entirely (an empty {} would still be stored and validated).
 *
 * A label whose row (code) was deleted here does NOT survive — the admin removing an option takes
 * its label with it, deliberately. (Orphan labels are only TOLERATED by the backend validator for
 * data that already exists; the authoring surface should not mint new ones.)
 */
export function rowsToValueLabels(rows: PicklistRow[]): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const code = r.value.trim();
    const label = (r.label ?? "").trim();
    if (!code || !label) continue;
    if (!(code in out)) out[code] = label; // duplicate codes: first row wins, same rule as duplicateIds
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** Update one row's display LABEL (never its code). */
export function relabelRow(rows: PicklistRow[], id: string, label: string): PicklistRow[] {
  return rows.map((r) => (r.id === id ? { ...r, label } : r));
}

/** Append a new empty row (the UI focuses it for typing). */
export function addRow(rows: PicklistRow[], value = ""): PicklistRow[] {
  return [...rows, { id: nextId(), value }];
}

export function removeRow(rows: PicklistRow[], id: string): PicklistRow[] {
  return rows.filter((r) => r.id !== id);
}

export function renameRow(rows: PicklistRow[], id: string, value: string): PicklistRow[] {
  return rows.map((r) => (r.id === id ? { ...r, value } : r));
}

/** Move the row with `id` so it lands at the current position of `overId` (dnd-kit semantics). */
export function moveRow(rows: PicklistRow[], id: string, overId: string): PicklistRow[] {
  const from = rows.findIndex((r) => r.id === id);
  const to = rows.findIndex((r) => r.id === overId);
  if (from < 0 || to < 0 || from === to) return rows;
  const next = [...rows];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

/**
 * Ids of rows whose TRIMMED value collides with an earlier row (first occurrence stays clean).
 * Empty rows are ignored — they get dropped at serialize time anyway.
 */
export function duplicateIds(rows: PicklistRow[]): Set<string> {
  const seen = new Set<string>();
  const dupes = new Set<string>();
  for (const r of rows) {
    const v = r.value.trim();
    if (!v) continue;
    if (seen.has(v)) dupes.add(r.id);
    else seen.add(v);
  }
  return dupes;
}

/** True when serialized rows differ from the stored values OR stored labels (dirty check for the
 * save button — without the label half, editing only a label leaves the save button grey). */
export function rowsChanged(rows: PicklistRow[], stored: string[], storedLabels?: Record<string, string>): boolean {
  const next = rowsToValues(rows);
  if (next.length !== stored.length) return true;
  if (next.some((v, i) => v !== stored[i])) return true;
  const nextLabels = rowsToValueLabels(rows) ?? {};
  const prevLabels = storedLabels ?? {};
  const keys = new Set([...Object.keys(nextLabels), ...Object.keys(prevLabels)]);
  for (const k of keys) {
    if ((nextLabels[k] ?? "") !== (prevLabels[k] ?? "")) return true;
  }
  return false;
}
