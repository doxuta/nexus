// ReportList (specs/074 lát f2-report-list-view, T248): groups, ?bang=/?app= filters, selection links, four states.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
vi.mock("next/navigation", async (orig) => ({ ...(await orig<typeof import("next/navigation")>()), useRouter: () => ({ push: vi.fn() }), usePathname: () => "/studio/du-an/demo/dung/bao-cao" }));

import { ReportList } from "./ReportList";
import { mkChange, WithPreview } from "@/test/previewProbe";
import type { DashboardSummary, ReportSummary } from "@/lib/api";

const R: ReportSummary[] = [
  { id: "r1", name: "Hợp đồng hiệu lực", entityName: "crm_contracts", type: "tabular", isActive: true },
  { id: "r2", name: "Pipeline", entityName: "crm_opportunities", type: "summary", isActive: true },
];
const D: DashboardSummary[] = [
  { id: "d1", name: "Tổng quan bán hàng", widgets: [{ id: "w", title: "x", type: "kpi", entity: "crm_opportunities", metric: "count" }] },
  { id: "d2", name: "Hỗ trợ", widgets: [{ id: "w", title: "x", type: "kpi", entity: "tk_tickets", metric: "count" }] },
];
const base = {
  reports: R, dashboards: D, entityNames: null, entity: undefined, selectedId: "r1", status: "ready" as const,
  reportHref: (id: string) => `/s?bc=${id}`, dashboardHref: (id: string) => `/dashboards/${id}`,
};

describe("ReportList", () => {
  it("groups reports and dashboards; the selected report is aria-current", () => {
    render(<ReportList {...base} />);
    expect(screen.getByRole("heading", { name: "Báo cáo" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Trang tổng quan" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Hợp đồng hiệu lực" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Pipeline" })).toHaveAttribute("href", "/s?bc=r2");
    expect(screen.getByRole("link", { name: "Hỗ trợ" })).toHaveAttribute("href", "/dashboards/d2");
  });
  it("?bang= keeps only that table's reports", () => {
    render(<ReportList {...base} entity="crm_contracts" />);
    expect(screen.queryByRole("link", { name: "Pipeline" })).toBeNull();
    expect(screen.getByRole("link", { name: "Hợp đồng hiệu lực" })).toBeInTheDocument();
  });
  it("?app= keeps reports of the app's tables and dashboards with a widget on them", () => {
    render(<ReportList {...base} entityNames={new Set(["crm_opportunities"])} />);
    expect(screen.queryByRole("link", { name: "Hợp đồng hiệu lực" })).toBeNull();
    expect(screen.getByRole("link", { name: "Pipeline" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Tổng quan bán hàng" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Hỗ trợ" })).toBeNull();
  });
  it("loading, error and empty are said out loud", () => {
    const { rerender } = render(<ReportList {...base} status="loading" />);
    expect(screen.getByRole("status")).toBeInTheDocument();
    rerender(<ReportList {...base} status="error" />);
    expect(screen.getByText("Không tải được danh sách báo cáo.")).toBeInTheDocument();
    rerender(<ReportList {...base} reports={[]} dashboards={[]} />);
    expect(within(screen.getByRole("navigation")).getByText("Chưa có báo cáo nào")).toBeInTheDocument();
  });
  // specs/074 lát f2-draft-labels-reports-brand (T355): a delivered project's draft overlay says what the customer will see.
  it("labels reports and dashboards from draftState; live and undelivered rows carry no label", () => {
    const rs: ReportSummary[] = [
      { ...R[0], draftState: "new" }, { ...R[1], draftState: "edited" }, { id: "r3", name: "Cũ", entityName: "crm_contracts", draftState: "live" },
    ];
    const ds: DashboardSummary[] = [{ ...D[0], draftState: "deleted" }, D[1]];
    render(<ReportList {...base} reports={rs} dashboards={ds} />);
    const row = (name: string) => screen.getByRole("link", { name: new RegExp(name) });
    expect(row("Hợp đồng hiệu lực")).toHaveTextContent("Mới, khách thấy khi xuất bản");
    expect(row("Pipeline")).toHaveTextContent("Nháp");
    expect(row("Cũ")).not.toHaveTextContent(/Nháp|Mới|Sẽ xoá/);
    expect(row("Tổng quan bán hàng")).toHaveTextContent("Sẽ xoá khi xuất bản");
    expect(row("Hỗ trợ")).not.toHaveTextContent(/Nháp|Mới|Sẽ xoá/);
  });
  it("an undelivered project (no draftState at all) shows no label", () => {
    render(<ReportList {...base} />);
    expect(screen.queryByText(/Nháp|Mới, khách/)).toBeNull();
  });
});

// specs/074 lát f2-ai-preview-menu-access-report-brand (T409): ghost rows for an assistant report change.
describe("ReportList — assistant preview (docType report)", () => {
  const renderWith = (c: ReturnType<typeof mkChange> | null) => render(<WithPreview change={c}><ReportList {...base} /></WithPreview>);

  it("create: a ghost row 'Đề xuất' in the reports group, not a link", async () => {
    renderWith(mkChange("report", "create", { target: { name: "doanh_thu" }, label: "Báo cáo doanh thu" }));
    const row = (await screen.findByText("Báo cáo doanh thu")).closest("li")!;
    expect(row.className).toContain("nx-proposed");
    expect(row.textContent).toContain("Đề xuất");
    expect(row.querySelector("a")).toBeNull();
  });

  it("delete: the real row stays a link, tinted and marked 'Sẽ xoá'", async () => {
    renderWith(mkChange("report", "delete", { docKey: "r2", label: "Pipeline" }));
    const link = await screen.findByRole("link", { name: /Pipeline/ });
    expect(link.closest("li")!.className).toContain("nx-proposed");
    expect(link.textContent).toContain("Sẽ xoá");
    expect(screen.getByRole("link", { name: "Hợp đồng hiệu lực" }).closest("li")!.className).not.toContain("nx-proposed");
  });

  it("no active change: no ghost (regression)", () => {
    renderWith(null);
    expect(document.querySelectorAll(".nx-proposed")).toHaveLength(0);
  });
});
