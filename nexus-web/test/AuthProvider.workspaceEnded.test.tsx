// specs/077 T153 (#681 (2)): a Studio tab whose builder session was closed elsewhere (a preview tab's 401 sends the tab
// back to the Studio page, whose first /auth/me then answers WORKSPACE_ENDED) must land on /studio with the home login
// intact — never on /login, which would look like the builder was signed out.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const getMe = vi.fn();
vi.mock("@/lib/api", () => ({
  login: vi.fn(),
  logout: vi.fn(),
  verifyMfa: vi.fn(),
  getMe: (...a: unknown[]) => getMe(...a),
  getCachedMe: vi.fn(() => null),
  isAuthed: vi.fn(() => false),
  ensureAuth: vi.fn(async () => {}),
}));

import { AuthProvider, useAuth } from "@/lib/auth/AuthProvider";

function Probe() {
  const { isLoading } = useAuth();
  return <span data-testid="loading">{String(isLoading)}</span>;
}

const assign = vi.fn();
const realLocation = window.location;

function mount() {
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, pathname: "/studio/du-an/lab/dung/chay-thu", search: "" } });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <AuthProvider>
        <Probe />
      </AuthProvider>
    </QueryClientProvider>,
  );
}

describe("AuthProvider bootstrap when the workspace session already ended", () => {
  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: realLocation });
    assign.mockReset();
    getMe.mockReset();
  });

  it("goes to /studio on WORKSPACE_ENDED", async () => {
    getMe.mockRejectedValue(Object.assign(new Error("Phiên dựng đã kết thúc"), { status: 401, code: "WORKSPACE_ENDED" }));
    mount();
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio"));
    // Still loading: RequireAuth must not get a chance to replace() to /login while the hard navigation is pending.
    expect(screen.getByTestId("loading").textContent).toBe("true");
  });

  it("does not redirect on any other failure (RequireAuth owns /login)", async () => {
    getMe.mockRejectedValue(Object.assign(new Error("x"), { status: 401, code: "UNAUTHORIZED" }));
    mount();
    await waitFor(() => expect(screen.getByTestId("loading").textContent).toBe("false"));
    expect(assign).not.toHaveBeenCalled();
  });
});
