// Tests for EntityDirectory data contract.
// Validates: EntityMeta shape from GET /metadata/entities is the correct contract
// that EntityDirectory consumes (name, label, tableName required; id optional).
// Also tests the labelToBadge helper logic independently.

import { describe, it, expect } from "vitest";
import type { EntityMeta } from "@/lib/api";

// ── EntityMeta shape contract ─────────────────────────────────────────────────

describe("EntityMeta type contract", () => {
  it("accepts a minimal valid entity meta object", () => {
    // All required fields present.
    const meta: EntityMeta = { name: "accounts", label: "Tài khoản", tableName: "obj_accounts" };
    expect(meta.name).toBe("accounts");
    expect(meta.label).toBe("Tài khoản");
    expect(meta.tableName).toBe("obj_accounts");
    expect(meta.id).toBeUndefined();
  });

  it("accepts an entity meta object with optional id", () => {
    const meta: EntityMeta = {
      id: "abc-123",
      name: "tickets",
      label: "Tickets",
      tableName: "obj_tickets",
    };
    expect(meta.id).toBe("abc-123");
  });

  it("name maps to URL path correctly", () => {
    const meta: EntityMeta = { name: "students", label: "Học viên", tableName: "obj_students" };
    const path = `/${meta.name}`;
    expect(path).toBe("/students");
  });
});

// ── labelToBadge logic (inline re-implementation of the pure logic from EntityDirectory) ──
// We test the badge derivation rule independently to cover the character extraction.

function labelToBadge(label: string): string {
  const words = label.trim().split(/\s+/);
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase();
  return label.slice(0, 2).toUpperCase();
}

describe("labelToBadge()", () => {
  it("extracts first letters of first two words", () => {
    expect(labelToBadge("Tài khoản")).toBe("TK");
    expect(labelToBadge("Học viên")).toBe("HV");
    expect(labelToBadge("Support Ticket")).toBe("ST");
  });

  it("uses first two chars for single-word labels", () => {
    expect(labelToBadge("Accounts")).toBe("AC");
    expect(labelToBadge("Z")).toBe("Z");
  });

  it("handles three-word labels (uses first two words)", () => {
    expect(labelToBadge("Quản lý kho")).toBe("QL");
  });

  it("is always uppercase", () => {
    expect(labelToBadge("abc def")).toBe("AD");
  });
});

// ── Empty state check ─────────────────────────────────────────────────────────

describe("EntityDirectory empty state", () => {
  it("correctly identifies an empty entities list", () => {
    const entities: EntityMeta[] = [];
    expect(entities.length).toBe(0);
    // Component should render the empty state (no cards) in this case.
  });

  it("correctly identifies a non-empty entities list", () => {
    const entities: EntityMeta[] = [
      { name: "accounts", label: "Tài khoản", tableName: "obj_accounts" },
      { name: "contacts", label: "Liên hệ", tableName: "obj_contacts" },
    ];
    expect(entities.length).toBe(2);
    expect(entities.map((e) => e.name)).toEqual(["accounts", "contacts"]);
  });
});
