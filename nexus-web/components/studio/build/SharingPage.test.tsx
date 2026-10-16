// SharingPage (specs/074 T223): the Studio page speaks S13 business words, no engine vocabulary or raw column codes.
import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";

vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/acme/dung/nguoi-dung-quyen/chia-se",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn() }),
}));

import { SharingPage } from "./SharingPage";

describe("SharingPage", () => {
  it("shows no machine words and no team-set table", () => {
    const { container } = render(<EmbeddedSurface><SharingPage slug="acme" /></EmbeddedSurface>);
    const text = container.textContent ?? "";
    expect(text).toContain("Chia sẻ & Chủ hồ sơ");
    for (const bad of ["tenant", "ACL", "engine", "team set", "Team Sets", "Bộ nhóm", "accessLevel", "created_by", "owner", "set·"]) {
      expect(text).not.toContain(bad);
    }
  });
});
