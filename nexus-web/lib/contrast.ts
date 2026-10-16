// WCAG contrast of white text on a colour. ponytail: white-on-accent only (the brand button), other text colours not modelled.
export const CONTRAST_MIN = 4.5;

/** Contrast ratio of white over `hex` (#RRGGBB), or null when `hex` is not a valid colour. */
export function contrastOnWhite(hex: string | undefined): number | null {
  const m = /^#([0-9a-f]{6})$/i.exec(hex ?? "");
  if (!m) return null;
  const [r, g, b] = [0, 2, 4].map((i) => {
    const c = parseInt(m[1].slice(i, i + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 1.05 / (0.2126 * r + 0.7152 * g + 0.0722 * b + 0.05);
}
