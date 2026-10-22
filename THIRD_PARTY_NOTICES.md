# Third-party notices

Nexus ships the third-party material listed here. Every entry records the name, the version, the licence,
and where it lives in this repository or in the built product. Code that is only *read for study* (Twenty,
Plane, NocoDB, ToolJet — AGPL/SSPL/fair-code) is never copied into the product and is not listed.

Policy (campaign XƯỞNG HOÀN CHỈNH, decision D1): only MIT, Apache-2.0, BSD, ISC or OFL material may be
vendored or bundled. Apache-2.0 material additionally carries its NOTICE file and a note of every file we
changed. Add a row here in the same commit that brings the material in.

| Material | Version | Licence | Where | Notes |
|---|---|---|---|---|
| Salesforce Lightning Design System — CSS/SCSS (`@salesforce-ux/design-system`) | 2.264.1 | BSD-3-Clause | `nexus-web/node_modules/@salesforce-ux/design-system`, bundled into the web build's CSS | Copyright (c) 2015, salesforce.com, inc. All rights reserved. Redistribution and use in source and binary forms, with or without modification, are permitted provided that the conditions of the BSD 3-Clause licence are met; see `node_modules/@salesforce-ux/design-system/README.md` §Licenses. The name of salesforce.com may not be used to endorse or promote Nexus. At build time the CSS is transformed so literal brand colours read tenant branding variables (a modification; permitted by BSD-3). |
| SLDS icons and images (`assets/icons`, `assets/images`) | — | CC BY-ND 4.0 | **Not used** | Deliberately not shipped: an open alternative (Lucide) exists, so the no-derivatives licence is avoided (campaign D3 + §6). SLDS icon *container* classes (colour tiles) are CSS and fall under BSD-3 above. |
| SLDS blueprint markup | 2.264.1 | BSD-3-Clause | `nexus-web/components/slds/**` (markup structure and class names reproduced in React wrappers) | Rendered for reference by `nexus-web/scripts/render-slds-blueprints.cjs` from the showcase bundles in the npm package. |
| Lucide icons (`lucide-react`) | ^1.21 | ISC | `nexus-web/node_modules/lucide-react`, bundled into the web build | Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as part of Feather (MIT). All other copyright (c) for Lucide are held by Lucide Contributors 2022. |
| Inter typeface | Google Fonts (variable `opsz,wght`; subsets latin + vietnamese, about 88 KB woff2 in total; `app/layout.tsx` loads `axes: ["opsz"]`) | SIL Open Font License 1.1 | self-hosted by `next/font/google` in the web build | Copyright (c) 2016 The Inter Project Authors. https://github.com/rsms/inter |
| gstack `/careful` guardrail (github.com/garrytan/gstack) | ported 2026-09 | MIT | `tools/careful/check-careful.{sh,py}` (moves to `.claude/hooks/` once a human relocates it; the hook refuses agent writes there) | Copyright (c) 2026 Garry Tan. MIT License, full text at github.com/garrytan/gstack/blob/main/LICENSE. Changed: telemetry and session scoping dropped; Go/MySQL/golang-migrate/docker patterns, the Write/Edit guardrail-file tier and the time cap added. |
