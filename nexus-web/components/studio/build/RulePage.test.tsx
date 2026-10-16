// specs/074 lát f2-rule-page (T094): the rule page hands the URL to the rule builder as props.
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { WorkflowBuilderProps } from "@/components/studio/WorkflowBuilder";

const replace = vi.fn();
vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/northwind/dung/tu-dong/luat/wf-1",
  useRouter: () => ({ replace, push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  appsCatalog: vi.fn(async () => []),
}));
let props: WorkflowBuilderProps = {};
vi.mock("@/components/studio/WorkflowBuilder", () => ({
  WorkflowBuilder: (p: WorkflowBuilderProps) => {
    props = p;
    return <p>trình dựng luật</p>;
  },
}));

import { RulePage } from "./RulePage";

const wrap = (ui: React.ReactElement) => render(<QueryClientProvider client={new QueryClient()}>{ui}</QueryClientProvider>);

beforeEach(() => {
  replace.mockReset();
  props = {};
});

describe("RulePage", () => {
  it("luat/<id> opens that rule, only the rule (no list), with the way back to Tự động", () => {
    wrap(<RulePage slug="northwind" id="wf-1" app="ticketing" />);
    expect(props.initialRuleId).toBe("wf-1");
    expect(props.hideList).toBe(true);
    expect(props.newDraft).toBeUndefined();
    expect(screen.getByRole("link", { name: "← Tự động" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/tu-dong?app=ticketing");
  });

  it("luat/moi?bang=&khi=&ten= opens a new draft", () => {
    wrap(<RulePage slug="northwind" bang="tk_tickets" khi="after_update" ten="Nhắc việc" />);
    expect(props.initialRuleId).toBeUndefined();
    expect(props.newDraft).toEqual({ entity: "tk_tickets", trigger: "after_update", name: "Nhắc việc" });
  });

  it("onSelectRule writes /<id> onto the URL; onDeleted goes back to the list", () => {
    wrap(<RulePage slug="northwind" bang="tk_tickets" />);
    props.onSelectRule?.("wf-9");
    expect(replace).toHaveBeenCalledWith("/studio/du-an/northwind/dung/tu-dong/luat/wf-9", { scroll: false });
    props.onDeleted?.();
    expect(replace).toHaveBeenLastCalledWith("/studio/du-an/northwind/dung/tu-dong", undefined);
  });
});

// specs/074 lát f2-ai-preview-data-rules (T400, T463)
import { WithGhost, mkChange } from "@/components/studio/assistant/ghostTestKit";

describe("RulePage: assistant ghost", () => {
  const upd = mkChange({
    docType: "workflow", docKey: "wf-1", op: "update", label: "Đổi luật Nhắc lịch", target: { docType: "workflow", name: "x" },
    before: { trigger: "after_create", actions: [{ type: "email_alert", to: "a@b.vn" }] },
    after: { trigger: "after_update", actions: [{ type: "email_alert", to: "a@b.vn" }] },
  });
  it("update: Trước and Sau side by side with 'Đề xuất', no control in the ghost; the editor still mounts", () => {
    wrap(<WithGhost change={upd}><RulePage slug="northwind" id="wf-1" /></WithGhost>);
    const g = screen.getByRole("group", { name: "Đề xuất của trợ lý: Đổi luật Nhắc lịch" });
    expect(g).toHaveTextContent("Đề xuất");
    const before = screen.getByRole("region", { name: "Trước" });
    const after = screen.getByRole("region", { name: "Sau" });
    expect(before.parentElement).toBe(after.parentElement);
    expect(before.parentElement?.querySelectorAll("a,button,input").length).toBe(0);
    expect(before.textContent).not.toBe(after.textContent);
    expect(document.querySelector("[aria-live=polite]")).toHaveTextContent(/Có một đề xuất/);
    expect(screen.getByText("trình dựng luật")).toBeInTheDocument();
  });
  it("a proposal for another rule paints nothing here", () => {
    wrap(<WithGhost change={upd}><RulePage slug="northwind" id="wf-2" /></WithGhost>);
    expect(screen.queryByRole("group")).toBeNull();
    expect(screen.queryByRole("region", { name: "Trước" })).toBeNull();
  });
});
