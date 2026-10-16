// specs/073 T091 (FR-005, D-UI-27): every builder the Studio frame will mount is rendered inside <EmbeddedSurface>
// and must not repeat the frame — no card, no page header, no h1, no coloured icon tile, no tablist, no docked panel,
// no badge, no avatar, no raw tabs bar, no illustration. A builder that does not meet a rule has an EXEMPTION in
// studio-embedded-registry.tsx with its reason; the number of exemptions may only go down (ratchet "embeddedExempt").
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EMBEDDED_REGISTRY, EMBEDDED_RULES, EMBEDDED_WRAPPERS } from "./studio-embedded-registry";
import { ratchet } from "./studioGateScan";
import { measureEmbedded, renderEmbedded } from "./studioEmbeddedHarness";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true }, isAuthenticated: true }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), prefetch: vi.fn() }),
  usePathname: () => "/studio",
  useSearchParams: () => new URLSearchParams(),
}));

const originalFetch = global.fetch;

beforeEach(() => {
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "test-token");
});
afterEach(() => {
  cleanup();
  global.fetch = originalFetch;
  window.localStorage.clear();
});

describe("Studio builders draw one surface (inside EmbeddedSurface)", () => {
  const mounted = EMBEDDED_REGISTRY.filter((e) => !e.pending);

  it.each(mounted.map((e) => [e.id] as const))("%s: every rule is met or has an exemption, and no exemption is stale", async (id) => {
    const entry = mounted.find((e) => e.id === id)!;
    const { container } = await renderEmbedded(entry);
    const hits = measureEmbedded(container);
    const exempt = entry.exempt ?? {};
    const violations = Object.entries(hits).filter(([rule]) => !(rule in exempt)).map(([rule, n]) => `${rule}: ${n}`);
    const stale = Object.keys(exempt).filter((rule) => !(rule in hits));
    expect({ violations, stale }, `${id}: a violation needs a fix or a reasoned exemption; a stale exemption must be deleted`).toEqual({ violations: [], stale: [] });
  });

  it("the exemption list only shrinks (ceiling in studio-gates.allowlist.json)", () => {
    const counts: Record<string, number> = {};
    for (const e of EMBEDDED_REGISTRY) for (const rule of Object.keys(e.exempt ?? {})) counts[`${e.id}::${rule}`] = 1;
    expect(ratchet("embeddedExempt", counts)).toEqual([]);
  });

  it("every exemption carries a reason that names the slice that removes it", () => {
    for (const e of EMBEDDED_REGISTRY)
      for (const [rule, why] of Object.entries(e.exempt ?? {})) expect(why, `${e.id}::${rule}`).toMatch(/\((?:F\d|kh\d|u\d|so\d|ph\d)[^)]*\)/);
  });

  it("the registry starts with the nine builders of the plan, EntityPage mounted (kh1b2)", () => {
    const ids = EMBEDDED_REGISTRY.map((e) => e.id);
    for (const id of ["WorkflowBuilder", "BrandingScreen", "UserList", "AuditLog", "DoctorScreen", "TeamsList", "ImportScreen", "EntityFieldEditor", "EntityPage"])
      expect(ids).toContain(id);
    expect(EMBEDDED_REGISTRY.find((e) => e.id === "EntityPage")?.pending).toBeUndefined();
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("the wrapper ledger", () => {
  // By name, not a count: ObjectHomePage joins at F6 (u7-nen-nhung-mau-trang) by adding its name to EMBEDDED_WRAPPERS.
  it.each([...EMBEDDED_WRAPPERS])("%s reads EmbeddedSurface", (name) => {
    const file = join(process.cwd(), "components/slds", `${name}.tsx`);
    expect(existsSync(file), file).toBe(true);
    expect(readFileSync(file, "utf8")).toMatch(/useEmbedded\(/);
  });
});

describe("self-check: the counter has teeth", () => {
  it("raw markup of every rule inside an EmbeddedSurface is counted", () => {
    const { container } = render(
      <EmbeddedSurface>
        <div className="slds-card">
          <h1>x</h1>
          <div className="slds-page-header" />
          <span className="slds-icon_container slds-icon-standard-account" />
          <div role="tablist" />
          <div className="slds-panel slds-panel_docked" />
          <span className="slds-badge" />
          <span className="slds-avatar" />
          <div className="slds-tabs_default" />
          <div className="slds-illustration" />
        </div>
      </EmbeddedSurface>,
    );
    expect(Object.keys(measureEmbedded(container)).sort()).toEqual(Object.keys(EMBEDDED_RULES).sort());
  });

  it("the wrappers' own embedded markup is not counted (Badge, Avatar, EmptyState)", async () => {
    const { Badge } = await import("@/components/slds/Badge");
    const { EmptyState } = await import("@/components/slds/EmptyState");
    const { container } = render(
      <EmbeddedSurface>
        <Badge theme="success">Bật</Badge>
        <EmptyState title="Chưa có" description="Thêm" />
      </EmbeddedSurface>,
    );
    expect(measureEmbedded(container)).toEqual({});
  });
});
