// lib/entityGroups.ts — sidebar DỮ LIỆU grouping (W3 A, plan §2.1).
// Pure logic: pinned entities float to the top (in pin order); the remainder stays a flat
// list when ≤ GROUP_THRESHOLD entities, else groups by module prefix (segment before the
// first "_": crm_* → "CRM", clinic_* → "Clinic", …). A prefix only becomes a group when
// ≥2 entities share it — singletons and un-prefixed entities fall into "Khác".
// NOTHING here is entity-specific: prefixes come from the metadata names themselves;
// PREFIX_LABELS is a cosmetic display map only (unknown prefixes get an uppercased label).

import type { EntityMeta } from "@/lib/api";

export const GROUP_THRESHOLD = 8;

export interface EntityGroup {
  key: string; // prefix ("crm") or "__other"
  label: string; // display label ("CRM", "Khác")
  items: EntityMeta[];
}

export interface GroupedEntities {
  /** Pinned entities in pin order (only those still present + visible). */
  pinned: EntityMeta[];
  /** Non-pinned entities as module groups; single group key "__flat" when not grouping. */
  groups: EntityGroup[];
  /** True when the module grouping kicked in (>GROUP_THRESHOLD unpinned entities). */
  grouped: boolean;
}

// Cosmetic labels for well-known module prefixes (display only — grouping itself is generic).
const PREFIX_LABELS: Record<string, string> = {
  crm: "CRM",
  clinic: "Clinic",
  lms: "LMS",
  erp: "ERP",
  hr: "HR",
};

const OTHER_KEY = "__other";
const OTHER_LABEL = "Khác";

function prefixOf(name: string): string | null {
  const i = name.indexOf("_");
  if (i <= 0) return null; // no prefix, or leading underscore
  return name.slice(0, i);
}

export function groupEntities(entities: EntityMeta[], pins: string[]): GroupedEntities {
  const byName = new Map(entities.map((e) => [e.name, e]));
  const pinned = pins
    .map((p) => byName.get(p))
    .filter((e): e is EntityMeta => !!e);
  const pinnedSet = new Set(pinned.map((e) => e.name));
  const rest = entities.filter((e) => !pinnedSet.has(e.name));

  if (rest.length <= GROUP_THRESHOLD) {
    return {
      pinned,
      groups: rest.length ? [{ key: "__flat", label: "", items: rest }] : [],
      grouped: false,
    };
  }

  // Count prefix occurrences; only prefixes shared by ≥2 entities form a group.
  const counts = new Map<string, number>();
  for (const e of rest) {
    const p = prefixOf(e.name);
    if (p) counts.set(p, (counts.get(p) ?? 0) + 1);
  }

  const groupMap = new Map<string, EntityGroup>();
  for (const e of rest) {
    const p = prefixOf(e.name);
    const key = p && (counts.get(p) ?? 0) >= 2 ? p : OTHER_KEY;
    let g = groupMap.get(key);
    if (!g) {
      g = {
        key,
        label: key === OTHER_KEY ? OTHER_LABEL : PREFIX_LABELS[key] ?? key.toUpperCase(),
        items: [],
      };
      groupMap.set(key, g);
    }
    g.items.push(e);
  }

  // Real module groups first (in first-appearance order), "Khác" always last.
  const groups = [...groupMap.values()].sort((a, b) => {
    if (a.key === OTHER_KEY) return 1;
    if (b.key === OTHER_KEY) return -1;
    return 0;
  });

  return { pinned, groups, grouped: true };
}
