import { describe, it, expect } from "vitest";
import { timerPreview, timerFromCondition, DEADLINE_UNITS, deadlinePreview, deadlineParamsFromAction, deadlinePolicyPayload, type WorkflowTimer } from "./workflowTimer";

const base: WorkflowTimer = {
  dateField: "expiry_date",
  offset: { amount: 3, unit: "days" },
  direction: "before",
};

describe("timerPreview", () => {
  it("renders plain-language Vietnamese with the field label", () => {
    expect(timerPreview(base, "Ngày hết hạn")).toBe("3 ngày trước Ngày hết hạn");
  });

  it("falls back to the raw field name without a label", () => {
    expect(timerPreview(base)).toBe("3 ngày trước expiry_date");
  });

  it("names created_at in plain language", () => {
    expect(
      timerPreview({ dateField: "created_at", offset: { amount: 4, unit: "business_hours" }, direction: "after" }),
    ).toBe("4 giờ làm việc sau thời điểm tạo bản ghi");
  });

  it("names updated_at as the last edit (#287 — nothing changed for N days)", () => {
    expect(
      timerPreview({ dateField: "updated_at", offset: { amount: 14, unit: "days" }, direction: "after" }),
    ).toBe("14 ngày sau lần sửa gần nhất");
  });

  it("shows a placeholder for an unset amount", () => {
    expect(timerPreview({ ...base, offset: { amount: 0, unit: "hours" } }, "Hạn chót")).toBe("… giờ trước Hạn chót");
  });
});

describe("timerFromCondition", () => {
  it("parses a stored condition JSON with a timer", () => {
    const cond = {
      match: "and",
      rules: [],
      timer: { dateField: "renew_on", offset: { amount: 2, unit: "hours" }, direction: "after", calendarId: "cal-1" },
    };
    expect(timerFromCondition(cond)).toEqual({
      dateField: "renew_on",
      offset: { amount: 2, unit: "hours" },
      direction: "after",
      calendarId: "cal-1",
    });
  });

  it("returns null for a condition without a timer (event rules)", () => {
    expect(timerFromCondition({ match: "and", rules: [{ field: "s", op: "eq", value: "x" }] })).toBeNull();
    expect(timerFromCondition(null)).toBeNull();
    expect(timerFromCondition(undefined)).toBeNull();
  });

  it("normalizes a partially-shaped timer defensively", () => {
    const parsed = timerFromCondition({ timer: { dateField: "d" } });
    expect(parsed).toEqual({ dateField: "d", offset: { amount: 1, unit: "days" }, direction: "before", calendarId: undefined });
  });
});

// RECURRENCE (#3b) — the FE half. The preview is the only place a maker sees what a rule will do
// before saving it, so it must state the TOTAL number of fires: "repeats 4 more times" and "5 times
// in total" are the same rule described two ways, and only one of them matches the count field they
// just typed.
describe("timerPreview — recurrence", () => {
  it("no repeat → the one-shot sentence is unchanged", () => {
    expect(timerPreview({ ...base, repeat: undefined })).not.toContain("nhắc lại");
  });

  it("repeat → says the interval AND the total count", () => {
    const s = timerPreview({ ...base, repeat: { every: 2, unit: "days", count: 5 } });
    expect(s).toContain("mỗi 2 ngày");
    expect(s).toContain("tổng 5 lần");
  });

  it("weeks render in Vietnamese, not the wire key", () => {
    expect(timerPreview({ ...base, repeat: { every: 1, unit: "weeks", count: 3 } })).toContain("tuần");
  });
});

describe("timerFromCondition — recurrence", () => {
  it("round-trips a stored repeat", () => {
    const t = timerFromCondition({
      timer: { dateField: "due", offset: { amount: 1, unit: "days" }, direction: "before",
               repeat: { every: 3, unit: "weeks", count: 4 } },
    });
    expect(t?.repeat).toEqual({ every: 3, unit: "weeks", count: 4 });
  });

  it("a MALFORMED repeat reads as no recurrence, never a guessed one", () => {
    // A reminder that fires more often than configured is worse than one that does not repeat.
    for (const bad of [{ every: "3" }, { unit: "days" }, null, 7, {}]) {
      const t = timerFromCondition({
        timer: { dateField: "due", offset: { amount: 1, unit: "days" }, direction: "before", repeat: bad },
      });
      expect(t?.repeat).toBeUndefined();
    }
  });
});

describe("set_deadline action helpers", () => {
  it("offers the SAME units the trigger does — a deadline the admin cannot express is a missing capability", () => {
    expect(DEADLINE_UNITS.map((u) => u.key)).toEqual(["minutes", "hours", "days", "business_hours"]);
  });

  it("previews a deadline in plain Vietnamese with the field label", () => {
    expect(deadlinePreview({ field: "due_date", offset: { amount: 4, unit: "hours" } }, "Hạn xử lý"))
      .toBe("4 giờ kể từ lúc luật chạy, ghi vào Hạn xử lý");
  });

  it("falls back to the raw field name, and marks an unset amount", () => {
    expect(deadlinePreview({ field: "due_date", offset: { amount: 0, unit: "days" } }))
      .toBe("… ngày kể từ lúc luật chạy, ghi vào due_date");
  });

  // Regression guard for the hydration trap: the builder's generic mapper stringifies every value,
  // so a nested offset would come back as "[object Object]" and re-saving the rule would drop it.
  it("hydrates a stored action's nested offset into flat params", () => {
    expect(deadlineParamsFromAction({ type: "set_deadline", field: "due_date", offset: { amount: 90, unit: "minutes" } }))
      .toEqual({ field: "due_date", amount: "90", unit: "minutes" });
    expect(String({ amount: 90, unit: "minutes" })).toBe("[object Object]"); // what the generic path would store
  });

  it("survives a malformed stored action instead of inventing an offset", () => {
    expect(deadlineParamsFromAction({ type: "set_deadline" })).toEqual({ field: "", amount: "", unit: "hours" });
  });

  // Queue #79: a rule pinned to its own working calendar. Re-saving must send the pin back, or the
  // rule silently walks the tenant's default calendar with a green save to show for it.
  it("carries a pinned calendarId through hydrate → payload", () => {
    const params = deadlineParamsFromAction({
      type: "set_deadline", field: "due_date", offset: { amount: 12, unit: "business_hours" }, calendarId: "cal-247",
    });
    expect(deadlinePolicyPayload(params)).toEqual({ calendarId: "cal-247" });
  });

  // ...but only while the unit walks a calendar. The server refuses a pin on any other unit
  // (SET_DEADLINE_CALENDAR_INVALID) and the dropdown that clears it is hidden on that unit, so
  // switching the unit to wall-clock hours must drop it here instead of trapping the admin behind a 422.
  it("drops the pin once the unit no longer walks a calendar", () => {
    const params = deadlineParamsFromAction({
      type: "set_deadline", field: "due_date", offset: { amount: 12, unit: "business_hours" }, calendarId: "cal-247",
    });
    expect(deadlinePolicyPayload({ ...params, unit: "hours" })).toEqual({});
  });
});
