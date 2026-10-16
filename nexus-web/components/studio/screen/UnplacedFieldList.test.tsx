import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FieldMeta } from "@/lib/api";
import { UnplacedFieldList } from "./UnplacedFieldList";

const mk = (n: number): FieldMeta[] => Array.from({ length: n }, (_, i) => ({ name: `f${i}`, label: `Trường ${i}`, fieldType: "text", isRequired: i === 0 }) as FieldMeta);

describe("UnplacedFieldList (specs/075 T014)", () => {
  it("empty: says everything is placed", () => {
    render(<UnplacedFieldList fields={[]} onPlace={() => {}} />);
    expect(screen.getByText("Mọi trường đã nằm trên form.")).toBeInTheDocument();
  });

  it("rows carry type and required, with a count in the title; Đặt calls back", async () => {
    const onPlace = vi.fn();
    render(<UnplacedFieldList fields={mk(2)} onPlace={onPlace} />);
    expect(screen.getByRole("heading", { name: /Trường chưa đặt\s*2/ })).toBeInTheDocument();
    expect(screen.getByText("Văn bản · bắt buộc")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Đặt Trường 1 lên form" }));
    expect(onPlace).toHaveBeenCalledWith("f1");
  });

  it("search appears only above 8 fields and ignores accents", async () => {
    const { rerender } = render(<UnplacedFieldList fields={mk(8)} onPlace={() => {}} />);
    expect(screen.queryByRole("searchbox")).toBeNull();
    const nine = mk(9);
    nine[3] = { ...nine[3], label: "Điện thoại" } as FieldMeta;
    rerender(<UnplacedFieldList fields={nine} onPlace={() => {}} />);
    await userEvent.type(screen.getByRole("searchbox"), "dien thoai");
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByText("Điện thoại")).toBeInTheDocument();
  });

  it("an unplaced field opened by ?truong= is tinted", () => {
    render(<UnplacedFieldList fields={mk(2)} highlight="f1" onPlace={() => {}} />);
    expect(screen.getByText("Trường 1").closest("li")).toHaveClass("slds-theme_shade");
    expect(screen.getByText("Trường 0").closest("li")).not.toHaveClass("slds-theme_shade");
  });
});
