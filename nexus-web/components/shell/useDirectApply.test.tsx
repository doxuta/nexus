import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  getPublishState: vi.fn(),
  ApiError: class ApiError extends Error {
    constructor(public status: number) {
      super("x");
    }
  },
}));
import { getPublishState } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { useDirectApply } from "./useDirectApply";

const run = vi.fn();

function Probe() {
  const { guard, dialog } = useDirectApply();
  return (
    <>
      <button onClick={() => void guard("Lưu ma trận", run)}>save</button>
      {dialog}
    </>
  );
}

beforeEach(() => {
  run.mockReset();
  vi.mocked(getPublishState).mockReset();
});

describe("useDirectApply (specs/063 S7-5)", () => {
  it("delivered project: asks first, runs only after 'Áp ngay'", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    render(<Probe />);
    fireEvent.click(screen.getByText("save"));
    expect(await screen.findByText("Thay đổi này ÁP NGAY cho người dùng thật")).toBeTruthy();
    expect(run).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText("Áp ngay"));
    await waitFor(() => expect(run).toHaveBeenCalledTimes(1));
  });

  it("delivered project: Huỷ does not run it", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    render(<Probe />);
    fireEvent.click(screen.getByText("save"));
    fireEvent.click(await screen.findByText("Huỷ"));
    expect(run).not.toHaveBeenCalled();
  });

  it("not delivered: runs at once, no Modal", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: false } as never);
    render(<Probe />);
    fireEvent.click(screen.getByText("save"));
    await waitFor(() => expect(run).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("Áp ngay")).toBeNull();
  });

  it("403 on publish state (legacy customer admin): no Modal, runs at once", async () => {
    vi.mocked(getPublishState).mockRejectedValue(new Error("403"));
    render(<Probe />);
    fireEvent.click(screen.getByText("save"));
    await waitFor(() => expect(run).toHaveBeenCalledTimes(1));
    expect(screen.queryByText("Áp ngay")).toBeNull();
  });

  function InlineProbe() {
    const { guard, dialog } = useDirectApply({ inline: true });
    return (
      <>
        <button onClick={() => void guard("Đổi ô", run)}>cell</button>
        {dialog}
      </>
    );
  }

  it("inline: first publish in the same page (drafts bus) makes the next edit ask", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: false } as never);
    render(<InlineProbe />);
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    emitDraftsChanged();
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(2));
    await new Promise((r) => setTimeout(r, 0));
    fireEvent.click(screen.getByText("cell"));
    expect(await screen.findByText("Áp ngay")).toBeTruthy();
    expect(run).not.toHaveBeenCalled();
  });

  it("inline: a failed read on mount is not cached as 'not delivered'", async () => {
    vi.mocked(getPublishState).mockRejectedValueOnce(new Error("500"));
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as never);
    render(<InlineProbe />);
    await waitFor(() => expect(getPublishState).toHaveBeenCalledTimes(1));
    await new Promise((r) => setTimeout(r, 0));
    fireEvent.click(screen.getByText("cell"));
    expect(await screen.findByText("Áp ngay")).toBeTruthy();
    expect(run).not.toHaveBeenCalled();
  });
});
