import { describe, expect, it, vi, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { useSurfaceWidth } from "./useSurfaceWidth";

let cb: ((e: { contentRect: { width: number } }[]) => void) | null = null;
class FakeRO {
  constructor(fn: typeof cb) { cb = fn; }
  observe() {}
  disconnect() {}
}
function Probe() {
  const { ref, stacked } = useSurfaceWidth();
  return <div ref={ref} data-testid="s">{stacked ? "stacked" : "wide"}</div>;
}

afterEach(() => { vi.unstubAllGlobals(); cb = null; });

describe("useSurfaceWidth", () => {
  it("stacks when the SURFACE is under 1000px even though the window is wider", () => {
    vi.stubGlobal("ResizeObserver", FakeRO);
    render(<Probe />);
    expect(screen.getByTestId("s")).toHaveTextContent("wide");
    act(() => cb?.([{ contentRect: { width: 890 } }]));
    expect(screen.getByTestId("s")).toHaveTextContent("stacked");
    act(() => cb?.([{ contentRect: { width: 1100 } }]));
    expect(screen.getByTestId("s")).toHaveTextContent("wide");
  });
});
