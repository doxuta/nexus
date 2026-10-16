import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Card } from "@/components/slds/Card";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { PageHeader } from "@/components/slds/PageHeader";
import { Tabs } from "@/components/slds/Tabs";
import { CustomerPreviewFrame } from "./CustomerPreviewFrame";

const frame = () => screen.getByLabelText("Xem trước đúng màn khách thấy");

describe("CustomerPreviewFrame", () => {
  it("carries nx-face-customer and renders real SLDS children", () => {
    render(<CustomerPreviewFrame appName="Khách A"><Card title="Thẻ"><Badge>b</Badge></Card></CustomerPreviewFrame>);
    expect(frame().className).toContain("nx-face-customer");
    expect(frame().querySelector(".slds-card")).not.toBeNull();
    expect(frame().querySelector(".slds-badge")).not.toBeNull();
  });

  it("empty accent falls back to #0176d3 as an inline brand scope (never {})", () => {
    render(<CustomerPreviewFrame appName="A" accentColor="">x</CustomerPreviewFrame>);
    expect(frame().style.getPropertyValue("--slds-r-color-brand-50")).toBe("#0176d3");
  });

  it("malformed accent also falls back", () => {
    render(<CustomerPreviewFrame appName="A" accentColor="red">x</CustomerPreviewFrame>);
    expect(frame().style.getPropertyValue("--slds-r-color-brand-50")).toBe("#0176d3");
  });

  it("contains position:fixed children (the form template's docked footer stays inside the frame)", () => {
    render(<CustomerPreviewFrame appName="A"><p>x</p></CustomerPreviewFrame>);
    expect(frame().style.contain).toBe("layout");
  });

  it("customer accent is scoped to the frame only", () => {
    render(<CustomerPreviewFrame appName="A" accentColor="#aa2200">x</CustomerPreviewFrame>);
    expect(frame().style.getPropertyValue("--slds-r-color-brand-50")).toBe("#aa2200");
    expect(document.documentElement.style.getPropertyValue("--slds-r-color-brand-50")).toBe("");
  });

  it("renders the customer app bar: name, caption, allowlisted logo only", () => {
    const { container, rerender } = render(<CustomerPreviewFrame appName="Khách A" caption="Bản xem" logoUrl="https://x.test/l.png">x</CustomerPreviewFrame>);
    expect(screen.getByText("Khách A")).toBeTruthy();
    expect(screen.getByText("Bản xem")).toBeTruthy();
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://x.test/l.png");
    rerender(<CustomerPreviewFrame appName="Khách A" logoUrl="javascript:alert(1)">x</CustomerPreviewFrame>);
    expect(container.querySelector("img")).toBeNull();
  });

  it("stays customer-face markup when placed inside EmbeddedSurface", () => {
    const { container } = render(
      <EmbeddedSurface>
        <CustomerPreviewFrame appName="Khách A">
          <PageHeader title="Hồ sơ" />
          <Card title="Thẻ"><Badge theme="success">Đang dùng</Badge><Avatar name="An Bình" /></Card>
          <Tabs label="Mục" items={[{ value: "a", label: "Chi tiết", content: "x" }]} />
          <EmptyState title="Trống" />
        </CustomerPreviewFrame>
      </EmbeddedSurface>,
    );
    const f = frame();
    expect(f.className).toContain("nx-face-customer");
    for (const sel of [".slds-card", "[role=tablist]", ".slds-badge", ".slds-avatar", ".slds-illustration", ".slds-page-header", "h1"]) {
      expect(f.querySelector(sel), sel).not.toBeNull();
    }
    expect(container.querySelector(".nx-status")).toBeNull();
  });
});
