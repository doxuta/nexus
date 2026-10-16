// specs/048 S7b / FE-before-save-hooks-5 — the map step's "Chạy tự động hoá" checkbox: off by default
// (a big file must not mail/webhook by surprise), says what it does, and only a ticked box sends
// runAutomations=true with the real run.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ImportScreen } from "@/components/screens/ImportScreen";
import { logout } from "@/lib/api";

vi.mock("@/lib/metadata/hooks", () => ({ useEntityLabel: () => "Khách hàng" }));

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const FIELDS = [
  { name: "email", label: "Email", fieldType: "email", isUnique: true },
  { name: "name", label: "Tên", fieldType: "text" },
];

function routeFetch(bodies: FormData[]): typeof fetch {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS }, 200);
    if (u.endsWith("/import")) {
      bodies.push(init?.body as FormData);
      return jsonRes({ data: { total: 1, inserted: 1, failed: 0, errors: [] } }, 200);
    }
    return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
  }) as unknown as typeof fetch;
}

async function reachMapStep() {
  await waitFor(() => expect(global.fetch).toHaveBeenCalled());
  await act(async () => {});
  const input = document.querySelector('input[type="file"]') as HTMLInputElement;
  fireEvent.change(input, { target: { files: [new File(["email,name\na@b.c,An\n"], "t.csv", { type: "text/csv" })] } });
  await waitFor(() => expect(screen.getByText("Ánh xạ cột CSV")).toBeInTheDocument());
}

const box = () => screen.getByRole("checkbox", { name: /Chạy tự động hoá/ });

async function runImport() {
  fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
  const run = await screen.findByRole("button", { name: /Nhập \d+ dòng hợp lệ|Chạy nhập khẩu/ });
  await waitFor(() => expect(run).toBeEnabled());
  fireEvent.click(run);
  await screen.findByText(/Kết quả/i);
}

describe("ImportScreen — chạy tự động hoá", () => {
  const originalFetch = global.fetch;
  beforeEach(async () => {
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("is off by default and warns what ticking does", async () => {
    global.fetch = routeFetch([]);
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();
    expect(box()).not.toBeChecked();
    expect(screen.getByText(/email, webhook/i)).toBeInTheDocument();
  });

  it("an untouched box sends no runAutomations", async () => {
    const bodies: FormData[] = [];
    global.fetch = routeFetch(bodies);
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();
    await runImport();
    const real = bodies.filter((b) => b.get("dryRun") !== "true");
    expect(real).toHaveLength(1);
    expect(real[0].get("runAutomations")).toBeNull();
  });

  it("a ticked box sends runAutomations=true with the real run only", async () => {
    const bodies: FormData[] = [];
    global.fetch = routeFetch(bodies);
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();
    await userEvent.click(box());
    expect(box()).toBeChecked();
    await runImport();
    const real = bodies.filter((b) => b.get("dryRun") !== "true");
    expect(real).toHaveLength(1);
    expect(real[0].get("runAutomations")).toBe("true");
    expect(bodies.filter((b) => b.get("dryRun") === "true").every((b) => b.get("runAutomations") === null)).toBe(true);
  });
});
