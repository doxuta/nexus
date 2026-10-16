// InboundHookScreen — CONG-TIN-HIEU Studio contract: list renders hook rows (URL, entity,
// upsert key, active badge), empty state invites creation, the editor's upsert-key select
// offers ONLY unique fields, and a server 422 surfaces verbatim. lib/api mocked at the seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { InboundHookScreen } from "./InboundHookScreen";
import userEvent from "@testing-library/user-event";
import {
  ensureAuth, listEntities, listFields, listInboundHooks, createInboundHook,
  rotateInboundHookSecret, deleteInboundHook, listHookDeliveries,
  ApiError, type InboundHook,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listEntities: vi.fn(),
    listFields: vi.fn(),
    listInboundHooks: vi.fn(),
    listHookDeliveries: vi.fn(),
    createInboundHook: vi.fn(),
    updateInboundHook: vi.fn(),
    deleteInboundHook: vi.fn(),
    rotateInboundHookSecret: vi.fn(),
  };
});

const HOOKS: InboundHook[] = [
  {
    id: "h1", name: "bank-in", entityName: "ih_orders", secret: "whk_x",
    mapping: [{ source: "data.code", field: "code" }], matchField: "code",
    isActive: true, createdAt: "2026-07-22T00:00:00Z", updatedAt: "2026-07-22T00:00:00Z",
  },
  {
    id: "h2", name: "ghtk-status", entityName: "tk_tickets", secret: "whk_y",
    mapping: [], isActive: false, createdAt: "2026-07-22T00:00:00Z", updatedAt: "2026-07-22T00:00:00Z",
  },
];

function renderScreen(props: React.ComponentProps<typeof InboundHookScreen> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <InboundHookScreen {...props} />
    </QueryClientProvider>,
  );
}

async function rowMenu(item: string, hook = "bank-in") {
  await screen.findByRole("grid", { name: "Danh sách cổng tín hiệu" });
  (await screen.findByRole("button", { name: `Thêm thao tác cho ${hook}` })).focus();
  await userEvent.keyboard("{Enter}");
  await userEvent.click(screen.getByRole("menuitem", { name: item }));
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listInboundHooks).mockReset().mockResolvedValue(HOOKS);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "ih_orders", label: "Orders", tableName: "obj_ih_orders" },
  ]);
  vi.mocked(listFields).mockReset().mockResolvedValue([
    { name: "code", label: "Code", fieldType: "text", isUnique: true },
    { name: "amount", label: "Amount", fieldType: "number" },
  ]);
});

describe("InboundHookScreen", () => {
  it("renders hook rows with entity, upsert key and active badges", async () => {
    renderScreen();
    expect(await screen.findByText("bank-in")).toBeInTheDocument();
    expect(screen.getByText("ih_orders")).toBeInTheDocument();
    expect(screen.getByText("code")).toBeInTheDocument();
    expect(screen.getByText("Luôn tạo mới")).toBeInTheDocument();
    expect(screen.getByText("Đang mở")).toBeInTheDocument();
    expect(screen.getByText("Đang tắt")).toBeInTheDocument();
  });

  it("empty state invites creating the first hook", async () => {
    vi.mocked(listInboundHooks).mockResolvedValue([]);
    renderScreen();
    await waitFor(() => expect(document.body.textContent).toContain("Chưa có cổng tín hiệu nào"));
  });

  it("upsert-key select offers ONLY unique fields, and a 422 surfaces verbatim", async () => {
    vi.mocked(createInboundHook).mockRejectedValue(
      new ApiError(422, "HOOK_INVALID", "matchField phải là field unique (khoá upsert)"),
    );
    renderScreen();
    fireEvent.click(await screen.findByText("Tạo cổng"));

    const entitySelect = screen.getByLabelText("Đối tượng đích");
    fireEvent.change(entitySelect, { target: { value: "ih_orders" } });

    // Unique-only upsert select: "code" is offered, "amount" (non-unique) is not.
    const matchSelect = screen.getByLabelText("Khoá cập nhật (trường duy nhất, tuỳ chọn)") as HTMLSelectElement;
    await waitFor(() => expect(matchSelect.querySelectorAll("option")).toHaveLength(2)); // placeholder + code
    expect(Array.from(matchSelect.options).map((o) => o.value)).toEqual(["", "code"]);

    fireEvent.change(screen.getByPlaceholderText("bank-in"), { target: { value: "q" } });
    fireEvent.click(screen.getByText("Lưu cổng"));
    await waitFor(() =>
      expect(screen.getByText(/matchField phải là field unique/)).toBeInTheDocument(),
    );
    // The panel scrolls and the button bar is pinned to its bottom: the error sits right above that
    // bar, so it is in view wherever the user scrolled to before pressing Lưu.
    const note = screen.getByText(/matchField phải là field unique/).closest(".slds-scoped-notification")!;
    expect(note.nextElementSibling).toContainElement(screen.getByRole("button", { name: "Lưu cổng" }));
  });
});


// specs/074 lát f2-automation-hook-calendar (T160): rotate/delete confirm IN the page, the new key shows once, in place.
describe("InboundHookScreen — in-page confirm, one-time key, deep link", () => {
  beforeEach(() => {
    vi.mocked(rotateInboundHookSecret).mockReset().mockResolvedValue({ ...HOOKS[0], secret: "whk_fresh" });
    vi.mocked(deleteInboundHook).mockReset().mockResolvedValue(undefined as never);
    vi.mocked(listHookDeliveries).mockReset().mockResolvedValue([]);
  });

  it("changing the key asks in the page (0 dialogs), then shows the new key once and loses it on leaving", async () => {
    const first = renderScreen();
    await rowMenu("Đổi khoá");
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    expect(await screen.findByText(/bank-in.*mất hiệu lực/)).toBeInTheDocument();
    expect(rotateInboundHookSecret).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Đổi khoá ngay" }));
    await waitFor(() => expect(rotateInboundHookSecret).toHaveBeenCalledWith("h1"));
    expect(await screen.findByDisplayValue("whk_fresh")).toBeInTheDocument();
    expect(screen.getByText(/chỉ hiện một lần/)).toBeInTheDocument();
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    first.unmount();
    renderScreen();
    await screen.findByText("bank-in");
    expect(screen.queryByDisplayValue("whk_fresh")).toBeNull();
  });

  it("hiding the key after copying removes it", async () => {
    renderScreen();
    await rowMenu("Đổi khoá");
    fireEvent.click(await screen.findByRole("button", { name: "Đổi khoá ngay" }));
    await screen.findByDisplayValue("whk_fresh");
    fireEvent.click(screen.getByRole("button", { name: "Đã chép, ẩn khoá" }));
    expect(screen.queryByDisplayValue("whk_fresh")).toBeNull();
  });

  it("deleting asks in the page (0 dialogs) and Huỷ keeps the hook", async () => {
    renderScreen();
    await rowMenu("Xoá");
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    fireEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
    expect(deleteInboundHook).not.toHaveBeenCalled();
    await rowMenu("Xoá");
    fireEvent.click(await screen.findByRole("button", { name: "Xoá cổng" }));
    await waitFor(() => expect(deleteInboundHook).toHaveBeenCalledWith("h1"));
  });

  it("initialHookId opens that hook's editor; opening and closing report through onSelect", async () => {
    const onSelect = vi.fn();
    renderScreen({ initialHookId: "h2", onSelect });
    expect(await screen.findByRole("heading", { name: "Sửa cổng tín hiệu" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("bank-in")).toHaveValue("ghtk-status");
    fireEvent.click(within(document.querySelector(".slds-panel") as HTMLElement).getByRole("button", { name: "Huỷ" }));
    expect(onSelect).toHaveBeenLastCalledWith(null);
    fireEvent.click(await screen.findByRole("button", { name: "bank-in" }));
    expect(onSelect).toHaveBeenLastCalledWith("h1");
  });

  it("an initialHookId that is not a hook of this project opens nothing", async () => {
    renderScreen({ initialHookId: "nope" });
    await screen.findByText("bank-in");
    expect(screen.queryByRole("heading", { name: "Sửa cổng tín hiệu" })).toBeNull();
  });
});
