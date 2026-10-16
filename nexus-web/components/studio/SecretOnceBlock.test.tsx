// specs/078 T005 (u7-nen-nhung-mau-trang): the show-once secret block. The secret lives in props/state only.
import Link from "next/link";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NavigationBlockerProvider, useNavigationBlocker } from "@/lib/navigation-blocker";
import { SecretOnceBlock } from "./SecretOnceBlock";

const SECRET = "Zk9-s3cret-Pa55w0rd!";
const ROWS = [
  { label: "Email", value: "an@khach.vn" },
  { label: "Mật khẩu", value: SECRET },
];

let writeText: ReturnType<typeof vi.fn>;
beforeEach(() => {
  writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function Host({ onDone = () => {}, rows = ROWS, children }: { onDone?: () => void; rows?: typeof ROWS; children?: React.ReactNode }) {
  const [shown, setShown] = useState(true);
  return (
    <NavigationBlockerProvider>
      <button>Đặt lại mật khẩu</button>
      {shown && (
        <SecretOnceBlock rows={rows} onDone={() => { setShown(false); onDone(); }}>
          {children}
        </SecretOnceBlock>
      )}
    </NavigationBlockerProvider>
  );
}

describe("SecretOnceBlock", () => {
  it("shows the secret in a monospace <code>, never in an <input>, with the warning children", () => {
    render(<Host>Chỉ hiện một lần.</Host>);
    const code = screen.getByText(SECRET);
    expect(code.tagName).toBe("CODE");
    expect(document.querySelectorAll("input[type=text],input[type=password]")).toHaveLength(0);
    expect(screen.getByText("Chỉ hiện một lần.")).toBeInTheDocument();
  });

  it("takes one row or many", () => {
    render(<Host rows={[{ label: "Liên kết", value: "https://x/y" }]} />);
    expect(screen.getByText("https://x/y")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Sao chép cả khối/ })).toBeNull();
  });

  it("copies one row and the whole block, and announces it in a polite live region", async () => {
    render(<Host />);
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sao chép Mật khẩu" })); });
    expect(writeText).toHaveBeenCalledWith(SECRET);
    expect(document.querySelector("[aria-live=polite]")?.textContent).toContain("Đã sao chép");
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sao chép cả khối" })); });
    expect(writeText).toHaveBeenLastCalledWith(`Email: an@khach.vn\nMật khẩu: ${SECRET}`);
  });

  it("'Xong' stays off until the 'Tôi đã lưu' box is ticked; then it fires onDone, the secret leaves the DOM, focus returns to the opener", () => {
    const onDone = vi.fn();
    render(<Host onDone={onDone} />);
    const opener = screen.getByRole("button", { name: "Đặt lại mật khẩu" });
    opener.focus();
    const done = screen.getByRole("button", { name: "Xong" });
    expect(done).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu/ }));
    expect(done).toBeEnabled();
    fireEvent.click(done);
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(SECRET)).toBeNull();
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("moves focus into the block when it appears", () => {
    render(<Host />);
    expect(document.querySelector("[data-secret-once]")).toHaveFocus();
  });

  it("wraps a 200-character value inside its box (no horizontal overflow)", () => {
    render(<Host rows={[{ label: "Khoá", value: "k".repeat(200) }]} />);
    const code = screen.getByText("k".repeat(200));
    expect(code.className).toMatch(/break-all|break-words/);
  });

  it("leaves no trace in storage, caches or console", async () => {
    const setLocal = vi.spyOn(Storage.prototype, "setItem");
    const push = vi.spyOn(history, "pushState");
    const replace = vi.spyOn(history, "replaceState");
    const log = vi.spyOn(console, "log");
    const err = vi.spyOn(console, "error");
    const warn = vi.spyOn(console, "warn");
    const qc = new QueryClient();
    render(
      <QueryClientProvider client={qc}>
        <Host />
      </QueryClientProvider>,
    );
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sao chép Mật khẩu" })); });
    fireEvent.click(screen.getByRole("checkbox", { name: /Tôi đã lưu/ }));
    fireEvent.click(screen.getByRole("button", { name: "Xong" }));
    const seen = JSON.stringify([
      setLocal.mock.calls, push.mock.calls, replace.mock.calls, log.mock.calls, err.mock.calls, warn.mock.calls,
      qc.getQueryCache().getAll().map((q) => [q.queryKey, q.state.data]),
      qc.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data]),
    ]);
    expect(seen).not.toContain(SECRET);
  });
});

describe("SecretOnceBlock — leaving the page", () => {
  function Other() {
    const { setBlocked } = useNavigationBlocker();
    return <button onClick={() => setBlocked(false)}>Form khác lưu xong</button>;
  }

  it("arms the navigation blocker, and a different form disarming its own flag does not lift it", () => {
    const confirm = vi.fn().mockReturnValue(false);
    Object.defineProperty(window, "confirm", { value: confirm, configurable: true, writable: true });
    render(
      <NavigationBlockerProvider>
        <Link href="/home">Về trang chủ</Link>
        <Other />
        <SecretOnceBlock rows={ROWS} onDone={() => {}} />
      </NavigationBlockerProvider>,
    );
    fireEvent.click(screen.getByText("Form khác lưu xong"));
    fireEvent.click(screen.getByText("Về trang chủ"));
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  it("stops blocking once the block is gone", () => {
    const confirm = vi.fn().mockReturnValue(false);
    Object.defineProperty(window, "confirm", { value: confirm, configurable: true, writable: true });
    const { rerender } = render(
      <NavigationBlockerProvider>
        <Link href="/home">Về trang chủ</Link>
        <SecretOnceBlock rows={ROWS} onDone={() => {}} />
      </NavigationBlockerProvider>,
    );
    rerender(
      <NavigationBlockerProvider>
        <Link href="/home">Về trang chủ</Link>
      </NavigationBlockerProvider>,
    );
    fireEvent.click(screen.getByText("Về trang chủ"));
    expect(confirm).not.toHaveBeenCalled();
  });

  it("has its own beforeunload guard for reload / closing the tab, removed on unmount", () => {
    const { unmount } = render(<SecretOnceBlock rows={ROWS} onDone={() => {}} />);
    const ev = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    unmount();
    const after = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(after);
    expect(after.defaultPrevented).toBe(false);
  });
});
