import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { Textarea } from "./Textarea";

describe("Textarea (blueprint textarea)", () => {
  it("renders slds-textarea linked to its label; required is native", () => {
    render(<Textarea label="Ghi chú" required placeholder="Nhập ghi chú…" />);
    const ta = screen.getByRole("textbox", { name: /Ghi chú/ });
    expect(ta.tagName).toBe("TEXTAREA");
    expect(ta).toHaveClass("slds-textarea");
    expect(ta).toBeRequired();
  });

  it("error: slds-has-error + aria-describedby → message", () => {
    render(<Textarea label="Mô tả" error="Nhập giá trị." />);
    const ta = screen.getByRole("textbox", { name: "Mô tả" });
    expect(ta).toHaveAttribute("aria-invalid", "true");
    expect(ta).toHaveAccessibleDescription("Nhập giá trị.");
    expect(ta.closest(".slds-form-element")).toHaveClass("slds-has-error");
  });

  it("disabled is native", () => {
    render(<Textarea label="Mô tả" disabled />);
    expect(screen.getByRole("textbox", { name: "Mô tả" })).toBeDisabled();
  });

  it("staticValue renders the blueprint's read-only state (span label, bottom border, <p>), lines as <br>", () => {
    const { container } = render(<Textarea label="Mô tả" staticValue={"Khách hàng thân thiết từ 2019.\nƯu tiên email."} />);
    expect(screen.queryByRole("textbox")).toBeNull();
    const root = container.firstElementChild!;
    expect(root).not.toHaveClass("slds-form-element_readonly");
    expect(root.querySelector("span.slds-form-element__label")).toHaveTextContent("Mô tả");
    expect(root.querySelector(".slds-form-element__control")).toHaveClass("slds-border_bottom");
    const p = root.querySelector(".slds-form-element__static > p")!;
    expect(p.querySelectorAll("br")).toHaveLength(1);
    expect(p.innerHTML).toBe("Khách hàng thân thiết từ 2019.<br>Ưu tiên email.");
  });

  it("readOnly stays the native attribute, so register() still fills and reads the textarea", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { note: "Gọi lại thứ Hai" } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Textarea label="Ghi chú" readOnly {...register("note")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    const ta = screen.getByRole("textbox", { name: "Ghi chú" });
    expect(ta).toHaveAttribute("readonly");
    expect(ta).toHaveValue("Gọi lại thứ Hai");
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ note: "Gọi lại thứ Hai" });
  });

  it("works with react-hook-form register", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { note: "" } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Textarea label="Ghi chú" {...register("note")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    await userEvent.type(screen.getByRole("textbox", { name: "Ghi chú" }), "Gọi lại thứ Hai");
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ note: "Gọi lại thứ Hai" });
  });
});

describe("Textarea in a record form (blueprint form-element/record-detail edit-stacked)", () => {
  it("editing reaches the frame as slds-is-editing, never the native textarea", () => {
    render(<Textarea label="Mô tả" layout="stacked" editing />);
    const ta = screen.getByRole("textbox", { name: "Mô tả" });
    expect(ta.closest(".slds-form-element")).toHaveClass("slds-form-element_stacked", "slds-is-editing");
    expect(ta).not.toHaveAttribute("editing");
  });
});
