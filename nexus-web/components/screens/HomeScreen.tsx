"use client";

// HomeScreen — màn #7, W3 widget stack v2 (plan §3.9): Chào (giờ + ngày) → Việc của tôi
// (GET /approvals/pending thật) → Vừa xem (recents per-user localStorage, capture ở
// LayoutDetail) → Lối tắt đã ghim (pins từ sidebar + Tạo mới) → KPI thật (pagination.total
// per-entity, W0-QW3) → Cập nhật gần đây (hero entity, server-side) + Bảng điều khiển.
// Mọi widget: dữ liệu thật hoặc empty-state trung thực — KHÔNG số bịa, KHÔNG "Sắp có".
//
// SLDS (specs/038 T028, RESEARCH §5 /home — no "home" blueprint, composed like Lightning Home):
// page-headers/base for the greeting, one `cards` card per widget, `tiles` for list rows and the
// KPI counts (board tiles in ONE card, not a row of equal cards), `data-tables/base` inside the
// "Cập nhật gần đây" card (cards related-list-table), `badges` for counts, `illustration` for
// empty cards. Tailwind only lays the cards out.
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import {
  BarChart3, CheckSquare, Clock, History, House, LayoutGrid, ListTodo, Plus, RefreshCw, Star,
} from "lucide-react";
import Link from "next/link";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useEntities, useFields } from "@/lib/metadata/hooks";
import {
  listRecordsPage, listDashboards, listPendingApprovals, listWorkQueues, workItemCounts,
} from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { recordLabel } from "@/lib/recordLabel";
import { usePins } from "@/lib/pins";
import { pruneRecentRecords, onRecentsChanged, type RecentRecord } from "@/lib/recents";
import { ALL_ITEMS, useActiveApp } from "@/lib/apps";
import { selectAppHome, scopeToAppEntities, pickAppQueues } from "@/lib/appHome";
import { AppHomeDashboard } from "@/components/screens/AppHomeDashboard";
import { getLocale, t, tf } from "@/lib/i18n";
import { PageHeader } from "@/components/slds/PageHeader";
import { Card, RelatedListCard } from "@/components/slds/Card";
import { Badge } from "@/components/slds/Badge";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { buttonClassName } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { Spinner } from "@/components/slds/Spinner";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

// KPI tile definition
interface KpiTile {
  label: string;
  value: string | number;
  entity: string;
}

// "x phút trước" — thời gian tương đối gọn cho widget Vừa xem (không lib ngoài).
function relTime(at: number): string {
  const d = Date.now() - at;
  if (d < 60_000) return "vừa xong";
  if (d < 3_600_000) return `${Math.floor(d / 60_000)} phút trước`;
  if (d < 86_400_000) return `${Math.floor(d / 3_600_000)} giờ trước`;
  return new Date(at).toLocaleDateString("vi-VN", { day: "2-digit", month: "2-digit", timeZone: "Asia/Ho_Chi_Minh" });
}

const shortDate = (iso?: string) =>
  iso ? new Date(iso).toLocaleDateString("vi-VN", { day: "2-digit", month: "2-digit", timeZone: "Asia/Ho_Chi_Minh" }) : "—";

/** Card-body spinner: SLDS centres it absolutely, so the box needs position + height. */
function CardSpinner({ label }: { label: string }) {
  return (
    <div className="slds-is-relative h-16">
      <Spinner size="small" label={label} />
    </div>
  );
}

/** One list row: blueprint tiles "with icon" (media) + "with actions" (spread title row). */
function TileItem({
  href, title, icon, iconName, right, children,
}: {
  href: string;
  title: string;
  icon?: typeof ListTodo;
  iconName?: string;
  right?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <li className="slds-item">
      <article className={clsx("slds-tile", icon && "slds-media")}>
        {icon && (
          <div className="slds-media__figure">
            <Icon icon={icon} category="standard" name={iconName} size="small" />
          </div>
        )}
        <div className="slds-media__body">
          <div className="slds-grid slds-grid_align-spread slds-grid_vertical-align-center slds-has-flexi-truncate">
            <h3 className="slds-tile__title slds-truncate" title={title}>
              <Link href={href}>{title}</Link>
            </h3>
            {right && <div className="slds-shrink-none ml-3">{right}</div>}
          </div>
          {children && <div className="slds-tile__detail">{children}</div>}
        </div>
      </article>
    </li>
  );
}

/** Tile detail meta line: "a · b" (blueprint tiles "article"). */
function MetaLine({ items }: { items: ReactNode[] }) {
  return (
    <ul className="slds-list_horizontal slds-has-dividers_right slds-text-color_weak">
      {items.map((it, i) => (
        <li key={i} className="slds-item slds-truncate">{it}</li>
      ))}
    </ul>
  );
}

export function HomeScreen() {
  const { user } = useAuth();
  const { data: entities = [] } = useEntities();

  // App-face simplification (Founder 2026-07-10): when an app is the active lens, Home is
  // scoped to the app's own nav (stats = nav entities, recent = first nav entity, dashboards
  // = nav dashboards). All Items keeps today's behavior exactly — every branch below falls
  // back to the pre-existing All-Items path when !inAppMode (INV-1).
  const { activeApp, activeNav, activeMeta, navError } = useActiveApp();
  const inAppMode = activeApp !== ALL_ITEMS;
  const appSel = selectAppHome(inAppMode ? activeNav : null);

  // Per-entity counts — same mechanism for both modes, only the entity SOURCE differs:
  // All Items counts every visible entity (D2: hero = most records); app mode counts the
  // app's first-4 nav entities, keeping the admin-arranged nav order.
  const countNames = inAppMode
    ? appSel.statEntities.map((s) => s.name)
    : entities.map((e) => e.name);
  const { data: entityCounts } = useQuery({
    queryKey: ["home-entity-counts", countNames.join(",")],
    queryFn: async () =>
      Promise.all(countNames.map(async (name) => {
        // A FAILED count is not a count of zero. Returning 0 put an authoritative "0" on the tile
        // next to real numbers — the reader has no way to tell "there is nothing" from "we could
        // not ask", and on a fresh app those look identical. null renders as "—", the honest
        // primitive this screen already uses for work-queue counts.
        try { const p = await listRecordsPage(name, "limit=1"); return { name, total: p.pagination.total as number | null }; }
        catch { return { name, total: null as number | null }; }
      })),
    enabled: countNames.length > 0,
    staleTime: 60_000,
  });
  // D2 (All Items only): pick the entity with the MOST records as the hero entity — not the
  // alphabetically-first one. App mode (DP-1.4): hero = the first nav entity that HAS records
  // (nav order; entityCounts preserves it) — a data-less first entity (fresh "Sản phẩm") no
  // longer pins "Cập nhật gần đây" to an empty list. Fallback: the first nav entity as before.
  // Unknown counts sort last: a tile we could not read must not be picked as the hero.
  const topEntity = entityCounts?.length ? [...entityCounts].sort((a, b) => (b.total ?? -1) - (a.total ?? -1))[0] : undefined;
  const allItemsHero = (topEntity && (topEntity.total ?? 0) > 0) ? topEntity.name : entities[0]?.name;
  const appHero = entityCounts?.find((ec) => (ec.total ?? 0) > 0)?.name ?? appSel.recentEntity?.name;
  const firstEntity = inAppMode ? appHero : allItemsHero;
  const firstEntityLabel = inAppMode
    ? appSel.statEntities.find((s) => s.name === firstEntity)?.label ?? appSel.recentEntity?.label ?? firstEntity
    : entities.find((e) => e.name === firstEntity)?.label ?? firstEntity;
  const { data: recentFields = [], isLoading: recentFieldsLoading } = useFields(firstEntity ?? "");
  // DP-2.1: expand relation fields so recordLabel can name join-entities by their endpoints
  // ("Trần Thị Mai — Digital Marketing" instead of #shortid). Waits for fields (one fetch, not two).
  const recentQS = useMemo(() => {
    const rels = recentFields.filter((f) => f.fieldType === "relation").map((f) => f.name);
    const base = "limit=6&sort=updated_at&order=desc";
    return rels.length ? `${base}&expand=${rels.map(encodeURIComponent).join(",")}` : base;
  }, [recentFields]);
  const { data: recentPage, isLoading: recentsLoading } = useQuery({
    queryKey: qk.records(firstEntity ?? "", recentQS),
    queryFn: () => listRecordsPage(firstEntity ?? "", recentQS),
    enabled: !!firstEntity && !recentFieldsLoading,
  });
  const recentRecords = recentPage?.records ?? [];

  // Pinned dashboards. All Items: tenant catalog. App mode: dashboards come from the app nav;
  // DP-4.2 — when the app nav has NONE, fall back to the tenant catalog here on Home (labeled
  // "toàn workspace") so app-mode users still have A path to dashboards (the trimmed TỔNG QUAN
  // removed the sidebar link by design; reachability shouldn't die with it).
  const appNavHasDashboards = appSel.dashboards.length > 0;
  const { data: dashboards = [] } = useQuery({
    queryKey: qk.dashboards(),
    queryFn: listDashboards,
    staleTime: 5 * 60 * 1000,
    enabled: !inAppMode || (activeNav !== null && !appNavHasDashboards),
  });

  // W0-QW3: "Việc của tôi" thật — pending approvals (hook pattern from ApprovalsInbox).
  const { data: allPendingApprovals = [], isLoading: approvalsLoading } = useQuery({
    queryKey: qk.pendingApprovals(),
    queryFn: listPendingApprovals,
    staleTime: 60_000,
  });

  // specs/043 S5: the work-item inbox counts {mine, pool, overdue} — one row on the card, links to /approvals.
  const { data: inboxCounts } = useQuery({
    queryKey: qk.workItemCounts(),
    queryFn: workItemCounts,
    staleTime: 60_000,
    retry: false,
  });
  const hasInbox = !!inboxCounts && inboxCounts.mine + inboxCounts.pool > 0;

  // VIEC-HOM-NAY: role-based queue cards — live caller-scoped counts, deep-link per card.
  const { data: allWorkQueues = [], isLoading: queuesFetching } = useQuery({
    queryKey: ["work-queues"],
    queryFn: listWorkQueues,
    staleTime: 60_000,
  });

  // An app's home shows only the work of the app's own tables (queues + approvals read an entity);
  // All Items keeps the tenant-wide list. While the app nav loads, nothing is shown (not the wrong app's).
  const pendingApprovals = inAppMode ? scopeToAppEntities(allPendingApprovals, activeNav, (a) => a.EntityName) : allPendingApprovals;
  // The builder's explicit queue pick (homeQueueIds) wins; no pick = guessed from the menu tables.
  const explicitQueues = inAppMode ? activeMeta?.homeQueueIds : undefined;
  const workQueues = inAppMode ? pickAppQueues(allWorkQueues, activeNav, explicitQueues, (q) => q.entityName) : allWorkQueues;
  // A nav that FAILED to load must not leave the card spinning forever: nothing to scope by, nothing shown.
  const queuesLoading = queuesFetching || (inAppMode && activeNav === null && !navError && !explicitQueues?.length);

  // W3: "Vừa xem" — recents per-user từ localStorage (LayoutDetail ghi khi record load thật).
  // Entries whose entity no longer exists are pruned on load: they render as a dead link labelled
  // with the machine name, and the entity list needed to spot them is already on this screen.
  // entityNames is empty until /metadata/entities lands, and pruneRecentRecords treats an empty
  // list as "not loaded" rather than "nothing is valid".
  const entityNames = useMemo(() => entities.map((e) => e.name), [entities]);
  const [visited, setVisited] = useState<RecentRecord[]>([]);
  useEffect(() => {
    const reload = () => setVisited(pruneRecentRecords(entityNames));
    reload();
    return onRecentsChanged(reload);
  }, [entityNames]);

  // W3: "Lối tắt" — pinned entities từ sidebar (per-user), kèm shortcut Tạo mới.
  const { pins } = usePins();
  const pinnedEntities = pins
    .map((p) => entities.find((e) => e.name === p))
    .filter((e): e is NonNullable<typeof e> => !!e);

  // W0-QW3: KPI tiles = real per-entity counts from pagination.total (already fetched above
  // for the hero pick) — the 3 dead "Sắp có" tiles are gone. All Items: top-4 entities by
  // record count. App mode: the app's nav entities in nav order (labels from the nav — the
  // app's own naming), counts from the SAME query; entityCounts preserves countNames order.
  const entityLabelOf = (name: string) => entities.find((e) => e.name === name)?.label ?? name;
  const topEntities = [...(entityCounts ?? [])].sort((a, b) => (b.total ?? -1) - (a.total ?? -1)).slice(0, 4);
  const tileLabelOf = (name: string) =>
    inAppMode ? (appSel.statEntities.find((s) => s.name === name)?.label ?? name) : entityLabelOf(name);
  const kpiSource = inAppMode ? (entityCounts ?? []) : topEntities;
  const kpiTiles: KpiTile[] = kpiSource.map((ec) => ({
    label: t("page.home.kpiTotal").replace("{label}", tileLabelOf(ec.name)),
    value: ec.total == null ? "—" : ec.total.toLocaleString("vi-VN"),
    // Cố ý KHÔNG có dòng phụ: nó từng ghi "Đối tượng: Khách hàng" ngay dưới "Tổng Khách hàng"
    // — lặp lại đúng cái nhãn vừa đọc, và dán thêm "Đối tượng", từ của người dựng engine chứ
    // không phải của người bán hàng (MỘT XƯỞNG TRỤ 3).
    entity: ec.name,
  }));

  // Section visibility — All Items expressions equal today's constants (INV-1); app mode
  // hides sections that have no source in the app nav instead of rendering hollow shells.
  // While the app nav itself is loading, keep skeletons up (no flash-then-pop layout shift).
  const appNavLoading = inAppMode && activeNav === null;
  const showKpis = inAppMode
    ? appNavLoading || appSel.statEntities.length > 0
    : kpiTiles.length > 0 || entities.length > 0;
  const showRecent = !inAppMode || appNavLoading || !!appSel.recentEntity;
  // DP-4.2: app-mode always shows the section — nav dashboards when present, else the tenant
  // catalog fallback (reachability; the section labels the fallback honestly).
  const showDashboards = !inAppMode || appSel.dashboards.length > 0 || dashboards.length > 0;

  const now = new Date();
  const greetHour = now.getHours();
  const greetWord = t(greetHour < 12 ? "page.home.greet.morning" : greetHour < 18 ? "page.home.greet.afternoon" : "page.home.greet.evening");
  const firstName = user?.fullName?.split(" ").pop() ?? user?.email?.split("@")[0] ?? t("page.home.fallbackName");
  const todayRaw = now.toLocaleDateString(getLocale() === "en" ? "en-US" : "vi-VN", {
    weekday: "long", day: "numeric", month: "long", year: "numeric", timeZone: "Asia/Ho_Chi_Minh",
  });
  const today = todayRaw.charAt(0).toLocaleUpperCase("vi") + todayRaw.slice(1);
  const greeting = `${greetWord}, ${firstName}`;
  const header = (meta: string) => (
    <PageHeader icon={{ glyph: House, name: "home" }} title={greeting} meta={meta} />
  );

  // Friendly empty state: non-admin inside an app whose nav resolved to ZERO visible items
  // (ACL hid everything or the app is unconfigured) — one calm line instead of hollow
  // sections. Admins keep the full home (Studio Home's checklist + sidebar hint point at the fix).
  if (inAppMode && !user?.isAdmin && appSel.isEmpty) {
    return (
      <div className="mx-auto flex w-full min-w-0 max-w-[1080px] flex-col gap-4 p-4">
        {header(today)}
        <ScopedNotification>{t("page.home.emptyApp")}</ScopedNotification>
      </div>
    );
  }

  const workTotal = pendingApprovals.length + workQueues.reduce((n, q) => n + (q.count ?? 0), 0);
  const noWork = !queuesLoading && !approvalsLoading && !hasInbox && workQueues.length === 0 && pendingApprovals.length === 0;
  const recentRows = recentRecords.map((r) => ({
    // R2-01: the metadata-driven label heuristic (composite names, first/last, title…) instead of
    // assuming a "name" field — otherwise contacts/etc fall back to the raw UUID.
    id: r.id,
    label: recordLabel(r, recentFields),
    updated: shortDate(r.updatedAt),
  }));
  const recentTitle = firstEntity ? `${t("page.home.recent")} · ${firstEntityLabel}` : t("page.home.recent");
  const dashboardsTitle = inAppMode && !appNavHasDashboards
    ? `${t("page.home.dashboards")} · ${t("page.home.dashboardsWorkspace")}`
    : t("page.home.dashboards");
  // W0-QW4: /dashboard (legacy) → /dashboards (route thật)
  const dashboardsAllHref = !inAppMode || !appNavHasDashboards ? "/dashboards" : undefined;
  const dashboardItems = inAppMode && appNavHasDashboards
    ? appSel.dashboards.map((d) => ({ key: d.refId, href: d.href, label: d.label }))
    // W0-QW4: từng trỏ `/dashboard` (legacy, mọi card về cùng 1 chỗ) → viewer thật
    : dashboards.slice(0, 4).map((d) => ({ key: d.id, href: `/dashboards/${d.id}`, label: d.name }));

  return (
    <div className="mx-auto flex w-full min-w-0 max-w-[1080px] flex-col gap-4 p-4">
      {/* 1 · Chào — greeting + ngày thật */}
      {header(`${today} · ${t("page.home.subtitle")}`)}

      {/* DP-1.4: the admin getting-started checklist (all-builder tasks) moved to Studio Home —
          the customer face stays in the customer's role even for admins. */}

      {/* Lightning Home: main column (work, counts, latest records) + side column (recents,
          shortcuts, dashboards). Cards stack inside a column and take SLDS's own
          `.slds-card + .slds-card` spacing; the grid only places the two columns. */}
      <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[1fr_320px]">
        <div className="min-w-0">
          {/* App home dashboard — the dashboard the app's builder chose as its front page. */}
          {inAppMode && activeMeta?.homeDashboardId && (
            <AppHomeDashboard dashboardId={activeMeta.homeDashboardId} />
          )}

          {/* 2 · Việc của tôi (approvals thật + hàng đợi) */}
          <Card
            title={t("page.home.myWork")}
            icon={{ glyph: ListTodo, name: "task" }}
            count={workTotal > 0 ? workTotal : undefined}
            padded={false}
            footer={
              pendingApprovals.length > 5 && (
                <Link className="slds-card__footer-action" href="/approvals">
                  {t("page.home.approvalsAll").replace("{n}", String(pendingApprovals.length))}
                </Link>
              )
            }
          >
            {noWork ? (
              <EmptyState icon={Clock} title={t("page.home.noApprovals")} />
            ) : (
              <>
                {queuesLoading && <CardSpinner label={t("page.home.queuesLoading")} />}
                <ul className="slds-has-dividers_top-space">
                  {hasInbox && inboxCounts && (
                    <TileItem
                      href="/approvals"
                      title={t("page.inbox.title")}
                      icon={CheckSquare}
                      iconName="approval"
                      right={inboxCounts.overdue > 0 ? <Badge theme="error" className="tabular-nums">{tf("page.inbox.home.overdue", { n: inboxCounts.overdue })}</Badge> : undefined}
                    >
                      <MetaLine items={[tf("page.inbox.home.mine", { n: inboxCounts.mine }), tf("page.inbox.home.pool", { n: inboxCounts.pool })]} />
                    </TileItem>
                  )}
                  {/* VIEC-HOM-NAY queue cards — one row per queue the caller may see. */}
                  {workQueues.map((q) => (
                    <TileItem
                      key={q.id}
                      href={q.href}
                      title={q.label}
                      icon={ListTodo}
                      iconName="work-queue"
                      right={
                        <Badge theme={(q.count ?? 0) > 0 ? "warning" : undefined} className="tabular-nums">
                          {q.count == null ? "—" : q.count.toLocaleString("vi-VN")}
                        </Badge>
                      }
                    />
                  ))}
                  {pendingApprovals.slice(0, 5).map((a) => {
                    // F7-4: server-resolved display labels (new camelCase keys riding the legacy
                    // PascalCase rows). Absent/empty → today's slug+id (honest fallback).
                    const { entityLabel, recordLabel: recLabel } =
                      a as typeof a & { entityLabel?: string; recordLabel?: string };
                    const entity = entityLabel || a.EntityName;
                    const step = t("page.home.approvalStep").replace("{n}", String(a.CurrentStep + 1));
                    return (
                      <TileItem
                        key={a.ID}
                        href="/approvals"
                        title={recLabel ? `${entity}: ${recLabel}` : t("page.home.approvalFallback").replace("{entity}", entity)}
                        icon={CheckSquare}
                        iconName="approval"
                      >
                        <MetaLine items={recLabel ? [step] : [step, a.RecordID?.slice(0, 8) ?? "—"]} />
                      </TileItem>
                    );
                  })}
                  {!approvalsLoading && pendingApprovals.length === 0 && (
                    <li className="slds-item slds-text-color_weak">{t("page.home.noApprovals")}</li>
                  )}
                </ul>
                {approvalsLoading && <CardSpinner label={t("page.home.loading")} />}
              </>
            )}
          </Card>

          {/* 5 · KPI — số ĐẾM THẬT per-entity (W0-QW3), board tiles trong MỘT card. App mode: entity
              của app theo thứ tự nav. Chưa có số → card loading (không ô giả). */}
          {showKpis && (
            <Card title={t("page.home.kpi")} icon={{ glyph: BarChart3, name: "metrics" }} loading={kpiTiles.length === 0}>
              <ul className="grid grid-cols-2 gap-4 sm:grid-cols-4">
                {/* One li per tile (blueprint tiles "board" list), so SLDS's `.slds-tile + .slds-tile`
                    stacking margin never offsets a grid cell. */}
                {kpiTiles.map((tile) => (
                  <li key={tile.entity}>
                    <article className="slds-tile">
                      <h3 className="slds-tile__title slds-truncate" title={tile.label}>
                        <Link href={`/${tile.entity}`}>{tile.label}</Link>
                      </h3>
                      <div className="slds-tile__detail">
                        <p className="slds-text-heading_large tabular-nums">{tile.value}</p>
                      </div>
                    </article>
                  </li>
                ))}
              </ul>
            </Card>
          )}

          {/* Cập nhật gần đây (server-side; All Items: hero entity, app mode: entity đầu nav) */}
          {showRecent && (recentsLoading || appNavLoading || recentRows.length === 0 ? (
            <Card
              title={recentTitle}
              icon={{ glyph: RefreshCw, name: "record-update" }}
              loading={recentsLoading || appNavLoading}
              viewAllHref={firstEntity ? `/${firstEntity}` : undefined}
            >
              <EmptyState
                title={t("page.home.recentEmpty")}
                action={firstEntity && (
                  <Link href={`/${firstEntity}/new`} className={buttonClassName("neutral")}>
                    {t("page.home.recentCreateFirst")}
                  </Link>
                )}
              />
            </Card>
          ) : (
            <RelatedListCard
              title={recentTitle}
              icon={{ glyph: RefreshCw, name: "record-update" }}
              columns={[
                { key: "label", label: t("page.home.recentColName") },
                { key: "updated", label: t("page.home.recentColUpdated") },
              ]}
              rows={recentRows}
              // SLDS related list: "(n)" is the record total; the table shows only the first 6.
              count={recentPage?.pagination.total}
              rowKey={(r) => r.id}
              rowHref={(r) => `/${firstEntity}/${r.id}`}
              viewAllHref={firstEntity ? `/${firstEntity}` : undefined}
            />
          ))}
        </div>

        <div className="min-w-0">
          {/* 3 · Vừa xem (recents per-user) */}
          <Card title={t("page.home.visited")} icon={{ glyph: History, name: "recent" }} padded={false}>
            {visited.length === 0 ? (
              <EmptyState icon={History} title={t("page.home.visitedEmpty")} description={t("page.home.visitedEmptyHint")} />
            ) : (
              <ul className="slds-has-dividers_top-space">
                {visited.slice(0, 6).map((v) => (
                  <TileItem key={`${v.entity}-${v.id}`} href={`/${v.entity}/${v.id}`} title={v.label}>
                    <MetaLine items={[entityLabelOf(v.entity), relTime(v.at)]} />
                  </TileItem>
                ))}
              </ul>
            )}
          </Card>

          {/* 4 · Lối tắt — pinned entities (đồng bộ ngôi sao ở sidebar). CHỈ ở All-Items: app-mode
              nav rows cố ý không có ngôi sao (nav app ĐÃ là lối tắt được curate) — hiện khối này
              trong app-mode là dạy một thao tác không tồn tại. */}
          {(!inAppMode || pinnedEntities.length > 0) && (
            <Card title={t("page.home.shortcuts")} icon={{ glyph: Star, name: "link" }}>
              {pinnedEntities.length === 0 ? (
                <p className="slds-text-color_weak">{t("page.home.shortcutsEmpty")}</p>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {pinnedEntities.map((e) => {
                    const create = t("page.home.shortcutNew").replace("{label}", e.label);
                    return (
                      <ButtonGroup key={e.name} label={e.label}>
                        <Link href={`/${e.name}`} className={buttonClassName("neutral")}>
                          {e.label}
                        </Link>
                        <Link
                          href={`/${e.name}/new`}
                          aria-label={create}
                          title={create}
                          className="slds-button slds-button_icon slds-button_icon-border-filled"
                        >
                          <Plus aria-hidden="true" className="slds-button__icon nx-lucide" />
                        </Link>
                      </ButtonGroup>
                    );
                  })}
                </div>
              )}
            </Card>
          )}

          {/* Bảng điều khiển — All Items: catalog toàn tenant; app mode: dashboard trong nav
              của app, hoặc (DP-4.2) fallback catalog toàn workspace khi nav không gắn cái nào —
              đường tới dashboards không chết theo sidebar đã rút gọn. */}
          {showDashboards && (
            <Card
              title={dashboardsTitle}
              icon={{ glyph: LayoutGrid, name: "dashboard" }}
              padded={false}
              viewAllHref={dashboardsAllHref}
            >
              {dashboardItems.length === 0 ? (
                <EmptyState
                  icon={LayoutGrid}
                  title={t("page.home.dashboardsEmpty")}
                  // DASH-MERGE: creation lives on /dashboards now (admin button there) — the old
                  // Studio builder is gone. Non-admins get honest copy, no dead CTA.
                  description={user?.isAdmin ? undefined : t("page.home.dashboardsAskAdmin")}
                  action={user?.isAdmin && (
                    <Link href="/dashboards" className={buttonClassName("neutral")}>
                      {t("page.home.dashboardsCreate")}
                    </Link>
                  )}
                />
              ) : (
                <ul className="slds-has-dividers_top-space">
                  {dashboardItems.map((d) => (
                    <TileItem key={d.key} href={d.href} title={d.label} icon={LayoutGrid} iconName="dashboard" />
                  ))}
                </ul>
              )}
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}
