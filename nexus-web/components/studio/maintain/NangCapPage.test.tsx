// specs/079 f7-nang-cap-hien-co T031/T031a: "Nâng cấp" page = honest platform line, one row per installed pack with
// version-in-use vs shipped version, "Đối chiếu" per row, and a folded install history.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const hoisted = vi.hoisted(() => ({ ok: true }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listPackInstalls: vi.fn(),
  listBuiltinVerticals: vi.fn(),
  planPackUpgrade: vi.fn(),
  listEntities: vi.fn(async () => [{ name: "patients", label: "Bệnh nhân", tableName: "t" }]),
  adminApps: vi.fn(async () => []),
  diffPackInstall: vi.fn(async () => ({ slug: "crm", versions: ["1.7.0"], untracked: false, generatedAt: "", items: [{ itemType: "entity", itemKey: "crm_accounts", packVersion: "1.7.0", status: "unchanged" }] })),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
}));

import { listBuiltinVerticals, listPackInstalls, planPackUpgrade } from "@/lib/api";
import { NangCapPage } from "./NangCapPage";

const inst = (slug: string, version: string, installedAt: string, result?: Record<string, unknown>) => ({ id: `${slug}${version}`, slug, version, installedAt, result });
const built = (slug: string, version: string, name = slug) => ({ slug, manifest: { name, version }, entityCount: 3 });
const view = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <NangCapPage slug="acme" />
    </QueryClientProvider>,
  );

beforeEach(() => {
  hoisted.ok = true;
  vi.mocked(listPackInstalls).mockReset();
  vi.mocked(listBuiltinVerticals).mockReset();
});

describe("NangCapPage", () => {
  it("no installs: one honest sentence, no list, no history", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([]);
    view();
    expect(await screen.findByText("Dự án chưa cài gói mẫu nào, nên chưa có gì để nâng cấp.")).toBeInTheDocument();
    expect(screen.queryByText(/Lịch sử cài/)).toBeNull();
    expect(screen.getByText(/Nền tảng Nexus do Nexus cập nhật/)).toBeInTheDocument();
  });

  it("each pack shows version in use, install date and a status; Đối chiếu opens the diff under the row", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      inst("crm", "1.7.0", "2026-10-02T03:00:00Z", { entitiesCreated: ["a", "b"], samplesInserted: 5 }),
      inst("crm", "1.6.0", "2026-09-01T03:00:00Z"),
      inst("lms", "1.0.0", "2026-09-10T03:00:00Z"),
    ]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([built("crm", "1.8.0", "Quản lý khách hàng") as never]);
    view();
    const row = (await screen.findByText("Quản lý khách hàng")).closest("li") as HTMLElement;
    expect(row).toHaveTextContent("Đang dùng 1.7.0 · cài ngày 02/10/2026");
    expect(within(row).getByText("Có bản mới 1.8.0")).toBeInTheDocument();
    // the older 1.6.0 ledger row never makes a second pack row
    expect(screen.getAllByRole("listitem").filter((li) => li.textContent?.includes("Đang dùng"))).toHaveLength(2);
    const lms = screen.getAllByText("lms")[0].closest("li") as HTMLElement;
    expect(within(lms).queryByRole("link")).toBeNull(); // not shipped with Nexus: only Đối chiếu
    expect(within(lms).getByRole("button", { name: /Đối chiếu/ })).toBeInTheDocument();
    fireEvent.click(within(row).getByRole("button", { name: /Đối chiếu/ }));
    expect(await within(row).findByText("crm_accounts")).toBeInTheDocument();
    expect(document.querySelectorAll(".slds-badge")).toHaveLength(0);
  });

  it("same version reads 'Đã là bản mới nhất'", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([inst("crm", "1.8.0", "2026-10-02T03:00:00Z")]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([built("crm", "1.8.0") as never]);
    view();
    expect(await screen.findByText("Đã là bản mới nhất")).toBeInTheDocument();
    // nothing to upgrade to at the same version: no "Xem nâng cấp" link, "Đối chiếu" stays
    expect(screen.queryByText("Xem nâng cấp")).toBeNull();
    expect(screen.getByRole("button", { name: "Đối chiếu" })).toBeInTheDocument();
  });

  it("history is folded, newest first, with counts only when the ledger has them", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      inst("crm", "1.6.0", "2026-09-01T03:00:00Z"),
      inst("crm", "1.7.0", "2026-10-02T03:00:00Z", { entitiesCreated: ["a", "b"], samplesInserted: 5 }),
    ]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([]);
    view();
    const sum = await screen.findByText("Lịch sử cài (2)");
    fireEvent.click(sum);
    const rows = screen.getAllByTestId("install-history-row");
    expect(rows[0]).toHaveTextContent("v1.7.0");
    expect(rows[0]).toHaveTextContent("2 bảng · 5 bản ghi mẫu");
    expect(rows[1]).toHaveTextContent("v1.6.0");
    expect(rows[1]).not.toHaveTextContent("bảng ·");
    expect(vi.mocked(listPackInstalls)).toHaveBeenCalledTimes(1);
  });

  it("a token that does not belong to the project makes zero calls", async () => {
    hoisted.ok = false;
    view();
    await new Promise((r) => setTimeout(r, 30));
    expect(listPackInstalls).not.toHaveBeenCalled();
    expect(listBuiltinVerticals).not.toHaveBeenCalled();
  });

  it("a failed read says so with a retry, not an empty page", async () => {
    vi.mocked(listPackInstalls).mockRejectedValue(new Error("boom"));
    vi.mocked(listBuiltinVerticals).mockResolvedValue([]);
    view();
    await waitFor(() => expect(screen.getByRole("button", { name: /Thử lại/ })).toBeInTheDocument());
  });

  it("ends with the export form, even when no pack is installed (specs/079 FR-041c)", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([]);
    view();
    expect(await screen.findByRole("heading", { name: "Xuất thành tệp gói mẫu" })).toBeInTheDocument();
    expect(await screen.findByText("Bệnh nhân")).toBeInTheDocument();
  });

  it("Nâng cấp bằng tệp: a wrong file is refused in Vietnamese before any request; a right one opens the plan on {pack}", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([inst("crm", "1.7.0", "2026-10-02T03:00:00Z")]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([]);
    vi.mocked(planPackUpgrade).mockReset().mockResolvedValue({ slug: "crm", fromVersions: ["1.7.0"], toVersion: "1.8.0", source: "upload", untracked: false, delivered: false, planHash: "h", summary: {}, warnings: [], items: [] });
    const { container } = view();
    fireEvent.click(await screen.findByRole("button", { name: "Nâng cấp bằng tệp: crm" }));
    const input = () => container.querySelector("input[type=file]") as HTMLInputElement;
    const send = (manifest: Record<string, unknown>) => {
      const f = new File([JSON.stringify({ manifest, entities: [{ name: "a" }] })], "p.json", { type: "application/json" });
      fireEvent.change(input(), { target: { files: [f] } });
    };
    send({ slug: "lms", version: "1.8.0" });
    expect(await screen.findByText(/không phải gói “crm”/)).toBeInTheDocument();
    expect(planPackUpgrade).not.toHaveBeenCalled();
    send({ slug: "crm", version: "1.8.0" });
    await waitFor(() => expect(planPackUpgrade).toHaveBeenCalledTimes(1));
    expect(vi.mocked(planPackUpgrade).mock.calls[0][1]).toHaveProperty("pack");
  });
});
