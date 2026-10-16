import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { Bell } from "lucide-react";
import type { ReactNode } from "react";
import { Avatar } from "./Avatar";
import { Badge } from "./Badge";
import { Card, RelatedListCard } from "./Card";
import { EmbeddedSurface, EmbeddedSurfaceReset } from "./EmbeddedSurface";
import { EmptyState } from "./EmptyState";
import { PageHeader } from "./PageHeader";
import { Panel } from "./Panel";
import { Tabs } from "./Tabs";

// Radix mints a fresh id per mount; ids are the only legitimate difference between two renders.
const norm = (html: string) => html.replace(/(?:radix-)?[«:]r[0-9a-z]+[»:]|radix-[^"]+/g, "ID");
const wrap = (ui: ReactNode) => <EmbeddedSurface>{ui}</EmbeddedSurface>;
const tabItems = [
  { value: "a", label: "Trường", content: <p>noi dung a</p> },
  { value: "b", label: "Quy tắc", content: <p>noi dung b</p> },
];

// Every wrapper that reads EmbeddedSurface, by name (spec FR-005: the list grows when ObjectHomePage joins).
const outside: Record<string, ReactNode> = {
  PageHeader: (
    <PageHeader
      title="Tiêu đề"
      icon={{ glyph: Bell, name: "account" }}
      meta="meta"
      actions={[<button key="1">Sửa</button>]}
      breadcrumbs={[{ label: "Gốc", href: "/" }]}
      details={[{ label: "Chủ", value: "An" }]}
    />
  ),
  Card: (
    <Card title="Thẻ" actions={<button>Thêm</button>} footer="chân">
      <span>thân</span>
    </Card>
  ),
  Panel: (
    <Panel open title="Chi tiết">
      <span>thân</span>
    </Panel>
  ),
  Tabs: <Tabs label="Mục" items={tabItems} />,
  Badge: (
    <Badge theme="success" iconLeft={Bell}>
      Đang dùng
    </Badge>
  ),
  Avatar: <Avatar name="Nguyễn An" shape="circle" />,
  EmptyState: <EmptyState title="Chưa có" description="Thêm một cái" action={<button>Thêm</button>} />,
};

describe("outside EmbeddedSurface nothing changes", () => {
  it.each(Object.keys(outside))("%s: same DOM under EmbeddedSurfaceReset (customer face)", (name) => {
    const plain = norm(render(<div>{outside[name]}</div>).container.innerHTML);
    const reset = norm(render(
      <EmbeddedSurface>
        <EmbeddedSurfaceReset>
          <div>{outside[name]}</div>
        </EmbeddedSurfaceReset>
      </EmbeddedSurface>,
    ).container.innerHTML);
    expect(reset).toBe(plain);
  });

  it("EmbeddedSurface value={false} is a no-op", () => {
    const plain = render(<div>{outside.Card}</div>).container.innerHTML;
    expect(
      render(
        <EmbeddedSurface value={false}>
          <div>{outside.Card}</div>
        </EmbeddedSurface>,
      ).container.innerHTML,
    ).toBe(plain);
  });

  it("keeps the customer markup (guards the comparison above)", () => {
    const { container } = render(
      <div>
        {outside.PageHeader}
        {outside.Card}
        {outside.Tabs}
        {outside.Badge}
        {outside.Avatar}
        {outside.EmptyState}
      </div>,
    );
    for (const sel of ["h1", ".slds-page-header", ".slds-card", "[role=tablist]", ".slds-badge", ".slds-avatar", ".slds-illustration"]) {
      expect(container.querySelector(sel), sel).not.toBeNull();
    }
  });
});

describe("inside EmbeddedSurface", () => {
  it("PageHeader: flat H2, no H1/page-header/icon, meta + actions on the right, details row, no breadcrumbs", () => {
    const { container } = render(wrap(outside.PageHeader));
    expect(container.querySelector("h1")).toBeNull();
    expect(container.querySelector(".slds-page-header")).toBeNull();
    expect(container.querySelector("[class*=slds-icon-standard]")).toBeNull();
    expect(container.querySelector("h2")?.textContent).toBe("Tiêu đề");
    expect(container.textContent).toContain("meta");
    expect(container.querySelector("button")?.textContent).toBe("Sửa");
    expect(container.querySelector("header > div")?.className).toContain("slds-border_bottom");
    expect(container.textContent).toContain("Chủ");
    expect(container.textContent).toContain("An");
    expect(container.querySelector("nav")).toBeNull();
    expect(container.textContent).not.toContain("Gốc");
  });

  it("Card: flat section with a rule on top, header + actions; nested cards are flat too", () => {
    const { container } = render(
      wrap(
        <Card title="Ngoài" count={2} actions={<button>Thêm</button>}>
          <Card title="Trong">
            <i>x</i>
          </Card>
        </Card>,
      ),
    );
    expect(container.querySelector(".slds-card")).toBeNull();
    expect(container.querySelectorAll("section.slds-border_top")).toHaveLength(2);
    expect(container.querySelector("h2.slds-card__header-title")).toBeNull();
    expect(container.querySelector("h3")?.textContent).toBe("Ngoài (2)");
    expect(container.querySelector("button")?.textContent).toBe("Thêm");
    expect(container.querySelector("i")).not.toBeNull();
  });

  it("Card: loading shows a status spinner; hideHeader keeps the title for screen readers", () => {
    const a = render(wrap(<Card title="T" loading />)).container;
    expect(a.querySelector("[role=status]")).not.toBeNull();
    const b = render(wrap(<Card title="T" hideHeader>c</Card>)).container;
    expect(b.querySelector("h3")?.parentElement?.className).toContain("slds-assistive-text");
  });

  it("RelatedListCard keeps its table, flat", () => {
    const { container } = render(
      wrap(<RelatedListCard title="LQ" columns={[{ key: "n", label: "Tên" }]} rows={[{ n: "a" }]} rowKey={(r) => String(r.n)} />),
    );
    expect(container.querySelector(".slds-card")).toBeNull();
    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
  });

  it("Panel: 300px column inside the surface, not docked to the edge; open state and close kept", () => {
    const { container } = render(
      wrap(
        <Panel open title="Chi tiết" onClose={() => {}}>
          <span>thân</span>
        </Panel>,
      ),
    );
    const panel = container.querySelector(".slds-panel") as HTMLElement;
    expect(panel.className).toContain("slds-is-open");
    expect(panel.className).toContain("w-[300px]");
    expect(panel.className).not.toMatch(/slds-size_|slds-panel_docked/);
    expect(panel.className).toContain("slds-border_left");
    expect(container.querySelector(".slds-panel__close")).not.toBeNull();
    expect(container.textContent).toContain("thân");
  });

  it("Panel: closed panel inside the surface is hidden", () => {
    const { container } = render(wrap(<Panel open={false} title="Chi tiết">x</Panel>));
    expect((container.querySelector(".slds-panel") as HTMLElement).className).toContain("hidden");
  });

  it("Panel: narrow + responsive + open keeps the docked overlay look and role=dialog", () => {
    const prev = window.matchMedia;
    window.matchMedia = ((query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    })) as unknown as typeof window.matchMedia;
    try {
      const { container } = render(
        wrap(
          <Panel open responsive title="Chi tiết">
            x
          </Panel>,
        ),
      );
      const panel = container.querySelector(".slds-panel") as HTMLElement;
      expect(panel.getAttribute("role")).toBe("dialog");
      expect(panel.className).toContain("slds-panel_docked");
      expect(panel.className).not.toContain("w-[300px]");
    } finally {
      window.matchMedia = prev;
    }
  });

  it("Tabs: item.error marker stays on the section heading", () => {
    const { container } = render(
      wrap(<Tabs label="Mục" items={[{ value: "a", label: "Trường", error: "Có lỗi", content: <p>x</p> }]} />),
    );
    expect(container.querySelector("section > h3")?.textContent).toContain("Có lỗi");
  });

  it("Tabs: no tablist; each item is a scrolled section with a heading, every panel mounted", () => {
    const { container } = render(wrap(<Tabs label="Mục" items={tabItems} />));
    expect(container.querySelector("[role=tablist]")).toBeNull();
    expect(container.querySelector(".slds-tabs_default, .slds-tabs_scoped")).toBeNull();
    const heads = [...container.querySelectorAll("section.slds-border_top > h3")].map((h) => h.textContent);
    expect(heads).toEqual(["Trường", "Quy tắc"]);
    expect(container.textContent).toContain("noi dung a");
    expect(container.textContent).toContain("noi dung b");
  });

  it("Tabs link mode is navigation, not flattened", () => {
    const { container } = render(wrap(<Tabs mode="links" label="Khu" current="a" items={[{ value: "a", label: "A", href: "/a" }]} />));
    expect(container.querySelector("nav a[aria-current=page]")).not.toBeNull();
  });

  it("Badge: nx-status text, no icon; theme -> tone; no theme -> weak; text kept", () => {
    const tone = (ui: ReactNode) => render(wrap(ui)).container.querySelector("span.nx-status") as HTMLElement;
    const ok = tone(
      <Badge theme="success" iconLeft={Bell}>
        Đang dùng
      </Badge>,
    );
    expect(ok.className).toContain("nx-status_success");
    expect(ok.textContent).toBe("Đang dùng");
    expect(ok.querySelector("svg")).toBeNull();
    expect(tone(<Badge theme="warning">w</Badge>).className).toContain("nx-status_warn");
    expect(tone(<Badge theme="error">e</Badge>).className).toContain("nx-status_error");
    expect(tone(<Badge>n</Badge>).className).toContain("nx-status_weak");
    expect(render(wrap(<Badge>n</Badge>)).container.querySelector(".slds-badge")).toBeNull();
  });

  it("Avatar draws nothing", () => {
    expect(render(wrap(<Avatar name="Nguyễn An" />)).container.innerHTML).toBe("");
  });

  it("EmptyState: compact, left aligned, no illustration/glyph, h3 small, action kept", () => {
    const { container } = render(wrap(outside.EmptyState));
    expect(container.querySelector(".slds-illustration")).toBeNull();
    expect(container.querySelector("svg")).toBeNull();
    expect(container.querySelector("h3")?.className).toContain("slds-text-heading_small");
    expect(container.querySelector("h3")?.className).not.toContain("heading_medium");
    expect(container.textContent).toContain("Thêm một cái");
    expect(container.querySelector("button")).not.toBeNull();
  });

  it("EmbeddedSurfaceReset: wrappers inside render customer markup again", () => {
    const { container } = render(
      wrap(
        <EmbeddedSurfaceReset>
          {outside.Card}
          {outside.Badge}
          {outside.Avatar}
          {outside.EmptyState}
          {outside.Tabs}
          {outside.PageHeader}
        </EmbeddedSurfaceReset>,
      ),
    );
    for (const sel of [".slds-card", ".slds-badge", ".slds-avatar", ".slds-illustration", "[role=tablist]", ".slds-page-header", "h1"]) {
      expect(container.querySelector(sel), sel).not.toBeNull();
    }
  });
});

// specs/073 T099: a canvas builder (xyflow, three columns) assumes a shell as tall as the viewport, but the work
// surface scrolls with the page, so its `h-full` canvas collapsed to 0 (measured in Chrome). The shell gives such a
// builder a floor: the viewport minus the sticky stack, as a flex column the builder's root can fill.
describe("EmbeddedSurface canvas", () => {
  it("canvas: a flex column with min-height = viewport minus the sticky stack; children inside it", () => {
    const { container } = render(
      <EmbeddedSurface canvas>
        <span data-testid="kid">x</span>
      </EmbeddedSurface>,
    );
    const shell = container.firstElementChild as HTMLElement;
    expect(shell.style.minHeight).toBe("calc(100dvh - var(--nx-sticky-stack, 0px))");
    expect(shell.className).toContain("flex-col");
    expect(shell.querySelector("[data-testid=kid]")).not.toBeNull();
  });

  it("default: no wrapper element at all (children are the container's own children)", () => {
    const { container } = render(
      <EmbeddedSurface>
        <span data-testid="kid">x</span>
      </EmbeddedSurface>,
    );
    expect(container.firstElementChild?.getAttribute("data-testid")).toBe("kid");
  });

  it("canvas still switches the wrappers to the embedded form", () => {
    const { container } = render(<EmbeddedSurface canvas>{outside.Badge}</EmbeddedSurface>);
    expect(container.querySelector(".slds-badge")).toBeNull();
  });
});
