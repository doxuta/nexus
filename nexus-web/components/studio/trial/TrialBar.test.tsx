import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const openDraftPreview = vi.fn();
const flag = { on: false };
const inWs = vi.fn(() => false);
vi.mock("@/lib/studioViewAs", () => ({ openDraftPreview: (...a: unknown[]) => openDraftPreview(...a), get previewInWorkspace() { return flag.on; } }));
const state = vi.fn();
vi.mock("../usePublishState", () => ({ usePublishState: () => state() }));
vi.mock("@/lib/workspace", () => ({ inWorkspace: () => inWs() }));

import { TrialBar } from "./TrialBar";

const APP = { id: "a1", name: "crm" };
const q = (delivered: boolean, pendingCount: number) => ({ data: { delivered, pendingCount }, isError: false });
const mount = (props: Partial<React.ComponentProps<typeof TrialBar>> = {}) =>
  render(<TrialBar slug="northwind" app={APP} draft={true} onDraftChange={() => {}} {...props} />);

beforeEach(() => {
  vi.clearAllMocks();
  state.mockReturnValue(q(true, 2));
  flag.on = false;
  inWs.mockReturnValue(false);
});

describe("TrialBar", () => {
  it.each([[false, 2], [true, 0]])("hides both controls when delivered=%s pending=%s", (d, n) => {
    state.mockReturnValue(q(d, n));
    mount();
    expect(screen.queryByRole("radio")).toBeNull();
    expect(screen.queryByRole("button", { name: /Xem thử bản nháp/ })).toBeNull();
  });

  it("delivered with a draft: toggle + button, two different explanations", () => {
    mount();
    expect(screen.getAllByRole("radio")).toHaveLength(2);
    expect((screen.getByLabelText("Bản nháp") as HTMLInputElement).checked).toBe(true);
    expect(screen.getByRole("button", { name: "Xem thử bản nháp" })).toBeTruthy();
    expect(screen.getByTestId("trial-draft-explain-role")).toBeTruthy();
    expect(screen.getByTestId("trial-draft-explain-tab")).toBeTruthy();
    expect(screen.getByTestId("trial-draft-explain-role").textContent).not.toBe(screen.getByTestId("trial-draft-explain-tab").textContent);
  });

  it("choosing Bản đang chạy reports draft=false", () => {
    const f = vi.fn();
    mount({ onDraftChange: f });
    fireEvent.click(screen.getByLabelText("Bản đang chạy"));
    expect(f).toHaveBeenCalledWith(false);
  });

  it("Xem thử bản nháp opens the draft tab path and nothing else", () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Xem thử bản nháp" }));
    expect(openDraftPreview).toHaveBeenCalledWith({ appSlug: "crm" });
  });

  it("failed flag write prints the one-line problem", () => {
    openDraftPreview.mockImplementation(() => {
      throw new Error("x");
    });
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Xem thử bản nháp" }));
    expect(screen.getByRole("alert").textContent).toBe("Chưa mở được bản nháp.");
  });

  it("inside a customer workspace: draft button shows only with the previewInWorkspace flag on", () => {
    inWs.mockReturnValue(true);
    flag.on = true;
    const { unmount } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Xem thử bản nháp" }));
    expect(openDraftPreview).toHaveBeenCalledWith({ appSlug: "crm" });
    unmount();
    flag.on = false;
    mount();
    expect(screen.queryByRole("button", { name: /Xem thử bản nháp/ })).toBeNull();
  });
});
