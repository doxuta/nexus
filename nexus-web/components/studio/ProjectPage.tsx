"use client";

// The page body of /studio/du-an/<slug>/<duong…> (specs/073 FR-015, FR-016): reads the path, then
//   root                → replace to the first page (Dữ liệu);
//   not a known page    → a plain "no such page" with a way back (never a blank);
//   declared, not built → PendingPage (two honest lines, FR-132);
//   mounted             → its component.
// The component table is the only place a route id becomes a component; routes mount here as their slice lands.
import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useEffect, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { listEntities } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { ApiKeys } from "@/components/screens/admin/ApiKeys";
import { CredentialsPage } from "./operate/CredentialsPage";
import { AuditLog } from "@/components/screens/admin/AuditLog";
import { BackgroundJobs } from "@/components/screens/admin/BackgroundJobs";
import { EmailLog } from "@/components/screens/admin/EmailLog";
import { SsoProviders } from "@/components/screens/admin/SsoProviders";
import { UserList } from "@/components/screens/admin/UserList";
import { DataTablesPage } from "./operate/DataTablesPage";
import { InboundDeliveriesPage } from "./operate/InboundDeliveriesPage";
import { HealthPage } from "./operate/HealthPage";
import { OperateOverview } from "./operate/OperateOverview";
import { EntityRecordsPage, KnownEntityGate } from "./operate/EntityRecordsPage";
import { OperateBatch, OperateImport } from "./operate/ImportBatchPages";
import { OperateRecycleBin } from "./operate/RecycleBinPage";
import { parseStudioPath, studioHref, type ParsedStudioPath } from "@/lib/studioRoutes";
import { FieldAclPage } from "./build/FieldAclPage";
import { SharingPage } from "./build/SharingPage";
import { TeamsPage } from "./build/TeamsPage";
import { RolePage } from "./build/RolePage";
import { AccessPage } from "./build/AccessPage";
import { AppsPage } from "./build/AppsPage";
import { BrandingPage } from "./build/BrandingPage";
import { HandoffPage } from "./handoff/HandoffPage";
import { TrialPage } from "./TrialDeliverPages";
import { ReportsPage } from "./build/ReportsPage";
import { ApprovalPage } from "./build/ApprovalPage";
import { QueuePage } from "./build/QueuePage";
import { DashboardPage } from "./build/DashboardPage";
import { SubPage } from "./AutomationPage";
import { AddFromPackPage } from "./build/AddFromPackPage";
import { EntityListPage } from "./build/EntityListPage";
import { RuleListPage } from "./build/RuleListPage";
import { RulePage } from "./build/RulePage";
import { EntityPage } from "./EntityPage";
import { CalendarPage } from "./build/CalendarPage";
import { HookPage } from "./build/HookPage";
import { KhamPage } from "./maintain/KhamPage";
import { NangCapPage } from "./maintain/NangCapPage";
import { HistoryPage } from "./maintain/HistoryPage";
import { BackupsPage } from "./maintain/BackupsPage";
import { RestorePage } from "./maintain/RestorePage";
import { UpgradePage } from "./maintain/UpgradePage";
import { VersionPage } from "./maintain/VersionPage";
import { AUTO_PAGES, backTo, RunsPage, USER_PAGES, WithRelated } from "./OperatePages";
import { RecordPage } from "./operate/RecordPage";
import { PendingPage } from "./PendingPage";
import { QueryState } from "./QueryState";
import { RelationMap } from "./relmap/RelationMap";
import { ProjectSettings } from "./ProjectSettings";
import { ScreensPage } from "./build/ScreensPage";

// Same cache entry EntityListPage reads: creating a table there is what lets its page open here.
const ENTITIES_QUERY_KEY = ["studio", "count", "entities"];

// `van-hanh/nguoi-dung/nhat-ky`: `?nguoi=` opens the log filtered to one user and follows the actor filter (replace, so Back
// does not step through every filter change).
function AuditRoute({ actor }: { actor?: string }) {
  const router = useRouter();
  const pathname = usePathname();
  return <AuditLog initialActorId={actor} onActorChange={(id) => router.replace(`${pathname}${id ? `?nguoi=${encodeURIComponent(id)}` : ""}`, { scroll: false })} />;
}

// `dung/du-lieu/<bang>`: the table comes from the project's own list; `?truong=` follows the open field (replace, so
// Back does not step through every field); a deleted table returns to the list.
function EntityRoute({ slug, bang, truong, isNew }: { slug: string; bang: string; truong: string | null; isNew: boolean }) {
  const router = useRouter();
  const pathname = usePathname();
  const q = useQuery({ queryKey: ENTITIES_QUERY_KEY, queryFn: listEntities, staleTime: 60_000 });
  const entity = q.data?.find((e) => e.name === bang);
  const list = studioHref(slug, "dung/du-lieu");
  return (
    <QueryState query={q}>
      {entity ? (
        <EntityPage
          key={bang}
          slug={slug}
          entity={entity}
          field={truong}
          isNew={isNew}
          onFieldChange={(name) => {
            if ((name ?? null) !== truong) router.replace(`${pathname}${name ? `?truong=${encodeURIComponent(name)}` : ""}`, { scroll: false });
          }}
          onDeleted={() => router.replace(list)}
        />
      ) : (
        <div className="slds-p-around_large">
          <p className="nx-lede">{tf("studio.entity.notFound", { name: bang })}</p>
          <p className="slds-text-color_weak">{t("studio.entity.notFoundHint")}</p>
          <p className="slds-m-top_small">
            <Link href={list}>{`← ${t("studio.entity.back")}`}</Link>
          </p>
        </div>
      )}
    </QueryState>
  );
}

const emailStatus = (v?: string): "failed" | "queued" | "sent" | "" =>
  v === undefined ? "failed" : v === "failed" || v === "queued" || v === "sent" ? v : "";

type Ctx = { slug: string; parsed: ParsedStudioPath };

// Every mounted builder sits behind the locked scope so it sees the whole project (FR-031).
export const MOUNTED: Record<string, (c: Ctx) => ReactNode> = {
  EntityListPage: ({ slug, parsed }) => <EntityListPage slug={slug} app={parsed.query.app} />,
  AddFromPackPage: ({ slug }) => (
    <SubPage back={{ label: t("studio.build.pack.back"), href: studioHref(slug, "dung/du-lieu") }}>
      <AddFromPackPage slug={slug} />
    </SubPage>
  ),
  EntityPage: ({ slug, parsed }) => <EntityRoute slug={slug} bang={parsed.params.bang ?? ""} truong={parsed.query.truong ?? null} isNew={parsed.query.moi === "1"} />,
  // The diagram is a canvas (xyflow): the shell must give it a height (T099). ?bang= focuses one table.
  RelationMap: ({ slug, parsed }) => (
    <EmbeddedSurface canvas>
      <RelationMap slug={slug} focusEntity={parsed.query.bang} />
    </EmbeddedSurface>
  ),
  ScreensPage: ({ slug, parsed }) => <ScreensPage slug={slug} entity={parsed.params.bang ?? parsed.query.bang} man={parsed.query.man} app={parsed.query.app} truong={parsed.query.truong} lienQuan={parsed.query["lien-quan"]} cheDo={parsed.query["che-do"]} />,
  AppsPage: ({ slug, parsed }) => (
    <SubPage back={{ label: t("studio.build.screens.back"), href: studioHref(slug, "dung/man-hinh") }}>
      <AppsPage slug={slug} app={parsed.query.app} />
    </SubPage>
  ),
  AutomationPage: ({ slug, parsed }) => <RuleListPage slug={slug} app={parsed.query.app} bang={parsed.query.bang} />,
  RulePage: ({ slug, parsed }) => (
    <RulePage slug={slug} id={parsed.params.id} app={parsed.query.app} bang={parsed.query.bang} khi={parsed.query.khi} ten={parsed.query.ten} />
  ),
  ApprovalPage: ({ slug, parsed }) => <SubPage back={{ label: t("studio.build.automation.back"), href: studioHref(slug, "dung/tu-dong") }}><ApprovalPage slug={slug} id={parsed.params.id} app={parsed.query.app} bang={parsed.query.bang} /></SubPage>,
  QueuePage: ({ slug, parsed }) => <SubPage back={{ label: t("studio.build.automation.back"), href: studioHref(slug, "dung/tu-dong") }}><QueuePage slug={slug} id={parsed.params.id} app={parsed.query.app} bang={parsed.query.bang} /></SubPage>,
  HookPage: ({ slug, parsed }) => <HookPage slug={slug} hookId={parsed.params.id} />,
  CalendarPage: ({ slug }) => <CalendarPage slug={slug} />,
  // Người dùng & quyền, Báo cáo, Thương hiệu, Chạy thử, Giao (kh3).
  AccessPage: ({ slug, parsed }) => <AccessPage slug={slug} app={parsed.query.app} bang={parsed.query.bang} />,
  RolePage: ({ slug, parsed }) => <RolePage slug={slug} roleId={parsed.params.id ?? ""} />,
  FieldAclPage: ({ slug, parsed }) => <FieldAclPage slug={slug} bang={parsed.query.bang} truong={parsed.query.truong} />,
  TeamsPage: ({ slug }) => <TeamsPage slug={slug} />,
  SharingPage: ({ slug }) => <SharingPage slug={slug} />,
  ReportsPage: ({ slug, parsed }) => <ReportsPage slug={slug} bc={parsed.query.bc} bang={parsed.query.bang} app={parsed.query.app} />,
  DashboardPage: ({ slug, parsed }) => <DashboardPage slug={slug} id={parsed.params.id ?? ""} />,
  BrandingPage: ({ slug }) => <BrandingPage slug={slug} />,
  TrialPage: ({ slug }) => <TrialPage slug={slug} />,
  DeliverPage: ({ slug }) => <HandoffPage slug={slug} />,
  // Vận hành (kh4). Each screen is the one the customer's admin already has; only the frame around it is new.
  UserList: ({ slug, parsed }) => <WithRelated slug={slug} pages={USER_PAGES}><UserList studioSlug={slug} person={parsed.query.nguoi} /></WithRelated>,
  SsoProviders: ({ slug }) => <SubPage back={backTo(slug, "van-hanh/nguoi-dung", "studio.route.operUsers")}><SsoProviders /></SubPage>,
  AuditLog: ({ slug, parsed }) => (
    <SubPage back={backTo(slug, "van-hanh/nguoi-dung", "studio.route.operUsers")}>
      <AuditRoute actor={parsed.query.nguoi} />
    </SubPage>
  ),
  DataTablesPage: ({ slug }) => <DataTablesPage slug={slug} />,
  EntityRecordsPage: ({ slug, parsed }) => <EntityRecordsPage slug={slug} bang={parsed.params.bang ?? ""} />,
  OperateBatchCreate: ({ slug, parsed }) => (
    <KnownEntityGate slug={slug} bang={parsed.params.bang ?? ""}>{() => <OperateBatch slug={slug} bang={parsed.params.bang ?? ""} />}</KnownEntityGate>
  ),
  OperateImport: ({ slug, parsed }) => (
    <KnownEntityGate slug={slug} bang={parsed.params.bang ?? ""}>{() => <OperateImport slug={slug} bang={parsed.params.bang ?? ""} />}</KnownEntityGate>
  ),
  OperateRecycleBin: ({ slug, parsed }) => (
    <KnownEntityGate slug={slug} bang={parsed.params.bang ?? ""}>{() => <OperateRecycleBin slug={slug} bang={parsed.params.bang ?? ""} />}</KnownEntityGate>
  ),
  // `…/<bang>/<id>` and `…/<bang>/moi`: the customer's own record screen in a frame (lát u7-du-lieu-ban-ghi).
  RecordPage: ({ slug, parsed }) => (
    <RecordPage
      slug={slug}
      bang={parsed.params.bang ?? ""}
      id={parsed.params.id}
      edit={parsed.query.sua === "1"}
      cloneFrom={parsed.query.cloneFrom}
      search={new URLSearchParams(parsed.query).toString()}
    />
  ),
  HealthPage: ({ slug, parsed }) => <HealthPage slug={slug} khoang={parsed.query.khoang} />,
  OperateOverview: ({ slug }) => <WithRelated slug={slug} pages={AUTO_PAGES}><OperateOverview slug={slug} /></WithRelated>,
  BackgroundJobs: ({ slug, parsed }) => {
    const emailLogHref = studioHref(slug, "van-hanh/tu-dong/email");
    return parsed.route?.id === "van-hanh/tu-dong/viec-nen" ? (
      <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}><BackgroundJobs emailLogHref={emailLogHref} /></SubPage>
    ) : (
      <WithRelated slug={slug} pages={AUTO_PAGES}><BackgroundJobs emailLogHref={emailLogHref} /></WithRelated>
    );
  },
  // `?trang-thai=` picks the status (absent = Lỗi, anything else = all); a record opens in the same tab, in this frame.
  EmailLog: ({ slug, parsed }) => (
    <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}>
      <EmailLog
        key={parsed.query["trang-thai"] ?? ""}
        initialStatus={emailStatus(parsed.query["trang-thai"])}
        recordHref={(bang, id) => studioHref(slug, "van-hanh/du-lieu/:bang/:id", { bang, id })}
      />
    </SubPage>
  ),
  WorkflowRunHistory: ({ slug, parsed }) => <RunsPage slug={slug} ruleId={parsed.query.luat} />,
  ApiKeys: ({ slug }) => <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}><ApiKeys slug={slug} /></SubPage>,
  Credentials: ({ slug }) => <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}><CredentialsPage slug={slug} /></SubPage>,
  InboundLink: ({ slug, parsed }) => <SubPage back={backTo(slug, "van-hanh/tu-dong", "studio.route.operAuto")}><InboundDeliveriesPage slug={slug} cong={parsed.query.cong} /></SubPage>,
  // Bảo trì (kh4)
  DoctorScreen: ({ slug }) => <KhamPage slug={slug} />,
  PackDiffPanel: ({ slug }) => <NangCapPage slug={slug} />,
  HistoryPage: ({ slug }) => <HistoryPage slug={slug} />,
  VersionPage: ({ slug, parsed }) => <VersionPage slug={slug} id={parsed.params.id ?? ""} loai={parsed.query.loai} bang={parsed.query.bang} />,
  UpgradePage: ({ slug, parsed }) => (
    <SubPage back={{ label: t("studio.route.upgrade"), href: studioHref(slug, "bao-tri/nang-cap") }}>
      <UpgradePage project={slug} pack={parsed.params.id ?? ""} />
    </SubPage>
  ),
  BackupsPage: ({ slug }) => <BackupsPage slug={slug} />,
  RestorePage: ({ slug, parsed }) => <RestorePage project={slug} backupId={parsed.params.id ?? ""} lan={parsed.query.lan} />,
  ProjectSettings: ({ slug }) => <ProjectSettings slug={slug} />,
};

/** Where "up" is for a page that is declared but not built; default = the first page. */
function pendingParent(slug: string, routeId: string): { label: string; href: string } | undefined {
  if (routeId === "dung/du-lieu" || !routeId.startsWith("dung/")) return undefined;
  if (routeId.startsWith("dung/tu-dong/")) return { label: t("studio.build.automation.back"), href: studioHref(slug, "dung/tu-dong") };
  if (routeId === "dung/du-lieu/them-tu-goi") return { label: t("studio.build.automation.packOld"), href: "/studio/verticals" };
  return { label: t("studio.route.dataList"), href: studioHref(slug, "dung/du-lieu") };
}

export function ProjectPage({ slug }: { slug: string }) {
  const pathname = usePathname();
  const router = useRouter();
  const search = useSearchParams().toString();
  const parsed = parseStudioPath(search ? `${pathname}?${search}` : pathname); // pathname has no query; the page reads ?bang= / ?man=
  const root = parsed?.root === true;
  useEffect(() => {
    if (root) router.replace(studioHref(slug, "dung/du-lieu"));
  }, [root, router, slug]);

  if (root) return null;
  const route = parsed?.route ?? null;
  const first = { label: t("studio.route.dataList"), href: studioHref(slug, "dung/du-lieu") };
  if (!route) {
    return (
      <div className="slds-p-around_large">
        <p className="nx-lede">{t("studio.notFound.title")}</p>
        <p className="slds-text-color_weak">{t("studio.notFound.hint")}</p>
        <p className="slds-m-top_small">
          <Link href={first.href}>{first.label}</Link>
        </p>
      </div>
    );
  }
  const render = route.mounted && route.mounts ? MOUNTED[route.mounts] : undefined;
  if (render && parsed) {
    return render({ slug, parsed });
  }
  const pending = <PendingPage feature={t(route.labelKey)} parent={pendingParent(slug, route.id)} />;
  return pending;
}
