import { describe, expect, it } from "vitest";
import { buildLaneQuery } from "./kanbanLane";

describe("buildLaneQuery", () => {
  const base = { laneField: "status", limit: 50, expand: [] as string[] };

  it("lane only: eq on the lane value", () => {
    const q = new URLSearchParams(buildLaneQuery({ ...base, col: { key: "new", isEmpty: false } })!);
    expect(q.get("status[eq]")).toBe("new");
    expect(q.get("limit")).toBe("50");
  });

  it("empty lane: isnull", () => {
    const q = new URLSearchParams(buildLaneQuery({ ...base, col: { key: "", isEmpty: true } })!);
    expect(q.get("status[isnull]")).toBe("1");
  });

  it("carries the list filter and search into every lane query", () => {
    const q = new URLSearchParams(
      buildLaneQuery({
        ...base,
        col: { key: "new", isEmpty: false },
        filterQuery: "type[eq]=apartment&price[gte]=100",
        q: "can ho",
      })!,
    );
    expect(q.get("type[eq]")).toBe("apartment");
    expect(q.get("price[gte]")).toBe("100");
    expect(q.get("q")).toBe("can ho");
    expect(q.get("status[eq]")).toBe("new");
  });

  it("filter pinning the lane field to ANOTHER value empties the lane (no query)", () => {
    expect(
      buildLaneQuery({ ...base, col: { key: "won", isEmpty: false }, filterQuery: "status[eq]=new" }),
    ).toBeNull();
    expect(buildLaneQuery({ ...base, col: { key: "", isEmpty: true }, filterQuery: "status[eq]=new" })).toBeNull();
    expect(
      buildLaneQuery({ ...base, col: { key: "new", isEmpty: false }, filterQuery: "status[isnull]=1" }),
    ).toBeNull();
  });

  it("filter on the lane field equal to the lane value keeps the lane", () => {
    const q = new URLSearchParams(
      buildLaneQuery({ ...base, col: { key: "new", isEmpty: false }, filterQuery: "status[eq]=new" })!,
    );
    expect(q.get("status[eq]")).toBe("new");
  });
});
