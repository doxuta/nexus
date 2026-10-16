// specs/073 T434 (FR-025c): signing in again from the "Phiên đăng nhập hết hạn" screen is an identity change inside the
// same page (router.replace, not a full load), so what the query cache holds about the customer the session was for must
// be gone before anything is drawn for whoever signs in next.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { useEffect } from "react";
import { describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api", () => ({
  login: vi.fn(),
  logout: vi.fn(),
  verifyMfa: vi.fn(),
  getMe: vi.fn(async () => ({ id: "u" })),
  getCachedMe: vi.fn(() => null),
  isAuthed: vi.fn(() => false),
  ensureAuth: vi.fn(async () => {}),
}));

import { AuthProvider, useAuth } from "@/lib/auth/AuthProvider";

describe("AuthProvider.clearExpired", () => {
  it("cancels in-flight queries and clears the whole cache, then lowers the flag", async () => {
    const qc = new QueryClient();
    qc.setQueryData(["entities"], [{ name: "khach_a_only" }]);
    const order: string[] = [];
    vi.spyOn(qc, "cancelQueries").mockImplementation(async () => {
      order.push("cancel");
    });
    const realClear = qc.clear.bind(qc);
    vi.spyOn(qc, "clear").mockImplementation(() => {
      order.push("clear");
      realClear();
    });
    let auth!: ReturnType<typeof useAuth>;
    function Probe() {
      const a = useAuth();
      useEffect(() => {
        auth = a;
      });
      return null;
    }
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <Probe />
        </AuthProvider>
      </QueryClientProvider>,
    );
    await act(async () => {
      window.dispatchEvent(new CustomEvent("nexus:session-expired"));
    });
    expect(auth.sessionExpired).toBe(true);
    await act(async () => {
      auth.clearExpired();
    });
    expect(order).toEqual(["cancel", "clear"]);
    expect(qc.getQueryData(["entities"])).toBeUndefined();
    expect(auth.sessionExpired).toBe(false);
  });
});
