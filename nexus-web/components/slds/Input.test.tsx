import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { useState } from "react";
import { Search } from "lucide-react";
import { Input } from "./Input";

describe("Input (blueprint input)", () => {
  it("links label and input; required is native", () => {
    render(<Input label="Tên" required placeholder="Nhập tên…" />);
    const input = screen.getByRole("textbox", { name: /Tên/ });
    expect(input).toHaveClass("slds-input");
    expect(input).toBeRequired();
    expect(input.closest(".slds-form-element__control")).not.toBeNull();
  });

  it("error: aria-invalid + aria-describedby → help line", () => {
    render(<Input label="Email" error="Nhập giá trị." />);
    const input = screen.getByRole("textbox", { name: "Email" });
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveAccessibleDescription("Nhập giá trị.");
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
  });

  it("inline help describes the input without marking it invalid", () => {
    render(<Input label="Điện thoại" help="VD: 0912 345 678" />);
    const input = screen.getByRole("textbox", { name: "Điện thoại" });
    expect(input).toHaveAccessibleDescription("VD: 0912 345 678");
    expect(input).not.toHaveAttribute("aria-invalid");
  });

  it("left icon → slds-input-has-icon_left with the glyph as slds-input__icon", () => {
    render(<Input label="Tìm" iconLeft={Search} />);
    const control = screen.getByRole("textbox", { name: "Tìm" }).parentElement!;
    expect(control).toHaveClass("slds-input-has-icon", "slds-input-has-icon_left");
    const svg = control.querySelector("svg")!;
    expect(svg).toHaveClass("slds-icon", "slds-input__icon", "slds-input__icon_left", "nx-lucide");
    expect(svg).toHaveAttribute("aria-hidden", "true");
  });

  it("left icon + clear → _left-right; clear empties the field, fires onChange and refocuses", async () => {
    const onChange = vi.fn();
    function Controlled() {
      const [v, setV] = useState("Hà Nội");
      return (
        <>
          <Input
            label="Tỉnh"
            iconLeft={Search}
            clearable
            value={v}
            onChange={(e) => {
              onChange(e.target.value);
              setV(e.target.value);
            }}
          />
          <output>{v}</output>
        </>
      );
    }
    render(<Controlled />);
    const input = screen.getByRole("textbox", { name: "Tỉnh" });
    expect(input.parentElement).toHaveClass("slds-input-has-icon_left-right");
    const clear = screen.getByRole("button", { name: "Xoá nội dung" });
    expect(clear).toHaveClass("slds-button", "slds-button_icon", "slds-input__icon", "slds-input__icon_right");
    expect(clear).toHaveAttribute("type", "button");
    await userEvent.click(clear);
    // The parent's state must follow, not just the DOM (the native setter alone empties the DOM).
    expect(onChange).toHaveBeenCalledExactlyOnceWith("");
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    expect(input).toHaveValue("");
    expect(input).toHaveFocus();
  });

  it("loading → icon group right with spinner + clear button", () => {
    render(<Input label="Tìm" iconLeft={Search} clearable loading />);
    const control = screen.getByRole("textbox", { name: "Tìm" }).parentElement!;
    expect(control).toHaveClass("slds-input-has-icon_left-right", "slds-input-has-icon_group-right");
    const group = control.querySelector(".slds-input__icon-group.slds-input__icon-group_right")!;
    expect(group.querySelector('[role="status"].slds-spinner.slds-input__spinner')).toHaveTextContent("Đang tải");
    expect(group.querySelector("button.slds-input__icon_right")).not.toBeNull();
  });

  it("fixed text addons: slds-input-has-fixed-addon + aria-labelledby chain", () => {
    render(<Input label="Giá" fixedTextLeft="₫" fixedTextRight="VND" />);
    const input = screen.getByRole("textbox", { name: "Giá ₫ VND" });
    expect(input.parentElement).toHaveClass("slds-input-has-fixed-addon");
    expect(input.parentElement!.querySelectorAll(".slds-form-element__addon")).toHaveLength(2);
  });

  it("numbers align right with tabular digits", () => {
    render(<Input label="Số lượng" type="number" />);
    const input = screen.getByRole("spinbutton", { name: "Số lượng" });
    expect(input).toHaveClass("slds-text-align_right", "tabular-nums");
  });

  it("disabled and readOnly are native; readOnly hides the clear button", () => {
    const { rerender } = render(<Input label="Mã" disabled clearable />);
    expect(screen.getByRole("textbox", { name: "Mã" })).toBeDisabled();
    expect(screen.queryByRole("button")).toBeNull();
    rerender(<Input label="Mã" readOnly clearable defaultValue="KH-001" />);
    expect(screen.getByRole("textbox", { name: "Mã" })).toHaveAttribute("readonly");
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("labelHidden keeps the name for assistive tech", () => {
    render(<Input label="Tìm kiếm" labelHidden />);
    const label = screen.getByText("Tìm kiếm");
    expect(label).toHaveClass("slds-assistive-text");
    expect(screen.getByRole("textbox", { name: "Tìm kiếm" })).toBeInTheDocument();
  });

  it("works with react-hook-form register (ref reaches the native input, clear updates the form)", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit, watch } = useForm({ defaultValues: { name: "Cũ", city: "Huế" } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Input label="Tên" {...register("name")} />
          <Input label="Thành phố" clearable {...register("city")} />
          <output>{watch("city")}</output>
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    const name = screen.getByRole("textbox", { name: "Tên" });
    expect(name).toHaveValue("Cũ");
    await userEvent.clear(name);
    await userEvent.type(name, "Mới");
    expect(screen.getByRole("status")).toHaveTextContent("Huế");
    await userEvent.click(screen.getByRole("button", { name: "Xoá nội dung" }));
    // The form state is empty right after the click — before any blur could re-read the DOM.
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
    expect(screen.getByRole("textbox", { name: "Thành phố" })).toHaveFocus();
    // The clear button is type="button": clicking it must not submit, so onSubmit fires once.
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ name: "Mới", city: "" });
  });
});

describe("Input in a record form (blueprint form-element/horizontal edit-horizontal)", () => {
  it("editing reaches the frame as slds-is-editing, never the native input", () => {
    render(<Input label="Tên công ty" layout="horizontal" editing />);
    const input = screen.getByRole("textbox", { name: "Tên công ty" });
    expect(input.closest(".slds-form-element")).toHaveClass("slds-form-element_horizontal", "slds-is-editing");
    expect(input).not.toHaveAttribute("editing");
  });
});
