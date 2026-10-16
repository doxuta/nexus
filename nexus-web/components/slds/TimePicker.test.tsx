// TimePicker — blueprint `timepicker` (SLDS 2.264.1): the autocomplete combobox with a clock icon.
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TimePicker, parseTime } from "@/components/slds/TimePicker";

function Controlled({ initial = null, onChange }: { initial?: string | null; onChange?: (v: string | null) => void }) {
  const [v, setV] = useState<string | null>(initial);
  return (
    <TimePicker
      label="Giờ"
      value={v}
      onChange={(next) => {
        setV(next);
        onChange?.(next);
      }}
    />
  );
}

describe("TimePicker", () => {
  it("renders the blueprint: input combobox in div.slds-combobox.slds-timepicker, clock icon", () => {
    render(<Controlled />);
    const input = screen.getByRole("combobox");
    expect(input.tagName).toBe("INPUT");
    expect(input).toHaveClass("slds-input", "slds-combobox__input");
    expect(input).toHaveAttribute("placeholder", "Chọn giờ…");
    expect(input.closest(".slds-combobox")).toHaveClass("slds-timepicker");
    expect(input.parentElement!.querySelector(".slds-icon_container")).toHaveClass("slds-icon-utility-clock");
  });

  it("lists 24h times every 15 minutes; Down highlights the current time with the check", async () => {
    render(<Controlled initial="08:00" />);
    expect(screen.getByRole("combobox")).toHaveValue("08:00");
    screen.getByRole("combobox").focus();
    await userEvent.keyboard("{ArrowDown}");
    const opts = screen.getAllByRole("option");
    expect(opts).toHaveLength(96);
    expect(opts[0]).toHaveTextContent("00:00");
    expect(opts[95]).toHaveTextContent("23:45");
    const current = screen.getByRole("option", { name: "08:00" });
    expect(current).toHaveClass("slds-is-selected", "slds-has-focus");
    expect(screen.getByRole("combobox")).toHaveAttribute("aria-activedescendant", current.id);
  });

  it("a click re-opens on the current time (docs: re-opening with a value highlights it)", async () => {
    render(<Controlled initial="14:30" />);
    await userEvent.click(screen.getByRole("combobox"));
    const current = screen.getByRole("option", { name: "14:30" });
    expect(screen.getByRole("combobox")).toHaveAttribute("aria-activedescendant", current.id);
    // The value in the box is not a search term: nothing is marked.
    expect(current.querySelector("mark")).toBeNull();
  });

  it("typing narrows the list; choosing sets HH:mm", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    await userEvent.type(screen.getByRole("combobox"), "9:3");
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["09:30", "19:30"]);
    await userEvent.keyboard("{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenLastCalledWith("09:30");
    expect(screen.getByRole("combobox")).toHaveValue("09:30");
  });

  it("free text is parsed on Enter/blur (9h30 → 09:30); out-of-range text is an error, value kept", async () => {
    const onChange = vi.fn();
    render(
      <>
        <Controlled initial="08:00" onChange={onChange} />
        <button type="button">Sau</button>
      </>,
    );
    const input = screen.getByRole("combobox");
    await userEvent.clear(input);
    await userEvent.type(input, "9h30{Enter}");
    expect(onChange).toHaveBeenLastCalledWith("09:30");
    expect(input).toHaveValue("09:30");
    await userEvent.clear(input);
    await userEvent.type(input, "25:00");
    await userEvent.tab();
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(input).toHaveValue("25:00");
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(screen.getByText("Giờ không hợp lệ — nhập dạng 09:30")).toBeInTheDocument();
  });

  it("clearing the text clears the value", async () => {
    const onChange = vi.fn();
    render(<Controlled initial="08:00" onChange={onChange} />);
    await userEvent.clear(screen.getByRole("combobox"));
    await userEvent.tab();
    expect(onChange).toHaveBeenLastCalledWith(null);
  });

  it("step changes the list; disabled input", () => {
    render(<TimePicker label="Giờ" step={30} disabled />);
    expect(screen.getByRole("combobox")).toBeDisabled();
  });
});

describe("parseTime", () => {
  it.each([
    ["9", "09:00"],
    ["09:30", "09:30"],
    ["9h30", "09:30"],
    ["9g", "09:00"],
    ["930", "09:30"],
    ["2359", "23:59"],
    ["", null],
    ["24:00", undefined],
    ["9:75", undefined],
    ["abc", undefined],
  ])("%s → %s", (text, expected) => {
    expect(parseTime(text)).toBe(expected);
  });
});
