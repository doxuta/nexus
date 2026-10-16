// specs/048 T148 — the "before" half of the KHI card: a before_save rule picks when it applies
// (create, update, both — radio-button-group), and every before rule carries the always-on warning
// that it runs synchronously inside EVERY save/delete of the table, imports and API included.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BeforeRuleEditor } from "./BeforeRuleEditor";

describe("BeforeRuleEditor", () => {
  it("before_save: 'Áp dụng khi' is a radio-button-group of three, the current one checked", async () => {
    const onSaveOnChange = vi.fn();
    render(<BeforeRuleEditor trigger="before_save" saveOn="update" onSaveOnChange={onSaveOnChange} />);
    const group = screen.getByRole("radiogroup", { name: "Áp dụng khi" });
    expect(group.querySelector(".slds-radio_button-group")).not.toBeNull();
    expect(screen.getByRole("radio", { name: "Chỉ khi cập nhật" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Tạo mới và cập nhật" })).not.toBeChecked();

    await userEvent.click(screen.getByRole("radio", { name: "Chỉ khi tạo mới" }));
    expect(onSaveOnChange).toHaveBeenLastCalledWith("create");
    await userEvent.click(screen.getByRole("radio", { name: "Tạo mới và cập nhật" }));
    expect(onSaveOnChange).toHaveBeenLastCalledWith("");
  });

  it("before_save: 'both' is the default (saveOn absent)", () => {
    render(<BeforeRuleEditor trigger="before_save" saveOn="" onSaveOnChange={() => {}} />);
    expect(screen.getByRole("radio", { name: "Tạo mới và cập nhật" })).toBeChecked();
  });

  it("the synchronous-run warning is always shown and names imports and the API", () => {
    const { rerender } = render(<BeforeRuleEditor trigger="before_save" saveOn="" onSaveOnChange={() => {}} />);
    const warn = screen.getByText(/MỌI lần lưu/);
    expect(warn.closest(".slds-scoped-notification")).toHaveClass("slds-theme_warning");
    expect(warn.textContent).toMatch(/nhập CSV/);
    expect(warn.textContent).toMatch(/API/);

    rerender(<BeforeRuleEditor trigger="before_delete" saveOn="" onSaveOnChange={() => {}} />);
    expect(screen.getByText(/MỌI lần xoá/)).toBeInTheDocument();
  });

  it("before_delete has no 'Áp dụng khi' (saveOn is a before_save setting)", () => {
    render(<BeforeRuleEditor trigger="before_delete" saveOn="" onSaveOnChange={() => {}} />);
    expect(screen.queryByRole("radiogroup", { name: "Áp dụng khi" })).toBeNull();
  });

  it("a saveOn refusal from the server sits on the group", () => {
    render(<BeforeRuleEditor trigger="before_save" saveOn="" onSaveOnChange={() => {}} saveOnError="condition.saveOn không hợp lệ" />);
    const group = screen.getByRole("radiogroup", { name: "Áp dụng khi" });
    expect(group).toHaveClass("slds-has-error");
    expect(group).toHaveTextContent("condition.saveOn không hợp lệ");
  });
});
