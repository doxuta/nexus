// AccessPage (specs/074 lát f2-access-matrix, T190): `dung/nguoi-dung-quyen` = the roles x tables matrix, the owner note,
// "+ Vai mới" in place (no dialog; the new role starts view-only on every table), the app filter and the related column.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary, EntityMeta, RoleDetail } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/acme/dung/nguoi-dung-quyen",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  homeSlug: () => "northwind",
}));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), info: vi.fn(), error: vi.fn(), withAction: vi.fn() } }));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const ent = (name: string, label: string): EntityMeta => ({ id: `e-${name}`, name, label, tableName: `obj_${name}` });
const role = (id: string, label: string, actions: RoleDetail["actions"]): RoleDetail => ({ id, name: id, label, tenantId: "t", actions });

const listEntities = vi.fn();
const appsCatalog = vi.fn();
const appNav = vi.fn();
const adminApps = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: (...a: unknown[]) => listEntities(...a),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  adminApps: (...a: unknown[]) => adminApps(...a),
}));
const listRoles = vi.fn();
const getRole = vi.fn();
const createRole = vi.fn();
const getFieldAclData = vi.fn();
const listTeams = vi.fn();
vi.mock("@/lib/admin", async (orig) => ({
  ...(await orig<typeof import("@/lib/admin")>()),
  listRoles: (...a: unknown[]) => listRoles(...a),
  getRole: (...a: unknown[]) => getRole(...a),
  createRole: (...a: unknown[]) => createRole(...a),
  getFieldAclData: (...a: unknown[]) => getFieldAclData(...a),
  listTeams: (...a: unknown[]) => listTeams(...a),
  updateRolePermissions: vi.fn(),
}));

import { AccessPage } from "./AccessPage";

const ROLES = [
  role("r1", "Lễ tân", [{ entityName: "invoice", action: "view", accessLevel: "all" }]),
  role("r2", "Kế toán", []),
  role("r3", "Quản lý", [{ entityName: "invoice", action: "view", accessLevel: "all" }, { entityName: "visit", action: "view", accessLevel: "all" }]),
];

function renderPage(props: { app?: string; bang?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccessPage slug="acme" {...props} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listEntities.mockResolvedValue([ent("invoice", "Hoá đơn"), ent("visit", "Lịch hẹn")]);
  appsCatalog.mockResolvedValue([app("clinic"), app("shop")]);
  appNav.mockImplementation(async (n: string) => ({ app: app(n), items: [{ type: "entity", refId: n, label: n, href: `/${n === "clinic" ? "visit" : "invoice"}` }] }));
  adminApps.mockResolvedValue([]);
  listRoles.mockResolvedValue(ROLES.map(({ id, name, label }) => ({ id, name, label })));
  getRole.mockImplementation(async (id: string) => ROLES.find((r) => r.id === id));
  createRole.mockResolvedValue(role("r9", "Bảo vệ", []));
  getFieldAclData.mockResolvedValue({ defaultPolicy: "allow", grants: [{ roleId: "r1", entityName: "invoice", fieldName: "price", canView: false, canEdit: false }] });
  listTeams.mockResolvedValue([{ id: "t1", name: "A" }, { id: "t2", name: "B" }]);
});

describe("AccessPage: four states", () => {
  it("loading, then the matrix with every role and table, and the note about the customer's owner", async () => {
    renderPage();
    expect(document.querySelector(".slds-spinner")).toBeTruthy();
    expect(await screen.findByRole("combobox", { name: "Lễ tân trên Hoá đơn: Chỉ xem" })).toBeInTheDocument();
    expect(screen.getAllByRole("row")).toHaveLength(1 + ROLES.length);
    expect(screen.getByText(/chủ khách tự thêm nhân viên/)).toBeInTheDocument();
    expect(getRole).toHaveBeenCalledTimes(ROLES.length); // 1 list + one read per role
  });

  it("error: says so, and Thử lại reads again", async () => {
    listRoles.mockRejectedValueOnce(new Error("boom"));
    renderPage();
    expect(await screen.findByText(/Chưa đọc được vai và bảng/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(await screen.findByRole("combobox", { name: /Lễ tân trên Hoá đơn/ })).toBeInTheDocument();
  });

  it("no role yet: an empty state with the one way forward", async () => {
    listRoles.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByText("Chưa có vai nào")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "+ Vai mới" }));
    expect(await screen.findByRole("form", { name: "+ Vai mới" })).toBeInTheDocument();
  });

  it("no table yet: 'Chưa có bảng để phân quyền' with a link to the Dữ liệu step", async () => {
    listEntities.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByText("Chưa có bảng để phân quyền")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở Dữ liệu" })).toHaveAttribute("href", "/studio/du-an/acme/dung/du-lieu");
  });
});

describe("+ Vai mới", () => {
  it("opens a form in the page (no dialog) and creates a role that sees every table, read-only", async () => {
    renderPage();
    await screen.findByRole("combobox", { name: /Lễ tân trên Hoá đơn/ });
    fireEvent.click(screen.getByRole("button", { name: "+ Vai mới" }));
    const form = await screen.findByRole("form", { name: "+ Vai mới" });
    expect(document.querySelectorAll("[role=dialog],[role=alertdialog]")).toHaveLength(0);
    fireEvent.change(within(form).getByRole("textbox", { name: /Tên vai/ }), { target: { value: "Bảo vệ" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo vai" }));
    await waitFor(() => expect(createRole).toHaveBeenCalledTimes(1));
    expect(createRole).toHaveBeenCalledWith({ name: "bao_ve", label: "Bảo vệ", actions: [{ entityName: "*", action: "view", accessLevel: "all" }] });
    await waitFor(() => expect(screen.queryByRole("form", { name: "+ Vai mới" })).toBeNull());
  });

  it("a server refusal stays in the form", async () => {
    createRole.mockRejectedValue(new Error("Mã vai đã có"));
    renderPage();
    await screen.findByRole("combobox", { name: /Lễ tân trên Hoá đơn/ });
    fireEvent.click(screen.getByRole("button", { name: "+ Vai mới" }));
    const form = await screen.findByRole("form", { name: "+ Vai mới" });
    fireEvent.change(within(form).getByRole("textbox", { name: /Tên vai/ }), { target: { value: "Lễ tân" } });
    fireEvent.click(within(form).getByRole("button", { name: "Tạo vai" }));
    expect(await within(form).findByText("Mã vai đã có")).toBeInTheDocument();
  });
});

describe("app filter and related column", () => {
  it("lists one 'bù quyền còn thiếu' control per app of the project", async () => {
    adminApps.mockResolvedValue([{ id: "a1", name: "clinic", label: "Phòng khám" }]);
    renderPage();
    const aside = await screen.findByRole("complementary");
    expect(await within(aside).findByText("Quyền còn thiếu")).toBeInTheDocument();
    expect(await within(aside).findByText("Phòng khám")).toBeInTheDocument();
  });

  it("?app= keeps only that app's tables as columns; the filter sits in the page", async () => {
    renderPage({ app: "clinic" });
    await screen.findByRole("combobox", { name: /Lễ tân trên Lịch hẹn/ });
    expect(screen.queryByRole("combobox", { name: /trên Hoá đơn/ })).toBeNull();
    expect(screen.getByRole("combobox", { name: "Xem theo app" })).toHaveValue("clinic");
  });

  it("?bang= narrows to that table and offers the way back to all", async () => {
    renderPage({ bang: "invoice" });
    await screen.findByRole("combobox", { name: /Lễ tân trên Hoá đơn/ });
    expect(screen.queryByRole("combobox", { name: /trên Lịch hẹn/ })).toBeNull();
    expect(screen.getByRole("link", { name: "Xem tất cả" })).toHaveAttribute("href", "/studio/du-an/acme/dung/nguoi-dung-quyen");
  });

  it("the right column counts field rights and teams and links the three sub pages", async () => {
    renderPage();
    const aside = await screen.findByRole("complementary");
    expect(await within(aside).findByText("Quyền theo trường")).toBeInTheDocument();
    await waitFor(() => expect(within(aside).getByText("(1)")).toBeInTheDocument());
    expect(await within(aside).findByText("(2)")).toBeInTheDocument();
    expect(within(aside).getByRole("link", { name: "Mở Quyền theo trường" })).toHaveAttribute("href", "/studio/du-an/acme/dung/nguoi-dung-quyen/quyen-truong");
    expect(within(aside).getByRole("link", { name: "Mở Đội" })).toHaveAttribute("href", "/studio/du-an/acme/dung/nguoi-dung-quyen/doi");
    expect(within(aside).getByRole("link", { name: "Mở Chia sẻ" })).toHaveAttribute("href", "/studio/du-an/acme/dung/nguoi-dung-quyen/chia-se");
  });
});
