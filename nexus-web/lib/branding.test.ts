// applyBranding contract (AO-THUONG-HIEU p1 + specs/038 FR-003): the tenant accent becomes the SLDS
// brand ramp (--slds-r-color-brand-*; step 50 = the accent itself, every other step a mix of it), so
// every SLDS brand pixel follows it; favicon/title follow the document, and clearing restores the
// default look — the exact invariants a stale cache or a malformed hex must not break.
import { describe, it, expect, beforeEach } from "vitest";
import { applyBranding, brandScopeStyle, clearBranding, readCachedBranding } from "./branding";

describe("applyBranding", () => {
  beforeEach(() => {
    window.localStorage.clear();
    clearBranding();
    document.title = "Nexus";
  });

  it("sets the SLDS brand ramp from a valid hex and caches the document", () => {
    applyBranding({ appName: "ABC Manager", accentColor: "#123456" });
    const s = document.documentElement.style;
    expect(s.getPropertyValue("--slds-r-color-brand-50")).toBe("#123456");
    // Darker steps (hover/active/links) and lighter steps (focus ring, tints) derive from it.
    for (const step of [10, 20, 30, 40, 60, 65, 90, 95]) {
      expect(s.getPropertyValue(`--slds-r-color-brand-${step}`)).toContain("#123456");
    }
    // The legacy Nexus Be accent vars are no longer written — they alias the SLDS hook in CSS.
    expect(s.getPropertyValue("--ac")).toBe("");
    expect(document.title).toBe("ABC Manager");
    expect(readCachedBranding().appName).toBe("ABC Manager");
  });

  it("rejects a non-hex accent (no CSS injection) and clears stale vars", () => {
    applyBranding({ accentColor: "#123456" });
    applyBranding({ accentColor: "#123456;background:url(x)" });
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-50")).toBe("");
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-40")).toBe("");
  });

  it("un-branding restores the captured default title", () => {
    document.title = "Nexus — Nền tảng";
    applyBranding({ appName: "ABC Manager" });
    expect(document.title).toBe("ABC Manager");
    applyBranding({});
    expect(document.title).toBe("Nexus — Nền tảng");
  });

  it("persist:false brands the page without touching the cache (login preview)", () => {
    applyBranding({ accentColor: "#ABCDEF" }, { persist: false });
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-50")).toBe("#ABCDEF");
    expect(readCachedBranding()).toEqual({});
  });

  it("a cached logoUrl outside the allowlist never reaches the favicon", () => {
    applyBranding({ logoUrl: "javascript:alert(1)" }, { persist: false });
    const icon = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
    expect(icon?.href).not.toContain("javascript");
  });

  it("clearBranding removes vars, cache and restores the default favicon", () => {
    applyBranding({ accentColor: "#0F6E84", logoUrl: "data:image/png;base64,AAAA" });
    clearBranding();
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-50")).toBe("");
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-60")).toBe("");
    expect(readCachedBranding()).toEqual({});
    const icon = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
    expect(icon?.href).toContain("/icon.svg");
  });
});

// Studio → Thương hiệu previews an UNSAVED accent on real SLDS components. SLDS resolves its brand
// hooks on <html> (and Studio pins that ramp to stock), so the preview subtree must carry its own
// ramp AND re-declare the global hooks derived from it — otherwise the brand button keeps the
// value <html> computed.
describe("brandScopeStyle", () => {
  it("carries the ramp plus the SLDS global hooks that derive from it", () => {
    const s = brandScopeStyle("#2e844a");
    expect(s["--slds-r-color-brand-50"]).toBe("#2e844a");
    expect(s["--slds-r-color-brand-40"]).toContain("#2e844a");
    expect(s["--slds-g-color-accent-container-1"]).toBe("var(--slds-r-color-brand-50)");
    expect(s["--slds-g-color-brand-base-60"]).toBe("var(--slds-r-color-brand-60)");
  });

  it("is empty for a missing or malformed hex (no CSS injection)", () => {
    expect(brandScopeStyle(undefined)).toEqual({});
    expect(brandScopeStyle("#12345;x:y")).toEqual({});
  });

  it("applyBranding writes the same ramp onto <html>", () => {
    applyBranding({ accentColor: "#2e844a" }, { persist: false });
    const s = brandScopeStyle("#2e844a");
    for (const step of [10, 50, 95]) {
      const v = `--slds-r-color-brand-${step}`;
      expect(document.documentElement.style.getPropertyValue(v)).toBe(s[v]);
    }
    clearBranding();
  });
});
