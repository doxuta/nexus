// DatePicker / DateRangePicker — blueprint `datepickers` base + range (SLDS 2.264.1) on react-day-picker.
// Keyboard per the datepickers docs: the grid is one focus stop; arrows move by day/week, Home/End to
// the row ends, PageUp/PageDown by month (Shift = year), Enter picks and closes, Esc closes; focus goes
// back to the input, never the calendar button. Opens on input click (focus stays) or on the button
// (focus goes to the selected day, else today).
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DatePicker, DateRangePicker, parseDisplayDate, type DateRange } from "@/components/slds/DatePicker";

beforeEach(() => {
  // Only Date is faked: Radix and user-event need real timers.
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(2026, 5, 18, 10, 0)); // Thu 18 June 2026
});
afterEach(() => vi.useRealTimers());

function Controlled({ initial = null, onChange, ...rest }: { initial?: string | null; onChange?: (v: string | null) => void; min?: string; max?: string }) {
  const [v, setV] = useState<string | null>(initial);
  return (
    <DatePicker
      label="Ngày hẹn"
      value={v}
      onChange={(next) => {
        setV(next);
        onChange?.(next);
      }}
      {...rest}
    />
  );
}

const button = () => screen.getByRole("button", { name: "Chọn ngày" });
const input = () => screen.getByRole("textbox", { name: "Ngày hẹn" });
async function openFromButton() {
  button().focus();
  await userEvent.keyboard("{Enter}");
  return screen.getByRole("dialog");
}
const cell = (label: string) => screen.getByRole("gridcell", { name: label });

describe("DatePicker", () => {
  it("renders the closed blueprint markup", () => {
    const { container } = render(<Controlled />);
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-form-element", "slds-dropdown-trigger", "slds-dropdown-trigger_click");
    expect(root).not.toHaveClass("slds-is-open");
    const control = input().parentElement!;
    expect(control).toHaveClass("slds-form-element__control", "slds-input-has-icon", "slds-input-has-icon_right");
    expect(input()).toHaveClass("slds-input");
    expect(button()).toHaveClass("slds-button", "slds-button_icon", "slds-input__icon", "slds-input__icon_right");
    expect(button()).toHaveAttribute("title", "Chọn ngày");
    const help = root.querySelector(".slds-form-element__help.slds-assistive-text")!;
    expect(help).toHaveTextContent("Định dạng: 31/12/2024");
    expect(input().getAttribute("aria-describedby")).toContain(help.id);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens from the button: blueprint calendar, Monday first, Vietnamese names, focus on today", async () => {
    const { container } = render(<Controlled />);
    const dialog = await openFromButton();
    expect(container.firstElementChild).toHaveClass("slds-is-open");
    expect(dialog).toHaveClass("slds-datepicker", "slds-dropdown", "slds-dropdown_left");
    expect(dialog).toHaveAttribute("aria-label", "Chọn ngày: Tháng 6");
    const filter = dialog.firstElementChild as HTMLElement;
    expect(filter).toHaveClass("slds-datepicker__filter", "slds-grid");
    expect(within(filter).getByRole("button", { name: "Tháng trước" })).toHaveClass("slds-button_icon-container");
    expect(within(filter).getByRole("button", { name: "Tháng sau" })).toHaveClass("slds-button_icon-container");
    const heading = within(filter).getByRole("heading", { level: 2 });
    expect(heading).toHaveTextContent("Tháng 6");
    expect(heading).toHaveAttribute("aria-live", "polite");
    expect(within(filter).getByRole("combobox", { name: "Chọn năm" })).toHaveValue("2026");
    const grid = within(dialog).getByRole("grid");
    expect(grid).toHaveClass("slds-datepicker__month");
    expect(grid).toHaveAttribute("aria-labelledby", heading.id);
    const heads = within(grid).getAllByRole("columnheader");
    expect(heads.map((h) => h.textContent)).toEqual(["T2", "T3", "T4", "T5", "T6", "T7", "CN"]);
    expect(heads[0].querySelector("abbr")).toHaveAttribute("title", "Thứ Hai");
    const today = cell("18 tháng 6 năm 2026");
    expect(today).toHaveClass("slds-is-today");
    expect(today).toHaveAttribute("aria-current", "date");
    expect(today).toHaveAttribute("tabindex", "0");
    expect(today.firstElementChild).toHaveClass("slds-day");
    expect(document.activeElement).toBe(today);
    expect(cell("1 tháng 7 năm 2026")).toHaveClass("slds-day_adjacent-month");
    expect(dialog.lastElementChild).toHaveClass("slds-button", "slds-align_absolute-center", "slds-text-link");
    expect(dialog.lastElementChild).toHaveTextContent("Hôm nay");
  });

  it("shows the value as dd/MM/yyyy and opens on it", async () => {
    render(<Controlled initial="2026-06-24" />);
    expect(input()).toHaveValue("24/06/2026");
    await openFromButton();
    const day = cell("24 tháng 6 năm 2026");
    expect(day).toHaveClass("slds-is-selected");
    expect(day).toHaveAttribute("aria-selected", "true");
    expect(document.activeElement).toBe(day);
  });

  it("arrows, PageDown and Enter: picks, closes and returns focus to the input", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    await openFromButton();
    await userEvent.keyboard("{ArrowRight}");
    expect(document.activeElement).toBe(cell("19 tháng 6 năm 2026"));
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(cell("26 tháng 6 năm 2026"));
    await userEvent.keyboard("{PageDown}");
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Tháng 7");
    expect(document.activeElement).toBe(cell("26 tháng 7 năm 2026"));
    await userEvent.keyboard("{Enter}");
    expect(onChange).toHaveBeenLastCalledWith("2026-07-26");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(input()).toHaveValue("26/07/2026");
    expect(document.activeElement).toBe(input());
  });

  it("Esc closes without picking and focuses the input", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    await openFromButton();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(input());
  });

  it("a click on the input opens the calendar but keeps focus in the input; clicking a day picks it", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    await userEvent.click(input());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(document.activeElement).toBe(input());
    await userEvent.click(cell("3 tháng 6 năm 2026"));
    expect(onChange).toHaveBeenLastCalledWith("2026-06-03");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("month buttons, year select and Today", async () => {
    const onChange = vi.fn();
    render(<Controlled onChange={onChange} />);
    const dialog = await openFromButton();
    await userEvent.click(within(dialog).getByRole("button", { name: "Tháng sau" }));
    expect(within(dialog).getByRole("heading", { level: 2 })).toHaveTextContent("Tháng 7");
    await userEvent.click(within(dialog).getByRole("button", { name: "Tháng trước" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Tháng trước" }));
    expect(within(dialog).getByRole("heading", { level: 2 })).toHaveTextContent("Tháng 5");
    await userEvent.selectOptions(within(dialog).getByRole("combobox", { name: "Chọn năm" }), "2027");
    expect(screen.getByRole("gridcell", { name: "1 tháng 5 năm 2027" })).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Hôm nay" }));
    expect(onChange).toHaveBeenLastCalledWith("2026-06-18");
  });

  it("typed dates are parsed on blur; an invalid one keeps the value and shows the error", async () => {
    const onChange = vi.fn();
    render(
      <>
        <Controlled initial="2026-06-24" onChange={onChange} />
        <button type="button">Sau</button>
      </>,
    );
    await userEvent.clear(input());
    await userEvent.type(input(), "3/7/2026");
    await userEvent.click(screen.getByRole("button", { name: "Sau" }));
    expect(onChange).toHaveBeenLastCalledWith("2026-07-03");
    expect(input()).toHaveValue("03/07/2026");
    await userEvent.clear(input());
    await userEvent.type(input(), "31/02/2026");
    await userEvent.click(screen.getByRole("button", { name: "Sau" }));
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(input()).toHaveValue("31/02/2026");
    expect(input()).toHaveAttribute("aria-invalid", "true");
    expect(input().closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(screen.getByText("Ngày không hợp lệ — nhập dạng 31/12/2024")).toBeInTheDocument();
    await userEvent.clear(input());
    await userEvent.click(screen.getByRole("button", { name: "Sau" }));
    expect(onChange).toHaveBeenLastCalledWith(null);
  });

  it("min/max: days outside are disabled and cannot be picked", async () => {
    const onChange = vi.fn();
    render(<Controlled min="2026-06-10" max="2026-06-20" onChange={onChange} />);
    await openFromButton();
    const early = cell("9 tháng 6 năm 2026");
    expect(early).toHaveClass("slds-disabled-text");
    expect(early).toHaveAttribute("aria-disabled", "true");
    await userEvent.click(early);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("clicking another field while the calendar is open leaves focus on that field", async () => {
    // FocusScope hands focus back in a setTimeout(0), after the click has already moved it.
    const flush = () => act(() => new Promise((r) => setTimeout(r, 20)));
    render(
      <>
        <Controlled />
        <input aria-label="Khác" />
      </>,
    );
    const other = screen.getByRole("textbox", { name: "Khác" });
    await userEvent.click(input());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.click(other);
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(other);
    // Opened from the button (focus in the grid): same.
    await openFromButton();
    await userEvent.click(other);
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(other);
  });

  // Tab looping inside the open calendar is Radix FocusScope's (loop): happy-dom's TreeWalker returns
  // no tabbables, so it is verified in the browser (puppeteer), not here.

  it("disabled: input and button disabled", () => {
    render(<DatePicker label="Ngày hẹn" disabled />);
    expect(input()).toBeDisabled();
    expect(button()).toBeDisabled();
  });
});

describe("DateRangePicker", () => {
  function Range({ initial, onChange }: { initial: DateRange; onChange?: (v: DateRange) => void }) {
    const [v, setV] = useState<DateRange>(initial);
    return (
      <DateRangePicker
        label="Thời gian"
        value={v}
        onChange={(next) => {
          setV(next);
          onChange?.(next);
        }}
      />
    );
  }

  it("blueprint: compound form, fieldset + legend, two date fields in one row", () => {
    const { container } = render(<Range initial={{ from: null, to: null }} />);
    expect(container.firstElementChild).toHaveClass("slds-form", "slds-form_compound");
    const fieldset = screen.getByRole("group", { name: "Thời gian" });
    expect(fieldset.tagName).toBe("FIELDSET");
    expect(fieldset.querySelector("legend")).toHaveClass("slds-form-element__label", "slds-form-element__legend");
    const row = fieldset.querySelector(".slds-form-element__control > .slds-form-element__group > .slds-form-element__row")!;
    expect(row.children).toHaveLength(2);
    expect(screen.getByRole("textbox", { name: "Từ ngày" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Đến ngày" })).toBeInTheDocument();
  });

  it("shows the range across weeks with the blueprint row/day classes", async () => {
    render(<Range initial={{ from: "2026-06-24", to: "2026-06-30" }} />);
    const buttons = screen.getAllByRole("button", { name: "Chọn ngày" });
    buttons[1].focus();
    await userEvent.keyboard("{Enter}");
    const grid = screen.getByRole("grid");
    expect(grid).toHaveAttribute("aria-multiselectable", "true");
    for (const d of [24, 25, 28, 29, 30]) {
      const c = cell(`${d} tháng 6 năm 2026`);
      expect(c).toHaveClass("slds-is-selected", "slds-is-selected-multi");
      expect(c).toHaveAttribute("aria-selected", "true");
      expect(c.closest("tr")).toHaveClass("slds-has-multi-row-selection");
    }
    expect(cell("23 tháng 6 năm 2026")).not.toHaveClass("slds-is-selected");
    // The end picker cannot go before the start.
    expect(cell("23 tháng 6 năm 2026")).toHaveAttribute("aria-disabled", "true");
  });

  it("start only: that day selected, its row slds-has-multi-selection; picking the end reports both", async () => {
    const onChange = vi.fn();
    render(<Range initial={{ from: "2026-06-24", to: null }} onChange={onChange} />);
    screen.getAllByRole("button", { name: "Chọn ngày" })[1].focus();
    await userEvent.keyboard("{Enter}");
    const start = cell("24 tháng 6 năm 2026");
    expect(start).toHaveClass("slds-is-selected");
    expect(start).not.toHaveClass("slds-is-selected-multi");
    expect(start.closest("tr")).toHaveClass("slds-has-multi-selection");
    await userEvent.click(cell("26 tháng 6 năm 2026"));
    expect(onChange).toHaveBeenLastCalledWith({ from: "2026-06-24", to: "2026-06-26" });
  });
  it("end only: that day selected, its row slds-has-multi-selection; the start calendar opens on its month", async () => {
    render(<Range initial={{ from: null, to: "2026-08-10" }} />);
    screen.getAllByRole("button", { name: "Chọn ngày" })[1].focus();
    await userEvent.keyboard("{Enter}");
    const end = cell("10 tháng 8 năm 2026");
    expect(end).toHaveClass("slds-is-selected");
    expect(end).toHaveAttribute("aria-selected", "true");
    expect(end).not.toHaveClass("slds-is-selected-multi");
    expect(end.closest("tr")).toHaveClass("slds-has-multi-selection");
    await userEvent.keyboard("{Escape}");
    screen.getAllByRole("button", { name: "Chọn ngày" })[0].focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Tháng 8");
    expect(cell("10 tháng 8 năm 2026")).toHaveClass("slds-is-selected");
  });

  it("required + error: one mark on the legend, both inputs required, invalid and described by the error", () => {
    const { container } = render(<DateRangePicker label="Thời gian" required error="Chọn khoảng ngày" />);
    expect(container.querySelectorAll("abbr.slds-required")).toHaveLength(1);
    expect(screen.getByRole("group", { name: "Thời gian" }).querySelector("legend abbr.slds-required")).not.toBeNull();
    const help = screen.getByText("Chọn khoảng ngày");
    expect(help).toHaveClass("slds-form-element__help");
    expect(help.id).not.toBe("");
    for (const name of ["Từ ngày", "Đến ngày"]) {
      const box = screen.getByRole("textbox", { name });
      expect(box).toBeRequired();
      expect(box).toHaveAttribute("aria-invalid", "true");
      expect(box.getAttribute("aria-describedby")!.split(" ")).toContain(help.id);
      expect(box.closest(".slds-dropdown-trigger")).toHaveClass("slds-has-error");
    }
  });
});

describe("parseDisplayDate", () => {
  it.each([
    ["24/06/2026", [2026, 5, 24]],
    ["3/7/2026", [2026, 6, 3]],
    ["03-07-2026", [2026, 6, 3]],
    ["03.07.2026", [2026, 6, 3]],
    ["03072026", [2026, 6, 3]],
  ])("%s", (text, [y, m, d]) => {
    const date = parseDisplayDate(text)!;
    expect([date.getFullYear(), date.getMonth(), date.getDate()]).toEqual([y, m, d]);
  });
  it.each(["31/02/2026", "00/01/2026", "1/13/2026", "2026-06-24", "abc"])("%s is not a date", (text) => {
    expect(parseDisplayDate(text)).toBeUndefined();
  });
});
