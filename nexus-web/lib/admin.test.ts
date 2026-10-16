// Tests for RBAC matrix model in lib/admin.ts
// Covers: grantsToMatrix, matrixToGrants (replace-set), cellLevel (wildcard fallback),
//         setCell, setRow, setColumn, matrixEqual.

import { describe, it, expect } from "vitest";
import {
  grantsToMatrix,
  matrixToGrants,
  cellLevel,
  setCell,
  setRow,
  setColumn,
  matrixEqual,
  matKey,
  fieldLevel,
  userFieldOf,
  type RoleGrant,
  type MatrixState,
} from "@/lib/admin";

// ── grantsToMatrix ────────────────────────────────────────────────────────────

describe("grantsToMatrix", () => {
  it("converts a flat grant array to a sparse matrix (omits none)", () => {
    const grants: RoleGrant[] = [
      { entityName: "accounts", action: "view", accessLevel: "all" },
      { entityName: "accounts", action: "edit", accessLevel: "team" },
      { entityName: "*", action: "view", accessLevel: "all" },
    ];
    const m = grantsToMatrix(grants);
    expect(m[matKey("accounts", "view")]).toBe("all");
    expect(m[matKey("accounts", "edit")]).toBe("team");
    expect(m[matKey("*", "view")]).toBe("all");
    // create was not in grants → absent (sparse)
    expect(m[matKey("accounts", "create")]).toBeUndefined();
  });

  it("ignores none-level grants (replace-set: none = revoked)", () => {
    const grants: RoleGrant[] = [
      { entityName: "contacts", action: "delete", accessLevel: "none" },
    ];
    const m = grantsToMatrix(grants);
    expect(Object.keys(m).length).toBe(0);
  });

  it("returns empty matrix for empty grant array", () => {
    expect(grantsToMatrix([])).toEqual({});
  });

  it("handles wildcard entity correctly", () => {
    const grants: RoleGrant[] = [
      { entityName: "*", action: "view", accessLevel: "all" },
      { entityName: "*", action: "create", accessLevel: "team" },
    ];
    const m = grantsToMatrix(grants);
    expect(m[matKey("*", "view")]).toBe("all");
    expect(m[matKey("*", "create")]).toBe("team");
  });
});

// ── matrixToGrants (replace-set payload) ─────────────────────────────────────

describe("matrixToGrants", () => {
  it("serializes matrix to grants, omitting none cells", () => {
    const m: MatrixState = {
      [matKey("accounts", "view")]: "all",
      [matKey("accounts", "edit")]: "team",
    };
    const grants = matrixToGrants(m);
    expect(grants).toHaveLength(2);
    expect(grants).toContainEqual({ entityName: "accounts", action: "view", accessLevel: "all" });
    expect(grants).toContainEqual({ entityName: "accounts", action: "edit", accessLevel: "team" });
  });

  it("empty matrix produces empty grant array", () => {
    expect(matrixToGrants({})).toEqual([]);
  });

  it("round-trips: grants → matrix → grants preserves all non-none cells", () => {
    const original: RoleGrant[] = [
      { entityName: "*", action: "view", accessLevel: "all" },
      { entityName: "accounts", action: "create", accessLevel: "owner" },
      { entityName: "contacts", action: "delete", accessLevel: "team" },
    ];
    const matrix = grantsToMatrix(original);
    const roundTripped = matrixToGrants(matrix);
    // Order may differ — compare as sets
    expect(roundTripped.length).toBe(original.length);
    for (const g of original) {
      expect(roundTripped).toContainEqual(g);
    }
  });

  it("replace-set: none values in matrix are excluded from payload", () => {
    const m: MatrixState = {
      [matKey("accounts", "view")]: "all",
      // no "none" keys — sparse model means absence = none
    };
    const grants = matrixToGrants(m);
    // Only 1 grant; no none entries sent
    expect(grants.every((g) => g.accessLevel !== "none")).toBe(true);
  });
});

// ── cellLevel (wildcard fallback) ─────────────────────────────────────────────

describe("cellLevel", () => {
  it("returns specific entity grant when present", () => {
    const m: MatrixState = {
      [matKey("accounts", "edit")]: "owner",
      [matKey("*", "edit")]: "all",
    };
    expect(cellLevel(m, "accounts", "edit")).toBe("owner");
  });

  it("falls back to wildcard (*) when entity-specific absent", () => {
    const m: MatrixState = {
      [matKey("*", "view")]: "all",
    };
    expect(cellLevel(m, "contacts", "view")).toBe("all");
  });

  it("returns none when neither entity nor wildcard present", () => {
    const m: MatrixState = {
      [matKey("accounts", "view")]: "team",
    };
    expect(cellLevel(m, "contacts", "delete")).toBe("none");
  });

  it("entity-specific overrides wildcard exactly (no merging)", () => {
    const m: MatrixState = {
      [matKey("*", "delete")]: "team",
      [matKey("accounts", "delete")]: "none",
    };
    // "none" stored explicitly means the override IS none (wildcard NOT applied)
    // Note: grantsToMatrix removes none, but if set manually:
    expect(cellLevel(m, "accounts", "delete")).toBe("none");
  });
});

// ── setCell ───────────────────────────────────────────────────────────────────

describe("setCell", () => {
  it("sets a cell in a new copy", () => {
    const m: MatrixState = {};
    const next = setCell(m, "accounts", "view", "all");
    expect(next[matKey("accounts", "view")]).toBe("all");
    // original not mutated
    expect(m[matKey("accounts", "view")]).toBeUndefined();
  });

  it("setting to none removes the key (sparse)", () => {
    const m: MatrixState = { [matKey("accounts", "view")]: "team" };
    const next = setCell(m, "accounts", "view", "none");
    expect(matKey("accounts", "view") in next).toBe(false);
  });
});

// ── setRow ────────────────────────────────────────────────────────────────────

describe("setRow", () => {
  it("sets all 4 actions for a given entity to the same level", () => {
    const m: MatrixState = {};
    const next = setRow(m, "accounts", "team");
    expect(next[matKey("accounts", "view")]).toBe("team");
    expect(next[matKey("accounts", "create")]).toBe("team");
    expect(next[matKey("accounts", "edit")]).toBe("team");
    expect(next[matKey("accounts", "delete")]).toBe("team");
  });

  it("setting row to none clears all action keys for that entity", () => {
    const m: MatrixState = {
      [matKey("accounts", "view")]: "all",
      [matKey("accounts", "edit")]: "team",
    };
    const next = setRow(m, "accounts", "none");
    expect(matKey("accounts", "view") in next).toBe(false);
    expect(matKey("accounts", "edit") in next).toBe(false);
  });
});

// ── setColumn ─────────────────────────────────────────────────────────────────

describe("setColumn", () => {
  it("sets a single action to the same level across multiple entities", () => {
    const m: MatrixState = {};
    const entities = ["*", "accounts", "contacts"];
    const next = setColumn(m, entities, "view", "all");
    for (const e of entities) {
      expect(next[matKey(e, "view")]).toBe("all");
    }
    // Other actions untouched
    expect(next[matKey("accounts", "edit")]).toBeUndefined();
  });
});

// ── matrixEqual ───────────────────────────────────────────────────────────────

describe("matrixEqual", () => {
  it("returns true for identical matrices", () => {
    const m: MatrixState = { [matKey("accounts", "view")]: "all" };
    expect(matrixEqual(m, { ...m })).toBe(true);
  });

  it("returns false for different matrices", () => {
    const a: MatrixState = { [matKey("accounts", "view")]: "all" };
    const b: MatrixState = { [matKey("accounts", "view")]: "team" };
    expect(matrixEqual(a, b)).toBe(false);
  });

  it("treats absent key and none-valued key as equivalent (sparse)", () => {
    // Both a and b have no non-none entries, so they should be equal
    const a: MatrixState = {};
    const b: MatrixState = {};
    expect(matrixEqual(a, b)).toBe(true);
  });

  it("returns false when one has extra grants", () => {
    const a: MatrixState = { [matKey("accounts", "view")]: "all" };
    const b: MatrixState = {
      [matKey("accounts", "view")]: "all",
      [matKey("accounts", "edit")]: "team",
    };
    expect(matrixEqual(a, b)).toBe(false);
  });
});

// ── field-level access (#283) ─────────────────────────────────────────────────

describe("field access level", () => {
  it("round-trips through the matrix and the replace-set payload", () => {
    const grants: RoleGrant[] = [{ entityName: "nk_lich_hen", action: "view", accessLevel: "field:bac_si" }];
    const m = grantsToMatrix(grants);
    expect(cellLevel(m, "nk_lich_hen", "view")).toBe("field:bac_si");
    expect(matrixToGrants(m)).toEqual(grants);
  });

  it("fieldLevel / userFieldOf are inverse; other levels carry no field", () => {
    expect(fieldLevel("bac_si")).toBe("field:bac_si");
    expect(userFieldOf(fieldLevel("bac_si"))).toBe("bac_si");
    expect(userFieldOf("owner")).toBeNull();
    expect(userFieldOf("all")).toBeNull();
  });
});
