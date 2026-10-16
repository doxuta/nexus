import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { appendToken, relationTargets, templateVarGroups } from "./workflowTemplateVars";

// Lịch hẹn → Bệnh nhân (relation) + email_bn (lookup mirroring the patient's email).
const APPOINTMENT: FieldMeta[] = [
  { name: "tieu_de", label: "Tiêu đề", fieldType: "text" },
  { name: "gio_hen", label: "Giờ hẹn", fieldType: "datetime" },
  { name: "phi", label: "Phí", fieldType: "currency" },
  { name: "benh_nhan", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan" } },
  { name: "email_bn", label: "Email bệnh nhân", fieldType: "lookup", options: { via: "benh_nhan", sourceField: "email" } },
  { name: "ten_bn", label: "Tên bệnh nhân", fieldType: "lookup", options: { via: "benh_nhan", sourceField: "ho_ten" } },
];
const PATIENT: FieldMeta[] = [
  { name: "ho_ten", label: "Họ tên", fieldType: "text" },
  { name: "email", label: "Email", fieldType: "email" },
  { name: "bac_si", label: "Bác sĩ", fieldType: "relation", options: { relatedEntity: "bac_si" } },
];

describe("templateVarGroups", () => {
  it("offers the linked patient's email for 'Gửi tới' — and nothing that is not an address", () => {
    const groups = templateVarGroups(APPOINTMENT, { benh_nhan: PATIENT }, true);
    expect(groups.map((g) => g.vars.map((v) => v.token))).toEqual([["{{email_bn}}"], ["{{benh_nhan.email}}"]]);
    expect(groups[1].vars[0].label).toBe("Bệnh nhân › Email");
  });

  it("offers every renderable field for subject/body — no ids, no currency objects", () => {
    const [own, patient] = templateVarGroups(APPOINTMENT, { benh_nhan: PATIENT }, false);
    expect(own.vars.map((v) => v.token)).toEqual(["{{tieu_de}}", "{{gio_hen}}", "{{email_bn}}", "{{ten_bn}}"]);
    expect(patient.vars.map((v) => v.token)).toEqual(["{{benh_nhan.ho_ten}}", "{{benh_nhan.email}}"]);
  });

  it("a relation whose fields are not loaded yet has no group (and a lookup onto it is not called an email)", () => {
    expect(templateVarGroups(APPOINTMENT, {}, true)).toEqual([]);
  });
});

// LMS/Ticketing/CRM shape: every built-in pack declares the address as a TEXT field named "email"
// (vertical_pack/builtin.go — lms_students.email, tk_customers.email), and a field's type cannot be
// changed afterwards (FIELD_TYPE_IMMUTABLE).
const ENROLMENT: FieldMeta[] = [
  { name: "ma", label: "Mã", fieldType: "text" },
  { name: "student", label: "Học viên", fieldType: "relation", options: { relatedEntity: "lms_students" } },
  { name: "email_hv", label: "Mail học viên", fieldType: "lookup", options: { via: "student", sourceField: "email" } },
  { name: "hoc_phi_hv", label: "Học phí HV", fieldType: "lookup", options: { via: "student", sourceField: "hoc_phi" } },
  { name: "tong", label: "Tổng buổi", fieldType: "rollup", options: { function: "count" } },
  { name: "lien_quan", label: "Liên quan", fieldType: "polymorphic_relation", options: { targetEntities: ["lms_students"] } },
  { name: "thanh_tien", label: "Thành tiền", fieldType: "formula", options: { expression: "a * b", resultType: "currency" } },
  { name: "diem_tb", label: "Điểm TB", fieldType: "formula", options: { expression: "a + b" } },
];
const STUDENT: FieldMeta[] = [
  { name: "full_name", label: "Họ tên", fieldType: "text" },
  { name: "email", label: "Email", fieldType: "text" },
  { name: "contact", label: "E-mail phụ huynh", fieldType: "text" },
  { name: "ghi_chu", label: "Ghi chú", fieldType: "text" },
  { name: "so_buoi", label: "Số buổi", fieldType: "rollup", options: { function: "count" } },
  { name: "hoc_phi", label: "Học phí", fieldType: "currency" },
];

describe("templateVarGroups — pack shapes and values that never print", () => {
  it("'Gửi tới' offers a TEXT field named or labelled email (every built-in pack), not other text", () => {
    const groups = templateVarGroups(ENROLMENT, { lms_students: STUDENT }, true);
    expect(groups.map((g) => g.vars.map((v) => v.token))).toEqual([
      ["{{email_hv}}"],
      ["{{student.email}}", "{{student.contact}}"],
    ]);
  });

  it("subject/body never offers a value the engine renders empty: rollup, polymorphic, currency formula, a lookup of a currency", () => {
    const [own, student] = templateVarGroups(ENROLMENT, { lms_students: STUDENT }, false);
    expect(own.vars.map((v) => v.token)).toEqual(["{{ma}}", "{{email_hv}}", "{{diem_tb}}"]);
    expect(student.vars.map((v) => v.token)).toEqual([
      "{{student.full_name}}", "{{student.email}}", "{{student.contact}}", "{{student.ghi_chu}}",
    ]);
  });
});

// DAILY-QUEUE #336: a `user` field is a group of its own — the person's email and name.
describe("templateVarGroups — user fields", () => {
  const TICKET: FieldMeta[] = [
    { name: "tieu_de", label: "Tiêu đề", fieldType: "text" },
    { name: "nguoi_phu_trach", label: "Người phụ trách", fieldType: "user" },
  ];

  it("offers the responsible person's email for 'Gửi tới', no name", () => {
    const groups = templateVarGroups(TICKET, {}, true);
    expect(groups.map((g) => g.vars.map((v) => v.token))).toEqual([["{{nguoi_phu_trach.email}}"]]);
    expect(groups[0].vars[0].label).toBe("Người phụ trách › Email");
  });

  it("offers email and name for subject/body, still no raw id of the user field itself", () => {
    const [own, owner] = templateVarGroups(TICKET, {}, false);
    expect(own.vars.map((v) => v.token)).toEqual(["{{tieu_de}}"]);
    expect(owner.vars.map((v) => v.token)).toEqual(["{{nguoi_phu_trach.email}}", "{{nguoi_phu_trach.name}}"]);
  });
});

describe("helpers", () => {
  it("relationTargets lists each linked entity once", () => {
    expect(relationTargets([...APPOINTMENT, { name: "bn2", fieldType: "relation", options: { relatedEntity: "benh_nhan" } }])).toEqual(["benh_nhan"]);
  });
  it("appendToken adds one space, never two", () => {
    expect(appendToken("", "{{a}}")).toBe("{{a}}");
    expect(appendToken("Chào", "{{a}}")).toBe("Chào {{a}}");
    expect(appendToken("Chào ", "{{a}}")).toBe("Chào {{a}}");
  });
});
