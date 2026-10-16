// Pure slug helper (no React) — shared by app names (lib/apps.tsx) and Studio sub-field names.

/** Slug suggestion from a Vietnamese label: strip diacritics, lowercase, spaces→_, keep
 * [a-z0-9_], ensure a leading letter, cap at 60 (server rule ^[a-z][a-z0-9_]{0,59}$). */
export function slugifyAppName(label: string): string {
  const ascii = label
    .normalize("NFD")
    .replace(/[\u0300-\u036f]/g, "") // combining diacritics
    .replace(/đ/g, "d")
    .replace(/Đ/g, "D")
    .toLowerCase();
  let slug = ascii
    .replace(/[\s-]+/g, "_")
    .replace(/[^a-z0-9_]/g, "")
    .replace(/_+/g, "_")
    .replace(/^_+|_+$/g, "");
  slug = slug.replace(/^[^a-z]+/, ""); // must start with a letter
  return slug.slice(0, 60);
}
