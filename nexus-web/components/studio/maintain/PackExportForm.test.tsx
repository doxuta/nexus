// specs/079 f7-xuat-goi T042: export the project as a template-pack file, in place on bao-tri/nang-cap.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const hoisted = vi.hoisted(() => ({ ok: true, home: "northwind" }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: vi.fn(),
  adminApps: vi.fn(),
  adminApp: vi.fn(),
  exportVertical: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
  homeSlug: () => hoisted.home,
}));

import { ApiError, adminApp, adminApps, exportVertical, listEntities } from "@/lib/api";
import { PackExportForm } from "./PackExportForm";

const ents = [
  { id: "e1", name: "patients", label: "Bệnh nhân", tableName: "t1" },
  { id: "e2", name: "visits", label: "Lịch khám", tableName: "t2" },
  { id: "e3", name: "invoices", label: "Hoá đơn", tableName: "t3" },
];
const view = (project = "acme") =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PackExportForm project={project} />
    </QueryClientProvider>,
  );
const runBtn = () => screen.getByRole("button", { name: /^Xuất/ });
const pick = async (label: string) => fireEvent.click((await screen.findByText(label)).closest("tr")!.querySelector("input[type=checkbox]")!);
let anchors: HTMLAnchorElement[];

beforeEach(() => {
  hoisted.ok = true;
  hoisted.home = "northwind";
  vi.mocked(listEntities).mockReset();
  vi.mocked(adminApps).mockReset();
  vi.mocked(listEntities).mockResolvedValue(ents as never);
  vi.mocked(adminApps).mockResolvedValue([{ id: "a1", name: "clinic", label: "Phòng khám", icon: "heart", color: null, isDefault: false, sortOrder: 0, description: null, navCount: 2 }] as never);
  vi.mocked(adminApp).mockResolvedValue({ app: {} as never, items: [{ id: "n1", itemType: "entity", refId: "e1", label: null, sortOrder: 0 }, { id: "n2", itemType: "entity", refId: "e2", label: null, sortOrder: 1 }] });
  vi.mocked(exportVertical).mockReset();
  vi.mocked(exportVertical).mockResolvedValue({ pack: { manifest: { slug: "clinic" } } as never, warnings: [] });
  anchors = [];
  URL.createObjectURL = vi.fn(() => "blob:x");
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { anchors.push(this); });
});

describe("PackExportForm", () => {
  it("no table picked: Xuất is disabled", async () => {
    view();
    await screen.findByText("Bệnh nhân");
    expect(runBtn()).toBeDisabled();
  });

  it("choosing an app preselects its menu tables and sends the app identity", async () => {
    view();
    await screen.findByText("Bệnh nhân");
    fireEvent.change(await screen.findByLabelText("Lấy bảng theo app"), { target: { value: "a1" } });
    await waitFor(() => expect(runBtn()).toBeEnabled());
    fireEvent.click(runBtn());
    await waitFor(() => expect(exportVertical).toHaveBeenCalledWith(["patients", "visits"], false, { slug: "clinic", name: "Phòng khám", icon: "heart" }));
    expect(anchors[0].download).toBe("pack-clinic.json");
    expect(URL.revokeObjectURL).toHaveBeenCalled();
    expect(await screen.findByText("Đã tải tệp về.")).toBeInTheDocument();
  });

  it("includeData is off by default; turning it on warns and adds -co-du-lieu to the file name", async () => {
    view("northwind");
    await pick("Hoá đơn");
    expect(screen.getByLabelText("Kèm bản ghi đang có trong dự án")).not.toBeChecked();
    expect(screen.queryByText("Tệp sẽ chứa bản ghi thật, kể cả dữ liệu của khách.")).toBeNull();
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    expect(screen.getByText("Tệp sẽ chứa bản ghi thật, kể cả dữ liệu của khách.")).toBeInTheDocument();
    fireEvent.click(runBtn());
    await waitFor(() => expect(exportVertical).toHaveBeenCalledWith(["invoices"], true, undefined));
    expect(anchors[0].download).toBe("vertical-pack-co-du-lieu.json");
  });

  it("customer project + includeData: confirm names the project first; Esc cancels and nothing is exported", async () => {
    view("acme");
    await pick("Hoá đơn");
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    fireEvent.click(runBtn());
    expect(screen.getByText("Tệp sẽ chứa bản ghi thật của khách acme. Đừng dùng tệp này làm gói mẫu cho khách khác.")).toBeInTheDocument();
    expect(exportVertical).not.toHaveBeenCalled();
    fireEvent.keyDown(screen.getByRole("button", { name: "Huỷ" }), { key: "Escape" });
    expect(screen.queryByText(/Đừng dùng tệp này/)).toBeNull();
    expect(exportVertical).not.toHaveBeenCalled();
    fireEvent.click(runBtn());
    fireEvent.click(screen.getByRole("button", { name: "Vẫn xuất" }));
    await waitFor(() => expect(exportVertical).toHaveBeenCalledTimes(1));
  });

  it("focus returns to Xuất after Huỷ, after Vẫn xuất finishes, and after a direct export", async () => {
    view("acme");
    await pick("Hoá đơn");
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    fireEvent.click(runBtn());
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(runBtn()).toHaveFocus());
    fireEvent.click(runBtn());
    fireEvent.click(screen.getByRole("button", { name: "Vẫn xuất" }));
    await screen.findByText("Đã tải tệp về.");
    await waitFor(() => expect(runBtn()).toHaveFocus());
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    runBtn().focus();
    fireEvent.click(runBtn());
    await waitFor(() => expect(exportVertical).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(runBtn()).toHaveFocus());
  });

  it("the workshop project (home) exports data without the customer confirm", async () => {
    view("northwind");
    await pick("Hoá đơn");
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    fireEvent.click(runBtn());
    await waitFor(() => expect(exportVertical).toHaveBeenCalledTimes(1));
    expect(screen.queryByText(/Đừng dùng tệp này/)).toBeNull();
  });

  it("warnings are listed as 'Không đóng gói được', never dropped", async () => {
    vi.mocked(exportVertical).mockResolvedValue({ pack: {} as never, warnings: ["Luật A có webhook", "Báo cáo B có liên kết"] });
    view("northwind");
    await pick("Hoá đơn");
    fireEvent.click(runBtn());
    expect(await screen.findByText("Không đóng gói được (2)")).toBeInTheDocument();
    expect(screen.getByText("Luật A có webhook")).toBeInTheDocument();
    expect(screen.getByText("Báo cáo B có liên kết")).toBeInTheDocument();
  });

  it("a 4xx shows a Vietnamese sentence in place and no download", async () => {
    vi.mocked(exportVertical).mockRejectedValue(new ApiError(403, "FORBIDDEN", "no"));
    view("northwind");
    await pick("Hoá đơn");
    fireEvent.click(runBtn());
    expect(await screen.findByRole("alert")).toHaveTextContent("Bạn không có quyền thực hiện thao tác này.");
    expect(anchors).toHaveLength(0);
    expect(screen.queryByText("Đã tải tệp về.")).toBeNull();
  });

  it("listEntities fails: error + retry, never the false 'no tables' sentence", async () => {
    vi.mocked(listEntities).mockRejectedValueOnce(new ApiError(500, "INTERNAL", "x"));
    view("northwind");
    const retry = await screen.findByRole("button", { name: "Thử lại" });
    expect(screen.queryByText("Dự án chưa có bảng nào để đóng gói.")).toBeNull();
    fireEvent.click(retry);
    expect(await screen.findByText("Bệnh nhân")).toBeInTheDocument();
  });

  it("no tables really: says so", async () => {
    vi.mocked(listEntities).mockResolvedValue([]);
    view("northwind");
    expect(await screen.findByText("Dự án chưa có bảng nào để đóng gói.")).toBeInTheDocument();
  });

  it("adminApps fails: the app picker shows the error", async () => {
    vi.mocked(adminApps).mockRejectedValue(new ApiError(500, "INTERNAL", "x"));
    view("northwind");
    expect(await screen.findAllByRole("alert")).not.toHaveLength(0);
  });

  it("adminApp fails: no app identity is kept, Xuất sends none", async () => {
    vi.mocked(adminApp).mockRejectedValue(new ApiError(500, "INTERNAL", "x"));
    view("northwind");
    await pick("Hoá đơn");
    fireEvent.change(await screen.findByLabelText("Lấy bảng theo app"), { target: { value: "a1" } });
    await screen.findByRole("alert");
    fireEvent.click(runBtn());
    await waitFor(() => expect(exportVertical).toHaveBeenCalledWith(["invoices"], false, undefined));
  });

  it("switching project resets includeData and closes the confirm: no call carries the old project's choice", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const ui = (p: string) => (
      <QueryClientProvider client={client}><PackExportForm project={p} /></QueryClientProvider>
    );
    const { rerender } = render(ui("acme"));
    await pick("Hoá đơn");
    fireEvent.click(screen.getByLabelText("Kèm bản ghi đang có trong dự án"));
    fireEvent.click(runBtn());
    expect(screen.getByText(/Đừng dùng tệp này/)).toBeInTheDocument();
    rerender(ui("beta"));
    expect(screen.queryByText(/Đừng dùng tệp này/)).toBeNull();
    await screen.findByText("Bệnh nhân");
    expect(screen.getByLabelText("Kèm bản ghi đang có trong dự án")).not.toBeChecked();
    expect(exportVertical).not.toHaveBeenCalled();
  });

  it("no dialog, no card; the blob never reaches storage", async () => {
    const set = vi.spyOn(Storage.prototype, "setItem");
    const { container } = view("northwind");
    await pick("Hoá đơn");
    fireEvent.click(runBtn());
    await screen.findByText("Đã tải tệp về.");
    expect(document.querySelector("[role=dialog]")).toBeNull();
    expect(container.querySelector(".slds-card")).toBeNull();
    expect(set.mock.calls.filter(([, v]) => String(v).includes("blob:"))).toHaveLength(0);
  });

  it("the token of this tab does not belong to the project: zero reads", async () => {
    hoisted.ok = false;
    view();
    await new Promise((r) => setTimeout(r, 20));
    expect(listEntities).not.toHaveBeenCalled();
    expect(adminApps).not.toHaveBeenCalled();
  });
});
