// Component test — /settings on SLDS (specs/038 T032): vertical tabs (Giao diện / Ngôn ngữ /
// Giới thiệu), no light/dark switch any more (FR-012), and the language choice still persisted
// to nexus.prefs.v1 — the key the i18n provider reads.
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { PreferencesScreen } from "@/components/screens/PreferencesScreen";

function storedPrefs() {
  return JSON.parse(window.localStorage.getItem("nexus.prefs.v1") ?? "{}");
}

describe("PreferencesScreen (SLDS)", () => {
  beforeEach(() => window.localStorage.clear());
  afterEach(() => window.localStorage.clear());

  it("renders the three preference groups as vertical tabs, appearance first, with no dark-mode option", () => {
    render(<PreferencesScreen />);
    const tabs = screen.getAllByRole("tab").map((el) => el.textContent);
    expect(tabs).toEqual(["Giao diện", "Ngôn ngữ", "Giới thiệu"]);
    expect(screen.getByRole("tab", { name: "Giao diện" })).toHaveAttribute("aria-selected", "true");
    expect(screen.queryByText("Tối")).not.toBeInTheDocument();
    expect(screen.getByText(/Mật độ hàng chỉnh ngay trên từng danh sách/)).toBeInTheDocument();
  });

  it("choosing English on the Ngôn ngữ tab writes locale=en to nexus.prefs.v1", () => {
    render(<PreferencesScreen />);
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Ngôn ngữ" }));
    fireEvent.click(screen.getByRole("tab", { name: "Ngôn ngữ" }));
    expect(screen.getByRole("radiogroup")).toBeInTheDocument();
    expect(screen.getByLabelText("Tiếng Việt")).toBeChecked();
    fireEvent.click(screen.getByLabelText("English"));
    expect(screen.getByLabelText("English")).toBeChecked();
    expect(storedPrefs().locale).toBe("en");
    // Blueprint badges/base success state: text only, no icon (a glyph would sit grey on green).
    const saved = screen.getByText("Đã lưu").closest(".slds-badge")!;
    expect(saved).toHaveClass("slds-theme_success");
    expect(saved.querySelector(".slds-badge__icon, svg")).toBeNull();
  });

  it("the Giới thiệu tab keeps the product facts and the support address", () => {
    render(<PreferencesScreen />);
    expect(screen.getByText("1.0.0-beta")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "support@example.com" })).toHaveAttribute("href", "mailto:support@example.com");
  });
});
