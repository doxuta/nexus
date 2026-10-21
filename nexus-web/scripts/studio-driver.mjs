// What the browser scripts share to drive Nexus the way a person does: demo-loop.mjs (the product loop, replayed in CI)
// and builder-bench.mjs (the eval of the AI builder, issue #4). Labels come from lib/i18n/vi.ts by key; each persona signs
// in through the real API, in a browser context of its own; the API is only read, to learn ids and to check what the
// screen claims.
import fs from "node:fs";
import path from "node:path";

// The system Chrome (puppeteer-core downloads none): CHROME_PATH, else the macOS app, else the first one on PATH.
export function findChrome() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH;
  const dirs = (process.env.PATH ?? "").split(path.delimiter).filter(Boolean);
  return ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    ...["google-chrome", "chromium", "chromium-browser"].flatMap((name) => dirs.map((d) => path.join(d, name)))]
    .find((p) => fs.existsSync(p)) ?? null;
}

// Labels come from the UI's own catalog by key (the UI renders t(key) in its default locale, Vietnamese), so a reworded
// label cannot silently break a selector.
const VI = Object.fromEntries(
  [...fs.readFileSync(new URL("../lib/i18n/vi.ts", import.meta.url), "utf8").matchAll(/^\s*"([\w.]+)":\s*("(?:[^"\\]|\\.)*"),?\s*$/gm)]
    .map(([, k, v]) => [k, JSON.parse(v)]),
);
export function t(key, vars = {}) {
  if (!(key in VI)) throw new Error(`lib/i18n/vi.ts has no key ${key}`);
  return VI[key].replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}
export const attr = (v) => JSON.stringify(v); // a CSS attribute value

export const ASSISTANT = '[data-testid="assistant-dock"]';

/** One running stack: `base` is the web origin, the API answers at `${base}/api/v1`. `api()` reads as `asAdmin()`'s persona. */
export function connect({ base, tenant, browser }) {
  const API = `${base}/api/v1`;
  const errors = new Map(); // page → its page errors, printed when a step fails
  let adminToken;

  async function login({ email, password }) {
    const r = await fetch(`${API}/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ tenant, email, password }),
    });
    const j = await r.json();
    if (!j.data?.accessToken) throw new Error(`login ${email}: ${JSON.stringify(j)}`);
    return j.data;
  }
  async function api(p, init = {}) {
    const r = await fetch(`${API}${p}`, { ...init, headers: { Authorization: `Bearer ${adminToken}`, "Content-Type": "application/json", ...init.headers } });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(`${init.method ?? "GET"} ${p}: ${r.status} ${JSON.stringify(j).slice(0, 300)}`);
    return j;
  }
  const headVersion = async () => (await api("/admin/publish/state")).data.headVersion?.versionNo ?? 0;
  const drafts = async () => (await api("/admin/publish/changes")).data ?? [];

  // Signs in through the real API and seeds the session the way lib/api.ts login() stores it (remember-me →
  // localStorage), in a browser context of its own, so the builder and the rep never share storage (same as ui-shots.mjs).
  async function signIn(persona) {
    const s = await login(persona);
    const ctx = await browser.createBrowserContext();
    const page = await ctx.newPage();
    page.setDefaultTimeout(30_000);
    errors.set(page, []);
    page.on("pageerror", (err) => errors.get(page).push(String(err.message ?? err)));
    await page.evaluateOnNewDocument((s, tenant) => {
      localStorage.setItem("nexus_remember", "1");
      localStorage.setItem("nexus_token", s.accessToken);
      if (s.refreshToken) localStorage.setItem("nexus_refresh", s.refreshToken);
      if (s.user) localStorage.setItem("nexus_user", JSON.stringify(s.user));
      localStorage.setItem("nexus_tenant", tenant);
    }, s, tenant);
    return page;
  }

  const go = (page, p) => page.goto(`${base}${p}`, { waitUntil: "networkidle2", timeout: 60_000 });
  const studio = (p) => `/studio/du-an/${tenant}/${p}`;

  /** Header → Xuất bản → the review lists the drafts → Xuất bản; returns the new version number. Without `route`, from the
   *  page already open: the header must have learned of the drafts by itself (#17), not from a reload. */
  async function publishFromHeader(page, route) {
    const before = await headVersion();
    if ((await drafts()).length === 0) throw new Error("nothing to publish: no drafts");
    if (route) await go(page, route);
    await (await button(page, t("studio.publish.open"), '[data-testid="project-header"]')).click();
    await page.waitForSelector('[data-testid="publish-submit"]:not([disabled])');
    await waitText(page, t("studio.publish.count", { n: (await drafts()).length }), '[role="dialog"]');
    return before + 1;
  }
  async function submitPublish(page, want) {
    await page.click('[data-testid="publish-submit"]');
    await waitText(page, t("studio.publish.status.live", { n: want }), '[data-testid="project-header"]');
    const now = await headVersion();
    if (now !== want) throw new Error(`the server's head version is ${now}; want ${want}`);
  }

  return {
    API, errors, login, api, headVersion, drafts, signIn, go, studio, publishFromHeader, submitPublish,
    async asAdmin(persona) {
      adminToken = (await login(persona)).accessToken;
    },
  };
}

/** The first enabled button under `scope` whose text is exactly `text` (waits for it). */
export async function button(page, text, scope = "body") {
  const h = await page.waitForFunction(
    (scope, text) => [...document.querySelectorAll(`${scope} button`)].find((b) => b.textContent.trim() === text && !b.disabled && b.getAttribute("aria-disabled") !== "true"),
    {},
    scope,
    text,
  );
  return h.asElement();
}
export const waitText = (page, text, scope = "body") =>
  page.waitForFunction((scope, text) => document.querySelector(scope)?.innerText.includes(text), {}, scope, text);
export const waitPath = (page, p) => page.waitForFunction((p) => location.pathname === p, {}, p);

/** The assistant's cards: title, state (from the aria-label "<state>: <title>") and the visible text of each. */
export const cards = (page) =>
  page.$$eval(`${ASSISTANT} article`, (arts) => arts.map((a) => ({
    title: a.querySelector("h3")?.textContent.trim() ?? "",
    label: a.getAttribute("aria-label") ?? "",
    text: a.innerText,
  })));
/** Clicks the button labelled `text` inside card `i`. */
export async function clickInCard(page, i, text) {
  const h = await page.waitForFunction(
    (scope, i, text) => [...(document.querySelectorAll(`${scope} article`)[i]?.querySelectorAll("button") ?? [])]
      .find((b) => b.textContent.trim() === text && !b.disabled),
    {}, ASSISTANT, i, text,
  );
  await h.asElement().click();
}
/** Waits until card `i` leaves the open/applying states; it must be fully applied ("Áp dụng một phần" fails too). */
export async function waitApplied(page, i) {
  const state = await page.waitForFunction(
    (scope, i, done, open, applying) => {
      const a = document.querySelectorAll(`${scope} article`)[i];
      const l = a?.getAttribute("aria-label") ?? "";
      if (l.startsWith(`${open}: `) || l.startsWith(`${applying}: `)) return false;
      return l.startsWith(`${done}: `) ? "done" : `not applied: ${l} — ${a?.innerText.slice(-200)}`;
    },
    { timeout: 120_000, polling: 300 },
    ASSISTANT, i, t("studio.assistant.card.state.done"), t("studio.assistant.card.state.open"), t("studio.assistant.card.state.applying"),
  );
  const got = await state.jsonValue();
  if (got !== "done") throw new Error(got);
}

/** The app's menu tab for `entity` (the context bar), clicked as a person does (never a typed URL). */
export async function openFromMenu(page, entity) {
  const tab = await page.waitForFunction(
    (label) => [...document.querySelectorAll(".slds-context-bar__secondary a")].find((a) => a.textContent.trim().endsWith(label)),
    {}, entity.label,
  );
  await tab.asElement().click();
  await waitPath(page, `/${entity.name}`);
  await page.waitForSelector("main table thead th");
}
