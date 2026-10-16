import { describe, expect, it } from "vitest";
import { inEntityScope, roleTouchesScope } from "@/lib/workshopScope";
import type { RoleGrant } from "@/lib/api";

const g = (entityName: string, accessLevel: RoleGrant["accessLevel"] = "all"): RoleGrant => ({ entityName, action: "view", accessLevel });
const app = new Set(["clinic_patients", "clinic_visits"]);

describe("roleTouchesScope (#302)", () => {
  it("keeps a role granting on one of the app's tables", () => {
    expect(roleTouchesScope([g("clinic_patients")], app)).toBe(true);
  });
  it("hides a role whose grants are all on another app", () => {
    expect(roleTouchesScope([g("crm_accounts"), g("crm_contacts")], app)).toBe(false);
  });
  it("keeps a wildcard role (it reaches this app)", () => {
    expect(roleTouchesScope([g("*")], app)).toBe(true);
  });
  it("keeps a role with no grant yet (just created in the workshop)", () => {
    expect(roleTouchesScope([], app)).toBe(true);
    expect(roleTouchesScope([g("crm_accounts", "none")], app)).toBe(true);
  });
  it("a 'none' grant on the app's table does not count as touching it", () => {
    expect(roleTouchesScope([g("clinic_patients", "none"), g("crm_accounts")], app)).toBe(false);
  });
});

describe("inEntityScope (#302)", () => {
  const rows = [{ e: "clinic_visits" }, { e: "crm_accounts" }, { e: undefined }];
  it("null scope passes everything", () => {
    expect(inEntityScope(rows, null, (r) => r.e)).toHaveLength(3);
  });
  it("filters foreign rows but keeps unattributable ones", () => {
    expect(inEntityScope(rows, app, (r) => r.e).map((r) => r.e)).toEqual(["clinic_visits", undefined]);
  });
});
