// AO-THUONG-HIEU p1 — client-side branding engine (../specs/013-tenant-branding/spec.md §5).
//
// Since specs/038 (SLDS) the reskin surface is the SLDS brand ramp: postcss-slds-brand.cjs rewrote
// every brand literal in the SLDS CSS to `var(--slds-r-color-brand-N, <stock>)`, so setting those
// raw hooks INLINE on <html> recolours every brand pixel (buttons, links, active tabs, focus ring).
// Step 50 is the tenant accent itself; darker steps mix it toward black (hover/active/links),
// lighter steps toward white (focus ring, tints). applyBranding also swaps the favicon/title and
// caches the document in localStorage so the next boot paints branded without waiting for the net.
// Scope: App face (customer chrome) + /login + tab. Studio face keeps Nexus (spec §1).
import type { Branding } from "@/lib/api";
import { inWorkspace } from "@/lib/workspace";

const STORAGE_KEY = "nexus_branding";
/** Custom event fired whenever branding is applied/cleared — useBranding() re-renders on it. */
export const BRANDING_EVENT = "nexus:branding";

// [step, mix] — how each SLDS brand step derives from the accent (step 50). The stock SLDS ramp
// spans ~#000310 … #eef4ff around #0176d3; these mixes follow the same light/dark spread.
const BRAND_RAMP: ReadonlyArray<readonly [number, string]> = [
  [5, "black 90%"], [10, "black 75%"], [15, "black 65%"], [20, "black 55%"], [30, "black 35%"],
  [35, "black 25%"], [40, "black 15%"], [45, "black 7%"], [55, "white 10%"], [60, "white 20%"],
  [65, "white 32%"], [70, "white 45%"], [80, "white 62%"], [85, "white 72%"], [90, "white 82%"],
  [95, "white 92%"],
];
const BRAND_VAR = (step: number) => `--slds-r-color-brand-${step}`;
const HEX_RE = /^#[0-9a-fA-F]{6}$/;

// Mirror of the server's logoUrl allowlist (defense-in-depth like HEX_RE: the cache is not a
// trusted input, and a future sink must not inherit an unvalidated URL). Keep in sync with
// services.brandingLogoPrefixes.
const LOGO_PREFIXES = [
  "https://", "http://",
  "data:image/png;base64,", "data:image/jpeg;base64,",
  "data:image/svg+xml;base64,", "data:image/webp;base64,",
];

export function safeLogoUrl(url: string | undefined): string | undefined {
  if (!url) return undefined;
  return LOGO_PREFIXES.some((p) => url.startsWith(p)) ? url : undefined;
}

// The pre-branding document title, captured on the unbranded→branded transition and released
// on restore, so un-branding brings the tab name back (review: the old asymmetry left a
// customer name on the tab after "Trở về mặc định Nexus").
let defaultTitle: string | null = null;

function setTitle(appName: string | undefined): void {
  if (appName) {
    if (defaultTitle === null) defaultTitle = document.title;
    document.title = appName;
  } else if (defaultTitle !== null) {
    document.title = defaultTitle;
    defaultTitle = null;
  }
}

/** Read the cached branding document (empty object when none/invalid). */
export function readCachedBranding(): Branding {
  if (typeof window === "undefined") return {};
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    const parsed = raw ? (JSON.parse(raw) as Branding) : {};
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

/** The SLDS brand ramp for an accent: step 50 = the accent, the rest mixed from it. {} when the
 *  hex is missing or malformed (the value lands in CSS — never pass it through unchecked). */
function brandRamp(hex: string | undefined): Record<string, string> {
  if (!hex || !HEX_RE.test(hex)) return {};
  const ramp: Record<string, string> = { [BRAND_VAR(50)]: hex };
  for (const [step, mix] of BRAND_RAMP) ramp[BRAND_VAR(step)] = `color-mix(in oklch, ${hex}, ${mix})`;
  return ramp;
}

// SLDS 2.264.1 declares these global hooks on `:where(html)` from the ramp. A custom property is
// resolved where it is declared, so a subtree that carries its own ramp must re-declare them or it
// keeps <html>'s values (brand button, accent borders). [hook, ramp step] — mirror of the SLDS CSS.
const HOOKS_FROM_RAMP: ReadonlyArray<readonly [string, number]> = [
  ["--slds-g-color-border-accent-1", 50], ["--slds-g-color-border-accent-2", 40], ["--slds-g-color-border-accent-3", 30],
  ["--slds-g-color-accent-1", 60], ["--slds-g-color-accent-2", 50], ["--slds-g-color-accent-3", 40],
  ["--slds-g-color-accent-container-1", 50], ["--slds-g-color-accent-container-2", 40],
  ["--slds-g-color-accent-container-3", 30], ["--slds-g-color-accent-dark-1", 20], ["--slds-g-color-accent-dark-2", 10],
  ["--slds-g-color-accent-light-1", 95], ["--slds-g-color-accent-light-2", 90],
  ...[10, 15, 20, 30, 40, 45, 50, 55, 60, 65, 70, 80, 90, 95].map((step) => [`--slds-g-color-brand-base-${step}`, step] as const),
];

/** Inline style that brands ONE subtree (the Studio → Thương hiệu preview) without touching the
 *  page: the ramp plus the global hooks derived from it. {} for a missing/malformed hex. */
export function brandScopeStyle(hex: string | undefined): Record<string, string> {
  const ramp = brandRamp(hex);
  if (!ramp[BRAND_VAR(50)]) return {};
  const style = { ...ramp };
  for (const [hook, step] of HOOKS_FROM_RAMP) style[hook] = `var(${BRAND_VAR(step)})`;
  return style;
}

/** Apply a branding document to the live page. Empty/absent fields fall back to the default
 *  Nexus look (vars removed, favicon + title restored). `persist: false` skips the localStorage
 *  cache — used by the login-page preview of a TYPED slug, so glancing at another tenant's
 *  branded login never pins their brand on this machine (security audit WARNING). */
export function applyBranding(b: Branding, opts?: { persist?: boolean }): void {
  if (typeof document === "undefined") return;
  const root = document.documentElement;

  const ramp = brandRamp(b.accentColor);
  for (const v of [BRAND_VAR(50), ...BRAND_RAMP.map(([step]) => BRAND_VAR(step))]) {
    if (ramp[v]) root.style.setProperty(v, ramp[v]);
    else root.style.removeProperty(v);
  }

  setFavicon(safeLogoUrl(b.logoUrl));
  setTitle(b.appName);

  // Inside a customer workspace the document is the CUSTOMER's: the boot cache is Nexus's (specs/062 L2b),
  // a reload of this tab or the next Nexus tab must not paint the customer's brand.
  if (opts?.persist !== false && !inWorkspace()) {
    try {
      const hasContent = Boolean(b.appName || b.logoUrl || b.accentColor);
      if (hasContent) window.localStorage.setItem(STORAGE_KEY, JSON.stringify(b));
      else window.localStorage.removeItem(STORAGE_KEY);
    } catch {
      // storage full/blocked — the live page is still branded, only the boot cache is lost
    }
  }
  window.dispatchEvent(new CustomEvent(BRANDING_EVENT, { detail: b }));
}

/** Drop branding entirely (logout / reset): vars off, cache cleared, listeners notified. */
export function clearBranding(): void {
  applyBranding({});
}

const DEFAULT_FAVICON = "/icon.svg";

function setFavicon(url: string | undefined): void {
  const href = url || DEFAULT_FAVICON;
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  if (!link) {
    link = document.createElement("link");
    link.rel = "icon";
    document.head.appendChild(link);
  }
  if (link.href !== href) link.href = href;
}
