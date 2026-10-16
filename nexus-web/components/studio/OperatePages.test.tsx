import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const push = vi.fn();
vi.mock("next/navigation", async (orig) => ({ ...(await orig<typeof import("next/navigation")>()), useRouter: () => ({ push, replace: vi.fn() }) }));
vi.mock("./WorkflowRunHistory", () => ({ WorkflowRunHistory: ({ workflowId }: { workflowId: string }) => <div data-testid="history">{workflowId}</div> }));
vi.mock("./AutomationPage", () => ({ SubPage: ({ children }: { children: React.ReactNode }) => <div>{children}</div> }));
vi.mock("./useProjectQuery", () => ({
  useProjectQuery: () => ({ data: [{ id: "w1", name: "Gửi kế toán", entityName: "crm_accounts" }, { id: "w2", name: "Nhắc lịch hẹn", entityName: "crm_contacts" }], isPending: false, isError: false, error: null, refetch: () => undefined }),
}));
vi.mock("./QueryState", () => ({ QueryState: ({ children }: { children: React.ReactNode }) => <>{children}</> }));

import { RunsPage } from "./OperatePages";

beforeEach(() => push.mockClear());

describe("RunsPage rule picker (#814)", () => {
  it("a chosen rule shows a picker holding that rule; picking another keeps the page and swaps ?luat=", () => {
    render(<RunsPage slug="northwind" ruleId="w1" />);
    const pick = screen.getByRole("combobox", { name: "Luật" }) as HTMLSelectElement;
    expect(pick.value).toBe("w1");
    expect(screen.getByTestId("history")).toHaveTextContent("w1");
    fireEvent.change(pick, { target: { value: "w2" } });
    expect(push).toHaveBeenCalledWith("/studio/du-an/northwind/van-hanh/tu-dong/lan-chay?luat=w2");
  });
  it("no rule chosen keeps the link list and no picker", () => {
    render(<RunsPage slug="northwind" />);
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getByRole("link", { name: "Nhắc lịch hẹn" })).toBeInTheDocument();
  });
});
