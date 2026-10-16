import { describe, it, expect, vi } from "vitest";
import type { QueryClient } from "@tanstack/react-query";
import { invalidateAfterApply, shouldRefreshAfterApply } from "./aiInvalidate";
import { qk } from "./query/keys";

describe("aiInvalidate", () => {
  const refreshed: (readonly unknown[])[] = [
    qk.entities(), qk.fields("x"), qk.layout("x"), qk.records("x"), qk.apps(), qk.adminApps(), qk.reports(), qk.dashboards(),
    ["studio", "count", "entities"], ["studio", "sampleData", "northwind", "a,b"],
    ["project", "northwind", "studio", "sampleData", "a"], ["publish", "state"], ["publish", "changes"], ["project", "current"],
    ["admin-work-queues"], ["a-key-nobody-listed-yet"],
  ];
  it.each(refreshed.map((k) => [k]))("refreshes %j", (key) => expect(shouldRefreshAfterApply(key)).toBe(true));

  const kept: (readonly unknown[])[] = [qk.me(), qk.permissions(), qk.notifications(), qk.notificationCount(), ["ai", "sessions"]];
  it.each(kept.map((k) => [k]))("leaves %j alone", (key) => expect(shouldRefreshAfterApply(key)).toBe(false));

  it("invalidateAfterApply hands the predicate to the query client", () => {
    const invalidateQueries = vi.fn();
    invalidateAfterApply({ invalidateQueries } as unknown as QueryClient);
    const { predicate } = invalidateQueries.mock.calls[0][0];
    expect(predicate({ queryKey: ["me"] })).toBe(false);
    expect(predicate({ queryKey: ["apps"] })).toBe(true);
  });
});
