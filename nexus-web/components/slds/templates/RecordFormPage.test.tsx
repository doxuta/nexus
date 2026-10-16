// RecordFormPage — form page template (expandable-section + docked-form-footer + scoped-notifications,
// modal variant on modals/base).
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import { RecordFormPage, type RecordFormPageProps, type RecordFormSection } from "./RecordFormPage";

const icon = { glyph: Building2, name: "account" };
const sections: RecordFormSection[] = [
  { id: "info", title: "Thông tin chung", body: <input aria-label="Tên khách hàng" /> },
  { id: "address", title: "Địa chỉ", body: <input aria-label="Tỉnh/thành" />, collapsed: true },
];

describe("RecordFormPage", () => {
  it("docked={false} inertFooter makes the button row inert", () => {
    const { container } = render(
      <RecordFormPage title="X" sections={sections} onSubmit={() => {}} onCancel={() => {}} docked={false} inertFooter />,
    );
    expect(screen.getByRole("button", { name: "Lưu" }).parentElement).toHaveAttribute("inert");
    expect(container.querySelector("form")).not.toHaveAttribute("inert");
  });

  it("page docked={false}: inline footer row, no docked footer, no fixed position (specs/075 T010)", () => {
    const { container } = render(
      <RecordFormPage icon={icon} title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} docked={false} />,
    );
    expect(container.querySelector(".slds-docked-form-footer")).toBeNull();
    const save = screen.getByRole("button", { name: "Lưu" });
    expect(save.parentElement).toHaveClass("slds-grid", "slds-grid_align-end");
    expect(container.innerHTML).not.toContain("fixed");
    expect(container.firstElementChild).not.toHaveClass("slds-p-bottom_xx-large");
  });

  it("page: base header, one expandable section per layout section, docked footer bound to the form", async () => {
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault());
    const onCancel = vi.fn();
    const { container } = render(
      <RecordFormPage icon={icon} objectLabel="Khách hàng" title="Khách hàng mới" sections={sections} onSubmit={onSubmit} onCancel={onCancel} />,
    );
    expect(container.querySelector(".slds-page-header")).not.toBeNull();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Khách hàng mới");

    const open = screen.getByRole("button", { name: "Thông tin chung" });
    expect(open).toHaveClass("slds-button", "slds-section__title-action");
    expect(open).toHaveAttribute("aria-expanded", "true");
    expect(open.parentElement).toHaveClass("slds-section__title");
    expect(open.closest(".slds-section")).toHaveClass("slds-is-open");
    const content = document.getElementById(open.getAttribute("aria-controls")!)!;
    expect(content).toHaveClass("slds-section__content");
    expect(content).not.toHaveAttribute("hidden");
    expect(within(content).getByRole("textbox", { name: "Tên khách hàng" })).toBeInTheDocument();

    const footer = container.querySelector(".slds-docked-form-footer")!;
    // The fixed footer covers the last 3rem of the page: the root reserves it with padding, which
    // (unlike a margin on the last card) cannot collapse out of the root.
    expect(container.firstElementChild).toHaveClass("slds-p-bottom_xx-large");
    const cancel = within(footer as HTMLElement).getByRole("button", { name: "Huỷ" });
    const save = within(footer as HTMLElement).getByRole("button", { name: "Lưu" });
    expect(cancel).toHaveClass("slds-button", "slds-button_neutral");
    expect(cancel).toHaveAttribute("type", "button");
    expect(save).toHaveClass("slds-button", "slds-button_brand");
    expect(save).toHaveAttribute("type", "submit");
    const form = container.querySelector("form")!;
    expect(save).toHaveAttribute("form", form.id);
    expect(form).toHaveAttribute("novalidate");

    await userEvent.click(cancel);
    expect(onCancel).toHaveBeenCalledOnce();
    await userEvent.click(save);
    expect(onSubmit).toHaveBeenCalledOnce();
  });

  it("sections toggle; a collapsed one starts closed; one with an error is forced open", async () => {
    const { rerender } = render(<RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} />);
    const address = screen.getByRole("button", { name: "Địa chỉ" });
    expect(address).toHaveAttribute("aria-expanded", "false");
    expect(address.closest(".slds-section")).not.toHaveClass("slds-is-open");
    expect(document.getElementById(address.getAttribute("aria-controls")!)).toHaveAttribute("hidden");

    await userEvent.click(address);
    expect(address).toHaveAttribute("aria-expanded", "true");
    expect(document.getElementById(address.getAttribute("aria-controls")!)).not.toHaveAttribute("hidden");

    const info = screen.getByRole("button", { name: "Thông tin chung" });
    info.focus();
    await userEvent.keyboard("{Enter}");
    expect(info).toHaveAttribute("aria-expanded", "false");
    await userEvent.keyboard(" ");
    expect(info).toHaveAttribute("aria-expanded", "true");

    rerender(
      <RecordFormPage
        title="Khách hàng mới"
        sections={[sections[0], { ...sections[1], hasError: true }]}
        onSubmit={() => {}}
        onCancel={() => {}}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Địa chỉ" }));
    expect(screen.getByRole("button", { name: "Địa chỉ" })).toHaveAttribute("aria-expanded", "true");

    // The user fixes the field: the section stays where it was, it does not snap shut.
    rerender(<RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} />);
    expect(screen.getByRole("button", { name: "Địa chỉ" })).toHaveAttribute("aria-expanded", "true");
  });

  it("an error opens a collapsed section untouched by the user, and it stays open once the error clears", () => {
    const withError = [sections[0], { ...sections[1], hasError: true }];
    const { rerender } = render(<RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} />);
    const address = screen.getByRole("button", { name: "Địa chỉ" });
    const content = () => document.getElementById(address.getAttribute("aria-controls")!)!;
    expect(address).toHaveAttribute("aria-expanded", "false");

    // The save comes back 422 on a field of the collapsed section; nobody clicks its title.
    rerender(<RecordFormPage title="Khách hàng mới" sections={withError} onSubmit={() => {}} onCancel={() => {}} />);
    expect(address).toHaveAttribute("aria-expanded", "true");
    expect(content()).not.toHaveAttribute("hidden");
    screen.getByRole("textbox", { name: "Tỉnh/thành" }).focus();

    // react-hook-form clears the error as the user types (reValidateMode onChange): the section must
    // not collapse over the field that has focus.
    rerender(<RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} />);
    expect(address).toHaveAttribute("aria-expanded", "true");
    expect(address.closest(".slds-section")).toHaveClass("slds-is-open");
    expect(content()).not.toHaveAttribute("hidden");
  });

  it("a section mounted with an error starts open", () => {
    render(
      <RecordFormPage title="Khách hàng mới" sections={[{ ...sections[1], hasError: true }]} onSubmit={() => {}} onCancel={() => {}} />,
    );
    expect(screen.getByRole("button", { name: "Địa chỉ" })).toHaveAttribute("aria-expanded", "true");
  });

  it("saving: footer buttons disabled, save shows the saving label, form is busy", () => {
    const { container } = render(
      <RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} saving />,
    );
    const footer = container.querySelector(".slds-docked-form-footer") as HTMLElement;
    expect(within(footer).getByRole("button", { name: "Huỷ" })).toBeDisabled();
    expect(within(footer).getByRole("button", { name: "Đang lưu…" })).toBeDisabled();
    expect(container.querySelector("form")).toHaveAttribute("aria-busy", "true");
  });

  it("form-level error is a scoped notification above the sections", () => {
    const { container } = render(
      <RecordFormPage
        title="Sửa khách hàng"
        sections={sections}
        notification={{ tone: "warning", body: "Bản ghi đã được người khác sửa." }}
        onSubmit={() => {}}
        onCancel={() => {}}
      />,
    );
    const note = screen.getByText("Bản ghi đã được người khác sửa.").closest(".slds-scoped-notification")!;
    expect(note).toHaveClass("slds-media", "slds-media_center", "slds-theme_warning");
    expect(note).toHaveAttribute("role", "status");
    expect(note.querySelector(".slds-media__figure .slds-icon_container")).toHaveClass("slds-icon-utility-warning");
    expect(within(note as HTMLElement).getByText("Cảnh báo")).toHaveClass("slds-assistive-text");
    const form = container.querySelector("form")!;
    expect(form.contains(note)).toBe(true);
    expect(note.compareDocumentPosition(form.querySelector(".slds-section")!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("the notification's live region is in the form before the message arrives", () => {
    const props = { title: "Sửa khách hàng", sections, onSubmit: () => {}, onCancel: () => {} };
    const { container, rerender } = render(<RecordFormPage {...props} />);
    // A live region inserted together with its text is often not read (VoiceOver, NVDA): the region
    // must already be there, empty and unstyled, when the failed save fills it.
    const region = container.querySelector("form > [role=status]");
    expect(region).not.toBeNull();
    expect(region).toBeEmptyDOMElement();
    expect(region!.className).toBe("");

    rerender(<RecordFormPage {...props} notification={{ tone: "error", body: "Không lưu được: mất kết nối." }} />);
    expect(container.querySelector("form > [role=status]")).toBe(region);
    expect(region).toHaveClass("slds-scoped-notification", "slds-theme_error");
    expect(region).toHaveTextContent("Không lưu được: mất kết nối.");
  });

  it("modal variant: modal footer instead of the docked footer, Esc closes", async () => {
    const onOpenChange = vi.fn();
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault());
    render(
      <RecordFormPage
        variant="modal"
        open
        onOpenChange={onOpenChange}
        title="Tạo nhanh khách hàng"
        sections={sections}
        onSubmit={onSubmit}
        onCancel={() => {}}
      />,
    );
    const dialog = screen.getByRole("dialog", { name: "Tạo nhanh khách hàng" });
    expect(dialog).toHaveClass("slds-modal");
    expect(document.querySelector(".slds-docked-form-footer")).toBeNull();
    const save = within(dialog).getByRole("button", { name: "Lưu" });
    expect(save.closest(".slds-modal__footer")).not.toBeNull();
    const form = dialog.querySelector("form")!;
    expect(form.closest(".slds-modal__content")).not.toBeNull();
    expect(save).toHaveAttribute("form", form.id);
    await userEvent.click(save);
    expect(onSubmit).toHaveBeenCalledOnce();

    await userEvent.keyboard("{Escape}");
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("modal variant: Huỷ, Esc and the X take one close path that reports both callbacks", async () => {
    const onOpenChange = vi.fn();
    const onCancel = vi.fn();
    const props = { variant: "modal", open: true, onOpenChange, title: "Tạo nhanh khách hàng", sections, onSubmit: () => {}, onCancel } as const;
    const { unmount } = render(<RecordFormPage {...props} />);
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(onOpenChange).toHaveBeenCalledExactlyOnceWith(false);
    expect(onCancel).toHaveBeenCalledOnce();
    unmount();

    for (const dismiss of [() => userEvent.keyboard("{Escape}"), () => userEvent.click(screen.getByRole("button", { name: "Huỷ và đóng" }))]) {
      onOpenChange.mockClear();
      onCancel.mockClear();
      const r = render(<RecordFormPage {...props} />);
      await dismiss();
      expect(onOpenChange).toHaveBeenCalledExactlyOnceWith(false);
      expect(onCancel).toHaveBeenCalledOnce();
      r.unmount();
    }

    // Opening from the trigger is not a cancel.
    onOpenChange.mockClear();
    onCancel.mockClear();
    const trigger = <button type="button">Tạo nhanh</button>;
    render(<RecordFormPage {...props} open={false} trigger={trigger} />);
    await userEvent.click(screen.getByRole("button", { name: "Tạo nhanh" }));
    expect(onOpenChange).toHaveBeenCalledExactlyOnceWith(true);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it("modal variant: a record form gets the medium modal (two-column sections need the width)", () => {
    render(
      <RecordFormPage variant="modal" open onOpenChange={() => {}} title="Tạo nhanh khách hàng" sections={sections} onSubmit={() => {}} onCancel={() => {}} />,
    );
    expect(screen.getByRole("dialog", { name: "Tạo nhanh khách hàng" })).toHaveClass("slds-modal", "slds-modal_medium");
  });

  it("modal variant: `status` stands in for the form in the SAME dialog until there is a form to fill", async () => {
    const onCancel = vi.fn();
    const props = { variant: "modal", open: true, onOpenChange: () => {}, title: "Tạo nhanh khách hàng", onSubmit: () => {}, onCancel } as const;
    const { rerender } = render(<RecordFormPage {...props} sections={[]} status={<p>Đang tải biểu mẫu…</p>} />);
    const dialog = screen.getByRole("dialog", { name: "Tạo nhanh khách hàng" });
    expect(within(dialog).getByText("Đang tải biểu mẫu…").closest(".slds-modal__content")).not.toBeNull();
    expect(dialog.querySelector("form")).toBeNull();
    // Nothing to save yet: the footer offers Huỷ only, and it still closes.
    const footer = dialog.querySelector(".slds-modal__footer") as HTMLElement;
    expect(within(footer).getAllByRole("button").map((b) => b.textContent)).toEqual(["Huỷ"]);
    await userEvent.click(within(footer).getByRole("button", { name: "Huỷ" }));
    expect(onCancel).toHaveBeenCalledOnce();

    // The form arrives: the same dialog node (one focus trap, one focus return), now with the form.
    rerender(<RecordFormPage {...props} sections={sections} />);
    expect(screen.getByRole("dialog", { name: "Tạo nhanh khách hàng" })).toBe(dialog);
    expect(dialog.querySelector("form")).not.toBeNull();
    expect(within(dialog).getByRole("button", { name: "Lưu" })).toBeInTheDocument();
  });

  it("formRef hands the caller the <form> (to move focus to the first invalid control)", () => {
    const formRef = { current: null as HTMLFormElement | null };
    render(<RecordFormPage title="Khách hàng mới" sections={sections} onSubmit={() => {}} onCancel={() => {}} formRef={formRef} />);
    expect(formRef.current).toBeInstanceOf(HTMLFormElement);
  });

  it("modal variant is controlled only: without `open` Radix would close the dialog mid-save", () => {
    // @ts-expect-error `open` and `onOpenChange` are required for variant="modal".
    const uncontrolled: RecordFormPageProps = { variant: "modal", title: "x", sections, onSubmit: () => {}, onCancel: () => {} };
    expect(uncontrolled.variant).toBe("modal");
  });

  it("modal variant: Esc and the close button wait while saving", async () => {
    const onOpenChange = vi.fn();
    render(
      <RecordFormPage
        variant="modal"
        open
        onOpenChange={onOpenChange}
        title="Tạo nhanh khách hàng"
        sections={sections}
        onSubmit={() => {}}
        onCancel={() => {}}
        saving
      />,
    );
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("button", { name: "Huỷ và đóng" }));
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Tạo nhanh khách hàng" })).toBeInTheDocument();
  });
});
