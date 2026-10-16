// specs/073 kh4 T271-T277: Vận hành and Bảo trì mounted in the project frame; the pages whose engine is not built stay honest.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/northwind/van-hanh/nguoi-dung";
let search = "";
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(search),
}));
const auditProps = vi.fn();
const runProps = vi.fn();
const diffSlugs: string[] = [];
vi.mock("@/components/screens/admin/UserList", () => ({ UserList: () => <p>danh sách người dùng</p> }));
vi.mock("@/components/screens/admin/SsoProviders", () => ({ SsoProviders: () => <p>nhà cung cấp SSO</p> }));
vi.mock("@/components/screens/admin/AuditLog", () => ({
  AuditLog: (p: { initialActorId?: string }) => {
    auditProps(p);
    return <p>nhật ký hoạt động</p>;
  },
}));
vi.mock("./operate/InboundDeliveriesPage", () => ({ InboundDeliveriesPage: ({ cong }: { cong?: string }) => <p>dữ liệu nhận vào {cong}</p> }));
vi.mock("./operate/HealthPage", () => ({ HealthPage: ({ slug, khoang }: { slug: string; khoang?: string }) => <p>{`sức khoẻ ${slug} ${khoang ?? "-"}`}</p> }));
vi.mock("./operate/DataTablesPage", () => ({ DataTablesPage: () => <p>các bảng</p> }));
vi.mock("./operate/OperateOverview", () => ({ OperateOverview: () => <p>tổng quan tự động</p> }));
vi.mock("@/components/screens/admin/BackgroundJobs", () => ({ BackgroundJobs: () => <p>việc nền</p> }));
vi.mock("@/components/screens/admin/EmailLog", () => ({ EmailLog: () => <p>nhật ký email</p> }));
vi.mock("@/components/screens/admin/ApiKeys", () => ({ ApiKeys: () => <p>khoá API</p> }));
vi.mock("./operate/CredentialsPage", () => ({ CredentialsPage: () => <p>kho khoá kết nối</p> }));
vi.mock("@/components/screens/admin/DoctorScreen", () => ({ DoctorScreen: () => <p>bác sĩ hệ thống</p> }));
vi.mock("./WorkflowRunHistory", () => ({
  WorkflowRunHistory: (p: { workflowId: string; entityName?: string }) => {
    runProps(p);
    return <p>lịch sử chạy</p>;
  },
}));
vi.mock("./PackDiffPanel", () => ({
  PackDiffPanel: ({ slug }: { slug: string }) => {
    diffSlugs.push(slug);
    return <p>{`đối chiếu ${slug}`}</p>;
  },
}));
vi.mock("./maintain/ReadinessSection", () => ({ ReadinessSection: () => <p>độ sẵn sàng</p> }));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  homeSlug: () => "northwind",
}));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listWorkflows: async () => [
    { id: "w1", name: "Gửi thư chào", entityName: "crm_accounts" },
    { id: "w2", name: "Nhắc lịch", entityName: "crm_leads" },
  ],
  listPackInstalls: async () => [
    { id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-09-01T00:00:00Z" },
    { id: "i2", slug: "crm", version: "1.1.0", installedAt: "2026-10-01T00:00:00Z" },
    { id: "i3", slug: "lms", version: "1.0.0", installedAt: "2026-09-10T00:00:00Z" },
  ],
}));

import { ProjectPage } from "./ProjectPage";

const view = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <ProjectPage slug="northwind" />
  </QueryClientProvider>
);
const go = (path: string, q = "") => {
  pathname = `/studio/du-an/northwind/${path}`;
  search = q;
};

beforeEach(() => {
  auditProps.mockReset();
  runProps.mockReset();
  diffSlugs.length = 0;
  go("van-hanh/nguoi-dung");
});

describe("Vận hành", () => {
  it("Người dùng mounts UserList with the two related pages beside it,", () => {
    render(view());
    expect(screen.getByText("danh sách người dùng")).toBeInTheDocument();
    const aside = screen.getByRole("complementary");
    expect(within(aside).getAllByRole("link").map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
      ["Đăng nhập một lần", "/studio/du-an/northwind/van-hanh/nguoi-dung/dang-nhap-mot-lan"],
      ["Nhật ký hoạt động", "/studio/du-an/northwind/van-hanh/nguoi-dung/nhat-ky"],
    ]);
  });

  it("Đăng nhập một lần and Nhật ký have '← Người dùng'; ?nguoi= goes to the log as the actor filter", () => {
    go("van-hanh/nguoi-dung/dang-nhap-mot-lan");
    const a = render(view());
    expect(screen.getByText("nhà cung cấp SSO")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Người dùng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/nguoi-dung");
    a.unmount();
    go("van-hanh/nguoi-dung/nhat-ky", "nguoi=u-2");
    render(view());
    expect(screen.getByText("nhật ký hoạt động")).toBeInTheDocument();
    expect(auditProps).toHaveBeenLastCalledWith(expect.objectContaining({ initialActorId: "u-2" }));
  });

  it("Dữ liệu mounts the table directory", () => {
    go("van-hanh/du-lieu");
    render(view());
    expect(screen.getByText("các bảng")).toBeInTheDocument();
  });

  it("Tự động: the overview with the related column that lists only pages that exist", () => {
    go("van-hanh/tu-dong");
    render(view());
    expect(screen.getByText("tổng quan tự động")).toBeInTheDocument();
    const aside = screen.getByRole("complementary");
    expect(within(aside).getAllByRole("link").map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
      ["Lần chạy luật", "/studio/du-an/northwind/van-hanh/tu-dong/lan-chay"],
      ["Email đã gửi", "/studio/du-an/northwind/van-hanh/tu-dong/email"],
      ["Việc chạy nền", "/studio/du-an/northwind/van-hanh/tu-dong/viec-nen"],
      ["Dữ liệu nhận vào", "/studio/du-an/northwind/van-hanh/tu-dong/cong-nhan"],
      ["Khoá kết nối", "/studio/du-an/northwind/van-hanh/tu-dong/khoa-ket-noi"],
      ["Khoá truy cập", "/studio/du-an/northwind/van-hanh/tu-dong/khoa-truy-cap"],
    ]);
  });

  it.each([
    ["viec-nen", "việc nền"],
    ["email", "nhật ký email"],
    ["khoa-truy-cap", "khoá API"],
    ["khoa-ket-noi", "kho khoá kết nối"],
  ])("tu-dong/%s mounts its screen with '← Việc tự động & kết nối'", (seg, text) => {
    go(`van-hanh/tu-dong/${seg}`);
    render(view());
    expect(screen.getByText(text)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Việc tự động & kết nối" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong");
  });

  it("Lần chạy luật: no rule picked lists the rules; ?luat= shows that rule's run history", async () => {
    go("van-hanh/tu-dong/lan-chay");
    const a = render(view());
    const link = await screen.findByRole("link", { name: "Gửi thư chào" });
    expect(link).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/tu-dong/lan-chay?luat=w1");
    a.unmount();
    go("van-hanh/tu-dong/lan-chay", "luat=w2");
    render(view());
    expect(await screen.findByText("lịch sử chạy")).toBeInTheDocument();
    expect(runProps).toHaveBeenLastCalledWith({ workflowId: "w2", entityName: "crm_leads" });
  });

  it("Dữ liệu nhận vào mounts its own page and passes ?cong", () => {
    go("van-hanh/tu-dong/cong-nhan?cong=h1");
    render(view());
    expect(screen.getByText("dữ liệu nhận vào h1")).toBeInTheDocument();
    expect(screen.queryByText(/chưa có trong bản Nexus này/)).toBeNull();
  });
});

describe("Sức khoẻ & mức dùng", () => {
  it("mounts its own page and passes ?khoang (specs/078 u7-muc-dung-bieu-do)", () => {
    go("van-hanh/suc-khoe?khoang=90");
    render(view());
    expect(screen.getByText("sức khoẻ northwind 90")).toBeInTheDocument();
    expect(screen.queryByText(/chưa có trong bản Nexus này/)).toBeNull();
  });
});

describe("Bảo trì", () => {
  it("Khám sức khoẻ: the doctor and the per-app readiness", () => {
    go("bao-tri/kham");
    render(view());
    expect(screen.getByText("bác sĩ hệ thống")).toBeInTheDocument();
    expect(screen.getByText("độ sẵn sàng")).toBeInTheDocument();
  });

  it("Nâng cấp: the platform line, and one row per installed pack (not per install) with Đối chiếu", async () => {
    go("bao-tri/nang-cap");
    render(view());
    expect(screen.getByText("Nền tảng Nexus do Nexus cập nhật cho mọi dự án cùng lúc; chưa có công cụ nâng cấp theo dự án.")).toBeInTheDocument();
    const rows = await screen.findAllByRole("button", { name: /Đối chiếu/ });
    expect(rows).toHaveLength(2);
    expect(screen.queryByText(/đối chiếu crm/)).toBeNull();
    rows[0].click();
    expect(await screen.findByText("đối chiếu crm")).toBeInTheDocument();
  });

  it("Khôi phục: an id that is not a UUID is not a page (the plan page itself is tested in RestorePage.test)", () => {
    go("bao-tri/sao-luu/khoi-phuc/b1");
    render(view());
    expect(screen.getByText("Không có trang này trong dự án")).toBeInTheDocument();
  });

  it("Sức khoẻ mounts its own page", () => {
    go("van-hanh/suc-khoe");
    render(view());
    expect(screen.getByText("sức khoẻ northwind -")).toBeInTheDocument();
  });
});
