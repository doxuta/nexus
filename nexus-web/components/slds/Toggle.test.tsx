import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useForm } from "react-hook-form";
import { Toggle } from "./Toggle";

// The label wraps the on/off text too, so without SLDS CSS (which hides the inactive one) the
// computed name is "<label> Bật Tắt" — match the label as a prefix.
describe("Toggle (blueprint checkbox-toggle)", () => {
  it("renders label.slds-checkbox_toggle with role=switch described by the on/off text", () => {
    const { container } = render(<Toggle label="Nhận thông báo" />);
    const sw = screen.getByRole("switch", { name: /^Nhận thông báo/ });
    const label = sw.parentElement!;
    expect(label.tagName).toBe("LABEL");
    expect(label).toHaveClass("slds-checkbox_toggle", "slds-grid");
    expect(label.firstElementChild).toHaveClass("slds-form-element__label", "slds-m-bottom_none");
    const faux = container.querySelector(".slds-checkbox_faux_container")!;
    expect(sw).toHaveAttribute("aria-describedby", faux.id);
    expect(faux.querySelector(".slds-checkbox_on")).toHaveTextContent("Bật");
    expect(faux.querySelector(".slds-checkbox_off")).toHaveTextContent("Tắt");
    expect(container.firstElementChild).toHaveClass("slds-form-element");
  });

  it("help text sits under the switch and joins its description (on/off text + help)", () => {
    const { container } = render(<Toggle label="Mặc định" help="Bật ở đây sẽ bỏ cờ ở lịch cũ." />);
    const sw = screen.getByRole("switch", { name: /^Mặc định/ });
    const help = container.querySelector(".slds-form-element__help")!;
    expect(help).toHaveTextContent("Bật ở đây sẽ bỏ cờ ở lịch cũ.");
    expect(help.parentElement).toBe(container.firstElementChild);
    const faux = container.querySelector(".slds-checkbox_faux_container")!;
    expect(sw.getAttribute("aria-describedby")?.split(" ")).toEqual([faux.id, help.id]);
    expect(sw).toHaveAccessibleDescription(/Bật ở đây sẽ bỏ cờ ở lịch cũ\.$/);
  });

  it("Space flips it; onChange sees checked", async () => {
    const onChange = vi.fn();
    render(<Toggle label="Tự động gán" onChange={(e) => onChange(e.target.checked)} />);
    const sw = screen.getByRole("switch", { name: /^Tự động gán/ });
    sw.focus();
    await userEvent.keyboard(" ");
    expect(sw).toBeChecked();
    expect(onChange).toHaveBeenLastCalledWith(true);
  });

  it("custom on/off text, disabled", () => {
    render(<Toggle label="Trạng thái" onText="Hoạt động" offText="Ngừng" disabled defaultChecked />);
    const sw = screen.getByRole("switch", { name: /^Trạng thái/ });
    expect(sw).toBeDisabled();
    expect(sw).toBeChecked();
    expect(screen.getByText("Hoạt động")).toHaveClass("slds-checkbox_on");
  });

  it("works with react-hook-form register", async () => {
    const onSubmit = vi.fn();
    function Form() {
      const { register, handleSubmit } = useForm({ defaultValues: { active: true } });
      return (
        <form onSubmit={handleSubmit(onSubmit)}>
          <Toggle label="Kích hoạt" {...register("active")} />
          <button type="submit">Lưu</button>
        </form>
      );
    }
    render(<Form />);
    const sw = screen.getByRole("switch", { name: /^Kích hoạt/ });
    expect(sw).toBeChecked();
    await userEvent.click(sw);
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledOnce());
    expect(onSubmit.mock.calls[0][0]).toEqual({ active: false });
  });
});
