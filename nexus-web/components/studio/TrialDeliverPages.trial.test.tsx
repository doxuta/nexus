import { render, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
let appsFixture: { id: string; name: string; label: string; isDefault: boolean }[] = [];
const TWO = [
  { id: "a1", name: "crm", label: "CRM", isDefault: false },
  { id: "a2", name: "lms", label: "LMS", isDefault: true },
];
const replace = vi.fn();
const tableApp = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
  usePathname: () => "/studio/du-an/northwind/dung/chay-thu",
  useSearchParams: () => new URLSearchParams(search),
}));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  adminApps: async () => appsFixture,
  adminApp: async () => ({ items: [] }),
  listEntities: async () => [],
}));
vi.mock("./useProjectQuery", async () => {
  const { useQuery } = await import("@tanstack/react-query");
  return { useProjectQuery: (s: string, k: unknown[], fn: () => Promise<unknown>) => useQuery({ queryKey: [s, ...k], queryFn: fn }) };
});
vi.mock("./trial/TrialRolesTable", () => ({
  TrialRolesTable: ({ app }: { app: { id: string; name: string } | null }) => {
    tableApp(app);
    return <div data-testid="table" />;
  },
}));
vi.mock("./trial/TrialStageSwitch", () => ({ TrialStageSwitch: () => null }));
vi.mock("./SampleDataCard", () => ({ SampleDataCard: () => null }));
vi.mock("./ReadinessChecklist", () => ({ AppsReadiness: () => null }));

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TrialPage } from "./TrialDeliverPages";

function ui() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <TrialPage slug="northwind" />
    </QueryClientProvider>,
  );
}
const last = () => tableApp.mock.calls.at(-1)?.[0];

beforeEach(() => {
  vi.clearAllMocks();
  search = "";
  appsFixture = TWO;
});

describe("TrialPage app choice (the appId handed to 'Xem như' comes from the project's own list)", () => {
  it("no ?app= with 2 apps: the default app", async () => {
    ui();
    await waitFor(() => expect(last()?.id).toBe("a2"));
    expect(replace).not.toHaveBeenCalled();
  });

  it("?app=<valid>: that app", async () => {
    search = "app=crm";
    ui();
    await waitFor(() => expect(last()?.id).toBe("a1"));
    expect(replace).not.toHaveBeenCalled();
  });

  it("?app=<not in this project>: falls back to the default app and the parameter is dropped from the URL", async () => {
    search = "app=foreign-app";
    ui();
    await waitFor(() => expect(last()?.id).toBe("a2"));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/chay-thu"));
  });

  it("a foreign ?app= is dropped but other parameters stay", async () => {
    search = "app=foreign-app&draft=1";
    ui();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/chay-thu?draft=1"));
  });

  it("one app: that app, no picker", async () => {
    appsFixture = [{ id: "only", name: "solo", label: "Solo", isDefault: false }];
    ui();
    await waitFor(() => expect(last()?.id).toBe("only"));
    expect(replace).not.toHaveBeenCalled();
  });

  it("2 apps and none isDefault: the first app", async () => {
    appsFixture = TWO.map((a) => ({ ...a, isDefault: false }));
    ui();
    await waitFor(() => expect(last()?.id).toBe("a1"));
  });

  it("2 apps: the button gets the chosen app's slug and id (name is the slug), not the default's", async () => {
    search = "app=crm";
    ui();
    await waitFor(() => expect(last()).toMatchObject({ id: "a1", name: "crm" }));
  });
});
