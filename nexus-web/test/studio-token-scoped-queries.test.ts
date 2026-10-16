// specs/073 kh1a T165 (FR-022): the four reads that depend on the tenant of the token — GET /admin/health,
// /admin/publish/state, /admin/verticals/installs, /admin/projects/current — may only be called through
// useProjectQuery. Each slice that wires one of them adds its lib/api.ts function name below, in the same commit.
import { describe, expect, it } from "vitest";
import { findStrayFetcherUse, listSources, ratchet, read } from "./studioGateScan";

/**
 * Names of lib/api.ts functions for the four token-scoped routes. Each slice that wires another adds its name.
 * getPublishState and listPackInstalls already exist and DraftBar calls them directly: that count is a ratchet ceiling
 * (test/studio-gates.allowlist.json, gate "tokenScoped") that may only go down as the call sites move into the hook.
 */
export const TOKEN_SCOPED_FETCHERS: readonly string[] = [
  "getPublishState", "listPackInstalls",
  // specs/079 f7-lich-su-quay-lai: the rollback plan is read by RollbackPlanConfirm through the hook.
  "getRollbackPlan",
  // specs/076 FR-110: the assistant card's before/after read of the draft rows (PublishReview's own reads are under the ceiling).
  // NOTE: its callers (readRows / planUndo in lib/aiApply.ts) are outside SCAN_DIRS; their guard is assertScope = useScopeGate (aiApply.draft.test.ts: scope false ⇒ 0 calls).
  "listDraftChanges",
  // specs/078 FR-002: the Vận hành reads that depend on the token tenant.
  "getTenantHealth", "getUsage", "listHookDeliveries", "listInboundHooks", "listApiKeys", "listRecentWorkflowRuns", "listBackgroundJobs", "listEmailLog",
  // specs/079 f7-lich-su-dong-thoi-gian: the publish history of the customer project.
  "listVersions", "getVersion",
];

describe("token-scoped fetchers are only called through useProjectQuery", () => {
  it("no listed fetcher is used outside the hook in app/studio or components/studio beyond the ceiling", () => {
    const counts: Record<string, number> = {};
    for (const f of listSources().filter((f) => !f.endsWith("useProjectQuery.ts"))) {
      const n = findStrayFetcherUse(read(f), TOKEN_SCOPED_FETCHERS).length;
      if (n > 0) counts[f] = n;
    }
    expect(ratchet("tokenScoped", counts)).toEqual([]);
  });

  // The gate is only worth something if it can fail: prove the scanner sees a stray call and lets a wrapped one through.
  it("self-check: a call outside the hook is caught, a call inside it is not", () => {
    const names = ["getHealth"];
    expect(findStrayFetcherUse("const h = await getHealth();", names)).toEqual(["getHealth"]);
    expect(findStrayFetcherUse("useQuery({ queryFn: getHealth })", names)).toEqual(["getHealth"]);
    expect(findStrayFetcherUse('useProjectQuery(slug, ["health"], () => getHealth())', names)).toEqual([]);
    expect(findStrayFetcherUse('useProjectQuery(slug, ["health"], getHealth)', names)).toEqual([]);
    expect(findStrayFetcherUse('import { getHealth } from "@/lib/api";\nuseProjectQuery(s, ["h"], getHealth);', names)).toEqual([]);
    expect(findStrayFetcherUse("// getHealth is called through the hook", names)).toEqual([]);
    expect(findStrayFetcherUse("obj.getHealth()", names)).toEqual([]);
    expect(findStrayFetcherUse("await api.getHealth()", names)).toEqual(["getHealth"]);
  });
});

// startPreviewSession (F5, "Xem như") is intentionally absent from TOKEN_SCOPED_FETCHERS: it is a write that
// authenticates with getBuilderToken() to mint the preview token. Listing it would force it through useProjectQuery,
// which reads with the project token, and break the call. Do not add it.
describe("F5 view-as", () => {
  it("startPreviewSession is not a token-scoped fetcher", () => {
    expect(TOKEN_SCOPED_FETCHERS).not.toContain("startPreviewSession");
  });
});
