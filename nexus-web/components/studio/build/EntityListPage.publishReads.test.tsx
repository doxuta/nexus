// Issue #53: a Studio page that mounts useDirectApply (here the table list, whose "+ Bảng mới" form asks before seeding
// grants) inside the real project frame. The frame reads the publish state once (usePublishState, shared per project since
// #46); useDirectApply used to add a read of its own on mount and on every announcement. One announcement, one read.
import { QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/du-lieu",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("@/components/shell/ModeSwitch", () => ({ useFacePaths: () => ({ appHref: "/accounts" }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), info: vi.fn(), error: vi.fn(), withAction: vi.fn() } }));
vi.mock("@/lib/studioContext", async (orig) => ({
  ...(await orig<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  useStudioTransition: () => false,
  useStudioTransitionFailed: () => false,
  useReloadOnIdentityStorage: () => {},
}));

const api = vi.hoisted(() => ({
  listEntities: vi.fn(), appsCatalog: vi.fn(), appNav: vi.fn(), listFields: vi.fn(), listAllRelationships: vi.fn(),
  listWorkflows: vi.fn(), adminApp: vi.fn(), listRecords: vi.fn(), getPublishState: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), getUsage: () => Promise.reject(new Error("no usage")) }));

import type { AppSummary } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { makeClient } from "@/lib/query/client";
import { ProjectFrame } from "../ProjectFrame";
import { EntityListPage } from "./EntityListPage";

const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  api.listEntities.mockResolvedValue([{ id: "e-a", name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" }]);
  api.appsCatalog.mockResolvedValue([app("crm")]);
  api.appNav.mockResolvedValue({ app: app("crm"), items: [{ type: "entity", refId: "crm_accounts", label: "crm_accounts", href: "/crm_accounts" }] });
  api.listFields.mockResolvedValue([{ name: "a" }]);
  api.listAllRelationships.mockResolvedValue([]);
  api.listWorkflows.mockResolvedValue([]);
  api.adminApp.mockResolvedValue({ items: [] });
  api.listRecords.mockResolvedValue([]);
  api.getPublishState.mockResolvedValue({ delivered: true, pendingCount: 0, draftableTypes: [], directTypes: [], headVersion: { versionNo: 2 } });
});

describe("a Studio page with useDirectApply in the project frame (#53)", () => {
  it("one announcement on the drafts bus sends one publish-state read, and the page still knows the project is delivered", async () => {
    const qc = makeClient(); // the app's 2-minute staleTime: only an announcement re-reads
    render(
      <QueryClientProvider client={qc}>
        <ProjectFrame slug="northwind"><EntityListPage slug="northwind" /></ProjectFrame>
      </QueryClientProvider>,
    );
    await screen.findByRole("link", { name: "Khách hàng" });
    await waitFor(() => expect(qc.isFetching()).toBe(0));
    await new Promise((r) => setTimeout(r, 20));
    expect(api.getPublishState).toHaveBeenCalledTimes(1); // the frame's read; the page adds none of its own

    for (let i = 0; i < 2; i++) {
      const before = api.getPublishState.mock.calls.length;
      act(() => emitDraftsChanged("screen-save"));
      await new Promise((r) => setTimeout(r, 20));
      await waitFor(() => expect(qc.isFetching()).toBe(0));
      expect(api.getPublishState.mock.calls.length - before).toBe(1);
    }

    // useDirectApply answers from that read: a table in an app is asked in the form, with no read at the click.
    fireEvent.click(screen.getByRole("button", { name: "+ Bảng mới" }));
    const form = await screen.findByRole("form", { name: "+ Bảng mới" });
    fireEvent.change(within(form).getByLabelText(/Tên hiển thị/), { target: { value: "Đơn hàng" } });
    expect(await within(form).findByText("Áp ngay cho người dùng thật")).toBeInTheDocument();
    const before = api.getPublishState.mock.calls.length;
    fireEvent.click(within(form).getByRole("button", { name: "Tạo bảng" }));
    expect(await within(form).findByRole("button", { name: "Áp ngay" })).toBeInTheDocument();
    expect(api.getPublishState.mock.calls.length).toBe(before);
  });
});
