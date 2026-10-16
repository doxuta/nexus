"use client";

// /profile — hồ sơ /me, dữ liệu THẬT từ AuthProvider (/auth/me).
// SLDS (specs/038 T032): record home of the signed-in user.
//   page-headers/record-home — user tile, "Hồ sơ của tôi", name, highlight fields
//   left 2/3:  cards › form-element/record-detail (read-only fields, avatar beside the name) ·
//              "Phiên đăng nhập" card — the live login sessions (GET /auth/sessions, specs/055) as a
//              divided list + logout-all behind a modals/base confirm
//   right 1/3: "Đổi mật khẩu" card (input + checkbox) · MfaCard (step-up flows in modals/base)
// Password change: 401 = wrong current pw (inline on current-pw field); 422 = weak/invalid new pw
// (inline on new-pw field); anything else = scoped error notification.
// MFA (FE-CATCHUP-1): the card takes the REAL mfaEnabled flag from /auth/me and refetchMe after a
// change so the nexus_user cache never goes stale.
import { useState, type FormEvent, type ReactNode } from "react";
import { Lock, UserRound } from "lucide-react";
import { useAuth } from "@/lib/auth/AuthProvider";
import {
  changePassword, endMySession, listMySessions, logoutEverywhere, tenantSlug, ApiError,
  type AuthMe,
} from "@/lib/api";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { MfaCard } from "@/components/screens/MfaCard";
import { UserSessionsCard } from "@/components/screens/admin/UserSessionsCard";
import { Avatar } from "@/components/slds/Avatar";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { FormElement } from "@/components/slds/FormElement";
import { Input } from "@/components/slds/Input";
import { PageHeader } from "@/components/slds/PageHeader";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export function Profile() {
  const { user, refetchMe } = useAuth();
  const name = user?.fullName || user?.email || "…";
  const org = tenantSlug();
  return (
    <div className="flex min-h-0 flex-1 overflow-y-auto">
      <div className="slds-p-around_medium slds-size_1-of-1">
        <PageHeader
          variant="record-home"
          icon={{ glyph: UserRound, name: "user" }}
          label={t("page.profile.objectLabel")}
          title={name}
          details={user ? [
            { label: t("page.profile.field.email"), value: user.email },
            { label: t("page.profile.field.role"), value: roleLabel(user) },
            ...(org ? [{ label: t("page.profile.field.org"), value: org }] : []),
          ] : undefined}
        />
        <div className="slds-grid slds-wrap slds-gutters_x-small">
          <div className="slds-col slds-size_1-of-1 slds-large-size_2-of-3 slds-m-top_medium">
            <InfoCard user={user} name={name} org={org} />
            <SessionsCard />
          </div>
          <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-3 slds-m-top_medium">
            <ChangePasswordCard />
            <MfaCard mfaEnabled={user?.mfaEnabled} onMfaChanged={refetchMe} />
          </div>
        </div>
      </div>
    </div>
  );
}

// Human role label from the real /auth/me payload — roleName when assigned, else the admin flag.
function roleLabel(user: AuthMe | null): string {
  if (!user) return "";
  return user.roleName || t(user.isAdmin ? "page.profile.role.admin" : "page.profile.role.user");
}

// ── Thông tin tài khoản — form-element/record-detail, chỉ những gì /auth/me thật sự biết ──
function InfoCard({ user, name, org }: { user: AuthMe | null; name: string; org: string }) {
  const rows: { label: string; value: ReactNode }[] = [
    {
      label: t("page.profile.field.fullName"),
      value: (
        <>
          <Avatar name={name} shape="circle" size="x-small" className="slds-m-right_xx-small" />
          {name}
        </>
      ),
    },
    { label: t("page.profile.field.email"), value: user?.email ?? "" },
    { label: t("page.profile.field.role"), value: roleLabel(user) },
    ...(org ? [{ label: t("page.profile.field.org"), value: org }] : []),
    {
      label: t("page.profile.field.mfa"),
      value: t(user?.mfaEnabled ? "page.profile.field.mfaOn" : "page.profile.field.mfaOff"),
    },
  ];
  // Two fields per row, like the record-detail blueprint's two-column form; an odd last field
  // gets the blueprint's empty listitem so it does not stretch across both columns.
  const pairs: (typeof rows)[] = [];
  for (let i = 0; i < rows.length; i += 2) pairs.push(rows.slice(i, i + 2));
  return (
    <Card title={t("page.profile.info.title")} icon={{ glyph: UserRound, name: "user" }}>
      <div className="slds-form" role="list">
        {pairs.map((pair) => (
          <div key={pair[0].label} className="slds-form__row">
            {pair.map((r) => (
              <div key={r.label} className="slds-form__item" role="listitem">
                <FormElement label={r.label} layout="stacked" readOnly>
                  {r.value}
                </FormElement>
              </div>
            ))}
            {pair.length === 1 && <div className="slds-form__item" role="listitem" />}
          </div>
        ))}
      </div>
    </Card>
  );
}

// ── Đổi mật khẩu — wired to PUT /auth/me/password ──────────────────────────────────
// Error routing: 401 (UNAUTHORIZED) → wrong current password → inline on current-pw field
//               422 (any)          → weak/invalid new password → inline on new-pw field
//               other              → scoped error notification
function ChangePasswordCard() {
  const [show, setShow] = useState(false);
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [loading, setLoading] = useState(false);
  // Inline field-level errors (null = no error)
  const [currentErr, setCurrentErr] = useState<string | null>(null);
  const [nextErr, setNextErr] = useState<string | null>(null);
  const [generalErr, setGeneralErr] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  function clearErrors() {
    setCurrentErr(null);
    setNextErr(null);
    setGeneralErr(null);
    setSuccess(false);
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    clearErrors();
    // Client-side validation
    if (!current) { setCurrentErr(t("page.profile.password.errCurrentRequired")); return; }
    if (!next) { setNextErr(t("page.profile.password.errNewRequired")); return; }
    if (next.length < 8) { setNextErr(t("page.profile.password.errTooShort")); return; }
    if (!confirm) { setNextErr(t("page.profile.password.errConfirmRequired")); return; }
    if (next !== confirm) { setNextErr(t("page.profile.password.errMismatch")); return; }
    setLoading(true);
    try {
      await changePassword(current, next);
      setSuccess(true);
      setCurrent(""); setNext(""); setConfirm("");
      toast.success(t("page.profile.password.toastSuccess"));
    } catch (e2) {
      if (e2 instanceof ApiError) {
        if (e2.status === 401) {
          // Wrong current password
          setCurrentErr(t("page.profile.password.errWrongCurrent"));
        } else if (e2.status === 422) {
          // Weak or invalid new password
          setNextErr(e2.message || t("page.profile.password.errInvalidNew"));
        } else {
          setGeneralErr(e2.message || t("page.profile.password.errGeneric"));
        }
      } else {
        setGeneralErr(t("page.profile.password.errRetry"));
      }
    } finally {
      setLoading(false);
    }
  }

  const type = show ? "text" : "password";
  return (
    <Card title={t("page.profile.password.title")} icon={{ glyph: Lock, name: "password" }}>
      <form onSubmit={handleSubmit} noValidate>
        <Input
          layout="stacked"
          label={t("page.profile.password.current")}
          type={type}
          value={current}
          onChange={(e) => { setCurrent(e.target.value); if (currentErr) setCurrentErr(null); }}
          error={currentErr ?? undefined}
          autoComplete="current-password"
        />
        <Input
          layout="stacked"
          label={t("page.profile.password.new")}
          type={type}
          value={next}
          onChange={(e) => { setNext(e.target.value); if (nextErr) setNextErr(null); }}
          help={t("page.profile.password.hint")}
          error={nextErr ?? undefined}
          autoComplete="new-password"
        />
        <Input
          layout="stacked"
          label={t("page.profile.password.confirm")}
          type={type}
          value={confirm}
          onChange={(e) => { setConfirm(e.target.value); if (nextErr) setNextErr(null); }}
          autoComplete="new-password"
        />
        <Checkbox
          layout="stacked"
          label={t("page.profile.password.show")}
          checked={show}
          onChange={(e) => setShow(e.target.checked)}
        />
        {generalErr && (
          <ScopedNotification theme="error" className="slds-m-top_small">
            {generalErr}
          </ScopedNotification>
        )}
        {success && (
          <ScopedNotification theme="success" className="slds-m-top_small">
            {t("page.profile.password.success")}
          </ScopedNotification>
        )}
        <div className="slds-m-top_small">
          <Button variant="brand" type="submit" disabled={loading} stretch>
            {loading ? t("page.profile.password.submitting") : t("page.profile.password.submit")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

// ── Phiên đăng nhập — my own devices (specs/055). The list/confirm lives in the shared
// UserSessionsCard (also used by the admin's user detail, specs/065 #313); this wires it to
// /auth/sessions and adds "Đăng xuất mọi thiết bị". Defined at module level so the card's
// `load`/`end` identities are stable.
const loadMine = listMySessions;
const endMine = endMySession;
async function logoutAllAndLeave(): Promise<void> {
  await logoutEverywhere();
  window.location.href = "/login";
}
function SessionsCard() {
  return <UserSessionsCard load={loadMine} end={endMine} revokeAll={logoutAllAndLeave} />;
}
