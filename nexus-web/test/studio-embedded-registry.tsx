// Registry of the builders that will be mounted inside the Studio frame (specs/073 T091, FR-005, D-UI-27).
// Each entry says how to render ONE builder inside <EmbeddedSurface> against a faked network, and which
// of the embedded-surface rules it does NOT meet yet (an exemption carries its reason and the slice that fixes it).
// Exemptions may only be removed: studio-single-surface.test.tsx compares their count with the ceiling in
// studio-gates.allowlist.json ("embeddedExempt") and fails on a new one. Numbers behind the reasons were measured
// on the real DOM by scripts/embedded-builders-measure.mjs (Chrome, /dev/embedded) and are in quickstart.md.
import type { ReactElement } from "react";

/** Selector per rule; the count of matches inside the embedded container must be 0. */
export const EMBEDDED_RULES = {
  card: ".slds-card",
  h1: "h1",
  pageHeader: ".slds-page-header",
  iconTile: "[class*='slds-icon-standard-']",
  tablist: "[role='tablist']",
  dockedPanel: ".slds-panel_docked",
  badge: ".slds-badge",
  avatar: ".slds-avatar",
  tabsChrome: ".slds-tabs_default, .slds-tabs_scoped",
  illustration: ".slds-illustration",
} as const;
export type EmbeddedRule = keyof typeof EMBEDDED_RULES;

/**
 * The wrappers that read EmbeddedSurface, by name (not a count): ObjectHomePage joins in F6 (u7-nen-nhung-mau-trang)
 * and only has to add its name here.
 */
export const EMBEDDED_WRAPPERS = ["PageHeader", "Card", "Panel", "Tabs", "Badge", "Avatar", "EmptyState", "templates/ObjectHomePage"] as const;

export interface EmbeddedEntry {
  id: string;
  /** Source of the component under nexus-web (looked up in the dialog ledger). */
  file?: string;
  /** Accessible name of the button that starts "create" in this builder (studio-no-create-modal.test.tsx clicks it). */
  openCreate?: string;
  /** The builder is a canvas (xyflow, three columns) that needs the shell to give it a height: <EmbeddedSurface canvas>. */
  canvas?: boolean;
  /** Fake network: URL substring (longest match wins) -> JSON body. Anything else answers `{ data: [] }`. */
  routes?: Record<string, unknown>;
  /** Not written yet: the registry lists it, the gate skips it with this reason (the slice that adds it). */
  pending?: string;
  render?: () => Promise<ReactElement>;
  /** Rules this builder does not meet yet -> reason (and the slice that fixes it). */
  exempt?: Partial<Record<EmbeddedRule, string>>;
}

// Fixtures: enough rows to make a builder draw its rows (avatars, chips, tabs), not only its empty state.
const USERS = [
  { id: "u-1", tenantId: "t-1", email: "admin@example.com", fullName: "Nexus Admin", status: "active", isAdmin: true },
  { id: "u-2", tenantId: "t-1", email: "test@example.com", fullName: "Trần Sales Rep", status: "active", isAdmin: false, roleId: "r-1" },
];
const ROLES = [{ id: "r-1", name: "sales_staff", label: "Nhân viên kinh doanh" }];
const ENTITIES = [{ id: "e-1", name: "crm_accounts", label: "Khách hàng", tableName: "crm_accounts" }];
const FIELDS = [
  { id: "f-1", name: "name", label: "Tên", fieldType: "text", isRequired: true },
  { id: "f-2", name: "email", label: "Email", fieldType: "email", isUnique: true },
  { id: "f-3", name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"] } },
];
const RULE = { id: "w-1", name: "Gửi thư chào", entityName: "crm_accounts", trigger: "after_create", isActive: true, actions: [], condition: { match: "and", rules: [] } };

export const EMBEDDED_REGISTRY: EmbeddedEntry[] = [
  {
    id: "WorkflowBuilder",
    file: "components/studio/WorkflowBuilder.tsx",
    openCreate: "Tạo luật mới",
    routes: { "/admin/workflows": { data: { workflows: [RULE] } }, "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS } },
    exempt: {
      h1: "tiêu đề danh sách luật là <h1> thô (WorkflowBuilder.tsx, cột trái) (F2, khu Dựng tự động hoá)",
      iconTile: "ô icon màu thô ở đầu danh sách luật (F2, khu Dựng tự động hoá)",
    },
    render: async () => { const { WorkflowBuilder } = await import("@/components/studio/WorkflowBuilder"); return <WorkflowBuilder />; },
  },
  {
    // specs/074 lát 11: one rule on its own page — flat sections, no card / tabs / h1 / tile.
    id: "RulePage",
    file: "components/studio/build/RulePage.tsx",
    routes: { "/admin/workflows": { data: { workflows: [RULE] } }, "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS } },
    render: async () => { const { RulePage } = await import("@/components/studio/build/RulePage"); return <RulePage slug="northwind" id="w-1" />; },
  },
  { id: "BrandingScreen", render: async () => { const { BrandingScreen } = await import("@/components/studio/BrandingScreen"); return <BrandingScreen />; } },
  {
    id: "UserList",
    // Embedded, UserList renders UserListStudio: the file with no dialog is the one the dialog ledger must judge.
    file: "components/studio/operate/UserListStudio.tsx",
    openCreate: "Người dùng mới",
    // specs/078 u7-nguoi-dung-danh-sach: embedded, the list is the flat in-place page; it reads through useProjectQuery,
    // so /auth/me must answer as the operator at home ("northwind").
    routes: { "/admin/users": { data: { users: USERS } }, "/admin/roles": { data: { roles: ROLES } }, "/auth/me": { data: { id: "u-1", isPlatformOperator: true } } },
    render: async () => { const { UserList } = await import("@/components/screens/admin/UserList"); return <UserList studioSlug="northwind" />; },
  },
  {
    id: "AuditLog",
    file: "components/screens/admin/AuditLog.tsx",
    routes: { "/audit": { data: [{ id: "a-1", actorUserId: "u-1", entityName: "crm_accounts", recordId: "r-1", action: "update", at: "2026-10-09T01:00:00Z", diff: { before: { name: "A" }, after: { name: "B" } } }] }, "/admin/users": { data: { users: USERS } } },
    render: async () => { const { AuditLog } = await import("@/components/screens/admin/AuditLog"); return <AuditLog />; },
  },
  // kh4 (specs/073 T271): the screens mounted in Vận hành / Bảo trì / Tài khoản. Fixtures only need to make them draw rows.
  { id: "SsoProviders", file: "components/screens/admin/SsoProviders.tsx", routes: { "/admin/sso": { data: { providers: [] } } }, render: async () => { const { SsoProviders } = await import("@/components/screens/admin/SsoProviders"); return <SsoProviders />; } },
  { id: "EntityDirectory", routes: { "/metadata/entities": { data: ENTITIES } }, render: async () => { const { EntityDirectory } = await import("@/components/screens/EntityDirectory"); return <EntityDirectory />; } },
  {
    id: "OperateOverview",
    routes: { "/auth/me": { data: { id: "u-1", isPlatformOperator: true } }, "/admin/jobs": { data: [], pagination: {} }, "/admin/email-log": { data: [], pagination: {}, meta: {} }, "/admin/workflows/runs": { data: { runs: [] } }, "/admin/workflows": { data: { workflows: [RULE] } }, "/admin/inbound-hooks": { data: { hooks: [] } }, "/admin/api-keys": { data: { apiKeys: [] } } },
    render: async () => { const { OperateOverview } = await import("@/components/studio/operate/OperateOverview"); return <OperateOverview slug="northwind" />; },
  },
  { id: "BackgroundJobs", file: "components/screens/admin/BackgroundJobs.tsx", render: async () => { const { BackgroundJobs } = await import("@/components/screens/admin/BackgroundJobs"); return <BackgroundJobs />; } },
  { id: "WorkflowRunHistory", routes: { "/admin/workflows": { data: { workflows: [RULE] } } }, render: async () => { const { WorkflowRunHistory } = await import("@/components/studio/WorkflowRunHistory"); return <WorkflowRunHistory workflowId="w-1" entityName="crm_accounts" />; } },
  { id: "NamedCredentials", routes: { "/admin/credentials": { data: { credentials: [{ id: "c-1", name: "cong_giao_hang", label: "Cổng giao hàng", baseUrl: "https://ship.example.com", authType: "hmac", headerName: "", headerPrefix: "", username: "", status: "ready", secretHint: "ab12", extraHeaderNames: [], rotatedAt: null, previousExpiresAt: null, usageCount: 0, createdAt: "", updatedAt: "" }] } } }, render: async () => { const { NamedCredentials } = await import("@/components/screens/admin/NamedCredentials"); return <NamedCredentials />; } },
  { id: "ApiKeys", file: "components/screens/admin/ApiKeys.tsx", openCreate: "Tạo khoá truy cập", routes: { "/auth/me": { data: { id: "u-1", isPlatformOperator: true } }, "/admin/api-keys": { data: { apiKeys: [] } }, "/admin/roles": { data: { roles: ROLES } } }, render: async () => { const { ApiKeys } = await import("@/components/screens/admin/ApiKeys"); return <ApiKeys slug="northwind" />; } },
  { id: "PackDiffPanel", render: async () => { const { PackDiffPanel } = await import("@/components/studio/PackDiffPanel"); return <PackDiffPanel slug="crm" />; } },
  { id: "Profile", render: async () => { const { Profile } = await import("@/components/screens/Profile"); return <Profile />; } },
  { id: "MfaCard", render: async () => { const { MfaCard } = await import("@/components/screens/MfaCard"); return <MfaCard mfaEnabled={false} onMfaChanged={() => {}} />; } },
  { id: "DoctorScreen", render: async () => { const { DoctorScreen } = await import("@/components/screens/admin/DoctorScreen"); return <DoctorScreen />; } },
  {
    id: "TeamsList",
    file: "components/screens/admin/TeamsList.tsx",
    openCreate: "Tạo nhóm",
    routes: { "/admin/teams": { data: { teams: [{ id: "tm-1", tenantId: "t-1", name: "Kinh doanh" }] } }, "/admin/users": { data: { users: USERS } } },
    render: async () => { const { TeamsList } = await import("@/components/screens/admin/TeamsList"); return <TeamsList />; },
  },
  {
    id: "ImportScreen",
    routes: { "/metadata/fields/": { data: FIELDS } },
    render: async () => { const { ImportScreen } = await import("@/components/screens/ImportScreen"); return <ImportScreen entity="crm_accounts" />; },
  },
  {
    id: "EntityFieldEditor",
    file: "components/studio/EntityFieldEditor.tsx",
    openCreate: "Thêm trường",
    routes: { "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS } },
    render: async () => { const { EntityFieldEditor } = await import("@/components/studio/EntityFieldEditor"); return <EntityFieldEditor entityName="crm_accounts" />; },
  },
  {
    id: "AppsManager",
    file: "components/studio/AppsManager.tsx",
    // "Tạo app" is an in-page strip since f2-apps-menu; only "Sửa" still opens CreateAppDialog (components/apps, its own ledger row).
    routes: { "/admin/apps": { data: { apps: [{ id: "a-1", name: "crm", label: "CRM", isActive: true, nav: [] }] } }, "/metadata/entities": { data: ENTITIES } },
    render: async () => { const { AppsManager } = await import("@/components/studio/AppsManager"); return <AppsManager />; },
  },
  {
    id: "ApprovalBuilder",
    file: "components/studio/ApprovalBuilder.tsx",
    routes: { "/metadata/entities": { data: ENTITIES } },
    exempt: {
      iconTile: "ô icon màu thô ở đầu danh sách quy trình (F2, khu Dựng tự động hoá)",
    },
    render: async () => { const { ApprovalBuilder } = await import("@/components/studio/ApprovalBuilder"); return <ApprovalBuilder />; },
  },
  {
    id: "WorkQueueBuilder",
    file: "components/studio/WorkQueueBuilder.tsx",
    routes: { "/metadata/entities": { data: ENTITIES } },
    render: async () => { const { WorkQueueBuilder } = await import("@/components/studio/WorkQueueBuilder"); return <WorkQueueBuilder />; },
  },
  {
    id: "InboundHookScreen",
    file: "components/studio/InboundHookScreen.tsx",
    routes: { "/metadata/entities": { data: ENTITIES } },
    render: async () => { const { InboundHookScreen } = await import("@/components/studio/InboundHookScreen"); return <InboundHookScreen />; },
  },
  {
    id: "BusinessCalendars",
    file: "components/screens/admin/BusinessCalendars.tsx",
    render: async () => { const { BusinessCalendars } = await import("@/components/screens/admin/BusinessCalendars"); return <BusinessCalendars />; },
  },
  // EntityPage is the table page on dung/du-lieu/<bang> (T156/T169, kh1b2).
  {
    id: "EntityPage",
    file: "components/studio/EntityPage.tsx",
    routes: { "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS } },
    render: async () => {
      const { EntityPage } = await import("@/components/studio/EntityPage");
      return <EntityPage slug="northwind" entity={ENTITIES[0]} field={null} onFieldChange={() => {}} onDeleted={() => {}} />;
    },
  },
  // specs/078 u7-nen-nhung-mau-trang: the Vận hành screens F6 will mount. Exemptions name the slice that removes each.
  {
    id: "UserSessionsCard",
    file: "components/screens/admin/UserSessionsCard.tsx",
    render: async () => {
      const { UserSessionsCard } = await import("@/components/screens/admin/UserSessionsCard");
      const row = { id: "s-1", browser: "Chrome", os: "macOS", ip: "10.0.0.1", lastUsedAt: "2026-10-09T01:00:00Z", expiresAt: "2026-10-30T01:00:00Z", current: false };
      return <UserSessionsCard load={async () => [row]} end={async () => {}} />;
    },
  },
  { id: "EmailLog", file: "components/screens/admin/EmailLog.tsx", render: async () => { const { EmailLog } = await import("@/components/screens/admin/EmailLog"); return <EmailLog />; } },
  {
    id: "LayoutList",
    file: "components/layout/LayoutList.tsx",
    routes: { "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS } },
    render: async () => { const { LayoutList } = await import("@/components/layout/LayoutList"); return <LayoutList entity="crm_accounts" />; },
  },
  {
    // specs/078 u7-du-lieu-danh-sach-bang: the table list and one table's record list, 0 exemptions (the list body is the
    // embedded ObjectHomePage; the page draws only the back row).
    id: "DataTablesPage",
    file: "components/studio/operate/DataTablesPage.tsx",
    routes: { "/metadata/entities": { data: ENTITIES }, "/auth/me": { data: { id: "u-1", isPlatformOperator: true } } },
    render: async () => { const { DataTablesPage } = await import("@/components/studio/operate/DataTablesPage"); return <DataTablesPage slug="northwind" />; },
  },
  {
    id: "EntityRecordsPage",
    file: "components/studio/operate/EntityRecordsPage.tsx",
    routes: { "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS }, "/auth/me": { data: { id: "u-1", isPlatformOperator: true } } },
    render: async () => { const { EntityRecordsPage } = await import("@/components/studio/operate/EntityRecordsPage"); return <EntityRecordsPage slug="northwind" bang="crm_accounts" />; },
  },
  {
    // specs/078 u7-du-lieu-ban-ghi: one record in CustomerPreviewFrame (the frame is a picture of the customer's screen, so its
    // inside is not counted); reads the table list through useProjectQuery, so /auth/me answers as the operator at home.
    id: "RecordPage",
    file: "components/studio/operate/RecordPage.tsx",
    routes: { "/metadata/entities": { data: ENTITIES }, "/metadata/fields/": { data: FIELDS }, "/auth/me": { data: { id: "u-1", isPlatformOperator: true } } },
    render: async () => { const { RecordPage } = await import("@/components/studio/operate/RecordPage"); return <RecordPage slug="northwind" bang="crm_accounts" id="r-1" />; },
  },
  {
    id: "CalendarView",
    file: "components/layout/CalendarView.tsx",
    render: async () => { const { CalendarView } = await import("@/components/layout/CalendarView"); return <CalendarView entity="crm_accounts" fields={[{ id: "f-d", name: "due", label: "Hạn", fieldType: "date" } as never]} />; },
  },
  { id: "RecycleBinScreen", file: "components/screens/RecycleBinScreen.tsx", render: async () => { const { RecycleBinScreen } = await import("@/components/screens/RecycleBinScreen"); return <RecycleBinScreen entity="crm_accounts" />; } },
  {
    id: "BatchCreateForm",
    file: "components/layout/BatchCreateForm.tsx",
    routes: { "/metadata/fields/": { data: FIELDS } },
    render: async () => { const { BatchCreateForm } = await import("@/components/layout/BatchCreateForm"); return <BatchCreateForm entity="crm_accounts" />; },
  },
];
