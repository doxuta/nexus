// AppsManager (/studio/apps) + NavEditor (?panel=menu) on SLDS — specs/038 T039.
// The list is a data-tables grid with row actions; the nav editor is the dueling-picklist blueprint
// (left: what can be added, right: the app's menu in display order) with the blueprint's keyboard
// model (Space grabs/drops, arrows move, Cmd/Ctrl+arrow crosses lists) and pointer drag via dnd-kit.
// Behaviour pinned here is the old screen's: save = replace-all PUT in array order, label override
// trimmed and omitted when blank, save disabled until something changed, delete never touches data.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AppsManager, NavEditor } from "./AppsManager";
import {
  ApiError, adminApps, adminApp, createApp, getPublishState, listDashboards, replaceAppNav, updateApp, deleteApp,
  type AdminApp, type PublishState, type AdminAppDetail, type EntityMeta,
} from "@/lib/api";
import { toast } from "@/components/ui/Toaster";
import { onDraftsChanged } from "@/lib/draftEvents";
import { mkChange, WithPreview } from "@/test/previewProbe";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    adminApps: vi.fn(),
    adminApp: vi.fn(),
    listDashboards: vi.fn(),
    replaceAppNav: vi.fn(),
    updateApp: vi.fn(),
    deleteApp: vi.fn(),
    createApp: vi.fn(),
    getPublishState: vi.fn(),
  };
});

const ENTITIES: EntityMeta[] = [
  { id: "e-acc", name: "crm_accounts", label: "Khách hàng", tableName: "obj_crm_accounts" },
  { id: "e-con", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" },
  { id: "e-ctr", name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" },
];
// Extra entities per test (e.g. two tables sharing a label); reset in beforeEach.
let extraEntities: EntityMeta[] = [];
const useEntitiesSpy = vi.fn();
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: (o?: unknown) => { useEntitiesSpy(o); return { data: [...ENTITIES, ...extraEntities], isLoading: false }; },
}));

// The create/edit dialog has its own surface (ActiveAppProvider, createApp…); stub it at the seam.
vi.mock("@/components/apps/CreateAppDialog", () => ({
  CreateAppDialog: ({ open, app, onCreated }: { open: boolean; app?: AdminApp | null; onCreated?: unknown }) =>
    open ? <div data-testid={app ? `edit-dialog-${app.name}` : "create-dialog"} data-stays={onCreated ? "yes" : "no"} /> : null,
}));

// Path under test: inside a Studio project the create dialog must stay on the page (specs/073 kh2).
let mockPath = "/studio/apps";
vi.mock("next/navigation", async (importOriginal) => {
  const actual = await importOriginal<typeof import("next/navigation")>();
  return { ...actual, usePathname: () => mockPath, useRouter: () => ({ replace: vi.fn() }), useSearchParams: () => new URLSearchParams() };
});

// useDraftView (delivered-workspace signal) reads the builder flag from the auth context.
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { isAdmin: true } }) }));

vi.mock("@/components/ui/Toaster", () => ({
  toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn() },
}));

const CRM: AdminApp = {
  id: "app-crm", name: "crm", label: "CRM", icon: "briefcase", color: "ac",
  isDefault: true, sortOrder: 0, description: null, navCount: 2,
};
const LMS: AdminApp = {
  id: "app-lms", name: "lms", label: "LMS", icon: null, color: "cy",
  isDefault: false, sortOrder: 1, description: "Học vụ", navCount: 0,
};
const DETAIL: AdminAppDetail = {
  app: CRM,
  items: [
    { id: "n1", itemType: "entity", refId: "e-acc", label: null, sortOrder: 0 },
    { id: "n2", itemType: "entity", refId: "e-con", label: "Đầu mối", sortOrder: 1 },
  ],
};

function wrap(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  extraEntities = [];
  useEntitiesSpy.mockClear();
  mockPath = "/studio/apps";
  vi.mocked(adminApps).mockReset().mockResolvedValue([CRM, LMS]);
  vi.mocked(adminApp).mockReset().mockResolvedValue(DETAIL);
  vi.mocked(listDashboards).mockReset().mockResolvedValue([
    { id: "d1", name: "Doanh số", description: null, widgets: [] },
  ]);
  vi.mocked(replaceAppNav).mockReset().mockResolvedValue([]);
  vi.mocked(updateApp).mockReset().mockResolvedValue(LMS);
  vi.mocked(deleteApp).mockReset().mockResolvedValue(undefined);
  vi.mocked(createApp).mockReset().mockResolvedValue({ ...LMS, id: "app-new", name: "moi", label: "Mới" });
  vi.mocked(getPublishState).mockReset().mockRejectedValue(new Error("not the builder / not delivered"));
  vi.mocked(toast.success).mockReset();
  vi.mocked(toast.error).mockReset();
});

// The option's own label — the first text of its truncate span; the slug and "(gốc: …)" that follow
// are secondary text.
const optionTexts = (list: HTMLElement) =>
  within(list).queryAllByRole("option").map((o) => o.querySelector(".slds-truncate")?.firstChild?.textContent);

async function renderEditor(onBack = vi.fn(), onSaved = vi.fn().mockResolvedValue(undefined)) {
  wrap(<NavEditor app={CRM} onBack={onBack} onSaved={onSaved} />);
  const menu = await screen.findByRole("listbox", { name: "Menu của app (2)" });
  const available = screen.getByRole("listbox", { name: "Có sẵn" });
  return { menu, available, onBack, onSaved };
}

describe("NavEditor — dueling-picklist blueprint", () => {
  it("renders the blueprint: two listboxes, menu in nav order with label overrides, available excludes what is in the menu", async () => {
    const { menu, available } = await renderEditor();
    expect(menu.closest(".slds-dueling-list")).not.toBeNull();
    expect(menu).toHaveClass("slds-listbox", "slds-listbox_vertical");
    expect(menu.closest(".slds-dueling-list__options")).not.toBeNull();
    expect(screen.getByRole("group", { name: "Chọn bảng cho menu" })).toBeInTheDocument();

    expect(optionTexts(menu)).toEqual(["Khách hàng", "Đầu mối"]);
    // The override shows its original name alongside (never loses the real entity).
    expect(within(menu).getByRole("option", { name: "Đầu mối (gốc: Liên hệ) crm_contacts" })).toBeInTheDocument();

    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    expect(within(available).getByText("Bảng dữ liệu")).toBeInTheDocument();
    expect(within(available).getByText("Bảng điều khiển")).toBeInTheDocument();
    // Nothing changed yet → nothing to save.
    expect(screen.getByRole("button", { name: "Lưu menu" })).toBeDisabled();
  });

  it("select + Thêm vào menu moves the item to the end of the menu; Bỏ khỏi menu moves it back", async () => {
    const { menu, available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toContain("Doanh số"));
    // Blueprint: the move buttons are never disabled — with nothing selected they do nothing.
    const addBtn = screen.getByRole("button", { name: "Thêm vào menu" });
    const removeBtn = screen.getByRole("button", { name: "Bỏ khỏi menu" });
    expect(addBtn).toBeEnabled();
    expect(removeBtn).toBeEnabled();

    const dash = within(available).getByRole("option", { name: /Doanh số/ });
    await userEvent.click(dash);
    expect(dash).toHaveAttribute("aria-selected", "true");
    expect(dash).toHaveClass("slds-is-selected");
    await userEvent.click(addBtn);

    const menuNow = screen.getByRole("listbox", { name: "Menu của app (3)" });
    expect(optionTexts(menuNow)).toEqual(["Khách hàng", "Đầu mối", "Doanh số"]);
    expect(optionTexts(available)).toEqual(["Hợp đồng"]);
    expect(document.querySelector('[aria-live="assertive"]')).toHaveTextContent("Đã thêm Doanh số vào menu.");
    // Blueprint docs: "With the move button: the items are unselected… focus should remain on the
    // move button."
    expect(within(menuNow).getByRole("option", { name: /Doanh số/ })).toHaveAttribute("aria-selected", "false");
    expect(addBtn).toHaveFocus();
    expect(addBtn).toBeEnabled();

    await userEvent.click(within(menuNow).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(removeBtn);
    expect(optionTexts(screen.getByRole("listbox", { name: "Menu của app (2)" }))).toEqual(["Đầu mối", "Doanh số"]);
    expect(optionTexts(available)).toContain("Khách hàng");
    expect(within(available).getByRole("option", { name: /Khách hàng/ })).toHaveAttribute("aria-selected", "false");
    expect(removeBtn).toHaveFocus();
    expect(menu).toBeDefined();
  });

  it("move buttons by keyboard: focus stays on the button (never disabled under the user)", async () => {
    const { available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    screen.getByRole("searchbox", { name: "Tìm bảng dữ liệu hoặc bảng điều khiển" }).focus();
    await userEvent.tab();
    const first = within(available).getByRole("option", { name: /Hợp đồng/ });
    expect(first).toHaveFocus();
    // Blueprint docs: "When an item is focused, it becomes selected."
    expect(first).toHaveAttribute("aria-selected", "true");
    await userEvent.tab();
    const addBtn = screen.getByRole("button", { name: "Thêm vào menu" });
    expect(addBtn).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(optionTexts(screen.getByRole("listbox", { name: "Menu của app (3)" }))).toEqual(["Khách hàng", "Đầu mối", "Hợp đồng"]);
    expect(addBtn).toHaveFocus();
    // Nothing selected in Có sẵn any more: another Enter is a no-op, focus still on the button.
    await userEvent.keyboard("{Enter}");
    expect(optionTexts(available)).toEqual(["Doanh số"]);
    expect(addBtn).toHaveFocus();
  });

  it("Chuyển lên reorders; save sends the replace-all payload in the new order with the trimmed override", async () => {
    const { menu, onBack, onSaved } = await renderEditor();
    await userEvent.click(within(menu).getByRole("option", { name: /Đầu mối/ }));
    await userEvent.click(screen.getByRole("button", { name: "Chuyển lên" }));
    expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]);
    expect(document.querySelector('[aria-live="assertive"]')).toHaveTextContent("Đầu mối: vị trí hiện tại 1 trên 2.");
    // The first item cannot go further up: the button stays enabled and focused, the order holds.
    const up = screen.getByRole("button", { name: "Chuyển lên" });
    await userEvent.click(up);
    expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]);
    expect(up).toHaveFocus();
    expect(up).toBeEnabled();
    // The moved item stays selected (its label field is still there).
    expect(within(menu).getByRole("option", { name: /Đầu mối/ })).toHaveAttribute("aria-selected", "true");

    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledWith("app-crm", [
      { itemType: "entity", refId: "e-con", label: "Đầu mối" },
      { itemType: "entity", refId: "e-acc" },
    ]));
    await waitFor(() => expect(onBack).toHaveBeenCalled());
    expect(onSaved).toHaveBeenCalled();
    expect(toast.success).toHaveBeenCalledWith("Đã lưu menu của “CRM”");
  });

  it("keyboard model of the blueprint: Space grabs, ArrowUp moves, Space drops — announced in the live region", async () => {
    const { menu } = await renderEditor();
    const second = within(menu).getByRole("option", { name: /Đầu mối/ });
    second.focus();
    fireEvent.keyDown(second, { key: " " });
    const live = document.querySelector('[aria-live="assertive"]');
    expect(live).toHaveTextContent("Đầu mối: vị trí hiện tại 2 trên 2. Dùng mũi tên lên/xuống để di chuyển trong danh sách.");
    expect(second).toHaveClass("slds-is-grabbed");

    fireEvent.keyDown(second, { key: "ArrowUp" });
    expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]);
    const moved = within(menu).getByRole("option", { name: /Đầu mối/ });
    expect(moved).toHaveFocus();
    fireEvent.keyDown(moved, { key: " " });
    expect(live).toHaveTextContent("Đầu mối: vị trí cuối 1 trên 2.");
    expect(moved).not.toHaveClass("slds-is-grabbed");
    expect(screen.getByRole("button", { name: "Lưu menu" })).toBeEnabled();
  });

  it("ArrowDown without a grab only moves the selection; Ctrl+ArrowRight adds from the left list", async () => {
    const { available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    const first = within(available).getByRole("option", { name: /Hợp đồng/ });
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowDown" });
    const next = within(available).getByRole("option", { name: /Doanh số/ });
    expect(next).toHaveAttribute("aria-selected", "true");
    expect(next).toHaveFocus();
    fireEvent.keyDown(next, { key: "ArrowRight", ctrlKey: true });
    const menuNow = screen.getByRole("listbox", { name: "Menu của app (3)" });
    expect(optionTexts(menuNow)).toEqual(["Khách hàng", "Đầu mối", "Doanh số"]);
    // Keyboard shortcut: focus follows the item into the target list, where it is selected.
    const moved = within(menuNow).getByRole("option", { name: /Doanh số/ });
    expect(moved).toHaveFocus();
    expect(moved).toHaveAttribute("aria-selected", "true");
    expect(moved).toHaveAttribute("tabindex", "0");
  });

  it("Ctrl+ArrowLeft sends a menu item back to Có sẵn with focus following", async () => {
    const { menu, available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    const first = within(menu).getByRole("option", { name: /Khách hàng/ });
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowLeft", ctrlKey: true });
    expect(optionTexts(screen.getByRole("listbox", { name: "Menu của app (1)" }))).toEqual(["Đầu mối"]);
    const back = within(available).getByRole("option", { name: /Khách hàng/ });
    expect(back).toHaveFocus();
    expect(back).toHaveAttribute("aria-selected", "true");
    expect(document.querySelector('[aria-live="assertive"]')).toHaveTextContent("Đã bỏ Khách hàng khỏi menu.");
  });

  it("double-click moves an option across and focus follows it", async () => {
    const { available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    await userEvent.dblClick(within(available).getByRole("option", { name: /Hợp đồng/ }));
    const moved = within(screen.getByRole("listbox", { name: "Menu của app (3)" })).getByRole("option", { name: /Hợp đồng/ });
    expect(moved).toHaveFocus();
    expect(moved).toHaveAttribute("aria-selected", "true");
  });

  it("roving tabindex: one tab stop per list; focus returns to the last selected item", async () => {
    const { menu, available } = await renderEditor();
    await waitFor(() => expect(optionTexts(available)).toEqual(["Hợp đồng", "Doanh số"]));
    const tabIndexes = (list: HTMLElement) => within(list).getAllByRole("option").map((o) => o.getAttribute("tabindex"));
    expect(tabIndexes(available)).toEqual(["0", "-1"]);
    expect(tabIndexes(menu)).toEqual(["0", "-1"]);
    // Nothing is selected before the user touches a list.
    expect(within(available).getAllByRole("option").every((o) => o.getAttribute("aria-selected") === "false")).toBe(true);

    const first = within(available).getByRole("option", { name: /Hợp đồng/ });
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(tabIndexes(available)).toEqual(["-1", "0"]);
    // Leave the list (to the move button) and come back: focus lands on the last selected item.
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Thêm vào menu" })).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(within(available).getByRole("option", { name: /Doanh số/ })).toHaveFocus();
    // Each list keeps its own selection: selecting in the menu leaves Có sẵn's selection alone.
    await userEvent.click(within(menu).getByRole("option", { name: /Đầu mối/ }));
    expect(within(available).getByRole("option", { name: /Doanh số/ })).toHaveAttribute("aria-selected", "true");
    expect(tabIndexes(menu)).toEqual(["-1", "0"]);
  });

  it("both listboxes are described by the drag-and-drop instructions", async () => {
    const { menu, available } = await renderEditor();
    for (const list of [menu, available]) {
      const hint = document.getElementById(list.getAttribute("aria-describedby") ?? "");
      expect(hint).toHaveTextContent("bấm phím cách để nhấc lên");
      expect(hint).toHaveClass("slds-assistive-text");
    }
  });

  it("grabbed: ArrowDown moves the item and keeps focus on it; Escape ends the grab", async () => {
    const { menu } = await renderEditor();
    const first = within(menu).getByRole("option", { name: /Khách hàng/ });
    first.focus();
    fireEvent.keyDown(first, { key: " " });
    expect(first).toHaveClass("slds-is-grabbed");
    // ArrowDown makes React move the focused node itself (a moved node loses focus); focus must end
    // on the item again.
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]);
    const moved = within(menu).getByRole("option", { name: /Khách hàng/ });
    expect(moved).toHaveFocus();
    expect(document.querySelector('[aria-live="assertive"]')).toHaveTextContent("Khách hàng: vị trí hiện tại 2 trên 2.");

    fireEvent.keyDown(moved, { key: "Escape" });
    expect(moved).not.toHaveClass("slds-is-grabbed");
    // Not grabbed any more: ArrowUp only moves the selection.
    fireEvent.keyDown(moved, { key: "ArrowUp" });
    expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]);
    expect(within(menu).getByRole("option", { name: /Đầu mối/ })).toHaveFocus();
  });

  it("pointer drag (dnd-kit) reorders the menu and announces the drop", async () => {
    const { menu } = await renderEditor();
    const opts = within(menu).getAllByRole("option");
    // happy-dom has no layout: give each option a 40px row so collision detection finds the target.
    opts.forEach((el, i) => {
      el.getBoundingClientRect = () =>
        ({ x: 0, y: i * 40, top: i * 40, left: 0, bottom: i * 40 + 40, right: 200, width: 200, height: 40, toJSON: () => ({}) }) as DOMRect;
    });
    fireEvent.pointerDown(opts[0], { isPrimary: true, button: 0, clientX: 10, clientY: 10 });
    fireEvent.pointerMove(document, { isPrimary: true, clientX: 10, clientY: 30 });
    fireEvent.pointerMove(document, { isPrimary: true, clientX: 10, clientY: 60 });
    fireEvent.pointerUp(document, { isPrimary: true, clientX: 10, clientY: 60 });
    await waitFor(() => expect(optionTexts(menu)).toEqual(["Đầu mối", "Khách hàng"]));
    expect(document.querySelector('[aria-live="assertive"]')).toHaveTextContent("Khách hàng: vị trí cuối 2 trên 2.");
    expect(screen.getByRole("button", { name: "Lưu menu" })).toBeEnabled();
  });

  it("each table option shows its slug, so two tables with the same label can be told apart", async () => {
    extraEntities = [{ id: "e-tk", name: "tk_customers", label: "Khách hàng", tableName: "obj_tk_customers" }];
    const { menu, available } = await renderEditor();
    const inMenu = within(menu).getByRole("option", { name: "Khách hàng crm_accounts" });
    const other = await within(available).findByRole("option", { name: "Khách hàng tk_customers" });
    expect(inMenu.querySelector(".slds-text-color_weak")).toHaveTextContent("crm_accounts");
    expect(other.querySelector(".slds-text-color_weak")).toHaveTextContent("tk_customers");
    // A selected option is white on brand blue: the slug drops the grey weak colour there (contrast).
    await userEvent.click(other);
    expect(other).toHaveAttribute("aria-selected", "true");
    expect(other.querySelector(".slds-text-color_weak")).toBeNull();
    expect(other).toHaveTextContent("tk_customers");
    // Dashboards have no slug to show.
    expect(within(available).getByRole("option", { name: "Doanh số" })).toBeInTheDocument();
  });

  it("autoFocus moves focus to the editor region (the control that opened it is gone)", async () => {
    wrap(<NavEditor app={CRM} onBack={vi.fn()} onSaved={vi.fn()} autoFocus />);
    await screen.findByRole("listbox", { name: "Menu của app (2)" });
    expect(screen.getByRole("region", { name: "Menu của “CRM”" })).toHaveFocus();
  });

  it("the selected menu item's custom label is edited below the lists and saved trimmed", async () => {
    const { menu } = await renderEditor();
    expect(screen.getByText("Chọn một mục ở cột Menu của app để đặt nhãn riêng cho nó.")).toBeInTheDocument();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    const input = screen.getByLabelText("Nhãn tuỳ chỉnh cho Khách hàng");
    await userEvent.type(input, "  Khách VIP  ");
    // The menu column shows the override live, with the original name beside it.
    expect(optionTexts(menu)[0]).toBe("Khách VIP");
    expect(within(menu).getByRole("option", { name: "Khách VIP (gốc: Khách hàng) crm_accounts" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledWith("app-crm", [
      { itemType: "entity", refId: "e-acc", label: "Khách VIP" },
      { itemType: "entity", refId: "e-con", label: "Đầu mối" },
    ]));
  });

  it("save failure keeps the editor open with an error toast", async () => {
    vi.mocked(replaceAppNav).mockRejectedValueOnce(new Error("422"));
    const { menu, onBack } = await renderEditor();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi menu" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Không lưu được menu — kiểm tra lại các mục rồi thử lại."));
    expect(onBack).not.toHaveBeenCalled();
  });
});

describe("AppsManager — /studio/apps list", () => {
  it("lists apps in an SLDS data table: app page link, slug, menu count, default badge", async () => {
    wrap(<AppsManager />);
    const grid = await screen.findByRole("grid", { name: "Danh sách app" });
    expect(grid).toHaveClass("slds-table");
    const crm = await within(grid).findByRole("link", { name: "CRM" });
    expect(crm).toHaveAttribute("href", "/studio/du-an/northwind/dung/man-hinh/app?app=crm");
    expect(within(grid).getByText("Mặc định")).toHaveClass("slds-badge");
    expect(within(grid).getByText("lms")).toBeInTheDocument();
    expect(within(grid).getByText("2 mục")).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Apps");
    // ObjectHomePage template (contracts/shell.md): header and table share ONE card; the page action
    // is the blueprint's neutral button.
    const card = grid.closest("article.slds-card");
    expect(card).not.toBeNull();
    expect(card!.querySelector(".slds-page-header")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Tạo app" })).toHaveClass("slds-button", "slds-button_neutral");
  });

  it("Tạo app opens an in-page form, never a dialog", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("grid", { name: "Danh sách app" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo app" }));
    expect(screen.getByRole("textbox", { name: /Tên app/ })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByTestId("create-dialog")).toBeNull();
  });

  it("row actions: Sửa menu opens the nav editor, Quay lại returns to the list", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "CRM" });
    screen.getByRole("button", { name: "Thêm thao tác cho CRM" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Sửa menu" }));
    expect(await screen.findByRole("heading", { name: "Menu của “CRM”" })).toBeInTheDocument();
    // The row menu that opened it is gone with the list: focus moves into the editor.
    await waitFor(() => expect(screen.getByRole("region", { name: "Menu của “CRM”" })).toHaveFocus());
    await userEvent.click(screen.getByRole("button", { name: "Quay lại" }));
    expect(await screen.findByRole("grid", { name: "Danh sách app" })).toBeInTheDocument();
  });

  it("row actions: Sửa opens the edit dialog for that app; Đặt làm app mặc định sends the full-replace PUT", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Sửa tên, icon, màu, mô tả" }));
    expect(screen.getByTestId("edit-dialog-lms")).toBeInTheDocument();

    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Đặt làm app mặc định" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalledWith("app-lms", {
      label: "LMS", description: "Học vụ", icon: null, color: "cy", isDefault: true, sortOrder: 1,
    }));
  });

  it("the default app has no Đặt làm app mặc định action", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "CRM" });
    screen.getByRole("button", { name: "Thêm thao tác cho CRM" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByRole("menuitem", { name: "Sửa menu" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Đặt làm app mặc định" })).toBeNull();
  });

  it("Xoá app asks first (data is kept), then deletes the lens only", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá app" }));
    expect(await screen.findByText(/KHÔNG bị xoá/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(deleteApp).not.toHaveBeenCalled();
    await userEvent.click(screen.getAllByRole("button", { name: "Xoá app" }).at(-1)!);
    await waitFor(() => expect(deleteApp).toHaveBeenCalledWith("app-lms"));
    expect(toast.success).toHaveBeenCalledWith("Đã xoá app “LMS” — dữ liệu bên trong vẫn nguyên");
  });

  // specs/063 S7-4 T083
  it("a draft-edited app carries the draft badge; a live app does not", async () => {
    vi.mocked(adminApps).mockResolvedValue([{ ...CRM, draftState: "edited" }, LMS]);
    wrap(<AppsManager />);
    const grid = await screen.findByRole("grid", { name: "Danh sách app" });
    await within(grid).findByRole("link", { name: "CRM" });
    const badges = within(grid).getAllByText("Nháp");
    expect(badges).toHaveLength(1);
    expect(within(within(grid).getByRole("link", { name: "LMS" }).closest("tr") as HTMLElement).queryByText("Nháp")).toBeNull();
  });

  it("before delivery (or for a non-builder) nothing is said about Áp ngay", async () => {
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    expect(screen.queryByText(/Áp ngay/)).toBeNull();
  });

  it("delivered workspace: the page and the delete confirm say Áp ngay", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as PublishState);
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    expect(await screen.findByText(/Áp ngay: Tạo và xoá app có hiệu lực ngay/)).toBeInTheDocument();
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá app" }));
    expect(await screen.findByText(/Xoá app có hiệu lực ngay cho người dùng thật/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("no apps → illustration empty state with a create CTA", async () => {
    vi.mocked(adminApps).mockResolvedValue([]);
    wrap(<AppsManager />);
    const title = await screen.findByText("Chưa có app nào");
    expect(title.closest("article.slds-card")?.querySelector(".slds-page-header")).not.toBeNull();
    expect(screen.queryByRole("grid")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Tạo app đầu tiên" }));
    expect(screen.getByRole("textbox", { name: /Tên app/ })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("AppsManager — Studio split mode (specs/074 lát 24)", () => {
  beforeEach(() => { mockPath = "/studio/du-an/northwind/dung/man-hinh/app"; });

  it("'+ App mới' opens a strip in the page (0 dialog); creating selects the new app and never writes nexus.activeApp", async () => {
    localStorage.setItem("nexus.activeApp", "crm");
    const onSelectApp = vi.fn();
    wrap(<AppsManager onSelectApp={onSelectApp} />);
    await userEvent.click(await screen.findByRole("button", { name: "+ App mới" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.type(screen.getByRole("textbox", { name: /Tên app/ }), "Mới");
    await userEvent.click(screen.getByRole("button", { name: /^Tạo app$/ }));
    await waitFor(() => expect(createApp).toHaveBeenCalledWith({ name: "moi", label: "Mới" }));
    await waitFor(() => expect(onSelectApp).toHaveBeenCalledWith("moi"));
    expect(localStorage.getItem("nexus.activeApp")).toBe("crm");
  });

  it("picking an app calls onSelectApp and does not touch nexus.activeApp", async () => {
    localStorage.setItem("nexus.activeApp", "crm");
    const onSelectApp = vi.fn();
    wrap(<AppsManager app="crm" onSelectApp={onSelectApp} />);
    const link = await screen.findByRole("link", { name: "LMS" });
    await userEvent.click(link);
    expect(onSelectApp).toHaveBeenCalledWith("lms");
    expect(localStorage.getItem("nexus.activeApp")).toBe("crm");
    expect(screen.getByRole("link", { name: "CRM" })).toHaveAttribute("aria-current", "true");
  });

  it("delete is confirmed in the page and names the consequence; deleting the open app clears the selection", async () => {
    const onSelectApp = vi.fn();
    wrap(<AppsManager app="lms" onSelectApp={onSelectApp} />);
    await screen.findByRole("link", { name: "LMS" });
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá app" }));
    expect(await screen.findByText(/KHÔNG bị xoá/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    await userEvent.click(screen.getAllByRole("button", { name: "Xoá app" }).at(-1)!);
    await waitFor(() => expect(deleteApp).toHaveBeenCalledWith("app-lms"));
    await waitFor(() => expect(onSelectApp).toHaveBeenCalledWith(null));
  });
});

// #53: on a delivered project PUT /admin/apps/:id and PUT /admin/apps/:id/nav write an app draft, and DELETE drops the app's
// draft (DraftAppStore.SoftDelete): each changes the header's pending count. The bus hears it only once the write has
// answered (a recount sent earlier reads the old count), and never on a refusal.
describe("AppsManager — the drafts bus (#53)", () => {
  let heard: string[] = [];
  let off = () => {};
  beforeEach(() => { heard = []; off = onDraftsChanged((s) => heard.push(s)); });
  afterEach(() => off());
  // The write answers only when `answer()` is called.
  function held<T>(mock: { mockReturnValueOnce: (p: Promise<T>) => unknown }, value: T) {
    let answer!: () => void;
    mock.mockReturnValueOnce(new Promise<T>((r) => { answer = () => r(value); }));
    return () => answer();
  }
  const pending = async () => { await new Promise((r) => setTimeout(r, 20)); expect(heard).toEqual([]); };

  it("Lưu menu", async () => {
    const answer = held(vi.mocked(replaceAppNav), []);
    const { menu, onBack } = await renderEditor();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi menu" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledTimes(1));
    await pending();
    answer();
    await waitFor(() => expect(onBack).toHaveBeenCalled());
    expect(heard).toEqual(["app"]);
  });

  it("Lưu menu refused (409): nothing is announced", async () => {
    vi.mocked(replaceAppNav).mockRejectedValueOnce(new ApiError(409, "APP_NAV_STALE", "stale"));
    const { menu } = await renderEditor();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi menu" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    expect(await screen.findByText(/Menu đã đổi ở nơi khác/)).toBeInTheDocument();
    expect(heard).toEqual([]);
  });

  it("Đặt làm app mặc định", async () => {
    const answer = held(vi.mocked(updateApp), LMS);
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Đặt làm app mặc định" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalledTimes(1));
    await pending();
    answer();
    await waitFor(() => expect(heard).toEqual(["app"]));
  });

  it("Xoá app", async () => {
    const answer = held(vi.mocked(deleteApp), undefined);
    wrap(<AppsManager />);
    await screen.findByRole("link", { name: "LMS" });
    screen.getByRole("button", { name: "Thêm thao tác cho LMS" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(screen.getByRole("menuitem", { name: "Xoá app" }));
    await userEvent.click(screen.getAllByRole("button", { name: "Xoá app" }).at(-1)!);
    await waitFor(() => expect(deleteApp).toHaveBeenCalledTimes(1));
    await pending();
    answer();
    await waitFor(() => expect(heard).toEqual(["app"]));
  });
});

describe("NavEditor — stale menu (409)", () => {
  it("409 on save keeps the draft, says the menu changed elsewhere and offers Tải lại", async () => {
    vi.mocked(replaceAppNav).mockRejectedValueOnce(new ApiError(409, "APP_NAV_STALE", "stale"));
    const { menu, onBack } = await renderEditor();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi menu" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    expect(await screen.findByText(/Menu đã đổi ở nơi khác/)).toBeInTheDocument();
    expect(toast.error).not.toHaveBeenCalled();
    expect(onBack).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Tải lại" }));
    await waitFor(() => expect(screen.queryByText(/Menu đã đổi ở nơi khác/)).toBeNull());
    expect(optionTexts(screen.getByRole("listbox", { name: "Menu của app (2)" }))).toEqual(["Khách hàng", "Đầu mối"]);
  });

  it("without onBack the editor has no Quay lại and stays open after a save", async () => {
    wrap(<NavEditor app={CRM} onSaved={vi.fn().mockResolvedValue(undefined)} />);
    const menu = await screen.findByRole("listbox", { name: "Menu của app (2)" });
    expect(screen.queryByRole("button", { name: "Quay lại" })).toBeNull();
    await userEvent.click(within(menu).getByRole("option", { name: /Khách hàng/ }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi menu" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu menu" }));
    await waitFor(() => expect(replaceAppNav).toHaveBeenCalledWith("app-crm", [{ itemType: "entity", refId: "e-con", label: "Đầu mối" }]));
    expect(screen.getByRole("listbox", { name: "Menu của app (2)" })).toBeInTheDocument();
  });
});

// specs/074 lát f2-draft-labels-data-menu T369: the menu editor reads tables over the draft overlay once delivered.
describe("NavEditor — nhãn nháp của bảng trong menu", () => {
  it("before delivery: no draft read, no draft label", async () => {
    wrap(<NavEditor app={CRM} onSaved={vi.fn()} />);
    await screen.findByText("Hợp đồng");
    expect(useEntitiesSpy).not.toHaveBeenCalledWith({ draftRead: true });
    expect(screen.queryByText("Mới, khách thấy khi xuất bản")).toBeNull();
  });
  it("delivered: reads the draft overlay; a new table is offered with its label, a to-be-deleted one is not offered", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as PublishState);
    extraEntities = [
      { id: "e-new", name: "crm_visits", label: "Lượt ghé", tableName: "obj_crm_visits", draftState: "new" },
      { id: "e-del", name: "crm_old", label: "Bảng cũ", tableName: "obj_crm_old", draftState: "deleted" },
    ];
    wrap(<NavEditor app={CRM} onSaved={vi.fn()} />);
    const row = (await screen.findByText("Lượt ghé")).closest("[role=option]") as HTMLElement;
    expect(await within(row).findByText("Mới, khách thấy khi xuất bản")).toBeInTheDocument();
    await waitFor(() => expect(useEntitiesSpy).toHaveBeenCalledWith({ draftRead: true }));
    expect(screen.queryByText("Bảng cũ")).toBeNull();
  });
  it("delivered: a menu item whose table is marked to delete says so", async () => {
    vi.mocked(getPublishState).mockResolvedValue({ delivered: true } as PublishState);
    ENTITIES[0] = { ...ENTITIES[0], draftState: "deleted" };
    try {
      wrap(<NavEditor app={CRM} onSaved={vi.fn()} />);
      const row = (await screen.findAllByText(/Khách hàng/)).map((n) => n.closest("[role=option]") as HTMLElement).find(Boolean)!;
      expect(await within(row).findByText("Sẽ xoá khi xuất bản")).toBeInTheDocument();
    } finally {
      delete ENTITIES[0].draftState;
    }
  });
});

// specs/074 lát f2-ai-preview-menu-access-report-brand (T408): an assistant "app" update draws the new menu rows in place.
describe("NavEditor — assistant preview (docType app)", () => {
  const nav = (...rows: string[][]) => ({ nav: rows.map(([type, name, label]) => ({ type, name, label })) });
  const change = mkChange("app", "update", {
    target: { name: "crm" },
    before: nav(["entity", "crm_accounts", ""], ["entity", "crm_contacts", "Đầu mối"]),
    after: nav(["entity", "crm_accounts", ""], ["entity", "crm_visits", ""], ["entity", "crm_contacts", "Đầu mối"]),
  });
  const open = (c: ReturnType<typeof mkChange> | null, app = CRM) => wrap(<WithPreview change={c}><NavEditor app={app} onSaved={vi.fn()} /></WithPreview>);

  it("shows '+ mục' at its new position and keeps the real rows, custom labels and the save bar untouched", async () => {
    extraEntities = [{ id: "e-vis", name: "crm_visits", label: "Lượt ghé", tableName: "obj_crm_visits" }];
    open(change);
    const ghost = await screen.findByText(/\+ mục/);
    const row = ghost.closest("li")!;
    expect(row.className).toContain("nx-proposed");
    expect(row.getAttribute("data-proposal-key")).toBe("app-update");
    expect(row.textContent).toContain("Lượt ghé");
    const menu = screen.getByRole("listbox", { name: /Menu của app/ });
    const labels = Array.from(menu.querySelectorAll("li")).map((li) => li.textContent ?? "");
    expect(labels[1]).toContain("Lượt ghé"); // between the two real rows
    expect(labels[0]).toContain("Khách hàng");
    expect(labels[2]).toContain("Đầu mối"); // the custom label survives
    expect(replaceAppNav).not.toHaveBeenCalled();
  });

  it("another app's change paints nothing here", async () => {
    open(change, LMS);
    await screen.findByRole("listbox", { name: /Menu của app/ });
    expect(screen.queryByText(/\+ mục/)).toBeNull();
  });

  it("no active change: no ghost (regression)", async () => {
    open(null);
    await screen.findByRole("listbox", { name: /Menu của app/ });
    expect(document.querySelectorAll(".nx-proposed")).toHaveLength(0);
  });
});
