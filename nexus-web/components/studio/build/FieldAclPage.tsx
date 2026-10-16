"use client";

// FieldAclPage (specs/074 lát f2-access-field-acl, T207): "Quyền theo trường" on its own URL
// (dung/nguoi-dung-quyen/quyen-truong?bang=&truong=). The URL is the source of truth: it picks the table and the row,
// and a table picked in the screen is written back with replace (Back does not step through every table). A table or
// field that is not in the project says so instead of falling back to the first table.
import { usePathname, useRouter } from "next/navigation";
import { FieldACL } from "@/components/screens/admin/FieldACL";
import { listEntities, listFields } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { SubPage } from "../AutomationPage";
import { QueryState } from "../QueryState";
import { useProjectQuery } from "../useProjectQuery";

export function FieldAclPage({ slug, bang, truong }: { slug: string; bang?: string; truong?: string }) {
  const router = useRouter();
  const pathname = usePathname();
  const entities = useProjectQuery(slug, ["fieldAcl", "entities"], listEntities);
  const known = bang ? entities.data?.some((e) => e.name === bang) : true;
  // Only read when the URL names a field (to check it exists): the screen reads the table's fields itself.
  const fields = useProjectQuery(slug, ["fieldAcl", "fields", bang ?? "", truong ?? ""], () => (bang && truong ? listFields(bang) : Promise.resolve([])), { refetchOnWindowFocus: false });
  const back = studioHref(slug, "dung/nguoi-dung-quyen", {}, bang ? { bang } : undefined);
  const unknown = known === false || (truong != null && known === true && fields.data != null && !fields.data.some((f) => f.name === truong));
  const pending = !entities.data || (truong != null && bang != null && !fields.data && !fields.isError);
  return (
    <SubPage back={{ label: t("studio.route.access"), href: back }}>
      <QueryState query={entities}>
        {pending ? null : unknown ? (
          <div className="slds-p-around_large">
            <p className="nx-lede">{known === false ? tf("studio.build.access.fieldAcl.unknownTable", { name: bang ?? "" }) : t("studio.build.access.fieldAcl.unknown")}</p>
            <p className="slds-text-color_weak">{t("studio.build.access.fieldAcl.unknownHint")}</p>
          </div>
        ) : (
          <FieldACL
            inlineApply
            entity={bang}
            field={truong}
            onChange={(entity, field) => {
              const q = new URLSearchParams({ bang: entity });
              if (field) q.set("truong", field);
              router.replace(`${pathname}?${q.toString()}`, { scroll: false });
            }}
          />
        )}
      </QueryState>
    </SubPage>
  );
}
