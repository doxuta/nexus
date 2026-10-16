// PreviewBanner — specs/064 T043: the strip over the customer app while a tab previews a role.
// Real lib/api + lib/previewSession (only fetch and location are faked): the point is that the exit
// path really clears the tab's preview and navigates, including when the API is down.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PreviewBanner } from "./PreviewBanner";
import { getPreview, setPreview, endPreviewExit, takePreviewNotice } from "@/lib/previewSession";
import { DRAFT_PREVIEW_KEY, inDraftPreviewTab } from "@/lib/previewMode";
import { PREVIEW_READ_ONLY_EVENT } from "./PreviewBanner";
import { toast } from "@/components/ui/Toaster";

vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));
// The previewed role's grants (the hint reads them); the real hook needs a QueryClient + AuthProvider.
const perms = vi.hoisted(() => ({ grants: [] as { entityName: string; action: string; accessLevel: string }[], isLoading: false }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ grants: perms.grants, isLoading: perms.isLoading }) }));

const base = {
  token: "PREVIEW-TOK",
  sessionId: "s-1",
  role: { id: "r-1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" },
  returnTo: "/studio/apps/crm",
};

describe("PreviewBanner", () => {
  const originalFetch = global.fetch;
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    endPreviewExit();
    assign = vi.fn();
    Object.defineProperty(window, "location", { configurable: true, value: { ...originalLocation, assign } });
  });
  afterEach(() => {
    global.fetch = originalFetch;
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    vi.useRealTimers();
  });

  it("renders nothing when the tab is not previewing", () => {
    const { container } = render(<PreviewBanner />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names the role and the minutes left, as a status region with a labelled Thoát button", () => {
    setPreview({ ...base, expiresAt: new Date(Date.now() + 25 * 60_000 + 20_000).toISOString() });
    render(<PreviewBanner />);
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Đang xem như Nhân viên kinh doanh");
    expect(status).toHaveTextContent("chỉ xem");
    expect(status).toHaveTextContent("còn 25 phút");
    expect(screen.getByRole("button", { name: "Thoát" })).toBeInTheDocument();
  });

  // specs/064 S9-9 (T132): the session that reads the draft layer says so, on the strip itself.
  it("a draft session carries a 'Bản nháp' badge; a live one does not", () => {
    const exp = new Date(Date.now() + 25 * 60_000).toISOString();
    setPreview({ ...base, expiresAt: exp, draft: true });
    const { unmount } = render(<PreviewBanner />);
    expect(screen.getByRole("status")).toHaveTextContent("Bản nháp");
    unmount();
    window.sessionStorage.clear();
    setPreview({ ...base, expiresAt: exp, draft: false });
    render(<PreviewBanner />);
    expect(screen.getByRole("status")).not.toHaveTextContent("Bản nháp");
  });

  it("under a minute left reads 'còn dưới 1 phút'", () => {
    setPreview({ ...base, expiresAt: new Date(Date.now() + 30_000).toISOString() });
    render(<PreviewBanner />);
    expect(screen.getByRole("status")).toHaveTextContent("còn dưới 1 phút");
  });

  // FR-017: a role that sees only "của tôi"/"đội" rows previews as an EMPTY list (the synthetic subject owns
  // nothing) - the strip says why, so the builder does not read it as a bug.
  describe("owner/team hint", () => {
    const view = (accessLevel: string, entityName = "crm_opportunities") => ({ entityName, action: "view", accessLevel });
    beforeEach(() => {
      setPreview({ ...base, expiresAt: new Date(Date.now() + 600_000).toISOString() });
    });
    afterEach(() => {
      perms.grants = [];
      perms.isLoading = false;
    });

    it.each(["owner", "team"])("a view grant at '%s' adds 'Một số mục chỉ hiện bản ghi mình phụ trách'", (level) => {
      perms.grants = [view("all", "crm_accounts"), view(level)];
      render(<PreviewBanner />);
      expect(screen.getByRole("status")).toHaveTextContent("Một số mục chỉ hiện bản ghi mình phụ trách");
    });

    it("an owner-level '*' (any action) grant also adds the hint", () => {
      perms.grants = [{ entityName: "crm_opportunities", action: "*", accessLevel: "owner" }];
      render(<PreviewBanner />);
      expect(screen.getByRole("status")).toHaveTextContent("Một số mục chỉ hiện bản ghi mình phụ trách");
    });

    it("no hint while the grants are still loading", () => {
      perms.grants = [view("owner")];
      perms.isLoading = true;
      render(<PreviewBanner />);
      expect(screen.getByRole("status")).not.toHaveTextContent("chỉ hiện bản ghi mình phụ trách");
    });

    it("no hint when every view grant is 'all' (or the owner level is on an action other than view)", () => {
      perms.grants = [view("all"), { entityName: "crm_accounts", action: "edit", accessLevel: "owner" }];
      render(<PreviewBanner />);
      expect(screen.getByRole("status")).not.toHaveTextContent("chỉ hiện bản ghi mình phụ trách");
    });
  });

  it("a refused write (nexus:preview-read-only) becomes one Toast, only while previewing", () => {
    vi.mocked(toast.info).mockClear();
    window.dispatchEvent(new CustomEvent("nexus:preview-read-only"));
    expect(toast.info).not.toHaveBeenCalled();
    setPreview({ ...base, expiresAt: new Date(Date.now() + 600_000).toISOString() });
    render(<PreviewBanner />);
    window.dispatchEvent(new CustomEvent("nexus:preview-read-only"));
    expect(toast.info).toHaveBeenCalledTimes(1);
    expect(toast.info).toHaveBeenCalledWith("Đang xem thử — không ghi được");
  });

  it("Thoát: DELETE /preview-sessions/current?reason=exit with the preview token, clears the preview, goes back to Studio", async () => {
    setPreview({ ...base, expiresAt: new Date(Date.now() + 600_000).toISOString() });
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<PreviewBanner />);
    fireEvent.click(screen.getByRole("button", { name: "Thoát" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio/apps/crm"));
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toContain("/preview-sessions/current?reason=exit");
    expect(init.method).toBe("DELETE");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer PREVIEW-TOK");
    expect(getPreview()).toBeNull();
  });

  it("Thoát still cleans up and leaves when the API call fails", async () => {
    setPreview({ ...base, expiresAt: new Date(Date.now() + 600_000).toISOString() });
    global.fetch = vi.fn(async () => {
      throw new Error("network down");
    }) as unknown as typeof fetch;
    render(<PreviewBanner />);
    fireEvent.click(screen.getByRole("button", { name: "Thoát" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio/apps/crm"));
    expect(getPreview()).toBeNull();
  });

  it("a preview that already expired leaves on its own and leaves the notice for the Studio", async () => {
    setPreview({ ...base, expiresAt: new Date(Date.now() - 1000).toISOString() });
    global.fetch = vi.fn() as unknown as typeof fetch;
    render(<PreviewBanner />);
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio/apps/crm"));
    expect(getPreview()).toBeNull();
    expect(takePreviewNotice()).toBe("ended");
    expect(global.fetch).not.toHaveBeenCalled();
  });
});


// specs/063 S7-3: the SAME strip carries the draft-preview variant.
describe("PreviewBanner — draft variant", () => {
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.history.pushState({}, "", "/");
    assign = vi.fn();
    Object.defineProperty(window, "location", { configurable: true, value: { ...originalLocation, assign, search: "", pathname: "/" } });
    vi.mocked(toast.info).mockClear();
  });
  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
  });

  function openDraftTab(ret = "/studio/apps/crm") {
    window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: ret }));
  }

  it("says BẢN NHÁP, that the customer has not seen it, and offers Thoát", () => {
    openDraftTab();
    window.localStorage.setItem("nexus_tenant", "phong-kham-a");
    render(<PreviewBanner />);
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Bạn đang xem BẢN NHÁP");
    expect(status).toHaveTextContent("khách chưa thấy");
    expect(status).toHaveTextContent("phong-kham-a");
    expect(screen.getByRole("button", { name: "Thoát" })).toBeInTheDocument();
  });

  it("Thoát clears the tab flag and goes back to where the tab was opened from", () => {
    openDraftTab("/studio/apps/crm");
    render(<PreviewBanner />);
    fireEvent.click(screen.getByRole("button", { name: "Thoát" }));
    expect(inDraftPreviewTab()).toBe(false);
    expect(assign).toHaveBeenCalledWith("/studio/apps/crm");
  });

  it("a refused write (403 PREVIEW_READ_ONLY) answers with one Toast", () => {
    openDraftTab();
    render(<PreviewBanner />);
    window.dispatchEvent(new CustomEvent(PREVIEW_READ_ONLY_EVENT));
    expect(toast.info).toHaveBeenCalledWith("Đang xem bản nháp — chỉ đọc");
  });

  it("a role preview wins over the draft flag; neither flag renders nothing", () => {
    const { container } = render(<PreviewBanner />);
    expect(container).toBeEmptyDOMElement();
  });
});
