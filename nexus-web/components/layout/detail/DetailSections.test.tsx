// DetailSections — per-view help text on the record page (form-element/record-detail, SLDS 2.264.1):
// the blueprint puts help in `slds-form-element__icon` as an info button with a tooltip, never as a
// help line under the value. The printed document (collapsible={false}) is sent to customers, so the
// maker's hint ("VD: 0912…") must not print at all.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta, LayoutSection } from "@/lib/api";
import { DetailSections } from "./DetailSections";

const FIELDS: Record<string, FieldMeta> = {
  phone: { name: "phone", label: "Điện thoại", fieldType: "text" },
  name: { name: "name", label: "Tên", fieldType: "text" },
};
const SECTIONS: LayoutSection[] = [
  { title: "Liên hệ", fields: [{ field: "phone", helpText: "VD: 0912 345 678" }, "name"] },
];
const VALUES = { phone: "0987 654 321", name: "An Phát" };

describe("DetailSections — help text", () => {
  it("on screen: an info button in the label's icon slot, the text in its tooltip", async () => {
    const { container } = render(<DetailSections sections={SECTIONS} fieldsByName={FIELDS} values={VALUES} />);
    expect(container.querySelector(".slds-form-element__help")).toBeNull();
    expect(screen.queryByText("VD: 0912 345 678")).toBeNull();

    const help = screen.getByRole("button", { name: "Trợ giúp: Điện thoại" });
    expect(help.closest(".slds-form-element__icon")).not.toBeNull();
    // Only the field that has help text gets the button.
    expect(screen.getAllByRole("button", { name: /^Trợ giúp/ })).toHaveLength(1);

    await userEvent.tab(); // section toggle
    await userEvent.tab();
    expect(help).toHaveFocus();
    expect(await screen.findByRole("tooltip")).toHaveTextContent("VD: 0912 345 678");
  });

  it("printed document: no help at all", () => {
    const { container } = render(
      <DetailSections sections={SECTIONS} fieldsByName={FIELDS} values={VALUES} collapsible={false} />,
    );
    expect(screen.queryByText(/VD: 0912/)).toBeNull();
    expect(container.querySelector(".slds-form-element__icon")).toBeNull();
    expect(screen.getByText("0987 654 321")).toBeInTheDocument();
  });
});
