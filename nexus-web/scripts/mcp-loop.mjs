#!/usr/bin/env node
// The MCP loop in a real browser (issue #3): agents propose, humans dispose. The builder creates an agent key in the
// Studio ("Kết nối agent (MCP)"), a headless Claude Code session connects with the exact `claude mcp add` line the
// Studio shows and is asked for a Suppliers table; the proposal shows up as a card in the key's conversation, the
// builder applies it to drafts and it is published; the sales rep can use the table only once access is granted; the
// builder revokes the key and /mcp refuses it.
//
//   node scripts/mcp-loop.mjs <out-dir>          (from nexus-web/, against a FRESH one-command demo)
//
// A screenshot per step that shows something into <out-dir>, plus claude-transcript.jsonl (stream-json, nxm_ tokens
// redacted).
// NEXUS_PORT as for docker compose (default 8080), or NEXUS_URL (web origin) and NEXUS_API (API base) · CHROME_PATH ·
// CLAUDE_BIN (default claude) · CLAUDE_ARGS (extra flags, e.g. "--model opus"). Needs a logged-in Claude Code.
// The key never reaches the terminal, a file or a screenshot: it is masked on screen, and the MCP config that carries
// it lives in a private temp dir removed right after the run.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import puppeteer from "puppeteer-core";

const BASE = (process.env.NEXUS_URL ?? `http://localhost:${process.env.NEXUS_PORT || 8080}`).replace(/\/$/, "");
const API = (process.env.NEXUS_API ?? `${BASE}/api/v1`).replace(/\/$/, "");
const TENANT = "northwind";
const PERSONAS = {
  builder: { email: "admin@example.com", password: "Demo-Admin-2026!" },
  rep: { email: "test@example.com", password: "Demo-Rep-2026!" }, // a non-admin: admins bypass every ACL
};
const PROMPT = "Add a Suppliers table with name, email, phone and a rating from 1 to 5.";
const TABLE = "suppliers";
const REP_ROLE = "crm_sales_rep";
const CLAUDE = process.env.CLAUDE_BIN ?? "claude";
const CLAUDE_ARGS = (process.env.CLAUDE_ARGS ?? "").split(/\s+/).filter(Boolean);
// A whole token (43 chars after nxm_) is a secret; the 12-char start the Studio lists is not.
const redact = (s) => String(s).replace(/nxm_[A-Za-z0-9_-]{40,}/g, "nxm_[REDACTED]");

const outDir = process.argv[2];
if (!outDir) {
  console.error("usage: node scripts/mcp-loop.mjs <out-dir>");
  process.exit(2);
}

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

// Labels come from the UI's own catalog by key, so a reworded label cannot silently break a selector.
const VI = Object.fromEntries(
  [...fs.readFileSync(new URL("../lib/i18n/vi.ts", import.meta.url), "utf8").matchAll(/^\s*"([\w.]+)":\s*("(?:[^"\\]|\\.)*"),?\s*$/gm)]
    .map(([, k, v]) => [k, JSON.parse(v)]),
);
function t(key, vars = {}) {
  if (!(key in VI)) throw new Error(`lib/i18n/vi.ts has no key ${key}`);
  return VI[key].replace(/\{(\w+)\}/g, (m, k) => (k in vars ? String(vars[k]) : m));
}

// ── API (to read state and check what the screen claims; every builder action goes through the browser) ──
async function login({ email, password }) {
  const r = await fetch(`${API}/auth/login`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ tenant: TENANT, email, password }) });
  const j = await r.json();
  if (!j.data?.accessToken) throw new Error(`login ${email}: ${JSON.stringify(j).slice(0, 200)}`);
  return j.data;
}
let builderToken, repToken;
async function call(token, p, init = {}) {
  const r = await fetch(`${API}${p}`, { ...init, headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json", ...init.headers } });
  const j = await r.json().catch(() => ({}));
  return { status: r.status, body: j };
}
async function api(p, init = {}) {
  const { status, body } = await call(builderToken, p, init);
  if (status >= 300) throw new Error(`${init.method ?? "GET"} ${p}: ${status} ${JSON.stringify(body).slice(0, 300)}`);
  return body;
}
const drafts = async () => (await api("/admin/publish/changes")).data ?? [];
const headVersion = async () => (await api("/admin/publish/state")).data.headVersion?.versionNo ?? 0;

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
  args: process.env.CI ? ["--no-sandbox"] : [],
});
const errors = new Map();
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
const KEYS_PAGE = `/studio/du-an/${TENANT}/van-hanh/tu-dong/khoa-truy-cap`;
const dock = '[data-testid="assistant-dock"]';

async function button(page, text, scope = "body") {
  const h = await page.waitForFunction(
    (scope, text) => [...document.querySelectorAll(`${scope} button`)].find((b) => (b.textContent.trim() === text || b.getAttribute("aria-label") === text) && !b.disabled && b.getAttribute("aria-disabled") !== "true"),
    {}, scope, text,
  );
  return h.asElement();
}
const waitText = (page, text, scope = "body") =>
  page.waitForFunction((scope, text) => document.querySelector(scope)?.innerText.includes(text), {}, scope, text);
// The key is on screen once: mask it before any screenshot.
const maskKeys = (page) => page.evaluate(() => {
  const walk = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let n = walk.nextNode(); n; n = walk.nextNode()) if (/nxm_[A-Za-z0-9_-]{20,}/.test(n.nodeValue)) n.nodeValue = n.nodeValue.replace(/nxm_[A-Za-z0-9_-]{20,}/g, "nxm_[REDACTED]");
});

// ── the loop ──
let builder, rep, secret, endpoint, prefix, session, entity;
let active;
const steps = [
  ["builder-creates-an-agent-key", async () => {
    active = builder = await signIn(PERSONAS.builder);
    await go(builder, KEYS_PAGE);
    await (await button(builder, t("studio.operate.mcp.new"))).click();
    const line = await builder.waitForFunction(() =>
      [...document.querySelectorAll('[data-testid="mcp-secret"] code')].map((c) => c.textContent).find((s) => s.startsWith("claude mcp add")));
    const m = /^claude mcp add --transport http nexus (\S+) --header "Authorization: Bearer (nxm_[A-Za-z0-9_-]{43})"$/.exec(await line.jsonValue());
    if (!m) throw new Error(`the Studio's command line is not the claude mcp add call: ${redact(await line.jsonValue())}`);
    [, endpoint, secret] = m;
    prefix = secret.slice(4, 16);
    const tokens = (await api("/admin/mcp-tokens")).data.tokens;
    session = tokens.find((x) => x.tokenPrefix === prefix)?.sessionId;
    if (!session) throw new Error("the new key is not in the builder's list");
    await maskKeys(builder);
    return `key nxm_${prefix}… for ${endpoint}, conversation ${session}`;
  }],

  ["key-shown-once", async () => {
    // The SLDS checkbox draws its own box over a hidden input: a DOM click on the input is what a click on the box does.
    await builder.$eval('[data-testid="mcp-secret"] input[type="checkbox"]', (el) => el.click());
    await (await button(builder, t("studio.operate.secret.done"))).click();
    await builder.waitForFunction(() => !document.querySelector('[data-testid="mcp-secret"]'));
    if ((await builder.content()).includes(secret)) throw new Error("the key is still in the page after Xong");
    await waitText(builder, `nxm_${prefix}…`, "main");
    return "after Xong only the start of the key is listed";
  }],

  ["agent-proposes-over-mcp", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "nexus-mcp-"));
    const cfg = path.join(dir, "mcp.json");
    fs.writeFileSync(cfg, JSON.stringify({ mcpServers: { nexus: { type: "http", url: endpoint, headers: { Authorization: `Bearer ${secret}` } } } }), { mode: 0o600 });
    let run;
    try {
      run = spawnSync(CLAUDE, ["-p", PROMPT, "--mcp-config", cfg, "--strict-mcp-config", "--allowedTools", "mcp__nexus",
        "--output-format", "stream-json", "--verbose", "--no-session-persistence", ...CLAUDE_ARGS],
      { cwd: dir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 15 * 60_000, maxBuffer: 64 << 20 });
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
    fs.writeFileSync(path.join(outDir, "claude-transcript.jsonl"), redact(run.stdout ?? ""));
    if (run.status !== 0) throw new Error(`claude exited ${run.status}: ${redact(run.stderr ?? "").slice(-400)}`);
    const events = (run.stdout ?? "").split("\n").filter(Boolean).map((l) => JSON.parse(l));
    const calls = events.flatMap((e) => (e.type === "assistant" ? e.message.content.filter((c) => c.type === "tool_use").map((c) => c.name) : []));
    const result = events.find((e) => e.type === "result");
    if (!calls.some((n) => n === "mcp__nexus__nexus_propose_entity")) throw new Error(`the agent never proposed a table: ${calls.join(", ")}`);
    return `tools: ${calls.map((n) => n.replace("mcp__nexus__", "")).join(", ")} · ${result?.usage?.output_tokens ?? "?"} output tokens`;
  }],

  ["card-in-the-studio-conversation", async () => {
    await go(builder, KEYS_PAGE);
    await (await button(builder, t("studio.operate.mcp.open.rowOf", { prefix: `nxm_${prefix}…` }))).click();
    const open = t("studio.assistant.card.state.open");
    await builder.waitForSelector(`${dock} article[aria-label^="${open}: "]`, { timeout: 30_000 });
    const cards = await builder.$$eval(`${dock} article`, (a) => a.map((x) => x.getAttribute("aria-label")));
    const thread = (await api(`/admin/ai/builder/sessions/${session}`)).data.messages.flatMap((m) => m.draft ?? []);
    const table = thread.find((d) => d.kind === "entity" && /supplier/i.test(d.entity?.name ?? ""));
    if (!table) throw new Error(`no table proposal in the conversation: ${JSON.stringify(thread.map((d) => d.kind))}`);
    entity = { name: table.entity.name, fields: table.entity.fields.map((f) => `${f.name}:${f.fieldType}`) };
    return `${cards.length} card(s): ${cards.join(" | ")} · ${entity.name}(${entity.fields.join(", ")})`;
  }],

  ["applied-to-drafts", async () => {
    const eligible = [t("studio.assistant.card.apply"), t("studio.assistant.draft.applyDraft")];
    const applyAll = t("studio.assistant.card.applyAll").split("{n}")[0];
    const targets = await builder.$$eval(`${dock} article`, (arts, eligible) =>
      arts.map((a, i) => ([...a.querySelectorAll("button")].some((b) => eligible.includes(b.textContent.trim())) ? i : -1)).filter((i) => i >= 0), eligible);
    if (targets.length === 0) throw new Error("no card can be applied");
    let bulk = null;
    for (const b of await builder.$$(`${dock} button`)) if ((await b.evaluate((x) => x.textContent.trim())).startsWith(applyAll)) bulk = b;
    if (bulk) await bulk.click();
    else await (await button(builder, eligible.find(Boolean), dock)).click().catch(async () => (await button(builder, eligible[1], dock)).click());
    const done = t("studio.assistant.card.state.done");
    const state = await builder.waitForFunction(
      (scope, targets, done, open, applying) => {
        const labels = targets.map((i) => document.querySelectorAll(`${scope} article`)[i]?.getAttribute("aria-label") ?? "");
        if (labels.some((l) => l.startsWith(`${open}: `) || l.startsWith(`${applying}: `))) return false;
        const bad = labels.find((l) => !l.startsWith(`${done}: `));
        return bad === undefined ? "done" : `not applied: ${bad}`;
      },
      { timeout: 120_000, polling: 300 }, dock, targets, done, t("studio.assistant.card.state.open"), t("studio.assistant.card.state.applying"),
    );
    if ((await state.jsonValue()) !== "done") throw new Error(await state.jsonValue());
    const pending = await drafts();
    if (!pending.some((c) => c.docType === "entity" && c.op === "create" && c.entityName === entity.name)) {
      throw new Error(`the table is not among the drafts: ${JSON.stringify(pending.map((c) => `${c.op} ${c.docType} ${c.entityName ?? c.label}`))}`);
    }
    return `${targets.length} card(s) applied; drafts: ${pending.map((c) => `${c.op} ${c.docType} "${c.label}"`).join(", ")}`;
  }],

  ["published", async () => {
    active = null; // API-only steps: nothing new on screen to show
    // until #16: the workshop's header has no Publish button on main yet (PR #16 adds it), so this step calls the same
    // POST /admin/publish the button calls, with the change set the builder just reviewed.
    const before = await headVersion();
    const changes = await api("/admin/publish/changes");
    await api("/admin/publish", { method: "POST", body: JSON.stringify({ changesetHash: changes.meta.changesetHash, note: "MCP: Suppliers table" }) });
    if ((await headVersion()) !== before + 1) throw new Error("no new version");
    return `version ${before + 1} is live (${changes.data.length} change(s))`;
  }],

  ["rep-refused-before-access", async () => {
    repToken = (await login(PERSONAS.rep)).accessToken;
    const { status, body } = await call(repToken, `/${entity.name}`);
    if (status !== 403) throw new Error(`the rep reads ${entity.name} before any grant: ${status} ${body.code ?? ""}`);
    return `GET /${entity.name} as the rep → ${status} ${body.code}`;
  }],

  ["rep-granted", async () => {
    // until #2: the AI cannot propose access yet, so the script grants the rep's role what a builder would.
    const roles = (await api("/admin/roles")).data.roles;
    const role = (await api(`/admin/roles/${roles.find((r) => r.name === REP_ROLE).id}`)).data.role;
    const actions = [...role.actions.filter((a) => a.entityName !== entity.name),
      ...["view", "create", "edit"].map((action) => ({ entityName: entity.name, action, accessLevel: "all" }))];
    await api(`/admin/roles/${role.id}`, { method: "PUT", body: JSON.stringify({ name: role.name, label: role.label, actions }) });
    return `${REP_ROLE} on ${entity.name}: view, create, edit (all)`;
  }],

  ["rep-uses-suppliers", async () => {
    active = rep = await signIn(PERSONAS.rep);
    const nameField = "name";
    const { status, body } = await call(repToken, `/${entity.name}`, { method: "POST", body: JSON.stringify({ [nameField]: "Acme Paper Co." }) });
    if (status !== 201) throw new Error(`the rep creates a supplier: ${status} ${JSON.stringify(body).slice(0, 200)}`);
    await go(rep, `/${entity.name}`);
    await waitText(rep, "Acme Paper Co.", "main");
    return `the rep created and lists "Acme Paper Co." in ${entity.name}`;
  }],

  ["key-revoked", async () => {
    active = builder;
    await go(builder, KEYS_PAGE);
    await (await button(builder, t("studio.operate.mcp.revoke.rowOf", { prefix: `nxm_${prefix}…` }))).click();
    // The confirm of InlineConfirm (the row's own "Thu hồi" button has the same text but no aria-describedby).
    const confirm = await builder.waitForFunction((label) =>
      [...document.querySelectorAll("main button[aria-describedby]")].find((b) => b.textContent.trim() === label && !b.disabled), {}, t("studio.operate.mcp.revoke.confirm"));
    await confirm.asElement().click();
    await builder.waitForFunction((p) => !document.querySelector("main").innerText.includes(p), {}, `nxm_${prefix}…`);
    const r = await fetch(endpoint, { method: "POST", headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json", Accept: "application/json, text/event-stream" }, body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/list" }) });
    if (r.status !== 401) throw new Error(`/mcp answers ${r.status} to the revoked key`);
    return "the key left the list and /mcp answers 401 to it";
  }],
];

fs.mkdirSync(outDir, { recursive: true });
let failed = null;
try {
  builderToken = (await login(PERSONAS.builder)).accessToken;
  await preflight();
  for (const [i, [name, run]] of steps.entries()) {
    const file = path.join(outDir, `${String(i + 1).padStart(2, "0")}-${name}.png`);
    const t0 = Date.now();
    try {
      const note = await run();
      if (active) {
        await maskKeys(active);
        await active.screenshot({ path: file });
      }
      console.log(`ok   ${String(i + 1).padStart(2)} ${name} (${((Date.now() - t0) / 1000).toFixed(1)}s) — ${redact(note)}`);
    } catch (err) {
      failed = name;
      if (active) await maskKeys(active).then(() => active.screenshot({ path: file.replace(/\.png$/, "-FAILED.png") })).catch(() => {});
      console.error(`FAIL ${String(i + 1).padStart(2)} ${name}: ${redact(err.message)}`);
      for (const e of (active && errors.get(active)) ?? []) console.error(`     page error: ${redact(e)}`);
      break;
    }
  }
} catch (err) {
  failed = "setup";
  console.error(`FAIL setup: ${redact(err.message)}`);
} finally {
  await browser.close();
}
if (failed) process.exit(1);
console.log(`mcp loop passed: ${steps.length} steps, screenshots and the redacted transcript in ${outDir}`);
