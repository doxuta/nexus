"use client";

// Chạy thử › Vai: the roles of THIS project. The key carries the project code (useProjectQuery), so project A's
// roles are never painted for project B; fresh so a role added in "Người dùng & quyền" shows on return.
import { listRoles } from "@/lib/api";
import { useProjectQuery } from "../useProjectQuery";

export const trialRolesKey = ["studio", "trial", "roles"] as const;

export function useTrialRoles(slug: string) {
  return useProjectQuery(slug, trialRolesKey, listRoles, { fresh: true });
}
