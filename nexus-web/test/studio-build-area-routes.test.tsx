// specs/074 lát f2-cleanup-scope-hub (T417): every mounted Studio route names a component that ProjectPage can really
// render, and ProjectPage reaches for no component the cleanup deleted.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { ROUTES } from "@/lib/studioRoutes";
import { MOUNTED } from "@/components/studio/ProjectPage";

describe("Studio routes", () => {
  const mounted = ROUTES.filter((r) => r.mounted);

  it("there are routes to check", () => {
    expect(mounted.length).toBeGreaterThanOrEqual(20);
  });

  it.each(mounted.map((r) => [r.id, r.mounts] as const))("%s mounts %s", (_id, mounts) => {
    expect(mounts).toBeTruthy();
    expect(typeof MOUNTED[mounts as string]).toBe("function");
  });

  it("ProjectPage imports no deleted component", () => {
    const src = readFileSync(join(__dirname, "../components/studio/ProjectPage.tsx"), "utf8");
    expect(src.replace(/\/\/.*$/gm, "")).not.toMatch(/EntityHub|DataModelBuilder|StudioScopeOverride|ScopeChip/);
  });
});
