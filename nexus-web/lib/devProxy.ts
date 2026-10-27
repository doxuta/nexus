import type { NextRequest } from "next/server";

// DEV-ONLY proxy to the Go backend, behind app/api/v1/[...path] (Layout Engine L2 — browser E2E) and app/mcp (issue #22).
//
// The Go backend (:8090) is CORS fail-closed: it only accepts same-origin / no-Origin requests, so
// a browser on the Next dev port (localhost:3000) is rejected with 403 on EVERY call (login + GETs).
// A plain next.config rewrite doesn't help because it forwards the browser's Origin header verbatim.
//
// The Route Handlers run on the Next SERVER and re-issue the request to the backend with NO Origin
// header (server-to-server fetch), so the browser stays same-origin (/api/v1/*) and the backend sees
// a same-origin/no-Origin call → 200. /mcp rides the same proxy so the `claude mcp add` line the Studio
// prints on `next dev` (lib/admin.ts mcpEndpoint: the dev origin + /mcp) reaches the API. Guarded by
// NODE_ENV so it is inert in the standalone prod build (prod sets NEXT_PUBLIC_API_BASE to the real
// backend URL, and the demo's Caddy routes /api and /mcp to the API before Next).
//
// Scope: dev verification only. Not a production proxy.

// Headers we must NOT forward to the backend (hop-by-hop or origin/host that would re-trigger CORS).
const STRIP = new Set([
  "host",
  "origin",
  "referer",
  "connection",
  "content-length",
  "accept-encoding",
]);

/** Re-issues `req` to the backend at `path` (its query string kept); 404 outside `next dev`. */
export async function devProxy(req: NextRequest, path: string): Promise<Response> {
  if (process.env.NODE_ENV !== "development") {
    return new Response("Not found", { status: 404 });
  }

  const target = process.env.DEV_API_PROXY_TARGET || "http://localhost:8090";
  const url = `${target}${path}${req.nextUrl.search}`; // preserve ?expand=/?sort=/?page= etc.

  const headers = new Headers();
  req.headers.forEach((value, key) => {
    if (!STRIP.has(key.toLowerCase())) headers.set(key, value);
  });

  const hasBody = req.method !== "GET" && req.method !== "HEAD";
  const body = hasBody ? await req.arrayBuffer() : undefined;

  const upstream = await fetch(url, {
    method: req.method,
    headers,
    body,
    redirect: "manual",
  });

  // Mirror the upstream response (status + body + content-type) back to the browser, same-origin.
  const resHeaders = new Headers();
  const ct = upstream.headers.get("content-type");
  if (ct) resHeaders.set("content-type", ct);
  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: resHeaders,
  });
}
