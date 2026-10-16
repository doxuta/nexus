// lib/ssoError.ts — SSO redirect-flow error code → Vietnamese UI message (#48 SSO-FE-REDIRECT).
// Two producers of these codes:
//   • GET /auth/sso/callback failures 302 back to /login?ssoError=<CODE> (coarse, no detail —
//     anti-enumeration: the BE never says WHICH part of the identity was wrong).
//   • POST /auth/sso/exchange failures (401 SSO_HANDOFF_INVALID for every useless code).
// Separate from lib/errorMessages.ts errorMessage(): that takes an ApiError instance; here the
// code arrives as a bare query-param string with no error object to wrap.

const SSO_ERROR_MESSAGES: Record<string, string> = {
  SSO_STATE_INVALID: "Phiên SSO không hợp lệ hoặc đã hết hạn — thử đăng nhập lại.",
  SSO_STATE_MISMATCH: "Phiên SSO không khớp với trình duyệt này — thử đăng nhập lại.",
  SSO_EXCHANGE_FAILED: "Không đổi được mã xác thực với nhà cung cấp SSO — thử lại.",
  SSO_ID_TOKEN_INVALID: "Nhà cung cấp SSO trả về danh tính không hợp lệ — liên hệ quản trị viên.",
  SSO_LOGIN_NOT_AUTHORIZED: "Tài khoản này chưa được cấp quyền đăng nhập SSO — liên hệ quản trị viên.",
  SSO_PROVIDER_NOT_FOUND: "Không tìm thấy provider SSO — kiểm tra lại issuer hoặc ID đã nhập.",
  SSO_PROVIDER_DISCOVERY_FAILED: "Không kết nối được nhà cung cấp SSO — thử lại sau.",
  MFA_UNAVAILABLE: "Hệ thống chưa cấu hình khóa MFA — liên hệ quản trị.",
  SSO_NOT_CONFIGURED: "SSO chưa được bật trên máy chủ — liên hệ quản trị viên.",
  // POST /auth/sso/exchange: unknown / already used / expired handoff code (single-use, 120s).
  SSO_HANDOFF_INVALID: "Phiên đăng nhập SSO đã hết hạn hoặc đã được sử dụng — đăng nhập lại.",
};

export const SSO_ERROR_FALLBACK = "Đăng nhập SSO thất bại — thử lại.";

/**
 * Resolve an ?ssoError= code (or an exchange error code) to a Vietnamese message.
 * null/empty → null (no banner); unknown code → generic fallback (the code set may grow BE-side).
 */
export function ssoErrorMessage(code: string | null | undefined): string | null {
  if (!code) return null;
  return SSO_ERROR_MESSAGES[code] ?? SSO_ERROR_FALLBACK;
}
