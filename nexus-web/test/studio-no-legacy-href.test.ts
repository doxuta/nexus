// specs/073 kh6 T341/T345 (FR-142): nothing outside the files kh7 deletes may link to the old Studio routes.
// The pattern does NOT demand a quote in front, so ', " and ` (template literals) all land. Comments are dropped first.
// No allow-list any more (kh7 deleted the old IA): every file is scanned.
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = process.cwd();
const LEGACY = /\/studio\/(model|layout|relationships|workflows|approvals|workqueues|hooks|branding|dashboards|reports|deploy|apps)\b/g;
const DIRS = ["app", "components", "lib", "scripts"];

function sources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    if (name === "node_modules" || name === ".next") continue;
    const full = join(dir, name);
    if (statSync(full).isDirectory()) sources(full, out);
    else if (/\.(ts|tsx|mjs|js)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name)) out.push(relative(ROOT, full));
  }
  return out;
}

// Not studioGateScan.stripComments: its block-comment pass starts at the "/*" of a comment that says "/studio/*" and
// swallows real code up to the next "*/" (AboutScreen and CommandPalette hid from the gate that way). Here a block
// comment must open after whitespace, "{" or "(", and a line comment after whitespace or at line start.
const stripComments = (text: string) =>
  text.replace(/(^|[\s{(])\/\*[\s\S]*?\*\//g, "$1").replace(/(^|\s)\/\/.*$/gm, "$1");

const countLegacy = (text: string) => (stripComments(text).match(LEGACY) ?? []).length;

describe("no link to an old Studio route", () => {
  it("no file carries one", () => {
    const bad = DIRS.flatMap((d) => sources(join(ROOT, d)))
      .map((f) => [f, countLegacy(readFileSync(join(ROOT, f), "utf8"))] as const)
      .filter(([, n]) => n > 0)
      .map(([f, n]) => `${f}: ${n}`);
    expect(bad).toEqual([]);
  });

  it("self-check: single, double and backtick quotes and a bare path all count", () => {
    expect(countLegacy(`href="/studio/model"`)).toBe(1);
    expect(countLegacy(`href='/studio/apps'`)).toBe(1);
    expect(countLegacy("href={`/studio/apps/${x}`}")).toBe(1);
    expect(countLegacy(`router.push(base + "/studio/layout?e=1")`)).toBe(1);
    expect(countLegacy("// /studio/model is gone")).toBe(0);
    expect(countLegacy("// link /studio/* here\nhref=\"/studio/model\"")).toBe(1);
    expect(countLegacy(`"/studio/du-an/x/dung/du-lieu"`)).toBe(0);
    expect(countLegacy(`"/studio/verticals"`)).toBe(0);
  });
});
