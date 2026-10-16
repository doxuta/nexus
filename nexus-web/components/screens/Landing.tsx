// Landing — trang chủ công khai "/" (E2-IA-DESIGN-SYSTEM.md §10.3). SSG tĩnh, KHÔNG gọi API,
// KHÔNG số liệu khách bịa đặt. Nexus là ENGINE/nhà-máy NỘI BỘ Nexus để dựng phần mềm cho khách
// (không phải SaaS tự đăng ký) — copy phản ánh đúng vị thế này, không viết kiểu "Sign up free".
// Server Component (không "use client") — 2 island client nhỏ (MarketingAuthGate, Reveal) là
// phần tương tác duy nhất, phần còn lại render tĩnh 100%.
import Link from "next/link";
import Image from "next/image";
import {
  Boxes, Blocks, PackageCheck, Database, Network, ShieldCheck, Zap, BarChart3, Upload,
  ArrowRight,
} from "lucide-react";
import { NexusLogo } from "@/components/brand/NexusLogo";
import { StatusBadge } from "@/components/ui/bits";
import { Reveal } from "@/components/marketing/Reveal";
import { MarketingAuthGate } from "@/components/marketing/MarketingAuthGate";
import { BrowserFrame } from "@/components/marketing/BrowserFrame";
import heroApp from "@/public/landing/hero-app.png";

const ICON_STROKE = 1.75; // §10.4: Lucide line-art strokeWidth 1.75 thống nhất trên landing

const FACTORY_STEPS = [
  {
    n: "01",
    icon: Boxes,
    title: "Model hoá",
    body: "Đưa nghiệp vụ khách hàng thành entity, field, quan hệ — không viết code cứng cho từng dự án.",
  },
  {
    n: "02",
    icon: Blocks,
    title: "Lắp metadata",
    body: "Layout, workflow, phân quyền, báo cáo lắp từ metadata — engine tự sinh API và giao diện.",
  },
  {
    n: "03",
    icon: PackageCheck,
    title: "Bàn giao",
    body: "Vertical chạy được ngay trên cùng một lõi — mở rộng sang ngành khác không viết lại từ đầu.",
  },
];

const CAPABILITIES = [
  {
    icon: Database,
    title: "Entity động",
    body: "Tạo entity và field mới không cần deploy lại — schema sống trong metadata, không hardcode struct.",
  },
  {
    icon: Network,
    title: "Quan hệ · trường tính toán",
    body: "Quan hệ 1-1 / 1-N / N-N và trường tính toán tự cập nhật theo dữ liệu liên quan.",
  },
  {
    icon: ShieldCheck,
    title: "ACL 3 tầng",
    body: "Phân quyền theo module, hành động, và từng bản ghi (chia sẻ theo team).",
  },
  {
    icon: Zap,
    title: "Workflow",
    body: "Tự động hoá theo sự kiện — cập nhật trường, giao việc, gửi thông báo.",
  },
  {
    icon: BarChart3,
    title: "Reporting",
    body: "Báo cáo và dashboard dựng từ cùng lớp metadata với dữ liệu nghiệp vụ, không cần công cụ BI riêng.",
  },
  {
    icon: Upload,
    title: "Import",
    body: "Nhập dữ liệu CSV có ánh xạ trường và xem trước lỗi trước khi ghi vào hệ thống.",
  },
];

export function Landing() {
  return (
    <div className="min-h-screen bg-bg">
      <MarketingAuthGate />

      {/* Nav tối giản — brand + CTA. Sticky + backdrop-blur nhẹ: nội dung cuộn dưới nav
          vẫn đọc được ngữ cảnh, nav không "trôi mất" trên trang dài (token bg-bg/85, không màu mới). */}
      <header className="sticky top-0 z-40 border-b border-bd bg-bg/85 backdrop-blur-sm">
        <div className="mx-auto flex h-16 max-w-[1120px] items-center justify-between px-6">
          <NexusLogo size={26} />
          <nav className="flex items-center gap-5">
            <Link
              href="/login?next=/studio"
              className="hidden text-body font-medium text-mut transition-colors hover:text-fg sm:block"
            >
              Xem Studio
            </Link>
            <Link
              href="/login"
              className="flex h-9 items-center rounded-input border border-bd px-3.5 text-body font-medium text-fg transition-colors hover:border-ac hover:text-ac"
            >
              Đăng nhập
            </Link>
          </nav>
        </div>
      </header>

      {/* Hero — KHÔNG bọc Reveal: nội dung above-the-fold phải hiện ngay ở lần vẽ đầu tiên,
          scroll-reveal chỉ dành cho nội dung xuất hiện KHI cuộn tới (§10.3), không phải hero.
          Cấu trúc Vercel/Stripe: headline → 1 dòng sub → CTA → screenshot THẬT của app bên dưới,
          đè nhẹ lên section kế tiếp (âm margin + z-10). */}
      <section className="mx-auto max-w-[1120px] px-6 pt-16 sm:pt-24">
        <div className="mx-auto max-w-[780px] text-center">
          <h1 className="text-balance font-display text-34px font-semibold leading-[1.15] tracking-tight text-fg sm:text-48px">
            Dựng ứng dụng nghiệp vụ trong ngày, không phải hàng tháng.
          </h1>
          <p className="mx-auto mt-5 max-w-[600px] text-pretty text-title leading-relaxed text-mut sm:text-16px">
            Nexus là nền tảng metadata-driven — nhà máy nội bộ của Nexus để dựng CRM, LMS,
            Ticketing… và các ứng dụng nghiệp vụ khác từ metadata, trên cùng một lõi engine.
          </p>
          <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
            <Link
              href="/login"
              className="flex h-10 items-center gap-1.5 rounded-input bg-ac px-4 text-13-5px font-medium text-acf transition-colors hover:bg-ac2"
            >
              Đăng nhập
              <ArrowRight className="size-4" strokeWidth={ICON_STROKE} />
            </Link>
            <a
              href="#nang-luc"
              className="flex h-10 items-center rounded-input border border-bd px-4 text-13-5px font-medium text-fg transition-colors hover:border-ac hover:text-ac"
            >
              Tìm hiểu năng lực
            </a>
          </div>
        </div>

        {/* Screenshot thật (public/landing/hero-app.png — app CRM đang chạy), khung browser-chrome.
            Âm margin dưới + pt lớn ở section kế = ảnh "gối" lên đường ranh section (Stripe-style). */}
        <div className="relative z-10 mx-auto -mb-16 mt-12 max-w-[1000px] sm:-mb-24 sm:mt-16">
          <BrowserFrame>
            <Image
              src={heroApp}
              alt="Giao diện thật của Nexus — ứng dụng CRM với sidebar theo app và danh sách cơ hội bán hàng"
              priority
              placeholder="blur"
              sizes="(min-width: 1064px) 1000px, 92vw"
              className="w-full"
            />
          </BrowserFrame>
        </div>
      </section>

      {/* Nhà máy 90/10 — pt lớn nhường chỗ cho screenshot hero đè xuống */}
      <section className="border-t border-bd bg-panel">
        <div className="mx-auto max-w-[1120px] px-6 pb-16 pt-32 sm:pb-20 sm:pt-44">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">
              NHÀ MÁY 90/10
            </p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">
              90% mỗi dự án thừa hưởng engine, 10% là custom đặc thù ngành.
            </h2>
          </Reveal>

          <div className="mt-10 grid grid-cols-1 gap-8 sm:grid-cols-3 sm:gap-0">
            {FACTORY_STEPS.map((step, i) => {
              const Icon = step.icon;
              return (
                <Reveal
                  key={step.n}
                  delayMs={Math.min(i, 5) * 40}
                  className={i > 0 ? "sm:border-l sm:border-bd sm:pl-8" : ""}
                >
                  <div className="flex items-center gap-2.5">
                    <Icon className="size-5 text-ac" strokeWidth={ICON_STROKE} />
                    <span className="font-mono text-sub text-fnt">{step.n}</span>
                  </div>
                  <h3 className="mt-3 text-title font-semibold text-fg">{step.title}</h3>
                  <p className="mt-1.5 text-13-5px leading-relaxed text-mut">{step.body}</p>
                </Reveal>
              );
            })}
          </div>
        </div>
      </section>

      {/* Năng lực engine — id cho CTA "Tìm hiểu năng lực"; scroll-mt né nav sticky */}
      <section id="nang-luc" className="scroll-mt-16 border-t border-bd">
        <div className="mx-auto max-w-[1120px] px-6 py-16 sm:py-20">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">
              NĂNG LỰC ENGINE
            </p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">
              Một lõi metadata, nhiều ứng dụng nghiệp vụ.
            </h2>
          </Reveal>

          <div className="mt-10 grid grid-cols-1 gap-x-8 gap-y-9 sm:grid-cols-2 lg:grid-cols-3">
            {CAPABILITIES.map((cap, i) => {
              const Icon = cap.icon;
              return (
                <Reveal key={cap.title} delayMs={Math.min(i, 5) * 40} className="border-t border-bd pt-5">
                  <Icon className="size-5 text-ac" strokeWidth={ICON_STROKE} />
                  <h3 className="mt-3 text-14-5px font-semibold text-fg">{cap.title}</h3>
                  <p className="mt-1.5 text-13-5px leading-relaxed text-mut">{cap.body}</p>
                </Reveal>
              );
            })}
          </div>
        </div>
      </section>

      {/* Verticals — cả 3 (CRM · Ticketing · LMS) đều đã dựng và chạy trên cùng engine */}
      <section className="border-t border-bd bg-panel">
        <div className="mx-auto max-w-[1120px] px-6 py-16 sm:py-20">
          <Reveal>
            <p className="font-mono text-caption font-medium tracking-wider text-fnt">
              VERTICAL
            </p>
            <h2 className="mt-2 font-display text-24px font-semibold text-fg sm:text-28px">
              Ba vertical chạy trên cùng một engine.
            </h2>
          </Reveal>

          <div className="mt-8 space-y-4">
            <Reveal className="flex flex-col justify-between gap-4 rounded-card border border-bd bg-bg p-6 sm:flex-row sm:items-center">
              <div>
                <h3 className="text-title font-semibold text-fg">CRM — Bán hàng &amp; quan hệ khách hàng</h3>
                <p className="mt-1 text-13-5px text-mut">
                  Vertical đầu tiên chạy trên engine Nexus — account, quan hệ, phân quyền, báo cáo.
                </p>
              </div>
              <StatusBadge label="Đang chạy" tone="ok" />
            </Reveal>

            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <Reveal delayMs={40} className="flex items-center justify-between gap-4 rounded-card border border-bd bg-bg p-6">
                <div>
                  <h3 className="text-14px font-semibold text-fg">Ticketing — Hỗ trợ &amp; xử lý yêu cầu</h3>
                  <p className="mt-1 text-body text-mut">Phiếu hỗ trợ, SLA, phân loại và định tuyến — chạy trên cùng engine.</p>
                </div>
                <StatusBadge label="Đang chạy" tone="ok" />
              </Reveal>
              <Reveal delayMs={80} className="flex items-center justify-between gap-4 rounded-card border border-bd bg-bg p-6">
                <div>
                  <h3 className="text-14px font-semibold text-fg">LMS — Quản lý đào tạo</h3>
                  <p className="mt-1 text-body text-mut">Khoá học, học viên, ghi danh và bảng điểm — chạy trên cùng engine.</p>
                </div>
                <StatusBadge label="Đang chạy" tone="ok" />
              </Reveal>
            </div>
          </div>
        </div>
      </section>

      {/* Footer */}
      <footer className="border-t border-bd">
        <div className="mx-auto flex max-w-[1120px] flex-col items-start justify-between gap-2 px-6 py-8 text-12-5px text-fnt sm:flex-row sm:items-center">
          <p>© 2026 Nexus — Nexus là nền tảng nội bộ, không phải sản phẩm SaaS bán lẻ.</p>
          <Link href="/login" className="text-fnt transition-colors hover:text-ac">
            Đăng nhập
          </Link>
        </div>
      </footer>
    </div>
  );
}
