// SampleGenerateForm — specs/077 T091 (f5-sample-generate): the in-place sample generator that replaces the old modal.
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const generateSampleData = vi.fn();
const getPublishState = vi.fn();
const listPackInstalls = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  generateSampleData: (...a: unknown[]) => generateSampleData(...a),
  getPublishState: () => getPublishState(),
  listPackInstalls: () => listPackInstalls(),
}));
// The identity gate is covered by useProjectQuery's own tests; here the query just runs.
vi.mock("../useProjectQuery", () => ({
  useProjectQuery: (slug: string, key: unknown[], fn: () => Promise<unknown>) => useQuery({ queryKey: ["project", slug, ...key], queryFn: fn }),
}));

import { ApiError } from "@/lib/api";
import { skipReasonKey } from "@/lib/sampleLabels";
import { SampleGenerateForm } from "./SampleGenerateForm";

const NAMES = ["customers", "appointments", "docs"];
const LABELS = new Map([["customers", "Khách hàng"], ["appointments", "Lịch hẹn"], ["docs", "Hồ sơ"]]);
const OK = { batchId: "b", created: [{ entity: "customers", count: 5 }], failed: [], skipped: [], addedParents: [] };

function ui(initial?: string[]) {
  const onClose = vi.fn();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  render(
    <QueryClientProvider client={qc}>
      <SampleGenerateForm slug="crm" entityNames={NAMES} labels={LABELS} initial={initial} onClose={onClose} />
    </QueryClientProvider>,
  );
  return { user: userEvent.setup(), onClose, invalidate };
}

const ticks = () => screen.getAllByRole("checkbox").map((b) => (b as HTMLInputElement).checked);

beforeEach(() => {
  vi.clearAllMocks();
  getPublishState.mockResolvedValue({ delivered: false });
  listPackInstalls.mockResolvedValue([{ slug: "crm" }]);
  generateSampleData.mockResolvedValue(OK);
});

describe("SampleGenerateForm", () => {
  it("offers 'Từ gói' only when a pack is installed, and sends source=pack", async () => {
    const { user } = ui();
    await user.click(await screen.findByLabelText(/Từ gói ngành đã cài/));
    await user.click(screen.getByRole("button", { name: "Sinh" }));
    await waitFor(() => expect(generateSampleData).toHaveBeenCalledWith(["customers", "appointments", "docs"], 5, "pack"));
  });

  it.each([["empty", () => listPackInstalls.mockResolvedValue([])], ["error", () => listPackInstalls.mockRejectedValue(new Error("x"))]])(
    "no pack (%s): 'Từ gói' hidden and nothing is generated to probe",
    async (_n, arrange) => {
      arrange();
      ui();
      await waitFor(() => expect(listPackInstalls).toHaveBeenCalled());
      await new Promise((r) => setTimeout(r, 20));
      expect(screen.queryByLabelText(/Từ gói ngành đã cài/)).toBeNull();
      expect(screen.getByLabelText(/Theo trường/)).toBeInTheDocument();
      expect(generateSampleData).not.toHaveBeenCalled();
    },
  );

  it("opened for the whole section: every table ticked", () => {
    ui();
    expect(ticks()).toEqual([true, true, true]);
  });

  it("opened from one row ticks only that table", () => {
    ui(["docs"]);
    expect(ticks()).toEqual([false, false, true]);
  });

  it("'Bỏ chọn hết' / 'Chọn hết' toggle every table and the run button follows", async () => {
    const { user } = ui();
    await user.click(screen.getByRole("button", { name: "Bỏ chọn hết" }));
    expect(screen.getByRole("button", { name: "Sinh" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Chọn hết" }));
    expect(screen.getByRole("button", { name: "Sinh" })).toBeEnabled();
  });

  it("sends the ticked tables, the row count and the source; result is in a polite live region, never a dialog", async () => {
    const { user, invalidate } = ui();
    await user.click(screen.getByLabelText("Hồ sơ"));
    await user.selectOptions(screen.getByLabelText("Số dòng mỗi bảng"), "10");
    await user.click(screen.getByRole("button", { name: "Sinh" }));
    await waitFor(() => expect(generateSampleData).toHaveBeenCalledWith(["customers", "appointments"], 10, "auto"));
    const row = await screen.findByTestId("sample-gen-created-customers");
    expect(row).toHaveTextContent("Khách hàng");
    expect(row.closest("[aria-live='polite']")).not.toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["project", "crm", "studio", "sampleData"] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["project", "crm", "studio", "handoff"] });
  });

  it("a double click sends ONE request", async () => {
    generateSampleData.mockReturnValue(new Promise(() => {}));
    const { user } = ui();
    await user.dblClick(screen.getByRole("button", { name: "Sinh" }));
    expect(generateSampleData).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Đang sinh…" })).toBeDisabled();
  });

  it("Esc closes the form", async () => {
    const { user, onClose } = ui();
    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("reports parents, refused rows and skipped tables in Vietnamese", async () => {
    generateSampleData.mockResolvedValue({
      batchId: "b",
      created: [{ entity: "appointments", count: 5 }],
      failed: [{ entity: "customers", index: 1, code: "X", error: "Email đã tồn tại" }],
      skipped: [{ entity: "docs", code: "SAMPLE_FILE_REQUIRED" }],
      addedParents: ["customers"],
    });
    const { user } = ui();
    await user.click(screen.getByRole("button", { name: "Sinh" }));
    expect(await screen.findByTestId("sample-gen-parents")).toHaveTextContent("Đã sinh thêm bảng cha bắt buộc: Khách hàng");
    expect(screen.getByTestId("sample-gen-failed-customers")).toHaveTextContent("Email đã tồn tại");
    expect(screen.getByTestId("sample-gen-skipped-docs")).toHaveTextContent("cần tệp đính kèm bắt buộc");
  });

  it.each([
    [new ApiError(422, "SAMPLE_PER_ENTITY_INVALID", "Số dòng mỗi bảng phải từ 1 đến 20."), "Số dòng mỗi bảng phải từ 1 đến 20."],
    [new ApiError(409, "SAMPLE_BUSY", "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau."), "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau."],
  ])("shows the sentence for error %#", async (err, text) => {
    generateSampleData.mockRejectedValue(err);
    const { user } = ui();
    await user.click(screen.getByRole("button", { name: "Sinh" }));
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("a delivered project warns that samples show up in the customer's app and renames the button", async () => {
    getPublishState.mockResolvedValue({ delivered: true });
    ui();
    expect(await screen.findByText(/hiện ngay trong app khách đang dùng/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sinh vào app đang chạy" })).toBeInTheDocument();
  });

  it("unknown delivery state prints no warning", async () => {
    getPublishState.mockRejectedValue(new Error("x"));
    ui();
    await waitFor(() => expect(getPublishState).toHaveBeenCalled());
    expect(screen.queryByText(/hiện ngay trong app khách/)).toBeNull();
  });
});

describe("skipReasonKey", () => {
  it("maps the known codes and hides an unknown one behind the generic sentence", () => {
    for (const c of ["SAMPLE_FILE_REQUIRED", "SAMPLE_NO_USER", "SAMPLE_PARENT_EMPTY", "SAMPLE_ALREADY_PRESENT"]) expect(skipReasonKey(c)).toContain(c);
    expect(skipReasonKey("SAMPLE_WHATEVER")).toBe("studio.build.trial.generate.skip.other");
  });
});
