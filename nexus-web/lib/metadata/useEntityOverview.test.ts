// useEntityOverview (specs/074 FR-010, T011): one row per table built from four existing sources, nothing stored.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AdminRelationship, AppSummary, EntityMeta, FieldMeta, WorkflowSummary } from "@/lib/api";

const listFields = vi.fn();
const listAllRelationships = vi.fn();
const listWorkflows = vi.fn();
const appNav = vi.fn();
const listRecords = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listFields: (...a: unknown[]) => listFields(...a),
  listAllRelationships: (...a: unknown[]) => listAllRelationships(...a),
  listWorkflows: (...a: unknown[]) => listWorkflows(...a),
  appNav: (...a: unknown[]) => appNav(...a),
  listRecords: (...a: unknown[]) => listRecords(...a),
}));

import { ENTITY_OVERVIEW_CAP, buildEntityOverview, useEntityOverview } from "./useEntityOverview";

const ent = (name: string): EntityMeta => ({ id: `id-${name}`, name, label: name.toUpperCase(), tableName: `obj_${name}` });
const app = (name: string): AppSummary => ({ id: `id-${name}`, name, label: name.toUpperCase(), icon: null, color: null, isDefault: false, sortOrder: 0 });
const rel = (child: string, parent: string, type = "1-N"): AdminRelationship => ({
  id: `${child}-${parent}`, name: `${child}_${parent}`, parentEntityName: parent, childEntityName: child, relationshipType: type,
});
const fields = (n: number) => Array.from({ length: n }, (_, i) => ({ name: `f${i}` }) as FieldMeta);

describe("buildEntityOverview (pure)", () => {
  const base = { entities: [ent("a"), ent("b"), ent("c")], fieldCounts: { a: 3, b: 0 }, relationships: [] as AdminRelationship[], workflows: [] as WorkflowSummary[], navByApp: [] as { app: AppSummary; names: Set<string> }[] };

  it("fieldCount is null for a table whose fields were not asked for, 0 stays 0", () => {
    const rows = buildEntityOverview(base);
    expect(rows.map((r) => r.fieldCount)).toEqual([3, 0, null]);
  });

  it("groups links by the SOURCE table: the child that holds the relation field points at its parent; N-N points both ways", () => {
    const rows = buildEntityOverview({ ...base, relationships: [rel("b", "a"), rel("c", "a"), rel("b", "c", "N-N")] });
    const by = Object.fromEntries(rows.map((r) => [r.name, r.linksTo]));
    expect(by.a).toEqual([]); // a is only ever a parent in 1-N: the arrow starts at the table holding the field
    expect(by.b).toEqual(["a", "c"]);
    expect(by.c).toEqual(["a", "b"]);
  });

  it("counts rules by entityName; a rule with no entity counts for nobody", () => {
    const wf = (id: string, entityName?: string) => ({ id, name: id, entityName }) as WorkflowSummary;
    const rows = buildEntityOverview({ ...base, workflows: [wf("1", "a"), wf("2", "a"), wf("3", "c"), wf("4")] });
    expect(rows.map((r) => r.ruleCount)).toEqual([2, 0, 1]);
  });

  it("inverts the app menus into table -> apps", () => {
    const rows = buildEntityOverview({
      ...base,
      navByApp: [{ app: app("crm"), names: new Set(["a", "b"]) }, { app: app("tk"), names: new Set(["b"]) }],
    });
    expect(rows.map((r) => r.apps.map((x) => x.name))).toEqual([["crm"], ["crm", "tk"], []]);
  });
});

describe("useEntityOverview", () => {
  const wrapper = () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const W = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: qc }, children);
    return W;
  };
  beforeEach(() => {
    for (const m of [listFields, listAllRelationships, listWorkflows, appNav, listRecords]) m.mockReset();
    listFields.mockImplementation(async () => fields(2));
    listAllRelationships.mockResolvedValue([]);
    listWorkflows.mockResolvedValue([]);
    appNav.mockImplementation(async (n: string) => ({ app: app(n), items: [{ type: "entity", refId: "x", label: "x", href: "/t0" }] }));
  });

  it(`asks fields for at most ${ENTITY_OVERVIEW_CAP} tables: the next one has fieldCount null; and never counts records`, async () => {
    const entities = Array.from({ length: ENTITY_OVERVIEW_CAP + 1 }, (_, i) => ent(`t${i}`));
    const { result } = renderHook(() => useEntityOverview(entities, [app("crm")], "northwind"), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.rows[0].fieldCount).toBe(2));
    await waitFor(() => expect(result.current.loading.fields).toBe(false));
    expect(listFields).toHaveBeenCalledTimes(ENTITY_OVERVIEW_CAP);
    expect(result.current.rows[ENTITY_OVERVIEW_CAP].fieldCount).toBeNull();
    expect(result.current.rows[ENTITY_OVERVIEW_CAP - 1].fieldCount).toBe(2);
    expect(listRecords).not.toHaveBeenCalled();
    expect(listAllRelationships).toHaveBeenCalledTimes(1);
    expect(listWorkflows).toHaveBeenCalledTimes(1);
    expect(appNav).toHaveBeenCalledTimes(1);
  });

  it("reports each source as loading until it lands, and a failed source as error (not as zero)", async () => {
    listWorkflows.mockRejectedValue(new Error("x"));
    const { result } = renderHook(() => useEntityOverview([ent("t0")], [app("crm")], "northwind"), { wrapper: wrapper() });
    expect(result.current.loading.rules).toBe(true);
    await waitFor(() => expect(result.current.error.rules).toBe(true));
    expect(result.current.rows[0].ruleCount).toBeNull();
    await waitFor(() => expect(result.current.rows[0].apps.map((a) => a.name)).toEqual(["crm"]));
  });
});
