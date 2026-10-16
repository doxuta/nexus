// Component test — ImportScreen dedup wiring (IMPORT-DEDUP-FE): the map-step "Chống trùng
// lặp (dedup)" selector (candidates = MAPPED string-coercing fields only; number/currency/…
// are silent no-ops on the BE, so they must not be offered), the exactly-one-unique default,
// and the result step splitting RECORD_DUPLICATE rows out from other failures.
// On SLDS (specs/038 T026) the selectors are kit Comboboxes (blueprint combobox/base), so option
// lists are read from the opened listbox instead of a native <select>.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, act, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ImportScreen } from "@/components/screens/ImportScreen";
import { logout } from "@/lib/api";

// The page header names the object; the real hook needs the auth + query providers.
vi.mock("@/lib/metadata/hooks", () => ({ useEntityLabel: () => "Khách hàng" }));

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const FIELDS = [
  { name: "email", label: "Email", fieldType: "email", isUnique: true },
  { name: "name", label: "Tên", fieldType: "text" },
  { name: "score", label: "Điểm", fieldType: "number" },
];

// Routes /metadata/fields; captures the /import POST body when importBody is given.
function routeFetch(importBody?: unknown, capture?: (fd: FormData) => void): typeof fetch {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS }, 200);
    if (u.endsWith("/import")) {
      capture?.(init?.body as FormData);
      return jsonRes(importBody ?? { data: { total: 0, inserted: 0, failed: 0, errors: [] } }, 200);
    }
    return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
  }) as unknown as typeof fetch;
}

// Uploads a CSV through the hidden file input and waits for the map step.
// Must flush the fields-load effect FIRST: parseFile guesses the mapping (and the dedup
// default) from the `fields` state at parse time — uploading before it commits would
// silently produce an empty mapping in the test only.
async function reachMapStep() {
  await waitFor(() => expect(global.fetch).toHaveBeenCalled());
  await act(async () => {}); // flush setFields from the resolved metadata fetch
  const input = document.querySelector('input[type="file"]') as HTMLInputElement;
  const file = new File(["email,name,score\na@b.c,An,5\n"], "test.csv", { type: "text/csv" });
  fireEvent.change(input, { target: { files: [file] } });
  await waitFor(() => {
    expect(screen.getByText("Ánh xạ cột CSV")).toBeInTheDocument();
  });
}

const dedupBox = () => screen.getByRole("combobox", { name: "Chống trùng lặp (dedup)" });

// specs/061: the preview's dry run names how many rows will go in; the button is usable once it answered.
async function clickRun() {
  const btn = await screen.findByRole("button", { name: /Nhập \d+ dòng hợp lệ|Chạy nhập khẩu/ });
  await waitFor(() => expect(btn).toBeEnabled());
  fireEvent.click(btn);
}

// Opens a combobox, reads its option labels, closes it again.
async function optionTexts(box: HTMLElement): Promise<string[]> {
  await userEvent.click(box);
  const texts = screen.getAllByRole("option").map((o) => o.textContent ?? "");
  await userEvent.keyboard("{Escape}");
  return texts;
}

describe("ImportScreen — dedup", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api module token cache
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("map step offers mapped string fields only, defaults to the single unique field", async () => {
    global.fetch = routeFetch();
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();

    // Exactly one unique field mapped → preselected as default.
    expect(dedupBox()).toHaveTextContent("Email (email) · unique");
    const texts = await optionTexts(dedupBox());
    expect(texts[0]).toBe("— Không dedup —");
    expect(texts.some((t) => t.includes("(email)") && t.includes("· unique"))).toBe(true);
    expect(texts.some((t) => t.includes("(name)"))).toBe(true);
    // number field: BE coerces to float — dedup would be a silent no-op → NOT offered.
    expect(texts.some((t) => t.includes("(score)"))).toBe(false);
  });

  it("unmapping the selected dedup field resets the selection to no-dedup", async () => {
    global.fetch = routeFetch();
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();

    expect(dedupBox()).toHaveTextContent("Email (email) · unique");
    // The "email" CSV column — unmap it (— Bỏ qua —).
    await userEvent.click(screen.getByRole("combobox", { name: "Trường cho cột email" }));
    await userEvent.click(screen.getByRole("option", { name: "— Bỏ qua —" }));
    await waitFor(() => expect(dedupBox()).toHaveTextContent("— Không dedup —"));
    // And the candidate list no longer offers the unmapped field.
    const texts = await optionTexts(dedupBox());
    expect(texts.some((t) => t.includes("(email)"))).toBe(false);
  });

  it("sends dedupField with the import and splits duplicate rows out in the result", async () => {
    let sentBody: FormData | null = null;
    global.fetch = routeFetch(
      {
        data: {
          total: 3, inserted: 1, failed: 2,
          errors: [
            { row: 2, field: "email", code: "RECORD_DUPLICATE", message: 'giá trị "a@b.c" đã tồn tại' },
            { row: 3, field: "name", code: "RECORD_REQUIRED_MISSING", message: "field bắt buộc thiếu giá trị" },
          ],
        },
      },
      (fd) => { sentBody = fd; },
    );
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();

    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    // Preview: mapped columns under their field labels, the CSV rows as data.
    const preview = await screen.findByRole("table", { name: "Xem trước dữ liệu sẽ nhập" });
    expect(preview).toHaveTextContent("Email");
    expect(preview).toHaveTextContent("a@b.c");
    await clickRun();

    await waitFor(() => {
      expect(screen.getByText("Kết quả nhập khẩu")).toBeInTheDocument();
    });
    // The default dedup field went out on the wire — on the real run, which is not a dry run.
    expect(sentBody!.get("dedupField")).toBe("email");
    expect(sentBody!.get("dryRun")).toBeNull();
    // Stats split: inserted / skipped-duplicate / other failures (label above value — the
    // page-header detail-block order).
    expect(screen.getByText("Tạo thành công").nextSibling?.textContent).toBe("1");
    expect(screen.getByText("Bỏ qua (trùng)").nextSibling?.textContent).toBe("1");
    expect(screen.getByText("Lỗi khác").nextSibling?.textContent).toBe("1");
    expect(screen.getByText(/1 dòng bị bỏ qua vì trùng lặp, 1 dòng lỗi/)).toBeInTheDocument();
    // Duplicate row is tagged distinctly in the error table.
    expect(screen.getByText("Trùng")).toBeInTheDocument();
    expect(screen.getByText(/đã tồn tại/)).toBeInTheDocument();
  });

  it("result without duplicates keeps the plain 3-stat layout (no fake dup column)", async () => {
    global.fetch = routeFetch({ data: { total: 1, inserted: 1, failed: 0, errors: [] } });
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();

    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    await clickRun();

    await waitFor(() => {
      expect(screen.getByText("Kết quả nhập khẩu")).toBeInTheDocument();
    });
    expect(screen.queryByText("Bỏ qua (trùng)")).not.toBeInTheDocument();
    expect(screen.getByText("Thất bại")).toBeInTheDocument();
    expect(screen.getByText(/không có lỗi/)).toBeInTheDocument();
    expect(screen.queryByText(/chưa ghi được lịch sử/)).not.toBeInTheDocument();
  });

  // specs/048 S7a: rows are stored but their history job could not be queued — say so.
  it("warns when the history of the imported rows could not be queued", async () => {
    global.fetch = routeFetch({ data: { total: 2, inserted: 2, failed: 0, errors: [], auditJobsFailed: 1 } });
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();

    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    await clickRun();

    await waitFor(() => {
      expect(screen.getByText(/chưa ghi được lịch sử/)).toBeInTheDocument();
    });
  });

  // queue #144: a field locked for the caller (readOnly) is refused FIELD_FORBIDDEN for the WHOLE
  // import if mapped — the auto-guess used to map a same-named CSV column onto it, so every import
  // of that file failed. Like "Tạo nhiều dòng" (#13), the locked field is not a mapping target.
  it("a field locked for the caller is neither auto-mapped nor offered as a target", async () => {
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      if (String(url).includes("/metadata/fields/")) {
        return jsonRes({ data: [...FIELDS, { name: "amount", label: "Giá trị", fieldType: "number", readOnly: true }] }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
    render(<ImportScreen entity="accounts" />);
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["name,amount\nAn,5\n"], "t.csv", { type: "text/csv" })] } });
    await screen.findByText("Ánh xạ cột CSV");

    const amountCol = screen.getByRole("combobox", { name: "Trường cho cột amount" });
    expect(amountCol).toHaveTextContent("— Bỏ qua —");
    const texts = await optionTexts(amountCol);
    expect(texts.some((t) => t.includes("(amount)"))).toBe(false);
    expect(texts.some((t) => t.includes("(name)"))).toBe(true);
  });

  it("rejects a file that is not CSV and stays on the upload step", async () => {
    global.fetch = routeFetch();
    render(<ImportScreen entity="accounts" />);
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    // Drag & drop bypasses the picker's `accept` filter, so the screen checks the file itself.
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    const xlsx = new File(["PK"], "khach.xlsx", {
      type: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    });
    fireEvent.change(input, { target: { files: [xlsx] } });
    expect(await screen.findByText(/Chỉ nhận tệp .csv/)).toBeInTheDocument();
    expect(screen.queryByText("Ánh xạ cột CSV")).not.toBeInTheDocument();
  });
});

// specs/048 T136: rows refused by a before-save rule come back in errors[] with the rule's sentence
// (one entry per broken rule); the result table shows that sentence under the field's label, never
// the code. When the rules cannot be read the whole import is refused with 503 "busy, retry".
describe("ImportScreen — before-save rules", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  async function runImport() {
    render(<ImportScreen entity="accounts" />);
    await reachMapStep();
    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    await clickRun();
  }

  it("a vetoed row lists the rule's sentence under the field's label, no code", async () => {
    global.fetch = routeFetch({
      data: {
        total: 3, inserted: 2, failed: 1,
        errors: [{ row: 2, field: "name", code: "RECORD_HOOK_VETO", message: "Tên khách phải viết hoa chữ đầu" }],
      },
    });
    await runImport();
    const cell = await screen.findByText("Tên khách phải viết hoa chữ đầu", {}, { timeout: 3000 });
    const row = cell.closest("tr")!;
    expect(row.querySelector("th")).toHaveTextContent("2");
    expect(row).toHaveTextContent("Tên");
    expect(document.body.textContent).not.toMatch(/RECORD_HOOK_VETO/);
  });

  it("503 RECORD_HOOKS_UNAVAILABLE ⇒ 'busy, retry', nothing imported", async () => {
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS }, 200);
      if (u.endsWith("/import")) return jsonRes({ error: "Hệ thống bận, thử lại sau ít giây", code: "RECORD_HOOKS_UNAVAILABLE" }, 503);
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
    await runImport();
    expect(await screen.findByText(/Hệ thống bận, thử lại sau ít giây/, {}, { timeout: 3000 })).toBeInTheDocument();
    expect(screen.queryByText("Kết quả nhập khẩu")).toBeNull();
  });
});

// specs/061 US2: "Xem trước" asks the server (dryRun) and marks each refused cell before anything
// is written; the run button names how many rows will go in.
describe("ImportScreen — preview dry run", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  // Answers the dry run with dryBody and the real run with runBody; records every /import body.
  function dryRunFetch(dryBody: unknown, runBody: unknown, sent: FormData[]): typeof fetch {
    return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      if (u.includes("/metadata/fields/")) return jsonRes({ data: FIELDS }, 200);
      if (u.endsWith("/import")) {
        const fd = init?.body as FormData;
        sent.push(fd);
        return jsonRes(fd.get("dryRun") === "true" ? dryBody : runBody, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
  }

  async function uploadCsv(text: string) {
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File([text], "khach.csv", { type: "text/csv" })] } });
    await screen.findByText("Ánh xạ cột CSV");
  }

  it("marks the refused cell with its message and blocks a run with nothing valid", async () => {
    const sent: FormData[] = [];
    global.fetch = dryRunFetch(
      {
        data: {
          total: 1, inserted: 0, failed: 1, valid: 0, dryRun: true,
          errors: [{ row: 1, field: "score", code: "RECORD_INVALID_TYPE", message: 'giá trị "mười" không phải số' }],
        },
      },
      { data: { total: 1, inserted: 0, failed: 1, errors: [] } },
      sent,
    );
    render(<ImportScreen entity="accounts" />);
    await uploadCsv("email,name,score\na@b.c,An,mười\n");
    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));

    const run = await screen.findByRole("button", { name: "Nhập 0 dòng hợp lệ" });
    expect(run).toBeDisabled();
    expect(sent).toHaveLength(1);
    expect(sent[0].get("dryRun")).toBe("true");
    const preview = screen.getByRole("table", { name: "Xem trước dữ liệu sẽ nhập" });
    const cell = within(preview).getByText("mười").closest("td")!;
    expect(cell).toHaveClass("slds-has-error");
    expect(cell).toHaveTextContent('giá trị "mười" không phải số');
    expect(screen.getByText(/Không dòng nào hợp lệ/)).toBeInTheDocument();
    // The error list names the field by its label.
    const list = screen.getByRole("table", { name: "Lỗi từng dòng" });
    expect(list).toHaveTextContent("Điểm");
  });

  it("names the valid rows on the run button and imports without dryRun", async () => {
    const sent: FormData[] = [];
    global.fetch = dryRunFetch(
      {
        data: {
          total: 3, inserted: 0, failed: 1, valid: 2, dryRun: true,
          errors: [{ row: 3, message: "field relation thiếu" }],
        },
      },
      { data: { total: 3, inserted: 2, failed: 1, valid: 2, errors: [{ row: 3, message: "field relation thiếu" }] } },
      sent,
    );
    render(<ImportScreen entity="accounts" />);
    await uploadCsv("email,name,score\na@b.c,An,1\nb@b.c,Bình,2\nc@b.c,Chi,3\n");
    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));

    expect(await screen.findByText(/1 dòng có lỗi sẽ bị bỏ qua, 2 dòng hợp lệ sẽ được nhập/)).toBeInTheDocument();
    // A row error without a field marks the row number.
    expect(screen.getByTitle("Dòng 3 có lỗi")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Nhập 2 dòng hợp lệ" }));
    await screen.findByText("Kết quả nhập khẩu");
    expect(sent).toHaveLength(2);
    expect(sent[1].get("dryRun")).toBeNull();
  });

  it("reads a Vietnamese-region Excel file separated by ';'", async () => {
    global.fetch = dryRunFetch({ data: { total: 1, inserted: 0, failed: 0, valid: 1, errors: [] } }, {}, []);
    render(<ImportScreen entity="accounts" />);
    await uploadCsv("email;name;score\na@b.c;An, Jr.;5\n");
    // Three columns, auto-mapped by name — not one column called "email;name;score".
    expect(screen.getByRole("combobox", { name: "Trường cho cột score" })).toHaveTextContent("(score)");
    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    const preview = await screen.findByRole("table", { name: "Xem trước dữ liệu sẽ nhập" });
    expect(preview).toHaveTextContent("An, Jr.");
    expect(await screen.findByText("Cả 1 dòng hợp lệ — sẵn sàng nhập.")).toBeInTheDocument();
  });
});

// specs/061 US3 (#268): a column mapped to a relation offers "Khớp theo" — the target's text/email/
// code fields — and the choice goes to the server as matchBy, for the preview and for the run.
describe("ImportScreen — relation column matched by name", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  const DEAL_FIELDS = [
    { name: "name", label: "Tên", fieldType: "text" },
    { name: "project", label: "Dự án", fieldType: "relation", options: { relatedEntity: "projects" } },
  ];
  const PROJECT_FIELDS = [
    { name: "ten_du_an", label: "Tên dự án", fieldType: "text", isRequired: true },
    { name: "ma_du_an", label: "Mã dự án", fieldType: "text", isUnique: true },
    { name: "gia", label: "Giá", fieldType: "currency" },
  ];

  it("offers the target's matchable fields and sends the choice as matchBy", async () => {
    const sent: FormData[] = [];
    const fetched: string[] = [];
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      fetched.push(u);
      if (u.includes("/metadata/fields/deals")) return jsonRes({ data: DEAL_FIELDS }, 200);
      if (u.includes("/metadata/fields/projects")) return jsonRes({ data: PROJECT_FIELDS }, 200);
      if (u.endsWith("/import")) {
        sent.push(init?.body as FormData);
        return jsonRes({ data: { total: 1, inserted: 0, failed: 0, valid: 1, dryRun: true, errors: [] } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
    render(<ImportScreen entity="deals" />);
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["name,project\nCăn 1,PGR\n"], "d.csv", { type: "text/csv" })] } });
    await screen.findByText("Ánh xạ cột CSV");

    const matchBox = await screen.findByRole("combobox", { name: /Khớp «Dự án» theo/ });
    await waitFor(() => expect(fetched.filter((u) => u.includes("/metadata/fields/projects"))).toHaveLength(1));
    const texts = await optionTexts(matchBox);
    expect(texts).toEqual(["Tên của bản ghi (mặc định)", "Tên dự án", "Mã dự án · không trùng"]);
    await userEvent.click(matchBox);
    await userEvent.click(screen.getByRole("option", { name: "Mã dự án · không trùng" }));

    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    await screen.findByText("Cả 1 dòng hợp lệ — sẵn sàng nhập.");
    expect(sent[0].get("matchBy")).toBe(JSON.stringify({ project: "ma_du_an" }));
    await clickRun();
    await screen.findByText("Kết quả nhập khẩu");
    expect(sent[1].get("matchBy")).toBe(JSON.stringify({ project: "ma_du_an" }));
  });

  it("drops the choice when the column is no longer mapped to the relation", async () => {
    const sent: FormData[] = [];
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      if (u.includes("/metadata/fields/deals")) return jsonRes({ data: DEAL_FIELDS }, 200);
      if (u.includes("/metadata/fields/projects")) return jsonRes({ data: PROJECT_FIELDS }, 200);
      if (u.endsWith("/import")) {
        const fd = init?.body as FormData;
        sent.push(fd);
        // As the server does (buildMapping): a column mapped to no field refuses the whole file.
        const mapping = JSON.parse(String(fd.get("mapping"))) as Record<string, string>;
        if (Object.values(mapping).includes("")) {
          return jsonRes({ error: 'mapping chỉ tới field "" không tồn tại trong metadata', code: "RECORD_UNKNOWN_FIELD" }, 400);
        }
        return jsonRes({ data: { total: 1, inserted: 0, failed: 0, valid: 1, errors: [] } }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
    render(<ImportScreen entity="deals" />);
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["name,project\nCăn 1,PGR\n"], "d.csv", { type: "text/csv" })] } });
    await screen.findByText("Ánh xạ cột CSV");

    const matchBox = await screen.findByRole("combobox", { name: /Khớp «Dự án» theo/ });
    await waitFor(async () => expect(await optionTexts(matchBox)).toContain("Tên dự án"));
    await userEvent.click(matchBox);
    await userEvent.click(screen.getByRole("option", { name: "Tên dự án" }));
    await userEvent.click(screen.getByRole("combobox", { name: "Trường cho cột project" }));
    await userEvent.click(screen.getByRole("option", { name: "— Bỏ qua —" }));
    expect(screen.queryByRole("combobox", { name: /Khớp «Dự án» theo/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /Xem trước/ }));
    await screen.findByText("Cả 1 dòng hợp lệ — sẵn sàng nhập.");
    expect(sent[0].get("matchBy")).toBeNull();
    // "— Bỏ qua —" leaves the column out of the mapping, it is not sent as a field named "".
    expect(JSON.parse(String(sent[0].get("mapping")))).toEqual({ name: "name" });
  });

  // queue #357: a column mapped to a person field says what to write in it.
  it("tells the maker a person column takes an e-mail or a full name", async () => {
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      if (String(url).includes("/metadata/fields/deals")) {
        return jsonRes({ data: [{ name: "name", label: "Tên", fieldType: "text" }, { name: "owner_user", label: "Phụ trách", fieldType: "user" }] }, 200);
      }
      return jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500);
    }) as unknown as typeof fetch;
    render(<ImportScreen entity="deals" />);
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    await act(async () => {});
    const input = document.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [new File(["name,owner_user\nCăn 1,an@example.com\n"], "d.csv", { type: "text/csv" })] } });
    await screen.findByText("Ánh xạ cột CSV");
    expect(await screen.findByText(/ghi email hoặc họ tên của người phụ trách/)).toBeTruthy();
  });
});
