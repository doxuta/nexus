// VerticalLibrary ("Cài mẫu & Bàn giao") — three pinned contracts on the merged screen:
//   1. FE-CATCHUP-2 rendering: pack cards show name/version/entityCount from the LIVE
//      /admin/verticals/builtin manifest payload, never a hardcoded version string.
//   2. Card install flow (Wave B2 merge): "Cài đặt" on a card → inline dry-run plan →
//      ConfirmDialog → real install → per-component result. No route hop, no sessionStorage.
//   3. Workspace khách (TENANT-PROVISION MS-B, ported from the deleted DeployScreen):
//      GET /admin/tenants probe gates the panel (FORBIDDEN → exact honest note; other errors →
//      retry-able note that never claims "no permission"); show-once credentials stay gated by
//      the acknowledgement checkbox and vanish from the DOM after "Xong".
// lib/api is mocked at the module seam (same pattern as the assistant tests); the studio catalog,
// orderBuiltinsForDisplay, verticalPlan derivations and the JSX are all real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { VerticalLibrary } from "./VerticalLibrary";
import {
  ensureAuth, listBuiltinVerticals, listEntities, listPackInstalls, dryRunInstall, installPack,
  exportVertical, listTenants, listTenantBackups, provisionTenant, retireTenant, purgeTenant, appsCatalog, planPackUpgrade, listPackUpgrades, enterWorkspace, setOwnerTier, revertOwnerTier, ApiError,
  type AppSummary, type UpgradePlan, type BuiltinVertical, type TenantSummary, type ProvisionedTenant,
} from "@/lib/api";
import { ActiveAppProvider, useActiveApp } from "@/lib/apps";
import { qk } from "@/lib/query/keys";
import { clearWorkspace, setWorkspace } from "@/lib/workspace";
import { toast } from "@/components/ui/Toaster";
import type { InstallPlan, InstallResult } from "@/lib/verticalPlan";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  usePathname: () => "/studio/verticals",
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, // keeps ApiError etc. real
    ensureAuth: vi.fn(),
    listBuiltinVerticals: vi.fn(),
    listEntities: vi.fn(),
    listPackInstalls: vi.fn(),
    dryRunInstall: vi.fn(),
    installPack: vi.fn(),
    exportVertical: vi.fn(),
    listTenantBackups: vi.fn(),
    listTenants: vi.fn(),
    provisionTenant: vi.fn(),
    retireTenant: vi.fn(),
    purgeTenant: vi.fn(),
    appsCatalog: vi.fn(),
    planPackUpgrade: vi.fn(),
    listPackUpgrades: vi.fn(),
    enterWorkspace: vi.fn(),
    setOwnerTier: vi.fn(),
    revertOwnerTier: vi.fn(),
  };
});

vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

// The REAL ActiveAppProvider wraps every render: it is the only observer of the sidebar app
// catalog (qk.apps), so it is what proves an install re-reads that catalog.
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true, tenantId: "t-1" } }) }));

/** Prints the sidebar app catalog as ActiveAppProvider sees it. */
function CatalogProbe() {
  const { apps } = useActiveApp();
  return <div data-testid="catalog">{apps.map((a) => a.name).join(",")}</div>;
}

function renderLib(extra?: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ActiveAppProvider>
        <VerticalLibrary />
        {extra}
      </ActiveAppProvider>
    </QueryClientProvider>,
  );
  return qc;
}

// The REAL registry manifests as the BE ships them today (builtin.go crmPack/clinicPack).
const LIVE_BUILTINS: BuiltinVertical[] = [
  {
    slug: "clinic",
    manifest: { name: "Clinic", slug: "clinic", version: "1.0.0", description: "Bệnh nhân và lịch hẹn khám (phòng khám cơ bản).", author: "Nexus" },
    entityCount: 2,
  },
  {
    slug: "crm",
    manifest: { name: "CRM", slug: "crm", version: "1.1.0", description: "Khách hàng, liên hệ, cơ hội, leads và hoạt động bán hàng (CRM đầy đủ).", author: "Nexus" },
    entityCount: 5,
  },
];

const Nexus: TenantSummary = {
  id: "t-1", slug: "northwind", name: "Nexus", status: "active",
  createdAt: "2026-07-01T00:00:00Z", usableAdminCount: 1, usableOwnerCount: 0, convertibleAdminCount: 0,
};

// A workspace already retired — the ONLY state purge accepts. It stays in the registry precisely
// so the second half of the lifecycle stays reachable.
const RETIRED: TenantSummary = {
  id: "t-9", slug: "khachb", name: "Khách B", status: "active",
  createdAt: "2026-07-01T00:00:00Z", usableAdminCount: 0, usableOwnerCount: 0, convertibleAdminCount: 0,
  retiredAt: "2026-08-01T00:00:00Z",
};

const CLEAN_PLAN: InstallPlan = {
  entitiesToCreate: ["accounts", "contacts"],
  entitiesToSkip: [],
  fieldsToAdd: [],
  nnToRegister: [],
  nnToSkip: [],
  layoutsToInstall: ["accounts"],
  rolesToCreate: [],
  rolesToSkip: [],
  samplesCount: 3,
  conflicts: [],
};

const INSTALL_RESULT: InstallResult = {
  entitiesCreated: ["accounts", "contacts"],
  entitiesSkipped: [],
  relationFieldsAdded: [],
  fieldsSkipped: [],
  layoutsInstalled: ["accounts"],
  rolesCreated: [],
  rolesSkipped: [],
  nnRelationsCreated: [],
  nnRelationsSkipped: [],
  deferredFieldsAdded: [],
  samplesInserted: 3,
  linksCreated: 0,
  dashboardsCreated: [],
  dashboardsSkipped: [],
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listBuiltinVerticals).mockReset().mockResolvedValue(LIVE_BUILTINS);
  vi.mocked(listEntities).mockReset().mockResolvedValue([]);
  vi.mocked(listPackInstalls).mockReset().mockResolvedValue([]);
  vi.mocked(dryRunInstall).mockReset();
  vi.mocked(installPack).mockReset();
  // Default: the operator probe fails FORBIDDEN → tests of unrelated areas see no panel.
  vi.mocked(listTenants).mockReset().mockRejectedValue(new ApiError(403, "FORBIDDEN", "Bạn không có quyền"));
  vi.mocked(provisionTenant).mockReset();
  vi.mocked(retireTenant).mockReset().mockResolvedValue(undefined);
  vi.mocked(purgeTenant).mockReset();
  vi.mocked(listTenantBackups).mockReset().mockResolvedValue({
    data: [], meta: { currentSchemaVersion: 85, maintenanceUntil: null, nextCursor: null },
  });
  vi.mocked(appsCatalog).mockReset().mockResolvedValue([]);
  vi.mocked(enterWorkspace).mockReset().mockResolvedValue(undefined);
  vi.mocked(setOwnerTier).mockReset().mockResolvedValue({ converted: 2, skippedWithRole: 1, ownerRoleId: "r1", revokeFailed: 0 });
  vi.mocked(revertOwnerTier).mockReset().mockResolvedValue({ reverted: 3, revokeFailed: 0 });
  vi.mocked(toast.success).mockClear();
  vi.mocked(listPackUpgrades).mockReset().mockResolvedValue([]);
  vi.mocked(planPackUpgrade).mockReset();
  vi.mocked(toast.error).mockClear();
  clearWorkspace();
});

describe("VerticalLibrary — live manifest rendering (FE-CATCHUP-2)", () => {
  it("CRM card shows v1.1.0 and clinic v1.0.0 straight from the API manifests", async () => {
    renderLib();

    // Deepened CRM's version is finally visible pre-install.
    expect(await screen.findByText("v1.1.0")).toBeInTheDocument();
    expect(screen.getByText("v1.0.0")).toBeInTheDocument();

    // Entity counts come from the API's entityCount, not the static catalog chips.
    expect(screen.getByText(/5 đối tượng ·/)).toBeInTheDocument();
    expect(screen.getByText(/2 đối tượng ·/)).toBeInTheDocument();

    // Both packs are out of the "Lộ trình" roadmap section (they DO install).
    expect(screen.getByText("CRM / Bán hàng")).toBeInTheDocument();
    expect(screen.getByText("Phòng khám")).toBeInTheDocument();
  });

  it("version is NOT hardcoded: a bumped manifest version renders as-is", async () => {
    vi.mocked(listBuiltinVerticals).mockResolvedValue([
      {
        slug: "crm",
        manifest: { name: "CRM", slug: "crm", version: "9.9.9", description: "x", author: "Nexus" },
        entityCount: 7,
      },
    ]);
    renderLib();

    expect(await screen.findByText("v9.9.9")).toBeInTheDocument();
    expect(screen.queryByText("v1.1.0")).not.toBeInTheDocument();
    expect(screen.getByText(/7 đối tượng ·/)).toBeInTheDocument();
  });

  it("a builtin with no static facts entry renders gracefully from the manifest alone", async () => {
    vi.mocked(listBuiltinVerticals).mockResolvedValue([
      {
        slug: "helpdesk",
        manifest: { name: "Helpdesk", slug: "helpdesk", version: "0.3.0", description: "Ticket và SLA cơ bản.", author: "Nexus" },
        entityCount: 3,
      },
    ]);
    renderLib();

    // Name, version, count and the manifest's own description — no crash, nothing invented.
    expect(await screen.findByText("Helpdesk")).toBeInTheDocument();
    expect(screen.getByText("v0.3.0")).toBeInTheDocument();
    expect(screen.getByText(/3 đối tượng ·/)).toBeInTheDocument();
    expect(screen.getByText("Ticket và SLA cơ bản.")).toBeInTheDocument();
  });

  it("ledger-installed version and live pack version render side by side with an upgrade hint", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      { id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" },
    ]);
    renderLib();

    // Installed badge carries the LEDGER version; the title badge carries the LIVE pack version.
    expect(await screen.findByText(/Đã cài v1\.0\.0/)).toBeInTheDocument();
    expect(screen.getByText("v1.1.0")).toBeInTheDocument();
    // Honest upgrade hint since ledger (1.0.0) < shipped (1.1.0).
    expect(screen.getByText(/pack hiện ở v1\.1\.0/)).toBeInTheDocument();
    // The card has no upgrade button any more: the hint must point at the real place, not at a button.
    expect(screen.getByText(/mở Bảo trì > Nâng cấp/)).toBeInTheDocument();
    expect(screen.queryByText(/bấm Nâng cấp/)).toBeNull();
  });

  const EMPTY_PLAN: UpgradePlan = {
    slug: "crm", fromVersions: ["1.0.0"], toVersion: "1.1.0", source: "builtin" as const, untracked: false, delivered: false,
    planHash: "h", summary: {}, warnings: [], items: [],
  };

  // specs/079 f7-nang-cap-ap (T148): the card's temporary "Nâng cấp" is gone; upgrading lives in Bảo trì > Nâng cấp.
  it("installed cards have no 'Nâng cấp' button and read no upgrade plan; only the install button stays", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      { id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" },
    ]);
    vi.mocked(planPackUpgrade).mockResolvedValue(EMPTY_PLAN);
    renderLib();

    await screen.findByText(/Đã cài v1\.0\.0/);
    expect(screen.getAllByRole("button", { name: "Cài lại" })).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Nâng cấp" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Cài đặt" })).toHaveLength(1);
    expect(planPackUpgrade).not.toHaveBeenCalled();
  });

  it("an installed card shows the latest upgrade run", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      { id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" },
    ]);
    vi.mocked(listPackUpgrades).mockResolvedValue([{
      id: "r1", slug: "crm", fromVersions: "1.0.0", toVersion: "1.1.0", source: "builtin", mode: "live", status: "partial",
      decisions: [], results: [], counts: {}, startedAt: "2026-10-14T03:00:00Z",
    }]);
    renderLib();
    expect(await screen.findByText(/Lần nâng cấp gần nhất:.*xong một phần/)).toBeInTheDocument();
    expect(listPackUpgrades).toHaveBeenCalledWith("crm", 1);
  });

  const packFile = (slug: string) => new File(
    [JSON.stringify({ manifest: { name: "Gói thử", slug, version: "2.0.0" }, entities: [{ name: "qa_devices" }] })],
    "pack.json", { type: "application/json" },
  );
  const pickFile = (f: File) => fireEvent.change(document.querySelector("input[type=file]") as HTMLInputElement, { target: { files: [f] } });

  it("importing a file whose pack is already installed opens the upgrade with that file as the source", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([{ id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" }]);
    vi.mocked(planPackUpgrade).mockResolvedValue(EMPTY_PLAN);
    renderLib();
    await screen.findByText(/Đã cài v1\.0\.0/);
    pickFile(packFile("crm"));
    await waitFor(() => expect(planPackUpgrade).toHaveBeenCalled());
    expect(vi.mocked(planPackUpgrade).mock.calls[0][0]).toBe("crm");
    expect(vi.mocked(planPackUpgrade).mock.calls[0][1]).toHaveProperty("pack");
    expect(dryRunInstall).not.toHaveBeenCalled();
  });

  it("importing a file for a pack that is NOT installed still opens the install flow", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([{ id: "i1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" }]);
    vi.mocked(dryRunInstall).mockResolvedValue(CLEAN_PLAN);
    renderLib();
    await screen.findByText(/Đã cài v1\.0\.0/);
    pickFile(packFile("brand-new"));
    await waitFor(() => expect(dryRunInstall).toHaveBeenCalled());
    expect(planPackUpgrade).not.toHaveBeenCalled();
  });
});

describe("VerticalLibrary — inline card install flow (Wave B2 merge)", () => {
  it("'Cài đặt' on a card → dry-run plan inline → confirm dialog → install → result", async () => {
    vi.mocked(listBuiltinVerticals).mockResolvedValue([LIVE_BUILTINS[1]]); // just CRM
    vi.mocked(dryRunInstall).mockResolvedValue(CLEAN_PLAN);
    vi.mocked(installPack).mockResolvedValue(INSTALL_RESULT);
    renderLib();

    // Open the flow from the card CTA — it opens as an SLDS modal over the library.
    fireEvent.click(await screen.findByRole("button", { name: "Cài đặt" }));
    const modal = await screen.findByRole("dialog");
    expect(modal).toHaveClass("slds-modal");
    expect(within(modal).getByText("Cài “CRM / Bán hàng”")).toBeInTheDocument();

    // Dry-run ran for THIS builtin and the clean-plan banner renders.
    await waitFor(() => expect(dryRunInstall).toHaveBeenCalledWith({ builtin: "crm" }));
    expect(await within(modal).findByText("Sẵn sàng cài")).toBeInTheDocument();
    // "accounts" shows in the entities diff AND the layouts section — both from the real plan.
    expect(screen.getAllByText("accounts").length).toBeGreaterThan(0);

    // Footer "Cài đặt" opens the confirm prompt (simple variant — zero conflicts).
    fireEvent.click(screen.getByRole("button", { name: /^Cài đặt$/ }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Cài “CRM / Bán hàng” vào workspace?")).toBeInTheDocument();

    // Confirm → real install → result view with per-component counts.
    fireEvent.click(within(dialog).getByRole("button", { name: "Cài đặt" }));
    await waitFor(() => expect(installPack).toHaveBeenCalledWith({ builtin: "crm" }));
    expect(await screen.findByText("Cài đặt hoàn tất")).toBeInTheDocument();
    expect(screen.getByText("Đối tượng đã tạo")).toBeInTheDocument();
    // Ledger refreshed so the card badge can flip to "Đã cài".
    expect(listPackInstalls).toHaveBeenCalledTimes(2);
  });

  // A pack mints its own sidebar app (vertical_pack appmint.go). The App Launcher and the Studio
  // scope read the catalog ActiveAppProvider caches for 5 minutes — an install that does not
  // invalidate it leaves the new app invisible, and its workspace resolves to "*" (tenant-wide).
  const LMS_APP: AppSummary = {
    id: "app-lms", name: "lms", label: "LMS", icon: null, color: null, isDefault: false, sortOrder: 1,
  };

  async function installCrmFromCard() {
    fireEvent.click(await screen.findByRole("button", { name: "Cài đặt" }));
    await screen.findByText("Sẵn sàng cài");
    fireEvent.click(screen.getByRole("button", { name: /^Cài đặt$/ }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cài đặt" }));
  }

  it("a real install re-reads the sidebar app catalog and marks the admin app list stale", async () => {
    vi.mocked(listBuiltinVerticals).mockResolvedValue([LIVE_BUILTINS[1]]);
    vi.mocked(dryRunInstall).mockResolvedValue(CLEAN_PLAN);
    vi.mocked(installPack).mockResolvedValue(INSTALL_RESULT);
    vi.mocked(appsCatalog).mockResolvedValueOnce([]).mockResolvedValue([LMS_APP]);
    const qc = renderLib(<CatalogProbe />);
    qc.setQueryData(qk.adminApps(), []); // a workspace visited earlier left this cached

    await waitFor(() => expect(appsCatalog).toHaveBeenCalledTimes(1));
    await installCrmFromCard();
    expect(await screen.findByText("Cài đặt hoàn tất")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId("catalog")).toHaveTextContent("lms"));
    expect(appsCatalog).toHaveBeenCalledTimes(2);
    expect(qc.getQueryState(qk.adminApps())?.isInvalidated).toBe(true);
  });

  it("a failed install response still re-reads the catalog (the server may have minted the app)", async () => {
    vi.mocked(listBuiltinVerticals).mockResolvedValue([LIVE_BUILTINS[1]]);
    vi.mocked(dryRunInstall).mockResolvedValue(CLEAN_PLAN);
    vi.mocked(installPack).mockRejectedValue(new ApiError(500, "INTERNAL", "Lỗi máy chủ"));
    renderLib(<CatalogProbe />);

    await waitFor(() => expect(appsCatalog).toHaveBeenCalledTimes(1));
    await installCrmFromCard();
    expect(await screen.findByText(/Lỗi máy chủ/)).toBeInTheDocument();
    await waitFor(() => expect(appsCatalog).toHaveBeenCalledTimes(2));
  });
});

describe("VerticalLibrary — export + history", () => {
  it("export picks entities in the table and downloads the pack with exactly those names", async () => {
    vi.mocked(listEntities).mockResolvedValue([
      { id: "e1", name: "crm_accounts", label: "Khách hàng" },
      { id: "e2", name: "tk_customers", label: "Khách hàng" },
      { id: "e3", name: "crm_contacts", label: "Liên hệ" },
    ] as never);
    vi.mocked(exportVertical).mockResolvedValue({ pack: { manifest: { name: "x" } } as never, warnings: ["Bỏ qua 1 vai"] });
    const createObjectURL = vi.fn(() => "blob:pack");
    const revokeObjectURL = vi.fn();
    Object.assign(URL, { createObjectURL, revokeObjectURL });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    renderLib();

    // Same label, different entities: the technical name tells them apart.
    const row = (name: string) => within(screen.getByText(name).closest("tr")!).getByRole("checkbox");
    await screen.findByText("tk_customers");
    fireEvent.click(row("tk_customers"));
    fireEvent.click(row("crm_contacts"));
    fireEvent.click(screen.getByRole("button", { name: "Xuất (2)" }));

    await waitFor(() => expect(exportVertical).toHaveBeenCalledWith(["tk_customers", "crm_contacts"], false, undefined));
    expect(await screen.findByText("Đã tải về")).toBeInTheDocument();
    expect(click).toHaveBeenCalled();
    expect(screen.getByText("Bỏ qua 1 vai")).toBeInTheDocument();
    click.mockRestore();
  });

  it("history tab lists ledger installs newest-first in a table", async () => {
    vi.mocked(listPackInstalls).mockResolvedValue([
      { id: "i1", slug: "clinic", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z", result: { entitiesCreated: ["a", "b"], samplesInserted: 4 } },
      { id: "i2", slug: "crm", version: "1.1.0", installedAt: "2026-07-05T10:00:00Z" },
    ] as never);
    renderLib();
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "Lịch sử cài đặt" }));

    const table = await screen.findByRole("grid", { name: "Lịch sử cài đặt pack" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(within(rows[0]).getByText("CRM / Bán hàng")).toBeInTheDocument();
    expect(within(rows[1]).getByText("Phòng khám")).toBeInTheDocument();
    expect(within(rows[1]).getByText("2")).toBeInTheDocument();
  });
});

describe("VerticalLibrary — Workspace khách (operator-only panel, ported from DeployScreen)", () => {
  it("non-operator (FORBIDDEN) → panel hidden, honest note shown", async () => {
    vi.mocked(listTenants).mockRejectedValue(new ApiError(403, "FORBIDDEN", "Bạn không có quyền"));
    renderLib();

    await waitFor(() => {
      expect(screen.getByText("Tài khoản này không có quyền điều phối workspace khách.")).toBeInTheDocument();
    });
    expect(screen.queryByText("Workspace khách")).not.toBeInTheDocument();
  });

  it("unknown probe error (not FORBIDDEN) → retry-able note, never claims no-permission", async () => {
    vi.mocked(listTenants).mockRejectedValue(new Error("network down"));
    renderLib();

    await waitFor(() => {
      expect(screen.getByText(/Không kiểm tra được quyền điều phối workspace khách/)).toBeInTheDocument();
    });
    expect(screen.queryByText("Tài khoản này không có quyền điều phối workspace khách.")).not.toBeInTheDocument();
  });

  it("operator (200) → renders the tenant registry honestly (slug, status, usable-admin count)", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus]);
    renderLib();

    await waitFor(() => {
      expect(screen.getByText("Workspace khách")).toBeInTheDocument();
    });
    expect(screen.getByText("northwind")).toBeInTheDocument();
    expect(screen.getByText("1 quản trị viên · 0 chủ khách")).toBeInTheDocument();
  });

  // specs/068 T030: every customer row — a retired one too — opens that customer's backup panel.
  it("'Sao lưu & khôi phục' on a row opens the backup panel of THAT customer (retired ones too)", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus, RETIRED]);
    renderLib();
    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    const buttons = await screen.findAllByRole("button", { name: /^Sao lưu & khôi phục/ });
    expect(buttons).toHaveLength(2);

    fireEvent.click(buttons[1]); // the retired customer's row
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Sao lưu của khách — Khách B")).toBeInTheDocument();
    await waitFor(() => expect(listTenantBackups).toHaveBeenCalledWith("t-9", expect.anything()));
    expect(await within(dialog).findByText("Chưa có bản sao lưu nào")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Sao lưu ngay" })).toBeEnabled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Đóng" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("create with generated password → show-once panel gated by checkbox; secret gone from DOM after Xong", async () => {
    vi.mocked(listTenants).mockResolvedValueOnce([]).mockResolvedValueOnce([Nexus]); // refresh after "Xong"
    const provisioned: ProvisionedTenant = {
      tenant: { id: "t-2", slug: "acme", name: "ACME", status: "active", createdAt: "2026-07-14T00:00:00Z" },
      admin: { id: "u-1", email: "admin@acme.com", isAdmin: false, canManageUsers: true },
      oneTimeCredentials: { password: "S3cret-Pass-xyz" },
    };
    vi.mocked(provisionTenant).mockResolvedValue(provisioned);
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /Tạo workspace mới/ }));

    fireEvent.change(screen.getByLabelText(/Tên workspace/), { target: { value: "ACME" } });
    fireEvent.change(screen.getByLabelText(/Email chủ khách/), { target: { value: "admin@acme.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo workspace" }));

    await waitFor(() => {
      expect(screen.getByTestId("tenant-password")).toHaveTextContent("S3cret-Pass-xyz");
    });
    expect(provisionTenant).toHaveBeenCalledWith(
      expect.objectContaining({ slug: "acme", adminEmail: "admin@acme.com", generatePassword: true }),
    );

    // specs/065 D2 + FR-042: the account is called what it is, and the two handover sentences are said.
    expect(screen.getByText(/Đây là tài khoản chủ khách/)).toBeInTheDocument();
    expect(screen.getByText(/giữ mật khẩu này \(chỉ hiện một lần\)/)).toBeInTheDocument();
    expect(screen.getByText(/ghi nhật ký với tên chủ khách/)).toBeInTheDocument();
    expect(screen.getByText(/dặn khách đổi mật khẩu ở lần đăng nhập đầu/)).toBeInTheDocument();

    // "Xong" stays disabled until the acknowledgement checkbox is checked.
    const doneBtn = screen.getByRole("button", { name: "Xong" });
    expect(doneBtn).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu\/gửi credentials/ }));
    expect(doneBtn).not.toBeDisabled();

    fireEvent.click(doneBtn);
    await waitFor(() => {
      expect(screen.queryByTestId("tenant-password")).not.toBeInTheDocument();
    });
    expect(document.body.textContent).not.toContain("S3cret-Pass-xyz");
  });

  it("TENANT_FIELD_REQUIRED with details.field=slug → highlights the slug field inline, not a form banner", async () => {
    vi.mocked(listTenants).mockResolvedValue([]);
    vi.mocked(provisionTenant).mockRejectedValue(
      new ApiError(422, "TENANT_FIELD_REQUIRED", "thiếu trường bắt buộc \"slug\"", { field: "slug" }),
    );
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /Tạo workspace mới/ }));
    fireEvent.change(screen.getByLabelText(/Tên workspace/), { target: { value: "ACME" } });
    fireEvent.change(screen.getByLabelText(/Email chủ khách/), { target: { value: "admin@acme.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo workspace" }));

    await waitFor(() => {
      expect(screen.getByText("Bắt buộc.")).toBeInTheDocument();
    });
  });
});

// ── Owner tier (specs/065 lát D): convert a workspace's admins to customer owners and back ──
describe("VerticalLibrary — owner tier per workspace", () => {
  const OLD: TenantSummary = { ...Nexus, id: "t-5", slug: "khacha", name: "Khách A", usableAdminCount: 2, usableOwnerCount: 0, convertibleAdminCount: 2 };
  const CONVERTED: TenantSummary = { ...Nexus, id: "t-6", slug: "khachc", name: "Khách C", usableAdminCount: 0, usableOwnerCount: 2, convertibleAdminCount: 0 };
  const HOME: TenantSummary = { ...Nexus, id: "t-1", slug: "northwind", name: "Nexus", usableAdminCount: 1, usableOwnerCount: 0, convertibleAdminCount: 1 };
  const SUSPENDED: TenantSummary = { ...OLD, id: "t-7", slug: "khachd", name: "Khách D", status: "inactive" };

  async function open(list: TenantSummary[]) {
    vi.mocked(listTenants).mockResolvedValue(list);
    renderLib();
    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
  }

  it("shows both counts, and each action only when it has something to act on", async () => {
    await open([OLD, CONVERTED]);
    expect(screen.getByText("2 quản trị viên · 0 chủ khách")).toBeInTheDocument();
    expect(screen.getByText("0 quản trị viên · 2 chủ khách")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /^Chuyển sang chủ khách/ })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: /^Trả về quản trị viên/ })).toHaveLength(1);
  });

  it("hides both actions on the builder's own workspace and on a suspended one", async () => {
    await open([HOME, SUSPENDED]);
    expect(screen.queryByRole("button", { name: /^Chuyển sang chủ khách/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Trả về quản trị viên/ })).toBeNull();
  });

  it("convert: the prompt states the consequence + the exit, then POSTs and toasts the counts", async () => {
    await open([OLD]);
    fireEvent.click(screen.getByRole("button", { name: /^Chuyển sang chủ khách/ }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/không phải ranh giới bảo mật/)).toBeInTheDocument();
    expect(within(dialog).getByText(/2 quản trị viên thành chủ khách/)).toBeInTheDocument();
    expect(within(dialog).getByText(/mật khẩu một lần/)).toBeInTheDocument();
    expect(within(dialog).getByText(/ghi nhật ký với tên chủ khách/)).toBeInTheDocument();
    expect(within(dialog).getByText(/đổi mật khẩu ở lần đăng nhập đầu/)).toBeInTheDocument();
    expect(dialog.textContent).not.toMatch(/link đặt mật khẩu/);
    expect(setOwnerTier).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: /^Chuyển sang chủ khách/ }));
    await waitFor(() => expect(setOwnerTier).toHaveBeenCalledWith("t-5"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã chuyển 2 tài khoản sang chủ khách. Bỏ qua 1 (đã có vai)."));
  });

  it("revert: the prompt says how many owners come back and warns off the leaked-account case", async () => {
    await open([CONVERTED]);
    fireEvent.click(screen.getByRole("button", { name: /^Trả về quản trị viên/ }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/2 chủ khách sẽ thành quản trị viên/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Không dùng khi nghi tài khoản chủ khách bị lộ/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: /^Trả về quản trị viên/ }));
    await waitFor(() => expect(revertOwnerTier).toHaveBeenCalledWith("t-6"));
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã trả 3 chủ khách về quản trị viên."));
  });

  it("a revoke failure is reported, not hidden", async () => {
    vi.mocked(revertOwnerTier).mockResolvedValue({ reverted: 1, revokeFailed: 1 });
    await open([CONVERTED]);
    fireEvent.click(screen.getByRole("button", { name: /^Trả về quản trị viên/ }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: /^Trả về quản trị viên/ }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Chưa đăng xuất hết phiên của 1 tài khoản — kiểm tra lại."));
  });

  it("the retire prompt shows the owner count next to the admin count", async () => {
    await open([CONVERTED]);
    fireEvent.click(screen.getByRole("button", { name: "Xoá" }));
    expect(await screen.findByText("0 quản trị viên và 2 chủ khách mất quyền đăng nhập")).toBeInTheDocument();
  });
});

// ── Tenant purge (the second, irreversible half of the lifecycle) ──
//
// The orphan-endpoint gate matches loosely on path prefix and is blind to HTTP method, so it
// reported "0 orphans" the moment POST /admin/tenants/:id/purge was registered — with no caller
// anywhere. These tests are the guard that gate cannot be: they prove the button exists on the
// right workspace, that opening it COUNTS first, and that only an explicit confirm deletes.
describe("VerticalLibrary — purge a retired workspace", () => {
  const PREVIEW = {
    tenantId: "t-9", slug: "khachb", dryRun: true, totalRows: 1234,
    tables: [{ table: "obj_crm_accounts", rows: 1000 }, { table: "users", rows: 234 }],
  };

  it("a LIVE workspace offers only 'Xoá' (retire) — purge is not reachable in one step", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus]);
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Xoá" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Xoá vĩnh viễn" })).toBeNull();
  });

  it("a RETIRED workspace stays listed (honestly labelled) and offers 'Xoá vĩnh viễn'", async () => {
    vi.mocked(listTenants).mockResolvedValue([RETIRED]);
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    expect(screen.getByText("khachb")).toBeInTheDocument();
    expect(screen.getByText("Đã ngừng")).toBeInTheDocument();
    // It must NOT claim usable admins for a workspace nobody can log into.
    expect(screen.queryByText(/quản trị viên ·/)).toBeNull();
    expect(screen.getByRole("button", { name: "Xoá vĩnh viễn" })).toBeInTheDocument();
  });

  it("opening the dialog DRY-RUNS first and shows the real row count before asking to confirm", async () => {
    vi.mocked(listTenants).mockResolvedValue([RETIRED]);
    vi.mocked(purgeTenant).mockResolvedValue(PREVIEW);
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Xoá vĩnh viễn" }));

    // dryRun=true on the probe: opening a dialog must never delete anything.
    await waitFor(() => expect(purgeTenant).toHaveBeenCalledWith("t-9", "khachb", true));
    expect(await screen.findByText(/1.234 dòng dữ liệu trong 2 bảng/)).toBeInTheDocument();
  });

  it("confirming types the slug and sends dryRun=false — the only path that deletes", async () => {
    vi.mocked(listTenants).mockResolvedValue([RETIRED]);
    vi.mocked(purgeTenant).mockResolvedValue(PREVIEW);
    renderLib();

    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Xoá vĩnh viễn" }));
    await screen.findByText(/1.234 dòng dữ liệu/);

    // The confirm button is LOCKED until the slug is typed exactly — the dialog's own guard.
    // Two buttons share this label (the row's and the dialog's), but the modal marks the page
    // behind it aria-hidden, so the accessible query resolves to the dialog's confirm button.
    const confirm = screen.getByRole("button", { name: "Xoá vĩnh viễn" });
    expect(confirm).toBeDisabled();

    fireEvent.change(screen.getByLabelText("Gõ khachb để xác nhận"), { target: { value: "khachb" } });
    expect(confirm).toBeEnabled();

    vi.mocked(purgeTenant).mockResolvedValue({ ...PREVIEW, dryRun: false });
    fireEvent.click(confirm);

    // dryRun=false reaches the API only here, after an explicit typed confirmation.
    await waitFor(() => {
      expect(vi.mocked(purgeTenant).mock.calls.some((c) => c[2] === false)).toBe(true);
    });
  });
});

// specs/062-customer-workspace L2 (T034): "Mở Studio của khách" builds straight inside the customer.
describe("VerticalLibrary — Mở Studio của khách (builder workspace session)", () => {
  const KHACHA: TenantSummary = {
    id: "t-2", slug: "khacha", name: "Khách A", status: "active",
    createdAt: "2026-07-01T00:00:00Z", usableAdminCount: 1, usableOwnerCount: 0, convertibleAdminCount: 0,
  };
  const SUSPENDED: TenantSummary = { ...KHACHA, id: "t-3", slug: "khachc", name: "Khách C", status: "suspended" };

  it("the button sits on an active, un-retired customer row only — not on the builder's own tenant, a retired or a suspended one", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus, KHACHA, RETIRED, SUSPENDED]);
    renderLib();
    await waitFor(() => expect(screen.getByText("Workspace khách")).toBeInTheDocument());
    const buttons = await screen.findAllByRole("button", { name: /^Mở Studio của khách/ });
    expect(buttons).toHaveLength(1);
    expect(buttons[0]).toHaveAccessibleName("Mở Studio của khách Khách A");
  });

  it("clicking it opens the session for THAT customer's slug", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus, KHACHA]);
    renderLib();
    fireEvent.click(await screen.findByRole("button", { name: "Mở Studio của khách Khách A" }));
    await waitFor(() => expect(enterWorkspace).toHaveBeenCalledWith("khacha"));
  });

  it("a refused open says why in Vietnamese words and re-enables the button (no half state)", async () => {
    vi.mocked(listTenants).mockResolvedValue([Nexus, KHACHA]);
    vi.mocked(enterWorkspace).mockRejectedValue(new ApiError(404, "TENANT_NOT_FOUND", "Không tìm thấy workspace"));
    renderLib();
    const btn = await screen.findByRole("button", { name: "Mở Studio của khách Khách A" });
    fireEvent.click(btn);
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Không tìm thấy khách này."));
    expect(btn).toBeEnabled();
  });

  it("inside a session: the customer area is one reminder (no tenant probe, no registry), 'Cài mẫu' still installs into the current workspace", async () => {
    // a live workspace needs the home login that opened it (D-21) — storage readers resolve the scope through it
    localStorage.setItem("nexus_user", JSON.stringify({ id: "u-1", tenantId: "t-1", email: "a@b.vn" }));
    localStorage.setItem("nexus_token", "HOME");
    setWorkspace({
      accessToken: "WS", expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u-1",
      session: { id: "ws-1", tenantId: "t-2", tenantSlug: "khacha", tenantName: "Khách A", openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" },
    });
    vi.mocked(listBuiltinVerticals).mockResolvedValue([LIVE_BUILTINS[1]]);
    vi.mocked(dryRunInstall).mockResolvedValue(CLEAN_PLAN);
    vi.mocked(installPack).mockResolvedValue(INSTALL_RESULT);
    renderLib();

    expect(await screen.findByText("Về Nexus để quản lý không gian khách.")).toBeInTheDocument();
    expect(listTenants).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /^Mở Studio của khách/ })).not.toBeInTheDocument();

    // The pack card still installs — into the workspace the token is in: no `targetTenant` ever sent.
    fireEvent.click(await screen.findByRole("button", { name: "Cài đặt" }));
    await screen.findByText("Sẵn sàng cài");
    fireEvent.click(screen.getByRole("button", { name: /^Cài đặt$/ }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cài đặt" }));
    await waitFor(() => expect(installPack).toHaveBeenCalledWith({ builtin: "crm" }));
    expect(dryRunInstall).toHaveBeenCalledWith({ builtin: "crm" });
  });
});
