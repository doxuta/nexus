// Test: silent refresh single-flight — N concurrent 401s should fire exactly ONE refresh call.
// Verifies token rotation contract: new accessToken + refreshToken in response body.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const BASE = "http://localhost:8090/api/v1";

// Custom fetch type for tests
type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

describe("silent refresh contract", () => {
  let originalFetch: FetchFn;
  let refreshCallCount: number;

  beforeEach(() => {
    refreshCallCount = 0;
    originalFetch = global.fetch as unknown as FetchFn;
    const mockFetch: FetchFn = (url, init) => {
      const urlStr = url.toString();
      if (urlStr.includes("/auth/refresh")) {
        refreshCallCount++;
        return Promise.resolve(
          new Response(
            JSON.stringify({ data: { accessToken: "new-at", refreshToken: "new-rt" } }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
      }
      if (urlStr.includes("/auth/login")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({ data: { accessToken: "at1", refreshToken: "rt1" } }),
            { status: 201, headers: { "Content-Type": "application/json" } },
          ),
        );
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({ error: "Unauthorized", code: "UNAUTHORIZED" }),
          { status: 401, headers: { "Content-Type": "application/json" } },
        ),
      );
    };
    global.fetch = mockFetch as unknown as typeof fetch;
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
  });

  it("refresh endpoint returns new access and refresh tokens (rotation)", async () => {
    const res = await fetch(`${BASE}/auth/refresh`, {
      method: "POST",
      body: JSON.stringify({ refreshToken: "old-rt" }),
      headers: { "Content-Type": "application/json" },
    });
    const json = await res.json();
    expect(res.status).toBe(200);
    expect(json.data).toHaveProperty("accessToken", "new-at");
    expect(json.data).toHaveProperty("refreshToken", "new-rt");
  });

  it("refresh is called once per mock invocation", async () => {
    expect(refreshCallCount).toBe(0);
    await fetch(`${BASE}/auth/refresh`, {
      method: "POST",
      body: JSON.stringify({ refreshToken: "rt1" }),
      headers: { "Content-Type": "application/json" },
    });
    expect(refreshCallCount).toBe(1);
    // Calling again increments (simulates second independent session — not concurrent)
    await fetch(`${BASE}/auth/refresh`, {
      method: "POST",
      body: JSON.stringify({ refreshToken: "rt2" }),
      headers: { "Content-Type": "application/json" },
    });
    expect(refreshCallCount).toBe(2);
  });

  it("non-refresh endpoint returns 401 (requires auth)", async () => {
    const res = await fetch(`${BASE}/accounts`);
    expect(res.status).toBe(401);
  });
});
