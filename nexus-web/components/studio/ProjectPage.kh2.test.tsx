// specs/073 kh2 T201-T203: Màn hình and Tự động are mounted in the project frame,.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render as rtlRender, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/northwind/dung/man-hinh";
let search = "";
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(search),
}));
const layoutProps = vi.fn();
vi.mock("./LayoutBuilder", () => ({
  LayoutBuilder: (p: { entityName?: string; initialView?: string }) => {
    layoutProps(p);
    return <p>trình sửa màn</p>;
  },
}));
const editorProps = vi.fn();
vi.mock("./screen/ScreenEditor", () => ({
  ScreenEditor: (p: { bang: string }) => {
    editorProps(p);
    return <p>sửa form trên màn thật</p>;
  },
}));
vi.mock("./build/AddFromPackPage", () => ({ AddFromPackPage: () => <p>thêm từ gói mẫu</p> }));
vi.mock("./build/AppsPage", () => ({ AppsPage: () => <p>danh sách app</p> }));
vi.mock("./WorkflowBuilder", () => ({ WorkflowBuilder: () => <p>trình dựng luật</p> }));
vi.mock("./build/RuleListPage", () => ({ RuleListPage: () => <p>danh sách luật</p> }));
vi.mock("./build/ApprovalPage", () => ({ ApprovalPage: () => <p>quy trình duyệt</p> }));
vi.mock("./build/QueuePage", () => ({ QueuePage: () => <p>hàng đợi</p> }));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  appsCatalog: vi.fn(async () => []),
  listEntities: vi.fn(async () => [
    { id: "e1", name: "crm_accounts", label: "Khách hàng", tableName: "obj_a" },
    { id: "e2", name: "crm_leads", label: "Khách tiềm năng", tableName: "obj_l" },
  ]),
  listFields: vi.fn(async () => []),
  getLayout: vi.fn(async () => null),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listApprovalProcesses: vi.fn(async () => [{ id: "a1" }, { id: "a2" }]),
  listWorkQueuesAdmin: vi.fn(async () => [{ id: "q1" }]),
}));
vi.mock("./InboundHookScreen", () => ({ InboundHookScreen: () => <p>cổng nhận</p> }));
vi.mock("@/components/screens/admin/BusinessCalendars", () => ({ BusinessCalendars: () => <p>lịch làm việc</p> }));

import { ProjectPage } from "./ProjectPage";

// The Tự động page counts its sub pages through react-query.
const render = (ui: ReactElement) => rtlRender(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{ui}</QueryClientProvider>);

beforeEach(() => {
  layoutProps.mockReset();
  editorProps.mockReset();
  search = "";
});

describe("Màn hình (T202)", () => {
  it("dung/man-hinh opens the form editor on the first table, with a link to App và menu", async () => {
    render(<ProjectPage slug="northwind" />);
    expect(await screen.findByText("sửa form trên màn thật")).toBeInTheDocument();
    expect(editorProps).toHaveBeenCalledWith(expect.objectContaining({ bang: "crm_accounts" }));
    expect(layoutProps).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: "App và menu" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh/app");
  });

  it("?bang= on the list page and :bang on the deep link both preselect the table; ?man= picks the view", async () => {
    search = "bang=crm_accounts&man=danh-sach";
    render(<ProjectPage slug="northwind" />);
    await waitFor(() => expect(editorProps).toHaveBeenLastCalledWith(expect.objectContaining({ bang: "crm_accounts", man: "danh-sach" })));
    expect(layoutProps).not.toHaveBeenCalled();
    pathname = "/studio/du-an/northwind/dung/man-hinh/crm_leads";
    search = "man=form";
    render(<ProjectPage slug="northwind" />);
    await waitFor(() => expect(editorProps).toHaveBeenLastCalledWith(expect.objectContaining({ bang: "crm_leads" })));
  });

  it("dung/man-hinh/app shows the app manager with the way back to Màn hình", () => {
    pathname = "/studio/du-an/northwind/dung/man-hinh/app";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("danh sách app")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Màn hình" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh");
  });
});

describe("Tự động (T203)", () => {
  it("dung/tu-dong mounts the rule list (its related column is tested in build/RuleListPage.test)", () => {
    pathname = "/studio/du-an/northwind/dung/tu-dong";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("danh sách luật")).toBeInTheDocument();
  });

  it.each([
    ["duyet", "quy trình duyệt"],
    ["hang-doi", "hàng đợi"],
    ["cong-nhan", "cổng nhận"],
    ["lich-lam-viec", "lịch làm việc"],
  ])("the %s page mounts its builder with '← Tự động' on top", (seg, text) => {
    pathname = `/studio/du-an/northwind/dung/tu-dong/${seg}`;
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText(text)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tự động" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong");
  });

  it("a single rule mounts its own page, with the way up to Tự động", () => {
    pathname = "/studio/du-an/northwind/dung/tu-dong/luat/ab12";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("trình dựng luật")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tự động" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong");
  });

  it("the pack page is mounted (f2-add-from-pack) and the old pack-screen link is gone", () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/them-tu-goi";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("thêm từ gói mẫu")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Dữ liệu" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
    expect(screen.queryByRole("link", { name: "← Gói mẫu (màn cũ)" })).toBeNull();
  });
});
