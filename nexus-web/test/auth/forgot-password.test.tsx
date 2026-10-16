// /forgot-password — SLDS migration (specs/038 T033). Pins the anti-enumeration contract (every
// well-formed submit shows the SAME neutral success), the 429 countdown and the network retry,
// plus the SLDS markup the page now renders.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ forgotPassword: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { ApiError } from "@/lib/api";
import ForgotPasswordPage from "@/app/(auth)/forgot-password/page";

beforeEach(() => {
  api.forgotPassword.mockReset();
});

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

function fill(org = "northwind", email = "ten@example.com") {
  fireEvent.change(field("Mã tổ chức"), { target: { value: org } });
  fireEvent.change(field("Email"), { target: { value: email } });
}

async function click(name: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

function region(theme: "error" | "success") {
  const el = document.querySelector(`.slds-scoped-notification.slds-theme_${theme}`);
  expect(el).not.toBeNull();
  return el as HTMLElement;
}

describe("/forgot-password", () => {
  it("renders the SLDS form in a card", () => {
    render(<ForgotPasswordPage />);
    expect(screen.getByRole("heading", { level: 1, name: "Quên mật khẩu" })).toBeInTheDocument();
    expect(field("Mã tổ chức")).toHaveClass("slds-input");
    expect(field("Email")).toHaveClass("slds-input");
    expect(screen.getByRole("button", { name: "Gửi liên kết đặt lại" })).toHaveClass("slds-button_brand");
    expect(screen.getByRole("link", { name: "Quay lại đăng nhập" })).toHaveAttribute("href", "/login");
    expect(document.querySelector(".slds-card")).not.toBeNull();
  });

  it("refuses an incomplete form without calling the API", async () => {
    render(<ForgotPasswordPage />);
    await click("Gửi liên kết đặt lại");
    expect(within(region("error")).getByText("Nhập đủ mã tổ chức và email.")).toBeInTheDocument();
    expect(api.forgotPassword).not.toHaveBeenCalled();
  });

  it("shows the same neutral success whatever the account (anti-enumeration)", async () => {
    api.forgotPassword.mockResolvedValue(undefined);
    render(<ForgotPasswordPage />);
    fill(" northwind ", " ten@example.com ");
    await click("Gửi liên kết đặt lại");
    expect(api.forgotPassword).toHaveBeenCalledWith("northwind", "ten@example.com");
    expect(screen.getByRole("heading", { level: 1, name: "Đã gửi yêu cầu" })).toBeInTheDocument();
    expect(
      within(region("success")).getByText("Nếu email tồn tại trong hệ thống, liên kết đặt lại đã được gửi."),
    ).toBeInTheDocument();
    const back = screen.getByRole("link", { name: "Quay lại đăng nhập" });
    expect(back).toHaveAttribute("href", "/login");
    expect(back).toHaveClass("slds-button_brand");
  });

  it("locks the button with a countdown on 429", async () => {
    api.forgotPassword.mockRejectedValue(new ApiError(429, "RATE_LIMITED", "", { retryAfter: 60 }));
    render(<ForgotPasswordPage />);
    fill();
    await click("Gửi liên kết đặt lại");
    expect(within(region("error")).getByText("Thử quá nhiều lần — đợi 60s.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Thử lại sau 01:00" })).toBeDisabled();
  });

  it("shows the server message for other API errors", async () => {
    api.forgotPassword.mockRejectedValue(new ApiError(500, "INTERNAL", "Máy chủ lỗi"));
    render(<ForgotPasswordPage />);
    fill();
    await click("Gửi liên kết đặt lại");
    expect(within(region("error")).getByText("Máy chủ lỗi")).toBeInTheDocument();
  });

  it("offers a retry on a network failure", async () => {
    api.forgotPassword.mockRejectedValueOnce(new TypeError("Failed to fetch")).mockResolvedValueOnce(undefined);
    render(<ForgotPasswordPage />);
    fill();
    await click("Gửi liên kết đặt lại");
    expect(
      within(region("error")).getByText("Không thể kết nối máy chủ. Kiểm tra mạng và thử lại."),
    ).toBeInTheDocument();
    await click("Thử lại");
    expect(api.forgotPassword).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("heading", { level: 1, name: "Đã gửi yêu cầu" })).toBeInTheDocument();
  });
});
