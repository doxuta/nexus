// AboutScreen (/about) on SLDS — cards + accordion (specs/038 T032, RESEARCH §5 /about).
// The F7-5 face split must survive the migration: a non-admin (or auth still resolving) gets the
// short how-to with ZERO /studio links (RequireAdmin would bounce them to /home); an admin gets
// the full explainer, whose glossary is now an SLDS accordion fed by lib/help-content.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { AboutScreen } from "./AboutScreen";
import { allHelpEntries } from "@/lib/help-content";

const auth = vi.hoisted(() => ({ user: null as null | { id: string; isAdmin: boolean; isPlatformOperator?: boolean } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const TOKENS = /\b(bg-panel2?|text-fg|border-bd2?|text-fnt|text-mut|rounded-card|rounded-input|bg-acd|text-ac-hi|text-ac|font-display|font-mono)\b/;

beforeEach(() => {
  auth.user = null;
});

describe("AboutScreen (SLDS)", () => {
  it("non-admin: short guide in SLDS cards, no /studio link, approvals reachable", () => {
    auth.user = { id: "u2", isAdmin: false };
    const { container } = render(<AboutScreen />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Hướng dẫn sử dụng");
    expect(container.querySelector(".slds-page-header")).not.toBeNull();
    expect(container.querySelector("article.slds-card")).not.toBeNull();
    expect(container.querySelector('a[href^="/studio"]')).toBeNull();
    expect(screen.getByRole("link", { name: "Phê duyệt" })).toHaveAttribute("href", "/approvals");
    expect(screen.getByText(/Liên hệ quản trị viên của tổ chức bạn/)).toBeInTheDocument();
    expect(container.innerHTML).not.toMatch(TOKENS);
  });

  it("auth still resolving: the end-user guide, never the Studio pitch", () => {
    const { container } = render(<AboutScreen />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Hướng dẫn sử dụng");
    expect(container.querySelector('a[href^="/studio"]')).toBeNull();
  });

  it("operator: full explainer — factory steps link into the Studio project, status badges are SLDS badges", () => {
    auth.user = { id: "u1", isAdmin: true, isPlatformOperator: true };
    const { container } = render(<AboutScreen face="app" />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Giới thiệu Nexus");
    expect(screen.getByRole("link", { name: /Bảng/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
    expect(screen.getByRole("link", { name: /Màn hình/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh");
    expect(screen.getByRole("link", { name: /Sơ đồ liên kết/ })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/so-do");
    expect(container.innerHTML).not.toMatch(/Model entity|Layout<|Workflow \/ Approval/);
    expect(screen.getByText("CRM — đang chạy").closest(".slds-badge")).toHaveClass("slds-theme_success");
    expect(screen.getByText("Bạn đang ở đây").closest(".slds-badge")).not.toBeNull();
    expect(container.innerHTML).not.toMatch(TOKENS);
  });

  it("a customer admin (not the builder) sees the explainer without any link into Studio", () => {
    auth.user = { id: "u1", isAdmin: true };
    const { container } = render(<AboutScreen face="app" />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Giới thiệu Nexus");
    expect(container.querySelector('a[href^="/studio"]')).toBeNull();
  });

  it("admin: the glossary is an SLDS accordion with one section per help entry", () => {
    auth.user = { id: "u1", isAdmin: true };
    const { container } = render(<AboutScreen />);
    const entries = allHelpEntries();
    const buttons = container.querySelectorAll(".slds-accordion .slds-accordion__summary-action");
    expect(buttons).toHaveLength(entries.length);
    const second = buttons[1] as HTMLButtonElement;
    expect(second).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(second);
    expect(second).toHaveAttribute("aria-expanded", "true");
  });
});
