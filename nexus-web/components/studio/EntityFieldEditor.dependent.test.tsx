// Queue #37 (dependent-picklist-b, specs/052 §8): a Nexus engineer declares a Tỉnh → Quận/Huyện pair
// in the Studio field inspector — the controlling field, then which districts each province allows —
// instead of typing PUT /admin/fields by hand, and is warned about configs the engine tolerates but
// that break the form: orphan child values, map keys the parent dropped, the pair at different
// visibility levels for a role.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { FieldMeta } from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: true } }),
}));

const listFields = vi.fn();
const updateField = vi.fn();
const listRoles = vi.fn();
const getFieldAcl = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listEntities: vi.fn().mockResolvedValue([{ id: "e1", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" }]),
  listFields: (...a: unknown[]) => listFields(...a),
  updateField: (...a: unknown[]) => updateField(...a),
  listRoles: (...a: unknown[]) => listRoles(...a),
}));
vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  getFieldAcl: (...a: unknown[]) => getFieldAcl(...a),
}));

import { EntityFieldEditor } from "./EntityFieldEditor";
import { WorkshopScopeProvider } from "@/lib/workshopScope";

const SCOPE = {
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_contacts"]),
  panelHref: (p: string, extra?: Record<string, string>) => `/studio/apps/crm?panel=${p}${extra ? `&${new URLSearchParams(extra)}` : ""}`,
};

const province: FieldMeta = {
  id: "f-prov", name: "province", label: "Tỉnh/TP", fieldType: "picklist", options: { values: ["Hà Nội", "Đà Nẵng"] },
};
const district: FieldMeta = {
  id: "f-dist", name: "district", label: "Quận/Huyện", fieldType: "picklist",
  options: { values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà"] },
};
const tags: FieldMeta = { id: "f-tags", name: "tags", label: "Nhãn", fieldType: "multi_select", options: { values: ["a"] } };

function renderEditor(workshop = false) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui = (
    <QueryClientProvider client={qc}>
      <EntityFieldEditor entityName="crm_contacts" />
    </QueryClientProvider>
  );
  render(workshop ? <WorkshopScopeProvider value={SCOPE}>{ui}</WorkshopScopeProvider> : ui);
}
async function openField(label: string) {
  fireEvent.click(await screen.findByRole("button", { name: label }));
  await screen.findByDisplayValue(label);
  return document.getElementById("nx-field-inspector") as HTMLElement;
}
const saveButton = () => screen.getByRole("button", { name: /^Lưu$/ });
const pick = async (scope: HTMLElement, label: string, option: string) => {
  fireEvent.click(within(scope).getByRole("combobox", { name: label }));
  fireEvent.click(await within(scope).findByRole("option", { name: option }));
};
const allow = (dialog: HTMLElement, ...values: string[]) => {
  for (const v of values) {
    fireEvent.click(within(dialog).getByRole("option", { name: v }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Chuyển mục đang chọn sang Được phép" }));
  }
};

beforeEach(() => {
  vi.clearAllMocks();
  listFields.mockResolvedValue([province, district, tags]);
  updateField.mockImplementation(async (_id: string, p: Partial<FieldMeta>) => ({ ...district, ...p }));
  getFieldAcl.mockResolvedValue([]);
  listRoles.mockResolvedValue([]);
});

describe("EntityFieldEditor — dependent picklist (#37)", () => {
  it("picks the controlling field, maps each province to its districts in a dialog, and saves the pair", async () => {
    renderEditor();
    const panel = await openField("Quận/Huyện");

    // Only other single picklists may control it — never itself, never a multi_select.
    const controlling = within(panel).getByRole("combobox", { name: "Trường điều khiển" });
    fireEvent.click(controlling);
    await within(panel).findByRole("option", { name: "Không phụ thuộc" });
    const list = document.getElementById(controlling.getAttribute("aria-controls")!)!;
    const offered = within(list).getAllByRole("option").map((o) => o.textContent);
    expect(offered).toEqual(["Không phụ thuộc", "Tỉnh/TP"]);
    fireEvent.click(within(panel).getByRole("option", { name: "Tỉnh/TP" }));

    // Nothing mapped yet: the save stays locked and says why — and every value is not listed as
    // an "orphan" (the summary already says 0/2).
    expect(within(panel).getByText("0/2 giá trị của Tỉnh/TP đã có danh sách Quận/Huyện")).toBeInTheDocument();
    expect(within(panel).queryByText(/Không giá trị nào của/)).toBeNull();
    expect(saveButton()).toBeDisabled();
    expect(saveButton()).toHaveAttribute("title", "Chưa có giá trị Tỉnh/TP nào được ánh xạ — mở “Sửa ánh xạ”");

    fireEvent.click(within(panel).getByRole("button", { name: "Sửa ánh xạ" }));
    const dialog = await screen.findByRole("dialog", { name: "Ánh xạ Tỉnh/TP → Quận/Huyện" });
    await pick(dialog, "Giá trị của Tỉnh/TP", "Hà Nội");
    allow(dialog, "Ba Đình", "Hoàn Kiếm");
    await pick(dialog, "Giá trị của Tỉnh/TP", "Đà Nẵng");
    allow(dialog, "Hải Châu");
    fireEvent.click(within(dialog).getByRole("button", { name: "Áp dụng" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Ánh xạ/ })).toBeNull());

    // Sơn Trà is allowed under no province: nobody could ever pick it.
    expect(within(panel).getByText(/Không giá trị nào của Tỉnh\/TP cho phép: Sơn Trà/)).toBeInTheDocument();
    expect(within(panel).getByText("2/2 giá trị của Tỉnh/TP đã có danh sách Quận/Huyện")).toBeInTheDocument();

    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledTimes(1));
    expect(updateField).toHaveBeenCalledWith("f-dist", {
      options: {
        values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà"],
        controllingField: "province",
        dependentValues: { "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu"] },
      },
    });
  });

  it("Huỷ in the dialog keeps the map as it was", async () => {
    listFields.mockResolvedValue([province, {
      ...district, options: { ...district.options, controllingField: "province", dependentValues: { "Hà Nội": ["Ba Đình"] } },
    }]);
    renderEditor();
    const panel = await openField("Quận/Huyện");
    fireEvent.click(within(panel).getByRole("button", { name: "Sửa ánh xạ" }));
    const dialog = await screen.findByRole("dialog", { name: "Ánh xạ Tỉnh/TP → Quận/Huyện" });
    await pick(dialog, "Giá trị của Tỉnh/TP", "Hà Nội");
    allow(dialog, "Hoàn Kiếm");
    fireEvent.click(within(dialog).getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: /Ánh xạ/ })).toBeNull());
    expect(saveButton()).toBeDisabled(); // nothing changed
  });

  it("warns about a dropped parent value (and removes it), a different visibility per role", async () => {
    listFields.mockResolvedValue([province, {
      ...district,
      defaultValue: "Ba Đình",
      options: {
        ...district.options,
        controllingField: "province",
        dependentValues: { "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu", "Sơn Trà"], "Cần Thơ": ["Sơn Trà"] },
      },
    }]);
    getFieldAcl.mockResolvedValue([
      { roleId: "r-sales", entityName: "crm_contacts", fieldName: "district", canView: true, canEdit: false },
    ]);
    listRoles.mockResolvedValue([{ id: "r-sales", name: "crm_sales_rep", label: "Nhân viên bán hàng" }]);
    renderEditor();
    const panel = await openField("Quận/Huyện");

    expect(await within(panel).findByText(/khác mức hiển thị với vai: Nhân viên bán hàng/)).toBeInTheDocument();
    expect(getFieldAcl).toHaveBeenCalledWith("crm_contacts");

    const orphan = within(panel).getByText(/không còn trong Tỉnh\/TP: Cần Thơ/).closest(".slds-scoped-notification") as HTMLElement;
    fireEvent.click(within(orphan).getByRole("button", { name: "Gỡ" }));
    await waitFor(() => expect(within(panel).queryByText(/Cần Thơ/)).toBeNull());

    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledTimes(1));
    expect(updateField.mock.calls[0][1].options.dependentValues).toEqual({
      "Hà Nội": ["Ba Đình", "Hoàn Kiếm"], "Đà Nẵng": ["Hải Châu", "Sơn Trà"],
    });
  });

  // Queue #302: the field-ACL links stay inside the app's workshop (inspector + visibility warning).
  // specs/073 kh3: the page exists now, so outside a workshop the links stay in the project frame (same tab, no /admin/field-acl).
  it("outside a workshop both field-ACL links open the project's field-permission page", async () => {
    listFields.mockResolvedValue([province, { ...district, options: { ...district.options, controllingField: "province", dependentValues: { "Hà Nội": ["Ba Đình"] } } }, tags]);
    getFieldAcl.mockResolvedValue([
      { roleId: "r-sales", entityName: "crm_contacts", fieldName: "district", canView: true, canEdit: false },
    ]);
    listRoles.mockResolvedValue([{ id: "r-sales", name: "crm_sales_rep", label: "Nhân viên bán hàng" }]);
    renderEditor(false);
    const panel = await openField("Quận/Huyện");
    await within(panel).findByText(/khác mức hiển thị với vai: Nhân viên bán hàng/);
    const links = within(panel).getAllByRole("link", { name: "Phân quyền trường này" });
    expect(links.length).toBeGreaterThanOrEqual(2);
    for (const a of links) {
      expect(a).toHaveAttribute("href", "/studio/du-an/northwind/dung/nguoi-dung-quyen/quyen-truong?bang=crm_contacts&truong=district");
    }
  });

  it("inside a workshop both field-ACL links open the workshop panel, outside they open /admin/field-acl", async () => {
    listFields.mockResolvedValue([province, { ...district, options: { ...district.options, controllingField: "province", dependentValues: { "Hà Nội": ["Ba Đình"] } } }, tags]);
    getFieldAcl.mockResolvedValue([
      { roleId: "r-sales", entityName: "crm_contacts", fieldName: "district", canView: true, canEdit: false },
    ]);
    listRoles.mockResolvedValue([{ id: "r-sales", name: "crm_sales_rep", label: "Nhân viên bán hàng" }]);
    renderEditor(true);
    const panel = await openField("Quận/Huyện");
    await within(panel).findByText(/khác mức hiển thị với vai: Nhân viên bán hàng/);
    const links = within(panel).getAllByRole("link", { name: "Phân quyền trường này" });
    expect(links.length).toBeGreaterThanOrEqual(2);
    for (const a of links) {
      expect(a).toHaveAttribute("href", "/studio/apps/crm?panel=quyen-truong&entity=crm_contacts&field=district");
    }
  });

  it("'Không phụ thuộc' turns the pair off; multi_select fields get no dependency section", async () => {
    listFields.mockResolvedValue([province, {
      ...district, options: { ...district.options, controllingField: "province", dependentValues: { "Hà Nội": ["Ba Đình"] } },
    }, tags]);
    renderEditor();
    let panel = await openField("Quận/Huyện");
    await pick(panel, "Trường điều khiển", "Không phụ thuộc");
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateField).toHaveBeenCalledWith("f-dist", {
      options: { values: ["Ba Đình", "Hoàn Kiếm", "Hải Châu", "Sơn Trà"] },
    }));

    panel = await openField("Nhãn");
    expect(within(panel).queryByRole("combobox", { name: "Trường điều khiển" })).toBeNull();
  });
});
