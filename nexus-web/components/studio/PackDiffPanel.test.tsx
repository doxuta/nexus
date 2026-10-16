// PackDiffPanel — KHUON-SONG-P1 rendering contract (../specs/011-pack-provenance/spec.md §4.4): the four
// honest per-item states render with their Vietnamese labels + changed paths, the untracked
// banner replaces the list for pre-provenance installs, and the summary line counts drift.
// lib/api is mocked at the module seam (same pattern as VerticalLibrary.test.tsx).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { PackDiffPanel } from "./PackDiffPanel";
import { diffPackInstall, type PackDiffReport } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, diffPackInstall: vi.fn() };
});

const REPORT: PackDiffReport = {
  slug: "crm",
  versions: ["1.2.0"],
  untracked: false,
  generatedAt: "2026-07-21T00:00:00Z",
  items: [
    { itemType: "entity", itemKey: "crm_accounts", packVersion: "1.2.0", status: "unchanged" },
    { itemType: "workflow", itemKey: "Định tuyến lead mới", packVersion: "1.2.0", status: "modified", changedPaths: ["isActive", "condition.match"] },
    { itemType: "dashboard", itemKey: "CRM Overview", packVersion: "1.2.0", status: "missing" },
    { itemType: "report", itemKey: "Pipeline theo stage", packVersion: "1.2.0", status: "undiffable" },
  ],
};

beforeEach(() => {
  vi.mocked(diffPackInstall).mockReset();
});

describe("PackDiffPanel", () => {
  it("renders the four states with business labels and the drift summary, no Badge", async () => {
    vi.mocked(diffPackInstall).mockResolvedValue(REPORT);
    const { container } = render(<PackDiffPanel slug="crm" />);

    expect(await screen.findByText("Nguyên bản gói")).toBeInTheDocument();
    expect(screen.getByText(/^Đã tuỳ chỉnh/)).toBeInTheDocument();
    expect(screen.getByText("Đã xoá khỏi dự án")).toBeInTheDocument();
    expect(screen.getByText("Không đối chiếu được")).toBeInTheDocument();
    expect(screen.getByText(/lệch ở 2 chỗ/)).toBeInTheDocument();
    expect(screen.getByText(/Còn nguyên 1\/4 mục so với bản gói v1\.2\.0/)).toBeInTheDocument();
    expect(screen.getByText("Luật tự động")).toBeInTheDocument();
    expect(container.querySelectorAll(".slds-badge")).toHaveLength(0);
    for (const bad of ["khuôn", "workspace", "Đối tượng", "Bố cục", "Dashboard"]) expect(container.textContent).not.toContain(bad);
  });

  it("raw drift paths appear only after opening 'Chi tiết', cut to 4 + ellipsis", async () => {
    vi.mocked(diffPackInstall).mockResolvedValue({
      ...REPORT,
      items: [{ itemType: "workflow", itemKey: "Luật A", packVersion: "1.2.0", status: "modified", changedPaths: ["p1", "p2", "p3", "p4", "p5"] }],
    });
    render(<PackDiffPanel slug="crm" />);
    await screen.findByText("Luật A");
    expect(screen.queryByText(/p1/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Chi tiết/ }));
    expect(screen.getByText(/p1, p2, p3, p4…/)).toBeInTheDocument();
  });

  it("all-clean report says everything matches", async () => {
    vi.mocked(diffPackInstall).mockResolvedValue({
      ...REPORT,
      items: [{ itemType: "entity", itemKey: "e", packVersion: "1.2.0", status: "unchanged" }],
    });
    render(<PackDiffPanel slug="crm" />);
    expect(await screen.findByText(/Cả 1 mục còn nguyên so với bản gói v1\.2\.0/)).toBeInTheDocument();
  });

  it("untracked install renders the honest sentence instead of a list", async () => {
    vi.mocked(diffPackInstall).mockResolvedValue({ ...REPORT, untracked: true, items: [] });
    render(<PackDiffPanel slug="crm" />);
    expect(await screen.findByText(/cài trước khi hệ thống ghi nguồn gốc từng mục nên chưa đối chiếu được/)).toBeInTheDocument();
    expect(screen.queryByText("Nguyên bản gói")).toBeNull();
    expect(screen.queryByText(/Cài lại khuôn/)).toBeNull();
  });

  it("shows loading, then an error sentence", async () => {
    vi.mocked(diffPackInstall).mockRejectedValue(new Error("x"));
    render(<PackDiffPanel slug="crm" />);
    expect(screen.getByText(/Đang đối chiếu/)).toBeInTheDocument();
    expect(await screen.findByText("Không đối chiếu được gói này.")).toBeInTheDocument();
  });

  it("a protected item keeps its status and gains the 'Giữ bản riêng' marker", async () => {
    vi.mocked(diffPackInstall).mockResolvedValue({
      ...REPORT,
      items: [
        { itemType: "layout", itemKey: "tk_tickets:list", packVersion: "1.2.0", status: "modified", changedPaths: ["columns"], protected: true },
        { itemType: "layout", itemKey: "tk_tickets:detail", packVersion: "1.2.0", status: "modified", changedPaths: ["sections"] },
      ],
    });
    render(<PackDiffPanel slug="crm" />);
    await screen.findByText("tk_tickets:list");
    expect(screen.getAllByText(/^Đã tuỳ chỉnh/)).toHaveLength(2);
    expect(screen.getAllByText("Giữ bản riêng")).toHaveLength(1);
    expect(screen.getByText("tk_tickets:list").closest("li")).toHaveTextContent("Giữ bản riêng");
  });
});
