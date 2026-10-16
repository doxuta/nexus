// specs/073 kh1a T146 (FR-023, architecture 1.7): no link or script in the Studio opens another tab on a Nexus route.
// A link to a customer's own URL or to documentation may, with rel="noopener noreferrer": it goes in the allow-list below
// by being counted in test/studio-gates.allowlist.json. Ratchet: counts may only go down.
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { countPerFile, ratchet, stripComments } from "./studioGateScan";

// newTabProps() (lib/workspace) spreads target="_blank" onto a link, so it is the same offence in a helper's clothes.
const BLANK = /(?:target\s*[=:]\s*\{?\s*["'`]_blank["'`]|window\.open\s*\(|newTabProps\s*\()/g;

describe("Studio opens no new tab", () => {
  it("no target=_blank or window.open beyond the allow-list", () => {
    expect(ratchet("blankTarget", countPerFile(BLANK))).toEqual([]);
  });

  it("self-check: the pattern sees the shapes this codebase uses", () => {
    const n = (s: string) => (stripComments(s).match(BLANK) ?? []).length;
    expect(n('<a href="/x" target="_blank" rel="noopener">')).toBe(1);
    expect(n("<a target='_blank'>")).toBe(1);
    expect(n('<a target={"_blank"}>')).toBe(1);
    expect(n("const p = { target: \"_blank\" as const }")).toBe(1);
    expect(n('window.open("/x")')).toBe(1);
    expect(n("<a href={x} {...newTabProps()}>")).toBe(1);
    expect(n('import { newTabProps } from "@/lib/workspace";')).toBe(0);
    expect(n('<a target="_self">')).toBe(0);
    expect(n("// target=\"_blank\" is banned")).toBe(0);
  });
});

// F5 (specs/077): "Xem như" opens the real app in the SAME tab through location.assign, never a new tab.
// startPreviewSession is deliberately NOT in TOKEN_SCOPED_FETCHERS (test/studio-token-scoped-queries.test.ts): it
// is a mutation sent with getBuilderToken() on purpose (it mints the preview token), not a read of the project's token.
describe("Xem như stays in the tab", () => {
  it("studioViewAs and TrialRolesTable never open a new tab and use location.assign", () => {
    const strip = (f: string) => stripComments(readFileSync(resolve(process.cwd(), f), "utf8"));
    const view = strip("lib/studioViewAs.ts");
    const table = strip("components/studio/trial/TrialRolesTable.tsx");
    expect(view.match(BLANK) ?? []).toEqual([]);
    expect(table.match(BLANK) ?? []).toEqual([]);
    expect(view).toMatch(/location\.assign\(/);
    expect(table).toMatch(/viewAsRole\(/);
  });
});
