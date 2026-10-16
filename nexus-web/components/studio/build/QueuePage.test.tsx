// QueuePage (specs/074 lát f2-automation-approval-queue): `dung/tu-dong/hang-doi[/<id>]` — same scope states as the
// approval page; `hang-doi/<id>` opens that queue, a queue of another app's table opens nothing.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AppSummary, WorkQueue } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/tu-dong/hang-doi",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const Q1: WorkQueue = { id: "q1", name: "leads_moi", label: "Lead mới", entityName: "crm_leads", filters: [], sortOrder: "asc", actionVerb: "open", position: 0, isActive: true };

const appsCatalog = vi.fn();
const appNav = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  appsCatalog: (...a: unknown[]) => appsCatalog(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listEntities: vi.fn(async () => []),
  listFields: vi.fn(async () => []),
  listRoles: vi.fn(async () => []),
  listWorkQueuesAdmin: vi.fn(async () => [Q1]),
}));

import { QueuePage } from "./QueuePage";

const navOf = (...names: string[]) => ({ app: app("x"), items: names.map((n) => ({ type: "entity", refId: n, label: n, href: `/${n}` })) });
function renderPage(props: { id?: string; app?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><QueuePage slug="northwind" {...props} /></QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  appsCatalog.mockResolvedValue([app("crm"), app("ticket")]);
  appNav.mockImplementation(async (n: string) => (n === "crm" ? navOf("crm_leads") : navOf("pm_tasks")));
});

describe("QueuePage", () => {
  it("an unknown app says so", async () => {
    renderPage({ app: "zzz" });
    expect(await screen.findByText("Không có app zzz")).toBeInTheDocument();
  });

  it("an app with no queue shows the empty sentence and opens nothing", async () => {
    renderPage({ app: "ticket", id: Q1.id });
    await waitFor(() => expect(document.body.textContent).toContain("Chưa có hàng đợi nào"));
    expect(screen.queryByText("Lead mới")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Sửa hàng đợi" })).toBeNull();
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/tu-dong/hang-doi?app=ticket", { scroll: false }));
  });

  it("hang-doi/<id> opens that queue and leaves the URL alone", async () => {
    renderPage({ app: "crm", id: Q1.id });
    expect(await screen.findByRole("heading", { name: "Sửa hàng đợi" })).toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 30));
    expect(replace).not.toHaveBeenCalled();
  });
});
