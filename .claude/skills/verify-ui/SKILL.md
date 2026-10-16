---
name: verify-ui
description: Prove a Nexus UI change works in a real browser — start your own server on a throwaway database, take screenshots of the affected routes with scripts/ui-shots.mjs and click through the flow with puppeteer as the non-admin rep. Use when a change touches nexus-web, or when the README needs fresh screenshots or a GIF.
---

# Verify a UI change

1. **Start the stack.** Either the one-command demo (`docker compose up --build`, then `http://localhost:8080`),
   or your own processes: the API on a free port against your own database, and
   `NEXT_PUBLIC_API_BASE=http://127.0.0.1:<api-port>/api/v1 npx next dev --webpack -p <web-port>` in `nexus-web/`.
   Check every listener is the PID you started (`lsof -iTCP:<port> -sTCP:LISTEN -nP`).
2. **Screenshots.**
   `NEXUS_API=<api base> NEXUS_FE=<web origin> node scripts/ui-shots.mjs <out-dir> [route-filter] --widths 1440,390`
   (from `nexus-web/`). It logs in as each persona through the real API and writes `manifest.json` and `report.json`
   (page errors and horizontal overflow per shot). A shot with a page error or overflow is a failure.
3. **Click-through as the rep.** Write a short puppeteer-core script (system Chrome, same login helper as
   `ui-shots.mjs`): do the user action, assert the visible text or state that proves it worked, screenshot it.
   Run the denied path too — the rep must be stopped where the access rules say so.
4. **Report** the screenshot paths, the click-through output and `report.json` totals.
5. **Clean up** — stop exactly the PIDs you started.
