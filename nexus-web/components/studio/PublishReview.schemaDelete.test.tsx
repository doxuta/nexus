import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

// specs/063-draft-publish S7-9: pending field / table deletes are listed apart ("Thao tác xoá") with the
// reference warnings, and a published version whose deletes did not finish offers "Thử lại".
const api = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(public status: number, public code: string, message: string, public details?: unknown) {
      super(message);
    }
  }
  return { ApiError, listDraftChanges: vi.fn(), publishDrafts: vi.fn(), discardDraft: vi.fn(), discardAllDrafts: vi.fn(), retrySchemaOps: vi.fn() };
});
vi.mock("@/lib/api", () => api);
const stateData = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const refetchState = vi.fn();
vi.mock("./usePublishState", () => ({ usePublishState: () => ({ data: stateData.current, refetch: refetchState }) }));
vi.mock("@/lib/studioContext", () => ({ activeProjectSlug: () => "acme" }));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: (m: string) => toastError(m) } }));

import { PublishReview } from "./PublishReview";

const change = (id: string, over = {}) => ({
  id, docType: "workflow", docKey: id, label: `Luật ${id}`, op: "update", entityName: "", updatedAt: "", updatedBy: "", conflict: false, ...over,
});
const open = (onClose = vi.fn()) =>
  render(
    <QueryClientProvider client={new QueryClient()}>
      <PublishReview anchor={<button type="button">Xuất bản</button>} onClose={onClose} />
    </QueryClientProvider>,
  );

beforeEach(() => {
  for (const f of [api.listDraftChanges, api.publishDrafts, api.retrySchemaOps, refetchState, toastSuccess, toastError]) f.mockReset();
  stateData.current = { delivered: true, draftableTypes: ["workflow", "field", "entity"], directTypes: [], propagationSeconds: 60, schemaRetry: null };
});

describe("PublishReview · deferred deletes", () => {
  it("lists a field delete under 'Thao tác xoá' with its reference warning, apart from the other changes", async () => {
    api.listDraftChanges.mockResolvedValue({
      changes: [
        change("a"),
        change("f1", { docType: "field", op: "delete", label: "Giai đoạn · deals", warnings: ['Báo cáo "Pipeline" dùng trường "stage"'] }),
      ],
      changesetHash: "h1",
    });
    open();
    expect(await screen.findByText("Luật a")).toBeTruthy();
    expect(screen.getByText("Thao tác xoá")).toBeTruthy();
    expect(screen.getByText(/Giai đoạn · deals/)).toBeTruthy();
    expect(screen.getByText(/Lưu ý: Báo cáo "Pipeline" dùng trường "stage"/)).toBeTruthy();
    // a warning never blocks the publish button
    expect((screen.getByTestId("publish-submit") as HTMLButtonElement).disabled).toBe(false);
  });

  it("the routing sentence does not promise a table EDIT goes through a draft (only its delete does)", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("a")], changesetHash: "h1" });
    open();
    await screen.findByText("Luật a");
    expect(screen.getByText(/Bảng \(chỉ thao tác xoá; sửa nhãn, mô tả áp ngay\)/)).toBeTruthy();
  });

  it("no 'Thao tác xoá' section when nothing is being deleted", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("a")], changesetHash: "h1" });
    open();
    await screen.findByText("Luật a");
    expect(screen.queryByText("Thao tác xoá")).toBeNull();
  });

  it("a publish whose deletes failed toasts the warning; the version still counts as published", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("t1", { docType: "entity", op: "delete", label: "Deals" })], changesetHash: "h1" });
    api.publishDrafts.mockResolvedValue({ versionNo: 4, kind: "publish", docCount: 1, publishedAt: "", propagationSeconds: 60, schemaStatus: "failed", schemaOps: [] });
    const onClose = vi.fn();
    open(onClose);
    await screen.findByText(/Deals/);
    fireEvent.click(screen.getByTestId("publish-submit"));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(toastSuccess).toHaveBeenCalled();
    expect(toastError).toHaveBeenCalledWith("Đã xuất bản phiên bản 4, nhưng còn thao tác xoá chưa xong. Mở Xuất bản để thử lại.");
  });

  it("'Thử lại' calls retry-schema with the version id and reports done", async () => {
    stateData.current = {
      ...stateData.current,
      schemaRetry: { versionId: "v-uuid", versionNo: 4, ops: [{ docType: "entity", docKey: "e1", label: "Deals", applyStatus: "failed", error: "boom" }] },
    };
    api.listDraftChanges.mockResolvedValue({ changes: [], changesetHash: "h1" });
    api.retrySchemaOps.mockResolvedValue({ schemaStatus: "done", schemaOps: [] });
    open();
    expect(await screen.findByText("Xoá chưa xong · phiên bản 4")).toBeTruthy();
    expect(screen.getByText(/Deals/)).toBeTruthy();
    expect(screen.getByText(/boom/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(api.retrySchemaOps).toHaveBeenCalledWith("v-uuid"));
    expect(await screen.findByText("Đã xoá xong.")).toBeTruthy();
    expect(toastSuccess).toHaveBeenCalledWith("Đã xoá xong.");
    expect(refetchState).toHaveBeenCalled();
  });

  it("409 SCHEMA_RETRY_NOT_NEEDED (another tab finished it) reads as done, not as an error", async () => {
    stateData.current = { ...stateData.current, schemaRetry: { versionId: "v-uuid", versionNo: 4, ops: [] } };
    api.listDraftChanges.mockResolvedValue({ changes: [], changesetHash: "h1" });
    api.retrySchemaOps.mockRejectedValue(new api.ApiError(409, "SCHEMA_RETRY_NOT_NEEDED", "x"));
    open();
    fireEvent.click(await screen.findByRole("button", { name: "Thử lại" }));
    expect(await screen.findByText("Đã xoá xong.")).toBeTruthy();
    expect(toastSuccess).not.toHaveBeenCalled();
  });

  it("no retry panel when the state has none", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("a")], changesetHash: "h1" });
    open();
    await screen.findByText("Luật a");
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
  });
});
