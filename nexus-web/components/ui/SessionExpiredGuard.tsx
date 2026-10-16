"use client";

// SessionExpiredGuard — mounts at root, listens via AuthProvider, shows modal on session expiry.
// Intercepts "nexus:session-expired" events from silentRefresh failures and redirects gracefully
// to /login?next=<current-path>&reason=expired — no silent data loss.

import { useAuth } from "@/lib/auth/AuthProvider";
import { SessionExpiredModal } from "@/components/ui/SessionExpiredModal";

export function SessionExpiredGuard() {
  const { sessionExpired, clearExpired } = useAuth();
  return <SessionExpiredModal open={sessionExpired} onClose={clearExpired} />;
}
