// #276: typing in a number counter must never be rewritten mid-keystroke
// (min 5: "3" → 5, then "0" → 50 saved wrong durations).
import { useState } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { FieldInput } from "@/components/fields/widgets";
import type { FieldSpec } from "@/lib/metadata";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, ensureAuth: vi.fn().mockResolvedValue(undefined) };
});
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: [] }) }));

const field = { name: "thoi_luong", label: "Thời lượng", fieldType: "number", options: { min: 5, max: 480 } } as unknown as FieldSpec;

function Harness({ init = "" as unknown, onValue }: { init?: unknown; onValue: (v: unknown) => void }) {
  const [v, setV] = useState<unknown>(init);
  return <FieldInput field={field} value={v} onChange={(x) => { setV(x); onValue(x); }} />;
}
const box = () => screen.getByRole("textbox", { name: /Thời lượng/ }) as HTMLInputElement;

describe("NumberCounter — gõ tự do, chỉ ép khi rời ô (#276)", () => {
  it("gõ '3' rồi '0' ra 30, không bị nhảy 5/50", () => {
    const seen = vi.fn();
    render(<Harness onValue={seen} />);
    fireEvent.change(box(), { target: { value: "3" } });
    expect(box().value).toBe("3");
    fireEvent.change(box(), { target: { value: "30" } });
    expect(box().value).toBe("30");
    expect(seen).toHaveBeenLastCalledWith(30);
  });

  it("xoá hết được (rỗng, không thành 0)", () => {
    const seen = vi.fn();
    render(<Harness init={30} onValue={seen} />);
    fireEvent.change(box(), { target: { value: "" } });
    expect(box().value).toBe("");
    expect(seen).toHaveBeenLastCalledWith("");
  });

  it("nhập 999 rồi rời ô ⇒ ép về 480; nhập 2 rời ô ⇒ 5", () => {
    render(<Harness onValue={vi.fn()} />);
    fireEvent.change(box(), { target: { value: "999" } });
    expect(box().value).toBe("999");
    fireEvent.blur(box());
    expect(box().value).toBe("480");
    fireEvent.change(box(), { target: { value: "2" } });
    fireEvent.blur(box());
    expect(box().value).toBe("5");
  });
});
