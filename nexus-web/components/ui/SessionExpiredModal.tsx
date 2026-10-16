"use client";

// SessionExpiredModal — shown when 401 → refresh-fail forces re-auth.
// Pattern: mount in the app shell; visibility driven by AuthProvider "sessionExpired" flag.
// On redirect: preserves ?next= so user lands back where they were.
// No silent data loss: modal blocks UI before redirect.

import { useEffect, useState } from "react";
import { LogIn, ShieldAlert } from "lucide-react";
import { usePathname, useRouter } from "next/navigation";

interface Props {
  /** Set to true when the auth session has expired and cannot be refreshed */
  open: boolean;
  /** Called after the user clicks "Đăng nhập lại" and redirect fires */
  onClose?: () => void;
}

export function SessionExpiredModal({ open, onClose }: Props) {
  const router = useRouter();
  const pathname = usePathname();
  const [countdown, setCountdown] = useState(5);

  // Auto-redirect after 5 seconds to avoid indefinitely blocking UI
  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (!open) setCountdown(5);
  }

  useEffect(() => {
    if (!open) return;
    const interval = setInterval(() => {
      setCountdown((c) => {
        if (c <= 1) {
          clearInterval(interval);
          redirect();
          return 0;
        }
        return c - 1;
      });
    }, 1000);
    return () => clearInterval(interval);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  function redirect() {
    const next = encodeURIComponent(pathname ?? "/");
    onClose?.();
    router.replace(`/login?next=${next}&reason=expired`);
  }

  if (!open) return null;

  return (
    // Overlay — blocks interaction, no silent data loss
    <div
      className="fixed inset-0 z-[200] flex items-center justify-center bg-bg/80 backdrop-blur-sm"
      aria-modal="true"
      role="dialog"
      aria-labelledby="session-expired-title"
    >
      <div className="mx-4 w-full max-w-[380px] overflow-hidden rounded-card border border-dg/30 bg-panel shadow-pop">
        <div className="flex items-center gap-3 border-b border-bd px-5 py-4">
          <span className="grid size-9 place-items-center rounded-full bg-dgd">
            <ShieldAlert className="size-5 text-dgf" strokeWidth={2} />
          </span>
          <div>
            <h2
              id="session-expired-title"
              className="font-display text-title font-semibold text-fg"
            >
              Phiên đăng nhập hết hạn
            </h2>
            <p className="text-sub text-fnt">Không thể làm mới phiên tự động.</p>
          </div>
        </div>
        <div className="px-5 py-4">
          <p className="text-body leading-relaxed text-mut">
            Phiên của bạn đã hết hạn và không thể tự động gia hạn.
            Trang hiện tại sẽ được giữ nguyên sau khi đăng nhập lại.
          </p>
          <button
            onClick={redirect}
            className="mt-4 flex w-full items-center justify-center gap-2 rounded-input bg-ac py-2.5 text-13-5px font-medium text-acf transition-colors hover:bg-ac2"
          >
            <LogIn className="size-4" strokeWidth={2} />
            Đăng nhập lại ({countdown}s)
          </button>
        </div>
      </div>
    </div>
  );
}
