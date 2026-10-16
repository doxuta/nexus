// Embedded in the Studio frame (specs/078 lát u7-viec-nen-email, T114): the resend question opens IN the page right under the
// row, the job type reads in the maker's words, the link to the mail log is the prop, and the customer face is unchanged.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { BackgroundJobs } from "@/components/screens/admin/BackgroundJobs";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";
import type { BackgroundJob } from "@/lib/admin";

const jsonRes = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const HOOK: BackgroundJob = {
  id: "j1", jobType: "outbound_webhook", status: "failed", attempts: 1, maxAttempts: 10, lastError: "status 422",
  workflowId: "wf-1", runAt: "2026-09-30T02:00:00Z", createdAt: "2026-09-30T02:00:00Z", finishedAt: "2026-09-30T02:00:01Z",
};
const MAIL: BackgroundJob = { ...HOOK, id: "j2", jobType: "email_alert", lastError: "dial tcp" };

function mock(retry?: () => Response, jobs: BackgroundJob[] = [HOOK, MAIL]) {
  const posts: string[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/admin/workflows")) return jsonRes({ data: { workflows: [{ id: "wf-1", name: "Báo đơn" }] } }, 200);
    if (u.endsWith("/retry") && method === "POST") {
      posts.push(u);
      return retry ? retry() : jsonRes({ data: { job: { ...HOOK, status: "queued", attempts: 0, lastError: null } } }, 200);
    }
    if (u.includes("/admin/jobs")) return jsonRes({ data: jobs, pagination: { limit: 50, nextCursor: null } }, 200);
    return jsonRes({ error: "unmocked", code: "TEST_UNMOCKED" }, 500);
  }) as unknown as typeof fetch;
  return posts;
}

const embedded = (props: { emailLogHref?: string } = {}) =>
  render(<EmbeddedSurface><BackgroundJobs {...props} /></EmbeddedSurface>);

describe("BackgroundJobs embedded", () => {
  const orig = global.fetch;
  beforeEach(async () => {
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "t");
  });
  afterEach(() => { global.fetch = orig; });

  it("asks under the row, in the page, and POSTs only after the second press", async () => {
    const posts = mock();
    embedded();
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    fireEvent.click(await within(grid).findByRole("button", { name: /^Gửi lại/ }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    const ask = await screen.findByText(/Cùng mã giao nhận nên bên nhận không nhận trùng/);
    expect(ask).toHaveTextContent("Gửi lại việc này tới");
    expect(posts).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Gửi lại" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]).toMatch(/\/admin\/jobs\/j1\/retry$/);
  });

  it("409 JOB_NOT_RETRYABLE reads in one sentence", async () => {
    mock(() => jsonRes({ error: "x", code: "JOB_NOT_RETRYABLE" }, 409));
    embedded();
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    fireEvent.click(await within(grid).findByRole("button", { name: /^Gửi lại/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Gửi lại" }));
    expect(await screen.findByText(/Chỉ gửi lại được việc gửi dữ liệu bị lỗi/)).toBeInTheDocument();
  });

  it("labels the type 'Gửi dữ liệu sang hệ thống khác' and links the mail log through the prop", async () => {
    mock();
    embedded({ emailLogHref: "/studio/du-an/a/van-hanh/tu-dong/email" });
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByText("Gửi dữ liệu sang hệ thống khác")).toBeInTheDocument();
    expect(within(grid).queryByText("Giao webhook")).not.toBeInTheDocument();
    expect(within(grid).getByRole("link")).toHaveAttribute("href", "/studio/du-an/a/van-hanh/tu-dong/email");
  });

  it("the customer face keeps the modal prompt, the old label and the old link", async () => {
    mock();
    render(<BackgroundJobs />);
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByText("Giao webhook")).toBeInTheDocument();
    expect(within(grid).getByRole("link")).toHaveAttribute("href", "/admin/email-log");
    fireEvent.click(within(grid).getByRole("button", { name: /^Gửi lại/ }));
    expect(await screen.findByRole("alertdialog")).toBeInTheDocument();
  });

  // u7-viec-nen-ten-khoa (T158): the key column rides the server field inside the Studio frame too.
  it("shows the key name in 'Khoá' when the server sends credentialName, never a secret", async () => {
    mock(undefined, [{ ...HOOK, credentialName: "stripe-prod" }, { ...MAIL, credentialName: null }]);
    embedded();
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    expect(await within(grid).findByRole("columnheader", { name: "Khoá" })).toBeInTheDocument();
    expect(within(grid).getByText("stripe-prod")).toBeInTheDocument();
  });

  it("has no 'Khoá' column and no notice when the server omits credentialName", async () => {
    mock();
    embedded();
    const grid = await screen.findByRole("grid", { name: "Danh sách việc nền" });
    await within(grid).findAllByText("Báo đơn");
    expect(within(grid).queryByRole("columnheader", { name: "Khoá" })).not.toBeInTheDocument();
  });
});
