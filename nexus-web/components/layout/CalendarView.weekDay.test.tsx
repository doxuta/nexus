// DAILY-QUEUE #288 (calendar-week-day-view) — a front desk sees the week on an hour axis, each booking
// at its hour and titled by the patient name; switching to Tháng/Ngày keeps the same data.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const perm = { on: false };
vi.mock("@/lib/metadata/permissions", () => ({
  usePermissions: () => ({ can: () => perm.on, grants: [], isLoading: false }),
}));

// dnd-kit's pointer drag does not run in jsdom: keep the DndContext props so a test can fire onDragEnd.
const dnd: { onDragEnd?: (e: unknown) => void } = {};
vi.mock("@dnd-kit/core", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@dnd-kit/core")>();
  return {
    ...actual,
    DndContext: (p: { onDragEnd: (e: unknown) => void }) => {
      dnd.onDragEnd = p.onDragEnd;
      return <actual.DndContext {...(p as object)} />;
    },
  };
});

const quick = vi.fn();
vi.mock("@/components/layout/QuickCreateModal", () => ({
  QuickCreateModal: (p: { prefill?: Record<string, unknown> }) => {
    quick(p.prefill);
    return null;
  },
}));

const listMock = vi.fn();
const updateMock = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn().mockResolvedValue(undefined),
    updateRecord: (...a: unknown[]) => updateMock(...a),
    listRecordsPage: (...a: unknown[]) => listMock(...a),
  };
});

import { CalendarView } from "./CalendarView";
import type { FieldMeta } from "@/lib/api";

const fields = [
  { id: "1", name: "patient", label: "Bệnh nhân", fieldType: "relation", isLabel: true },
  { id: "2", name: "starts_at", label: "Giờ hẹn", fieldType: "datetime" },
] as unknown as FieldMeta[];

// Today 09:30 in Vietnam = 02:30Z.
const today = new Date(Date.now() + 7 * 3600_000).toISOString().slice(0, 10);
const rec = (id: string, name: string, hhmmZ: string) => ({
  id, tenantId: "t", data: { patient: { id: "p" + id, label: name }, starts_at: `${today}T${hhmmZ}:00Z` },
});

function renderCal() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <CalendarView entity="lich_hen" fields={fields} />
    </QueryClientProvider>,
  );
}

describe("CalendarView week/day", () => {
  beforeEach(() => {
    listMock.mockReset();
    updateMock.mockReset().mockResolvedValue({});
    quick.mockReset();
    perm.on = false;
    listMock.mockResolvedValue({
      records: [rec("a", "Nguyễn Văn An", "02:30"), rec("b", "Trần Thị Bình", "02:40")],
      pagination: { page: 1, limit: 200, total: 2, totalPages: 1 },
    });
  });

  it("opens on the week for a datetime field, titles events by the patient and shows the clock", async () => {
    renderCal();
    expect(await screen.findByText("09:30 Nguyễn Văn An")).toBeInTheDocument();
    expect(screen.getByText("09:40 Trần Thị Bình")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tuần" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("09:00")).toBeInTheDocument(); // hour axis
    // relation names are fetched expanded, and the span query covers the whole last day
    const q = String(listMock.mock.calls[0][1]);
    expect(q).toContain("expand=patient");
    expect(decodeURIComponent(q.replace(/\+/g, " "))).toMatch(/starts_at\[between\]=\d{4}-\d{2}-\d{2}T17:00:00Z,\d{4}-\d{2}-\d{2}T16:59:59Z/);
  });

  it("switches to Ngày and Tháng", async () => {
    renderCal();
    await screen.findByText("09:30 Nguyễn Văn An");
    fireEvent.click(screen.getByRole("button", { name: "Ngày" }));
    await waitFor(() => expect(screen.getByRole("group", { name: /^Lịch ngày/ })).toBeInTheDocument());
    expect(await screen.findByText("09:30 Nguyễn Văn An")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Tháng" }));
    expect(await screen.findByRole("table", { name: /^Lịch Tháng/ })).toBeInTheDocument();
    expect(await screen.findByText("09:30 Nguyễn Văn An")).toBeInTheDocument();
  });

  const drop = (id: string, over: string) => dnd.onDragEnd!({ active: { id }, over: { id: over } });

  it("dropping on another hour moves the hour, keeps the minutes, sends +07:00", async () => {
    perm.on = true;
    renderCal();
    await screen.findByText("09:40 Trần Thị Bình");
    drop("b", `${today}@660`); // 11:00 slot
    await waitFor(() => expect(updateMock).toHaveBeenCalledWith("lich_hen", "b", { starts_at: `${today}T11:40:00+07:00` }));
  });

  it("dropping inside the same hour is a no-op (a sloppy drag must not rewrite 09:40 to 09:00)", async () => {
    perm.on = true;
    renderCal();
    await screen.findByText("09:40 Trần Thị Bình");
    drop("b", `${today}@540`);
    await new Promise((r) => setTimeout(r, 20));
    expect(updateMock).not.toHaveBeenCalled();
  });

  it("clicking an empty hour slot opens the create form prefilled at that hour", async () => {
    perm.on = true;
    const { container } = renderCal();
    await screen.findByText("09:40 Trần Thị Bình");
    fireEvent.click(container.querySelector(".cursor-pointer") as HTMLElement); // first slot: 07:00 Monday
    await waitFor(() => expect(quick).toHaveBeenCalled());
    expect(String(quick.mock.calls.at(-1)![0].starts_at)).toMatch(/^\d{4}-\d{2}-\d{2}T07:00:00\+07:00$/);
  });

  // specs/078 T078a: a host that navigates to a full page (the Studio frame) passes onCreateAt; then no modal opens.
  it("with onCreateAt, a click hands the field and wire value to the host and opens no modal", async () => {
    perm.on = true;
    const onCreateAt = vi.fn();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(
      <QueryClientProvider client={qc}>
        <CalendarView entity="lich_hen" fields={fields} onCreateAt={onCreateAt} />
      </QueryClientProvider>,
    );
    await screen.findByText("09:40 Trần Thị Bình");
    fireEvent.click(container.querySelector(".cursor-pointer") as HTMLElement);
    expect(onCreateAt).toHaveBeenCalledTimes(1);
    expect(onCreateAt.mock.calls[0][0]).toBe("starts_at");
    expect(String(onCreateAt.mock.calls[0][1])).toMatch(/^\d{4}-\d{2}-\d{2}T07:00:00\+07:00$/);
    expect(quick).not.toHaveBeenCalled();
  });
});
