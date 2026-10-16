// specs/073 so1a T452 (FR-091): the eight-segment build meter — a picture with a spoken label, never a number.
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { projectRow } from "@/app/dev/kit/sections/projectRows";
import { buildSteps } from "@/lib/buildSteps";
import { BuildMeter } from "./BuildMeter";

const steps = (entities: number, branded = false) =>
  buildSteps(projectRow({ branded, counts: { ...projectRow().counts, entities } }), { home: false });

describe("BuildMeter", () => {
  it("is one image whose label counts the enough steps out of eight", () => {
    render(<BuildMeter steps={steps(2, true)} />);
    // data + branding are enough; the optional screens/automation steps are not
    expect(screen.getByRole("img", { name: "2 trên 8 bước dựng đã đủ" })).toBeInTheDocument();
  });

  it("draws eight segments and fills exactly the enough ones", () => {
    const { container } = render(<BuildMeter steps={steps(2, true)} />);
    const segs = container.querySelectorAll("[data-segment]");
    expect(segs).toHaveLength(8);
    expect([...segs].map((s) => s.getAttribute("data-segment"))).toEqual(["done", "idle", "idle", "idle", "idle", "done", "idle", "idle"]);
  });

  it("prints no number and nothing readable apart from the label", () => {
    const { container } = render(<BuildMeter steps={steps(2, true)} />);
    expect(container.textContent).toBe("");
  });

  it("nothing known yet (a step is unknown) counts as not enough, never as enough", () => {
    render(<BuildMeter steps={buildSteps(null, { home: false })} />);
    expect(screen.getByRole("img", { name: "0 trên 8 bước dựng đã đủ" })).toBeInTheDocument();
  });
});
