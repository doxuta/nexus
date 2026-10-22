// ReportCreateForm (specs/074 lát f2-report-list-view, T249): created in place, no dialog, 422 stays on the form.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportCreateForm } from "./ReportCreateForm";
import { ApiError, createReport, listFields } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), createReport: vi.fn(), listFields: vi.fn() }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: [{ name: "crm_appointments", label: "Lịch hẹn" }] }) }));

const wrap = (ui: React.ReactNode) => render(<QueryClientProvider client={new QueryClient()}>{ui}</QueryClientProvider>);

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listFields).mockResolvedValue([{ name: "starts_at", label: "Bắt đầu" }] as never);
});

describe("ReportCreateForm", () => {
  it("creates the report and hands it to onCreated without opening a dialog", async () => {
    vi.mocked(createReport).mockResolvedValue({ id: "r9", name: "Lịch hẹn theo tháng", entityName: "crm_appointments" } as never);
    const onCreated = vi.fn();
    wrap(<ReportCreateForm onCreated={onCreated} onCancel={() => {}} />);
    await userEvent.type(screen.getByLabelText("Tên báo cáo"), "Lịch hẹn theo tháng");
    await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(expect.objectContaining({ id: "r9" })));
    expect(vi.mocked(createReport).mock.calls[0][0]).toMatchObject({ name: "Lịch hẹn theo tháng", entityName: "crm_appointments", type: "tabular" });
    expect(document.querySelector("[role=dialog]")).toBeNull();
  });
  it("a 422 shows the sentence and keeps the form", async () => {
    vi.mocked(createReport).mockRejectedValue(new ApiError(422, "REPORT_CONFIG_INVALID", "Cấu hình sai", {}));
    const onCreated = vi.fn();
    wrap(<ReportCreateForm onCreated={onCreated} onCancel={() => {}} />);
    await userEvent.type(screen.getByLabelText("Tên báo cáo"), "A");
    await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
    expect(await screen.findByText("Cấu hình sai")).toBeInTheDocument();
    expect(onCreated).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Tên báo cáo")).toBeInTheDocument();
  });
  // #53: on a delivered project a new report is a report draft, so the header's pending count must recount. The bus hears
  // it only once the POST has answered (a recount sent earlier reads the old count), and never on a refusal.
  it("announces a draft on the drafts bus once the create has answered, never before, never on a refusal", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      let answer!: () => void;
      vi.mocked(createReport).mockReturnValueOnce(new Promise((r) => { answer = () => r({ id: "r9", name: "A", entityName: "crm_appointments" } as never); }));
      const onCreated = vi.fn();
      wrap(<ReportCreateForm onCreated={onCreated} onCancel={() => {}} />);
      await userEvent.type(screen.getByLabelText("Tên báo cáo"), "A");
      await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
      await waitFor(() => expect(createReport).toHaveBeenCalledTimes(1));
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual([]);
      answer();
      await waitFor(() => expect(onCreated).toHaveBeenCalled());
      expect(heard).toEqual(["report"]);
      vi.mocked(createReport).mockRejectedValueOnce(new ApiError(422, "REPORT_CONFIG_INVALID", "Cấu hình sai", {}));
      await userEvent.click(screen.getByRole("button", { name: "Tạo báo cáo" }));
      expect(await screen.findByText("Cấu hình sai")).toBeInTheDocument();
      expect(heard).toEqual(["report"]);
    } finally {
      off();
    }
  });
});
