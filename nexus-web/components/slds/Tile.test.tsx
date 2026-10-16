// Tile — blueprint tiles/base (SLDS 2.264.1): article.slds-tile > h3.slds-tile__title + div.slds-tile__detail.
import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Tile } from "./Tile";

describe("Tile", () => {
  it("renders the base blueprint markup", () => {
    const { container } = render(<Tile title="Lịch hẹn hôm nay">12</Tile>);
    const el = container.firstElementChild!;
    expect(el.tagName).toBe("ARTICLE");
    expect(el).toHaveClass("slds-tile");
    expect(el.children[0].tagName).toBe("H3");
    expect(el.children[0]).toHaveClass("slds-tile__title", "slds-truncate");
    expect(el.children[0]).toHaveAttribute("title", "Lịch hẹn hôm nay");
    expect(el.children[1]).toHaveClass("slds-tile__detail");
    expect(el.children[1]).toHaveTextContent("12");
  });

  it("omits the detail block when there are no children", () => {
    const { container } = render(<Tile title="Trống" />);
    expect(container.querySelector(".slds-tile__detail")).toBeNull();
  });
});
