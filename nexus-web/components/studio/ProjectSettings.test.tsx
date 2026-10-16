// specs/073 so2 T480/T486: customer project settings; the page closes when the ledger fails.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

const replace = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace, push: vi.fn() }) }));
const api = vi.hoisted(() => ({
  listProjects: vi.fn(), patchProject: vi.fn(), healthCheckProject: vi.fn(), retireTenant: vi.fn(), listTenants: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/studioContext", () => ({ homeSlug: () => "northwind" }));

import { ProjectSettings } from "./ProjectSettings";

const row = (o = {}) => ({
  tenantId: "t-1", slug: "acme", name: "Acme", tenantStatus: "active", industry: "Giáo dục", origin: null, originPack: null,
  stage: "building", createdAt: "2026-10-01T00:00:00Z", trialSince: null, deliveredAt: null, branded: false,
  health: { status: "warn", critical: 0, warn: 1, checks: [{ checkId: "no_owner", severity: "warn", count: 1 }], appsNotReady: [], checkedAt: null },
  counts: {}, lastActiveAt: null, ...o,
});
const mount = (slug: string) =>
  render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><ProjectSettings key={slug} slug={slug} /></QueryClientProvider>);

beforeEach(() => { Object.values(api).forEach((f) => f.mockReset()); replace.mockReset(); });

describe("ProjectSettings customer", () => {
  it("name and code are read-only text; industry saves via patchProject with the row tenantId", async () => {
    api.listProjects.mockResolvedValue([row()]);
    api.patchProject.mockResolvedValue(row({ industry: "Y tế" }));
    mount("acme");
    expect(await screen.findByText("Acme")).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: /^Tên/ })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Ngành" }), { target: { value: "Y tế" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu ngành" }));
    await waitFor(() => expect(api.patchProject).toHaveBeenCalledWith("t-1", { industry: "Y tế" }));
  });

  it("Chạy thử switch patches trial; Khám lại runs the check and lists checks", async () => {
    api.listProjects.mockResolvedValue([row()]);
    api.patchProject.mockResolvedValue(row({ trialSince: "2026-10-02T00:00:00Z" }));
    api.healthCheckProject.mockResolvedValue(row().health);
    mount("acme");
    fireEvent.click(await screen.findByRole("switch", { name: /^Chạy thử/ }));
    await waitFor(() => expect(api.patchProject).toHaveBeenCalledWith("t-1", { trial: true }));
    fireEvent.click(screen.getByRole("button", { name: "Khám lại" }));
    await waitFor(() => expect(api.healthCheckProject).toHaveBeenCalledWith("t-1"));
    expect(screen.getByText(/no_owner/)).toBeInTheDocument();
  });

  it("retire is an inline confirm naming the project; no purge button on a live project", async () => {
    api.listProjects.mockResolvedValue([row()]);
    api.retireTenant.mockResolvedValue(undefined);
    mount("acme");
    fireEvent.click(await screen.findByRole("button", { name: "Ngưng dự án…" }));
    expect(screen.getByText(/Ngưng dự án Acme/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
    expect(screen.queryByRole("button", { name: /Xoá hẳn/ })).toBeNull();
    expect(screen.getByText(/xoá hẳn ở mục Đã ngưng/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Ngưng dự án" }));
    await waitFor(() => expect(api.retireTenant).toHaveBeenCalledWith("t-1"));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/studio"));
  });
});

describe("ProjectSettings owner tier group (078 T183)", () => {
  it("customer settings mount the Chủ khách group for the ledger row id; the workshop does not", async () => {
    api.listProjects.mockResolvedValue([row()]);
    api.listTenants.mockResolvedValue([{ id: "t-1", slug: "acme", name: "Acme", status: "active", createdAt: "", usableAdminCount: 1, usableOwnerCount: 0, convertibleAdminCount: 1 }]);
    const { unmount } = mount("acme");
    expect(await screen.findByText(/Hiện có 0 chủ khách đăng nhập được và 1 quản trị viên/)).toBeInTheDocument();
    unmount();
    api.listTenants.mockClear();
    mount("northwind");
    expect(screen.queryByText(/Hiện có/)).toBeNull();
    expect(api.listTenants).not.toHaveBeenCalled();
  });
});

describe("ProjectSettings closes when the ledger fails (FR-027)", () => {
  it("5xx: error + retry, no tenant call", async () => {
    api.listProjects.mockRejectedValue(new Error("boom"));
    mount("acme");
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Ngưng dự án…" })).toBeNull();
    expect(api.patchProject).not.toHaveBeenCalled();
  });
  it("no row for the slug: empty state, nothing mounted", async () => {
    api.listProjects.mockResolvedValue([row({ slug: "other" })]);
    mount("acme");
    expect(await screen.findByText("Không tìm thấy dự án này")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Ngưng dự án…" })).toBeNull();
  });
});

describe("ProjectSettings backup link (specs/079 f7-sao-luu-lich-xoa T130)", () => {
  it("exactly one Lich sao luu link to the backup tab of this project", async () => {
    api.listProjects.mockResolvedValue([row()]);
    mount("acme");
    const links = await screen.findAllByRole("link", { name: "Lịch sao lưu" });
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute("href")).toContain("bao-tri/sao-luu");
  });
});
