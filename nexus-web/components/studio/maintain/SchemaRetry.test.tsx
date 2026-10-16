// specs/079 f7-lich-su-hoan-tac-thu-lai T076: "Thử lại" a failed table/field delete: confirm first, POST retry-schema only after it.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), getVersion: vi.fn(), retrySchemaOps: vi.fn() }));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), useStudioIdentity: () => ({ matches: () => true }) }));
const toastSuccess = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: vi.fn() } }));

import { getVersion, retrySchemaOps } from "@/lib/api";
import { SchemaRetry } from "./SchemaRetry";

const detail = () => ({
  id: "v1", seq: 2, kind: "publish" as const, versionNo: 2, note: "", docCount: 1, createdAt: "2026-10-14T08:00:00Z", createdBy: "u", createdByName: "A", createdByLabel: "A", rollbackOf: null, schemaStatus: "failed",
  docs: [{ docType: "field", docKey: "f", label: "Ghi chú nội bộ", op: "delete" as const, changedPaths: [], moreChanged: 0, before: null, after: null, applyStatus: "failed" as const, applyError: "lock timeout" }],
});
function show(status: string, onDone = vi.fn()) {
  const view = render(<QueryClientProvider client={new QueryClient()}><SchemaRetry project="acme" versionId="v1" status={status} label="Thử lại" onDone={onDone} /></QueryClientProvider>);
  return { onDone, view };
}
beforeEach(() => {
  vi.mocked(getVersion).mockReset().mockResolvedValue(detail());
  vi.mocked(retrySchemaOps).mockReset();
  toastSuccess.mockReset();
});
async function openConfirm() {
  fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
  await screen.findByText(/Ghi chú nội bộ/);
}

describe("SchemaRetry", () => {
  it("failed: red 'Xoá chưa xong' + Thử lại; confirm names what and sends nothing yet", async () => {
    show("failed");
    expect(screen.getByText("Xoá chưa xong")).toBeTruthy();
    await openConfirm();
    expect(screen.getByText(/bị ẩn khỏi app của khách/)).toBeTruthy();
    expect(retrySchemaOps).not.toHaveBeenCalled();
    expect(document.activeElement?.textContent).toBe("Huỷ");
  });

  it("posts retry-schema only after the confirm, then refreshes", async () => {
    vi.mocked(retrySchemaOps).mockResolvedValue({ schemaStatus: "done", schemaOps: [] });
    const { onDone } = show("failed");
    await openConfirm();
    fireEvent.click(screen.getByRole("button", { name: "Xoá tiếp" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(retrySchemaOps).toHaveBeenCalledWith("v1");
    expect(toastSuccess).toHaveBeenCalledWith("Đã xoá xong.");
  });

  it("409 SCHEMA_RETRY_NOT_NEEDED refreshes the status without an error", async () => {
    vi.mocked(retrySchemaOps).mockRejectedValue(new ApiError(409, "SCHEMA_RETRY_NOT_NEEDED", "x"));
    const { onDone } = show("failed");
    await openConfirm();
    fireEvent.click(screen.getByRole("button", { name: "Xoá tiếp" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("still failing: stays open with a sentence", async () => {
    vi.mocked(retrySchemaOps).mockResolvedValue({ schemaStatus: "failed", schemaOps: [] });
    show("failed");
    await openConfirm();
    fireEvent.click(screen.getByRole("button", { name: "Xoá tiếp" }));
    expect(await screen.findByText("Vẫn còn thao tác xoá chưa xong.")).toBeTruthy();
  });

  it("pending: 'Đang xoá…' and no button; done: nothing", () => {
    const { view } = show("pending");
    expect(screen.getByText("Đang xoá…")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
    view.unmount();
    const done = show("done");
    expect(done.view.container.textContent).toBe("");
  });
});
