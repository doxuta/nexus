"use client";

// /forgot-password — wired to the REAL backend (ENG-AUTH2 #1, docs/api-conventions.md §3a).
// POST /auth/forgot-password ALWAYS returns 200 regardless of whether the tenant/email exist
// (anti-enumeration) — there is no "email not found" branch to build; every well-formed submit
// shows the SAME neutral success panel. Frame (brand band + centred card) comes from AuthShell in
// app/(auth)/layout.tsx — same treatment as login/reset/SSO complete.
// SLDS (specs/038 T033): card + form-element/input + buttons; errors and the neutral success are
// `scoped-notifications` (RESEARCH-2026-09 §5.2).
import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { WifiOff } from "lucide-react";
import { forgotPassword, ApiError } from "@/lib/api";
import { t } from "@/lib/i18n";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { AuthCard } from "@/components/screens/AuthShell";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export default function ForgotPasswordPage() {
  return <ForgotPasswordForm />;
}

function ForgotPasswordForm() {
  const [org, setOrg] = useState("");
  const [email, setEmail] = useState("");
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [networkErr, setNetworkErr] = useState(false);
  const [retryAfter, setRetryAfter] = useState(0);
  const [sent, setSent] = useState(false);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // Countdown for 429 lockout (mirrors LoginForm).
  useEffect(() => {
    if (retryAfter <= 0) return;
    timerRef.current = setInterval(() => setRetryAfter((s) => (s > 0 ? s - 1 : 0)), 1000);
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
  }, [retryAfter]);

  async function submit() {
    if (!org.trim() || !email.trim()) {
      setErr(t("page.forgotPassword.err.missing"));
      return;
    }
    if (retryAfter > 0) return;
    setLoading(true);
    setErr(null);
    setNetworkErr(false);
    try {
      await forgotPassword(org.trim(), email.trim());
      // Anti-enumeration: this branch runs whether or not the tenant/email actually exist —
      // the server already collapsed that distinction server-side.
      setSent(true);
    } catch (e2) {
      if (e2 instanceof ApiError) {
        if (e2.status === 429) {
          const ra = (e2.details as { retryAfter?: number })?.retryAfter ?? 60;
          setRetryAfter(ra);
          setErr(t("page.auth.tooMany").replace("{n}", String(ra)));
        } else {
          setErr(e2.message || t("page.forgotPassword.err.generic"));
        }
      } else {
        // fetch() rejected before an HTTP response existed — offline / DNS / CORS / server down.
        setNetworkErr(true);
        setErr(t("page.auth.networkError"));
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

  if (sent) {
    return (
      <AuthCard>
        <NexusLogo size={32} />
        <h1 className="slds-text-heading_medium">{t("page.forgotPassword.sent.title")}</h1>
        <ScopedNotification theme="success">{t("page.forgotPassword.sent.desc")}</ScopedNotification>
        <Link href="/login" className={buttonClassName("brand", { stretch: true })}>
          {t("page.auth.backToLogin")}
        </Link>
      </AuthCard>
    );
  }

  return (
    <AuthCard busy={loading}>
      <NexusLogo size={32} />
      <div className="flex flex-col gap-1">
        <h1 className="slds-text-heading_medium">{t("page.forgotPassword.title")}</h1>
        <p className="slds-text-color_weak">{t("page.forgotPassword.subtitle")}</p>
      </div>

      <form className="flex flex-col gap-4" onSubmit={handleSubmit} noValidate>
        <Input
          label={t("page.auth.orgLabel")}
          value={org}
          onChange={(e) => setOrg(e.target.value)}
          placeholder={t("page.auth.orgPlaceholder")}
          autoComplete="organization"
        />
        <Input
          label={t("auth.login.email")}
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder={t("page.auth.emailPlaceholder")}
          autoComplete="email"
        />

        {err && (
          <ScopedNotification theme="error" icon={networkErr ? WifiOff : undefined}>
            {err}{" "}
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
            ? t("page.forgotPassword.loading")
            : locked
            ? `${t("page.auth.retryIn")} ${mm}:${ss}`
            : t("page.forgotPassword.submit")}
        </Button>
      </form>

      <p className="slds-text-align_center">
        <Link href="/login">{t("page.auth.backToLogin")}</Link>
      </p>
    </AuthCard>
  );
}
