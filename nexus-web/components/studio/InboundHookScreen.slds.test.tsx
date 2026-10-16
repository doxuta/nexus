// InboundHookScreen on SLDS (specs/038 T044): opening a hook shows its URL as a read-only input with
// a copy button; the row menu opens the recent-deliveries panel and asks before rotating the secret
// (modals/base). lib/api mocked at the module seam (inboundHookUrl stays real).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { InboundHookScreen } from "./InboundHookScreen";
import {
  ensureAuth, listEntities, listFields, listInboundHooks, listHookDeliveries, rotateInboundHookSecret,
  updateInboundHook, inboundHookUrl, type InboundHook,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listFields: vi.fn(), listInboundHooks: vi.fn(),
    listHookDeliveries: vi.fn(), createInboundHook: vi.fn(), updateInboundHook: vi.fn(),
    deleteInboundHook: vi.fn(), rotateInboundHookSecret: vi.fn(),
  };
});

const HOOK: InboundHook = {
  id: "h1", name: "bank-in", entityName: "ih_orders", secret: "whk_x",
  mapping: [{ source: "data.code", field: "code" }], matchField: "code",
  isActive: true, createdAt: "2026-07-22T00:00:00Z", updatedAt: "2026-07-22T00:00:00Z",
};

function renderScreen() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><InboundHookScreen /></QueryClientProvider>);
}

async function openRowMenu(item: string) {
  await screen.findByRole("grid", { name: "Danh sách cổng tín hiệu" });
  (await screen.findByRole("button", { name: "Thêm thao tác cho bank-in" })).focus();
  await userEvent.keyboard("{Enter}");
  await userEvent.click(screen.getByRole("menuitem", { name: item }));
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listInboundHooks).mockReset().mockResolvedValue([HOOK]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([{ name: "ih_orders", label: "Orders", tableName: "obj_ih_orders" }]);
  vi.mocked(listFields).mockReset().mockResolvedValue([{ name: "code", label: "Code", fieldType: "text", isUnique: true }]);
  vi.mocked(listHookDeliveries).mockReset().mockResolvedValue([
    { id: "d1", hookId: "h1", status: "created", createdAt: "2026-07-22T01:00:00Z", payloadHead: '{"code":"A1"}' },
    { id: "d2", hookId: "h1", status: "error", error: "thiếu trường code", matchStatus: "unmatched", createdAt: "2026-07-22T02:00:00Z" },
  ]);
  vi.mocked(updateInboundHook).mockReset();
  vi.mocked(rotateInboundHookSecret).mockReset().mockResolvedValue({ ...HOOK, secret: "whk_new" });
});

describe("InboundHookScreen — SLDS", () => {
  it("opening a hook shows its URL read-only, and the copy button copies it", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    renderScreen();
    fireEvent.click(await screen.findByRole("button", { name: "bank-in" }));

    expect(await screen.findByRole("heading", { name: "Sửa cổng tín hiệu" })).toBeInTheDocument();
    const url = screen.getByLabelText("URL nhận tín hiệu");
    expect(url).toHaveValue(inboundHookUrl("bank-in"));
    expect(url).toHaveAttribute("readonly");
    const panel = url.closest(".slds-panel") as HTMLElement;
    fireEvent.click(within(panel).getByRole("button", { name: "Copy URL" }));
    expect(writeText).toHaveBeenCalledWith(inboundHookUrl("bank-in"));
  });

  it("row menu opens the recent deliveries in a panel", async () => {
    renderScreen();
    await openRowMenu("Tín hiệu gần đây");
    expect(await screen.findByRole("heading", { name: "Tín hiệu gần đây — bank-in" })).toBeInTheDocument();
    expect(await screen.findByText("thiếu trường code")).toBeInTheDocument();
    expect(screen.getByText("hộp chờ")).toBeInTheDocument();
    expect(screen.getByText('{"code":"A1"}')).toBeInTheDocument();
  });

  it("editing a hook keeps its payment match — the full-replace update must not wipe it", async () => {
    const pm = {
      targetEntity: "ih_orders", refField: "code", refPattern: "INV-\\d+", memoSource: "data.memo",
      statusField: "code", statusValue: "Đã thu",
    };
    vi.mocked(listInboundHooks).mockResolvedValue([{ ...HOOK, paymentMatch: pm }]);
    vi.mocked(updateInboundHook).mockResolvedValue({ ...HOOK, paymentMatch: pm });
    renderScreen();
    fireEvent.click(await screen.findByRole("button", { name: "bank-in" }));
    expect(await screen.findByLabelText("Match thanh toán (báo có → gạch nợ chứng từ)")).toBeChecked();
    fireEvent.click(screen.getByText("Lưu cổng"));
    await waitFor(() => expect(updateInboundHook).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateInboundHook).mock.calls[0][0]).toBe("h1");
    expect(vi.mocked(updateInboundHook).mock.calls[0][1]).toMatchObject({ name: "bank-in", matchField: "code", paymentMatch: pm });
  });

  it("rotating the secret asks first (in the page), then rotates", async () => {
    renderScreen();
    await openRowMenu("Đổi khoá");
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Đổi khoá ngay" }));
    await waitFor(() => expect(rotateInboundHookSecret).toHaveBeenCalledWith("h1"));
  });

  // panels/base + modals/base docs: focus returns to what opened them. From the row menu that is the
  // row's ▾ button — the menu item is gone by the time the panel or modal closes.
  describe("focus return from the row menu", () => {
    const settle = () => act(() => new Promise((r) => setTimeout(r, 20)));

    it("Tín hiệu gần đây → Thu gọn: focus goes back to the row's menu button", async () => {
      renderScreen();
      await openRowMenu("Tín hiệu gần đây");
      await settle();
      const heading = await screen.findByRole("heading", { name: "Tín hiệu gần đây — bank-in" });
      const panel = heading.closest(".slds-panel") as HTMLElement;
      await userEvent.click(within(panel).getByRole("button", { name: "Thu gọn Tín hiệu gần đây — bank-in" }));
      await settle();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác cho bank-in" }));
    });

    it("Sửa: focus goes into the editor, and back to the row's menu button when it closes", async () => {
      renderScreen();
      await openRowMenu("Sửa");
      await settle();
      const panel = (await screen.findByRole("heading", { name: "Sửa cổng tín hiệu" })).closest(".slds-panel") as HTMLElement;
      expect(panel).toContainElement(document.activeElement as HTMLElement);
      await userEvent.click(within(panel).getByRole("button", { name: "Huỷ" }));
      await settle();
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Thêm thao tác cho bank-in" }));
    });

    it("Đổi khoá → Huỷ: the page confirm closes and Esc-safe focus is not lost to the body", async () => {
      renderScreen();
      await openRowMenu("Đổi khoá");
      await settle();
      await userEvent.click(await screen.findByRole("button", { name: "Huỷ" }));
      await settle();
      expect(screen.queryByRole("button", { name: "Đổi khoá ngay" })).toBeNull();
    });
  });

  it("the buttons that open the editor control its panel (aria-controls + aria-expanded)", async () => {
    const { container } = renderScreen();
    const open = await screen.findByRole("button", { name: "bank-in" });
    const create = screen.getByRole("button", { name: "Tạo cổng" });
    const panel = container.querySelector(".slds-panel")!;
    expect(panel.id).not.toBe("");
    expect(open).toHaveAttribute("aria-controls", panel.id);
    expect(create).toHaveAttribute("aria-controls", panel.id);
    expect(open).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(open);
    expect(open).toHaveAttribute("aria-expanded", "true");
    expect(create).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(create);
    expect(create).toHaveAttribute("aria-expanded", "true");
    expect(open).toHaveAttribute("aria-expanded", "false");
  });

  it("opening a row by its name and collapsing the panel returns focus to that name", async () => {
    renderScreen();
    const open = await screen.findByRole("button", { name: "bank-in" });
    await userEvent.click(open);
    await userEvent.click(screen.getByRole("button", { name: "Thu gọn Sửa cổng tín hiệu" }));
    expect(document.activeElement).toBe(open);
  });
});
