// specs/073 kh3 T234-T238: Người dùng & quyền, Báo cáo, Thương hiệu, Chạy thử and Giao cho khách mounted in the project frame.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/acme/dung/nguoi-dung-quyen";
let search = "";
let slug = "acme";
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(search),
}));
const props = vi.fn();
vi.mock("./build/AccessPage", () => ({ AccessPage: (p: object) => (props("access", p), <p>ma trận vai</p>) }));
vi.mock("./build/RolePage", () => ({ RolePage: (p: { roleId: string }) => (props("matrix", p), <p>ma trận quyền</p>) }));
vi.mock("@/components/screens/admin/FieldACL", () => ({ FieldACL: (p: object) => (props("fieldAcl", p), <p>quyền theo trường</p>) }));
vi.mock("@/components/screens/admin/TeamsList", () => ({ TeamsList: () => <p>các đội</p> }));
vi.mock("@/components/screens/admin/SharingAdmin", () => ({ SharingAdmin: () => <p>chia sẻ</p> }));
vi.mock("./build/ReportsPage", () => ({ ReportsPage: (p: object) => (props("reports", p), <p>danh sách báo cáo</p>) }));
vi.mock("./BrandingScreen", () => ({ BrandingScreen: () => <p>thương hiệu của khách</p> }));
vi.mock("./AclBackfillControl", () => ({ AclBackfillControl: ({ appId }: { appId: string }) => <button>{`Bù quyền ${appId}`}</button> }));
vi.mock("./trial/SampleDataByTable", () => ({ SampleDataByTable: ({ slug }: { slug: string }) => <p>{`mẫu: ${slug}`}</p> }));
vi.mock("./ReadinessChecklist", () => ({ AppsReadiness: () => <p>độ sẵn sàng</p> }));
vi.mock("@/lib/workspace", async (orig) => ({ ...(await orig<typeof import("@/lib/workspace")>()), inWorkspace: () => slug !== "northwind" }));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  homeSlug: () => "northwind",
}));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  adminApps: async () => [{ id: "a1", name: "crm", label: "CRM" }],
  adminApp: async () => ({ app: { id: "a1", name: "crm", label: "CRM" }, items: [{ id: "n1", itemType: "entity", refId: "e1", label: null, sortOrder: 1 }] }),
  listEntities: async () => [{ id: "e1", name: "crm_accounts", label: "Khách hàng" }],
  listFields: async () => [{ name: "phone", label: "Số điện thoại", fieldType: "phone" }],
  listRoles: async () => [{ id: "r1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" }],
  getSampleDataSummary: async () => ({ total: 0, entities: [] }),
  getPublishState: async () => ({ delivered: false, headVersion: null, pendingCount: 0 }),
}));

// Two stored restrictions: the related link counts them (specs/074 T208).
vi.mock("@/lib/admin", async (orig) => ({
  ...(await orig<typeof import("@/lib/admin")>()),
  getFieldAclData: async () => ({ defaultPolicy: "allow", grants: [{}, {}] }),
}));

import { ProjectPage } from "./ProjectPage";

const view = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <ProjectPage slug={slug} />
  </QueryClientProvider>
);
const go = (path: string, q = "", s = "acme") => {
  slug = s;
  pathname = `/studio/du-an/${s}/${path}`;
  search = q;
};

beforeEach(() => {
  props.mockReset();
  go("dung/nguoi-dung-quyen");
});

describe("Người dùng & quyền", () => {
  it("mounts the roles x tables page (its own test holds the matrix and the related column)", () => {
    render(view());
    expect(screen.getByText("ma trận vai")).toBeInTheDocument();
    expect(props).toHaveBeenCalledWith("access", { slug: "acme", app: undefined, bang: undefined });
  });

  it("?bang= and ?app= are handed to the page", () => {
    go("dung/nguoi-dung-quyen", "bang=crm_accounts&app=crm");
    render(view());
    expect(props).toHaveBeenCalledWith("access", { slug: "acme", app: "crm", bang: "crm_accounts" });
  });

  it("vai/<id> mounts the role page of that role (its own test holds the way back)", () => {
    go("dung/nguoi-dung-quyen/vai/r-7");
    render(view());
    expect(screen.getByText("ma trận quyền")).toBeInTheDocument();
    expect(props).toHaveBeenCalledWith("matrix", { slug: "acme", roleId: "r-7" });
  });

  it("quyen-truong hands ?bang= and ?truong= to FieldACL as props", async () => {
    go("dung/nguoi-dung-quyen/quyen-truong", "bang=crm_accounts&truong=phone");
    render(view());
    expect(await screen.findByText("quyền theo trường")).toBeInTheDocument();
    expect(props).toHaveBeenCalledWith("fieldAcl", { entity: "crm_accounts", field: "phone", onChange: expect.any(Function), inlineApply: true });
  });

  it.each([
    ["doi", "các đội"],
    ["chia-se", "chia sẻ"],
  ])("%s mounts its screen with the way back", (seg, text) => {
    go(`dung/nguoi-dung-quyen/${seg}`);
    render(view());
    expect(screen.getByText(text)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Người dùng & quyền" })).toBeInTheDocument();
  });
});

describe("Báo cáo", () => {
  it("mounts the report list page with ?bc= / ?bang= / ?app= from the URL", () => {
    go("dung/bao-cao", "bang=crm_accounts&bc=r1&app=crm");
    render(view());
    expect(screen.getByText("danh sách báo cáo")).toBeInTheDocument();
    expect(props).toHaveBeenCalledWith("reports", { slug: expect.any(String), bc: "r1", bang: "crm_accounts", app: "crm" });
  });

  it("a single report/dashboard config page is still pending (the viewer is outside the frame)", () => {
    go("dung/bao-cao/r1");
    render(view());
    expect(screen.queryByText("danh sách báo cáo")).toBeNull();
  });
});

describe("Thương hiệu", () => {
  it("mounts the branding screen", () => {
    go("dung/thuong-hieu");
    render(view());
    expect(screen.getByText("thương hiệu của khách")).toBeInTheDocument();
  });
});

describe("Chạy thử", () => {
  it("customer project: roles listed with the view-as button (#400 landed), sample tally, same-tab link to the app", async () => {
    go("dung/chay-thu");
    render(view());
    expect(await screen.findByText("mẫu: "+slug)).toBeInTheDocument();
    expect(await screen.findByText("Nhân viên kinh doanh")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Xem như Nhân viên kinh doanh" })).toBeEnabled();
    expect(screen.queryByText("Xem như vai chưa có trong bản Nexus này.")).toBeNull();
    const open = screen.getByRole("link", { name: "Mở app CRM" });
    expect(open).toHaveAttribute("href", "/home?app=crm");
    expect(open).not.toHaveAttribute("target");
  });

  it("at the Nexus workshop the button works", async () => {
    go("dung/chay-thu", "", "northwind");
    render(view());
    await screen.findByText("mẫu: "+slug);
    expect(await screen.findByRole("button", { name: "Xem như Nhân viên kinh doanh" })).toBeEnabled();
  });
});

describe("Giao cho khách", () => {
  it("shows the handover checklist and a blocked deliver button (the app is not ready)", async () => {
    go("dung/giao-cho-khach");
    render(view());
    expect(await screen.findByText("Chưa kiểm được độ sẵn sàng của app CRM.")).toBeInTheDocument();
    expect((await screen.findByRole("button", { name: "Giao cho khách: phiên bản 1" })).getAttribute("aria-disabled")).toBe("true");
  });

  it("typing the URL at the Nexus workshop gives an empty state, not the checklist", () => {
    go("dung/giao-cho-khach", "", "northwind");
    render(view());
    expect(screen.queryByText("Chưa có app nào để giao.")).toBeNull();
    expect(screen.getByText("Dự án của xưởng Nexus không giao cho khách.")).toBeInTheDocument();
  });
});
