#!/usr/bin/env node
// specs/074 lát f2-rule-toggle-error T123: every rule must survive GET -> PUT verbatim -> GET unchanged
// (except isActive / updatedAt). Exit 1 and name the rule on the first mismatch.
//   NEXUS_API=http://localhost:8613/api/v1 node scripts/check-rule-roundtrip.mjs
// Writes to the rules of the target DB: run it ONLY against a cloned database.
// MUTATE=intervalSeconds drops that key from the PUT body (server keeps the old value: stays green); MUTATE_TO=12345 sends a wrong value instead (must go red).
const API = process.env.NEXUS_API ?? "http://localhost:8613/api/v1";
const MUTATE = process.env.MUTATE;
// Login is rate-limited (429 after a few tries): pass NEXUS_TOKEN to reuse one session across runs.
const token = process.env.NEXUS_TOKEN ?? (process.env.NEXUS_TOKEN_FILE ? (await import("node:fs")).readFileSync(process.env.NEXUS_TOKEN_FILE, "utf8").trim() : null) ?? (await (await fetch(`${API}/auth/login`, {
  method: "POST", headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ tenant: process.env.TENANT ?? "northwind", email: "admin@example.com", password: "Demo-Admin-2026!" }),
})).json()).data.accessToken;
const call = async (method, path, body) => {
  const r = await fetch(`${API}${path}`, { method, headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  const j = await r.json().catch(() => ({}));
  return { status: r.status, data: j.data, err: j };
};
const deepSort = (v) => Array.isArray(v) ? v.map(deepSort) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, deepSort(v[k])])) : v;
const norm = (w) => { const { isActive, updatedAt, ...rest } = w; return JSON.stringify(deepSort(rest)); };

const list = await call("GET", "/admin/workflows?limit=500");
const rules = list.data?.workflows ?? [];
let bad = 0;
for (const r of rules) {
  const before = (await call("GET", `/admin/workflows/${r.id}`)).data?.workflow;
  const body = structuredClone(before);
  if (MUTATE) { delete body[MUTATE]; if (process.env.MUTATE_TO) body[MUTATE] = Number(process.env.MUTATE_TO); }
  const put = await call("PUT", `/admin/workflows/${r.id}`, body);
  const after = (await call("GET", `/admin/workflows/${r.id}`)).data?.workflow;
  if (put.status >= 300 || norm(before) !== norm(after)) {
    bad++;
    console.log(`FAIL ${r.id} "${r.name}" PUT=${put.status} ${put.status >= 300 ? JSON.stringify(put.err).slice(0, 200) : "differs after round trip"}`);
  } else console.log(`PASS ${r.id} "${r.name}"`);
}
console.log(`${rules.length} rules, ${bad} mismatched`);
process.exit(bad ? 1 : 0);
