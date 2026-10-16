import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(public status: number, public code: string, message: string, public details?: unknown) {
      super(message);
    }
  }
  return { ApiError, listDraftChanges: vi.fn(), publishDrafts: vi.fn(), discardDraft: vi.fn(), discardAllDrafts: vi.fn() };
});
vi.mock("@/lib/api", () => api);
const refetchState = vi.fn();
vi.mock("./usePublishState", () => ({
  usePublishState: () => ({ data: { delivered: true, draftableTypes: ["workflow"], directTypes: [], propagationSeconds: 60 }, refetch: refetchState }),
}));
vi.mock("@/lib/studioContext", () => ({ activeProjectSlug: () => "acme" }));
const toastSuccess = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: vi.fn() } }));

import { PublishReview } from "./PublishReview";

const change = (id: string, over = {}) => ({
  id, docType: "workflow", docKey: id, label: `Luật ${id}`, op: "update", entityName: "", updatedAt: "", updatedBy: "", conflict: false, ...over,
});
const wrapper = (ui: React.ReactElement) => (
  <QueryClientProvider client={new QueryClient()}>{ui}</QueryClientProvider>
);
const open = (onClose = vi.fn()) =>
  render(wrapper(<PublishReview anchor={<button type="button">Xuất bản</button>} onClose={onClose} />));

beforeEach(() => {
  for (const f of [api.listDraftChanges, api.publishDrafts, api.discardDraft, api.discardAllDrafts, refetchState, toastSuccess]) f.mockReset();
  api.listDraftChanges.mockResolvedValue({ changes: [change("a"), change("b")], changesetHash: "h1" });
});

describe("PublishReview", () => {
  it("shows the first 5 changed paths and +N for the rest", async () => {
    api.listDraftChanges.mockResolvedValue({
      changes: [change("a", { changedPaths: ["p1", "p2", "p3", "p4", "p5", "p6", "p7"], moreChanged: 3 })], changesetHash: "h1",
    });
    open();
    expect(await screen.findByText(/Đổi: p1, p2, p3, p4, p5/)).toBeTruthy();
    expect(screen.getByText(/\+5 chỗ khác/)).toBeTruthy();
    expect(screen.queryByText(/p6/)).toBeNull();
  });
  it("lists the pending changes and publishes with the fingerprint, then toasts the version", async () => {
    api.publishDrafts.mockResolvedValue({ versionNo: 3, kind: "publish", docCount: 2, publishedAt: "", propagationSeconds: 60 });
    const onClose = vi.fn();
    open(onClose);
    expect(await screen.findByText("Luật a")).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/Ghi chú/), { target: { value: "bản tháng 10" } });
    fireEvent.click(screen.getByTestId("publish-submit"));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(api.publishDrafts).toHaveBeenCalledWith("h1", { note: "bản tháng 10", force: false });
    expect(toastSuccess).toHaveBeenCalledWith("Đã xuất bản phiên bản 3 · máy chủ khác cập nhật trong tối đa 60 giây");
  });

  it("409 DRAFTS_CHANGED reloads the list and tells the builder", async () => {
    api.publishDrafts.mockRejectedValueOnce(new api.ApiError(409, "DRAFTS_CHANGED", "x"));
    api.listDraftChanges.mockResolvedValueOnce({ changes: [change("a"), change("b")], changesetHash: "h1" });
    api.listDraftChanges.mockResolvedValueOnce({ changes: [change("a")], changesetHash: "h2" });
    open();
    await screen.findByText("Luật a");
    fireEvent.click(await screen.findByTestId("publish-submit"));
    expect(await screen.findByText(/vừa đổi ở nơi khác/)).toBeTruthy();
    expect(api.listDraftChanges).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Luật b")).toBeNull();
  });

  it("409 NOTHING_TO_PUBLISH refreshes the state without a red error", async () => {
    api.publishDrafts.mockRejectedValueOnce(new api.ApiError(409, "NOTHING_TO_PUBLISH", "x"));
    open();
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByTestId("publish-submit"));
    await waitFor(() => expect(refetchState).toHaveBeenCalled());
    expect(screen.getByText(/Không còn gì để xuất bản/)).toBeTruthy();
    expect(document.querySelector(".slds-theme_error")).toBeNull();
  });

  it("PUBLISH_CONFLICT: force only after an inline confirm naming the documents; 'Xem lại' has focus first; Esc cancels", async () => {
    api.publishDrafts.mockRejectedValueOnce(new api.ApiError(409, "PUBLISH_CONFLICT", "x", { conflicts: [{ docType: "workflow", docKey: "a", label: "Luật a" }] }));
    api.listDraftChanges.mockResolvedValueOnce({ changes: [change("a"), change("b")], changesetHash: "h1" });
    api.listDraftChanges.mockResolvedValue({ changes: [change("a", { conflict: true }), change("b")], changesetHash: "h1" });
    open();
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByTestId("publish-submit"));
    fireEvent.click(await screen.findByRole("button", { name: "Ghi đè" }));
    expect(api.publishDrafts).toHaveBeenCalledTimes(1);
    expect(api.publishDrafts.mock.calls[0][1]?.force).toBeFalsy();
    const review = screen.getByRole("button", { name: "Xem lại" });
    expect(document.activeElement).toBe(review);
    expect(screen.getAllByText(/Luật a/).length).toBeGreaterThan(0);
    fireEvent.keyDown(review, { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Xem lại" })).toBeNull();
    expect(api.publishDrafts).toHaveBeenCalledTimes(1);

    api.publishDrafts.mockResolvedValueOnce({ versionNo: 4, kind: "publish", docCount: 2, publishedAt: "", propagationSeconds: 60 });
    fireEvent.click(screen.getByRole("button", { name: "Ghi đè" }));
    fireEvent.click(await screen.findByRole("button", { name: "Ghi đè và xuất bản" }));
    await waitFor(() => expect(api.publishDrafts).toHaveBeenCalledTimes(2));
    expect(api.publishDrafts.mock.calls[1][1]).toMatchObject({ force: true });
  });

  it("PUBLISH_BUSY retries once, then gives up with a message", async () => {
    api.publishDrafts.mockRejectedValue(new api.ApiError(409, "PUBLISH_BUSY", "x"));
    open();
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByTestId("publish-submit"));
    expect(await screen.findByText(/đang bận xuất bản/)).toBeTruthy();
    expect(api.publishDrafts).toHaveBeenCalledTimes(2);
  });

  it("422 PUBLISH_INVALID prints the errors per line", async () => {
    api.publishDrafts.mockRejectedValueOnce(new api.ApiError(422, "PUBLISH_INVALID", "x", { errors: [{ message: "Thiếu trường A" }, { message: "Sai tên B" }] }));
    open();
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByTestId("publish-submit"));
    expect(await screen.findByText(/Thiếu trường A/)).toBeTruthy();
    expect(screen.getByText(/Sai tên B/)).toBeTruthy();
  });

  it("discards one line; 404 DRAFT_NOT_FOUND counts as already discarded and refreshes", async () => {
    api.discardDraft.mockRejectedValueOnce(new api.ApiError(404, "DRAFT_NOT_FOUND", "x"));
    api.listDraftChanges.mockResolvedValueOnce({ changes: [change("a"), change("b")], changesetHash: "h1" });
    api.listDraftChanges.mockResolvedValueOnce({ changes: [change("b")], changesetHash: "h3" });
    open();
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByRole("button", { name: "Bỏ thay đổi Luật a" }));
    await waitFor(() => expect(screen.queryByText("Luật a")).toBeNull());
    expect(api.discardDraft).toHaveBeenCalledWith("a");
    expect(document.querySelector(".slds-theme_error")).toBeNull();
  });

  it("'Bỏ toàn bộ nháp' goes through an InlineConfirm and sends the fingerprint", async () => {
    api.discardAllDrafts.mockResolvedValue(undefined);
    const onClose = vi.fn();
    open(onClose);
    await screen.findByText("Luật a");
    fireEvent.click(screen.getByRole("button", { name: "Bỏ toàn bộ nháp" }));
    expect(api.discardAllDrafts).not.toHaveBeenCalled();
    fireEvent.click(await screen.findByRole("button", { name: "Bỏ toàn bộ" }));
    await waitFor(() => expect(api.discardAllDrafts).toHaveBeenCalledWith("h1"));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("a table / field born live and hidden (discardable:false) has no 'Bỏ' button and still publishes", async () => {
    api.listDraftChanges.mockResolvedValue({
      changes: [change("n", { docType: "field", op: "create", label: "Ghi chú · clinic_patients", discardable: false })], changesetHash: "h9",
    });
    api.publishDrafts.mockResolvedValue({ versionNo: 2, kind: "publish", docCount: 1, publishedAt: "", propagationSeconds: 60 });
    const onClose = vi.fn();
    open(onClose);
    expect(await screen.findByText(/Ghi chú · clinic_patients/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Bỏ thay đổi/ })).toBeNull();
    expect((screen.getByRole("button", { name: "Bỏ toàn bộ nháp" }) as HTMLButtonElement).disabled).toBe(true);
    const submit = screen.getByTestId("publish-submit") as HTMLButtonElement;
    expect(submit.disabled).toBe(false);
    fireEvent.click(submit);
    await waitFor(() => expect(api.publishDrafts).toHaveBeenCalledWith("h9", { note: undefined, force: false }));
  });

  it("under 768px it is a Modal (role=dialog with aria-modal)", async () => {
    window.matchMedia = ((q: string) => ({ matches: q.includes("max-width"), media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia;
    open();
    await screen.findByText("Luật a");
    expect(document.querySelector(".slds-modal")).not.toBeNull();
  });
});
