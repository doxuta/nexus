// specs/076 ai3 T051: the assistant open/closed store shared by the header button and the panel.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { isAssistantOpen, setAssistantOpen, toggleAssistant, resetAssistantOpenForTests, subscribeAssistantOpen } from "./assistantOpen";

function wide(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({
    matches: on, media: q, addEventListener: () => {}, removeEventListener: () => {},
  })) as unknown as typeof window.matchMedia;
}

beforeEach(() => { window.localStorage.clear(); resetAssistantOpenForTests(); wide(true); });

describe("assistantOpen", () => {
  it("docked mode is open by default from 1280px and remembers the choice", () => {
    expect(isAssistantOpen()).toBe(true);
    setAssistantOpen(false);
    expect(window.localStorage.getItem("nexus.studio.assistantOpen")).toBe("0");
    expect(isAssistantOpen()).toBe(false);
    toggleAssistant();
    expect(isAssistantOpen()).toBe(true);
  });
  it("overlay mode starts closed and ignores the stored docked value", () => {
    window.localStorage.setItem("nexus.studio.assistantOpen", "1");
    wide(false);
    expect(isAssistantOpen()).toBe(false);
    setAssistantOpen(true);
    expect(isAssistantOpen()).toBe(true);
    expect(window.localStorage.getItem("nexus.studio.assistantOpen")).toBe("1"); // overlay never writes the docked value
  });
  it("crossing the breakpoint follows the new mode's own state", () => {
    setAssistantOpen(false); // docked closed
    wide(false);
    expect(isAssistantOpen()).toBe(false);
    setAssistantOpen(true); // overlay open
    wide(true);
    expect(isAssistantOpen()).toBe(false); // docked is still the stored "closed"
  });
  it("storage that throws is not fatal", () => {
    const spy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("blocked"); });
    expect(isAssistantOpen()).toBe(true);
    spy.mockRestore();
  });
  it("the nexus:assistant-open command opens it", () => {
    setAssistantOpen(false);
    const off = subscribeAssistantOpen(() => {});
    window.dispatchEvent(new Event("nexus:assistant-open"));
    expect(isAssistantOpen()).toBe(true);
    off();
  });
});
