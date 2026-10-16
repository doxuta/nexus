// specs/073 kh1a T146/T175 (architecture 7.4): the Studio string gate.
//  (a) no Vietnamese typed straight into Studio source (it goes through t()/tf());
//  (b) no engine word on screen: the S13 forbidden list, now with widget, index, chỉ mục, bố cục;
//  (c) no internal code on screen (spec numbers, queue numbers, slice letters, "sổ dự án", "tầng nháp").
// (a) is a ratchet from the tree as it was (test/studio-gates.allowlist.json); (b)/(c) apply to every `studio.*` value,
// with a ratchet by key for values that already broke the rule. Component namespaces that embedded builders reuse
// (page.fieldEditor.* …) join (b) when the slice that touches them rewrites their values.
import { describe, expect, it } from "vitest";
import vi from "@/lib/i18n/vi";
import { countPerFile, ratchet } from "../../test/studioGateScan";

// Vietnamese-only letters. Any of these outside a comment in Studio source is text that skipped the dictionary
// (dictionary keys are ASCII), so the gate counts the lines that carry one.
const VI_LINE = /^.*[àáạảãâầấậẩẫăằắặẳẵèéẹẻẽêềếệểễìíịỉĩòóọỏõôồốộổỗơờớợởỡùúụủũưừứựửữỳýỵỷỹđ].*$/gimu;

const FORBIDDEN_WORDS = [
  "tenant", "entity", "layout", "pack", "vertical", "workflow", "metadata", "webhook", "dashboard", "workspace",
  "dry-run", "record", "field", "role", "ACL", "grant", "baseline", "rollback", "draft", "publish", "slug", "token",
  "session", "builder", "schema", "endpoint", "payload", "doctor", "audit", "backup", "restore", "preview", "API",
  "SSO", "MFA", "widget", "index", "chỉ mục", "bố cục",
];
const FORBIDDEN_PHRASES = ["chạy khô", "người vận hành", "không gian khách"];
const LETTER = "[\\p{L}\\p{N}_]";
const WORD_RE = new RegExp(`(?<!${LETTER})(?:${[...FORBIDDEN_WORDS, ...FORBIDDEN_PHRASES].map((w) => w.replace(/[\\^$*+?.()|[\]{}]/g, "\\$&")).join("|")})(?!${LETTER})`, "iu");
const INTERNAL_CODE_RE = /specs\/\d|#\d{3}|lát [A-Z]\d|sổ dự án|tầng nháp/i;
const PLACEHOLDER = /\{\w+\}/g;

/** Rule broken by a dictionary value, or null. `{slug}`-style placeholders are not text. */
export function badValue(value: string): "forbidden-word" | "internal-code" | null {
  const text = value.replace(PLACEHOLDER, " ");
  if (WORD_RE.test(text)) return "forbidden-word";
  if (INTERNAL_CODE_RE.test(text)) return "internal-code";
  return null;
}

describe("Studio strings", () => {
  it("(a) no Vietnamese typed into Studio source — beyond the allow-list", () => {
    expect(ratchet("strings", countPerFile(VI_LINE))).toEqual([]);
  });

  it("(b)(c) no forbidden word or internal code in any studio.* value — beyond the allow-list", () => {
    const counts = Object.fromEntries(
      Object.entries(vi)
        .filter(([k]) => k.startsWith("studio."))
        .flatMap(([k, v]) => (badValue(v as string) ? [[k, 1] as const] : [])),
    );
    expect(ratchet("studioValues", counts)).toEqual([]);
  });

  it("the studio.* namespace is not empty (the gate is looking at something)", () => {
    expect(Object.keys(vi).filter((k) => k.startsWith("studio.")).length).toBeGreaterThan(100);
  });

  // The gate must be able to fail.
  it("self-check: the four new words, the older ones and the internal codes are caught; plain words are not", () => {
    for (const bad of ["Tạo widget mới", "Chỉ mục của bảng", "Sửa bố cục", "Bảng index", "Chỉ Mục", "Mở workspace", "Một Tenant", "Chạy khô thử", "Người vận hành Nexus", "ACL của vai", "Gọi API"]) {
      expect(badValue(bad), bad).toBe("forbidden-word");
    }
    for (const bad of ["Xem specs/073", "Lát #492", "lát F1", "Từ sổ dự án", "Qua tầng nháp"]) {
      expect(badValue(bad), bad).toBe("internal-code");
    }
    for (const ok of ["Bảng và trường của dự án.", "Xuất bản phiên bản đầu tiên", "{slug} đã sẵn sàng", "Giao diện chính", "Khuôn mẫu", "Chỉ xem", "Index"].slice(0, 6)) {
      expect(badValue(ok), ok).toBeNull();
    }
    // whole words only: "indexed" / "records" style substrings of longer words are not hits
    expect(badValue("Thư mục reindexing")).toBeNull();
    expect(badValue("Mã #12 trong danh sách")).toBeNull();
  });

  it("self-check: the Vietnamese-line pattern sees typed text and ignores keys and ASCII", () => {
    expect("const a = 'Lưu';".match(VI_LINE)).toHaveLength(1);
    expect(`<p>{t("studio.gate.retry")}</p>`.match(VI_LINE)).toBeNull();
    expect("const n = 3; // Lưu".replace(/\/\/.*$/gm, "").match(VI_LINE)).toBeNull();
  });
});
