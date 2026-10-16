// #277: datetime shows date AND time; the relative hint points the right way (a future due time was
// "vừa xong" because relativeVN only handled now - d >= 0).
import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { FieldDisplay, relativeVN } from "@/components/fields/display";
import type { FieldSpec } from "@/lib/metadata";

const H = 3_600_000;
const now = new Date("2026-10-07T03:00:00Z").getTime();
const at = (ms: number) => new Date(now + ms);

describe("relativeVN", () => {
  it.each([
    ["vừa qua", -10 * 60_000, "vừa xong"],
    ["quá khứ giờ", -5 * H, "5 giờ trước"],
    ["quá khứ ngày", -49 * H, "2 ngày trước"],
    ["tương lai <1 giờ", 30 * 60_000, "còn dưới 1 giờ"],
    ["tương lai giờ", 6 * H + 60_000, "còn 6 giờ"],
    ["tương lai ngày", 50 * H, "còn 2 ngày"],
  ])("%s", (_n, delta, want) => {
    expect(relativeVN(at(delta), now)).toBe(want);
  });
});

describe("FieldDisplay datetime", () => {
  it("in đủ giờ phút theo giờ Việt Nam", () => {
    const f = { name: "hen", label: "Hẹn", fieldType: "datetime" } as unknown as FieldSpec;
    const { container } = render(<FieldDisplay field={f} value="2026-10-01T02:00:00Z" />);
    expect(container.textContent).toContain("01/10/2026");
    expect(container.textContent).toContain("09:00");
  });

  // Server dialect: space separator, no zone, stored UTC. Must not depend on the browser TZ.
  it.each(["Asia/Ho_Chi_Minh", "UTC", "America/New_York"])("dạng server không múi giờ đọc là UTC (TZ=%s)", (tz) => {
    const prev = process.env.TZ;
    process.env.TZ = tz;
    try {
      const f = { name: "hen", label: "Hẹn", fieldType: "datetime" } as unknown as FieldSpec;
      const { container } = render(<FieldDisplay field={f} value="2026-10-01 02:00:00" />);
      expect(container.textContent).toContain("01/10/2026");
      expect(container.textContent).toContain("09:00");
    } finally {
      process.env.TZ = prev;
    }
  });
});
