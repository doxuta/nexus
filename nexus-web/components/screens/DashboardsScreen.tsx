"use client";

// A4.1 Dashboard List — /dashboards
// Endpoint: GET /dashboards (non-admin catalog, auth-only). Verified live 2026-07-01.
// DASH-MERGE (2026-07-25): creation lives HERE now (admin button + name dialog → viewer),
// not in Studio — the Studio DashboardBuilder is gone, /studio/dashboards redirects here.
// SLDS (specs/038 T029, RESEARCH §5 row /dashboards): the ObjectHomePage template (page-headers/
// object-home inside a card, with the list search) over a grid of tiles (blueprint `tiles`, "with
// icon" + "article" examples: title link, description, then widget count and shapes as a
// horizontal definition list); create = blueprint `modals`; empty/no-match = `illustration`.

import Link from "next/link";
import { useState } from "react";
import { useRouter } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { LayoutDashboard } from "lucide-react";
import { createDashboard, hasDraftChange, listAdminDashboards, listDashboards, type DashboardCatalogItem } from "@/lib/api";
import { DraftViewButton, useDraftView } from "@/components/analytics/useDraftView";
import { Badge } from "@/components/slds/Badge";
import type { I18nKey } from "@/lib/i18n/vi";
import { qk } from "@/lib/query/keys";
import { ScopeHiddenNote } from "@/components/studio/ScopeHiddenNote";
import { dashboardInScope } from "@/lib/appScope";
import { useWorkshopScope } from "@/lib/workshopScope";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t, tf } from "@/lib/i18n";
import { errorMessage } from "@/lib/errorMessages";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";

const ICON = { glyph: LayoutDashboard, name: "dashboard" };

/** /dashboards and, inside an app workshop (?panel=dashboard, #302), the same catalogue filtered to the app. */
export function DashboardsScreen() {
  const [q, setQ] = useState("");
  const workshop = useWorkshopScope();
  const dashHref = (id: string) => (workshop ? workshop.panelHref("dashboard", { dashboard: id }) : `/dashboards/${id}`);
  const { user } = useAuth();
  const isAdmin = Boolean(user?.isAdmin);
  const router = useRouter();
  const queryClient = useQueryClient();
  // specs/063 S7-2: the builder can list the catalog over the unpublished draft (?draft=1).
  const draftView = useDraftView();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: draftView.on ? [...qk.dashboards(), "draft"] : qk.dashboards(),
    queryFn: listDashboards,
    staleTime: 30_000,
  });

  // Inside an app workshop the open catalogue cannot say which tables a dashboard reads (its widgets carry
  // only {id,type}); the builder's own list can. Attribution is best-effort: a dashboard we cannot attribute
  // stays visible (never hide what we cannot place).
  const [showAll, setShowAll] = useState(false);
  const scoped = !!workshop?.entityNames && !showAll;
  const adminQ = useQuery({
    queryKey: ["workshop", "dashboards-attribution"],
    queryFn: listAdminDashboards,
    enabled: !!workshop?.entityNames && isAdmin,
    staleTime: 30_000,
  });

  // Create dialog (admin) — name it, then land in the viewer where all authoring lives.
  const [createOpen, setCreateOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [creating, setCreating] = useState(false);

  async function handleCreate() {
    const name = newName.trim();
    if (!name || creating) return;
    setCreating(true);
    try {
      const d = await createDashboard(name, []);
      queryClient.invalidateQueries({ queryKey: qk.dashboards() });
      // A delivered workspace creates it in the DRAFT: open it on the draft or the live read finds nothing.
      router.push(`${dashHref(d.id)}${hasDraftChange(d.draftState) ? (workshop ? "&draft=1" : "?draft=1") : ""}`);
    } catch (e) {
      toast.error(errorMessage(e));
      setCreating(false);
    }
  }

  function openCreate() {
    setNewName("");
    setCreateOpen(true);
  }

  // Catalog items, not full dashboards: their widgets carry only {id, type} (the open /dashboards
  // list no longer ships entity/metric/filter config). This page draws a count and a type preview,
  // which is exactly what survives — see DashboardCatalogItem.
  const needle = q.toLowerCase();
  const byQuery: DashboardCatalogItem[] = (data ?? []).filter((d) =>
    !q || d.name.toLowerCase().includes(needle) || d.description?.toLowerCase().includes(needle),
  );
  const attribution = new Map((adminQ.data ?? []).map((d) => [d.id, d.widgets.map((w) => w.entity)]));
  const items = scoped
    ? byQuery.filter((d) => {
        const ents = attribution.get(d.id);
        return !ents || dashboardInScope(ents, workshop!.entityNames);
      })
    : byQuery;
  const hiddenCount = byQuery.length - items.length;

  const createButton = (
    <Button variant="neutral" onClick={openCreate}>
      {t("page.dashboards.create")}
    </Button>
  );

  const empty =
    !isLoading && !isError && items.length === 0 ? (
      // Role-split call to action: an admin creates HERE; everyone else is told who can.
      <EmptyState
        icon={LayoutDashboard}
        title={t(q ? "page.dashboards.noResults" : "page.dashboards.empty")}
        description={t(q ? "page.dashboards.noResultsHint" : isAdmin ? "page.dashboards.emptyAdmin" : "page.dashboards.emptyUser")}
        action={!q && isAdmin ? createButton : undefined}
        className="py-8"
      />
    ) : undefined;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4">
      <ObjectHomePage
        icon={ICON}
        label={t("page.dashboards.label")}
        title={t("page.dashboards.title")}
        meta={
          <>
            {isLoading ? "…" : tf("page.dashboards.count", { n: items.length })} · {t("page.dashboards.subtitle")}
          </>
        }
        actions={isAdmin ? [{ label: t("page.dashboards.create"), onSelect: openCreate }] : undefined}
        search={{ value: q, onChange: setQ, placeholder: t("page.dashboards.search") }}
        controls={[<DraftViewButton key="draft-view" view={draftView} />]}
        empty={empty}
      >
        <div className="slds-border_top p-4">
          {workshop?.entityNames && (
            <ScopeHiddenNote hidden={hiddenCount} noun="dashboards" showAll={showAll} onToggle={() => setShowAll((v) => !v)} />
          )}
          {isLoading && (
            <div className="relative h-24">
              <Spinner size="small" />
            </div>
          )}
          {isError && (
            <ScopedNotification theme="error">
              {error instanceof Error ? error.message : t("page.dashboards.loadError")}
            </ScopedNotification>
          )}
          {!isLoading && !isError && (
            <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              {items.map((d) => (
                <li key={d.id}>
                  <article className="slds-tile slds-media slds-box h-full">
                    <div className="slds-media__figure">
                      <Icon icon={ICON.glyph} category="standard" name={ICON.name} size="small" />
                    </div>
                    <div className="slds-media__body min-w-0">
                      <h3 className="slds-tile__title slds-truncate" title={d.name}>
                        <Link href={draftView.keep(dashHref(d.id))}>{d.name}</Link>
                        {hasDraftChange(d.draftState) && (
                          <Badge theme={d.draftState === "deleted" ? "warning" : undefined} className="slds-m-left_x-small">
                            {t(`draft.state.${d.draftState}` as I18nKey)}
                          </Badge>
                        )}
                      </h3>
                      <div className="slds-tile__detail">
                        {d.description && <p className="line-clamp-2">{d.description}</p>}
                        <dl className="slds-list_horizontal slds-wrap mt-1">
                          <dt className="slds-item_label slds-text-color_weak slds-truncate">{t("page.dashboards.widgets")}:</dt>
                          <dd className="slds-item_detail slds-truncate">{d.widgets.length}</dd>
                          <dt className="slds-item_label slds-text-color_weak slds-truncate">{t("page.dashboards.widgetTypes")}:</dt>
                          <dd className="slds-item_detail">
                            <ul className="slds-list_horizontal slds-has-dividers_right">
                              {d.widgets.slice(0, 4).map((w) => (
                                <li key={w.id} className="slds-item">
                                  {w.type}
                                </li>
                              ))}
                              {d.widgets.length > 4 && <li className="slds-item">+{d.widgets.length - 4}</li>}
                            </ul>
                          </dd>
                        </dl>
                      </div>
                    </div>
                  </article>
                </li>
              ))}
            </ul>
          )}
        </div>
      </ObjectHomePage>

      {/* Create dialog — name only; widgets get assembled in the viewer's authoring panel. */}
      <Modal
        open={createOpen}
        onOpenChange={(o) => !creating && setCreateOpen(o)}
        title={t("page.dashboards.create")}
        size="small"
        footer={
          <>
            <Button onClick={() => setCreateOpen(false)} disabled={creating}>
              {t("action.cancel")}
            </Button>
            <Button type="submit" form="nx-create-dashboard" variant="brand" disabled={creating || !newName.trim()}>
              {t("page.dashboards.createSubmit")}
            </Button>
          </>
        }
      >
        <form
          id="nx-create-dashboard"
          onSubmit={(e) => {
            e.preventDefault();
            void handleCreate();
          }}
        >
          <Input
            label={t("page.dashboards.createName")}
            required
            autoFocus
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            disabled={creating}
            placeholder={t("page.dashboards.createPlaceholder")}
            help={t("page.dashboards.createHelp")}
          />
        </form>
      </Modal>
    </div>
  );
}
