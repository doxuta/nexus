// QuickCreateModal — STUDIO-DISCOVERABILITY 1.4c contract: the `prefill` prop must reach the
// LayoutForm untouched (that's how a related-list create pre-links the parent FK).
// LayoutForm is mocked at the module seam: the dialog itself is LayoutForm's `modal` mode
// (RecordFormPage modal variant — covered in LayoutForm.test.tsx, Esc/popup layering included);
// the prop wiring here is real code.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QuickCreateModal } from "./QuickCreateModal";

vi.mock("@/components/layout/LayoutForm", () => ({
  LayoutForm: (props: { entity: string; modal?: boolean; prefill?: Record<string, unknown>; onCancel?: () => void; onSaved?: (id: string) => void }) => (
    <div data-testid="layout-form" data-entity={props.entity} data-modal={String(!!props.modal)} data-prefill={JSON.stringify(props.prefill ?? null)}>
      <button type="button" onClick={props.onCancel}>cancel</button>
      <button type="button" onClick={() => props.onSaved?.("r-9")}>saved</button>
    </div>
  ),
}));

vi.mock("@/lib/metadata/hooks", () => ({
  useEntityLabel: (entity: string) => entity,
}));

describe("QuickCreateModal — prefill pass-through", () => {
  it("passes prefill to the embedded LayoutForm", () => {
    render(
      <QuickCreateModal
        entity="orders"
        open
        onClose={() => {}}
        prefill={{ customer: "p1" }}
      />,
    );

    const form = screen.getByTestId("layout-form");
    expect(form).toHaveAttribute("data-modal", "true");
    expect(form).toHaveAttribute("data-entity", "orders");
    expect(form).toHaveAttribute("data-prefill", JSON.stringify({ customer: "p1" }));
  });

  it("renders nothing while closed", () => {
    render(
      <QuickCreateModal entity="orders" open={false} onClose={() => {}} prefill={{ customer: "p1" }} />,
    );
    expect(screen.queryByTestId("layout-form")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("the form's cancel closes; a save reports the id, then closes", () => {
    const onClose = vi.fn();
    const onSaved = vi.fn();
    render(<QuickCreateModal entity="orders" open onClose={onClose} onSaved={onSaved} />);
    fireEvent.click(screen.getByRole("button", { name: "cancel" }));
    expect(onClose).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole("button", { name: "saved" }));
    expect(onSaved).toHaveBeenCalledWith("r-9");
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
