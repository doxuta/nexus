// specs/078 T022: inside the Studio work surface UserList is the in-place page; the customer face keeps the old screen.
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { UserList } from "./UserList";

vi.mock("@/components/studio/operate/UserListStudio", () => ({ UserListStudio: ({ slug }: { slug: string }) => <p>trang mới của {slug}</p> }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), listUsers: vi.fn().mockResolvedValue([]), listRoles: vi.fn().mockResolvedValue([]), ensureAuth: vi.fn().mockResolvedValue(undefined) }));

describe("UserList entry", () => {
  it("embedded with a project: renders the in-place page for that project", () => {
    render(<EmbeddedSurface><UserList studioSlug="northwind" /></EmbeddedSurface>);
    expect(screen.getByText("trang mới của northwind")).toBeInTheDocument();
  });
  it("outside the work surface: the old screen (Thêm người dùng opens a dialog from it)", async () => {
    render(<UserList />);
    expect(await screen.findByRole("button", { name: /Thêm người dùng/ })).toBeInTheDocument();
    expect(screen.queryByText(/trang mới của/)).toBeNull();
  });
});
