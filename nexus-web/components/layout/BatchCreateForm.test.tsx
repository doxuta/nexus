// BatchCreateForm on SLDS (specs/038 T025) — data-tables/inline-edit with every cell open for
// editing: one row per record, cells are the shared field widgets (labelled "<trường> – dòng N" for
// assistive tech), the server's per-row result marks the row (error icon + the failing cell).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { ApiError, type FieldMeta } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";

const seam = vi.hoisted(() => ({ createRecordsBulk: vi.fn() }));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
}));
vi.mock("@/lib/metadata/hooks", () => ({ useEntityLabel: () => "Cơ hội" }));

const FIELDS: FieldMeta[] = [
  { id: "f1", name: "name", label: "Tên cơ hội", fieldType: "text", isRequired: true },
  { id: "f2", name: "total", label: "Tổng", fieldType: "rollup" },
];

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listFields: vi.fn(async () => FIELDS),
    createRecordsBulk: (...args: unknown[]) => seam.createRecordsBulk(...args),
  };
});

import { BatchCreateForm } from "./BatchCreateForm";

beforeEach(() => seam.createRecordsBulk.mockReset());

describe("BatchCreateForm — inline-edit table (SLDS)", () => {
  it("draws the inline-edit table: header per field, open cells labelled by field + row, computed cells locked", async () => {
    const { container } = render(<BatchCreateForm entity="crm_opportunities" />);
    await screen.findByRole("textbox", { name: "Tên cơ hội – dòng 1" });
    const table = container.querySelector("table")!;
    expect(table).toHaveClass("slds-table", "slds-table_bordered", "slds-table_edit", "slds-table_fixed-layout");
    expect(table.closest(".slds-table_edit_container")).not.toBeNull();
    expect(within(table).getByRole("columnheader", { name: /Tên cơ hội/ })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 2" })).toBeInTheDocument();
    // rollup: no input, a locked "Tự động" cell
    const locked = within(table).getAllByText("Tự động")[0].closest("td")!;
    expect(locked).toHaveAttribute("aria-readonly", "true");
    expect(locked.querySelector("input")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Thêm dòng" }));
    expect(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 3" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Gửi 3 dòng" })).toHaveClass("slds-button_brand");
  });

  it("submit: payload per row in order; a failed row gets the error icon and its failing cell the error state", async () => {
    seam.createRecordsBulk.mockResolvedValue({
      mode: "partial",
      created: 1,
      failed: 1,
      skipped: 0,
      results: [
        { index: 0, status: "created", id: "r1" },
        { index: 1, status: "failed", code: "RECORD_REQUIRED_MISSING", error: "thiếu tên", details: { field: "name" } },
      ],
    });
    render(<BatchCreateForm entity="crm_opportunities" />);
    fireEvent.change(await screen.findByRole("textbox", { name: "Tên cơ hội – dòng 1" }), { target: { value: "Đơn A" } });
    fireEvent.click(screen.getByRole("radio", { name: "Từng dòng" }));
    fireEvent.click(screen.getByRole("button", { name: "Gửi 2 dòng" }));

    await waitFor(() => expect(seam.createRecordsBulk).toHaveBeenCalledTimes(1));
    const [entity, records, mode] = seam.createRecordsBulk.mock.calls[0];
    expect(entity).toBe("crm_opportunities");
    expect(mode).toBe("partial");
    expect(records).toEqual([{ name: "Đơn A" }, { name: "" }]); // required stays in: the server judges it

    expect(await screen.findByText("Dòng 2 có lỗi")).toHaveClass("slds-assistive-text");
    const failedInput = screen.getByRole("textbox", { name: "Tên cơ hội – dòng 2" });
    const failedCell = failedInput.closest("td")!;
    expect(failedCell).toHaveClass("slds-cell-edit", "slds-has-error");
    // The reason is where the user looks, not only in the status column far right of a wide table:
    // the failing cell's input is described by it (inline-edit "Cell edit — Error")…
    const msg = errorMessage(new ApiError(422, "RECORD_REQUIRED_MISSING", "thiếu tên"));
    const describedBy = failedInput.getAttribute("aria-describedby") ?? "";
    expect(describedBy.split(" ").map((id) => document.getElementById(id)?.textContent).join(" ")).toContain(msg);
    // …and the row's error button opens the "Resolve error" popover with it (inline-edit "Error - Row
    // level on save").
    const rowError = screen.getByRole("button", { name: "Dòng 2 có lỗi" });
    expect(rowError).toHaveClass("slds-button_icon-error");
    expect(rowError.closest("td")).toHaveClass("slds-cell-error");
    fireEvent.click(rowError);
    const pop = await screen.findByRole("dialog");
    expect(pop).toHaveClass("slds-popover", "slds-popover_error");
    // Portaled out of the table: SLDS gives a fixed-layout tbody its own stacking context
    // (translateZ(0)), so inside it the sticky header row would paint over the popover's top.
    expect(pop.closest("table")).toBeNull();
    expect(within(pop).getByText(msg)).toBeInTheDocument();
    fireEvent.keyDown(pop, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByText("Đã tạo")).toBeInTheDocument();
    expect(screen.getByText(/1 đã tạo · 1 lỗi · 0 bỏ qua/)).toBeInTheDocument();
    // Mixed partial result: drop the created rows, keep the ones to fix.
    fireEvent.click(screen.getByRole("button", { name: "Bỏ dòng đã tạo, giữ dòng cần sửa" }));
    expect(screen.queryByRole("textbox", { name: "Tên cơ hội – dòng 2" })).toBeNull();
    expect(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 1" })).toHaveValue("");
  });
});

// specs/052: the batch table runs the same rule as the record form, per row.
describe("BatchCreateForm — dependent picklist", () => {
  it("each row lists the children of its own parent, and a parent change blanks that row's child", async () => {
    const api = await import("@/lib/api");
    vi.mocked(api.listFields).mockResolvedValueOnce([
      { id: "fp", name: "province", label: "Tỉnh/TP", fieldType: "picklist", options: { values: ["Hà Nội", "Đà Nẵng"] } },
      {
        id: "fd", name: "district", label: "Quận/Huyện", fieldType: "picklist",
        options: { values: ["Ba Đình", "Hải Châu"], controllingField: "province", dependentValues: { "Hà Nội": ["Ba Đình"], "Đà Nẵng": ["Hải Châu"] } },
      },
    ]);
    render(<BatchCreateForm entity="crm_contacts" />);
    const pick = async (label: string, option: string) => {
      fireEvent.click(screen.getByRole("combobox", { name: label }));
      fireEvent.click(await screen.findByRole("option", { name: option }));
    };
    expect(await screen.findByRole("combobox", { name: "Quận/Huyện – dòng 1" })).toHaveAttribute("aria-disabled", "true");

    await pick("Tỉnh/TP – dòng 1", "Hà Nội");
    fireEvent.click(screen.getByRole("combobox", { name: "Quận/Huyện – dòng 1" }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Ba Đình"]);
    fireEvent.click(screen.getByRole("option", { name: "Ba Đình" }));

    await pick("Tỉnh/TP – dòng 1", "Đà Nẵng");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Quận/Huyện – dòng 1" })).not.toHaveTextContent("Ba Đình"));
    expect(screen.getByRole("combobox", { name: "Quận/Huyện – dòng 2" })).toHaveAttribute("aria-disabled", "true");
  });
});

// specs/048 T136: a row refused by a before-save rule (all-or-nothing mode) carries the rule's own
// sentence on the anchored cell, and never the error code.
describe("BatchCreateForm — a row refused by a before-save rule", () => {
  it("atomic: the vetoed row shows the rule's sentence on its cell, the others are skipped", async () => {
    seam.createRecordsBulk.mockResolvedValue({
      mode: "atomic",
      created: 0,
      failed: 1,
      skipped: 1,
      results: [
        { index: 0, status: "skipped", code: "BULK_ATOMIC_ABORTED", error: "aborted" },
        {
          index: 1,
          status: "failed",
          code: "RECORD_HOOK_VETO",
          error: "Tên cơ hội phải có mã khách",
          details: { field: "name", violations: [{ field: "name", message: "Tên cơ hội phải có mã khách" }] },
        },
      ],
    });
    render(<BatchCreateForm entity="crm_opportunities" />);
    fireEvent.change(await screen.findByRole("textbox", { name: "Tên cơ hội – dòng 1" }), { target: { value: "KH01 — Đơn A" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 2" }), { target: { value: "Đơn B" } });
    fireEvent.click(screen.getByRole("button", { name: "Gửi 2 dòng" }));
    await waitFor(() => expect(seam.createRecordsBulk).toHaveBeenCalledTimes(1));

    const failedInput = await screen.findByRole("textbox", { name: "Tên cơ hội – dòng 2" });
    await waitFor(() => expect(failedInput.closest("td")).toHaveClass("slds-has-error"));
    const describedBy = failedInput.getAttribute("aria-describedby") ?? "";
    expect(describedBy.split(" ").map((id) => document.getElementById(id)?.textContent).join(" ")).toContain("Tên cơ hội phải có mã khách");
    expect(document.body.textContent).not.toMatch(/RECORD_HOOK_VETO/);
  });
});

describe("BatchCreateForm — ?pf_<field> prefill (#304)", () => {
  it("every row, including the one added later, starts with the related list's parent value", async () => {
    window.history.pushState({}, "", "/crm_opportunities/batch?pf_name=L%E1%BB%9Bp%20A");
    try {
      render(<BatchCreateForm entity="crm_opportunities" />);
      expect(await screen.findByRole("textbox", { name: "Tên cơ hội – dòng 1" })).toHaveValue("Lớp A");
      expect(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 2" })).toHaveValue("Lớp A");
      fireEvent.click(screen.getByRole("button", { name: "Thêm dòng" }));
      expect(screen.getByRole("textbox", { name: "Tên cơ hội – dòng 3" })).toHaveValue("Lớp A");
    } finally {
      window.history.pushState({}, "", "/");
    }
  });
});
