"use client";

// /auth/sso/complete — landing of the SSO redirect flow (#48 SSO-FE-REDIRECT).
// The backend /auth/sso/callback 302s here with ?code=<one-time handoff code> (single-use,
// 120s TTL — the token pair itself NEVER rides in a URL). This page:
//   1. reads ?code, immediately strips it from the URL (history hygiene — the code is
//      single-use anyway, but it must not linger in browser history / referrers),
//   2. POSTs it to /auth/sso/exchange exactly ONCE (ref guard — React StrictMode runs
//      effects twice in dev and the code burns on first use),
//   3. ok → session stored (lib/api applyTokenPair) → refresh auth context → /home;
//      mfa → inline TOTP/backup-code step (same contract as the login page's step 2);
//      error → honest Vietnamese message + a way back to /login.
// Never logs the code or any token.
// SLDS (specs/038 T033): card + `spinners` (exchanging) + form-element/input (MFA) + error
// `scoped-notifications` (RESEARCH-2026-09 §5.2).
//
// useSearchParams() must sit under a <Suspense> boundary or `next build` fails (Next 16 docs,
// node_modules/next/dist/docs/01-app/.../use-search-params.md — "Missing Suspense boundary").
import { Suspense, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { ApiError, ssoExchange } from "@/lib/api";
import { useAuth } from "@/lib/auth/AuthProvider";
import { ssoErrorMessage, SSO_ERROR_FALLBACK } from "@/lib/ssoError";
import { t } from "@/lib/i18n";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { AuthCard } from "@/components/screens/AuthShell";
import { Button, buttonClassName } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

// Frame (brand band + centred card) comes from AuthShell in app/(auth)/layout.tsx —
// same treatment as login/forgot/reset.
export default function SsoCompletePage() {
  return (
    <Suspense fallback={null}>
      <SsoComplete />
    </Suspense>
  );
}

type Phase = "exchanging" | "mfa" | "error";

function SsoComplete() {
  const searchParams = useSearchParams();
  const router = useRouter();
  const { completeMfa, refetchMe } = useAuth();

  // The code is read ONCE, at mount: the effect below strips it from the URL right away, and
  // the no-code outcome is known before any effect runs (no setState inside the effect).
  const [code] = useState(() => searchParams.get("code") || null);
  const [phase, setPhase] = useState<Phase>(code ? "exchanging" : "error");
  const [err, setErr] = useState<string | null>(code ? null : t("page.ssoComplete.err.noCode"));
  // MFA step: non-null = identity accepted by the IdP, awaiting TOTP/backup code.
  const [pendingToken, setPendingToken] = useState<string | null>(null);
  const [mfaCode, setMfaCode] = useState("");
  const [loading, setLoading] = useState(false);
  const [retryAfter, setRetryAfter] = useState(0);
  const ranRef = useRef(false);

  // Countdown for a 429 lockout on the MFA step (mirrors the login page).
  useEffect(() => {
    if (retryAfter <= 0) return;
    const timer = setInterval(() => setRetryAfter((s) => (s > 0 ? s - 1 : 0)), 1000);
    return () => clearInterval(timer);
  }, [retryAfter]);

  useEffect(() => {
    // The handoff code is SINGLE-USE: guard against StrictMode's dev double-effect —
    // a second POST would burn on 401 SSO_HANDOFF_INVALID and kill a valid login.
    if (ranRef.current) return;
    ranRef.current = true;

    // Hygiene: drop ?code from the address bar + history entry right away.
    window.history.replaceState({}, "", "/auth/sso/complete");

    if (!code) return; // the error state was set at mount

    (async () => {
      try {
        const res = await ssoExchange(code);
        if (res.status === "mfa") {
          // IdP identity OK nhưng tài khoản bật MFA → bước 2 (KHÔNG có token nào được lưu).
          setPendingToken(res.pendingToken);
          setPhase("mfa");
          return;
        }
        // Session stored by ssoExchange(). AuthProvider bootstrapped BEFORE the token existed,
        // so refresh its user state before the client-side hop or RequireAuth would bounce us.
        try { await refetchMe(); } catch { /* /home re-resolves auth on its own */ }
        router.replace("/home");
      } catch (e) {
        setPhase("error");
        if (e instanceof ApiError) {
          // 401 SSO_HANDOFF_INVALID / 503 MFA_UNAVAILABLE map to friendly text; anything
          // else falls back to the server message or the generic SSO failure line.
          const mapped = ssoErrorMessage(e.code);
          setErr(mapped && mapped !== SSO_ERROR_FALLBACK ? mapped : (e.message || SSO_ERROR_FALLBACK));
        } else {
          setErr(t("page.ssoComplete.err.network"));
        }
      }
    })();
  }, [code, refetchMe, router]);

  // Bước 2 — đổi pendingToken + mã TOTP/dự phòng lấy session (POST /auth/mfa/verify).
  async function handleVerifyMfa(e: React.FormEvent) {
    e.preventDefault();
    if (!pendingToken) return;
    const value = mfaCode.trim();
    if (!value) {
      setErr(t("page.auth.mfa.err.missing"));
      return;
    }
    if (retryAfter > 0) return;
    setLoading(true);
    setErr(null);
    try {
      await completeMfa(pendingToken, value);
      router.replace("/home");
    } catch (e2) {
      if (e2 instanceof ApiError) {
        if (e2.status === 429) {
          const ra = (e2.details as { retryAfter?: number })?.retryAfter ?? 60;
          setRetryAfter(ra);
          setErr(t("page.auth.tooMany").replace("{n}", String(ra)));
        } else if (e2.code === "MFA_CODE_INVALID") {
          // pendingToken KHÔNG bị đốt khi mã sai — giữ nguyên bước 2, cho nhập lại.
          setErr(t("page.auth.mfa.err.codeInvalid"));
        } else if (e2.code === "MFA_PENDING_INVALID") {
          // Phiên bước-2 hết hạn (5 phút) / đã dùng → phải chạy lại SSO từ đầu.
          setPendingToken(null);
          setPhase("error");
          setErr(t("page.auth.mfa.err.expired"));
        } else if (e2.status === 503) {
          setErr(e2.message || t("page.auth.mfa.err.unavailable"));
        } else {
          setErr(e2.message || t("page.auth.mfa.err.generic"));
        }
      } else {
        setErr(t("page.auth.mfa.err.generic"));
      }
      setLoading(false);
    }
  }

  const mm = String(Math.floor(retryAfter / 60)).padStart(2, "0");
  const ss = String(retryAfter % 60).padStart(2, "0");
  const locked = retryAfter > 0;

  if (phase === "mfa") {
    return (
      <AuthCard busy={loading}>
        <NexusLogo size={32} />
        <div className="flex flex-col gap-1">
          <h1 className="slds-text-heading_medium">{t("page.auth.mfa.title")}</h1>
          <p className="slds-text-color_weak">{t("page.ssoComplete.mfaSubtitle")}</p>
        </div>

        <form className="flex flex-col gap-4" onSubmit={handleVerifyMfa} noValidate>
          <Input
            label={t("page.auth.mfa.code")}
            value={mfaCode}
            onChange={(e) => { setMfaCode(e.target.value); if (err) setErr(null); }}
            placeholder="123456"
            inputMode="numeric"
            autoComplete="one-time-code"
            autoFocus
            maxLength={32}
            className="tabular-nums"
          />

          {err && <ScopedNotification theme="error">{err}</ScopedNotification>}

          <Button type="submit" variant="brand" stretch disabled={loading || locked}>
            {loading
              ? t("page.auth.mfa.loading")
              : locked
              ? `${t("page.auth.retryIn")} ${mm}:${ss}`
              : t("page.auth.mfa.submit")}
          </Button>
        </form>

        <p className="slds-text-body_small slds-text-color_weak slds-text-align_center">{t("page.auth.mfa.note")}</p>
      </AuthCard>
    );
  }

  if (phase === "error") {
    return (
      <AuthCard>
        <NexusLogo size={32} />
        <h1 className="slds-text-heading_medium">{t("page.ssoComplete.error.title")}</h1>
        <ScopedNotification theme="error">{err ?? SSO_ERROR_FALLBACK}</ScopedNotification>
        <Link href="/login" className={buttonClassName("brand", { stretch: true })}>
          {t("page.ssoComplete.backToLogin")}
        </Link>
      </AuthCard>
    );
  }

  // phase === "exchanging"
  return (
    <AuthCard>
      <NexusLogo size={32} />
      <div className="flex flex-col items-center gap-3 py-4">
        <Spinner inline size="medium" variant="brand" />
        <p className="slds-text-heading_small">{t("page.ssoComplete.exchanging")}</p>
        <p className="slds-text-color_weak">{t("page.ssoComplete.wait")}</p>
      </div>
    </AuthCard>
  );
}
