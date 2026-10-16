// lib/invite.ts — pure view-model for the "invite / reset-password link" dialog (W7 §11.3).
// The API returns { resetLink, token, expiresAt, ttlMinutes, emailQueued }. The dialog shows a
// copyable link; when SMTP is unwired the backend still returns a live link/token (honest — admin
// hands it over manually). This helper decides which string to surface and which badge to show.

export interface ResetLinkResult {
  resetLink: string;
  token: string;
  expiresAt: string;
  ttlMinutes: number;
  emailQueued: boolean;
}

export interface InviteLinkView {
  /** The string to display + copy. Prefers the full resetLink; falls back to the raw token. */
  copyValue: string;
  /** True when resetLink was empty and we fell back to the bare token. */
  usedTokenFallback: boolean;
  /** "sent" = email was queued (badge "Đã gửi mail"); "manual" = admin must hand the link over. */
  emailBadge: "sent" | "manual";
}

export function inviteLinkView(r: ResetLinkResult): InviteLinkView {
  const hasLink = typeof r.resetLink === "string" && r.resetLink.trim() !== "";
  return {
    copyValue: hasLink ? r.resetLink : r.token,
    usedTokenFallback: !hasLink,
    emailBadge: r.emailQueued ? "sent" : "manual",
  };
}
