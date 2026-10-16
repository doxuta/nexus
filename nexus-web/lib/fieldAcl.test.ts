import fs from "node:fs";
import path from "node:path";
import { describe, it, expect } from "vitest";
import {
  cellKey,
  splitCellKey,
  stateFromGrant,
  grantBoolsFromState,
  cycleCellState,
  buildMatrix,
  getCellState,
  setCellState,
  changedKeys,
  isDirty,
  buildPutPayload,
  restrictionCountForField,
  requiredEngineCannotFill,
  studioCanSetDefault,
  lockedDeadEnds,
  deadEndRoleIds,
  DEFAULT_FILL_TYPES,
  type FieldAclGrant,
} from "@/lib/fieldAcl";

const ROLE = "11111111-role";
const ROLE2 = "22222222-role";

describe("fieldAcl — key helpers", () => {
  it("round-trips role + field through the composite key", () => {
    const k = cellKey(ROLE, "amount");
    expect(splitCellKey(k)).toEqual({ roleId: ROLE, fieldName: "amount" });
  });
  it("splits field names that contain no delimiter safely", () => {
    const k = cellKey(ROLE, "close_date");
    expect(splitCellKey(k).fieldName).toBe("close_date");
  });
});

describe("fieldAcl — tri-state mapping", () => {
  it("maps grant bools to tri-state (hidden normalizes canEdit)", () => {
    expect(stateFromGrant(true, true)).toBe("default");
    expect(stateFromGrant(true, false)).toBe("readonly");
    expect(stateFromGrant(false, false)).toBe("hidden");
    expect(stateFromGrant(false, true)).toBe("hidden"); // canView=false wins
  });
  it("maps tri-state back to grant bools; default clears via (true,true)", () => {
    expect(grantBoolsFromState("default")).toEqual({ canView: true, canEdit: true });
    expect(grantBoolsFromState("readonly")).toEqual({ canView: true, canEdit: false });
    expect(grantBoolsFromState("hidden")).toEqual({ canView: false, canEdit: false });
  });
  it("cycles default → readonly → hidden → default", () => {
    expect(cycleCellState("default")).toBe("readonly");
    expect(cycleCellState("readonly")).toBe("hidden");
    expect(cycleCellState("hidden")).toBe("default");
  });
});

describe("fieldAcl — matrix from grants", () => {
  const grants: FieldAclGrant[] = [
    { roleId: ROLE, entityName: "crm_opportunities", fieldName: "amount", canView: false, canEdit: false },
    { roleId: ROLE, entityName: "crm_opportunities", fieldName: "probability", canView: true, canEdit: false },
  ];
  it("builds a sparse matrix keeping only restricted cells", () => {
    const m = buildMatrix(grants);
    expect(getCellState(m, ROLE, "amount")).toBe("hidden");
    expect(getCellState(m, ROLE, "probability")).toBe("readonly");
    expect(getCellState(m, ROLE, "name")).toBe("default"); // absent ⇒ default
    expect(Object.keys(m)).toHaveLength(2);
  });
  it("setCellState clears the key when set back to default", () => {
    let m = buildMatrix(grants);
    m = setCellState(m, ROLE, "amount", "default");
    expect(getCellState(m, ROLE, "amount")).toBe("default");
    expect(cellKey(ROLE, "amount") in m).toBe(false);
  });
});

describe("fieldAcl — dirty tracking + upsert-by-key payload", () => {
  const saved = buildMatrix([
    { roleId: ROLE, entityName: "crm_opportunities", fieldName: "amount", canView: false, canEdit: false },
  ]);

  it("no change ⇒ not dirty, empty payload", () => {
    expect(isDirty(saved, saved)).toBe(false);
    expect(buildPutPayload(saved, saved, "crm_opportunities")).toEqual([]);
  });

  it("toggle then toggle back ⇒ not dirty (no phantom write)", () => {
    let draft = setCellState(saved, ROLE, "name", "hidden");
    draft = setCellState(draft, ROLE, "name", "default");
    expect(isDirty(draft, saved)).toBe(false);
    expect(changedKeys(draft, saved)).toEqual([]);
  });

  it("hiding a new cell emits an explicit (false,false) grant", () => {
    const draft = setCellState(saved, ROLE, "probability", "hidden");
    const payload = buildPutPayload(draft, saved, "crm_opportunities");
    expect(payload).toContainEqual({
      roleId: ROLE,
      entityName: "crm_opportunities",
      fieldName: "probability",
      canView: false,
      canEdit: false,
    });
  });

  // THE §11.1 warning: clearing a restriction must send EXPLICIT (true,true), never vanish.
  it("toggling a restricted cell back to default emits an EXPLICIT (true,true) clear", () => {
    const draft = setCellState(saved, ROLE, "amount", "default");
    const payload = buildPutPayload(draft, saved, "crm_opportunities");
    expect(payload).toEqual([
      {
        roleId: ROLE,
        entityName: "crm_opportunities",
        fieldName: "amount",
        canView: true,
        canEdit: true,
      },
    ]);
  });

  it("unchanged cells are NEVER in the payload (absent ⇒ backend keeps them)", () => {
    // Two roles restrict amount; change only ROLE2's → ROLE's stays absent.
    const base = buildMatrix([
      { roleId: ROLE, entityName: "crm_opportunities", fieldName: "amount", canView: false, canEdit: false },
      { roleId: ROLE2, entityName: "crm_opportunities", fieldName: "amount", canView: false, canEdit: false },
    ]);
    const draft = setCellState(base, ROLE2, "amount", "readonly");
    const payload = buildPutPayload(draft, base, "crm_opportunities");
    expect(payload).toHaveLength(1);
    expect(payload[0].roleId).toBe(ROLE2);
    expect(payload[0]).toMatchObject({ canView: true, canEdit: false });
  });

  it("payload order matches changedKeys so 422 details.index maps to a cell", () => {
    let draft = setCellState(saved, ROLE, "amount", "readonly");
    draft = setCellState(draft, ROLE, "probability", "hidden");
    const keys = changedKeys(draft, saved);
    const payload = buildPutPayload(draft, saved, "crm_opportunities");
    payload.forEach((g, i) => {
      expect(cellKey(g.roleId, g.fieldName)).toBe(keys[i]);
    });
  });
});

describe("fieldAcl — per-field restriction count", () => {
  it("counts restricted cells across roles for a field", () => {
    const m = buildMatrix([
      { roleId: ROLE, entityName: "e", fieldName: "amount", canView: false, canEdit: false },
      { roleId: ROLE2, entityName: "e", fieldName: "amount", canView: true, canEdit: false },
      { roleId: ROLE, entityName: "e", fieldName: "name", canView: true, canEdit: false },
    ]);
    expect(restrictionCountForField(m, "amount")).toBe(2);
    expect(restrictionCountForField(m, "name")).toBe(1);
    expect(restrictionCountForField(m, "stage")).toBe(0);
  });
});

// queue #146 — the FE mirror of services.requiredEngineCannotFill (field_acl.go): "required,
// written by clients, no default the engine materializes at create". A lock on such a field leaves
// the role unable to create anything (RECORD_REQUIRED_LOCKED); the matrix and the create forms say
// so up front instead of waiting for the 422.
describe("fieldAcl — required field the engine cannot fill (queue #146)", () => {
  const f = (fieldType: string, isRequired: boolean, defaultValue?: string) => ({ fieldType, isRequired, defaultValue });

  it.each([
    ["required text, no default", f("text", true), true],
    ["required text, blank default", f("text", true, "   "), true],
    ["required text with a default the engine writes", f("text", true, "Mới"), false],
    ["required number with default 0", f("number", true, "0"), false],
    ["required relation with a record-id default (API/pack)", f("relation", true, "rec-1"), false],
    // A stored default the engine DROPS is no default: the legacy currency/user/composite rows.
    ["required currency carrying a legacy default", f("currency", true, "5000"), true],
    ["required user carrying a legacy default", f("user", true, "u-1"), true],
    ["required composite carrying a legacy default", f("composite", true, "x"), true],
    // Server-assigned types are never written by a client, so a lock cannot strand a create.
    ["required formula", f("formula", true), false],
    ["required auto_number", f("auto_number", true), false],
    ["optional text", f("text", false), false],
  ])("%s", (_name, field, want) => {
    expect(requiredEngineCannotFill(field)).toBe(want);
  });

  // Drift guard: DEFAULT_FILL_TYPES must be exactly the branches of services.defaultValue that
  // write a stored default into a create (every `case` except "formula", which returns no value).
  // A type the engine starts (or stops) filling turns this red before the matrix lies about it.
  it("DEFAULT_FILL_TYPES matches the fill branches of services.defaultValue", () => {
    const src = fs.readFileSync(
      path.join(process.cwd(), "../nexus-platform/internal/services/record_service.go"),
      "utf8",
    );
    const start = src.indexOf("func defaultValue(");
    const end = src.indexOf("func defaultFills(");
    expect(start).toBeGreaterThan(-1);
    expect(end).toBeGreaterThan(start);
    const body = src.slice(start, end);
    const cases = [...body.matchAll(/^\tcase (.+):$/gm)].flatMap((m) => [...m[1].matchAll(/"([a-z_]+)"/g)].map((x) => x[1]));
    expect(cases).toContain("formula"); // the parse found the switch
    expect(new Set(cases.filter((c) => c !== "formula"))).toEqual(DEFAULT_FILL_TYPES);
  });

  it("studioCanSetDefault: only types the field builder has a default input for (relation has the record picker, queue #320)", () => {
    expect(studioCanSetDefault("text")).toBe(true);
    expect(studioCanSetDefault("picklist")).toBe(true);
    expect(studioCanSetDefault("relation")).toBe(true);
    expect(studioCanSetDefault("currency")).toBe(false);
    expect(studioCanSetDefault("user")).toBe(false);
  });

  it("lockedDeadEnds: the caller's read-only fields that strand a create, and nothing else", () => {
    const fields = [
      { name: "name", fieldType: "text", isRequired: true },
      { name: "amount", fieldType: "currency", isRequired: true, readOnly: true },
      { name: "stage", fieldType: "picklist", isRequired: true, defaultValue: "new", readOnly: true },
      { name: "note", fieldType: "text", readOnly: true },
      // A stored default the engine drops is no default — "has a defaultValue" is the wrong test.
      { name: "owner", fieldType: "user", isRequired: true, defaultValue: "u-1", readOnly: true },
      // Server-assigned: no client ever writes it, so a lock strands nothing.
      { name: "code", fieldType: "auto_number", isRequired: true, readOnly: true },
    ];
    expect(lockedDeadEnds(fields).map((x) => x.name)).toEqual(["amount", "owner"]);
  });

  it("deadEndRoleIds: every role whose cell locks the field (read-only or hidden), in role order", () => {
    const m = buildMatrix([
      { roleId: ROLE, entityName: "e", fieldName: "amount", canView: true, canEdit: false },
      { roleId: ROLE2, entityName: "e", fieldName: "amount", canView: false, canEdit: false },
      { roleId: ROLE, entityName: "e", fieldName: "name", canView: true, canEdit: false },
    ]);
    expect(deadEndRoleIds(m, "amount", [ROLE2, ROLE, "r3"])).toEqual([ROLE2, ROLE]);
    expect(deadEndRoleIds(m, "stage", [ROLE, ROLE2])).toEqual([]);
  });
});
