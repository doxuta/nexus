"use client";

// Tài khoản của tôi (specs/073 kh4 T276): the builder's own profile, password, two-step sign-in and sessions — the same
// Profile screen the customer face has (it already holds MfaCard). A `nha` page: inside a build session /auth answers 403,
// so the gate offers to go back to the workshop first.
import { Profile } from "@/components/screens/Profile";
import { homeSlug } from "@/lib/studioContext";
import { ContextGate } from "./ContextGate";

export function AccountPage() {
  return (
    <ContextGate slug={homeSlug()} ctx="nha">
      <div className="flex min-h-0 flex-1 flex-col">
        <Profile />
      </div>
    </ContextGate>
  );
}
