# E2 — IA Hai-Khuôn-Mặt + Design-System Một-Nguồn (nexus-web)

> Technical Doc theo skill `brainstorm-and-spec` (2026-07-02). Input: [`E1-ROUTE-AUDIT.md`](E1-ROUTE-AUDIT.md)
> (cb675bc) · handoff `_archive/design_handoff_nexus/` (hướng B "Atlas" đã chốt) · skill
> `design-anti-slop` · quyết định Founder đã khoá (memory `nexus-ux-redesign-decision`): **2 khuôn mặt
> + switcher, data thật, honest placeholder cho backend chưa có**. Doc này là NGUỒN cho E3 (thực thi)
> và E5 (browser-verify hành trình); NAV-MAP.md được viết lại theo doc này.

## 1. Tổng quan

**Vấn đề (E1 chứng minh, không phải giả định):** FE có đủ tính năng thật (28 route REAL) nhưng trải
nghiệm chắp vá: 3 kiểu gắn shell khác nhau, 7 route mock/legacy còn sống (1 bẫy real-list→fake-detail),
4 tính năng thật mồ côi không có link, NAV-MAP sai 90%, ~1.200 cỡ chữ arbitrary không thang.

**Mục tiêu:** một IA mạch lạc 2 khuôn mặt + một design-system cưỡng-chế-được (token đã có sẵn, cần
chuẩn hoá phần thiếu) — để MỌI thay đổi sau bám cùng hệ.

**Non-goals (YAGNI):** không đổi hướng thẩm mỹ (Atlas đã chốt, token OKLCH đã live); không adopt
shadcn wholesale (xem §3.B); không xây saved-views/Kanban (DEFER sẵn ở specs/041-layout-engine §8); không đụng
backend.

### Tiền-đề đã thách thức
| Tiền-đề | Phán quyết | Bằng chứng |
|---|---|---|
| "FE render phẳng từ sys_fields, cần build L2" | **SAI — đã build** | code-audit 847e7f9; L2 wired end-to-end |
| "Design-system phải làm mới" | **SAI — token layer đã đúng handoff** | globals.css OKLCH + 3 theme + dark parity; thiếu type-scale + primitives |
| "Route trùng cần quyết canonical" | **ĐÚNG nhưng đã có người thắng rõ** | Layout* stack real; legacy đều mock/self-labeled "cũ" |
| "2 khuôn mặt cần thiết kế" | **ĐÃ CÓ, cần làm sạch ranh giới** | ModeSwitch tồn tại; 3 cầu lẫn lộn (E1 §4) |

## 2. Quyết định kiến trúc (3 phương án mỗi quyết định — lập trường rõ)

### A. Hợp nhất shell (3 kiểu → ?)
1. **Route-group layouts (CHỌN)** — `(app)/layout.tsx` (RequireAuth+AppShell) ôm TOÀN BỘ mặt khách
   kể cả `/[entity]/*`; thêm `studio/layout.tsx` (RequireAdmin+StudioShell). Screen component bỏ
   chrome tự nhúng. *Effort M · Risk thấp (Next chuẩn) · Pro: 1 nguồn chrome, back/breadcrumb nhất
   quán, xoá ~7 bản copy Sidebar-import.*
2. Giữ nguyên 3 kiểu, chỉ sửa từng chỗ lệch. *Effort S · Pro: ít diff. Con: nguồn lỗi tái sinh mãi —
   chính là "vá lẻ" mandate cấm.*
3. Lateral: HOC `withShell(Screen)`. *Effort M · Con: chống lại App Router idiom, khó RSC.*
   **Lập trường: 1. Đổi ý nếu**: đo được route-group gây remount mất state đáng kể giữa các màn entity (chưa có bằng chứng).

### B. Thư viện primitives (bespoke → ?)
1. Adopt shadcn/Radix wholesale. *Effort L · Risk restyle toàn app; Con: E1 cho thấy bespoke đã phủ
   90% nhu cầu; anti-slop cảnh báo shadcn-sameness.*
2. **Consolidate bespoke thành `components/ui` chuẩn hoá (CHỌN)** — nâng `bits.tsx` thành thư viện:
   `EmptyState · Skeleton · Pagination · DataTable(mỏng trên LayoutList logic) · NavShell · Modal ·
   Drawer · Menu`, tất cả ăn token. Chỉ nhập lib ngoài cho case khó (đã có dnd-kit, xyflow, cmdk-style
   palette tự viết ổn). *Effort M · Risk thấp — code đã chạy, chỉ gom + thay call-site.*
3. Headless-only (Radix primitives + style riêng) cho modal/menu/combobox. *Effort M-L · để dành khi
   bespoke lộ bug a11y thật.*
   **Lập trường: 2. Đổi ý nếu**: audit a11y (focus-trap/aria) của Modal/Drawer bespoke fail nhiều điểm → chuyển 3.

### C. Số phận route legacy/mock
1. Xoá thẳng. *Risk: link cũ 404, mất tham chiếu.*
2. **Redirect + xoá theo wave (CHỌN)**: `/detail`→`/[entity]/[id]` (mất id → `/home`), `/form`→
   `/[entity]/new`, `/preview`→`/studio/preview` (cầu Studio có chủ đích, giữ banner "bản xem trước");
   `/atlas`+cụm mock+fixtures xoá hẳn (prototype đã archive ở `_archive/`); `/[entity]/preview` xoá
   (redirect về `/[entity]`). *Pro: không dead-link, diff kiểm soát được.*
3. Feature-flag ẩn dần. *Con: chi phí flag cho code không ai cần giữ.*
   **Lập trường: 2.**

## 3. IA chốt

### 3.1 Mặt APP KHÁCH (route-group `(app)`, shell = Sidebar + Topbar)
```
/home                     Trang chủ (tổng quan + đếm bản ghi)
/[entity]                 Danh sách (LayoutList — canonical)
/[entity]/[id]            Chi tiết (LayoutDetail)      /[entity]/new · /[entity]/[id]/edit  Form (LayoutForm)
/[entity]/import          Import CSV   ← nối nút "Nhập dữ liệu" ở LayoutList (đúng handoff màn 1)
/[entity]/deleted         Thùng rác    ← nối từ menu ⋯ của LayoutList
/dashboards[/[id]]        Xem dashboard  ·  /reports[/[id]]  Xem báo cáo
/approvals                Hộp phê duyệt  ·  /notifications  ·  /profile  ·  /settings (prefs cá nhân)
/entities                 Danh mục entity
/admin/{users,roles,teams,audit,field-acl,sharing}   Quản trị tenant (admin-only)
/login · /forgot-password · /reset-password           (auth, ngoài shell — NỐI backend #1 đã có)
Redirect giữ: / → /home · /admin → /admin/users · /audit · /dashboard · /sharing
```
### 3.2 Mặt STUDIO (route-group `studio/`, shell = StudioShell, admin-only, tông phân biệt)
```
/studio                   Home nhà-máy (pipeline dẫn dắt: Model → Quan hệ → Bố cục → Workflow → Phê duyệt → Báo cáo/Dashboard → Deploy)
/studio/model · /studio/relationships · /studio/layout · /studio/workflows · /studio/approvals
/studio/reports · /studio/dashboards · /studio/verticals (phần export real; catalog mock → label rõ)
/studio/preview           Cầu xem-trước App (thay /preview cũ; banner "Bản xem trước")
/studio/ai (đã xoá, specs/076 ai11) · /studio/deploy   HONEST-DISABLED (label "đang phát triển", không theater)
```
### 3.3 Luật ranh giới 2 mặt
- Cầu DUY NHẤT: `ModeSwitch` (admin-only) + `/studio/preview`. CommandPalette của App **bỏ** lệnh
  `/studio/*` (admin muốn dựng thì switch mặt — giữ mạch "đang ở app nào").
- `Topbar` tách: `AppTopbar` (⌘K, chuông, org) vs `StudioTopbar` (breadcrumb builder, nút preview).
  Dùng chung phần user-menu qua primitive `NavShell`.
- Xoá/không ship: `/detail /form /atlas /live /[entity]/preview` (theo §2.C; `/live` xoá — demo đã
  hết vai, LayoutList thay thế).

## 4. Design-system một-nguồn (codify cái đã có + bù cái thiếu)

**Đã CÓ (giữ nguyên, là chuẩn):** token OKLCH 3 tầng trong `globals.css` (`--ac/--ac2/--acd/--acb`,
`--bg/--panel/--panel2`, `--bd*`, `--fg/--mut/--fnt`, semantic `--ok/--cy/--wn/--dg/--neu` + nền `*d`);
3 theme tenant (`.theme-emerald|cobalt|terracotta`); dark `[data-theme]` pre-paint; font Be Vietnam Pro
(body — Geist thiếu subset VN) + Source Serif 4 (display) + Geist Mono (số/ID/mã); radius `--r-*`;
`lib/errorMessages.ts` + `ApiError`; Toaster.

**Bù (E3 wave-3):**
1. **Type-scale semantic** vào `@theme`: `--text-display 30px · --text-h1 22px · --text-title 15px ·
   --text-body 13px · --text-sub 12px · --text-caption 11px` (khớp thang handoff, min 10px, line-height
   ôm dấu ặ/ỹ/ữ) → codemod ~1.200 `text-[NNpx]` về class scale; cấm arbitrary size mới (grep-gate).
   > ✅ **Codemod xong 2026-09-01** (nợ Phase-5 specs/010 §7): 2.320 `text-[Npx]` → 0. Ràng buộc
   > "không đổi render" ⇒ chỉ 1.077 chỗ khớp px chính xác về được 6 token semantic; 1.243 chỗ còn
   > lại (nửa-pixel 12.5/11.5/10.5… là chính) về **21 token interim** `--text-<N>px`/`--text-<N>-5px`
   > cùng giá trị px (danh sách + lý do: comment `@theme` trong `globals.css`). Token KHÔNG khai
   > `--line-height` companion — chỉ emit `font-size`, leading giữ nguyên như arbitrary cũ.
   > Gate: `test/type-scale-gate.test.ts` (trong `vitest run`) cấm `text-[N…]` mới + pin tập token.
   > Nợ còn lại của mục này: **hội tụ interim → semantic** (ĐỔI RENDER — cần design-pass duyệt mắt).
2. **Primitives** (§2.B): `NavShell · EmptyState · Skeleton(mirror-cột) · Pagination(offset+cursor) ·
   DataTable · Modal · Drawer · Menu · FieldBadge` — mỗi cái ăn token, có states chuẩn.
3. **States chuẩn bắt buộc mọi màn:** loading=skeleton mirror layout (không spinner trần) · empty có
   CTA · error dùng `errorMessages` + retry · disabled có tooltip lý do (honest) · 401→`SessionExpiredGuard`
   · 409→conflict banner (đã có ở LayoutForm — thành pattern chung).
4. **Số/tiền VN:** `tabular-nums` + mono; `3.500.000.000 VNĐ`, rút gọn `3,50 tỷ VNĐ`; ngày `DD/MM/YYYY`.
5. **Dials anti-slop (app plane):** VARIANCE 4-5 · MOTION 3 · DENSITY 6-7; cấm tell (gradient tím,
   3-card đều, emoji heading, radius>16 tràn lan, glow dark…); dark = elevation-by-lightness.
6. **A11y gate:** contrast ≥4.5:1 (kể cả placeholder), focus ring `0 0 0 3px var(--acd)`,
   `prefers-reduced-motion`, focus-trap cho Modal/Drawer.

## 5. Patterns chuẩn (map lỗi & luồng)
- **Form:** RHF+zod; map `ApiError.details.field` (kể cả path composite `fullname.first`) → field error;
  422 codes theo `docs/api-conventions.md`; reward-early/punish-late.
- **Table:** LayoutList là bản chuẩn (offset + "Tải thêm"); wave-4 thêm cursor `after`/`nextCursor`
  (L2-HARDEN #3); filter chip faceted persist URL.
- **409:** banner + "Xem khác biệt"/"Tải lại" (LayoutForm pattern) — không ghi đè im lặng.
- **401:** SessionExpiredGuard → re-login giữ deep-link. **403 FIELD_FORBIDDEN:** toast + disable field.
- **Breadcrumb/back:** shell render từ route segment; không màn nào dead-end (luôn có đường về list/home).

## 6. Kế hoạch thực thi E3 (wave nhỏ, mỗi wave gate xanh + browser-verify)
| Wave | Việc (map TOP-10 của E1) | Gate |
|---|---|---|
| W1 — nối & sửa link (S) | TOP-10 #1 (import/deleted vào LayoutList; xoá `/live`) · #2 (CommandPalette mislabel) · #4 (redirect `/detail` `/form`, sửa `fullHref`) | build + click-thử từng link |
| W2 — dọn mock & 2 mặt (M) | #5 `/preview`→`/studio/preview` · #6 xoá `/atlas`+cụm mock+fixtures · bỏ lệnh studio khỏi palette · #10 nối forgot/reset vào backend #1 | build + hành trình login/forgot |
| W3 — shell + primitives (M-L) | #7 route-group hoá · #8 NavShell/primitives · tách App/Studio Topbar | build + E5 hai hành trình đầu-cuối |
| W4 — polish hệ (M) | #9 type-scale codemod + sweep màu · states chuẩn hoá · L2-HARDEN (inline-edit? ACL-chip, cursor, Save&new) | design pre-flight + browser-verify |

## 7. Security
- `/studio/*` + `/admin/*`: RequireAdmin ở layout (đang page-level rời rạc → dời lên layout, fail-closed).
- Không render tính năng theo role bằng CSS-ẩn — không mount (menu admin đã đúng, giữ).

## 8. Testing / E5 acceptance
- `npm run build` + `tsc` xanh mỗi wave; console sạch, network không 4xx/5xx lạ (browser-verify).
- **Hành trình App:** login → /home → /accounts → detail → edit(409 thử) → dashboard → notification — chụp liền mạch.
- **Hành trình Studio:** switch → model tạo entity → field → layout → preview → (deploy honest-disabled).
- Pre-flight anti-slop checklist mỗi màn đụng tới.

## 9. Tự-soi spec (step 7)
- TBD còn lại: (a) `/studio/ai` giữ mock-labeled hay ẩn hẳn — **đề xuất honest-disabled, chờ Founder
  nếu muốn khác**; (b) inline-edit Detail (L2-HARDEN #1) in/out — đề xuất OUT wave này (form edit đủ).
- Không mâu thuẫn với handoff/anti-slop đã phát hiện; scope mỗi wave ≤ 1 tuần-người, độc lập deploy.

---

## 10. E2b — Tu chính theo chỉ đạo Founder 2026-07-02: BRAND "NEXUS BE" (beige)

> Founder: *"theme của app thì màu be kết hợp cho dễ nhìn; logo Nexus; hiệu ứng trải nghiệm
> top-tier; icon chỉn chu; landing page đẹp chuyên nghiệp; sắp xếp lại bố cục + thêm menu/chức
> năng tối ưu."* — Tu chính này GHI ĐÈ hướng thẩm mỹ §1 ("không đổi hướng thẩm mỹ" hết hiệu lực
> cho palette), GIỮ NGUYÊN toàn bộ kiến trúc token một-nguồn + IA 2 khuôn mặt + primitives.

### 10.1 Palette "Nexus Be × Ngọc" (đổi GIÁ TRỊ biến trong globals.css, không đổi TÊN biến)

> ⚠️ **Founder 2026-07-02: BỎ TÍM HOÀN TOÀN → thay XANH NGỌC (teal/jade).** Không còn
> violet/purple ở bất kỳ đâu (accent, illustration, badge, gradient, logo). Grep hue tím
> `277|285` + `violet|purple|indigo` toàn nexus-web → dọn sạch.

Nguyên tắc: nền be ấm (hue OKLCH ~85), chữ nâu-than ấm, accent **XANH NGỌC** (teal hue ~190) —
be + ngọc = premium ấm-lạnh cân bằng. Ngọc (190) nằm giữa success-green (ok=150) và info-cyan
(cy=245): phân biệt bằng NGỮ CẢNH (accent = phần tử tương tác nút/link/selected; ok = badge
thành công tint-xanh-lá; cy = badge info). Nếu soi thật thấy accent lẫn ok/cy → nudge cy về 250
(bluer) để tách; KHÔNG đổi ok. Semantic colors giữ hue, chỉnh nhẹ độ ấm nếu cần.

```css
/* LIGHT (default) — thay thế các giá trị hiện tại */
--ac:  oklch(0.54 0.095 190);         /* xanh ngọc — primary actions (white text ≥4.5:1) */
--ac2: oklch(0.47 0.090 190);         /* hover, đậm hơn */
--acf: #fff;                          /* chữ trên accent */
--acd: oklch(0.54 0.095 190 / 0.10);  /* accent mờ — nền nhạt / selected row */
--acb: oklch(0.54 0.095 190 / 0.28);  /* accent border */
--bg:     oklch(0.972 0.012 85);      /* be nền trang (~#F7F2E9) */
--panel:  oklch(0.993 0.005 90);      /* thẻ/bảng — trắng ấm (~#FEFCF7) */
--panel2: oklch(0.958 0.014 85);      /* bề mặt phụ / header row */
--bd:  oklch(0.905 0.020 85);         /* viền chuẩn ấm */
--bd2: oklch(0.940 0.014 85);
--bd3: oklch(0.800 0.024 82);
--fg:  oklch(0.26 0.020 70);          /* nâu-than ấm (~#3B342B) */
--mut: oklch(0.47 0.022 74);
--fnt: oklch(0.59 0.020 76);
/* DARK — parity ấm (không đen tuyền): nền nâu-than */
--bg:     oklch(0.205 0.012 75);      /* (~#1E1A15) */
--panel:  oklch(0.245 0.013 75);
--panel2: oklch(0.275 0.014 75);
--bd:  oklch(0.34 0.016 75);  --bd2: oklch(0.30 0.014 75);  --bd3: oklch(0.46 0.018 75);
--fg:  oklch(0.93 0.012 85);  --mut: oklch(0.72 0.014 80);  --fnt: oklch(0.60 0.014 78);
--ac trong dark: oklch(0.68 0.10 190) (xanh ngọc sáng hơn để đạt contrast trên nền nâu-than tối).
```
- **Contrast bắt buộc:** fg/bg ≥ 12:1, mut/bg ≥ 6:1, acf/ac ≥ 4.5:1, chữ trên acd ≥ 4.5:1 —
  verify bằng screenshot cả 2 mode trước khi merge.
- `.theme-terra` (be + đồng) để dành làm theme per-tenant TÙY CHỌN sau — KHÔNG làm wave này.

### 10.2 Logo Nexus
- **Glyph "N-node":** chữ N dựng từ 2 cột + 1 đường chéo nối 4 node tròn (ẩn dụ metadata-graph
  nối entity) — SVG thuần, stroke 2px, 1 màu `currentColor` (tự ăn --fg/--acf theo ngữ cảnh).
- **Wordmark:** "Nexus" — font hệ hiện có (Geist/Inter theo stack), weight 650, tracking -2%;
  cạnh glyph khoảng cách = chiều rộng 1 node.
- Files: `components/brand/NexusLogo.tsx` (glyph + wordmark, props size/variant), favicon
  `app/icon.svg` (glyph đơn), dùng ở: Topbar App, StudioShell, /login, landing, README.
- KHÔNG dùng ảnh raster/AI-gen; SVG tay, đơn giản, đo lưới 24px.

### 10.3 Landing page `/` (public, trước login)
- Route `(marketing)/page.tsx` — SSG tĩnh, KHÔNG gọi API, KHÔNG fake số liệu khách.
- Sections (nội dung THẬT theo VISION — engine nội bộ Nexus, không phải SaaS bán lẻ):
  1. Hero: logo + tagline "Metadata-driven platform — dựng ứng dụng nghiệp vụ trong ngày" +
     CTA "Đăng nhập" (+ CTA phụ "Xem Studio" cho admin đã đăng nhập).
  2. "Nhà máy 90/10": 3 bước ngắn (Model hoá → Lắp metadata → Bàn giao) — icon line-art.
  3. Năng lực engine: grid 6 thẻ (Entity động · Quan hệ/Computed · ACL 3 tầng · Workflow ·
     Reporting · Import) — mỗi thẻ icon + 1 câu, KHÔNG số liệu bịa.
  4. Verticals: CRM (live) + LMS/Ticketing (roadmap, ghi rõ "roadmap" — honest).
  5. Footer: bản quyền Nexus, link docs nội bộ.
- Motion: fade-up nhẹ theo scroll (IntersectionObserver, prefers-reduced-motion tôn trọng),
  KHÔNG parallax/particle/gradient-mesh (anti-slop).
- Login route chuyển thành `/login` (đã có), `/` public landing; user đã auth vào `/` → redirect
  `/home`.

### 10.4 Icon & motion top-tier (luật, áp mọi màn)
- Icon: Lucide DUY NHẤT, size 16/20 theo ngữ cảnh, strokeWidth 1.75 thống nhất (sweep codemod);
  không emoji trong UI chrome (trừ nội dung user).
- Motion: 150–200ms ease-out cho hover/press; 250ms cho overlay (Modal/Drawer đã có primitives);
  danh sách stagger ≤ 40ms/item, tối đa 6 item đầu; page-transition fade 120ms; tôn trọng
  `prefers-reduced-motion` toàn cục (wrapper 1 chỗ).
- Skeleton thay spinner ở list/detail (đã có pattern — sweep nốt màn còn thiếu).

### 10.5 IA bổ sung (menu/chức năng "thêm này nọ" — giữ luật 2 mặt §3.3)
- App sidebar: thêm nhóm "Ghim" (pin entity hay dùng — localStorage per-user) + mục "Import"
  hiện có nổi cấp 1; recent-items (5 record vừa mở) dưới ⌘K.
- Studio: thêm "Chuỗi dựng app" (guided pipeline stepper Entity→Field→Layout→Relationship→
  Workflow→Deploy — E-mandate cũ) như trang `/studio` index thay landing rời rạc hiện tại.
- KHÔNG thêm route mới ngoài `(marketing)`; mọi bổ sung nằm trong shell hiện có.

### 10.6 Thứ tự thực thi (wave E3-W5, mỗi bước build xanh + browser-verify)
1. **W5a Token be** (globals.css light+dark, GIỮ tên biến) + contrast screenshots.
2. **W5b Logo** (NexusLogo.tsx + icon.svg + đặt vào Topbar/Studio/login).
3. **W5c Landing** `(marketing)` + redirect logic.
4. **W5d Icon/motion sweep** + IA bổ sung (Ghim/recent/pipeline stepper).
- Browser-verify đủ 2 mode sáng/tối mỗi bước; anti-slop pre-flight từng màn.

---

## 11. E2c — HỆ THỐNG ONBOARDING & TRỢ GIÚP TRONG SẢN PHẨM (Founder 2026-07-02)

> Founder: *"phải có luôn mục giới thiệu chi tiết về Nexus, hướng dẫn sử dụng, workflow thế nào,
> từng face; icon chấm hỏi hover vào giải thích cái đang rê chuột — cho dễ làm quen."*
> Mục tiêu: người mới (team Nexus nội bộ + khách được onboard) hiểu Nexus trong 5 phút, và
> mọi phần tử lạ đều tự-giải-thích tại chỗ. KHÔNG fake — nội dung THẬT, khớp năng lực đã build.

### 11.1 Trang "Giới thiệu Nexus" `(app)/about` + Studio `/studio/about`
- Một trang nội-dung (MDX/TSX tĩnh, KHÔNG API) — mở từ menu + từ icon `?` ở topbar.
- Nội dung (bám VISION-AND-DELIVERY — engine nội bộ Nexus, nhà-máy 90/10):
  1. **Nexus là gì**: engine metadata-driven sinh app nghiệp vụ; không hardcode module.
  2. **Hai khuôn mặt**: STUDIO (team Nexus dựng app) ↔ APP KHÁCH (người dùng cuối chạy nghiệp vụ)
     — sơ đồ + khi nào dùng cái nào + cách switch.
  3. **Workflow nhà-máy 8 bước** (Discovery → Model entity → Field → Layout → Relationship →
     Workflow/Approval → Report → Deploy) — mỗi bước 1 câu + link tới màn Studio tương ứng.
  4. **Năng lực engine** (link tới ENGINE-COVERAGE): field types, quan hệ 1-N/N-N/1-1,
     computed (lookup/rollup/formula), auto_number, ACL 3 tầng, workflow, reporting, import, MFA.
  5. **Glossary**: entity, field, layout, relationship, tenant, record, team-set — mỗi thuật
     ngữ 1 dòng (dùng chung nguồn với tooltip §11.3).

### 11.2 "Hướng dẫn theo khuôn mặt" (guided tour, tùy chọn, không ép)
- **App khách**: tour 5 bước lần đầu vào /home (list → mở record → sửa → dashboard → notification).
- **Studio**: tour pipeline dựng-app (tạo entity → thêm field → layout → preview → deploy).
- Cơ chế: overlay coach-marks nhẹ (tự viết, KHÔNG thêm dep tour nặng); "Bỏ qua"/"Không hiện lại"
  (localStorage per-user); có thể mở lại từ menu "Trợ giúp". prefers-reduced-motion tôn trọng.

### 11.3 Icon `?` + Tooltip ngữ cảnh (HelpHint) — luật áp toàn hệ
- Component **`components/help/HelpHint.tsx`**: icon `?` (Lucide `CircleHelp`, size 14, --mut),
  hover/focus → tooltip (primitive Tooltip đã có, hoặc bổ sung vào lib primitives §4) hiện 1-2
  câu giải thích. **A11y bắt buộc**: `aria-label` + focusable (keyboard mở được, không chỉ hover);
  `prefers-reduced-motion`; đóng bằng Esc; không chặn nội dung dưới.
- **Nguồn nội dung MỘT-NGUỒN**: `lib/help-content.ts` — map `key → {term, short, long?}`; tooltip
  đọc `short`, glossary/about đọc `long`. Sửa 1 chỗ, mọi nơi cập nhật. Tiếng Việt, ngắn, THẬT.
- **Đặt `?` ở đâu** (tối thiểu, không spam): mỗi nhãn field-type trong Studio field-builder;
  mỗi cột cấu hình lạ (cardinality, inverseField, options.padding/prefix, viaRelation, rollup
  metric); mỗi khái niệm ACL (owner/team/all/none, module vs field vs row); mỗi trạng thái
  record (409 conflict, soft-delete); header cột báo cáo. KHÔNG đặt `?` lên thứ đã hiển-nhiên.
- **Empty-state cũng dạy**: mỗi màn rỗng (chưa có entity/record/report) → 1 câu "cái này để làm
  gì" + CTA + link about — biến trạng thái rỗng thành onboarding.

### 11.4 Thực thi (wave E3-W5e — sau W5a-d beige/logo/landing)
1. `lib/help-content.ts` (nguồn thật, ~30-40 key khớp năng lực đã build) + `HelpHint` primitive.
2. Trang `/about` (App + Studio) — nội dung §11.1.
3. Rải `HelpHint` theo §11.3 (Studio builders trước — nơi thuật ngữ dày nhất).
4. Guided tour §11.2 (App + Studio) — làm sau cùng, tùy chọn.
- Gate: build xanh + browser-verify hover/focus/Esc/keyboard + a11y (axe nếu có) + 2 mode sáng/tối.
- Nguyên tắc: nội dung khớp CODE THẬT (đừng hứa tính năng chưa build); thứ chưa có → nói "roadmap".
