// specs/073 kh1a T144/T152 (FR-016): a route declared but not mounted says so in two left-aligned lines.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { t, tf } from "@/lib/i18n";
import { ROUTES } from "@/lib/studioRoutes";
import { PendingPage } from "./PendingPage";

const parent = { label: "Tự động", href: "/studio/du-an/acme/dung/tu-dong" };

describe("PendingPage", () => {
  it("says the feature is not in this build and links up", () => {
    render(<PendingPage feature="Cổng nhận dữ liệu" parent={parent} />);
    expect(screen.getByText("Cổng nhận dữ liệu chưa có trong bản Nexus này.")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "← Tự động" });
    expect(link).toHaveAttribute("href", parent.href);
  });

  it("is two lines: the sentence in .nx-lede and the link — no illustration, no EmptyState, no icon, no notification", () => {
    const { container } = render(<PendingPage feature="Báo cáo" parent={parent} />);
    expect(container.querySelector(".nx-lede")).not.toBeNull();
    expect(container.querySelector(".slds-illustration")).toBeNull();
    expect(container.querySelector("svg")).toBeNull();
    expect(container.querySelector(".slds-scoped-notification")).toBeNull();
    expect(container.querySelectorAll("p").length).toBe(2);
  });

  it("page is the default variant and inline uses the same sentence", () => {
    const a = render(<PendingPage feature="X" parent={parent} />);
    const page = a.container.textContent;
    a.unmount();
    const b = render(<PendingPage feature="X" parent={parent} variant="inline" />);
    expect(b.container.textContent).toBe(page);
    expect(b.container.firstElementChild).not.toBe(null);
  });

  it("the page variant carries page padding, the inline one does not", () => {
    const a = render(<PendingPage feature="X" parent={parent} />);
    expect(a.container.firstElementChild).toHaveClass("slds-p-around_large");
    a.unmount();
    const b = render(<PendingPage feature="X" parent={parent} variant="inline" />);
    expect(b.container.firstElementChild).not.toHaveClass("slds-p-around_large");
  });

  it("works without a parent link", () => {
    render(<PendingPage feature="X" />);
    expect(screen.queryByRole("link")).toBeNull();
  });
});

// specs/073 kh4 T272 (FR-132, string gate c): a page that is not built says one generic sentence and never leaks planning words.
describe("every pending route says the generic sentence and nothing about the plan", () => {
  const LEAK = /#\d+|specs?\/|\bspec\b|\blát\b|\bkh\d|\bF\d\b|\bT\d{3}\b/i;
  const pending = ROUTES.filter((r) => !r.mounted);

  it("the route table still has pending routes to check", () => {
    expect(pending.length).toBeGreaterThan(0);
  });

  it.each(pending.map((r) => [r.id, r.labelKey] as const))("%s", (_id, labelKey) => {
    const { container } = render(<PendingPage feature={t(labelKey)} />);
    expect(container.textContent).toBe(tf("studio.pending.generic", { feature: t(labelKey) }));
    expect(container.textContent).not.toMatch(LEAK);
    expect(t(labelKey)).not.toMatch(LEAK);
  });

  it("the shared pending strings carry no planning word either", () => {
    expect(t("studio.pending.generic")).not.toMatch(LEAK);
    expect(t("studio.pending.back")).not.toMatch(LEAK);
    expect(t("studio.newProject.pending")).not.toMatch(LEAK);
  });
});
