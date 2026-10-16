// recordLabel — VIP label heuristic (doc ../specs/028-layout-l2-renderer/spec.md §5bis, D4 Founder "vip pro").
//
// Picks the human-readable display name for ONE dynamic record (detail header, relation chip,
// breadcrumb, toast). NEVER hard-codes the entity — it infers the label from the record's own
// `data` map + the entity field metadata, ranked across several tiers:
//
//   1. Preferred-name fields (case-insensitive, in order):
//        name → title → label → full_name → display_name → subject → code → email
//      First one that EXISTS as a field AND has a non-empty value wins.
//   2. First `text`-type field (by field `name` ASC — matches L1 default-gen order) with a value.
//   2.5 Expanded relation labels (DP-2.1): join-entities with no text identity label themselves
//        by their endpoints — first two expanded {id,label} relation values, " — "-joined.
//   3. Join two short fields when no clear name field exists (e.g. first_name + last_name,
//        code + email) — only when BOTH are non-empty, space-joined.
//   4. Fallback to `#<first 8 chars of id>` — never the raw long UUID / null / undefined.
//
// Ref-guard (DP-2.1): any candidate whose value LOOKS like an id (uuid / 32-hex) is a reference,
// not a name — every tier skips it (a text field holding a uuid can never become the label).
//
// Formatting: trim, strip HTML (rich_text safety), cap ~60 chars + "…". Empty-safe → "(chưa đặt tên)".
//
// When L1 ships an explicit `labelField` (DEFER Đ8) it should be passed via opts.labelField and
// is consulted BEFORE the heuristic.

import type { FieldMeta } from "@/lib/api";

// Minimal record shape this works on — anything with an id + flat data map (ApiRecord-compatible).
export interface LabelableRecord {
  id: string;
  data: Record<string, unknown>;
}

export interface RecordLabelOptions {
  // Explicit display field (future L1 `labelField`); takes priority over the heuristic when set.
  labelField?: string;
  // Max characters before truncation with an ellipsis. Default 60.
  maxLength?: number;
}

// Tier-1 preferred name fields, highest priority first (compared case-insensitively).
const PREFERRED_NAME_FIELDS = [
  "name",
  "title",
  "label",
  "full_name",
  "display_name",
  "subject",
  "code",
  "email",
] as const;

// Common "short" field pairs to join when no single name field exists (tier 3).
const JOIN_PAIRS: ReadonlyArray<readonly [string, string]> = [
  ["first_name", "last_name"],
  ["last_name", "first_name"],
  ["given_name", "family_name"],
  ["code", "email"],
  ["code", "name"],
];

const FALLBACK_EMPTY = "(chưa đặt tên)";

// DP-2.1: a value that LOOKS like an id is a reference, not a display name — never promote it
// to a label (a text field holding "b1551a1a-5b97-…" reads as broken UI). Full-match only.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const HEX32_RE = /^[0-9a-f]{32}$/i;
function looksLikeRef(s: string): boolean {
  return UUID_RE.test(s) || HEX32_RE.test(s);
}

export function recordLabel(
  record: LabelableRecord,
  fields: FieldMeta[] = [],
  opts: RecordLabelOptions = {},
): string {
  const data = record?.data ?? {};
  const maxLength = opts.maxLength ?? 60;

  // Index field names lower-cased → canonical name, so lookups are case-insensitive but we
  // still read the real key out of `data`.
  const fieldNames = new Set(fields.map((f) => f.name));
  const lowerToName = new Map<string, string>();
  for (const f of fields) lowerToName.set(f.name.toLowerCase(), f.name);

  // Resolve a field name to its trimmed, cleaned, non-empty string value (or null).
  // Id-shaped values are refs, not names (DP-2.1) — rejected here so every tier skips them.
  const valueOf = (fieldName: string | undefined): string | null => {
    if (!fieldName) return null;
    const cleaned = clean(data[fieldName]);
    if (!cleaned || looksLikeRef(cleaned)) return null;
    return cleaned;
  };

  // ── Tier 0a (#479): the table's OWN explicit choice (isLabel + isLabelExplicit) beats even a
  // relation's labelField — the server's expand order: table choice > relation labelField > default.
  const explicitField = fields.find((f) => f.isLabel && f.isLabelExplicit);
  if (explicitField) {
    const v = valueOf(explicitField.name);
    if (v) return format(v, maxLength);
  }

  // ── Tier 0: explicit labelField (a relation's own choice) ──
  if (opts.labelField) {
    const v = valueOf(opts.labelField);
    if (v) return format(v, maxLength);
  }

  // ── Tier 0b (#272): the field the table itself names its rows by — served as `isLabel` on
  // GET /metadata/fields, the same choice the server's ?expand uses, so a title, a card and a
  // lookup chip can never disagree. Beats every guess below.
  const chosen = fields.find((f) => f.isLabel);
  if (chosen) {
    const v = valueOf(chosen.name);
    if (v) return format(v, maxLength);
  }

  // ── Tier 1: preferred name fields (case-insensitive, ranked) ──
  for (const pref of PREFERRED_NAME_FIELDS) {
    const realName =
      lowerToName.get(pref) ?? (fieldNames.size === 0 ? pref : undefined);
    const v = valueOf(realName);
    if (v) return format(v, maxLength);
  }

  // ── Tier 2: first text-type field (by name ASC) with a value ──
  const textFields = fields
    .filter((f) => f.fieldType === "text")
    .sort((a, b) => a.name.localeCompare(b.name));
  for (const f of textFields) {
    const v = valueOf(f.name);
    if (v) return format(v, maxLength);
  }

  // ── Tier 2.5 (DP-2.1): expanded relation labels — pure join-entities (lms_enrollments:
  // student × course) have NO text identity of their own; their name IS their endpoints.
  // Only fires when the fetch expanded the relation (value = { id, label }); raw id strings
  // are refs and were already rejected by valueOf. Join the first two with " — ".
  const relLabels: string[] = [];
  for (const f of fields) {
    if (f.fieldType !== "relation") continue;
    const v = data[f.name];
    if (v && typeof v === "object" && typeof (v as { label?: unknown }).label === "string") {
      const cleaned = clean(v);
      if (cleaned && !looksLikeRef(cleaned)) relLabels.push(cleaned);
    }
    if (relLabels.length === 2) break;
  }
  if (relLabels.length > 0) return format(relLabels.join(" — "), maxLength);

  // ── Tier 3: join two short fields when both present ──
  for (const [a, b] of JOIN_PAIRS) {
    const realA = lowerToName.get(a) ?? (fieldNames.size === 0 ? a : undefined);
    const realB = lowerToName.get(b) ?? (fieldNames.size === 0 ? b : undefined);
    const va = valueOf(realA);
    const vb = valueOf(realB);
    if (va && vb) return format(`${va} ${vb}`, maxLength);
  }

  // ── Tier 4: fallback to #<short id> (never a raw UUID / null) ──
  const id = record?.id;
  if (id && typeof id === "string" && id.trim() !== "") {
    return `#${id.slice(0, 8)}`;
  }

  return FALLBACK_EMPTY;
}

// ── helpers ──

// Coerce any value to a clean, trimmed, HTML-stripped string. Returns "" for empty/null/object.
function clean(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return stripTags(v).trim();
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  // Relation refs come back as { id, label } when expanded — prefer the label.
  if (typeof v === "object") {
    const o = v as Record<string, unknown>;
    if (typeof o.label === "string") return stripTags(o.label).trim();
    return "";
  }
  return "";
}

function stripTags(s: string): string {
  return s.replace(/<[^>]*>/g, "");
}

// Final formatting: cap to maxLength with an ellipsis (counting the ellipsis), empty-safe.
function format(s: string, maxLength: number): string {
  const t = s.trim();
  if (t === "") return FALLBACK_EMPTY;
  if (t.length <= maxLength) return t;
  return t.slice(0, Math.max(1, maxLength - 1)).trimEnd() + "…";
}
