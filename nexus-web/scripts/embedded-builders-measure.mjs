#!/usr/bin/env node
// specs/073 T099 (u0-nhung-b): measure every Studio builder INSIDE <EmbeddedSurface> in real Chrome, in a
// container whose height is its content (page scroll), which is what the Studio work surface will be.
// Per builder: the single-surface rule counts (same selectors as test/studio-embedded-registry.tsx), the height
// of the surface and of the canvas/columns, nested scrollers (two scroll bars), and the count of Nexus Be tokens in
// its source (the PAIRING.md "nhúng tạm" input, spec Nợ 14).
//
//   NEXUS_FE=http://localhost:3607 NEXUS_API=http://localhost:8607/api/v1 node scripts/embedded-builders-measure.mjs [id ...]
import puppeteer from "puppeteer-core";
import { readFileSync } from "node:fs";

const API = process.env.NEXUS_API ?? "http://localhost:8090/api/v1";
const FE = process.env.NEXUS_FE ?? "http://localhost:3000";
const CHROME = process.env.CHROME_PATH ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const VIEW_H = 900;

const RULES = {
  card: ".slds-card",
  h1: "h1",
  pageHeader: ".slds-page-header",
  iconTile: "[class*='slds-icon-standard-']",
  tablist: "[role='tablist']",
  dockedPanel: ".slds-panel_docked",
  badge: ".slds-badge",
  avatar: ".slds-avatar",
  tabsChrome: ".slds-tabs_default, .slds-tabs_scoped",
  illustration: ".slds-illustration",
};

const SOURCES = {
  WorkflowBuilder: ["components/studio/WorkflowBuilder.tsx"],
  BrandingScreen: ["components/studio/BrandingScreen.tsx"],
  UserList: ["components/screens/admin/UserList.tsx"],
  AuditLog: ["components/screens/admin/AuditLog.tsx"],
  DoctorScreen: ["components/screens/admin/DoctorScreen.tsx"],
  TeamsList: ["components/screens/admin/TeamsList.tsx"],
  ImportScreen: ["components/screens/ImportScreen.tsx"],
  EntityFieldEditor: ["components/studio/EntityFieldEditor.tsx"],
  DataModelBuilder: ["components/studio/DataModelBuilder.tsx"],
  LayoutBuilder: ["components/studio/LayoutBuilder.tsx"],
};

// Same pattern as test/studio-style-gate.test.ts (nexusBeToken).
const TW = "(?:bg|text|border|ring|fill|stroke|from|to|via|divide|outline|decoration|placeholder|accent|caret|shadow)";
const BE = new RegExp(`(?<![\\w-])${TW}-(?:bg|panel2?|bd[23]?|fg|mut|fnt|ac-on|ac-hi|ac2|acf|acd|acb|ac|ok[fdb]?|cyd|cy|wn[fd]?|dg[fdb]?|neu)(?![\\w-])`, "g");
const beTokens = (files) => files.reduce((n, f) => n + (readFileSync(f, "utf8").match(BE) ?? []).length, 0);

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

const sess = await login("admin@example.com", "Demo-Admin-2026!");
const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
const ctx = await browser.createBrowserContext();
const page = await ctx.newPage();
await page.setViewport({ width: 1440, height: VIEW_H });
await page.evaluateOnNewDocument((s) => {
  localStorage.setItem("nexus_remember", "1");
  localStorage.setItem("nexus_token", s.accessToken);
  if (s.refreshToken) localStorage.setItem("nexus_refresh", s.refreshToken);
  if (s.user) localStorage.setItem("nexus_user", JSON.stringify(s.user));
  localStorage.setItem("nexus_tenant", "northwind");
}, sess);

const ids = process.argv.slice(2).length ? process.argv.slice(2) : Object.keys(SOURCES);
const out = [];
for (const id of ids) {
  await page.goto(`${FE}/dev/embedded?b=${id}&e=crm_accounts`, { waitUntil: "networkidle2", timeout: 120000 }).catch(() => {});
  await new Promise((r) => setTimeout(r, 2500));
  const m = await page.evaluate((RULES, VIEW_H) => {
    const root = document.getElementById("nx-embedded-surface");
    if (!root) return { error: "no surface: " + document.body.innerText.slice(0, 120) };
    const counts = Object.fromEntries(Object.entries(RULES).map(([k, s]) => [k, root.querySelectorAll(s).length]));
    const rect = root.getBoundingClientRect();
    const scrollers = [...root.querySelectorAll("*")].filter((el) => {
      const cs = getComputedStyle(el);
      return /(auto|scroll)/.test(cs.overflowY) && el.scrollHeight > el.clientHeight + 1 && el.clientHeight > 0;
    });
    const canvas = root.querySelector(".react-flow");
    const collapsed = [...root.querySelectorAll(".react-flow, [class*='min-h-0']")].filter((el) => el.getBoundingClientRect().height < 40 && el.children.length > 0);
    return {
      counts,
      surfaceH: Math.round(rect.height),
      docScrollH: document.documentElement.scrollHeight,
      canvasH: canvas ? Math.round(canvas.getBoundingClientRect().height) : null,
      collapsed: collapsed.length,
      nestedScrollers: scrollers.length,
      text: root.innerText.slice(0, 60).replace(/\s+/g, " "),
      viewH: VIEW_H,
    };
  }, RULES, VIEW_H);
  const heightVerdict = m.error ? "n/a" : m.canvasH !== null && m.canvasH < 120 ? "sụp về 0" : m.collapsed > 0 ? "sụp về 0" : m.nestedScrollers > 0 && m.docScrollH > m.viewH + 1 ? "hai thanh cuộn lồng" : "đạt";
  out.push({ id, ...m, heightVerdict, nexusBeTokens: beTokens(SOURCES[id] ?? []) });
  await page.screenshot({ path: `/private/tmp/claude-501/embedded-${id}.png`, fullPage: false });
}
await browser.close();
console.log(JSON.stringify(out, null, 1));
