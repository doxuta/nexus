"use client";

// AboutScreen — "Giới thiệu Nexus" (E2-IA-DESIGN-SYSTEM.md §11.1). Nội dung TĨNH, KHÔNG gọi
// API record nào, KHÔNG số liệu bịa. F7-5 face-split theo NGƯỜI XEM (useAuth):
//   • admin/Nexus  → explainer đầy đủ (2 khuôn mặt + nhà máy 8 bước, có link /studio/*).
//   • non-admin   → hướng-dẫn-dùng-app NGẮN, zero link Studio — trước đây họ thấy pitch nhà
//     máy với link /studio/* và bị RequireAdmin âm thầm đá về /home (dead link).
// Client Component vì cần user.isAdmin từ AuthProvider.
//
// SLDS (specs/038 T032, RESEARCH §5 /about — no "about" blueprint, composed): page-headers/base,
// one `cards` card per section with `tiles` (media + utility icon) for the rows, `badges` for
// status, the glossary as `accordion/base`. The marketing Reveal fade-in and the NexusLogo hero
// glyph are gone — SLDS pages don't animate sections in, and the page header carries the icon.
import Link from "next/link";
import type { ReactNode } from "react";
import {
  Search, Boxes, Rows3, LayoutPanelLeft, GitFork, Zap, BarChart3, Rocket,
  Database, Network, Calculator, ShieldCheck, Upload, Wrench, AppWindow,
  KeyRound, LogIn, PanelLeft, CheckSquare, Bell, BookOpen, Info, Factory, Cpu, BookA, Layers,
} from "lucide-react";
import { allHelpEntries } from "@/lib/help-content";
import { useAuth } from "@/lib/auth/AuthProvider";
import { isMounted, studioHref, type RouteId } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { t } from "@/lib/i18n";
import { PageHeader } from "@/components/slds/PageHeader";
import { Card } from "@/components/slds/Card";
import { Badge } from "@/components/slds/Badge";
import { Icon } from "@/components/slds/Icon";
import { Accordion } from "@/components/slds/Accordion";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

// Nhà máy 8 bước (E2-IA-DESIGN-SYSTEM.md §11.1 điểm 3 — thứ tự CHỐT trong spec, không phải
// thứ tự nav Studio). Discovery không có màn Studio (bước con người, trước khi vào công cụ).
// `route` leads into the Studio project (builder only, see AboutScreen); `href` is a customer-face page.
const FACTORY_STEPS: Array<{
  n: string; icon: typeof Search; title: string; body: string; href?: string; route?: RouteId;
}> = [
  {
    n: "01", icon: Search, title: "Discovery",
    body: "Tìm hiểu nghiệp vụ khách hàng: đối tượng, quy trình, vai trò, báo cáo cần có — trước khi chạm tới Studio.",
  },
  {
    n: "02", icon: Boxes, title: "Bảng", route: "dung/du-lieu",
    body: "Định nghĩa các bảng dữ liệu của khách trong Studio — không cần deploy lại code.",
  },
  {
    n: "03", icon: Rows3, title: "Trường", route: "dung/du-lieu",
    body: "Thêm trường cho từng bảng: kiểu dữ liệu, bắt buộc, duy nhất, tuỳ chọn hiển thị.",
  },
  {
    n: "04", icon: LayoutPanelLeft, title: "Màn hình", route: "dung/man-hinh",
    body: "Sắp xếp màn hình nhập, danh sách và chi tiết — hệ thống tự vẽ cho App khách, không viết code.",
  },
  {
    n: "05", icon: GitFork, title: "Sơ đồ liên kết", route: "dung/du-lieu/so-do",
    body: "Nối các bảng bằng liên kết 1-1 / 1-N / N-N trên sơ đồ trực quan, kéo-thả để tạo N:N.",
  },
  {
    n: "06", icon: Zap, title: "Tự động & duyệt", route: "dung/tu-dong",
    body: "Tự động hoá theo sự kiện và luồng phê duyệt nhiều bước trước khi hành động có hiệu lực.",
  },
  {
    n: "07", icon: BarChart3, title: "Report", href: "/reports",
    body: "Dựng báo cáo tổng hợp và dashboard theo dõi sức khoẻ nghiệp vụ, cùng lớp metadata với dữ liệu.",
  },
  {
    n: "08", icon: Rocket, title: "Giao cho khách", route: "dung/giao-cho-khach",
    body: "Bàn giao cho khách vận hành trên App khách — đóng gói triển khai tự động đang phát triển.",
  },
];

const CAPABILITIES = [
  { icon: Database, title: "Entity & Field động", body: "Tạo entity, field mới không cần deploy lại — schema sống trong metadata, không hardcode struct." },
  { icon: Network, title: "Quan hệ 1-1 / 1-N / N-N", body: "Nối entity bằng quan hệ, xem trên sơ đồ trực quan; N-N tạo qua bảng trung gian." },
  { icon: Calculator, title: "Trường tính toán & số tự sinh", body: "Formula (công thức) đã dùng được trong Studio. Lookup, rollup, auto-number đã chạy ở backend — giao diện Studio đang được bổ sung." },
  { icon: ShieldCheck, title: "ACL 3 tầng", body: "Phân quyền theo module (entity × hành động), theo field (ẩn/chỉ-đọc), và theo từng bản ghi (team-set)." },
  { icon: LayoutPanelLeft, title: "Layout Engine", body: "Một bộ component render form/danh sách/chi tiết cho MỌI entity — sửa layout, không sửa code màn hình." },
  { icon: Zap, title: "Workflow & Phê duyệt", body: "Tự động hoá theo sự kiện (KHI → NẾU → THÌ) và luồng duyệt nhiều bước tuần tự trên bản ghi." },
  { icon: BarChart3, title: "Báo cáo & Dashboard", body: "Truy vấn tổng hợp (tổng/đếm/trung bình, nhóm theo field) và dashboard ghép nhiều widget." },
  { icon: Upload, title: "Nhập dữ liệu CSV", body: "Tải CSV, ánh xạ cột → field, xem trước lỗi trước khi ghi thật vào hệ thống." },
];

// Glossary — MỘT NGUỒN với tooltip HelpHint (lib/help-content.ts). Sắp theo tên thuật ngữ (vi).
const GLOSSARY = allHelpEntries().sort((a, b) => a.term.localeCompare(b.term, "vi"));

// ── Non-admin: how-to-use-your-app (F7-5) ──────────────────────────────────────
// SHORT + factual, zero /studio links (RequireAdmin would boomerang non-admins to /home).
const END_USER_GUIDE: Array<{ icon: typeof LogIn; title: string; body: string; href?: string }> = [
  {
    icon: LogIn, title: "Đăng nhập là vào việc",
    body: "Đăng nhập xong bạn vào thẳng app của tổ chức mình — màn hình, dữ liệu và quyền đã được đội triển khai chuẩn bị sẵn.",
  },
  {
    icon: PanelLeft, title: "Sidebar = dữ liệu của bạn",
    body: "Thanh bên trái liệt kê các loại dữ liệu bạn được xem và nhập. Bấm một mục để xem danh sách, bấm một dòng để xem chi tiết và sửa.",
  },
  {
    icon: Search, title: "Tìm kiếm & hỏi AI — ⌘K",
    body: "Nhấn ⌘K (Ctrl+K trên Windows) để tìm bản ghi theo tên trong mọi dữ liệu bạn được xem, hoặc hỏi AI về chính dữ liệu đó.",
  },
  {
    icon: CheckSquare, title: "Phê duyệt",
    body: "Bản ghi cần bạn duyệt xuất hiện ở Hàng chờ phê duyệt và trong \"Việc của tôi\" trên trang chủ. Duyệt hoặc từ chối kèm ghi chú.",
    href: "/approvals",
  },
  {
    icon: Bell, title: "Thông báo",
    body: "Chuông ở góc phải màn hình báo khi có việc mới cho bạn — yêu cầu phê duyệt, nhắc việc, cập nhật bản ghi.",
  },
];

/** One row: blueprint tiles "with icon" — object-colour tile, title (linked when it leads somewhere), body. */
function Row({ icon, color, title, body, href, eyebrow, extra }: {
  icon: typeof Search;
  /** `slds-icon-standard-<color>`; one colour per card. */
  color: string;
  title: string;
  body: string;
  href?: string;
  /** Small text before the title (factory step number). */
  eyebrow?: string;
  extra?: ReactNode;
}) {
  const heading = eyebrow ? `${eyebrow} · ${title}` : title;
  return (
    <article className="slds-tile slds-media">
      <div className="slds-media__figure">
        <Icon icon={icon} category="standard" name={color} size="small" />
      </div>
      <div className="slds-media__body">
        <h3 className="slds-tile__title" title={heading}>
          {href ? <Link href={href}>{heading}</Link> : heading}
        </h3>
        <div className="slds-tile__detail">
          <p>{body}</p>
          {extra}
        </div>
      </div>
    </article>
  );
}

/** Rows as a divided list; `li` wrappers keep `.slds-tile + .slds-tile` margins out of the list. */
function RowList({ children }: { children: ReactNode[] }) {
  return (
    <ul className="slds-has-dividers_top-space">
      {children.map((c, i) => (
        <li key={i} className="slds-item">{c}</li>
      ))}
    </ul>
  );
}

/** Section lead sentence under the card title. */
function Lead({ children }: { children: string }) {
  return <p className="slds-text-heading_small px-3 pb-3">{children}</p>;
}

function EndUserAbout() {
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="mx-auto flex w-full max-w-[720px] flex-col gap-4 p-4">
        <PageHeader
          icon={{ glyph: BookOpen, name: "knowledge" }}
          title={t("page.about.guide.title")}
          meta={t("page.about.guide.meta")}
        />
        <div>
          <Card title={t("page.about.guide.card")} icon={{ glyph: Info, name: "knowledge" }} padded={false}>
            <RowList>
              {END_USER_GUIDE.map((row) => (
                <Row key={row.title} icon={row.icon} color="knowledge" title={row.title} body={row.body} href={row.href} />
              ))}
            </RowList>
          </Card>
        </div>
        <ScopedNotification>{t("page.about.guide.note")}</ScopedNotification>
      </div>
    </div>
  );
}

export function AboutScreen({ face = "app" }: { face?: "app" | "studio" }) {
  const { user } = useAuth();
  // Non-admin (or auth still resolving) → end-user guide: no /studio links to dead-end on.
  // Admin → the full two-face + factory explainer below.
  if (!user?.isAdmin) return <EndUserAbout />;
  const isOperator = !!user.isPlatformOperator; // Studio links are for the builder, not for a customer admin
  const here = <Badge variant="lightest">{t("page.about.faces.current")}</Badge>;
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
      <div className="mx-auto flex w-full max-w-[880px] flex-col gap-4 p-4">
        <PageHeader
          icon={{ glyph: BookOpen, name: "knowledge" }}
          title={t("page.about.title")}
          meta={t("page.about.meta")}
        />

        {/* Cards stack with SLDS's own `.slds-card + .slds-card` spacing. */}
        <div>
          {/* 1. Nexus là gì */}
          <Card title={t("page.about.what.title")} icon={{ glyph: Info, name: "knowledge" }}>
            <div className="slds-text-longform">
              <h3 className="slds-text-heading_small">{t("page.about.what.lead")}</h3>
              <p>{t("page.about.what.body")}</p>
            </div>
          </Card>

          {/* 2. Hai khuôn mặt — the face this page was opened from carries the "here" badge. */}
          <Card title={t("page.about.faces.title")} icon={{ glyph: Layers, name: "apps" }}>
            <div className="slds-text-longform">
              <h3 className="slds-text-heading_small">{t("page.about.faces.lead")}</h3>
            </div>
            <ul className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <li>
                <Row
                  icon={Wrench}
                  color="apps-admin"
                  title={t("page.about.faces.studio.title")}
                  body={t("page.about.faces.studio.body")}
                  extra={
                    <>
                      <p className="slds-text-color_weak">{t("page.about.faces.studio.when")}</p>
                      {face === "studio" && here}
                    </>
                  }
                />
              </li>
              <li>
                <Row
                  icon={AppWindow}
                  color="app"
                  title={t("page.about.faces.app.title")}
                  body={t("page.about.faces.app.body")}
                  extra={
                    <>
                      <p className="slds-text-color_weak">{t("page.about.faces.app.when")}</p>
                      {face === "app" && here}
                    </>
                  }
                />
              </li>
            </ul>
            <p className="slds-text-color_weak mt-4">{t("page.about.faces.note")}</p>
          </Card>

          {/* 3. Workflow nhà máy 8 bước */}
          <Card title={t("page.about.factory.title")} icon={{ glyph: Factory, name: "work-queue" }} padded={false}>
            <Lead>{t("page.about.factory.lead")}</Lead>
            <RowList>
              {FACTORY_STEPS.map((step) => (
                <Row key={step.n} icon={step.icon} color="work-queue" eyebrow={step.n} title={step.title} body={step.body}
                  href={step.route ? (isOperator && isMounted(step.route) ? studioHref(activeProjectSlug(), step.route) : undefined) : step.href} />
              ))}
            </RowList>
          </Card>

          {/* 4. Năng lực engine */}
          <Card title={t("page.about.engine.title")} icon={{ glyph: Cpu, name: "metrics" }}>
            <div className="slds-text-longform">
              <h3 className="slds-text-heading_small">{t("page.about.engine.lead")}</h3>
            </div>
            <ul className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
              {CAPABILITIES.map((cap) => (
                <li key={cap.title}>
                  <Row icon={cap.icon} color="metrics" title={cap.title} body={cap.body} />
                </li>
              ))}
            </ul>
            <div className="mt-4">
              <ScopedNotification icon={KeyRound}>{t("page.about.engine.security")}</ScopedNotification>
            </div>
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <Badge theme="success">{t("page.about.engine.crm")}</Badge>
              <Badge>{t("page.about.engine.roadmap")}</Badge>
            </div>
          </Card>

          {/* 5. Glossary — one source with the "?" HelpHint tooltips. */}
          <Card title={t("page.about.glossary.title")} icon={{ glyph: BookA, name: "knowledge" }} padded={false}>
            <Lead>{t("page.about.glossary.lead")}</Lead>
            <Accordion
              multiple
              sections={GLOSSARY.map((e) => ({
                id: e.term,
                summary: e.term,
                content: <p>{e.long ?? e.short}</p>,
              }))}
            />
          </Card>
        </div>
      </div>
    </div>
  );
}
