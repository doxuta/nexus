import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { useState } from "react";
import { CalendarDays } from "lucide-react";
import { RadioGroup, RadioButtonGroup } from "./RadioGroup";

const PRIORITY = [
  { value: "low", label: "Thấp" },
  { value: "mid", label: "Trung bình" },
  { value: "high", label: "Cao" },
];

describe("RadioGroup (blueprint radio-group)", () => {
  it("fieldset[role=radiogroup] + legend + span.slds-radio > input + label > faux + text", () => {
    const { container } = render(<RadioGroup label="Mức ưu tiên" options={PRIORITY} />);
    const group = screen.getByRole("radiogroup", { name: "Mức ưu tiên" });
    expect(group.tagName).toBe("FIELDSET");
    expect(group).toHaveClass("slds-form-element");
    const radio = screen.getByRole("radio", { name: "Thấp" });
    expect(radio.parentElement!.tagName).toBe("SPAN");
    expect(radio.parentElement).toHaveClass("slds-radio");
    const label = radio.nextElementSibling!;
    expect(label).toHaveClass("slds-radio__label");
    expect(label.querySelector(".slds-radio_faux + .slds-form-element__label")).toHaveTextContent("Thấp");
    // all radios share one non-empty name so the browser groups them (nameless radios never group)
    const names = new Set(screen.getAllByRole("radio").map((r) => (r as HTMLInputElement).name));
    expect(names.size).toBe(1);
    expect([...names][0]).not.toBe("");
    expect(container.querySelector(".slds-form-element__control")).not.toBeNull();
  });

  it("arrow keys move the selection inside the group", async () => {
    render(<RadioGroup label="Mức ưu tiên" options={PRIORITY} defaultValue="low" />);
    const low = screen.getByRole("radio", { name: "Thấp" });
    low.focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("radio", { name: "Trung bình" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Trung bình" })).toHaveFocus();
  });

  it("two groups on one page stay separate: distinct names, arrows wrap inside their own group", async () => {
    render(
      <>
        <RadioGroup label="Nhóm A" options={PRIORITY} defaultValue="high" />
        <RadioGroup label="Nhóm B" options={PRIORITY} defaultValue="mid" />
      </>,
    );
    const a = screen.getByRole("radiogroup", { name: "Nhóm A" });
    const b = screen.getByRole("radiogroup", { name: "Nhóm B" });
    const nameOf = (g: HTMLElement) => (g.querySelector("input") as HTMLInputElement).name;
    expect(nameOf(a)).not.toBe("");
    expect(nameOf(b)).not.toBe("");
    expect(nameOf(a)).not.toBe(nameOf(b));
    within(a).getByRole("radio", { name: "Cao" }).focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(within(a).getByRole("radio", { name: "Thấp" })).toBeChecked();
    expect(within(a).getByRole("radio", { name: "Thấp" })).toHaveFocus();
    expect(within(b).getByRole("radio", { name: "Trung bình" })).toBeChecked();
    expect(within(b).getByRole("radio", { name: "Thấp" })).not.toBeChecked();
  });

  it("required → aria-required + slds-is-required; error after the control, describing each radio", () => {
    const { container } = render(
      <RadioGroup label="Mức ưu tiên" options={PRIORITY} required error="Chọn một mức." />,
    );
    const group = screen.getByRole("radiogroup", { name: /Mức ưu tiên/ });
    expect(group).toHaveAttribute("aria-required", "true");
    expect(group).toHaveClass("slds-has-error");
    screen.getAllByRole("radio").forEach((r) => expect(r).toHaveAccessibleDescription("Chọn một mức."));
    expect(container.firstElementChild!.lastElementChild).toHaveClass("slds-form-element__help");
  });

  it("disabled group and disabled option", () => {
    const { rerender } = render(<RadioGroup label="Mức" options={PRIORITY} disabled />);
    screen.getAllByRole("radio").forEach((r) => expect(r).toBeDisabled());
    rerender(<RadioGroup label="Mức" options={[...PRIORITY, { value: "x", label: "Khẩn", disabled: true }]} />);
    expect(screen.getByRole("radio", { name: "Khẩn" })).toBeDisabled();
    expect(screen.getByRole("radio", { name: "Cao" })).toBeEnabled();
  });

  it("controlled: onValueChange", async () => {
    const seen: string[] = [];
    function Controlled() {
      const [v, setV] = useState("low");
      return (
        <RadioGroup
          label="Mức"
          options={PRIORITY}
          value={v}
          onValueChange={(next) => {
            seen.push(next);
            setV(next);
          }}
        />
      );
    }
    render(<Controlled />);
    await userEvent.click(screen.getByRole("radio", { name: "Cao" }));
    expect(seen).toEqual(["high"]);
    expect(screen.getByRole("radio", { name: "Cao" })).toBeChecked();
  });

  it("react-hook-form register spreads onto every radio", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { priority: "mid" } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <RadioGroup label="Mức" options={PRIORITY} {...register("priority")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    expect(screen.getByRole("radio", { name: "Trung bình" })).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: "Cao" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ priority: "high" });
  });
});

describe("RadioButtonGroup (blueprint radio-button-group)", () => {
  it("div.slds-radio_button-group > span.slds-button.slds-radio_button > input + label > faux(text)", () => {
    render(<RadioButtonGroup label="Chế độ xem" options={[{ value: "day", label: "Ngày", icon: CalendarDays }, { value: "week", label: "Tuần" }]} />);
    const group = screen.getByRole("radiogroup", { name: "Chế độ xem" });
    expect(group.querySelector(".slds-form-element__control > .slds-radio_button-group")).not.toBeNull();
    const day = screen.getByRole("radio", { name: "Ngày" });
    expect(day.parentElement).toHaveClass("slds-button", "slds-radio_button");
    const faux = day.nextElementSibling!.querySelector(".slds-radio_faux")!;
    expect(day.nextElementSibling).toHaveClass("slds-radio_button__label");
    expect(faux).toHaveTextContent("Ngày");
    expect(faux.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_left");
  });

  it("arrow keys move the selection", async () => {
    render(<RadioButtonGroup label="Chế độ xem" options={PRIORITY} defaultValue="mid" />);
    screen.getByRole("radio", { name: "Trung bình" }).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("radio", { name: "Cao" })).toBeChecked();
  });

  it("labelHidden keeps the legend for assistive tech only (matrix cells where the headers label it)", () => {
    render(<RadioButtonGroup label="Tên · Kinh doanh" labelHidden options={PRIORITY} defaultValue="low" />);
    const group = screen.getByRole("radiogroup", { name: "Tên · Kinh doanh" });
    expect(group.querySelector("legend")).toHaveClass("slds-form-element__legend", "slds-assistive-text");
  });

  it("error line sits INSIDE the control, as in the blueprint", () => {
    const { container } = render(
      <RadioButtonGroup label="Chế độ xem" options={PRIORITY} required error="Chọn một chế độ." />,
    );
    const help = container.querySelector(".slds-form-element__help")!;
    expect(help.parentElement).toHaveClass("slds-form-element__control");
    screen.getAllByRole("radio").forEach((r) => expect(r).toHaveAccessibleDescription("Chọn một chế độ."));
  });
});

describe("RadioGroup in a record form (blueprint form-element/record-detail edit-stacked)", () => {
  it("editing reaches the fieldset as slds-is-editing", () => {
    render(<RadioGroup label="Mức ưu tiên" options={PRIORITY} layout="stacked" editing />);
    expect(screen.getByRole("radiogroup", { name: "Mức ưu tiên" })).toHaveClass("slds-form-element_stacked", "slds-is-editing");
  });
});
