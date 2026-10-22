"use client";

// CreateAppDialog — create/edit a sys_app (App container Phase 2). Shared by the App Launcher and
// /studio/apps. Create: label (required) auto-suggests the slug (editable, live-validated against the
// server rule); curated lucide icon picker; color = the SLDS custom icon colours the app tile uses
// (lib/apps appToneIcon — no hand-written colours). Edit: slug is immutable and the PUT is
// FULL-REPLACE, so we read-modify-write from the passed AdminApp row (isDefault/sortOrder preserved
// verbatim — a missing field would RESET it server-side).
// SLDS (specs/038 T039): modals/base + input/textarea in the form-element frame; icon and colour are
// stateful button-icons (`slds-is-selected` + aria-pressed) in a fieldset each.

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import { createApp, updateApp, listDashboards, listWorkQueuesAdmin, listRoles, ApiError, type AdminApp } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { qk } from "@/lib/query/keys";
import {
  APP_ICONS, APP_TONES, appIconByName, appToneIcon, isValidAppSlug, slugifyAppName, useActiveApp,
} from "@/lib/apps";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { CheckboxGroup } from "@/components/slds/Checkbox";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { FormElement } from "@/components/slds/FormElement";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Modal } from "@/components/slds/Modal";
import { Select } from "@/components/slds/Select";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Textarea } from "@/components/slds/Textarea";
import { useDirectApply } from "@/components/shell/useDirectApply";

/** Same ids regardless of order (a checkbox group has no meaningful order of its own). */
function sameIds(a: string[], b: string[] | undefined): boolean {
  const y = b ?? [];
  return a.length === y.length && a.every((id) => y.includes(id));
}

export function CreateAppDialog({
  open,
  onOpenChange,
  app = null,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Present ⇒ edit mode (slug locked, full-replace PUT from this row). */
  app?: AdminApp | null;
  /**
   * Studio project (specs/073 kh2): present ⇒ stay on this page after creating — no jump to the old /studio/apps/<name>
   * route and no write to the shared "active app" lens (FR-031); the caller decides where to go.
   */
  onCreated?: (name: string) => void;
}) {
  const editing = !!app;
  const qc = useQueryClient();
  const { setActiveApp } = useActiveApp();

  const [label, setLabel] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [icon, setIcon] = useState<string | null>(null);
  const [color, setColor] = useState<string | null>(null);
  const [description, setDescription] = useState("");
  const [homeDashboardId, setHomeDashboardId] = useState("");
  const [homeQueueIds, setHomeQueueIds] = useState<string[]>([]);
  const [defaultRoleIds, setDefaultRoleIds] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const { guard, dialog } = useDirectApply();
  const [fieldErr, setFieldErr] = useState<{ label?: string; name?: string }>({});
  const [formErr, setFormErr] = useState<string | null>(null);

  // (Re)prime on every open — edit prefills from the row, create starts blank. Adjusted while
  // rendering when open/app change (React's "state from props" pattern), not in an effect.
  const [primed, setPrimed] = useState<{ open: boolean; app: AdminApp | null }>({ open: false, app: null });
  if (primed.open !== open || primed.app !== app) {
    setPrimed({ open, app });
    if (open) {
      setLabel(app?.label ?? "");
      setSlug(app?.name ?? "");
      setSlugTouched(editing); // edit: never re-derive the (immutable) slug
      setIcon(app?.icon ?? null);
      setColor(app?.color ?? null);
      setDescription(app?.description ?? "");
      setHomeDashboardId(app?.homeDashboardId ?? "");
      setHomeQueueIds(app?.homeQueueIds ?? []);
      setDefaultRoleIds(app?.defaultRoleIds ?? []);
      setSaving(false);
      setFieldErr({});
      setFormErr(null);
    }
  }

  // Home page picker: only when editing (a brand-new app has no content yet). The catalog is the
  // non-admin dashboard list — the server validates the id again on save.
  const dashboardsQ = useQuery({ queryKey: qk.dashboards(), queryFn: listDashboards, enabled: open && editing, staleTime: 5 * 60 * 1000 });

  // Explicit home queues + "default app for these roles" (queue #483): admin reads, edit mode only.
  const queuesQ = useQuery({ queryKey: ["admin-work-queues"], queryFn: listWorkQueuesAdmin, enabled: open && editing, staleTime: 60_000 });
  const rolesQ = useQuery({ queryKey: qk.roles(), queryFn: listRoles, enabled: open && editing, staleTime: 60_000 });

  const slugValid = isValidAppSlug(slug);
  const canSubmit = label.trim() !== "" && (editing || slugValid) && !saving;
  const slugHint = t("page.apps.dialog.slugHint");

  function onLabelChange(v: string) {
    setLabel(v);
    if (!slugTouched) setSlug(slugifyAppName(v));
    setFieldErr((e) => ({ ...e, label: undefined }));
  }

  function mapError(e: unknown) {
    if (e instanceof ApiError) {
      switch (e.code) {
        case "APP_ALREADY_EXISTS":
          setFieldErr((f) => ({ ...f, name: t("page.apps.dialog.err.exists") }));
          return;
        case "APP_NAME_INVALID":
          setFieldErr((f) => ({ ...f, name: `${t("page.apps.dialog.err.invalid")} ${slugHint}` }));
          return;
        case "APP_LABEL_REQUIRED":
          setFieldErr((f) => ({ ...f, label: t("page.apps.dialog.err.labelRequired") }));
          return;
        case "APP_HOME_DASHBOARD_INVALID":
          setFormErr(t("page.apps.dialog.err.homeDashboard"));
          return;
        case "APP_HOME_QUEUE_INVALID":
          setFormErr(t("page.apps.dialog.err.homeQueue"));
          return;
        case "APP_DEFAULT_ROLE_INVALID":
          setFormErr(t("page.apps.dialog.err.defaultRole"));
          return;
        case "APP_FIELD_TOO_LONG":
          setFormErr(t("page.apps.dialog.err.tooLong"));
          return;
        default:
          setFormErr(e.message || t("page.apps.dialog.err.generic"));
          return;
      }
    }
    setFormErr(t("page.apps.dialog.err.generic"));
  }

  function submit() {
    if (!canSubmit) return;
    if (editing) void doSubmit();
    else void guard(t("direct.what.createApp"), doSubmit);
  }

  async function doSubmit() {
    setSaving(true);
    setFormErr(null);
    try {
      if (editing && app) {
        // FULL-REPLACE: send EVERY mutable field, unchanged ones straight from the row.
        await updateApp(app.id, {
          label: label.trim(),
          description: description.trim() === "" ? null : description.trim(),
          icon,
          color,
          isDefault: app.isDefault,
          sortOrder: app.sortOrder,
          homeDashboardId,
          // Sent only when the builder changed them: an omitted key means KEEP (never wipe a pick the
          // dialog could not load, e.g. a queue list that failed to fetch).
          ...(sameIds(homeQueueIds, app.homeQueueIds) ? {} : { homeQueueIds }),
          ...(sameIds(defaultRoleIds, app.defaultRoleIds) ? {} : { defaultRoleIds }),
        });
        emitDraftsChanged("app"); // an app draft on a delivered project: the header recounts
        await qc.invalidateQueries({ queryKey: qk.apps() });
        await qc.invalidateQueries({ queryKey: qk.adminApps() });
        toast.success(t("page.apps.dialog.toast.updated").replace("{label}", label.trim()));
      } else {
        // No emitDraftsChanged: POST /admin/apps applies at once even on a delivered project (DraftAppStore.Create), no draft.
        const created = await createApp({
          name: slug,
          label: label.trim(),
          ...(icon ? { icon } : {}),
          ...(color ? { color } : {}),
          ...(description.trim() ? { description: description.trim() } : {}),
        });
        await qc.invalidateQueries({ queryKey: qk.apps() });
        await qc.invalidateQueries({ queryKey: qk.adminApps() });
        if (!onCreated) setActiveApp(created.name); // the new app becomes the sidebar lens immediately
        toast.success(t("page.apps.dialog.toast.created").replace("{label}", created.label));
        if (onCreated) {
          onCreated(created.name);
          onOpenChange(false);
          return;
        }
        // Creating an app is the START of the maker journey: land on the project's App page. activeProjectSlug(), not
        // homeSlug(): a tab in a build session made this app in the customer's tenant, so that is the project.
        window.location.href = studioHref(activeProjectSlug(), "dung/man-hinh/app", {}, { app: created.name });
        return;
      }
      onOpenChange(false);
    } catch (e) {
      mapError(e);
    } finally {
      setSaving(false);
    }
  }

  const slugHelp = editing
    ? t("page.apps.dialog.slugLocked")
    : !slugValid && slug !== ""
      ? slugHint
      : `${t("page.apps.dialog.slugAuto")} ${slugHint}`;

  return (
    <>
    <Modal
      open={open}
      onOpenChange={(o) => { if (!saving) onOpenChange(o); }}
      title={editing ? t("page.apps.dialog.titleEdit") : t("page.apps.dialog.titleCreate")}
      // Câu này là câu ĐẦU TIÊN dạy người dùng "app là gì" — nói bằng thứ họ đã biết: menu, bảng,
      // dữ liệu (bản cũ dùng 5 thuật ngữ chưa ai dạy họ, phát hiện khi đóng vai tay mơ 2026-07-29).
      tagline={t("page.apps.dialog.tagline")}
      footer={
        <>
          <Button onClick={() => onOpenChange(false)} disabled={saving}>{t("page.apps.cancel")}</Button>
          <Button variant="brand" onClick={() => void submit()} disabled={!canSubmit}>
            {editing ? t("page.apps.dialog.submitEdit") : t("page.apps.dialog.submitCreate")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {formErr && <ScopedNotification theme="error">{formErr}</ScopedNotification>}

        <Input
          id="app-label"
          label={t("page.apps.dialog.label")}
          required
          value={label}
          onChange={(e) => onLabelChange(e.target.value)}
          disabled={saving}
          maxLength={100}
          placeholder={t("page.apps.dialog.labelPlaceholder")}
          error={fieldErr.label}
        />

        <Input
          id="app-slug"
          label={t("page.apps.dialog.slug")}
          value={slug}
          onChange={(e) => {
            setSlugTouched(true);
            setSlug(e.target.value);
            setFieldErr((f) => ({ ...f, name: undefined }));
          }}
          disabled={saving || editing}
          spellCheck={false}
          autoComplete="off"
          placeholder={t("page.apps.dialog.slugPlaceholder")}
          error={fieldErr.name ?? (!editing && slug !== "" && !slugValid ? slugHint : undefined)}
          help={slugHelp}
        />

        {/* Icon picker — curated lucide set, stateful button-icons. */}
        <FormElement as="fieldset" label={t("page.apps.dialog.icon")}>
          <div className="flex flex-wrap gap-1">
            {APP_ICONS.map(({ name, icon: glyph }) => {
              const on = icon === name;
              return (
                <ButtonIcon
                  key={name}
                  variant="border-filled"
                  icon={glyph}
                  label={name}
                  selected={on}
                  disabled={saving}
                  onClick={() => setIcon(on ? null : name)}
                />
              );
            })}
          </div>
        </FormElement>

        {/* Colour — the SLDS custom icon colours the app tile is drawn in, previewed with the chosen icon. */}
        <FormElement as="fieldset" label={t("page.apps.dialog.color")}>
          <div className="flex flex-wrap gap-1">
            {APP_TONES.map((tone) => {
              const on = color === tone.key;
              return (
                <button
                  key={tone.key}
                  type="button"
                  className={clsx("slds-button slds-button_icon slds-button_icon-border-filled", on && "slds-is-selected")}
                  aria-pressed={on}
                  title={tone.label}
                  disabled={saving}
                  onClick={() => setColor(on ? null : tone.key)}
                >
                  <Icon icon={appIconByName(icon)} category="custom" name={appToneIcon(tone.key)} size="x-small" />
                  <span className="slds-assistive-text">
                    {t("page.apps.dialog.colorOption").replace("{label}", tone.label)}
                  </span>
                </button>
              );
            })}
          </div>
        </FormElement>

        {editing && (
          <Select
            id="app-home-dashboard"
            label={t("page.apps.dialog.homeDashboard")}
            value={homeDashboardId}
            onChange={(e) => setHomeDashboardId(e.target.value)}
            disabled={saving}
            placeholder={t("page.apps.dialog.homeDashboardPlaceholder")}
            help={t("page.apps.dialog.homeDashboardHelp")}
            options={(dashboardsQ.data ?? []).map((d) => ({ value: d.id, label: d.name }))}
          />
        )}

        {editing && (
          <CheckboxGroup
            label={t("page.apps.dialog.homeQueues")}
            help={queuesQ.isError ? t("page.apps.dialog.homeQueuesError") : t("page.apps.dialog.homeQueuesHelp")}
            options={(queuesQ.data ?? []).filter((q) => q.isActive || homeQueueIds.includes(q.id)).map((q) => ({ value: q.id, label: q.label }))}
            value={homeQueueIds}
            onValueChange={setHomeQueueIds}
            disabled={saving}
          />
        )}

        {editing && (
          <CheckboxGroup
            label={t("page.apps.dialog.defaultRoles")}
            help={rolesQ.isError ? t("page.apps.dialog.defaultRolesError") : t("page.apps.dialog.defaultRolesHelp")}
            options={(rolesQ.data ?? []).map((r) => ({ value: r.id, label: r.label || r.name }))}
            value={defaultRoleIds}
            onValueChange={setDefaultRoleIds}
            disabled={saving}
          />
        )}

        <Textarea
          id="app-desc"
          label={t("page.apps.dialog.description")}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          disabled={saving}
          rows={2}
          placeholder={t("page.apps.dialog.descriptionPlaceholder")}
        />
      </div>
    </Modal>
    {dialog}
    </>
  );
}
