import { describe, expect, it } from "vitest";
import { isPanelKey, normalizePanel, panelLabel } from "./workshopPanels";

describe("workshopPanels", () => {
  it("known panel resolves its Vietnamese label", () => {
    expect(panelLabel("tong-quan")).toBe("Tổng quan");
    expect(panelLabel("bang")).toBe("Dữ liệu");
    expect(isPanelKey("bang")).toBe(true);
  });

  it("unknown panel echoes its own token — never a silent fallback to another label", () => {
    expect(panelLabel("xyz")).toBe("xyz");
    expect(isPanelKey("xyz")).toBe(false);
  });

  it("long unknown token is truncated before it ever hits a crumb/title", () => {
    const long = "a".repeat(80);
    expect(panelLabel(long).length).toBeLessThanOrEqual(41); // 40 chars + ellipsis
  });

  it("absent or blank ?panel= normalizes to the default tong-quan", () => {
    expect(normalizePanel(null)).toBe("tong-quan");
    expect(normalizePanel("")).toBe("tong-quan");
    expect(normalizePanel("   ")).toBe("tong-quan");
  });

  it("a real value passes through untouched", () => {
    expect(normalizePanel("menu")).toBe("menu");
  });
});
