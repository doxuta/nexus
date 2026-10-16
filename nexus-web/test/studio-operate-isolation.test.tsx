// specs/078 T012a(i) + FR-135, SC-015(b): one row per van-hanh/* route saying whether the page is isolated between customers
// (0 fetches when projectTokenMatches(S) is false; no data of the old S after switching S; no secret/form/armed confirm
// survives a switch). A route added to the table in lib/studioRoutes.ts WITHOUT a row here turns this test red. A row is
// "pending:<slice>" until the slice that mounts the new page proves it; the pending count may only go down.
//
// ponytail: the per-route behaviour cases (a)(b)(c) are written by the slice that flips its row to "covered", next to its page,
// because the page does not exist before then; this file holds the table and the ratchet that forces the work to happen.
import { describe, expect, it } from "vitest";
import { ROUTES } from "@/lib/studioRoutes";

type Isolation = "covered" | `pending:${string}`;

const TABLE: Record<string, Isolation> = {
  "van-hanh/nguoi-dung": "pending:u7-nguoi-dung-danh-sach",
  "van-hanh/nguoi-dung/dang-nhap-mot-lan": "pending:u7-dang-nhap-mot-lan-nhat-ky",
  "van-hanh/nguoi-dung/nhat-ky": "pending:u7-audit-nguoi-dung-bo-loc",
  "van-hanh/du-lieu": "pending:u7-du-lieu-danh-sach-bang",
  "van-hanh/du-lieu/:bang": "pending:u7-du-lieu-danh-sach-bang",
  // covered by components/studio/operate/RecordPage.test.tsx (token mismatch => 0 fetches; unknown table => 0 data calls)
  "van-hanh/du-lieu/:bang/moi": "covered",
  "van-hanh/du-lieu/:bang/tao-nhieu": "pending:u7-du-lieu-nhap-csv",
  "van-hanh/du-lieu/:bang/:id": "covered",
  "van-hanh/du-lieu/:bang/nhap": "pending:u7-du-lieu-nhap-csv",
  // covered by components/studio/operate/RecycleBinPage.test.tsx ("isolation": token of another project => the bin and purge are never called)
  "van-hanh/du-lieu/:bang/thung-rac": "covered",
  "van-hanh/tu-dong": "pending:u7-tu-dong-tong-quan",
  "van-hanh/tu-dong/lan-chay": "pending:u7-tu-dong-tong-quan",
  "van-hanh/tu-dong/email": "pending:u7-viec-nen-email",
  "van-hanh/tu-dong/viec-nen": "pending:u7-viec-nen-email",
  "van-hanh/tu-dong/cong-nhan": "pending:u7-cong-nhan-du-lieu-vao",
  // covered by components/studio/operate/CredentialsPage.test.tsx (token mismatch => 0 fetches, nothing drawn; key on slug resets the form)
  "van-hanh/tu-dong/khoa-ket-noi": "covered",
  // covered by components/screens/admin/ApiKeys.test.tsx (token mismatch => 0 fetches; key on slug drops form + one-time secret; assertScope false => 0 writes)
  "van-hanh/tu-dong/khoa-truy-cap": "covered",
  // covered by components/studio/operate/HealthPage.isolation.test.tsx (token of another project => 0 fetches)
  "van-hanh/suc-khoe": "covered",
};

/** Only goes down. Lower it in the commit that flips a row to "covered". */
const PENDING_CEILING = 12;

const operateRoutes = ROUTES.filter((r) => r.segments[0] === "van-hanh").map((r) => r.id);

describe("Vận hành isolation table (FR-135)", () => {
  it("has exactly one row per van-hanh/* route (a new route without a row is red)", () => {
    expect(Object.keys(TABLE).sort()).toEqual([...operateRoutes].sort());
  });

  it("every pending row names the slice that closes it", () => {
    for (const [route, state] of Object.entries(TABLE)) {
      if (state !== "covered") expect(state, route).toMatch(/^pending:u7-[a-z0-9-]+$/);
    }
  });

  it("the pending count only goes down", () => {
    const pending = Object.values(TABLE).filter((s) => s !== "covered").length;
    expect(pending).toBeLessThanOrEqual(PENDING_CEILING);
    if (pending < PENDING_CEILING) throw new Error(`lower PENDING_CEILING to ${pending}`);
  });
});
