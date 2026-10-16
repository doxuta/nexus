import { describe, it, expect } from "vitest";
import { formatBytes } from "@/lib/format";

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [1023, "1023 B"],
    [1536, "1,5 KB"],
    [3 * 1024 * 1024, "3,0 MB"],
    [1.25 * 1024 ** 3, "1,25 GB"],
  ])("%d -> %s", (n, want) => {
    expect(formatBytes(n)).toBe(want);
  });
});
