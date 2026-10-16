"use client";

// "Xem như vai này" hook shared by the role page (specs/074 lát 15-17). The pages themselves are build/AccessPage,
// RolePage, FieldAclPage, TeamsPage and SharingPage.
import { adminApps } from "@/lib/api";
import { previewInWorkspace, viewAsRole } from "@/lib/studioViewAs";
import { inWorkspace } from "@/lib/workspace";
import { useProjectQuery } from "./useProjectQuery";

/** "Xem như vai này" for the role page: previews in the default app (else the first); hidden where a preview cannot start. */
export function useRoleViewAs(slug: string) {
  const apps = useProjectQuery(slug, ["apps"], adminApps);
  // ponytail: previews in the default app (else the first); a picker per app waits for f2-access-matrix.
  const appList = apps.data ?? [];
  const previewApp = appList.find((a) => a.isDefault) ?? appList[0];
  return previewApp && !(inWorkspace() && !previewInWorkspace)
    ? { appLabel: previewApp.label || previewApp.name, open: (r: { id: string }) => viewAsRole({ roleId: r.id, appId: previewApp.id, appSlug: previewApp.name }) }
    : undefined;
}
