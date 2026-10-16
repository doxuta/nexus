import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { FieldDisplay } from "@/components/fields/display";
import { isExpandableType, valueLabel, type FieldSpec } from "@/lib/metadata";

const spec = (fieldType: string): FieldSpec =>
  ({ name: "assigned_to", label: "Phân công cho", fieldType, isRequired: false, options: {} } as unknown as FieldSpec);

// MỘT XƯỞNG (2026-07-29) — tìm ra khi đóng vai nhân viên bán hàng mở một lead thật: dòng
// "Phân công cho" in ra `b1551a1a-5b97-4640-91b5-f49d13bb118f`. Chính ô đó tồn tại để trả lời
// "ai phụ trách" thì lại không trả lời được.
//
// Backend không hề thiếu: kiểu field `user` có validate người-thật-đang-hoạt-động, và `?expand=`
// trả {id,label,email}. Thiếu là ĐOẠN NỐI:
//   1. `lib/metadata` không có "user" trong union FieldType → cả kiểu này vô hình với FE;
//   2. danh sách expand ở LayoutList và LayoutDetail đều lọc cứng `=== "relation"` — hai file,
//      cùng một thiếu sót, nên field `user` KHÔNG BAO GIỜ được giải tên ở bất kỳ màn nào;
//   3. `display.tsx` không có case "user" → giá trị đã expand rơi vào default String(value),
//      tức là in ra "[object Object]" — còn tệ hơn UUID.
// Ghim cả ba: một vị-ngữ dùng chung cho expand, và bậc thang tên → email → id.
describe("field kiểu `user` — hiện TÊN người, không phải UUID", () => {
  it("expand đủ → hiện tên (label)", () => {
    render(<FieldDisplay field={spec("user")} value={{ id: "u1", label: "Trần Sales Rep", email: "test@example.com" }} />);
    expect(screen.getByText("Trần Sales Rep")).toBeInTheDocument();
    expect(screen.queryByText(/object Object/)).toBeNull();
    expect(screen.queryByText("u1")).toBeNull();
  });

  it("người dùng chưa đặt họ tên → lùi về email, KHÔNG bịa tên", () => {
    render(<FieldDisplay field={spec("user")} value={{ id: "u1", label: "", email: "test@example.com" }} />);
    expect(screen.getByText("test@example.com")).toBeInTheDocument();
  });

  it("không có cả tên lẫn email → hiện id trần, tuyệt đối không [object Object]", () => {
    render(<FieldDisplay field={spec("user")} value={{ id: "u1" }} />);
    expect(screen.getByText("u1")).toBeInTheDocument();
    expect(screen.queryByText(/object Object/)).toBeNull();
  });

  it("caller quên ?expand= → hiện id trần chứ không rỗng (nói thật về thứ đang có)", () => {
    render(<FieldDisplay field={spec("user")} value="b1551a1a-5b97-4640-91b5-f49d13bb118f" />);
    expect(screen.getByText("b1551a1a-5b97-4640-91b5-f49d13bb118f")).toBeInTheDocument();
  });
});

describe("isExpandableType — MỘT nguồn cho danh sách ?expand=", () => {
  it("phủ cả relation LẪN user (user bị bỏ quên ở cả hai màn trước 2026-07-29)", () => {
    expect(isExpandableType("relation")).toBe(true);
    expect(isExpandableType("user")).toBe(true);
  });

  it("không expand các kiểu vô-hại — chỉ id trần mới cần dịch sang nhãn", () => {
    for (const t of ["text", "number", "picklist", "polymorphic_relation", "lookup", undefined]) {
      expect(isExpandableType(t)).toBe(false);
    }
  });
});

// ── valueLabels (B-PICKLIST-VN) — nhãn tiếng Việt trên MÃ tiếng Anh ──────────────────────────────
//
// options.values là MÃ lưu trữ và từng kiêm luôn chữ hiện ra, nên pack seed mã tiếng Anh là mặt
// khách đọc "New"/"Web"/"Urgent" nguyên văn. Dịch MÃ thì mồ côi record + phá queue/report/workflow
// (tất cả so mã bằng chuỗi tuyệt đối) — nên tiếng Việt sống ở tầng nhãn options.valueLabels, và mã
// không đổi một ký tự. Không có nhãn → hiện nguyên mã, không bịa.
describe("picklist valueLabels — người đọc thấy nhãn, hệ thống giữ mã", () => {
  const spec2 = (fieldType: string, options: Record<string, unknown>): FieldSpec =>
    ({ name: "status", label: "Trạng thái", fieldType, isRequired: false, options } as unknown as FieldSpec);

  it("resolver: có nhãn → nhãn; không có → nguyên mã", () => {
    const o = { valueLabels: { New: "Mới" } };
    expect(valueLabel(o, "New")).toBe("Mới");
    expect(valueLabel(o, "Lost")).toBe("Lost");
    expect(valueLabel(undefined, "New")).toBe("New");
  });

  it("FieldDisplay picklist: 'New' hiện thành 'Mới'", () => {
    render(<FieldDisplay field={spec2("picklist", { values: ["New"], valueLabels: { New: "Mới" } })} value="New" />);
    expect(screen.getByText("Mới")).toBeInTheDocument();
    expect(screen.queryByText("New")).toBeNull();
  });

  it("FieldDisplay picklist KHÔNG có valueLabels: hiện nguyên mã (pack cũ không vỡ)", () => {
    render(<FieldDisplay field={spec2("picklist", { values: ["New"] })} value="New" />);
    expect(screen.getByText("New")).toBeInTheDocument();
  });

  it("FieldDisplay multi_select: từng chip đi qua tầng nhãn", () => {
    render(<FieldDisplay field={spec2("multi_select", { values: ["Web", "Event"], valueLabels: { Web: "Website" } })} value={["Web", "Event"]} />);
    expect(screen.getByText("Website")).toBeInTheDocument();
    expect(screen.getByText("Event")).toBeInTheDocument(); // mã không nhãn hiện nguyên
  });
});
