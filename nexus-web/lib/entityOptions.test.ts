import { describe, expect, it } from "vitest";
import { entityOptions, entityOptionText } from "@/lib/entityOptions";

const E = (name: string, label: string) => ({ name, label, tableName: `obj_${name}` });
const apps = new Map([
  ["crm_customers", [{ name: "crm", label: "CRM" }]],
  ["dental_customers", [{ name: "nhakhoa", label: "Nha khoa" }]],
  ["crm_deals", [{ name: "crm", label: "CRM" }]],
]);

describe("entityOptions (#301)", () => {
  const ents = [E("dental_customers", "Khách hàng"), E("crm_customers", "Khách hàng"), E("crm_deals", "Cơ hội")];

  it("duplicate labels get code + app name as meta; unique labels stay as before", () => {
    const o = entityOptions(ents, apps);
    expect(o.find((x) => x.value === "crm_customers")).toMatchObject({ label: "Khách hàng", meta: "crm_customers · CRM" });
    expect(o.find((x) => x.value === "dental_customers")?.meta).toBe("dental_customers · Nha khoa");
    expect(o.find((x) => x.value === "crm_deals")).toEqual({ value: "crm_deals", label: "Cơ hội" });
  });

  it("duplicate with no app known still shows the code", () => {
    const o = entityOptions(ents, new Map());
    expect(o[0].meta).toBe("dental_customers");
  });

  it("preferApp puts that app's tables first, order otherwise stable", () => {
    const o = entityOptions(ents, apps, { preferApp: "crm" });
    expect(o.map((x) => x.value)).toEqual(["crm_customers", "crm_deals", "dental_customers"]);
    expect(entityOptions(ents, apps).map((x) => x.value)).toEqual(["dental_customers", "crm_customers", "crm_deals"]);
  });

  it("entityOptionText flattens label + meta for native selects", () => {
    const [a, , c] = entityOptions(ents, apps);
    expect(entityOptionText(a)).toBe("Khách hàng · dental_customers · Nha khoa");
    expect(entityOptionText(c)).toBe("Cơ hội");
  });
});

it("preferAppOf puts the tables sharing an app with the named table first", () => {
  const ents = [E("dental_customers", "Khách hàng"), E("crm_customers", "Khách hàng"), E("crm_deals", "Cơ hội")];
  expect(entityOptions(ents, apps, { preferAppOf: "crm_deals" }).map((x) => x.value)).toEqual(["crm_customers", "crm_deals", "dental_customers"]);
});
