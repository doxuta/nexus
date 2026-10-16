// specs/077 f5-sample-wipe T121/T122: in-page wipe confirmation (expectTotal snapshot) and the kept list.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SampleWipeConfirm } from "./SampleWipeConfirm";
import { ApiError, getSampleDataSummary, wipeSampleData, type SampleSummary } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, getSampleDataSummary: vi.fn(), wipeSampleData: vi.fn() };
});

const ent = (entity: string, label: string, live: number, inBin = 0) => ({
  entity,
  label,
  count: live + inBin,
  live,
  inBin,
  bySource: { generator: live + inBin, ai: 0, pack: 0 },
});
const summary = (rows: ReturnType<typeof ent>[]): SampleSummary => ({ total: rows.reduce((n, r) => n + r.count, 0), entities: rows });
const base = summary([ent("customers", "Khách hàng", 3, 1), ent("appointments", "Lịch hẹn", 2)]); // 6 rows, 1 in the bin
const wipeOk = { deleted: [{ entity: "customers", count: 4 }], kept: [], blobsRemoved: 0, sequencesReset: [] };

beforeEach(() => {
  vi.mocked(getSampleDataSummary).mockReset();
  vi.mocked(wipeSampleData).mockReset();
});

function setup(s: SampleSummary = base, scope?: string) {
  const onDone = vi.fn();
  const onCancel = vi.fn();
  const ui = (sum: SampleSummary) => (
    <SampleWipeConfirm summary={sum} entityNames={["customers", "appointments"]} scope={scope} onDone={onDone} onCancel={onCancel} />
  );
  const view = render(ui(s));
  return { ...view, onDone, onCancel, rerenderWith: (sum: SampleSummary) => view.rerender(ui(sum)) };
}

describe("SampleWipeConfirm", () => {
  it("names the total, the bin rows, the top tables, the irreversible warning; no dialog", () => {
    setup();
    expect(screen.getByText(/Xoá 6 bản ghi mẫu ở 2 bảng \(1 đang trong thùng rác\)\?.*không hoàn tác, gồm cả dòng mẫu đã bị sửa/)).toBeInTheDocument();
    expect(screen.getByTestId("sample-wipe-row-customers")).toHaveTextContent("Khách hàng: 4");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
  });

  it("lists at most 5 tables and says how many more", () => {
    const many = summary(["a", "b", "c", "d", "e", "f", "g"].map((n, i) => ent(n, n.toUpperCase(), 10 - i)));
    setup(many);
    expect(screen.getAllByTestId(/sample-wipe-row-/)).toHaveLength(5);
    expect(screen.getByText("và 2 bảng khác")).toBeInTheDocument();
  });

  it("sends the whole-app tables and the total it showed", async () => {
    vi.mocked(wipeSampleData).mockResolvedValue(wipeOk);
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 6 bản ghi" }));
    await waitFor(() => expect(onDone).toHaveBeenCalledWith(wipeOk));
    expect(vi.mocked(wipeSampleData)).toHaveBeenCalledWith(6, ["customers", "appointments"]);
  });

  it("a one-table wipe sends [table] and that table's own count", async () => {
    vi.mocked(wipeSampleData).mockResolvedValue(wipeOk);
    setup(base, "customers");
    expect(screen.getByText(/Xoá 4 bản ghi mẫu của bảng Khách hàng/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 4 bản ghi" }));
    await waitFor(() => expect(vi.mocked(wipeSampleData)).toHaveBeenCalledWith(4, ["customers"]));
  });

  it("a fresher read while the box is open does NOT change the number it confirms, only adds a line", async () => {
    vi.mocked(wipeSampleData).mockResolvedValue(wipeOk);
    const { rerenderWith } = setup();
    rerenderWith(summary([ent("customers", "Khách hàng", 9, 1), ent("appointments", "Lịch hẹn", 2)]));
    expect(screen.getByText(/Xoá 6 bản ghi mẫu ở 2 bảng/)).toBeInTheDocument();
    expect(screen.getByText(/Số dòng mẫu vừa thay đổi/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 6 bản ghi" }));
    await waitFor(() => expect(vi.mocked(wipeSampleData)).toHaveBeenCalledWith(6, expect.any(Array)));
  });

  it("409 SAMPLE_WIPE_STALE keeps the box open with the new numbers, focus back on Huỷ", async () => {
    vi.mocked(wipeSampleData).mockRejectedValueOnce(new ApiError(409, "SAMPLE_WIPE_STALE", "x", { current: 8 }));
    vi.mocked(getSampleDataSummary).mockResolvedValue(summary([ent("customers", "Khách hàng", 7, 1), ent("appointments", "Lịch hẹn", 2)]));
    const { onDone } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 6 bản ghi" }));
    expect(await screen.findByText("Số dòng mẫu đã thay đổi, xin xem lại rồi xoá.")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Xoá 10 bản ghi" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus());
    expect(onDone).not.toHaveBeenCalled();
    expect(vi.mocked(wipeSampleData)).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["SAMPLE_BUSY", "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau."],
    ["SAMPLE_WIPE_CONFIRM_REQUIRED", "Cần xác nhận số dòng trước khi xoá."],
  ])("%s has its own sentence", async (code, text) => {
    vi.mocked(wipeSampleData).mockRejectedValue(new ApiError(code === "SAMPLE_BUSY" ? 409 : 422, code, "x"));
    setup();
    await userEvent.click(screen.getByRole("button", { name: "Xoá 6 bản ghi" }));
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("a double click is one call (two clicks before React re-renders: only the in-flight guard stops the second)", async () => {
    let release: (v: typeof wipeOk) => void = () => {};
    vi.mocked(wipeSampleData).mockReturnValue(new Promise((r) => (release = r)));
    const { onDone } = setup();
    const go = screen.getByRole("button", { name: "Xoá 6 bản ghi" });
    act(() => {
      go.click();
      go.click();
    });
    expect(vi.mocked(wipeSampleData)).toHaveBeenCalledTimes(1);
    release(wipeOk);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
  });

  it("Esc cancels", async () => {
    const { onCancel } = setup();
    await userEvent.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalled();
  });
});
