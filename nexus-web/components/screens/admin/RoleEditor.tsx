"use client";

// RoleEditor (specs/074 T178): ONE open role = record header (name, code, "Xem như", delete) + the full ACLMatrix.
// Split out of RolesList so /admin/roles (split view + Modal/Prompt) and the Studio role page (own URL, UnsavedBar,
// delete confirmed in the page) draw the same role. ACLMatrix owns the load/save of the grants and the blocker arming.
import { useEffect, useState, type ReactNode } from "react";
import { Eye, ShieldCheck } from "lucide-react";
import { deleteRole, ensureAuth, getRole, ApiError, type RoleSummary } from "@/lib/admin";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Prompt } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { Spinner } from "@/components/slds/Spinner";
import { ActionGroup } from "@/components/slds/templates/ActionGroup";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { useDirectApply } from "@/components/shell/useDirectApply";
import { ACLMatrix } from "./ACLMatrix";

/** `viewAs` (Studio only): "Xem như vai này" on the open role; `open` starts the preview and navigates away. */
export interface RolesListViewAs {
  appLabel: string;
  open: (role: RoleSummary) => Promise<void>;
}

export interface RoleEditorProps {
  roleId: string;
  viewAs?: RolesListViewAs;
  entityScope?: ReadonlySet<string> | null;
  onDirtyChange?: (dirty: boolean) => void;
  /** The role is gone (deleted here). */
  onDeleted?: () => void;
  /** Delete is confirmed in the page (Studio) instead of a Prompt dialog (/admin/roles). */
  inlineDelete?: boolean;
  /** Studio: the page draws Lưu/Huỷ itself (UnsavedBar); the matrix card then has none. */
  renderBar?: React.ComponentProps<typeof ACLMatrix>["renderBar"];
  /** Shown above the matrix when the role has no grant at all. */
  emptyNote?: ReactNode;
}

const roleText = (r: RoleSummary) => r.label ?? r.name;

export function RoleEditor({ roleId, viewAs, entityScope, onDirtyChange, onDeleted, inlineDelete, renderBar, emptyNote }: RoleEditorProps) {
  const [role, setRole] = useState<(RoleSummary & { actions?: unknown[] }) | null>(null);
  const [state, setState] = useState<"loading" | "ok" | "missing" | "error">("loading");
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [viewBusy, setViewBusy] = useState(false);
  const { delivered } = useDirectApply();

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const r = await getRole(roleId);
        if (cancelled) return;
        setRole(r);
        setState("ok");
      } catch (e) {
        if (!cancelled) setState(e instanceof ApiError && e.status === 404 ? "missing" : "error");
      }
    })();
    return () => { cancelled = true; };
  }, [roleId]);

  async function handleDelete() {
    setBusy(true);
    try {
      await deleteRole(roleId);
      toast.success(t("page.adminRoles.deleted"));
      setConfirming(false);
      onDeleted?.();
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : t("page.adminRoles.deleteFailed"));
    } finally {
      setBusy(false);
    }
  }

  async function handleViewAs() {
    if (!viewAs || !role || viewBusy) return;
    setViewBusy(true);
    try {
      await viewAs.open({ id: role.id, name: role.name, label: role.label }); // the tab navigates away on success: stay locked
    } catch (ex) {
      toast.error(errorMessage(ex));
      setViewBusy(false);
    }
  }

  if (state === "loading") {
    return (
      <div className="slds-is-relative p-6">
        <Spinner size="small" />
      </div>
    );
  }
  if (state === "missing") {
    return <EmptyState size="large" icon={ShieldCheck} title={t("studio.build.access.role.notFound")} />;
  }
  if (state === "error" || !role) {
    return <p className="slds-text-color_error p-4">{t("page.adminRoles.loadError")}</p>;
  }

  const confirmMsg = fill(t("studio.build.access.role.deleteConfirm"), { label: roleText(role) });
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <PageHeader
        variant="record-home"
        icon={{ glyph: ShieldCheck, name: "user-role" }}
        label={t("page.adminRoles.objectLabel")}
        title={roleText(role)}
        actions={[
          ...(viewAs
            ? [
                <Button
                  key="view-as"
                  iconLeft={Eye}
                  disabled={viewBusy}
                  title={fill(t("studio.build.access.viewAs.why"), { app: viewAs.appLabel })}
                  onClick={() => void handleViewAs()}
                >
                  {t("studio.build.access.viewAs.role")}
                </Button>,
              ]
            : []),
          <ActionGroup key="actions" maxVisible={1} actions={[{ label: t("page.adminRoles.deleteRole"), onSelect: () => setConfirming(true) }]} />,
        ]}
        details={[{ label: t("page.adminRoles.code"), value: role.name }]}
      />
      {confirming && inlineDelete && (
        <InlineConfirm
          tone="error"
          message={confirmMsg}
          confirmLabel={t("studio.build.access.role.deleteYes")}
          busy={busy}
          onConfirm={() => void handleDelete()}
          onCancel={() => setConfirming(false)}
        >
          {delivered && <strong>{t("direct.notice")}</strong>}
        </InlineConfirm>
      )}
      {role.actions?.length === 0 && emptyNote}
      <div id="ma-tran">
        <ACLMatrix roleId={roleId} onDirtyChange={onDirtyChange} entityScope={entityScope ?? null} renderBar={renderBar} />
      </div>
      {!inlineDelete && (
        <Prompt
          open={confirming}
          onOpenChange={(o) => { if (!o && !busy) setConfirming(false); }}
          theme="error"
          title={fill(t("page.adminRoles.delete.title"), { label: roleText(role) })}
          footer={
            <>
              <Button disabled={busy} onClick={() => setConfirming(false)}>{t("page.adminRoles.cancel")}</Button>
              <Button variant="destructive" disabled={busy} onClick={() => void handleDelete()}>
                {t("page.adminRoles.delete.confirm")}
              </Button>
            </>
          }
        >
          {t("page.adminRoles.delete.body")}
          {delivered && <p className="slds-m-top_small"><strong>{t("direct.notice")}</strong></p>}
        </Prompt>
      )}
    </div>
  );
}
