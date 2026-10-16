// AddFromPackPage (specs/074 lát f2-add-from-pack, T083): pick a pack → see the plan (writes nothing) → install.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";
import { qk } from "@/lib/query/keys";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/du-lieu/them-tu-goi",
  useRouter: () => ({ push, replace: vi.fn(), back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
const listBuiltinVerticals = vi.fn();
const dryRunInstall = vi.fn();
const installPack = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listBuiltinVerticals: (...a: unknown[]) => listBuiltinVerticals(...a),
  dryRunInstall: (...a: unknown[]) => dryRunInstall(...a),
  installPack: (...a: unknown[]) => installPack(...a),
}));

import { AddFromPackPage } from "./AddFromPackPage";

const cleanPlan = {
  entitiesToCreate: ["lich_hen"], entitiesToSkip: [], fieldsToAdd: [], nnToRegister: [], nnToSkip: [], layoutsToInstall: [],
  rolesToCreate: [], rolesToSkip: [], conflicts: [], samplesCount: 0,
};
const result = { entitiesCreated: ["lich_hen"], fieldsAdded: 0, layoutsInstalled: [], nnRegistered: [], rolesCreated: [], samplesInserted: 0 };
const packs = [
  { slug: "clinic", manifest: { name: "Phòng khám", version: "1.0.0", description: "Lịch hẹn và hồ sơ bệnh nhân" }, entityCount: 3 },
  { slug: "crm", manifest: { name: "Bán hàng", version: "2.1.0" }, entityCount: 5 },
];

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const inv = vi.spyOn(qc, "invalidateQueries");
  render(<QueryClientProvider client={qc}><AddFromPackPage slug="northwind" /></QueryClientProvider>);
  return { inv };
}
const pick = async (name: string) => fireEvent.click(await screen.findByRole("radio", { name: new RegExp(name) }));

beforeEach(() => {
  vi.clearAllMocks();
  listBuiltinVerticals.mockResolvedValue(packs);
  dryRunInstall.mockResolvedValue(cleanPlan);
  installPack.mockResolvedValue(result);
});

describe("AddFromPackPage", () => {
  it("empty server shows the honest line", async () => {
    listBuiltinVerticals.mockResolvedValue([]);
    renderPage();
    expect(await screen.findByText("Chưa có gói mẫu nào trên máy chủ này")).toBeTruthy();
  });

  it("viewing the plan never writes", async () => {
    renderPage();
    await pick("Phòng khám");
    fireEvent.click(screen.getByRole("button", { name: "Xem kế hoạch cài" }));
    await waitFor(() => expect(dryRunInstall).toHaveBeenCalledWith({ builtin: "clinic" }));
    expect(await screen.findByRole("button", { name: "Cài vào dự án" })).toBeTruthy();
    expect(installPack).not.toHaveBeenCalled();
  });

  it("install refreshes tables and apps, then shows the result", async () => {
    const { inv } = renderPage();
    await pick("Phòng khám");
    fireEvent.click(screen.getByRole("button", { name: "Xem kế hoạch cài" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cài vào dự án" }));
    await waitFor(() => expect(installPack).toHaveBeenCalledWith({ builtin: "clinic" }));
    await waitFor(() => {
      const keys = inv.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
      expect(keys).toContain(JSON.stringify(qk.entities()));
      expect(keys).toContain(JSON.stringify(qk.apps()));
    });
    expect(await screen.findByText(/Cài đặt hoàn tất/)).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a delivered project says so, links to Nâng cấp, and keeps the plan visible", async () => {
    installPack.mockRejectedValue(new ApiError(409, "PROJECT_DELIVERED_INSTALL_BLOCKED", "x"));
    renderPage();
    await pick("Bán hàng");
    fireEvent.click(screen.getByRole("button", { name: "Xem kế hoạch cài" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cài vào dự án" }));
    const link = await screen.findByRole("link", { name: "Mở Nâng cấp" });
    expect(link.getAttribute("href")).toBe("/studio/du-an/northwind/bao-tri/nang-cap");
    expect(screen.getByRole("button", { name: "Cài vào dự án" })).toBeTruthy();
  });

  it("a duplicate record is shown by its code", async () => {
    installPack.mockRejectedValue(new ApiError(409, "RECORD_DUPLICATE", "dup"));
    renderPage();
    await pick("Bán hàng");
    fireEvent.click(screen.getByRole("button", { name: "Xem kế hoạch cài" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cài vào dự án" }));
    expect(await screen.findByText(/RECORD_DUPLICATE/)).toBeTruthy();
  });

  it("a file that is not a pack is refused by what is wrong with it", async () => {
    renderPage();
    await screen.findByRole("radio", { name: /Phòng khám/ });
    const input = document.querySelector("input[type=file]") as HTMLInputElement;
    const file = new File(['{"manifest":{"name":"x"}}'], "x.json", { type: "application/json" });
    fireEvent.change(input, { target: { files: [file] } });
    expect(await screen.findByText(/thiếu mảng/)).toBeTruthy();
    expect(dryRunInstall).not.toHaveBeenCalled();
  });
});
