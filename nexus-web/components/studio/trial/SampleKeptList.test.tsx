// specs/077 f5-sample-wipe T122: the kept list.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { SampleKeptList } from "./SampleKeptList";

describe("SampleKeptList", () => {
  const label = (e: string) => ({ customers: "Khách hàng", appointments: "Lịch hẹn" })[e] ?? e;
  it("renders nothing when empty", () => {
    const { container } = render(<SampleKeptList kept={[]} label={label} />);
    expect(container).toBeEmptyDOMElement();
  });
  it("one sentence per reason, short id, same-tab link", () => {
    render(
      <SampleKeptList
        label={label}
        kept={[
          { entity: "customers", recordId: "aaaa-111111", reason: "referenced_by_real", blockedBy: [{ entity: "appointments", field: "customer", count: 1 }] },
          { entity: "customers", recordId: "aaaa-222222", reason: "field_default", blockedBy: [{ entity: "appointments", field: "customer", count: 1 }] },
          { entity: "customers", recordId: "aaaa-333333", reason: "too_large", blockedBy: [] },
          { entity: "customers", recordId: "aaaa-444444", reason: "busy", blockedBy: [] },
        ]}
      />,
    );
    expect(screen.getByText("Còn giữ 4 bản ghi mẫu")).toBeInTheDocument();
    const first = screen.getByTestId("sample-kept-aaaa-111111");
    expect(first).toHaveTextContent("Khách hàng · …111111");
    expect(first).toHaveTextContent("Dữ liệu thật đang trỏ tới");
    expect(first).toHaveTextContent("Lịch hẹn › customer: 1 dòng");
    expect(screen.getByTestId("sample-kept-aaaa-222222")).toHaveTextContent("mặc định của trường Lịch hẹn › customer");
    expect(screen.getByTestId("sample-kept-aaaa-333333")).toHaveTextContent("Quá nhiều bản ghi");
    expect(screen.getByTestId("sample-kept-aaaa-444444")).toHaveTextContent("bấm xoá lại");
    const link = screen.getAllByRole("link")[0];
    expect(link).toHaveAttribute("href", "/customers/aaaa-111111");
    expect(link).not.toHaveAttribute("target");
  });
});
