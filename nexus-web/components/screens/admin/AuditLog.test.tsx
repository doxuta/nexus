// Component tests — AuditLog admin screen (W7 §11.2) on SLDS (specs/038 T048).
// Pins the behaviour the migration must keep:
//   * rows resolve the actor id to a name, the entity to its label, the action to a badge;
//   * the first row that has a diff opens by default; any row's diff opens/closes from its toggle;
//   * every filter (action, date preset) flows into GET /audit-log AND into the CSV export — one
//     filter source; "Xoá lọc" drops them all;
//   * "Tải thêm" pages with the server cursor and appends;
//   * an empty result says so.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuditLog } from "@/components/screens/admin/AuditLog";
import { listAuditFiltered, downloadAuditCsv, type AuditFilter } from "@/lib/admin";
import type { AuditEntry } from "@/lib/api";
import { presetRange } from "@/lib/runFilters";

vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  ensureAuth: vi.fn(async () => undefined),
  listAuditFiltered: vi.fn(),
  downloadAuditCsv: vi.fn(async () => undefined),
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listAdminUsers: vi.fn(async () => [{ id: "u1", email: "admin@example.com", fullName: "Nexus Admin" }]),
  listEntities: vi.fn(async () => [{ name: "crm_contacts", label: "Liên hệ" }]),
}));

const UPDATE: AuditEntry = {
  id: "a1",
  actorUserId: "u1",
  entityName: "crm_contacts",
  recordId: "8d2b02b5-0000-4000-8000-000000000001",
  action: "update",
  diff: { before: { first_name: "Lê Văn" }, after: { first_name: "Lê Văn An" } },
  at: "2026-09-17T06:28:00Z",
};
const DELETE: AuditEntry = {
  id: "a2",
  actorUserId: "u1",
  entityName: "crm_contacts",
  recordId: "9d45bc16-0000-4000-8000-000000000002",
  action: "delete",
  diff: { before: { last_name: "Trần" } },
  at: "2026-09-17T06:20:00Z",
};

const RESTORE: AuditEntry = {
  id: "a3",
  actorUserId: "u1",
  entityName: "crm_contacts",
  recordId: "9d45bc16-0000-4000-8000-000000000002",
  action: "restore",
  at: "2026-09-17T06:21:00Z",
};

const lastFilter = () => vi.mocked(listAuditFiltered).mock.calls.at(-1)?.[0] as AuditFilter;
const toggles = () => screen.getAllByRole("button", { name: /^Thay đổi của/ });

describe("AuditLog", () => {
  beforeEach(() => {
    vi.mocked(listAuditFiltered).mockReset();
    vi.mocked(downloadAuditCsv).mockClear();
    vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [UPDATE, DELETE, RESTORE], limit: 50 });
  });

  it("renders actor name, entity label and action badge; the first diff is open", async () => {
    render(<AuditLog />);
    await waitFor(() => expect(screen.getAllByText("Nexus Admin")).toHaveLength(3));
    expect(screen.getAllByText("Liên hệ").length).toBeGreaterThan(0);
    expect(screen.getByText("Sửa")).toBeInTheDocument();
    expect(screen.getByText("Xoá")).toBeInTheDocument();
    // restore (recycle-bin undelete) is a canonical action too — named, never the raw token.
    expect(screen.getByText("Khôi phục")).toBeInTheDocument();
    expect(screen.queryByText("restore")).not.toBeInTheDocument();

    // First row's diff open by default: field, struck-through before, after.
    const [first, second] = toggles();
    expect(first).toHaveAttribute("aria-expanded", "true");
    const diff = screen.getByRole("table", { name: "Trường thay đổi" });
    expect(within(diff).getByText("first_name")).toBeInTheDocument();
    expect(within(diff).getByText("Lê Văn")).toBeInTheDocument();
    expect(within(diff).getByText("Lê Văn An")).toBeInTheDocument();
    // The other row stays closed and its fields are not rendered.
    expect(second).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("last_name")).not.toBeInTheDocument();
  });

  it("a row's toggle opens and closes its diff", async () => {
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(2));
    const second = toggles()[1];
    fireEvent.click(second);
    expect(second).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("last_name")).toBeInTheDocument();
    fireEvent.click(second);
    expect(second).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("last_name")).not.toBeInTheDocument();
  });

  it("action + date preset filter the list and the CSV export alike; Xoá lọc clears them", async () => {
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(2));
    expect(lastFilter()).toMatchObject({ action: undefined, dateFrom: undefined, dateTo: undefined });

    await userEvent.click(screen.getByRole("combobox", { name: "Hành động" }));
    await userEvent.click(screen.getByRole("option", { name: "Sửa" }));
    await waitFor(() => expect(lastFilter()).toMatchObject({ action: "update" }));

    fireEvent.click(screen.getByRole("button", { name: "Khoảng ngày" }));
    const pop = screen.getByRole("dialog", { name: "Khoảng ngày" });
    fireEvent.change(within(pop).getByLabelText("Khoảng có sẵn"), { target: { value: "7d" } });
    const week = presetRange("7d", new Date());
    await waitFor(() => expect(lastFilter()).toMatchObject({ action: "update", ...week }));

    fireEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() => expect(downloadAuditCsv).toHaveBeenCalledTimes(1));
    expect(vi.mocked(downloadAuditCsv).mock.calls[0][0]).toMatchObject({ action: "update", ...week });

    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await userEvent.click(screen.getByRole("button", { name: "Xoá lọc" }));
    await waitFor(() => expect(lastFilter()).toMatchObject({ action: undefined, dateFrom: undefined, dateTo: undefined }));
    expect(screen.queryByRole("button", { name: "Xoá lọc" })).not.toBeInTheDocument();
  });

  // specs/062 T071: the owner narrows the trail to what a Nexus builder did. The choice rides the one
  // `filter` object, so the list request and the CSV export carry the same `actor=builder`.
  it("the 'Loại người thực hiện' choice stays hidden while no loaded row carries an actorLabel", async () => {
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(2));
    expect(screen.queryByRole("combobox", { name: "Loại người thực hiện" })).not.toBeInTheDocument();
  });

  it("'Người dựng Nexus' option sends actor=builder to the list and the CSV, and Xoá lọc drops it", async () => {
    vi.mocked(listAuditFiltered).mockResolvedValue({
      entries: [{ ...UPDATE, actorLabel: "An Nguyễn (Nexus)" }, DELETE], limit: 50,
    });
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(2));
    expect(lastFilter().actor).toBeUndefined();

    await userEvent.click(screen.getByRole("combobox", { name: "Loại người thực hiện" }));
    await userEvent.click(screen.getByRole("option", { name: "Người dựng Nexus" }));
    await waitFor(() => expect(lastFilter()).toMatchObject({ actor: "builder" }));

    fireEvent.click(screen.getByRole("button", { name: "Xuất CSV" }));
    await waitFor(() => expect(downloadAuditCsv).toHaveBeenCalledTimes(1));
    expect(vi.mocked(downloadAuditCsv).mock.calls[0][0]).toMatchObject({ actor: "builder" });

    await userEvent.click(screen.getByRole("button", { name: "Xoá lọc" }));
    await waitFor(() => expect(lastFilter().actor).toBeUndefined());
  });

  it("buildAuditQuery writes actor=builder only when chosen", async () => {
    const { buildAuditQuery } = await import("@/lib/admin");
    expect(buildAuditQuery({ actor: "builder", action: "update" })).toBe("action=update&actor=builder");
    expect(buildAuditQuery({ action: "update" })).toBe("action=update");
  });

  it("Xuất CSV keeps keyboard focus while it exports and after (the button is never remounted)", async () => {
    let finish!: () => void;
    vi.mocked(downloadAuditCsv).mockImplementationOnce(() => new Promise<void>((resolve) => { finish = resolve; }));
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(2));
    const btn = screen.getByRole("button", { name: "Xuất CSV" });
    btn.focus();
    fireEvent.click(btn);
    await waitFor(() => expect(btn).toHaveTextContent("Đang xuất…"));
    expect(btn).toBeDisabled();
    expect(document.activeElement).toBe(btn);
    finish();
    await waitFor(() => expect(btn).toHaveTextContent("Xuất CSV"));
    expect(btn).toBeEnabled();
    expect(document.activeElement).toBe(btn);
  });

  it("Tải thêm pages with the server cursor and appends the rows", async () => {
    vi.mocked(listAuditFiltered)
      .mockResolvedValueOnce({ entries: [UPDATE], limit: 50, nextCursor: "c-1" })
      .mockResolvedValueOnce({ entries: [DELETE], limit: 50 });
    render(<AuditLog />);
    await waitFor(() => expect(toggles()).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Tải thêm" }));
    await waitFor(() => expect(toggles()).toHaveLength(2));
    expect(lastFilter()).toMatchObject({ after: "c-1", limit: 50 });
    expect(screen.queryByRole("button", { name: "Tải thêm" })).not.toBeInTheDocument();
  });

  it("an empty result says nothing matches", async () => {
    vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [], limit: 50 });
    render(<AuditLog />);
    expect(await screen.findByText("Không có mục nhật ký nào khớp bộ lọc.")).toBeInTheDocument();
  });

  // specs/068 T030a (FR-061b/FR-065a): a workshop operator who backed up / downloaded this customer
  // is NOT one of the customer's users, so the lookup by id finds no name. The row must show the
  // human label the server wrote (diff.after.by) with a "Người dựng Nexus" badge — never a short uuid.
  describe("per-customer backup rows (__tenant_backup__)", () => {
    const OPERATOR_ID = "5c1e0000-aaaa-4bbb-8ccc-0000000000e1";
    const BACKUP_CREATE: AuditEntry = {
      id: "b1", actorUserId: OPERATOR_ID, entityName: "__tenant_backup__", recordId: "bk-1", action: "create",
      diff: { after: { backupId: "bk-1", kind: "manual", by: "Nguyễn Văn A (Nexus)" } }, at: "2026-10-07T01:00:00Z",
      actorLabel: "Nguyễn Văn A (Nexus)", // computed by the server (specs/062 L3) — the screen never reads diff.after.by
    };
    const BACKUP_DOWNLOAD: AuditEntry = { ...BACKUP_CREATE, id: "b2", action: "download", at: "2026-10-07T01:05:00Z" };
    const SCHEDULED: AuditEntry = {
      id: "b3", actorUserId: "f3000000-0000-0000-0000-000000000000", entityName: "__tenant_backup__", recordId: "bk-2",
      action: "create", diff: { after: { backupId: "bk-2", kind: "scheduled", by: "Tự sao lưu theo lịch" } }, at: "2026-10-07T00:00:00Z",
      actorLabel: "Tự sao lưu theo lịch", // the server labels any non-user actor on a meta row, system ids included
    };

    it("shows 'Họ tên (Tên xưởng)' + the builder badge, never the short id; names the noun and the download action", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [BACKUP_CREATE, BACKUP_DOWNLOAD], limit: 50 });
      render(<AuditLog />);
      // each row's actor cell: the label AND the badge (the open diff of the first row also lists
      // `by` as a value, so the badge is the handle on the actor cell)
      const badges = await screen.findAllByText("Người dựng Nexus");
      expect(badges).toHaveLength(2);
      for (const badge of badges) {
        expect(badge.closest("th")).toHaveTextContent("Nguyễn Văn A (Nexus)");
      }
      expect(screen.queryByText(OPERATOR_ID.replace(/-/g, "").slice(0, 8))).not.toBeInTheDocument();
      expect(screen.getAllByText("Hệ thống · sao lưu & khôi phục").length).toBeGreaterThan(0);
      expect(screen.getByText("Tải xuống")).toBeInTheDocument();
      expect(screen.queryByText("download")).not.toBeInTheDocument();
    });

    it("the schedule's system id reads 'Tự sao lưu theo lịch' (no builder badge)", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [SCHEDULED], limit: 50 });
      render(<AuditLog />);
      await waitFor(() => expect(screen.getAllByText("Tự sao lưu theo lịch").length).toBeGreaterThan(0));
      expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
      expect(screen.queryByText("f3000000")).not.toBeInTheDocument();
    });

    it("the screen trusts actorLabel only: a diff.after.by with no actorLabel never makes a builder badge", async () => {
      const forged: AuditEntry = { ...BACKUP_CREATE, actorLabel: undefined };
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [forged], limit: 50 });
      render(<AuditLog />);
      await waitFor(() => expect(screen.getAllByText("Hệ thống · sao lưu & khôi phục").length).toBeGreaterThan(0));
      expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
    });

    it("a customer's OWN user who is the actor keeps their real name (the label never overrides a known user)", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [{ ...BACKUP_CREATE, actorUserId: "u1" }], limit: 50 });
      render(<AuditLog />);
      await waitFor(() => expect(screen.getAllByText("Nexus Admin").length).toBeGreaterThan(0));
      expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
    });
  });

  // specs/064 T062: preview sessions - the trail names who looked as which role, in Vietnamese.
  describe("preview-session rows (__preview__)", () => {
    const BUILDER = "b0000000-aaaa-4bbb-8ccc-000000000001";
    const START: AuditEntry = {
      id: "p1", actorUserId: BUILDER, entityName: "__preview__", recordId: "7f1c3a2e-0000-4000-8000-0000000000aa", action: "create",
      diff: { after: { roleName: "Nhân viên kinh doanh" } }, at: "2026-10-08T01:00:00Z", actorLabel: "An Nguyễn (Nexus)",
    };
    const END: AuditEntry = { ...START, id: "p2", action: "delete", diff: { after: { reason: "exit" } }, at: "2026-10-08T01:10:00Z" };

    it("reads 'Bắt đầu' / 'Kết thúc xem như vai' with the Vietnamese noun - no raw key, no Tạo/Xoá", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [START, END], limit: 50 });
      render(<AuditLog />);
      expect(await screen.findByText("Bắt đầu xem như vai")).toBeInTheDocument();
      expect(screen.getByText("Kết thúc xem như vai")).toBeInTheDocument();
      expect(screen.getAllByText("Hệ thống · xem như vai").length).toBeGreaterThan(0);
      expect(screen.queryByText("__preview__")).not.toBeInTheDocument();
      expect(screen.queryByText("Tạo")).not.toBeInTheDocument();
      expect(screen.queryByText("Xoá")).not.toBeInTheDocument();
    });
  });

  // specs/062 L3 T056: the builder's name on the customer's trail, the layout rows, the session rows.
  describe("builder rows (specs/062 L3)", () => {
    const BUILDER = "b0000000-aaaa-4bbb-8ccc-000000000001";
    const LAYOUT: AuditEntry = {
      id: "l1", actorUserId: BUILDER, entityName: "__layout__", recordId: "lay-1", action: "update",
      diff: { before: { "crm_contacts/edit": { version: 1 } }, after: { "crm_contacts/edit": { version: 2 } } },
      at: "2026-10-08T01:00:00Z", actorLabel: "An Nguyễn (Nexus)",
    };
    const OPEN: AuditEntry = {
      id: "s1", actorUserId: BUILDER, entityName: "__workspace_session__", recordId: "ws-1", action: "create",
      diff: { after: { expiresAt: "2026-10-08T09:00:00Z" } }, at: "2026-10-08T01:00:00Z", actorLabel: "An Nguyễn (Nexus)",
    };
    const CLOSE: AuditEntry = {
      id: "s2", actorUserId: BUILDER, entityName: "__workspace_session__", recordId: "ws-1", action: "delete",
      diff: { before: { reason: "closed" } }, at: "2026-10-08T02:00:00Z", actorLabel: "An Nguyễn (Nexus)",
    };

    it("a layout edit names the builder (badge, no short id), the noun, and which table and screen", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [LAYOUT], limit: 50 });
      render(<AuditLog />);
      const badge = await screen.findByText("Người dựng Nexus");
      expect(badge.closest("th")).toHaveTextContent("An Nguyễn (Nexus)");
      expect(screen.queryByText(BUILDER.replace(/-/g, "").slice(0, 8))).not.toBeInTheDocument();
      expect(screen.getAllByText("Hệ thống · bố cục").length).toBeGreaterThan(0);
      expect(await screen.findByText("Liên hệ · Form nhập")).toBeInTheDocument();
    });

    it("opening a session reads 'Mở' with its hard end time; closing reads 'Đóng' (never Tạo/Xoá)", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [OPEN, CLOSE], limit: 50 });
      render(<AuditLog />);
      expect(await screen.findByText("Mở")).toBeInTheDocument();
      expect(screen.getByText("Đóng")).toBeInTheDocument();
      expect(screen.queryByText("Tạo")).not.toBeInTheDocument();
      expect(screen.queryByText("Xoá")).not.toBeInTheDocument();
      expect(screen.getByText(/^Kết thúc chậm nhất lúc \d{2}\/\d{2}\/\d{4} \d{2}:\d{2}$/)).toBeInTheDocument();
      expect(screen.getAllByText("Hệ thống · phiên dựng").length).toBeGreaterThan(0);
    });

    it("reports the actor filter to the page (URL write-back) and clears it with Xoá lọc (specs/078 T046)", async () => {
      const onActorChange = vi.fn();
      render(<AuditLog initialActorId="u1" onActorChange={onActorChange} />);
      await waitFor(() => expect(lastFilter()).toMatchObject({ actorUserId: "u1" }));
      expect(onActorChange).not.toHaveBeenCalled(); // opening with the id is not a change
      fireEvent.click(await screen.findByRole("button", { name: "Xoá lọc" }));
      expect(onActorChange).toHaveBeenLastCalledWith("");
      await waitFor(() => expect(lastFilter().actorUserId).toBeUndefined());
    });

    it("a known customer user keeps their name even if a label were present", async () => {
      vi.mocked(listAuditFiltered).mockResolvedValue({ entries: [{ ...LAYOUT, actorUserId: "u1" }], limit: 50 });
      render(<AuditLog />);
      await waitFor(() => expect(screen.getAllByText("Nexus Admin").length).toBeGreaterThan(0));
      expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
    });
  });
});
