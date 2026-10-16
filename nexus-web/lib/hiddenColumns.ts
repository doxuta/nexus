// HIDDEN-COL-SIGNAL — pure helper behind LayoutList's honest "N cột ẩn bởi quyền" chip.
//
// The list envelope's additive `meta.hiddenColumns` (api-conventions §14) counts the DISTINCT
// declared fields of the entity that field-ACL hides from the CALLER's role — the same set the
// server strips from every row. Without it, a permission-hidden column is indistinguishable from
// an empty one, so the FE deliberately shipped NO chip rather than a fake ("no-fake" decision,
// loop/STATE HIDDEN-COL-SIGNAL). The server omits `meta` entirely for admins and for counts of 0,
// and this helper returns null for absent/0/garbage — belt and suspenders: the chip renders ONLY
// on a positive, finite count.

/** Chip label for a given meta.hiddenColumns value; null = render no chip. */
export function aclHiddenChipLabel(hiddenColumns: number | undefined | null): string | null {
  if (typeof hiddenColumns !== "number" || !Number.isFinite(hiddenColumns)) return null;
  const n = Math.floor(hiddenColumns);
  if (n <= 0) return null;
  return `${n} cột ẩn bởi quyền`;
}
