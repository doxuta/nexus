// AppsPage (specs/074 lát f2-apps-menu, T307): four states and ?app= preselect.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AdminApp } from "@/lib/api";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/man-hinh/app",
  useRouter: () => ({ push: vi.fn(), replace, back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: true } }) }));
vi.mock("@/components/studio/AppsManager", () => ({
  AppsManager: (p: { app?: string | null }) => <div data-testid="list" data-app={p.app ?? ""} />,
  NavEditor: (p: { app: AdminApp }) => <div data-testid="editor">{p.app.label}</div>,
}));
const adminApps = vi.fn();
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), adminApps: (...a: unknown[]) => adminApps(...a) }));
import { AppsPage } from "./AppsPage";

const A = (name: string, label: string): AdminApp => ({ id: `id-${name}`, name, label, icon: null, color: null, isDefault: false, sortOrder: 0, description: null, navCount: 0 });
function mount(app?: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><AppsPage slug="northwind" app={app} /></QueryClientProvider>);
}

describe("AppsPage", () => {
  beforeEach(() => vi.clearAllMocks());
  it("loading: no list yet", () => {
    adminApps.mockReturnValue(new Promise(() => {}));
    mount();
    expect(screen.queryByTestId("list")).toBeNull();
  });
  it("error: says so and offers Thử lại", async () => {
    adminApps.mockRejectedValue(new Error("boom"));
    mount();
    expect(await screen.findByText("Không tải được danh sách app.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Thử lại" })).toBeTruthy();
  });
  it("empty: list shown (it owns the empty state), no editor, no pick hint", async () => {
    adminApps.mockResolvedValue([]);
    mount();
    await screen.findByTestId("list");
    expect(screen.queryByTestId("editor")).toBeNull();
    expect(screen.queryByText(/Chọn một app ở bên trái/)).toBeNull();
  });
  it("data without ?app=: list + pick hint", async () => {
    adminApps.mockResolvedValue([A("crm", "CRM")]);
    mount();
    await screen.findByTestId("list");
    expect(screen.getByText(/Chọn một app ở bên trái/)).toBeTruthy();
  });
  it("?app= preselects: editor of that app, list marks it", async () => {
    adminApps.mockResolvedValue([A("crm", "CRM"), A("lms", "LMS")]);
    mount("lms");
    expect((await screen.findByTestId("editor")).textContent).toBe("LMS");
    expect(screen.getByTestId("list").getAttribute("data-app")).toBe("lms");
  });
});
