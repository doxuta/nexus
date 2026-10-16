import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { maintainErrorText } from "./maintainErrors";

const E = (status: number, code: string, details: Record<string, unknown> = {}) => new ApiError(status, code, "msg máy chủ", details);
const CODES = ["TENANT_RESTORE_SCHEMA_MISMATCH", "BACKUP_DISK_LOW", "PACK_UPGRADE_IN_PROGRESS", "TENANT_RESTORE_TOO_LARGE", "ROLLBACK_NO_SNAPSHOT"];

describe("maintainErrorText", () => {
  it("schema mismatch carries both numbers", () => {
    const s = maintainErrorText(E(409, "TENANT_RESTORE_SCHEMA_MISMATCH", { backup: 7, current: 9 }));
    expect(s).toContain("7");
    expect(s).toContain("9");
  });
  it("disk low shows readable sizes", () => {
    const s = maintainErrorText(E(507, "BACKUP_DISK_LOW", { needBytes: 5 * 1024 * 1024, freeBytes: 1024 * 1024 }));
    expect(s).toMatch(/MB/);
  });
  it("upgrade in progress shows start time", () => {
    const s = maintainErrorText(E(409, "PACK_UPGRADE_IN_PROGRESS", { runId: "r1", startedAt: "2026-10-14T03:04:05Z" }));
    expect(s).toMatch(/\d{1,2}:\d{2}/);
  });
  it("too large", () => {
    const s = maintainErrorText(E(413, "TENANT_RESTORE_TOO_LARGE", { rows: 120, limit: 100 }));
    expect(s).toContain("120");
    expect(s).toContain("100");
  });
  it("no snapshot names doc types with business labels", () => {
    const s = maintainErrorText(E(409, "ROLLBACK_NO_SNAPSHOT", { docTypes: ["layout"] }));
    expect(s).not.toContain("layout");
    expect(s).toMatch(/\S/);
  });
  it("missing details drops the clause, never prints braces", () => {
    for (const c of CODES) expect(maintainErrorText(E(409, c))).not.toMatch(/[{}]/);
  });
  it("unknown code falls back to the server sentence", () => {
    expect(maintainErrorText(E(400, "SOMETHING_ELSE"))).toBeTruthy();
  });
  it("non-ApiError gives a generic sentence", () => {
    expect(maintainErrorText("boom")).toBeTruthy();
    expect(maintainErrorText(undefined)).toBeTruthy();
  });
  it("never contains machine codes", () => {
    for (const c of CODES) expect(maintainErrorText(E(409, c, { backup: 1, current: 2 }))).not.toMatch(/TENANT_|BACKUP_|PACK_UPGRADE_|ROLLBACK_/);
  });
});
