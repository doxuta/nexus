// specs/073 kh1b1 T154/T157 (FR-015, FR-016): which page body a project path shows, and the StudioShell choice by path.
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/northwind";
let search = "";
const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ replace, push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(search),
}));
vi.mock("./build/EntityListPage", () => ({ EntityListPage: () => <p>danh sách bảng</p> }));
vi.mock("./EntityPage", () => ({
  EntityPage: ({ entity, field, onFieldChange, onDeleted }: { entity: { name: string }; field: string | null; onFieldChange: (n: string | null) => void; onDeleted: () => void }) => (
    <div>
      <p>trang bảng {entity.name} {field ?? "-"}</p>
      <button onClick={() => onFieldChange("email")}>chọn email</button>
      <button onClick={() => onFieldChange(null)}>đóng trường</button>
      <button onClick={onDeleted}>đã xoá</button>
    </div>
  ),
}));
vi.mock("./relmap/RelationMap", () => ({
  RelationMap: ({ focusEntity }: { focusEntity?: string }) => <p>sơ đồ {focusEntity ?? "tất cả"}</p>,
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listEntities: () => Promise.resolve([{ id: "1", name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" }]),
}));
vi.mock("@/components/slds/shell/GlobalHeader", () => ({ ShellGlobalHeader: () => <header>global</header> }));
// StudioShell now feeds the header a switcher and asks who the token is (kh5); both are covered in StudioShell.test.tsx.
vi.mock("./ProjectSwitcher", () => ({ ProjectSwitcher: () => <span>switcher</span> }));
vi.mock("./StudioSessionBanner", () => ({ StudioSessionBanner: () => null })); // covered in StudioSessionBanner.test.tsx
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push: vi.fn() }) }));
vi.mock("@/lib/studioContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => false }),
}));

import { ProjectPage } from "./ProjectPage";
import { StudioShell } from "./StudioShell";

beforeEach(() => {
  replace.mockReset();
  search = "";
});

describe("ProjectPage", () => {
  it("the project root replaces itself with the first page and draws nothing", () => {
    const { container } = render(<ProjectPage slug="northwind" />);
    expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/du-lieu");
    expect(container).toBeEmptyDOMElement();
  });

  it("the mounted Dữ liệu page renders the table list", () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("danh sách bảng")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });

  it("a declared page that is not built yet says so in the standard two lines, with the way up", () => {
    pathname = "/studio/du-an/northwind/dung/bao-cao/r1";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("Cấu hình một báo cáo chưa có trong bản Nexus này.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Dữ liệu" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });

  it("the table page mounts EntityPage for a table of the project, with ?truong= passed down", async () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/crm_accounts";
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ProjectPage slug="northwind" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("trang bảng crm_accounts -")).toBeInTheDocument();
  });

  it("a table code the project does not have says so with the way back, not a blank or the old hub", async () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/khong_co";
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ProjectPage slug="northwind" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Không có bảng “khong_co” trong dự án này")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tất cả bảng" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });

  it("the diagram page mounts the relation map; ?bang= (a query, not part of the pathname) focuses one table", () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/so-do";
    const { unmount } = render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("sơ đồ tất cả")).toBeInTheDocument();
    unmount();
    search = "bang=crm_accounts";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("sơ đồ crm_accounts")).toBeInTheDocument();
  });

  it("?truong= on the table page reaches EntityPage (deep link to a field)", async () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/crm_accounts";
    search = "truong=name";
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ProjectPage slug="northwind" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("trang bảng crm_accounts name")).toBeInTheDocument();
  });

  it("the table route writes ?truong= with replace and no scroll jump, clears it on close, returns to the list on delete,", async () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/crm_accounts";
    search = "truong=email";
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ProjectPage slug="northwind" />
      </QueryClientProvider>,
    );
    await screen.findByText("trang bảng crm_accounts email");
    fireEvent.click(screen.getByText("chọn email")); // same as the open field: no navigation
    expect(replace).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText("đóng trường"));
    expect(replace).toHaveBeenLastCalledWith(pathname, { scroll: false });
    fireEvent.click(screen.getByText("đã xoá"));
    expect(replace).toHaveBeenLastCalledWith("/studio/du-an/northwind/dung/du-lieu");
  });

  it("selecting a field on the table route replaces the URL with ?truong=<name>", async () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/crm_accounts";
    search = "";
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ProjectPage slug="northwind" />
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByText("chọn email"));
    expect(replace).toHaveBeenCalledWith(`${pathname}?truong=email`, { scroll: false });
  });

  it("the diagram route gives the canvas a height (EmbeddedSurface canvas)", () => {
    pathname = "/studio/du-an/northwind/dung/du-lieu/so-do";
    const { container } = render(<ProjectPage slug="northwind" />);
    expect(container.querySelector("[style*=min-height]")).not.toBeNull();
  });

  it("a path that matches no page shows a plain sentence and a link back, not a blank", () => {
    pathname = "/studio/du-an/northwind/khong-co";
    render(<ProjectPage slug="northwind" />);
    expect(screen.getByText("Không có trang này trong dự án")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Dữ liệu" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });
});

describe("StudioShell", () => {
  it("non-project routes get one main and no builder header; project routes get neither (the frame brings its own)", () => {
    pathname = "/studio/verticals";
    const old = render(
      <StudioShell>
        <p>cũ</p>
      </StudioShell>,
    );
    expect(screen.getAllByRole("main")).toHaveLength(1);
    old.unmount();
    pathname = "/studio/du-an/northwind/dung/du-lieu";
    render(
      <StudioShell>
        <p>mới</p>
      </StudioShell>,
    );
    expect(screen.queryByRole("main")).not.toBeInTheDocument();
    expect(screen.getByText("mới")).toBeInTheDocument();
  });
});
