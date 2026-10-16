// Tests for the pure MFA panel state machine (lib/mfaPanel.ts) — the full self-service flow
// not-enrolled → enroll → activate → backup-codes → enabled → disable, plus every §3b error
// jump (409 already-enabled, 422 not-enrolled, 503 unavailable, 401 wrong password/code).
import { describe, it, expect } from "vitest";
import { ApiError } from "@/lib/api";
import {
  mfaReduce, initialMfaState, groupSecret, mfaFailureOutcome, type MfaPanelState,
} from "@/lib/mfaPanel";

const err = (status: number, code: string, retryAfter?: number) =>
  new ApiError(status, code, code, retryAfter !== undefined ? { retryAfter } : {});

describe("mfaReduce — happy path", () => {
  it("walks not-enrolled → enroll → activate → backup-codes → enabled", () => {
    let s: MfaPanelState = initialMfaState;
    expect(s.step).toEqual({ phase: "idle", known: "unknown" });

    s = mfaReduce(s, { type: "START_ENROLL" });
    expect(s.step.phase).toBe("enroll-password");

    s = mfaReduce(s, { type: "SUBMIT" });
    expect(s.busy).toBe(true);

    s = mfaReduce(s, { type: "ENROLL_OK", secret: "ABCD2345EFGH", otpauthUrl: "otpauth://totp/x" });
    expect(s.step).toEqual({ phase: "activate", secret: "ABCD2345EFGH", otpauthUrl: "otpauth://totp/x" });
    expect(s.busy).toBe(false);

    s = mfaReduce(s, { type: "SUBMIT" });
    s = mfaReduce(s, { type: "ACTIVATE_OK", codes: ["c1", "c2"] });
    expect(s.step).toEqual({ phase: "backup-codes", codes: ["c1", "c2"] });

    s = mfaReduce(s, { type: "CODES_SAVED" });
    expect(s.step.phase).toBe("enabled");
    expect(s.error).toBeNull();
  });

  it("disable flow: enabled → disable(from:enabled) → idle(known disabled)", () => {
    let s: MfaPanelState = { step: { phase: "enabled" }, error: null, busy: false };
    s = mfaReduce(s, { type: "START_DISABLE" });
    expect(s.step).toEqual({ phase: "disable", from: "enabled" });
    s = mfaReduce(s, { type: "SUBMIT" });
    s = mfaReduce(s, { type: "DISABLE_OK" });
    expect(s.step).toEqual({ phase: "idle", known: "disabled" });
  });

  it("START_DISABLE from idle (the 'đã bật từ trước?' link) records from:'idle' — unconfirmed", () => {
    const s = mfaReduce(
      { step: { phase: "idle", known: "unknown" }, error: null, busy: false },
      { type: "START_DISABLE" },
    );
    expect(s.step).toEqual({ phase: "disable", from: "idle" });
  });

  // MED-1 regression: CANCEL used to unconditionally jump disable → {phase:"enabled"}, which let
  // a user reach "ĐANG BẬT" (server-confirmed badge) just by clicking "Đã bật từ trước? Tắt MFA"
  // then Cancel — WITHOUT the server ever confirming MFA is on. It must return to the TRUE
  // origin instead; "enabled" is only reachable via a real activate or an enroll 409.
  it("CANCEL from disable returns to its true origin — never fakes 'enabled' without server confirmation", () => {
    // Origin was the confirmed-enabled state → Cancel legitimately returns there.
    const fromEnabledOrigin = mfaReduce(
      { step: { phase: "disable", from: "enabled" }, error: "x", busy: false },
      { type: "CANCEL" },
    );
    expect(fromEnabledOrigin.step).toEqual({ phase: "enabled" });
    expect(fromEnabledOrigin.error).toBeNull();

    // Origin was idle/unconfirmed (via the "already enabled? disable here" link, or after a
    // disable attempt that never proved the account was enabled) → Cancel must NOT claim
    // MFA is on.
    const fromIdleOrigin = mfaReduce(
      { step: { phase: "disable", from: "idle" }, error: "x", busy: false },
      { type: "CANCEL" },
    );
    expect(fromIdleOrigin.step).toEqual({ phase: "idle", known: "unknown" });
  });

  it("CANCEL from enroll-password returns to idle", () => {
    const fromEnroll = mfaReduce(
      { step: { phase: "enroll-password" }, error: null, busy: true },
      { type: "CANCEL" },
    );
    expect(fromEnroll.step).toEqual({ phase: "idle", known: "unknown" });
    expect(fromEnroll.busy).toBe(false);
  });
});

describe("mfaReduce — failures (§3b error map)", () => {
  it("wrong step-up password (401 INVALID_CREDENTIALS) at enroll → stays, surfaced inline", () => {
    const s = mfaReduce(
      { step: { phase: "enroll-password" }, error: null, busy: true },
      { type: "FAIL", error: err(401, "INVALID_CREDENTIALS") },
    );
    expect(s.step.phase).toBe("enroll-password");
    expect(s.error).toBe("Mật khẩu không đúng.");
    expect(s.busy).toBe(false);
  });

  it("password went stale at activate (401) → back to enroll-password (re-enroll)", () => {
    const s = mfaReduce(
      { step: { phase: "activate", secret: "S", otpauthUrl: "o" }, error: null, busy: true },
      { type: "FAIL", error: err(401, "INVALID_CREDENTIALS") },
    );
    expect(s.step.phase).toBe("enroll-password");
    expect(s.error).toContain("Mật khẩu");
  });

  it("409 MFA_ALREADY_ENABLED at enroll → jumps to enabled (state discovered)", () => {
    const s = mfaReduce(
      { step: { phase: "enroll-password" }, error: null, busy: true },
      { type: "FAIL", error: err(409, "MFA_ALREADY_ENABLED") },
    );
    expect(s.step.phase).toBe("enabled");
    expect(s.error).toContain("đã bật");
  });

  it("422 MFA_NOT_ENROLLED at disable → jumps to idle with known='disabled'", () => {
    const s = mfaReduce(
      { step: { phase: "disable", from: "enabled" }, error: null, busy: true },
      { type: "FAIL", error: err(422, "MFA_NOT_ENROLLED") },
    );
    expect(s.step).toEqual({ phase: "idle", known: "disabled" });
  });

  it("503 MFA_UNAVAILABLE → honest 'unavailable' phase, never a crash state", () => {
    const s = mfaReduce(initialMfaState, { type: "FAIL", error: err(503, "MFA_UNAVAILABLE") });
    expect(s.step.phase).toBe("unavailable");
    expect(s.error).toContain("liên hệ quản trị");
  });

  it("wrong TOTP (401 MFA_CODE_INVALID) at activate → stays in activate, secret retained", () => {
    const s = mfaReduce(
      { step: { phase: "activate", secret: "KEEP", otpauthUrl: "o" }, error: null, busy: true },
      { type: "FAIL", error: err(401, "MFA_CODE_INVALID") },
    );
    expect(s.step).toEqual({ phase: "activate", secret: "KEEP", otpauthUrl: "o" });
    expect(s.error).toContain("Mã xác thực không đúng");
  });

  it("429 MFA_TOO_MANY_ATTEMPTS carries the Retry-After into the message when known", () => {
    const withRa = mfaFailureOutcome(err(429, "MFA_TOO_MANY_ATTEMPTS", 900), { phase: "disable", from: "enabled" });
    expect(withRa.message).toContain("15 phút");
    const withoutRa = mfaFailureOutcome(err(429, "MFA_TOO_MANY_ATTEMPTS"), { phase: "disable", from: "enabled" });
    expect(withoutRa.message).toContain("thử lại sau");
  });

  it("expired access token (401 UNAUTHORIZED — req() never refreshes /auth/*) → session message, not a password blame", () => {
    const out = mfaFailureOutcome(err(401, "UNAUTHORIZED"), { phase: "enroll-password" });
    expect(out.message).toContain("Phiên đăng nhập");
    expect(out.jumpTo).toBeUndefined();
  });

  it("non-ApiError → generic message, state unchanged", () => {
    const s = mfaReduce(
      { step: { phase: "enroll-password" }, error: null, busy: true },
      { type: "FAIL", error: new TypeError("network down") },
    );
    expect(s.step.phase).toBe("enroll-password");
    expect(s.error).toContain("Không thể xử lý");
  });
});

describe("groupSecret", () => {
  it("groups base32 into 4-char clusters for display", () => {
    expect(groupSecret("ABCDEFGHIJKLMNOP")).toBe("ABCD EFGH IJKL MNOP");
    expect(groupSecret("ABCDE")).toBe("ABCD E");
    expect(groupSecret("")).toBe("");
  });
});

// FE-CATCHUP-1: /auth/me now carries mfaEnabled — SYNC_STATUS seeds the panel with the honest
// server-known state, but ONLY in resting phases; it must never yank an in-flight flow away.
describe("mfaReduce — SYNC_STATUS (honest init from /auth/me mfaEnabled)", () => {
  it("enabled:true from idle/unknown → 'enabled' (badge is server-truth, not an error probe)", () => {
    const s = mfaReduce(initialMfaState, { type: "SYNC_STATUS", enabled: true });
    expect(s.step).toEqual({ phase: "enabled" });
    expect(s.busy).toBe(false);
  });

  it("enabled:false from idle/unknown → idle with known:'disabled' (server-confirmed off)", () => {
    const s = mfaReduce(initialMfaState, { type: "SYNC_STATUS", enabled: false });
    expect(s.step).toEqual({ phase: "idle", known: "disabled" });
  });

  it("enabled:false while showing 'enabled' (disabled from another session) → corrected to idle/disabled", () => {
    const s = mfaReduce(
      { step: { phase: "enabled" }, error: null, busy: false },
      { type: "SYNC_STATUS", enabled: false },
    );
    expect(s.step).toEqual({ phase: "idle", known: "disabled" });
  });

  it("never stomps an in-flight flow: enroll-password / activate / backup-codes / disable are untouched", () => {
    const flows: MfaPanelState[] = [
      { step: { phase: "enroll-password" }, error: null, busy: false },
      { step: { phase: "activate", secret: "S", otpauthUrl: "o" }, error: null, busy: false },
      { step: { phase: "backup-codes", codes: ["c1"] }, error: null, busy: false },
      { step: { phase: "disable", from: "enabled" }, error: null, busy: false },
      { step: { phase: "unavailable" }, error: null, busy: false },
    ];
    for (const before of flows) {
      expect(mfaReduce(before, { type: "SYNC_STATUS", enabled: true }).step).toEqual(before.step);
      expect(mfaReduce(before, { type: "SYNC_STATUS", enabled: false }).step).toEqual(before.step);
    }
  });

  it("skipped entirely while a request is in flight (busy)", () => {
    const busyIdle: MfaPanelState = { step: { phase: "idle", known: "unknown" }, error: null, busy: true };
    expect(mfaReduce(busyIdle, { type: "SYNC_STATUS", enabled: true })).toBe(busyIdle);
  });

  it("idempotent on a matching resting state (no object churn)", () => {
    const enabled: MfaPanelState = { step: { phase: "enabled" }, error: null, busy: false };
    expect(mfaReduce(enabled, { type: "SYNC_STATUS", enabled: true })).toBe(enabled);
    const off: MfaPanelState = { step: { phase: "idle", known: "disabled" }, error: null, busy: false };
    expect(mfaReduce(off, { type: "SYNC_STATUS", enabled: false })).toBe(off);
  });
});
