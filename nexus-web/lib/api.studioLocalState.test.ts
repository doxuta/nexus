// specs/073 T435 / FR-087: every path that changes who is signed in (logout, login, SSO exchange, MFA verify) also wipes
// what the Studio remembers about customers — except the per-user recent-projects list.
import { beforeEach, describe, expect, it, vi } from "vitest";

const ok = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
const TOKENS = { data: { accessToken: "T", refreshToken: "R", user: { id: "u-2", fullName: "B", email: "b@x", isAdmin: false } } };

function seed() {
  window.localStorage.setItem("nexus.lastPath.builder:t-1:u-1", "/studio/du-an/acme/dung/du-lieu");
  window.localStorage.setItem("nexus.studio.assistantOpen", "1");
  window.localStorage.setItem("nexus.studio.recentProjects:u-1", JSON.stringify([{ slug: "acme", name: "Acme" }]));
  window.sessionStorage.setItem("nexus.studio.pendingBrief", "tả bằng lời");
}
function expectWiped() {
  expect(window.localStorage.getItem("nexus.lastPath.builder:t-1:u-1")).toBeNull();
  expect(window.localStorage.getItem("nexus.studio.assistantOpen")).toBeNull();
  expect(window.sessionStorage.getItem("nexus.studio.pendingBrief")).toBeNull();
  expect(window.localStorage.getItem("nexus.studio.recentProjects:u-1")).not.toBeNull();
}

async function fresh() {
  vi.resetModules();
  return import("@/lib/api");
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  global.fetch = vi.fn(async () => ok(TOKENS)) as unknown as typeof fetch;
});

describe("Studio local state is wiped when the signed-in person changes", () => {
  it("logout()", async () => {
    const api = await fresh();
    seed();
    await api.logout();
    expectWiped();
  });
  it("login()", async () => {
    const api = await fresh();
    seed();
    await api.login("northwind", "b@x", "pw");
    expectWiped();
  });
  it("ssoExchange()", async () => {
    const api = await fresh();
    seed();
    await api.ssoExchange("code");
    expectWiped();
  });
  it("verifyMfa()", async () => {
    const api = await fresh();
    seed();
    await api.verifyMfa("pending", "123456");
    expectWiped();
  });
});
