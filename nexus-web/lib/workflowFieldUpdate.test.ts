import { describe, it, expect } from "vitest";
import type { FieldMeta } from "@/lib/api";
import {
  relationChoices, fuTargetEntity, fuTargetFields, fuWritableFields, fuParamsFromAction, fuWidgetValue, fuWithValue, fuRetarget,
  fuPayload, fuValueMissing,
} from "./workflowFieldUpdate";

const INVOICE: FieldMeta[] = [
  { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["unpaid", "paid"] } },
  { name: "appointment", label: "Lịch hẹn", fieldType: "relation", options: { relatedEntity: "appointments" } },
  { name: "old_many", label: "Cũ", fieldType: "relation", options: { relatedEntity: "appointments", cardinality: "N-N" } },
  { name: "regarding", label: "Liên quan", fieldType: "polymorphic_relation", options: { targetEntities: ["appointments"] } },
  { name: "created_by", label: "Người tạo", fieldType: "text" },
];
const APPOINTMENT: FieldMeta[] = [
  { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["booked", "paid"] } },
  { name: "fee", label: "Phí", fieldType: "number" },
  { name: "total", label: "Tổng", fieldType: "formula" },
  { name: "doctor", label: "Bác sĩ", fieldType: "user" },
  { name: "scan", label: "Ảnh chụp", fieldType: "image" },
  { name: "deposit", label: "Tiền cọc", fieldType: "currency", options: { currencyCode: "USD" } },
  { name: "confirmed", label: "Đã xác nhận", fieldType: "boolean" },
];

describe("field_update editor helpers (specs/058)", () => {
  it("offers only to-one relations that name a target", () => {
    expect(relationChoices(INVOICE)).toEqual([{ name: "appointment", label: "Lịch hẹn", target: "appointments" }]);
    expect(fuTargetEntity("invoices", INVOICE, "appointment")).toBe("appointments");
    expect(fuTargetEntity("invoices", INVOICE, undefined)).toBe("invoices");
    expect(fuTargetEntity("invoices", INVOICE, "old_many")).toBeUndefined();
  });

  it("writable fields drop system and computed ones", () => {
    expect(fuWritableFields(APPOINTMENT).map((f) => f.name)).toEqual(["status", "fee", "doctor", "deposit", "confirmed"]);
    expect(fuWritableFields(INVOICE).map((f) => f.name)).toEqual(["status", "appointment", "old_many"]);
  });

  it("an opened rule saved untouched sends the stored value back as stored (a number stays a number)", () => {
    const p = fuParamsFromAction({ type: "field_update", via: "appointment", field: "fee", value: 100 });
    expect(p).toEqual({ field: "fee", via: "appointment", raw: "100" });
    expect(fuPayload(p, APPOINTMENT)).toEqual({ type: "field_update", via: "appointment", field: "fee", value: 100 });
    expect(fuWidgetValue(p, APPOINTMENT[1])).toBe(100);
  });

  it("an edited value is emitted typed for the target field", () => {
    const p = fuWithValue({ field: "fee", via: "appointment", raw: "1" }, "250");
    expect(p.raw).toBeUndefined();
    expect(fuPayload(p, APPOINTMENT)).toEqual({ type: "field_update", via: "appointment", field: "fee", value: 250 });
  });

  it("changing the target record resets the field and value; no via = the triggering record", () => {
    const p = fuRetarget({ field: "status", raw: "\"paid\"" }, { via: "appointment" }, undefined);
    expect(p).toEqual({ via: "appointment", field: "" });
    const q = fuRetarget(p, { field: "status" }, APPOINTMENT[0]);
    expect(fuWidgetValue(q, APPOINTMENT[0])).toBe("");
    const back = fuRetarget(q, { via: "" }, undefined);
    expect(back.via).toBeUndefined();
    expect(fuPayload({ ...back, field: "status", values: JSON.stringify([["status", "paid"]]) }, INVOICE))
      .toEqual({ type: "field_update", field: "status", value: "paid" });
  });

  it("the value is typed against the RELATED entity's fields for a via, the rule's own without one", () => {
    const byEntity = { appointments: APPOINTMENT };
    expect(fuTargetFields("invoices", INVOICE, byEntity, "appointment")).toBe(APPOINTMENT);
    expect(fuTargetFields("invoices", INVOICE, byEntity, undefined)).toBe(INVOICE);
    expect(fuTargetFields("invoices", INVOICE, {}, "appointment")).toEqual([]);     // not loaded yet
    expect(fuTargetFields("invoices", INVOICE, byEntity, "old_many")).toEqual([]);  // not a to-one relation
  });

  // Review finding: an empty value used to save as {type, via, field} with no value — the backend read
  // it as null and every run silently CLEARED the related record's field. The builder now refuses to
  // save it; clearing stays an API-only, explicit `value: null`.
  it("a chosen field with no value is missing — never emitted as a silent clear", () => {
    const deposit = APPOINTMENT.find((f) => f.name === "deposit");
    const status = APPOINTMENT[0];
    const picked = (meta: FieldMeta) => fuRetarget({ via: "appointment" }, { field: meta.name }, meta);
    expect(fuValueMissing(picked(status), APPOINTMENT)).toBe(true);
    expect(fuValueMissing(picked(deposit!), APPOINTMENT)).toBe(true);
    // fields of the related entity not loaded yet: the raw widget value passes through, "" is still empty
    expect(fuValueMissing(picked(status), [])).toBe(true);
    // a stored rule that carries no value at all
    expect(fuValueMissing({ field: "status", via: "appointment" }, APPOINTMENT)).toBe(true);
  });

  it("a real value, a false checkbox, an untouched stored value, or no field yet is not missing", () => {
    const pickedStatus = fuRetarget({ via: "appointment" }, { field: "status" }, APPOINTMENT[0]);
    expect(fuValueMissing(fuWithValue(pickedStatus, "paid"), APPOINTMENT)).toBe(false);
    const confirmed = APPOINTMENT.find((f) => f.name === "confirmed")!;
    expect(fuValueMissing(fuRetarget({}, { field: "confirmed" }, confirmed), APPOINTMENT)).toBe(false);
    expect(fuValueMissing(fuParamsFromAction({ type: "field_update", via: "appointment", field: "fee", value: 100 }), APPOINTMENT)).toBe(false);
    // An explicit null saved through the API travels back untouched (spec: opening and saving changes nothing).
    expect(fuValueMissing(fuParamsFromAction({ type: "field_update", field: "status", value: null }), APPOINTMENT)).toBe(false);
    expect(fuValueMissing({ via: "appointment", field: "" }, APPOINTMENT)).toBe(false);
  });

  // specs/048 S4 review: a before_save field_update carries onlyIfEmpty and valueSource. Opening the rule in
  // Studio and saving it untouched dropped both — the server then read "{{company}}" / a field name as a
  // constant, and "fill only when empty" as "overwrite on every save".
  it("a before_save rule's onlyIfEmpty and valueSource survive open → save", () => {
    const tmpl = { type: "field_update", field: "source", value: "{{company}}", onlyIfEmpty: true, valueSource: "template" };
    expect(fuPayload(fuParamsFromAction(tmpl), INVOICE)).toEqual(tmpl);
    const copy = { type: "field_update", field: "fee", value: "deposit", valueSource: "field" };
    expect(fuPayload(fuParamsFromAction(copy), APPOINTMENT)).toEqual(copy);
    // absent stays absent: an after_* rule refuses both keys
    expect(fuPayload(fuParamsFromAction({ type: "field_update", field: "fee", value: 1, onlyIfEmpty: false }), APPOINTMENT))
      .toEqual({ type: "field_update", field: "fee", value: 1 });
  });

  it("an edited template stays a template; picking another field drops the source, keeps onlyIfEmpty", () => {
    const NOTE: FieldMeta[] = [{ name: "note", label: "Ghi chú", fieldType: "text" }, APPOINTMENT[0]];
    const p = fuParamsFromAction({ type: "field_update", field: "note", value: "{{name}}", onlyIfEmpty: true, valueSource: "template" });
    expect(fuPayload(fuWithValue(p, "{{name}} — web"), NOTE))
      .toEqual({ type: "field_update", field: "note", value: "{{name}} — web", onlyIfEmpty: true, valueSource: "template" });
    const moved = fuRetarget(p, { field: "status" }, APPOINTMENT[0]);
    expect(moved.valueSource).toBeUndefined();
    expect(fuPayload(fuWithValue(moved, "paid"), NOTE)).toEqual({ type: "field_update", field: "status", value: "paid", onlyIfEmpty: true });
  });

  it("a related currency value is emitted as money of the RELATED field's currency", () => {
    const deposit = APPOINTMENT.find((f) => f.name === "deposit")!;
    const p = fuWithValue(fuRetarget({ via: "appointment" }, { field: "deposit" }, deposit), "250");
    expect(fuPayload(p, APPOINTMENT)).toEqual({ type: "field_update", via: "appointment", field: "deposit", value: { amount: 250, currencyCode: "USD" } });
  });
});
