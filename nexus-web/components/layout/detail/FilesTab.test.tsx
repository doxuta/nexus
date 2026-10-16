// FilesTab — file cards (files/base, SLDS 2.264.1). Every control names the file it acts on
// (blueprint: the file name sits in the crop's assistive text, "Preview: Proposal.pdf"), so a
// screen-reader user tabbing through N files never hears N identical "Xoá tệp" and deletes the
// wrong one. Nexus has no preview, so the crop is not a control: download and delete live in the
// card's actions menu. lib/api mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ensureAuth, deleteFile, downloadFile, type ApiRecord, type FieldMeta } from "@/lib/api";
import { FilesTab } from "./FilesTab";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, ensureAuth: vi.fn(), deleteFile: vi.fn(), downloadFile: vi.fn(), uploadFile: vi.fn() };
});

const FIELDS: FieldMeta[] = [{ name: "docs", label: "Hồ sơ", fieldType: "file" }];
const RECORD: ApiRecord = {
  id: "r1",
  version: 1,
  data: { docs: [{ id: "f1", name: "Hop-dong.pdf" }, { id: "f2", name: "Bao-gia.xlsx" }] },
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(deleteFile).mockReset().mockResolvedValue(undefined);
  vi.mocked(downloadFile).mockReset().mockResolvedValue(undefined);
});

describe("FilesTab", () => {
  it("download and delete buttons name the file; the crop is not a control", () => {
    const { container } = render(<FilesTab entity="crm_contracts" recordId="r1" record={RECORD} allFields={FIELDS} />);
    expect(screen.getByRole("button", { name: "Tải xuống: Hop-dong.pdf" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Xoá tệp: Hop-dong.pdf" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Tải xuống: Bao-gia.xlsx" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Xoá tệp: Bao-gia.xlsx" })).toBeInTheDocument();
    for (const crop of container.querySelectorAll(".slds-file__crop")) {
      expect(crop.tagName).not.toBe("BUTTON");
      expect(crop.tagName).not.toBe("A");
    }
  });

  it("deleting removes exactly the named file", async () => {
    render(<FilesTab entity="crm_contracts" recordId="r1" record={RECORD} allFields={FIELDS} />);
    fireEvent.click(screen.getByRole("button", { name: "Xoá tệp: Bao-gia.xlsx" }));
    await waitFor(() => expect(deleteFile).toHaveBeenCalledWith("f2"));
    await waitFor(() => expect(screen.queryByText("Bao-gia.xlsx")).toBeNull());
    expect(screen.getByText("Hop-dong.pdf")).toBeInTheDocument();
  });

  it("download passes the file's id and name", async () => {
    render(<FilesTab entity="crm_contracts" recordId="r1" record={RECORD} allFields={FIELDS} />);
    fireEvent.click(screen.getByRole("button", { name: "Tải xuống: Hop-dong.pdf" }));
    await waitFor(() => expect(downloadFile).toHaveBeenCalledWith("f1", "Hop-dong.pdf"));
  });
});

// DAILY-QUEUE #251: a record seen only through a manual share is read-only — no upload zone and no
// delete button (both are writes the server refuses); downloading stays.
describe("FilesTab — readOnly (hồ sơ chỉ thấy nhờ chia sẻ)", () => {
  it("không có vùng tải lên, không nút xoá; vẫn tải xuống được", () => {
    const { container } = render(<FilesTab entity="crm_contracts" recordId="r1" record={RECORD} allFields={FIELDS} readOnly />);
    expect(container.querySelector('input[type="file"]')).toBeNull();
    expect(screen.queryByText("Tải lên tệp mới")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Xoá tệp:/ })).toBeNull();
    expect(screen.getByRole("button", { name: "Tải xuống: Hop-dong.pdf" })).toBeInTheDocument();
  });

  it("không readOnly: có vùng tải lên (đối chứng)", () => {
    const { container } = render(<FilesTab entity="crm_contracts" recordId="r1" record={RECORD} allFields={FIELDS} />);
    expect(container.querySelector('input[type="file"]')).not.toBeNull();
  });
});
