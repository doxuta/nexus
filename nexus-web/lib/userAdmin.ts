// lib/userAdmin.ts — pure helpers of the Studio "Người dùng" page (specs/078 lát u7-nguoi-dung-danh-sach).
import { ApiError, type AdminUser } from "@/lib/api";
import { t } from "@/lib/i18n";

/** Password policy of the server (password_policy.go, RM-17): at least 8 characters, not blank, at most 72 bytes. */
export const PASSWORD_MIN_LENGTH = 8;
export const GENERATED_PASSWORD_LENGTH = 16;

// No look-alikes (0/O, 1/l/I): the builder reads this out or pastes it into a chat.
const ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";

/** Counts for the lede "{n} người, {m} đang hoạt động, {k} đã khoá". */
export function userLede(users: readonly Pick<AdminUser, "status">[]): { n: number; m: number; k: number } {
  const m = users.filter((u) => u.status === "active").length;
  return { n: users.length, m, k: users.length - m };
}

/** 16 characters from crypto.getRandomValues (never Math.random). Rejection sampling: no modulo bias. */
export function generatePassword(): string {
  const limit = 256 - (256 % ALPHABET.length);
  let out = "";
  while (out.length < GENERATED_PASSWORD_LENGTH) {
    const bytes = crypto.getRandomValues(new Uint8Array(GENERATED_PASSWORD_LENGTH * 2));
    for (const b of bytes) {
      if (b < limit && out.length < GENERATED_PASSWORD_LENGTH) out += ALPHABET[b % ALPHABET.length];
    }
  }
  return out;
}

/** The lock button of a row: an active person is locked, anyone else is unlocked. */
export function userRowActions(u: Pick<AdminUser, "status">): { lock: "lock" | "unlock"; next: "inactive" | "active" } {
  return u.status === "active" ? { lock: "lock", next: "inactive" } : { lock: "unlock", next: "active" };
}

/** Column "Cấp": null when the server did not say (`canManageUsers` absent) and for an ordinary employee. */
export function describeUserTier(u: Pick<AdminUser, "isAdmin" | "canManageUsers">): "owner" | "admin" | null {
  if (u.canManageUsers === undefined) return null;
  if (u.canManageUsers) return "owner";
  return u.isAdmin ? "admin" : null;
}

/** The sentence of one engine error code. PASSWORD_TOO_WEAK is on purpose not mapped: the server names the broken rule. */
export function userErrorSentence(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case "LAST_ADMIN": return t("studio.operate.users.err.lastAdmin");
      case "USER_NOT_ACTIVE": return t("studio.operate.users.err.notActive");
      case "USER_NOT_FOUND": return t("studio.operate.users.err.notFound");
      case "USER_STATUS_INVALID": return t("studio.operate.users.err.status");
      case "USER_TIER_CONFLICT": return t("studio.operate.users.err.tierConflict");
      case "USER_ALREADY_EXISTS": return t("studio.operate.users.err.exists");
      case "SELF_DELETE_FORBIDDEN": return t("studio.operate.userDetail.deleteSelf");
      default: return e.message || t("studio.operate.users.err.generic");
    }
  }
  return t("studio.operate.users.err.generic");
}
