"use client";

// RolePage (specs/074 lát f2-role-page, T179): ONE role on its own URL (dung/nguoi-dung-quyen/vai/<id>) — the role's
// header and full matrix on the left (RoleEditor), "Quyền theo trường của vai này (n)" in the 300px column on the right.
// Lưu/Huỷ are the page's UnsavedBar (FR-072); delete is confirmed in the page; leaving while unsaved is stopped by
// the navigation blocker armed inside the matrix.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { RoleEditor } from "@/components/screens/admin/RoleEditor";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { getFieldAclData } from "@/lib/admin";
import { fill } from "@/components/slds/DataTable";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { useRoleViewAs } from "../AccessPages";
import { SubPage } from "../AutomationPage";
import { UnsavedBar } from "../UnsavedBar";
import { useProjectQuery } from "../useProjectQuery";

export function RolePage({ slug, roleId }: { slug: string; roleId: string }) {
  const router = useRouter();
  const viewAs = useRoleViewAs(slug);
  const fields = useProjectQuery(slug, ["fieldAcl", "role", roleId], () => getFieldAclData(undefined, roleId));
  // Only the cells that differ from "can see and edit" are stored, so every grant here is a restriction.
  const locked = fields.data?.grants ?? [];
  const back = studioHref(slug, "dung/nguoi-dung-quyen");
  return (
    <SubPage back={{ label: t("studio.route.access"), href: back }}>
      <div className="flex flex-col items-stretch gap-4 lg:flex-row lg:items-start">
        <div className="min-w-0 flex-1 px-4">
          <RoleEditor
            key={roleId}
            roleId={roleId}
            viewAs={viewAs}
            inlineDelete
            onDeleted={() => router.push(back)}
            emptyNote={
              <ScopedNotification>
                {t("studio.build.access.role.emptyGrants")} <a href="#ma-tran">{t("studio.build.access.role.emptyGrantsLink")}</a>
              </ScopedNotification>
            }
            renderBar={(s) => <UnsavedBar dirty={s.dirty} saving={s.saving} error={s.error} onSave={s.save} onCancel={s.reset} />}
          />
        </div>
        <aside
          className="w-full shrink-0 px-4 lg:w-[300px] lg:pl-0"
          aria-label={fill(t("studio.build.access.role.fieldsTitle"), { n: locked.length })}
        >
          <h2 className="slds-text-heading_small slds-m-bottom_x-small">{fill(t("studio.build.access.role.fieldsTitle"), { n: locked.length })}</h2>
          {fields.isError ? (
            <p className="slds-text-color_error">{t("studio.build.access.role.fieldsError")}</p>
          ) : locked.length === 0 ? (
            <p className="slds-text-color_weak">{t("studio.build.access.role.fieldsEmpty")}</p>
          ) : (
            <ul className="slds-has-dividers_bottom">
              {locked.map((g) => (
                <li key={`${g.entityName}.${g.fieldName}`} className="slds-item slds-grid slds-grid_align-spread">
                  {/* ponytail: raw table.field codes; business labels need one metadata read per table. */}
                  <span className="slds-truncate">{`${g.entityName}.${g.fieldName}`}</span>
                  <span className="slds-text-color_weak">
                    {g.canView ? t("studio.build.access.role.fieldsReadonly") : t("studio.build.access.role.fieldsHidden")}
                  </span>
                </li>
              ))}
            </ul>
          )}
          <p className="slds-m-top_small">
            <Link href={studioHref(slug, "dung/nguoi-dung-quyen/quyen-truong")}>{t("studio.build.access.role.fieldsOpen")}</Link>
          </p>
        </aside>
      </div>
    </SubPage>
  );
}
