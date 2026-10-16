// FileSelector — blueprint `file-selector` base / image / integrated (SLDS 2.264.1).
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FileSelector } from "@/components/slds/FileSelector";

const png = () => new File(["x"], "logo.png", { type: "image/png" });
const drag = (files: File[]) => ({ dataTransfer: { types: ["Files"], files, dropEffect: "none" } });

describe("FileSelector", () => {
  it("files: group labelled by the form label; hidden native input + label body with faux button and drop text", () => {
    render(<FileSelector label="Tệp đính kèm" accept="image/png" onFiles={() => {}} />);
    const group = screen.getByRole("group", { name: "Tệp đính kèm" });
    expect(group).toHaveClass("slds-form-element");
    const selector = group.querySelector(".slds-file-selector")!;
    expect(selector).toHaveClass("slds-file-selector_files");
    const input = selector.querySelector("input[type=file]")!;
    expect(input).toHaveClass("slds-file-selector__input", "slds-assistive-text");
    expect(input).toHaveAttribute("accept", "image/png");
    const body = selector.querySelector("label.slds-file-selector__body")!;
    expect(body).toHaveAttribute("for", input.id);
    expect(body.querySelector(".slds-file-selector__button")).toHaveClass("slds-button", "slds-button_neutral");
    expect(body.querySelector(".slds-file-selector__button")).toHaveTextContent("Tải tệp lên");
    expect(body.querySelector(".slds-file-selector__text")).toHaveClass("slds-medium-show");
  });

  it("choosing through the native input hands the files over", async () => {
    const onFiles = vi.fn();
    render(<FileSelector label="Tệp đính kèm" multiple onFiles={onFiles} />);
    const input = document.querySelector<HTMLInputElement>("input[type=file]")!;
    await userEvent.upload(input, [png()]);
    expect(onFiles).toHaveBeenCalledWith([expect.objectContaining({ name: "logo.png" })]);
  });

  it("drag over lights the zone (slds-has-drag-over); drop hands the files over and clears it", () => {
    const onFiles = vi.fn();
    render(<FileSelector label="Tệp đính kèm" onFiles={onFiles} />);
    const zone = document.querySelector(".slds-file-selector__dropzone")!;
    const file = png();
    fireEvent.dragEnter(zone, drag([file]));
    expect(zone).toHaveClass("slds-has-drag-over");
    fireEvent.drop(zone, drag([file]));
    expect(zone).not.toHaveClass("slds-has-drag-over");
    expect(onFiles).toHaveBeenCalledWith([file]);
  });

  it("error: slds-has-error + help line describing the input; disabled ignores drops", () => {
    const onFiles = vi.fn();
    render(<FileSelector label="Tệp đính kèm" error="Không hỗ trợ kiểu tệp này" disabled onFiles={onFiles} />);
    const group = screen.getByRole("group");
    expect(group).toHaveClass("slds-has-error");
    const input = group.querySelector("input")!;
    expect(input).toBeDisabled();
    expect(document.getElementById(input.getAttribute("aria-describedby")!)).toHaveTextContent("Không hỗ trợ kiểu tệp này");
    const zone = group.querySelector(".slds-file-selector__dropzone")!;
    fireEvent.dragEnter(zone, drag([png()]));
    expect(zone).not.toHaveClass("slds-has-drag-over");
    fireEvent.drop(zone, drag([png()]));
    expect(onFiles).not.toHaveBeenCalled();
  });

  it("images variant uses the image zone and image wording", () => {
    render(<FileSelector label="Logo" variant="images" onFiles={() => {}} />);
    expect(document.querySelector(".slds-file-selector")).toHaveClass("slds-file-selector_images");
    expect(screen.getByText("Tải ảnh lên")).toHaveClass("slds-file-selector__button");
  });

  it("integrated: aria-hidden overlay over the content; slds-has-drag + slds-has-drag-over while dragging", () => {
    const onFiles = vi.fn();
    render(
      <FileSelector variant="integrated" onFiles={onFiles}>
        <p>Nội dung ghi chú</p>
      </FileSelector>,
    );
    const root = screen.getByText("Nội dung ghi chú").parentElement!;
    expect(root).toHaveClass("slds-file-selector", "slds-file-selector_integrated");
    const zone = root.querySelector(".slds-file-selector__dropzone_integrated")!;
    expect(zone).toHaveAttribute("aria-hidden", "true");
    expect(zone.querySelector("input")).toHaveAttribute("tabindex", "-1");
    fireEvent.dragEnter(root, drag([png()]));
    expect(zone).toHaveClass("slds-has-drag", "slds-has-drag-over");
    fireEvent.dragLeave(root);
    expect(zone).not.toHaveClass("slds-has-drag");
  });
});
