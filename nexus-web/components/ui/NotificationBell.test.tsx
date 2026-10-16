// NotiItem — the bell row rendered what the backend never sent, and never rendered what it did.
//
// The BE writes `message` on EVERY notification (bpm arm, SLA breach, workflow, assignment) and
// `title` only sometimes — the column is nullable and several producers leave it unset. The FE type
// declared `title` as REQUIRED, rendered only `title`, and branched on `subject` / `actorName` /
// `actorInitials`, three fields no producer has ever written. Net effect: a notification with no
// title rendered a blank line, and NO notification ever showed its sentence — the bell was a type
// badge and a timestamp. Textbook "the join nobody wrote", and it survived because both ends were
// individually fine.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { NotiItem } from "@/components/ui/NotificationBell";
import type { ApiNotification } from "@/lib/api";

const noti = (over: Partial<ApiNotification> = {}): ApiNotification => ({
  id: "n1",
  type: "approval",
  message: "Một bản ghi crm_quotes đang chờ bạn phê duyệt.",
  isRead: false,
  createdAt: new Date().toISOString(),
  ...over,
});

describe("NotiItem", () => {
  it("shows the message even when the producer set NO title", () => {
    // Exactly what bpm.AsyncEngine and the SLA scan emit: message only.
    render(<NotiItem noti={noti()} />);
    expect(screen.getByText(/đang chờ bạn phê duyệt/)).toBeInTheDocument();
  });

  it("shows BOTH when a title is present — the headline never replaces the sentence", () => {
    render(<NotiItem noti={noti({ title: "Được uỷ quyền phê duyệt" })} />);
    expect(screen.getByText("Được uỷ quyền phê duyệt")).toBeInTheDocument();
    expect(screen.getByText(/đang chờ bạn phê duyệt/)).toBeInTheDocument();
  });

  it("renders the type label, so the badge did not regress while fixing the body", () => {
    render(<NotiItem noti={noti()} />);
    // The approval branch of TYPE_META must still be reachable (the type column was itself a bug
    // once: every row was NULL and all five icon branches were dead).
    expect(screen.getByText("Cần duyệt")).toBeInTheDocument();
  });
});
