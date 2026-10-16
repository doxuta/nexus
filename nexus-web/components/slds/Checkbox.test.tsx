import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { useState } from "react";
import { Checkbox, CheckboxGroup } from "./Checkbox";

const DAYS = [
  { value: "mon", label: "Thứ Hai" },
  { value: "tue", label: "Thứ Ba" },
  { value: "wed", label: "Thứ Tư", disabled: true },
];

describe("Checkbox (blueprint checkbox)", () => {
  it("renders the custom DOM: .slds-checkbox > input + label > faux + label text", () => {
    const { container } = render(<Checkbox label="Gửi email chào mừng" />);
    const box = screen.getByRole("checkbox", { name: "Gửi email chào mừng" });
    const wrap = box.parentElement!;
    expect(wrap).toHaveClass("slds-checkbox");
    const label = wrap.querySelector("label.slds-checkbox__label")!;
    expect(label).toHaveAttribute("for", box.id);
    expect(label.querySelector(".slds-checkbox_faux + .slds-form-element__label")).toHaveTextContent(
      "Gửi email chào mừng",
    );
    expect(container.firstElementChild).toHaveClass("slds-form-element");
  });

  it("Space toggles it", async () => {
    render(<Checkbox label="Đồng ý" />);
    const box = screen.getByRole("checkbox", { name: "Đồng ý" });
    box.focus();
    await userEvent.keyboard(" ");
    expect(box).toBeChecked();
    await userEvent.keyboard(" ");
    expect(box).not.toBeChecked();
  });

  it("required puts the abbr right before the input; error describes the input", () => {
    render(<Checkbox label="Đồng ý điều khoản" required error="Bắt buộc phải đồng ý." />);
    const box = screen.getByRole("checkbox", { name: "Đồng ý điều khoản" });
    expect(box).toBeRequired();
    expect(box.previousElementSibling).toHaveClass("slds-required");
    expect(box).toHaveAccessibleDescription("Bắt buộc phải đồng ý.");
    expect(box.closest(".slds-form-element")).toHaveClass("slds-has-error");
  });

  it("indeterminate sets the DOM property (SLDS styles :indeterminate)", () => {
    const { rerender } = render(<Checkbox label="Chọn tất cả" indeterminate />);
    const box = screen.getByRole("checkbox", { name: "Chọn tất cả" }) as HTMLInputElement;
    expect(box.indeterminate).toBe(true);
    rerender(<Checkbox label="Chọn tất cả" indeterminate={false} />);
    expect(box.indeterminate).toBe(false);
  });

  it("disabled is native", () => {
    render(<Checkbox label="Khoá" disabled defaultChecked />);
    const box = screen.getByRole("checkbox", { name: "Khoá" });
    expect(box).toBeDisabled();
    expect(box).toBeChecked();
  });

  it("standalone variant: form-element label on top, span.slds-checkbox_standalone", () => {
    const { container } = render(<Checkbox label="Cần đánh giá bảo mật?" variant="standalone" layout="stacked" />);
    const box = screen.getByRole("checkbox", { name: "Cần đánh giá bảo mật?" });
    const wrap = box.parentElement!;
    expect(wrap.tagName).toBe("SPAN");
    expect(wrap).toHaveClass("slds-checkbox", "slds-checkbox_standalone");
    expect(box.nextElementSibling).toHaveClass("slds-checkbox_faux");
    expect(container.querySelector("label.slds-form-element__label")).toHaveAttribute("for", box.id);
    expect(container.firstElementChild).toHaveClass("slds-form-element_stacked");
  });

  it("react-hook-form register → boolean", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { vip: false } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Checkbox label="Khách VIP" {...register("vip")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Khách VIP" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ vip: true });
  });
});

describe("CheckboxGroup (blueprint checkbox — group)", () => {
  it("fieldset + legend; required → slds-is-required; error after the control", () => {
    const { container } = render(
      <CheckboxGroup label="Ngày học" name="days" options={DAYS} required error="Chọn ít nhất một ngày." />,
    );
    const fieldset = container.firstElementChild!;
    expect(fieldset.tagName).toBe("FIELDSET");
    expect(fieldset).toHaveClass("slds-form-element", "slds-is-required", "slds-has-error");
    expect(screen.getByRole("group", { name: /Ngày học/ })).toBe(fieldset);
    const boxes = screen.getAllByRole("checkbox");
    expect(boxes).toHaveLength(3);
    boxes.forEach((b) => expect(b).toHaveAccessibleDescription("Chọn ít nhất một ngày."));
    expect(screen.getByRole("checkbox", { name: "Thứ Tư" })).toBeDisabled();
    expect(fieldset.lastElementChild).toHaveClass("slds-form-element__help");
  });

  it("controlled: onValueChange gets the next array", async () => {
    const seen: string[][] = [];
    function Controlled() {
      const [v, setV] = useState<string[]>(["mon"]);
      return (
        <CheckboxGroup
          label="Ngày học"
          name="days"
          options={DAYS}
          value={v}
          onValueChange={(next) => {
            seen.push(next);
            setV(next);
          }}
        />
      );
    }
    render(<Controlled />);
    expect(screen.getByRole("checkbox", { name: "Thứ Hai" })).toBeChecked();
    await userEvent.click(screen.getByRole("checkbox", { name: "Thứ Ba" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Thứ Hai" }));
    expect(seen).toEqual([["mon", "tue"], ["tue"]]);
    expect(screen.getByRole("checkbox", { name: "Thứ Hai" })).not.toBeChecked();
  });

  it("react-hook-form register spreads onto every checkbox → string[]", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm<{ days: string[] }>({ defaultValues: { days: ["tue"] } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <CheckboxGroup label="Ngày học" options={DAYS} {...register("days")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    expect(screen.getByRole("checkbox", { name: "Thứ Ba" })).toBeChecked();
    await userEvent.click(screen.getByRole("checkbox", { name: "Thứ Hai" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0].days.sort()).toEqual(["mon", "tue"]);
  });
});

describe("Checkbox in a record form (blueprint form-element/horizontal edit-horizontal)", () => {
  it("editing reaches the frame as slds-is-editing (standalone box and group), never the native input", () => {
    const { container, rerender } = render(
      <Checkbox label="Cần đánh giá bảo mật?" variant="standalone" layout="horizontal" editing />,
    );
    expect(container.firstElementChild).toHaveClass("slds-form-element_horizontal", "slds-is-editing");
    expect(screen.getByRole("checkbox")).not.toHaveAttribute("editing");
    rerender(<CheckboxGroup label="Ngày học" options={DAYS} layout="horizontal" editing />);
    expect(screen.getByRole("group", { name: "Ngày học" })).toHaveClass("slds-is-editing");
  });
});
