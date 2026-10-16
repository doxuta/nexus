// specs/073 FR-001/FR-002 (lát u0-nen-token): the Studio token layer is ONE glue block in
// app/globals.css that only assigns values to SLDS hooks. This pins its shape: marker present,
// no hand-written colours, closed selector catalogue, no font-size on <html>, component hooks on
// `html:has(.nx-face-studio)` (so portalled Modal/toast inherit them), success tone >= 4.5:1.
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const css = readFileSync(join(process.cwd(), "app/globals.css"), "utf8");
const START = "/* — slds glue: u0-studio-tokens — */";
const start = css.indexOf(START);
const rest = start < 0 ? "" : css.slice(start + START.length);
const nextMarker = rest.search(/\/\* — slds glue: /);
const block = start < 0 ? "" : nextMarker < 0 ? rest : rest.slice(0, nextMarker);
// Drop comments before inspecting rules.
const code = block.replace(/\/\*[\s\S]*?\*\//g, "");
const rules = [...code.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
  selector: m[1].trim().replace(/\s+/g, " "),
  body: m[2],
}));
const bodyOf = (sel: string) => rules.find((r) => r.selector === sel)?.body ?? "";

const CATALOGUE = [
  /^html:has\(\.nx-face-studio\)(?: body| \.slds-text-heading_(?:large|medium|small))?$/,
  /^\.nx-face-studio$/,
  /^\.nx-tabs-zone$/,
  /^\.nx-path-steps(?: [\w.:\-]+)*$/,
  /^\.nx-proposed$/,
  /^\.nx-status(?:_(?:success|error|warn|weak|proposed))?$/,
  /^\.nx-dot(?:_(?:error|warning|info))?$/,
  /^\.nx-lede$/,
  /^\.nx-face-customer$/,
  /^html:has\(\.nx-face-studio\) \.nx-face-customer \.slds-text-heading_(?:large|medium|small)$/,
];

describe("Studio token glue block", () => {
  it("exists, after the s1-studio-shell block", () => {
    expect(start).toBeGreaterThan(css.indexOf("/* — slds glue: s1-studio-shell — */"));
    expect(rules.length).toBeGreaterThan(5);
  });

  it("has no hand-written colour (#hex / rgb / hsl)", () => {
    expect(rules.length).toBeGreaterThan(0);
    expect(code).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(code).not.toMatch(/\b(?:rgba?|hsla?)\(/);
  });

  it("uses only selectors from the closed catalogue", () => {
    expect(rules.length).toBeGreaterThan(0);
    const bad = rules
      .flatMap((r) => r.selector.split(",").map((s) => s.trim()))
      .filter((s) => !CATALOGUE.some((re) => re.test(s)));
    expect(bad).toEqual([]);
  });

  it("never sets font-size on <html> (it would move the rem root)", () => {
    expect(bodyOf("html:has(.nx-face-studio)")).not.toMatch(/font-size/);
    expect(bodyOf("html:has(.nx-face-studio) body")).toMatch(/font-size:\s*var\(--slds-g-font-scale-1\)/);
  });

  it("declares component hooks on html:has(.nx-face-studio), not on .nx-face-studio", () => {
    const hooks = bodyOf("html:has(.nx-face-studio)");
    for (const h of [
      "--slds-c-card-color-border",
      "--slds-c-card-radius-border",
      "--slds-c-modal-radius-border",
      "--slds-c-input-radius-border",
      "--slds-c-button-radius-border",
      "--slds-g-color-border-base-1",
    ]) {
      expect(hooks).toContain(h);
    }
    expect(hooks).toMatch(/scroll-padding-top:/);
  });

  it("brand 40-95 go to the electric-blue palette", () => {
    const hooks = bodyOf("html:has(.nx-face-studio)");
    expect(hooks).toMatch(/--slds-r-color-brand-40:\s*var\(--slds-g-color-palette-electric-blue-40\)\s*!important/);
    expect(hooks).toMatch(/--slds-r-color-brand-95:\s*var\(--slds-g-color-palette-electric-blue-95\)\s*!important/);
  });

  it("_success tone is success-base-40 (base-50 is only 4.19:1 on neutral-base-95)", () => {
    const ok = bodyOf(".nx-status_success");
    expect(ok).toContain("--slds-g-color-success-base-40");
    expect(ok).not.toContain("success-base-50");
  });

  it(".nx-face-customer restores the stock hooks", () => {
    const c = bodyOf(".nx-face-customer");
    expect(c).toMatch(/font-size:\s*var\(--slds-g-font-scale-base\)/);
    expect(c).toMatch(/--slds-g-color-border-base-1:\s*var\(--slds-g-color-neutral-base-80\)/);
    expect(c).toMatch(/--slds-c-card-radius-border:\s*initial/);
  });

  // FR-002: the customer face must not move. Every component hook the Studio layer assigns has to be
  // reset to `initial` inside .nx-face-customer, and the heading weights must revert to the base layer.
  it(".nx-face-customer resets EVERY --slds-c-* hook the layer assigns, and reverts heading weights", () => {
    const assigned = [...bodyOf("html:has(.nx-face-studio)").matchAll(/(--slds-c-[\w-]+):/g)].map((m) => m[1]);
    expect(assigned.length).toBeGreaterThanOrEqual(8);
    const c = bodyOf(".nx-face-customer");
    for (const h of assigned) expect(c, h).toMatch(new RegExp(`${h}:\\s*initial`));
    for (const sz of ["large", "medium", "small"]) {
      const sel = `html:has(.nx-face-studio) .nx-face-customer .slds-text-heading_${sz}`;
      expect(rules.some((r) => r.selector.split(",").map((x) => x.trim()).includes(sel) && /font-weight:\s*revert-layer/.test(r.body)), sel).toBe(true);
    }
  });
});
