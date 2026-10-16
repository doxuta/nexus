// DAILY-QUEUE #5 (kanban-calendar-declared-field), Studio half — declaring the Kanban column and
// the Calendar date field must be doable IN THE BUILDER, not by hand-posting layout JSON with curl.
//
// Two things get pinned here because either one alone looks built and does nothing:
//   1. the choice reaches the wire (updateLayout receives kanbanField / calendarField), and
//   2. changing only that choice marks the layout DIRTY.
// The dirty check compares a serialized snapshot of the list state against a baseline, and a new
// control left out of that snapshot is invisible to it — the maker then navigates away and loses
// the choice with no "unsaved changes" warning, because dirty is what arms the navigation guard.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

// No <NavigationBlockerProvider> around a bare render — stub the dirty-guard hook, but keep
// setBlocked as a spy: it IS the observable for "the builder considers this layout dirty".
const setBlockedMock = vi.fn();
vi.mock("@/lib/navigation-blocker", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/navigation-blocker")>();
  return {
    ...actual,
    useNavigationBlocker: () => ({ setBlocked: setBlockedMock, confirmLeave: () => true }),
  };
});

const updateLayoutMock = vi.fn();
const createLayoutMock = vi.fn();
const getLayoutMock = vi.fn();

/** The served list layout, optionally already carrying a declaration. */
const servedLayout = (listExtra: Record<string, unknown> = {}) => ({
  entityName: "tk_tickets",
  views: {
    list: {
      id: "L1",
      version: 1,
      columns: [{ field: "subject" }],
      defaultSort: { field: "created_at", order: "desc" },
      ...listExtra,
    },
  },
});

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    listEntities: vi.fn().mockResolvedValue([{ id: "e1", name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    // THREE picklists and TWO date fields, so the declared choice differs from the first of its kind.
    listFields: vi.fn().mockResolvedValue([
      { id: "f1", name: "subject", label: "Tiêu đề", fieldType: "text" },
      { id: "f2", name: "priority", label: "Mức ưu tiên", fieldType: "picklist", options: { values: ["low", "high"] } },
      { id: "f3", name: "channel", label: "Kênh tiếp nhận", fieldType: "picklist", options: { values: ["email"] } },
      { id: "f4", name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["open", "closed"] } },
      { id: "f5", name: "reported_at", label: "Ngày tiếp nhận", fieldType: "date" },
      { id: "f6", name: "due_date", label: "Hạn xử lý", fieldType: "datetime" },
    ]),
    listRecords: vi.fn().mockResolvedValue([]),
    getLayout: (...args: unknown[]) => getLayoutMock(...args),
    updateLayout: (...args: unknown[]) => updateLayoutMock(...args),
    createLayout: (...args: unknown[]) => createLayoutMock(...args),
  };
});

import { LayoutBuilder } from "./LayoutBuilder";

const saveButton = () => screen.getByRole("button", { name: /Cập nhật|Lưu bố cục|Đã lưu/ });
/** True once the builder has told the navigation guard this layout has unsaved changes. */
const isDirty = () => setBlockedMock.mock.calls.some(([v]) => v === true);

async function renderBuilderOnList() {
  render(<LayoutBuilder entityName="tk_tickets" initialView="List" />);
  // Wait for the FIELDS to land, not just for ListConfig to mount: layoutLoading starts false, so
  // the config panel renders immediately with an empty field list and every query below would read
  // a half-built screen. "THÊM CỘT" only appears once listFields has resolved.
  await screen.findByText("THÊM CỘT");
}

beforeEach(() => {
  getLayoutMock.mockReset();
  getLayoutMock.mockResolvedValue(servedLayout());
  setBlockedMock.mockReset();
  updateLayoutMock.mockReset();
  updateLayoutMock.mockResolvedValue(undefined);
  createLayoutMock.mockReset();
});

describe("Studio declares the Kanban and Calendar fields", () => {
  it("offers only picklists for Kanban and only date fields for Calendar", async () => {
    await renderBuilderOnList();
    const kanban = screen.getByLabelText("Cột Kanban");
    const optionNames = Array.from(kanban.querySelectorAll("option")).map((o) => (o as HTMLOptionElement).value);
    expect(optionNames).toEqual(["", "priority", "channel", "status"]);

    const calendar = screen.getByLabelText("Trường ngày cho Lịch");
    const dateNames = Array.from(calendar.querySelectorAll("option")).map((o) => (o as HTMLOptionElement).value);
    expect(dateNames).toEqual(["", "reported_at", "due_date"]);
  });

  it("changing only the Kanban column makes the layout dirty and saves it", async () => {
    await renderBuilderOnList();
    expect(isDirty()).toBe(false);

    fireEvent.change(screen.getByLabelText("Cột Kanban"), { target: { value: "status" } });
    await waitFor(() => expect(isDirty()).toBe(true));

    fireEvent.click(saveButton());
    await waitFor(() => expect(updateLayoutMock).toHaveBeenCalled());
    expect(updateLayoutMock.mock.calls[0][1]).toMatchObject({ kanbanField: "status" });
  });

  it("changing only the Calendar date field makes the layout dirty and saves it", async () => {
    await renderBuilderOnList();
    fireEvent.change(screen.getByLabelText("Trường ngày cho Lịch"), { target: { value: "due_date" } });
    await waitFor(() => expect(isDirty()).toBe(true));

    fireEvent.click(saveButton());
    await waitFor(() => expect(updateLayoutMock).toHaveBeenCalled());
    expect(updateLayoutMock.mock.calls[0][1]).toMatchObject({ calendarField: "due_date" });
  });

  it("omits the keys entirely when the maker declares nothing", async () => {
    await renderBuilderOnList();
    // Change an unrelated control, so this case exercises a real save that declares nothing.
    fireEvent.click(screen.getByRole("button", { name: /Mức ưu tiên/ }));
    await waitFor(() => expect(isDirty()).toBe(true));

    fireEvent.click(saveButton());
    await waitFor(() => expect(updateLayoutMock).toHaveBeenCalled());
    const sent = updateLayoutMock.mock.calls[0][1] as Record<string, unknown>;
    expect("kanbanField" in sent).toBe(false);
    expect("calendarField" in sent).toBe(false);
  });
});

describe("a declaration that already exists in the served layout", () => {
  it("seeds the dropdowns and survives a save that changes something else", async () => {
    getLayoutMock.mockResolvedValue(servedLayout({ kanbanField: "status", calendarField: "due_date" }));
    await renderBuilderOnList();

    expect((screen.getByLabelText("Cột Kanban") as HTMLSelectElement).value).toBe("status");
    expect((screen.getByLabelText("Trường ngày cho Lịch") as HTMLSelectElement).value).toBe("due_date");
    // Seeded, not dirty: opening the tab must not look like an unsaved edit.
    expect(isDirty()).toBe(false);

    // Editing an UNRELATED control must carry both declarations through, not drop them.
    fireEvent.click(screen.getByRole("button", { name: /Mức ưu tiên/ }));
    await waitFor(() => expect(isDirty()).toBe(true));
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateLayoutMock).toHaveBeenCalled());
    expect(updateLayoutMock.mock.calls[0][1]).toMatchObject({
      kanbanField: "status", calendarField: "due_date",
    });
  });

  it("shows a declaration that no longer fits instead of a blank dropdown", async () => {
    // `subject` is a text field here: the shape left behind when a picklist is deleted and
    // recreated with another type. A blank select would read as "Tự đoán" and hide the 422.
    getLayoutMock.mockResolvedValue(servedLayout({ kanbanField: "subject" }));
    await renderBuilderOnList();

    const kanban = screen.getByLabelText("Cột Kanban") as HTMLSelectElement;
    expect(kanban.value).toBe("subject");
    expect(kanban.selectedOptions[0].textContent).toContain("không dùng được nữa");
    expect(kanban.disabled).toBe(false); // the maker can still clear it

    // Clearing it back to "Tự đoán" is a real change, and the key leaves the payload.
    fireEvent.change(kanban, { target: { value: "" } });
    await waitFor(() => expect(isDirty()).toBe(true));
    fireEvent.click(saveButton());
    await waitFor(() => expect(updateLayoutMock).toHaveBeenCalled());
    expect("kanbanField" in (updateLayoutMock.mock.calls[0][1] as Record<string, unknown>)).toBe(false);
  });
});
