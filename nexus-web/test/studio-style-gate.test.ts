// specs/073 kh1a T146 (architecture 4.10): Studio screens take colour, type, radius and shadow from SLDS hooks, never from
// Tailwind utilities, hex codes or the old "Nexus Be" tokens; status is coloured text, never a Badge/Pill.
// Ratchet: the allow-list was generated from the tree as it was; counts may only go down (see studioGateScan.ts).
import { describe, expect, it } from "vitest";
import { countKinds, listSources, ratchet, stripComments } from "./studioGateScan";

const TW = "(?:bg|text|border|ring|fill|stroke|from|to|via|divide|outline|decoration|placeholder|accent|caret|shadow)";
export const KINDS: Record<string, RegExp> = {
  rounded: /(?<![\w-])rounded(?:-[\w[\]./%-]+)?(?![\w-])/g,
  shadow: /(?<![\w-])shadow(?:-[\w[\]./%-]+)?(?![\w-])/g,
  arbitraryBg: /(?<![\w-])bg-\[/g,
  arbitraryText: /(?<![\w-])text-\[/g,
  hex: /#[0-9a-fA-F]{3,8}\b/g,
  nexusBeToken: new RegExp(`(?<![\\w-])${TW}-(?:bg|panel2?|bd[23]?|fg|mut|fnt|ac-on|ac-hi|ac2|acf|acd|acb|ac|ok[fdb]?|cyd|cy|wn[fd]?|dg[fdb]?|neu)(?![\\w-])`, "g"),
};
// Only components/studio: app/studio is route glue and never draws a status.
const BADGE: Record<string, RegExp> = { badgeOrPill: /(?:import[^;]*\b(?:Badge|Pill)\b[^;]*;|<(?:Badge|Pill)\b)/g };

describe("Studio style gate", () => {
  it("no Tailwind rounding/shadow/arbitrary colour, no hex, no Nexus Be token — beyond the allow-list", () => {
    expect(ratchet("style", countKinds(KINDS))).toEqual([]);
  });

  it("no Badge or Pill in components/studio — beyond the allow-list", () => {
    expect(ratchet("badge", countKinds(BADGE, listSources(["components/studio"])))).toEqual([]);
  });

  // The gate must be able to fail.
  it("self-check: each pattern sees its own offence and leaves the SLDS way alone", () => {
    const hit = (kind: string, text: string) => (stripComments(text).match(KINDS[kind]) ?? []).length;
    expect(hit("rounded", '<div className="rounded-lg p-2">')).toBe(1);
    expect(hit("rounded", '<div className="rounded">')).toBe(1);
    expect(hit("shadow", '<div className="shadow-md">')).toBe(1);
    expect(hit("arbitraryBg", '<div className="bg-[#fff]">')).toBe(1);
    // built from parts: the type-scale gate greps this file for the literal and would flag the sample
    expect(hit("arbitraryText", `<div className="text-${"[15px]"}">`)).toBe(1);
    expect(hit("hex", 'style={{ color: "#1a2b3c" }}')).toBe(1);
    expect(hit("nexusBeToken", '<div className="bg-panel2 text-mut border-ac/30">')).toBe(3);
    expect(hit("nexusBeToken", '<div className="text-ac-hi">')).toBe(1);
    // the SLDS way and layout-only Tailwind are fine
    expect(hit("rounded", '<div className="slds-m-around_small flex gap-2 slds-border_top">')).toBe(0);
    expect(hit("nexusBeToken", '<div className="slds-text-color_weak text-left">')).toBe(0);
    expect(hit("hex", "&#39;")).toBe(0);
    expect((stripComments('import { Badge } from "@/components/slds/Badge";').match(BADGE.badgeOrPill) ?? []).length).toBe(1);
    expect((stripComments("<Pill>x</Pill>").match(BADGE.badgeOrPill) ?? []).length).toBe(1);
    expect((stripComments("// no Badge here").match(BADGE.badgeOrPill) ?? []).length).toBe(0);
  });
});
