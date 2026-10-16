// Shared scanners for the Studio ratchet gates (specs/073 FR-146: T146, T165, T174). Not a test file.
// A gate counts violations per file and compares with test/studio-gates.allowlist.json, which was GENERATED from the
// state of the tree when the gate landed: a count may go down (edit the file, it is a diff to review), never up, and a
// file that is not in the list must have zero. Regenerate with `STUDIO_GATE_WRITE=1 npx vitest run test/studio-*`.
import { existsSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";

const ROOT = process.cwd();
// New shared components (outside these two folders) must be added here when they are created.
export const SCAN_DIRS = ["app/studio", "components/studio"];
const ALLOWLIST = join(ROOT, "test/studio-gates.allowlist.json");

export function listSources(dirs: string[] = SCAN_DIRS): string[] {
  const out: string[] = [];
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const full = join(dir, name);
      if (statSync(full).isDirectory()) walk(full);
      else if (/\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name)) out.push(relative(ROOT, full));
    }
  };
  for (const d of dirs) if (existsSync(join(ROOT, d))) walk(join(ROOT, d));
  return out.sort();
}

export const read = (rel: string) => readFileSync(join(ROOT, rel), "utf8");

/** Drops /* … *​/ and // … comments (not the `//` of a URL inside a string: it needs a non-colon before it). */
export function stripComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:"'`\\])\/\/.*$/gm, "$1");
}

type Counts = Record<string, number>;
type Allow = Record<string, Counts>;

const load = (): Allow => (existsSync(ALLOWLIST) ? (JSON.parse(readFileSync(ALLOWLIST, "utf8")) as Allow) : {});

/**
 * Compare per-file counts to the allow-list. Returns the entries that differ from the ceiling: UP is a new violation, DOWN means the ceiling must be lowered so it keeps tightening (empty = gate green).
 * With STUDIO_GATE_WRITE=1 the list is rewritten from `counts` instead.
 */
export function ratchet(gate: string, counts: Counts): string[] {
  const all = load();
  if (process.env.STUDIO_GATE_WRITE === "1") {
    const clean = Object.fromEntries(Object.entries(counts).filter(([, n]) => n > 0).sort(([a], [b]) => a.localeCompare(b)));
    all[gate] = clean;
    writeFileSync(ALLOWLIST, `${JSON.stringify(Object.fromEntries(Object.entries(all).sort(([a], [b]) => a.localeCompare(b))), null, 2)}\n`);
    return [];
  }
  const allowed = all[gate] ?? {};
  return Object.entries(counts)
    .filter(([k, n]) => n !== (allowed[k] ?? 0))
    .map(([k, n]) => `${k}: ${n} (allowed ${allowed[k] ?? 0})${n < (allowed[k] ?? 0) ? ": lower the ceiling (STUDIO_GATE_WRITE=1)" : ""}`)
    .concat(Object.keys(allowed).filter((k) => !(k in counts)).map((k) => `${k}: 0 (allowed ${allowed[k]}): lower the ceiling (STUDIO_GATE_WRITE=1)`));
}

/** Counts of `re` per source file (comments stripped), zero entries kept out. */
export function countPerFile(re: RegExp, files = listSources()): Counts {
  const out: Counts = {};
  for (const f of files) {
    const n = (stripComments(read(f)).match(re) ?? []).length;
    if (n > 0) out[f] = n;
  }
  return out;
}

// ── Native browser dialogs (FR-065) ──
// Not preceded by a word char or `.` so `onConfirm(` / `modal.confirm(` are not hits (`window.` is), and a
// declaration `function confirm()` is the page's own handler, not the browser's.
export const NATIVE_DIALOG_RE = /(?<![\w.])(?<!function\s+)(?:window\.)?(?:confirm|alert|prompt)\(/g;
export const findNativeDialogs = (text: string): string[] => stripComments(text).match(NATIVE_DIALOG_RE) ?? [];

// ── Fetchers that read by the tenant of the token (FR-022) ──
/**
 * Occurrences of any name in `names` outside a useProjectQuery(...) call (and outside import lines).
 * Balanced-parenthesis scan of the call that follows each `useProjectQuery(`.
 */
export function findStrayFetcherUse(text: string, names: readonly string[]): string[] {
  if (names.length === 0) return [];
  const code = stripComments(text).replace(/^\s*import[\s\S]*?;\s*$/gm, "");
  const covered: Array<[number, number]> = [];
  for (const m of code.matchAll(/useProjectQuery\s*\(/g)) {
    let depth = 0;
    let i = m.index! + m[0].length - 1;
    for (; i < code.length; i++) {
      if (code[i] === "(") depth++;
      else if (code[i] === ")" && --depth === 0) break;
    }
    covered.push([m.index!, i]);
  }
  const hits: string[] = [];
  for (const name of names) {
    for (const m of code.matchAll(new RegExp(`(?:(?<=\\bapi\\.)|(?<![\\w$.]))${name}(?![\\w$])`, "g"))) {
      if (!covered.some(([a, b]) => m.index! >= a && m.index! <= b)) hits.push(name);
    }
  }
  return hits;
}

/** Counts keyed `file::kind` for several patterns at once (kinds with zero hits are left out). */
export function countKinds(kinds: Record<string, RegExp>, files = listSources()): Counts {
  const out: Counts = {};
  for (const f of files) {
    const code = stripComments(read(f));
    for (const [kind, re] of Object.entries(kinds)) {
      const n = (code.match(re) ?? []).length;
      if (n > 0) out[`${f}::${kind}`] = n;
    }
  }
  return out;
}
