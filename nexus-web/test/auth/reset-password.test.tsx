// /reset-password — SLDS migration (specs/038 T033). Pins the no-token and rejected-token screens,
// the client-side 8-char / mismatch checks (each error on the field that is wrong), the 422
// backstop, 429, network retry and the done screen.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ resetPassword: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

const nav = vi.hoisted(() => ({ params: new URLSearchParams("token=tok-1") }));
vi.mock("next/navigation", () => ({ useSearchParams: () => nav.params }));

import { ApiError } from "@/lib/api";
import ResetPasswordPage from "@/app/(auth)/reset-password/page";

beforeEach(() => {
  api.resetPassword.mockReset();
  nav.params = new URLSearchParams("token=tok-1");
});

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

function fill(pwd: string, confirm = pwd) {
  fireEvent.change(field("Mật khẩu mới"), { target: { value: pwd } });
  fireEvent.change(field("Xác nhận mật khẩu mới"), { target: { value: confirm } });
}

async function submit() {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Đặt lại mật khẩu" }));
  });
}

function region(theme: "error" | "success") {
  const el = document.querySelector(`.slds-scoped-notification.slds-theme_${theme}`);
  expect(el).not.toBeNull();
  return el as HTMLElement;
}

/** The SLDS form element (label + control + help line) wrapping a field. */
const frame = (label: string) => field(label).closest(".slds-form-element") as HTMLElement;

/** SLDS error state on exactly this field: frame class, message wired as its description, aria-invalid. */
function expectFieldError(label: string, message: string) {
  expect(frame(label)).toHaveClass("slds-has-error");
  expect(within(frame(label)).getByText(message)).toBeInTheDocument();
  expect(field(label)).toHaveAttribute("aria-invalid", "true");
  expect(field(label)).toHaveAccessibleDescription(message);
}

function expectFieldClean(label: string) {
  expect(frame(label)).not.toHaveClass("slds-has-error");
  expect(field(label)).not.toHaveAttribute("aria-invalid");
}

describe("/reset-password — link states", () => {
  it("has no form when the link carries no token", () => {
    nav.params = new URLSearchParams();
    render(<ResetPasswordPage />);
    expect(screen.getByRole("heading", { level: 1, name: "Liên kết không hợp lệ" })).toBeInTheDocument();
    expect(within(region("error")).getByText(/thiếu mã xác thực/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Yêu cầu liên kết mới" })).toHaveAttribute("href", "/forgot-password");
    expect(screen.getByRole("link", { name: "Quay lại đăng nhập" })).toHaveAttribute("href", "/login");
    expect(screen.queryByLabelText("Mật khẩu mới")).toBeNull();
  });

  it("shows the expired/used screen when the server rejects the token", async () => {
    api.resetPassword.mockRejectedValue(new ApiError(400, "RESET_TOKEN_INVALID", ""));
    render(<ResetPasswordPage />);
    fill("Abcdef@123");
    await submit();
    expect(api.resetPassword).toHaveBeenCalledWith("tok-1", "Abcdef@123");
    expect(
      screen.getByRole("heading", { level: 1, name: "Liên kết đã hết hạn hoặc không hợp lệ" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Yêu cầu liên kết mới" })).toHaveClass("slds-button_brand");
  });
});

describe("/reset-password — form", () => {
  it("renders SLDS password fields with the length hint", () => {
    render(<ResetPasswordPage />);
    expect(screen.getByRole("heading", { level: 1, name: "Đặt lại mật khẩu" })).toBeInTheDocument();
    expect(field("Mật khẩu mới")).toHaveClass("slds-input");
    expect(field("Mật khẩu mới")).toHaveAttribute("type", "password");
    expect(within(frame("Mật khẩu mới")).getByText("Tối thiểu 8 ký tự")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Đặt lại mật khẩu" })).toHaveClass("slds-button_brand");
  });

  it("rejects a short password on the new-password field itself, without calling the API", async () => {
    render(<ResetPasswordPage />);
    fill("abc", "abcdefghij");
    await submit();
    // The error replaces the length hint on the field that is wrong (SLDS form-element error state).
    expectFieldError("Mật khẩu mới", "Mật khẩu mới phải có ít nhất 8 ký tự.");
    expect(within(frame("Mật khẩu mới")).queryByText("Tối thiểu 8 ký tự")).toBeNull();
    expectFieldClean("Xác nhận mật khẩu mới");
    expect(api.resetPassword).not.toHaveBeenCalled();
  });

  it("rejects a mismatched confirmation on the confirm field", async () => {
    render(<ResetPasswordPage />);
    fill("Abcdef@123", "Abcdef@124");
    await submit();
    expectFieldError("Xác nhận mật khẩu mới", "Mật khẩu mới và xác nhận không khớp.");
    expectFieldClean("Mật khẩu mới");
    expect(api.resetPassword).not.toHaveBeenCalled();
  });

  it("clears a field error once the user edits the field", async () => {
    render(<ResetPasswordPage />);
    fill("abc");
    await submit();
    fireEvent.change(field("Mật khẩu mới"), { target: { value: "abcd" } });
    expectFieldClean("Mật khẩu mới");
    expect(within(frame("Mật khẩu mới")).getByText("Tối thiểu 8 ký tự")).toBeInTheDocument();
  });

  it("shows both passwords on request", () => {
    render(<ResetPasswordPage />);
    fireEvent.click(screen.getByRole("button", { name: "Hiện mật khẩu" }));
    expect(field("Mật khẩu mới")).toHaveAttribute("type", "text");
    expect(field("Xác nhận mật khẩu mới")).toHaveAttribute("type", "text");
    fireEvent.click(screen.getByRole("button", { name: "Ẩn mật khẩu" }));
    expect(field("Mật khẩu mới")).toHaveAttribute("type", "password");
  });

  it("puts the server's weak-password message (422) on the new-password field", async () => {
    api.resetPassword.mockRejectedValue(new ApiError(422, "PASSWORD_TOO_WEAK", "Mật khẩu quá yếu"));
    render(<ResetPasswordPage />);
    fill("Abcdef@123");
    await submit();
    expectFieldError("Mật khẩu mới", "Mật khẩu quá yếu");
    expectFieldClean("Xác nhận mật khẩu mới");
  });

  it("locks the button with a countdown on 429", async () => {
    api.resetPassword.mockRejectedValue(new ApiError(429, "RATE_LIMITED", "", { retryAfter: 90 }));
    render(<ResetPasswordPage />);
    fill("Abcdef@123");
    await submit();
    expect(within(region("error")).getByText("Thử quá nhiều lần — đợi 90s.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Thử lại sau 01:30" })).toBeDisabled();
  });

  it("offers a retry on a network failure, then shows the done screen", async () => {
    api.resetPassword.mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValueOnce(undefined);
    render(<ResetPasswordPage />);
    fill("Abcdef@123");
    await submit();
    expect(
      within(region("error")).getByText("Không thể kết nối máy chủ. Kiểm tra mạng và thử lại."),
    ).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    });
    expect(api.resetPassword).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("heading", { level: 1, name: "Đã đặt lại mật khẩu" })).toBeInTheDocument();
    expect(within(region("success")).getByText(/Mọi phiên đăng nhập cũ đã bị đăng xuất/)).toBeInTheDocument();
    const login = screen.getByRole("link", { name: "Đăng nhập" });
    expect(login).toHaveAttribute("href", "/login");
    expect(login).toHaveClass("slds-button_brand");
  });
});
