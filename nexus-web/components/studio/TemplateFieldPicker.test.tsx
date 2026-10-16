// specs/059 (DAILY-QUEUE #271): the email action's "Chọn người nhận" / "Chèn trường" menus. The
// builder picks the linked patient's email with the mouse instead of guessing a machine name.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { listFields, type FieldMeta } from "@/lib/api";
import { TemplateFieldPicker } from "./TemplateFieldPicker";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listFields: vi.fn(async (entity: string) =>
      entity === "benh_nhan"
        ? [
            { name: "ho_ten", label: "Họ tên", fieldType: "text" },
            { name: "email", label: "Email", fieldType: "email" },
          ]
        : entity === "khach_hang"
          ? [{ name: "email", label: "Email KH", fieldType: "text" }]
          : [],
    ),
  };
});

const APPOINTMENT: FieldMeta[] = [
  { name: "tieu_de", label: "Tiêu đề", fieldType: "text" },
  { name: "benh_nhan", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan" } },
];

beforeEach(() => vi.mocked(listFields).mockClear());

describe("TemplateFieldPicker", () => {
  it("offers the linked patient's email and hands back its placeholder", async () => {
    const onPick = vi.fn();
    render(<TemplateFieldPicker fields={APPOINTMENT} emailOnly label="Chọn người nhận" onPick={onPick} />);
    // Nothing is fetched until the builder opens the menu.
    expect(listFields).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Chọn người nhận" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Bệnh nhân › Email" }));

    expect(onPick).toHaveBeenCalledWith("{{benh_nhan.email}}");
    expect(listFields).toHaveBeenCalledWith("benh_nhan");
    // emailOnly: the patient's name is not an address.
    expect(screen.queryByRole("menuitem", { name: "Bệnh nhân › Họ tên" })).toBeNull();
  });

  it("subject/body menu lists the record's own fields and the linked record's", async () => {
    const onPick = vi.fn();
    render(<TemplateFieldPicker fields={APPOINTMENT} label="Chèn trường vào tiêu đề" onPick={onPick} />);
    await userEvent.click(screen.getByRole("button", { name: "Chèn trường vào tiêu đề" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Bệnh nhân › Họ tên" }));
    expect(onPick).toHaveBeenCalledWith("{{benh_nhan.ho_ten}}");

    await userEvent.click(screen.getByRole("button", { name: "Chèn trường vào tiêu đề" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Tiêu đề" }));
    expect(onPick).toHaveBeenLastCalledWith("{{tieu_de}}");
    // The linked entity's fields were fetched once, not on every open.
    expect(listFields).toHaveBeenCalledTimes(1);
  });

  // WorkflowBuilder keys the action card by index and keeps the editor mounted, so ONE picker lives on
  // across a switch to another rule (another entity) — it must load the new entity's fields, not
  // answer "no email field" from the first rule's cache.
  it("follows a switch to a rule on another entity", async () => {
    const ORDER: FieldMeta[] = [
      { name: "ma_don", label: "Mã đơn", fieldType: "text" },
      { name: "khach_hang", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "khach_hang" } },
    ];
    const { rerender } = render(<TemplateFieldPicker fields={APPOINTMENT} emailOnly label="Chọn người nhận" onPick={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Chọn người nhận" }));
    expect(await screen.findByRole("menuitem", { name: "Bệnh nhân › Email" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");

    const onPick = vi.fn();
    rerender(<TemplateFieldPicker fields={ORDER} emailOnly label="Chọn người nhận" onPick={onPick} />);
    await userEvent.click(screen.getByRole("button", { name: "Chọn người nhận" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Khách hàng › Email KH" }));
    expect(onPick).toHaveBeenCalledWith("{{khach_hang.email}}");
    expect(vi.mocked(listFields).mock.calls).toEqual([["benh_nhan"], ["khach_hang"]]);
  });

  // The rule's own fields may still be loading when the builder opens the menu.
  it("loads the linked entity when the rule's fields arrive while the menu is open", async () => {
    const { rerender } = render(<TemplateFieldPicker fields={[]} emailOnly label="Chọn người nhận" onPick={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Chọn người nhận" }));
    rerender(<TemplateFieldPicker fields={APPOINTMENT} emailOnly label="Chọn người nhận" onPick={vi.fn()} />);
    expect(await screen.findByRole("menuitem", { name: "Bệnh nhân › Email" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /Chưa có trường email/ })).toBeNull();
  });

  it("says why the list is empty instead of showing an empty menu", async () => {
    render(<TemplateFieldPicker fields={[{ name: "tieu_de", label: "Tiêu đề", fieldType: "text" }]} emailOnly label="Chọn người nhận" onPick={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Chọn người nhận" }));
    expect(await screen.findByRole("menuitem", { name: /Chưa có trường email/ })).toHaveAttribute("aria-disabled", "true");
  });
});
