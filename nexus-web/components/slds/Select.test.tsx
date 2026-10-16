import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { Select } from "./Select";

const STAGES = [
  { value: "new", label: "Mới" },
  { value: "won", label: "Thắng" },
  { value: "lost", label: "Thua", disabled: true },
];

describe("Select (blueprint select)", () => {
  it("wraps a native select in slds-select_container with an empty placeholder option", () => {
    render(<Select label="Giai đoạn" options={STAGES} />);
    const select = screen.getByRole("combobox", { name: "Giai đoạn" });
    expect(select).toHaveClass("slds-select");
    expect(select.parentElement).toHaveClass("slds-select_container");
    const opts = screen.getAllByRole("option");
    expect(opts[0]).toHaveValue("");
    expect(opts[0]).toHaveTextContent("Chọn…");
    expect(opts).toHaveLength(4);
    expect(screen.getByRole("option", { name: "Thua" })).toBeDisabled();
  });

  it("placeholder={false} drops the empty option; custom placeholder text", () => {
    const { rerender } = render(<Select label="Giai đoạn" options={STAGES} placeholder={false} />);
    expect(screen.getAllByRole("option")).toHaveLength(3);
    rerender(<Select label="Giai đoạn" options={STAGES} placeholder="-- Giai đoạn --" />);
    expect(screen.getAllByRole("option")[0]).toHaveTextContent("-- Giai đoạn --");
  });

  it("multiple drops the container and the placeholder (blueprint select-multiple-narrow)", () => {
    render(<Select label="Kênh" options={STAGES} multiple />);
    const select = screen.getByRole("listbox", { name: "Kênh" });
    expect(select.parentElement).toHaveClass("slds-form-element__control");
    expect(screen.getAllByRole("option")).toHaveLength(3);
  });

  it("required, disabled, error", () => {
    render(<Select label="Giai đoạn" options={STAGES} required disabled error="Chọn một giá trị." />);
    const select = screen.getByRole("combobox", { name: /Giai đoạn/ });
    expect(select).toBeRequired();
    expect(select).toBeDisabled();
    expect(select).toHaveAccessibleDescription("Chọn một giá trị.");
    expect(select).toHaveAttribute("aria-invalid", "true");
    expect(select.closest(".slds-form-element")).toHaveClass("slds-has-error");
  });

  // Keyboard (arrows / type-ahead) is the native <select>'s own behaviour; here we only check the
  // change reaches the caller.
  it("choosing an option fires onChange with its value", async () => {
    const onChange = vi.fn();
    render(<Select label="Giai đoạn" options={STAGES} onChange={(e) => onChange(e.target.value)} />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Giai đoạn" }), "won");
    expect(onChange).toHaveBeenLastCalledWith("won");
  });

  it("works with react-hook-form register", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { stage: "new" } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Select label="Giai đoạn" options={STAGES} {...register("stage")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    const select = screen.getByRole("combobox", { name: "Giai đoạn" });
    expect(select).toHaveValue("new");
    await userEvent.selectOptions(select, "won");
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ stage: "won" });
  });
});

describe("Select in a record form (blueprint form-element/record-detail edit-stacked)", () => {
  it("editing reaches the frame as slds-is-editing, never the native select", () => {
    render(<Select label="Giai đoạn" options={STAGES} layout="stacked" editing />);
    const select = screen.getByRole("combobox", { name: "Giai đoạn" });
    expect(select.closest(".slds-form-element")).toHaveClass("slds-form-element_stacked", "slds-is-editing");
    expect(select).not.toHaveAttribute("editing");
  });
});
