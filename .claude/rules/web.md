---
description: Web conventions for nexus-web
paths:
  - "nexus-web/**"
---

# Web conventions

- Next.js 16 differs from older versions: read `nexus-web/AGENTS.md` and the guide in `node_modules/next/dist/docs/`
  before using an API you have not used here yet.
- Dev server: `npx next dev --webpack` (Turbopack is not used here). The API base is `NEXT_PUBLIC_API_BASE`
  (build-time); in development `app/api/v1/[...path]` proxies to `DEV_API_PROXY_TARGET`.
- Look: SLDS classes and the components in `components/slds` first; Tailwind only for layout. No new color tokens.
  Studio gates (`test/studio-*.test.*`, `lib/i18n/studio-strings.test.ts`) fail on new raw styles, new modals for
  create flows and raw strings; their ratchets live in `test/studio-gates.allowlist.json`.
- Strings: a new user-facing string goes through `lib/i18n` (`t("key")`) with the key in both `vi.ts` and `en.ts`.
  `lib/i18n/studio-strings.test.ts` enforces it in the Studio; older customer screens still hold literals — move a
  screen's strings when you rework that screen, not in passing.
- Data: API calls live in `lib/api.ts` / `lib/admin.ts`; components do not call `fetch` directly.
- A UI change is verified by clicking it as the non-admin rep (skill `verify-ui`), not by reading the diff.
