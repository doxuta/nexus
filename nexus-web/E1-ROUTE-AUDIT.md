# E1 — Kiểm kê Route × Screen × Trạng-thái (nexus-web)

> Audit 2026-07-02 (Explore agent + `next build` thật, HEAD `847e7f9`). Đây là **input chốt cho E2**
> (IA + design-system). NAV-MAP.md cũ (2026-06-25) đã ~90% stale — coi là lịch sử, KHÔNG dùng làm
> nguồn IA; sẽ được viết lại ở E2. Stack: Next 16 App Router · React 19 · Tailwind v4 CSS-first
> (không config file) · TanStack Query · RHF+zod · KHÔNG shadcn/Radix — `components/ui/*` tự viết.

## 0. Hai khuôn mặt (đã tồn tại, chưa sạch ranh giới)

- **Builder (Studio nội bộ Nexus)** = `/studio/*` — `StudioSidebar` + `Topbar` import LẺ từng page
  (KHÔNG có `app/studio/layout.tsx`).
- **App khách** = `/home`, `/[entity]/*`, `/dashboards`… — 3 kiểu gắn shell KHÁC NHAU (xem §3).
- Cầu nối: `ModeSwitch` (admin-only, cả 2 sidebar) `/studio ↔ /home`; `/preview` là cầu
  Studio→App (render shell khách + banner tenant giả "Lan Cosmetics"); `CommandPalette` (nằm trong
  Topbar KHÁCH) expose 3 lệnh `/studio/*` cho admin.

## 1. Ma trận trạng thái (47 route từ `next build`)

### ✅ REAL (28) — API thật qua `lib/api.ts` (BASE `NEXT_PUBLIC_API_BASE`, dev-proxy `app/api/v1/[...path]/route.ts` → :8090)
`/home` · `/dashboards` · `/dashboards/[id]` · `/reports` · `/reports/[id]` · `/login` ·
`/[entity]` (LayoutListScreen) · `/[entity]/[id]` (LayoutDetailScreen) · `/[entity]/new` +
`/[entity]/[id]/edit` (LayoutFormScreen) · `/entities` · `/approvals` · `/notifications` ·
`/profile` · `/settings` (client-only prefs, localStorage) · `/admin/{users,roles,teams,audit}` ·
`/studio` · `/studio/{model,relationships,layout,workflows,approvals,reports,dashboards}`

### 🔗 REAL nhưng MỒ-CÔI (0 inbound link — chỉ cần nối nav là dùng được)
| Route | Screen | Việc cần |
|---|---|---|
| `/[entity]/import` | ImportScreen (CSV map→preview→run, real) | nút Import trong LayoutList |
| `/[entity]/deleted` | RecycleBinScreen (soft-delete restore, real) | nút Thùng rác trong LayoutList |
| `/live` | LiveAccounts (demo read real) | nav entry hoặc xoá |
| `/[entity]/preview` | EntityList legacy (self-labeled "cũ") | retire (xem §2) |

### 🎭 MOCK / FAKE (không được ship trong IA khách)
| Route | Nguồn fake | Verdict |
|---|---|---|
| `/detail` | `lib/sample` (SAMPLE_*) + `lib/forms` (ACCOUNT_FORM) | redirect → `/[entity]/[id]` rồi xoá |
| `/form` | `lib/forms` | redirect → `/[entity]/new` rồi xoá |
| `/atlas` (untracked) | NexusHiFiAtlas — kitchen-sink giữ sống cả cụm mock (AccountsList/RecordDetail/RecordForm/Settings/Dashboard/Audit) | xoá cùng cụm + fixtures `lib/{mock,sample,forms,entities}.ts` |
| ~~`/studio/ai`~~ (đã xoá, specs/076 ai11) | `lib/aibuilder` SAMPLE_PROMPT ("P5 nối LLM thật") | honest-disable hoặc giữ label rõ mock |
| `/studio/deploy` | `lib/studio` PIPELINE | honest-disable |
| `/studio/verticals` | HYBRID: catalog `VERTICALS` mock + export entity real | tách phần real, label phần mock |
| `/preview` | data real nhưng hardcode `accounts` + banner tenant giả | chuyển thành `/studio/preview` (cầu Studio) |

### 🚧 HONEST-PLACEHOLDER (đúng chuẩn D4 — giữ, quyết finish/hide ở E2)
`/forgot-password` · `/reset-password` (backend #1 ĐÃ có — nối được ngay!) · `/admin/field-acl` ·
`/admin/sharing` · widget file/image (create) · restore trong `screens/Audit.tsx`

### ↪️ REDIRECT STUB (giữ): `/` → `/home` · `/admin` → `/admin/users` · `/audit` → `/admin/audit` · `/dashboard` → `/dashboards` · `/sharing` → `/admin/sharing`

## 2. Trùng lặp — canonical thắng rõ ràng

| Mảng | CANONICAL (real, metadata-driven) | Legacy phải diệt |
|---|---|---|
| List | `LayoutList*` @ `/[entity]` | `screens/EntityList` @ `/preview`, `/[entity]/preview` |
| Detail | `LayoutDetail*` @ `/[entity]/[id]` | `screens/RecordDetail` @ `/detail` (mock) |
| Form | `LayoutForm*` @ `/[entity]/new`+`/edit` | `screens/RecordForm` @ `/form` (mock) + `screens/CreateRecord` (drawer trong EntityList) |

**Bẫy đang sống**: drawer của EntityList (real) có "Mở trang đầy đủ" `fullHref="/detail"` → trỏ vào
MOCK. Viewer↔Builder KHÔNG phải trùng: `/dashboards`+`/reports` (khách xem) vs `/studio/…` (dựng) —
giữ cả hai, sửa label trùng "Phê duyệt". Component chết xác nhận: `screens/Sharing.tsx` (0 importer).

## 3. Lỗi cấu trúc lớn nhất: 3 kiểu gắn shell

1. Route-group `(app)/layout.tsx` → AppShell: chỉ `/home /dashboards /reports`.
2. Page tự import Sidebar+Topbar: `/preview /detail /live /notifications /profile` + `admin/layout.tsx`.
3. Shell NHÚNG TRONG screen component: `Layout{List,Detail,Form}Screen`, EntityDirectory,
   RecycleBinScreen, ImportScreen.

→ E2 phải chốt: **một** `(app)` route-group + **một** `app/studio/layout.tsx`; screen component
không được tự render chrome.

## 4. NAV-MAP drift (đã xác nhận từng dòng)
- NAV-MAP nói `/settings` = "Phân quyền" → thực tế là PreferencesScreen; quyền đã dời `/admin/*`.
  `CommandPalette` cũng còn mislabel này + hop `/dashboard`.
- Toàn bộ mục "Đang dựng (route chưa có)" của NAV-MAP → **đã có trên disk hết**.
- Vắng khỏi NAV-MAP: tất cả `/admin/*`, `/[entity]/*` con, 9 route `/studio/*`, `/home`…

## 5. Design-system hiện trạng
- **Điểm mạnh giữ lại**: token OKLCH tập trung ở `app/globals.css` (`--ac --bg --panel --fg`,
  semantic `--ok/--wn/--dg/--cy`, radii `--r-*`); dark-mode `[data-theme]` pre-paint sạch;
  per-tenant theme class (`.theme-cobalt`…); `lib/errorMessages.ts` + `ApiError` map lỗi tập trung;
  Toaster thống nhất.
- **Nợ**: ~1.200 arbitrary type-size (`text-[12.5px]`×319, `text-[13px]`×276…) không có scale;
  `text-[#fff]` rải ở admin/*; primitives (Drawer/modal/menu/combobox) bespoke từng nơi;
  `bits.tsx` (EntityBadge/Avatar/StatusBadge) là mầm thư viện nhưng under-used; 2 sidebar trùng
  ~120 dòng; 3 bản copy offset-pagination (LayoutList/EntityList/Notifications).

## 6. TOP-10 dọn dẹp (impact ÷ effort — thứ tự thực thi E3)
1. Nối 3 route real-mồ-côi vào nav (import/deleted/live).
2. Sửa CommandPalette: "Phân quyền"→`/admin/roles`; "Bảng điều khiển"→`/dashboards`.
3. Viết lại NAV-MAP.md (E2 output).
4. Redirect `/detail`→`/[entity]/[id]`, `/form`→`/[entity]/new`; sửa `fullHref` của EntityList drawer.
5. Retire legacy EntityList list-path; `/preview` → `/studio/preview` (cầu Studio có chủ đích).
6. Xoá `/atlas` + cụm mock + fixtures `lib/{mock,sample,forms,entities,aibuilder}.ts` + mảng chết `lib/notifications.ts`.
7. Gom shell về route-group layouts (`(app)` + `studio/layout.tsx`).
8. Trích primitives chung: `<NavShell>` cho 2 sidebar; `EmptyState/Skeleton/Pagination/DataTable` từ bits.tsx; 1 bản pagination.
9. Type-scale semantic (`--text-*`) + codemod arbitrary sizes; sweep `text-[#fff]`.
10. Chốt số phận honest-placeholders: **nối `/forgot-password`+`/reset-password` vào backend #1 (ĐÃ CÓ — mig 000027)**; field-acl/sharing admin: finish hoặc ẩn khỏi nav.

## Phụ lục — mốc fixture để grep khi dọn
Mock roots: `lib/mock.ts` · `lib/sample.ts` · `lib/forms.ts` · `lib/entities.ts` · `lib/studio.ts`
(PIPELINE/VERTICALS) · `lib/aibuilder.ts` · `lib/notifications.ts` (mảng chết). Spine real:
`lib/api.ts` (+`lib/admin.ts`, `lib/analytics.ts`, `lib/profile.ts`, `lib/metadata/*`, `lib/query/*`).
