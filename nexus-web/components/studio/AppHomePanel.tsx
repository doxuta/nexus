"use client";

// AppHomePanel - "Trang chủ và app mặc định" on the app screen (specs/074 lát 27, queue #538). Writes the fields the
// customer face already reads (HomeScreen leads with homeDashboardId + homeQueueIds; resolveActiveApp prefers the app
// whose defaultRoleIds hold the caller's role) and previews them inside CustomerPreviewFrame.
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, listDashboards, listRoles, listWorkQueuesAdmin, saveAppHome, type AdminApp } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { CheckboxGroup } from "@/components/slds/Checkbox";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { CustomerPreviewFrame } from "./CustomerPreviewFrame";

const fill = (s: string, v: Record<string, string>) => s.replace(/\{(\w+)\}/g, (m, k: string) => v[k] ?? m);

export function AppHomePanel({ app }: { app: AdminApp }) {
  const qc = useQueryClient();
  const dashQ = useQuery({ queryKey: qk.dashboards(), queryFn: listDashboards, staleTime: 60_000 });
  const queuesQ = useQuery({ queryKey: ["admin-work-queues"], queryFn: listWorkQueuesAdmin, staleTime: 60_000 });
  const rolesQ = useQuery({ queryKey: qk.roles(), queryFn: listRoles, staleTime: 60_000 });
  const [dash, setDash] = useState(app.homeDashboardId ?? "");
  const [queueIds, setQueueIds] = useState<string[]>(app.homeQueueIds ?? []);
  const [roleIds, setRoleIds] = useState<string[]>(app.defaultRoleIds ?? []);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const queues = (queuesQ.data ?? []).filter((q) => q.isActive || queueIds.includes(q.id));
  const roles = (rolesQ.data ?? []).map((r) => ({ id: r.id, label: r.label || r.name }));
  const names = (ids: string[], all: { id: string; label: string }[]) =>
    ids.map((id) => all.find((x) => x.id === id)?.label).filter(Boolean).join(", ");
  const dashName = (dashQ.data ?? []).find((d) => d.id === dash)?.name;

  async function save() {
    setBusy(true);
    setErr(null);
    try {
      await saveAppHome(app, { homeDashboardId: dash, homeQueueIds: queueIds, defaultRoleIds: roleIds });
      await qc.invalidateQueries({ queryKey: qk.apps() });
      await qc.invalidateQueries({ queryKey: qk.adminApps() });
      toast.success(fill(t("studio.build.apps.home.saved"), { label: app.label }));
    } catch (e) {
      const code = e instanceof ApiError ? e.code : "";
      const status = e instanceof ApiError ? e.status : 0;
      setErr(
        code.startsWith("APP_HOME_") || code === "APP_DEFAULT_ROLE_INVALID" ? t("studio.build.apps.home.stale")
          : status === 409 ? t("studio.build.apps.home.conflict")
          : t("studio.build.apps.home.failed"),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title={t("studio.build.apps.home.title")}
      footer={<Button variant="brand" onClick={() => void save()} disabled={busy}>{t("studio.build.apps.home.save")}</Button>}>
      <div className="grid gap-4 md:grid-cols-2 slds-p-around_medium">
        <div className="flex flex-col gap-3">
          {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
          <Select
            id="app-home-panel-dashboard"
            label={t("studio.build.apps.home.dashboard")}
            value={dash}
            onChange={(e) => setDash(e.target.value)}
            disabled={busy}
            placeholder={t("studio.build.apps.home.dashboardNone")}
            options={(dashQ.data ?? []).map((d) => ({ value: d.id, label: d.name }))}
          />
          <CheckboxGroup
            label={t("studio.build.apps.home.queues")}
            help={queues.length === 0 ? t("studio.build.apps.home.queuesEmpty") : t("page.apps.dialog.homeQueuesHelp")}
            options={queues.map((q) => ({ value: q.id, label: q.label }))}
            value={queueIds}
            onValueChange={setQueueIds}
            disabled={busy}
          />
          <CheckboxGroup
            label={t("studio.build.apps.default.roles")}
            help={t("studio.build.apps.default.help")}
            options={roles.map((r) => ({ value: r.id, label: r.label }))}
            value={roleIds}
            onValueChange={setRoleIds}
            disabled={busy}
          />
        </div>
        <CustomerPreviewFrame appName={app.label} caption={t("studio.build.apps.home.preview")}>
          <ul className="flex flex-col gap-2" data-testid="app-home-preview">
            <li>{dashName ? fill(t("studio.build.apps.home.previewDash"), { name: dashName }) : t("studio.build.apps.home.previewNoDash")}</li>
            <li>{queueIds.length ? fill(t("studio.build.apps.home.previewQueues"), { names: names(queueIds, queues) }) : t("studio.build.apps.home.previewQueuesAuto")}</li>
            <li>{roleIds.length ? fill(t("studio.build.apps.default.previewRoles"), { names: names(roleIds, roles) }) : t("studio.build.apps.default.previewNone")}</li>
          </ul>
        </CustomerPreviewFrame>
      </div>
    </Card>
  );
}
