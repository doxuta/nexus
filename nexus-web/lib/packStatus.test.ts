// specs/079 f7-nang-cap-hien-co T029: installed pack version vs the version shipped with Nexus.
import { describe, expect, it } from "vitest";
import type { BuiltinVertical } from "@/lib/api";
import type { InstallRecord } from "@/lib/verticalPlan";
import { comparePackVersions, hasPackUpgrade, latestInstallBySlug, packStatus } from "./packStatus";

const inst = (slug: string, version: string, installedAt = "2026-10-01T00:00:00Z"): InstallRecord => ({ id: `${slug}-${version}`, slug, version, installedAt });
const built = (slug: string, version: string): BuiltinVertical => ({ slug, manifest: { name: slug, version } as BuiltinVertical["manifest"], entityCount: 1 });

describe("comparePackVersions", () => {
  it.each([
    ["1.8.0", "1.7.0", 1],
    ["1.7.0", "1.7.0", 0],
    ["1.10.0", "1.9.0", 1],
    ["1.2.0", "1.10.0", -1],
  ])("%s vs %s", (a, b, want) => expect(Math.sign(comparePackVersions(a, b) as number)).toBe(want));
  it.each([["", "1.0.0"], ["1.0", "1.0.0"], ["1.0.0-rc1", "1.0.0"], ["v1.0.0", "1.0.0"]])("%j is invalid", (a, b) => expect(comparePackVersions(a, b)).toBeNull());
});

describe("packStatus", () => {
  const avail = [built("crm", "1.8.0")];
  it("newer", () => expect(packStatus(inst("crm", "1.7.0"), avail)).toEqual({ kind: "newer", installed: "1.7.0", available: "1.8.0" }));
  it("same", () => expect(packStatus(inst("crm", "1.8.0"), avail).kind).toBe("same"));
  it("local-newer", () => expect(packStatus(inst("crm", "1.9.0"), avail).kind).toBe("local-newer"));
  it("not-available", () => expect(packStatus(inst("lms", "1.0.0"), avail).kind).toBe("not-available"));
  it("from-file", () => expect(packStatus(inst("", "1.0.0"), avail).kind).toBe("from-file"));
  it("invalid", () => expect(packStatus(inst("crm", ""), avail).kind).toBe("invalid"));
});

describe("latestInstallBySlug", () => {
  it("keeps only the newest row per pack, so an old row never reports a false upgrade", () => {
    const rows = [inst("crm", "1.7.0", "2026-10-02T00:00:00Z"), inst("crm", "1.6.0", "2026-09-01T00:00:00Z"), inst("lms", "1.0.0")];
    const got = latestInstallBySlug(rows);
    expect(got.map((r) => `${r.slug}@${r.version}`).sort()).toEqual(["crm@1.7.0", "lms@1.0.0"]);
  });
});

describe("hasPackUpgrade", () => {
  it("true only when a latest install is older than the shipped one", () => {
    expect(hasPackUpgrade([inst("crm", "1.7.0")], [built("crm", "1.8.0")])).toBe(true);
    expect(hasPackUpgrade([inst("crm", "1.6.0", "2026-09-01T00:00:00Z"), inst("crm", "1.8.0", "2026-10-02T00:00:00Z")], [built("crm", "1.8.0")])).toBe(false);
    expect(hasPackUpgrade([], [built("crm", "1.8.0")])).toBe(false);
  });
});
