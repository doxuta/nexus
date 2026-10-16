// Guard for components/studio/assistant/** (specs/076 FR-098, anti-slop): no Badge/Pill/chat bubbles, no hand-made look
// (colour/radius/shadow come from SLDS), and no "AI" in the assistant's own dictionary values.
import { describe, it, expect } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import vi from "@/lib/i18n/vi";

const ROOT = join(__dirname, "..", "components", "studio", "assistant");

function files(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    if (statSync(p).isDirectory()) return files(p);
    return /\.tsx?$/.test(f) && !/\.test\./.test(f) ? [p] : [];
  });
}

describe("studio assistant gate", () => {
  const sources = files(ROOT).map((p) => ({ p, s: readFileSync(p, "utf8") }));

  it("scans at least one file", () => expect(sources.length).toBeGreaterThan(0));

  it.each([
    ["Badge/Pill import", /import[^;]*\b(Badge|Pill)\b[^;]*from/],
    ["slds-chat", /slds-chat/],
    ["tailwind look utilities", /rounded-|shadow-|bg-\[|text-\[/],
    ["hex colour", /#[0-9a-fA-F]{6}\b/],
  ])("has no %s", (_n, re) => {
    for (const { p, s } of sources) expect(s, p).not.toMatch(re);
  });

  it('has no "AI" in studio.assistant.* values', () => {
    const bad = Object.entries(vi as Record<string, string>)
      .filter(([k, v]) => k.startsWith("studio.assistant.") && /\bAI\b/.test(v))
      // open/close/region predate this slice (#421) and are owned by the dock lát (ai3).
      .filter(([k]) => !["studio.assistant.open", "studio.assistant.region"].includes(k));
    expect(bad).toEqual([]);
  });
});
