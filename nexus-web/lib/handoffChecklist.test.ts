import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { AdminUser, AppReadiness } from "@/lib/api";
import type { BuildStepState } from "@/lib/buildSteps";
import { BLOCKING_STEP_KEYS, blockingCount, buildHandoffRows, canDeliver, usableOwners, type HandoffInput } from "./handoffChecklist";

const R = (o: Partial<AppReadiness> = {}): AppReadiness => ({
  entityCount: 2, entitiesMissingFields: [], allEntitiesHaveFields: true, hasNonAdminViewRole: true, hasNonAdminUserAssigned: true, ready: true, ...o,
});
const APP = (id: string, readiness?: AppReadiness | "error") => ({ id, name: id, label: id.toUpperCase(), readiness });
const step = (key: BuildStepState["key"], complete: boolean | null, optional = false): BuildStepState => ({ key, complete, optional, sublabel: "" });
const ALL_OK: BuildStepState[] = [
  step("data", true), step("screens", false, true), step("automation", false, true), step("access", true), step("reports", true), step("branding", true),
  step("trial", false), step("deliver", false),
];
const base = (o: Partial<HandoffInput> = {}): HandoffInput => ({ steps: ALL_OK, apps: [APP("crm", R())], samples: { total: 0 }, entityLabels: {}, ...o });
const row = (rows: ReturnType<typeof buildHandoffRows>, id: string) => rows.find((r) => r.id === id)!;

describe("buildHandoffRows", () => {
  it("blocking set is the required steps before Chạy thử", () => {
    expect([...BLOCKING_STEP_KEYS].sort()).toEqual(["access", "branding", "data", "reports"]);
  });
  it("steps null: unknown, not blocking", () => {
    const r = row(buildHandoffRows(base({ steps: null })), "steps");
    expect(r).toMatchObject({ tone: "unknown", blocking: false });
    expect(r.msg.key).toBe("studio.build.handover.row.steps.unknown");
    expect(r.action).toBeUndefined();
  });
  it("steps error: unknown, blocking, retry", () => {
    const r = row(buildHandoffRows(base({ steps: "error" })), "steps");
    expect(r).toMatchObject({ tone: "unknown", blocking: true, action: { kind: "retry", source: "steps" } });
  });
  it("all blocking steps done: ok even when Chạy thử is next", () => {
    const r = row(buildHandoffRows(base()), "steps");
    expect(r).toMatchObject({ tone: "ok", blocking: false });
    expect(r.action).toBeUndefined();
  });
  it("missing reports and branding: todo, blocking, opens first missing step", () => {
    const steps = ALL_OK.map((s) => (s.key === "reports" || s.key === "branding" ? { ...s, complete: false } : s));
    const r = row(buildHandoffRows(base({ steps })), "steps");
    expect(r).toMatchObject({ tone: "todo", blocking: true, action: { kind: "link", routeId: "dung/bao-cao" } });
    expect(r.msg.params).toMatchObject({ n: 2 });
  });
  it("screens/automation/trial incomplete never block", () => {
    const steps = ALL_OK.map((s) => (["screens", "automation", "trial"].includes(s.key) ? { ...s, complete: false } : s));
    expect(row(buildHandoffRows(base({ steps })), "steps").blocking).toBe(false);
  });
  it("complete null on a blocking step does not block", () => {
    const steps = ALL_OK.map((s) => (s.key === "data" ? { ...s, complete: null } : s));
    expect(row(buildHandoffRows(base({ steps })), "steps")).toMatchObject({ tone: "ok", blocking: false });
  });
  it("app ready: ok", () => {
    expect(row(buildHandoffRows(base()), "app:crm")).toMatchObject({ tone: "ok", blocking: false });
  });
  it("app priority fields > view role > staff, single action", () => {
    const rd = R({ allEntitiesHaveFields: false, entitiesMissingFields: ["student", "x"], hasNonAdminViewRole: false, hasNonAdminUserAssigned: false, ready: false });
    const r = row(buildHandoffRows(base({ apps: [APP("crm", rd)], entityLabels: { student: "Học viên" } })), "app:crm");
    expect(r).toMatchObject({ tone: "todo", blocking: true, action: { kind: "link", routeId: "dung/du-lieu/:bang", params: { bang: "student" }, labelKey: "studio.build.handover.action.addField" } });
    expect(r.msg.params?.entities).toContain("Học viên");
    expect(r.msg.params?.entities).toContain("x");
    expect(r.msg.params?.entities).not.toContain("student");
  });
  it("app missing view role then staff", () => {
    const a = row(buildHandoffRows(base({ apps: [APP("crm", R({ hasNonAdminViewRole: false, hasNonAdminUserAssigned: false, ready: false }))] })), "app:crm");
    expect(a.action).toMatchObject({ routeId: "dung/nguoi-dung-quyen", labelKey: "studio.build.handover.action.grantView" });
    const b = row(buildHandoffRows(base({ apps: [APP("crm", R({ hasNonAdminUserAssigned: false, ready: false }))] })), "app:crm");
    expect(b.action).toMatchObject({ routeId: "van-hanh/nguoi-dung", labelKey: "studio.build.handover.action.addStaff" });
  });
  it.each([[undefined], ["error" as const]])("app readiness %s: unknown, blocking, retry", (readiness) => {
    const r = row(buildHandoffRows(base({ apps: [APP("crm", readiness)] })), "app:crm");
    expect(r).toMatchObject({ tone: "unknown", blocking: true, action: { kind: "retry", source: "app:crm" } });
  });
  it("apps error: one unknown blocking row with retry", () => {
    const rows = buildHandoffRows(base({ apps: "error" }));
    expect(row(rows, "apps")).toMatchObject({ tone: "unknown", blocking: true, action: { kind: "retry", source: "apps" } });
    expect(rows.some((r) => r.id.startsWith("app:"))).toBe(false);
  });
  it("apps empty: blocking, opens App và menu", () => {
    expect(row(buildHandoffRows(base({ apps: [] })), "apps")).toMatchObject({ tone: "todo", blocking: true, action: { routeId: "dung/man-hinh/app" } });
  });
  it("samples gate: 0 ok; >0 blocks with a wipe button; error blocks as unknown", () => {
    expect(row(buildHandoffRows(base()), "samples")).toMatchObject({ tone: "ok", blocking: false });
    const t = row(buildHandoffRows(base({ samples: { total: 7 } })), "samples");
    expect(t).toMatchObject({ tone: "todo", blocking: true, action: { kind: "wipe" } });
    expect(t.note).toBeUndefined();
    expect(row(buildHandoffRows(base({ samples: "error" })), "samples")).toMatchObject({ tone: "unknown", blocking: true, action: { kind: "retry" } });
  });
  it("kept samples: blocks until ticked, then a reminder; canDeliver agrees", () => {
    const held = { total: 3, kept: 3 };
    const open = buildHandoffRows(base({ samples: held }));
    expect(row(open, "samples")).toMatchObject({ blocking: true, keptHeld: true });
    expect(canDeliver(open)).toBe(false);
    expect(canDeliver(open, { keptSampleAck: true })).toBe(true);
    const acked = buildHandoffRows(base({ samples: held, keptSampleAck: true }));
    expect(row(acked, "samples")).toMatchObject({ blocking: false, keptHeld: true });
    expect(row(acked, "samples").note?.key).toBe("studio.build.handover.row.nonBlocking");
    expect(canDeliver(acked)).toBe(true);
    // ack means nothing when other samples (not kept) remain
    expect(row(buildHandoffRows(base({ samples: { total: 5, kept: 3 }, keptSampleAck: true })), "samples").blocking).toBe(true);
  });
  it("non-ok non-blocking rows carry the note, blocking rows do not", () => {
    const rows = buildHandoffRows(base({ steps: null, apps: [APP("crm", "error")], samples: { total: 3 } }));
    for (const r of rows) {
      if (r.tone !== "ok" && !r.blocking) expect(r.note?.key).toBe("studio.build.handover.row.nonBlocking");
      if (r.blocking) expect(r.note).toBeUndefined();
    }
  });
  it("counts and canDeliver", () => {
    const bad = buildHandoffRows(base({ steps: "error", apps: [APP("a", "error"), APP("b", R())] }));
    expect(blockingCount(bad)).toBe(2);
    expect(canDeliver(bad)).toBe(false);
    const good = buildHandoffRows(base());
    expect(blockingCount(good)).toBe(0);
    expect(canDeliver(good)).toBe(true);
  });
  describe("owner row (f5-handoff-owner)", () => {
    const U = (o: Partial<AdminUser> = {}): AdminUser => ({ id: "u1", tenantId: "t", email: "chu@khach.vn", fullName: "", status: "active", isAdmin: false, canManageUsers: true, roleId: "r1", ...o });
    const ROLES = [{ id: "r1", name: "tenant_owner", label: "Chủ khách" }];
    const owner = (users: AdminUser[]) => ({ users, roles: ROLES });
    const ownerRows = (rows: ReturnType<typeof buildHandoffRows>) => rows.filter((r) => r.id.startsWith("owner"));

    it("owner undefined: no row at all", () => {
      expect(ownerRows(buildHandoffRows(base()))).toHaveLength(0);
    });
    it("usableOwners keeps active owners only", () => {
      const list = [U(), U({ id: "u2", status: "inactive" }), U({ id: "u3", canManageUsers: false }), U({ id: "u4", status: "disabled" })];
      expect(usableOwners(list).map((u) => u.id)).toEqual(["u1"]);
    });
    it("one usable owner: ok row with email and role, never blocking", () => {
      const rows = ownerRows(buildHandoffRows(base({ owner: owner([U()]) })));
      expect(rows).toHaveLength(1);
      expect(rows[0]).toMatchObject({ id: "owner:u1", tone: "ok", blocking: false, owner: { id: "u1" } });
      expect(rows[0].msg).toEqual({ key: "studio.build.handover.row.owner.ok", params: { email: "chu@khach.vn", role: "Chủ khách" } });
    });
    it("two owners: two rows", () => {
      expect(ownerRows(buildHandoffRows(base({ owner: owner([U(), U({ id: "u2", email: "b@k.vn" })]) })))).toHaveLength(2);
    });
    it("none usable (locked owner / admin only): reminder row, does not block", () => {
      const rows = buildHandoffRows(base({ owner: owner([U({ status: "inactive" }), U({ id: "a", canManageUsers: false, isAdmin: true })]) }));
      const r = row(rows, "owner");
      expect(r).toMatchObject({ tone: "todo", blocking: false });
      expect(r.msg.key).toBe("studio.build.handover.row.owner.none");
      expect(canDeliver(rows)).toBe(true);
    });
    it("load error: unknown with retry, never the 'none' sentence, never blocking", () => {
      const rows = buildHandoffRows(base({ owner: "error" }));
      const r = row(rows, "owner");
      expect(r).toMatchObject({ tone: "unknown", blocking: false, action: { kind: "retry", source: "owner" } });
      expect(r.msg.key).toBe("studio.build.handover.row.owner.unknown");
      expect(canDeliver(rows)).toBe(true);
    });
    it("canManageUsers missing everywhere (old server): unknown, not 'none'", () => {
      const rows = buildHandoffRows(base({ owner: owner([U({ canManageUsers: undefined, isAdmin: true })]) }));
      expect(row(rows, "owner").msg.key).toBe("studio.build.handover.row.owner.unknown");
    });
  });
  it("returns i18n keys, not Vietnamese text", () => {
    const src = readFileSync("lib/handoffChecklist.ts", "utf8").replace(/\/\/.*$/gm, "");
    expect(src).not.toMatch(/[àáạảãâầấậẩẫăằắặẳẵèéẹẻẽêềếệểễìíịỉĩòóọỏõôồốộổỗơờớợởỡùúụủũưừứựửữỳýỵỷỹđ]/i);
  });
});
