// BrandingBoot — specs/063-draft-publish S7-3 T073: a draft-preview tab paints the builder's DRAFT
// branding (admin route) and never puts it in the shared boot cache; every other tab keeps the public one.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { BrandingBoot } from "./BrandingBoot";
import { DRAFT_PREVIEW_KEY, clearDraftPreview } from "@/lib/previewMode";
import { getPublicBranding, getTenantBranding } from "@/lib/api";
import { applyBranding } from "@/lib/branding";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, getPublicBranding: vi.fn(), getTenantBranding: vi.fn(), tenantSlug: () => "acme" };
});
vi.mock("@/lib/branding", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/branding")>();
  return { ...actual, applyBranding: vi.fn() };
});

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.mocked(applyBranding).mockReset();
  vi.mocked(getPublicBranding).mockReset().mockResolvedValue({ accentColor: "#111111" });
  vi.mocked(getTenantBranding).mockReset().mockResolvedValue({ accentColor: "#222222" });
});
afterEach(() => clearDraftPreview());

describe("BrandingBoot", () => {
  it("an ordinary tab paints the PUBLIC document and caches it", async () => {
    render(<BrandingBoot />);
    await waitFor(() => expect(applyBranding).toHaveBeenCalledWith({ accentColor: "#111111" }, undefined));
    expect(getTenantBranding).not.toHaveBeenCalled();
  });

  it("a draft-preview tab paints the DRAFT document from the admin route, without touching the boot cache", async () => {
    window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: "/studio" }));
    render(<BrandingBoot />);
    await waitFor(() => expect(applyBranding).toHaveBeenCalledWith({ accentColor: "#222222" }, { persist: false }));
    expect(getPublicBranding).not.toHaveBeenCalled();
    for (const call of vi.mocked(applyBranding).mock.calls) expect(call[1]).toEqual({ persist: false });
  });

  it("when the draft cannot be read (not the builder) the tab falls back to the public document, still uncached", async () => {
    window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: "/studio" }));
    vi.mocked(getTenantBranding).mockRejectedValue(new Error("403"));
    render(<BrandingBoot />);
    await waitFor(() => expect(applyBranding).toHaveBeenCalledWith({ accentColor: "#111111" }, { persist: false }));
  });
});
