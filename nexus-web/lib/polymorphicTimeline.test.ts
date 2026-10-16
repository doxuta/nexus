// lib/polymorphicTimeline.test.ts — the reverse-lookup that decides WHICH lists a record's detail
// page shows. Wrong here = either a missing timeline (the bug this slice closes) or a query against
// an entity that cannot point at this record (a 422 the user would see as a broken page).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { listPolymorphicRecords, type FieldMeta } from "@/lib/api";
import { findPolymorphicSources, type EntityFields } from "./polymorphicTimeline";

const poly = (name: string, targets: unknown, label?: string): FieldMeta => ({
  name, label, fieldType: "polymorphic_relation", options: { targetEntities: targets },
});
const plain = (name: string, fieldType = "text"): FieldMeta => ({ name, fieldType });

describe("findPolymorphicSources", () => {
  it("returns the (entity, field) pairs whose allowlist contains the target entity", () => {
    const candidates: EntityFields[] = [
      { entity: "crm_activities", fields: [plain("subject"), poly("related_to", ["crm_accounts", "crm_contacts"], "Liên quan tới")] },
      { entity: "tk_tickets", fields: [poly("related_to", ["crm_accounts"])] },
    ];
    expect(findPolymorphicSources("crm_accounts", candidates)).toEqual([
      { entity: "crm_activities", fieldName: "related_to", fieldLabel: "Liên quan tới" },
      { entity: "tk_tickets", fieldName: "related_to", fieldLabel: "related_to" },
    ]);
  });

  it("skips a polymorphic field whose allowlist does NOT include the target", () => {
    const candidates: EntityFields[] = [
      { entity: "crm_activities", fields: [poly("related_to", ["crm_contacts", "crm_opportunities"])] },
    ];
    expect(findPolymorphicSources("crm_accounts", candidates)).toEqual([]);
  });

  it("ignores a field that merely NAMES an entity but is not polymorphic (relation/lookup/text)", () => {
    // A 1-N `relation` field is the parent-side related list's job (ParentRelatedSection); pulling
    // it in here would render the same rows twice under two different headings.
    const relation: FieldMeta = { name: "account", fieldType: "relation", options: { relatedEntity: "crm_accounts", targetEntities: ["crm_accounts"] } };
    const candidates: EntityFields[] = [
      { entity: "crm_contacts", fields: [relation, plain("account_name")] },
    ];
    expect(findPolymorphicSources("crm_accounts", candidates)).toEqual([]);
  });

  it("matches every target in a multi-target allowlist (one field, several entities)", () => {
    const candidates: EntityFields[] = [
      { entity: "crm_activities", fields: [poly("related_to", ["crm_accounts", "crm_contacts", "crm_opportunities"])] },
    ];
    for (const target of ["crm_accounts", "crm_contacts", "crm_opportunities"]) {
      expect(findPolymorphicSources(target, candidates)).toHaveLength(1);
    }
    expect(findPolymorphicSources("crm_leads", candidates)).toEqual([]);
  });

  it("TWO polymorphic fields on the SAME entity targeting this one → two separate pairs", () => {
    // e.g. an activity that is both "about" an account and "logged against" one — two independent
    // lists. Keying a section by entity alone would silently drop the second.
    const candidates: EntityFields[] = [
      { entity: "crm_activities", fields: [poly("related_to", ["crm_accounts"]), poly("regarding", ["crm_accounts"])] },
    ];
    expect(findPolymorphicSources("crm_accounts", candidates).map((s) => s.fieldName)).toEqual(["related_to", "regarding"]);
  });

  it("trims untrimmed target names and the target itself before comparing", () => {
    // The backend's polymorphicTargetList() trims on READ, so untrimmed values survive in sys_fields.
    const candidates: EntityFields[] = [{ entity: "crm_activities", fields: [poly("related_to", ["  crm_accounts "])] }];
    expect(findPolymorphicSources(" crm_accounts ", candidates)).toHaveLength(1);
  });

  it("survives malformed metadata: missing options, non-array targets, non-string entries", () => {
    const noOptions: FieldMeta = { name: "a", fieldType: "polymorphic_relation" };
    const candidates: EntityFields[] = [
      { entity: "e1", fields: [noOptions] },
      { entity: "e2", fields: [poly("b", "crm_accounts")] },
      { entity: "e3", fields: [poly("c", [42, null, "crm_accounts"])] },
      { entity: "e4", fields: [poly("d", ["", "   "])] },
    ];
    // Only e3 has a usable string target; the rest must be dropped, not throw.
    expect(findPolymorphicSources("crm_accounts", candidates)).toEqual([
      { entity: "e3", fieldName: "c", fieldLabel: "c" },
    ]);
  });

  it("empty target entity or no candidates → no sources", () => {
    const candidates: EntityFields[] = [{ entity: "crm_activities", fields: [poly("related_to", ["crm_accounts"])] }];
    expect(findPolymorphicSources("", candidates)).toEqual([]);
    expect(findPolymorphicSources("   ", candidates)).toEqual([]);
    expect(findPolymorphicSources("crm_accounts", [])).toEqual([]);
  });

  it("a self-targeting entity lists itself (a sub-activity pointing at an activity)", () => {
    const candidates: EntityFields[] = [
      { entity: "crm_activities", fields: [poly("related_to", ["crm_activities", "crm_accounts"])] },
    ];
    expect(findPolymorphicSources("crm_activities", candidates)).toEqual([
      { entity: "crm_activities", fieldName: "related_to", fieldLabel: "related_to" },
    ]);
  });
});

// The OTHER half of the join: the query the helper's output feeds. The suffixes are a wire contract
// with the engine (record_orchestration.go List() admits `<poly>_type` / `<poly>_id` and nothing
// else); misspell one and every test above still passes, tsc still passes, and the timeline is
// silently empty forever — this project's central bug class. Pinned here rather than in a new
// api-*.test.ts so the whole polymorphic-timeline contract reads in one file.
describe("listPolymorphicRecords — list query contract", () => {
  const originalFetch = global.fetch;
  beforeEach(() => {
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("filters on <field>_type / <field>_id and sorts newest-first", async () => {
    const seen: string[] = [];
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      seen.push(String(url));
      return new Response(JSON.stringify({ data: [{ id: "a1", version: 1, data: {} }], pagination: { page: 1, limit: 5, total: 7, totalPages: 2 } }),
        { status: 200, headers: { "Content-Type": "application/json" } });
    }) as unknown as typeof fetch;

    const page = await listPolymorphicRecords("crm_activities", "related_to", "crm_accounts", "acc-1", 5);

    expect(seen).toHaveLength(1);
    const q = new URL(seen[0], "http://x").searchParams;
    expect(q.get("related_to_type")).toBe("crm_accounts");
    expect(q.get("related_to_id")).toBe("acc-1");
    expect(q.get("limit")).toBe("5");
    // Explicit, not inherited from the engine default: "newest first" is the point of a timeline.
    expect(q.get("sort")).toBe("created_at");
    expect(q.get("order")).toBe("desc");
    // The entity being listed is the one that OWNS the field, not the record's entity.
    expect(seen[0]).toContain("/crm_activities?");
    // total comes from the pagination block (the count badge), not from the previewed rows.
    expect(page.total).toBe(7);
    expect(page.records).toHaveLength(1);
  });
});
