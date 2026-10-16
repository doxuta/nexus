import { describe, expect, it } from "vitest";
import { parseCsv } from "./ImportScreen";

// #360: row numbers in the preview must equal the server's (encoding/csv, RFC 4180).
describe("parseCsv", () => {
  it("keeps a line break inside quotes in one cell, one record", () => {
    const rows = parseCsv('Tên,Ghi chú\r\nA,"dòng 1\ndòng 2"\r\nB,"x ""y"", z"\r\n\r\nC,\r\n');
    expect(rows).toEqual([["Tên", "Ghi chú"], ["A", "dòng 1\ndòng 2"], ["B", 'x "y", z'], ["C", ""]]);
  });
  it("splits on semicolons", () => {
    expect(parseCsv("a;b\n1;2", ";")).toEqual([["a", "b"], ["1", "2"]]);
  });
});
