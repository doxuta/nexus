// HandoverWizard ("Bàn giao app này", EASE Nước 4) — the spec-§6 FE contracts:
//   1. Step Xuất preselects the app's nav entities and gates "Tiếp tục" on a REAL export whose
//      pack stays in state (no forced download).
//   2. Non-operator (GET /admin/tenants → FORBIDDEN) gets the honest escalation panel that NAMES
//      the need (operator Nexus) — never a one-line dead-end; "Tiếp tục" stays locked.
//   3. Operator path: pick a target → dry-run runs against the TARGET (installPack with
//      {dryRun, targetTenant}) → PlanView renders → apply installs with targetTenant and the
//      result names the TARGET workspace (no "workspace hiện tại" lie, no "Xem như khách" link).
//   4. Graceful degrade: a bare 400/404 from the targetTenant param renders the honest
//      "Backend chưa hỗ trợ cài chéo workspace" note (feature-detect, no version sniffing).
//   5. Creating a workspace inside the wizard reuses the show-once TenantCredentialsReveal and
//      auto-selects the new tenant as the target after the ack gate.
// lib/api is mocked at the module seam (same pattern as VerticalLibrary.test.tsx); the shared
// workspaceShared components, lib/handoverWizard gating and the JSX are all real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HandoverWizard } from "./HandoverWizard";
import { qk } from "@/lib/query/keys";
import {
  listEntities, listTenants, exportVertical, entityDependencies, installPack, provisionTenant, ApiError,
  type AdminApp, type TenantSummary, type ProvisionedTenant, type EntityMeta,
} from "@/lib/api";
import type { InstallPlan, InstallResult, Pack } from "@/lib/verticalPlan";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, // keeps ApiError etc. real
    listEntities: vi.fn(),
    listTenants: vi.fn(),
    exportVertical: vi.fn(),
    entityDependencies: vi.fn(),
    installPack: vi.fn(),
    provisionTenant: vi.fn(),
  };
});

const APP: AdminApp = {
  id: "app-1", name: "phong_kham", label: "Phòng khám", icon: "stethoscope", color: null,
  isDefault: false, sortOrder: 0, description: "App phòng khám", navCount: 2,
};

const ENTITIES = [
  { id: "e1", name: "patients", label: "Bệnh nhân" },
  { id: "e2", name: "visits", label: "Lượt khám" },
  { id: "e3", name: "invoices", label: "Hoá đơn" },
] as EntityMeta[];

const NAV = ["patients", "visits"];

const PACK: Pack = {
  manifest: { name: "Phòng khám", slug: "phong_kham", version: "1.0.0" },
  entities: [{ name: "patients" }, { name: "visits" }],
};

const ACME: TenantSummary = {
  id: "t-2", slug: "acme", name: "ACME", status: "active",
  createdAt: "2026-07-20T00:00:00Z", usableAdminCount: 1, usableOwnerCount: 2, convertibleAdminCount: 1,
};

const CLEAN_PLAN: InstallPlan = {
  entitiesToCreate: ["patients", "visits"],
  entitiesToSkip: [],
  fieldsToAdd: [],
  nnToRegister: [],
  nnToSkip: [],
  layoutsToInstall: ["patients"],
  rolesToCreate: [],
  rolesToSkip: [],
  samplesCount: 0,
  conflicts: [],
};

const RESULT: InstallResult = {
  entitiesCreated: ["patients", "visits"],
  entitiesSkipped: [],
  relationFieldsAdded: [],
  fieldsSkipped: [],
  layoutsInstalled: ["patients"],
  rolesCreated: [],
  rolesSkipped: [],
  nnRelationsCreated: [],
  nnRelationsSkipped: [],
  deferredFieldsAdded: [],
  samplesInserted: 0,
  linksCreated: 0,
  dashboardsCreated: [],
  dashboardsSkipped: [],
};

beforeEach(() => {
  vi.mocked(listEntities).mockReset().mockResolvedValue(ENTITIES);
  vi.mocked(listTenants).mockReset().mockResolvedValue([ACME]);
  vi.mocked(exportVertical).mockReset().mockResolvedValue({ pack: PACK, warnings: [] });
  vi.mocked(entityDependencies).mockReset().mockResolvedValue([]);
  vi.mocked(installPack).mockReset();
  vi.mocked(provisionTenant).mockReset();
});

let qc: QueryClient;
function renderWizard(onOpenChange = vi.fn()) {
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <HandoverWizard app={APP} navEntityNames={NAV} onOpenChange={onOpenChange} />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

/** The select checkbox of an entity's row in the export table. */
function entityBox(label: string) {
  return within(screen.getByText(label).closest("tr")!).getByRole("checkbox");
}

/** Drive step 1: wait for the entity table, export, then continue to the workspace step. */
async function exportAndContinue() {
  fireEvent.click(await screen.findByRole("button", { name: /Xuất pack \(2\)/ }));
  await screen.findByText(/Pack sẵn sàng/);
  fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ }));
}

describe("HandoverWizard — bước Xuất", () => {
  it("pre-ticks a table outside the menu that a field needs, and says why (#296)", async () => {
    vi.mocked(entityDependencies).mockResolvedValue([
      { name: "invoices", label: "Hoá đơn", reason: "trường \"Tổng tiền\" của bảng \"Lượt khám\" cần bảng này" },
    ]);
    renderWizard();
    await screen.findByText("Bệnh nhân");
    await waitFor(() => expect(entityBox("Hoá đơn")).toBeChecked());
    expect(entityDependencies).toHaveBeenCalledWith(NAV);
    expect(screen.getByText(/Tổng tiền/)).toBeInTheDocument();
  });

  it("preselects the app's nav entities and keeps the exported pack in state (no forced file)", async () => {
    renderWizard();

    // Table rows render from the live entity list; nav entities come preselected (2), the third not.
    await screen.findByText("Bệnh nhân");
    expect(entityBox("Bệnh nhân")).toBeChecked();
    expect(entityBox("Lượt khám")).toBeChecked();
    expect(entityBox("Hoá đơn")).not.toBeChecked();
    const exportBtn = screen.getByRole("button", { name: /Xuất pack \(2\)/ });

    // "Tiếp tục" locked until an export produced a pack.
    expect(screen.getByRole("button", { name: /Tiếp tục/ })).toBeDisabled();

    fireEvent.click(exportBtn);
    await screen.findByText(/Pack sẵn sàng — giữ trong wizard/);

    // The export carried the app identity + exactly the preselected nav entities.
    expect(exportVertical).toHaveBeenCalledWith(
      ["patients", "visits"], false,
      expect.objectContaining({ slug: "phong_kham", name: "Phòng khám" }),
    );
    // Optional archive copy offered, never required.
    expect(screen.getByRole("button", { name: /Tải bản sao/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tiếp tục/ })).not.toBeDisabled();
  });

  it("editing the chip selection invalidates the exported pack (state can never lie)", async () => {
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Xuất pack \(2\)/ }));
    await screen.findByText(/Pack sẵn sàng/);

    // Deselect one entity → pack gone, gate re-locks.
    fireEvent.click(entityBox("Lượt khám"));
    expect(screen.queryByText(/Pack sẵn sàng/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tiếp tục/ })).toBeDisabled();
  });
});

describe("HandoverWizard — bước Workspace", () => {
  it("non-operator (FORBIDDEN) → honest escalation naming the operator need, next stays locked", async () => {
    vi.mocked(listTenants).mockRejectedValue(new ApiError(403, "FORBIDDEN", "Bạn không có quyền"));
    renderWizard();
    await exportAndContinue();

    // Names the need — not a one-line refusal — and offers the pack copy + a re-probe.
    expect(await screen.findByText(/cần quyền điều phối \(operator Nexus\)/)).toBeInTheDocument();
    expect(screen.getByText(/Nhờ operator Nexus/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tải bản sao pack cho operator/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Kiểm tra lại quyền/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tiếp tục/ })).toBeDisabled();
  });

  it("probe network error → retry-able note that never claims no-permission", async () => {
    vi.mocked(listTenants).mockRejectedValue(new Error("network down"));
    renderWizard();
    await exportAndContinue();

    expect(await screen.findByText(/Không kiểm tra được quyền điều phối/)).toBeInTheDocument();
    expect(screen.queryByText(/cần quyền điều phối/)).not.toBeInTheDocument();
  });

  it("creating a workspace reuses the show-once reveal and auto-selects it as target", async () => {
    const provisioned: ProvisionedTenant = {
      tenant: { id: "t-9", slug: "hn_clinic", name: "HN Clinic", status: "active", createdAt: "2026-07-25T00:00:00Z" },
      admin: { id: "u-9", email: "admin@hnclinic.vn", isAdmin: true },
      oneTimeCredentials: { password: "S3cret-Pass-xyz" },
    };
    vi.mocked(provisionTenant).mockResolvedValue(provisioned);
    renderWizard();
    await exportAndContinue();

    fireEvent.click(await screen.findByRole("button", { name: /Tạo workspace mới/ }));
    fireEvent.change(screen.getByLabelText(/Tên workspace/), { target: { value: "HN Clinic" } });
    fireEvent.change(screen.getByLabelText(/Email chủ khách/), { target: { value: "admin@hnclinic.vn" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo workspace" }));

    // Show-once reveal (reused component) — gated by the acknowledgement checkbox.
    await waitFor(() => expect(screen.getByTestId("tenant-password")).toHaveTextContent("S3cret-Pass-xyz"));
    const done = screen.getByRole("button", { name: "Xong" });
    expect(done).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu\/gửi credentials/ }));
    fireEvent.click(done);

    // Back on the workspace step with the fresh tenant selected → next unlocked.
    await waitFor(() => expect(screen.getByRole("button", { name: /Tiếp tục/ })).not.toBeDisabled());
  });
});

describe("HandoverWizard — workspace picker (specs/065)", () => {
  it("the account column shows BOTH counts: quản trị viên · chủ khách", async () => {
    renderWizard();
    await exportAndContinue();
    expect(await screen.findByText("Quản trị viên · chủ khách")).toBeInTheDocument();
    expect(screen.getByText("1 · 2")).toBeInTheDocument();
  });
});

describe("HandoverWizard — Plan (dry-run trên ĐÍCH) & Apply", () => {
  async function toPlanStep() {
    renderWizard();
    await exportAndContinue();
    // Pick ACME as target, continue to the plan step.
    fireEvent.click(await screen.findByRole("radio"));
    fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ }));
  }

  it("dry-run hits installPack with {dryRun, targetTenant} and renders the PlanView", async () => {
    vi.mocked(installPack).mockResolvedValue(CLEAN_PLAN as unknown as InstallResult);
    await toPlanStep();

    await waitFor(() =>
      expect(installPack).toHaveBeenCalledWith({ pack: PACK }, { dryRun: true, targetTenant: "acme" }));
    expect(await screen.findByText("Sẵn sàng cài")).toBeInTheDocument();
    expect(screen.getByText(/Dry-run trên workspace/)).toBeInTheDocument();
  });

  it("bare 400 from the param → honest 'Backend chưa hỗ trợ cài chéo workspace' note", async () => {
    vi.mocked(installPack).mockRejectedValue(new ApiError(400, "UNKNOWN", "bad request"));
    await toPlanStep();

    expect(await screen.findByText("Backend chưa hỗ trợ cài chéo workspace")).toBeInTheDocument();
    expect(screen.getByText(/cập nhật server/)).toBeInTheDocument();
    // Blocked — cannot advance to apply.
    expect(screen.getByRole("button", { name: /Tiếp tục/ })).toBeDisabled();
  });

  it("apply installs with targetTenant and the result names the TARGET workspace", async () => {
    // 1st call = dry-run (plan), 2nd = real install (result).
    vi.mocked(installPack)
      .mockResolvedValueOnce(CLEAN_PLAN as unknown as InstallResult)
      .mockResolvedValueOnce(RESULT);
    await toPlanStep();
    await screen.findByText("Sẵn sàng cài");

    fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ })); // → apply step
    expect(await screen.findByText("Sẵn sàng bàn giao")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Cài vào “acme”/ }));
    // The confirm is an SLDS prompt (alertdialog) stacked over the wizard modal.
    const dialog = await screen.findByRole("alertdialog");
    // Clean plan → simple confirm naming BOTH the app and the target workspace.
    expect(within(dialog).getByText(/Cài “Phòng khám” vào workspace “acme”\?/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cài đặt" }));

    await waitFor(() =>
      expect(installPack).toHaveBeenLastCalledWith({ pack: PACK }, { targetTenant: "acme" }));
    expect(await screen.findByText("Cài đặt hoàn tất")).toBeInTheDocument();
    // Honest target copy — never the same-tenant line, never a same-tenant "Xem như khách" link.
    expect(screen.getByText(/Pack đã được cài vào workspace “acme”\./)).toBeInTheDocument();
    expect(screen.queryByText(/Pack đã được cài vào workspace hiện tại/)).not.toBeInTheDocument();
    expect(screen.queryByText("Xem như khách")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Mở workspace khách/ })).toBeInTheDocument();
  });

  it("a real apply re-reads the app catalogs (the target can be THIS workspace); the dry-run does not", async () => {
    vi.mocked(installPack)
      .mockResolvedValueOnce(CLEAN_PLAN as unknown as InstallResult)
      .mockResolvedValueOnce(RESULT);
    await toPlanStep();
    qc.setQueryData(qk.apps(), []);
    qc.setQueryData(qk.adminApps(), []);
    await screen.findByText("Sẵn sàng cài");
    expect(qc.getQueryState(qk.apps())?.isInvalidated).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ }));
    fireEvent.click(await screen.findByRole("button", { name: /Cài vào “acme”/ }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cài đặt" }));

    expect(await screen.findByText("Cài đặt hoàn tất")).toBeInTheDocument();
    expect(qc.getQueryState(qk.apps())?.isInvalidated).toBe(true);
    expect(qc.getQueryState(qk.adminApps())?.isInvalidated).toBe(true);
  });
});

describe("HandoverWizard — SLDS modal + progress indicator", () => {
  it("renders as an SLDS modal whose progress indicator follows the step", async () => {
    renderWizard();
    const modal = screen.getByRole("dialog");
    expect(modal).toHaveClass("slds-modal");
    expect(within(modal).getByText("Bước 1/4 · Xuất pack")).toBeInTheDocument();
    expect(modal.querySelectorAll(".slds-progress__item")).toHaveLength(4);
    expect(modal.querySelector(".slds-progress__item.slds-is-active")).not.toBeNull();

    await exportAndContinue();
    expect(await within(modal).findByText("Bước 2/4 · Workspace khách")).toBeInTheDocument();
    expect(modal.querySelectorAll(".slds-progress__item.slds-is-completed")).toHaveLength(1);
  });

  // RỔ-4 P1 on the workspace step: while the show-once reveal waits for its ack, "Quay lại" must
  // not lead to step 1, whose footer "Đóng" would unmount the wizard (and the only copy of the
  // password) past the close lock.
  it("un-acknowledged show-once reveal: Quay lại is locked and nothing closes the wizard until the ack", async () => {
    const provisioned: ProvisionedTenant = {
      tenant: { id: "t-9", slug: "hn_clinic", name: "HN Clinic", status: "active", createdAt: "2026-07-25T00:00:00Z" },
      admin: { id: "u-9", email: "admin@hnclinic.vn", isAdmin: true },
      oneTimeCredentials: { password: "S3cret-Pass-xyz" },
    };
    vi.mocked(provisionTenant).mockResolvedValue(provisioned);
    const onOpenChange = renderWizard();
    await exportAndContinue();

    fireEvent.click(await screen.findByRole("button", { name: /Tạo workspace mới/ }));
    fireEvent.change(screen.getByLabelText(/Tên workspace/), { target: { value: "HN Clinic" } });
    fireEvent.change(screen.getByLabelText(/Email chủ khách/), { target: { value: "admin@hnclinic.vn" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo workspace" }));
    await screen.findByTestId("tenant-password");

    const modal = screen.getByRole("dialog");
    const back = within(modal).getByRole("button", { name: "Quay lại" });
    expect(back).toBeDisabled();
    fireEvent.click(back);
    fireEvent.click(within(modal).getByRole("button", { name: "Huỷ và đóng" }));
    fireEvent.keyDown(modal, { key: "Escape" });
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByText("Bước 2/4 · Workspace khách")).toBeInTheDocument();
    expect(screen.getByTestId("tenant-password")).toHaveTextContent("S3cret-Pass-xyz");

    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu\/gửi credentials/ }));
    fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    await waitFor(() => expect(within(modal).getByRole("button", { name: "Quay lại" })).toBeEnabled());
  });

  // RỔ-4 P1: the plaintext password of a JUST-created workspace lives only in wizard memory.
  // Until the operator acknowledges saving it, NO path closes the wizard — not "Đóng", not the
  // modal's close button, not Esc.
  it("result screen with un-acknowledged fresh credentials: nothing closes the wizard until the ack", async () => {
    const provisioned: ProvisionedTenant = {
      tenant: { id: "t-9", slug: "hn_clinic", name: "HN Clinic", status: "active", createdAt: "2026-07-25T00:00:00Z" },
      admin: { id: "u-9", email: "admin@hnclinic.vn", isAdmin: true },
      oneTimeCredentials: { password: "S3cret-Pass-xyz" },
    };
    vi.mocked(provisionTenant).mockResolvedValue(provisioned);
    vi.mocked(installPack)
      .mockResolvedValueOnce(CLEAN_PLAN as unknown as InstallResult)
      .mockResolvedValueOnce(RESULT);
    const onOpenChange = renderWizard();
    await exportAndContinue();

    fireEvent.click(await screen.findByRole("button", { name: /Tạo workspace mới/ }));
    fireEvent.change(screen.getByLabelText(/Tên workspace/), { target: { value: "HN Clinic" } });
    fireEvent.change(screen.getByLabelText(/Email chủ khách/), { target: { value: "admin@hnclinic.vn" } });
    fireEvent.click(screen.getByRole("button", { name: "Tạo workspace" }));
    await screen.findByTestId("tenant-password");
    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu\/gửi credentials/ }));
    fireEvent.click(screen.getByRole("button", { name: "Xong" }));

    await waitFor(() => expect(screen.getByRole("button", { name: /Tiếp tục/ })).not.toBeDisabled());
    fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ })); // → plan
    await screen.findByText("Sẵn sàng cài");
    fireEvent.click(screen.getByRole("button", { name: /Tiếp tục/ })); // → apply
    fireEvent.click(await screen.findByRole("button", { name: /Cài vào “hn_clinic”/ }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cài đặt" }));
    expect(await screen.findByText("Cài đặt hoàn tất")).toBeInTheDocument();

    // The recap repeats the password; closing is locked on every path.
    expect(screen.getByText("S3cret-Pass-xyz")).toBeInTheDocument();
    const modal = screen.getByRole("dialog");
    const close = within(modal).getByRole("button", { name: "Đóng" });
    expect(close).toBeDisabled();
    fireEvent.click(within(modal).getByRole("button", { name: "Huỷ và đóng" }));
    fireEvent.keyDown(modal, { key: "Escape" });
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu khối bàn giao này/ }));
    expect(close).toBeEnabled();
    fireEvent.click(close);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
