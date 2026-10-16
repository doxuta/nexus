// specs/073 kh7 T374/T375: the old Studio IA (11 builder routes, /studio/apps, the old header and workspace) is deleted,
// not redirected. HandoverWizard stays until the Founder confirms T376 (Nợ 1: its only UI path to a customer tenant).
import { existsSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = process.cwd();
const GONE = [
  ...["model", "layout", "relationships", "workflows", "approvals", "workqueues", "hooks", "branding", "dashboards", "reports", "deploy", "apps"]
    .map((s) => `app/studio/${s}`),
  "app/admin/doctor",
  "app/admin/api-keys",
  ...["LegacyPanelRedirect", "AppWorkspace", "AppOverview", "StudioHeader", "StudioHome"].map((c) => `components/studio/${c}.tsx`),
  "lib/appPipeline.ts",
];
const KEPT = ["app/studio/verticals", "app/studio/du-an", "app/studio/moi", "app/studio/tai-khoan", "app/admin/page.tsx"];

describe("old Studio IA is gone", () => {
  it.each(GONE)("%s no longer exists", (p) => expect(existsSync(join(ROOT, p))).toBe(false));
  it.each(KEPT)("%s is kept", (p) => expect(existsSync(join(ROOT, p))).toBe(true));
});
