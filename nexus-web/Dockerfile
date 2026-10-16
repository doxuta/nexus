# Multi-stage production build for nexus-web (Next.js 16 standalone).
#
# Stages:
#   deps   — install production + dev deps once (layer-cached).
#   build  — compile the app with next build; output: 'standalone' bundles server + traces.
#   runner — minimal runtime image (no node_modules, non-root user).
#
# Usage (from repo root or via compose):
#   docker build -t nexus-web:prod nexus-web/
#   docker build --build-arg NEXT_PUBLIC_API_BASE=/api/v1 -t nexus-web:prod nexus-web/
#
# NEXT_PUBLIC_API_BASE is baked at BUILD time (Next.js inlines NEXT_PUBLIC_* at compile time).
# In production it must be the relative path /api/v1 so browser calls are same-origin (handled
# by Caddy, which strips /api prefix before forwarding to the Go backend).

# ── Stage 1: install dependencies ────────────────────────────────────────────
FROM node:22-alpine AS deps
WORKDIR /app

# Copy manifests first for layer-cache efficiency: deps only reinstall when lockfile changes.
COPY package.json package-lock.json ./
RUN npm ci --prefer-offline

# ── Stage 2: build ───────────────────────────────────────────────────────────
FROM node:22-alpine AS build
WORKDIR /app

# Allow the API base to be overridden at build time (e.g. for staging pointing at a different URL).
# Default is /api/v1 (same-origin, behind Caddy) — the only correct value for production.
ARG NEXT_PUBLIC_API_BASE=/api/v1
ENV NEXT_PUBLIC_API_BASE=${NEXT_PUBLIC_API_BASE}

# PRODUCTION turns OFF the demo auto-login (which signs in with seed admin creds). With this set
# to false, the app requires a real login at /login. Override to "true" only for a throwaway demo build.
ARG NEXT_PUBLIC_DEMO_MODE=false
ENV NEXT_PUBLIC_DEMO_MODE=${NEXT_PUBLIC_DEMO_MODE}

# Bring in installed node_modules from deps stage.
COPY --from=deps /app/node_modules ./node_modules

# Copy all source files (node_modules + .next excluded via .dockerignore).
COPY . .

# Compile. next.config.ts sets output: 'standalone', so the build emits:
#   .next/standalone/  — self-contained server (no node_modules install at runtime)
#   .next/static/      — hashed client-side assets (JS/CSS chunks)
RUN npm run build

# ── Stage 3: minimal runtime ─────────────────────────────────────────────────
FROM node:22-alpine AS runner
WORKDIR /app

# Non-root user (UID 1001 — avoids collision with node default UID 1000).
RUN addgroup --system --gid 1001 nodejs && \
    adduser  --system --uid 1001 --ingroup nodejs nextjs

# Copy the self-contained standalone server tree.
COPY --from=build --chown=nextjs:nodejs /app/.next/standalone ./

# Serve static assets locally: standalone/server.js will pick these up automatically
# when the directories exist alongside the bundle.
COPY --from=build --chown=nextjs:nodejs /app/.next/static ./.next/static
COPY --from=build --chown=nextjs:nodejs /app/public        ./public

USER nextjs

# PORT and HOSTNAME must be set so the standalone server binds on all interfaces
# (default is 127.0.0.1, which is unreachable from other containers in the compose network).
EXPOSE 3000
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

CMD ["node", "server.js"]
