"use client";

// W7 §11.2 — Audit Log Viewer v2 (admin).
// Filter bar: actor (Combobox /admin/users) · entity · action · date range (DateRangePicker →
// dateFrom/dateTo — NEVER mixed with from/to). Rows via GET /audit-log with cursor pagination.
// "Xuất CSV" hits /audit-log/export.csv with the SAME active filters (one buildAuditQuery source).
// Diff is expandable (diff may be null — the NULL-scan 500 is fixed backend-side).
//
// SLDS (specs/038 T048, RESEARCH §5 /admin/audit): the ObjectHomePage template (page-headers/
// object-home in a card; "Xuất CSV" as the page action, refresh as a list control); under it the
// filter row — combobox autocomplete (actor, entity), combobox select (action), the datepickers/range
// popover; the log is a data-tables/base table whose first column is the summary-detail toggle
// (switch glyph, aria-expanded) opening a full-width row with the field diff (data-tables/base).
// Not the DataTable component: it has no expandable row, and client-side sort over one cursor page
// of a server-ordered log would mislead.

import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { clsx } from "clsx";
import { ChevronDown, FilterX, History, RefreshCw, UserRound } from "lucide-react";
import {
  listAuditFiltered, downloadAuditCsv, buildAuditQuery, ensureAuth, ApiError,
  type AuditEntry, type AuditFilter,
} from "@/lib/admin";
import { listAdminUsers, listEntities, type AdminUser, type EntityMeta } from "@/lib/api";
import { ENGINE_ACTORS, META_ENTITY_NOUNS, metaVerb, systemActorLabel } from "@/lib/auditLabels";
import { formatVNDCompact } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { DateRangePicker } from "@/components/analytics/DateRangePicker";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Combobox } from "@/components/slds/Combobox";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

// System actors written by the engine for metadata / governance changes — friendly labels.
// One shared source (lib/auditLabels — DP-1.1) so StudioHome feed + this viewer agree.
const SYSTEM_ACTORS: Record<string, string> = Object.fromEntries(
  Object.keys(META_ENTITY_NOUNS).map((k) => [k, systemActorLabel(k)]),
);
// Synthetic entity names that appear in the trail as `entityName`.
const SYSTEM_ENTITIES = Object.keys(SYSTEM_ACTORS);

// internal/audit/audit.go canonical actions (restore = recycle-bin undelete; neutral badge).
const ACTIONS = ["create", "update", "delete", "restore"] as const;
const ACTION_THEME: Record<string, "success" | "warning" | "error"> = {
  create: "success",
  update: "warning",
  delete: "error",
};
// "download" (specs/068: someone took the customer's backup file) is named too, but is not a filter
// option: the filter list stays the four record actions the server's `action` filter documents.
const actionLabel = (a: string) =>
  (ACTIONS as readonly string[]).includes(a) || a === "download" ? t(`page.audit.action.${a}` as I18nKey) : a;

const PAGE_LIMIT = 50;
const COLS = 6;

function fmtAt(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getDate())}/${p(d.getMonth() + 1)}/${d.getFullYear()} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
function shortId(id: string | null | undefined): string {
  return id ? id.replace(/-/g, "").slice(0, 8) : "—";
}
function renderValue(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "object" && !Array.isArray(v) && "amount" in (v as object)) {
    return formatVNDCompact((v as { amount: number }).amount);
  }
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
// A role update carries the role's name on both sides so the row names the role (#66); drawn, an unchanged name
// read as a rename, so that one key is dropped (issue #67). Only that one: elsewhere the same value on both
// sides IS a change, e.g. a sensitive field is written "[redacted]" on both sides on purpose (audit_redact.go).
function buildDiffRows(entry: AuditEntry) {
  const before = entry.diff?.before ?? {};
  const after = entry.diff?.after ?? {};
  const paddedName = entry.entityName === ROLE_META && before.name === after.name;
  return Array.from(new Set([...Object.keys(before), ...Object.keys(after)]))
    .filter((k) => !(paddedName && k === "name"))
    .map((k) => ({ field: k, before: renderValue(before[k]), after: renderValue(after[k]) }));
}

const SESSION_META = "__workspace_session__";
const LAYOUT_META = "__layout__";
const ROLE_META = "__role__";

/** The action word of a row: a workspace session is "Mở"/"Đóng" (D-9 reuses create/delete), the rest by action. */
function rowVerb(entry: AuditEntry): string {
  if (entry.entityName === SESSION_META && entry.action === "create") return t("audit.session.open");
  if (entry.entityName === SESSION_META && entry.action === "delete") return t("audit.session.close");
  return metaVerb(entry.entityName, entry.action) ?? actionLabel(entry.action);
}

/** "Kết thúc chậm nhất lúc ..." for a session-open row, read from that row's own diff (FR-020). */
function sessionUntil(entry: AuditEntry): string | undefined {
  if (entry.entityName !== SESSION_META || entry.action !== "create") return undefined;
  const at = entry.diff?.after?.expiresAt;
  return typeof at === "string" && at ? tf("audit.session.until", { at: fmtAt(at) }) : undefined;
}

/** "<table label> · <screen>" for a layout row: its diff is keyed "<entityName>/<viewType>". */
function layoutTarget(entry: AuditEntry, tableLabel: (entityName: string) => string): string | undefined {
  if (entry.entityName !== LAYOUT_META) return undefined;
  const key = Object.keys(entry.diff?.after ?? entry.diff?.before ?? {})[0];
  const [entity, view] = key?.split("/") ?? [];
  if (!entity) return undefined;
  const viewKey = `audit.layout.view.${view}` as I18nKey;
  return [tableLabel(entity), view ? t(viewKey) : ""].filter(Boolean).join(" · ");
}

/** A role row's record cell names the role (issue #67): its name is in the diff, on both sides of an update, but an
 * unchanged name is no longer drawn there. */
function roleTarget(entry: AuditEntry): string | undefined {
  if (entry.entityName !== ROLE_META) return undefined;
  const name = entry.diff?.after?.name ?? entry.diff?.before?.name;
  return typeof name === "string" && name ? name : undefined;
}

/** The loaded list for one request key (filter + refresh tick). */
interface LoadedPage {
  key: string;
  entries: AuditEntry[];
  nextCursor?: string;
  err: string | null;
}

/** `initialActorId`: the user the page opens filtered to (Studio `?nguoi=`); the filter stays editable and `onActorChange` lets the page write it back to the URL. */
export function AuditLog({ initialActorId, onActorChange }: { initialActorId?: string; onActorChange?: (id: string) => void } = {}) {
  const embedded = useEmbedded();
  const [page, setPage] = useState<LoadedPage | null>(null);
  const [tick, setTick] = useState(0);
  const [loadingMore, setLoadingMore] = useState(false);
  const [exporting, setExporting] = useState(false);

  const [users, setUsers] = useState<AdminUser[]>([]);
  const [entities, setEntities] = useState<EntityMeta[]>([]);
  const actorNames = useMemo(() => Object.fromEntries(users.map((u) => [u.id, u.fullName || u.email])), [users]);
  const entityLabel = useCallback(
    (name: string) => entities.find((e) => e.name === name)?.label ?? SYSTEM_ACTORS[name] ?? name,
    [entities],
  );

  // Filter state — the single source both the list request and the export href derive from.
  const [actorUserId, setActorUserIdState] = useState(initialActorId ?? "");
  const setActorUserId = (id: string) => {
    setActorUserIdState(id);
    onActorChange?.(id);
  };
  const [entityName, setEntityName] = useState("");
  const [action, setAction] = useState("");
  const [builderOnly, setBuilderOnly] = useState(false);
  // The "Người dựng Nexus" choice only exists once the server has labelled a row (older servers send no actorLabel);
  // sticky so a narrower filter that happens to show no builder row doesn't pull the control away.
  const [sawBuilder, setSawBuilder] = useState(false);
  const [dateFrom, setDateFrom] = useState<string | undefined>();
  const [dateTo, setDateTo] = useState<string | undefined>();

  const filter: AuditFilter = useMemo(
    () => ({
      actorUserId: actorUserId || undefined,
      entityName: entityName || undefined,
      action: action || undefined,
      actor: builderOnly ? ("builder" as const) : undefined,
      dateFrom,
      dateTo,
    }),
    [actorUserId, entityName, action, builderOnly, dateFrom, dateTo],
  );

  // Resolve actor id → name (admin-only screen so /admin/users is available) + entity picker.
  useEffect(() => {
    let alive = true;
    Promise.all([listAdminUsers().catch(() => [] as AdminUser[]), listEntities().catch(() => [] as EntityMeta[])])
      .then(([us, es]) => { if (alive) { setUsers(us); setEntities(es); } });
    return () => { alive = false; };
  }, []);

  // One list request per (filter, refresh tick). `loading` is derived from the key instead of set in
  // the effect (react-hooks/set-state-in-effect), and a slower response for an older filter can no
  // longer overwrite the newer one.
  const requestKey = `${buildAuditQuery(filter)}#${tick}`;
  const loading = page?.key !== requestKey;
  const entries = page?.entries ?? [];
  const nextCursor = page?.nextCursor;
  const err = page?.err ?? null;

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const res = await listAuditFiltered({ ...filter, limit: PAGE_LIMIT });
        if (alive) {
          setPage({ key: requestKey, entries: res.entries, nextCursor: res.nextCursor, err: null });
          if (res.entries.some((e) => e.actorLabel)) setSawBuilder(true);
        }
      } catch (e) {
        if (alive) setPage({ key: requestKey, entries: [], err: e instanceof ApiError ? e.message : t("page.audit.loadError") });
      }
    })();
    return () => { alive = false; };
  }, [filter, requestKey]);

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const res = await listAuditFiltered({ ...filter, limit: PAGE_LIMIT, after: nextCursor });
      setPage((p) => p && { ...p, entries: [...p.entries, ...res.entries], nextCursor: res.nextCursor });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : t("page.audit.loadMoreError");
      setPage((p) => p && { ...p, err: msg });
    } finally {
      setLoadingMore(false);
    }
  };

  const handleExport = async () => {
    setExporting(true);
    try {
      await downloadAuditCsv(filter);
      toast.success(t("page.audit.exported"));
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t("page.audit.exportFailed"));
    } finally {
      setExporting(false);
    }
  };

  const actorOptions = useMemo(
    () => users.map((u) => ({
      value: u.id,
      label: u.fullName || u.email,
      meta: u.fullName ? u.email : undefined, // a nameless user's label already is the email
      icon: { glyph: UserRound, name: "user", label: t("page.audit.userKind") },
    })),
    [users],
  );
  // Reserved `__x__` keys show their friendly label only — the raw token never reaches the UI (DP-1.1).
  const entityOptions = useMemo(
    () => [
      ...entities.map((e) => ({ value: e.name, label: e.label, meta: e.name })),
      ...SYSTEM_ENTITIES.map((n) => ({ value: n, label: SYSTEM_ACTORS[n] ?? n })),
    ],
    [entities],
  );
  const actionOptions = [
    { value: "", label: t("page.audit.actionAll") },
    ...ACTIONS.map((a) => ({ value: a, label: actionLabel(a) })),
  ];

  const actorKindOptions = [
    { value: "", label: t("audit.actorKind.all") },
    { value: "builder", label: t("audit.byBuilder") },
  ];

  const hasFilter = Boolean(actorUserId || entityName || action || builderOnly || dateFrom || dateTo);
  const clearFilters = () => {
    setActorUserId(""); setEntityName(""); setAction(""); setBuilderOnly(false); setDateFrom(undefined); setDateTo(undefined);
  };

  let list: React.ReactNode;
  if (loading) {
    list = (
      <div className="relative min-h-24 slds-border_top">
        <Spinner size="medium" label={t("page.audit.loading")} />
      </div>
    );
  } else if (err) {
    list = (
      <div className="slds-p-around_medium slds-border_top">
        <ScopedNotification theme="error">{err}</ScopedNotification>
      </div>
    );
  } else if (entries.length === 0) {
    list = (
      <div className="slds-border_top">
        <EmptyState
          className="slds-p-around_large"
          icon={History}
          title={t(embedded ? "studio.operate.audit.empty" : "page.audit.empty")}
          description={hasFilter ? t("page.audit.emptyDesc") : undefined}
        />
      </div>
    );
  } else {
    list = (
      <>
        <div className="slds-scrollable_x">
          <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.audit.table")}>
            <thead>
              <tr className="slds-line-height_reset">
                <th scope="col" className="w-12">
                  <span className="slds-assistive-text">{t("page.audit.col.changes")}</span>
                </th>
                {(["at", "actor", "entity", "record", "action"] as const).map((c) => (
                  <th key={c} scope="col">
                    <div className="slds-truncate" title={t(`page.audit.col.${c}`)}>{t(`page.audit.col.${c}`)}</div>
                  </th>
                ))}
              </tr>
            </thead>
            {entries.map((e, i) => (
              <AuditRow
                key={e.id}
                entry={e}
                actorName={actorNames[e.actorUserId] ?? ENGINE_ACTORS[e.actorUserId]}
                entityLabel={entityLabel(e.entityName)}
                tableLabel={entityLabel}
                defaultOpen={i === 0 && !!e.diff}
              />
            ))}
          </table>
        </div>
        {nextCursor && (
          <div className="slds-p-around_small slds-text-align_center slds-border_top">
            <Button onClick={loadMore} disabled={loadingMore}>
              {loadingMore ? t("page.audit.loadingMore") : t("page.audit.loadMore")}
            </Button>
          </div>
        )}
      </>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="p-4">
        <ObjectHomePage
          icon={{ glyph: History, name: "logging" }}
          label={t("page.audit.label")}
          title={t("page.audit.title")}
          meta={loading ? "…" : tf(nextCursor ? "page.audit.metaMore" : "page.audit.meta", { n: entries.length })}
          actions={[{
            label: exporting ? t("page.audit.exporting") : t("page.audit.export"),
            title: t("page.audit.exportTitle"),
            disabled: exporting,
            onSelect: handleExport,
          }]}
          controls={[
            <ButtonIcon key="refresh" icon={RefreshCw} variant="border-filled" label={t("action.refresh")} onClick={() => setTick((n) => n + 1)} />,
          ]}
        >
          {/* Filter row — every control feeds the one `filter` the list and the export share. */}
          <div className="flex flex-wrap items-center gap-2 slds-p-horizontal_medium slds-p-bottom_small">
            <Combobox
              variant="autocomplete"
              className="w-64"
              label={t("page.audit.actor")}
              labelHidden
              placeholder={t("page.audit.actorAll")}
              noResultsText={t("page.audit.noUsers")}
              options={actorOptions}
              value={actorUserId || null}
              onChange={(v) => setActorUserId(v ?? "")}
            />
            {(sawBuilder || builderOnly) && (
              <Combobox
                className="w-52"
                label={t("audit.actorKind")}
                labelHidden
                options={actorKindOptions}
                value={builderOnly ? "builder" : ""}
                onChange={(v) => setBuilderOnly(v === "builder")}
              />
            )}
            <Combobox
              variant="autocomplete"
              className="w-64"
              label={t("page.audit.entity")}
              labelHidden
              placeholder={t("page.audit.entityAll")}
              noResultsText={t("page.audit.noEntities")}
              options={entityOptions}
              value={entityName || null}
              onChange={(v) => setEntityName(v ?? "")}
            />
            <Combobox
              className="w-44"
              label={t("page.audit.action")}
              labelHidden
              options={actionOptions}
              value={action}
              onChange={(v) => setAction(v ?? "")}
            />
            <DateRangePicker
              value={{ dateFrom, dateTo }}
              onChange={(next) => { setDateFrom(next.dateFrom); setDateTo(next.dateTo); }}
              help={t("page.audit.dateHelp")}
            />
            {hasFilter && (
              <Button variant="base" iconLeft={FilterX} onClick={clearFilters}>{t(embedded ? "studio.operate.audit.clearFilters" : "page.audit.clearFilters")}</Button>
            )}
          </div>
          {list}
        </ObjectHomePage>
      </div>
    </div>
  );
}

// ── One log entry: the summary row + (when open) the full-width diff row. One <tbody> per entry so
// the pair stays together. ────────────────────────────────────────────────────────────────────────
function AuditRow({ entry, actorName, entityLabel, tableLabel, defaultOpen }: {
  entry: AuditEntry;
  actorName?: string;
  entityLabel: string;
  /** entity name -> its label, to name the table a `__layout__` row is about. */
  tableLabel: (entityName: string) => string;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(!!defaultOpen);
  const diffId = useId();
  const diffRows = buildDiffRows(entry);
  const hasDiff = diffRows.length > 0;
  // Who acted, in order of trust: a user of THIS customer (resolved by the caller, which also maps the
  // engine's synthetic ids); else the label the SERVER computed for a Nexus builder who worked in this
  // workspace - "Ho ten (Ten xuong)" (specs/062 L3; the screen never reads diff.after.by itself, so a data
  // field called `by` can never forge the badge); else the system label; else a short id (last resort).
  const fromBuilder = !actorName && !!entry.actorLabel;
  const actorDisplay = actorName ?? entry.actorLabel ?? SYSTEM_ACTORS[entry.actorUserId] ?? shortId(entry.actorUserId);
  const verb = rowVerb(entry);
  const until = sessionUntil(entry);
  const target = layoutTarget(entry, tableLabel) ?? roleTarget(entry);
  const at = fmtAt(entry.at);
  const toggleLabel = tf("page.audit.toggleDiff", { actor: actorDisplay, entity: entityLabel, at });

  return (
    <tbody>
      <tr className="slds-hint-parent">
        <td>
          {hasDiff && (
            // Blueprint summary-detail: the switch glyph turns under .slds-is-open.
            <div className={clsx("slds-summary-detail", open && "slds-is-open")}>
              <button
                type="button"
                className="slds-button slds-button_icon"
                title={toggleLabel}
                aria-expanded={open}
                aria-controls={open ? diffId : undefined}
                onClick={() => setOpen((o) => !o)}
              >
                <ChevronDown aria-hidden="true" className="slds-button__icon slds-summary-detail__action-icon nx-lucide" />
                <span className="slds-assistive-text">{toggleLabel}</span>
              </button>
            </div>
          )}
        </td>
        <td>
          <div className="slds-truncate tabular-nums" title={at}>{at}</div>
        </td>
        <th scope="row">
          <div className="flex items-center gap-2">
            <Avatar name={actorDisplay} shape="circle" size="x-small" />
            <span className="slds-truncate" title={actorDisplay}>{actorDisplay}</span>
            {fromBuilder && <Badge variant="lightest">{t("audit.byBuilder")}</Badge>}
          </div>
        </th>
        <td>
          <div className="slds-truncate" title={entityLabel}>{entityLabel}</div>
          {until && <div className="slds-truncate slds-text-body_small slds-text-color_weak" title={until}>{until}</div>}
        </td>
        <td>
          {target
            ? <div className="slds-truncate" title={target}>{target}</div>
            : <div className="slds-truncate tabular-nums" title={entry.recordId || undefined}>{shortId(entry.recordId)}</div>}
        </td>
        <td>
          <Badge theme={ACTION_THEME[entry.action]}>{verb}</Badge>
        </td>
      </tr>
      {open && hasDiff && (
        <tr id={diffId}>
          <td colSpan={COLS} className="slds-theme_shade">
            <table
              className="slds-table slds-table_cell-buffer slds-table_bordered slds-no-row-hover"
              aria-label={t("page.audit.diffTable")}
            >
              <thead>
                <tr className="slds-line-height_reset">
                  <th scope="col" className="w-1/4">
                    <div className="slds-truncate">{t("page.audit.diff.field")}</div>
                  </th>
                  <th scope="col">
                    <div className="slds-truncate">{t("page.audit.diff.before")}</div>
                  </th>
                  <th scope="col">
                    <div className="slds-truncate">{t("page.audit.diff.after")}</div>
                  </th>
                </tr>
              </thead>
              <tbody>
                {diffRows.map((r) => (
                  <tr key={r.field}>
                    <th scope="row">
                      <div className="slds-truncate" title={r.field}>{r.field}</div>
                    </th>
                    <td className="slds-cell-wrap slds-text-color_weak">
                      {r.before === "—" ? r.before : <del>{r.before}</del>}
                    </td>
                    <td className="slds-cell-wrap">{r.after}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </td>
        </tr>
      )}
    </tbody>
  );
}
