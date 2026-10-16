"use client";

// ReportConfigPanel — admin-only report authoring folded INTO the app-face viewer
// (/reports/[id]) — kills the old /studio/reports split-brain. Every save invalidates the
// run query, so the result beside the panel re-renders LIVE — better than the retired Studio
// ReportBuilder where preview was save-gated on a separate screen.
//
// Rules mirrored 1:1 from the retired builder:
//   • tabular → config.columns ≥ 1 (lib/reportConfig, fallback = first field);
//   • summary/matrix → metric/field/groupBy + bucket ONLY on a date-kind groupBy
//     (lib/reportConfig), ONE join hop — FK-on-child or N-N — as "<rel>.<field>" tokens
//     (lib/reportJoin), with sum/avg DISABLED over an N-N hop (BE AGGREGATE_NN_FANOUT_UNSAFE);
//   • groupBy needs a physical index (BE AGGREGATE_REQUIRES_INDEXED) — relation/user/"Người phụ
//     trách" always have one; an unindexed field that can take one is pickable with an in-place
//     "Bật index ngay" (IndexFixNotice, Save blocked until then), one that cannot stays DISABLED
//     (lib/widgetAuthoring groupByChoices — specs/070 #265);
//   • fixed filters (every type, the list's condition builder) and the tabular sort are edited
//     here (specs/070 #266, lib/savedFilters); saved keys with no editor here (specs/049
//     subGroups/columnGroup/having) ride along on save — per type, never across an entity change
//     (lib/reportConfig `saved`). Pinning carries the fixed filters onto the widget.
// Pin-to-dashboard fixes the old builder's defect (it minted a NEW dashboard on every
// click): a picker APPENDS the widget to an existing dashboard (PUT full-replace with a
// fresh collision-free widget id); "Tạo bảng điều khiển mới" stays as the fallback.
//
// SLDS (specs/038 T030): the body of the viewer's docked panels/base — a stacked form
// (input · select · radio-button-group · checkbox), columns as dueling-picklist (order = column
// order), "qua <quan hệ>" as pills, errors as scoped-notifications, delete behind a prompt, the
// pin picker as a modal with a radio-group. Save/pin sit at the top of the body like the
// panels/filtering blueprint's header row, so they stay in view while the form scrolls.
import { useState, type ReactNode } from "react";
import Link from "next/link";
import { useAuth } from "@/lib/auth/AuthProvider";
import { studioHref } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Pin } from "lucide-react";
import {
  createDashboard, deleteReport, getAdminDashboard, getAdminReport, hasDraftChange, listAdminDashboards, listAllRelationships,
  updateDashboard, updateReport, type DashboardSummary, type ReportSummary,
} from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { qk } from "@/lib/query/keys";
import { buildReportConfig, isGridConfig, type GridAxis, type ReportType } from "@/lib/reportConfig";
import {
  buildJoinedField, isJoinedField, isMetricBlockedByNN, joinableRelationships, parseJoinedField,
  joinsPayload, relatedEntityOf, stripStaleJoinToken, withReportJoins,
} from "@/lib/reportJoin";
import { BUCKET_CHOICES, isBucketableGroupBy } from "@/lib/timeBuckets";
import {
  WIDGET_METRICS, groupByChoices, isIndexFixable, newWidgetId, normalizeWidgetBucket, tabularColumnChoices,
  valueFieldChoices, type GroupByChoice,
} from "@/lib/widgetAuthoring";
import { IndexFixNotice } from "./IndexFixNotice";
import { withOwnerFilter, type FilterRow } from "@/lib/filterModel";
import { filterRowIssues, filterRowsFromSaved, savedFromFilterRows } from "@/lib/savedFilters";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { useEntities, useFields } from "@/lib/metadata/hooks";
import { t, tf } from "@/lib/i18n";
import { useWorkshopScope } from "@/lib/workshopScope";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { DuelingPicklist } from "@/components/slds/DuelingPicklist";
import { Input } from "@/components/slds/Input";
import { Modal, Prompt } from "@/components/slds/Modal";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { Pill } from "@/components/slds/Pill";
import { RadioButtonGroup, RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";

const HAVING_OPS = ["gt", "gte", "lt", "lte", "eq", "neq", "between"] as const;
const SUB_SLOTS = 2; // BE: subGroups ≤ maxGridLevels-1
const axisKey = (a: GridAxis) => `${a.field}|${a.bucket ?? ""}`;
const asAxis = (v: unknown): GridAxis => {
  const o = (v && typeof v === "object" ? v : {}) as { field?: unknown; bucket?: unknown };
  return { field: typeof o.field === "string" ? o.field : "", bucket: typeof o.bucket === "string" ? o.bucket : "" };
};

const REPORT_TYPES: readonly ReportType[] = ["tabular", "summary", "matrix"];

// The form's source is the report ITSELF (GET /admin/reports/:id — the builder's own read overlays the
// draft), never the catalog row of the viewer: the catalog is a live-or-draft list that can be a minute
// stale, and a form seeded from it re-saved the old columns over the draft (specs/063 US3 #6).
export function ReportConfigPanel({ reportId, onSavedToDraft }: { reportId: string; onSavedToDraft?: () => void }) {
  const { data: meta, isError, error } = useQuery({
    queryKey: ["admin", "report", reportId],
    queryFn: () => getAdminReport(reportId),
    staleTime: 0,
    gcTime: 0, // a reopened panel must read the saved definition again
    retry: false,
  });
  // Error only while there is nothing to show: a failed background refetch must not unmount a form with unsaved edits.
  if (isError && !meta) {
    return <ScopedNotification theme="error">{error instanceof Error ? error.message : t("page.reports.config.saveError")}</ScopedNotification>;
  }
  if (!meta) return <div className="relative min-h-24"><Spinner size="small" /></div>;
  return <ReportConfigForm key={meta.id} reportId={reportId} meta={meta} onSavedToDraft={onSavedToDraft} />;
}

function ReportConfigForm({ reportId, meta, onSavedToDraft }: { reportId: string; meta: ReportSummary; onSavedToDraft?: () => void }) {
  const router = useRouter();
  const workshop = useWorkshopScope();
  const embedded = useEmbedded(); // inside the Studio frame nothing opens a dialog (D-UI-28)
  const isOperator = !!useAuth().user?.isPlatformOperator; // the way into Studio is for the builder only
  const qc = useQueryClient();

  // ── Editor state — snapshot of the saved definition at open time (parent remounts by id) ──
  const cfg = meta.config ?? {};
  const savedJoins = (cfg.joins as Array<{ relationship?: string; optional?: boolean }>) ?? [];
  const [name, setName] = useState(meta.name);
  const [entityName, setEntityName] = useState(meta.entityName ?? "");
  const [type, setType] = useState<ReportType>(
    meta.type === "summary" || meta.type === "matrix" ? meta.type : "tabular",
  );
  const [metric, setMetric] = useState((cfg.metric as string) || "count");
  const [field, setField] = useState((cfg.field as string) ?? "");
  const [groupBy, setGroupBy] = useState((cfg.groupBy as string) ?? "");
  const [bucket, setBucket] = useState(typeof cfg.bucket === "string" ? cfg.bucket : "");
  const [columns, setColumns] = useState<string[]>((cfg.columns as string[]) ?? []);
  const [joinRel, setJoinRel] = useState(savedJoins[0]?.relationship ?? "");
  const [joinOptional, setJoinOptional] = useState(savedJoins[0]?.optional === true);
  // Queue #293: an optional second hop that starts from the entity the first one reaches.
  const [join2Rel, setJoin2Rel] = useState(savedJoins[0]?.relationship ? (savedJoins[1]?.relationship ?? "") : "");
  const [join2Optional, setJoin2Optional] = useState(savedJoins[1]?.optional === true);
  // specs/070 #266: the fixed filters (every type) and the tabular sort are edited here.
  const [filterRows, setFilterRows] = useState<FilterRow[]>(() => filterRowsFromSaved(cfg.filters));
  const savedSort = cfg.sort as { field?: string; dir?: string } | undefined;
  const [sortField, setSortField] = useState(savedSort?.field ?? "");
  const [sortDir, setSortDir] = useState<"asc" | "desc">(savedSort?.dir === "asc" ? "asc" : "desc");

  // specs/049 F3: column axis (matrix), next group levels (summary), threshold on the leaf groups.
  const [colAxis, setColAxis] = useState<GridAxis>(() => asAxis(cfg.columnGroup));
  const [subAxes, setSubAxes] = useState<GridAxis[]>(() =>
    Array.from({ length: SUB_SLOTS }, (_, i) => asAxis(Array.isArray(cfg.subGroups) ? cfg.subGroups[i] : undefined)));
  const savedHaving = cfg.having as { on?: string; op?: string; values?: string[] } | undefined;
  const [hvOn, setHvOn] = useState<"value" | "count">(savedHaving?.on === "count" ? "count" : "value");
  const [hvOp, setHvOp] = useState(HAVING_OPS.some((o) => o === savedHaving?.op) ? (savedHaving?.op as string) : "gt");
  const [hv1, setHv1] = useState(savedHaving?.values?.[0] ?? "");
  const [hv2, setHv2] = useState(savedHaving?.values?.[1] ?? "");

  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);
  const [savedToDraft, setSavedToDraft] = useState(hasDraftChange(meta.draftState));
  const [delOpen, setDelOpen] = useState(false);
  const [delBusy, setDelBusy] = useState(false);

  // ── Metadata (long-TTL cached hooks) ──
  const { data: entities = [] } = useEntities();
  const { data: fields = [] } = useFields(entityName);
  const { data: allRels = [] } = useQuery({
    queryKey: ["metadata", "relationships", entityName],
    queryFn: () => listAllRelationships(entityName),
    enabled: !!entityName,
    staleTime: 10 * 60 * 1000,
  });
  const rels = joinableRelationships(allRels, entityName);
  const joinTargetRel = rels.find((r) => r.name === joinRel);
  const joinTarget = joinTargetRel ? relatedEntityOf(joinTargetRel, entityName) : null;
  const { data: joinFieldsRaw = [] } = useFields(joinTarget ?? "");
  // Hop 2: relationships of the entity hop 1 reaches (never hop 1's own relationship again).
  const { data: allRels2 = [] } = useQuery({
    queryKey: ["metadata", "relationships", joinTarget],
    queryFn: () => listAllRelationships(joinTarget ?? ""),
    enabled: !!joinTarget,
    staleTime: 10 * 60 * 1000,
  });
  const rels2 = joinTarget ? joinableRelationships(allRels2, joinTarget).filter((r) => r.name !== joinRel) : [];
  const join2TargetRel = joinRel ? rels2.find((r) => r.name === join2Rel) : undefined;
  const join2Target = join2TargetRel && joinTarget ? relatedEntityOf(join2TargetRel, joinTarget) : null;
  const { data: join2FieldsRaw = [] } = useFields(join2Target ?? "");
  // The chain as the engine sees it — hop 2 only exists on top of a hop 1.
  const hops = [
    { rel: joinRel, optional: joinOptional, relDef: joinTargetRel, entity: joinTarget, fieldsRaw: joinFieldsRaw },
    ...(joinRel && join2Rel ? [{ rel: join2Rel, optional: join2Optional, relDef: join2TargetRel, entity: join2Target, fieldsRaw: join2FieldsRaw }] : []),
  ].filter((h) => h.rel);
  const hopRels = hops.map((h) => h.rel);
  // Related-entity fields as "<rel>.<field>" picker tokens, through the SAME gates as the base
  // pickers: the metric picker offers only fields the engine can sum, the groupBy picker only
  // declared fields and with the physical-index gate (the joined axis resolves on the related
  // entity's allowlist — AGGREGATE_REQUIRES_INDEXED there too). One optgroup per hop.
  const joinedValueGroups = hops.map((h) => ({
    rel: h.rel,
    choices: valueFieldChoices(h.fieldsRaw).map((c) => ({ value: buildJoinedField(h.rel, c.value), label: c.label })),
  }));
  const joinedGroupGroups = hops.map((h) => ({
    rel: h.rel,
    choices: groupByChoices(h.fieldsRaw)
      .filter((c) => h.fieldsRaw.some((f) => f.name === c.value))
      .map((c) => ({ ...c, value: buildJoinedField(h.rel, c.value) })),
  }));
  const entityLabel = (n: string | null) => entities.find((e) => e.name === n)?.label ?? n ?? "";
  const nnLocked = joinTargetRel?.relationshipType === "N-N";
  const nn2Locked = join2TargetRel?.relationshipType === "N-N";
  // sum/avg are refused over an N-N hop in EITHER position (fan-out is symmetric).
  const metricBlocked = (m: string) => hops.some((h) => isMetricBlockedByNN(h.relDef, m));
  // The field (and its entity) behind an axis token — base, or "<rel>.<field>" on the join target —
  // for the in-place "Bật index ngay" notice. An axis still waiting for its index blocks Save: the
  // report would 422 AGGREGATE_REQUIRES_INDEXED on every run.
  const axisMeta = (token: string) => {
    const j = parseJoinedField(token);
    const hop = j ? hops.find((h) => h.rel === j.relationship) : undefined;
    return j
      ? { entity: hop?.entity ?? "", field: hop?.fieldsRaw.find((f) => f.name === j.field) }
      : { entity: entityName, field: fields.find((f) => f.name === token) };
  };
  const needsIndex = (token: string) => {
    const f = token ? axisMeta(token).field : undefined;
    return !!f && isIndexFixable(f);
  };
  // A sort field needs the same index (List → SORT_NOT_ALLOWED); a half-typed condition is flagged
  // on its row and blocks Save instead of being dropped from the saved report.
  const saveNeedsIndex = type === "tabular" ? needsIndex(sortField) : needsIndex(groupBy);
  const filterIssues = filterRowIssues(filterRows);
  // Same axis twice would be refused by the BE (REPORT_CONFIG_INVALID) — say it here instead.
  const gridUsed = (type === "matrix" ? [colAxis] : type === "summary" ? subAxes : []).filter((a) => a.field);
  const axisKeys = [axisKey({ field: groupBy, bucket: groupBy && bucket ? bucket : "" }), ...gridUsed.map(axisKey)];
  const dupAxis = type !== "tabular" && !!groupBy && new Set(axisKeys).size !== axisKeys.length;
  const gridNeedsIndex = gridUsed.some((a) => needsIndex(a.field));
  const hvIncomplete = type !== "tabular" && hv1.trim() !== "" && hvOp === "between" && hv2.trim() === "";
  const saveBlockedReason = dupAxis ? t("page.reports.config.axisDuplicate")
    : gridNeedsIndex ? t("page.reports.config.saveNeedsIndex")
    : hvIncomplete ? t("page.reports.config.havingIncomplete")
    : saveNeedsIndex
    ? t("page.reports.config.saveNeedsIndex")
    : Object.keys(filterIssues).length ? t("page.reports.config.saveFilterIncomplete") : null;
  const axisOption = (c: GroupByChoice) => ({
    value: c.value,
    label: c.indexed ? c.label : tf("page.reports.config.notIndexed", { label: c.label }),
    disabled: !c.indexed && !c.fixable,
  });

  // Tabular column options: every showable field, plus saved columns whose field is gone — the
  // admin must still see (and can remove) them instead of losing them silently.
  const columnOptions = tabularColumnChoices(fields);
  for (const c of columns) {
    if (!columnOptions.some((o) => o.value === c)) columnOptions.push({ value: c, label: c });
  }

  // Entity changed: joined + field-bound state is entity-scoped — reset it all (stale tokens
  // would 422 REPORT_JOIN_ENTITY_MISMATCH / RECORD_UNKNOWN_FIELD against the new base).
  function switchEntity(ename: string) {
    setEntityName(ename);
    setColumns([]); setGroupBy(""); setField(""); setBucket("");
    setFilterRows([]); setSortField("");
    setColAxis({ field: "" }); setSubAxes(subAxes.map(() => ({ field: "" }))); setHv1(""); setHv2("");
    setJoinRel(""); setJoinOptional(false); setJoin2Rel(""); setJoin2Optional(false);
  }

  // Join changed: keep base tokens, drop tokens namespaced onto a relationship no longer in the chain.
  function stripTokens(keep: string[]) {
    setGroupBy((g) => stripStaleJoinToken(g, keep));
    setField((f) => stripStaleJoinToken(f, keep));
    setColAxis((a) => ({ ...a, field: stripStaleJoinToken(a.field, keep) }));
    setSubAxes((xs) => xs.map((a) => ({ ...a, field: stripStaleJoinToken(a.field, keep) })));
  }

  // A disabled <option> that is ALREADY the value stays selected — greying sum/avg alone would
  // leave a config that saves straight into 422 AGGREGATE_NN_FANOUT_UNSAFE. count is the safe
  // fallback (always allowed over N-N).
  function switchJoinRel(relName: string) {
    setJoinRel(relName);
    // Hop 2 was picked among the OLD target's relationships — it no longer applies.
    setJoin2Rel(""); setJoin2Optional(false);
    stripTokens(relName ? [relName] : []);
    if (isMetricBlockedByNN(rels.find((r) => r.name === relName), metric)) setMetric("count");
    // Under N-N the base→link hop is INNER regardless of this flag (BE resolveReportJoin sets
    // Optional on hop 2 only), so carrying optional=true across a switch from a 1-N relationship
    // would save a config whose LEFT checkbox does nothing. Reset it with the relationship.
    if (rels.find((r) => r.name === relName)?.relationshipType === "N-N") setJoinOptional(false);
  }

  function switchJoin2Rel(relName: string) {
    setJoin2Rel(relName);
    stripTokens(relName ? [joinRel, relName] : [joinRel]);
    if (isMetricBlockedByNN(rels2.find((r) => r.name === relName), metric)) setMetric("count");
    if (rels2.find((r) => r.name === relName)?.relationshipType === "N-N") setJoin2Optional(false);
  }

  function buildCfg(): Record<string, unknown> {
    // Pure + tested (lib/reportConfig + lib/reportJoin): bucket ONLY for summary/matrix with a
    // date-kind groupBy, joins ONLY for summary/matrix with a selected relationship. PUT replaces
    // the whole config, so the saved keys this panel has no editor for (the specs/049
    // subGroups/columnGroup/having) ride along via `saved` — dropping them silently widened the
    // report ("hợp đồng hiệu lực" turned into every contract). filters + sort are edited here.
    return withReportJoins(
      buildReportConfig({
        type, columns, metric, field, groupBy, bucket, fields,
        fallbackColumn: fields[0]?.name ?? "id",
        saved: cfg, savedEntity: meta.entityName ?? "", entityName, joinRel, joinRels: hopRels,
        filters: savedFromFilterRows(filterRows, cfg.filters),
        sort: sortField ? { field: sortField, dir: sortDir } : null,
        columnGroup: colAxis.field ? colAxis : null,
        subGroups: subAxes.filter((a) => a.field),
        having: hv1.trim() ? { on: hvOn, op: hvOp, values: [hv1.trim(), hv2.trim()].slice(0, hvOp === "between" ? 2 : 1) } : null,
      }),
      type,
      // optional is forced false for N-N at the SAVE boundary too, not only in the UI: a config
      // loaded from an older report could carry optional=true on an N-N join, and persisting it
      // back would keep a flag the engine ignores alive in stored config forever.
      hops.map((h) => ({ relationship: h.rel, optional: h.relDef?.relationshipType === "N-N" ? false : h.optional })),
    );
  }

  async function handleSave() {
    setSaving(true); setSaveErr(null);
    try {
      const saved = await updateReport(reportId, {
        name: name.trim(), entityName, type, config: buildCfg(),
        isActive: meta.isActive !== false,
      });
      // A delivered workspace answers with draftState: the save went to the draft, not to the customer.
      const toDraft = hasDraftChange(saved?.draftState);
      setSavedToDraft(toDraft);
      // Refresh the catalog (viewer header reads it) + EVERY filter-variant of the run query
      // → the result re-renders live.
      await Promise.all([
        qc.invalidateQueries({ queryKey: qk.reports() }),
        qc.invalidateQueries({ queryKey: qk.reportRunAll(reportId) }),
      ]);
      toast.success(t(toDraft ? "draft.saved" : "page.reports.config.saved"));
      if (toDraft) {
        emitDraftsChanged("report"); // the draft bar recounts
        onSavedToDraft?.(); // the viewer switches to the draft so the result beside the panel shows this save
      }
    } catch (e) {
      setSaveErr(e instanceof Error ? e.message : t("page.reports.config.saveError"));
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete() {
    setDelBusy(true);
    try {
      await deleteReport(reportId);
      await qc.invalidateQueries({ queryKey: qk.reports() });
      toast.success(t("page.reports.config.deleted"));
      router.push(embedded ? studioHref(activeProjectSlug(), "dung/bao-cao") : workshop ? workshop.panelHref("bao-cao") : "/reports");
    } catch (e) {
      setDelBusy(false); setDelOpen(false);
      setSaveErr(e instanceof Error ? e.message : t("page.reports.config.deleteError"));
    }
  }

  // ── Pin to dashboard — picker (append to existing) + create-new fallback ──
  const [pinOpen, setPinOpen] = useState(false);
  const [pinTarget, setPinTarget] = useState("");
  const [pinBusy, setPinBusy] = useState<string | null>(null); // dashboard id | "new"
  const [pinErr, setPinErr] = useState<string | null>(null);
  const dashQ = useQuery({
    queryKey: ["admin", "dashboards"],
    queryFn: listAdminDashboards,
    enabled: pinOpen,
    staleTime: 30_000,
  });
  // A pinned card carries the report's joins (queue #294), so a "<rel>.<field>" token runs the same
  // engine and per-hop ACL as the report. A widget holds ONE axis: a column axis / extra levels / threshold would be
  // pinned as a silently different one-axis card. Refuse honestly (disabled + reason).
  // The card also takes groupBy + the fixed filters as they stand, so it waits for Save's gates:
  // an axis still without its index (the card would 422 AGGREGATE_REQUIRES_INDEXED on every run —
  // checked on groupBy for every type, since a tabular report's Save gate looks at the sort) and a
  // half-typed condition (savedFromFilterRows would drop it and the card would count wider).
  const pinBlockedReason = isGridConfig(buildCfg()) ? t("page.reports.config.pinBlockedGrid")
    : needsIndex(groupBy) ? t("page.reports.config.saveNeedsIndex") : saveBlockedReason;

  async function pinTo(target: DashboardSummary | null) {
    setPinBusy(target ? target.id : "new"); setPinErr(null);
    try {
      // normalizeWidgetBucket carries the report's time bucket onto the widget only when the
      // groupBy can hold it — same guard the dashboard save path uses.
      const widget = normalizeWidgetBucket(
        {
          id: newWidgetId(), title: name.trim() || meta.name, type: "bar", entity: entityName,
          metric, field: field || undefined, groupBy: groupBy || undefined, bucket: bucket || undefined,
          // The report's fixed filters go with it — a pinned card used to count every record.
          ...(savedFromFilterRows(filterRows, cfg.filters).length ? { filters: savedFromFilterRows(filterRows, cfg.filters) } : {}),
          // The report's relationship chain goes with it, or its "<rel>.<field>" axis would 422 on every run.
          ...(type !== "tabular" && hops.length ? { joins: joinsPayload(hops.map((h) => ({ relationship: h.rel, optional: h.relDef?.relationshipType === "N-N" ? false : h.optional }))) } : {}),
        },
        fields,
      );
      if (target) {
        // Append to the dashboard's CURRENT widgets, read fresh — not to the copy carried by the
        // catalog list. Two reasons, and either alone is enough: the open /dashboards catalog no
        // longer ships widget configs (a grantless employee could read the entity names and metrics
        // of tables they have no grant on from a discovery endpoint), and appending to a list-read
        // snapshot is a last-writer-wins wipe — anything added since the list loaded is silently
        // dropped.
        const fresh = await getAdminDashboard(target.id);
        await updateDashboard(
          fresh.id, fresh.name, [...(fresh.widgets ?? []), widget],
          fresh.description ?? undefined,
        );
        await Promise.all([
          qc.invalidateQueries({ queryKey: qk.dashboards() }),
          qc.invalidateQueries({ queryKey: qk.dashboardRunAll(target.id) }),
          qc.invalidateQueries({ queryKey: ["admin", "dashboards"] }),
        ]);
        toast.success(tf("page.reports.config.pinned", { name: target.name }));
      } else {
        await createDashboard(name.trim() || meta.name, [widget]);
        await Promise.all([
          qc.invalidateQueries({ queryKey: qk.dashboards() }),
          qc.invalidateQueries({ queryKey: ["admin", "dashboards"] }),
        ]);
        toast.success(t("page.reports.config.pinnedNew"));
      }
      setPinOpen(false);
    } catch (e) {
      setPinErr(e instanceof Error ? e.message : t("page.reports.config.pinError"));
    } finally {
      setPinBusy(null);
    }
  }

  const dashboards = dashQ.data ?? [];
  const chosen = dashboards.find((d) => d.id === pinTarget) ?? null;
  const pinFooter = (
<>
            <Button onClick={() => void pinTo(null)} disabled={!!pinBusy}>
              {pinBusy === "new" ? t("page.reports.config.pinning") : t("page.reports.config.pinNew")}
            </Button>
            <Button onClick={() => setPinOpen(false)} disabled={!!pinBusy}>{t("action.close")}</Button>
            <Button variant="brand" onClick={() => chosen && void pinTo(chosen)} disabled={!chosen || !!pinBusy}>
              {pinBusy && pinBusy !== "new" ? t("page.reports.config.pinning") : t("page.reports.config.pinSubmit")}
            </Button>
          </>
  );
  const pinBody = (
        <div className="flex flex-col gap-3">
          {dashQ.isLoading && (
            <div className="relative h-16">
              <Spinner size="small" label={t("page.reports.config.pinLoading")} />
            </div>
          )}
          {dashQ.isError && (
            <ScopedNotification theme="error">
              {dashQ.error instanceof Error ? dashQ.error.message : t("page.reports.config.pinLoadError")}
            </ScopedNotification>
          )}
          {!dashQ.isLoading && !dashQ.isError && dashboards.length === 0 && (
            <p className="slds-text-color_weak">{t("page.reports.config.pinEmpty")}</p>
          )}
          {dashboards.length > 0 && (
            <RadioGroup
              label={t("page.reports.config.pinList")}
              value={pinTarget}
              onValueChange={setPinTarget}
              disabled={!!pinBusy}
              options={dashboards.map((d) => ({
                value: d.id,
                label: `${d.name} · ${tf("page.reports.config.pinWidgets", { n: (d.widgets ?? []).length })}`,
              }))}
            />
          )}
          {pinErr && <ScopedNotification theme="error">{pinErr}</ScopedNotification>}
        </div>
  );
  const viaPill = (token: string) =>
    isJoinedField(token) && (
      <div className="pt-1">
        <Pill label={tf("page.reports.config.viaPill", { rel: parseJoinedField(token)?.relationship ?? "" })} />
      </div>
    );

  return (
    <div className="flex flex-col gap-4">
      <p className="slds-text-body_small slds-text-color_weak">{t("page.reports.config.hint")}</p>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="brand"
          onClick={handleSave}
          disabled={saving || !name.trim() || !entityName || !!saveBlockedReason}
          title={saveBlockedReason ?? undefined}
        >
          {saving ? t("page.reports.config.saving") : t("page.reports.config.save")}
        </Button>
        <Button
          iconLeft={Pin}
          onClick={() => { setPinTarget(""); setPinErr(null); setPinOpen(true); }}
          disabled={!!pinBlockedReason}
          title={pinBlockedReason ?? undefined}
        >
          {t("page.reports.config.pin")}
        </Button>
      </div>
      {savedToDraft && <ScopedNotification>{t("draft.saved")}</ScopedNotification>}
      {pinBlockedReason && <p className="slds-text-body_small slds-text-color_weak">{pinBlockedReason}</p>}

      {saveErr && <ScopedNotification theme="error">{saveErr}</ScopedNotification>}

      <Input label={t("page.reports.config.name")} required value={name} onChange={(e) => setName(e.target.value)} />
      <Select
        label={t("page.reports.config.entity")}
        value={entityName}
        onChange={(e) => switchEntity(e.target.value)}
        placeholder={false}
        options={[
          // Keep a stale/out-of-list value visible instead of silently snapping elsewhere.
          ...(entityName && !entities.some((e) => e.name === entityName) ? [{ value: entityName, label: entityName }] : []),
          ...entities.map((e) => ({ value: e.name, label: e.label })),
        ]}
      />
      <RadioButtonGroup
        label={t("page.reports.config.type")}
        className="[&_.slds-radio\_faux]:whitespace-nowrap [&_.slds-radio\_faux]:px-2"
        value={type}
        onValueChange={(v) => setType(v as ReportType)}
        options={REPORT_TYPES.map((k) => ({ value: k, label: t(`page.reports.type.${k}` as I18nKey) }))}
      />

      {type === "tabular" ? (
        <div>
          {/* REPORT-RELATION-LABELS: relation/user fields are offered as columns — the run
              resolves them to labels ("Khách hàng" reads "Công ty ABC"). */}
          <DuelingPicklist
            label={t("page.reports.config.columns")}
            sourceLabel={t("page.reports.config.columnsSource")}
            selectedLabel={t("page.reports.config.columnsSelected")}
            options={columnOptions}
            value={columns}
            onChange={setColumns}
            responsive
          />
          {columns.length === 0 && (
            <p className="slds-text-body_small slds-text-color_weak pt-1">{t("page.reports.config.columnsEmpty")}</p>
          )}
          {/* specs/070 #266: sort = the List sort allowlist, the same set as "Nhóm theo". */}
          <div className="pt-3">
            <Select
              label={t("page.reports.config.sort")}
              value={sortField}
              onChange={(e) => setSortField(e.target.value)}
              placeholder={t("page.reports.config.sortDefault")}
              options={groupByChoices(fields, sortField || undefined).map(axisOption)}
            />
            {sortField && <IndexFixNotice {...axisMeta(sortField)} />}
          </div>
          {sortField && (
            <div className="pt-2">
              <RadioButtonGroup
                label={t("page.reports.config.sortDir")}
                value={sortDir}
                onValueChange={(v) => setSortDir(v === "asc" ? "asc" : "desc")}
                options={[
                  { value: "desc", label: t("page.reports.config.sortDesc") },
                  { value: "asc", label: t("page.reports.config.sortAsc") },
                ]}
              />
            </div>
          )}
        </div>
      ) : (
        <>
          {/* REPORT-JOIN — one hop (FK-on-child or N-N); fields of the related entity appear in
              the groupBy/metric pickers as "<rel>.<field>". Hidden for tabular (BE rejects). */}
          {rels.length > 0 && (
            <div className="flex flex-col gap-2">
              <Select
                label={t("page.reports.config.join")}
                value={joinRel}
                onChange={(e) => switchJoinRel(e.target.value)}
                placeholder={t("page.reports.config.joinNone")}
                options={rels.map((r) => ({ value: r.name, label: `${r.name} → ${entityLabel(relatedEntityOf(r, entityName))}` }))}
                // N-N carries two truths the FK-on-child hop doesn't: sum/avg is refused, and the
                // base→link hop is INNER even when optional=true — say both.
                help={t(nnLocked ? "page.reports.config.joinNN" : "page.reports.config.joinHelp")}
              />
              {/* Disabled, not merely explained: under N-N this flag CANNOT do what its label says. */}
              {joinRel && (
                <Checkbox
                  label={t("page.reports.config.joinLeft")}
                  checked={joinOptional && !nnLocked}
                  disabled={nnLocked}
                  onChange={(e) => setJoinOptional(e.target.checked)}
                />
              )}
              {/* Queue #293: a second step from the entity the first one reaches (Hoá đơn → Lịch hẹn → Bác sĩ). */}
              {joinRel && rels2.length > 0 && (
                <>
                  <Select
                    label={t("page.reports.config.join2")}
                    value={join2Rel}
                    onChange={(e) => switchJoin2Rel(e.target.value)}
                    placeholder={t("page.reports.config.join2None")}
                    options={rels2.map((r) => ({ value: r.name, label: `${r.name} → ${entityLabel(relatedEntityOf(r, joinTarget ?? ""))}` }))}
                    help={t(nn2Locked ? "page.reports.config.joinNN" : "page.reports.config.join2Help")}
                  />
                  {join2Rel && (
                    <Checkbox
                      label={t("page.reports.config.joinLeft")}
                      checked={join2Optional && !nn2Locked}
                      disabled={nn2Locked}
                      onChange={(e) => setJoin2Optional(e.target.checked)}
                    />
                  )}
                </>
              )}
            </div>
          )}
          <div>
            <Select
              label={t("page.reports.config.groupBy")}
              value={groupBy}
              onChange={(e) => setGroupBy(e.target.value)}
              placeholder={t("page.reports.config.groupByNone")}
              // BE gate (AGGREGATE_REQUIRES_INDEXED): an unindexed field that can take an index is
              // pickable and gets "Bật index ngay" below; one that cannot stays DISABLED (dead end).
              options={groupByChoices(fields, isJoinedField(groupBy) ? undefined : groupBy || undefined).map(axisOption)}
              help={
                <>
                  {t("page.reports.config.groupByHelp")}{" "}
                  {isOperator && (
                    <Link href={studioHref(activeProjectSlug(), "dung/du-lieu/:bang", { bang: entityName })}>
                      {t("page.reports.config.groupByIndexLink")}
                    </Link>
                  )}
                </>
              }
            >
              {joinedGroupGroups.filter((g) => g.choices.length > 0).map((g) => (
                <optgroup key={g.rel} label={tf("page.reports.config.via", { rel: g.rel })}>
                  {g.choices.map(axisOption).map((o) => (
                    <option key={o.value} value={o.value} disabled={o.disabled}>{o.label}</option>
                  ))}
                </optgroup>
              ))}
            </Select>
            {viaPill(groupBy)}
            {groupBy && <IndexFixNotice {...axisMeta(groupBy)} />}
          </div>
          {/* W4 bucket — ONLY on a date-kind groupBy (else AGGREGATE_BUCKET_FIELD_NOT_DATE). */}
          {isBucketableGroupBy(groupBy, fields) && (
            <Select
              label={t("page.reports.config.bucket")}
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
              placeholder={false}
              options={BUCKET_CHOICES.map((b) => ({ value: b.value, label: b.label }))}
              help={t("page.reports.config.bucketHelp")}
            />
          )}
          {/* Same idiom as the groupBy index gate: an option the BE would refuse renders DISABLED
              with its reason — here AGGREGATE_NN_FANOUT_UNSAFE (sum/avg double-count over N-N). */}
          <Select
            label={t("page.reports.config.metric")}
            value={metric}
            onChange={(e) => setMetric(e.target.value)}
            placeholder={false}
            options={WIDGET_METRICS.map((m) => {
              const label = t(`page.reports.config.metric.${m}` as I18nKey);
              const blocked = metricBlocked(m);
              return { value: m, label: blocked ? tf("page.reports.config.metricNN", { label }) : label, disabled: blocked };
            })}
          />
          <div>
            <Select
              label={t("page.reports.config.field")}
              value={field}
              onChange={(e) => setField(e.target.value)}
              placeholder={t("page.reports.config.fieldAll")}
              options={valueFieldChoices(fields, isJoinedField(field) ? undefined : field || undefined)}
            >
              {joinedValueGroups.filter((g) => g.choices.length > 0).map((g) => (
                <optgroup key={g.rel} label={tf("page.reports.config.via", { rel: g.rel })}>
                  {g.choices.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
                </optgroup>
              ))}
            </Select>
            {viaPill(field)}
          </div>
        </>
      )}

      {type !== "tabular" && (
        <GridAuthoring
          type={type}
          groupBy={groupBy}
          colAxis={colAxis} setColAxis={setColAxis}
          subAxes={subAxes} setSubAxes={setSubAxes}
          axisOptions={(cur) => groupByChoices(fields, cur || undefined).map(axisOption)}
          joinedGroups={joinedGroupGroups.map((g) => ({ rel: g.rel, options: g.choices.map(axisOption) }))}
          bucketable={(f) => !!f && isBucketableGroupBy(f, fields)}
          axisNotice={(f) => <IndexFixNotice {...axisMeta(f)} />}
          dupAxis={dupAxis}
          levelLabel={fields.find((f) => f.name === ([...subAxes].reverse().find((a) => a.field)?.field || groupBy))?.label ?? ""}
          hvOn={hvOn} setHvOn={setHvOn} hvOp={hvOp} setHvOp={setHvOp}
          hv1={hv1} setHv1={setHv1} hv2={hv2} setHv2={setHv2}
          hvIncomplete={hvIncomplete}
        />
      )}

      {/* specs/070 #266: fixed filters — saved with the report, applied on every run for every
          viewer (base entity only: REPORT_JOIN_FILTER_UNSUPPORTED). The list's condition builder,
          AND-only, relative dates resolved by the server at run time. */}
      {/* Narrow column (studio right rail, 300px): field / operator / value wrap to one per line. */}
      <div className="@container">
        <div className="@max-sm:[&_li>fieldset>.slds-grid]:flex-wrap! @max-sm:[&_li>fieldset>.slds-grid>.slds-col]:basis-full!">
          <ExpressionBuilder
            title={t("page.reports.config.filters")}
            fields={withOwnerFilter(fields)}
            value={filterRows}
            onChange={setFilterRows}
            errors={filterIssues}
          />
          <p className="slds-text-body_small slds-text-color_weak pt-1">{t("page.reports.config.filtersHelp")}</p>
        </div>
      </div>

      <div className="slds-border_top pt-3">
        <Button variant="text-destructive" onClick={() => setDelOpen(true)}>
          {t("page.reports.config.delete")}
        </Button>
      </div>

      {/* Delete confirm — in place inside the Studio frame, a dialog on the customer's screen */}
      {embedded ? (
        delOpen && (
          <InlineConfirm
            tone="error"
            message={tf("page.reports.config.deleteBody", { name: meta.name })}
            confirmLabel={delBusy ? t("page.reports.config.deleting") : t("page.reports.config.delete")}
            onConfirm={() => void handleDelete()}
            onCancel={() => setDelOpen(false)}
            busy={delBusy}
          />
        )
      ) : (
      <Prompt
        open={delOpen}
        onOpenChange={(o) => { if (!delBusy) setDelOpen(o); }}
        theme="error"
        title={t("page.reports.config.deleteTitle")}
        footer={
          <>
            <Button onClick={() => setDelOpen(false)} disabled={delBusy}>{t("action.cancel")}</Button>
            <Button variant="destructive" onClick={() => void handleDelete()} disabled={delBusy}>
              {delBusy ? t("page.reports.config.deleting") : t("page.reports.config.delete")}
            </Button>
          </>
        }
      >
        <p>{tf("page.reports.config.deleteBody", { name: meta.name })}</p>
      </Prompt>
      )}

      {/* Pin picker — append to an existing dashboard, or create a new one (in place inside the Studio frame) */}
      {embedded ? (
        pinOpen && (
          <section aria-label={t("page.reports.config.pin")} className="slds-border_top pt-3">
            <p className="slds-text-heading_small">{t("page.reports.config.pin")}</p>
            <p className="slds-text-body_small slds-text-color_weak pb-2">{t("page.reports.config.pinTagline")}</p>
            {pinBody}
            <div className="slds-grid slds-grid_align-end flex-wrap gap-2 pt-3">{pinFooter}</div>
          </section>
        )
      ) : (
        <Modal
          open={pinOpen}
          onOpenChange={(o) => { if (!pinBusy) setPinOpen(o); }}
          title={t("page.reports.config.pin")}
          tagline={t("page.reports.config.pinTagline")}
          directionalFooter
          footer={pinFooter}
        >
          {pinBody}
        </Modal>
      )}
    </div>
  );
}

type Opt = { value: string; label: string; disabled?: boolean };

// specs/049 F3 — the grid keys of a summary (next levels) / matrix (column axis) and the threshold.
// Wording follows the viewer (FR-012): the threshold names WHAT it filters — cells (matrix) or the
// groups of the deepest level (summary) — never the row totals.
function GridAuthoring(p: {
  type: ReportType; groupBy: string;
  colAxis: GridAxis; setColAxis: (a: GridAxis) => void;
  subAxes: GridAxis[]; setSubAxes: (a: GridAxis[]) => void;
  axisOptions: (cur: string) => Opt[];
  joinedGroups: Array<{ rel: string; options: Opt[] }>;
  bucketable: (f: string) => boolean;
  axisNotice: (f: string) => ReactNode;
  dupAxis: boolean; levelLabel: string;
  hvOn: "value" | "count"; setHvOn: (v: "value" | "count") => void;
  hvOp: string; setHvOp: (v: string) => void;
  hv1: string; setHv1: (v: string) => void; hv2: string; setHv2: (v: string) => void;
  hvIncomplete: boolean;
}) {
  const axisRow = (a: GridAxis, set: (a: GridAxis) => void, label: string, none: string, help?: string) => (
    <div className="flex flex-col gap-2">
      <Select
        label={label}
        value={a.field}
        onChange={(e) => set({ field: e.target.value, bucket: "" })}
        placeholder={none}
        options={p.axisOptions(a.field)}
        help={help}
      >
        {p.joinedGroups.filter((g) => g.options.length > 0).map((g) => (
          <optgroup key={g.rel} label={tf("page.reports.config.via", { rel: g.rel })}>
            {g.options.map((o) => <option key={o.value} value={o.value} disabled={o.disabled}>{o.label}</option>)}
          </optgroup>
        ))}
      </Select>
      {a.field && p.axisNotice(a.field)}
      {p.bucketable(a.field) && (
        <Select
          label={t("page.reports.config.axisBucket")}
          value={a.bucket ?? ""}
          onChange={(e) => set({ ...a, bucket: e.target.value })}
          placeholder={false}
          options={BUCKET_CHOICES.map((b) => ({ value: b.value, label: b.label }))}
        />
      )}
    </div>
  );
  const matrix = p.type === "matrix";
  const hvLead = matrix
    ? t("page.reports.config.havingCell")
    : tf("page.reports.config.havingGroup", { level: p.levelLabel || t("page.reports.config.havingDeepest") });
  return (
    <div className="flex flex-col gap-3 slds-border_top pt-3">
      {matrix ? (
        <>
          {axisRow(p.colAxis, p.setColAxis, t("page.reports.config.colAxis"), t("page.reports.config.colAxisNone"), t("page.reports.config.colAxisHelp"))}
          {!p.colAxis.field && <p className="slds-text-body_small slds-text-color_weak">{t("page.reports.config.colAxisHint")}</p>}
        </>
      ) : p.groupBy ? (
        p.subAxes.map((a, i) => (i === 0 || p.subAxes[i - 1].field ? (
          <div key={i}>
            {axisRow(a, (n) => p.setSubAxes(p.subAxes.map((x, j) => (j === i ? n : x))),
              t(i === 0 ? "page.reports.config.nextGroup" : "page.reports.config.nextGroup2"), t("page.reports.config.nextGroupNone"))}
          </div>
        ) : null))
      ) : (
        <p className="slds-text-body_small slds-text-color_weak">{t("page.reports.config.nextGroupNeedsFirst")}</p>
      )}
      {p.dupAxis && <ScopedNotification theme="error">{t("page.reports.config.axisDuplicate")}</ScopedNotification>}

      <fieldset className="flex flex-col gap-2">
        <legend className="slds-form-element__legend slds-form-element__label">{hvLead}</legend>
        <div className="flex flex-wrap items-end gap-2">
          <Select
            label={t("page.reports.config.havingOn")}
            value={p.hvOn}
            onChange={(e) => p.setHvOn(e.target.value === "count" ? "count" : "value")}
            placeholder={false}
            options={[
              { value: "value", label: t("page.reports.viewer.having.on.value") },
              { value: "count", label: t("page.reports.viewer.having.on.count") },
            ]}
          />
          <Select
            label={t("page.reports.config.havingOp")}
            value={p.hvOp}
            onChange={(e) => p.setHvOp(e.target.value)}
            placeholder={false}
            options={HAVING_OPS.map((o) => ({ value: o, label: t(`page.reports.config.havingOp.${o}` as I18nKey) }))}
          />
          <Input label={t("page.reports.config.havingValue")} type="number" value={p.hv1} onChange={(e) => p.setHv1(e.target.value)} />
          {p.hvOp === "between" && (
            <Input label={t("page.reports.config.havingValue2")} type="number" value={p.hv2} onChange={(e) => p.setHv2(e.target.value)} />
          )}
        </div>
        <p className="slds-text-body_small slds-text-color_weak">
          {p.hvIncomplete ? t("page.reports.config.havingIncomplete") : t("page.reports.config.havingHelp")}
        </p>
      </fieldset>
    </div>
  );
}
