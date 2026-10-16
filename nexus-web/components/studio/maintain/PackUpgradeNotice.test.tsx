// specs/079 T035a: the health-check page tells which installed packs have a newer shipped version.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const hoisted = vi.hoisted(() => ({ ok: true }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listPackInstalls: vi.fn(),
  listBuiltinVerticals: vi.fn(),
}));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => hoisted.ok }),
}));

import { listBuiltinVerticals, listPackInstalls } from "@/lib/api";
import { PackUpgradeNotice } from "./PackUpgradeNotice";

const inst = (slug: string, version: string) => ({ id: slug + version, slug, version, installedAt: "2026-10-01T00:00:00Z" });
const view = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PackUpgradeNotice slug="acme" />
    </QueryClientProvider>,
  );

beforeEach(() => {
  hoisted.ok = true;
  vi.mocked(listPackInstalls).mockReset();
  vi.mocked(listBuiltinVerticals).mockReset();
});

describe("PackUpgradeNotice", () => {
  it("one Info line per pack that is behind, with a link to Nâng cấp", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([inst("crm", "1.7.0"), inst("lms", "1.0.0")]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([
      { slug: "crm", manifest: { name: "Quản lý khách hàng", version: "1.8.0" }, entityCount: 1 },
      { slug: "lms", manifest: { name: "Đào tạo", version: "1.0.0" }, entityCount: 1 },
    ]);
    view();
    expect(await screen.findByText(/Gói mẫu Quản lý khách hàng có bản 1\.8\.0\. App đang dùng bản 1\.7\.0\./)).toBeInTheDocument();
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Xem nâng cấp" })).toHaveAttribute("href", expect.stringContaining("/nang-cap"));
  });

  it("nothing when no pack is behind", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([inst("crm", "1.8.0")]);
    vi.mocked(listBuiltinVerticals).mockResolvedValue([{ slug: "crm", manifest: { name: "CRM", version: "1.8.0" }, entityCount: 1 }]);
    const { container } = view();
    await new Promise((r) => setTimeout(r, 30));
    expect(container).toBeEmptyDOMElement();
  });

  it("a token that does not belong to the project makes zero calls", async () => {
    hoisted.ok = false;
    view();
    await new Promise((r) => setTimeout(r, 30));
    expect(listPackInstalls).not.toHaveBeenCalled();
  });
});
