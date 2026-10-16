// Login — SLDS migration (specs/038 T033). Pins every behaviour the screen had before the move
// (2-auth.md: anti-enumeration 401, 429 countdown, MFA step 2, ?next=, ?ssoError=, remember-me,
// tenant branding preview, SSO buttons/manual panel) plus the SLDS markup it now renders.
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const auth = vi.hoisted(() => ({
  login: vi.fn(),
  completeMfa: vi.fn(),
  user: null as null | { email: string },
  isLoading: false,
}));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => auth }));

const nav = vi.hoisted(() => ({ params: new URLSearchParams() }));
vi.mock("next/navigation", () => ({ useSearchParams: () => nav.params }));

const brand = vi.hoisted(() => ({ doc: {} as { appName?: string; logoUrl?: string } }));
vi.mock("@/components/brand/BrandingBoot", () => ({ useBranding: () => brand.doc }));

const api = vi.hoisted(() => ({ getPublicBranding: vi.fn(), ssoPublicProviders: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

const branding = vi.hoisted(() => ({ applyBranding: vi.fn() }));
vi.mock("@/lib/branding", async (orig) => ({ ...(await orig<typeof import("@/lib/branding")>()), ...branding }));

import { ApiError, ssoStartUrl } from "@/lib/api";
import { Login } from "./Login";

const loc = { href: "", assign: vi.fn() };
const realLocation = window.location;

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  auth.login.mockReset();
  auth.completeMfa.mockReset();
  auth.user = null;
  auth.isLoading = false;
  nav.params = new URLSearchParams();
  brand.doc = {};
  api.getPublicBranding.mockReset().mockResolvedValue({});
  api.ssoPublicProviders.mockReset().mockResolvedValue([]);
  branding.applyBranding.mockReset();
  loc.href = "";
  loc.assign.mockReset();
  Object.defineProperty(window, "location", { configurable: true, value: loc });
});

afterEach(() => {
  vi.useRealTimers();
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

function fill(org = "northwind", email = "test@example.com", pwd = "Secret@1") {
  fireEvent.change(field("Mã tổ chức"), { target: { value: org } });
  fireEvent.change(field("Email"), { target: { value: email } });
  fireEvent.change(field("Mật khẩu"), { target: { value: pwd } });
}

async function submit(name = "Đăng nhập") {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
}

/** The form-level error region (SLDS scoped notification, error theme). */
function errorRegion() {
  const el = document.querySelector(".slds-scoped-notification.slds-theme_error");
  expect(el).not.toBeNull();
  return el as HTMLElement;
}

describe("Login — SLDS markup", () => {
  it("renders SLDS inputs, a brand submit button, the remember-me checkbox and the forgot link", () => {
    render(<Login />);
    for (const label of ["Mã tổ chức", "Email", "Mật khẩu"]) expect(field(label)).toHaveClass("slds-input");
    expect(screen.getByRole("heading", { level: 1, name: "Đăng nhập" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Đăng nhập" })).toHaveClass("slds-button", "slds-button_brand");
    expect(screen.getByLabelText("Ghi nhớ đăng nhập")).toHaveAttribute("type", "checkbox");
    expect(screen.getByRole("link", { name: "Quên mật khẩu?" })).toHaveAttribute("href", "/forgot-password");
    expect(document.querySelector(".slds-card")).not.toBeNull();
  });

  it("toggles the password between hidden and shown", () => {
    render(<Login />);
    expect(field("Mật khẩu")).toHaveAttribute("type", "password");
    fireEvent.click(screen.getByRole("button", { name: "Hiện mật khẩu" }));
    expect(field("Mật khẩu")).toHaveAttribute("type", "text");
    fireEvent.click(screen.getByRole("button", { name: "Ẩn mật khẩu" }));
    expect(field("Mật khẩu")).toHaveAttribute("type", "password");
  });

  it("shows the tenant's own logo and app name when branded", () => {
    brand.doc = { appName: "ACME CRM", logoUrl: "data:image/png;base64,AAAA" };
    render(<Login />);
    expect(screen.getByText("ACME CRM")).toBeInTheDocument();
    expect(document.querySelector('img[src^="data:image/png"]')).not.toBeNull();
  });
});

describe("Login — step 1", () => {
  it("refuses an incomplete form without calling the API", async () => {
    render(<Login />);
    await submit();
    expect(within(errorRegion()).getByText("Nhập đủ mã tổ chức, email và mật khẩu.")).toBeInTheDocument();
    expect(auth.login).not.toHaveBeenCalled();
  });

  it("logs in with remember-me and follows ?next=", async () => {
    nav.params = new URLSearchParams("next=/crm_accounts");
    auth.login.mockResolvedValue({ status: "ok", user: {} });
    render(<Login />);
    fill(" northwind ", " test@example.com ");
    fireEvent.click(screen.getByLabelText("Ghi nhớ đăng nhập"));
    await submit();
    expect(auth.login).toHaveBeenCalledWith("northwind", "test@example.com", "Secret@1", true);
    expect(loc.href).toBe("/crm_accounts");
  });

  it("defaults to /home after login", async () => {
    auth.login.mockResolvedValue({ status: "ok", user: {} });
    render(<Login />);
    fill();
    await submit();
    expect(auth.login).toHaveBeenCalledWith("northwind", "test@example.com", "Secret@1", false);
    expect(loc.href).toBe("/home");
  });

  it("redirects straight away when a session already exists", () => {
    auth.user = { email: "test@example.com" };
    nav.params = new URLSearchParams("next=/reports");
    render(<Login />);
    expect(loc.href).toBe("/reports");
  });

  it("answers a wrong credential with the one generic message (anti-enumeration)", async () => {
    auth.login.mockRejectedValue(new ApiError(401, "INVALID_CREDENTIALS", "Tài khoản không tồn tại"));
    render(<Login />);
    fill();
    await submit();
    expect(within(errorRegion()).getByText("Email hoặc mật khẩu không đúng.")).toBeInTheDocument();
    expect(screen.queryByText("Tài khoản không tồn tại")).toBeNull();
  });

  it("locks the button with a countdown on 429", async () => {
    auth.login.mockRejectedValue(new ApiError(429, "RATE_LIMITED", "", { retryAfter: 65 }));
    render(<Login />);
    fill();
    await submit();
    expect(within(errorRegion()).getByText("Thử quá nhiều lần — đợi 65s.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Thử lại sau 01:05" })).toBeDisabled();
  });

  it("reports MFA_UNAVAILABLE as a server fault, not a wrong password", async () => {
    auth.login.mockRejectedValue(new ApiError(503, "MFA_UNAVAILABLE", "Thiếu khóa MFA trên máy chủ"));
    render(<Login />);
    fill();
    await submit();
    expect(within(errorRegion()).getByText("Thiếu khóa MFA trên máy chủ")).toBeInTheDocument();
  });

  it("shows the real message for any other failure", async () => {
    auth.login.mockRejectedValue(new ApiError(500, "INTERNAL", "Máy chủ lỗi"));
    render(<Login />);
    fill();
    await submit();
    expect(within(errorRegion()).getByText("Máy chủ lỗi")).toBeInTheDocument();
  });

  it("shows the ?ssoError= banner mapped to Vietnamese", () => {
    nav.params = new URLSearchParams("ssoError=SSO_STATE_INVALID");
    render(<Login />);
    expect(within(errorRegion()).getByText("Phiên SSO không hợp lệ hoặc đã hết hạn — thử đăng nhập lại.")).toBeInTheDocument();
  });
});

describe("Login — MFA step 2", () => {
  async function toStep2() {
    auth.login.mockResolvedValue({ status: "mfa", pendingToken: "pend-1" });
    render(<Login />);
    fill();
    await submit();
    expect(screen.getByRole("heading", { level: 1, name: "Xác thực hai lớp" })).toBeInTheDocument();
  }

  it("asks for the code, then completes and redirects", async () => {
    await toStep2();
    await submit("Xác nhận");
    expect(within(errorRegion()).getByText("Nhập mã 6 số từ ứng dụng xác thực (hoặc mã dự phòng).")).toBeInTheDocument();
    auth.completeMfa.mockResolvedValue({});
    fireEvent.change(field("Mã xác thực"), { target: { value: " 123456 " } });
    await submit("Xác nhận");
    expect(auth.completeMfa).toHaveBeenCalledWith("pend-1", "123456");
    expect(loc.href).toBe("/home");
  });

  it("keeps step 2 on a wrong code", async () => {
    await toStep2();
    auth.completeMfa.mockRejectedValue(new ApiError(401, "MFA_CODE_INVALID", ""));
    fireEvent.change(field("Mã xác thực"), { target: { value: "000000" } });
    await submit("Xác nhận");
    expect(within(errorRegion()).getByText("Mã xác thực không đúng hoặc đã được sử dụng. Thử lại.")).toBeInTheDocument();
    expect(field("Mã xác thực")).toBeInTheDocument();
  });

  it("returns to step 1 with the password cleared when the pending session expired", async () => {
    await toStep2();
    auth.completeMfa.mockRejectedValue(new ApiError(401, "MFA_PENDING_INVALID", ""));
    fireEvent.change(field("Mã xác thực"), { target: { value: "000000" } });
    await submit("Xác nhận");
    expect(screen.getByRole("heading", { level: 1, name: "Đăng nhập" })).toBeInTheDocument();
    expect(within(errorRegion()).getByText("Phiên xác thực đã hết hạn — vui lòng đăng nhập lại.")).toBeInTheDocument();
    expect(field("Mật khẩu").value).toBe("");
    expect(field("Mã tổ chức").value).toBe("northwind");
  });

  it("goes back to step 1 on request", async () => {
    await toStep2();
    fireEvent.click(screen.getByRole("button", { name: "Quay lại đăng nhập" }));
    expect(screen.getByRole("heading", { level: 1, name: "Đăng nhập" })).toBeInTheDocument();
  });
});

describe("Login — tenant branding and SSO", () => {
  it("previews the typed tenant's branding without caching it (debounced)", async () => {
    api.getPublicBranding.mockResolvedValue({ accentColor: "#C2410C" });
    render(<Login />);
    fireEvent.change(field("Mã tổ chức"), { target: { value: "Northwind" } });
    expect(api.getPublicBranding).not.toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(450);
    });
    expect(api.getPublicBranding).toHaveBeenCalledWith("northwind");
    expect(branding.applyBranding).toHaveBeenCalledWith({ accentColor: "#C2410C" }, { persist: false });
  });

  it("offers a branded button per public provider and navigates to /start", async () => {
    api.ssoPublicProviders.mockResolvedValue([
      { id: "p-google", displayName: "Google", issuer: "https://accounts.google.com" },
      { id: "p-okta", displayName: "Okta", issuer: "https://acme.okta.com" },
    ]);
    render(<Login />);
    fireEvent.change(field("Mã tổ chức"), { target: { value: "northwind" } });
    await act(async () => {
      vi.advanceTimersByTime(450);
    });
    expect(screen.getByRole("button", { name: "Đăng nhập bằng Okta" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Đăng nhập bằng Google" }));
    expect(loc.assign).toHaveBeenCalledWith(ssoStartUrl("northwind", "p-google"));
    expect(screen.queryByRole("button", { name: "Đăng nhập bằng SSO" })).toBeNull();
    // Org emptied: the previous tenant's buttons go at once and the manual entry comes back.
    fireEvent.change(field("Mã tổ chức"), { target: { value: "" } });
    expect(screen.queryByRole("button", { name: "Đăng nhập bằng Google" })).toBeNull();
    expect(screen.getByRole("button", { name: "Đăng nhập bằng SSO" })).toBeInTheDocument();
  });

  it("falls back to the manual provider panel when the tenant lists none", async () => {
    render(<Login />);
    fireEvent.click(screen.getByRole("button", { name: "Đăng nhập bằng SSO" }));
    await submit("Tiếp tục với SSO");
    expect(within(errorRegion()).getByText("Nhập mã tổ chức ở ô phía trên trước.")).toBeInTheDocument();
    fireEvent.change(field("Mã tổ chức"), { target: { value: "northwind" } });
    await submit("Tiếp tục với SSO");
    expect(within(errorRegion()).getByText("Nhập issuer hoặc ID provider do quản trị viên cung cấp.")).toBeInTheDocument();
    fireEvent.change(field("Provider SSO (issuer hoặc ID)"), { target: { value: "https://accounts.google.com" } });
    await submit("Tiếp tục với SSO");
    expect(loc.assign).toHaveBeenCalledWith(ssoStartUrl("northwind", "https://accounts.google.com"));
  });
});
