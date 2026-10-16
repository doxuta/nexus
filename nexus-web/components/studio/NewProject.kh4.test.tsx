import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// specs/073 kh4 T276: Tài khoản (Dự án mới moved to NewProject.test.tsx, tao1) của tôi and Cài đặt dự án — honest and behind the `nha` gate.
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

const gate = vi.fn();
vi.mock("./ContextGate", () => ({
  ContextGate: (p: { slug: string; ctx: string; children: ReactNode }) => {
    gate({ slug: p.slug, ctx: p.ctx });
    return <div data-gate={p.ctx}>{p.children}</div>;
  },
}));
vi.mock("@/lib/studioContext", () => ({ homeSlug: () => "northwind" }));
vi.mock("@/components/screens/Profile", () => ({ Profile: () => <p>hồ sơ và xác thực hai bước</p> }));

import { AccountPage } from "./AccountPage";
import { ProjectSettings } from "./ProjectSettings";

describe("Tài khoản của tôi", () => {
  it("is the profile (with the two-step card inside it) behind the nha gate", () => {
    render(<AccountPage />);
    expect(gate).toHaveBeenLastCalledWith({ slug: "northwind", ctx: "nha" });
    expect(screen.getByText("hồ sơ và xác thực hai bước")).toBeInTheDocument();
  });
});

describe("Cài đặt dự án", () => {
  it("at the workshop it is read only: name, code, and that the customer does not see it", () => {
    const { container } = render(<ProjectSettings slug="northwind" />);
    expect(screen.getByText("Xưởng Nexus")).toBeInTheDocument();
    expect(screen.getByText("northwind")).toBeInTheDocument();
    expect(screen.getByText(/khách không thấy/)).toBeInTheDocument();
    expect(container.querySelector("input, button, select, textarea")).toBeNull();
  });

  it("for a customer project the page no longer shows the stub sentence (so2 landed real settings)", () => {
    render(<QueryClientProvider client={new QueryClient()}><ProjectSettings slug="acme" /></QueryClientProvider>);
    expect(screen.queryByText("Cài đặt dự án chưa có trong bản Nexus này.")).toBeNull();
  });
});
