// BrandingScreen contract (AO-THUONG-HIEU p1): loads the saved document into the form, saving
// pushes the full document and re-applies it live, reset sends {} — and a server 422 shows
// verbatim. lib/api mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrandingScreen } from "./BrandingScreen";
import { mkChange, WithPreview } from "@/test/previewProbe";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { toast } from "@/components/ui/Toaster";
import { ensureAuth, getTenantBrandingDoc, updateTenantBrandingDoc, ApiError } from "@/lib/api";

vi.mock("next/navigation", async (orig) => ({ ...(await orig<typeof import("next/navigation")>()), useRouter: () => ({ push: vi.fn() }), usePathname: () => "/studio/du-an/demo/dung/bao-cao" }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    getTenantBrandingDoc: vi.fn(),
    updateTenantBrandingDoc: vi.fn(),
  };
});

function renderScreen() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <BrandingScreen />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(getTenantBrandingDoc).mockReset().mockResolvedValue({ branding: { appName: "ABC Manager", accentColor: "#112233" } });
  vi.mocked(updateTenantBrandingDoc).mockReset().mockImplementation(async (b) => ({ branding: b }));
});

describe("BrandingScreen", () => {
  // The screen passes through a "Đang tải…" remount between first paint and query data, so
  // every test re-queries the input INSIDE waitFor (a captured reference goes stale/detached).
  const nameInput = () => screen.getByPlaceholderText("vd: ABC Manager") as HTMLInputElement;
  const loaded = () => waitFor(() => expect(nameInput().value).toBe("ABC Manager"));

  it("loads the saved document into the form and preview", async () => {
    renderScreen();
    await loaded();
    // The colour picker's summary input carries the saved accent (SLDS color-picker/base).
    expect(screen.getByLabelText("Màu chính")).toHaveValue("#112233");
  });

  it("saves the edited document (full replace)", async () => {
    renderScreen();
    await loaded();
    fireEvent.change(nameInput(), { target: { value: "XYZ CRM" } });
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() =>
      expect(updateTenantBrandingDoc).toHaveBeenCalledWith(
        expect.objectContaining({ appName: "XYZ CRM", accentColor: "#112233" }),
      ),
    );
  });

  it("reset sends an empty document", async () => {
    renderScreen();
    await loaded();
    fireEvent.click(screen.getByText("Trở về mặc định Nexus"));
    await waitFor(() => expect(updateTenantBrandingDoc).toHaveBeenCalledWith({}));
  });

  it("the preview wears the UNSAVED accent on real SLDS components, without saving", async () => {
    renderScreen();
    await loaded();
    fireEvent.change(screen.getByLabelText("Màu chính"), { target: { value: "#2e844a" } });
    const preview = screen.getByTestId("customer-preview-frame");
    expect(preview.style.getPropertyValue("--slds-r-color-brand-50")).toBe("#2e844a");
    expect(preview.style.getPropertyValue("--slds-g-color-accent-container-1")).toBe("var(--slds-r-color-brand-50)");
    expect(preview.querySelector(".slds-button_brand")).not.toBeNull();
    expect(updateTenantBrandingDoc).not.toHaveBeenCalled();
  });

  it("'Dùng màu mặc định' drops the accent from the saved document", async () => {
    renderScreen();
    await loaded();
    fireEvent.click(screen.getByRole("button", { name: "Dùng màu mặc định" }));
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() =>
      expect(updateTenantBrandingDoc).toHaveBeenCalledWith(expect.objectContaining({ accentColor: "" })),
    );
  });

  it("rejects an oversized logo before encoding it", async () => {
    renderScreen();
    await loaded();
    const input = document.querySelector<HTMLInputElement>("input[type=file]")!;
    const big = new File([new Uint8Array(80 * 1024)], "logo.png", { type: "image/png" });
    fireEvent.change(input, { target: { files: [big] } });
    expect(await screen.findByText(/Ảnh logo tối đa 72KB/)).toBeInTheDocument();
  });

  it("surfaces a server 422 verbatim", async () => {
    vi.mocked(updateTenantBrandingDoc).mockRejectedValue(
      new ApiError(422, "BRANDING_INVALID", "accentColor phải là mã hex dạng #RRGGBB"),
    );
    renderScreen();
    await loaded();
    fireEvent.change(nameInput(), { target: { value: "Đổi tên" } });
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() =>
      expect(screen.getByText(/accentColor phải là mã hex/)).toBeInTheDocument(),
    );
  });

  // specs/063 S7-3: a delivered workspace — the builder edits a DRAFT. The screen labels it, says the
  // customer has not got it yet, and never writes it into the shared boot cache (other tabs, login page).
  it("labels a draft, says the customer has not seen it, and keeps it out of the shared boot cache", async () => {
    window.localStorage.removeItem("nexus_branding");
    vi.mocked(getTenantBrandingDoc).mockResolvedValue({ branding: { appName: "ABC Manager", accentColor: "#112233" }, draftState: "edited" });
    vi.mocked(updateTenantBrandingDoc).mockImplementation(async (b) => ({ branding: b, draftState: "edited" }));
    renderScreen();
    await loaded();
    expect(screen.getAllByText("Nháp").length).toBeGreaterThan(0);
    expect(screen.getByText("Nháp")).toHaveClass("nx-status"); // coloured text, not a badge (D-UI-14)
    expect(screen.getByText(/khách chưa thấy/)).toBeInTheDocument();
    fireEvent.change(nameInput(), { target: { value: "Bản mới" } });
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() => expect(updateTenantBrandingDoc).toHaveBeenCalled());
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã lưu vào bản nháp · Khách chưa thấy"));
    expect(window.localStorage.getItem("nexus_branding")).toBeNull();
  });

  it("a live save still caches the branding and shows no draft label", async () => {
    window.localStorage.removeItem("nexus_branding");
    renderScreen();
    await loaded();
    expect(screen.queryByText("Nháp")).not.toBeInTheDocument();
    fireEvent.change(nameInput(), { target: { value: "ABC Mới" } });
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() => expect(window.localStorage.getItem("nexus_branding")).toContain("ABC Mới"));
  });

  // specs/073 kh3 T236 (FR-005, FR-007): the preview is a picture of the CUSTOMER's login card, so inside the flattened
  // Studio surface it must still draw the customer's tabs (a real tablist), not the scrolling headings Studio uses.
  it("inside an EmbeddedSurface the preview still draws the customer's face (real tablist)", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <EmbeddedSurface>
          <BrandingScreen tabs={["Khách hàng"]} />
        </EmbeddedSurface>
      </QueryClientProvider>,
    );
    await loaded();
    const preview = screen.getByTestId("customer-preview-frame");
    expect(preview.querySelector("[role=tablist]")).not.toBeNull();
    expect(preview.classList.contains("nx-face-customer")).toBe(true);
  });

  it("shows the UNSAVED name in the frame; no fake card/tab/progress bar from the old preview", async () => {
    renderScreen();
    await loaded();
    fireEvent.change(nameInput(), { target: { value: "Tên mới" } });
    const frame = screen.getByTestId("customer-preview-frame");
    expect(frame.textContent).toContain("Tên mới");
    expect(frame.querySelector(".slds-progress-bar")).toBeNull();
    expect(screen.getByTestId("branding-preview").querySelector(".slds-card")).toBeNull();
  });

  it("falls back to #0176d3 in the frame when no accent is set", async () => {
    vi.mocked(getTenantBrandingDoc).mockResolvedValue({ branding: { appName: "ABC Manager" } });
    renderScreen();
    await loaded();
    expect(screen.getByTestId("customer-preview-frame").style.getPropertyValue("--slds-r-color-brand-50")).toBe("#0176d3");
  });

  it("the frame is one inert picture labelled for assistive tech", async () => {
    renderScreen();
    await loaded();
    const pic = screen.getByRole("img", { name: "Xem trước màn khách thấy" });
    expect(pic.hasAttribute("inert")).toBe(true);
  });

  it("warns on low contrast but does not block saving; frame follows the colour", async () => {
    renderScreen();
    await loaded();
    fireEvent.change(screen.getByLabelText("Màu chính"), { target: { value: "#ffd700" } });
    expect(screen.getByText(/Chữ trắng trên màu này khó đọc \(1,4:1, cần ít nhất 4,5:1\)/)).toBeInTheDocument();
    expect(screen.getByTestId("customer-preview-frame").style.getPropertyValue("--slds-r-color-brand-50")).toBe("#ffd700");
    fireEvent.click(screen.getByText("Lưu thương hiệu"));
    await waitFor(() => expect(updateTenantBrandingDoc).toHaveBeenCalledWith(expect.objectContaining({ accentColor: "#ffd700" })));
  });

  it("no contrast warning for the stock colour", async () => {
    vi.mocked(getTenantBrandingDoc).mockResolvedValue({ branding: { accentColor: "#0176d3" } });
    renderScreen();
    await waitFor(() => expect(screen.getByLabelText("Màu chính")).toHaveValue("#0176d3"));
    expect(screen.queryByText(/khó đọc/)).toBeNull();
  });

  it("Lưu/Huỷ bar only while unsaved; Huỷ restores the saved name", async () => {
    renderScreen();
    await loaded();
    expect(screen.queryByText("Lưu thương hiệu")).toBeNull();
    fireEvent.change(nameInput(), { target: { value: "Khác" } });
    expect(screen.getByText("Lưu thương hiệu")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(nameInput().value).toBe("ABC Manager");
  });

  it("DOM order: name, colour, logo, frame, then the 'Nhận tiền' group", async () => {
    renderScreen();
    await loaded();
    const pos = (el: Element) => el;
    const els = [nameInput(), screen.getByLabelText("Màu chính"), document.querySelector("input[type=file]")!, screen.getByTestId("branding-preview"), screen.getByText("Nhận tiền")].map(pos);
    for (let i = 1; i < els.length; i++) expect(els[i - 1].compareDocumentPosition(els[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

// specs/074 lát f2-ai-preview-menu-access-report-brand (T410): an assistant branding change shows in the frame, with the old values.
describe("BrandingScreen — assistant preview (docType branding)", () => {
  const change = mkChange("branding", "update", {
    before: { appName: "ABC Manager", accentColor: "#112233" },
    after: { appName: "Phòng khám Nắng", accentColor: "#2e844a" },
  });
  const renderWith = (c: ReturnType<typeof mkChange> | null) => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={qc}><WithPreview change={c}><BrandingScreen /></WithPreview></QueryClientProvider>);
  };

  it("the frame wears the proposed name and colour, says 'Trước: …', and the form keeps the saved values", async () => {
    renderWith(change);
    const frameEl = () => screen.getByTestId("customer-preview-frame");
    await waitFor(() => expect(frameEl().textContent).toContain("Phòng khám Nắng"));
    await waitFor(() => expect(screen.getByLabelText("Màu chính")).toHaveValue("#112233"));
    const frame = frameEl();
    expect(frame.style.getPropertyValue("--slds-r-color-brand-50")).toBe("#2e844a");
    expect(frame.textContent).toContain("Trước: ABC Manager");
    expect(frame.querySelector(".nx-proposed")).not.toBeNull();
    expect(updateTenantBrandingDoc).not.toHaveBeenCalled();
  });

  it("Bỏ (clear) brings back the saved look", async () => {
    renderWith(change);
    const frameEl = () => screen.getByTestId("customer-preview-frame");
    await waitFor(() => expect(frameEl().textContent).toContain("Phòng khám Nắng"));
    fireEvent.click(screen.getByRole("button", { name: "clear-preview" }));
    await waitFor(() => expect(frameEl().textContent).not.toContain("Phòng khám Nắng"));
    const frame = frameEl();
    expect(frame.textContent).not.toContain("Trước:");
    expect(frame.style.getPropertyValue("--slds-r-color-brand-50")).toBe("#112233");
  });

  it("no active change: no ghost (regression)", async () => {
    renderWith(null);
    const frame = await screen.findByTestId("customer-preview-frame");
    expect(frame.querySelector(".nx-proposed")).toBeNull();
  });
});
