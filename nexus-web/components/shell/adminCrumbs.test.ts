// The admin crumb labels: a segment missing from ADMIN_LABELS is capitalized from its machine name.
import { describe, it, expect } from "vitest";
import { adminCrumbs } from "@/components/shell/AppShell";

describe("adminCrumbs", () => {
  it("labels /admin/email-log 'Quản trị / Nhật ký email', not 'Email-log'", () => {
    expect(adminCrumbs(["admin", "email-log"]).map((c) => c.label)).toEqual(["Quản trị", "Nhật ký email"]);
  });
});
