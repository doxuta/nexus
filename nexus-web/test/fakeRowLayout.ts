// happy-dom lays nothing out and its ResizeObserver never reports, so a measured overflow row
// (components/slds/useFit) always "fits". This gives every <li> a width, the tab bar (role=tablist)
// a width, and makes ResizeObserver report once on observe. Undo with vi.restoreAllMocks() +
// vi.unstubAllGlobals().
import { vi } from "vitest";

export function fakeRowLayout(barWidth: number, cellWidth = 100) {
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockImplementation(function (this: HTMLElement) {
    return this.tagName === "LI" ? cellWidth : 0;
  });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (this: HTMLElement) {
    return this.getAttribute("role") === "tablist" ? barWidth : 0;
  });
  vi.stubGlobal(
    "ResizeObserver",
    class {
      cb: ResizeObserverCallback;
      constructor(cb: ResizeObserverCallback) {
        this.cb = cb;
      }
      observe() {
        this.cb([], this as unknown as ResizeObserver);
      }
      unobserve() {}
      disconnect() {}
    },
  );
}
