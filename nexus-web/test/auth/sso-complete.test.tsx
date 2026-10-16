// /auth/sso/complete — SLDS migration (specs/038 T033). Pins the single-use handoff contract
// (exactly one exchange even under StrictMode, ?code stripped from the URL), the ok → /home,
// mfa → step 2 and error → card paths, plus the SLDS markup.
import { StrictMode } from "react";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ ssoExchange: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

const nav = vi.hoisted(() => ({ params: new URLSearchParams("code=hand-1"), replace: vi.fn() }));
vi.mock("next/navigation", () => ({
  useSearchParams: () => nav.params,
  useRouter: () => ({ replace: nav.replace }),
}));

const auth = vi.hoisted(() => ({ completeMfa: vi.fn(), refetchMe: vi.fn() }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

import { ApiError } from "@/lib/api";
import SsoCompletePage from "@/app/(auth)/auth/sso/complete/page";

let replaceState: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  api.ssoExchange.mockReset();
  nav.params = new URLSearchParams("code=hand-1");
  nav.replace.mockReset();
  auth.completeMfa.mockReset();
  auth.refetchMe.mockReset().mockResolvedValue(undefined);
  replaceState = vi.spyOn(window.history, "replaceState");
  replaceState.mockClear();
});

function errorRegion() {
  const el = document.querySelector(".slds-scoped-notification.slds-theme_error");
  expect(el).not.toBeNull();
  return el as HTMLElement;
}

describe("/auth/sso/complete", () => {
  it("shows the SLDS spinner while exchanging, then lands on /home — exchanging the code once", async () => {
    let resolve!: (v: unknown) => void;
    api.ssoExchange.mockReturnValue(new Promise((r) => (resolve = r)));
    render(
      <StrictMode>
        <SsoCompletePage />
      </StrictMode>,
    );
    expect(screen.getByText("Đang hoàn tất đăng nhập SSO…")).toBeInTheDocument();
    expect(document.querySelector(".slds-card .slds-spinner")).not.toBeNull();
    expect(replaceState).toHaveBeenCalledWith({}, "", "/auth/sso/complete");
    await act(async () => resolve({ status: "ok", user: {} }));
    expect(api.ssoExchange).toHaveBeenCalledTimes(1);
    expect(api.ssoExchange).toHaveBeenCalledWith("hand-1");
    expect(auth.refetchMe).toHaveBeenCalled();
    expect(nav.replace).toHaveBeenCalledWith("/home");
  });

  it("explains a link without a code, without calling the API", () => {
    nav.params = new URLSearchParams();
    render(<SsoCompletePage />);
    expect(screen.getByRole("heading", { level: 1, name: "Không hoàn tất được đăng nhập SSO" })).toBeInTheDocument();
    expect(within(errorRegion()).getByText("Đường dẫn thiếu mã hoàn tất SSO. Vui lòng đăng nhập lại.")).toBeInTheDocument();
    const back = screen.getByRole("link", { name: "Về trang đăng nhập" });
    expect(back).toHaveAttribute("href", "/login");
    expect(back).toHaveClass("slds-button_brand");
    expect(api.ssoExchange).not.toHaveBeenCalled();
  });

  it("maps a burnt/expired handoff code to Vietnamese", async () => {
    api.ssoExchange.mockRejectedValue(new ApiError(401, "SSO_HANDOFF_INVALID", "handoff invalid"));
    render(<SsoCompletePage />);
    await waitFor(() =>
      expect(within(errorRegion()).getByText("Phiên đăng nhập SSO đã hết hạn hoặc đã được sử dụng — đăng nhập lại.")).toBeInTheDocument(),
    );
  });

  it("says so when the server cannot be reached", async () => {
    api.ssoExchange.mockRejectedValue(new TypeError("Failed to fetch"));
    render(<SsoCompletePage />);
    await waitFor(() =>
      expect(within(errorRegion()).getByText("Không thể kết nối máy chủ. Kiểm tra mạng và thử đăng nhập lại.")).toBeInTheDocument(),
    );
  });

  describe("MFA step", () => {
    async function toMfa() {
      api.ssoExchange.mockResolvedValue({ status: "mfa", pendingToken: "pend-9" });
      render(<SsoCompletePage />);
      await screen.findByRole("heading", { level: 1, name: "Xác thực hai lớp" });
    }
    const code = () => screen.getByLabelText("Mã xác thực") as HTMLInputElement;
    const confirm = () =>
      act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Xác nhận" }));
      });

    it("asks for the code, then completes to /home", async () => {
      await toMfa();
      expect(code()).toHaveClass("slds-input");
      await confirm();
      expect(within(errorRegion()).getByText("Nhập mã 6 số từ ứng dụng xác thực (hoặc mã dự phòng).")).toBeInTheDocument();
      auth.completeMfa.mockResolvedValue({});
      fireEvent.change(code(), { target: { value: " 654321 " } });
      await confirm();
      expect(auth.completeMfa).toHaveBeenCalledWith("pend-9", "654321");
      expect(nav.replace).toHaveBeenCalledWith("/home");
    });

    it("keeps the step on a wrong code", async () => {
      await toMfa();
      auth.completeMfa.mockRejectedValue(new ApiError(401, "MFA_CODE_INVALID", ""));
      fireEvent.change(code(), { target: { value: "000000" } });
      await confirm();
      expect(within(errorRegion()).getByText("Mã xác thực không đúng hoặc đã được sử dụng. Thử lại.")).toBeInTheDocument();
      expect(code()).toBeInTheDocument();
    });

    it("falls to the error card when the pending session expired", async () => {
      await toMfa();
      auth.completeMfa.mockRejectedValue(new ApiError(401, "MFA_PENDING_INVALID", ""));
      fireEvent.change(code(), { target: { value: "000000" } });
      await confirm();
      expect(screen.getByRole("heading", { level: 1, name: "Không hoàn tất được đăng nhập SSO" })).toBeInTheDocument();
      expect(within(errorRegion()).getByText("Phiên xác thực đã hết hạn — vui lòng đăng nhập lại.")).toBeInTheDocument();
    });

    it("locks the button with a countdown on 429", async () => {
      await toMfa();
      auth.completeMfa.mockRejectedValue(new ApiError(429, "RATE_LIMITED", "", { retryAfter: 30 }));
      fireEvent.change(code(), { target: { value: "000000" } });
      await confirm();
      expect(screen.getByRole("button", { name: "Thử lại sau 00:30" })).toBeDisabled();
    });
  });
});
