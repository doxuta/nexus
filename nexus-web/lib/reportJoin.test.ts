// Tests for lib/reportJoin — REPORT-JOIN token + config helpers.
// BE contract (specs/025-report-join/spec.md + api-conventions §19/§19a): joined field = "<rel>.<field>"
// split at the FIRST dot; joins = [{relationship, optional}], summary/matrix only, ONE hop,
// FK-on-child (1-1/1-N) OR N-N — with sum/avg refused over an N-N hop (AGGREGATE_NN_FANOUT_UNSAFE).
import { describe, expect, it } from "vitest";
import {
  buildJoinedField,
  isJoinedField,
  isMetricBlockedByNN,
  joinableRelationships,
  parseJoinedField,
  relatedEntityOf,
  stripStaleJoinToken,
  withReportJoin,
  withReportJoins,
  joinsPayload,
} from "./reportJoin";

const RELS = [
  { name: "account", parentEntityName: "accounts", childEntityName: "contacts", relationshipType: "1-N" },
  { name: "profile", parentEntityName: "contacts", childEntityName: "profiles", relationshipType: "1-1" },
  { name: "tags", parentEntityName: "contacts", childEntityName: "tags", relationshipType: "N-N" },
  { name: "unrelated", parentEntityName: "deals", childEntityName: "quotes", relationshipType: "1-N" },
];

describe("buildJoinedField / parseJoinedField", () => {
  it("round-trips a joined token", () => {
    const tok = buildJoinedField("account", "industry");
    expect(tok).toBe("account.industry");
    expect(parseJoinedField(tok)).toEqual({ relationship: "account", field: "industry" });
  });
  it("splits at the FIRST dot only (backend splitJoinedToken parity)", () => {
    expect(parseJoinedField("a.b.c")).toEqual({ relationship: "a", field: "b.c" });
  });
  it("returns null for a base field", () => {
    expect(parseJoinedField("industry")).toBeNull();
    expect(isJoinedField("industry")).toBe(false);
    expect(isJoinedField("account.industry")).toBe(true);
  });
});

describe("joinableRelationships", () => {
  it("keeps every joinable type (1-1/1-N/N-N) touching the base entity", () => {
    const names = joinableRelationships(RELS, "contacts").map((r) => r.name);
    // N-N is joinable since REPORT-JOIN-NN (BE resolveNNJoinEdge); only "unrelated" is excluded.
    expect(names).toEqual(["account", "profile", "tags"]);
  });
  it("works from the parent side too (fan-out direction)", () => {
    expect(joinableRelationships(RELS, "accounts").map((r) => r.name)).toEqual(["account"]);
  });
  it("returns empty when nothing touches the entity", () => {
    expect(joinableRelationships(RELS, "invoices")).toEqual([]);
  });
  it("still excludes an unknown relationship type (fail closed — BE has no join shape for it)", () => {
    const weird = [{ name: "x", parentEntityName: "contacts", childEntityName: "leads", relationshipType: "unknown" }];
    expect(joinableRelationships(weird, "contacts")).toEqual([]);
  });
});

describe("isMetricBlockedByNN", () => {
  const nn = RELS[2]; // tags: contacts N-N tags
  it("blocks sum and avg over an N-N hop (BE AGGREGATE_NN_FANOUT_UNSAFE, unconditional)", () => {
    expect(isMetricBlockedByNN(nn, "sum")).toBe(true);
    expect(isMetricBlockedByNN(nn, "avg")).toBe(true);
  });
  it("allows count/min/max over an N-N hop (§19a matrix: fan-out breaks additive metrics only)", () => {
    expect(isMetricBlockedByNN(nn, "count")).toBe(false);
    expect(isMetricBlockedByNN(nn, "min")).toBe(false);
    expect(isMetricBlockedByNN(nn, "max")).toBe(false);
  });
  it("allows every metric over an FK-on-child hop or with no join selected", () => {
    for (const m of ["count", "sum", "avg", "min", "max"]) {
      expect(isMetricBlockedByNN(RELS[0], m)).toBe(false); // 1-N
      expect(isMetricBlockedByNN(RELS[1], m)).toBe(false); // 1-1
      expect(isMetricBlockedByNN(null, m)).toBe(false);
      expect(isMetricBlockedByNN(undefined, m)).toBe(false);
    }
  });
});

describe("relatedEntityOf", () => {
  it("resolves the other side from child and parent bases", () => {
    expect(relatedEntityOf(RELS[0], "contacts")).toBe("accounts");
    expect(relatedEntityOf(RELS[0], "accounts")).toBe("contacts");
  });
  it("is null when the base is not an edge", () => {
    expect(relatedEntityOf(RELS[0], "deals")).toBeNull();
  });
  it("handles a self-referential relationship", () => {
    const self = { name: "parent_task", parentEntityName: "tasks", childEntityName: "tasks", relationshipType: "1-N" };
    expect(relatedEntityOf(self, "tasks")).toBe("tasks");
  });
});

describe("stripStaleJoinToken", () => {
  it("keeps base tokens and matching joined tokens", () => {
    expect(stripStaleJoinToken("stage", "account")).toBe("stage");
    expect(stripStaleJoinToken("account.industry", "account")).toBe("account.industry");
  });
  it("clears a token namespaced onto a different (or removed) join", () => {
    expect(stripStaleJoinToken("account.industry", "profile")).toBe("");
    expect(stripStaleJoinToken("account.industry", "")).toBe("");
  });
});

describe("withReportJoin", () => {
  it("writes config.joins for summary/matrix with a selected relationship", () => {
    expect(withReportJoin({ metric: "count" }, "summary", { relationship: "account" })).toEqual({
      metric: "count",
      joins: [{ relationship: "account" }],
    });
    expect(withReportJoin({}, "matrix", { relationship: "account", optional: true })).toEqual({
      joins: [{ relationship: "account", optional: true }],
    });
  });
  it("never writes joins on tabular (backend: REPORT_JOIN_TABULAR_UNSUPPORTED)", () => {
    expect(withReportJoin({ columns: ["a"] }, "tabular", { relationship: "account" })).toEqual({ columns: ["a"] });
  });
  it("is a no-op without a selected relationship", () => {
    expect(withReportJoin({ metric: "count" }, "summary", null)).toEqual({ metric: "count" });
    expect(withReportJoin({ metric: "count" }, "summary", { relationship: "" })).toEqual({ metric: "count" });
  });
});

describe("two chained joins (queue #293)", () => {
  it("stripStaleJoinToken keeps a token on ANY relationship still in the chain", () => {
    expect(stripStaleJoinToken("doctor.name", ["appointment", "doctor"])).toBe("doctor.name");
    expect(stripStaleJoinToken("appointment.date", ["appointment", "doctor"])).toBe("appointment.date");
    expect(stripStaleJoinToken("doctor.name", ["appointment"])).toBe("");
  });
  it("withReportJoins writes the chain in order, dropping blanks, optional only when true", () => {
    expect(withReportJoins({}, "summary", [{ relationship: "appointment" }, { relationship: "doctor", optional: true }])).toEqual({
      joins: [{ relationship: "appointment" }, { relationship: "doctor", optional: true }],
    });
    expect(withReportJoins({ a: 1 }, "summary", [{ relationship: "" }])).toEqual({ a: 1 });
    expect(withReportJoins({}, "tabular", [{ relationship: "a" }, { relationship: "b" }])).toEqual({});
  });
  it("joinsPayload never exceeds the engine's two hops", () => {
    expect(joinsPayload([{ relationship: "a" }, { relationship: "b" }, { relationship: "c" }]).map((j) => j.relationship)).toEqual(["a", "b"]);
  });
});
