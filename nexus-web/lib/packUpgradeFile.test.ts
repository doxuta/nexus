// specs/079 f7-nang-cap-giu-tep: a pack file is checked in the browser, before any request.
import { describe, expect, it } from "vitest";
import { checkUpgradeFile } from "./packUpgrade";

const file = (manifest: Record<string, unknown>) => JSON.stringify({ manifest, entities: [{ name: "a" }] });
const err = (r: ReturnType<typeof checkUpgradeFile>) => (r.ok ? "" : r.error);

describe("checkUpgradeFile", () => {
  it("same pack, equal or newer version: ok", () => {
    expect(checkUpgradeFile(file({ slug: "crm", version: "1.8.0" }), "crm", "1.7.0").ok).toBe(true);
    expect(checkUpgradeFile(file({ slug: "crm", version: "1.7.0" }), "crm", "1.7.0").ok).toBe(true);
  });
  it("no slug / other pack / older version / not JSON: a Vietnamese sentence", () => {
    expect(err(checkUpgradeFile(file({ version: "1.8.0" }), "crm", "1.7.0"))).toMatch(/mã gói/);
    expect(err(checkUpgradeFile(file({ slug: "lms", version: "1.8.0" }), "crm", "1.7.0"))).toMatch(/lms/);
    expect(err(checkUpgradeFile(file({ slug: "crm", version: "1.6.0" }), "crm", "1.7.0"))).toMatch(/cũ hơn/);
    expect(err(checkUpgradeFile("not json", "crm", "1.7.0"))).toMatch(/JSON/);
  });
});
