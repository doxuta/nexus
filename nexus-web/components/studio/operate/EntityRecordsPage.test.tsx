// specs/078 lát u7-du-lieu-danh-sach-bang T056/T058a: the embedded record list and the unknown-table gate.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { EntityRecordsPage, KnownEntityGate } from "./EntityRecordsPage";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));
const listProps = vi.fn();
vi.mock("@/components/layout/LayoutList", () => ({
  LayoutList: (p: { entity: string; onCreate: () => void; onOpen: (id: string) => void; onNavigate: (h: string) => void }) => {
    listProps(p);
    return (
      <div>
        <button onClick={p.onCreate}>Tạo mới</button>
        <button onClick={() => p.onOpen("r-1")}>Mở dòng</button>
        <button onClick={() => p.onNavigate(`/${p.entity}/deleted`)}>Thùng rác</button>
        <button onClick={() => p.onNavigate("/other/import")}>Lạ</button>
      </div>
    );
  },
}));
const state = (o: object) => vi.mocked(useProjectQuery).mockReturnValue({ isPending: false, isError: false, error: null, data: undefined, refetch: vi.fn(), ...o } as never);
const base = "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts";

beforeEach(() => vi.clearAllMocks());

describe("EntityRecordsPage", () => {
  it("known table: only the back row is drawn by the page; create/open/trash go to Studio routes", async () => {
    state({ data: [{ name: "crm_accounts", label: "Khách hàng" }] });
    const { container } = render(<EntityRecordsPage slug="northwind" bang="crm_accounts" />);
    expect(screen.getByRole("link", { name: "← Tất cả bảng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/du-lieu");
    expect(container.querySelectorAll("h1,h2,article.slds-card")).toHaveLength(0);
    await userEvent.click(screen.getByText("Tạo mới"));
    expect(push).toHaveBeenLastCalledWith(`${base}/moi`);
    await userEvent.click(screen.getByText("Mở dòng"));
    expect(push).toHaveBeenLastCalledWith(`${base}/r-1`);
    await userEvent.click(screen.getByText("Thùng rác"));
    expect(push).toHaveBeenLastCalledWith(`${base}/thung-rac`);
    push.mockClear();
    await userEvent.click(screen.getByText("Lạ"));
    expect(push).not.toHaveBeenCalled();
  });
  it("unknown table: the sentence and the link back, the list is never mounted (no data call)", () => {
    state({ data: [{ name: "crm_accounts", label: "Khách hàng" }] });
    render(<EntityRecordsPage slug="northwind" bang="admin" />);
    expect(screen.getByText("Không tìm thấy bảng này")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tất cả bảng" })).toBeInTheDocument();
    expect(listProps).not.toHaveBeenCalled();
  });
  it("tables still loading: a spinner, the list is not mounted", () => {
    state({ isPending: true });
    render(<EntityRecordsPage slug="northwind" bang="crm_accounts" />);
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(listProps).not.toHaveBeenCalled();
  });
  it("tables failed: the retry button, the list is not mounted", () => {
    state({ isError: true, error: new Error("x") });
    render(<EntityRecordsPage slug="northwind" bang="crm_accounts" />);
    expect(screen.getByRole("button", { name: "Thử lại" })).toBeInTheDocument();
    expect(listProps).not.toHaveBeenCalled();
  });

  // T058a: the gate every du-lieu/<bang>/… page (nhap, tao-nhieu) goes through; children = the data-loading screen.
  it.each([
    ["unknown table", { data: [{ name: "crm_accounts", label: "Khách hàng" }] }, "admin"],
    ["tables loading", { isPending: true }, "crm_accounts"],
    ["tables failed", { isError: true, error: new Error("x") }, "crm_accounts"],
  ])("KnownEntityGate (%s): children never rendered, so no /<bang>/… fetch", (_n, st, bang) => {
    state(st);
    const spy = vi.spyOn(global, "fetch");
    const child = vi.fn(() => <p>child</p>);
    render(<KnownEntityGate slug="northwind" bang={bang}>{child}</KnownEntityGate>);
    expect(child).not.toHaveBeenCalled();
    expect(spy).not.toHaveBeenCalled();
    spy.mockRestore();
  });
});
