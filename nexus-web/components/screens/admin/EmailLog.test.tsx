// Component tests — "Nhật ký email" (specs/066 L1b), on SLDS (specs/038). Through the real lib/admin → fetch wire:
//   * the default view is the failed mail: GET /admin/email-log?status=failed&limit=50; ?status= in the URL
//     at mount time wins (the health screen links here);
//   * a row shows the MASKED recipient, the rule's subject TEMPLATE, the rule name ("Quy tắc đã xoá" when
//     the rule is gone), the status badge ("lần n" after a retry) and the error verbatim;
//   * the record link carries the TABLE LABEL ("Liên hệ"), never the machine name; a deleted table reads
//     "Bảng đã xoá" without a link;
//   * the server without mail configured gets a warning; an empty log says so; "Xem thêm" feeds the cursor.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { EmailLog } from "@/components/screens/admin/EmailLog";
import { ADMIN_LINKS } from "@/components/slds/shell/adminLinks";
import { logout } from "@/lib/api";
import type { EmailLogEntry } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const FAILED: EmailLogEntry = {
  id: "log-1",
  createdAt: "2026-10-01T02:00:00Z",
  finishedAt: "2026-10-01T02:00:05Z",
  source: "workflow",
  status: "failed",
  recipientMasked: "a***@gmail.com",
  subjectTemplate: "Chào {{ho_ten}}",
  workflowId: "wf-1",
  workflowName: "Chào khách mới",
  actionIndex: 1,
  entityName: "crm_contacts",
  recordId: "rec-1",
  jobId: "job-1",
  attempts: 5,
  lastError: "services.EmailAlertJobHandler: dial tcp: connection refused to [redacted]",
  resendOf: null,
  resentAt: null,
  createdBy: null,
  createdByName: null,
  canResend: false,
};
const SENT_GONE: EmailLogEntry = {
  ...FAILED,
  id: "log-2",
  status: "sent",
  attempts: 1,
  lastError: null,
  workflowName: null, // the rule was deleted
  entityName: "bang_cu", // the table was deleted
  recordId: "rec-2",
};

const ENTITIES = [{ name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" }];

interface Calls { gets: string[] }

function routedFetch(calls: Calls, pages: { data: EmailLogEntry[]; nextCursor?: string; mailConfigured?: boolean }[]) {
  let page = 0;
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/metadata/entities") && method === "GET") return jsonRes({ data: ENTITIES }, 200);
    if (u.includes("/admin/email-log") && method === "GET") {
      calls.gets.push(u);
      const p = pages[Math.min(page++, pages.length - 1)];
      return jsonRes({
        data: p.data,
        pagination: { limit: 50, nextCursor: p.nextCursor ?? null },
        meta: { mailConfigured: p.mailConfigured ?? true, queueEnabled: true },
      }, 200);
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

const query = (u: string) => new URL(u).searchParams;

describe("EmailLog", () => {
  let originalFetch: FetchFn;
  let calls: Calls;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    window.history.pushState({}, "", "/admin/email-log");
    calls = { gets: [] };
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.history.pushState({}, "", "/");
  });

  it("opens on the failed mail and shows masked recipient, template, rule, attempts and the error", async () => {
    global.fetch = routedFetch(calls, [{ data: [FAILED] }]) as unknown as typeof fetch;
    render(<EmailLog />);

    expect(await screen.findByText("a***@gmail.com")).toBeInTheDocument();
    expect(screen.getByText("Chào {{ho_ten}}")).toBeInTheDocument();
    expect(screen.getByText("Chào khách mới")).toBeInTheDocument();
    expect(screen.getByText("Lỗi", { selector: ".slds-badge" })).toBeInTheDocument();
    expect(screen.getByText("lần 5")).toBeInTheDocument();
    expect(screen.getByText(/connection refused to \[redacted\]/)).toBeInTheDocument();
    expect(query(calls.gets[0]).get("status")).toBe("failed");
    expect(query(calls.gets[0]).get("limit")).toBe("50");
  });

  it("names the record by the table LABEL, and a deleted rule / table plainly", async () => {
    global.fetch = routedFetch(calls, [{ data: [FAILED, SENT_GONE] }]) as unknown as typeof fetch;
    render(<EmailLog />);

    const link = await screen.findByRole("link", { name: "Liên hệ" });
    expect(link).toHaveAttribute("href", "/crm_contacts/rec-1");
    expect(screen.queryByText("crm_contacts")).not.toBeInTheDocument();
    expect(screen.getByText("Quy tắc đã xoá")).toBeInTheDocument();
    expect(screen.getByText("Bảng đã xoá")).toBeInTheDocument();
    expect(screen.queryByText("bang_cu")).not.toBeInTheDocument();
    expect(screen.getAllByRole("link")).toHaveLength(1); // the deleted table gets no link
    expect(screen.getByText("Đã gửi", { selector: ".slds-badge" })).toBeInTheDocument();
  });

  it("warns when the server has no mail configured", async () => {
    global.fetch = routedFetch(calls, [{ data: [FAILED], mailConfigured: false }]) as unknown as typeof fetch;
    render(<EmailLog />);
    expect(await screen.findByText(/chưa cấu hình gửi email/)).toBeInTheDocument();
  });

  it("says so when the log is empty", async () => {
    global.fetch = routedFetch(calls, [{ data: [] }]) as unknown as typeof fetch;
    render(<EmailLog />);
    expect(await screen.findByText("Không có email nào khớp bộ lọc")).toBeInTheDocument();
  });

  it("reads ?status= from the URL once at mount", async () => {
    window.history.pushState({}, "", "/admin/email-log?status=sent");
    global.fetch = routedFetch(calls, [{ data: [SENT_GONE] }]) as unknown as typeof fetch;
    render(<EmailLog />);
    await screen.findByText("Bảng đã xoá");
    expect(query(calls.gets[0]).get("status")).toBe("sent");
  });

  it("loads the next page with the cursor", async () => {
    global.fetch = routedFetch(calls, [
      { data: [FAILED], nextCursor: "CUR1" },
      { data: [{ ...FAILED, id: "log-3", recipientMasked: "b***@x.vn" }] },
    ]) as unknown as typeof fetch;
    render(<EmailLog />);
    fireEvent.click(await screen.findByRole("button", { name: "Xem thêm" }));
    await waitFor(() => expect(screen.getByText("b***@x.vn")).toBeInTheDocument());
    expect(query(calls.gets[1]).get("after")).toBe("CUR1");
    expect(screen.getByText("a***@gmail.com")).toBeInTheDocument();
  });

  // specs/066 L3: forgot-password mail, admin-issued link, and a rule mail that never had an address.
  const RESET: EmailLogEntry = {
    ...FAILED, id: "log-r", source: "password_reset", recipientMasked: "t***@example.com", subjectTemplate: "Đặt lại mật khẩu tài khoản Nexus",
    workflowId: null, workflowName: null, actionIndex: null, entityName: null, recordId: null, jobId: null, attempts: 1,
    lastError: "services.AuthService.sendResetMail: dial tcp: connection refused", canResend: false,
  };
  const LINK_BY_NAME: EmailLogEntry = { ...RESET, id: "log-l1", source: "reset_link", createdBy: "u-adm", createdByName: "Quản trị An" };
  const LINK_BY_BUILDER: EmailLogEntry = { ...RESET, id: "log-l2", source: "reset_link", createdBy: "u-operator", createdByName: null };
  const NO_ADDRESS: EmailLogEntry = {
    ...FAILED, id: "log-n", recipientMasked: "", attempts: 0, canResend: true,
    lastError: 'workflow.runEmailAlert: không có địa chỉ nhận — mẫu "{{email}}" ra rỗng trên bản ghi này',
  };

  it("shows the source of each mail, the issuer by name or as the builder, and no Gửi lại on an auth mail", async () => {
    global.fetch = routedFetch(calls, [{ data: [RESET, LINK_BY_NAME, LINK_BY_BUILDER, { ...FAILED, canResend: true }] }]) as unknown as typeof fetch;
    render(<EmailLog />);

    expect(await screen.findByText("Đặt lại mật khẩu", { selector: "td, td *" })).toBeInTheDocument();
    expect(screen.getByText("Link do quản trị cấp · bởi Quản trị An")).toBeInTheDocument();
    expect(screen.getByText("Link do quản trị cấp · bởi Người dựng")).toBeInTheDocument();
    expect(screen.getByText("Luật tự động")).toBeInTheDocument();
    // Only the rule mail offers "Gửi lại"; the three auth rows do not.
    expect(screen.getAllByRole("button", { name: /^Gửi lại email lúc/ })).toHaveLength(1);
  });

  it("names a mail that never had an address, and still offers Gửi lại for it", async () => {
    global.fetch = routedFetch(calls, [{ data: [NO_ADDRESS] }]) as unknown as typeof fetch;
    render(<EmailLog />);

    expect(await screen.findByText("Chưa gửi — không có địa chỉ nhận")).toBeInTheDocument();
    expect(screen.getByText(/không có địa chỉ nhận — mẫu/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Gửi lại email lúc/ })).toBeInTheDocument();
  });

  it("filters by source and sends it to the server (also on the next page)", async () => {
    global.fetch = routedFetch(calls, [
      { data: [FAILED] },
      { data: [LINK_BY_NAME], nextCursor: "CUR9" },
      { data: [{ ...LINK_BY_NAME, id: "log-l9" }] },
    ]) as unknown as typeof fetch;
    render(<EmailLog />);
    await screen.findByText("a***@gmail.com");
    expect(query(calls.gets[0]).get("source")).toBeNull();

    fireEvent.click(screen.getByRole("combobox", { name: "Nguồn" }));
    fireEvent.click(await screen.findByRole("option", { name: "Link do quản trị cấp" }));
    await waitFor(() => expect(calls.gets).toHaveLength(2));
    expect(query(calls.gets[1]).get("source")).toBe("reset_link");
    expect(query(calls.gets[1]).get("status")).toBe("failed");

    fireEvent.click(await screen.findByRole("button", { name: "Xem thêm" }));
    await waitFor(() => expect(calls.gets).toHaveLength(3));
    expect(query(calls.gets[2]).get("source")).toBe("reset_link");
    expect(query(calls.gets[2]).get("after")).toBe("CUR9");
  });

  it("is listed in the Setup menu for every customer admin (not operator-only)", () => {
    const link = ADMIN_LINKS.find((l) => l.href === "/admin/email-log");
    expect(link).toBeDefined();
    expect(link?.operatorOnly).toBeFalsy();
  });
});
