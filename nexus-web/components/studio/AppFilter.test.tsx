// AppFilter (specs/074 T013): the app filter of the Studio build pages — shown only when the project has > 1 app.
import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));

import { AppFilter } from "./AppFilter";

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const props = { slug: "northwind", route: "dung/du-lieu" as const, hiddenNoun: "bảng" };

describe("AppFilter", () => {
  beforeEach(() => replace.mockReset());

  it("renders nothing for 0 or 1 app", () => {
    const { container, rerender } = render(<AppFilter {...props} apps={[]} value={null} hiddenCount={0} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<AppFilter {...props} apps={[app("crm")]} value="crm" hiddenCount={3} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("with two apps: a select of the apps plus 'Tất cả app'", () => {
    render(<AppFilter {...props} apps={[app("crm"), app("tk")]} value={null} hiddenCount={0} />);
    const sel = screen.getByRole("combobox", { name: "Xem theo app" }) as HTMLSelectElement;
    expect([...sel.options].map((o) => o.textContent)).toEqual(["Tất cả app", "CRM", "TK"]);
    expect(sel.value).toBe("");
  });

  it("choosing an app writes ?app= through studioHref; 'Tất cả app' clears it", () => {
    const { rerender } = render(<AppFilter {...props} apps={[app("crm"), app("tk")]} value={null} hiddenCount={0} />);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "tk" } });
    expect(replace.mock.lastCall?.[0]).toBe("/studio/du-an/northwind/dung/du-lieu?app=tk");
    rerender(<AppFilter {...props} apps={[app("crm"), app("tk")]} value="tk" hiddenCount={0} />);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "" } });
    expect(replace.mock.lastCall?.[0]).toBe("/studio/du-an/northwind/dung/du-lieu");
  });

  it("filtered with hidden rows: 'n bảng của app khác đang ẩn' and 'Xem tất cả' is a plain text link without ?app=", () => {
    render(<AppFilter {...props} apps={[app("crm"), app("tk")]} value="crm" hiddenCount={4} />);
    expect(screen.getByText(/4 bảng của app khác đang ẩn/)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Xem tất cả" });
    expect(link).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
    expect(link.className).not.toMatch(/slds-button/);
  });

  it("no hidden line when nothing is hidden or when no app is chosen", () => {
    const { rerender } = render(<AppFilter {...props} apps={[app("crm"), app("tk")]} value="crm" hiddenCount={0} />);
    expect(screen.queryByText(/đang ẩn/)).toBeNull();
    rerender(<AppFilter {...props} apps={[app("crm"), app("tk")]} value={null} hiddenCount={5} />);
    expect(screen.queryByText(/đang ẩn/)).toBeNull();
  });
});
