// specs/079 FR-026: KhamPage turns the publish state into the doctor's deliveredState, failing closed.
import { cleanup, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";
import { homeSlug } from "@/lib/studioContext";

const state = vi.hoisted(() => ({ q: {} as Record<string, unknown> }));
vi.mock("../usePublishState", () => ({ usePublishState: () => state.q }));
vi.mock("./ReadinessSection", () => ({ ReadinessSection: () => <p>readiness</p> }));
vi.mock("./PackUpgradeNotice", () => ({ PackUpgradeNotice: () => null }));
vi.mock("@/components/screens/admin/DoctorScreen", () => ({
  DoctorScreen: ({ deliveredState }: { deliveredState: string }) => <p>state:{deliveredState}</p>,
}));
import { KhamPage } from "./KhamPage";

describe("KhamPage", () => {
  beforeEach(() => { state.q = {}; });
  it.each([
    ["still loading", { isPending: true }, "unknown"],
    ["delivered", { data: { delivered: true } }, "delivered"],
    ["not delivered", { data: { delivered: false } }, "not-delivered"],
    ["delivered missing from the answer", { data: {} }, "unknown"],
    ["other error", { isError: true, error: new ApiError(500, "UNKNOWN", "x") }, "unknown"],
    ["a real 404 with a code", { isError: true, error: new ApiError(404, "TENANT_NOT_FOUND", "x") }, "unknown"],
    ["route not built", { isError: true, error: new ApiError(404, "UNKNOWN", "404 page not found") }, "no-concept"],
  ])("%s ⇒ %s", (_n, q, want) => {
    state.q = q;
    render(<KhamPage slug="acme" />);
    expect(screen.getByText(`state:${want}`)).toBeInTheDocument();
    expect(screen.getByText("readiness")).toBeInTheDocument();
  });

  it("the snapshot note shows on a customer project only", () => {
    render(<KhamPage slug="acme" />);
    expect(screen.getByText(/không phải lần khám này/)).toBeInTheDocument();
    cleanup();
    render(<KhamPage slug={homeSlug()} />);
    expect(screen.queryByText(/không phải lần khám này/)).not.toBeInTheDocument();
  });
});
