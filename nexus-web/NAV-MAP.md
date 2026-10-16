# Nexus FE — Bản đồ điều hướng CANONICAL (E2, 2026-07-02)

> Viết lại toàn bộ theo [`E2-IA-DESIGN-SYSTEM.md`](E2-IA-DESIGN-SYSTEM.md) §3 (bản 06-25 đã stale 90%
> — xác nhận từng dòng ở [`E1-ROUTE-AUDIT.md`](E1-ROUTE-AUDIT.md)). Cột **Now** = trạng thái hiện
> tại; **Target** = đích E3 (wave W1–W4). Cập nhật cột Now MỖI khi route đổi (cùng commit).
> Quy tắc chung: không màn nào dead-end; mọi màn có-shell có Sidebar + Topbar từ route-group layout;
> route bị thay = redirect, không 404.

## Hai khuôn mặt — cầu nối DUY NHẤT
- **APP KHÁCH** `(app)` — `AppShell` (Sidebar + AppTopbar ⌘K/chuông/org). Người dùng tenant.
- **STUDIO** `/studio/*` — `StudioShell` (StudioSidebar + StudioTopbar), **admin-only**. Team Nexus lắp app.
- Cầu: `ModeSwitch` (admin-only, 2 sidebar) `/studio ↔ /home` · `/studio/preview` (xem-trước App).
  ⌘K của App **không** chứa lệnh `/studio/*` (✅ W2).

**✅ W3a (2026-07-02) — gom shell về route-group layouts** (chỉ Chrome composition, không restyle/không
tách primitives — đó là W3b): `app/studio/layout.tsx` mới (RequireAuth+RequireAdmin fail-closed →
`/home`, StudioSidebar+Topbar render 1 lần cho cả 12 route `/studio/*`; TRƯỚC đó KHÔNG route studio nào
có admin-gate — lỗ hổng này đã đóng). `app/approvals` `app/notifications` `app/profile` `app/settings`
`app/entities` dời vào `app/(app)/` (URL không đổi, route-group). Họ `app/[entity]/*` (list/detail/
edit/new/deleted/import/preview) dời vào `app/(app)/[entity]/*` — TRƯỚC đó cũng KHÔNG có RequireAuth
nào ở tầng này; nay thừa hưởng AppShell nên đã có. `AppShell` được nâng cấp để tự suy crumb + search-
placeholder theo entity (mirror y hệt text cũ của từng screen) thay vì mỗi screen tự vẽ Topbar riêng.
"StudioTopbar" trong dòng trên là tên gọi Ở-ĐÍCH của E2 §3.3 (chưa tách thành primitive riêng — W3a
vẫn dùng chung `components/shell/Topbar.tsx` cho cả 2 mặt, tách `StudioTopbar` là việc của W3b).
`admin/layout.tsx` **CHƯA đổi** (giữ `<Sidebar>`/`<Topbar>` riêng, không có RequireAuth/RequireAdmin —
cờ cho wave sau, ngoài phạm vi §2.A/§3 của W3a).

**✅ W5a/b/c (2026-07-02) — brand "Nexus Be"** (§10): token be (`app/globals.css` `:root` +
`[data-theme="dark"]` — GIỮ tên biến, đổi giá trị; `.theme-cobalt/terracotta/emerald` KHÔNG đụng)
· logo `components/brand/NexusLogo.tsx` (glyph "N-node" SVG tay + wordmark) thay badge chữ "N" cứng
ở `Sidebar.tsx`/`StudioSidebar.tsx`/`Login.tsx`, favicon `app/icon.svg` (glyph trần, teal) ·
landing `/` mới thay route redirect thẳng cũ (xem bảng MARKETING dưới) — route root đổi từ
`app/page.tsx` (server redirect `/home`, đã XOÁ) sang `app/(marketing)/page.tsx`.

**✅ ENG-BATCH #1 FE (2026-07-02) — Thêm nhiều (multi-row create)**: route mới `/[entity]/batch`
(`components/layout/BatchCreateForm.tsx`) — bảng nhập nhiều dòng cho MỘT entity, nối
`POST /:entity/bulk?mode=atomic|partial` (backend live cùng ngày). Nút "Thêm nhiều" ở toolbar
`LayoutList` (cạnh "Nhập dữ liệu"/"Tạo mới"). `AppShell` breadcrumb: thêm `batch → "Thêm nhiều"`
vào `ENTITY_SUBROUTE_LABELS`.

## MARKETING (public, route group `(marketing)`, không shell)
| Route | Screen | Now | Target |
|---|---|---|---|
| `/` | `Landing` (`components/screens/Landing.tsx`) | ✅ W5c (2026-07-02): SSG tĩnh, không gọi API. Chưa đăng nhập → thấy landing (hero + nhà máy 90/10 + 6 năng lực engine + verticals CRM live/LMS+Ticketing roadmap + footer). Đã đăng nhập (token localStorage) → `MarketingAuthGate` (client) redirect `/home` ngay khi mount — thay hành vi cũ (`app/page.tsx` server-redirect thẳng `/home`, đã xoá) | giữ |

## APP KHÁCH
| Route | Screen | Now | Target |
|---|---|---|---|
| `/login` | Login | ✅ real | giữ |
| `/forgot-password` · `/reset-password` | stub honest | 🚧 placeholder | **W2: nối backend #1 (mig 000027 ĐÃ có)** |
| `/home` | HomeScreen | ✅ real | giữ |
| `/[entity]` | LayoutListScreen | ✅ real (canonical) | W1: + nút Nhập dữ liệu, menu Thùng rác |
| `/[entity]/[id]` | LayoutDetailScreen | ✅ real | giữ (inline-edit = L2-HARDEN, OUT wave này) |
| `/[entity]/new` · `/[entity]/[id]/edit` | LayoutFormScreen | ✅ real | W4: Save-&-new |
| `/[entity]/import` | ImportScreen | ✅ real, đã nối (nút "Nhập dữ liệu" ở LayoutList) | W1: link từ LayoutList |
| `/[entity]/deleted` | RecycleBinScreen | ✅ real, đã nối (icon "Thùng rác" ở LayoutList) | W1: link từ menu ⋯ LayoutList |
| `/[entity]/batch` | BatchCreateForm | ✅ ENG-BATCH #1 FE (2026-07-02): tạo nhiều dòng 1 lần, nối `POST /:entity/bulk`, nút "Thêm nhiều" ở LayoutList | giữ |
| `/dashboards` · `/dashboards/[id]` | viewer | ✅ real | giữ (`/dashboard` redirect giữ) |
| `/reports` · `/reports/[id]` | viewer | ✅ real | giữ |
| `/approvals` | ApprovalsInbox | ✅ real | ✅ W3a: vào `(app)` group |
| `/notifications` · `/profile` · `/settings` · `/entities` | real | ✅ | ✅ W3a: vào `(app)` group |
| `/admin/{users,roles,teams,audit}` | real | ✅ | giữ (⚠️ W3a xác nhận: `admin/layout.tsx` KHÔNG có RequireAuth/RequireAdmin nào — chỉ ẩn menu GOVERNANCE qua `isAdmin`, không chặn truy cập URL trực tiếp; ngoài phạm vi §2.A/§3 của E3 W3a — cờ cho wave sau, không sửa ở đây) |
| `/admin/field-acl` · `/admin/sharing` | honest placeholder | 🚧 | finish-hoặc-ẩn (quyết ở W4) |
| `/audit` `/sharing` `/admin` | redirect | ✅ | giữ |

## STUDIO — MỘT XƯỞNG wave 1 (2026-07-29, `docs/NEXUS-MOT-XUONG-2026-07-29.md` §2b, khung)
`/studio/apps/[slug]` là workspace theo dự án: `AppWorkspace` (cột trái = danh tính app + cây điều
hướng nhóm theo công việc, phải = panel theo `?panel=`, dưới = AI dock). `StudioHome` (`/studio`)
hết là "pipeline 8 bước" — nay là DANH SÁCH DỰ ÁN (cửa vào) + "tiếp tục việc dở". `StudioSidebar`
chỉ còn chrome (logo · "Danh sách dự án" · "Cài mẫu & Bàn giao" · ModeSwitch · user) — cây công cụ
sống TRONG workspace. `lib/workingEntity.ts` (sessionStorage cross-builder) đã XOÁ — URL là nguồn
sự thật.

| Route | Screen | Now | Target |
|---|---|---|---|
| `/studio` | StudioHome | ✅ MỘT XƯỞNG: danh sách dự án (card + tạo mới + tiếp tục việc dở); `?needsApp=<panel>` = banner khi một route cũ redirect tới đây không có app scope | giữ |
| `/studio/apps` | AppsManager | ✅ real (CRUD app: tạo/sửa/xoá/mặc định/nav) | giữ — cửa vào quản trị đầy đủ, `/studio` là cửa vào nhanh |
| `/studio/apps/[slug]` | **AppWorkspace** (mới) | ✅ MỘT XƯỞNG wave 1: khung 3 vùng, panel router (`tong-quan`→AppOverview trimmed · `bang`→DataModelBuilder · `menu`→NavEditor · `thuong-hieu`→BrandingScreen · `tu-dong`→WorkflowBuilder · `duyet`→ApprovalBuilder · `hang-doi`→WorkQueueBuilder · `webhook`→InboundHookScreen · `ban-giao`→HandoverWizard trong panel riêng); 6 panel còn lại (`bao-cao`/`dashboard`/`vai-tro`/`nguoi-dung`/`quyen-truong`/`mau`) là cầu nối trung thực mở tab mới (`lib/workshopPanels.ts` `PANEL_BRIDGE`) | wave sau: entity-hub tabs (Trường·Bố cục·Quan hệ·Dữ liệu dưới `bang`) + nhúng thẳng 6 panel cầu nối + AI dock thật |
| `/studio/model` · `/workflows` · `/approvals` · `/workqueues` · `/hooks` · `/branding` | `LegacyPanelRedirect` (mới, thin) | ✅ MỘT XƯỞNG: có app scope → `/studio/apps/<slug>?panel=…` (giữ `?entity=`); không có → `/studio?needsApp=…` | giữ (route cũ không 404) |
| `/studio/layout` · `/studio/relationships` | LayoutBuilder · RelationshipDesigner | ✅ real, **KHÔNG redirect** (deviation có chủ đích wave 1 — chưa có panel workspace cho 2 tool này; redirect vào `panel=bang` sẽ là silent-fallback. Cây workspace link thẳng tới đây) | wave sau: thành tab trong hub `bang` |
| `/studio/verticals` | VerticalLibrary | ✅ real | giữ |
| ~~`/studio/ai`~~ | ~~AiBuilder~~ — **đã xoá (specs/076 ai11): 404; trợ lý duy nhất là ô cạnh phải trong dự án, ⌘K › "Mở trợ lý"** | ✅ real; nhận `?app=<slug>` từ AI dock của workspace (đặt scope qua cơ chế `?app=` sẵn có của `ActiveAppProvider`) | wave sau: nhúng inline trong AI dock thay vì điều hướng |
| `/studio/reports` · `/studio/dashboards` · `/studio/deploy` | redirect | ✅ giữ nguyên (đã là stub từ trước) | giữ |

## Route bị THAY/XOÁ (W1–W2)
| Route cũ | Số phận |
|---|---|
| `/detail` (mock) | ✅ W1: redirect → `/home` (mất context) · ✅ W2: screen `RecordDetail.tsx` xoá cùng cụm mock |
| `/form` (mock) | ✅ W1: redirect → `/home` (không đoán được entity) · ✅ W2: screen `RecordForm.tsx` xoá cùng cụm mock |
| `/preview` | ✅ W2: → `/studio/preview` (redirect giữ 1 wave) |
| `/[entity]/preview` (EntityList "cũ") | ✅ W2: redirect → `/[entity]`, xoá screen legacy (`EntityList.tsx`) + `CreateRecord` drawer |
| `/atlas` + cụm mock (AccountsList/RecordDetail/RecordForm/Settings/Dashboard/Audit/Sharing) + `lib/{mock,sample,forms,entities,aibuilder}.ts` + mảng `lib/notifications.ts` | ✅ W2: XOÁ (prototype đã lưu `_archive/`). `lib/notifications.ts` có 0 importer thật (Notifications screen dùng `lib/api`) → xoá cả file, không chỉ mảng. `lib/entities.ts` còn 3 importer thật ngoài cụm mock (`LayoutFormScreen`/`ImportScreen`/`RecycleBinScreen` — chỉ dùng `.label`) → chuyển sang `useEntityLabel()` (metadata thật) trước khi xoá |
| `/live` | ✅ W1: XOÁ (route + `LiveAccounts.tsx`, 0 importer còn lại; LayoutList thay thế) |

## ⌘K CommandPalette (App)
Đi tới: `/home` `/dashboards` `/reports` `/approvals` `/notifications` `/profile` `/settings`
`/admin/roles` (label "Phân quyền" — ✅ W1 đã sửa khỏi `/settings`) · `/[entity]` + `/[entity]/new` động.
Bỏ: ✅ W2: lệnh `/studio/*` đã bỏ (3 lệnh admin `studio-model/studio-layout/studio-rel` + mục "Hỏi AI" ungated → `/studio/ai`) · hop `/dashboard`→`/dashboards` (✅ W1 đã sửa, trỏ thẳng).
