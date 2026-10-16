// SLDS avatar (blueprint `avatar` base + initials, SLDS 2.264.1). Order of preference, as the blueprint
// docs say: image → initials → object icon.
//   image:    <span class="slds-avatar [slds-avatar_circle] [slds-avatar_<size>]"><img alt title/></span>
//   initials: <span class="slds-avatar …"><abbr class="slds-avatar__initials slds-icon-standard-<c>" title>AB</abbr></span>
//   icon:     <span class="slds-avatar …"><span class="slds-icon_container slds-icon-standard-<c>">…</span></span>
// The profile-image / group-image variants are not wrapped: they paint SLDS assets/images (CC BY-ND).
// Embedded (specs/073 FR-005): draws nothing, the name already sits next to it as text.
"use client";

import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import { User } from "lucide-react";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { Icon } from "@/components/slds/Icon";

/** SLDS rule: two or more words → first letter of the first and last word; one word → "Xx". */
export function avatarInitials(name: string): string {
  const words = name.normalize("NFC").trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return "";
  const first = (w: string) => Array.from(w)[0].toLocaleUpperCase("vi");
  if (words.length > 1) return first(words[0]) + first(words[words.length - 1]);
  const [a, b = ""] = Array.from(words[0]);
  return a.toLocaleUpperCase("vi") + b.toLocaleLowerCase("vi");
}

export interface AvatarProps {
  /** Person or record name — alt/title text and the source of the initials. */
  name: string;
  src?: string;
  /** SLDS default is the rounded square (records); use "circle" for people. */
  shape?: "square" | "circle";
  size?: "x-small" | "small" | "medium" | "large";
  /** Object icon tile shown instead of initials (blueprint "Fallback - Entity/User Icon"). */
  icon?: LucideIcon;
  /** `slds-icon-standard-<colorName>` behind initials/icon; default "user" for circles, "account" for squares. */
  colorName?: string;
  /** Grey initials on a light background (`slds-avatar__initials_inverse`). */
  inverse?: boolean;
  className?: string;
}

export function Avatar({ name, src, shape = "square", size, icon, colorName, inverse, className }: AvatarProps) {
  const embedded = useEmbedded();
  if (embedded) return null;
  const color = colorName ?? (shape === "circle" ? "user" : "account");
  const initials = avatarInitials(name);
  let body;
  if (src) {
    // eslint-disable-next-line @next/next/no-img-element -- blueprint markup; arbitrary user/record image URLs
    body = <img alt={name} src={src} title={name} />;
  } else if (icon || !initials) {
    body = <Icon icon={icon ?? User} category="standard" name={color} size="medium" label={name || undefined} />;
  } else {
    body = (
      <abbr
        className={clsx("slds-avatar__initials", inverse ? "slds-avatar__initials_inverse" : `slds-icon-standard-${color}`)}
        title={name}
      >
        {initials}
      </abbr>
    );
  }
  return (
    <span className={clsx("slds-avatar", shape === "circle" && "slds-avatar_circle", size && `slds-avatar_${size}`, className)}>
      {body}
    </span>
  );
}
