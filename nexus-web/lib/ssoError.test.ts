// Tests for the #48 SSO-FE-REDIRECT error-code → Vietnamese message map.
import { describe, it, expect } from "vitest";
import { ssoErrorMessage, SSO_ERROR_FALLBACK } from "@/lib/ssoError";

describe("ssoErrorMessage", () => {
  it("maps every coarse callback code the BE can 302 back with", () => {
    const codes = [
      "SSO_STATE_INVALID",
      "SSO_STATE_MISMATCH",
      "SSO_EXCHANGE_FAILED",
      "SSO_ID_TOKEN_INVALID",
      "SSO_LOGIN_NOT_AUTHORIZED",
      "SSO_PROVIDER_NOT_FOUND",
      "SSO_PROVIDER_DISCOVERY_FAILED",
      "MFA_UNAVAILABLE",
      "SSO_NOT_CONFIGURED",
      "SSO_HANDOFF_INVALID",
    ];
    for (const code of codes) {
      const msg = ssoErrorMessage(code);
      expect(msg, code).toBeTruthy();
      // Every mapped code gets a SPECIFIC message, never the generic fallback.
      expect(msg, code).not.toBe(SSO_ERROR_FALLBACK);
    }
  });

  it("unknown code → generic fallback (the BE code set may grow)", () => {
    expect(ssoErrorMessage("SSO_SOMETHING_NEW")).toBe(SSO_ERROR_FALLBACK);
  });

  it("absent/empty code → null (no banner)", () => {
    expect(ssoErrorMessage(null)).toBeNull();
    expect(ssoErrorMessage(undefined)).toBeNull();
    expect(ssoErrorMessage("")).toBeNull();
  });
});
