import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { NotifyRecipientPicker, parseRecipient } from "./NotifyRecipientPicker";

vi.mock("@/lib/api", () => ({
  listAdminUsers: vi.fn(async () => [{ id: "u1", email: "a@x.vn", fullName: "Bác sĩ An" }]),
  listRoles: vi.fn(async () => [{ id: "r1", name: "le_tan", label: "Lễ tân" }]),
}));
vi.mock("@/lib/admin", () => ({ listTeams: vi.fn(async () => [{ id: "tm1", name: "Nhóm A" }]) }));

const fields = [
  { name: "bac_si", label: "Bác sĩ", fieldType: "user" },
  { name: "ten", label: "Tên", fieldType: "text" },
] as never;

describe("parseRecipient", () => {
  it.each([
    ["", "owner", ""],
    ["$owner", "owner", ""],
    ["$field:bac_si", "field", "bac_si"],
    ["$role:r1", "role", "r1"],
    ["$team:tm1", "team", "tm1"],
    ["5a1c", "person", "5a1c"],
  ])("%s", (spec, kind, value) => expect(parseRecipient(spec)).toEqual({ kind, value }));
});

describe("NotifyRecipientPicker", () => {
  it("offers only user fields and emits $field:<name>", () => {
    const onChange = vi.fn();
    render(<NotifyRecipientPicker value="$field:" entityFields={fields} onChange={onChange} />);
    expect(screen.queryByRole("option", { name: "Tên" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Trường người dùng"), { target: { value: "bac_si" } });
    expect(onChange).toHaveBeenCalledWith("$field:bac_si");
  });

  it("lists roles by label and emits $role:<id>", async () => {
    const onChange = vi.fn();
    render(<NotifyRecipientPicker value="$role:" entityFields={fields} onChange={onChange} />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Lễ tân" })).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Vai"), { target: { value: "r1" } });
    expect(onChange).toHaveBeenCalledWith("$role:r1");
  });

  it("lists teams and emits $team:<id>", async () => {
    const onChange = vi.fn();
    render(<NotifyRecipientPicker value="$team:" entityFields={fields} onChange={onChange} />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Nhóm A" })).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Nhóm"), { target: { value: "tm1" } });
    expect(onChange).toHaveBeenCalledWith("$team:tm1");
  });

  it("a saved person emits the user id", async () => {
    const onChange = vi.fn();
    render(<NotifyRecipientPicker value="u1" entityFields={fields} onChange={onChange} />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Bác sĩ An" })).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Người"), { target: { value: "u1" } });
    expect(onChange).toHaveBeenCalledWith("u1");
  });

  it("a saved role is not called deleted while loading, only once the list says so", async () => {
    render(<NotifyRecipientPicker value="$role:gone" entityFields={fields} onChange={vi.fn()} />);
    expect(screen.queryByRole("option", { name: "(không còn trong danh sách)" })).toBeNull();
    await waitFor(() => expect(screen.getByRole("option", { name: "(không còn trong danh sách)" })).toBeTruthy());
  });

  it("choosing a person starts empty (not $owner) and emits the user id", async () => {
    const onChange = vi.fn();
    render(<NotifyRecipientPicker value="$owner" entityFields={fields} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Báo cho"), { target: { value: "person" } });
    expect(onChange).toHaveBeenLastCalledWith("");
  });
});
