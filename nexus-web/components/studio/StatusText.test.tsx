import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { render, screen } from "@testing-library/react";
import { StatusText, draftStateLabel } from "./StatusText";

describe("StatusText", () => {
  it.each(["success", "error", "warn", "weak", "proposed"] as const)("tone %s -> nx-status nx-status_%s", (tone) => {
    render(<StatusText tone={tone}>x</StatusText>);
    const el = screen.getByText("x");
    expect(el.className).toBe(`nx-status nx-status_${tone}`);
  });

  it("does not import Badge or Pill", () => {
    const src = readFileSync("components/studio/StatusText.tsx", "utf8");
    expect(src).not.toMatch(/import[^;]*\b(Badge|Pill)\b/);
  });
});

describe("draftStateLabel (table 4.8)", () => {
  it.each([
    ["edited", undefined, "warn", "Nháp"],
    ["new", undefined, "warn", "Mới, khách thấy khi xuất bản"],
    ["deleted", undefined, "warn", "Sẽ xoá khi xuất bản"],
    ["requiredOnPublish", undefined, "warn", "Bắt buộc sẽ bật khi xuất bản"],
    ["live", "direct", "warn", "Áp ngay cho người dùng thật"],
    ["live", "untracked", "warn", "Áp ngay, không có trong lịch sử"],
    ["deleteFailed", undefined, "error", "Xoá chưa xong · Thử lại"],
    ["proposed", undefined, "proposed", "Đề xuất"],
    ["inUse", undefined, "success", "Đang dùng"],
    ["complete", undefined, "success", "Đã đủ"],
    ["connecting", undefined, "success", "Đang kết nối"],
    ["locked", undefined, "error", "Đã khoá"],
    ["error", undefined, "error", "Lỗi"],
    ["viewOnly", undefined, "weak", "Chỉ xem"],
    ["hidden", undefined, "warn", "Không thấy"],
  ] as const)("%s/%s -> %s %s", (state, kind, tone, label) => {
    expect(draftStateLabel(state, kind)).toEqual({ tone, label });
  });

  it("shows nothing for a live/absent draftable row", () => {
    expect(draftStateLabel("live")).toBeNull();
    expect(draftStateLabel(undefined)).toBeNull();
  });
});
