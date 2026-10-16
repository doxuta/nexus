// specs/079 f7-khoi-phuc-ke-hoach: the restore plan page. Read only: the preview is the only call it makes, and it
// makes it only once tenantId and project code come from the same source (FR-101, FR-002a).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type ProjectRow, type RestorePlan } from "@/lib/api";

const nav = vi.hoisted(() => ({ replace: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: nav.replace, push: vi.fn() }) }));
const hoisted = vi.hoisted(() => ({ identity: { status: "ready", home: "northwind", me: { tenantId: "t-home", isPlatformOperator: true }, matches: (() => true) as (s: string) => boolean } as Record<string, unknown> }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  previewTenantRestore: vi.fn(),
  startTenantRestore: vi.fn(),
  listProjects: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => hoisted.identity,
}));

import { listProjects, previewTenantRestore, startTenantRestore } from "@/lib/api";
import { RestorePage } from "./RestorePage";

const BID = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
const ROW = (slug: string, tenantId: string): ProjectRow => ({ tenantId, slug, name: slug } as ProjectRow);
const PLAN: RestorePlan = {
  backupId: BID, backupCreatedAt: "2026-10-10T03:00:00Z", integrity: { ok: true }, schema: { backup: 12, current: 12, ok: true },
  groups: [{ group: "data", label: "Dữ liệu", tables: [{ table: "obj_1", label: "Khách hàng", rowsInBackup: 10, rowsNow: 12 }] }],
  blobs: { toWrite: 0, bytesToWrite: 0, toRemove: 0 }, warnings: [],
  accessChanges: { total: 0, roleGrantsWidened: 0, fieldAclReopened: 0, sharesRegranted: 0, membershipsReadded: 0, sensitiveFieldsCleared: 0, workflowsReenabled: 0, outboundDestinationsReenabled: 0, outboundHosts: [], usersAffected: 0, apiKeysAffected: 0 },
  kept: ["Mật khẩu"], preRestoreEstimate: { rows: 10, files: 0, bytes: 100, freeBytes: 1000 },
};

const view = (project = "acme") =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RestorePage project={project} backupId={BID} />
    </QueryClientProvider>,
  );

beforeEach(() => {
  hoisted.identity = { status: "ready", home: "northwind", me: { tenantId: "t-home", isPlatformOperator: true }, matches: () => true };
  vi.mocked(previewTenantRestore).mockReset().mockResolvedValue(PLAN);
  vi.mocked(startTenantRestore).mockReset();
  vi.mocked(listProjects).mockReset().mockResolvedValue([ROW("acme", "t-acme"), ROW("beta", "t-beta")]);
});

describe("RestorePage", () => {
  it("asks for the plan of THIS project's row (tenantId from the ledger, never the URL) and draws it", async () => {
    view();
    expect(await screen.findByText("Tệp sao lưu nguyên vẹn")).toBeInTheDocument();
    expect(previewTenantRestore).toHaveBeenCalledTimes(1);
    expect(previewTenantRestore).toHaveBeenCalledWith("t-acme", BID);
    expect(screen.getByRole("heading", { name: /^Khôi phục về bản sao lưu lúc / })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Sao lưu/ })).toHaveAttribute("href", "/studio/du-an/acme/bao-tri/sao-luu");
  });

  it("the plan alone writes nothing: the restore call waits for the typed code", async () => {
    view();
    await screen.findByText("Tệp sao lưu nguyên vẹn");
    expect(startTenantRestore).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Khôi phục về bản này" })).toBeInTheDocument();
  });

  it("no button when the file is corrupt or the schema differs", async () => {
    vi.mocked(previewTenantRestore).mockResolvedValue({ ...PLAN, schema: { backup: 9, current: 12, ok: false } });
    view();
    await screen.findByText(/Cấu trúc khác/);
    expect(screen.queryByRole("button", { name: "Khôi phục về bản này" })).toBeNull();
  });

  it("the prompt asks for the project code; a wrong code is answered INSIDE the prompt, the right one sends dryRun:false + confirmSlug and moves to ?lan=", async () => {
    const u = userEvent.setup();
    vi.mocked(startTenantRestore).mockRejectedValueOnce(new ApiError(422, "TENANT_RESTORE_CONFIRM_MISMATCH", "x"));
    view();
    await u.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));
    expect(screen.getByText(/Lịch sử xuất bản giữ nguyên/)).toBeInTheDocument();
    expect(screen.getByText(/Tự sao lưu trước, có thể hoàn tác/)).toBeInTheDocument();
    const box = screen.getByPlaceholderText("acme");
    const go = screen.getByRole("button", { name: "Khôi phục" });
    expect(go).toBeDisabled();
    await u.type(box, "acme");
    await u.click(go);
    expect(await screen.findByText(/Mã gõ vào chưa khớp/)).toBeInTheDocument();
    expect(nav.replace).not.toHaveBeenCalled();
    vi.mocked(startTenantRestore).mockResolvedValueOnce({ restoreId: "11111111-1111-4111-8111-111111111111", status: "running" });
    await u.click(screen.getByRole("button", { name: "Khôi phục" }));
    await waitFor(() => expect(nav.replace).toHaveBeenCalledWith(`/studio/du-an/acme/bao-tri/sao-luu/khoi-phuc/${BID}?lan=11111111-1111-4111-8111-111111111111`));
    expect(startTenantRestore).toHaveBeenLastCalledWith("t-acme", BID, "acme");
  });

  it("the widened-access block shows in the prompt when accessChanges.total > 0", async () => {
    const u = userEvent.setup();
    vi.mocked(previewTenantRestore).mockResolvedValue({ ...PLAN, accessChanges: { ...PLAN.accessChanges, total: 3, roleGrantsWidened: 3 } });
    view();
    await u.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));
    expect(screen.getByText(/Mở rộng lại quyền đã thu hẹp: 3 mục/)).toBeInTheDocument();
  });

  it("changing the project while the prompt is open closes it and sends nothing", async () => {
    const u = userEvent.setup();
    const q = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const tree = (project: string) => <QueryClientProvider client={q}><RestorePage project={project} backupId={BID} /></QueryClientProvider>;
    const r = render(tree("acme"));
    await u.click(await screen.findByRole("button", { name: "Khôi phục về bản này" }));
    expect(screen.getByPlaceholderText("acme")).toBeInTheDocument();
    r.rerender(tree("beta"));
    await waitFor(() => expect(screen.queryByPlaceholderText("acme")).toBeNull());
    expect(startTenantRestore).not.toHaveBeenCalled();
  });

  it("the ledger is still loading -> 0 calls to any per-customer route", async () => {
    vi.mocked(listProjects).mockReturnValue(new Promise(() => {}));
    view();
    await new Promise((r) => setTimeout(r, 30));
    expect(previewTenantRestore).not.toHaveBeenCalled();
  });

  it("the ledger failed or has no row for the code -> gate stays closed, honest sentence", async () => {
    vi.mocked(listProjects).mockResolvedValue([ROW("beta", "t-beta")]);
    view();
    expect(await screen.findByText(/Không tìm thấy dự án này/)).toBeInTheDocument();
    expect(previewTenantRestore).not.toHaveBeenCalled();
  });

  it("the ledger call fails -> honest sentence, 0 preview calls", async () => {
    vi.mocked(listProjects).mockRejectedValue(new Error("boom"));
    view();
    expect(await screen.findByText(/Chưa đọc được danh sách dự án/)).toBeInTheDocument();
    expect(previewTenantRestore).not.toHaveBeenCalled();
  });

  it("the Nexus workshop takes its tenantId from /auth/me only, and the server's refusal reads as one sentence", async () => {
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(422, "TENANT_RESTORE_SELF", "x"));
    view("northwind");
    expect(await screen.findByText("Không khôi phục được chính xưởng Nexus")).toBeInTheDocument();
    expect(previewTenantRestore).toHaveBeenCalledWith("t-home", BID);
    expect(listProjects).not.toHaveBeenCalled();
    expect(screen.queryByText("Tệp sao lưu nguyên vẹn")).toBeNull();
  });

  it("the workshop without a confirmed identity or without me.tenantId -> 0 calls", async () => {
    hoisted.identity = { status: "ready", home: "northwind", me: {}, matches: () => true };
    view("northwind");
    await new Promise((r) => setTimeout(r, 30));
    expect(previewTenantRestore).not.toHaveBeenCalled();
    hoisted.identity = { status: "ready", home: "northwind", me: { tenantId: "t-home", isPlatformOperator: true }, matches: () => false };
    view("northwind");
    await new Promise((r) => setTimeout(r, 30));
    expect(previewTenantRestore).not.toHaveBeenCalled();
  });

  it.each([
    [404, "BACKUP_NOT_FOUND", {}, "Không tìm thấy bản sao lưu này (có thể đã bị xoá)."],
    [409, "BACKUP_NOT_READY", {}, "Bản này chưa ở trạng thái"],
    [422, "TENANT_BACKUP_CORRUPT", {}, "Tệp sao lưu bị hỏng, không dùng được."],
    [409, "TENANT_RESTORE_SCHEMA_MISMATCH", { backup: 9, current: 12 }, "Bản sao lưu này tạo ở cấu trúc số 9, dự án đang ở số 12."],
    [409, "TENANT_RESTORE_SCHEMA_DRIFT", {}, "Cấu trúc trong bản sao lưu không khớp với bảng hiện có."],
    [422, "TENANT_RESTORE_TOO_LARGE", { rows: 5, limit: 3 }, "vượt giới hạn 3 dòng"],
    [422, "TENANT_RESTORE_RETIRED", {}, "Dự án đã ngưng, không khôi phục được."],
    [507, "BACKUP_DISK_LOW", { freeBytes: 1024, needBytes: 4096 }, "Máy chủ còn"],
    [409, "BACKUP_IN_PROGRESS", {}, "Đang có một bản sao lưu chạy."],
  ])("%s %s -> one fixed Vietnamese sentence", async (status, code, details, text) => {
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(status, code, "msg", details));
    view();
    expect(await screen.findByText(new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeInTheDocument();
    expect(screen.queryByText(code)).toBeNull();
  });

  it("a named refusal offers no retry; a dropped connection does", async () => {
    vi.mocked(previewTenantRestore).mockRejectedValueOnce(new ApiError(422, "TENANT_RESTORE_SELF", "x"));
    const a = view("northwind");
    await screen.findByText("Không khôi phục được chính xưởng Nexus");
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
    a.unmount();
    vi.mocked(previewTenantRestore).mockRejectedValueOnce(new TypeError("fetch failed"));
    view();
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeInTheDocument();
  });

  it("engine without the route -> 'chưa có', no plan", async () => {
    vi.mocked(previewTenantRestore).mockRejectedValue(new ApiError(404, "UNKNOWN", "404 page not found"));
    view();
    await waitFor(() => expect(screen.getByText("Khôi phục chưa có trong bản Nexus này")).toBeInTheDocument());
  });
});
