import { describe, expect, it } from "vitest";
import type { EntityMeta, FieldMeta, WorkflowDetail } from "@/lib/api";
import { ACTION_TYPES, TRIGGERS } from "@/components/studio/WorkflowBuilder";
import { ruleSentence } from "./ruleSentence";

const entities = [
  { name: "tk_tickets", label: "Phiếu", tableName: "tk_tickets" },
  { name: "tk_tasks", label: "Việc", tableName: "tk_tasks" },
] as EntityMeta[];
const fields = [
  { name: "status", label: "Trạng thái", fieldType: "select" },
  { name: "due", label: "Hạn xử lý", fieldType: "datetime" },
  { name: "assignee", label: "Người xử lý", fieldType: "text" },
  { name: "customer", label: "Khách", fieldType: "relation" },
] as FieldMeta[];

const wf = (over: Record<string, unknown>): WorkflowDetail =>
  ({ id: "w1", name: "r", entityName: "tk_tickets", trigger: "after_create", actions: [], ...over }) as WorkflowDetail;

describe("ruleSentence", () => {
  it.each<[string, Record<string, unknown>, string]>([
    ["after_create", {}, "Phiếu được tạo"],
    ["after_update", { trigger: "after_update" }, "Phiếu được sửa"],
    ["after_delete", { trigger: "after_delete" }, "Phiếu bị xoá"],
    ["scheduled", { trigger: "scheduled", intervalSeconds: 7200 }, "Cứ mỗi 2 giờ, với Phiếu"],
    ["scheduled once", { trigger: "scheduled", intervalSeconds: 0 }, "Một lần theo lịch, với Phiếu"],
    ["date_offset", { trigger: "date_offset", condition: { timer: { dateField: "due", offset: { amount: 3, unit: "days" }, direction: "before" } } }, "Đến lúc 3 ngày trước Hạn xử lý, với Phiếu"],
    ["before_save both", { trigger: "before_save" }, "Phiếu sắp được tạo hoặc sửa"],
    ["before_save create", { trigger: "before_save", condition: { saveOn: "create" } }, "Phiếu sắp được tạo"],
    ["before_save update", { trigger: "before_save", condition: { saveOn: "update" } }, "Phiếu sắp được sửa"],
    ["before_delete", { trigger: "before_delete" }, "Phiếu sắp bị xoá"],
  ])("Khi: %s", (_n, over, expected) => {
    expect(ruleSentence(wf(over), entities, fields).when).toBe(expected);
  });

  it("Nếu: empty condition reads 'luôn luôn'; rules join by match", () => {
    expect(ruleSentence(wf({}), entities, fields).cond).toBe("luôn luôn");
    const rules = [{ field: "status", op: "eq", value: "Khẩn" }, { field: "assignee", op: "isnull" }];
    expect(ruleSentence(wf({ condition: { match: "all", rules } }), entities, fields).cond).toBe("Trạng thái = Khẩn và Người xử lý trống");
    expect(ruleSentence(wf({ condition: { match: "any", rules } }), entities, fields).cond).toContain(" hoặc ");
    expect(ruleSentence(wf({ condition: { rules: [{ field: "status", op: "eq", value: { field: "assignee" } }] } }), entities, fields).cond).toBe("Trạng thái = Người xử lý");
  });

  it.each<[string, Record<string, unknown>, string]>([
    ["field_update", { type: "field_update", field: "status", value: "Mới" }, "Đặt Trạng thái thành Mới"],
    ["field_update via", { type: "field_update", field: "status", value: "x", via: "customer" }, "Đặt Trạng thái của Khách liên quan thành x"],
    ["field_update child", { type: "field_update", field: "status", value: "x", childEntity: "tk_tasks", childVia: "t" }, "Đặt Trạng thái cho mọi Việc liên quan thành x"],
    ["in_app_notification", { type: "in_app_notification", title: "Có phiếu mới" }, "Báo trong app: Có phiếu mới"],
    ["email_alert", { type: "email_alert", to: "a@b.vn" }, "Gửi email tới a@b.vn"],
    ["outbound_webhook", { type: "outbound_webhook" }, "Gửi dữ liệu sang hệ thống khác"],
    ["assign", { type: "assign", field: "assignee" }, "Chia việc vào Người xử lý lần lượt cho cả nhóm"],
    ["create_record", { type: "create_record", entityName: "tk_tasks" }, "Tạo Việc mới"],
    ["set_deadline", { type: "set_deadline", field: "due", offset: { amount: 4, unit: "hours" } }, "Đặt hạn vào Hạn xử lý: 4 giờ"],
    ["set_deadline calendar", { type: "set_deadline", field: "due", offset: { amount: 4, unit: "business_hours" }, calendarId: "c1" }, "Đặt hạn vào Hạn xử lý: 4 giờ làm việc, theo lịch làm việc đã chọn"],
    ["set_elapsed", { type: "set_elapsed", field: "due" }, "Ghi thời gian xử lý vào Hạn xử lý"],
    ["normalize", { type: "normalize", field: "assignee" }, "Chuẩn hoá chữ ở Người xử lý"],
    ["veto", { type: "veto", message: "Thiếu tên" }, "Chặn lại và báo: Thiếu tên"],
    ["start_approval", { type: "start_approval", processId: "p1" }, "Gửi bản ghi đi duyệt"],
  ])("Thì: %s", (_n, action, expected) => {
    expect(ruleSentence(wf({ actions: [action] }), entities, fields).then).toEqual([expected]);
  });

  // Drift guard: a new trigger/action key added to the builder without a sentence turns this red.
  it("covers every trigger and action key of the builder, never empty and never a raw code", () => {
    for (const trigger of TRIGGERS) {
      const s = ruleSentence(wf({ trigger, intervalSeconds: 60 }), entities, fields).when;
      expect(s, trigger).not.toBe("");
      expect(s, trigger).not.toContain(trigger);
      expect(s, trigger).not.toMatch(/\{\w+\}/);
    }
    for (const { key } of ACTION_TYPES) {
      const [s] = ruleSentence(wf({ actions: [{ type: key, field: "status" }] }), entities, fields).then;
      expect(s, key).toBeTruthy();
      expect(s, key).not.toBe("Một hành động chưa đọc được");
      expect(s, key).not.toContain(key);
      expect(s, key).not.toMatch(/\{\w+\}/);
    }
  });

  it("an unknown action reads neutrally", () => {
    expect(ruleSentence(wf({ actions: [{ type: "teleport" }] }), entities, fields).then).toEqual(["Một hành động chưa đọc được"]);
  });
});
