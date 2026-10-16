// specs/074 lát f2-cleanup-scope-hub (T418): the per-app Studio scope, EntityHub and DataModelBuilder are gone — and
// stay gone. Comments are stripped first: a comment may name history, code may not name a deleted thing.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = join(__dirname, "..");
const DEAD = /\b(useStudioScope|ScopeChip|ScopeSubtitle|StudioScopeOverride|entityHubTabs|EntityHub|DataModelBuilder)\b/;

function walk(dir: string, out: string[] = []): string[] {
  for (const n of readdirSync(dir)) {
    if (n === "node_modules" || n === ".next") continue;
    const p = join(dir, n);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(ts|tsx)$/.test(n)) out.push(p);
  }
  return out;
}

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:"'`\\])\/\/.*$/gm, "$1");
}

describe("studio scope / hub removal", () => {
  const files = ["app", "components", "lib"].flatMap((d) => walk(join(ROOT, d)));

  it("no code names a deleted symbol", () => {
    const hits = files.filter((f) => DEAD.test(stripComments(readFileSync(f, "utf8")))).map((f) => relative(ROOT, f));
    expect(hits).toEqual([]);
  });

  it("no file is named after a deleted component", () => {
    const hits = files.map((f) => relative(ROOT, f)).filter((f) => /(EntityHub|DataModelBuilder|ScopeChip|StudioScopeOverride|entityHubTabs|studioScope)\b/.test(f));
    expect(hits).toEqual([]);
  });
});
