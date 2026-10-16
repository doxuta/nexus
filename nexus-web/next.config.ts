import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Produce a self-contained .next/standalone bundle suitable for Docker (no node_modules install
  // needed at runtime). The standalone server.js is copied into the runner image; static assets
  // (.next/static + public/) are copied alongside it so the server picks them up automatically.
  output: "standalone",
  // Hide the Next.js dev overlay/indicator badge — keeps the dev-served demo looking clean
  // (no floating "N" dev badge in the corner during a live demo). Inert in prod builds anyway.
  devIndicators: false,
  // DEV-ONLY API proxy lives in app/api/v1/[...path]/route.ts (a Route Handler), NOT a rewrite.
  // The backend is CORS fail-closed, so a rewrite (which forwards the browser Origin) gets 403; the
  // Route Handler re-issues the request server-side with the Origin stripped → same-origin → 200.
  // It is NODE_ENV-guarded and inert in the standalone prod build (prod uses NEXT_PUBLIC_API_BASE).
};

export default nextConfig;
