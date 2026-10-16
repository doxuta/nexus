import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { FieldDisplay } from "@/components/fields/display";
import type { FieldSpec } from "@/lib/metadata";

const spec = (fieldType: string): FieldSpec =>
  ({ name: "x", label: "X", fieldType, isRequired: false, options: {} } as unknown as FieldSpec);

// MỘT XƯỞNG (2026-07-29): nhánh `formula` từng là String(value) trần trong khi nhánh `rollup` NGAY
// BÊN DƯỚI đã format đúng — nên "Thành tiền" của báo giá in ra `ƒ 3500000`, không dấu phẩy nghìn,
// không đơn vị. Cùng bài toán, hai lời giải, một cái bị bỏ quên. Phát hiện trên BẢN IN của một báo
// giá thật. Ghim cả hai nhánh vào CÙNG một kỳ vọng để chúng không trôi khỏi nhau lần nữa.
describe("field tính-sẵn — formula và rollup format GIỐNG NHAU", () => {
  for (const t of ["formula", "rollup"]) {
    it(`${t}: số nguyên lớn có dấu phân cách nghìn`, () => {
      render(<FieldDisplay field={spec(t)} value={3500000} />);
      expect(screen.getByText("3.500.000")).toBeInTheDocument();
    });

    it(`${t}: chuỗi DECIMAL của MySQL cũng được format`, () => {
      render(<FieldDisplay field={spec(t)} value={"235000.0000"} />);
      expect(screen.getByText("235.000")).toBeInTheDocument();
    });

    it(`${t}: giá trị tiền tệ {amount,currencyCode} ra tiền, không phải [object Object]`, () => {
      render(<FieldDisplay field={spec(t)} value={{ amount: 1500000, currencyCode: "VND" }} />);
      expect(screen.queryByText(/object Object/)).toBeNull();
      expect(screen.getByText(/1\.500\.000/)).toBeInTheDocument();
    });

    it(`${t}: chuỗi không phải số thì giữ NGUYÊN VĂN, không bịa thành NaN/0`, () => {
      render(<FieldDisplay field={spec(t)} value={"chưa tính được"} />);
      expect(screen.getByText("chưa tính được")).toBeInTheDocument();
    });
  }
});

// specs/053 §3.4: formula có resultType "text" hiện nguyên văn, kể cả chuỗi trông như số.
describe("formula resultType text — hiện nguyên văn", () => {
  const textSpec = { ...spec("formula"), options: { resultType: "text" } } as FieldSpec;
  for (const v of ["001", "1.000"]) {
    it(`"${v}" giữ nguyên`, () => {
      render(<FieldDisplay field={textSpec} value={v} />);
      expect(screen.getByText(v)).toBeInTheDocument();
    });
  }
});
