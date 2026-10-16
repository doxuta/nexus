// specs/073 nhap1b: what is left of the draft bar — only the one-way first publish, for a project not yet delivered.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(public status: number, public code: string, message: string) { super(message); }
  }
  return { ApiError, publishBaseline: vi.fn() };
});
vi.mock("@/lib/api", () => api);
let state: { delivered: boolean } | undefined;
vi.mock("./usePublishState", () => ({ usePublishState: () => ({ data: state }) }));
const toastSuccess = vi.fn();
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: (m: string) => toastSuccess(m), error: vi.fn() } }));
const drafts = vi.fn();
vi.mock("@/lib/draftEvents", () => ({ emitDraftsChanged: () => drafts() }));

import { DraftBar } from "./DraftBar";

const ui = () => render(<QueryClientProvider client={new QueryClient()}><DraftBar slug="acme" /></QueryClientProvider>);
beforeEach(() => { api.publishBaseline.mockReset(); toastSuccess.mockReset(); drafts.mockReset(); state = { delivered: false }; });

describe("DraftBar — first publish only", () => {
  it("not delivered: a first publish that spells out it is one-way, then recounts", async () => {
    api.publishBaseline.mockResolvedValue({ versionNo: 1, kind: "baseline", docCount: 7, publishedAt: "x", propagationSeconds: 60 });
    const user = userEvent.setup();
    ui();
    await user.click(screen.getByRole("button", { name: "Xuất bản lần đầu" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Không thể quay về chế độ ghi thẳng/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Xuất bản lần đầu" }));
    await waitFor(() => expect(api.publishBaseline).toHaveBeenCalledTimes(1));
    expect(toastSuccess).toHaveBeenCalledWith("Phiên bản 1 · đã chụp 7 tài liệu");
    await waitFor(() => expect(drafts).toHaveBeenCalled());
  });

  it("an error on the first publish stays in the modal", async () => {
    api.publishBaseline.mockRejectedValue(new api.ApiError(500, "X", "Hỏng rồi"));
    const user = userEvent.setup();
    ui();
    await user.click(screen.getByRole("button", { name: "Xuất bản lần đầu" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Xuất bản lần đầu" }));
    expect(await within(dialog).findByText("Hỏng rồi")).toBeInTheDocument();
  });

  it("delivered, or no state (403 / backend off): nothing at all", () => {
    state = { delivered: true };
    const a = ui();
    expect(a.container).toBeEmptyDOMElement();
    a.unmount();
    state = undefined;
    expect(ui().container).toBeEmptyDOMElement();
  });
});
