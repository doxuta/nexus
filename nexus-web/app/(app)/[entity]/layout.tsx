"use client";

// Route guard for every sub-route of /[entity] that WRITES — one place, not per page. The list hides
// its "Tạo mới" button with the same `can("create", entity)` (usePermissions); typing /<bảng>/new
// used to open the form anyway and only fail at save with a 403. Create-type routes (new, batch,
// import) need create; /<id>/edit needs edit. List, detail and the recycle bin pass through (their
// own screens enforce view). The server remains the authority — this just stops offering the form.
import { usePathname } from "next/navigation";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t } from "@/lib/i18n";
import { usePermissions } from "@/lib/metadata/permissions";

const CREATE_ROUTES = new Set(["new", "batch", "import"]);

export default function EntityLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname() ?? "";
  const { can, isLoading, isError } = usePermissions();
  const [entity, second, third] = pathname.split("/").filter(Boolean);

  const need = CREATE_ROUTES.has(second ?? "") ? "create" : third === "edit" ? "edit" : null;
  if (!need || !entity) return <>{children}</>;
  if (isLoading) return null;
  // A failed permissions fetch is "could not ask", not "denied": let the server's 403 speak.
  if (isError || can(need, entity)) return <>{children}</>;
  return (
    <div className="slds-p-around_medium">
      <ScopedNotification theme="warning">
        {t(need === "create" ? "page.entity.guard.noCreate" : "page.entity.guard.noEdit")}
      </ScopedNotification>
    </div>
  );
}
