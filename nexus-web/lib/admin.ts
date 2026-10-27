// lib/admin.ts — Admin & Security API layer (Slice 5).
// All endpoints verified live against http://localhost:8090/api/v1 on 2026-06-30.
// Envelope: named-noun { data: { users/user/role/roles/team/teams/members } } — NOT generic record envelope.

import {
  listRoles,
  getRole,
  updateRolePermissions,
  listAdminUsers,
  ApiError,
  ensureAuth,
  getToken,
  apiRequest,
  type LoginSession,
  type AdminUser,
  type RoleSummary,
  type RoleDetail,
  type RoleGrant,
  type RoleActionVerb,
  type RoleAccessLevel,
  type AuditEntry,
  type ApiNotification,
} from "@/lib/api";
import type { FieldAclGrant } from "@/lib/fieldAcl";
import type { ResetLinkResult } from "@/lib/invite";

export type {
  AdminUser,
  RoleSummary,
  RoleDetail,
  RoleGrant,
  RoleActionVerb,
  RoleAccessLevel,
  AuditEntry,
  ApiNotification,
};
export { listRoles, getRole, updateRolePermissions, listAdminUsers, ApiError, ensureAuth };

// Base URL (same as api.ts).
const BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8080/api/v1";

// Every admin call goes through the shared client, so an expired access token is refreshed and the
// call retried once (#290). The private fetch that used to live here had no 401 recovery.
const adminReq = apiRequest;

// ──────────────────────────────────────────────
// USERS (C1/C2/C3)
// Verified: GET/POST /admin/users, GET/PUT/DELETE /admin/users/:id
// ──────────────────────────────────────────────

export interface CreateUserInput {
  email: string;
  password: string;
  fullName: string;
  status?: "active" | "inactive";
  isAdmin?: boolean;
  roleId?: string;
}

export interface UpdateUserInput {
  fullName?: string;
  status?: "active" | "inactive";
  isAdmin?: boolean;
  roleId?: string;
  password?: string;
}

/** GET /admin/users → { data: { users: AdminUser[] } } */
export async function listUsers(): Promise<AdminUser[]> {
  const r = await adminReq<{ data: { users: AdminUser[] } }>("GET", "/admin/users");
  return r.data?.users ?? [];
}

/** GET /admin/users/:id → { data: { user: AdminUser } } */
export async function getUser(id: string): Promise<AdminUser> {
  const r = await adminReq<{ data: { user: AdminUser } }>("GET", `/admin/users/${encodeURIComponent(id)}`);
  return r.data.user;
}

/** POST /admin/users → { data: { user: AdminUser } } — 201 */
export async function createUser(input: CreateUserInput): Promise<AdminUser> {
  const r = await adminReq<{ data: { user: AdminUser } }>("POST", "/admin/users", input);
  return r.data.user;
}

/** PUT /admin/users/:id → { data: { user: AdminUser } } */
export async function updateUser(id: string, input: UpdateUserInput): Promise<AdminUser> {
  const r = await adminReq<{ data: { user: AdminUser } }>("PUT", `/admin/users/${encodeURIComponent(id)}`, input);
  return r.data.user;
}

/** PUT /admin/users/:id/user-manager → { data: { user } } — the builder's switch for the customer-owner
 *  capability (specs/065). Operator only; turning it off never touches the user's role. */
export async function setUserManager(id: string, enabled: boolean): Promise<AdminUser> {
  const r = await adminReq<{ data: { user: AdminUser } }>("PUT", `/admin/users/${encodeURIComponent(id)}/user-manager`, { enabled });
  return r.data.user;
}

/** DELETE /admin/users/:id → 204 */
export async function deleteUser(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/users/${encodeURIComponent(id)}`);
}

/** GET /admin/users/:id/sessions → { data: LoginSession[] } — that user's live logins (specs/055, #313). */
export async function listUserSessions(id: string): Promise<LoginSession[]> {
  const r = await adminReq<{ data: LoginSession[] | null }>("GET", `/admin/users/${encodeURIComponent(id)}/sessions`);
  return r.data ?? [];
}

/** DELETE /admin/users/:id/sessions/:sessionId → 204 — that device is refused on its next request. */
export async function endUserSession(id: string, sessionId: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/users/${encodeURIComponent(id)}/sessions/${encodeURIComponent(sessionId)}`);
}

// ──────────────────────────────────────────────
// ROLES (C4/C5) — GET/POST already in api.ts; expose convenience wrappers
// ──────────────────────────────────────────────

/** POST /admin/roles → { data: { role: RoleDetail } } */
export async function createRole(input: { name: string; label: string; actions?: RoleGrant[] }): Promise<RoleDetail> {
  const r = await adminReq<{ data: { role: RoleDetail } }>("POST", "/admin/roles", {
    name: input.name,
    label: input.label,
    actions: input.actions ?? [],
  });
  return r.data.role;
}

/** DELETE /admin/roles/:id → 204 */
export async function deleteRole(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/roles/${id}`);
}

// ──────────────────────────────────────────────
// TEAMS (C7/C8)
// Verified: GET/POST /admin/teams, GET/PUT/DELETE /admin/teams/:id,
//           GET/POST /admin/teams/:id/members, DELETE /admin/teams/:id/members/:userId
// ──────────────────────────────────────────────

export interface AdminTeam {
  id: string;
  tenantId: string;
  name: string;
  parentId?: string;
}

export interface AdminTeamMember {
  userId: string;
  teamId: string;
}

/** GET /admin/teams → { data: { teams: AdminTeam[] } } */
export async function listTeams(): Promise<AdminTeam[]> {
  const r = await adminReq<{ data: { teams: AdminTeam[] } }>("GET", "/admin/teams");
  return r.data?.teams ?? [];
}

/** GET /admin/teams/:id → { data: { team: AdminTeam } } */
export async function getTeam(id: string): Promise<AdminTeam> {
  const r = await adminReq<{ data: { team: AdminTeam } }>("GET", `/admin/teams/${id}`);
  return r.data.team;
}

/** POST /admin/teams → { data: { team: AdminTeam } } */
export async function createTeam(name: string, parentId?: string): Promise<AdminTeam> {
  const r = await adminReq<{ data: { team: AdminTeam } }>("POST", "/admin/teams", { name, parentId: parentId ?? null });
  return r.data.team;
}

/** PUT /admin/teams/:id → { data: { team: AdminTeam } }. Apply-when-present: an omitted key leaves
 *  the stored value; parentId "" detaches the team from the hierarchy. A parent that is the team
 *  itself or any of its descendants is refused 422 TEAM_PARENT_CYCLE — the ApiError message is
 *  the readable reason to show. */
export async function updateTeam(id: string, patch: { name?: string; parentId?: string }): Promise<AdminTeam> {
  const r = await adminReq<{ data: { team: AdminTeam } }>("PUT", `/admin/teams/${id}`, patch);
  return r.data.team;
}

/** DELETE /admin/teams/:id → 204 */
export async function deleteTeam(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/teams/${id}`);
}

/** GET /admin/teams/:id/members → { data: { members: AdminUser[] } } */
export async function listTeamMembers(teamId: string): Promise<AdminUser[]> {
  const r = await adminReq<{ data: { members: AdminUser[] } }>("GET", `/admin/teams/${teamId}/members`);
  return r.data?.members ?? [];
}

/** POST /admin/teams/:id/members → { data: { member: true, teamId, userId } } */
export async function addTeamMember(teamId: string, userId: string): Promise<void> {
  await adminReq<unknown>("POST", `/admin/teams/${teamId}/members`, { userId });
}

/** DELETE /admin/teams/:id/members/:userId → 204 */
export async function removeTeamMember(teamId: string, userId: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/teams/${teamId}/members/${userId}`);
}

// ──────────────────────────────────────────────
// AUDIT LOG v2 (C10 / W7 §11.2) — filter params + CSV export + date filters
// GET /audit-log?limit=&entityName=&action=&actorUserId=&actor=builder&dateFrom=&dateTo=&after=
//   • limit-bug FIXED backend-side (diff IS NULL no longer 500s); >10 rows work now.
//   • pagination.limit echoes the CLAMPED limit (max 200). Cursor via ?after=.
//   • Date filters use ONLY the dateFrom/dateTo family (YYYY-MM-DD). NEVER mix with from/to
//     (backend 400s AUDIT_INVALID_TIME). buildAuditQuery below deliberately omits from/to.
//   • Export (/audit-log/export.csv) reuses the SAME buildAuditQuery ⇒ identical active filters.
// ──────────────────────────────────────────────

export interface AuditFilter {
  entityName?: string;
  action?: string;
  actorUserId?: string;
  /** "builder" = only what a Nexus builder did in this workspace (server: actorLabel IS NOT NULL). */
  actor?: "builder";
  /** YYYY-MM-DD (UTC whole-day, inclusive lower bound). */
  dateFrom?: string;
  /** YYYY-MM-DD (UTC whole-day, exclusive at D+1). */
  dateTo?: string;
  limit?: number;
  after?: string;
}

/** Pure — build the shared filter query string (no limit/after; those are list-only paging). */
export function buildAuditQuery(filter: AuditFilter): string {
  const p = new URLSearchParams();
  if (filter.entityName) p.set("entityName", filter.entityName);
  if (filter.action) p.set("action", filter.action);
  if (filter.actorUserId) p.set("actorUserId", filter.actorUserId);
  if (filter.actor) p.set("actor", filter.actor);
  if (filter.dateFrom) p.set("dateFrom", filter.dateFrom);
  if (filter.dateTo) p.set("dateTo", filter.dateTo);
  return p.toString();
}

export interface AuditPage {
  entries: AuditEntry[];
  limit: number;
  nextCursor?: string;
}

export async function listAuditFiltered(filter: AuditFilter = {}): Promise<AuditPage> {
  const p = new URLSearchParams(buildAuditQuery(filter));
  p.set("limit", String(filter.limit ?? 100));
  if (filter.after) p.set("after", filter.after);
  const r = await adminReq<{ data: AuditEntry[]; pagination?: { limit?: number; nextCursor?: string } }>(
    "GET",
    `/audit-log?${p}`,
  );
  return {
    entries: r.data ?? [],
    limit: r.pagination?.limit ?? filter.limit ?? 100,
    nextCursor: r.pagination?.nextCursor,
  };
}

/** Download the audit trail as CSV, applying the SAME active filters as the on-screen list. */
export async function downloadAuditCsv(filter: AuditFilter): Promise<void> {
  const qs = buildAuditQuery(filter);
  const t = getToken();
  const res = await fetch(`${BASE}/audit-log/export.csv${qs ? `?${qs}` : ""}`, {
    headers: t ? { Authorization: `Bearer ${t}` } : {},
  });
  if (!res.ok) {
    const json = await res.json().catch(() => ({}));
    throw new ApiError(res.status, json.code ?? "EXPORT_FAILED", json.error ?? res.statusText, json.details ?? {});
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `audit-log-${new Date().toISOString().slice(0, 10)}.csv`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

// ──────────────────────────────────────────────
// FIELD-ACL admin CRUD (W7 §11.1) — config surface for A6 field-level security.
// GET returns only cells with a live effect; PUT is UPSERT-BY-KEY (absent key kept as-is);
// grant (true,true) DELETES the row. See lib/fieldAcl.ts for the pure matrix model.
// ──────────────────────────────────────────────

export interface FieldAclData {
  defaultPolicy: string;
  grants: FieldAclGrant[];
  /** Only with ?entity= (queue #306): fields a lock would strand a create on — the SERVER's predicate. */
  deadEndIfLocked?: string[];
}

/** GET /admin/field-acl?entity=&roleId= — both filters optional. */
export async function getFieldAclData(entity?: string, roleId?: string): Promise<FieldAclData> {
  const p = new URLSearchParams();
  if (entity) p.set("entity", entity);
  if (roleId) p.set("roleId", roleId);
  const qs = p.toString();
  const r = await adminReq<{ data: FieldAclData }>("GET", `/admin/field-acl${qs ? `?${qs}` : ""}`);
  return { defaultPolicy: r.data?.defaultPolicy ?? "allow", grants: r.data?.grants ?? [], deadEndIfLocked: r.data?.deadEndIfLocked };
}

/** Grants only — for callers that do not need the dead-end list. */
export async function getFieldAcl(entity?: string, roleId?: string): Promise<FieldAclGrant[]> {
  return (await getFieldAclData(entity, roleId)).grants;
}

/** PUT /admin/field-acl — bulk upsert-by-key. Send every CHANGED cell (buildPutPayload). */
export async function putFieldAcl(grants: FieldAclGrant[]): Promise<{ applied: number; cleared: number }> {
  const r = await adminReq<{ data: { applied: number; cleared: number } }>("PUT", "/admin/field-acl", { grants });
  return r.data ?? { applied: 0, cleared: 0 };
}

/** DELETE /admin/field-acl?roleId=&entity=&field= — drop restriction(s). */
export async function deleteFieldAcl(roleId: string, entity?: string, field?: string): Promise<number> {
  const p = new URLSearchParams({ roleId });
  if (entity) p.set("entity", entity);
  if (field) p.set("field", field);
  const r = await adminReq<{ data: { deleted: number } }>("DELETE", `/admin/field-acl?${p}`);
  return r.data?.deleted ?? 0;
}

// ──────────────────────────────────────────────
// INVITE / reset-password link (W7 §11.3) — mint a live reset link for a user.
// Reuses the forgot-password machinery; returns the raw token to the admin (already authed).
// ──────────────────────────────────────────────

/** POST /admin/users/:id/reset-password-link → 201 { resetLink, token, expiresAt, ttlMinutes, emailQueued }. */
export async function createResetLink(userId: string): Promise<ResetLinkResult> {
  const r = await adminReq<{ data: ResetLinkResult }>("POST", `/admin/users/${userId}/reset-password-link`, {});
  return r.data;
}

// ──────────────────────────────────────────────
// TEAM-SETS read-only (W7 §11.4) — sharing screens. ENGINE-DERIVED, no write surface.
// ──────────────────────────────────────────────

export interface AdminTeamSet {
  id: string;
  primaryTeamId: string | null;
  teams: { id: string; name: string }[];
  createdAt: string;
}

/** GET /admin/team-sets → { data: { teamSets: AdminTeamSet[] } }. */
export async function listTeamSets(): Promise<AdminTeamSet[]> {
  const r = await adminReq<{ data: { teamSets: AdminTeamSet[] } }>("GET", "/admin/team-sets");
  return r.data?.teamSets ?? [];
}

// ──────────────────────────────────────────────
// SSO PROVIDERS admin CRUD (ENG-AUTH2 #8, api-conventions §3c.4) — adminGated.
// Envelope: generic { data: ... } (NOT named-noun) — verified against sso_handler.go:
//   GET  → { data: SsoProvider[] } · POST → 201 { data: provider } · PATCH → { data: provider }
//   DELETE → 204. clientSecret KHÔNG BAO GIỜ được trả lại (ClientSecretEnc có json:"-").
// PATCH là bề mặt sửa DUY NHẤT và chỉ nhận {enabled?, trustIdpMfa?} (partial — field vắng mặt
// giữ nguyên; body rỗng → 422 SSO_PROVIDER_FIELD_REQUIRED). Đổi issuer/clientId/secret =
// xoá + tạo lại (BE contract, mirror FIELD_TYPE_IMMUTABLE-style narrow PATCH).
// ──────────────────────────────────────────────

export interface SsoProvider {
  id: string;
  tenantId: string;
  issuer: string;
  clientId: string;
  displayName?: string;
  enabled: boolean;
  jitProvisioning: boolean;
  /** SSO-MFA-POLICY (§3c.8): true = tin MFA của IdP, BỎ QUA MFA cục bộ cho provider này. */
  trustIdpMfa: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface CreateSsoProviderInput {
  /** URL https tuyệt đối tới OIDC IdP (BE kiểm SHAPE lúc tạo, discovery thật chạy lúc /start). */
  issuer: string;
  clientId: string;
  /** Chỉ gửi MỘT lần lúc tạo — server seal (AES-256-GCM) và không bao giờ echo lại. */
  clientSecret: string;
  displayName?: string;
  /** Mặc định false (SAFE DEFAULT). trustIdpMfa KHÔNG settable lúc tạo — phải PATCH sau (quyết định thứ hai). */
  jitProvisioning?: boolean;
}

export interface UpdateSsoProviderPatch {
  enabled?: boolean;
  trustIdpMfa?: boolean;
}

/** GET /admin/sso/providers → { data: SsoProvider[] } — kể cả provider đã tắt (admin cần thấy để bật lại). */
export async function listSsoProviders(): Promise<SsoProvider[]> {
  const r = await adminReq<{ data: SsoProvider[] }>("GET", "/admin/sso/providers");
  return r.data ?? [];
}

/** POST /admin/sso/providers → 201 { data: provider }. Lỗi: 422 SSO_PROVIDER_FIELD_REQUIRED /
 *  SSO_PROVIDER_ISSUER_INVALID · 409 SSO_PROVIDER_ALREADY_EXISTS · 503 SSO_NOT_CONFIGURED. */
export async function createSsoProvider(input: CreateSsoProviderInput): Promise<SsoProvider> {
  const r = await adminReq<{ data: SsoProvider }>("POST", "/admin/sso/providers", input);
  return r.data;
}

/** PATCH /admin/sso/providers/:id — partial: chỉ gửi key được cung cấp (absent = giữ nguyên).
 *  Cả hai vắng mặt → fail sớm client-side với ĐÚNG code BE sẽ trả (422 SSO_PROVIDER_FIELD_REQUIRED),
 *  khỏi tốn một round-trip chắc chắn lỗi. */
export async function updateSsoProvider(id: string, patch: UpdateSsoProviderPatch): Promise<SsoProvider> {
  const body: Record<string, boolean> = {};
  if (patch.enabled !== undefined) body.enabled = patch.enabled;
  if (patch.trustIdpMfa !== undefined) body.trustIdpMfa = patch.trustIdpMfa;
  if (Object.keys(body).length === 0) {
    throw new ApiError(
      422,
      "SSO_PROVIDER_FIELD_REQUIRED",
      "Cần ít nhất một trong hai trường 'enabled' hoặc 'trustIdpMfa'",
      { fields: ["enabled", "trustIdpMfa"] },
    );
  }
  const r = await adminReq<{ data: SsoProvider }>("PATCH", `/admin/sso/providers/${id}`, body);
  return r.data;
}

/** DELETE /admin/sso/providers/:id → 204. Hard-delete; FK CASCADE thu hồi mọi identity đã link. */
export async function deleteSsoProvider(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/sso/providers/${id}`);
}

// ──────────────────────────────────────────────
// API KEYS — máy-với-máy (APIKEY-1, api-conventions §3e)
// BE contract (nexus-platform/internal/handlers/admin_apikey_handler.go):
//   GET    /admin/api-keys     → { data: { apiKeys: ApiKey[] } } — KHÔNG BAO GIỜ chứa secret/hash.
//   POST   /admin/api-keys     → 201 { data: { apiKey, secret: "nxk_..." } } — secret CHỈ ở đây,
//                                server chỉ lưu SHA-256, không hiện lại được.
//   DELETE /admin/api-keys/:id → 204 — thu hồi mềm (revoked_at, giữ hàng cho audit), idempotent.
// Khoá luôn mint IsAdmin=false (privilege floor) — không bao giờ gọi được /admin/* bằng khoá.
// ──────────────────────────────────────────────

export interface ApiKey {
  id: string;
  tenantId: string;
  roleId: string;
  name: string;
  /** Lát public đầu của phần random — hiển thị `nxk_<prefix>…` để nhận diện, KHÔNG phải secret. */
  keyPrefix: string;
  createdBy?: string;
  lastUsedAt?: string;
  expiresAt?: string;
  revokedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface CreateApiKeyInput {
  name: string;
  roleId: string;
  /** RFC3339, tuỳ chọn — bỏ trống = không hết hạn. Quá khứ → 422 APIKEY_EXPIRY_INVALID. */
  expiresAt?: string;
}

/** GET /admin/api-keys — kể cả khoá đã thu hồi (admin cần thấy để audit). */
export async function listApiKeys(): Promise<ApiKey[]> {
  const r = await adminReq<{ data: { apiKeys: ApiKey[] } }>("GET", "/admin/api-keys");
  return r.data?.apiKeys ?? [];
}

/** POST /admin/api-keys → { apiKey, secret }. Secret hiện MỘT lần duy nhất — copy ngay.
 *  Lỗi: 422 APIKEY_NAME_REQUIRED / APIKEY_NAME_TOO_LONG / APIKEY_ROLE_INVALID / APIKEY_EXPIRY_INVALID. */
export async function createApiKey(input: CreateApiKeyInput): Promise<{ apiKey: ApiKey; secret: string }> {
  const r = await adminReq<{ data: { apiKey: ApiKey; secret: string } }>("POST", "/admin/api-keys", input);
  return r.data;
}

/** DELETE /admin/api-keys/:id → 204. Thu hồi mềm — khoá chết ngay ở request kế tiếp. 404 APIKEY_NOT_FOUND. */
export async function revokeApiKey(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/api-keys/${id}`);
}

// ──────────────────────────────────────────────
// MCP — connect an outside agent (issue #3)
// BE contract (nexus-platform/internal/handlers/admin_mcp_token_handler.go, builderGated):
//   GET    /admin/mcp-tokens     → { data: { tokens } } — the caller's live tokens, never a secret/hash.
//   POST   /admin/mcp-tokens     → 201 { data: { token, secret: "nxm_..." } } — secret here ONLY; at most 8 h.
//   DELETE /admin/mcp-tokens/:id → 204, idempotent; 404 MCP_TOKEN_NOT_FOUND for someone else's.
// The token works on /mcp only; the agent's proposals land in token.sessionId, the caller's assistant thread.
// ──────────────────────────────────────────────

export interface McpToken {
  id: string;
  /** The assistant thread the agent's proposals land in (GET /admin/ai/builder/sessions/:id). */
  sessionId: string;
  /** Public start of the token, shown as `nxm_<prefix>…`; not a secret. */
  tokenPrefix: string;
  expiresAt: string;
  lastUsedAt?: string;
  createdAt: string;
}

export async function listMcpTokens(): Promise<McpToken[]> {
  const r = await adminReq<{ data: { tokens: McpToken[] } }>("GET", "/admin/mcp-tokens");
  return r.data?.tokens ?? [];
}

/** The secret is in this answer only: call with a plain await, never a useMutation. */
export async function createMcpToken(): Promise<{ token: McpToken; secret: string }> {
  const r = await adminReq<{ data: { token: McpToken; secret: string } }>("POST", "/admin/mcp-tokens", {});
  return r.data;
}

export async function revokeMcpToken(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/mcp-tokens/${encodeURIComponent(id)}`);
}

/** Where an agent connects: the API's origin + /mcp (the one-command demo serves both on one origin; on `next dev` with a relative
 * API base it is the dev origin, and app/mcp proxies it to the API). */
export function mcpEndpoint(): string {
  return `${new URL(BASE, window.location.origin).origin}/mcp`;
}

/** The exact Claude Code line that connects this workspace's MCP endpoint with `secret`. */
export function claudeMcpAddCommand(secret: string, endpoint = mcpEndpoint()): string {
  return `claude mcp add --transport http nexus ${endpoint} --header "Authorization: Bearer ${secret}"`;
}

// ──────────────────────────────────────────────
// KHO KHOÁ KẾT NỐI — named credentials (specs/050 S8a)
// BE contract (nexus-platform/internal/handlers/admin_credential_handler.go, adminGated):
//   GET    /admin/credentials      → { data: { credentials } } — KHÔNG BAO GIỜ secret/envelope.
//   GET    /admin/credentials/:id  → { data: { credential, usages } }
//   POST   /admin/credentials      → 201 { data: { credential, secret? } } — secret CHỈ có khi Nexus
//                                    tự sinh (hmac), hiện một lần; khoá tự nhập không bao giờ trả lại.
//   PUT    /admin/credentials/:id  → { data: { credential } } — nhãn/địa chỉ (cùng máy chủ)/header/username.
//   POST   /admin/credentials/:id/rotate → { data: { credential, secret? } } (S8b).
//   DELETE /admin/credentials/:id  → 204; 409 CREDENTIAL_IN_USE kèm details.usages; 503 thiếu khoá chủ.
// ──────────────────────────────────────────────

export type CredentialAuthType = "header" | "basic" | "hmac" | "url_token";
export type CredentialStatus = "ready" | "needs_secret" | "grace" | "key_unavailable";

export interface NamedCredential {
  id: string;
  name: string;
  label: string;
  baseUrl: string;
  authType: CredentialAuthType;
  headerName: string;
  headerPrefix: string;
  username: string;
  status: CredentialStatus;
  /** `…abcd` — 4 ký tự cuối, không phải secret. */
  secretHint: string;
  extraHeaderNames: string[];
  rotatedAt: string | null;
  previousExpiresAt: string | null;
  usageCount: number;
  createdAt: string;
  updatedAt: string;
}

export interface CredentialUsage {
  workflowId: string;
  workflowName: string;
  actionIndex: number;
}

export interface CreateCredentialInput {
  name: string;
  label?: string;
  baseUrl: string;
  authType: CredentialAuthType;
  headerName?: string;
  headerPrefix?: string;
  username?: string;
  secret?: string;
  generateSecret?: boolean;
  extraHeaders?: Record<string, string>;
}

export async function listCredentials(): Promise<NamedCredential[]> {
  const r = await adminReq<{ data: { credentials: NamedCredential[] } }>("GET", "/admin/credentials");
  return r.data?.credentials ?? [];
}

export async function getCredential(id: string): Promise<{ credential: NamedCredential; usages: CredentialUsage[] }> {
  const r = await adminReq<{ data: { credential: NamedCredential; usages: CredentialUsage[] } }>("GET", `/admin/credentials/${id}`);
  return { credential: r.data.credential, usages: r.data.usages ?? [] };
}

/** `secret` có mặt CHỈ khi Nexus tự sinh khoá hmac — hiện một lần. 422 kèm `details.field`; 409 CREDENTIAL_NAME_TAKEN. */
export async function createCredential(input: CreateCredentialInput): Promise<{ credential: NamedCredential; secret?: string }> {
  const r = await adminReq<{ data: { credential: NamedCredential; secret?: string } }>("POST", "/admin/credentials", input);
  return r.data;
}

export interface UpdateCredentialInput {
  label?: string;
  baseUrl?: string;
  headerName?: string;
  headerPrefix?: string;
  username?: string;
}

/** PUT /admin/credentials/:id — chỉ trường không bí mật. 422 CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE khi đổi sang máy chủ khác. */
export async function updateCredential(id: string, input: UpdateCredentialInput): Promise<NamedCredential> {
  const r = await adminReq<{ data: { credential: NamedCredential } }>("PUT", `/admin/credentials/${id}`, input);
  return r.data.credential;
}

export interface RotateCredentialInput {
  secret?: string;
  generateSecret?: boolean;
  /** Có mặt = THAY toàn bộ header bổ sung (rỗng = xoá hết); vắng = giữ nguyên. */
  extraHeaders?: Record<string, string>;
  /** Chỉ HMAC có khoá mới; 0..168 giờ. */
  graceHours?: number;
  baseUrl?: string;
  username?: string;
}

/** POST /admin/credentials/:id/rotate — `secret` trả về CHỈ khi Nexus tự sinh (hiện một lần, Cache-Control: no-store). */
export async function rotateCredential(id: string, input: RotateCredentialInput): Promise<{ credential: NamedCredential; secret?: string }> {
  const r = await adminReq<{ data: { credential: NamedCredential; secret?: string } }>("POST", `/admin/credentials/${id}/rotate`, input);
  return r.data;
}

/** 204. 409 CREDENTIAL_IN_USE → `ApiError.details.usages` liệt kê quy trình đang dùng. */
export async function deleteCredential(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/credentials/${id}`);
}

// ──────────────────────────────────────────────
// BUSINESS CALENDARS — lịch làm việc (CALENDAR-1, api-conventions §17)
// BE contract (nexus-platform/internal/handlers/admin_calendar_handler.go):
//   GET    /admin/business-calendars      → { data: { calendars: BusinessCalendar[] } }
//   POST   /admin/business-calendars      → 201 { data: { calendar } }
//   PUT    /admin/business-calendars/:id  → { data: { calendar } } — full-replace
//   DELETE /admin/business-calendars/:id  → 204 (soft delete)
// hours: key mon..sun, mỗi ngày là mảng khoảng {start,end} "HH:MM" LOCAL theo timezone
// (end được phép "24:00"); holidays: "YYYY-MM-DD". isDefault: ≤1 lịch/tenant — set ở đây,
// server tự bỏ cờ lịch cũ. Tenant CHƯA có lịch default → SLA engine dùng wall-clock.
// ──────────────────────────────────────────────

export interface CalendarInterval {
  start: string;
  end: string;
}

export type CalendarHours = Record<string, CalendarInterval[]>;

export interface BusinessCalendar {
  id: string;
  tenantId: string;
  name: string;
  timezone: string;
  hours: CalendarHours;
  holidays: string[];
  isDefault: boolean;
  createdBy?: string | null;
  updatedBy?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface BusinessCalendarInput {
  name: string;
  timezone: string;
  hours: CalendarHours;
  holidays?: string[];
  isDefault?: boolean;
}

/** GET /admin/business-calendars — default xếp đầu. */
export async function listBusinessCalendars(): Promise<BusinessCalendar[]> {
  const r = await adminReq<{ data: { calendars: BusinessCalendar[] } }>("GET", "/admin/business-calendars");
  return r.data?.calendars ?? [];
}

/** POST /admin/business-calendars → 201. Lỗi: 422 CALENDAR_TIMEZONE_INVALID /
 *  CALENDAR_HOURS_INVALID / CALENDAR_HOURS_EMPTY / CALENDAR_HOLIDAYS_INVALID /
 *  CALENDAR_NAME_REQUIRED, 409 CALENDAR_ALREADY_EXISTS. */
export async function createBusinessCalendar(input: BusinessCalendarInput): Promise<BusinessCalendar> {
  const r = await adminReq<{ data: { calendar: BusinessCalendar } }>("POST", "/admin/business-calendars", input);
  return r.data.calendar;
}

/** PUT /admin/business-calendars/:id — full-replace. 404 CALENDAR_NOT_FOUND. */
export async function updateBusinessCalendar(id: string, input: BusinessCalendarInput): Promise<BusinessCalendar> {
  const r = await adminReq<{ data: { calendar: BusinessCalendar } }>("PUT", `/admin/business-calendars/${id}`, input);
  return r.data.calendar;
}

/** DELETE /admin/business-calendars/:id → 204 (soft). Xoá lịch default → tenant hết default (SLA về wall-clock). */
export async function deleteBusinessCalendar(id: string): Promise<void> {
  await adminReq<unknown>("DELETE", `/admin/business-calendars/${id}`);
}

// ──────────────────────────────────────────────
// VIỆC NỀN — "Việc nền" của quản trị khách (DAILY-QUEUE #31–#33, specs/021 §3, api-conventions §16)
// BE contract (nexus-platform/internal/handlers/admin_jobs_handler.go), adminGated (NOT operator):
//   GET  /admin/jobs?type=&status=&limit=&after= → { data: BackgroundJob[], pagination: { limit, nextCursor } }
//        newest first, keyset cursor; never a payload — lastError is stored already masked.
//   POST /admin/jobs/:id/retry → 200 { data: { job } } — the SAME row back in the queue (same
//        X-Nexus-Delivery-Id). 404 JOB_NOT_FOUND · 409 JOB_NOT_RETRYABLE (only a failed
//        outbound_webhook) · 503 JOBS_DISABLED.
// ──────────────────────────────────────────────

export type BackgroundJobStatus = "queued" | "running" | "done" | "failed";

export interface BackgroundJob {
  id: string;
  jobType: string;
  status: BackgroundJobStatus;
  attempts: number;
  maxAttempts: number;
  /** The receiver's / handler's error as stored (masked at the source: origin + HTTP status, no secret). */
  lastError: string | null;
  /** The rule the job came from (null for a job no rule produced). */
  workflowId: string | null;
  /** NAME of the vault key a webhook job used (never the secret); null = none / deleted; absent on a server without #342. */
  credentialName?: string | null;
  runAt: string;
  createdAt: string;
  finishedAt: string | null;
}

export interface BackgroundJobFilter {
  type?: string;
  status?: BackgroundJobStatus;
  limit?: number;
  after?: string;
}

export interface BackgroundJobPage {
  jobs: BackgroundJob[];
  nextCursor?: string;
}

export async function listBackgroundJobs(filter: BackgroundJobFilter = {}): Promise<BackgroundJobPage> {
  const p = new URLSearchParams();
  if (filter.type) p.set("type", filter.type);
  if (filter.status) p.set("status", filter.status);
  p.set("limit", String(filter.limit ?? 50));
  if (filter.after) p.set("after", filter.after);
  const r = await adminReq<{ data: BackgroundJob[]; pagination?: { nextCursor?: string | null } }>(
    "GET",
    `/admin/jobs?${p}`,
  );
  return { jobs: r.data ?? [], nextCursor: r.pagination?.nextCursor ?? undefined };
}

/** "Gửi lại" one failed outbound_webhook of the tenant; returns the requeued row. */
export async function retryBackgroundJob(id: string): Promise<BackgroundJob> {
  const r = await adminReq<{ data: { job: BackgroundJob } }>("POST", `/admin/jobs/${encodeURIComponent(id)}/retry`);
  return r.data.job;
}

// ──────────────────────────────────────────────
// NHẬT KÝ EMAIL — email mà luật tự động đã gửi / bị lỗi (specs/066 L1b)
// BE contract (nexus-platform/internal/handlers/admin_email_log_handler.go), adminGated (NOT operator):
//   GET /admin/email-log?status=&source=&limit=&after= → { data: EmailLogEntry[], pagination: { limit, nextCursor },
//        meta: { mailConfigured, queueEnabled } } — newest first, keyset cursor. Never an address, a rendered
//        subject or a body: recipientMasked is "l***@gmail.com", subjectTemplate is the RULE's template (or the
//        fixed reset-mail title) and lastError is masked at the source. 400 EMAIL_LOG_FILTER_INVALID · 400 INVALID_CURSOR.
//        specs/066 L3: rows also come from a forgot-password request and from a link a tenant admin issued.
// ──────────────────────────────────────────────

export type EmailLogStatus = "queued" | "sent" | "failed";
/** What produced the mail: an automation rule, "Quên mật khẩu", or a link the customer's admin issued. */
export type EmailLogSource = "workflow" | "password_reset" | "reset_link";

export interface EmailLogEntry {
  id: string;
  createdAt: string;
  finishedAt: string | null;
  source: EmailLogSource;
  status: EmailLogStatus;
  recipientMasked: string;
  subjectTemplate: string | null;
  workflowId: string | null;
  /** The rule's current name; null once the rule was deleted. */
  workflowName: string | null;
  actionIndex: number | null;
  entityName: string | null;
  recordId: string | null;
  jobId: string | null;
  attempts: number;
  lastError: string | null;
  /** Set on a row that IS a resend: the failed row it replaces. */
  resendOf: string | null;
  /** Set on a failed row that was already resent (each row can be resent once). */
  resentAt: string | null;
  /** Who issued a reset link / pressed "Gửi lại" (user id); null on a rule's or a forgot-password mail. */
  createdBy: string | null;
  /** That person's name when they are a user of this customer; null with a createdBy = the builder ("Người dựng"). */
  createdByName: string | null;
  /** The server's resend policy: a failed, not-yet-resent rule mail whose rule + record still exist. */
  canResend: boolean;
}

export interface EmailLogFilter {
  status?: EmailLogStatus;
  source?: EmailLogSource;
  limit?: number;
  after?: string;
}

export interface EmailLogPage {
  entries: EmailLogEntry[];
  nextCursor?: string;
  /** false = the server has no mail configured: every row is a mail that was never delivered. */
  mailConfigured: boolean;
  queueEnabled: boolean;
}

export async function listEmailLog(filter: EmailLogFilter = {}): Promise<EmailLogPage> {
  const p = new URLSearchParams();
  if (filter.status) p.set("status", filter.status);
  if (filter.source) p.set("source", filter.source);
  p.set("limit", String(filter.limit ?? 50));
  if (filter.after) p.set("after", filter.after);
  const r = await adminReq<{
    data: EmailLogEntry[];
    pagination?: { nextCursor?: string | null };
    meta?: { mailConfigured?: boolean; queueEnabled?: boolean };
  }>("GET", `/admin/email-log?${p}`);
  return {
    entries: r.data ?? [],
    nextCursor: r.pagination?.nextCursor ?? undefined,
    mailConfigured: r.meta?.mailConfigured ?? true,
    queueEnabled: r.meta?.queueEnabled ?? true,
  };
}

// "Gửi lại" a failed rule mail (specs/066 L2). The mail is REBUILT on the server from the live rule and the
// record's current data; the body can name nothing else. dryRun absent/true = a PREVIEW (nothing sent);
// {dryRun:false} sends. Each failed row can be resent once: the loser of a double press gets 409
// EMAIL_NOT_RESENDABLE. Other codes: 404 EMAIL_LOG_NOT_FOUND · 409 EMAIL_RESEND_RULE_GONE |
// EMAIL_RESEND_RULE_DISABLED | EMAIL_RESEND_RECORD_GONE · 422 EMAIL_RESEND_NO_RECIPIENT · 503 MAIL_NOT_CONFIGURED · 500 EMAIL_RESEND_FAILED.
export interface EmailResendPreview {
  dryRun: true;
  recipientMasked: string;
  /** The subject as it WILL be sent (rendered from the record's current data). */
  subject: string;
  /** The rule's templates differ from the ones this mail was first sent with. */
  ruleChangedSinceSend: boolean;
}
export interface EmailResendSent {
  dryRun: false;
  /** The new log row (null only if the server's best-effort log write was lost). */
  entry: EmailLogEntry | null;
}

// ── Mức dùng (specs/066 L4) ──
// GET /admin/usage?from=YYYY-MM-DD&to=YYYY-MM-DD → { data: UsageReport }. The day list is dense (an empty
// day is a row of zeros); today is counted live, earlier days come from the hourly roll-up.
export interface UsageCounts {
  workflowRuns: number;
  workflowRunsFailed: number;
  emailsSent: number;
  emailsFailed: number;
  webhooksDelivered: number;
  webhooksFailed: number;
  hooksReceived: number;
  hooksFailed: number;
  /** Distinct people who signed in that day (per day only: the totals row carries 0 — see current.usersSeen30d). */
  usersSeen: number;
}

/** The "Hiện tại" block (specs/066 L5): a snapshot taken by the hourly roll-up, or counted live when none yet. */
export interface UsageCurrent {
  usersActive: number;
  usersSeen30d: number;
  recordsTotal: number;
  /** Live rows per table, labelled with the business name from the customer's own metadata. */
  records: { entity: string; label: string; count: number }[];
  /** Attachment bytes, binned files included until they are deleted for good. */
  fileBytes: number;
  fileCount: number;
  computedAt: string;
}

export interface UsageDay extends UsageCounts {
  /** YYYY-MM-DD in the server's APP_TIMEZONE. */
  day: string;
}

export interface UsageReport {
  timezone: string;
  from: string;
  to: string;
  /** false = the server's hourly roll-up is off: only today is counted. */
  rollupEnabled: boolean;
  /** ISO time of the last roll-up of this customer's figures; null when none yet. */
  lastRollupAt: string | null;
  days: UsageDay[];
  totals: UsageCounts;
  current?: UsageCurrent;
}

export async function getUsage(from?: string, to?: string): Promise<UsageReport> {
  const p = new URLSearchParams();
  if (from) p.set("from", from);
  if (to) p.set("to", to);
  const q = p.toString();
  const r = await adminReq<{ data: UsageReport }>("GET", `/admin/usage${q ? `?${q}` : ""}`);
  return r.data;
}

// BE contract (nexus-platform/internal/services/tenant_health.go), adminGated: GET /admin/health.
export type HealthLevel = "ok" | "warn" | "critical";
export interface TenantHealth {
  status: HealthLevel;
  generatedAt: string;
  queueEnabled: boolean;
  jobs: { failed7d: { jobType: string; count: number; lastAt: string }[]; stuck: number };
  workflowRuns: {
    failed24h: number;
    failed7d: number;
    topFailing: { workflowId: string; name: string; failed: number; lastAt: string; lastError: string }[];
  };
  emails: { failed24h: number; failed7d: number; unresentFailed: number };
  inboundHooks: { failed24h: number; byHook: { hookId: string; name: string; failed: number; lastAt: string }[] };
  doctor: { status: HealthLevel | "unknown"; critical: number; warn: number; checkedAt: string | null };
}

export async function getTenantHealth(): Promise<TenantHealth> {
  const r = await adminReq<{ data: TenantHealth }>("GET", "/admin/health");
  return r.data;
}

export async function resendEmail(id: string): Promise<EmailResendPreview>;
export async function resendEmail(id: string, dryRun: false): Promise<EmailResendSent>;
export async function resendEmail(id: string, dryRun?: boolean): Promise<EmailResendPreview | EmailResendSent> {
  const r = await adminReq<{ data: EmailResendPreview | EmailResendSent }>(
    "POST",
    `/admin/email-log/${encodeURIComponent(id)}/resend`,
    dryRun === false ? { dryRun: false } : {},
  );
  return r.data;
}

// ──────────────────────────────────────────────
// NOTIFICATIONS (full page — not just bell)
// GET /notifications?page=&limit= → { data: ApiNotification[], pagination: {...} }
// POST /notifications/read-all → 200
// POST /notifications/:id/read → 200
// ──────────────────────────────────────────────

// ApiNotification is imported at top and re-exported in the export block above.

export interface NotificationsPage {
  items: ApiNotification[];
  pagination: { page: number; limit: number; total: number; totalPages: number };
}

export async function listNotificationsPage(page = 1, limit = 30, unreadOnly = false): Promise<NotificationsPage> {
  const p = new URLSearchParams({ page: String(page), limit: String(limit) });
  if (unreadOnly) p.set("unread", "true");
  const r = await adminReq<{ data: ApiNotification[]; pagination?: { page?: number; limit?: number; total?: number; totalPages?: number } }>(
    "GET", `/notifications?${p}`,
  );
  const items = Array.isArray(r.data) ? r.data : [];
  const pg = r.pagination ?? {};
  return {
    items,
    pagination: {
      page: pg.page ?? page,
      limit: pg.limit ?? limit,
      total: pg.total ?? items.length,
      totalPages: pg.totalPages ?? 1,
    },
  };
}

export async function markNotificationRead(id: string): Promise<void> {
  await adminReq<unknown>("POST", `/notifications/${id}/read`, {});
}

export async function markAllNotificationsRead(): Promise<void> {
  await adminReq<unknown>("POST", "/notifications/read-all", {});
}

// ──────────────────────────────────────────────
// RBAC MATRIX MODEL — pure functions (no IO), used by ACLMatrix widget + tests
// ──────────────────────────────────────────────

// "field:<tên trường>" (#283) = hồ sơ mà trường Người dùng đó trỏ tới tôi (cộng hồ sơ tôi tạo).
export type FieldAccessLevel = `field:${string}`;
export type AccessLevel = "none" | "owner" | "team" | "all" | FieldAccessLevel;
/** The four levels every row/column menu offers; a field level is per-entity so it is picked cell by cell. */
export type BaseAccessLevel = Exclude<AccessLevel, FieldAccessLevel>;
export type ActionVerb = "view" | "create" | "edit" | "delete";
export const ACTION_VERBS: ActionVerb[] = ["view", "create", "edit", "delete"];
export const ACCESS_LEVELS: BaseAccessLevel[] = ["none", "owner", "team", "all"];

/** Level for "records whose user field `field` is me". */
export const fieldLevel = (field: string): FieldAccessLevel => `field:${field}`;
/** The user-field name inside a "field:<name>" level, or null for any other level. */
export const userFieldOf = (level: AccessLevel): string | null =>
  level.startsWith("field:") ? level.slice("field:".length) : null;

// Sparse matrix key: "<entityName>:<action>"
export type MatrixKey = string;
export type MatrixState = Record<MatrixKey, AccessLevel>;

/** Build a MatrixKey */
export const matKey = (entity: string, action: ActionVerb): MatrixKey => `${entity}:${action}`;

/** Convert a flat grant array from the API into a sparse MatrixState (omit "none" entries). */
export function grantsToMatrix(grants: RoleGrant[]): MatrixState {
  const m: MatrixState = {};
  for (const g of grants) {
    if (g.accessLevel !== "none") {
      m[matKey(g.entityName, g.action as ActionVerb)] = g.accessLevel as AccessLevel;
    }
  }
  return m;
}

/** Convert a MatrixState back into a grant array for PUT /admin/roles/:id (replace-set).
 *  Omit "none" cells — sending none = revoking (backend semantics). */
export function matrixToGrants(matrix: MatrixState): RoleGrant[] {
  return Object.entries(matrix)
    .filter(([, level]) => level !== "none")
    .map(([key, level]) => {
      const [entityName, action] = key.split(":");
      return {
        entityName,
        action: action as RoleActionVerb,
        accessLevel: level as RoleAccessLevel,
      };
    });
}

/** Read the effective access level for a cell, falling back to wildcard (*) if entity row absent. */
export function cellLevel(matrix: MatrixState, entity: string, action: ActionVerb): AccessLevel {
  const specific = matrix[matKey(entity, action)];
  if (specific) return specific;
  const wildcard = matrix[matKey("*", action)];
  if (wildcard) return wildcard;
  return "none";
}

/** Set a single cell in a copy of the matrix. Pass "none" to clear (will be omitted on serialization). */
export function setCell(matrix: MatrixState, entity: string, action: ActionVerb, level: AccessLevel): MatrixState {
  const next = { ...matrix };
  const k = matKey(entity, action);
  if (level === "none") {
    delete next[k];
  } else {
    next[k] = level;
  }
  return next;
}

/** Set an entire row (all actions) for one entity to the same level. */
export function setRow(matrix: MatrixState, entity: string, level: AccessLevel): MatrixState {
  let next = { ...matrix };
  for (const a of ACTION_VERBS) {
    next = setCell(next, entity, a, level);
  }
  return next;
}

/** Set an entire column (all entities in the given list) for one action to the same level. */
export function setColumn(matrix: MatrixState, entities: string[], action: ActionVerb, level: AccessLevel): MatrixState {
  let next = { ...matrix };
  for (const e of entities) {
    next = setCell(next, e, action, level);
  }
  return next;
}

/** Check if two MatrixState objects are equivalent (same grants). */
export function matrixEqual(a: MatrixState, b: MatrixState): boolean {
  const keysA = Object.keys(a).filter((k) => a[k] !== "none");
  const keysB = Object.keys(b).filter((k) => b[k] !== "none");
  if (keysA.length !== keysB.length) return false;
  for (const k of keysA) {
    if (a[k] !== b[k]) return false;
  }
  return true;
}
