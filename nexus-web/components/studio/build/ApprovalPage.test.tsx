// ApprovalPage (specs/074 lát f2-automation-approval-queue, T147): `dung/tu-dong/duyet[/<id>]`. Four states of the scope
// (unknown app · failed · app without a process · ready), `duyet/<id>` opens exactly that process, and an id outside the
// app is dropped and written out of the URL (the #456 bug, now at the page level).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ApprovalProcessSummary, AppSummary, EntityMeta } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/tu-dong/duyet",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const ent = (name: string, label: string): EntityMeta => ({ id: `e-${name}`, name, label, tableName: `obj_${name}` });
const proc = (id: string, name: string, entityName: string): ApprovalProcessSummary => ({
  id, name, entityName, trigger: "manual", isActive: true,
  steps: [{ id: "s1", approverType: "role", approverId: "r1", slaSeconds: 3600 }],
});
const CRM_PROC = proc("a1111111-1111-1111-1111-111111111111", "Duyệt hợp đồng", "crm_accounts");

vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true }) }));
const getPublishState = vi.fn();
const updateApprovalProcess = vi.fn();
const appsCatalog = vi.fn();
const appNav = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listEntities: vi.fn(async () => [ent("crm_accounts", "Khách hàng"), ent("pm_tasks", "Công việc")]),
  listAdminUsers: vi.fn().mockResolvedValue([]),
  listRoles: vi.fn().mockResolvedValue([{ id: "r1", name: "mgr", label: "Trưởng phòng" }]),
  listFields: vi.fn().mockResolvedValue([]),
  listApprovalProcesses: vi.fn(async () => [CRM_PROC]),
  getPublishState: (...a: unknown[]) => getPublishState(...a),
  updateApprovalProcess: (...a: unknown[]) => updateApprovalProcess(...a),
}));

import { ApprovalPage } from "./ApprovalPage";

const navOf = (...names: string[]) => ({ app: app("x"), items: names.map((n) => ({ type: "entity", refId: n, label: n, href: `/${n}` })) });
function renderPage(props: { id?: string; app?: string; bang?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><ApprovalPage slug="northwind" {...props} /></QueryClientProvider>);
}
const nameBox = () => screen.queryByRole("textbox", { name: "Tên quy trình" }) as HTMLInputElement | null;

beforeEach(() => {
  vi.clearAllMocks();
  getPublishState.mockResolvedValue({ delivered: false, draftableTypes: ["workflow"], directTypes: ["role"] });
  updateApprovalProcess.mockImplementation(async (_id: string, p: object) => ({ ...CRM_PROC, ...p }));
  appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
  appNav.mockImplementation(async (n: string) => (n === "crm" ? navOf("crm_accounts") : navOf("pm_tasks")));
});

describe("ApprovalPage", () => {
  it("an app code that is not the project's says so, with a way to all", async () => {
    renderPage({ app: "zzz" });
    expect(await screen.findByText("Không có app zzz")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Xem tất cả" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong/duyet");
    expect(nameBox()).toBeNull();
  });

  it("an app whose tables failed to load is an error, never the unfiltered list", async () => {
    appNav.mockRejectedValue(new Error("boom"));
    renderPage({ app: "crm" });
    expect(await screen.findByText("Không tải được bảng của app này.")).toBeInTheDocument();
    expect(screen.queryByText(CRM_PROC.name)).toBeNull();
  });

  it("an app with no process opens none of another app's, and the URL carries no id", async () => {
    renderPage({ app: "ticket", id: CRM_PROC.id });
    await screen.findByText(/^0 quy trình/);
    expect(nameBox()).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/tu-dong/duyet?app=ticket", { scroll: false }));
  });

  it("duyet/<id> opens exactly that process and leaves the URL alone", async () => {
    renderPage({ app: "crm", id: CRM_PROC.id });
    await waitFor(() => expect(nameBox()?.value).toBe(CRM_PROC.name));
    await new Promise((r) => setTimeout(r, 30));
    expect(replace).not.toHaveBeenCalled();
  });

  it("with no id the first process of the app opens and the URL follows it", async () => {
    renderPage({ app: "crm" });
    await waitFor(() => expect(nameBox()?.value).toBe(CRM_PROC.name));
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`/studio/du-an/northwind/dung/tu-dong/duyet/${CRM_PROC.id}?app=crm`, { scroll: false }));
  });
});

// specs/074 lát f2-draft-labels-rules (T345): an approval process has no draft — on a delivered project saving it applies at once.
describe("ApprovalPage: Áp ngay", () => {
  const save = () => screen.findByRole("button", { name: /^(Lưu(?! nháp)|Cập nhật)/ });
  it("delivered project: label shown, save asks first and writes only after the confirmation", async () => {
    getPublishState.mockResolvedValue({ delivered: true, draftableTypes: ["workflow"], directTypes: ["role"] });
    renderPage({ app: "crm", id: CRM_PROC.id });
    await waitFor(() => expect(nameBox()?.value).toBe(CRM_PROC.name));
    expect(await screen.findByText("Áp ngay, không có trong lịch sử")).toBeInTheDocument();
    fireEvent.click(await save());
    expect(await screen.findByText(/ÁP NGAY cho người dùng thật/)).toBeInTheDocument();
    expect(updateApprovalProcess).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Lưu và áp ngay" }));
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
  });
  it("not delivered: no label, save writes straight away", async () => {
    renderPage({ app: "crm", id: CRM_PROC.id });
    await waitFor(() => expect(nameBox()?.value).toBe(CRM_PROC.name));
    fireEvent.click(await save());
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("Áp ngay, không có trong lịch sử")).toBeNull();
  });
});
