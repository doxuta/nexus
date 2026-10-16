// Component test — MfaCard wiring: the reducer (lib/mfaPanel, tested separately) driven
// through the real DOM with fetch mocked. Covers the enroll happy path up to backup codes,
// the wrong-step-up-password (401 INVALID_CREDENTIALS) inline error, and (FE-CATCHUP-1) the
// honest initialisation from /auth/me's mfaEnabled prop + the post-activate/disable refresh.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MfaCard } from "@/components/screens/MfaCard";
import { logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

describe("MfaCard", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api module token cache
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("starts honest: not-enrolled view with the unknown-state note and the enable button", () => {
    render(<MfaCard />);
    expect(screen.getByText("Xác thực hai lớp (MFA)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ })).toBeInTheDocument();
    // No fake status badge before the server has confirmed anything
    expect(screen.queryByText("ĐANG BẬT")).not.toBeInTheDocument();
    expect(screen.getByText(/chưa đọc được từ máy chủ/)).toBeInTheDocument();
  });

  it("wrong step-up password → 401 INVALID_CREDENTIALS surfaced inline, stays on password step", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Mật khẩu không đúng", code: "INVALID_CREDENTIALS" }, 401),
    ) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "wrong-pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));

    await waitFor(() => {
      expect(screen.getByText("Mật khẩu không đúng.")).toBeInTheDocument();
    });
    // Still on the password step — the form is NOT locked
    expect(screen.getByLabelText("Mật khẩu")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tiếp tục" })).not.toBeDisabled();
  });

  it("LOW-1: activate 401 INVALID_CREDENTIALS jumps back to enroll-password with the password field CLEARED", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/x" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        // The password went stale between enroll and activate (e.g. changed elsewhere).
        return jsonRes({ error: "Mật khẩu không đúng", code: "INVALID_CREDENTIALS" }, 401);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "stale-pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByLabelText("Mã 6 số từ ứng dụng")).toBeInTheDocument());

    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));

    // Jumped back to the password step — the STALE password must NOT be re-presented.
    await waitFor(() => {
      expect(screen.getByText(/Mật khẩu không còn đúng/)).toBeInTheDocument();
    });
    expect((screen.getByLabelText("Mật khẩu") as HTMLInputElement).value).toBe("");
  });

  it("enroll happy path: password → secret shown → code → backup codes ONCE → enabled", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    // Step 1: enroll returns the TOTP secret + provisioning URL
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/Nexus:a?secret=ABCDEFGHIJKLMNOP&issuer=Nexus" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        return jsonRes({ data: { backupCodes: Array.from({ length: 10 }, (_, i) => `bkp-${i}`) } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "correct-pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));

    // Secret is displayed grouped for manual entry (no QR dep — honest fallback)
    await waitFor(() => {
      expect(screen.getByDisplayValue("ABCD EFGH IJKL MNOP")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));

    // Backup codes shown once, with the save-confirm gate
    await waitFor(() => {
      expect(screen.getByText("bkp-0")).toBeInTheDocument();
      expect(screen.getByText("bkp-9")).toBeInTheDocument();
    });
    expect(screen.getByText(/một lần duy nhất/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Tôi đã lưu mã dự phòng/ }));

    // Enabled state: badge + disable entry; codes are gone
    expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tắt MFA/ })).toBeInTheDocument();
    expect(screen.queryByText("bkp-0")).not.toBeInTheDocument();
  });

  it("SLDS modal: closing the dialog on the backup-codes step never falls back to a false 'not enabled'", async () => {
    render(<MfaCard mfaEnabled={false} onMfaChanged={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));
    // The step-up flow runs in a dialog, not inline in the card.
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/x" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        return jsonRes({ data: { backupCodes: ["b-1", "b-2"] } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByLabelText("Mã 6 số từ ứng dụng")).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));
    await waitFor(() => expect(screen.getByText("b-1")).toBeInTheDocument());

    // MFA is already ON server-side here — the dialog's close button must land on "enabled".
    fireEvent.click(screen.getByRole("button", { name: "Huỷ và đóng" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Bật xác thực 2 lớp/ })).not.toBeInTheDocument();
  });

  it("mfaEnabled={false}: cancelling the enroll dialog returns to server-known OFF, not 'unknown'", () => {
    render(<MfaCard mfaEnabled={false} onMfaChanged={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByText(/chưa đọc được từ máy chủ/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Đã bật từ trước/ })).not.toBeInTheDocument();
  });

  it("503 MFA_UNAVAILABLE at enroll → honest disabled note, no crash", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Xác thực hai lớp (MFA) chưa được cấu hình trên hệ thống", code: "MFA_UNAVAILABLE" }, 503),
    ) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));

    await waitFor(() => {
      expect(screen.getByText(/chưa cấu hình khóa MFA/)).toBeInTheDocument();
    });
    expect(screen.getByText(/MFA_SECRET_ENC_KEY/)).toBeInTheDocument();
  });

  it("enroll on an already-enabled account (409) → jumps straight to the enabled view", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Đã bật", code: "MFA_ALREADY_ENABLED" }, 409),
    ) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));

    await waitFor(() => {
      expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument();
    });
    expect(screen.getByRole("button", { name: /Tắt MFA/ })).toBeInTheDocument();
  });

  it("MED-1: 'Đã bật từ trước? Tắt MFA' → Cancel must NOT show the fake 'ĐANG BẬT' badge", async () => {
    render(<MfaCard />);
    // Enters the disable form WITHOUT the server ever confirming MFA is enabled.
    fireEvent.click(screen.getByRole("button", { name: /Đã bật từ trước\? Tắt MFA/ }));
    expect(screen.getByRole("button", { name: /Tắt MFA/ })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));

    // Back to the honest not-enrolled/unknown view — never claims MFA is on unconfirmed.
    expect(screen.queryByText("ĐANG BẬT")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ })).toBeInTheDocument();
  });

  it("MED-1: after a real 409-confirmed 'enabled', START_DISABLE → Cancel correctly returns to enabled", async () => {
    render(<MfaCard />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Đã bật", code: "MFA_ALREADY_ENABLED" }, 409),
    ) as unknown as typeof fetch;
    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: /Tắt MFA/ }));
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));

    // This time "enabled" is legitimate — the server confirmed it via the 409 above.
    expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument();
  });

  // ── FE-CATCHUP-1: honest initialisation from /auth/me's mfaEnabled ──────────────────

  it("mfaEnabled={true} → opens in the ENABLED state with NO error probe (no /auth/mfa call)", () => {
    const fetchMock = vi.fn(async () => jsonRes({}, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<MfaCard mfaEnabled={true} onMfaChanged={() => {}} />);

    // The badge + disable entry render straight from the flag — server truth, not a 409 probe.
    expect(screen.getByText("ĐANG BẬT")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Tắt MFA/ })).toBeInTheDocument();
    // No unknown-state disclaimer and no enable CTA.
    expect(screen.queryByText(/chưa đọc được từ máy chủ/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Bật xác thực 2 lớp/ })).not.toBeInTheDocument();
    // Zero network activity — the state came from the prop, not an action-error probe.
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("mfaEnabled={false} → server-confirmed OFF: no unknown note, no 'đã bật từ trước?' escape hatch", () => {
    render(<MfaCard mfaEnabled={false} onMfaChanged={() => {}} />);

    expect(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ })).toBeInTheDocument();
    expect(screen.queryByText("ĐANG BẬT")).not.toBeInTheDocument();
    // known:"disabled" — the unknown-state disclaimer AND the "already enabled?" link both go.
    expect(screen.queryByText(/chưa đọc được từ máy chủ/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Đã bật từ trước/ })).not.toBeInTheDocument();
  });

  it("after a successful activate → onMfaChanged fires so the caller refreshes /auth/me", async () => {
    const onMfaChanged = vi.fn();
    render(<MfaCard mfaEnabled={false} onMfaChanged={onMfaChanged} />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/x" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        return jsonRes({ data: { backupCodes: ["b-1", "b-2"] } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByLabelText("Mã 6 số từ ứng dụng")).toBeInTheDocument());
    expect(onMfaChanged).not.toHaveBeenCalled(); // enroll alone changes nothing server-side

    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));

    await waitFor(() => expect(screen.getByText("b-1")).toBeInTheDocument());
    // MFA just turned ON (server-confirmed) → the cached mfaEnabled must be refreshed NOW,
    // before the user even confirms the backup codes.
    expect(onMfaChanged).toHaveBeenCalledTimes(1);
  });

  it("after a successful disable → onMfaChanged fires and the panel returns to server-known OFF", async () => {
    const onMfaChanged = vi.fn();
    render(<MfaCard mfaEnabled={true} onMfaChanged={onMfaChanged} />);
    fireEvent.click(screen.getByRole("button", { name: /Tắt MFA/ }));

    global.fetch = vi.fn(async (url: RequestInfo | URL) =>
      String(url).includes("/auth/mfa/disable")
        ? jsonRes({ data: { ok: true } }, 200)
        : jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500),
    ) as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.change(screen.getByLabelText("Mã xác thực / mã dự phòng"), { target: { value: "654321" } });
    fireEvent.click(screen.getByRole("button", { name: "Tắt MFA" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ })).toBeInTheDocument();
    });
    expect(onMfaChanged).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("ĐANG BẬT")).not.toBeInTheDocument();
  });

  it("no onMfaChanged prop → falls back to a direct GET /auth/me after activate (cache refresh)", async () => {
    render(<MfaCard mfaEnabled={false} />);
    fireEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));

    const fetchMock = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/x" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        return jsonRes({ data: { backupCodes: ["b-1"] } }, 200);
      }
      if (u.includes("/auth/me")) {
        return jsonRes({ data: { id: "u1", email: "a@example.com", fullName: "A", isAdmin: false, mfaEnabled: true } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByLabelText("Mã 6 số từ ứng dụng")).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));

    await waitFor(() => expect(screen.getByText("b-1")).toBeInTheDocument());
    await waitFor(() => {
      const meCalls = fetchMock.mock.calls.filter((c) => String(c[0]).includes("/auth/me"));
      expect(meCalls.length).toBe(1);
    });
    // getMe() re-cached the fresh flag — nexus_user is not stale.
    expect(JSON.parse(window.localStorage.getItem("nexus_user") ?? "{}").mfaEnabled).toBe(true);
  });

  // ── SLDS modals, Keyboard Interaction: closing moves focus back to what opened the dialog;
  //    when the flow removed that button, focus goes to the card's new primary action. ──────
  function mockEnrollActivate() {
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/auth/mfa/enroll")) {
        return jsonRes({ data: { secret: "ABCDEFGHIJKLMNOP", otpauthUrl: "otpauth://totp/x" } }, 200);
      }
      if (u.includes("/auth/mfa/activate")) {
        return jsonRes({ data: { backupCodes: ["b-1", "b-2"] } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
  }

  it("Escape on the enroll dialog returns focus to 'Bật xác thực 2 lớp'", async () => {
    render(<MfaCard mfaEnabled={false} onMfaChanged={() => {}} />);
    const opener = screen.getByRole("button", { name: /Bật xác thực 2 lớp/ });
    await userEvent.click(opener);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(opener).toHaveFocus());
  });

  it("after 'Tôi đã lưu mã dự phòng' focus lands on the card's new action 'Tắt MFA', not <body>", async () => {
    render(<MfaCard mfaEnabled={false} onMfaChanged={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));
    mockEnrollActivate();
    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByLabelText("Mã 6 số từ ứng dụng")).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText("Mã 6 số từ ứng dụng"), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Kích hoạt" }));
    await waitFor(() => expect(screen.getByText("b-1")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /Tôi đã lưu mã dự phòng/ }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: /Tắt MFA/ })).toHaveFocus());
  });

  it("503 MFA_UNAVAILABLE closes the dialog with focus on the card's note, not <body>", async () => {
    render(<MfaCard />);
    await userEvent.click(screen.getByRole("button", { name: /Bật xác thực 2 lớp/ }));
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Xác thực hai lớp (MFA) chưa được cấu hình trên hệ thống", code: "MFA_UNAVAILABLE" }, 503),
    ) as unknown as typeof fetch;
    fireEvent.change(screen.getByLabelText("Mật khẩu"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "Tiếp tục" }));
    await waitFor(() => expect(screen.getByText(/chưa cấu hình khóa MFA/)).toBeInTheDocument());
    const card = screen.getByText("Xác thực hai lớp (MFA)").closest("article")!;
    await waitFor(() => expect(card.contains(document.activeElement)).toBe(true));
    expect(document.activeElement).toContainElement(screen.getByText(/chưa cấu hình khóa MFA/));
  });

  it("'ĐANG BẬT' is the blueprint's success badge: text only, no icon", () => {
    render(<MfaCard mfaEnabled={true} onMfaChanged={() => {}} />);
    const badge = screen.getByText("ĐANG BẬT").closest(".slds-badge")!;
    expect(badge).toHaveClass("slds-theme_success");
    expect(badge.querySelector(".slds-badge__icon, svg")).toBeNull();
  });
});
