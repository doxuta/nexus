// StudioPreviewGate — specs/064 T047: the Studio never runs inside a preview tab.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { StudioPreviewGate } from "./StudioPreviewGate";
import { getPreview, PREVIEW_STORAGE_KEY, setPreview, setPreviewNotice, endPreviewExit, isPreviewExiting } from "@/lib/previewSession";
import { toast } from "@/components/ui/Toaster";

vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

const preview = {
  token: "PREVIEW-TOK",
  sessionId: "s-1",
  role: { id: "r-1", name: "crm_sales_rep" },
  expiresAt: "2099-01-01T00:00:00Z",
  returnTo: "/studio/apps/crm",
};

describe("StudioPreviewGate", () => {
  const originalFetch = global.fetch;
  const originalLocation = window.location;
  let reload: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    endPreviewExit();
    vi.mocked(toast.info).mockClear();
    reload = vi.fn();
    Object.defineProperty(window, "location", { configurable: true, value: { ...originalLocation, reload } });
  });
  afterEach(() => {
    global.fetch = originalFetch;
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
  });

  it("no preview: shows the Studio", async () => {
    render(<StudioPreviewGate><p>studio</p></StudioPreviewGate>);
    expect(await screen.findByText("studio")).toBeInTheDocument();
    expect(reload).not.toHaveBeenCalled();
  });

  it("a preview in this tab: never renders the Studio, ends the session as 'left', clears it, reloads", async () => {
    setPreview(preview);
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<StudioPreviewGate><p>studio</p></StudioPreviewGate>);
    expect(screen.queryByText("studio")).toBeNull();
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(String((fetchMock.mock.calls[0] as unknown[])[0])).toContain("/preview-sessions/current?reason=left");
    expect(getPreview()).toBeNull();
    expect(isPreviewExiting()).toBe(true); // nothing on the old page may carry the builder's token while reloading
    expect(screen.queryByText("studio")).toBeNull();
  });

  it("leaves even when the end call fails", async () => {
    setPreview(preview);
    global.fetch = vi.fn(async () => {
      throw new Error("down");
    }) as unknown as typeof fetch;
    render(<StudioPreviewGate><p>studio</p></StudioPreviewGate>);
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(getPreview()).toBeNull();
  });

  it("a record without a token is no preview: no call, the record is wiped, the page reloads (never a silent spinner)", async () => {
    window.sessionStorage.setItem(PREVIEW_STORAGE_KEY, JSON.stringify({ role: { id: "r-1", name: "x" } }));
    const fetchMock = vi.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<StudioPreviewGate><p>studio</p></StudioPreviewGate>);
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(fetchMock).not.toHaveBeenCalled();
    expect(window.sessionStorage.getItem(PREVIEW_STORAGE_KEY)).toBeNull();
  });

  it("after a preview ended on its own, the Studio says so once", async () => {
    setPreviewNotice("ended");
    render(<StudioPreviewGate><p>studio</p></StudioPreviewGate>);
    await screen.findByText("studio");
    await waitFor(() => expect(toast.info).toHaveBeenCalledWith("Phiên xem thử đã kết thúc"));
    expect(toast.info).toHaveBeenCalledTimes(1);
  });
});
