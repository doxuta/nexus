// specs/078 lát u7-du-lieu-danh-sach-bang T055: the table of tables.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { DataTablesPage } from "./DataTablesPage";

vi.mock("@/components/studio/useProjectQuery", () => ({ useProjectQuery: vi.fn() }));
const base = { isPending: false, isError: false, error: null, data: undefined, refetch: vi.fn() };
// The page reads two queries (table list, usage): answer by key so each gets its own state.
const state = (o: object, usage: object = {}) =>
  vi.mocked(useProjectQuery).mockImplementation(((_s: string, key: string[]) => ({ ...base, ...(key[1] === "usage" ? usage : o) })) as never);
const ENT = [
  { name: "crm_accounts", label: "Khách hàng" },
  { name: "crm_leads", label: "Tiềm năng" },
];

beforeEach(() => vi.clearAllMocks());

describe("DataTablesPage", () => {
  it("loading: a spinner", () => {
    state({ isPending: true });
    render(<DataTablesPage slug="northwind" />);
    expect(screen.getByRole("status")).toBeInTheDocument();
  });
  it("empty: the sentence and the link to the Dữ liệu step", () => {
    state({ data: [] });
    render(<DataTablesPage slug="northwind" />);
    expect(screen.getByText(/Chưa có bảng nào. Anh thêm bảng đầu tiên ở bước Dữ liệu/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở bước Dữ liệu" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });
  it("one row per table: bold name link, Nhập CSV and Thùng rác at the row end, no letter tiles, no count columns", () => {
    state({ data: ENT });
    const { container } = render(<DataTablesPage slug="northwind" />);
    const row = screen.getByText("Khách hàng").closest("tr") as HTMLElement;
    expect(screen.getByText("Khách hàng").tagName).toBe("STRONG");
    expect(within(row).getByRole("link", { name: "Khách hàng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts");
    expect(within(row).getByRole("link", { name: "Nhập CSV vào Khách hàng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts/nhap");
    expect(within(row).getByRole("link", { name: "Thùng rác của Khách hàng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/du-lieu/crm_accounts/thung-rac");
    expect(container.querySelector(".slds-avatar, .slds-app-launcher__tile, article.slds-card")).toBeNull();
    expect(screen.queryByText(/Thêm 7 ngày qua/)).toBeNull();
  });
  it("search narrows the rows; no match says so", async () => {
    state({ data: ENT });
    render(<DataTablesPage slug="northwind" />);
    await userEvent.type(screen.getByRole("searchbox", { name: "Tìm bảng" }), "tiềm");
    expect(screen.queryByText("Khách hàng")).toBeNull();
    expect(screen.getByText("Tiềm năng")).toBeInTheDocument();
    await userEvent.clear(screen.getByRole("searchbox", { name: "Tìm bảng" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Tìm bảng" }), "zzz");
    expect(screen.getByText("Không có bảng nào khớp “zzz”.")).toBeInTheDocument();
  });
  it("usage: Bản ghi column right-aligned, lede carries the total and HH:mm, deleted table not counted", () => {
    const computedAt = new Date(2026, 9, 14, 9, 5).toISOString();
    state({ data: ENT }, { data: { current: { computedAt, records: [{ entity: "crm_accounts", label: "x", count: 1200 }, { entity: "gone", label: "y", count: 99 }] } } });
    render(<DataTablesPage slug="northwind" />);
    const row = screen.getByText("Khách hàng").closest("tr") as HTMLElement;
    expect(within(row).getByText("1.200")).toHaveClass("tabular-nums");
    expect(screen.getByRole("columnheader", { name: "Bản ghi" })).toBeInTheDocument();
    expect(screen.getByText(/2 bảng/).parentElement).toHaveTextContent("2 bảng, 1.200 bản ghi · cập nhật lúc 09:05");
  });
  it("usage failing: column blank with the sentence, table list still shown", () => {
    state({ data: ENT }, { isError: true });
    render(<DataTablesPage slug="northwind" />);
    expect(screen.getByText("Chưa đọc được số bản ghi")).toBeInTheDocument();
    expect(screen.getByText("Khách hàng")).toBeInTheDocument();
    expect(screen.queryByText(/bản ghi ·/)).toBeNull();
  });
});
