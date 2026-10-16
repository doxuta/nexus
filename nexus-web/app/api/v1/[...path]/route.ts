import type { NextRequest } from "next/server";

// DEV-ONLY API proxy (Layout Engine L2 — browser E2E).
//
// The Go backend (:8090) is CORS fail-closed: it only accepts same-origin / no-Origin requests, so
// a browser on the Next dev port (localhost:3000) is rejected with 403 on EVERY call (login + GETs).
// A plain next.config rewrite doesn't help because it forwards the browser's Origin header verbatim.
//
// This Route Handler runs on the Next SERVER and re-issues the request to the backend with NO Origin
// header (server-to-server fetch), so the browser stays same-origin (/api/v1/*) and the backend sees
// a same-origin/no-Origin call → 200. Guarded by NODE_ENV so it is inert in the standalone prod build
// (prod sets NEXT_PUBLIC_API_BASE to the real backend URL and never hits this handler).
//
// Scope: dev verification only. Not a production proxy.

const TARGET = process.env.DEV_API_PROXY_TARGET || "http://localhost:8090";

// Headers we must NOT forward to the backend (hop-by-hop or origin/host that would re-trigger CORS).
const STRIP = new Set([
  "host",
  "origin",
  "referer",
  "connection",
  "content-length",
  "accept-encoding",
]);

async function proxy(req: NextRequest, path: string[]) {
  if (process.env.NODE_ENV !== "development") {
    return new Response("Not found", { status: 404 });
  }

  const search = req.nextUrl.search; // preserve ?expand=/?sort=/?page= etc.
  const url = `${TARGET}/api/v1/${path.join("/")}${search}`;

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

type Ctx = { params: Promise<{ path: string[] }> };

export async function GET(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return proxy(req, path);
}
export async function POST(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return proxy(req, path);
}
export async function PUT(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return proxy(req, path);
}
export async function PATCH(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return proxy(req, path);
}
export async function DELETE(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return proxy(req, path);
}
