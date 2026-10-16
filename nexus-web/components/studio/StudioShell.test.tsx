// specs/073 kh5 T314/T315: the Studio shell wires the new header (switcher, no Setup gear, assistant toggle only where the
// token belongs to the project) and drops the old builder header on the front page.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import type { GlobalHeaderProps } from "@/components/slds/shell/GlobalHeader";

const st = vi.hoisted(() => ({ pathname: "/studio", matches: true, failed: false }));
vi.mock("next/navigation", () => ({ usePathname: () => st.pathname }));
vi.mock("@/lib/studioContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ matches: () => st.matches }),
  useStudioTransitionFailed: () => st.failed,
}));
vi.mock("./StudioSessionBanner", () => ({ StudioSessionBanner: () => <div data-testid="session-banner" /> }));
vi.mock("./ProjectSwitcher", () => ({ ProjectSwitcher: ({ currentSlug }: { currentSlug: string | null }) => <span data-testid="switcher">{String(currentSlug)}</span> }));
vi.mock("@/components/slds/shell/GlobalHeader", () => ({
  ShellGlobalHeader: (p: Pick<GlobalHeaderProps, "switcher" | "assistantToggle" | "hideSetup">) => (
    <div data-testid="gh" data-hide-setup={String(!!p.hideSetup)}>
      {p.switcher}
      {p.assistantToggle && (
        <button type="button" aria-pressed={p.assistantToggle.pressed} onClick={p.assistantToggle.onToggle}>
          Trợ lý
        </button>
      )}
    </div>
  ),
}));

import { StudioShell } from "./StudioShell";

const shell = (children: ReactNode = <p>nội dung</p>) => render(<StudioShell>{children}</StudioShell>);

beforeEach(() => {
  st.matches = true;
  st.failed = false;
});

describe("StudioShell", () => {
  it("front page: header with switcher and no Setup gear, content in one main", () => {
    st.pathname = "/studio";
    const { container } = shell();
    expect(screen.getByTestId("gh")).toHaveAttribute("data-hide-setup", "true");
    expect(screen.getByTestId("switcher")).toHaveTextContent("null");
    expect(container.querySelectorAll("main")).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Trợ lý" })).toBeNull();
  });

  it("the session strip sits right under the global header on every Studio route (specs/073 FR-082)", () => {
    for (const path of ["/studio", "/studio/verticals", "/studio/du-an/acme/dung/du-lieu"]) {
      st.pathname = path;
      const { unmount } = shell();
      expect(screen.getByTestId("gh").nextElementSibling).toBe(screen.getByTestId("session-banner"));
      unmount();
    }
  });

  it("other non-project routes get the same header and one main, no builder header (kh7)", () => {
    st.pathname = "/studio/verticals";
    const { container } = shell();
    expect(container.querySelectorAll("main")).toHaveLength(1);
    expect(container.querySelector(".slds-builder-header")).toBeNull();
    expect(screen.getByTestId("switcher")).toHaveTextContent("null");
  });

  it("inside a project whose token matches: the toggle is offered and docks/undocks the panel, no navigation", async () => {
    st.pathname = "/studio/du-an/northwind/dung/du-lieu";
    shell();
    expect(screen.getByTestId("switcher")).toHaveTextContent("northwind");
    const toggle = screen.getByRole("button", { name: "Trợ lý" });
    const before = toggle.getAttribute("aria-pressed");
    await userEvent.click(toggle);
    expect(toggle.getAttribute("aria-pressed")).not.toBe(before);
  });

  it("inside a project whose token does NOT match: no assistant toggle", () => {
    st.pathname = "/studio/du-an/acme/dung/du-lieu";
    st.matches = false;
    shell();
    expect(screen.queryByRole("button", { name: "Trợ lý" })).toBeNull();
  });

  // A switch started from the header on a page that is NOT a project (front page, assistant, new project…) used to fail
  // in silence: the old session was closed, the latch was up, and nothing drew the way out (specs/073 FR-085(3)).
  it("a failed switch on a non-project route shows the failure with both ways out instead of the page", () => {
    st.pathname = "/studio/moi";
    st.failed = true;
    shell(<p>nội dung</p>);
    expect(screen.queryByText("nội dung")).toBeNull();
    expect(screen.getByRole("button", { name: "Thử lại" })).toHaveFocus();
    expect(screen.getByRole("button", { name: "Về danh sách dự án" })).toBeInTheDocument();
  });

  it("inside a project the frame draws the failure itself, so the shell does not add a second one", () => {
    st.pathname = "/studio/du-an/acme/dung/du-lieu";
    st.failed = true;
    shell(<p>khung dự án</p>);
    expect(screen.getByText("khung dự án")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
  });
});
