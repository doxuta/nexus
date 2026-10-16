// ExpressionBuilder — blueprint `expression` base / custom-logic (SLDS 2.264.1), FilterRow contract of
// lib/filterModel.ts (the FilterBuilder it replaces).
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { Controller, useForm } from "react-hook-form";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ExpressionBuilder, conditionText } from "@/components/slds/ExpressionBuilder";
import { filtersToQuery, type FilterField, type FilterGroup, type FilterMatch, type FilterRow } from "@/lib/filterModel";

vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listUserDirectory: vi.fn().mockResolvedValue([{ id: "u-9", fullName: "KTV Nguyễn A", email: "a@example.com" }]),
}));

const FIELDS: FilterField[] = [
  { name: "name", label: "Tên", fieldType: "text" },
  { name: "amount", label: "Giá trị HĐ", fieldType: "currency" },
  { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"], valueLabels: { new: "Mới", won: "Thắng" } } },
  { name: "vip", label: "Khách VIP", fieldType: "boolean" },
  { name: "closed", label: "Ngày chốt", fieldType: "date" },
  { name: "logo", label: "Logo", fieldType: "image" },
  { name: "ownerId", label: "Người phụ trách", fieldType: "user" },
];
const row = (id: string, field: string, op: string, value = ""): FilterRow => ({ id, field, op, value });

function Harness(props: { initial?: FilterRow[]; withMatch?: boolean; withGroups?: boolean; errors?: Record<string, string>; disabled?: boolean }) {
  const [rows, setRows] = useState<FilterRow[]>(props.initial ?? [row("1", "amount", "gt", "1000")]);
  const [match, setMatch] = useState<FilterMatch>("all");
  const [logic, setLogic] = useState("");
  const [groups, setGroups] = useState<FilterGroup[]>([]);
  return (
    <>
      <ExpressionBuilder
        fields={FIELDS}
        value={rows}
        onChange={setRows}
        errors={props.errors}
        disabled={props.disabled}
        {...(props.withMatch && { match, onMatchChange: setMatch, customLogic: logic, onCustomLogicChange: setLogic })}
        {...(props.withGroups && { groups, onGroupsChange: setGroups })}
      />
      <output data-testid="query">{filtersToQuery(rows)}</output>
      <output data-testid="groups">{JSON.stringify(groups.map((g) => g.rows.length))}</output>
    </>
  );
}

const query = () => screen.getByTestId("query").textContent;
const rows = () => Array.from(document.querySelectorAll<HTMLElement>("li.slds-expression__row"));
async function pick(combobox: HTMLElement, option: string) {
  await userEvent.click(combobox);
  await userEvent.click(screen.getByRole("option", { name: option }));
}
const comboboxIn = (el: HTMLElement, name: string) => within(el).getByRole("combobox", { name });

describe("ExpressionBuilder — person value (#449)", () => {
  it("picks the owner by name and stores the user id", async () => {
    render(<Harness initial={[row("1", "ownerId", "eq")]} />);
    await userEvent.click(screen.getByPlaceholderText("Chọn người…"));
    await userEvent.click(await screen.findByRole("option", { name: /KTV Nguyễn A/ }));
    expect(query()).toBe("ownerId%5Beq%5D=u-9");
  });
});

describe("ExpressionBuilder", () => {
  it("markup: slds-expression > title, ul > li.slds-expression__row > fieldset > legend + gutters grid; AND-only has no match select", () => {
    render(<Harness initial={[row("1", "amount", "gt", "1000"), row("2", "stage", "eq", "won")]} />);
    const root = document.querySelector(".slds-expression")!;
    expect(root.querySelector("h2.slds-expression__title")).toHaveTextContent("Điều kiện");
    expect(root.querySelector(".slds-expression__options")).toBeNull();
    const [first, second] = rows();
    const legend1 = first.querySelector("fieldset > legend.slds-expression__legend")!;
    expect(legend1.textContent).toBe("Điều kiện 1");
    expect(legend1.querySelector(".slds-assistive-text")).toHaveTextContent("Điều kiện 1");
    expect(second.querySelector("legend > span:not(.slds-assistive-text)")).toHaveTextContent("VÀ");
    const cols = first.querySelectorAll("fieldset > .slds-grid.slds-gutters_xx-small > .slds-col");
    expect(Array.from(cols).map((c) => c.className)).toEqual(["slds-col", "slds-col slds-grow-none", "slds-col", "slds-col slds-grow-none"]);
    const del = within(first).getByRole("button", { name: "Xoá điều kiện 1" });
    expect(del).toHaveClass("slds-button_icon-border-filled");
    // The blueprint's label holds a no-break space: an empty label collapses and lifts the button 4px.
    expect(del.closest(".slds-form-element")!.querySelector(".slds-form-element__label")!.textContent).toBe("\u00a0");
    expect(comboboxIn(first, "Trường")).toHaveTextContent("Giá trị HĐ");
    expect(comboboxIn(first, "Toán tử")).toHaveTextContent("lớn hơn");
    expect(within(first).getByRole("spinbutton", { name: "Giá trị" })).toHaveValue(1000);
    const buttons = root.lastElementChild!;
    expect(buttons).toHaveClass("slds-expression__buttons");
    expect(within(buttons as HTMLElement).getByRole("button", { name: "Thêm điều kiện" })).toHaveClass("slds-button_neutral");
    expect(within(buttons as HTMLElement).queryByRole("button", { name: "Thêm nhóm" })).toBeNull();
  });

  it("add a condition: operator and value wait for a field (disabled), picking a field defaults to 'bằng'", async () => {
    render(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Thêm điều kiện" }));
    const [r] = rows();
    expect(comboboxIn(r, "Toán tử")).toHaveAttribute("aria-disabled", "true");
    expect(within(r).getByRole("textbox", { name: "Giá trị" })).toBeDisabled();
    // Non-filterable types never reach the field list.
    await userEvent.click(comboboxIn(r, "Trường"));
    expect(screen.queryByRole("option", { name: "Logo" })).toBeNull();
    await userEvent.click(screen.getByRole("option", { name: "Tên" }));
    expect(comboboxIn(r, "Toán tử")).toHaveTextContent("bằng");
    await userEvent.type(within(r).getByRole("textbox", { name: "Giá trị" }), "Acme");
    expect(query()).toBe("name%5Beq%5D=Acme");
  });

  it("typed values keep the FilterBuilder encoding: picklist code, Có/Không, between 'lo,hi', null ops disable the value", async () => {
    render(<Harness initial={[row("1", "stage", "eq"), row("2", "vip", "eq"), row("3", "amount", "between"), row("4", "name", "eq")]} />);
    const [stage, vip, amount, name] = rows();
    await pick(comboboxIn(stage, "Giá trị"), "Thắng");
    await pick(comboboxIn(vip, "Giá trị"), "Có");
    await userEvent.type(within(amount).getByRole("spinbutton", { name: "Từ" }), "50");
    await userEvent.type(within(amount).getByRole("spinbutton", { name: "Đến" }), "200");
    expect(decodeURIComponent(query()!)).toBe("stage[eq]=won&vip[eq]=true&amount[between]=50,200");
    await pick(comboboxIn(name, "Toán tử"), "rỗng");
    expect(within(name).getByRole("textbox", { name: "Giá trị" })).toBeDisabled();
    expect(decodeURIComponent(query()!)).toContain("name[isnull]=1");
  });

  it("date equals offers the relative presets (server-resolved tokens) before a specific date", async () => {
    render(<Harness initial={[row("1", "closed", "eq")]} />);
    const select = within(rows()[0]).getByRole("combobox", { name: "Giá trị" }) as HTMLSelectElement;
    expect(select.tagName).toBe("SELECT");
    expect(within(rows()[0]).getByLabelText("Ngày")).toHaveAttribute("type", "date");
    await userEvent.selectOptions(select, "Tuần này");
    expect(query()).toBe("closed%5Beq%5D=this_week");
    expect(within(rows()[0]).queryByLabelText("Ngày")).toBeNull();
  });

  it("engine vocabulary (workflow conditions): the caller's valueless operators disable the value, relativeDates={false} keeps a plain date", () => {
    // The workflow engine has its own null operator ("changed" — did the field move?) and does not
    // resolve relative-date tokens, so a caller must be able to switch both off.
    const WF_OPS = ["eq", "gt", "isnull", "changed", "changedTo"] as const;
    render(
      <ExpressionBuilder
        fields={FIELDS}
        value={[row("1", "name", "changed"), row("2", "name", "changedTo", "A"), row("3", "closed", "eq", "2026-09-01")]}
        onChange={() => {}}
        opsFor={() => WF_OPS}
        valueless={(op) => op === "isnull" || op === "changed"}
        relativeDates={false}
      />,
    );
    const [changed, changedTo, closed] = rows();
    expect(within(changed).getByRole("combobox", { name: "Toán tử" })).toHaveTextContent("vừa đổi");
    expect(within(changed).getByRole("textbox", { name: "Giá trị" })).toBeDisabled();
    expect(within(changedTo).getByRole("combobox", { name: "Toán tử" })).toHaveTextContent("vừa đổi thành");
    expect(within(changedTo).getByRole("textbox", { name: "Giá trị" })).toHaveValue("A");
    expect(within(closed).queryByRole("combobox", { name: "Giá trị" })).toBeNull();
    expect(within(closed).getByLabelText("Giá trị")).toHaveAttribute("type", "date");
    expect(within(closed).getByLabelText("Giá trị")).toHaveValue("2026-09-01");
  });

  it("delete removes the row", async () => {
    render(<Harness initial={[row("1", "name", "eq", "A"), row("2", "name", "eq", "B")]} />);
    await userEvent.click(screen.getByRole("button", { name: "Xoá điều kiện 1" }));
    expect(query()).toBe("name%5Beq%5D=B");
  });

  it("focus management (blueprint docs): add → new row's field; add group → its mode select; delete → next row, the last → previous", async () => {
    // Ids unlike nextFilterId()'s counter, so the rows added below never share a key with these.
    render(<Harness withGroups initial={[row("a", "name", "eq", "A"), row("b", "name", "eq", "B"), row("c", "name", "eq", "C")]} />);
    // Deleting the first (or a middle) row focuses the field combobox of the next row.
    screen.getByRole("button", { name: "Xoá điều kiện 1" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(rows()).toHaveLength(2);
    expect(comboboxIn(rows()[0], "Trường")).toHaveFocus();
    // Deleting the last row focuses the previous one.
    await userEvent.click(screen.getByRole("button", { name: "Xoá điều kiện 2" }));
    expect(comboboxIn(rows()[0], "Trường")).toHaveFocus();
    await userEvent.click(screen.getAllByRole("button", { name: "Thêm điều kiện" }).at(-1)!);
    expect(rows()).toHaveLength(2);
    expect(comboboxIn(rows()[1], "Trường")).toHaveFocus();
    await userEvent.click(screen.getByRole("button", { name: "Thêm nhóm" }));
    const group = document.querySelector<HTMLElement>("li.slds-expression__group")!;
    expect(within(group).getByRole("combobox", { name: "Áp dụng khi" })).toHaveFocus();
    await userEvent.click(within(group).getByRole("button", { name: "Thêm điều kiện" }));
    const groupRows = () => Array.from(group.querySelectorAll<HTMLElement>("li.slds-expression__row_group"));
    expect(comboboxIn(groupRows()[1], "Trường")).toHaveFocus();
    await userEvent.click(within(group).getByRole("button", { name: "Xoá điều kiện 2 của nhóm 1" }));
    expect(comboboxIn(groupRows()[0], "Trường")).toHaveFocus();
    // The group's last row out removes the group: focus falls back to the previous top-level item.
    await userEvent.click(within(group).getByRole("button", { name: "Xoá điều kiện 1 của nhóm 1" }));
    expect(document.querySelector("li.slds-expression__group")).toBeNull();
    expect(comboboxIn(rows()[1], "Trường")).toHaveFocus();
    // Nothing left to land on: the "Thêm điều kiện" button takes focus.
    await userEvent.click(screen.getByRole("button", { name: "Xoá điều kiện 2" }));
    await userEvent.click(screen.getByRole("button", { name: "Xoá điều kiện 1" }));
    expect(rows()).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Thêm điều kiện" })).toHaveFocus();
  });

  it("match select: 'Bất kỳ' turns the legends into HOẶC; custom logic numbers them and validates the logic", async () => {
    render(<Harness withMatch initial={[row("1", "name", "eq", "A"), row("2", "amount", "gt", "5")]} />);
    const options = document.querySelector(".slds-expression__options")!;
    const matchBox = within(options as HTMLElement).getByRole("combobox", { name: "Áp dụng khi" });
    expect(matchBox).toHaveTextContent("Tất cả điều kiện đúng");
    await pick(matchBox, "Bất kỳ điều kiện nào");
    expect(rows()[1].querySelector("legend > span:not(.slds-assistive-text)")).toHaveTextContent("HOẶC");
    await pick(matchBox, "Thoả logic tuỳ chỉnh");
    expect(rows().map((r) => r.querySelector("legend > span:not(.slds-assistive-text)")?.textContent)).toEqual(["1", "2"]);
    const logic = document.querySelector(".slds-expression__custom-logic input")!;
    expect(logic).toHaveAccessibleName("Logic tuỳ chỉnh");
    await userEvent.type(logic, "1 OR 3");
    expect(logic.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(screen.getByText("Không có điều kiện 3")).toHaveClass("slds-form-element__help");
    await userEvent.clear(logic);
    await userEvent.type(logic, "1 or 2");
    expect(logic.closest(".slds-form-element")).not.toHaveClass("slds-has-error");
  });

  it("groups: li.slds-expression__group with group legend, own AND/OR select, _row_group rows; the last row out removes the group", async () => {
    render(<Harness withGroups />);
    await userEvent.click(screen.getByRole("button", { name: "Thêm nhóm" }));
    const group = document.querySelector<HTMLElement>("li.slds-expression__group")!;
    const legend = group.querySelector("fieldset > legend")!;
    expect(legend).toHaveClass("slds-expression__legend", "slds-expression__legend_group");
    expect(legend).toHaveTextContent("VÀ");
    expect(legend.querySelector(".slds-assistive-text")).toHaveTextContent("Nhóm điều kiện 1");
    expect(within(group).getByRole("combobox", { name: "Áp dụng khi" })).toBeInTheDocument();
    await userEvent.click(within(group).getByRole("button", { name: "Thêm điều kiện" }));
    expect(group.querySelectorAll("li.slds-expression__row.slds-expression__row_group")).toHaveLength(2);
    expect(screen.getByTestId("groups").textContent).toBe("[2]");
    await pick(within(group).getByRole("combobox", { name: "Áp dụng khi" }), "Bất kỳ điều kiện nào");
    expect(group.querySelectorAll("li.slds-expression__row_group")[1].querySelector("legend")).toHaveTextContent("HOẶC");
    await userEvent.click(within(group).getByRole("button", { name: "Xoá điều kiện 2 của nhóm 1" }));
    await userEvent.click(within(group).getByRole("button", { name: "Xoá điều kiện 1 của nhóm 1" }));
    expect(document.querySelector("li.slds-expression__group")).toBeNull();
    expect(screen.getByTestId("groups").textContent).toBe("[]");
  });

  it("error on a row's value: slds-has-error + help line (blueprint with-error)", () => {
    render(<Harness initial={[row("7", "name", "eq")]} errors={{ "7": "Nhập giá trị" }} />);
    const input = within(rows()[0]).getByRole("textbox", { name: "Giá trị" });
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input.closest(".slds-form-element")).toHaveClass("slds-has-error");
    expect(input).toHaveAccessibleDescription("Nhập giá trị");
  });

  it("disabled: every control and button is off", () => {
    render(<Harness disabled />);
    for (const cb of screen.getAllByRole("combobox")) expect(cb).toHaveAttribute("aria-disabled", "true");
    for (const b of screen.getAllByRole("button")) expect(b).toBeDisabled();
    const value = screen.getByRole("spinbutton", { name: "Giá trị" });
    expect(value).toBeDisabled();
    expect(value).toHaveValue(1000); // disabled, not blanked like a row without an operator
  });

  it("binds into react-hook-form through <Controller>", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { control, handleSubmit } = useForm<{ conditions: FilterRow[] }>({ defaultValues: { conditions: [] } });
      return (
        <form onSubmit={handleSubmit((v) => onSubmit(v.conditions))}>
          <Controller control={control} name="conditions" render={({ field }) => <ExpressionBuilder fields={FIELDS} value={field.value} onChange={field.onChange} />} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    await userEvent.click(screen.getByRole("button", { name: "Thêm điều kiện" }));
    await pick(comboboxIn(rows()[0], "Trường"), "Tên");
    await userEvent.type(within(rows()[0]).getByRole("textbox", { name: "Giá trị" }), "Lan");
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(onSubmit).toHaveBeenCalledWith([expect.objectContaining({ field: "name", op: "eq", value: "Lan" })]);
  });
});

describe("conditionText", () => {
  it("operator + quoted value labels; relative dates unquoted; null ops alone", () => {
    const stage = FIELDS[2];
    expect(conditionText(row("1", "stage", "in", "new,won"), stage)).toBe('thuộc "Mới", "Thắng"');
    expect(conditionText(row("1", "amount", "between", "50,200"), FIELDS[1])).toBe('trong khoảng "50" – "200"');
    expect(conditionText(row("1", "closed", "eq", "this_week"), FIELDS[4])).toBe("bằng Tuần này");
    expect(conditionText(row("1", "name", "notnull"), FIELDS[0])).toBe("không rỗng");
    expect(conditionText(row("1", "", ""), undefined)).toBe("");
  });
});
