import { describe, it, expect } from "vitest";
import {
  planToDiffSections,
  conflictSeverity,
  partitionConflicts,
  requiresTypeName,
  planIsClean,
  installStatusForSlug,
  sortInstallsNewestFirst,
  manifestDisplay,
  installResultSummary,
  parsePackFile,
  packContentsFromPlan,
  type InstallPlan,
  type InstallResult,
  type InstallRecord,
} from "./verticalPlan";

// A plan mirroring the live crm dry-run (all skips + unique conflict + layout overwrites).
const crmPlan: InstallPlan = {
  entitiesToCreate: [],
  entitiesToSkip: ["crm_accounts", "crm_contacts", "crm_opportunities", "crm_leads", "crm_activities"],
  fieldsToAdd: [],
  nnToRegister: [],
  nnToSkip: ["crm_contacts_opportunities"],
  layoutsToInstall: ["crm_accounts:list", "crm_accounts:detail"],
  rolesToCreate: [],
  rolesToSkip: ["crm_sales_rep", "crm_sales_manager"],
  samplesCount: 21,
  conflicts: [
    { kind: "unique_slot_held", detail: "obj_crm_contacts.email…", entity: "crm_contacts", field: "email" },
    { kind: "layout_overwrite", detail: "crm_accounts:list…", entity: "crm_accounts" },
  ],
};

// A clean plan for a novel pack (create entities, zero conflict).
const cleanPlan: InstallPlan = {
  entitiesToCreate: ["demo_widgets"],
  entitiesToSkip: [],
  fieldsToAdd: [
    { entity: "demo_widgets", field: "owner", deferred: false },
    { entity: "demo_widgets", field: "total_value", deferred: true },
  ],
  nnToRegister: ["demo_widgets_parts"],
  nnToSkip: [],
  layoutsToInstall: ["demo_widgets:list"],
  rolesToCreate: ["demo_admin"],
  rolesToSkip: [],
  samplesCount: 0,
  conflicts: [],
};

// A pack that ships a receiving endpoint. Kept separate from cleanPlan so the "pack predating
// PACK-HOOKS has no hooks arrays at all" case stays covered by every other fixture.
const hookPlan: InstallPlan = {
  ...cleanPlan,
  hooksToCreate: ["bank-credit"],
  hooksToSkip: ["legacy-feed"],
};

// A pack that ships the full late-pass set — the shape a real vertical actually has.
const latePassPlan: InstallPlan = {
  ...cleanPlan,
  teamsToCreate: ["Đội Sales"],
  dashboardsToCreate: ["Tổng quan"],
  workflowsToCreate: ["Escalate on create"],
  workflowsToSkip: ["SLA timer"],
  reportsToCreate: ["Doanh thu tháng"],
  workQueuesToCreate: ["Việc hôm nay"],
  approvalsToCreate: ["crm_quotes:Duyệt giá cao"],
  hooksToCreate: ["bank-credit"],
};

describe("inbound hooks in the plan view", () => {
  it("surfaces the hook section — an install that stands up a live endpoint must say so", () => {
    const sections = planToDiffSections(hookPlan);
    const hooks = sections.find((s) => s.key === "hooks");
    expect(hooks).toBeDefined();
    expect(hooks!.count).toBe(2);
    expect(hooks!.rows[0]).toEqual({
      label: "bank-credit",
      tone: "create",
      note: "endpoint nhận tín hiệu — secret sinh mới",
    });
    expect(hooks!.rows[1].tone).toBe("skip");
    expect(packContentsFromPlan(hookPlan).hooksCount).toBe(2);
  });

  it("a pack with no hooks shows no hook section and counts zero", () => {
    expect(planToDiffSections(cleanPlan).some((s) => s.key === "hooks")).toBe(false);
    expect(packContentsFromPlan(cleanPlan).hooksCount).toBe(0);
  });
});

describe("late passes are visible in the plan", () => {
  it("every pass the backend runs has a section — install never does more than the plan says", () => {
    const keys = planToDiffSections(latePassPlan).map((s) => s.key);
    for (const k of ["teams", "dashboards", "workflows", "reports", "workQueues", "approvals", "hooks"]) {
      expect(keys).toContain(k);
    }
    const wf = planToDiffSections(latePassPlan).find((s) => s.key === "workflows")!;
    expect(wf.count).toBe(2);
    expect(wf.rows[0]).toEqual({ label: "Escalate on create", tone: "create", note: "sẽ chạy ngay sau khi cài" });
    expect(wf.rows[1]).toEqual({ label: "SLA timer", tone: "skip", note: "đã có" });
  });

  it("counts group the late passes the way the preview card reads them", () => {
    const c = packContentsFromPlan(latePassPlan);
    expect(c.teamsCount).toBe(1);
    expect(c.insightsCount).toBe(2); // 1 dashboard + 1 report
    expect(c.automationCount).toBe(4); // 2 workflows + 1 approval + 1 queue
    expect(c.hooksCount).toBe(1);
  });

  it("a plan from an older server (no late-pass keys at all) yields no sections and zero counts", () => {
    const keys = planToDiffSections(cleanPlan).map((s) => s.key);
    for (const k of ["teams", "dashboards", "workflows", "reports", "workQueues", "approvals", "hooks"]) {
      expect(keys).not.toContain(k);
    }
    const c = packContentsFromPlan(cleanPlan);
    expect([c.teamsCount, c.insightsCount, c.automationCount, c.hooksCount]).toEqual([0, 0, 0, 0]);
  });
});

describe("planToDiffSections", () => {
  it("groups skips as skip-tone and drops empty sections", () => {
    const sections = planToDiffSections(crmPlan);
    const keys = sections.map((s) => s.key);
    expect(keys).toContain("entities");
    expect(keys).toContain("nn");
    expect(keys).toContain("layouts");
    expect(keys).toContain("roles");
    // No fields to add → no fields section.
    expect(keys.every((k) => !k.startsWith("fields:"))).toBe(true);
    const ent = sections.find((s) => s.key === "entities")!;
    expect(ent.rows.every((r) => r.tone === "skip")).toBe(true);
    expect(ent.count).toBe(5);
  });

  it("marks created entities teal and groups fields per entity, tagging deferred rollups", () => {
    const sections = planToDiffSections(cleanPlan);
    const ent = sections.find((s) => s.key === "entities")!;
    expect(ent.rows[0]).toMatchObject({ label: "demo_widgets", tone: "create" });

    const fields = sections.find((s) => s.key === "fields:demo_widgets")!;
    expect(fields.count).toBe(2);
    const deferred = fields.rows.find((r) => r.label === "total_value")!;
    expect(deferred.note).toContain("hoãn");
    const plain = fields.rows.find((r) => r.label === "owner")!;
    expect(plain.note).toBeUndefined();
  });

  it("layouts are info-tone (upsert), not create", () => {
    const sections = planToDiffSections(cleanPlan);
    const layouts = sections.find((s) => s.key === "layouts")!;
    expect(layouts.rows.every((r) => r.tone === "info")).toBe(true);
  });
});

describe("conflictSeverity upgrade_available", () => {
  it("is soft, not fail-loud hard", () => {
    expect(conflictSeverity("upgrade_available")).toMatchObject({ severity: "soft", tone: "wn" });
  });
});

describe("conflictSeverity", () => {
  it("unique_slot_held is HARD/danger", () => {
    expect(conflictSeverity("unique_slot_held")).toMatchObject({ severity: "hard", tone: "dg" });
  });
  it("layout_overwrite is SOFT/warn", () => {
    expect(conflictSeverity("layout_overwrite")).toMatchObject({ severity: "soft", tone: "wn" });
  });
  it("unknown kinds default HARD (fail-loud)", () => {
    expect(conflictSeverity("some_future_kind").severity).toBe("hard");
  });
});

describe("partitionConflicts", () => {
  it("splits hard vs soft preserving membership", () => {
    const { hard, soft } = partitionConflicts(crmPlan.conflicts);
    expect(hard.map((c) => c.kind)).toEqual(["unique_slot_held"]);
    expect(soft.map((c) => c.kind)).toEqual(["layout_overwrite"]);
  });
});

describe("requiresTypeName / planIsClean", () => {
  it("ANY conflict (even soft-only) forces type-name and is not clean", () => {
    const softOnly: InstallPlan = { ...cleanPlan, conflicts: [{ kind: "layout_overwrite", detail: "x" }] };
    expect(requiresTypeName(softOnly)).toBe(true);
    expect(planIsClean(softOnly)).toBe(false);
  });
  it("crm plan (has hard + soft) requires type-name", () => {
    expect(requiresTypeName(crmPlan)).toBe(true);
    expect(planIsClean(crmPlan)).toBe(false);
  });
  it("zero-conflict plan is clean → simple confirm", () => {
    expect(requiresTypeName(cleanPlan)).toBe(false);
    expect(planIsClean(cleanPlan)).toBe(true);
  });
});

describe("installStatusForSlug", () => {
  const installs: InstallRecord[] = [
    { id: "1", slug: "crm", version: "1.0.0", installedAt: "2026-07-01T10:00:00Z" },
    { id: "2", slug: "crm", version: "1.1.0", installedAt: "2026-07-03T10:00:00Z" },
    { id: "3", slug: "clinic", version: "1.0.0", installedAt: "2026-07-02T10:00:00Z" },
  ];
  it("picks the newest ledger row per slug as current install", () => {
    const st = installStatusForSlug("crm", installs);
    expect(st).toMatchObject({ installed: true, version: "1.1.0", installedAt: "2026-07-03T10:00:00Z" });
  });
  it("returns not-installed when no ledger row exists (honest — predates ledger)", () => {
    expect(installStatusForSlug("hrm", installs)).toEqual({ installed: false });
  });
  it("treats empty version string as undefined", () => {
    const st = installStatusForSlug("x", [{ id: "9", slug: "x", version: "", installedAt: "2026-07-03T10:00:00Z" }]);
    expect(st.installed).toBe(true);
    expect(st.version).toBeUndefined();
  });
});

describe("sortInstallsNewestFirst", () => {
  it("orders by installedAt desc without mutating input", () => {
    const input: InstallRecord[] = [
      { id: "a", slug: "x", version: "1", installedAt: "2026-07-01T00:00:00Z" },
      { id: "b", slug: "x", version: "2", installedAt: "2026-07-05T00:00:00Z" },
      { id: "c", slug: "x", version: "3", installedAt: "2026-07-03T00:00:00Z" },
    ];
    const out = sortInstallsNewestFirst(input);
    expect(out.map((r) => r.id)).toEqual(["b", "c", "a"]);
    expect(input.map((r) => r.id)).toEqual(["a", "b", "c"]); // unmutated
  });
});

describe("packContentsFromPlan", () => {
  it("fully-installed pack (all skips) still shows full contents, tagged exists", () => {
    // crmPlan mirrors the live crm dry-run on an already-installed workspace.
    const c = packContentsFromPlan(crmPlan);
    expect(c.entities).toHaveLength(5);
    expect(c.entities.every((e) => e.exists)).toBe(true);
    expect(c.nnCount).toBe(1); // registered N-N still counted (pack-total, not delta)
    expect(c.rolesCount).toBe(2);
    expect(c.layoutsCount).toBe(2);
    expect(c.samplesCount).toBe(21);
    expect(c.fieldsToAddCount).toBe(0);
    expect(c.rollupCount).toBe(0);
  });

  it("fresh pack: to-create entities first with exists=false, rollups counted from deferred", () => {
    const c = packContentsFromPlan(cleanPlan);
    expect(c.entities).toEqual([{ name: "demo_widgets", exists: false }]);
    expect(c.fieldsToAddCount).toBe(2);
    expect(c.rollupCount).toBe(1); // total_value is deferred (Pass-2b rollup)
    expect(c.nnCount).toBe(1);
    expect(c.rolesCount).toBe(1);
    expect(c.samplesCount).toBe(0);
  });

  it("mixed plan orders to-create before already-present", () => {
    const mixed: InstallPlan = {
      ...cleanPlan,
      entitiesToCreate: ["b_new"],
      entitiesToSkip: ["a_old"],
    };
    const c = packContentsFromPlan(mixed);
    expect(c.entities).toEqual([
      { name: "b_new", exists: false },
      { name: "a_old", exists: true },
    ]);
  });
});

describe("manifestDisplay", () => {
  it("falls back to the given name when manifest is absent", () => {
    expect(manifestDisplay(undefined, "CRM")).toEqual({ name: "CRM" });
  });
  it("normalises a full manifest and drops empty optionals", () => {
    const d = manifestDisplay(
      { name: "CRM", slug: "crm", version: "1.1.0", description: "  ", author: "Nexus" },
      "fallback",
    );
    expect(d).toMatchObject({ name: "CRM", slug: "crm", version: "1.1.0", author: "Nexus" });
    expect(d.description).toBeUndefined();
  });
});

describe("installResultSummary", () => {
  const base: InstallResult = {
    entitiesCreated: [], entitiesSkipped: [], relationFieldsAdded: [], fieldsSkipped: [],
    layoutsInstalled: [], rolesCreated: [], rolesSkipped: [], nnRelationsCreated: [],
    nnRelationsSkipped: [], deferredFieldsAdded: [], samplesInserted: 0, linksCreated: 0,
    dashboardsCreated: [], dashboardsSkipped: [],
  };
  it("drops zero-count lines and keeps the real ones", () => {
    const r: InstallResult = {
      ...base,
      entitiesCreated: ["a", "b", "c"],
      samplesInserted: 21,
      layoutsInstalled: ["a:list"],
    };
    const lines = installResultSummary(r);
    const byLabel = Object.fromEntries(lines.map((l) => [l.label, l.count]));
    expect(byLabel["Đối tượng đã tạo"]).toBe(3);
    expect(byLabel["Bản ghi mẫu đã chèn"]).toBe(21);
    expect(byLabel["Bố cục đã cài"]).toBe(1);
    // Zero-count lines absent.
    expect(lines.find((l) => l.label === "Vai trò đã tạo")).toBeUndefined();
  });
  it("empty result → empty summary", () => {
    expect(installResultSummary(base)).toEqual([]);
  });
});

describe("parsePackFile", () => {
  it("rejects non-JSON", () => {
    const r = parsePackFile("{not json");
    expect(r.ok).toBe(false);
  });
  it("rejects a pack with no entities", () => {
    const r = parsePackFile(JSON.stringify({ manifest: { name: "x", version: "1.0.0" }, entities: [] }));
    expect(r.ok).toBe(false);
  });
  it("rejects an entity missing a name", () => {
    const r = parsePackFile(JSON.stringify({ entities: [{ label: "no name" }] }));
    expect(r.ok).toBe(false);
  });
  it("accepts a bare pack and reads its manifest", () => {
    const r = parsePackFile(
      JSON.stringify({ manifest: { name: "Demo", version: "2.0.0" }, entities: [{ name: "demo_x" }] }),
    );
    expect(r.ok).toBe(true);
    if (r.ok) {
      expect(r.entityCount).toBe(1);
      expect(r.manifest.name).toBe("Demo");
      expect(r.manifest.version).toBe("2.0.0");
    }
  });
  it("unwraps an export envelope { data: { pack } } (round-trips a downloaded file)", () => {
    const envelope = { data: { pack: { manifest: { name: "Exp", version: "1.0.0" }, entities: [{ name: "e1" }] } } };
    const r = parsePackFile(JSON.stringify(envelope));
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.pack.entities?.[0]).toMatchObject({ name: "e1" });
  });
  it("falls back to a neutral name when the pack has no manifest", () => {
    const r = parsePackFile(JSON.stringify({ entities: [{ name: "e1" }] }));
    expect(r.ok).toBe(true);
    if (r.ok) {
      expect(r.manifest.name).toBe("Pack nhập từ tệp");
      expect(r.manifest.version).toBeUndefined();
    }
  });
});
