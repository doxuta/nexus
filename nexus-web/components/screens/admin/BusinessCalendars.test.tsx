// Component tests — BusinessCalendars admin screen (CALENDAR-1 FE).
// Pins the contract that matters:
//   * list renders name/timezone/summary + the "Mặc định" badge honestly;
//   * create POSTs the exact BE shape {name, timezone, hours{mon..sun}, holidays, isDefault};
//   * delete goes through an explicit ConfirmDialog and DELETEs the right id — deleting the
//     default states the wall-clock fallback consequence.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { BusinessCalendars, workingDayCount, WEEKDAYS } from "@/components/screens/admin/BusinessCalendars";
import { logout } from "@/lib/api";
import type { BusinessCalendar } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

const VN_CAL: BusinessCalendar = {
  id: "cal-1",
  tenantId: "t-1",
  name: "Giờ hành chính",
  timezone: "Asia/Ho_Chi_Minh",
  hours: {
    mon: [{ start: "08:00", end: "12:00" }, { start: "13:00", end: "17:00" }],
    tue: [{ start: "08:00", end: "17:00" }],
    sat: [],
    sun: [],
  },
  holidays: ["2026-09-02"],
  isDefault: true,
  createdAt: "2026-07-11T09:00:00Z",
  updatedAt: "2026-07-11T09:00:00Z",
};

// URL-routed fetch mock: ensureAuth needs POST /auth/login; list/create/delete captured.
function routedFetch(calendars: BusinessCalendar[], opts?: {
  onCreate?: (init?: RequestInit) => Response;
  onDelete?: (url: string) => void;
  deleteRes?: Response;
}) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/auth/login")) {
      return jsonRes({ data: {
        accessToken: "at-test", refreshToken: "rt-test",
        user: { id: "u1", email: "admin@example.com", fullName: "Admin", isAdmin: true },
      } }, 200);
    }
    if (u.includes("/admin/business-calendars") && method === "GET") {
      return jsonRes({ data: { calendars } }, 200);
    }
    if (u.includes("/admin/business-calendars") && method === "POST") {
      return opts?.onCreate
        ? opts.onCreate(init)
        : jsonRes({ data: { calendar: VN_CAL } }, 201);
    }
    if (u.includes("/admin/business-calendars/") && method === "DELETE") {
      opts?.onDelete?.(u);
      return opts?.deleteRes ?? new Response(null, { status: 204 });
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("BusinessCalendars", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api token cache
    window.localStorage.clear();
    window.sessionStorage.clear();
    // Authenticated seam: seed a token so ensureAuth() is a no-op — screens under test must not
    // depend on DEMO_MODE auto-login (fail-closed since the 401 tenant-flip fix).
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("renders calendars with timezone, week summary and default badge", async () => {
    global.fetch = routedFetch([VN_CAL]) as unknown as typeof fetch;
    render(<BusinessCalendars />);

    await waitFor(() => {
      expect(screen.getByText("Giờ hành chính")).toBeInTheDocument();
    });
    expect(screen.getByText("Mặc định")).toBeInTheDocument();
    // 2 working days (mon, tue) · 1 holiday, timezone shown verbatim.
    expect(screen.getByText(/Asia\/Ho_Chi_Minh · 2 ngày làm việc\/tuần · 1 ngày lễ/)).toBeInTheDocument();
  });

  it("calendar cards sit in their own grid cells (no SLDS card+card margin inside the grid)", async () => {
    global.fetch = routedFetch([VN_CAL, { ...VN_CAL, id: "cal-2", name: "Ca đêm", isDefault: false }]) as unknown as typeof fetch;
    const { container } = render(<BusinessCalendars />);
    await waitFor(() => {
      expect(screen.getByText("Ca đêm")).toBeInTheDocument();
    });
    const cards = [...container.querySelectorAll("article.slds-card")];
    expect(cards).toHaveLength(2);
    // `.slds-card+.slds-card{margin-top:1rem}` would push the second grid column down.
    for (const card of cards) expect(card.previousElementSibling?.matches("article.slds-card") ?? false).toBe(false);
  });

  it("create POSTs the exact BE shape (hours per weekday + holidays + isDefault)", async () => {
    const createBodies: Record<string, unknown>[] = [];
    global.fetch = routedFetch([], {
      onCreate: (init) => {
        createBodies.push(JSON.parse(String(init?.body)));
        return jsonRes({ data: { calendar: VN_CAL } }, 201);
      },
    }) as unknown as typeof fetch;
    render(<BusinessCalendars />);

    await waitFor(() => {
      expect(screen.getByText("Chưa có lịch làm việc nào")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /Tạo lịch/ }));

    fireEvent.change(screen.getByLabelText(/Tên lịch/), { target: { value: "Giờ hành chính" } });
    // Add a holiday through the date picker (typed dd/MM/yyyy, committed on blur) — it lands as
    // a pill in the Vietnamese display format; the payload stays ISO (asserted below).
    const holidayInput = screen.getByLabelText("Thêm ngày lễ");
    fireEvent.change(holidayInput, { target: { value: "02/09/2026" } });
    fireEvent.blur(holidayInput);
    fireEvent.click(screen.getByRole("button", { name: "Thêm" }));
    expect(screen.getByText("02/09/2026")).toBeInTheDocument();
    // Mark default (checkbox-toggle: a native checkbox with role="switch").
    fireEvent.click(screen.getByRole("switch", { name: /Đặt làm lịch mặc định/ }));

    fireEvent.click(screen.getByRole("button", { name: /Tạo lịch/ }));
    await waitFor(() => {
      expect(createBodies).toHaveLength(1);
    });
    const body = createBodies[0] as {
      name: string; timezone: string; isDefault: boolean; holidays: string[];
      hours: Record<string, { start: string; end: string }[]>;
    };
    expect(body.name).toBe("Giờ hành chính");
    expect(body.timezone).toBe("Asia/Ho_Chi_Minh");
    expect(body.isDefault).toBe(true);
    expect(body.holidays).toEqual(["2026-09-02"]);
    // The default template: mon–fri two intervals (lunch break), sat/sun off.
    expect(body.hours.mon).toEqual([
      { start: "08:00", end: "12:00" },
      { start: "13:00", end: "17:00" },
    ]);
    expect(body.hours.sat).toEqual([]);
    expect(body.hours.sun).toEqual([]);
  });

  it("the default switch is described by its consequence (the old default loses its flag)", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<BusinessCalendars />);
    await waitFor(() => {
      expect(screen.getByText("Chưa có lịch làm việc nào")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /Tạo lịch/ }));
    expect(screen.getByRole("switch", { name: /Đặt làm lịch mặc định/ })).toHaveAccessibleDescription(/tự bỏ cờ ở lịch cũ/);
  });

  it("saving without a name sends nothing and moves focus to the name field, which carries the error", async () => {
    const createBodies: unknown[] = [];
    global.fetch = routedFetch([], {
      onCreate: (init) => {
        createBodies.push(init?.body);
        return jsonRes({ data: { calendar: VN_CAL } }, 201);
      },
    }) as unknown as typeof fetch;
    render(<BusinessCalendars />);
    await waitFor(() => {
      expect(screen.getByText("Chưa có lịch làm việc nào")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /Tạo lịch/ }));
    const submit = screen.getByRole("button", { name: /Tạo lịch/ });
    submit.focus();
    fireEvent.click(submit);
    const name = screen.getByRole("textbox", { name: /Tên lịch/ });
    await waitFor(() => expect(name).toHaveFocus());
    expect(name).toHaveAttribute("aria-invalid", "true");
    expect(name).toHaveAccessibleDescription(/Nhập tên lịch/);
    expect(createBodies).toHaveLength(0);
  });

  it("delete asks via ConfirmDialog (stating the wall-clock fallback) and DELETEs the right id", async () => {
    const deleted: string[] = [];
    global.fetch = routedFetch([VN_CAL], { onDelete: (u) => deleted.push(u) }) as unknown as typeof fetch;
    render(<BusinessCalendars />);

    await waitFor(() => {
      expect(screen.getByText("Giờ hành chính")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /Xoá lịch Giờ hành chính/ }));
    // No DELETE yet — deleting the DEFAULT calendar warns about the wall-clock fallback.
    expect(deleted).toHaveLength(0);
    expect(screen.getByText(/SLA quay về giờ treo tường/)).toBeInTheDocument();

    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Xoá lịch" }));
    await waitFor(() => {
      expect(deleted).toHaveLength(1);
    });
    expect(deleted[0]).toContain("/admin/business-calendars/cal-1");
  });

  it("a calendar pinned by rules cannot be deleted: the dialog stays open and names the rules (#326)", async () => {
    const msg = "Lịch đang được các luật sau dùng: Nhắc hạn, SLA ticket — gỡ lịch khỏi các luật đó trước";
    global.fetch = routedFetch([VN_CAL], {
      deleteRes: jsonRes({ error: msg, code: "CALENDAR_IN_USE", details: { usages: [] } }, 409),
    }) as unknown as typeof fetch;
    render(<BusinessCalendars />);
    await waitFor(() => { expect(screen.getByText("Giờ hành chính")).toBeInTheDocument(); });
    fireEvent.click(screen.getByRole("button", { name: /Xoá lịch Giờ hành chính/ }));
    fireEvent.click(screen.getByRole("button", { name: "Xoá lịch" }));
    await waitFor(() => { expect(screen.getByText(/Nhắc hạn, SLA ticket/)).toBeInTheDocument(); });
    expect(screen.queryByText(/CALENDAR_IN_USE/)).not.toBeInTheDocument();
  });
});

describe("BusinessCalendars — shared across the project", () => {
  it("says the calendars are shared by the whole project", async () => {
    global.fetch = routedFetch([VN_CAL]) as unknown as typeof fetch;
    render(<BusinessCalendars />);
    expect(await screen.findByText(/Lịch làm việc dùng chung cho cả dự án/)).toBeInTheDocument();
  });
});

describe("workingDayCount", () => {
  it("counts only weekdays with at least one interval", () => {
    expect(workingDayCount(VN_CAL.hours)).toBe(2);
    expect(workingDayCount({})).toBe(0);
    const all: Record<string, { start: string; end: string }[]> = {};
    for (const d of WEEKDAYS) all[d.key] = [{ start: "00:00", end: "24:00" }];
    expect(workingDayCount(all)).toBe(7);
  });
});
