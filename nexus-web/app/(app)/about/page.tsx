import type { Metadata } from "next";
import { AboutScreen } from "@/components/screens/AboutScreen";

// (app)/about — "Giới thiệu Nexus" (E2-IA-DESIGN-SYSTEM.md §11.1). Mở từ Sidebar + icon "?"
// ở Topbar + CommandPalette. Trong shell (app) route-group (AppShell) — RequireAuth ở layout.
export const metadata: Metadata = {
  title: "Giới thiệu Nexus",
};

export default function AboutPage() {
  return <AboutScreen face="app" />;
}
