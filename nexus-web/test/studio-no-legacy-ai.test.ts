// specs/076 ai11 T201: the old AI Builder page is deleted, not redirected. One assistant: the docked panel.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = process.cwd();
const grep = (args: string[]): string[] => {
  try {
    return execFileSync("grep", ["-rn", ...args, "--include=*.ts", "--include=*.tsx", "--include=*.mjs",
      "--exclude-dir=node_modules", "--exclude-dir=.next", "--exclude=studio-no-legacy-ai.test.ts", "."], { cwd: ROOT, encoding: "utf8" })
      .split("\n").filter(Boolean);
  } catch {
    return []; // grep exits 1 on no match
  }
};

describe("old AI Builder page is gone", () => {
  it.each(["app/studio/ai", "components/studio/AiBuilder.tsx", "components/studio/AiBuilder.test.tsx",
    "components/studio/AiBuilder.context.test.tsx", "components/studio/AiBuilderRoute.tsx"])("%s no longer exists", (p) => {
    expect(existsSync(join(ROOT, p))).toBe(false);
  });
  it("no source names AiBuilder / AiBuilderRoute", () => expect(grep(["-wE", "AiBuilder|AiBuilderRoute"])).toEqual([]));
  it("no source routes to /studio/ai", () => expect(grep(["/studio/ai"])).toEqual([]));
  // Keys still read by the panel / apply module / persisted footers stay (renaming a `foot` key would orphan saved cards).
  it("no page.aiBuilder.* / page.studio.ai.* key in the dictionary is unused", () => {
    const keys = [...readFileSync(join(ROOT, "lib/i18n/vi.ts"), "utf8").matchAll(/"(page\.(?:aiBuilder|studio\.ai)\.[^"]+)"\s*:/g)].map((m) => m[1]);
    const unused = keys.filter((k) => grep(["-F", `"${k}"`]).filter((l) => !l.includes("lib/i18n/")).length === 0);
    expect(unused).toEqual([]);
  });
  it("⌘K offers 'Mở trợ lý' that fires nexus:assistant-open", () => {
    const src = readFileSync(join(ROOT, "components/ui/CommandPalette.tsx"), "utf8");
    expect(src).toContain("Mở trợ lý");
    expect(src).toContain("ASSISTANT_OPEN_EVENT");
  });
  it("the header toggle docks the panel instead of navigating", () => {
    const src = readFileSync(join(ROOT, "components/studio/StudioShell.tsx"), "utf8");
    expect(src).toContain("toggleAssistant");
  });
});
