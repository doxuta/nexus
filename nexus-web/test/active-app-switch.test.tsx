// Queue #297, found live: the app query client keeps the previous key's data while a key changes
// (D17, placeholderData: keepPreviousData), so right after switching from Ticketing to Clinic the
// provider said "active app = Clinic, nav loaded" and handed out TICKETING's tables. Every Studio
// builder soft-filtered on that (the old useStudioScope): Clinic's automation panel opened Ticketing's SLA
// rule — Xoá/Cập nhật live — until Clinic's nav landed. Pinned with the PRODUCTION query client.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { ActiveAppProvider, useActiveApp } from "@/lib/apps";
import { entityNamesFromNav } from "@/lib/appScope";
import { NexusQueryProvider } from "@/lib/query/client";
import { appNav, appsCatalog, type AppNav, type AppSummary } from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1" } }) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, appsCatalog: vi.fn(), appNav: vi.fn() };
});

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name, icon: null, color: null, isDefault: false, sortOrder: 0 });
const nav = (name: string, entities: string[]): AppNav => ({
  app: app(name),
  items: entities.map((e) => ({ type: "entity", refId: `id-${e}`, label: e, href: `/${e}` })),
});

let releaseClinic: (v: AppNav) => void = () => {};

function Probe() {
  const { activeApp, activeMeta, activeNav, isLoading, setActiveApp } = useActiveApp();
  const scope = { app: activeMeta, entityNames: entityNamesFromNav(activeNav), isLoading };
  return (
    <div>
      <p data-testid="state">
        {`${activeApp}|${activeNav ? activeNav.map((i) => i.href).join(",") : "null"}|${isLoading}`}
      </p>
      <p data-testid="scope">
        {`${scope.app?.name ?? "null"}|${scope.entityNames ? [...scope.entityNames].join(",") : "null"}|${scope.isLoading}`}
      </p>
      <button type="button" onClick={() => setActiveApp("clinic")}>to clinic</button>
    </div>
  );
}

beforeEach(() => {
  // pre-L2b bare key + a signed-in home user ⇒ adopted once (specs/062 L2b)
  localStorage.setItem("nexus_user", JSON.stringify({ id: "u-1", tenantId: "t-1", email: "a@b.vn" }));
  localStorage.setItem("nexus.activeApp", "ticketing");
  vi.mocked(appsCatalog).mockResolvedValue([app("ticketing"), app("clinic")]);
  vi.mocked(appNav).mockImplementation((name: string) => name === "ticketing"
    ? Promise.resolve(nav("ticketing", ["tk_tickets"]))
    : new Promise<AppNav>((resolve) => { releaseClinic = resolve; }));
});

describe("ActiveAppProvider across an app switch", () => {
  it("never hands out the previous app's nav as the new app's — the scope stays loading until it lands", async () => {
    render(<NexusQueryProvider><ActiveAppProvider><Probe /></ActiveAppProvider></NexusQueryProvider>);
    await waitFor(() => expect(screen.getByTestId("state")).toHaveTextContent("ticketing|/tk_tickets|false"));

    await act(async () => { screen.getByRole("button", { name: "to clinic" }).click(); });
    // Clinic's nav is still in flight: NOT Ticketing's tables under Clinic's name.
    expect(screen.getByTestId("state")).toHaveTextContent("clinic|null|true");
    expect(screen.getByTestId("scope")).toHaveTextContent("clinic|null|true");

    await act(async () => { releaseClinic(nav("clinic", ["clinic_patients"])); });
    await waitFor(() => expect(screen.getByTestId("scope")).toHaveTextContent("clinic|clinic_patients|false"));
  });
});
