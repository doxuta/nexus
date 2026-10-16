// specs/076 f4-proposal-overlay-hook T141/T143: the preview contract (one change at a time, hook lookups, empty PAINTED).
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

const push = vi.fn();
let pathname = "/studio/du-an/demo/dung/du-lieu";
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }), usePathname: () => pathname }));

import { PAINTED, ProposalOverlayProvider, useProposalOverlay, useProposedChange, useProposedChanges, type ProposedChange } from "./ProposalOverlay";

const mk = (key: string, over: Partial<ProposedChange> = {}): ProposedChange => ({
  proposalKey: key, docType: "field", op: "create", label: "x",
  target: { docType: "field", entity: "lich_hen", field: key }, ...over,
});
const painted = [{ docType: "field", op: "create" as const }];
function wrap({ children }: { children: ReactNode }) {
  return <ProposalOverlayProvider slug="demo" painted={painted}>{children}</ProposalOverlayProvider>;
}

beforeEach(() => { push.mockClear(); pathname = "/studio/du-an/demo/dung/du-lieu"; });

describe("ProposalOverlay", () => {
  it("PAINTED lists only what editors draw: a kind nobody paints (layout:create) gets no preview", () => {
    expect(PAINTED.map((p) => `${p.docType}:${p.op}`).sort()).toEqual(
      ["access:update", "app:update", "branding:update", "entity:create", "entity:delete", "field:create", "field:update", "layout:update",
        "report:create", "report:delete", "report:update", "workflow:create", "workflow:delete", "workflow:update"],
    );
    const { result } = renderHook(() => useProposalOverlay(), { wrapper: ({ children }) => <ProposalOverlayProvider slug="demo">{children}</ProposalOverlayProvider> });
    expect(result.current?.canPreview(mk("a", { docType: "layout", op: "create", target: { docType: "layout", entity: "lich_hen" } }))).toBe(false);
    expect(result.current?.canPreview(mk("a"))).toBe(true);
  });

  it("canPreview needs a PAINTED row for (docType, op) and a valid target", () => {
    const { result } = renderHook(() => useProposalOverlay(), { wrapper: wrap });
    expect(result.current?.canPreview(mk("a"))).toBe(true);
    expect(result.current?.canPreview(mk("a", { op: "update" }))).toBe(false);
    expect(result.current?.canPreview(mk("a", { target: { docType: "field", entity: "../x" } }))).toBe(false);
  });

  it("keeps at most ONE change: turning another on turns the first off", () => {
    const { result } = renderHook(() => ({ o: useProposalOverlay(), a: useProposedChange({ docType: "field", field: "a" }), b: useProposedChange({ docType: "field", field: "b" }) }), { wrapper: wrap });
    act(() => result.current.o!.toggle(mk("a")));
    expect(result.current.a?.proposalKey).toBe("a");
    expect(result.current.b).toBeNull();
    act(() => result.current.o!.toggle(mk("b")));
    expect(result.current.a).toBeNull();
    expect(result.current.b?.proposalKey).toBe("b");
    act(() => result.current.o!.toggle(mk("b")));
    expect(result.current.b).toBeNull();
  });

  it("useProposedChanges lists matches by docType and filter", () => {
    const { result } = renderHook(() => ({ o: useProposalOverlay(), same: useProposedChanges("field", { entity: "lich_hen" }), other: useProposedChanges("field", { entity: "khac" }), wrong: useProposedChanges("layout") }), { wrapper: wrap });
    act(() => result.current.o!.toggle(mk("a")));
    expect(result.current.same).toHaveLength(1);
    expect(result.current.other).toHaveLength(0);
    expect(result.current.wrong).toHaveLength(0);
  });

  it("navigates with scroll:false when the page differs, and not when it already draws the kind", () => {
    const { result } = renderHook(() => useProposalOverlay(), { wrapper: wrap });
    act(() => result.current!.toggle(mk("a")));
    expect(push).toHaveBeenCalledWith("/studio/du-an/demo/dung/du-lieu/lich_hen?truong=a", { scroll: false });
    expect(result.current!.active?.opened).toBe("Dựng › Dữ liệu");
    push.mockClear();
    pathname = "/studio/du-an/demo/dung/man-hinh/lich_hen";
    const r2 = renderHook(() => useProposalOverlay(), { wrapper: wrap });
    act(() => r2.result.current!.toggle(mk("b")));
    expect(push).not.toHaveBeenCalled();
    expect(r2.result.current!.active?.opened).toBeUndefined();
  });

  it("a new project (new provider key) starts empty", () => {
    const { result, unmount } = renderHook(() => useProposalOverlay(), { wrapper: wrap });
    act(() => result.current!.toggle(mk("a")));
    unmount();
    const again = renderHook(() => useProposalOverlay(), { wrapper: wrap });
    expect(again.result.current!.active).toBeNull();
  });

  it("outside a provider the hooks answer null", () => {
    const { result } = renderHook(() => ({ o: useProposalOverlay(), c: useProposedChange({ docType: "field" }) }));
    expect(result.current.o).toBeNull();
    expect(result.current.c).toBeNull();
    render(<div />);
  });
});

// T143: a PAINTED row without a painter would promise a preview nobody draws.
describe("PAINTED rows have painters", () => {
  const files = (dir: string): string[] =>
    fs.readdirSync(dir, { withFileTypes: true }).flatMap((d) => d.isDirectory() ? files(path.join(dir, d.name)) : /\.tsx?$/.test(d.name) && !/\.test\./.test(d.name) ? [path.join(dir, d.name)] : []);
  it("every row has a file under components/studio calling useProposedChange for its docType", () => {
    const src = files(path.join(process.cwd(), "components/studio")).filter((f) => !f.endsWith("ProposalOverlay.tsx")).map((f) => fs.readFileSync(f, "utf8"));
    for (const row of PAINTED) {
      expect(src.some((s) => /useProposedChanges?\(/.test(s) && s.includes(`"${row.docType}"`)), row.docType).toBe(true);
    }
  });
});
