// lib/orgAdmin.ts — the customer owner's "Cài đặt › Người dùng" API (specs/065-customer-owner-tier, lát B).
// /org-admin/* is NOT under /admin: the owner is not an admin. The server (RequireUserManager) re-reads the
// owner flag on every request; nothing here is a permission check.

import { apiRequest, type LoginSession } from "@/lib/api";
import type { ResetLinkResult } from "@/lib/invite";

export type OrgTier = "staff" | "owner" | "admin";

/** What an owner sees of an account: no operator flag, no tenant id. `manageable=false` rows are read-only. */
export interface OrgUser {
  id: string;
  email: string;
  fullName?: string;
  status: string;
  roleId?: string;
  tier: OrgTier;
  manageable: boolean;
}

/** A role in the owner's picker. `assignable=false` (the owner role) is listed but cannot be given. */
export interface OrgRole { id: string; label: string; assignable: boolean }

/** Everything an owner may change. `roleId: ""` clears the role. */
export interface OrgUpdateInput {
  fullName?: string;
  status?: "active" | "inactive";
  roleId?: string;
}

/** GET /org-admin/users */
export async function listOrgUsers(): Promise<OrgUser[]> {
  const r = await apiRequest<{ data: { users: OrgUser[] } }>("GET", "/org-admin/users");
  return r.data?.users ?? [];
}

/** GET /org-admin/users/:id */
export async function getOrgUser(id: string): Promise<OrgUser> {
  const r = await apiRequest<{ data: { user: OrgUser } }>("GET", `/org-admin/users/${encodeURIComponent(id)}`);
  return r.data.user;
}

/** PUT /org-admin/users/:id */
export async function updateOrgUser(id: string, input: OrgUpdateInput): Promise<OrgUser> {
  const r = await apiRequest<{ data: { user: OrgUser } }>("PUT", `/org-admin/users/${encodeURIComponent(id)}`, input);
  return r.data.user;
}

/** What an owner supplies to add staff. No password: the server generates one and never returns it. */
export interface OrgCreateInput { email: string; fullName?: string; roleId?: string }

/** POST /org-admin/users → 201, always a plain active staff account. */
export async function createOrgUser(input: OrgCreateInput): Promise<OrgUser> {
  const r = await apiRequest<{ data: { user: OrgUser } }>("POST", "/org-admin/users", input);
  return r.data.user;
}

/** POST /org-admin/users/:id/reset-password-link → the one-shot invite link (same shape as the admin's). */
export async function issueOrgInviteLink(id: string): Promise<ResetLinkResult> {
  const r = await apiRequest<{ data: ResetLinkResult }>("POST", `/org-admin/users/${encodeURIComponent(id)}/reset-password-link`, {});
  return r.data;
}

/** GET /org-admin/users/:id/sessions */
export async function listOrgUserSessions(id: string): Promise<LoginSession[]> {
  const r = await apiRequest<{ data: LoginSession[] | null }>("GET", `/org-admin/users/${encodeURIComponent(id)}/sessions`);
  return r.data ?? [];
}

/** DELETE /org-admin/users/:id/sessions/:sessionId → 204. */
export async function endOrgUserSession(id: string, sessionId: string): Promise<void> {
  await apiRequest<unknown>("DELETE", `/org-admin/users/${encodeURIComponent(id)}/sessions/${encodeURIComponent(sessionId)}`);
}

/** GET /org-admin/roles */
export async function listOrgRoles(): Promise<OrgRole[]> {
  const r = await apiRequest<{ data: { roles: OrgRole[] } }>("GET", "/org-admin/roles");
  return r.data?.roles ?? [];
}
