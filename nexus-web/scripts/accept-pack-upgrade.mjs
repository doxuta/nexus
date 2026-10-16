#!/usr/bin/env node
// Acceptance for the one-pack upgrade page (specs/079 f7-nang-cap-*): clicks the real thing in Chrome.
//   NEXUS_API=http://localhost:8602/api/v1 NEXUS_FE=http://localhost:3602 node scripts/accept-pack-upgrade.mjs <out-dir> [--apply]
// Without --apply it only reads (list -> page -> three columns). With --apply it chooses, confirms and applies:
// run it ONLY against a cloned database. Exit 1 when a check fails.
import fs from "node:fs";
import path from "node:path";
import puppeteer from "puppeteer-core";

const API = process.env.NEXUS_API ?? "http://localhost:8602/api/v1";
const FE = process.env.NEXUS_FE ?? "http://localhost:3602";
const CHROME = process.env.CHROME_PATH ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const TENANT = "northwind";
const PACK = process.env.PACK ?? "ticketing";
const [outDir] = process.argv.slice(2);
const APPLY = process.argv.includes("--apply");
fs.mkdirSync(outDir, { recursive: true });

const r = await fetch(`${API}/auth/login`, {
  method: "POST", headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ tenant: TENANT, email: "admin@example.com", password: "Demo-Admin-2026!" }),
});
const s = (await r.json()).data;
const fails = [];
const check = (ok, what) => { console.log(`${ok ? "PASS" : "FAIL"} ${what}`); if (!ok) fails.push(what); };

const browser = await puppeteer.launch({ executablePath: CHROME, headless: true, defaultViewport: { width: 1440, height: 900 } });
const errors = [];
try {
  const page = await browser.newPage();
  page.on("pageerror", (e) => errors.push(String(e.message ?? e)));
  page.on("response", (x) => { if (x.status() >= 400 && process.env.DEBUG) console.log("HTTP", x.status(), x.url()); });
  const writes = [];
  page.on("request", (q) => { if (q.method() === "POST" && q.url().includes("/upgrade") && !q.url().includes("upgrade-plan")) writes.push(q.url()); });
  await page.evaluateOnNewDocument((s, tenant) => {
    localStorage.setItem("nexus_remember", "1");
    localStorage.setItem("nexus_token", s.accessToken);
    if (s.refreshToken) localStorage.setItem("nexus_refresh", s.refreshToken);
    if (s.user) localStorage.setItem("nexus_user", JSON.stringify(s.user));
    localStorage.setItem("nexus_tenant", tenant);
  }, s, TENANT);
  const shot = (n) => page.screenshot({ path: path.join(outDir, `${n}.png`) });
  const textOf = () => page.evaluate(() => document.body.innerText);
  const waitText = async (txt) => {
    try {
      await page.waitForFunction((x) => document.body.innerText.includes(x), { timeout: 45000 }, txt);
    } catch (e) {
      await shot("fail");
      console.log(`FAIL waiting for "${txt}" at ${page.url()}\n${(await textOf()).slice(0, 600)}\n${errors.join("\n")}`);
      throw e;
    }
  };
  const clickByText = async (sel, txt) => {
    const ok = await page.evaluate((sel, txt) => {
      const el = [...document.querySelectorAll(sel)].find((e) => e.textContent.trim().startsWith(txt) || e.getAttribute("aria-label")?.startsWith(txt));
      if (!el) return false;
      el.click();
      return true;
    }, sel, txt);
    check(ok, `click ${sel} "${txt}"`);
  };

  // 1. the list page links to the pack's upgrade page
  await page.goto(`${FE}/studio/du-an/${TENANT}/bao-tri/nang-cap`, { waitUntil: "networkidle2", timeout: 90000 });
  await waitText("Gói mẫu đã cài");
  await new Promise((r) => setTimeout(r, 800));
  await shot("1-list");
  await clickByText("a", `Xem nâng cấp: ${PACK}`);

  // 2. the page: plan read, groups, summary
  await waitText("Cần anh quyết");
  await new Promise((r) => setTimeout(r, 800));
  const body = await textOf();
  check(/Gói mẫu .+: v.+ lên v/.test(body), "title shows the version range");
  check(body.includes("Cần anh quyết") && body.includes("Gói đổi, áp an toàn"), "groups shown in words");
  check(!/conflict_no_base|update_safe|keep_always|ADAPTER_NOT_READY/.test(body), "no machine code on screen");
  check(writes.length === 0, "nothing written by just opening the page");
  await shot("2-plan");

  // 3. three columns for the first item that has versions
  await clickByText("button", "Xem ba bản");
  await page.waitForSelector("table.slds-table");
  const heads = await page.$$eval("table.slds-table th[scope=col]", (h) => h.map((x) => x.textContent.trim()));
  check(["Bản gói cũ", "Của anh", "Bản gói mới"].every((x) => heads.includes(x)), `three columns (${heads.join(" | ")})`);
  await shot("3-three-columns");

  // 4. history section is there
  check((await textOf()).includes("Lịch sử nâng cấp"), "history section present");

  if (APPLY) {
    await clickByText("button", "Áp nâng cấp (");
    await waitText("Sẽ ghi");
    check((await page.$$("[role=dialog]")).length === 0, "the question opens in the page, not in a dialog");
    await shot("4-confirm");
    await clickByText("button", "Áp nâng cấp");
    await waitText("Kết quả lần nâng cấp");
    await new Promise((r) => setTimeout(r, 1200));
    check(writes.length === 1, "exactly one apply request");
    await shot("5-result");
    const after = await textOf();
    check(after.includes("Đã áp"), "result table shows the outcomes");
    await page.reload({ waitUntil: "networkidle2" });
    await waitText("Lịch sử nâng cấp");
    await new Promise((r) => setTimeout(r, 1000));
    check(!(await textOf()).includes("Chưa có lần nâng cấp nào."), "history now has the run");
    await shot("6-history");
  }

  // 5. narrow width, no horizontal overflow
  await page.setViewport({ width: 390, height: 844 });
  await new Promise((r) => setTimeout(r, 500));
  const overflow = await page.evaluate(() => document.scrollingElement.scrollWidth - innerWidth);
  check(overflow <= 0, `no horizontal overflow at 390 (+${overflow})`);
  await shot("7-390");
  check(errors.length === 0, `no page errors ${errors.join(" | ")}`);
} finally {
  await browser.close();
}
process.exit(fails.length ? 1 : 0);
