// RuleListPage (specs/074 lát f2-rule-list): `dung/tu-dong` — rules read as Khi/Nếu/Thì, filtered by app/table,
// '+ Luật mới' opens an in-place form (no dialog) that hands over to `luat/moi`, right column counts the four siblings.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary } from "@/lib/api";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/tu-dong",
  useRouter: () => ({ push, replace: vi.fn(), back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const W = (id: string, name: string, entityName: string, extra: Record<string, unknown> = {}) =>
  ({ id, name, entityName, trigger: "after_create", isActive: true, condition: null, actions: [{ type: "email_alert", to: "a@b.vn" }], ...extra });
const rules = [W("w1", "Báo phiếu mới", "tk_tickets"), W("w2", "Nhắc lead", "crm_leads", { isActive: false }), W("w3", "Chặn lưu", "tk_tickets", { trigger: "before_save" })];

const appsCatalog = vi.fn();
const appNav = vi.fn();
const listWorkflows = vi.fn();
const listRecentWorkflowRuns = vi.fn();
const setWorkflowActive = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listWorkflows: (...a: unknown[]) => listWorkflows(...a),
  getWorkflow: vi.fn(async (id: string) => rules.find((r) => r.id === id)),
  listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu" }, { name: "crm_leads", label: "Lead" }]),
  listFields: vi.fn(async () => []),
  listRecentWorkflowRuns: (...a: unknown[]) => listRecentWorkflowRuns(...a),
  setWorkflowActive: (...a: unknown[]) => setWorkflowActive(...a),
  listApprovalProcesses: vi.fn(async () => [{ id: "a" }, { id: "b" }]),
  listWorkQueuesAdmin: vi.fn(async () => [{ id: "q" }]),
  listInboundHooks: vi.fn(async () => []),
}));
vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  listBusinessCalendars: vi.fn(async () => [{ id: "c" }]),
}));

import { RuleListPage } from "./RuleListPage";

function renderPage(props: { app?: string; bang?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><RuleListPage slug="northwind" {...props} /></QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
  appNav.mockImplementation(async (a: string) => ({ app: app(a), items: [{ type: "entity", refId: "x", label: "x", href: a === "ticket" ? "/tk_tickets" : "/crm_leads" }] }));
  listWorkflows.mockResolvedValue(rules);
  listRecentWorkflowRuns.mockResolvedValue([]);
});

describe("RuleListPage", () => {
  it("lists every rule as Khi/Nếu/Thì when unfiltered", async () => {
    renderPage();
    expect(await screen.findByText("Báo phiếu mới")).toBeTruthy();
    expect(screen.getByText("Nhắc lead")).toBeTruthy();
    expect(screen.getAllByText("Khi").length).toBe(3);
    expect(screen.getByText("Chạy khi lưu")).toBeTruthy();
  });
  it("?app= keeps only that app's rules and says how many are hidden", async () => {
    renderPage({ app: "ticket" });
    expect(await screen.findByText("Báo phiếu mới")).toBeTruthy();
    expect(screen.queryByText("Nhắc lead")).toBeNull();
    expect(screen.getByText(/1 luật của app khác đang ẩn/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Xem tất cả" })).toBeTruthy();
  });
  it("?bang= narrows to one table", async () => {
    renderPage({ bang: "crm_leads" });
    expect(await screen.findByText("Nhắc lead")).toBeTruthy();
    expect(screen.queryByText("Báo phiếu mới")).toBeNull();
  });
  it("an app with no rule shows the empty state with 'Viết luật đầu tiên'", async () => {
    listWorkflows.mockResolvedValue([rules[1]]);
    renderPage({ app: "ticket" });
    expect(await screen.findByText("App TICKET chưa có luật")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Viết luật đầu tiên" })).toBeTruthy();
  });
  it("'+ Luật mới' opens the form in place (no dialog) and hands over to luat/moi", async () => {
    renderPage({ app: "ticket" });
    await screen.findByText("Báo phiếu mới");
    await userEvent.click(screen.getByRole("button", { name: "+ Luật mới" }));
    expect(document.querySelector("[role=dialog]")).toBeNull();
    await userEvent.type(screen.getByLabelText("Tên luật"), "Rule X");
    await userEvent.click(screen.getByRole("button", { name: "Viết luật" }));
    expect(push).toHaveBeenCalledTimes(1);
    const url = String(push.mock.calls[0][0]);
    expect(url).toContain("/dung/tu-dong/luat/moi");
    expect(url).toContain("ten=Rule%20X");
    expect(url).toContain("bang=tk_tickets");
    expect(url).toContain("app=ticket");
  });
  it("right column counts the four sibling pages", async () => {
    renderPage();
    const aside = await screen.findByRole("complementary");
    await waitFor(() => expect(within(aside).getByText("Quy trình duyệt (2)")).toBeTruthy());
    expect(within(aside).getByText("Hàng đợi việc (1)")).toBeTruthy();
    expect(within(aside).getByText("Cổng nhận dữ liệu (0)")).toBeTruthy();
    expect(within(aside).getByText("Lịch làm việc (1)")).toBeTruthy();
  });
  // T126: failed mark rests on the FIRST run of each rule in the page; ids go in groups of <= 100.
  it("marks only the rule whose newest run failed, as text", async () => {
    listRecentWorkflowRuns.mockResolvedValue([{ id: "r1", workflowId: "w1", status: "failed" }, { id: "r2", workflowId: "w2", status: "success" }, { id: "r0", workflowId: "w2", status: "failed" }]);
    renderPage();
    expect(await screen.findAllByRole("button", { name: "Lần chạy gần nhất lỗi" })).toHaveLength(1);
  });
  it("asks for runs in groups of at most 100 rule ids", async () => {
    listWorkflows.mockResolvedValue(Array.from({ length: 101 }, (_, i) => W(`x${i}`, `Luật ${i}`, "tk_tickets")));
    renderPage();
    await screen.findByText("Luật 0");
    await waitFor(() => expect(listRecentWorkflowRuns).toHaveBeenCalledTimes(2));
    expect(listRecentWorkflowRuns.mock.calls.map((c) => (c[1] as string[]).length).sort()).toEqual([1, 100]);
  });
  it("switch calls the PATCH and announces the result", async () => {
    setWorkflowActive.mockResolvedValue({ isActive: false });
    renderPage();
    await userEvent.click(await screen.findByRole("switch", { name: "Bật luật Nhắc lead" }).catch(() => screen.findByLabelText("Bật luật Nhắc lead")));
    await waitFor(() => expect(setWorkflowActive).toHaveBeenCalledWith("w2", true));
  });
});

// specs/074 lát f2-ai-preview-data-rules (T399, T463)
import { Off, WithGhost, mkChange } from "@/components/studio/assistant/ghostTestKit";

describe("RuleListPage: assistant ghost", () => {
  const renderGhost = (change: Parameters<typeof WithGhost>[0]["change"]) => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={qc}><WithGhost change={change}><Off /><RuleListPage slug="northwind" /></WithGhost></QueryClientProvider>);
  };
  it("create: a ghost rule row 'Đề xuất' without controls, above the real rules; Bỏ removes it", async () => {
    renderGhost(mkChange({ docType: "workflow", op: "create", label: "Nhắc lịch hẹn" }));
    const ghost = await screen.findByRole("group", { name: "Đề xuất của trợ lý: Nhắc lịch hẹn" });
    expect(ghost).toHaveTextContent("Đề xuất");
    expect(ghost.closest("li")?.querySelectorAll("a,button,input").length).toBe(0);
    expect(await screen.findByText("Báo phiếu mới")).toBeInTheDocument();
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent(/Có một đề xuất/);
    await userEvent.click(screen.getByRole("button", { name: "ghost-off" }));
    expect(screen.queryByRole("group")).toBeNull();
  });
  it("regression: no proposal, no ghost", async () => {
    renderGhost(undefined);
    await screen.findByText("Báo phiếu mới");
    expect(screen.queryByRole("group")).toBeNull();
  });
});
