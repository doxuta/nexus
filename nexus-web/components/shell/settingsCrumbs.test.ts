// specs/065: /settings/users reads "Cài đặt / Người dùng"; "users" stays out of STATIC_LABELS so a tenant entity
// named users is still an entity (isEntitySegment).
import { describe, it, expect } from "vitest";
import { staticCrumbs } from "@/components/shell/AppShell";

describe("settings crumbs", () => {
  it("labels /settings/users 'Cài đặt / Người dùng' with live links", () => {
    expect(staticCrumbs(["settings", "users"])).toEqual([
      { label: "Cài đặt", href: "/settings" },
      { label: "Người dùng", href: "/settings/users" },
    ]);
  });

  it("does not rename a top-level 'users' segment (that would be an entity)", () => {
    expect(staticCrumbs(["users"]).map((c) => c.label)).toEqual(["Users"]);
  });
});
