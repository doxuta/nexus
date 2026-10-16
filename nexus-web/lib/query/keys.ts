// Query key factory — centralised keys for TanStack Query cache.
// Pattern: qk.<domain>(<args>) returns a readonly array used as queryKey.
// Invalidation: qk.records(entity) invalidates ALL queries for that entity.

export const qk = {
  // Auth / user
  me: () => ["me"] as const,
  permissions: () => ["permissions", "me"] as const,

  // Project ledger (GET /admin/projects, specs/073): feeds the board, the switcher and the "Cần anh xem" band
  projects: () => ["projects"] as const,

  // Notifications
  notifications: (limit?: number) => ["notifications", limit ?? "all"] as const,
  notificationCount: () => ["notifications", "unread-count"] as const,

  // Metadata
  entities: () => ["metadata", "entities"] as const,
  fields: (entity: string) => ["metadata", "fields", entity] as const,
  // Relationships of one entity, or of the whole project (GET /metadata/relationships) — the Studio table list's "Liên kết tới"
  entityLinks: (entity?: string) => ["metadata", "relationships", entity ?? "all"] as const,
  layout: (entity: string) => ["metadata", "layout", entity] as const,

  // Records (entity = dynamic e.g. "accounts", "tickets")
  records: (entity: string, query?: string) => ["records", entity, query ?? ""] as const,
  record: (entity: string, id: string) => ["record", entity, id] as const,

  // Aggregation
  aggregate: (entity: string, metric: string, field?: string, groupBy?: string) =>
    ["aggregate", entity, metric, field ?? "", groupBy ?? ""] as const,

  // Apps (App container — sidebar lens)
  apps: () => ["apps"] as const,
  appNav: (name: string) => ["apps", "nav", name] as const,
  adminApps: () => ["admin", "apps"] as const,
  adminApp: (id: string) => ["admin", "apps", id] as const,

  // Rules of the project (GET /admin/workflows)
  adminWorkflows: () => ["admin", "workflows"] as const,
  // Newest runs of the listed rules (red "last run failed" mark), keyed by the rule ids asked for
  adminWorkflowRuns: (ids: string) => ["admin", "workflows", "runs", ids] as const,
  approvalProcesses: () => ["admin", "approval-processes"] as const,
  workQueuesAdmin: () => ["admin", "work-queues"] as const,
  inboundHooks: () => ["admin", "inbound-hooks"] as const,
  businessCalendars: () => ["admin", "business-calendars"] as const,
  adminWorkflow: (id: string) => ["admin", "workflows", "detail", id] as const,
  // Reports of the project (GET /admin/reports) — the table page's "Báo cáo" related list
  adminReports: () => ["admin", "reports"] as const,
  adminDashboards: () => ["admin", "dashboards"] as const,

  // Dashboards
  dashboards: () => ["dashboards"] as const,
  // P2 #13: filters included in key so TanStack Query re-fetches when they change.
  dashboardRun: (id: string, filters?: Record<string, string>) =>
    ["dashboard", id, "run", filters ?? {}] as const,
  // Prefix of every dashboardRun filter-variant — invalidate after the definition changed.
  dashboardRunAll: (id: string) => ["dashboard", id] as const,

  // Reports
  reports: () => ["reports"] as const,
  // W4: run-overlay filters in the key so TanStack Query re-fetches when they change.
  reportRun: (id: string, filters?: Record<string, string>) =>
    ["report", id, "run", filters ?? {}] as const,
  // Prefix of every reportRun filter-variant — invalidate after a config edit (admin panel).
  reportRunAll: (id: string) => ["report", id] as const,

  // Roles
  roles: () => ["roles"] as const,
  role: (id: string) => ["role", id] as const,
  // Field-ACL restrictions of one entity (admin, GET /admin/field-acl?entity=)
  fieldAcl: (entity: string) => ["admin", "field-acl", entity] as const,

  // Audit (global)
  auditLog: (limit?: number) => ["audit-log", limit ?? 50] as const,
  // Per-record history (keyed by entity + id for targeted invalidation)
  recordHistory: (entity: string, id: string, limit?: number) => ["record-history", entity, id, limit ?? 20] as const,

  // Files attached to a record
  recordFiles: (entity: string, id: string) => ["record-files", entity, id] as const,

  // N-N relationships
  relationships: (entity: string, id: string, relName: string) => ["relationships", entity, id, relName] as const,

  // Approvals inbox
  pendingApprovals: () => ["approvals", "pending"] as const,
  // "Việc của tôi" (specs/043 S5): invalidate qk.workItems() to refresh every tab + the counts card.
  workItems: (view?: string) => (view ? ["work-items", view] : ["work-items"]) as readonly string[],
  workItemCounts: () => ["work-items", "counts"] as const,

  // Import jobs (for status polling if needed)
  importJob: (entity: string, jobId: string) => ["import", entity, jobId] as const,
};
