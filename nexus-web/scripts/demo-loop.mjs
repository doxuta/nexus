#!/usr/bin/env node
// The whole product loop in a real browser (issues #1 and #2): the builder asks the assistant for a helpdesk app and says
// who will use it; the assistant proposes the table, the app and the sales reps' access; the builder previews the cards
// (the table on the Data page, the access as ghost cells on the access matrix), applies them and publishes once; the rep
// opens Tickets from the app switcher, creates a ticket, sees the builder's too, edits their own and is refused the
// builder's; the builder changes the list layout, publishes it and rolls back, and the rep sees the previous layout again.
//
//   node scripts/demo-loop.mjs <out-dir>          (from nexus-web/, against a FRESH one-command demo)
//
// One screenshot per step into <out-dir>; the first failing step prints its name and the script exits 1.
// NEXUS_PORT as for docker compose (default 8080), or NEXUS_URL · CHROME_PATH (default: the macOS app, else
// google-chrome / chromium / chromium-browser on PATH).
//
// Without a model or key the assistant answers from the committed cassette:
//   AI_ENABLED=true AI_GATEWAY_MODE=replay AI_GATEWAY_CASSETTE=/cassettes/demo-loop.jsonl docker compose up --build
// A replayed stack answers ONE run: for another, `docker compose down -v`, then up again. Re-record only when the
// steps change, on a fresh stack with AI_GATEWAY_MODE=record plus a live AI_GATEWAY_BASE_URL / AI_GATEWAY_API_KEY:
// replay checks every call against the recording (tools offered, the prompt, the shape of the conversation), so a
// changed prompt or tool list fails as a stale cassette instead of answering with another turn's reply.
import fs from "node:fs";
import path from "node:path";
import puppeteer from "puppeteer-core";

const BASE = (process.env.NEXUS_URL ?? `http://localhost:${process.env.NEXUS_PORT || 8080}`).replace(/\/$/, "");
const API = `${BASE}/api/v1`;
const TENANT = "northwind";
const PERSONAS = {
  builder: { email: "admin@example.com", password: "Demo-Admin-2026!" },
  // A non-admin: is_admin bypasses every ACL, so only the rep proves the access rules.
  rep: { email: "test@example.com", password: "Demo-Rep-2026!" },
};
// Fixed: the cassette is keyed on it. It says who will use the app, so the assistant proposes the access too (#2).
const PROMPT =
  "Build a small helpdesk app: a Tickets table with a title, a description, a priority (low/medium/high) and a status " +
  "(open/closed), and a menu entry for it. Our sales reps (Nhân viên kinh doanh) can see all tickets, create them, and " +
  "edit their own; nobody but managers deletes.";
const REP_ROLE = "crm_sales_rep"; // the role the seed gives the rep: "Nhân viên kinh doanh"
const TABLE = "tickets"; // what the recorded proposal builds: a stack that already has it has run the loop before
const TICKET = "Printer on floor 2 is jammed"; // the rep's
const TICKET_EDITED = `${TICKET} (toner replaced)`;
const BUILDER_TICKET = "Projector in room 3 flickers"; // the builder's: the rep sees it and may not edit it
const AI_TIMEOUT = 10 * 60_000; // a live model may take minutes; a replay answers at once

const outDir = process.argv[2];
if (!outDir) {
  console.error("usage: node scripts/demo-loop.mjs <out-dir>");
  process.exit(2);
}

// The system Chrome (puppeteer-core downloads none): CHROME_PATH, else the macOS app, else the first one on PATH.
function findChrome() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH;
  const dirs = (process.env.PATH ?? "").split(path.delimiter).filter(Boolean);
  return ["/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    ...["google-chrome", "chromium", "chromium-browser"].flatMap((name) => dirs.map((d) => path.join(d, name)))]
    .find((p) => fs.existsSync(p)) ?? null;
}
const CHROME = findChrome();
if (!CHROME) {
  console.error("FAIL setup: no Chrome found — set CHROME_PATH to a Chrome or Chromium binary");
  process.exit(1);
}

// Labels come from the UI's own catalog by key (the UI renders t(key) in its default locale, Vietnamese), so a reworded
// label cannot silently break a selector.
const VI = Object.fromEntries(
  [...fs.readFileSync(new URL("../lib/i18n/vi.ts", import.meta.url), "utf8").matchAll(/^\s*"([\w.]+)":\s*("(?:[^"\\]|\\.)*"),?\s*$/gm)]
    .map(([, k, v]) => [k, JSON.parse(v)]),
);
function t(key, vars = {}) {
  if (!(key in VI)) throw new Error(`lib/i18n/vi.ts has no key ${key}`);
  return VI[key].replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}
const attr = (v) => JSON.stringify(v); // a CSS attribute value

// ── API reads (to learn ids and names, and to check what the screen claims); every action goes through the browser ──
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
let adminToken;
async function api(p, init = {}) {
  const r = await fetch(`${API}${p}`, { ...init, headers: { Authorization: `Bearer ${adminToken}`, "Content-Type": "application/json", ...init.headers } });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(`${init.method ?? "GET"} ${p}: ${r.status} ${JSON.stringify(j).slice(0, 300)}`);
  return j;
}
const headVersion = async () => (await api("/admin/publish/state")).data.headVersion?.versionNo ?? 0;
const drafts = async () => (await api("/admin/publish/changes")).data ?? [];

// The cassette answers one run, and the run builds the table it then uses: refuse a stack that has been used.
async function preflight() {
  const why = [];
  if (((await api("/metadata/entities")).data ?? []).some((e) => e.name === TABLE)) why.push(`a ${TABLE} table already exists`);
  const pending = (await drafts()).length;
  if (pending > 0) why.push(`${pending} draft change(s) are pending`);
  if (why.length) throw new Error(`needs a fresh stack: docker compose down -v, then up again (${why.join("; ")})`);
}

// ── browser ──
const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  defaultViewport: { width: 1440, height: 900 },
  args: process.env.CI ? ["--no-sandbox"] : [], // CI runners only: the sandbox needs user namespaces they may not grant
});
const errors = new Map(); // page → its page errors, printed when a step fails

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
  }, s, TENANT);
  return page;
}

const go = (page, p) => page.goto(`${BASE}${p}`, { waitUntil: "networkidle2", timeout: 60_000 });
const studio = (p) => `/studio/du-an/${TENANT}/${p}`;

/** The first enabled button under `scope` whose text is exactly `text` (waits for it). */
async function button(page, text, scope = "body") {
  const h = await page.waitForFunction(
    (scope, text) => [...document.querySelectorAll(`${scope} button`)].find((b) => b.textContent.trim() === text && !b.disabled && b.getAttribute("aria-disabled") !== "true"),
    {},
    scope,
    text,
  );
  return h.asElement();
}
const waitText = (page, text, scope = "body") =>
  page.waitForFunction((scope, text) => document.querySelector(scope)?.innerText.includes(text), {}, scope, text);
const waitPath = (page, p) => page.waitForFunction((p) => location.pathname === p, {}, p);

/** Header → Xuất bản → the review lists the drafts → Xuất bản; returns the new version number. */
async function publishFromHeader(page, route) {
  const before = await headVersion();
  if ((await drafts()).length === 0) throw new Error("nothing to publish: no drafts");
  await go(page, route); // the header reads the publish state on load
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

/** "Tạo mới" on the list, as `page`'s user: every required text gets `title`, every required pick list its first value. */
async function createTicket(page, title, { onList = false } = {}) {
  if (!onList) await go(page, `/${entity.name}`);
  await (await button(page, t("page.list.new"), "main")).click();
  const form = '[role="dialog"]';
  await page.waitForSelector(`${form} form`);
  const texts = await page.$$(`${form} input[type="text"][required]:not([role="combobox"])`);
  if (texts.length === 0) throw new Error("the create form has no required text field for the title");
  for (const input of texts) await input.type(title);
  const picks = await page.$$eval(`${form} [role="combobox"][aria-labelledby]`, (cs) =>
    cs.map((c) => c.id).filter((id, i) => document.getElementById(cs[i].getAttribute("aria-labelledby"))?.textContent.trim().startsWith("*")));
  for (const id of picks) {
    await page.click(`[id=${attr(id)}]`);
    const option = await page.waitForSelector('[role="option"]:not([aria-disabled="true"])', { visible: true });
    await option.click();
  }
  await page.click(`${form} button[type="submit"]`);
  await page.waitForFunction((name) => new RegExp(`^/${name}/[0-9a-f-]{36}$`).test(location.pathname), {}, entity.name);
  return new URL(page.url()).pathname.split("/").pop();
}

/** A request on one record, sent from `page` with its user's own token: "<status>", or "<status> <error code>". */
const requestAs = (page, method, id, body) =>
  page.evaluate(async (method, url, body) => {
    const r = await fetch(url, {
      method,
      headers: { Authorization: `Bearer ${localStorage.getItem("nexus_token")}`, ...(body ? { "Content-Type": "application/json" } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
    const code = r.ok ? "" : ((await r.json().catch(() => ({}))).code ?? "");
    return code ? `${r.status} ${code}` : String(r.status);
  }, method, `${API}/${entity.name}/${id}`, body ?? null);

/** The column headers the rep's list shows. */
const listColumns = (page) =>
  page.$$eval("main table thead th", (ths) => ths.map((th) => th.textContent.trim()).filter(Boolean));
async function repList(page) {
  await go(page, `/${entity.name}`);
  await page.waitForSelector("main table thead th");
  return listColumns(page);
}

/** The assistant's cards: title, state (from the aria-label "<state>: <title>") and the visible text of each. */
const cards = (page) =>
  page.$$eval(`${assistant} article`, (arts) => arts.map((a) => ({
    title: a.querySelector("h3")?.textContent.trim() ?? "",
    label: a.getAttribute("aria-label") ?? "",
    text: a.innerText,
  })));
/** The index of the access card: the one whose "where" line is Người dùng & quyền (fails the step when there is none). */
async function accessCard(page) {
  const all = await cards(page);
  const i = all.findIndex((c) => c.text.split("\n").some((l) => l.trim() === t("studio.assistant.where.access")));
  if (i < 0) throw new Error(`no access card: ${JSON.stringify(all.map((c) => c.title))}`);
  return i;
}
/** Clicks the button labelled `text` inside card `i`. */
async function clickInCard(page, i, text) {
  const h = await page.waitForFunction(
    (scope, i, text) => [...(document.querySelectorAll(`${scope} article`)[i]?.querySelectorAll("button") ?? [])]
      .find((b) => b.textContent.trim() === text && !b.disabled),
    {}, assistant, i, text,
  );
  await h.asElement().click();
}
/** Waits until card `i` leaves the open/applying states; it must be fully applied ("Áp dụng một phần" fails too). */
async function waitApplied(page, i) {
  const state = await page.waitForFunction(
    (scope, i, done, open, applying) => {
      const a = document.querySelectorAll(`${scope} article`)[i];
      const l = a?.getAttribute("aria-label") ?? "";
      if (l.startsWith(`${open}: `) || l.startsWith(`${applying}: `)) return false;
      return l.startsWith(`${done}: `) ? "done" : `not applied: ${l} — ${a?.innerText.slice(-200)}`;
    },
    { timeout: 120_000, polling: 300 },
    assistant, i, t("studio.assistant.card.state.done"), t("studio.assistant.card.state.open"), t("studio.assistant.card.state.applying"),
  );
  const got = await state.jsonValue();
  if (got !== "done") throw new Error(got);
}
/** The rep's grants on one table, as {view, create, edit, delete} → access level ("none" when absent). */
async function grantsOn(roleId, table) {
  const grants = (await api(`/admin/roles/${roleId}`)).data.role.actions.filter((a) => a.entityName === table);
  return Object.fromEntries(["view", "create", "edit", "delete"].map((a) => [a, grants.find((g) => g.action === a)?.accessLevel ?? "none"]));
}
/** The app's menu tab for the table (the context bar), clicked as a person does (never a typed URL). */
async function openFromMenu(page) {
  const tab = await page.waitForFunction(
    (label) => [...document.querySelectorAll(".slds-context-bar__secondary a")].find((a) => a.textContent.trim().endsWith(label)),
    {}, entity.label,
  );
  await tab.asElement().click();
  await waitPath(page, `/${entity.name}`);
  await page.waitForSelector("main table thead th");
}

// ── the loop ──
let builder, rep, entity, repRole, recordId, builderRecordId, titleField, hidden, publishedV2, app, crmBefore;
let active; // the page the current step works in: the runner screenshots it
const assistant = '[data-testid="assistant-dock"]';
const steps = [
  ["builder-asks-the-assistant", async () => {
    active = builder = await signIn(PERSONAS.builder);
    await go(builder, studio("dung/du-lieu"));
    const toggle = await builder.waitForSelector('[data-testid="assistant-toggle"]');
    if ((await toggle.evaluate((b) => b.getAttribute("aria-pressed"))) !== "true") await toggle.click();
    const box = await builder.waitForSelector(`${assistant} textarea:not([disabled])`, { visible: true });
    await box.type(PROMPT);
    await builder.keyboard.press("Enter");
    // Done when a proposal card is open; a reply without one, or an error in its place, fails the step.
    const outcome = await builder.waitForFunction(
      (scope, open, cancel) => {
        const dock = document.querySelector(scope);
        if (dock.querySelector(`article[aria-label^="${open}: "]`)) return "cards";
        const log = dock.querySelector('[role="log"]');
        const sending = [...dock.querySelectorAll("button")].some((b) => b.textContent.trim() === cancel);
        return !sending && log && log.children.length >= 2 ? `no proposal: ${log.innerText.slice(-400)}` : false;
      },
      { timeout: AI_TIMEOUT, polling: 500 },
      assistant, t("studio.assistant.card.state.open"), t("studio.assistant.cancel"),
    );
    const got = await outcome.jsonValue();
    if (got !== "cards") throw new Error(got);
    const all = await cards(builder);
    await accessCard(builder); // the access was proposed, not left for the builder to set by hand
    return `${all.length} proposal card(s): ${all.map((c) => c.title).join(" | ")}`;
  }],

  ["proposal-previewed", async () => {
    await (await button(builder, t("studio.assistant.preview.show"), assistant)).click();
    // The ghost is drawn on the page itself, not in the assistant.
    await builder.waitForFunction(
      (scope) => [...document.querySelectorAll(".nx-proposed")].some((e) => !e.closest(scope)),
      {},
      assistant,
    );
    return "the proposed table is drawn as a ghost row on the page";
  }],

  ["access-previewed-on-the-matrix", async () => {
    // The access card's preview opens Người dùng & quyền: the rep's role gets ghost cells on the proposed table (a ghost
    // column, as the table is not built yet), with the level the card leads to.
    const i = await accessCard(builder);
    const said = (await cards(builder))[i].title;
    await clickInCard(builder, i, t("studio.assistant.preview.show"));
    await waitPath(builder, studio("dung/nguoi-dung-quyen"));
    const note = t("studio.build.ai.access.cell", { level: t("studio.build.access.level.edit") });
    const cell = await builder.waitForFunction(
      (scope, note) => [...document.querySelectorAll("main td.nx-proposed")].find((td) => !td.closest(scope) && td.innerText.includes(note)),
      {}, assistant, note,
    );
    const row = await cell.asElement().evaluate((td) => td.closest("tr").querySelector("th a").textContent.trim());
    const col = await cell.asElement().evaluate((td) => td.closest("table").querySelectorAll("thead th")[td.cellIndex].innerText.split("\n")[0].trim());
    return `card: "${said}"; matrix: ${row} × ${col} → "${note}"`;
  }],

  ["proposals-applied", async () => {
    // "Áp dụng tất cả" applies what goes to drafts (the table, hidden from the customer until Publish); the app and the
    // access apply at once for real users, so each is asked in place: "Áp ngay", then "Áp ngay" again in the card.
    repRole = (await api("/admin/roles")).data.roles.find((r) => r.name === REP_ROLE);
    if (!repRole) throw new Error(`no role ${REP_ROLE}`);
    crmBefore = (await api(`/admin/roles/${repRole.id}`)).data.role.actions.filter((a) => a.entityName.startsWith("crm_"));
    const appsBefore = new Set((await api("/admin/apps")).data.apps.map((a) => a.name));
    const applyAll = t("studio.assistant.card.applyAll").split("{n}")[0];
    let bulk = null;
    for (const b of await builder.$$(`${assistant} button`)) if ((await b.evaluate((x) => x.textContent.trim())).startsWith(applyAll)) bulk = b;
    const all = await cards(builder);
    const table = all.findIndex((c) => c.title.startsWith(t("studio.assistant.view.entity").split("{name}")[0]));
    if (table < 0) throw new Error(`no table card: ${JSON.stringify(all.map((c) => c.title))}`);
    if (bulk) await bulk.click();
    else await clickInCard(builder, table, t("studio.assistant.card.apply"));
    await waitApplied(builder, table);
    const now = [];
    for (const [i, c] of (await cards(builder)).entries()) {
      if (!c.label.startsWith(`${t("studio.assistant.card.state.open")}: `)) continue;
      await clickInCard(builder, i, t("studio.assistant.draft.applyNow"));
      // The question in the card: the app's "Áp ngay cho người dùng thật: …", the access card's own wording.
      await builder.waitForFunction((scope, i, asks) => asks.some((ask) => document.querySelectorAll(`${scope} article`)[i]?.innerText.includes(ask)),
        {}, assistant, i, ["studio.assistant.draft.confirmNow", "studio.assistant.access.confirmHidden"].map((k) => t(k).split("{desc}")[0]));
      await clickInCard(builder, i, t("studio.assistant.draft.confirmNowAction"));
      await waitApplied(builder, i);
      now.push(c.title);
    }
    // What landed: the new table among the drafts, the app, and the rep's role with the access the prompt asked for.
    const pending = await drafts();
    const created = pending.find((c) => c.docType === "entity" && c.op === "create");
    if (!created?.entityName) throw new Error(`no new table among the drafts: ${JSON.stringify(pending.map((c) => `${c.op} ${c.docType} ${c.label}`))}`);
    entity = { name: created.entityName, label: created.label };
    app = (await api("/admin/apps")).data.apps.find((a) => !appsBefore.has(a.name));
    if (!app) throw new Error("no new app");
    const want = { view: "all", create: "all", edit: "owner", delete: "none" };
    const got = await grantsOn(repRole.id, entity.name);
    if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`role ${REP_ROLE} on ${entity.name}: ${JSON.stringify(got)}; want ${JSON.stringify(want)}`);
    // Additive: the role's CRM grants are exactly as they were.
    const crmAfter = (await api(`/admin/roles/${repRole.id}`)).data.role.actions.filter((a) => a.entityName.startsWith("crm_"));
    const sig = (gs) => JSON.stringify(gs.map((g) => `${g.entityName}|${g.action}|${g.accessLevel}`).sort());
    if (sig(crmAfter) !== sig(crmBefore)) throw new Error(`the rep's CRM grants changed: ${sig(crmBefore)} → ${sig(crmAfter)}`);
    return `applied to drafts: table; applied now: ${now.join(" | ")}; ${repRole.label} (${REP_ROLE}) on ${entity.name}: view all, create, edit own, no delete; ${crmAfter.length} CRM grants untouched; drafts: ${pending.map((c) => `${c.op} ${c.docType} "${c.label}"`).join(", ")}`;
  }],

  ["publish-review", async () => {
    publishedV2 = await publishFromHeader(builder, studio("dung/du-lieu"));
    return `${(await drafts()).length} change(s) listed`;
  }],

  ["published", async () => {
    await submitPublish(builder, publishedV2);
    return `version ${publishedV2} is live`;
  }],

  ["builder-creates-a-ticket", async () => {
    // The builder's own ticket, filed from the app like anyone's: the rep will see it and must not be able to edit it.
    builderRecordId = await createTicket(builder, BUILDER_TICKET);
    return `created ${entity.name}/${builderRecordId} as ${PERSONAS.builder.email}`;
  }],

  ["rep-opens-the-app-switcher", async () => {
    // Not by URL: the app launcher lists the new app for the rep, as they can see one of its menu entries.
    active = rep = await signIn(PERSONAS.rep);
    await go(rep, "/home");
    await rep.click(`button[title=${attr(t("slds.appLauncher.open"))}]`);
    await rep.waitForFunction(
      (label) => [...document.querySelectorAll('[role="dialog"] .slds-app-launcher__tile a')].some((a) => a.textContent.trim() === label),
      {}, app.label,
    );
    const tiles = await rep.$$eval('[role="dialog"] .slds-app-launcher__tile a', (as) => as.map((a) => a.textContent.trim()));
    return `the launcher lists: ${tiles.join(" | ")}`;
  }],

  ["rep-opens-tickets-from-the-app-menu", async () => {
    const tile = await rep.waitForFunction(
      (label) => [...document.querySelectorAll('[role="dialog"] .slds-app-launcher__tile a')].find((a) => a.textContent.trim() === label),
      {}, app.label,
    );
    await tile.asElement().click();
    await openFromMenu(rep);
    await waitText(rep, BUILDER_TICKET, "main table tbody");
    return `"${app.label}" → "${entity.label}": the list shows "${BUILDER_TICKET}"`;
  }],

  ["rep-creates-a-ticket", async () => {
    recordId = await createTicket(rep, TICKET, { onList: true });
    return `created ${entity.name}/${recordId} as ${PERSONAS.rep.email}`;
  }],

  ["rep-sees-both-tickets", async () => {
    await openFromMenu(rep);
    for (const title of [TICKET, BUILDER_TICKET]) await waitText(rep, title, "main table tbody");
    return `"${TICKET}" (theirs) and "${BUILDER_TICKET}" (the builder's) are listed`;
  }],

  ["rep-edits-their-own-ticket", async () => {
    await go(rep, `/${entity.name}/${recordId}`);
    await (await button(rep, t("page.record.action.edit"), "main")).click();
    await waitPath(rep, `/${entity.name}/${recordId}/edit`);
    const input = await rep.waitForFunction((title) => [...document.querySelectorAll('main input[type="text"]')].find((i) => i.value === title), {}, TICKET);
    await input.asElement().evaluate((el) => el.select());
    await input.asElement().type(TICKET_EDITED);
    await rep.click('main button[type="submit"]');
    await waitPath(rep, `/${entity.name}/${recordId}`);
    await waitText(rep, TICKET_EDITED, "main");
    // The save sticks: the server holds the new title, in the field the form showed it in.
    const saved = (await api(`/${entity.name}/${recordId}`)).data;
    titleField = Object.keys(saved.data ?? saved).find((k) => (saved.data ?? saved)[k] === TICKET_EDITED);
    if (!titleField) throw new Error(`the server's record has no field holding "${TICKET_EDITED}": ${JSON.stringify(saved).slice(0, 300)}`);
    return `"${TICKET}" → "${TICKET_EDITED}", saved in ${titleField}`;
  }],

  ["rep-refused-on-the-builders-ticket", async () => {
    // Row level: the rep opens the builder's ticket; its page has no "Sửa" and says it belongs to someone else (the
    // misleading /edit form is #18), and the server refuses the very PUT it accepts on the rep's own ticket. Outside the
    // edit scope the server answers 404, as for a ticket that is not there (by design: LayoutDetail's outOfEditScope), so
    // the GET beside it shows that the ticket is there and readable. And no delete at all, not even of their own ticket.
    await go(rep, `/${entity.name}/${builderRecordId}`);
    await waitText(rep, t("page.record.editScope.readOnly"), "main");
    await waitText(rep, BUILDER_TICKET, "main");
    const edit = await rep.$$eval("main button, main a", (els, label) => els.filter((e) => e.textContent.trim() === label).length, t("page.record.action.edit"));
    if (edit > 0) throw new Error(`the builder's ticket offers the rep "${t("page.record.action.edit")}"`);
    const body = { [titleField]: TICKET_EDITED };
    const got = {
      "PUT own": await requestAs(rep, "PUT", recordId, body),
      "GET the builder's": await requestAs(rep, "GET", builderRecordId),
      "PUT the builder's": await requestAs(rep, "PUT", builderRecordId, body),
      "DELETE own": await requestAs(rep, "DELETE", recordId),
    };
    const want = { "PUT own": "200", "GET the builder's": "200", "PUT the builder's": "404 RECORD_NOT_FOUND", "DELETE own": "403 FORBIDDEN" };
    const said = Object.entries(got).map(([k, v]) => `${k} → ${v}`).join(", ");
    if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`as the rep: ${said}; want ${JSON.stringify(want)}`);
    const kept = (await api(`/${entity.name}/${builderRecordId}`)).data;
    if (JSON.stringify(kept).includes(TICKET_EDITED)) throw new Error("the builder's ticket was changed by the rep");
    return `"${t("page.record.editScope.readOnly")}"; ${said}`;
  }],

  ["builder-changes-the-list-layout", async () => {
    active = builder;
    // The small change: the list stops showing the priority column.
    const fields = (await api(`/metadata/fields/${entity.name}`)).data;
    const priority = fields.find((f) => /priorit/i.test(f.name) || /priorit|ưu tiên/i.test(f.label ?? ""));
    if (!priority) throw new Error(`${entity.name} has no priority field: ${fields.map((f) => f.name)}`);
    hidden = priority.label || priority.name;
    await go(builder, studio(`dung/man-hinh/${entity.name}?man=danh-sach`));
    const box = `input[aria-label=${attr(t("studio.build.screens.list.cols.show", { name: hidden }))}]`;
    const input = await builder.waitForSelector(`${box}:checked`);
    // The SLDS checkbox draws its own box over a hidden input: click its label, as a person does.
    await (await input.evaluateHandle((e) => e.labels[0])).asElement().click();
    await builder.waitForSelector(`${box}:not(:checked)`);
    await (await button(builder, t("studio.build.screens.bar.save"))).click();
    for (let i = 0; !(await drafts()).some((c) => c.docType === "layout"); i++) {
      if (i > 40) throw new Error("the saved list layout never reached the drafts");
      await new Promise((r) => setTimeout(r, 250));
    }
    return `column "${hidden}" hidden; drafts: ${(await drafts()).map((c) => `${c.op} ${c.docType} "${c.label}"`).join(", ")}`;
  }],

  ["layout-published", async () => {
    const want = await publishFromHeader(builder, studio(`dung/man-hinh/${entity.name}?man=danh-sach`));
    await submitPublish(builder, want);
    return `version ${want} is live`;
  }],

  ["rep-sees-the-new-layout", async () => {
    active = rep;
    const cols = await repList(rep);
    if (cols.some((c) => c.includes(hidden))) throw new Error(`the rep's list still shows "${hidden}": ${cols}`);
    return `columns: ${cols.join(" | ")}`;
  }],

  ["builder-rolls-back", async () => {
    active = builder;
    const before = await headVersion();
    await go(builder, studio("bao-tri/lich-su"));
    await builder.click(`button[aria-label=${attr(t("studio.maintain.rollback.submitFor", { n: publishedV2 }))}]`);
    // The plan names what goes back; the confirm unlocks once it is read.
    const confirm = await builder.waitForFunction(
      (label) => [...document.querySelectorAll("main button[aria-describedby]")].find((b) => b.textContent.trim() === label && !b.disabled),
      {},
      t("studio.maintain.rollback.submit"),
    );
    await confirm.asElement().click();
    await waitText(builder, t("studio.maintain.rollback.done", { n: publishedV2, m: before + 1 }));
    const state = (await api("/admin/publish/state")).data.headVersion;
    if (state?.versionNo !== before + 1 || state?.kind !== "rollback") throw new Error(`head is ${JSON.stringify(state)}; want rollback version ${before + 1}`);
    return `back to version ${publishedV2}, published as version ${before + 1}`;
  }],

  ["rep-sees-the-previous-layout", async () => {
    active = rep;
    const cols = await repList(rep);
    if (!cols.some((c) => c.includes(hidden))) throw new Error(`the rep's list lacks "${hidden}" after the rollback: ${cols}`);
    return `columns: ${cols.join(" | ")}`;
  }],
];

fs.mkdirSync(outDir, { recursive: true });
let failed = null;
try {
  adminToken = (await login(PERSONAS.builder)).accessToken;
  await preflight();
  for (const [i, [name, run]] of steps.entries()) {
    const file = path.join(outDir, `${String(i + 1).padStart(2, "0")}-${name}.png`);
    const t0 = Date.now();
    try {
      const note = await run();
      await active.screenshot({ path: file });
      console.log(`ok   ${String(i + 1).padStart(2)} ${name} (${((Date.now() - t0) / 1000).toFixed(1)}s) — ${note}`);
    } catch (err) {
      failed = name;
      await active?.screenshot({ path: file.replace(/\.png$/, "-FAILED.png") }).catch(() => {});
      console.error(`FAIL ${String(i + 1).padStart(2)} ${name}: ${err.message}`);
      for (const e of (active && errors.get(active)) ?? []) console.error(`     page error: ${e}`);
      break;
    }
  }
} catch (err) {
  failed = "setup";
  console.error(`FAIL setup: ${err.message}`);
} finally {
  await browser.close();
}
if (failed) process.exit(1);
console.log(`demo loop passed: ${steps.length} steps, screenshots in ${outDir}`);
