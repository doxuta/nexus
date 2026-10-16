// lib/assistantOpen.ts — is the docked assistant open? (specs/076 ai3 T057)
// Two modes with their OWN state: docked (>=1280px, remembered in localStorage, open by default) and overlay (narrower,
// in memory, closed on every load — a panel laid over the page must never greet the maker unasked).
// An external store so the header button and the panel read one value (useSyncExternalStore).
import { useSyncExternalStore } from "react";

const KEY = "nexus.studio.assistantOpen"; // swept with the other nexus.studio.* keys on sign-out
const WIDE = "(min-width: 1280px)";
export const ASSISTANT_OPEN_EVENT = "nexus:assistant-open";

let overlayOpen = false;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((fn) => fn());

const isWide = (): boolean => {
  try {
    return window.matchMedia(WIDE).matches;
  } catch {
    return true;
  }
};
const storedDocked = (): boolean => {
  try {
    return window.localStorage.getItem(KEY) !== "0";
  } catch {
    return true;
  }
};

export const isAssistantOpen = (): boolean => (isWide() ? storedDocked() : overlayOpen);

export function setAssistantOpen(open: boolean): void {
  if (isWide()) {
    try {
      window.localStorage.setItem(KEY, open ? "1" : "0");
    } catch {
      /* blocked storage: the choice lasts until reload only through the emit below */
    }
  } else {
    overlayOpen = open;
  }
  emit();
}

export const toggleAssistant = (): void => setAssistantOpen(!isAssistantOpen());

export function subscribeAssistantOpen(fn: () => void): () => void {
  listeners.add(fn);
  const onCmd = () => setAssistantOpen(true);
  window.addEventListener(ASSISTANT_OPEN_EVENT, onCmd);
  let mq: MediaQueryList | null = null;
  try {
    mq = window.matchMedia(WIDE);
    mq.addEventListener("change", fn);
  } catch {
    /* no matchMedia: stays in one mode */
  }
  return () => {
    listeners.delete(fn);
    window.removeEventListener(ASSISTANT_OPEN_EVENT, onCmd);
    mq?.removeEventListener("change", fn);
  };
}

export const useAssistantOpen = (): boolean => useSyncExternalStore(subscribeAssistantOpen, isAssistantOpen, () => false);

export function resetAssistantOpenForTests(): void {
  overlayOpen = false;
  listeners.clear();
}
