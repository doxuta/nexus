// specs/078 lát u7-muc-dung-bieu-do T170: DailyChart — flat daily series, end-point label, keyboard tooltip, table twin.
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { DailyChart } from "./DailyChart";

const days = ["2026-10-10", "2026-10-11", "2026-10-12", "2026-10-13", "2026-10-14"];
const points = days.map((day, i) => ({ day, values: { ok: [3, 0, 5, 2, 7][i], bad: [0, 0, 1, 0, 2][i] } }));
const base = { points, formatDay: (d: string) => d.split("-").reverse().join("/") };
const one = [{ key: "ok", label: "Thành công", tone: "normal" as const }];
const two = [...one, { key: "bad", label: "Lỗi", tone: "error" as const }];

describe("DailyChart", () => {
  it("labels only the last value, '{n} hôm nay', and the x axis only the first day and 'Hôm nay'", () => {
    const { container } = render(<DailyChart {...base} series={one} />);
    expect(screen.getByText("7 hôm nay")).toBeInTheDocument();
    const axis = container.querySelector("[data-axis]") as HTMLElement;
    expect(axis.querySelectorAll("[data-axis-label]")).toHaveLength(2);
    expect(within(axis).getByText("10/10/2026")).toBeInTheDocument();
    expect(within(axis).getByText("Hôm nay")).toBeInTheDocument();
    expect(container.querySelectorAll("[data-end-label]")).toHaveLength(1);
  });
  it("one series: no legend; two series: legend, the failing one has icon + word, not only colour", () => {
    const { rerender } = render(<DailyChart {...base} series={one} />);
    expect(screen.queryByRole("list", { name: "Chú giải" })).toBeNull();
    rerender(<DailyChart {...base} series={two} />);
    const legend = screen.getByRole("list", { name: "Chú giải" });
    expect(within(legend).getByText("Lỗi")).toBeInTheDocument();
    expect(legend.querySelector("svg[data-tone-icon='error']")).not.toBeNull();
  });
  it("'Xem dạng bảng' toggles a table with exactly the chart data (empty day = 0)", async () => {
    render(<DailyChart {...base} series={two} />);
    const btn = screen.getByRole("button", { name: "Xem dạng bảng" });
    expect(btn).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(btn);
    expect(screen.getByRole("button", { name: /Ẩn bảng/ })).toHaveAttribute("aria-expanded", "true");
    const rows = screen.getAllByRole("row");
    expect(rows).toHaveLength(1 + points.length);
    expect(within(rows[2]).getAllByRole("cell").map((c) => c.textContent)).toEqual(["11/10/2026", "0", "0"]);
    expect(within(rows[5]).getAllByRole("cell").map((c) => c.textContent)).toEqual(["14/10/2026", "7", "2"]);
  });
  it("keyboard: focus the plot, arrows move the crosshair and the tooltip names the day and every value", async () => {
    render(<DailyChart {...base} series={two} />);
    const plot = screen.getByRole("group", { name: /Biểu đồ/ });
    plot.focus();
    expect(plot).toHaveFocus();
    expect(screen.queryByRole("tooltip")).toBeNull();
    await userEvent.keyboard("{ArrowLeft}");
    expect(screen.getByRole("tooltip")).toHaveTextContent("13/10/2026");
    await userEvent.keyboard("{ArrowLeft}");
    expect(screen.getByRole("tooltip")).toHaveTextContent("12/10/2026");
    expect(screen.getByRole("tooltip")).toHaveTextContent("Thành công: 5");
    expect(screen.getByRole("tooltip")).toHaveTextContent("Lỗi: 1");
    await userEvent.keyboard("{Home}");
    expect(screen.getByRole("tooltip")).toHaveTextContent("10/10/2026");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).toBeNull();
  });
  it("one axis only: no second y axis", () => {
    const { container } = render(<DailyChart {...base} series={two} />);
    expect(container.querySelectorAll("[data-y-axis]").length).toBeLessThanOrEqual(1);
  });
});
