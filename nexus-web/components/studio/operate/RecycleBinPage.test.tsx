// specs/078 lát u7-du-lieu-thung-rac-xoa-han T093 + T099 (the part a unit can prove): the recycle bin in the Studio frame.
// RecycleBinScreen is the REAL component; /auth/me (useStudioIdentity), the router and the network are the seams.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, listEntities, listDeletedPage, purgeRecord, restoreRecord, type ApiRecord, type EntityLayout, type EntityMeta } from "@/lib/api";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { KnownEntityGate } from "./EntityRecordsPage";
import { OperateRecycleBin } from "./RecycleBinPage";

const idState = vi.hoisted(() => ({
  status: "ready" as "ready" | "loading" | "error",
  me: { isPlatformOperator: true } as { isPlatformOperator?: boolean; workspace?: unknown } | undefined,
  matches: true,
  activeSlug: "acme",
}));
const perm = vi.hoisted(() => ({ canDelete: true }));
const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), warning: vi.fn() }));

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }), usePathname: () => "/studio/du-an/acme/van-hanh/du-lieu/crm_accounts/thung-rac" }));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ status: idState.status, me: idState.me, matches: () => idState.matches }),
  transitionPhase: () => "idle",
  activeProjectSlug: () => idState.activeSlug,
}));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: true } }) }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: (a: string) => a !== "delete" || perm.canDelete, isLoading: false }) }));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" }, { name: "crm_deals", label: "Cơ hội", tableName: "obj_crm_deals" }] }),
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: toasts }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn(async () => {}),
  getLayout: vi.fn(async () => ({ views: { list: { columns: [{ field: "name", width: 200 }] } } }) as unknown as EntityLayout),
  listFields: vi.fn(async (e: string) => (e === "crm_deals" ? [{ name: "account_id", label: "Khách hàng", fieldType: "relation" }] : [{ name: "name", label: "Tên", fieldType: "text" }])),
  listEntities: vi.fn(),
  listDeletedPage: vi.fn(),
  restoreRecord: vi.fn(),
  purgeRecord: vi.fn(),
}));

const ENTITIES: EntityMeta[] = [{ name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" }];
const ROWS: ApiRecord[] = [{ id: "r1", version: 1, data: { name: "Acme Corp" } }, { id: "r2", version: 1, data: { name: "Globex Inc" } }];
const REPORT = {
  entity: "crm_accounts", id: "r1", dryRun: true,
  records: [{ entity: "crm_accounts", id: "r1" }, { entity: "crm_accounts", id: "r9" }],
  cleared: [{ entity: "crm_deals", id: "d1", field: "account_id" }],
  blocked: [], tables: [{ table: "obj_crm_accounts", rows: 2 }, { table: "sys_files", rows: 1 }], totalRows: 3, blobsRemoved: 0,
  blobsNote: "1 tệp đính kèm sẽ bị xoá khỏi kho lưu trữ",
};

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <EmbeddedSurface>
        <KnownEntityGate slug="acme" bang="crm_accounts">{() => <OperateRecycleBin slug="acme" bang="crm_accounts" />}</KnownEntityGate>
      </EmbeddedSurface>
    </QueryClientProvider>,
  );
}
const openMenu = async (name: string, item: string) => {
  await userEvent.click(await screen.findByRole("button", { name: `Thêm thao tác cho ${name}` }));
  await userEvent.click(await screen.findByRole("menuitem", { name: item }));
};
const purgeCalls = (dryRun: boolean) => vi.mocked(purgeRecord).mock.calls.filter((c) => c[2]?.dryRun === dryRun);

beforeEach(() => {
  vi.clearAllMocks();
  Object.assign(idState, { status: "ready", me: { isPlatformOperator: true }, matches: true, activeSlug: "acme" });
  perm.canDelete = true;
  vi.mocked(listEntities).mockResolvedValue(ENTITIES);
  vi.mocked(listDeletedPage).mockResolvedValue({ records: ROWS, pagination: { page: 1, limit: 50, total: 2, totalPages: 1 } });
  vi.mocked(restoreRecord).mockResolvedValue({ id: "r1" });
  vi.mocked(purgeRecord).mockImplementation(async (_e, _i, o) => (o.dryRun ? { ...REPORT, dryRun: true } : { ...REPORT, dryRun: false, blobsNote: undefined, blobsRemoved: 1 }));
});

describe("Studio recycle bin", () => {
  it("embedded: the bin table, no dialog, no retention promise", async () => {
    const { container } = mount();
    await screen.findByText("Acme Corp");
    expect(container.querySelectorAll("[role=dialog],[role=alertdialog]")).toHaveLength(0);
    expect(container.textContent).not.toMatch(/\d+ ngày/);
  });

  it("Khôi phục asks in the page (no dialog) and restores", async () => {
    mount();
    await screen.findByText("Acme Corp");
    await openMenu("Acme Corp", "Khôi phục");
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.getByText('Khôi phục "Acme Corp" về danh sách đang dùng?')).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Khôi phục" }));
    await waitFor(() => expect(restoreRecord).toHaveBeenCalledWith("crm_accounts", "r1"));
    await waitFor(() => expect(screen.queryByText("Acme Corp")).toBeNull());
  });

  it("restore 409 names the clashing field", async () => {
    vi.mocked(restoreRecord).mockRejectedValue(new ApiError(409, "RECORD_DUPLICATE", "dup", { field: "name" }));
    mount();
    await screen.findByText("Acme Corp");
    await openMenu("Acme Corp", "Khôi phục");
    await userEvent.click(screen.getByRole("button", { name: "Khôi phục" }));
    await waitFor(() => expect(toasts.error).toHaveBeenCalled());
    expect(String(toasts.error.mock.calls[0][0])).toContain('"Tên"');
  });

  it("without the delete grant: read-only sentence, no row actions", async () => {
    perm.canDelete = false;
    idState.me = { isPlatformOperator: false };
    mount();
    await screen.findByText("Acme Corp");
    expect(screen.getByText("Anh chỉ xem được thùng rác.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Thêm thao tác cho/ })).toBeNull();
  });

  describe("Xoá vĩnh viễn is offered only when /auth/me says builder (FR-058)", () => {
    it.each([
      ["loading", { status: "loading", me: undefined }],
      ["error", { status: "error", me: undefined }],
      ["legacy customer admin", { status: "ready", me: { isPlatformOperator: false } }],
      ["flag missing and no workspace", { status: "ready", me: {} }],
    ] as const)("%s => no menu item", async (_n, st) => {
      Object.assign(idState, st);
      mount();
      await screen.findByText("Acme Corp");
      await userEvent.click(screen.getByRole("button", { name: "Thêm thao tác cho Acme Corp" }));
      expect(await screen.findByRole("menuitem", { name: "Khôi phục" })).toBeInTheDocument();
      expect(screen.queryByRole("menuitem", { name: "Xoá vĩnh viễn" })).toBeNull();
    });

    it("builder in a build session whose /auth/me omits the flag but carries workspace => offered", async () => {
      Object.assign(idState, { me: { workspace: { tenantSlug: "acme" } } });
      mount();
      await screen.findByText("Acme Corp");
      await userEvent.click(screen.getByRole("button", { name: "Thêm thao tác cho Acme Corp" }));
      expect(await screen.findByRole("menuitem", { name: "Xoá vĩnh viễn" })).toBeInTheDocument();
    });
  });

  describe("two-phase permanent delete", () => {
    it("phase 1 runs the dry run on open and says what goes, in business words; phase 2 sends dryRun:false once", async () => {
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      await screen.findByText(/Sẽ xoá hẳn/);
      const body = document.body.textContent ?? "";
      expect(body).toContain("Sẽ xoá hẳn 2 bản ghi (kể cả 1 bản ghi con trong thùng), 1 tệp đính kèm; không khôi phục được.");
      expect(body).toContain('1 bản ghi Cơ hội khác sẽ bị bỏ trống liên kết "Khách hàng".');
      expect(body).toContain("1 tệp đính kèm sẽ bị xoá khỏi kho lưu trữ");
      expect(body).not.toMatch(/obj_|sys_files|ở \d+ bảng/);
      expect(purgeCalls(true)).toHaveLength(1);
      expect(purgeCalls(false)).toHaveLength(0);

      const confirm = screen.getByRole("button", { name: "Xoá vĩnh viễn" });
      await userEvent.dblClick(confirm);
      await waitFor(() => expect(screen.queryByText("Globex Inc")).not.toBeNull());
      await waitFor(() => expect(screen.queryByText("Acme Corp")).toBeNull());
      expect(purgeCalls(false)).toHaveLength(1);
      expect(purgeRecord).toHaveBeenLastCalledWith("crm_accounts", "r1", { dryRun: false });
      expect(toasts.success).toHaveBeenCalled();
    });

    it("phase 2 with blobsNote warns verbatim instead of plain success", async () => {
      const note = "CHƯA xoá 1 tệp: VẪN CÒN trên đĩa";
      vi.mocked(purgeRecord).mockImplementation(async (_e, _i, o) => ({ ...REPORT, dryRun: o.dryRun, ...(o.dryRun ? {} : { blobsNote: note }) }));
      mount();
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      await screen.findByText(/Sẽ xoá hẳn/);
      await userEvent.click(screen.getByRole("button", { name: "Xoá vĩnh viễn" }));
      await waitFor(() => expect(toasts.warning).toHaveBeenCalled());
      expect(String(toasts.warning.mock.calls[0][0])).toContain(note);
      expect(toasts.success).not.toHaveBeenCalled();
    });

    it("phase 1 failing (500) => no confirm button, only Thử lại; retry works", async () => {
      vi.mocked(purgeRecord).mockRejectedValueOnce(new ApiError(500, "INTERNAL", "boom"));
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      expect(await screen.findByRole("button", { name: "Thử lại" })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Xoá vĩnh viễn" })).toBeNull();
      await userEvent.click(screen.getByRole("button", { name: "Thử lại" }));
      expect(await screen.findByRole("button", { name: "Xoá vĩnh viễn" })).toBeInTheDocument();
      expect(purgeCalls(false)).toHaveLength(0);
    });

    it("blocked => the live children are named by label and the confirm button is off", async () => {
      vi.mocked(purgeRecord).mockResolvedValue({ ...REPORT, blocked: [{ entity: "crm_deals", field: "account_id", count: 3 }] });
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      expect(await screen.findByText(/3 bản ghi Cơ hội còn đang dùng bản ghi này/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Xoá vĩnh viễn" })).toBeDisabled();
      expect(document.body.textContent).not.toContain("crm_deals");
    });

    it("the customer changes between the two phases => phase 2 is never sent", async () => {
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      await screen.findByText(/Sẽ xoá hẳn/);
      idState.activeSlug = "globex";
      await userEvent.click(screen.getByRole("button", { name: "Xoá vĩnh viễn" }));
      expect(purgeCalls(false)).toHaveLength(0);
      expect(screen.queryByText(/Sẽ xoá hẳn/)).toBeNull();
    });

    it("a report for another record is not confirmable", async () => {
      vi.mocked(purgeRecord).mockResolvedValue({ ...REPORT, id: "other" });
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      await screen.findByRole("button", { name: "Thử lại" });
      expect(screen.queryByRole("button", { name: "Xoá vĩnh viễn" })).toBeNull();
    });

    it("403 hides the action for the rest of the session and says why", async () => {
      vi.mocked(purgeRecord).mockRejectedValue(new ApiError(403, "FORBIDDEN", "no"));
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      expect(await screen.findByText("Việc này cần quyền người dựng Nexus.")).toBeInTheDocument();
      await userEvent.click(screen.getByRole("button", { name: "Thêm thao tác cho Globex Inc" }));
      expect(await screen.findByRole("menuitem", { name: "Khôi phục" })).toBeInTheDocument();
      expect(screen.queryByRole("menuitem", { name: "Xoá vĩnh viễn" })).toBeNull();
    });

    it.each([
      [new ApiError(422, "RECORD_PURGE_TOO_LARGE", "x", { max: 500 }), "Quá 500 bản ghi cho một lần; không xoá gì cả."],
      [new ApiError(503, "RECORD_WRITE_CONFLICT", "x"), "Hệ thống đang bận, bấm lại."],
      [new ApiError(409, "RECORD_PURGE_LIVE_CHILDREN", "x", { blockers: [{ entity: "crm_deals", field: "f", count: 2 }] }), "2 bản ghi Cơ hội còn đang dùng bản ghi này."],
    ])("a refused phase 2 says its own sentence and stays on screen", async (err, sentence) => {
      mount();
      await screen.findByText("Acme Corp");
      await openMenu("Acme Corp", "Xoá vĩnh viễn");
      await screen.findByText(/Sẽ xoá hẳn/);
      vi.mocked(purgeRecord).mockRejectedValueOnce(err);
      await userEvent.click(screen.getByRole("button", { name: "Xoá vĩnh viễn" }));
      expect(await screen.findByText(sentence)).toBeInTheDocument();
      expect(screen.getByText("Acme Corp")).toBeInTheDocument();
    });
  });

  it("isolation: a token that is not this project's => the bin is never fetched (FR-135)", async () => {
    idState.matches = false;
    mount();
    await new Promise((r) => setTimeout(r, 30));
    expect(listDeletedPage).not.toHaveBeenCalled();
    expect(purgeRecord).not.toHaveBeenCalled();
    expect(within(document.body).queryByText("Acme Corp")).toBeNull();
  });
});
