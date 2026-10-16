import type { Metadata } from "next";
import { Landing } from "@/components/screens/Landing";

// "/" — landing công khai, TRƯỚC login (E2-IA-DESIGN-SYSTEM.md §10.3). Thay route group
// (marketing) cho app/page.tsx cũ (vốn redirect thẳng /home — xem NAV-MAP.md). Route group
// không đổi URL: (marketing)/page.tsx vẫn map "/". SSG tĩnh — không gọi API ở tầng route.
//
// OpenGraph: og.png là crop 1200×630 từ hero-app.png (screenshot THẬT của app, tạo bằng sips
// — không asset bịa). metadataBase resolve URL tương đối; set NEXT_PUBLIC_SITE_URL ở môi
// trường deploy (fallback localhost chỉ để build/dev không cảnh báo thiếu base).
const SITE_URL = process.env.NEXT_PUBLIC_SITE_URL || "http://localhost:3000";
const TITLE = "Nexus — Nền tảng metadata-driven của Nexus";
const DESCRIPTION =
  "Nhà máy nội bộ Nexus dựng ứng dụng nghiệp vụ (CRM, LMS, Ticketing…) từ metadata — 90% engine, 10% custom.";
const OG_IMAGE = {
  url: "/landing/og.png",
  width: 1200,
  height: 630,
  alt: "Giao diện Nexus — ứng dụng CRM với danh sách cơ hội bán hàng",
};

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: TITLE,
  description: DESCRIPTION,
  openGraph: {
    type: "website",
    locale: "vi_VN",
    url: "/",
    siteName: "Nexus",
    title: TITLE,
    description: DESCRIPTION,
    images: [OG_IMAGE],
  },
  twitter: {
    card: "summary_large_image",
    title: TITLE,
    description: DESCRIPTION,
    images: [OG_IMAGE.url],
  },
};

export default function Page() {
  return <Landing />;
}
