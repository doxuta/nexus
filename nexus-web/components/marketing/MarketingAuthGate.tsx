"use client";

// MarketingAuthGate — user đã đăng nhập (token trong localStorage) mở "/" → điều hướng thẳng
// về /home thay vì thấy landing công khai (E2-IA-DESIGN-SYSTEM.md §10.3: "user đã auth vào /
// → redirect /home"). Chỉ đọc localStorage (isAuthed) — KHÔNG gọi API — nên landing vẫn SSG
// tĩnh cho người chưa đăng nhập/no-JS; đây là progressive-enhancement thuần client, cùng mẫu
// với RequireAuth (lib/auth/guards.tsx).
import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { isAuthed } from "@/lib/api";

export function MarketingAuthGate() {
  const router = useRouter();

  useEffect(() => {
    if (isAuthed()) router.replace("/home");
  }, [router]);

  return null;
}
