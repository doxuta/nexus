"use client";

// Show-once secret (specs/078 FR-006): a password, a reset link, a client secret, shown ONCE where it was made.
// Carries the pattern of TenantCredentialsReveal (copy per row + whole block + bold warning + "Tôi đã lưu" gating "Xong")
// and the secret step of the ApiKeys modal. The value lives only in props/state: never in a query/mutation cache,
// URL, storage or console. The caller must hand it over from a direct `await`, not a useMutation (mutation cache keeps variables/data).
// ponytail: this stops leaks on the FRONT end only; it does not replace server-side secrecy (the server keeps only a hash).
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Check, Copy } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { fill } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { useHoldNavigation } from "@/lib/navigation-blocker";
import { t } from "@/lib/i18n";

export interface SecretRow {
  label: string;
  value: string;
}

export interface SecretOnceBlockProps {
  rows: SecretRow[];
  /** One line under the warning (what the value is for). */
  hint?: string;
  /** Called by "Xong"; the caller unmounts the block, which is what removes the secret from the DOM. */
  onDone: () => void;
  /** Extra warning text, shown in bold under the notification. */
  children?: ReactNode;
}

export function SecretOnceBlock({ rows, hint, onDone, children }: SecretOnceBlockProps) {
  const box = useRef<HTMLDivElement>(null);
  const opener = useRef<HTMLElement | null>(null);
  const [acked, setAcked] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  // Leaving by link / back is stopped by the NavigationBlockerProvider; reload and closing the tab need our own listener,
  // which the provider does not install.
  useHoldNavigation(true);
  useEffect(() => {
    const onUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onUnload);
    return () => window.removeEventListener("beforeunload", onUnload);
  }, []);

  // Focus moves into the block when it appears, and returns to the button that made it once it is gone.
  useEffect(() => {
    const from = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    opener.current = from;
    box.current?.focus();
    return () => {
      const target = opener.current;
      if (target?.isConnected) target.focus();
    };
  }, []);

  const copy = async (key: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setFailed(false);
      setCopied(key);
      setTimeout(() => setCopied((c) => (c === key ? null : c)), 2000);
    } catch {
      setFailed(true);
    }
  };
  const block = rows.map((r) => `${r.label}: ${r.value}`).join("\n");

  return (
    <div ref={box} tabIndex={-1} data-secret-once className="slds-theme_shade slds-p-around_medium flex flex-col gap-3">
      <ScopedNotification theme="warning">
        <strong>{t("studio.operate.secret.leaveWarning")}</strong>
      </ScopedNotification>
      {children && <div className="font-bold">{children}</div>}
      {hint && <p className="slds-text-body_small slds-text-color_weak">{hint}</p>}
      {rows.map((r) => (
        <div key={r.label} className="flex items-end gap-2">
          <div className="min-w-0 flex-1">
            <div className="slds-form-element__label">{r.label}</div>
            <code className="block break-all font-mono slds-text-body_regular">{r.value}</code>
          </div>
          <Button
            iconLeft={copied === r.label ? Check : Copy}
            onClick={() => copy(r.label, r.value)}
            aria-label={fill(t("studio.operate.secret.copyOne"), { label: r.label })}
          >
            {t("studio.operate.secret.copy")}
          </Button>
        </div>
      ))}
      {rows.length > 1 && (
        <div>
          <Button iconLeft={copied === "\u0000block" ? Check : Copy} onClick={() => copy("\u0000block", block)}>
            {t("studio.operate.secret.copyBlock")}
          </Button>
        </div>
      )}
      <div aria-live="polite" className="slds-text-body_small">
        {copied ? t("studio.operate.secret.copied") : failed ? t("studio.operate.secret.copyFail") : ""}
      </div>
      <Checkbox label={t("studio.operate.secret.ack")} checked={acked} onChange={(e) => setAcked(e.target.checked)} />
      <div>
        <Button variant="brand" onClick={onDone} disabled={!acked}>
          {t("studio.operate.secret.done")}
        </Button>
      </div>
    </div>
  );
}
