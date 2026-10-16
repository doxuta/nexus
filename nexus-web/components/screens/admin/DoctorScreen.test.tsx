// Component tests — DoctorScreen as a checklist (DOCTOR-CORE FE ../specs/008-nexus-doctor/spec.md §2.4; redrawn by
// specs/079 f7-kham-ve-lai). Pins: rows by severity, count:0 hidden, one button per row, the question in the row for a
// fix with consequences (and for any fix on a delivered/unknown project), the safe batch, the automatic second scan,
// report-only rows without a button, no native dialog, none of the old accordion/badge/progress/card chrome.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { DoctorScreen, CHECK_META, metaFor } from "@/components/screens/admin/DoctorScreen";
import { logout, type DoctorFinding } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

const SCANNED_AT = new Date(2026, 6, 10, 9, 30).toISOString();
const RESCANNED_AT = new Date(2026, 6, 10, 9, 31).toISOString();

function finding(over: Partial<DoctorFinding> & { checkId: string }): DoctorFinding {
  return {
    severity: "warn",
    autoFixSafe: true,
    count: 1,
    items: [{ key: "rel_abc", detail: "parent entity 'projects' deleted" }],
    ...over,
  };
}

// URL-routed fetch mock; scan responses are a SEQUENCE (initial scan, then the post-fix re-scan).
function routedFetch(opts: {
  scans: (() => Response)[];
  fix?: (init?: RequestInit) => Response;
  flush?: () => Response;
}) {
  const scanQueue = [...opts.scans];
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/auth/login")) {
      return jsonRes({ data: {
        accessToken: "at-test", refreshToken: "rt-test",
        user: { id: "u1", email: "admin@example.com", fullName: "Admin", isAdmin: true },
      } }, 200);
    }
    if (u.includes("/admin/doctor/scan") && method === "POST") {
      const next = scanQueue.shift();
      return next ? next() : jsonRes({ data: { findings: [], scannedAt: SCANNED_AT } }, 200);
    }
    if (u.includes("/admin/doctor/fix") && method === "POST") {
      return opts.fix ? opts.fix(init) : jsonRes({ data: { results: [] } }, 200);
    }
    if (u.includes("/admin/doctor/flush-caches") && method === "POST") {
      return opts.flush
        ? opts.flush()
        : jsonRes({ data: { flushed: ["meta", "acl", "workflow"], note: "in-proc cache — chỉ instance này; replica khác chờ TTL" } }, 200);
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

async function scanOnce() {
  fireEvent.click(screen.getByRole("button", { name: /Khám sức khoẻ|Khám lại/ }));
}
const ok = (findings: DoctorFinding[], at = SCANNED_AT) => () => jsonRes({ data: { findings, scannedAt: at } }, 200);
const fixCalls = () =>
  (global.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).includes("/admin/doctor/fix"));

describe("DoctorScreen", () => {
  let originalFetch: FetchFn;
  let nativeDialogs: ReturnType<typeof vi.fn>[];

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api module token cache between tests
    window.localStorage.clear();
    window.sessionStorage.clear();
    // Authenticated seam: seed a token so ensureAuth() is a no-op.
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    // jsdom does not implement them: plain stubs that count calls.
    nativeDialogs = [vi.fn(() => true), vi.fn(), vi.fn(() => null)];
    window.confirm = nativeDialogs[0] as unknown as typeof window.confirm;
    window.alert = nativeDialogs[1] as unknown as typeof window.alert;
    window.prompt = nativeDialogs[2] as unknown as typeof window.prompt;
  });

  afterEach(() => {
    // FR-007: not one native browser dialog in any state.
    expect(nativeDialogs.map((s) => s.mock.calls.length)).toEqual([0, 0, 0]);
    vi.restoreAllMocks();
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("not scanned yet: a sentence and one primary button, no big empty state", () => {
    render(<DoctorScreen />);
    expect(screen.getByText("Chưa khám trong lần mở này.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Khám sức khoẻ" })).toHaveClass("slds-button_brand");
    expect(screen.getByRole("button", { name: "Làm mới bộ nhớ đệm" })).toBeInTheDocument();
    expect(document.querySelector(".slds-illustration")).toBeNull();
  });

  it("rows by severity (critical first), count:0 hidden, no old chrome", async () => {
    global.fetch = routedFetch({
      scans: [ok([
        finding({ checkId: "B1", severity: "warn", count: 3, truncated: true }),
        finding({ checkId: "C1", severity: "critical", count: 7 }),
        finding({ checkId: "C2", severity: "info", count: 0, items: [] }), // clean — must NOT render
      ])],
    }) as unknown as typeof fetch;
    render(<DoctorScreen />);
    await scanOnce();

    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    expect(screen.queryByText(CHECK_META.C2.label)).not.toBeInTheDocument();
    const labels = screen.getAllByRole("listitem").map((li) => li.querySelector("p")?.textContent);
    expect(labels).toEqual([CHECK_META.C1.label, CHECK_META.B1.label]);
    expect(screen.getByText(/khoản lệch · đã cắt bớt/)).toBeInTheDocument();
    // the primary button becomes "Khám lại" (outlined) once something was scanned
    expect(screen.getByRole("button", { name: "Khám lại" })).not.toHaveClass("slds-button_brand");
    expect(screen.getByText(/Khám lúc 09:30/)).toBeInTheDocument();
    // none of the old chrome, and the check code is not on the surface outside the detail
    for (const sel of [".slds-badge", ".slds-accordion", ".slds-progress-bar", "article.slds-card", "[class*='slds-icon-standard-']", "[role=dialog]"]) {
      expect(document.querySelector(sel), sel).toBeNull();
    }
    expect(screen.queryByText("C1")).not.toBeInTheDocument();
  });

  it("'Xem n khoản' is a button in the sub line (aria-expanded) and opens the item table; truncated says so", async () => {
    global.fetch = routedFetch({ scans: [ok([finding({ checkId: "B1", count: 3, truncated: true })])] }) as unknown as typeof fetch;
    render(<DoctorScreen />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    expect(screen.queryByText("rel_abc")).not.toBeInTheDocument();
    const view = screen.getByRole("button", { name: "Xem 3 khoản" });
    expect(view).toHaveAttribute("aria-expanded", "false");
    // exactly one button at the end of the row: the fix button
    const row = screen.getByRole("listitem");
    expect(within(row).getAllByRole("button").map((b) => b.textContent)).toEqual(["Xem 3 khoản", "Sửa"]);
    fireEvent.click(view);
    expect(screen.getByRole("button", { name: "Ẩn 3 khoản" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("rel_abc")).toBeInTheDocument();
    expect(screen.getByText(/Danh sách đã cắt bớt/)).toBeInTheDocument();
  });

  it("a safe check on a not-delivered project is fixed straight away, then re-scanned: results list + 'tự khám lại'", async () => {
    global.fetch = routedFetch({
      scans: [ok([finding({ checkId: "B1", count: 3 })]), ok([], RESCANNED_AT)],
      fix: () => jsonRes({ data: { results: [{ checkId: "B1", fixed: 3, skipped: 0, failed: 0, errors: [] }] } }, 200),
    }) as unknown as typeof fetch;
    render(<DoctorScreen deliveredState="not-delivered" />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: `Sửa ${CHECK_META.B1.label}` }));

    const head = await screen.findByRole("heading", { name: "Kết quả sửa" });
    expect(screen.getByText("Đã sửa 3 · Bỏ qua 0 · Lỗi 0")).toBeInTheDocument();
    expect(screen.getByText("Đã tự khám lại lúc 09:31")).toBeInTheDocument();
    // focus lands on the results heading, not on <body>
    await waitFor(() => expect(document.activeElement).toBe(head));
    expect(fixCalls()).toHaveLength(1);
    expect(JSON.parse(String(fixCalls()[0][1].body))).toEqual({ checkIds: ["B1"] });
    // a clean second scan is one line
    expect(screen.getByText("Đã khám 20 khoản, khoản nào cũng khớp.")).toBeInTheDocument();
    expect(screen.getByText(/không phát hiện lệch nào/)).toBeInTheDocument();
    const scans = (global.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(([u]) => String(u).includes("/admin/doctor/scan"));
    expect(scans).toHaveLength(2);
  });

  it("a needs-confirm check asks in the row first (nothing sent), Esc cancels and returns focus to 'Sửa', confirm sends confirm:true", async () => {
    global.fetch = routedFetch({
      scans: [ok([finding({ checkId: "B2", autoFixSafe: false, count: 2 })]), ok([], RESCANNED_AT)],
      fix: () => jsonRes({ data: { results: [{ checkId: "B2", fixed: 2, skipped: 0, failed: 0, errors: [] }] } }, 200),
    }) as unknown as typeof fetch;
    render(<DoctorScreen deliveredState="not-delivered" />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B2.label)).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /Sửa tất cả/ })).not.toBeInTheDocument();

    const fixBtn = screen.getByRole("button", { name: `Sửa ${CHECK_META.B2.label}` });
    fireEvent.click(fixBtn);
    expect(fixCalls()).toHaveLength(0);
    const cancel = screen.getByRole("button", { name: "Huỷ" });
    fireEvent.keyDown(cancel, { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Tôi hiểu, sửa" })).not.toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(fixBtn));
    expect(fixCalls()).toHaveLength(0);

    fireEvent.click(fixBtn);
    fireEvent.click(screen.getByRole("button", { name: "Tôi hiểu, sửa" }));
    await waitFor(() => expect(fixCalls()).toHaveLength(1));
    expect(JSON.parse(String(fixCalls()[0][1].body))).toEqual({ checkIds: ["B2"], confirm: true });
  });

  it("delivered or unknown: even a safe fix asks first with the 'áp ngay' sentence; not-delivered / no-concept do not", async () => {
    for (const [state, asks] of [["delivered", true], ["unknown", true], ["not-delivered", false], ["no-concept", false]] as const) {
      global.fetch = routedFetch({
        scans: [ok([finding({ checkId: "B1", count: 3 })])],
        fix: () => jsonRes({ data: { results: [{ checkId: "B1", fixed: 3, skipped: 0, failed: 0, errors: [] }] } }, 200),
      }) as unknown as typeof fetch;
      const view = render(<DoctorScreen deliveredState={state} />);
      await scanOnce();
      await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
      fireEvent.click(screen.getByRole("button", { name: `Sửa ${CHECK_META.B1.label}` }));
      if (asks) {
        expect(screen.getByText(/áp ngay cho người dùng thật/)).toBeInTheDocument();
        expect(fixCalls()).toHaveLength(0);
      } else {
        await waitFor(() => expect(fixCalls()).toHaveLength(1));
      }
      view.unmount();
    }
  });

  it("'Sửa tất cả (an toàn)' carries ONLY fixable ids; on a delivered project it asks with the count first", async () => {
    global.fetch = routedFetch({
      scans: [ok([
        finding({ checkId: "B1", autoFixSafe: true, count: 3 }),
        finding({ checkId: "B2", autoFixSafe: false, count: 2 }),
        finding({ checkId: "B5", severity: "info", autoFixSafe: false, reportOnly: true, count: 1 }),
      ]), ok([finding({ checkId: "B2", autoFixSafe: false, count: 2 })], RESCANNED_AT)],
      fix: () => jsonRes({ data: { results: [{ checkId: "B1", fixed: 3, skipped: 0, failed: 0, errors: [] }] } }, 200),
    }) as unknown as typeof fetch;
    render(<DoctorScreen deliveredState="delivered" />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Sửa tất cả (an toàn)" }));
    expect(fixCalls()).toHaveLength(0);
    expect(screen.getByText(/Sửa 1 khoản an toàn/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Sửa 1 khoản" }));
    await waitFor(() => expect(fixCalls()).toHaveLength(1));
    expect(JSON.parse(String(fixCalls()[0][1].body))).toEqual({ checkIds: ["B1"] });
    // B1 is gone from the fresh report, B2 remains
    await waitFor(() => expect(screen.queryByRole("button", { name: `Sửa ${CHECK_META.B1.label}` })).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: `Sửa ${CHECK_META.B2.label}` })).toBeInTheDocument();
  });

  it("report-only: words 'sửa tay' (not a button), 'Cần tự xử lý' in the description, out of the batch", async () => {
    global.fetch = routedFetch({
      scans: [ok([finding({ checkId: "B5", severity: "info", autoFixSafe: false, reportOnly: true, count: 1 })])],
    }) as unknown as typeof fetch;
    render(<DoctorScreen />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(/Cần tự xử lý/)).toBeInTheDocument());
    const manual = screen.getByText("sửa tay");
    expect(manual.closest("button")).toBeNull();
    expect(screen.queryByRole("button", { name: `Sửa ${CHECK_META.B5.label}` })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Sửa tất cả/ })).not.toBeInTheDocument();
  });

  it("a failed fix shows the sentence for the server's code, not the code; the stale report stays", async () => {
    global.fetch = routedFetch({
      scans: [ok([finding({ checkId: "B1", count: 3 })])],
      fix: () => jsonRes({ error: "Check này cần xác nhận tường minh", code: "DOCTOR_CONFIRM_REQUIRED" }, 422),
    }) as unknown as typeof fetch;
    render(<DoctorScreen deliveredState="not-delivered" />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: `Sửa ${CHECK_META.B1.label}` }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/Check này cần xác nhận tường minh/);
    expect(alert.textContent).not.toMatch(/DOCTOR_CONFIRM_REQUIRED/);
    expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument();
  });

  it("a network failure on scan says so in words and leaves the button usable", async () => {
    global.fetch = vi.fn(async () => { throw new TypeError("net"); }) as unknown as typeof fetch;
    render(<DoctorScreen />);
    await scanOnce();
    expect(await screen.findByText("Chưa khám được. Thử lại sau ít phút.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Khám sức khoẻ" })).toBeEnabled();
  });

  it("a fix result with errors shows them under the line, in the error tone", async () => {
    global.fetch = routedFetch({
      scans: [ok([finding({ checkId: "B1", count: 3 })]), ok([finding({ checkId: "B1", count: 1 })], RESCANNED_AT)],
      fix: () => jsonRes({ data: { results: [{ checkId: "B1", fixed: 2, skipped: 0, failed: 1, errors: ["không sửa được rel_abc"] }] } }, 200),
    }) as unknown as typeof fetch;
    render(<DoctorScreen deliveredState="not-delivered" />);
    await scanOnce();
    await waitFor(() => expect(screen.getByText(CHECK_META.B1.label)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: `Sửa ${CHECK_META.B1.label}` }));
    expect(await screen.findByText("Đã sửa 2 · Bỏ qua 0 · Lỗi 1")).toBeInTheDocument();
    expect(screen.getByText("không sửa được rel_abc")).toBeInTheDocument();
  });

  it("'Làm mới bộ nhớ đệm' posts flush-caches and shows the replica note inside an aria-live region", async () => {
    global.fetch = routedFetch({ scans: [] }) as unknown as typeof fetch;
    render(<DoctorScreen />);
    fireEvent.click(screen.getByRole("button", { name: "Làm mới bộ nhớ đệm" }));
    const note = await screen.findByText(/replica khác chờ TTL/);
    expect(note.closest("[aria-live]")).not.toBeNull();
    expect(screen.getByText(/Đã làm mới bộ nhớ đệm: meta, acl, workflow/)).toBeInTheDocument();
    const flushCalls = (global.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      ([u, init]) => String(u).includes("/admin/doctor/flush-caches") && (init as RequestInit)?.method === "POST",
    );
    expect(flushCalls).toHaveLength(1);
  });
});

describe("metaFor", () => {
  it("resolves exact ids, prefix ids (B1_PHANTOM_…), and degrades unknown ids to the raw id", () => {
    expect(metaFor("B1").label).toBe(CHECK_META.B1.label);
    expect(metaFor("B1_PHANTOM_RELATIONSHIPS").label).toBe(CHECK_META.B1.label);
    expect(metaFor("A9_FUTURE_CHECK").label).toBe("A9_FUTURE_CHECK"); // honest fallback, no crash
    expect(metaFor("D2_CURRENCY_CODE_MISMATCH").label).toBe("Bản ghi mang sai mã tiền tệ");
    expect(metaFor("D3_APPROVAL_SEAT_DRIFT").label).toBe("Hồ sơ chờ duyệt chưa có người nhận việc");
    expect(metaFor("C9_LOCKED_REQUIRED_FIELDS").label).toBe("Vai bị khoá trường bắt buộc");
    // C10 is a needs-confirm, ONE-WAY move: the words next to the question say so.
    const c10 = metaFor("C10_WEBHOOK_INLINE_SECRET");
    expect(c10.desc).toMatch(/một chiều/);
    expect(c10.desc).toMatch(/không Gửi lại được/);
    expect(c10.desc).toMatch(/đã xoá/);
    expect(metaFor("C11_CREDENTIAL_RETIRED_KEY").desc).toMatch(/về 0/);
    expect(metaFor("C12_CREDENTIAL_UNDECRYPTABLE").desc).toMatch(/KHÔNG gửi/);
  });

  it("every check speaks in the builder's words: no engine vocabulary in a label or description", () => {
    const banned = /generated column|obj_|rel_\*|tombstone|drift|\bcache\b|\blayout\b|\bworkflow\b|\bentity\b|\bmetadata\b/i;
    for (const [id, m] of Object.entries(CHECK_META)) {
      expect(`${m.label} ${m.desc}`, id).not.toMatch(banned);
    }
  });
});
