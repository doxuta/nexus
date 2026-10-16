// DateInput wire contract (dogfood 2026-07-15 — calendar-picked dates 422'd RECORD_INVALID_DATE):
// the MODEL value is the backend wire format (date `YYYY-MM-DD`; datetime naive-with-seconds
// `YYYY-MM-DDTHH:mm:ss` — record_service.go datetimeLayouts), while the button DISPLAYS the human
// DD/MM/YYYY (· HH:mm) form. parseWire also accepts the server's space-form echo, RFC3339, and the
// legacy DMY display strings the old build wrote into drafts.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { FieldInput, FieldRow, parseWire } from "@/components/fields/widgets";
import * as api from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listFields: vi.fn().mockResolvedValue([{ id: "f1", name: "name", label: "Tên", fieldType: "text" }]),
    listRecords: vi.fn().mockResolvedValue([{ id: "acc-1", version: 1, data: { name: "Acme Corp" } }]),
    getRecord: vi.fn().mockResolvedValue({ id: "acc-1", version: 1, data: { name: "Acme Corp" } }),
  };
});
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({ data: [{ name: "crm_accounts", label: "Khách hàng" }] }),
}));
import type { FieldSpec } from "@/lib/metadata";

const dateField = { name: "ngay_bat_dau", label: "Ngày bắt đầu", fieldType: "date" } as unknown as FieldSpec;
const dtField = { name: "hen_luc", label: "Hẹn lúc", fieldType: "datetime" } as unknown as FieldSpec;

describe("parseWire", () => {
  it("parses every accepted encoding to the same calendar day", () => {
    for (const [v, time] of [
      ["2026-08-01", ""],                    // wire date
      ["2026-08-01T09:30:00", "09:30"],      // wire datetime (naive, seconds)
      ["2026-08-01 09:30:00", "09:30"],      // server space-form echo
      ["2026-08-01T09:30:00Z", "09:30"],     // RFC3339
      ["01/08/2026", ""],                    // legacy display date
      ["01/08/2026 · 09:30", "09:30"],       // legacy display datetime
    ] as const) {
      const p = parseWire(v);
      expect(p, v).not.toBeNull();
      expect([p!.d.getFullYear(), p!.d.getMonth() + 1, p!.d.getDate()], v).toEqual([2026, 8, 1]);
      expect(p!.time, v).toBe(time);
    }
  });

  it("rejects garbage", () => {
    expect(parseWire("")).toBeNull();
    expect(parseWire("hôm qua")).toBeNull();
    expect(parseWire("13-2026-01")).toBeNull();
  });
});

describe("DateInput emits wire format (SLDS datepicker + timepicker)", () => {
  it("date pick → YYYY-MM-DD (never the display string)", () => {
    const onChange = vi.fn();
    render(<FieldInput field={dateField} value="" onChange={onChange} />);
    fireEvent.click(screen.getByRole("textbox", { name: "Ngày bắt đầu" })); // input click opens the calendar
    fireEvent.click(screen.getByText("15"));          // pick day 15 of the current month
    expect(onChange).toHaveBeenCalledTimes(1);
    const emitted = onChange.mock.calls[0][0] as string;
    expect(emitted).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    expect(emitted.endsWith("-15")).toBe(true);
  });

  it("datetime pick → Vietnam wall clock with offset YYYY-MM-DDTHH:mm:00+07:00 (#278)", () => {
    const onChange = vi.fn();
    render(<FieldInput field={dtField} value="" onChange={onChange} />);
    fireEvent.click(screen.getByRole("textbox", { name: "Ngày" }));
    fireEvent.click(screen.getByText("15"));
    const emitted = onChange.mock.calls[0][0] as string;
    expect(emitted).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:00\+07:00$/);
  });

  // #278: the store is UTC; a rule that set the deadline to 12:00 Hanoi time is stored 05:00 UTC and
  // the form must show 12:00 — and typing 09:00 must come back as 09:00 (a real +07:00 instant).
  it("shows a stored UTC datetime as Vietnam wall clock", () => {
    render(<FieldInput field={dtField} value="2026-08-01T05:00:00Z" onChange={() => {}} />);
    expect(screen.getByDisplayValue("12:00")).toBeInTheDocument();
  });

  it("rolls the day over when UTC evening is already the next day in Vietnam", () => {
    render(<FieldInput field={dtField} value="2026-08-01T20:30:00Z" onChange={() => {}} />);
    expect(screen.getByDisplayValue("03:30")).toBeInTheDocument();
    expect(screen.getByDisplayValue("02/08/2026")).toBeInTheDocument();
  });

  it("displays a wire value as DD/MM/YYYY and a wire datetime with its time", () => {
    render(<FieldInput field={dateField} value="2026-08-01" onChange={() => {}} />);
    expect(screen.getByDisplayValue("01/08/2026")).toBeInTheDocument();
    render(<FieldInput field={dtField} value="2026-08-01 07:45:00" onChange={() => {}} />);
    expect(screen.getAllByDisplayValue("01/08/2026")).toHaveLength(2);
    expect(screen.getByDisplayValue("14:45")).toBeInTheDocument();
  });
});

// SLDS migration (specs/038 T024): FieldRow no longer draws its own label row — it hands the frame
// (label, required mark, help, error) to the SLDS control inside it, so the label is a real
// <label for> on the control (blueprint form-element/record-detail edit-stacked).
describe("FieldRow + FieldInput — one SLDS form element per field", () => {
  const textField = { name: "name", label: "Tên công ty", fieldType: "text", required: true } as unknown as FieldSpec;

  it("labels the control, marks it required, and puts the error on it", () => {
    const { container } = render(
      <FieldRow field={textField} error="Trường này là bắt buộc">
        <FieldInput field={textField} value="" onChange={() => {}} error />
      </FieldRow>,
    );
    const input = screen.getByRole("textbox", { name: /Tên công ty/ });
    expect(input).toHaveClass("slds-input");
    expect(input).toHaveAttribute("aria-invalid", "true");
    const el = container.querySelector(".slds-form-element")!;
    expect(el).toHaveClass("slds-form-element_stacked", "slds-is-editing", "slds-has-error");
    expect(el.querySelector("abbr.slds-required")).not.toBeNull();
    expect(screen.getByText("Trường này là bắt buộc")).toHaveClass("slds-form-element__help");
  });

  it("outside a FieldRow (a table cell) the label is for assistive tech only", () => {
    render(<FieldInput field={textField} value="" onChange={() => {}} a11yLabel="Tên công ty – dòng 2" />);
    const label = screen.getByText("Tên công ty – dòng 2");
    expect(label).toHaveClass("slds-assistive-text");
    expect(screen.getByRole("textbox", { name: "Tên công ty – dòng 2" })).toBeInTheDocument();
  });

  it("picklist: a human picks the Vietnamese label, the field stores the CODE", () => {
    const onChange = vi.fn();
    const pl = {
      name: "status", label: "Trạng thái", fieldType: "picklist",
      options: { values: ["New", "Won"], valueLabels: { New: "Mới", Won: "Thắng" } },
    } as unknown as FieldSpec;
    render(<FieldInput field={pl} value="" onChange={onChange} />);
    fireEvent.click(screen.getByRole("combobox", { name: "Trạng thái" }));
    fireEvent.click(screen.getByRole("option", { name: "Thắng" }));
    expect(onChange).toHaveBeenCalledWith("Won");
  });

  it("boolean: a standalone SLDS checkbox bound to true/false", () => {
    const onChange = vi.fn();
    const bool = { name: "active", label: "Đang hoạt động", fieldType: "boolean" } as unknown as FieldSpec;
    const { container } = render(<FieldInput field={bool} value={false} onChange={onChange} />);
    expect(container.querySelector(".slds-checkbox_standalone")).not.toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "Đang hoạt động" }));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("relation: focusing the field lists records without typing; choosing stores the id", async () => {
    const onChange = vi.fn();
    const rel = {
      name: "account", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "crm_accounts" },
    } as unknown as FieldSpec;
    render(<FieldInput field={rel} value="" onChange={onChange} />);
    const box = screen.getByRole("combobox", { name: "Khách hàng" });
    fireEvent.focus(box);
    fireEvent.click(box);
    fireEvent.click(await screen.findByRole("option", { name: /Acme Corp/ }));
    expect(onChange).toHaveBeenCalledWith("acc-1");
    expect(api.listRecords).toHaveBeenCalledWith("crm_accounts", "");
  });

  it("relation (#304): filterBy lists only the chosen parent's records; waits while the parent is empty", async () => {
    const dev = {
      name: "device", label: "Thiết bị", fieldType: "relation",
      options: { relatedEntity: "crm_accounts", filterBy: { field: "customer", parentField: "customer" } },
    } as unknown as FieldSpec;
    const fieldsByName = { customer: { name: "customer", label: "Khách", fieldType: "relation" } } as never;
    const { unmount } = render(<FieldInput field={dev} value="" onChange={vi.fn()} siblings={{ values: { customer: "" }, fieldsByName }} />);
    const blocked = screen.getByRole("combobox", { name: "Thiết bị" });
    expect(blocked).toBeDisabled();
    expect(blocked).toHaveAttribute("placeholder", "Chọn Khách trước");
    unmount();
    vi.mocked(api.listRecords).mockClear();
    render(<FieldInput field={dev} value="" onChange={vi.fn()} siblings={{ values: { customer: "c 1" }, fieldsByName }} />);
    const box = screen.getByRole("combobox", { name: "Thiết bị" });
    fireEvent.focus(box);
    fireEvent.click(box);
    await screen.findByRole("option", { name: /Acme Corp/ });
    expect(api.listRecords).toHaveBeenCalledWith("crm_accounts", "customer=c%201");
  });

  it("relation (#272): the option is named by the target table's isLabel field, not a guessed 'name'", async () => {
    vi.mocked(api.listFields).mockResolvedValue([
      { id: "f1", name: "name", label: "Tên", fieldType: "text" },
      { id: "f2", name: "ten_kh", label: "Tên khách", fieldType: "text", isLabel: true },
    ] as never);
    vi.mocked(api.listRecords).mockResolvedValue([
      { id: "acc-9", version: 1, data: { name: "legacy", ten_kh: "Công ty Hoa Sen" } },
    ] as never);
    const rel = {
      name: "account", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "crm_accounts" },
    } as unknown as FieldSpec;
    render(<FieldInput field={rel} value="" onChange={vi.fn()} />);
    const box = screen.getByRole("combobox", { name: "Khách hàng" });
    fireEvent.focus(box);
    fireEvent.click(box);
    try {
      expect(await screen.findByRole("option", { name: /Công ty Hoa Sen/ })).toBeInTheDocument();
    } finally {
      // restore the file-level defaults for later tests
      vi.mocked(api.listFields).mockResolvedValue([{ id: "f1", name: "name", label: "Tên", fieldType: "text" }] as never);
      vi.mocked(api.listRecords).mockResolvedValue([{ id: "acc-1", version: 1, data: { name: "Acme Corp" } }] as never);
    }
  });

  it.each([
    ["inferred isLabel: the relation's own labelField wins", false, "Mã KH-1"],
    ["explicit table choice: it beats the relation's labelField", true, "Công ty Hoa Sen"],
  ])("relation (#479): %s", async (_n, explicit, shown) => {
    vi.mocked(api.listFields).mockResolvedValue([
      { id: "f1", name: "ma", label: "Mã", fieldType: "text" },
      { id: "f2", name: "ten_kh", label: "Tên khách", fieldType: "text", isLabel: true, isLabelExplicit: explicit },
    ] as never);
    vi.mocked(api.listRecords).mockResolvedValue([
      { id: "acc-9", version: 1, data: { ma: "Mã KH-1", ten_kh: "Công ty Hoa Sen" } },
    ] as never);
    const rel = {
      name: "account", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "crm_accounts", labelField: "ma" },
    } as unknown as FieldSpec;
    render(<FieldInput field={rel} value="" onChange={vi.fn()} />);
    const box = screen.getByRole("combobox", { name: "Khách hàng" });
    fireEvent.focus(box);
    fireEvent.click(box);
    try {
      expect(await screen.findByRole("option", { name: new RegExp(shown) })).toBeInTheDocument();
    } finally {
      vi.mocked(api.listFields).mockResolvedValue([{ id: "f1", name: "name", label: "Tên", fieldType: "text" }] as never);
      vi.mocked(api.listRecords).mockResolvedValue([{ id: "acc-1", version: 1, data: { name: "Acme Corp" } }] as never);
    }
  });
});

// Compound fieldsets (datetime date+time, polymorphic entity+record, rating stars): the fieldset's
// error line is the only message, so every control inside must be invalid AND described by it —
// otherwise a screen reader hears nothing on the control the user has to fix (blueprint
// datetime-picker error: the inputs carry aria-invalid + aria-describedby to the fieldset's line).
describe("compound fields put the fieldset's error on each control inside", () => {
  const ERR = "Bắt buộc nhập";
  const describedText = (el: Element) =>
    (el.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");

  it("datetime: date input and time combobox are invalid and read the error line", () => {
    const dt = { ...dtField, required: true } as unknown as FieldSpec;
    render(
      <FieldRow field={dt} error={ERR}>
        <FieldInput field={dt} value="" onChange={() => {}} error />
      </FieldRow>,
    );
    const date = screen.getByRole("textbox", { name: /Ngày/ });
    const time = screen.getByRole("combobox", { name: /Giờ/ });
    for (const el of [date, time]) {
      expect(el).toHaveAttribute("aria-invalid", "true");
      expect(describedText(el)).toContain(ERR);
    }
  });

  it("polymorphic: the entity combobox and the record lookup are invalid and read the error line", () => {
    const poly = {
      name: "regarding", label: "Liên quan", fieldType: "polymorphic_relation", required: true,
      options: { targetEntities: ["crm_accounts"] },
    } as unknown as FieldSpec;
    render(
      <FieldRow field={poly} error={ERR}>
        <FieldInput field={poly} value={null} onChange={() => {}} error />
      </FieldRow>,
    );
    const boxes = screen.getAllByRole("combobox");
    expect(boxes).toHaveLength(2); // one target → the entity is preselected and the lookup shows
    for (const el of boxes) {
      expect(el).toHaveAttribute("aria-invalid", "true");
      expect(describedText(el)).toContain(ERR);
    }
  });

  it("rating: each star reads the error line", () => {
    const rating = { name: "score", label: "Đánh giá", fieldType: "rating", required: true, options: { max: 3 } } as unknown as FieldSpec;
    render(
      <FieldRow field={rating} error={ERR}>
        <FieldInput field={rating} value={0} onChange={() => {}} error />
      </FieldRow>,
    );
    const stars = screen.getAllByRole("button", { name: /sao/ });
    expect(stars).toHaveLength(3);
    for (const el of stars) expect(describedText(el)).toContain(ERR);
  });

  it("a table cell with a message: the control is described by it (batch row error)", () => {
    const textField = { name: "name", label: "Tên", fieldType: "text" } as unknown as FieldSpec;
    render(<FieldInput field={textField} value="" onChange={() => {}} error="Thiếu tên" a11yLabel="Tên – dòng 2" />);
    const input = screen.getByRole("textbox", { name: "Tên – dòng 2" });
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(describedText(input)).toContain("Thiếu tên");
  });
});
