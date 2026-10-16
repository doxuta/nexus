// Rule-builder tests: the Huỷ / Lưu bar of a saved rule shows only once the form holds a change (specs/074 FR-072),
// so a test that saves "untouched" first makes the smallest real edit — a trailing space in the rule name.
import { fireEvent, screen } from "@testing-library/react";

export async function touchRule() {
  const box = (await screen.findByRole("textbox", { name: /Tên luật/ }, { timeout: 3000 })) as HTMLInputElement;
  fireEvent.change(box, { target: { value: `${box.value} ` } });
}
