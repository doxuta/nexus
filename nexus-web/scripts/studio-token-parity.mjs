#!/usr/bin/env node
// specs/073 FR-002 / kiến trúc UI 4.2: the Studio token layer must NOT leak into the customer face.
// Real Chrome. Renders the SAME template (/dev/kit "Studio" probe: .nx-parity-probe) in three places and
// compares getComputedStyle of brand button, card, input, tab and heading:
//   A  inside .nx-face-customer, which sits inside the Studio shell      (page /dev/kit?section=studio)
//   B  the real customer route /crm_accounts/new (button + input) and the same probe with the layer off
//   C  inside the Studio but outside .nx-face-customer
// Expect A == B for every property, C != B. Also: a Prompt and a toast opened inside Studio share the
// Studio look (button radius pill) with the Studio working surface.
//
//   node scripts/studio-token-parity.mjs        (FE :$NEXUS_FE or 3000, API :$NEXUS_API)
import puppeteer from "puppeteer-core";

const API = process.env.NEXUS_API ?? "http://localhost:8090/api/v1";
const FE = process.env.NEXUS_FE ?? "http://localhost:3000";
const CHROME = process.env.CHROME_PATH ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

async function login(email, password) {
  const r = await fetch(`${API}/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant: "northwind", email, password }),
  });
  const j = await r.json();
  if (!j.data?.accessToken) throw new Error(`login ${email}: ${JSON.stringify(j)}`);
  return j.data;
}

// Mirrors how lib/api.ts login() persists a remember-me session (same as ui-shots.mjs).
async function openAs(browser, sess) {
  const ctx = await browser.createBrowserContext();
  const page = await ctx.newPage();
  await page.setViewport({ width: 1440, height: 900 });
  await page.evaluateOnNewDocument((s) => {
    localStorage.setItem("nexus_remember", "1");
    localStorage.setItem("nexus_token", s.accessToken);
    if (s.refreshToken) localStorage.setItem("nexus_refresh", s.refreshToken);
    if (s.user) localStorage.setItem("nexus_user", JSON.stringify(s.user));
    localStorage.setItem("nexus_tenant", "northwind");
  }, sess);
  return page;
}

const PROPS = {
  button: ["borderRadius", "height", "backgroundColor", "fontSize"],
  card: ["borderRadius", "borderTopColor", "borderTopWidth"],
  input: ["borderRadius", "borderTopColor", "height"],
  tab: ["height", "fontSize"],
  heading: ["fontWeight", "fontSize"],
};
const SEL = {
  button: ".slds-button_brand",
  card: ".slds-card",
  input: "input.slds-input",
  tab: ".slds-tabs_default__link",
  heading: ".slds-text-heading_medium",
};

async function measure(page, scope) {
  return page.evaluate(
    (scopeSel, SEL, PROPS) => {
      const root = document.querySelector(scopeSel) ?? document;
      const out = {};
      for (const [k, sel] of Object.entries(SEL)) {
        const el = root.querySelector(sel);
        if (!el) { out[k] = null; continue; }
        const cs = getComputedStyle(el);
        out[k] = Object.fromEntries(PROPS[k].map((p) => [p, cs[p]]));
      }
      return out;
    },
    scope, SEL, PROPS,
  );
}

const diff = (a, b, only = Object.keys(PROPS)) => {
  const d = [];
  for (const k of only) {
    if (!a[k] || !b[k]) { d.push(`${k}: missing (${a[k] ? "B" : "A"})`); continue; }
    for (const p of PROPS[k]) if (a[k][p] !== b[k][p]) d.push(`${k}.${p}: ${a[k][p]} vs ${b[k][p]}`);
  }
  return d;
};

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
let failed = false;
try {
  // Customer reference B: a non-admin on the real create form (is_admin would skip ACL, campaign §5).
  const repPage = await openAs(browser, await login("test@example.com", "Demo-Rep-2026!"));
  await repPage.goto(`${FE}/crm_accounts/new`, { waitUntil: "networkidle2" });
  const B = await measure(repPage, "main");

  // Studio: A and C come from the probe on /dev/kit (its StudioSection carries .nx-face-studio).
  const page = await openAs(browser, await login("admin@example.com", "Demo-Admin-2026!"));
  await page.goto(`${FE}/dev/kit#studio`, { waitUntil: "networkidle2" });
  const A = await measure(page, ".nx-parity-probe[data-where='customer']");
  const C = await measure(page, ".nx-parity-probe[data-where='studio']");

  // B2: the same probe with the layer switched off (?studio=off) = full customer baseline, incl. tabs/heading.
  const base = await openAs(browser, await login("admin@example.com", "Demo-Admin-2026!"));
  await base.goto(`${FE}/dev/kit?studio=off#studio`, { waitUntil: "networkidle2" });
  const B2 = await measure(base, ".nx-parity-probe[data-where='customer']");

  // F: the real CustomerPreviewFrame (not a hand-made .nx-face-customer wrapper) must match the same baseline.
  const F = await measure(page, ".nx-parity-probe[data-where='frame']");
  const fb = [...diff(F, B, ["button", "input"]), ...diff(F, B2)];
  console.log(fb.length ? `FAIL F!=B (CustomerPreviewFrame leaks Studio):\n  ${fb.join("\n  ")}` : "OK   F == B (CustomerPreviewFrame matches the real customer face)");
  if (fb.length) failed = true;

  const ab = [...diff(A, B, ["button", "input"]), ...diff(A, B2)];
  const cb = diff(C, B2);
  console.log(ab.length ? `FAIL A!=B (customer frame leaks Studio):\n  ${ab.join("\n  ")}` : "OK   A == B (frame in Studio matches the real customer face)");
  console.log(cb.length ? `OK   C != B (${cb.length} props differ: Studio look is on)` : "FAIL C == B (Studio layer is not applied)");
  if (ab.length || !cb.length) failed = true;

  // Portalled overlay: a Prompt opened in Studio lands on <body>, outside .nx-face-studio, and must
  // still wear the Studio button shape (hooks live on html:has(), not on .nx-face-studio).
  await page.click("#parity-prompt-open");
  await page.waitForSelector(".slds-modal_prompt .slds-button");
  const pr = await page.$eval(".slds-modal_prompt .slds-button", (el) => getComputedStyle(el).borderRadius);
  const studioBtn = C.button?.borderRadius;
  console.log(pr === studioBtn ? `OK   Prompt button radius == Studio button (${pr})` : `FAIL Prompt button radius ${pr} != Studio button ${studioBtn}`);
  if (pr !== studioBtn) failed = true;
  // Toast (u0-toast): the layout Toaster renders in <body>, outside .nx-face-studio. It must be the SLDS
  // toast and its close button must wear the Studio button shape, like the Prompt above.
  await page.evaluate(() => [...document.querySelectorAll("button")].find((b) => b.textContent.includes("Bắn toast thành công"))?.click());
  await page.waitForSelector(".slds-notify_toast .slds-button");
  const tr = await page.$eval(".slds-notify_toast .slds-button", (el) => getComputedStyle(el).borderRadius);
  const prBtn = await page.$eval(".slds-modal_prompt .slds-button", (el) => getComputedStyle(el).borderRadius);
  console.log(tr === prBtn ? `OK   toast button radius == Prompt button (${tr})` : `FAIL toast button radius ${tr} != Prompt button ${prBtn}`);
  if (tr !== prBtn) failed = true;
  if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/studio-toast.png` });
  // Contrast (WCAG 2.x, >= 4.5:1): the five StatusText tones on the page, card, shade-strip and selected-row
  // grounds, white text on a brand button, and a link on the page ground. Colours are read from the rendered
  // /dev/kit block (data-testid="status-grounds"), so a token change shows up here.
  const rows = await page.evaluate(() => {
    const cv = document.createElement("canvas").getContext("2d");
    const rgb = (c) => { cv.clearRect(0, 0, 1, 1); cv.fillStyle = "#000"; cv.fillStyle = c; cv.fillRect(0, 0, 1, 1); return [...cv.getImageData(0, 0, 1, 1).data]; };
    const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    const lin = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
    const ratio = (a, b) => { const [x, y] = [lum(rgb(a)), lum(rgb(b))].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
    const bgOf = (el) => { for (let e = el; e; e = e.parentElement) { const c = getComputedStyle(e).backgroundColor; if (rgb(c)[3] === 255) return c; } return "rgb(255,255,255)"; };
    const out = [];
    for (const g of document.querySelectorAll("[data-testid='status-grounds'] [data-ground]")) {
      const bg = bgOf(g);
      for (const el of g.querySelectorAll(".nx-status")) {
        const tone = [...el.classList].find((c) => c.startsWith("nx-status_"));
        out.push([`${g.dataset.ground}/${tone}/${el.textContent}`, ratio(getComputedStyle(el).color, bg)]);
      }
      const btn = g.querySelector(".slds-button_brand");
      if (btn) out.push(["brand button text", ratio(getComputedStyle(btn).color, getComputedStyle(btn).backgroundColor)]);
      const a = g.querySelector("a");
      if (a) out.push(["link on page", ratio(getComputedStyle(a).color, bg)]);
    }
    return out;
  });
  const low = rows.filter(([, r]) => r < 4.5);
  console.log(low.length ? `FAIL contrast < 4.5:1:\n  ${low.map(([n, r]) => `${n} ${r.toFixed(2)}`).join("\n  ")}` : `OK   contrast >= 4.5:1 on ${rows.length} measured pairs (min ${Math.min(...rows.map(([, r]) => r)).toFixed(2)})`);
  if (low.length || rows.length < 20) { failed = true; if (rows.length < 20) console.log(`FAIL only ${rows.length} contrast pairs measured`); }
} finally {
  await browser.close();
}
process.exit(failed ? 1 : 0);
