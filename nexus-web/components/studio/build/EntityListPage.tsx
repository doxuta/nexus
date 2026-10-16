"use client";

// EntityListPage (specs/074 FR-010, lát f2-data-list): `dung/du-lieu` — every table of the project in one list, plus
// "+ Bảng mới" in place. It replaces DataModelBuilder on this route. The frame (ProjectFrame) already draws the card and
// the step header, so this page is only the work surface: the app filter, the search box, the new-table strip and the
// DataTable (blueprint data-tables/default, the whole row opens the table). The form is a shaded strip, never a dialog.
// Scope is the explicit `?app=` (lib/appScope), never the shared "active app" lens.

import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { X } from "lucide-react";
import { appsCatalog, listEntities, type EntityMeta } from "@/lib/api";
import { undoDraftDelete } from "@/lib/draftUndo";
import { StatusText, draftStateLabel } from "@/components/studio/StatusText";
import { splitByScope, useAppEntityNames } from "@/lib/appScope";
import { useEntityOverview, type EntityOverviewRow } from "@/lib/metadata/useEntityOverview";
import { createEntityInApp, createErrorField, slugify } from "@/lib/newEntity";
import { qk } from "@/lib/query/keys";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { isMounted, studioHref } from "@/lib/studioRoutes";
import { resolveCreateApp } from "@/lib/appScope";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { AppFilter } from "@/components/studio/AppFilter";
import { useDirectApply } from "@/components/shell/useDirectApply";
import { useProposedChange, type ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { GhostLive, ProposedGhost } from "@/components/studio/assistant/ProposedGhost";

// The same cache entry the project tree and the table page resolve tables from: refreshing it after a create is what
// lets the new table's page open instead of flashing "Không tìm thấy bảng".
type ListRow = EntityOverviewRow & {
  linkLabels: string;
  id?: string;
  /** Set only on the builder's draft overlay of a delivered project (specs/063 S7-9 + 074 E-F2-2). */
  draftState?: EntityMeta["draftState"];
  pend: { fields: boolean; links: boolean; rules: boolean; apps: boolean };
  /** The assistant's proposal for this row (specs/074 f2-ai-preview-data-rules); `ghostNew` = the row itself is the ghost. */
  ghost?: ProposedChange;
  ghostNew?: boolean;
};

const ENTITIES_QUERY_KEY = ["studio", "count", "entities"];
/** Sentinel of the "Thuộc app" select: the maker chose not to put the table in any app. */
const NO_APP = "-";

/** StatusText of a draft table ("Mới…" / "Sẽ xoá…" + Hoàn tác). Nothing before delivery: the API sends no draftState. */
function DraftMark({ row }: { row: ListRow }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const mark = draftStateLabel(row.draftState);
  if (!mark) return null;
  async function undo() {
    if (!row.id || busy) return;
    setBusy(true);
    try {
      await undoDraftDelete("entity", row.id);
      await qc.invalidateQueries({ queryKey: ENTITIES_QUERY_KEY });
      await qc.invalidateQueries({ queryKey: qk.entities() });
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <span className="slds-m-left_x-small">
      <StatusText tone={mark.tone}>{mark.label}</StatusText>
      {row.draftState === "deleted" && row.id && (
        <Button variant="base" className="slds-m-left_x-small" disabled={busy} aria-label={tf("page.dataModel.undoDeleteLabel", { name: row.label })} onClick={(e) => { e.stopPropagation(); void undo(); }}>
          {t("page.entityHub.undoDelete")}
        </Button>
      )}
    </span>
  );
}

export function EntityListPage({ slug, app: appParam }: { slug: string; app?: string }) {
  const qc = useQueryClient();
  const router = useGuardedRouter();
  const appCode = appParam ?? null;
  const proposed = useProposedChange({ docType: "entity" });

  const entitiesQ = useQuery({ queryKey: ENTITIES_QUERY_KEY, queryFn: listEntities, staleTime: 60_000 });
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const entities = useMemo(() => entitiesQ.data ?? [], [entitiesQ.data]);
  const apps = useMemo(() => appsQ.data ?? [], [appsQ.data]);
  const scope = useAppEntityNames(appCode);
  const overview = useEntityOverview(entities, apps, slug);
  const { hasRecords } = overview;
  const { fields: fieldsLoading, links: linksLoading, rules: rulesLoading, apps: appsLoading } = overview.loading;

  const [search, setSearch] = useState("");
  // Two tables can share a label (crm_accounts and tk_customers are both "Khách hàng"): the code tells them apart.
  const labelOf = useMemo(() => {
    const count = new Map<string, number>();
    for (const e of entities) count.set(e.label || e.name, (count.get(e.label || e.name) ?? 0) + 1);
    return new Map(entities.map((e) => {
      const l = e.label || e.name;
      return [e.name, (count.get(l) ?? 0) > 1 ? `${l} (${e.name})` : l] as const;
    }));
  }, [entities]);

  const { visible: inScope, hidden } = useMemo(
    () => splitByScope(overview.rows, scope.entityNames, (r) => r.name),
    [overview.rows, scope.entityNames],
  );
  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (!needle) return inScope;
    return inScope.filter((r) => r.label.toLowerCase().includes(needle) || r.name.toLowerCase().includes(needle));
  }, [inScope, search]);

  // ── "Bảng mới" strip ──
  const [open, setOpen] = useState(false);
  const [label, setLabel] = useState("");
  const [name, setName] = useState("");
  const [nameTouched, setNameTouched] = useState(false);
  const [picked, setPicked] = useState<string | null>(null); // null = untouched, so the default follows the apps as they load
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<{ field: "name" | "label" | null; message: string } | null>(null);
  const [created, setCreated] = useState<{ name: string; label: string; warn: string } | null>(null);
  const newBtn = useRef<HTMLButtonElement>(null);
  const firstField = useRef<HTMLInputElement>(null);
  const defaultApp = resolveCreateApp(apps, appCode)?.id ?? (apps.length === 0 && appsQ.isSuccess ? NO_APP : "");
  const appId = picked ?? defaultApp;
  // A table in an app seeds default grants for the roles at once: on a delivered project that is asked in the form (lát f2-direct-apply-access).
  const { guard, dialog, notice: applyNotice } = useDirectApply({ inline: true });
  const seeds = appId !== NO_APP && appId !== "";
  useEffect(() => {
    if (open) firstField.current?.focus();
  }, [open]);

  function openForm() {
    setLabel(""); setName(""); setNameTouched(false); setPicked(null); setErr(null); setCreated(null);
    setOpen(true);
  }
  function closeForm() {
    setOpen(false);
    newBtn.current?.focus();
  }

  async function submit() {
    if (busy || !name || !label || !appId) return;
    setBusy(true);
    setErr(null);
    try {
      const target = appId === NO_APP ? null : apps.find((a) => a.id === appId) ?? null;
      const r = await createEntityInApp({ name, label, app: target });
      const shown = r.entity.label || r.entity.name;
      await qc.fetchQuery({ queryKey: ENTITIES_QUERY_KEY, queryFn: listEntities, staleTime: 0 });
      void qc.invalidateQueries({ queryKey: qk.entities() });
      void qc.invalidateQueries({ queryKey: qk.apps() }); // prefix of every appNav
      void qc.invalidateQueries({ queryKey: qk.adminApps() });
      setOpen(false);
      // Never silent: a table without grants or without a menu row is invisible to staff, and the builder (admin) cannot see that.
      const warn = r.navError
        ? t("studio.build.data.navFailed")
        : !target
          ? tf("studio.build.data.warnNoApp", { label: shown })
          : r.aclSeed.appScoped && !r.aclSeed.seeded
            ? r.aclSeed.warning || tf("studio.build.data.warnGrant", { label: shown })
            : "";
      if (warn) {
        setCreated({ name: r.entity.name, label: shown, warn });
        return;
      }
      if (target && r.addedToNav) {
        toast.withAction(tf("studio.build.data.addedToMenu", { label: shown, app: target.label }), {
          label: t("studio.build.data.undo"),
          onClick: () => {
            void r.undo().then(
              () => {
                void qc.invalidateQueries({ queryKey: qk.apps() });
                void qc.invalidateQueries({ queryKey: qk.adminApps() });
                toast.info(tf("studio.build.data.undone", { label: shown, app: target.label }));
              },
              () => toast.error(t("studio.build.data.undoFailed")),
            );
          },
        });
      }
      router.push(studioHref(slug, "dung/du-lieu/:bang", { bang: r.entity.name }, { moi: "1" }));
    } catch (e) {
      setErr({ field: createErrorField(e), message: errorMessage(e) });
    } finally {
      setBusy(false);
    }
  }

  const packHref = isMounted("dung/du-lieu/them-tu-goi") ? studioHref(slug, "dung/du-lieu/them-tu-goi") : null;
  const allHref = studioHref(slug, "dung/du-lieu");
  const newButton = (
    <Button ref={newBtn} onClick={openForm}>
      {t("studio.build.data.new")}
    </Button>
  );

  const form = open && (
    <form
      className="slds-theme_shade slds-p-around_medium"
      aria-label={t("studio.build.data.new")}
      onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); closeForm(); } }}
      onSubmit={(e) => {
        e.preventDefault();
        if (!seeds) return void submit();
        void guard(tf("studio.build.access.applyNow.table", { label: label.trim() || name }), submit);
      }}
    >
      <div className="grid gap-3 md:grid-cols-3 md:items-start">
        <Input
          ref={firstField}
          label={t("studio.build.data.form.label")}
          required
          value={label}
          placeholder={t("studio.build.data.form.labelPh")}
          error={err?.field === "label" ? err.message : undefined}
          onChange={(e) => { setLabel(e.target.value); if (!nameTouched) setName(slugify(e.target.value)); }}
          layout="stacked"
        />
        <Input
          label={t("studio.build.data.form.name")}
          required
          value={name}
          help={t("studio.build.data.form.nameHelp")}
          error={err?.field === "name" ? err.message : undefined}
          onChange={(e) => { setName(e.target.value); setNameTouched(true); }}
          layout="stacked"
        />
        <Select
          label={t("studio.build.data.form.app")}
          required
          value={appId}
          placeholder={t("studio.build.data.form.appPh")}
          onChange={(e) => setPicked(e.target.value)}
          options={[...apps.map((a) => ({ value: a.id, label: a.label })), { value: NO_APP, label: t("studio.build.data.form.appNone") }]}
          help={appId ? undefined : t("studio.build.data.form.appWhy")}
          layout="stacked"
        />
      </div>
      {apps.length === 0 && appsQ.isSuccess && (
        <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.data.noApp")}</p>
      )}
      {err && !err.field && (
        <div className="slds-m-top_small">
          <ScopedNotification theme="error">{err.message || t("studio.build.data.form.createFailed")}</ScopedNotification>
        </div>
      )}
      {seeds && applyNotice && <p className="slds-m-top_small">{applyNotice}</p>}
      {dialog ? (
        <div className="slds-m-top_small">{dialog}</div>
      ) : (
        <div className="slds-m-top_small flex gap-2">
          <Button variant="brand" type="submit" disabled={busy || !name || !label || !appId}>{t("studio.build.data.form.submit")}</Button>
          <Button onClick={closeForm}>{t("action.cancel")}</Button>
        </div>
      )}
    </form>
  );

  const notice = created && (
    <ScopedNotification theme="warning">
      <span className="flex flex-wrap items-center gap-2">
        <span>{created.warn}</span>
        {isMounted("dung/du-lieu/:bang") && (
          <Link href={studioHref(slug, "dung/du-lieu/:bang", { bang: created.name }, { moi: "1" })}>
            {tf("studio.build.data.openCreated", { label: created.label })}
          </Link>
        )}
        <ButtonIcon icon={X} variant="inverse" size="small" label={t("studio.build.data.dismiss")} onClick={() => setCreated(null)} />
      </span>
    </ScopedNotification>
  );

  // Loading flags and link labels travel INSIDE the row data: the column set depends on `slug` only, because a rebuilt
  // column set remounts every cell (and takes a focused link out from under the keyboard).
  const metaOf = useMemo(() => new Map(entities.map((e) => [e.name, e] as const)), [entities]);
  const listRows = useMemo<ListRow[]>(() => {
    const real: ListRow[] = rows.map((r) => ({
      ...r,
      id: metaOf.get(r.name)?.id,
      draftState: metaOf.get(r.name)?.draftState,
      linkLabels: r.linksTo.map((n) => labelOf.get(n) ?? n).join(", "),
      pend: { fields: r.fieldCount === null && fieldsLoading, links: linksLoading, rules: rulesLoading, apps: appsLoading },
      ghost: proposed && proposed.op !== "create" && proposed.target.entity === r.name ? proposed : undefined,
    }));
    if (!proposed || proposed.op !== "create") return real;
    const none = { fields: false, links: false, rules: false, apps: false };
    return [{ name: `ghost:${proposed.proposalKey}`, label: proposed.label, fieldCount: null, linksTo: [], ruleCount: null, records: null, apps: [], linkLabels: "", pend: none, ghost: proposed, ghostNew: true }, ...real];
  }, [rows, labelOf, metaOf, fieldsLoading, linksLoading, rulesLoading, appsLoading, proposed]);
  const columns = useMemo<ColumnDef<ListRow>[]>(() => {
    const pending = t("studio.build.data.pending");
    const none = t("studio.build.data.none");
    const recordsCol: ColumnDef<ListRow>[] = hasRecords ? [{
      id: "records",
      header: t("studio.build.data.records"),
      accessorFn: (r) => r.records ?? -1,
      meta: { numeric: true },
      cell: ({ row: { original: r } }) => (r.records === null ? none : r.records.toLocaleString("vi-VN")),
    }] : [];
    return [
      {
        id: "label",
        header: t("studio.build.data.col.table"),
        accessorFn: (r) => r.label,
        cell: ({ row }) => row.original.ghostNew && row.original.ghost ? (
          <ProposedGhost change={row.original.ghost} />
        ) : (
          <>
            <Link href={studioHref(slug, "dung/du-lieu/:bang", { bang: row.original.name })}>{row.original.label}</Link>
            <span className="slds-text-body_small slds-text-color_weak slds-m-left_x-small">{row.original.name}</span>
            <DraftMark row={row.original} />
            {row.original.ghost && <ProposedGhost change={row.original.ghost} className="slds-m-left_x-small" />}
          </>
        ),
      },
      {
        id: "fields",
        header: t("studio.build.data.col.fields"),
        accessorFn: (r) => r.fieldCount ?? -1,
        meta: { numeric: true },
        cell: ({ row: { original: r } }) => (r.pend.fields ? pending : r.fieldCount === null ? none : String(r.fieldCount)),
      },
      {
        id: "links",
        header: t("studio.build.data.col.links"),
        enableSorting: false,
        cell: ({ row: { original: r } }) => (r.pend.links ? pending : r.linkLabels || none),
      },
      {
        id: "rules",
        header: t("studio.build.data.col.rules"),
        accessorFn: (r) => r.ruleCount ?? -1,
        meta: { numeric: true },
        cell: ({ row: { original: r } }) => (r.pend.rules ? pending : r.ruleCount ? String(r.ruleCount) : none),
      },
      {
        id: "apps",
        header: t("studio.build.data.col.apps"),
        enableSorting: false,
        cell: ({ row: { original: r } }) =>
          r.pend.apps ? pending
          : r.apps.length ? r.apps.map((a) => a.label).join(", ")
          : <span className="slds-text-color_weak">{t("studio.build.data.noApps")}</span>,
      },
      ...recordsCol,
    ];
  }, [slug, hasRecords]);

  // ── what to draw ──
  let body: React.ReactNode;
  if (entitiesQ.isError) {
    body = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{t("studio.build.data.error.load")} {errorMessage(entitiesQ.error)}</ScopedNotification>
        <div className="slds-m-top_small"><Button onClick={() => void entitiesQ.refetch()}>{t("studio.query.retry")}</Button></div>
      </div>
    );
  } else if (scope.unknown) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.data.unknownApp", { app: appCode ?? "" })} description={t("studio.build.data.unknownAppDesc")}
        action={<Link href={allHref}>{t("studio.build.data.showAll")}</Link>} />
    );
  } else if (entitiesQ.isPending || scope.isLoading) {
    body = (
      <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>
    );
  } else if (scope.error) {
    body = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{t("studio.build.data.error.app")}</ScopedNotification>
        <p className="slds-m-top_small"><Link href={allHref}>{t("studio.build.data.showAll")}</Link></p>
      </div>
    );
  } else if (entities.length === 0) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={t("studio.build.data.empty.title")} description={t("studio.build.data.empty.desc")}
        action={<span className="flex flex-wrap items-center gap-3"><Button variant="brand" onClick={openForm}>{t("studio.build.data.empty.action")}</Button>{packHref && <Link href={packHref}>{t("studio.build.data.fromPack")}</Link>}</span>} />
    );
  } else if (inScope.length === 0 && scope.app) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.data.empty.appTitle", { app: scope.app.label })} description={t("studio.build.data.empty.appDesc")}
        action={<span className="flex flex-wrap items-center gap-3">{newButton}<Link href={allHref}>{t("studio.build.data.showAll")}</Link></span>} />
    );
  } else if (rows.length === 0) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.data.empty.noMatch", { q: search.trim() })} description={t("studio.build.data.empty.noMatchDesc")} />
    );
  } else {
    body = (
      <DataTable
        label={t("studio.build.data.grid")}
        columns={columns}
        data={listRows}
        getRowId={(r) => r.name}
        sortable
        stacked
        rowClassName={(r) => (r.ghost ? "nx-proposed" : undefined)}
        onRowClick={(r) => !r.ghostNew && router.push(studioHref(slug, "dung/du-lieu/:bang", { bang: r.name }))}
      />
    );
  }

  // No toolbar when the page has nothing to filter: the empty states carry their own single button.
  const showToolbar = !entitiesQ.isError && !scope.unknown && !scope.isLoading && entities.length > 0 && !(scope.app && inScope.length === 0);
  return (
    <div>
      <GhostLive change={proposed} />
      {showToolbar && (
        <div className="slds-p-horizontal_medium slds-p-bottom_small slds-grid slds-grid_vertical-align-center slds-wrap">
          <div className="slds-m-right_small slds-grow max-w-sm">
            <Input
              label={t("studio.build.data.search")}
              labelHidden
              type="search"
              value={search}
              placeholder={t("studio.build.data.search")}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
          <AppFilter slug={slug} route="dung/du-lieu" apps={apps} value={appCode} hiddenCount={hidden} hiddenNoun={t("studio.build.data.noun")} />
          <div className="slds-m-left_small slds-grid slds-grid_vertical-align-center ml-auto">
            {packHref && <Link href={packHref} className="slds-m-right_small">{t("studio.build.data.fromPack")}</Link>}
            {newButton}
          </div>
        </div>
      )}
      {(form || notice) && (
        <div className="slds-p-horizontal_medium slds-p-bottom_small">
          {form}
          {notice}
        </div>
      )}
      {body}
    </div>
  );
}
