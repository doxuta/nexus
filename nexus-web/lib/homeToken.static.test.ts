// specs/073 ph1a T421 / FR-084 point (1) / SC-011: the builder's HOME token is the one credential that must never
// reach a customer's data. getHomeToken() therefore has a short, closed list of callers, and a new one fails here:
//   • nothing outside lib/api.ts may call it (components, studioContext, hooks: they ask getToken()/the session layer);
//   • inside lib/api.ts only the functions named below, each for a reason written beside it.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = join(__dirname, "..");
const SKIP_DIRS = new Set(["node_modules", ".next", "dist", "build", "coverage", "test-results"]);

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const full = join(dir, name);
    if (SKIP_DIRS.has(name)) return [];
    if (statSync(full).isDirectory()) return sources(full);
    return /\.(ts|tsx)$/.test(name) && !/\.test\.tsx?$/.test(name) ? [full] : [];
  });
}

/** Lines of `text` that call getHomeToken( — comments excluded. */
function callLines(text: string): number[] {
  return text
    .split("\n")
    .flatMap((line, i) => (/^\s*(\/\/|\*|\/\*)/.test(line) ? [] : /\bgetHomeToken\s*\(/.test(line) ? [i] : []));
}

// Why each may touch the home token. resumeWorkspace/closeWorkspace/enterWorkspace reach it only through postWorkspaceSession.
const ALLOWED_IN_API: Record<string, string> = {
  getHomeToken: "the definition",
  getBuilderToken: "outside a session the builder's own token IS the home token",
  liveWorkspace: "voids a session whose home login is gone",
  postWorkspaceSession: "open/resume: the only call that is ever made with the home token against another tenant's session route",
  logoutEverywhere: "after leaving the session, /auth/logout-all is asked as the home login",
  logout: "the home login signs itself out",
};

describe("who may use the home token (FR-084 point 1)", () => {
  it("no file outside lib/api.ts calls getHomeToken()", () => {
    const offenders = sources(ROOT)
      .filter((f) => relative(ROOT, f) !== join("lib", "api.ts"))
      .filter((f) => callLines(readFileSync(f, "utf8")).length > 0)
      .map((f) => relative(ROOT, f));
    expect(offenders).toEqual([]);
  });

  it("inside lib/api.ts only the named functions call it", () => {
    const text = readFileSync(join(ROOT, "lib", "api.ts"), "utf8");
    const lines = text.split("\n");
    const found = callLines(text).map((i) => {
      for (let j = i; j >= 0; j--) {
        const m = /^(?:export\s+)?(?:async\s+)?function\s+(\w+)/.exec(lines[j]);
        if (m) return m[1];
      }
      return "(top level)";
    });
    expect(found.length).toBeGreaterThan(0); // the scan can see something
    expect([...new Set(found)].filter((n) => !(n in ALLOWED_IN_API))).toEqual([]);
  });
});
