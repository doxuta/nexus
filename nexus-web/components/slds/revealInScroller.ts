// Scrolls `el` into view inside `scroller` only. Element.scrollIntoView also scrolls every ancestor,
// including the window, which drags the page when a strip sits below the first screen.
export function revealInScroller(el: HTMLElement | null | undefined, scroller: HTMLElement | null | undefined) {
  if (!el || !scroller) return;
  const a = el.getBoundingClientRect();
  const b = scroller.getBoundingClientRect();
  if (a.left < b.left) scroller.scrollLeft -= b.left - a.left;
  else if (a.right > b.right) scroller.scrollLeft += a.right - b.right;
}
