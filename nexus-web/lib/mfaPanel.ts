// mfaPanel — pure state machine for the MFA self-service card (Profile → Bảo mật).
// Backend contract: docs/api-conventions.md §3b (ENG-AUTH2 #7). Kept as a plain reducer so the
// whole flow (not-enrolled → enroll → activate → backup-codes → enabled → disable) is unit-
// testable without rendering a component or mocking fetch.
//
// STATE SOURCE (FE-CATCHUP-1, 2026-07-08): GET /auth/me now ALWAYS carries the honest
// `mfaEnabled: true|false` flag — the card feeds it in via SYNC_STATUS so the panel opens
// KNOWING on/off instead of starting "unknown". The machine still boots at idle/unknown
// (SSR-safe: no localStorage read in an initializer) and keeps the pre-flag error-response
// discovery as belt-and-suspenders for an older BE / a flag gone stale mid-session:
//   • enroll → 409 MFA_ALREADY_ENABLED  ⇒ jump to "enabled"
//   • activate/disable → 422 MFA_NOT_ENROLLED ⇒ jump back to "idle" (known disabled)
// SYNC_STATUS only applies in RESTING phases (idle/enabled) — it must never stomp an
// in-flight enroll/activate/backup-codes/disable step the user is in the middle of.

import { ApiError } from "@/lib/api";

// ── Phases ──────────────────────────────────────────────────────────────────────────
export type MfaPhase =
  /** Chưa bật (hoặc chưa biết) — entry point. `known` phân biệt "server xác nhận đã tắt" vs "chưa rõ". */
  | { phase: "idle"; known: "unknown" | "disabled" }
  /** Step-up: nhập mật khẩu để gọi POST /auth/mfa/enroll. */
  | { phase: "enroll-password" }
  /** Enroll xong: hiển thị secret + otpauthUrl; nhập mã 6 số đầu tiên để activate. */
  | { phase: "activate"; secret: string; otpauthUrl: string }
  /** Activate xong: 10 backup code hiển thị MỘT LẦN — chờ xác nhận "đã lưu". */
  | { phase: "backup-codes"; codes: string[] }
  /** MFA đang bật (đã xác nhận qua activate thành công hoặc 409 từ enroll). */
  | { phase: "enabled" }
  /**
   * Nhập mật khẩu + mã (TOTP hoặc backup) để tắt. `from` ghi lại phase XUẤT PHÁT (server đã
   * xác nhận "enabled" hay chưa — vào từ "idle" nghĩa là user tự nói "đã bật từ trước", KHÔNG
   * phải server xác nhận) để CANCEL/lỗi biết trả về đâu — không bao giờ bịa "đang bật" khi
   * chưa có xác nhận thật (MED-1: trước đây CANCEL luôn nhảy → "enabled" bất kể xuất phát).
   */
  | { phase: "disable"; from: "enabled" | "idle" }
  /** 503 MFA_UNAVAILABLE — server thiếu MFA_SECRET_ENC_KEY. Honest-disabled, không crash. */
  | { phase: "unavailable" };

export interface MfaPanelState {
  step: MfaPhase;
  /** Thông điệp lỗi hiển thị inline (null = không lỗi). */
  error: string | null;
  /** true trong lúc một API call đang bay — khoá nút submit. */
  busy: boolean;
}

export const initialMfaState: MfaPanelState = {
  step: { phase: "idle", known: "unknown" },
  error: null,
  busy: false,
};

// ── Events ──────────────────────────────────────────────────────────────────────────
export type MfaEvent =
  | { type: "SYNC_STATUS"; enabled: boolean }                  // /auth/me mfaEnabled → resting phase thật
  | { type: "START_ENROLL" }                                   // idle → enroll-password
  | { type: "START_DISABLE" }                                  // idle | enabled → disable(from:current)
  | { type: "CANCEL" }                                         // any input phase → idle/enabled
  | { type: "SUBMIT" }                                         // đánh dấu busy trước khi gọi API
  | { type: "ENROLL_OK"; secret: string; otpauthUrl: string }  // enroll-password → activate
  | { type: "ACTIVATE_OK"; codes: string[] }                   // activate → backup-codes
  | { type: "CODES_SAVED" }                                    // backup-codes → enabled
  | { type: "DISABLE_OK" }                                     // disable → idle (known disabled)
  | { type: "FAIL"; error: unknown };                          // ApiError → message + có thể nhảy phase

// ── Error mapping ───────────────────────────────────────────────────────────────────
// Dịch ApiError của §3b sang thông điệp tiếng Việt + phase đích (nếu lỗi tiết lộ trạng thái).
interface FailureOutcome {
  message: string;
  /** Khi lỗi cho biết trạng thái thật (409/422/503) → nhảy thẳng sang phase tương ứng. */
  jumpTo?: MfaPhase;
}

export function mfaFailureOutcome(e: unknown, current: MfaPhase): FailureOutcome {
  if (!(e instanceof ApiError)) {
    return { message: "Không thể xử lý yêu cầu MFA. Thử lại." };
  }
  switch (e.code) {
    case "INVALID_CREDENTIALS":
      // Step-up password sai. Nếu đang ở activate (mật khẩu được giữ lại từ bước enroll)
      // → quay về enroll-password để nhập lại (re-enroll sẽ sinh secret mới — chấp nhận).
      return current.phase === "activate"
        ? { message: "Mật khẩu không còn đúng — nhập lại để tiếp tục.", jumpTo: { phase: "enroll-password" } }
        : { message: "Mật khẩu không đúng." };
    case "MFA_PASSWORD_REQUIRED":
      return { message: "Cần nhập mật khẩu để xác nhận thao tác." };
    case "MFA_CODE_INVALID":
      return { message: "Mã xác thực không đúng hoặc đã được sử dụng. Thử lại." };
    case "MFA_ALREADY_ENABLED":
      // Enroll khi đã bật → giờ ta BIẾT trạng thái: enabled.
      return { message: "Tài khoản này đã bật xác thực hai lớp.", jumpTo: { phase: "enabled" } };
    case "MFA_NOT_ENROLLED":
      // Activate/disable khi chưa enroll → BIẾT trạng thái: disabled.
      return {
        message: "Tài khoản chưa đăng ký xác thực hai lớp.",
        jumpTo: { phase: "idle", known: "disabled" },
      };
    case "MFA_TOO_MANY_ATTEMPTS": {
      const ra = typeof e.details.retryAfter === "number" ? e.details.retryAfter : undefined;
      return {
        message: ra
          ? `Quá nhiều lần thử sai — thử lại sau khoảng ${Math.ceil(ra / 60)} phút.`
          : "Quá nhiều lần thử sai. Vui lòng thử lại sau.",
      };
    }
    case "MFA_UNAVAILABLE":
      return {
        message: "Hệ thống chưa cấu hình khóa MFA — liên hệ quản trị.",
        jumpTo: { phase: "unavailable" },
      };
    case "UNAUTHORIZED":
      // req() không silent-refresh đường /auth/* → access token hết hạn lộ ra ở đây.
      return { message: "Phiên đăng nhập đã hết hạn — tải lại trang rồi thử lại." };
    default:
      return { message: e.message || "Không thể xử lý yêu cầu MFA. Thử lại." };
  }
}

// ── Reducer ─────────────────────────────────────────────────────────────────────────
export function mfaReduce(state: MfaPanelState, ev: MfaEvent): MfaPanelState {
  switch (ev.type) {
    case "SYNC_STATUS": {
      // Honest server-known state from /auth/me (mfaEnabled). Applied ONLY in resting phases
      // (idle/enabled) and never mid-request — a user standing in enroll/activate/backup-codes/
      // disable must not have the form yanked away by a background /auth/me refresh. The
      // action-error jumps (409/422) remain the source of truth inside a flow.
      if (state.busy) return state;
      if (ev.enabled) {
        return state.step.phase === "idle"
          ? { step: { phase: "enabled" }, error: state.error, busy: false }
          : state;
      }
      if (state.step.phase === "enabled") {
        // Server says OFF while we show ON (e.g. disabled from another session) — correct it.
        return { step: { phase: "idle", known: "disabled" }, error: state.error, busy: false };
      }
      if (state.step.phase === "idle" && state.step.known !== "disabled") {
        return { step: { phase: "idle", known: "disabled" }, error: state.error, busy: false };
      }
      return state;
    }
    case "START_ENROLL":
      return { step: { phase: "enroll-password" }, error: null, busy: false };
    case "START_DISABLE": {
      // Ghi lại nơi xuất phát: chỉ "enabled" khi ta THỰC SỰ đang ở phase đã-xác-nhận-bật;
      // bất kỳ nơi khác (vd idle, qua link "đã bật từ trước?") → "idle" (chưa xác nhận).
      const from: "enabled" | "idle" = state.step.phase === "enabled" ? "enabled" : "idle";
      return { step: { phase: "disable", from }, error: null, busy: false };
    }
    case "CANCEL":
      // MED-1: quay lại đúng nơi xuất phát — KHÔNG bao giờ tự nhận "đang bật" khi server
      // chưa xác nhận. Chỉ disable(from:"enabled") mới về enabled; mọi trường hợp khác
      // (kể cả disable(from:"idle") sau một lần thất bại) → idle/unknown.
      return {
        step: state.step.phase === "disable" && state.step.from === "enabled"
          ? { phase: "enabled" }
          : { phase: "idle", known: "unknown" },
        error: null,
        busy: false,
      };
    case "SUBMIT":
      return { ...state, error: null, busy: true };
    case "ENROLL_OK":
      return { step: { phase: "activate", secret: ev.secret, otpauthUrl: ev.otpauthUrl }, error: null, busy: false };
    case "ACTIVATE_OK":
      return { step: { phase: "backup-codes", codes: ev.codes }, error: null, busy: false };
    case "CODES_SAVED":
      return { step: { phase: "enabled" }, error: null, busy: false };
    case "DISABLE_OK":
      return { step: { phase: "idle", known: "disabled" }, error: null, busy: false };
    case "FAIL": {
      const { message, jumpTo } = mfaFailureOutcome(ev.error, state.step);
      return { step: jumpTo ?? state.step, error: message, busy: false };
    }
  }
}

// ── Display helpers ─────────────────────────────────────────────────────────────────
/** Nhóm secret base32 thành cụm 4 ký tự cho dễ chép tay (chỉ để HIỂN THỊ — copy dùng bản gốc). */
export function groupSecret(secret: string): string {
  return secret.replace(/(.{4})/g, "$1 ").trim();
}
