// CalendarPage (specs/074 lát f2-draft-labels-rules T349): a working calendar has no draft, so on a delivered project
// saving asks in place ("Lưu và áp ngay") before it calls the API; on a project not delivered it saves at once.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const getPublishState = vi.fn();
const createBusinessCalendar = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/tu-dong/lich-lam-viec",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), forward: vi.fn(), refresh: vi.fn(), prefetch: vi.fn() }),
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  getPublishState: (...a: unknown[]) => getPublishState(...a),
}));

vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true }) }));
vi.mock("@/lib/admin", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/admin")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listBusinessCalendars: vi.fn().mockResolvedValue([]),
  createBusinessCalendar: (...a: unknown[]) => createBusinessCalendar(...a),
}));

import { CalendarPage } from "./CalendarPage";

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><CalendarPage slug="northwind" /></QueryClientProvider>);
}
async function fillAndSave() {
  fireEvent.click(await screen.findByRole("button", { name: "Tạo lịch" }));
  fireEvent.change(await screen.findByLabelText(/Tên lịch/), { target: { value: "Giờ hành chính" } });
  fireEvent.click(screen.getByRole("button", { name: "Tạo lịch" }));
}

beforeEach(() => {
  vi.clearAllMocks();
  createBusinessCalendar.mockResolvedValue({ id: "c1" });
});

describe("CalendarPage apply-now gate", () => {
  it("delivered: asks first, saves only after confirming", async () => {
    getPublishState.mockResolvedValue({ delivered: true, draftableTypes: ["workflow"], directTypes: [] });
    renderPage();
    await screen.findByText("Áp ngay, không có trong lịch sử"); // label first: publish state has loaded
    await fillAndSave();
    expect(await screen.findByRole("button", { name: "Lưu và áp ngay" })).toBeTruthy();
    expect(createBusinessCalendar).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Lưu và áp ngay" }));
    await waitFor(() => expect(createBusinessCalendar).toHaveBeenCalledTimes(1));
  });

  it("not delivered: saves at once, no question", async () => {
    getPublishState.mockResolvedValue({ delivered: false, draftableTypes: ["workflow"], directTypes: [] });
    renderPage();
    await fillAndSave();
    await waitFor(() => expect(createBusinessCalendar).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("button", { name: "Lưu và áp ngay" })).toBeNull();
  });
});
