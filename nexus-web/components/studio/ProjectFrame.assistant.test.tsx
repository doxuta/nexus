// specs/076 ai3 T063a: the assistant inside the project frame — mounted once, survives a step change, dies with the
// project, never appears on a page that must not have it, and sits inside an <aside>.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/northwind/dung/du-lieu";
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("./DraftBar", () => ({ DraftBar: () => <span /> }));
vi.mock("@/components/shell/ModeSwitch", () => ({ useFacePaths: () => ({ appHref: "/accounts" }) }));

const st = { phase: "idle", matches: true, ws: null as { tenantSlug: string; tenantName: string } | null };
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: st.ws }, home: "northwind", sessionReadable: true, matches: () => st.matches, retry: vi.fn() }),
  useStudioTransition: () => st.phase !== "idle",
  useStudioTransitionFailed: () => false,
  useReloadOnIdentityStorage: () => {},
  transitionPhase: () => st.phase,
  workspaceOf: () => null,
  homeSlug: () => "northwind",
  sessionReadable: () => true,
  beginTransition: vi.fn(), clearAndAssign: vi.fn(), endPreviewIfAny: vi.fn(), enterProject: vi.fn(), exitProject: vi.fn(), switchProject: vi.fn(),
}));
vi.mock("@/lib/previewSession", () => ({ getPreviewToken: () => null }));
vi.mock("./usePublishState", () => ({ usePublishState: () => ({ data: undefined }) }));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: () => Promise.resolve([]),
  aiHealth: vi.fn(async () => true),
  listAiSessions: vi.fn(async () => []),
}));

import { aiHealth, listAiSessions } from "@/lib/api";
import { resetAssistantOpenForTests } from "@/lib/assistantOpen";
import { ProjectFrame } from "./ProjectFrame";

function Probe() {
  return <p data-testid="probe" />;
}
let qc = new QueryClient();
const frame = (slug = "northwind") => (
  <QueryClientProvider client={qc}>
    <ProjectFrame slug={slug}><Probe /></ProjectFrame>
  </QueryClientProvider>
);

beforeEach(() => {
  qc = new QueryClient(); st.phase = "idle"; st.matches = true; st.ws = null;
  pathname = "/studio/du-an/northwind/dung/du-lieu";
  vi.mocked(aiHealth).mockClear(); vi.mocked(listAiSessions).mockClear();
  resetAssistantOpenForTests(); window.localStorage.clear();
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: true, media: q, addEventListener: () => {}, removeEventListener: () => {} })) as unknown as typeof window.matchMedia;
});

describe("ProjectFrame — assistant", () => {
  it("is drawn once, inside an <aside>, beside the page", async () => {
    render(frame());
    expect(await screen.findAllByRole("complementary", { name: "Trợ lý AI của dự án" })).toHaveLength(1);
    expect(screen.getAllByTestId("assistant-toggle")).toHaveLength(1);
  });

  it("a step change (new pathname) keeps the same assistant and its typed text", async () => {
    const { rerender } = render(frame());
    const box = (await screen.findByRole("textbox")) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "dở dang" } });
    pathname = "/studio/du-an/northwind/dung/tu-dong";
    rerender(frame());
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("dở dang");
  });

  it("another project (new slug) starts from nothing", async () => {
    const { rerender } = render(frame());
    fireEvent.change(await screen.findByRole("textbox"), { target: { value: "của dự án A" } });
    pathname = "/studio/du-an/acme/dung/du-lieu";
    st.ws = { tenantSlug: "acme", tenantName: "Khách A" }; // the session of the other project
    rerender(frame("acme"));
    const box = (await screen.findByRole("textbox")) as HTMLTextAreaElement;
    expect(box.value).toBe("");
  });

  it("a context switch in flight removes the panel (surface unmounted)", async () => {
    const { rerender } = render(frame());
    await screen.findByRole("complementary", { name: "Trợ lý AI của dự án" });
    st.phase = "running";
    rerender(frame());
    expect(screen.queryByRole("complementary", { name: "Trợ lý AI của dự án" })).toBeNull();
  });

  it("an operator-only page (cai-dat) has no assistant and makes no assistant call", async () => {
    pathname = "/studio/du-an/northwind/cai-dat";
    render(frame());
    await act(async () => {});
    expect(screen.queryByRole("complementary", { name: "Trợ lý AI của dự án" })).toBeNull();
    expect(screen.queryByTestId("assistant-toggle")).toBeNull();
    expect(aiHealth).not.toHaveBeenCalled();
    expect(listAiSessions).not.toHaveBeenCalled();
  });
});
