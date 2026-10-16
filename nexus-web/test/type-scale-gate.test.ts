// Type-scale gate (E2 §4.1 · nợ Phase-5 specs/010-app-container/spec.md §7).
// Codemod 2026-09-01 đưa 2.320 `text-[Npx]` về token @theme (6 semantic + 21 interim px,
// mapping 1:1 px — không đổi render). Gate này giữ nợ không tái phình:
//   1. Cấm arbitrary font-size mới (`text-[13px]`, `text-[0.8rem]`, `text-[length:…]`)
//      trong toàn bộ source — cỡ chữ mới phải dùng token semantic trong globals.css.
//   2. Pin đúng tập token --text-* trong globals.css: thêm/bớt token (nhất là interim px)
//      là quyết định design-system, phải sửa test này cùng commit một cách có ý thức.
import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

const ROOT = process.cwd(); // vitest chạy từ nexus-web/
const SCAN_DIRS = ["app", "components", "lib", "test"];
const SCAN_EXT = /\.(ts|tsx|css)$/;
const SELF = "test/type-scale-gate.test.ts";

// Arbitrary FONT-SIZE: text-[<số>…] hoặc text-[length:…]. KHÔNG bắt màu (text-[#fff],
// text-[var(--x)]) — màu arbitrary không thuộc gate này.
const ARBITRARY_SIZE = /text-\[(?:length:|[0-9])/;

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) {
      if (name === "node_modules" || name === ".next") continue;
      walk(p, out);
    } else if (SCAN_EXT.test(name)) {
      out.push(p);
    }
  }
  return out;
}

describe("type-scale gate", () => {
  it("không có arbitrary font-size text-[N…] mới trong source", () => {
    const violations: string[] = [];
    for (const dir of SCAN_DIRS) {
      for (const file of walk(join(ROOT, dir))) {
        const rel = relative(ROOT, file);
        if (rel === SELF) continue;
        const lines = readFileSync(file, "utf8").split("\n");
        lines.forEach((line, i) => {
          if (ARBITRARY_SIZE.test(line)) violations.push(`${rel}:${i + 1}  ${line.trim().slice(0, 120)}`);
        });
      }
    }
    expect(
      violations,
      `Arbitrary font-size bị cấm (E2 §4.1) — dùng token type-scale trong app/globals.css ` +
        `(text-display/h1/title/body/sub/caption; cỡ lệch scale cần lý do = quyết định design-system, ` +
        `xem comment @theme):\n${violations.join("\n")}`,
    ).toEqual([]);
  });

  it("tập token --text-* trong globals.css đúng như đã pin (semantic + interim)", () => {
    const css = readFileSync(join(ROOT, "app", "globals.css"), "utf8");
    const tokens = [...css.matchAll(/--text-([a-z0-9-]+)\s*:/g)]
      .map((m) => m[1])
      .filter((t) => !t.endsWith("-line-height")) // companion (nếu có ai thêm) không phải cỡ
      .sort();
    const semantic = ["display", "h1", "title", "body", "sub", "caption"];
    // Interim px (codemod 2026-09-01) — CHỈ ĐƯỢC BỚT khi hội tụ về semantic, không được thêm.
    const interim = [
      "9px", "9-5px", "10px", "10-5px", "11-5px", "12-5px", "13-5px", "14px", "14-5px",
      "16px", "17px", "18px", "19px", "20px", "21px", "24px", "26px", "28px", "32px", "34px", "48px",
    ];
    expect(tokens).toEqual([...semantic, ...interim].sort());
  });
});
