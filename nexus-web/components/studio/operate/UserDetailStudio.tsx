"use client";

// Vận hành › Người dùng › một người (`?nguoi=<id>`), specs/078 lát u7-nguoi-dung-chi-tiet (FR-026..FR-028).
// A "settings form" page, not a card: back link, name, one group of fields with a Lưu/Huỷ bar that appears only
// while something changed, the person's devices, and the delete as a confirm IN the page. No dialog anywhere.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import { UserSessionsCard } from "@/components/screens/admin/UserSessionsCard";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { fill } from "@/components/slds/DataTable";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { Input } from "@/components/slds/Input";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { toast } from "@/components/ui/Toaster";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { QueryState } from "@/components/studio/QueryState";
import { UnsavedBar } from "@/components/studio/UnsavedBar";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { ApiError, deleteUser, endUserSession, getUser, listRoles, listUserSessions, setUserManager, updateUser, type AdminUser, type UpdateUserInput } from "@/lib/admin";
import { t } from "@/lib/i18n";
import { useStudioIdentity } from "@/lib/studioContext";
import { studioHref } from "@/lib/studioRoutes";
import { userErrorSentence } from "@/lib/userAdmin";

const name = (u: Pick<AdminUser, "fullName" | "email">) => u.fullName || u.email;

export function UserDetailStudio({ slug, id }: { slug: string; id: string }) {
  const user = useProjectQuery(slug, ["operate", "user", id], () => getUser(id));
  const roles = useProjectQuery(slug, ["operate", "roles"], listRoles);
  const listHref = studioHref(slug, "van-hanh/nguoi-dung");
  const back = <Link href={listHref}>{t("studio.operate.userDetail.back")}</Link>;
  const missing = user.isError && user.error instanceof ApiError && user.error.status === 404;
  return (
    <div className="slds-p-around_medium flex flex-col gap-4">
      <p>{back}</p>
      {missing ? (
        <ScopedNotification theme="error">{t("studio.operate.userDetail.missing")}</ScopedNotification>
      ) : user.isError ? (
        <div>
          <ScopedNotification theme="error">{t("studio.operate.userDetail.loadError")}</ScopedNotification>
          <div className="slds-m-top_small">
            <Button variant="neutral" onClick={() => void user.refetch()}>{t("studio.query.retry")}</Button>
          </div>
        </div>
      ) : (
        <QueryState query={user}>
          {user.data && (
            <QueryState query={roles}>
              {roles.data && <Loaded key={user.data.id} slug={slug} person={user.data} roles={roles.data} />}
            </QueryState>
          )}
        </QueryState>
      )}
    </div>
  );
}

function Loaded({ slug, person, roles }: { slug: string; person: AdminUser; roles: { id: string; name: string; label?: string }[] }) {
  const router = useRouter();
  const qc = useQueryClient();
  const me = useStudioIdentity().me as { id?: string } | undefined;
  const isSelf = me?.id === person.id;
  const [base, setBase] = useState(person);
  const [form, setForm] = useState({ fullName: person.fullName ?? "", roleId: person.roleId ?? "", status: person.status as "active" | "inactive", isAdmin: person.isAdmin });
  const [owner, setOwner] = useState(person.canManageUsers);
  const [askOwner, setAskOwner] = useState(false);
  const [ownerBusy, setOwnerBusy] = useState(false);
  const [ownerErr, setOwnerErr] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [askDelete, setAskDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  // A save can end the person's logins on the server (role change); the devices list must be read again.
  const [devicesKey, setDevicesKey] = useState(0);

  const goList = () => router.push(studioHref(slug, "van-hanh/nguoi-dung"));
  const invalidate = () => qc.invalidateQueries({ queryKey: ["project", slug, "operate"] });
  // Forget the gone person's cached detail first, so nothing refetches it into a "not found" flash.
  const dropPerson = () => {
    qc.removeQueries({ queryKey: ["project", slug, "operate", "user", person.id] });
    void invalidate();
  };
  // Cancelling the in-page confirm remounts the delete button; give focus back to it.
  const delWrap = useRef<HTMLDivElement>(null);
  const wasAsking = useRef(false);
  // Same for the owner-tier confirm: the checkbox is disabled while asking, so focus is lost on cancel.
  const ownerWrap = useRef<HTMLDivElement>(null);
  const wasAskingOwner = useRef(false);
  const dirty =
    form.fullName !== (base.fullName ?? "") || form.roleId !== (base.roleId ?? "") || form.status !== base.status || form.isAdmin !== base.isAdmin;
  const reset = () => setForm({ fullName: base.fullName ?? "", roleId: base.roleId ?? "", status: base.status as "active" | "inactive", isAdmin: base.isAdmin });

  useEffect(() => {
    if (wasAsking.current && !askDelete) delWrap.current?.querySelector("button")?.focus();
    wasAsking.current = askDelete;
  }, [askDelete]);

  useEffect(() => {
    if (wasAskingOwner.current && !askOwner) ownerWrap.current?.querySelector("input")?.focus();
    wasAskingOwner.current = askOwner;
  }, [askOwner]);

  // In-app leaving is stopped by UnsavedBar (NavigationBlocker); reload / closing the tab needs this listener.
  useEffect(() => {
    if (!dirty) return;
    const onUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onUnload);
    return () => window.removeEventListener("beforeunload", onUnload);
  }, [dirty]);

  async function save() {
    // Only what changed: a bare rename must not touch the role (and so must not sign the person out). "" clears the role.
    const patch: UpdateUserInput = {};
    if (form.fullName !== (base.fullName ?? "")) patch.fullName = form.fullName;
    if (form.status !== base.status) patch.status = form.status;
    if (form.isAdmin !== base.isAdmin) patch.isAdmin = form.isAdmin;
    if (form.roleId !== (base.roleId ?? "")) patch.roleId = form.roleId;
    setSaving(true);
    setErr(null);
    try {
      const next = await updateUser(person.id, patch);
      setBase(next);
      setDevicesKey((k) => k + 1);
      toast.success(fill(t("studio.operate.userDetail.saved"), { name: name(next) }));
      void invalidate();
    } catch (e) {
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") {
        // The page unmounts on goList, so the sentence must outlive it: a toast, not local state.
        toast.error(t("studio.operate.userDetail.gone"));
        dropPerson();
        goList();
      } else {
        setErr(userErrorSentence(e));
      }
    } finally {
      setSaving(false);
    }
  }

  async function switchOwner(enabled: boolean) {
    setOwnerBusy(true);
    setOwnerErr(null);
    try {
      const next = await setUserManager(person.id, enabled);
      setOwner(next.canManageUsers ?? enabled);
      setAskOwner(false);
      toast.success(fill(t(enabled ? "studio.operate.tier.setOn" : "studio.operate.tier.setOff"), { name: name(base) }));
      void invalidate();
    } catch (e) {
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") {
        toast.error(t("studio.operate.userDetail.gone"));
        dropPerson();
        goList();
      } else {
        setOwnerErr(userErrorSentence(e));
      }
    } finally {
      setOwnerBusy(false);
    }
  }

  async function remove() {
    setDeleting(true);
    setDeleteErr(null);
    try {
      await deleteUser(person.id);
      toast.success(fill(t("studio.operate.userDetail.deleted"), { name: name(base) }));
      dropPerson();
      goList();
    } catch (e) {
      if (e instanceof ApiError && e.code === "USER_NOT_FOUND") {
        toast.error(t("studio.operate.userDetail.gone"));
        dropPerson();
        goList();
      } else {
        setDeleteErr(e instanceof ApiError && e.code === "LAST_ADMIN" ? t("studio.operate.userDetail.lastAdmin") : userErrorSentence(e));
      }
    } finally {
      setDeleting(false);
    }
  }

  const loadSessions = useCallback(() => listUserSessions(person.id), [person.id]);
  const endSession = useCallback((sid: string) => endUserSession(person.id, sid), [person.id]);

  return (
    <>
      <div className="flex flex-col gap-1">
        <h2 className="slds-text-heading_medium">{name(base)}</h2>
        <p className="slds-text-color_weak">{base.email}</p>
        <p>
          <Link href={studioHref(slug, "van-hanh/nguoi-dung/nhat-ky", {}, { nguoi: person.id })}>{t("studio.operate.userDetail.related")}</Link>
        </p>
      </div>

      <section aria-labelledby="nx-user-info" className="flex flex-col gap-3">
        <h3 id="nx-user-info" className="slds-text-heading_small">{t("studio.operate.userDetail.group")}</h3>
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <form
          className="grid gap-3 md:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (dirty && !saving) void save();
          }}
        >
          <Input label={t("studio.operate.userDetail.fullName")} autoComplete="off" value={form.fullName} onChange={(e) => setForm((f) => ({ ...f, fullName: e.target.value }))} />
          <Select
            label={t("studio.operate.userDetail.role")}
            value={form.roleId}
            onChange={(e) => setForm((f) => ({ ...f, roleId: e.target.value }))}
            options={roles.map((r) => ({ value: r.id, label: r.label ?? r.name }))}
            placeholder={t("studio.operate.users.noRole")}
          />
          <RadioButtonGroup
            label={t("studio.operate.userDetail.status")}
            value={form.status}
            onValueChange={(v) => setForm((f) => ({ ...f, status: v as "active" | "inactive" }))}
            options={[
              { value: "active", label: t("studio.operate.users.status.active") },
              { value: "inactive", label: t("studio.operate.users.status.locked") },
            ]}
          />
          <Checkbox label={t("studio.operate.userDetail.isAdmin")} checked={form.isAdmin} onChange={(e) => setForm((f) => ({ ...f, isAdmin: e.target.checked }))} />
        </form>
        <UnsavedBar dirty={dirty} saving={saving} onSave={() => void save()} onCancel={reset} saveLabel={t("studio.operate.userDetail.save")} />
      </section>

      {owner !== undefined && !base.isAdmin && (
        <section aria-labelledby="nx-user-tier" className="flex flex-col gap-2">
          <h3 id="nx-user-tier" className="slds-text-heading_small">{t("studio.operate.users.col.tier")}</h3>
          {ownerErr && <ScopedNotification theme="error">{ownerErr}</ScopedNotification>}
          <div ref={ownerWrap}>
            <Checkbox
              label={t("studio.operate.tier.label")}
              checked={owner}
              disabled={ownerBusy || askOwner}
              onChange={(e) => (e.target.checked ? setAskOwner(true) : void switchOwner(false))}
            />
          </div>
          <p className="slds-text-body_small slds-text-color_weak">{t("studio.operate.tier.help")}</p>
          {askOwner && (
            <InlineConfirm
              tone="warning"
              message={fill(t("studio.operate.tier.confirm"), { name: name(base) })}
              confirmLabel={t("studio.operate.tier.on")}
              busy={ownerBusy}
              onConfirm={() => void switchOwner(true)}
              onCancel={() => setAskOwner(false)}
            />
          )}
        </section>
      )}

      {/* The card is always the flat one here: this page lives only in the work surface. */}
      <EmbeddedSurface>
        <UserSessionsCard key={devicesKey} load={loadSessions} end={endSession} description={t("studio.operate.userDetail.sessionsDesc")} />
      </EmbeddedSurface>

      <section className="flex flex-col gap-2">
        {deleteErr && <ScopedNotification theme="error">{deleteErr}</ScopedNotification>}
        {askDelete ? (
          <InlineConfirm
            tone="error"
            message={fill(t("studio.operate.userDetail.deleteConfirm"), { name: name(base) })}
            confirmLabel={t("studio.operate.userDetail.deleteConfirmLabel")}
            busy={deleting}
            onConfirm={() => void remove()}
            onCancel={() => setAskDelete(false)}
          />
        ) : (
          <div ref={delWrap}>
            <Button
              variant="text-destructive"
              disabled={isSelf}
              title={isSelf ? t("studio.operate.userDetail.deleteSelf") : undefined}
              onClick={() => { setDeleteErr(null); setAskDelete(true); }}
            >
              {t("studio.operate.userDetail.delete")}
            </Button>
          </div>
        )}
      </section>
    </>
  );
}
