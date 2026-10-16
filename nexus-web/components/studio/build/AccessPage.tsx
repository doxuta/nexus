"use client";

// AccessPage (specs/074 FR-030..FR-034, lát f2-access-matrix): `dung/nguoi-dung-quyen`, the work surface of the step
// "Người dùng & quyền". The frame already draws the card and the step sentence ("Đã đủ. 3 vai: …"), so this is the
// matrix of roles x tables (AccessMatrix), the note about who adds staff after the handover, "+ Vai mới" as a strip in
// the page (never a dialog; the new role starts view-only on every table) and, in the 300px column, the sibling pages
// with their counts and the per-app "bù quyền còn thiếu". Scope is the explicit `?app=` (lib/appScope).
import Link from "next/link";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createRole, getFieldAclData, getRole, listRoles, listTeams } from "@/lib/admin";
import { adminApps, appsCatalog, listEntities, type RoleDetail } from "@/lib/api";
import { splitByScope, useAppEntityNames } from "@/lib/appScope";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { slugify } from "@/lib/newEntity";
import { qk } from "@/lib/query/keys";
import { studioHref } from "@/lib/studioRoutes";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { AppFilter } from "../AppFilter";
import { AclBackfillControl } from "../AclBackfillControl";
import { AccessMatrix } from "../AccessMatrix";
import { useRoleViewAs } from "../AccessPages";
import { RelatedLists, type RelatedSection } from "../RelatedLists";
import { useProjectQuery } from "../useProjectQuery";

const ENTITIES_QUERY_KEY = ["studio", "count", "entities"]; // the cache entry the table list reads too

export function AccessPage({ slug, app: appParam, bang }: { slug: string; app?: string; bang?: string }) {
  const qc = useQueryClient();
  const appCode = appParam ?? null;
  // 1 list + one read per role: the matrix needs every role's grants.
  const rolesKey = ["access", "roles"] as const;
  const rolesQ = useProjectQuery(slug, rolesKey, async () => Promise.all((await listRoles()).map((r) => getRole(r.id))));
  const entitiesQ = useQuery({ queryKey: ENTITIES_QUERY_KEY, queryFn: listEntities, staleTime: 60_000 });
  const appsQ = useQuery({ queryKey: qk.apps(), queryFn: appsCatalog, staleTime: 60_000 });
  const viewAs = useRoleViewAs(slug);
  const adminAppsQ = useProjectQuery(slug, ["apps"], adminApps);
  const fieldsQ = useProjectQuery(slug, ["access", "fieldAcl"], () => getFieldAclData());
  const teamsQ = useProjectQuery(slug, ["access", "teams"], listTeams);
  const scope = useAppEntityNames(appCode);

  const roles = rolesQ.data ?? [];
  const entities = entitiesQ.data ?? [];
  const apps = appsQ.data ?? [];
  const { visible, hidden } = splitByScope(entities, scope.entityNames, (e) => e.name);
  const tables = (bang ? visible.filter((e) => e.name === bang) : visible).map((e) => ({ name: e.name, label: e.label || e.name }));

  // ── "+ Vai mới" strip ──
  const [open, setOpen] = useState(false);
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  async function submit() {
    const text = label.trim();
    if (busy || !text) return;
    setBusy(true);
    setErr("");
    try {
      // A new role starts as "can look at every table"; its cells read "follows every table" and link to the role page, where it is narrowed table by table.
      const made = await createRole({ name: slugify(text) || "vai_moi", label: text, actions: [{ entityName: "*", action: "view", accessLevel: "all" }] });
      toast.success(tf("page.adminRoles.created", { label: made.label ?? made.name }));
      setOpen(false);
      setLabel("");
      await qc.invalidateQueries({ queryKey: ["project", slug, ...rolesKey] });
    } catch (e) {
      setErr(errorMessage(e) || t("studio.build.access.new.failed"));
    } finally {
      setBusy(false);
    }
  }
  const newButton = <Button onClick={() => setOpen(true)}>{t("studio.build.access.newRole")}</Button>;
  const form = open && (
    <form
      className="slds-theme_shade slds-p-around_medium slds-m-bottom_small"
      aria-label={t("studio.build.access.newRole")}
      onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); setOpen(false); } }}
      onSubmit={(e) => { e.preventDefault(); void submit(); }}
    >
      <div className="max-w-sm">
        <Input
          autoFocus
          label={t("studio.build.access.new.name")}
          required
          value={label}
          placeholder={t("studio.build.access.new.namePh")}
          help={t("studio.build.access.new.help")}
          error={err || undefined}
          onChange={(e) => setLabel(e.target.value)}
          layout="stacked"
        />
      </div>
      <div className="slds-m-top_small flex gap-2">
        <Button variant="brand" type="submit" disabled={busy || !label.trim()}>{t("studio.build.access.new.submit")}</Button>
        <Button onClick={() => setOpen(false)}>{t("action.cancel")}</Button>
      </div>
    </form>
  );

  // ── what to draw ──
  const allHref = studioHref(slug, "dung/nguoi-dung-quyen");
  let body: React.ReactNode;
  if (rolesQ.isError || entitiesQ.isError) {
    body = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{t("studio.build.access.error.load")}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button onClick={() => { void rolesQ.refetch(); void entitiesQ.refetch(); }}>{t("studio.query.retry")}</Button>
        </div>
      </div>
    );
  } else if (rolesQ.isPending || entitiesQ.isPending || scope.isLoading) {
    body = <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>;
  } else if (roles.length === 0) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={t("studio.build.access.empty.noRoles.title")} description={t("studio.build.access.empty.noRoles.desc")}
        action={open ? undefined : <Button variant="brand" onClick={() => setOpen(true)}>{t("studio.build.access.newRole")}</Button>} />
    );
  } else if (entities.length === 0) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={t("studio.build.access.empty.noTables.title")} description={t("studio.build.access.empty.noTables.desc")}
        action={<Link href={studioHref(slug, "dung/du-lieu")}>{t("studio.build.access.empty.noTables.link")}</Link>} />
    );
  } else if (tables.length === 0) {
    body = (
      <EmptyState plain className="slds-p-around_medium" title={tf("studio.build.access.empty.appTitle", { app: scope.app?.label ?? appCode ?? "" })} description={t("studio.build.access.empty.noTables.desc")}
        action={<Link href={allHref}>{t("studio.build.data.showAll")}</Link>} />
    );
  } else {
    body = (
      <AccessMatrix
        roles={roles}
        tables={tables}
        viewAs={viewAs}
        roleHref={(id) => studioHref(slug, "dung/nguoi-dung-quyen/vai/:id", { id })}
        onSaved={(saved: RoleDetail) => qc.setQueryData<RoleDetail[]>(["project", slug, ...rolesKey], (list) => list?.map((r) => (r.id === saved.id ? saved : r)))}
      />
    );
  }

  const sub = (id: "dung/nguoi-dung-quyen/quyen-truong" | "dung/nguoi-dung-quyen/doi" | "dung/nguoi-dung-quyen/chia-se") =>
    studioHref(slug, id, {}, id.endsWith("quyen-truong") && bang ? { bang } : undefined);
  const counted = (
    id: string, title: string, q: { isPending: boolean; isError: boolean; refetch: () => unknown }, count: number | undefined,
    openKey: "fieldsOpen" | "teamsOpen", route: Parameters<typeof sub>[0],
  ): RelatedSection => ({
    id,
    title,
    state: q.isPending ? "loading" : q.isError ? "error" : "ready",
    count,
    error: t("studio.build.access.related.error"),
    onRetry: () => void q.refetch(),
    items: [<Link key="open" href={sub(route)}>{t(`studio.build.access.related.${openKey}`)}</Link>],
    empty: "",
  });
  const sections: RelatedSection[] = [
    counted("fields", t("studio.build.access.related.fields"), fieldsQ, fieldsQ.data?.grants.length, "fieldsOpen", "dung/nguoi-dung-quyen/quyen-truong"),
    counted("teams", t("studio.build.access.related.teams"), teamsQ, teamsQ.data?.length, "teamsOpen", "dung/nguoi-dung-quyen/doi"),
    { id: "sharing", title: t("studio.build.access.related.sharing"), state: "ready", items: [<Link key="open" href={sub("dung/nguoi-dung-quyen/chia-se")}>{t("studio.build.access.related.sharingOpen")}</Link>], empty: "" },
  ];
  const appList = adminAppsQ.data ?? [];
  if (appList.length > 0) {
    sections.push({
      id: "backfill",
      title: t("studio.build.access.backfillTitle"),
      state: "ready",
      empty: "",
      items: [
        <p key="why" className="slds-text-body_small slds-text-color_weak">{t("studio.build.access.backfillWhy")}</p>,
        ...appList.map((a) => (
          <div key={a.id}>
            <p className="slds-text-title_bold">{a.label || a.name}</p>
            <div className="flex flex-wrap items-center gap-2"><AclBackfillControl appId={a.id} appLabel={a.label || a.name} /></div>
          </div>
        )),
      ],
    });
  }

  const showToolbar = !rolesQ.isError && !entitiesQ.isError && !rolesQ.isPending && !entitiesQ.isPending && !scope.isLoading && roles.length > 0;
  return (
    <div className="flex flex-col items-stretch gap-4 lg:flex-row lg:items-start">
      <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        <p className="slds-text-body_regular slds-text-color_weak slds-p-horizontal_medium slds-p-bottom_small">{t("studio.build.access.ownerNote")}</p>
        {showToolbar && (
          <div className="slds-p-horizontal_medium slds-p-bottom_small slds-grid slds-grid_vertical-align-center slds-wrap">
            <AppFilter slug={slug} route="dung/nguoi-dung-quyen" apps={apps} value={appCode} hiddenCount={hidden} hiddenNoun={t("studio.build.data.noun")} query={{ bang }} />
            {bang && (
              <Link href={studioHref(slug, "dung/nguoi-dung-quyen", {}, appCode ? { app: appCode } : undefined)} className="slds-m-left_small">
                {t("studio.build.data.showAll")}
              </Link>
            )}
            <div className="slds-m-left_small ml-auto">{newButton}</div>
          </div>
        )}
        {form && <div className="slds-p-horizontal_medium">{form}</div>}
        {body}
      </div>
      <div className="w-full shrink-0 px-4 lg:w-[300px] lg:pl-0">
        <RelatedLists label={t("studio.build.access.related")} sections={sections} />
      </div>
    </div>
  );
}
