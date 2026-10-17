#!/usr/bin/env node
// builder-bench (issue #4): the AI builder, judged the way its user would judge it. For each case of
// evals/builder-bench/cases, on a stack reset to the seeded state, the builder asks the Studio assistant, applies every card
// it proposes with the cards' own buttons and publishes from the header; then the sales rep (test@example.com, not an
// admin) must read the tables the case names (scripts/acceptance-acl.py) and open them in the browser, from the app
// switcher when the case builds an app. What it saw goes to <out-dir>/obs, one JSON per attempt. The grades come from
// `go run ./cmd/aieval -bench ../evals/builder-bench -obs <out-dir>/obs` (nexus-platform/), which owns the rules.
//
//   node scripts/builder-bench.mjs <out-dir> [--only id,id] [--runs N] [--cap TOKENS]        (from nexus-web/)
//
// The stack: `NEXUS_PORT=8093 docker compose -p nexus-bench up -d` from the repo root, once, then this script. Before the
// first attempt it dumps the freshly seeded database to <out-dir>/seeded.sql. Before every attempt it restores that dump,
// checks that the restored database dumps to the same bytes as the first restore (<out-dir>/seeded.sha256) and recreates
// the api container, so every attempt starts from the seeded state with nothing in memory. The api runs with
// AI_GATEWAY_MODE=record: every model call of an attempt lands in <out-dir>/cassettes, the attempt's transcript.
// Env: AI_MODEL (the gateway's id of the model), AI_GATEWAY_BASE_URL and AI_GATEWAY_API_KEY (from the shell; the key is
// never written), NEXUS_PORT (default 8093), BENCH_PROJECT (default nexus-bench), CHROME_PATH.
// --replay <file> answers every attempt from <out-dir>/cassettes/<file> instead of the gateway (AI_GATEWAY_MODE=replay): a
// dry run of the stages after the model, for no tokens, e.g. with the demo loop's cassette and the app-helpdesk case.
// Attempts already in <out-dir>/obs are skipped, so running again with a higher --runs adds attempts. Attempts go round
// by round (every case once, then every case again). The run stops before an attempt once the tokens recorded in
// <out-dir>/obs reach --cap (default 5,000,000).
import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import puppeteer from "puppeteer-core";
import { ASSISTANT, attr, connect, findChrome, openFromMenu, t } from "./studio-driver.mjs";

const ROOT = fileURLToPath(new URL("../..", import.meta.url));
const PORT = process.env.NEXUS_PORT || "8093";
const BASE = `http://localhost:${PORT}`;
const PROJECT = process.env.BENCH_PROJECT || "nexus-bench";
let ACC; // the workspace and its two people, as the stack was seeded (seedAccounts)
const AI_TIMEOUT = 10 * 60_000; // one turn; the panel's own limit is MaxSteps × the per-call timeout
// The public id of a model: a gateway may prefix it ("<route>/claude-…"); the bench writes the id alone.
const publicId = (m) => String(m ?? "").replace(/^[a-z0-9]+\/(?=claude-)/, "");
const MODEL = publicId(process.env.AI_MODEL);

const args = process.argv.slice(2);
const opt = (name, def) => (args.includes(`--${name}`) ? args[args.indexOf(`--${name}`) + 1] : def);
const outDir = args[0] && !args[0].startsWith("--") ? path.resolve(args[0]) : null;
const REPLAY = opt("replay", "");
if (!outDir || !MODEL || !(process.env.AI_GATEWAY_BASE_URL || REPLAY)) {
  console.error("usage: AI_MODEL=… AI_GATEWAY_BASE_URL=… [AI_GATEWAY_API_KEY=…] node scripts/builder-bench.mjs <out-dir> [--only id,id] [--runs N] [--cap TOKENS] [--replay FILE]");
  process.exit(2);
}
const RUNS = Number(opt("runs", "1"));
const CAP = Number(opt("cap", "5000000"));
const CHROME = findChrome();
if (!CHROME) {
  console.error("FAIL setup: no Chrome found — set CHROME_PATH to a Chrome or Chromium binary");
  process.exit(1);
}

const GROUPS = ["app", "access", "edit", "adversarial"];
const CASES_DIR = path.join(ROOT, "evals/builder-bench/cases");
const cases = fs.readdirSync(CASES_DIR).filter((f) => f.endsWith(".json"))
  .map((f) => JSON.parse(fs.readFileSync(path.join(CASES_DIR, f), "utf8")))
  .sort((a, b) => GROUPS.indexOf(a.group) - GROUPS.indexOf(b.group) || a.id.localeCompare(b.id));
const only = opt("only", "").split(",").filter(Boolean);
for (const id of only) if (!cases.some((c) => c.id === id)) throw new Error(`no case ${id} in ${CASES_DIR}`);
const chosen = only.length ? cases.filter((c) => only.includes(c.id)) : cases;

for (const d of ["obs", "cassettes", "shots"]) fs.mkdirSync(path.join(outDir, d), { recursive: true });
const OVERRIDE = path.join(outDir, "compose.bench.yml");
fs.writeFileSync(OVERRIDE, `# builder-bench: the api records each attempt's model calls into ${outDir}/cassettes.\nservices:\n  api:\n    volumes:\n      - ${JSON.stringify(`${path.join(outDir, "cassettes")}:/bench-cassettes`)}\n`);
const COMMIT = spawnSync("git", ["rev-parse", "--short", "HEAD"], { cwd: ROOT, encoding: "utf8" }).stdout.trim();

// ── the stack ──
function compose(argv, { env = {}, input } = {}) {
  const r = spawnSync("docker", ["compose", "-p", PROJECT, "-f", path.join(ROOT, "docker-compose.yml"), "-f", OVERRIDE, ...argv], {
    cwd: ROOT, env: { ...process.env, NEXUS_PORT: PORT, ...env }, input, maxBuffer: 512 << 20,
  });
  if (r.status !== 0) throw new Error(`docker compose ${argv.slice(0, 4).join(" ")}: exit ${r.status} ${String(r.stderr ?? "").slice(-400)}`);
  return r.stdout; // a Buffer
}
const sql = (q) => compose(["exec", "-T", "mysql", "mysql", "-uroot", "-N", "-B", "nexus", "-e", q]).toString("utf8");
const dumpDb = () => compose(["exec", "-T", "mysql", "mysqldump", "-uroot", "--single-transaction", "--skip-dump-date", "--routines", "--triggers", "--events", "--hex-blob", "nexus"]);
const sha256 = (b) => crypto.createHash("sha256").update(b).digest("hex");
function restoreDb(dump) {
  compose(["exec", "-T", "mysql", "mysql", "-uroot", "-e", "DROP DATABASE nexus; CREATE DATABASE nexus CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;"]);
  compose(["exec", "-T", "mysql", "mysql", "-uroot", "nexus"], { input: dump });
}
// The workspace, the builder and the sales rep (a non-admin: admins bypass every ACL) the stack was seeded with, read from
// the seed container's own environment, so no password lives in this file.
function seedAccounts() {
  const id = compose(["ps", "-a", "-q", "seed"]).toString("utf8").trim().split("\n")[0];
  if (!id) throw new Error(`no seed container in ${PROJECT}: NEXUS_PORT=${PORT} docker compose -p ${PROJECT} up -d`);
  const env = Object.fromEntries(JSON.parse(spawnSync("docker", ["inspect", id, "--format", "{{json .Config.Env}}"], { encoding: "utf8" }).stdout)
    .map((kv) => [kv.slice(0, kv.indexOf("=")), kv.slice(kv.indexOf("=") + 1)]));
  return {
    tenant: env.NEXUS_TENANT,
    builder: { email: env.NEXUS_ADMIN_EMAIL, password: env.NEXUS_ADMIN_PASS },
    rep: { email: env.NEXUS_STAFF_EMAIL, password: env.NEXUS_STAFF_PASS },
  };
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function healthy() {
  for (const end = Date.now() + 120_000; Date.now() < end; await sleep(1000)) {
    try {
      const ai = await fetch(`${BASE}/api/v1/ai/health`).then((r) => (r.ok ? r.json() : null));
      if (ai?.data?.enabled === true) return;
    } catch { /* not up yet */ }
  }
  throw new Error(`the api at ${BASE} is not up with AI enabled after 120 s`);
}

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true,
  defaultViewport: { width: 1440, height: 900 },
  args: process.env.CI ? ["--no-sandbox"] : [],
});

// The seeded state, once: the source must be a stack nothing has happened on since the seed.
async function seededState() {
  const dumpFile = path.join(outDir, "seeded.sql");
  const shaFile = path.join(outDir, "seeded.sha256");
  if (fs.existsSync(dumpFile) && fs.existsSync(shaFile)) return { dump: fs.readFileSync(dumpFile), sha: fs.readFileSync(shaFile, "utf8").trim() };
  const nx = connect({ base: BASE, tenant: ACC.tenant, browser });
  await nx.asAdmin(ACC.builder);
  const why = [];
  const head = (await nx.api("/admin/publish/state")).data.headVersion;
  if (head?.versionNo !== 1 || head?.kind !== "baseline") why.push(`head version ${JSON.stringify(head)}`);
  if ((await nx.drafts()).length > 0) why.push("draft changes are pending");
  const extra = (await nx.api("/metadata/entities")).data.map((e) => e.name).filter((n) => !n.startsWith("crm_"));
  if (extra.length > 0) why.push(`tables beyond the CRM pack: ${extra.join(", ")}`);
  if ((await nx.api("/admin/ai/builder/sessions")).data.length > 0) why.push("the assistant already has conversations");
  if (why.length) throw new Error(`the stack is not freshly seeded (${why.join("; ")}): docker compose -p ${PROJECT} down -v, then up -d`);
  compose(["stop", "api"]);
  const dump = dumpDb();
  fs.writeFileSync(dumpFile, dump);
  // A restored database shows its column charsets in SHOW CREATE TABLE where the first one did not: the canonical bytes
  // are those of the dump restored, which every attempt then reproduces exactly.
  restoreDb(dump);
  const sha = sha256(dumpDb());
  fs.writeFileSync(shaFile, `${sha}\n`);
  return { dump, sha };
}

function reset(seed, cassette) {
  compose(["rm", "-sf", "api"]);
  restoreDb(seed.dump);
  const sha = sha256(dumpDb());
  if (sha !== seed.sha) throw new Error(`the restored database is not the seeded state: sha256 ${sha}, want ${seed.sha}`);
  const mode = REPLAY ? { AI_GATEWAY_MODE: "replay", AI_GATEWAY_CASSETTE: `/bench-cassettes/${REPLAY}` }
    : { AI_GATEWAY_MODE: "record", AI_GATEWAY_CASSETTE: `/bench-cassettes/${cassette}` };
  compose(["up", "-d", "--no-deps", "api"], { env: { AI_ENABLED: "true", ...mode } });
  return sha;
}

// ── the builder ──
// The panel is sending while its progress line offers Huỷ (a card's own questions have a Huỷ too, inside the card).
const sending = (scope, cancel) =>
  [...(document.querySelector(scope)?.querySelectorAll("button") ?? [])].some((b) => b.textContent.trim() === cancel && !b.closest("article"));

/** Types the prompt in the assistant panel and waits for the answer; the answer itself is read from the session. */
async function ask(nx, page, prompt) {
  await nx.go(page, nx.studio("dung/du-lieu"));
  const toggle = await page.waitForSelector('[data-testid="assistant-toggle"]');
  if ((await toggle.evaluate((b) => b.getAttribute("aria-pressed"))) !== "true") await toggle.click();
  const box = await page.waitForSelector(`${ASSISTANT} textarea:not([disabled])`, { visible: true });
  await box.type(prompt);
  await page.keyboard.press("Enter");
  const cancel = t("studio.assistant.cancel");
  await page.waitForFunction(sending, { timeout: 15_000, polling: 200 }, ASSISTANT, cancel).catch(() => {});
  const out = { reply: "", draft: [] };
  try {
    // Done when nothing is sending and the answer (or an error) sits under the question.
    await page.waitForFunction((scope, cancel) => {
      const dock = document.querySelector(scope);
      const busy = [...(dock?.querySelectorAll("button") ?? [])].some((b) => b.textContent.trim() === cancel && !b.closest("article"));
      return !busy && dock?.querySelector('[role="log"]')?.children.length >= 2;
    }, { timeout: AI_TIMEOUT, polling: 500 }, ASSISTANT, cancel);
  } catch {
    out.turnError = `no answer within ${AI_TIMEOUT / 60_000} min`;
  }
  // An error in place of the answer: the panel's notification with its Thử lại, outside any card.
  const shown = await page.evaluate((scope, retry) => {
    const b = [...document.querySelectorAll(`${scope} [role="log"] button`)].find((x) => x.textContent.trim() === retry && !x.closest("article"));
    return b ? (b.parentElement?.querySelector(".slds-scoped-notification")?.innerText ?? "").trim() || "error" : "";
  }, ASSISTANT, t("studio.assistant.card.retry"));
  if (shown) out.turnError = shown;
  const sessions = (await nx.api("/admin/ai/builder/sessions")).data;
  if (sessions.length > 0) {
    const s = (await nx.api(`/admin/ai/builder/sessions/${sessions[0].id}`)).data;
    const first = s.messages.find((m) => m.role === "assistant");
    out.reply = first?.content ?? "";
    out.draft = first?.draft ?? [];
    out.sessionId = s.id;
  }
  return out;
}

const STATE = Object.fromEntries(["open", "applying", "done", "warn", "discarded", "undone", "sealed"]
  .map((k) => [k, t(`studio.assistant.card.state.${k}`)]));
/** The first `n` cards (this turn's; a turn the panel sends later to report a failed apply adds its own below). */
const cardStates = (page, n) => page.$$eval(`${ASSISTANT} article`, (arts, n, S, retry) => arts.slice(0, n).map((a) => {
  const label = a.getAttribute("aria-label") ?? "";
  let state = Object.keys(S).find((k) => label.startsWith(`${S[k]}: `)) ?? "unknown";
  const alert = a.querySelector('[role="alert"]')?.innerText.trim() ?? "";
  if (state === "open" && alert && [...a.querySelectorAll("button")].some((b) => b.textContent.trim() === retry)) state = "error";
  const note = state === "error" ? alert : state === "warn" ? (a.querySelector('[role="status"]')?.innerText.trim() ?? "") : "";
  return { title: a.querySelector("h3")?.textContent.trim() || label, state, message: note };
}), n, STATE, t("studio.assistant.card.retry"));
const primary = (page, i) => page.evaluate((scope, i, labels) => {
  const a = document.querySelectorAll(`${scope} article`)[i];
  return [...(a?.querySelectorAll("button") ?? [])].map((b) => b.textContent.trim()).find((x) => labels.includes(x)) ?? null;
}, ASSISTANT, i, [t("studio.assistant.card.apply"), t("studio.assistant.draft.applyDraft"), t("studio.assistant.draft.applyNow"), t("studio.assistant.card.deleteAction")]);
async function settled(page, idx, timeout = 180_000) {
  await page.waitForFunction((scope, idx, open, applying, retry) => idx.every((i) => {
    const a = document.querySelectorAll(`${scope} article`)[i];
    const l = a?.getAttribute("aria-label") ?? "";
    if (l.startsWith(`${applying}: `)) return false;
    if (!l.startsWith(`${open}: `)) return true;
    return !!a.querySelector('[role="alert"]') && [...a.querySelectorAll("button")].some((b) => b.textContent.trim() === retry);
  }), { timeout, polling: 300 }, ASSISTANT, idx, STATE.open, STATE.applying, t("studio.assistant.card.retry"));
}

/** The element `sel` of card `i` whose text is `text`, as a handle (clicked like a person: scrolled to, then the mouse). */
async function inCard(page, i, sel, text, timeout = 10_000) {
  const h = await page.waitForFunction((scope, i, sel, text) => [...(document.querySelectorAll(`${scope} article`)[i]?.querySelectorAll(sel) ?? [])]
    .find((b) => b.textContent.trim() === text && !b.disabled), { timeout }, ASSISTANT, i, sel, text);
  return h.asElement();
}

/** Applies every card the way a builder does: "Áp dụng tất cả", then each card still open, answering its question. */
async function applyCards(page, n) {
  const bulkLabels = [t("studio.assistant.card.apply"), t("studio.assistant.draft.applyDraft")];
  const before = [];
  for (let i = 0; i < n; i++) before.push(await primary(page, i));
  const applyAll = t("studio.assistant.card.applyAll").split("{n}")[0];
  const bulk = await page.evaluateHandle((scope, prefix) => [...document.querySelectorAll(`${scope} button`)]
    .find((b) => b.textContent.trim().startsWith(prefix) && !b.closest("article") && !b.disabled) ?? null, ASSISTANT, applyAll);
  if (bulk.asElement()) {
    await bulk.asElement().click();
    await settled(page, before.map((l, i) => (bulkLabels.includes(l) ? i : -1)).filter((i) => i >= 0)).catch(() => {});
  }
  const confirms = [t("studio.assistant.draft.confirmNowAction"), t("studio.assistant.sample.confirmRealAction")];
  for (let i = 0; i < n; i++) {
    if ((await cardStates(page, n))[i].state !== "open") continue;
    const label = await primary(page, i);
    if (!label || label === t("studio.assistant.card.deleteAction")) continue; // a deletion is never applied by the bench
    await inCard(page, i, "button", label).then((b) => b.click());
    // "Áp ngay" asks in the card first (InlineConfirm: its buttons carry aria-describedby).
    const asked = await page.waitForFunction((scope, i, confirms, applying, open) => {
      const a = document.querySelectorAll(`${scope} article`)[i];
      const b = [...(a?.querySelectorAll("button[aria-describedby]") ?? [])].find((x) => confirms.includes(x.textContent.trim()) && !x.disabled);
      if (b) return "ask";
      return (a?.getAttribute("aria-label") ?? "").startsWith(`${open}: `) && !a.querySelector('[role="alert"]') ? false : "went";
    }, { timeout: 10_000, polling: 200 }, ASSISTANT, i, confirms, STATE.applying, STATE.open).then((h) => h.jsonValue()).catch(() => "went");
    if (asked === "ask") {
      for (const c of confirms) {
        const b = await inCard(page, i, "button[aria-describedby]", c, 1_000).catch(() => null);
        if (b) {
          await b.click();
          break;
        }
      }
    }
    await settled(page, [i], 120_000).catch(() => {});
  }
  return cardStates(page, n);
}

/** Waits until the panel sends nothing more (a failed apply makes it report the failure to the model on its own). */
async function idle(page) {
  await sleep(2_000);
  await page.waitForFunction((scope, cancel) => ![...(document.querySelector(scope)?.querySelectorAll("button") ?? [])]
    .some((b) => b.textContent.trim() === cancel && !b.closest("article")), { timeout: AI_TIMEOUT, polling: 500 }, ASSISTANT, t("studio.assistant.cancel"));
}

async function publish(nx, page) {
  const p = { pending: (await nx.drafts()).length, before: await nx.headVersion() };
  if (p.pending > 0) {
    try {
      const want = await nx.publishFromHeader(page, nx.studio("dung/du-lieu"));
      await nx.submitPublish(page, want);
    } catch (e) {
      p.error = String(e?.message ?? e);
    }
  }
  p.after = await nx.headVersion();
  return p;
}

// ── the rep ──
function repTables(c, draft) {
  const out = [];
  for (const name of c.repTables ?? []) {
    const names = name === "@new" ? draft.filter((d) => d.kind === "entity" && d.entity?.name).map((d) => d.entity.name) : [name];
    for (const n of names) if (!out.includes(n)) out.push(n);
  }
  return out;
}

function acceptance(tables) {
  const r = spawnSync("python3", [path.join(ROOT, "nexus-platform/scripts/acceptance-acl.py"), "--base", `${BASE}/api/v1`,
    "--tenant", ACC.tenant, "--admin", ACC.builder.email, "--staff", ACC.rep.email, "--expect", tables.join(",")],
  // the script reads the passwords from its environment (never argv), and gets nothing else of ours
  { encoding: "utf8", timeout: 180_000, env: { PATH: process.env.PATH, HOME: process.env.HOME, NEXUS_ADMIN_PASS: ACC.builder.password, NEXUS_STAFF_PASS: ACC.rep.password } });
  return { exit: r.status ?? -1, output: `${r.stdout ?? ""}${r.stderr ?? ""}` };
}

/** The rep opens each table: from the app switcher and the app's menu when the case builds an app, else at its URL. */
async function smoke(nx, rep, c, tables, newApps) {
  const viaApp = (c.expectAllKinds ?? []).includes("app");
  const via = viaApp ? "app" : "url";
  const labels = Object.fromEntries((await nx.api("/metadata/entities")).data.map((e) => [e.name, e.label]));
  if (viaApp) {
    const app = newApps[0];
    if (!app) return tables.map((table) => ({ table, via, ok: false, error: "the apply built no app" }));
    try {
      await nx.go(rep, "/home");
      await rep.click(`button[title=${attr(t("slds.appLauncher.open"))}]`);
      const tile = await rep.waitForFunction((label) => [...document.querySelectorAll('[role="dialog"] .slds-app-launcher__tile a')]
        .find((a) => a.textContent.trim() === label), {}, app.label);
      await tile.asElement().click();
    } catch (e) {
      return tables.map((table) => ({ table, via, ok: false, error: `the rep's app switcher has no "${app.label}": ${e.message}` }));
    }
  }
  const out = [];
  for (const table of tables) {
    try {
      if (viaApp) {
        const label = labels[table] ?? table;
        const listed = await rep.waitForFunction((label) => [...document.querySelectorAll(".slds-context-bar__secondary a")]
          .some((a) => a.textContent.trim().endsWith(label)), { timeout: 15_000 }, label).then(() => true, () => false);
        if (!listed) throw new Error(`the app's menu has no "${label}"`);
        await openFromMenu(rep, { name: table, label });
      } else {
        await nx.go(rep, `/${table}`);
        const got = await rep.waitForFunction(() => (document.querySelector("main table thead th") ? "list"
          : document.querySelector("main .slds-scoped-notification")?.innerText.trim() || false)).then((h) => h.jsonValue());
        if (got !== "list") throw new Error(got);
      }
      out.push({ table, via, ok: true });
    } catch (e) {
      const shown = await rep.$eval("main .slds-scoped-notification", (n) => n.innerText.trim()).catch(() => "");
      out.push({ table, via, ok: false, error: shown || String(e?.message ?? e) });
    }
  }
  return out;
}

// ── what the attempt cost ──
function ledger() {
  return sql("SELECT feature, COALESCE(prompt_tokens,0), COALESCE(output_tokens,0), tool_calls, outcome, COALESCE(latency_ms,0) FROM sys_ai_runs ORDER BY created_at, id")
    .split("\n").filter(Boolean).map((l) => {
      const [feature, p, o, calls, outcome, ms] = l.split("\t");
      return { feature, promptTokens: +p, outputTokens: +o, toolCalls: +calls, outcome, latencyMs: +ms };
    });
}
function transcript(file) {
  const f = path.join(outDir, "cassettes", file);
  if (!fs.existsSync(f)) return { calls: 0, sentBytes: 0, tools: [] };
  const lines = fs.readFileSync(f, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
  return {
    calls: lines.length,
    sentBytes: lines.reduce((n, e) => n + Buffer.byteLength(JSON.stringify(e.request)), 0),
    tools: lines.flatMap((e) => (e.response?.choices?.[0]?.message?.tool_calls ?? []).map((c) => c.function?.name)),
  };
}
const KEY = process.env.AI_GATEWAY_API_KEY ?? "";
// Nothing the driver records carries a token or a password; the key and a gateway's model alias are scrubbed anyway.
const scrub = (s) => (KEY ? s.split(KEY).join("[redacted]") : s).replace(/\b[a-z0-9]+\/(claude-[a-z0-9.-]+)/g, "$1");
const tokensSpent = () => fs.readdirSync(path.join(outDir, "obs")).filter((f) => f.endsWith(".json"))
  .map((f) => JSON.parse(fs.readFileSync(path.join(outDir, "obs", f), "utf8")))
  .reduce((n, o) => n + (o.runs ?? []).reduce((m, r) => m + r.promptTokens + r.outputTokens, 0), 0);

async function attempt(c, n, seed) {
  const file = `${c.id}.${MODEL}.${n}`;
  const o = { case: c.id, model: MODEL, attempt: n, commit: COMMIT, started: new Date().toISOString(), reply: "", draft: [], cards: [] };
  const t0 = Date.now();
  const pages = {};
  try {
    o.seededSha256 = reset(seed, `${file}.jsonl`);
    await healthy();
    const nx = connect({ base: BASE, tenant: ACC.tenant, browser });
    await nx.asAdmin(ACC.builder);
    if ((await nx.drafts()).length > 0 || (await nx.headVersion()) !== 1) throw new Error("the reset stack is not at the seeded version");
    pages.builder = await nx.signIn(ACC.builder);
    Object.assign(o, await ask(nx, pages.builder, c.prompt));
    if (!c.expectNoDraft && o.draft.length > 0) {
      const appsBefore = new Set((await nx.api("/admin/apps")).data.apps.map((a) => a.name));
      o.cards = await applyCards(pages.builder, o.draft.length);
      await idle(pages.builder);
      o.newApps = (await nx.api("/admin/apps")).data.apps.filter((a) => !appsBefore.has(a.name)).map((a) => ({ name: a.name, label: a.label }));
      o.publish = await publish(nx, pages.builder);
      o.repTables = repTables(c, o.draft);
      if (o.repTables.length > 0) {
        o.acceptance = acceptance(o.repTables);
        pages.rep = await nx.signIn(ACC.rep);
        o.smoke = await smoke(nx, pages.rep, c, o.repTables, o.newApps);
      }
    }
    if (o.sessionId) {
      // Turns after the first: the panel reporting a failed apply to the model, and the model's answer.
      const s = (await nx.api(`/admin/ai/builder/sessions/${o.sessionId}`)).data;
      o.followUps = s.messages.filter((m) => m.role === "assistant").slice(1).map((m) => ({ reply: m.content, kinds: (m.draft ?? []).map((d) => d.kind) }));
    }
  } catch (e) {
    o.error = String(e?.message ?? e); // the driver's own failure, reported as is
  } finally {
    o.seconds = Math.round((Date.now() - t0) / 1000);
    try {
      o.runs = ledger();
    } catch (e) {
      o.error ??= `reading sys_ai_runs: ${e.message}`;
    }
    Object.assign(o, transcript(`${file}.jsonl`));
    for (const [who, page] of Object.entries(pages)) {
      await page.screenshot({ path: path.join(outDir, "shots", `${file}-${who}.png`), fullPage: true }).catch(() => {});
      await page.browserContext().close().catch(() => {});
    }
    fs.writeFileSync(path.join(outDir, "obs", `${file}.json`), scrub(JSON.stringify(o, null, 2)));
  }
  return o;
}

function line(o) {
  const tokens = (o.runs ?? []).reduce((m, r) => m + r.promptTokens + r.outputTokens, 0);
  const done = (o.cards ?? []).filter((x) => x.state === "done").length;
  const kinds = (o.draft ?? []).map((d) => d.kind).join(",") || "no draft";
  const pub = o.publish ? (o.publish.after > o.publish.before ? `v${o.publish.after} live` : `publish ✗ ${o.publish.error ?? `${o.publish.pending} pending`}`) : "";
  const rep = o.acceptance ? `acl exit ${o.acceptance.exit}` : "";
  const sm = o.smoke ? `smoke ${o.smoke.filter((x) => x.ok).length}/${o.smoke.length}` : "";
  return [`${o.case} #${o.attempt}`, kinds, o.cards?.length ? `cards ${done}/${o.cards.length} done` : "", pub, rep, sm,
    o.turnError ? `turn: ${o.turnError}` : "", o.error ? `driver: ${o.error}` : "", `${tokens.toLocaleString("en-US")} tokens, ${o.seconds} s`]
    .filter(Boolean).join(" · ");
}

let code = 0;
try {
  ACC = seedAccounts();
  const seed = await seededState();
  console.log(`builder-bench · ${MODEL} · ${chosen.length} case(s) × ${RUNS} · commit ${COMMIT} · seeded sha256 ${seed.sha.slice(0, 12)}`);
  stop: for (let n = 1; n <= RUNS; n++) {
    for (const c of chosen) {
      if (fs.existsSync(path.join(outDir, "obs", `${c.id}.${MODEL}.${n}.json`))) continue;
      const spent = tokensSpent();
      if (spent >= CAP) {
        console.log(`STOP: ${spent.toLocaleString("en-US")} tokens recorded in ${outDir}/obs, the cap is ${CAP.toLocaleString("en-US")}`);
        code = 3;
        break stop;
      }
      console.log(scrub(line(await attempt(c, n, seed))));
    }
  }
} catch (e) {
  console.error(`FAIL setup: ${scrub(String(e?.message ?? e))}`);
  code = 1;
} finally {
  await browser.close();
}
process.exit(code);
