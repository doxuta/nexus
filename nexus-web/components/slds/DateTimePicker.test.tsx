// DateTimePicker — blueprint `datetime-picker` (SLDS 2.264.1): a date field and a time field in one
// compound fieldset; the value is an ISO datetime with the local offset.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { formatISO } from "date-fns";
import { DateTimePicker } from "@/components/slds/DateTimePicker";

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(2026, 8, 20, 9, 0));
});
afterEach(() => vi.useRealTimers());

const ISO_OFFSET = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}([+-]\d{2}:\d{2}|Z)$/;

function Controlled({ initial = null, onChange, required }: { initial?: string | null; onChange?: (v: string | null) => void; required?: boolean }) {
  const [v, setV] = useState<string | null>(initial);
  return (
    <>
      <DateTimePicker
        label="Lịch hẹn"
        required={required}
        value={v}
        onChange={(next) => {
          setV(next);
          onChange?.(next);
        }}
      />
      <button type="button">Sau</button>
    </>
  );
}

const dateInput = () => screen.getByRole("textbox", { name: "Ngày" });
const timeInput = () => screen.getByRole("combobox", { name: "Giờ" });

describe("DateTimePicker", () => {
  it("renders the blueprint: div.slds-form > fieldset.slds-form-element_compound > legend + row of date and time fields", () => {
    const { container } = render(<Controlled required />);
    expect(container.firstElementChild).toHaveClass("slds-form");
    const fieldset = screen.getByRole("group", { name: "Lịch hẹn" });
    expect(fieldset).toHaveClass("slds-form-element", "slds-form-element_compound");
    expect(fieldset.querySelector("legend")).toHaveClass("slds-form-element__label", "slds-form-element__legend");
    const row = fieldset.querySelector(".slds-form-element__control > .slds-form-element__group > .slds-form-element__row")!;
    expect(row.children).toHaveLength(2);
    expect(row.children[0]).toHaveClass("slds-form-element", "slds-dropdown-trigger", "slds-dropdown-trigger_click");
    expect(dateInput()).toHaveAttribute("placeholder", "Chọn ngày…");
    expect(timeInput().closest(".slds-combobox")).toHaveClass("slds-timepicker");
  });

  it("required: the legend carries the only mark; both inputs are required for assistive tech", () => {
    const { container } = render(<Controlled required />);
    expect(container.querySelectorAll("abbr.slds-required")).toHaveLength(1);
    expect(container.querySelector("legend abbr.slds-required")).not.toBeNull();
    expect(dateInput()).toBeRequired();
    expect(timeInput()).toHaveAttribute("aria-required", "true");
  });

  it("error: blueprint error state — date form element has-error, inputs invalid and described by the line", () => {
    render(<DateTimePicker label="Lịch hẹn" error="Hãy nhập lịch hẹn." />);
    const fieldset = screen.getByRole("group", { name: "Lịch hẹn" });
    expect(fieldset).toHaveClass("slds-has-error");
    expect(fieldset).not.toHaveAttribute("aria-describedby");
    const help = screen.getByText("Hãy nhập lịch hẹn.");
    expect(screen.getAllByText("Hãy nhập lịch hẹn.")).toHaveLength(1);
    expect(help.parentElement).toBe(fieldset);
    expect(dateInput().closest(".slds-dropdown-trigger")).toHaveClass("slds-has-error");
    for (const box of [dateInput(), timeInput()]) {
      expect(box).toHaveAttribute("aria-invalid", "true");
      expect(box.getAttribute("aria-describedby")!.split(" ")).toContain(help.id);
    }
  });

  it("shows an ISO datetime in local date + time", () => {
    render(<Controlled initial={formatISO(new Date(2026, 8, 24, 14, 30))} />);
    expect(dateInput()).toHaveValue("24/09/2026");
    expect(timeInput()).toHaveValue("14:30");
  });

  it("reports an ISO datetime with offset once both parts are set", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    await userEvent.type(dateInput(), "24/09/2026");
    await userEvent.click(timeInput());
    expect(onChange).not.toHaveBeenCalled();
    await userEvent.type(timeInput(), "14:30{Enter}");
    const out = onChange.mock.lastCall![0] as string;
    expect(out).toMatch(ISO_OFFSET);
    expect(new Date(out).getTime()).toBe(new Date(2026, 8, 24, 14, 30).getTime());
  });

  it("a half-filled value is reported as null and flagged once focus leaves the pair", async () => {
    const onChange = vi.fn();
    render(<Controlled initial={formatISO(new Date(2026, 8, 24, 14, 30))} onChange={onChange} />);
    await userEvent.clear(timeInput());
    await userEvent.click(screen.getByRole("button", { name: "Sau" }));
    expect(onChange).toHaveBeenLastCalledWith(null);
    expect(dateInput()).toHaveValue("24/09/2026");
    const fieldset = screen.getByRole("group", { name: "Lịch hẹn" });
    expect(fieldset).toHaveClass("slds-has-error");
    const help = screen.getByText("Chọn đủ cả ngày và giờ");
    expect(help).toHaveClass("slds-form-element__help");
    for (const box of [dateInput(), timeInput()]) {
      expect(box).toHaveAttribute("aria-invalid", "true");
      expect(box.getAttribute("aria-describedby")!.split(" ")).toContain(help.id);
    }
  });

  it("disabled: both parts", () => {
    render(<DateTimePicker label="Lịch hẹn" disabled />);
    expect(dateInput()).toBeDisabled();
    expect(timeInput()).toBeDisabled();
  });
});
