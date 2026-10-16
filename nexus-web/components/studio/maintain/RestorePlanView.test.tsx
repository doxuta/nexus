// specs/079 f7-khoi-phuc-ke-hoach: the restore plan, drawn from the server's answer. Pure view: no network.
import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { RestorePlan } from "@/lib/api";
import { RestorePlanView } from "./RestorePlanView";

const ACCESS0 = {
  total: 0, roleGrantsWidened: 0, fieldAclReopened: 0, sharesRegranted: 0, membershipsReadded: 0,
  sensitiveFieldsCleared: 0, workflowsReenabled: 0, outboundDestinationsReenabled: 0, outboundHosts: [], usersAffected: 0, apiKeysAffected: 0,
};
const PLAN = (extra: Partial<RestorePlan> = {}): RestorePlan => ({
  backupId: "b1", backupCreatedAt: "2026-10-10T03:00:00Z", integrity: { ok: true }, schema: { backup: 12, current: 12, ok: true },
  groups: [
    { group: "data", label: "Dữ liệu", tables: [{ table: "obj_9f2a", label: "Khách hàng", rowsInBackup: 1200, rowsNow: 1350 }] },
    { group: "config", label: "Cấu hình", tables: [{ table: "sys_fields", label: "Trường", rowsInBackup: 40, rowsNow: 44 }] },
    { group: "access", label: "Quyền", tables: [{ table: "sys_roles", label: "Vai", rowsInBackup: 3, rowsNow: 3 }] },
    { group: "files", label: "Tệp", tables: [] },
  ],
  blobs: { toWrite: 7, bytesToWrite: 2048, toRemove: 2 },
  warnings: [{ code: "ROLE_LOST", count: 2 }, { code: "ACCESS_WIDENED", count: 4 }],
  accessChanges: ACCESS0,
  kept: ["Mật khẩu và phiên đăng nhập", "Lịch sử xuất bản"],
  preRestoreEstimate: { rows: 1500, files: 9, bytes: 4096, freeBytes: 10_000_000 },
  ...extra,
});

describe("RestorePlanView", () => {
  it("states the consequence, the two checks and that nothing was written", () => {
    render(<RestorePlanView plan={PLAN()} />);
    expect(screen.getByText(/chưa có gì bị thay đổi/i)).toBeInTheDocument();
    expect(screen.getByText("Tệp sao lưu nguyên vẹn")).toBeInTheDocument();
    expect(screen.getByText(/Cấu trúc khớp/)).toBeInTheDocument();
  });

  it("accessChanges.total = 0 -> no widened-access block", () => {
    render(<RestorePlanView plan={PLAN()} />);
    expect(screen.queryByText(/mở rộng lại quyền/i)).toBeNull();
  });

  it("accessChanges.total > 0 -> one notification ABOVE every group, kinds > 0, host names, people and keys", () => {
    const a = { ...ACCESS0, total: 6, roleGrantsWidened: 4, workflowsReenabled: 2, outboundHosts: ["hooks.example.com"], usersAffected: 3, apiKeysAffected: 1 };
    const { container } = render(<RestorePlanView plan={PLAN({ accessChanges: a })} />);
    const note = screen.getByText(/mở rộng lại quyền/i).closest(".slds-scoped-notification") as HTMLElement;
    expect(note).not.toBeNull();
    expect(within(note).getByText("4 quyền theo vai")).toBeInTheDocument();
    expect(within(note).getByText("2 luật tự động bật lại")).toBeInTheDocument();
    expect(within(note).queryByText(/quyền trường/)).toBeNull();
    expect(note.textContent).toContain("hooks.example.com");
    expect(note.textContent).toContain("3 người dùng");
    expect(note.textContent).toContain("1 khoá truy cập");
    const firstGroup = container.querySelector("section[aria-label='Dữ liệu']") as HTMLElement;
    expect(note.compareDocumentPosition(firstGroup) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("accessChanges absent -> says it does not know, never 'no change'", () => {
    render(<RestorePlanView plan={PLAN({ accessChanges: undefined as never })} />);
    expect(screen.getByText("Chưa xác định được quyền sẽ đổi thế nào")).toBeInTheDocument();
    expect(screen.queryByText(/mở rộng lại quyền/i)).toBeNull();
  });

  it("four groups, each a table 'Bảng · Trong bản · Hiện có' with tabular numbers; no obj_* name on screen", () => {
    const { container } = render(<RestorePlanView plan={PLAN()} />);
    for (const name of ["Dữ liệu", "Cấu hình", "Quyền", "Tệp"]) expect(container.querySelector(`section[aria-label='${name}']`)).not.toBeNull();
    const data = container.querySelector("section[aria-label='Dữ liệu']") as HTMLElement;
    expect(within(data).getByRole("columnheader", { name: "Bảng" })).toBeInTheDocument();
    expect(within(data).getByRole("columnheader", { name: "Trong bản" })).toBeInTheDocument();
    expect(within(data).getByRole("columnheader", { name: "Hiện có" })).toBeInTheDocument();
    expect(within(data).getByText("Khách hàng")).toBeInTheDocument();
    const nums = data.querySelectorAll("td.tabular-nums");
    expect(nums).toHaveLength(2);
    expect(nums[0].textContent).toBe("1.200");
    expect(nums[1].textContent).toBe("1.350");
    expect(container.textContent).not.toMatch(/obj_/);
  });

  it("an empty group says so; the files line carries the blobs", () => {
    const { container } = render(<RestorePlanView plan={PLAN()} />);
    const files = container.querySelector("section[aria-label='Tệp']") as HTMLElement;
    expect(within(files).getByText("Không có gì trong nhóm này.")).toBeInTheDocument();
    expect(files.textContent).toMatch(/ghi lại 7 tệp/);
    expect(files.textContent).toMatch(/gỡ 2 tệp/);
  });

  it("warnings by code (ACCESS_WIDENED is the access block, not a bullet), 'Giữ nguyên', estimate and free space", () => {
    render(<RestorePlanView plan={PLAN()} />);
    expect(screen.getByText(/2 người dùng sẽ mất vai/)).toBeInTheDocument();
    expect(screen.queryByText(/ACCESS_WIDENED/)).toBeNull();
    expect(screen.getByRole("heading", { name: "Giữ nguyên" })).toBeInTheDocument();
    expect(screen.getByText("Lịch sử xuất bản")).toBeInTheDocument();
    expect(screen.getByText(/1\.500 dòng/)).toBeInTheDocument();
    expect(screen.getByText(/còn trống/)).toBeInTheDocument();
  });

  it("a real server answer has null warnings and only the groups that hold tables (measured on a clone)", () => {
    const { container } = render(<RestorePlanView plan={PLAN({ warnings: null as never, kept: null as never, groups: [{ group: "config", label: "Cấu hình", tables: null as never }] })} />);
    expect(container.querySelector("section[aria-label='Cấu hình']")).not.toBeNull();
    expect(container.querySelector("section[aria-label='Dữ liệu']")).not.toBeNull();
  });

  it("draws no restore button yet (that is the next slice)", () => {
    render(<RestorePlanView plan={PLAN()} />);
    expect(screen.queryByRole("button")).toBeNull();
  });
});
