// specs/079 f7-sao-luu-bang T089: the pure words of the backup page.
import { describe, expect, it } from "vitest";
import type { AccessChanges, TenantBackup } from "@/lib/api";
import { backupDataText, backupFileStem, backupKindLabel, backupSizeText, backupWarningText, notRestorableText, restoreLock, accessChangesRows, restoreWarningText, tableLabel, restoreStepIndex, restoreFailureText, restoreReportGroups } from "./maintenance";

const B = (x: Partial<TenantBackup> = {}): TenantBackup => ({
  id: "b1", tenantId: "t1", kind: "manual", status: "ready", note: null, sizeBytes: 1536, sha256: null, schemaVersion: 1, formatVersion: 1,
  rowCount: 12400, tableCount: 5, fileCount: 320, blobBytes: 0, warnings: [], error: null, createdAt: "2026-10-14T08:05:00Z", finishedAt: null,
  createdBy: null, restorable: true, notRestorableReason: null, ...x,
});

describe("maintenance words", () => {
  it.each([
    ["SELF", "Không khôi phục được chính xưởng Nexus."],
    ["SCHEMA_MISMATCH", "Bản này tạo ở phiên bản cấu trúc khác; chưa khôi phục được."],
    ["FILE_MISSING", "Tệp của bản này không còn trên máy chủ."],
    ["NOT_READY", "Bản này chưa sẵn sàng."],
    ["TENANT_RETIRED", "Khách này đã ngưng."],
  ] as const)("notRestorableText %s", (r, want) => expect(notRestorableText(r)).toBe(want));

  it("kind", () => {
    expect(backupKindLabel("manual")).toBe("Thủ công");
    expect(backupKindLabel("scheduled")).toBe("Theo lịch");
    expect(backupKindLabel("pre_restore")).toBe("Tự sao lưu trước khi khôi phục");
  });

  it("warning, size, data", () => {
    expect(backupWarningText([{ code: "BLOB_MISSING", count: 3 }])).toBe("3 tệp đính kèm không tìm thấy");
    expect(backupWarningText([])).toBe("");
    expect(backupSizeText(B())).toBe("1,5 KB");
    expect(backupSizeText(B({ sizeBytes: null }))).toBe("—");
    expect(backupDataText(B())).toBe("12.400 bản ghi · 320 tệp");
    expect(backupDataText(B({ rowCount: null }))).toBe("");
  });

  it("file stem is <slug>-<yyyymmdd-hhmm>", () => {
    expect(backupFileStem("acme", new Date(2026, 9, 14, 8, 5).toISOString())).toBe("acme-20261014-0805");
  });

  it("restoreLock: only restorable === true unlocks; the reason says why", () => {
    expect(restoreLock(B())).toBeNull();
    expect(restoreLock(B({ restorable: false, notRestorableReason: "SELF" }))).toBe("Không khôi phục được chính xưởng Nexus.");
    expect(restoreLock(B({ restorable: undefined as unknown as boolean }))).toBe("Chưa biết bản này khôi phục được không.");
  });
});

// specs/079 f7-khoi-phuc-ke-hoach: the pure sentences of the restore plan page.

const NONE: AccessChanges = {
  total: 0, roleGrantsWidened: 0, fieldAclReopened: 0, sharesRegranted: 0, membershipsReadded: 0,
  sensitiveFieldsCleared: 0, workflowsReenabled: 0, outboundDestinationsReenabled: 0, outboundHosts: [], usersAffected: 0, apiKeysAffected: 0,
};

describe("restoreWarningText", () => {
  it.each([
    ["ROLE_LOST", 3, "3 người dùng sẽ mất vai"],
    ["MEMBERSHIP_SKIPPED", 2, "2 dòng thành viên đội sẽ bị bỏ"],
    ["PENDING_JOBS_KEPT", 5, "5 việc nền đang chờ được giữ nguyên"],
    ["BLOB_MISSING_IN_BACKUP", 1, "1 tệp đính kèm không có trong bản sao lưu"],
    ["WEBHOOK_KEY_DROPPED", 4, "4 luật gửi dữ liệu ra ngoài sẽ không còn khoá ký"],
  ] as const)("%s", (code, n, text) => {
    expect(restoreWarningText(code, n)).toContain(text);
  });
  it("ACCESS_WIDENED has no line of its own (the access block carries it)", () => {
    expect(restoreWarningText("ACCESS_WIDENED", 9)).toBeNull();
  });
  it("an unknown code is dropped, never printed raw", () => {
    expect(restoreWarningText("SOMETHING_NEW" as never, 1)).toBeNull();
  });
});

describe("accessChangesRows", () => {
  it("lists only kinds above zero, in the spec order", () => {
    const rows = accessChangesRows({ ...NONE, total: 5, workflowsReenabled: 2, roleGrantsWidened: 3 });
    expect(rows).toEqual(["3 quyền theo vai", "2 luật tự động bật lại"]);
  });
  it("all zero -> no rows", () => {
    expect(accessChangesRows(NONE)).toEqual([]);
  });
});

describe("tableLabel", () => {
  it("shows the server label as is", () => expect(tableLabel({ table: "customers", label: "Khách hàng" })).toBe("Khách hàng"));
  it("never prints an obj_* table name", () => {
    for (const [table, label] of [["sys_business_calendars", "sys_business_calendars"], ["acl_roles", "acl_roles"], ["team_memberships", ""], ["sys_files", "sys_files"]])
      expect(tableLabel({ table, label })).toBe("Bảng chưa đặt tên");
    expect(tableLabel({ table: "obj_ab12cd", label: "" })).not.toMatch(/obj_/);
    expect(tableLabel({ table: "obj_ab12cd", label: "obj_ab12cd" })).not.toMatch(/obj_/);
  });
});

type R = NonNullable<TenantBackup["restore"]>;
const RS = (x: Partial<R> = {}): R => ({ sourceBackupId: "s", status: "running", step: null, committedAt: null, report: null, finishedAt: null, ...x });

describe("restoreStepIndex (the step the server wrote, never guessed from time)", () => {
  it.each([["lock", 0], ["prebackup", 1], ["apply", 2], ["doctor", 3]] as const)("running at %s -> %i", (step, i) => {
    expect(restoreStepIndex(RS({ step }))).toBe(i);
  });
  it("running with no step yet -> 0", () => expect(restoreStepIndex(RS())).toBe(0));
  it("done -> 4 (all four steps finished)", () => expect(restoreStepIndex(RS({ status: "done", step: "doctor" }))).toBe(4));
  it("failed -> the recorded step; none recorded -> 0", () => {
    expect(restoreStepIndex(RS({ status: "failed", step: "apply" }))).toBe(2);
    expect(restoreStepIndex(RS({ status: "failed", step: null }))).toBe(0);
  });
  it("no restore block -> -1", () => {
    expect(restoreStepIndex(null)).toBe(-1);
    expect(restoreStepIndex(undefined)).toBe(-1);
  });
});

describe("restoreFailureText", () => {
  it("before committedAt: the customer's data did not change", () => {
    expect(restoreFailureText(RS({ status: "failed", step: "prebackup" }), "TENANT_RESTORE_FAILED")).toMatch(/dữ liệu của khách không đổi/);
  });
  it("after committedAt: says the data HAS changed (and never says it did not)", () => {
    const s = restoreFailureText(RS({ status: "failed", step: "doctor", committedAt: "2026-10-14T08:00:00Z" }), "TENANT_RESTORE_FAILED");
    expect(s).toMatch(/dữ liệu đã đổi/);
    expect(s).not.toMatch(/không đổi/);
  });
  it("PREBACKUP_FAILED: the customer was not touched; CONFLICT has its own sentence", () => {
    expect(restoreFailureText(RS({ status: "failed", step: "prebackup" }), "TENANT_RESTORE_PREBACKUP_FAILED")).toMatch(/không bị đụng gì/);
    expect(restoreFailureText(RS({ status: "failed", step: "lock" }), "TENANT_RESTORE_CONFLICT")).toMatch(/đang đổi/);
  });
  it("never prints a machine code", () => {
    expect(restoreFailureText(RS({ status: "failed" }), "TENANT_RESTORE_FAILED")).not.toMatch(/TENANT_RESTORE/);
    expect(restoreFailureText(RS({ status: "failed" }), "SOMETHING_NEW")).not.toMatch(/SOMETHING_NEW/);
  });
});

describe("restoreReportGroups", () => {
  it("sums rows per group by table class and never keeps a physical name", () => {
    const g = restoreReportGroups({ deleted: { obj_a: 5, obj_b: 1, sys_workflows: 2, acl_roles: 3 }, inserted: { obj_a: 7, sys_workflows: 2 } });
    expect(g).toEqual([
      { group: "data", deleted: 6, inserted: 7 },
      { group: "config", deleted: 2, inserted: 2 },
      { group: "access", deleted: 3, inserted: 0 },
    ]);
  });
  it("classifies like the server policy: rel_ and data-class sys_ tables are data, sys_files is files", () => {
    const g = restoreReportGroups({ deleted: { rel_a_b: 4, sys_record_notes: 1, sys_notifications: 1, sys_unique_index: 1, sys_sample_records: 1, sys_sequences: 1, sys_files: 9, team_memberships: 2, sys_field_acl: 1 }, inserted: {} });
    expect(g).toEqual([
      { group: "data", deleted: 9, inserted: 0 },
      { group: "access", deleted: 3, inserted: 0 },
      { group: "files", deleted: 9, inserted: 0 },
    ]);
  });
  it("empty report -> no rows", () => expect(restoreReportGroups({})).toEqual([]));
});
