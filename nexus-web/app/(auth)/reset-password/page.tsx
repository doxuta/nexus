"use client";

// /reset-password — wired to the REAL backend (ENG-AUTH2 #1, docs/api-conventions.md §3a).
// Reads ?token= from the URL (the link a user clicks from the reset email) and POSTs it + a
// new password to POST /auth/reset-password. Server derives tenant from the token row — this
// page never asks for / sends a tenant. Frame (brand band + centred card) comes from AuthShell in
// app/(auth)/layout.tsx — same treatment as login/forgot/SSO complete.
// SLDS (specs/038 T033): card + form-element/input (each password error sits on the field that is
// wrong — too short / 422 on the new password, mismatch on the confirmation — `slds-has-error`)
// + buttons; link states and request errors are `scoped-notifications` (RESEARCH-2026-09 §5.2).
//
// useSearchParams() must sit under a <Suspense> boundary or `next build` fails (Next 16 docs,
// node_modules/next/dist/docs/01-app/.../use-search-params.md — "Missing Suspense boundary").
import { Suspense, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Eye, EyeOff, WifiOff } from "lucide-react";
import { resetPassword, ApiError } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { AuthCard } from "@/components/screens/AuthShell";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

const MIN_PASSWORD_LENGTH = 8; // mirrors internal/services/password_policy.go minPasswordLength (RM-17)

export default function ResetPasswordPage() {
  return (
    <Suspense fallback={null}>
      <ResetPasswordForm />
    </Suspense>
  );
}

function ResetPasswordForm() {
  const searchParams = useSearchParams();
  // `?token=` absent → null; `?token=` present-but-empty → "" (per Next docs). Treat both as
  // "no usable token" — there is nothing to POST.
  const token = searchParams.get("token") || null;

  const [pwd, setPwd] = useState("");
  const [confirm, setConfirm] = useState("");
  const [show, setShow] = useState(false);
  const [loading, setLoading] = useState(false);
  const [pwdErr, setPwdErr] = useState<string | null>(null); // new-password field: too short / 422
  const [confirmErr, setConfirmErr] = useState<string | null>(null); // confirm field: mismatch
  const [generalErr, setGeneralErr] = useState<string | null>(null);
  const [networkErr, setNetworkErr] = useState(false);
  const [tokenInvalid, setTokenInvalid] = useState(false);
  const [retryAfter, setRetryAfter] = useState(0);
  const [done, setDone] = useState(false);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  useEffect(() => {
    if (retryAfter <= 0) return;
    timerRef.current = setInterval(() => setRetryAfter((s) => (s > 0 ? s - 1 : 0)), 1000);
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
  }, [retryAfter]);

  function clearErrors() {
    setPwdErr(null);
    setConfirmErr(null);
    setGeneralErr(null);
    setNetworkErr(false);
  }

  async function submit() {
    if (!token) return; // guarded by the no-token screen below, but keep this safe.
    clearErrors();
    if (pwd.length < MIN_PASSWORD_LENGTH) {
      setPwdErr(t("page.resetPassword.err.tooShort").replace("{n}", String(MIN_PASSWORD_LENGTH)));
      return;
    }
    if (pwd !== confirm) {
      setConfirmErr(t("page.resetPassword.err.mismatch"));
      return;
    }
    if (retryAfter > 0) return;
    setLoading(true);
    try {
      await resetPassword(token, pwd);
      setDone(true);
    } catch (e2) {
      if (e2 instanceof ApiError) {
        if (e2.code === "RESET_TOKEN_INVALID") {
          setTokenInvalid(true);
        } else if (e2.status === 429) {
          const ra = (e2.details as { retryAfter?: number })?.retryAfter ?? 60;
          setRetryAfter(ra);
          setGeneralErr(t("page.auth.tooMany").replace("{n}", String(ra)));
        } else if (e2.status === 422) {
          // Weak/invalid new password (422 PASSWORD_TOO_WEAK, RM-17) — server-side backstop of
          // the same 8-char rule checked above. errorMessage falls back to the server's specific
          // Vietnamese message (the code is deliberately unmapped — see lib/errorMessages.ts).
          setPwdErr(errorMessage(e2));
        } else {
          setGeneralErr(errorMessage(e2));
        }
      } else {
        setNetworkErr(true);
        setGeneralErr(t("page.auth.networkError"));
      }
    } finally {
      setLoading(false);
    }
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    void submit();
  }

  const mm = String(Math.floor(retryAfter / 60)).padStart(2, "0");
  const ss = String(retryAfter % 60).padStart(2, "0");
  const locked = retryAfter > 0;

  // No token in the URL at all — nothing to submit. Honest error, no form.
  if (!token) {
    return <LinkProblem title={t("page.resetPassword.noToken.title")} desc={t("page.resetPassword.noToken.desc")} />;
  }

  // Token was rejected by the server (expired / already used / unknown) — mirror the same
  // "request a new one" recovery path (server does not distinguish the reason, anti-probe).
  if (tokenInvalid) {
    return <LinkProblem title={t("page.resetPassword.invalid.title")} desc={t("page.resetPassword.invalid.desc")} />;
  }

  if (done) {
    return (
      <AuthCard>
        <NexusLogo size={32} />
        <h1 className="slds-text-heading_medium">{t("page.resetPassword.done.title")}</h1>
        <ScopedNotification theme="success">{t("page.resetPassword.done.desc")}</ScopedNotification>
        <Link href="/login" className={buttonClassName("brand", { stretch: true })}>
          {t("auth.login.submit")}
        </Link>
      </AuthCard>
    );
  }

  return (
    <AuthCard busy={loading}>
      <NexusLogo size={32} />
      <div className="flex flex-col gap-1">
        <h1 className="slds-text-heading_medium">{t("page.resetPassword.title")}</h1>
        <p className="slds-text-color_weak">{t("page.resetPassword.subtitle")}</p>
      </div>

      <form className="flex flex-col gap-4" onSubmit={handleSubmit} noValidate>
        <Input
          label={t("page.resetPassword.newPassword")}
          type={show ? "text" : "password"}
          value={pwd}
          onChange={(e) => {
            setPwd(e.target.value);
            setPwdErr(null);
            setConfirmErr(null); // the mismatch involves this field too
          }}
          placeholder="••••••••••"
          autoComplete="new-password"
          error={pwdErr ?? undefined}
          help={t("page.resetPassword.hint").replace("{n}", String(MIN_PASSWORD_LENGTH))}
        />
        <Input
          label={t("page.resetPassword.confirm")}
          type={show ? "text" : "password"}
          value={confirm}
          onChange={(e) => {
            setConfirm(e.target.value);
            setConfirmErr(null);
          }}
          placeholder="••••••••••"
          autoComplete="new-password"
          error={confirmErr ?? undefined}
        />

        <div>
          <Button variant="base" iconLeft={show ? EyeOff : Eye} onClick={() => setShow((s) => !s)}>
            {show ? t("page.auth.hidePassword") : t("page.auth.showPassword")}
          </Button>
        </div>

        {generalErr && (
          <ScopedNotification theme="error" icon={networkErr ? WifiOff : undefined}>
            {generalErr}{" "}
            {networkErr && (
              // inverse: the one SLDS button variant meant for a dark/coloured (error) surface
              <Button variant="inverse" onClick={() => void submit()}>
                {t("page.auth.retry")}
              </Button>
            )}
          </ScopedNotification>
        )}

        <Button type="submit" variant="brand" stretch disabled={loading || locked}>
          {loading
            ? t("page.resetPassword.loading")
            : locked
            ? `${t("page.auth.retryIn")} ${mm}:${ss}`
            : t("page.resetPassword.submit")}
        </Button>
      </form>

      <p className="slds-text-align_center">
        <Link href="/login">{t("page.auth.backToLogin")}</Link>
      </p>
    </AuthCard>
  );
}

/** Unusable link (missing / rejected token): say so, and offer the two ways out. */
function LinkProblem({ title, desc }: { title: string; desc: string }) {
  return (
    <AuthCard>
      <NexusLogo size={32} />
      <h1 className="slds-text-heading_medium">{title}</h1>
      <ScopedNotification theme="error">{desc}</ScopedNotification>
      <Link href="/forgot-password" className={buttonClassName("brand", { stretch: true })}>
        {t("page.resetPassword.requestNew")}
      </Link>
      <p className="slds-text-align_center">
        <Link href="/login">{t("page.auth.backToLogin")}</Link>
      </p>
    </AuthCard>
  );
}
