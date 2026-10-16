// C10 — Audit Log Viewer (admin) — GET /audit-log with server-side filters
// Restore is [UI-ahead] (404 confirmed 2026-06-30).
import { AuditLog } from "@/components/screens/admin/AuditLog";

export default function AdminAuditPage() {
  return <AuditLog />;
}
