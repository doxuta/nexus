// RunFilterBar on SLDS (specs/038 T029) — the run overlay filters of /dashboards/[id] and
// /reports/[id]: a date-range popover (preset select + datepickers/range), the owner scope
// (admin: combobox over /admin/users; everyone else: "only mine"), removable pills.
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { presetRange, type RunFilters } from "@/lib/runFilters";

const api = vi.hoisted(() => ({ getMe: vi.fn(), getCachedMe: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
const admin = vi.hoisted(() => ({ listUsers: vi.fn() }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), ...admin }));

import { RunFilterBar } from "./RunFilterBar";

const USERS = [
  { id: "a1", fullName: "Quản trị Nexus", email: "admin@example.com" },
  { id: "u2", fullName: "Trần Sales Rep", email: "test@example.com" },
];

function renderBar(filters: RunFilters, onChange = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <RunFilterBar filters={filters} onChange={onChange} />
    </QueryClientProvider>,
  );
  return onChange;
}

beforeEach(() => {
  api.getCachedMe.mockReset().mockReturnValue({ id: "u2", isAdmin: false });
  api.getMe.mockReset().mockResolvedValue({ id: "u2", isAdmin: false });
  admin.listUsers.mockReset().mockResolvedValue(USERS);
});

describe("RunFilterBar — date range", () => {
  it("a preset applies its range; the clear button only works when a range is set", () => {
    const onChange = renderBar({});
    fireEvent.click(screen.getByRole("button", { name: "Khoảng ngày" }));
    const pop = screen.getByRole("dialog", { name: "Khoảng ngày" });
    expect(within(pop).getByRole("button", { name: "Xoá khoảng ngày" })).toBeDisabled();
    fireEvent.change(within(pop).getByLabelText("Khoảng có sẵn"), { target: { value: "30d" } });
    expect(onChange).toHaveBeenCalledWith(presetRange("30d", new Date()));
  });

  it("names the active preset on the trigger and clears the range", () => {
    const onChange = renderBar(presetRange("7d", new Date()));
    fireEvent.click(screen.getByRole("button", { name: "Khoảng ngày: 7 ngày qua" }));
    const pop = screen.getByRole("dialog", { name: "Khoảng ngày" });
    expect(within(pop).getByLabelText("Khoảng có sẵn")).toHaveValue("7d");
    fireEvent.click(within(pop).getByRole("button", { name: "Xoá khoảng ngày" }));
    expect(onChange).toHaveBeenCalledWith({ dateFrom: undefined, dateTo: undefined });
  });

  it("custom dates are typed into the range fields", () => {
    const onChange = renderBar({ dateFrom: "2026-09-01", dateTo: "2026-09-29" });
    fireEvent.click(screen.getByRole("button", { name: "Khoảng ngày: 01/09/2026 – 29/09/2026" }));
    const to = screen.getByLabelText("Đến ngày");
    fireEvent.change(to, { target: { value: "15/09/2026" } });
    fireEvent.blur(to);
    expect(onChange).toHaveBeenCalledWith({ dateFrom: "2026-09-01", dateTo: "2026-09-15" });
  });
});

describe("RunFilterBar — owner scope", () => {
  it("a non-admin can only narrow to their own records", async () => {
    const onChange = renderBar({});
    const mine = await screen.findByRole("checkbox", { name: "Chỉ của tôi" });
    fireEvent.click(mine);
    expect(onChange).toHaveBeenCalledWith({ ownerId: "u2" });
    expect(screen.queryByRole("combobox", { name: "Người tạo" })).toBeNull();
  });

  it("an admin picks any creator from the user directory", async () => {
    api.getCachedMe.mockReturnValue({ id: "a1", isAdmin: true });
    api.getMe.mockResolvedValue({ id: "a1", isAdmin: true });
    const onChange = renderBar({});
    const box = await screen.findByRole("combobox", { name: "Người tạo" });
    fireEvent.click(box);
    fireEvent.click(await screen.findByRole("option", { name: /Trần Sales Rep/ }));
    expect(onChange).toHaveBeenCalledWith({ ownerId: "u2" });
  });

  it("an admin finds a named creator by typing their email", async () => {
    api.getCachedMe.mockReturnValue({ id: "a1", isAdmin: true });
    api.getMe.mockResolvedValue({ id: "a1", isAdmin: true });
    const onChange = renderBar({});
    const box = await screen.findByRole("combobox", { name: "Người tạo" });
    fireEvent.change(box, { target: { value: "test@example" } });
    const opts = await screen.findAllByRole("option");
    expect(opts).toHaveLength(1);
    // Person option (user icon + name, email on the meta line) — the email that matched is visible.
    expect(opts[0].querySelector(".slds-listbox__option-meta")).toHaveTextContent("test@example.com");
    fireEvent.click(within(opts[0]).getByText("Trần Sales Rep"));
    expect(onChange).toHaveBeenCalledWith({ ownerId: "u2" });
  });

  it("lists a user who has no full name by email (the directory allows a null name)", async () => {
    api.getCachedMe.mockReturnValue({ id: "a1", isAdmin: true });
    api.getMe.mockResolvedValue({ id: "a1", isAdmin: true });
    admin.listUsers.mockResolvedValue([...USERS, { id: "u3", fullName: null as unknown as string, email: "khach@example.com" }]);
    const onChange = renderBar({});
    fireEvent.click(await screen.findByRole("combobox", { name: "Người tạo" }));
    fireEvent.click(await screen.findByRole("option", { name: /khach@example\.com/ }));
    expect(onChange).toHaveBeenCalledWith({ ownerId: "u3" });
  });
});

describe("RunFilterBar — applied filter pills", () => {
  it("each active filter is a removable pill; two or more add 'Xoá tất cả'", async () => {
    const onChange = renderBar({ dateFrom: "2026-09-01", dateTo: "2026-09-29", ownerId: "u2" });
    const remove = await screen.findByRole("button", { name: /Bỏ lọc Chỉ của tôi/ });
    fireEvent.click(remove);
    expect(onChange).toHaveBeenLastCalledWith({ dateFrom: "2026-09-01", dateTo: "2026-09-29" });
    fireEvent.click(screen.getByRole("button", { name: "Xoá tất cả" }));
    expect(onChange).toHaveBeenLastCalledWith({});
  });
});
