import { describe, it, expect } from "vitest";
import { inviteLinkView, type ResetLinkResult } from "@/lib/invite";

const base: ResetLinkResult = {
  resetLink: "https://app.nexus.vn/reset?token=RAW123",
  token: "RAW123",
  expiresAt: "2026-07-03T11:00:00Z",
  ttlMinutes: 30,
  emailQueued: false,
};

describe("inviteLinkView — reset-link dialog view-model", () => {
  it("prefers the full resetLink when present", () => {
    const v = inviteLinkView(base);
    expect(v.copyValue).toBe(base.resetLink);
    expect(v.usedTokenFallback).toBe(false);
  });

  it("falls back to the raw token when resetLink is empty", () => {
    const v = inviteLinkView({ ...base, resetLink: "" });
    expect(v.copyValue).toBe("RAW123");
    expect(v.usedTokenFallback).toBe(true);
  });

  it("treats whitespace-only resetLink as empty", () => {
    const v = inviteLinkView({ ...base, resetLink: "   " });
    expect(v.usedTokenFallback).toBe(true);
    expect(v.copyValue).toBe("RAW123");
  });

  it("badge = 'manual' when email not queued (admin hands link over)", () => {
    expect(inviteLinkView({ ...base, emailQueued: false }).emailBadge).toBe("manual");
  });

  it("badge = 'sent' when email queued", () => {
    expect(inviteLinkView({ ...base, emailQueued: true }).emailBadge).toBe("sent");
  });
});
