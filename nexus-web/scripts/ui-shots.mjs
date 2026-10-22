#!/usr/bin/env node
// Screenshot every nexus-web route as the right persona, for the SLDS migration's before/after
// evidence (loop/CAMPAIGN-XUONG-HOAN-CHINH-2026-09-28.md §7).
//
//   node scripts/ui-shots.mjs <out-dir> [route-filter-substring] [routes.json] [--widths 1440,1024,390,375]
//
// --widths (default 1440,390): one PNG per route per width, named <route>-<width>.png. Besides
// manifest.json it writes report.json (page errors + horizontal overflow per shot).
//
// routes.json (optional) replaces the built-in route list: [["/path?query", "rep"|"admin"|null], …].
//
// Needs the API on :8090 and the FE on :3000. Uses the system Chrome (puppeteer-core, no browser
// download). Personas log in through the real API and the session is seeded exactly the way
// lib/api.ts login() stores it (remember-me → localStorage). Dynamic ids are resolved from the API
// at run time, so the manifest never goes stale when data is reseeded.
import fs from "node:fs";
import path from "node:path";
import puppeteer from "puppeteer-core";

const API = process.env.NEXUS_API ?? "http://localhost:8090/api/v1";
const FE = process.env.NEXUS_FE ?? "http://localhost:3000";
const CHROME = process.env.CHROME_PATH ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const TENANT = "northwind";
const PERSONAS = {
  // Customer face: a non-admin, because is_admin bypasses every ACL (campaign §5).
  rep: { email: "test@example.com", password: "Demo-Rep-2026!" },
  admin: { email: "admin@example.com", password: "Demo-Admin-2026!" },
};

const argv = process.argv.slice(2);
const wi = argv.indexOf("--widths");
const WIDTHS = (wi >= 0 ? (argv.splice(wi, 2)[1] ?? "") : "1440,390").split(",").map(Number).filter((n) => n > 0);
const [outDir, filter = "", routesFile] = argv;
if (!outDir) {
  console.error("usage: node scripts/ui-shots.mjs <out-dir> [route-filter] [routes.json] [--widths 1440,390]");
  process.exit(2);
}

async function login({ email, password }) {
  const r = await fetch(`${API}/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant: TENANT, email, password }),
  });
  const j = await r.json();
  if (!j.data?.accessToken) throw new Error(`login ${email}: ${JSON.stringify(j)}`);
  return j.data;
}

async function apiGet(tok, p) {
  const r = await fetch(`${API}${p}`, { headers: { Authorization: `Bearer ${tok}` } });
  return r.ok ? r.json() : null;
}

async function resolveIds(tok) {
  const firstId = async (entity) => (await apiGet(tok, `/${entity}?limit=1`))?.data?.[0]?.id ?? "missing";
  const dash = await apiGet(tok, "/dashboards");
  const rep = await apiGet(tok, "/reports");
  const apps = await apiGet(tok, "/apps");
  const appr = await apiGet(tok, "/approvals/pending");
  return {
    entity: "crm_accounts",
    record: await firstId("crm_accounts"),
    dashboard: dash?.data?.dashboards?.[0]?.id ?? "missing",
    report: rep?.data?.reports?.[0]?.id ?? "missing",
    app: apps?.data?.apps?.[0]?.name ?? "crm",
    approval: appr?.data?.[0]?.id ?? appr?.data?.items?.[0]?.id ?? "missing",
  };
}

// [route, persona]. Customer face + auth as the rep; Admin/Studio/dev as the admin.
function routes(ids) {
  const e = ids.entity;
  return [
    ["/", null], ["/login", null], ["/forgot-password", null], ["/reset-password", null],
    ["/auth/sso/complete", null],
    [`/${e}`, "rep"], [`/${e}/${ids.record}`, "rep"], [`/${e}/${ids.record}/edit`, "rep"],
    [`/${e}/new`, "rep"], [`/${e}/batch`, "rep"], [`/${e}/import`, "rep"], [`/${e}/deleted`, "rep"],
    ["/home", "rep"], ["/about", "rep"], ["/entities", "rep"], ["/notifications", "rep"],
    ["/profile", "rep"], ["/settings", "rep"],
    ["/approvals", "rep"], [`/approvals/${ids.approval}`, "rep"],
    ["/dashboards", "rep"], [`/dashboards/${ids.dashboard}`, "rep"],
    ["/reports", "rep"], [`/reports/${ids.report}`, "rep"],
    ["/admin", "admin"], ["/admin/users", "admin"], ["/admin/roles", "admin"], ["/admin/teams", "admin"],
    ["/admin/sharing", "admin"], ["/admin/field-acl", "admin"],
    ["/admin/audit", "admin"], ["/admin/business-calendars", "admin"],
    ["/admin/sso", "admin"],
    // The builder's pages live under /studio/du-an/<tenant>/ (lib/studioRoutes.ts is the table; mounted pages only).
    ["/studio", "admin"],
    ...["dung/du-lieu", "dung/du-lieu/so-do", "dung/man-hinh", "dung/man-hinh/app", "dung/tu-dong",
      "dung/tu-dong/duyet", "dung/tu-dong/hang-doi", "dung/tu-dong/cong-nhan", "dung/tu-dong/lich-lam-viec",
      "van-hanh/tu-dong", "van-hanh/tu-dong/lan-chay", "van-hanh/tu-dong/viec-nen", "van-hanh/tu-dong/cong-nhan",
      "van-hanh/tu-dong/khoa-truy-cap", "bao-tri/kham"]
      .map((page) => [`/studio/du-an/${TENANT}/${page}`, "admin"]),
    ["/studio/moi", "admin"], ["/studio/tai-khoan", "admin"], ["/studio/verticals", "admin"],
    ["/dev/kit", "admin"],
  ];
}

const slug = (r) => (r === "/" ? "root" : r.slice(1).replace(/[^a-zA-Z0-9]+/g, "_")).slice(0, 100);

const sessions = {};
for (const [k, p] of Object.entries(PERSONAS)) sessions[k] = await login(p);
const ids = await resolveIds(sessions.admin.accessToken);
fs.mkdirSync(outDir, { recursive: true });

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, defaultViewport: { width: 1440, height: 900 } });
const manifest = [];
try {
  // routes.json entries may use {entity} {record} {app} {dashboard} {report} {approval}, resolved from the API.
  const list = routesFile
    ? JSON.parse(fs.readFileSync(routesFile, "utf8")).map(([r, p]) => [r.replace(/\{(\w+)\}/g, (m, k) => { if (!(k in ids)) throw new Error(`routes.json: unknown placeholder ${m}`); return ids[k]; }), p])
    : routes(ids);
  for (const [route, persona] of list) {
    if (filter && !route.includes(filter)) continue;
    for (const width of WIDTHS) {
      // A fresh context per shot: the session is re-seeded and no state leaks between widths.
      const errors = [];
      const ctx = await browser.createBrowserContext();
      const page = await ctx.newPage();
      page.on("pageerror", (err) => errors.push(String(err.message ?? err)));
      // NEXUS_LANG=en shoots the English UI; the default is Vietnamese (headless Chrome lists only English, which the
      // UI would otherwise follow: getLocale in lib/i18n).
      await page.evaluateOnNewDocument((locale) => localStorage.setItem("nexus.prefs.v1", JSON.stringify({ locale })), process.env.NEXUS_LANG === "en" ? "en" : "vi");
      if (!persona) {
        // Anonymous routes: demo mode (NEXT_PUBLIC_DEMO_MODE) would auto-login and bounce /login to
        // /home. The logout opt-out flag lib/api.ts honours keeps the page anonymous.
        await page.evaluateOnNewDocument(() => localStorage.setItem("nexus_logged_out", "1"));
      }
      if (persona) {
        const s = sessions[persona];
        // Seed storage on the FE origin before the app boots, mirroring lib/api.ts login().
        await page.evaluateOnNewDocument((s, tenant) => {
          localStorage.setItem("nexus_remember", "1");
          localStorage.setItem("nexus_token", s.accessToken);
          if (s.refreshToken) localStorage.setItem("nexus_refresh", s.refreshToken);
          if (s.user) localStorage.setItem("nexus_user", JSON.stringify(s.user));
          localStorage.setItem("nexus_tenant", tenant);
        }, s, TENANT);
      }
      await page.setViewport({ width, height: width < 700 ? 844 : 900 });
      let status = 0;
      try {
        const res = await page.goto(`${FE}${route}`, { waitUntil: "networkidle2", timeout: 60000 });
        status = res?.status() ?? 0;
      } catch (err) {
        errors.push(`goto: ${err.message}`);
      }
      await new Promise((r) => setTimeout(r, 800)); // let client-side queries paint
      const file = path.join(outDir, `${slug(route)}-${width}.png`);
      await page.screenshot({ path: file });
      const overflow = await page.evaluate(() => document.scrollingElement.scrollWidth - innerWidth).catch(() => null);
      const u = new URL(page.url());
      const finalPath = u.pathname + u.search;
      manifest.push({ route, persona, width, status, finalPath, file: path.basename(file), overflowPx: overflow, errors: [...errors] });
      await ctx.close();
      console.log(`${status} ${persona ?? "anon"} ${route} @${width}${finalPath !== route ? ` → ${finalPath}` : ""}${errors.length ? ` (${errors.length} err)` : ""}${overflow > 0 ? ` overflow+${overflow}` : ""}`);
    }
  }
} finally {
  await browser.close();
  writeReports();
}
// A shot with a page error (an uncaught error, a hydration mismatch) fails the run (#6), as the verify-ui skill says.
const broken = manifest.filter((m) => m.errors.length > 0);
if (broken.length) {
  for (const m of broken) console.error(`FAIL ${m.route} @${m.width}: ${m.errors.join(" | ")}`);
  process.exit(1);
}
function writeReports() {
fs.writeFileSync(path.join(outDir, "report.json"), JSON.stringify(manifest.map(({ route, width, status, overflowPx, errors }) => ({ route, width, status, overflowPx, errors })), null, 2));
fs.writeFileSync(path.join(outDir, "manifest.json"), JSON.stringify({ takenAt: new Date().toISOString(), ids, shots: manifest }, null, 2));
}
