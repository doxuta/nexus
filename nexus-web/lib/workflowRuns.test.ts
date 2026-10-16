// lib/workflowRuns.test.ts — W6 pure-shaping tests: run-row mapping + dry-run trace model.
import { describe, it, expect } from "vitest";
import {
  runStatusMeta,
  triggerLabel,
  shapeActionsResult,
  actionResultTone,
  actionStateLabel,
  relTime,
  runDurationMs,
  fmtDuration,
  traceValue,
  shapeTraceStep,
  wf01Field,
  actionRowPointer,
  FIELD_TARGET_ACTIONS,
  traceInstantVN,
  actionTitle,
} from "./workflowRuns";

describe("wf01Field", () => {
  it("prefers details.field when the backend sends it (contract §9.1)", () => {
    expect(wf01Field({ field: "opp_code" }, 'field "other" là …')).toBe("opp_code");
  });
  it("falls back to the quoted field name in the live message (details omitted)", () => {
    expect(
      wf01Field({}, 'field "lead_code" là computed/assigned field (auto_number) — workflow không được ghi đè'),
    ).toBe("lead_code");
    expect(wf01Field(undefined, 'field "nonexistent_x" không tồn tại trong metadata')).toBe("nonexistent_x");
  });
  it("returns null when nothing identifies the field", () => {
    expect(wf01Field({}, "some other error")).toBeNull();
  });
});

// Queue #79: SET_DEADLINE_CALENDAR_* point at the action ROW by index (details.field =
// "actions[i].calendarId") — a field name cannot say which of two set_deadline pins is dead.
describe("actionRowPointer", () => {
  it("reads the row index and key", () => {
    expect(actionRowPointer({ field: "actions[2].calendarId" })).toEqual({ index: 2, key: "calendarId" });
  });
  it("is null for a plain field name or no details", () => {
    expect(actionRowPointer({ field: "due_date" })).toBeNull();
    expect(actionRowPointer(undefined)).toBeNull();
  });
});

describe("runStatusMeta", () => {
  it("maps the three contract statuses to tones (success teal / failed danger / skipped muted)", () => {
    expect(runStatusMeta("success")).toEqual({ tone: "ac", label: "Thành công" });
    expect(runStatusMeta("failed")).toEqual({ tone: "dg", label: "Lỗi" });
    expect(runStatusMeta("skipped")).toEqual({ tone: "neu", label: "Bỏ qua" });
  });
  it("falls back to muted + raw for unknown statuses", () => {
    expect(runStatusMeta("weird")).toEqual({ tone: "neu", label: "weird" });
    expect(runStatusMeta("")).toEqual({ tone: "neu", label: "—" });
  });
});

describe("triggerLabel", () => {
  it("maps the 4 backend triggers to Vietnamese", () => {
    expect(triggerLabel("after_create")).toBe("Tạo mới");
    expect(triggerLabel("after_update")).toBe("Cập nhật");
    expect(triggerLabel("after_delete")).toBe("Xoá");
    expect(triggerLabel("scheduled")).toBe("Theo lịch");
    expect(triggerLabel("date_offset")).toBe("Mốc thời gian");
  });
  it("passes through unknown triggers verbatim", () => {
    expect(triggerLabel("custom_hook")).toBe("custom_hook");
  });
});

describe("shapeActionsResult", () => {
  it("null (skipped run) → none", () => {
    expect(shapeActionsResult(null)).toEqual({ kind: "none" });
    expect(shapeActionsResult(undefined)).toEqual({ kind: "none" });
  });
  it("array → actions, filtering junk entries", () => {
    const r = shapeActionsResult([
      { type: "field_update", field: "stage", ok: true },
      "junk",
      { type: "email_alert", ok: false, error: "smtp down" },
    ]);
    expect(r.kind).toBe("actions");
    if (r.kind === "actions") {
      expect(r.actions).toHaveLength(2);
      expect(r.actions[0].type).toBe("field_update");
      expect(r.actions[1].error).toBe("smtp down");
    }
  });
  it("empty array → none (no fake 'ran 0 actions' row)", () => {
    expect(shapeActionsResult([])).toEqual({ kind: "none" });
  });
  it("scheduled summary object → scheduled with safe number coercion", () => {
    expect(shapeActionsResult({ scanned: 10, matched: 3, failed: 1 })).toEqual({
      kind: "scheduled",
      scanned: 10,
      matched: 3,
      failed: 1,
      webhooksHeld: 0,
      warned: 0,
      warnings: [],
    });
    expect(shapeActionsResult({ scanned: 5 })).toEqual({
      kind: "scheduled",
      scanned: 5,
      matched: 0,
      failed: 0,
      webhooksHeld: 0,
      warned: 0,
      warnings: [],
    });
  });
  // Review of DAILY-QUEUE #26: a scheduled firing whose earlier webhook deliveries are still
  // pending HOLDS its own (never sent for that firing) and says so in webhooksHeld — a "success,
  // 2 matched" row must not hide it.
  it("scheduled summary carries webhooksHeld", () => {
    expect(shapeActionsResult({ scanned: 2, matched: 2, failed: 0, webhooksHeld: 2 })).toEqual({
      kind: "scheduled",
      scanned: 2,
      matched: 2,
      failed: 0,
      webhooksHeld: 2,
      warned: 0,
      warnings: [],
    });
  });
  // DAILY-QUEUE #105 (a): a scheduled firing's wall-clock deadlines used to lose their warning in the
  // one-row summary; it now carries a count and each distinct sentence (non-strings dropped).
  it("scheduled summary carries warned + warnings", () => {
    const w = "Tổ chức chưa có lịch làm việc mặc định dùng được — hạn này tính theo giờ treo tường.";
    expect(shapeActionsResult({ scanned: 3, matched: 3, failed: 0, warned: 3, warnings: [w, 7] })).toEqual({
      kind: "scheduled",
      scanned: 3,
      matched: 3,
      failed: 0,
      webhooksHeld: 0,
      warned: 3,
      warnings: [w],
    });
  });
  it("random object without summary keys → none", () => {
    expect(shapeActionsResult({ foo: "bar" })).toEqual({ kind: "none" });
  });
});

describe("actionResultTone", () => {
  // DAILY-QUEUE #82: a business_hours deadline stamped on the wall-clock fallback is ok:true, and
  // before this the row drew the same green dot as a deadline computed through the calendar.
  it("failed → dg, warning → wn, plain success → ok", () => {
    expect(actionResultTone({ type: "set_deadline", ok: false, error: "x" })).toBe("dg");
    expect(
      actionResultTone({ type: "set_deadline", ok: true, warning: "hạn này tính theo giờ treo tường" }),
    ).toBe("wn");
    expect(actionResultTone({ type: "set_deadline", ok: true })).toBe("ok");
    expect(actionResultTone({ type: "set_deadline", ok: true, warning: "" })).toBe("ok");
  });
  // #338: a run still `running` has placeholder slots with no outcome — not a green dot.
  it("a slot with no outcome yet is neutral and says Đang chạy while the run runs", () => {
    expect(actionResultTone({ type: "email_alert" })).toBe("neu");
    expect(actionStateLabel({ type: "email_alert" }, "running")).toBe("Đang chạy");
    expect(actionStateLabel({ type: "email_alert", ok: true }, "running")).toBeNull();
    expect(runStatusMeta("running")).toEqual({ tone: "neu", label: "Đang chạy" });
  });
  // #339: the email slot of a run whose mail the queue still holds is not a green "done".
  it("a queued delivery is pending, a sent one is ok; both carry a word", () => {
    expect(actionResultTone({ type: "email_alert", ok: true, delivery: "queued" })).toBe("pn");
    expect(actionStateLabel({ type: "email_alert", ok: true, delivery: "queued" }, "success")).toBe("Đang chờ gửi");
    expect(actionResultTone({ type: "email_alert", ok: true, delivery: "sent" })).toBe("ok");
    expect(actionStateLabel({ type: "email_alert", ok: true, delivery: "sent" }, "success")).toBe("Đã gửi");
    expect(actionStateLabel({ type: "email_alert", ok: false, error: "x", delivery: "failed" }, "failed")).toBeNull();
    expect(actionStateLabel({ type: "field_update", ok: true }, "success")).toBeNull();
  });
  it("a failure outranks a warning", () => {
    expect(actionResultTone({ type: "set_deadline", ok: false, warning: "w", error: "e" })).toBe("dg");
  });
});

describe("relTime / duration", () => {
  const NOW = new Date("2026-07-03T12:00:00Z").getTime();
  it("buckets relative time in Vietnamese", () => {
    expect(relTime("2026-07-03T11:59:40Z", NOW)).toBe("vừa xong");
    expect(relTime("2026-07-03T11:55:00Z", NOW)).toBe("5 phút trước");
    expect(relTime("2026-07-03T09:00:00Z", NOW)).toBe("3 giờ trước");
    expect(relTime("2026-07-01T12:00:00Z", NOW)).toBe("2 ngày trước");
  });
  it("returns — for garbage timestamps", () => {
    expect(relTime("not-a-date", NOW)).toBe("—");
  });
  it("computes duration ms and formats it", () => {
    expect(
      runDurationMs({ startedAt: "2026-07-03T08:00:00Z", finishedAt: "2026-07-03T08:00:01Z" }),
    ).toBe(1000);
    expect(runDurationMs({ startedAt: "bad", finishedAt: "2026-07-03T08:00:01Z" })).toBeNull();
    expect(runDurationMs({ status: "running", startedAt: "2026-07-03T08:00:00Z", finishedAt: "2026-07-03T08:00:00Z" })).toBeNull();
    expect(fmtDuration(250)).toBe("250ms");
    expect(fmtDuration(1000)).toBe("1s");
    expect(fmtDuration(1234)).toBe("1,2s");
    expect(fmtDuration(null)).toBe("—");
  });
});

describe("traceValue", () => {
  it("never shows literal null/undefined and marks empty strings", () => {
    expect(traceValue(null)).toBe("—");
    expect(traceValue(undefined)).toBe("—");
    expect(traceValue("")).toBe("(rỗng)");
    expect(traceValue("won")).toBe("won");
    expect(traceValue(42)).toBe("42");
    expect(traceValue(false)).toBe("false");
    expect(traceValue({ a: 1 })).toBe('{"a":1}');
  });
});

describe("shapeTraceStep", () => {
  it("trigger step: matched → pass", () => {
    const s = shapeTraceStep({
      step: 1,
      kind: "trigger",
      matched: true,
      detail: { trigger: "after_update" },
    });
    expect(s.verdict).toBe("pass");
    expect(s.verdictLabel).toBe("Khớp");
    expect(s.rows[0].value).toBe("Cập nhật");
  });

  it("condition step: field/op/expected/actual rows + fail verdict", () => {
    const s = shapeTraceStep({
      step: 2,
      kind: "condition",
      matched: false,
      detail: { field: "status", op: "eq", value: "won", actual: "new" },
    });
    expect(s.verdict).toBe("fail");
    expect(s.verdictLabel).toBe("Không khớp");
    expect(s.rows).toEqual([
      { label: "Trường", value: "status", mono: true },
      { label: "Toán tử", value: "=" },
      { label: "Kỳ vọng", value: "won" },
      { label: "Thực tế", value: "new" },
    ]);
  });

  it("condition matchAll → 'always matches' honest row", () => {
    const s = shapeTraceStep({ step: 2, kind: "condition", matched: true, detail: { matchAll: true } });
    expect(s.verdict).toBe("pass");
    expect(s.verdictLabel).toBe("Luôn khớp");
    expect(s.rows[0].value).toContain("luôn khớp");
  });

  it("field_update action: before→after rows, pass when wouldApply", () => {
    const s = shapeTraceStep({
      step: 3,
      kind: "action",
      evaluated: true,
      detail: { type: "field_update", field: "stage", before: "new", after: "closed", wouldApply: true },
    });
    expect(s.title).toBe("Cập nhật trường");
    expect(s.verdict).toBe("pass");
    expect(s.rows).toEqual([
      { label: "Trường", value: "stage", mono: true },
      { label: "Trước", value: "new" },
      { label: "Sau", value: "closed" },
    ]);
  });

  it("field_update on a related record (specs/058): names the relation + id, no invented 'before'", () => {
    const s = shapeTraceStep({
      step: 3,
      kind: "action",
      evaluated: true,
      detail: { type: "field_update", via: "appointment", relatedId: "ap-7", field: "status", after: "paid", wouldApply: true },
    });
    expect(s.verdict).toBe("pass");
    expect(s.rows).toEqual([
      { label: "Bản ghi liên quan", value: "appointment · ap-7", mono: true },
      { label: "Trường", value: "status", mono: true },
      { label: "Sau", value: "paid" },
    ]);
  });

  it("field_update on a related record that is not linked: will not run, and the note says why", () => {
    const note = "bản ghi chưa liên kết \"appointment\" — không có bản ghi liên quan để cập nhật";
    const s = shapeTraceStep({
      step: 3,
      kind: "action",
      evaluated: false,
      detail: { type: "field_update", via: "appointment", field: "status", after: "paid", wouldApply: false, note },
    });
    expect(s.warning).toBe(note);
    expect(s.rows[0]).toEqual({ label: "Bản ghi liên quan", value: "appointment · —", mono: true });
  });

  it("field_update with WF-01 error → blocked + error surfaced", () => {
    const s = shapeTraceStep({
      step: 3,
      kind: "action",
      evaluated: true,
      detail: {
        type: "field_update",
        field: "opp_code",
        wouldApply: false,
        error: "RECORD_AUTONUMBER_READONLY",
      },
    });
    expect(s.verdict).toBe("blocked");
    expect(s.error).toBe("RECORD_AUTONUMBER_READONLY");
  });

  it("action not evaluated (condition failed upstream) → skipped", () => {
    const s = shapeTraceStep({
      step: 4,
      kind: "action",
      evaluated: false,
      detail: { type: "email_alert", to: "a@b.vn", subject: "s", body: "b", wouldSend: false },
    });
    expect(s.verdict).toBe("skipped");
    expect(s.verdictLabel).toBe("Không chạy");
  });

  it("email wouldSend=false without error → fail (honest 'would not send')", () => {
    const s = shapeTraceStep({
      step: 4,
      kind: "action",
      evaluated: true,
      detail: { type: "email_alert", to: "", subject: "s", body: "b", wouldSend: false },
    });
    expect(s.verdict).toBe("fail");
    expect(s.rows[0]).toEqual({ label: "Đến", value: "(rỗng)", mono: true });
  });

  it("webhook: url row + pretty payload", () => {
    const s = shapeTraceStep({
      step: 5,
      kind: "action",
      evaluated: true,
      detail: {
        type: "outbound_webhook",
        url: "https://x.vn/hook",
        payload: { event: "after_update", record: { id: "r1" } },
        wouldSend: true,
      },
    });
    expect(s.rows[0].value).toBe("https://x.vn/hook");
    expect(s.payload).toContain('"event": "after_update"');
  });

  it("webhook payload given as JSON string is re-indented", () => {
    const s = shapeTraceStep({
      step: 5,
      kind: "action",
      evaluated: true,
      detail: { type: "outbound_webhook", url: "https://x.vn", payload: '{"a":1}', wouldSend: true },
    });
    expect(s.payload).toBe('{\n  "a": 1\n}');
  });

  it("unknown action type dumps detail keys instead of hiding them", () => {
    const s = shapeTraceStep({
      step: 6,
      kind: "action",
      evaluated: true,
      // NOT create_record any more — that is a shipped action with a Vietnamese label now. The
      // property under test is the fallback for a type this build genuinely does not know.
      detail: { type: "teleport_record", entity: "tasks" },
    });
    expect(s.title).toBe("teleport_record");
    expect(s.rows).toEqual([{ label: "entity", value: "tasks" }]);
  });
});

describe("FIELD_TARGET_ACTIONS", () => {
  // The builder pins a 422's details.field onto the offending ACTION ROW. It used to test for
  // field_update alone, so `assign` and `set_deadline` — which raise the same codes from the same
  // guards — dropped their message into the generic footer, where a rule with several actions
  // gives no clue which one to fix.
  it("covers every action whose config names a target field", () => {
    expect([...FIELD_TARGET_ACTIONS].sort()).toEqual(["assign", "field_update", "normalize", "set_deadline", "set_elapsed", "veto"]);
  });

  it("excludes actions that do not target a field on the trigger record", () => {
    for (const t of ["in_app_notification", "email_alert", "outbound_webhook", "create_record"]) {
      expect(FIELD_TARGET_ACTIONS.has(t)).toBe(false);
    }
  });
});

describe("shapeTraceStep — set_deadline", () => {
  const step = {
    step: 3,
    kind: "action" as const,
    evaluated: true,
    detail: {
      type: "set_deadline",
      field: "due_date",
      before: null,
      offset: { amount: 4, unit: "hours" },
      after: "2026-09-11T08:23:30Z",
      wouldApply: true,
    },
  };

  it("reads like a date, not like a payload", () => {
    const out = shapeTraceStep(step);
    expect(out.title).toBe("Đặt hạn xử lý");
    expect(out.verdict).toBe("pass");
    expect(out.rows).toEqual([
      { label: "Trường", value: "due_date", mono: true },
      { label: "Cộng thêm", value: "4 giờ" },
      { label: "Trước", value: "—" },
      { label: "Hạn sẽ đặt", value: "15:23 11/09/2026", mono: true },
    ]);
    // The default arm would have emitted a raw {"amount":4,"unit":"hours"} row labelled "offset".
    expect(JSON.stringify(out.rows)).not.toContain('{\\"amount\\"');
  });

  it("surfaces a blocked preview instead of pretending it would run", () => {
    const out = shapeTraceStep({
      ...step,
      detail: { ...step.detail, wouldApply: false, error: 'field "subject" phải là kiểu datetime' },
    });
    expect(out.verdict).toBe("blocked");
    expect(out.error).toContain("datetime");
  });

  // The rows are a FIXED list, so a key the backend starts sending lands nowhere — the `default`
  // arm that dumps unknown keys is unreachable once an action has its own case. `calendar` is
  // exactly that key: the backend sets it when a business_hours deadline fell back to wall-clock
  // because the org has no working calendar. Losing it is not cosmetic — the instant looks
  // perfectly normal either way, so this note is the only thing on screen telling them apart.
  it("shows the wall-clock fallback note when the org has no working calendar", () => {
    const out = shapeTraceStep({
      ...step,
      detail: {
        ...step.detail,
        offset: { amount: 4, unit: "business_hours" },
        calendar: "Tổ chức chưa khai lịch làm việc — hạn sẽ tính theo giờ thường",
      },
    });
    expect(out.rows).toContainEqual({
      label: "Lịch làm việc",
      value: "Tổ chức chưa khai lịch làm việc — hạn sẽ tính theo giờ thường",
    });
  });

  it("says nothing about the calendar when the org HAS one", () => {
    const out = shapeTraceStep({
      ...step,
      detail: { ...step.detail, offset: { amount: 4, unit: "business_hours" } },
    });
    expect(out.rows.some((r) => r.label === "Lịch làm việc")).toBe(false);
    expect(out.rows.some((r) => r.label === "Lịch ghim")).toBe(false);
  });

  // Queue #79: a deadline pinned to its own calendar. "12 giờ làm việc → 16:34 thứ Tư" on a 24/7
  // pin reads like an arithmetic bug unless the trace says which clock it walked — by NAME when
  // the builder knows the calendar, by id when it does not (deleted: the `calendar` note explains).
  it("names the pinned calendar the deadline walked", () => {
    const pinned = { ...step, detail: { ...step.detail, offset: { amount: 12, unit: "business_hours" }, calendarId: "cal-247" } };
    expect(shapeTraceStep(pinned, { "cal-247": "Trực 24/7" }).rows).toContainEqual({ label: "Lịch ghim", value: "Trực 24/7" });
    expect(shapeTraceStep(pinned).rows).toContainEqual({ label: "Lịch ghim", value: "cal-247", mono: true });
  });

  // Queue #81: a tighten_only rule that meets a hand-typed deadline previews wouldApply:false with
  // a `keep` sentence and no error. Without these rows the dialog printed "Hạn sẽ đặt: <instant>" —
  // an instant the run will NOT write — under a bare "Không thực hiện", and the reason was lost.
  // anchor created_at also explains why "+4 giờ làm việc" can land on a date already past.
  it("says why a tighten_only rule keeps the deadline, and what it measured from", () => {
    const keep = "hạn trên bản ghi khác dấu hạn máy — là hạn gõ tay, không bao giờ bị đè";
    const out = shapeTraceStep({
      ...step,
      detail: {
        ...step.detail,
        before: "2027-03-01T09:00:00Z",
        offset: { amount: 4, unit: "business_hours" },
        after: "2026-09-28T06:00:00Z",
        mode: "tighten_only",
        stampField: "due_date_auto",
        anchor: "created_at",
        keep,
        wouldApply: false,
      },
    });
    expect(out.verdict).toBe("fail");
    expect(out.verdictLabel).toBe("Giữ hạn cũ");
    expect(out.error).toBeUndefined();
    expect(out.rows).toContainEqual({ label: "Giữ hạn cũ vì", value: keep });
    expect(out.rows).toContainEqual({ label: "Hạn tính được", value: "13:00 28/09/2026", mono: true });
    expect(out.rows.some((r) => r.label === "Hạn sẽ đặt")).toBe(false);
    expect(out.rows).toContainEqual({ label: "Mốc tính", value: "Lúc tạo bản ghi" });
    expect(out.rows).toContainEqual({ label: "Chế độ", value: "Chỉ rút ngắn — không lùi hạn đã hứa, không đè hạn gõ tay" });
    expect(out.rows).toContainEqual({ label: "Dấu hạn máy", value: "due_date_auto", mono: true });
  });

  it("keeps 'Hạn sẽ đặt' when a tighten_only rule WILL write", () => {
    const out = shapeTraceStep({
      ...step,
      detail: { ...step.detail, mode: "tighten_only", stampField: "due_date_auto" },
    });
    expect(out.verdict).toBe("pass");
    expect(out.rows).toContainEqual({ label: "Hạn sẽ đặt", value: "15:23 11/09/2026", mono: true });
    expect(out.rows.some((r) => r.label === "Giữ hạn cũ vì")).toBe(false);
    // No anchor key = the fire instant, the default every rule before #81 used — no extra row.
    expect(out.rows.some((r) => r.label === "Mốc tính")).toBe(false);
  });
});

describe("traceInstantVN", () => {
  // D6: a VN product never shows a UTC clock. The two shapes below arrive in the SAME trace row and
  // parse differently — `new Date` reads the RFC3339 one as UTC and the MySQL literal as LOCAL.
  it("renders an RFC3339 instant in Vietnam time", () => {
    expect(traceInstantVN("2026-09-11T08:23:30Z")).toBe("15:23 11/09/2026");
  });

  it("treats the persisted MySQL literal as UTC, not as the viewer's local clock", () => {
    // 2026-01-01 20:00:00 UTC is 2026-01-02 03:00 in VN — a naive parse would print 20:00 01/01
    // and, on the row whose job is comparing two instants, silently lose a day.
    expect(traceInstantVN("2026-01-01 20:00:00")).toBe("03:00 02/01/2026");
  });

  it("leaves a non-instant alone instead of printing Invalid Date", () => {
    expect(traceInstantVN(null)).toBe("—");
    expect(traceInstantVN("")).toBe("(rỗng)"); // same wording traceValue uses everywhere else
    expect(traceInstantVN("chưa đặt")).toBe("chưa đặt");
  });
});

// FE-stop-clock-action (specs/046): the dry-run of `set_elapsed` answers "how many working minutes
// would this write, counted from when". Without its own arm the default dumped raw wire keys
// ("from"/"after") and an ISO instant in UTC — the one widget whose job is letting a person check
// the number against the ticket they know.
describe("shapeTraceStep — set_elapsed", () => {
  const step = {
    step: 3,
    kind: "action" as const,
    evaluated: true,
    detail: {
      type: "set_elapsed",
      field: "resolution_minutes",
      before: null,
      from: "2026-09-25T09:00:00Z",
      after: 600,
      wouldApply: true,
    },
  };

  it("reads as minutes counted from the creation instant in Vietnam time", () => {
    const out = shapeTraceStep(step);
    expect(out.title).toBe("Ghi thời gian xử lý");
    expect(out.verdict).toBe("pass");
    expect(out.rows).toEqual([
      { label: "Trường", value: "resolution_minutes", mono: true },
      { label: "Trước", value: "—" },
      { label: "Tính từ (lúc tạo)", value: "16:00 25/09/2026" },
      { label: "Số phút sẽ ghi", value: "600 phút (≈ 10 giờ làm việc)", mono: true },
    ]);
    expect(out.warning).toBeUndefined();
  });

  it("groups thousands and rounds the hours the Vietnamese way", () => {
    const out = shapeTraceStep({ ...step, detail: { ...step.detail, before: 12, after: 1234 } });
    expect(out.rows).toContainEqual({ label: "Trước", value: "12" });
    expect(out.rows).toContainEqual({ label: "Số phút sẽ ghi", value: "1.234 phút (≈ 20,6 giờ làm việc)", mono: true });
  });

  it("does not claim hours for less than one hour", () => {
    const out = shapeTraceStep({ ...step, detail: { ...step.detail, after: 0 } });
    expect(out.rows).toContainEqual({ label: "Số phút sẽ ghi", value: "0 phút", mono: true });
  });

  // `calendar` is set only when the org has no working calendar and the minutes fall back to the
  // wall clock. The number looks normal either way, so the note is a WARNING, not a quiet row — and
  // the hours must not be called working hours.
  it("warns when the org has no working calendar and stops calling the hours working hours", () => {
    const note = "Tổ chức chưa khai lịch làm việc — số phút sẽ tính theo giờ thường";
    const out = shapeTraceStep({ ...step, detail: { ...step.detail, after: 5138, calendar: note } });
    expect(out.warning).toBe(note);
    expect(out.rows).toContainEqual({ label: "Số phút sẽ ghi", value: "5.138 phút (≈ 85,6 giờ)", mono: true });
  });

  it("a blocked preview carries the error and no number", () => {
    const out = shapeTraceStep({
      ...step,
      detail: {
        type: "set_elapsed",
        field: "subject",
        before: "Máy in hỏng",
        from: "2026-09-25T09:00:00Z",
        after: 600,
        wouldApply: false,
        error: 'field "subject" phải là kiểu number (đang là text) — SET_ELAPSED_FIELD_TYPE_INVALID',
      },
    });
    expect(out.verdict).toBe("blocked");
    expect(out.error).toContain("SET_ELAPSED_FIELD_TYPE_INVALID");
    const noNumber = shapeTraceStep({
      ...step,
      detail: { type: "set_elapsed", field: "resolution_minutes", before: null, wouldApply: false, error: "after_delete: …" },
    });
    expect(noNumber.rows).toContainEqual({ label: "Tính từ (lúc tạo)", value: "—" });
    expect(noNumber.rows).toContainEqual({ label: "Số phút sẽ ghi", value: "—", mono: true });
  });
});

describe("actionTitle", () => {
  it("names every shipped action in Vietnamese and passes an unknown token through", () => {
    expect(actionTitle("set_elapsed")).toBe("Ghi thời gian xử lý");
    expect(actionTitle("set_deadline")).toBe("Đặt hạn xử lý");
    expect(actionTitle("veto_x")).toBe("veto_x");
  });
});

// specs/048 S3 (T064): the runs the "before" rules leave — a refusal is "Đã chặn", an exempt system
// write names why the rule let it through, and an odd shape never breaks the history screen.
describe("before-rule runs", () => {
  it("blocked ⇒ 'Đã chặn' in the error tone", () => {
    expect(runStatusMeta("blocked")).toEqual({ tone: "dg", label: "Đã chặn" });
    expect(runStatusMeta("blocked", [{ type: "veto", field: "close_date", ok: true }])).toEqual({ tone: "dg", label: "Đã chặn" });
  });
  it("skipped with an exemption reason names it", () => {
    expect(runStatusMeta("skipped", [{ type: "veto", field: "status", ok: true, skipped: "approval_outcome" }])).toEqual({
      tone: "neu",
      label: "Bỏ qua (ghi kết quả duyệt)",
    });
    expect(runStatusMeta("skipped", [{ type: "veto", ok: true, skipped: "payment_match" }])).toEqual({
      tone: "neu",
      label: "Bỏ qua (đối soát thanh toán)",
    });
  });
  it("skipped without a known reason stays 'Bỏ qua'; odd shapes never break", () => {
    expect(runStatusMeta("skipped", null)).toEqual({ tone: "neu", label: "Bỏ qua" });
    expect(runStatusMeta("skipped", [{ type: "veto", skipped: "who_knows" }])).toEqual({ tone: "neu", label: "Bỏ qua" });
    expect(runStatusMeta("skipped", { scanned: 3 })).toEqual({ tone: "neu", label: "Bỏ qua" });
    expect(runStatusMeta("skipped", ["junk", 7])).toEqual({ tone: "neu", label: "Bỏ qua" });
    expect(runStatusMeta("blocked", "garbage")).toEqual({ tone: "dg", label: "Đã chặn" });
  });
  it("the two before triggers read in Vietnamese", () => {
    expect(triggerLabel("before_save")).toBe("Trước khi lưu");
    expect(triggerLabel("before_delete")).toBe("Trước khi xoá");
  });
  it("a batch summary is its own shape, not a scheduled scan", () => {
    expect(shapeActionsResult({ blocked: 2, applied: 0, failed: 0 })).toEqual({ kind: "batch", blocked: 2, applied: 0, failed: 0 });
  });
  it("the veto action has a Vietnamese title", () => {
    expect(actionTitle("veto")).toBe("Chặn");
  });
});
