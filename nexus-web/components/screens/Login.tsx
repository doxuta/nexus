"use client";

// Login — màn đăng nhập standalone. Spec: 2-auth.md + api-conventions §3b.3 (MFA 2 bước).
// Frame (brand band + card giữa trang) do AuthShell trong app/(auth)/layout.tsx cung cấp —
// dùng chung cho cả forgot/reset/SSO complete. File này CHỈ còn form + logic.
// SLDS (specs/038 T033): không có blueprint đăng nhập → ghép `cards` + `form-element`/`input` +
// `checkbox` + `buttons` + `scoped-notifications` (lỗi của form) + `spinners` (RESEARCH §5.2).
// Wires: POST /auth/login (via AuthProvider.login), remember-me, ?next= redirect,
// 429 Retry-After countdown, anti-enumeration 401 message.
// MFA: login() trả { status:"mfa", pendingToken } → form chuyển sang bước 2 nhập mã 6 số
// (hoặc mã dự phòng) → POST /auth/mfa/verify (AuthProvider.completeMfa) → vào app.
import { Suspense, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Eye, EyeOff, KeyRound, ShieldCheck } from "lucide-react";
import { useAuth } from "@/lib/auth/AuthProvider";
import { ApiError, getPublicBranding, ssoPublicProviders, ssoStartUrl, type PublicSSOProvider } from "@/lib/api";
import { ssoErrorMessage } from "@/lib/ssoError";
import { t } from "@/lib/i18n";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { useBranding } from "@/components/brand/BrandingBoot";
import { applyBranding } from "@/lib/branding";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { AuthCard } from "./AuthShell";

// Màn 11 — Đăng nhập standalone (KHÔNG shell).
export function Login() {
  return (
    <Suspense fallback={null}>
      <LoginForm />
    </Suspense>
  );
}

/** Form-level error (scoped to the card, not the whole app — that would be `alert`). */
function FormError({ children }: { children: React.ReactNode }) {
  return <ScopedNotification theme="error">{children}</ScopedNotification>;
}

function LoginForm() {
  const { login, completeMfa, user, isLoading: authLoading } = useAuth();
  const searchParams = useSearchParams();
  const [org, setOrg] = useState("");
  const [email, setEmail] = useState("");
  const [pwd, setPwd] = useState("");
  const [show, setShow] = useState(false);
  const [remember, setRemember] = useState(false);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // AO-THUONG-HIEU: brand the login screen for the tenant being typed — debounce the public
  // branding fetch on the org field; applyBranding() drives accent vars + favicon + the hook.
  // persist:false — a TYPED slug is a preview, never cached (glancing at another tenant's login
  // must not pin their brand on this machine); the seq guard drops out-of-order responses so a
  // slow answer for a stale slug cannot overwrite the one being typed (review SHOULD-FIX 2).
  const branding = useBranding();
  const brandSeq = useRef(0);
  useEffect(() => {
    const slug = org.trim().toLowerCase();
    if (!slug) return;
    const seq = ++brandSeq.current;
    const timer = setTimeout(() => {
      getPublicBranding(slug)
        .then((b) => { if (brandSeq.current === seq) applyBranding(b, { persist: false }); })
        .catch(() => {});
    }, 450);
    return () => clearTimeout(timer);
  }, [org]);
  const [retryAfter, setRetryAfter] = useState(0);
  // MFA step 2: non-null = credentials accepted, awaiting TOTP/backup code (token 5 phút, dùng 1 lần)
  const [pendingToken, setPendingToken] = useState<string | null>(null);
  const [mfaCode, setMfaCode] = useState("");
  // SSO (ENG-AUTH2 #8): panel nhập provider (fallback). SSO-PROVIDER-PUBLIC-LIST (2026-07-25):
  // GET /auth/sso/:tenant/providers nay là PUBLIC → khi tenant đã gõ có provider bật, login
  // hiện NÚT BRANDED ("Đăng nhập bằng Google") thay vì bắt nhập issuer tay; danh sách rỗng /
  // lỗi / SSO tắt → giữ nguyên panel nhập tay như cũ (honest fallback).
  const [ssoOpen, setSsoOpen] = useState(false);
  const [ssoProvider, setSsoProvider] = useState("");
  const [ssoErr, setSsoErr] = useState<string | null>(null);
  const [ssoFetched, setSsoFetched] = useState<{ slug: string; list: PublicSSOProvider[] }>({ slug: "", list: [] });
  // Debounce theo org — cùng nhịp 450ms với branding fetch phía trên; org đổi giữa chừng
  // (seq guard) thì kết quả cũ bị bỏ, không vẽ nút của tenant khác.
  const ssoListSeq = useRef(0);
  useEffect(() => {
    const slug = org.trim();
    const seq = ++ssoListSeq.current;
    if (!slug) return;
    const timer = setTimeout(() => {
      ssoPublicProviders(slug)
        .then((ps) => { if (ssoListSeq.current === seq) setSsoFetched({ slug, list: ps }); })
        .catch(() => { if (ssoListSeq.current === seq) setSsoFetched({ slug, list: [] }); });
    }, 450);
    return () => clearTimeout(timer);
  }, [org]);
  // The buttons belong to the slug they were fetched for: an emptied or retyped org hides them at
  // once (derived here, not reset inside the effect — no cascading render).
  const ssoList = ssoFetched.slug === org.trim() ? ssoFetched.list : [];
  // #48 SSO-FE-REDIRECT: a failed SSO callback 302s back here as /login?ssoError=<CODE>
  // (coarse, anti-enumeration). Map it once to a Vietnamese banner above the form.
  const ssoCallbackErr = ssoErrorMessage(searchParams.get("ssoError"));
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // Redirect if already logged in
  useEffect(() => {
    if (!authLoading && user) {
      const next = searchParams.get("next") ?? "/home";
      window.location.href = next;
    }
  }, [user, authLoading, searchParams]);

  // Countdown for 429 lockout
  useEffect(() => {
    if (retryAfter <= 0) return;
    timerRef.current = setInterval(() => setRetryAfter((s) => (s > 0 ? s - 1 : 0)), 1000);
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
  }, [retryAfter]);

  const tooMany = (e: ApiError) => {
    const ra = (e.details as { retryAfter?: number })?.retryAfter ?? 60;
    setRetryAfter(ra);
    setErr(t("page.auth.tooMany").replace("{n}", String(ra)));
  };

  async function handleLogin(e: React.FormEvent) {
    e.preventDefault();
    if (!org || !email || !pwd) {
      setErr(t("page.login.err.missing"));
      return;
    }
    if (retryAfter > 0) return;
    setLoading(true);
    setErr(null);
    try {
      const res = await login(org.trim(), email.trim(), pwd, remember);
      if (res.status === "mfa") {
        // Đúng mật khẩu nhưng tài khoản bật MFA → bước 2 (KHÔNG có token nào được lưu).
        setPendingToken(res.pendingToken);
        setMfaCode("");
        setLoading(false);
        return;
      }
      const next = searchParams.get("next") ?? "/home";
      window.location.href = next;
    } catch (e2) {
      if (e2 instanceof ApiError && e2.status === 429) {
        // Parse Retry-After from details or default to 60s
        tooMany(e2);
      } else if (e2 instanceof ApiError && e2.status === 503 && e2.code === "MFA_UNAVAILABLE") {
        // MFA-ENFORCE fail-closed: đúng mật khẩu nhưng server thiếu khóa MFA — báo thật,
        // đừng đổ lỗi "sai mật khẩu".
        setErr(e2.message || t("page.auth.mfa.err.unavailable"));
      } else if (e2 instanceof ApiError && e2.status === 401) {
        // Anti-enumeration: always the same generic message for a real credential mismatch.
        setErr(t("page.login.err.invalid"));
      } else {
        // MED-2: any OTHER status (500, network fault, the defensive MFA_PENDING_MALFORMED
        // throw from login() when the server's mfaPending reply is malformed, etc.) is NOT a
        // wrong-password case — the anti-enumeration message would falsely blame the user's
        // credentials for a server/client fault. Show the real message instead.
        setErr(e2 instanceof ApiError ? (e2.message || t("page.login.err.generic")) : t("page.login.err.generic"));
      }
      setLoading(false);
    }
  }

  // Bước 2 — đổi pendingToken + mã TOTP/dự phòng lấy token pair thật (POST /auth/mfa/verify).
  async function handleVerifyMfa(e: React.FormEvent) {
    e.preventDefault();
    if (!pendingToken) return;
    const code = mfaCode.trim();
    if (!code) {
      setErr(t("page.auth.mfa.err.missing"));
      return;
    }
    if (retryAfter > 0) return;
    setLoading(true);
    setErr(null);
    try {
      await completeMfa(pendingToken, code);
      const next = searchParams.get("next") ?? "/home";
      window.location.href = next;
    } catch (e2) {
      if (e2 instanceof ApiError) {
        if (e2.status === 429) {
          tooMany(e2);
        } else if (e2.code === "MFA_CODE_INVALID") {
          // pendingToken KHÔNG bị đốt khi mã sai — giữ nguyên bước 2, cho nhập lại.
          setErr(t("page.auth.mfa.err.codeInvalid"));
        } else if (e2.code === "MFA_PENDING_INVALID") {
          // Phiên bước-2 hết hạn (5 phút) / đã dùng → quay lại bước 1.
          setPendingToken(null);
          setMfaCode("");
          setPwd("");
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

  function backToStep1() {
    setPendingToken(null);
    setMfaCode("");
    setErr(null);
    setLoading(false);
  }

  // SSO = OAuth authorization-code redirect: điều hướng CẢ TRANG (không fetch) — response /start
  // là 302 tới IdP + cookie bind sso_state chỉ đi theo top-level navigation. Sau khi IdP xác
  // thực, backend /callback 302 về /auth/sso/complete?code=<handoff> (#48 SSO-FE-REDIRECT) —
  // trang đó đổi code lấy session; lỗi callback quay lại đây dạng /login?ssoError=<CODE>.
  function handleSsoStart(e: React.FormEvent) {
    e.preventDefault();
    if (!org.trim()) {
      setSsoErr(t("page.login.sso.err.org"));
      return;
    }
    if (!ssoProvider.trim()) {
      setSsoErr(t("page.login.sso.err.provider"));
      return;
    }
    window.location.assign(ssoStartUrl(org, ssoProvider));
  }

  const mm = String(Math.floor(retryAfter / 60)).padStart(2, "0");
  const ss = String(retryAfter % 60).padStart(2, "0");
  const locked = retryAfter > 0;
  const lockedLabel = `${t("page.auth.retryIn")} ${mm}:${ss}`;

  // Brand mark: branded tenant → its logo/app name; default → Nexus. Shared by both steps.
  const brandMark = branding.logoUrl || branding.appName ? (
    <span className="flex items-center gap-2">
      {branding.logoUrl ? (
        // eslint-disable-next-line @next/next/no-img-element -- tenant-supplied URL/data-URI
        <img src={branding.logoUrl} alt="" className="size-8 object-contain" />
      ) : (
        <NexusLogo size={32} variant="glyph" />
      )}
      {branding.appName && <span className="slds-text-heading_small">{branding.appName}</span>}
    </span>
  ) : (
    <NexusLogo size={32} />
  );

  // ── Bước 2: nhập mã MFA ──
  if (pendingToken) {
    return (
      <AuthCard busy={loading}>
        {brandMark}
        <div className="flex flex-col gap-1">
          <h1 className="slds-text-heading_medium">{t("page.auth.mfa.title")}</h1>
          <p className="slds-text-color_weak">{t("page.login.mfaSubtitle")}</p>
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

          {err && <FormError>{err}</FormError>}

          <Button type="submit" variant="brand" stretch disabled={loading || locked}>
            {loading ? t("page.auth.mfa.loading") : locked ? lockedLabel : t("page.auth.mfa.submit")}
          </Button>
          <Button variant="base" stretch onClick={backToStep1}>
            {t("page.auth.backToLogin")}
          </Button>
        </form>

        <p className="slds-text-body_small slds-text-color_weak slds-text-align_center">{t("page.auth.mfa.note")}</p>
      </AuthCard>
    );
  }

  return (
    <AuthCard busy={loading}>
      {/* Brand — tenant's own when branded (AO-THUONG-HIEU) */}
      {brandMark}
      <div className="flex flex-col gap-1">
        <h1 className="slds-text-heading_medium">{t("auth.login.title")}</h1>
        <p className="slds-text-color_weak">{t("page.login.subtitle")}</p>
      </div>

      {ssoCallbackErr && <FormError>{ssoCallbackErr}</FormError>}

      <form className="flex flex-col gap-4" onSubmit={handleLogin} noValidate>
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
        <Input
          label={t("auth.login.password")}
          type={show ? "text" : "password"}
          value={pwd}
          onChange={(e) => setPwd(e.target.value)}
          placeholder="••••••••••"
          autoComplete="current-password"
        />

        <div className="flex flex-wrap items-center justify-between gap-2">
          <Checkbox
            label={t("auth.login.rememberMe")}
            checked={remember}
            onChange={(e) => setRemember(e.target.checked)}
          />
          <Button variant="base" iconLeft={show ? EyeOff : Eye} onClick={() => setShow((s) => !s)}>
            {show ? t("page.auth.hidePassword") : t("page.auth.showPassword")}
          </Button>
        </div>

        {err && <FormError>{err}</FormError>}

        <Button type="submit" variant="brand" stretch disabled={loading || locked}>
          {loading ? t("auth.login.loading") : locked ? lockedLabel : t("auth.login.submit")}
        </Button>

        <p className="slds-text-align_center">
          <Link href="/forgot-password">{t("auth.login.forgotPassword")}</Link>
        </p>
      </form>

      {/* ── SSO (ENG-AUTH2 #8) — form riêng, KHÔNG lồng trong form mật khẩu ── */}
      <div className="flex flex-col gap-3">
        <p className="slds-text-body_small slds-text-color_weak slds-text-align_center">{t("page.login.or")}</p>

        {/* SSO-PROVIDER-PUBLIC-LIST: tenant đã gõ có provider bật → nút branded trực tiếp
            (Google nhận diện qua issuer accounts.google.com). Bấm = top-level navigation tới
            /start (bắt buộc — cookie bind sso_state chỉ đi theo navigation, không fetch). */}
        {ssoList.map((p) => {
          const isGoogle = p.issuer.includes("accounts.google.com");
          return (
            <Button
              key={p.id}
              variant="neutral"
              stretch
              iconLeft={isGoogle ? undefined : KeyRound}
              onClick={() => {
                if (!org.trim()) { setSsoErr(t("page.login.sso.err.orgShort")); return; }
                window.location.assign(ssoStartUrl(org, p.id));
              }}
            >
              {isGoogle && <GoogleGlyph />}
              {t("page.login.sso.with").replace("{name}", isGoogle ? "Google" : p.displayName)}
            </Button>
          );
        })}

        {ssoList.length === 0 && !ssoOpen ? (
          <Button variant="neutral" stretch iconLeft={KeyRound} onClick={() => setSsoOpen(true)}>
            {t("page.login.sso.open")}
          </Button>
        ) : ssoList.length === 0 && ssoOpen ? (
          <form className="flex flex-col gap-3" onSubmit={handleSsoStart} noValidate>
            <Input
              label={t("page.login.sso.provider")}
              value={ssoProvider}
              onChange={(e) => { setSsoProvider(e.target.value); if (ssoErr) setSsoErr(null); }}
              placeholder="https://accounts.google.com"
              autoFocus
              inputMode="url"
            />

            {ssoErr && <FormError>{ssoErr}</FormError>}

            <Button type="submit" variant="neutral" stretch>
              {t("page.login.sso.continue")}
            </Button>

            {/* Tổ chức chưa có provider công khai (hoặc chưa gõ mã tổ chức) → /start bắt buộc
                chỉ định provider, nên phải nhập tay issuer/ID. */}
            <p className="slds-text-body_small slds-text-color_weak">{t("page.login.sso.help")}</p>
          </form>
        ) : null}
      </div>

      <p className="slds-text-body_small slds-text-color_weak flex items-center justify-center gap-1">
        <Icon icon={ShieldCheck} size="xx-small" tone="light" /> {t("page.login.secure")}
      </p>
    </AuthCard>
  );
}

// Official multi-color Google "G" (static SVG, no external asset — CSP-safe). The hex fills are
// Google's mandated brand colours (sign-in branding guidelines), not a Nexus style choice.
function GoogleGlyph() {
  return (
    <svg className="slds-button__icon slds-button__icon_left" viewBox="0 0 24 24" aria-hidden="true">
      <path fill="#4285F4" d="M23.52 12.27c0-.85-.08-1.66-.22-2.45H12v4.63h6.46a5.52 5.52 0 0 1-2.4 3.62v3h3.88c2.27-2.09 3.58-5.17 3.58-8.8Z" />
      <path fill="#34A853" d="M12 24c3.24 0 5.96-1.07 7.94-2.91l-3.88-3c-1.08.72-2.45 1.15-4.06 1.15-3.13 0-5.78-2.11-6.72-4.95H1.27v3.1A12 12 0 0 0 12 24Z" />
      <path fill="#FBBC05" d="M5.28 14.29a7.21 7.21 0 0 1 0-4.58v-3.1H1.27a12 12 0 0 0 0 10.78l4.01-3.1Z" />
      <path fill="#EA4335" d="M12 4.77c1.76 0 3.35.6 4.6 1.8l3.44-3.44A11.98 11.98 0 0 0 1.27 6.61l4.01 3.1C6.22 6.87 8.87 4.77 12 4.77Z" />
    </svg>
  );
}
