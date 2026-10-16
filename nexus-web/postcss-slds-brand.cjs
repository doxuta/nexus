// PostCSS plugin: make SLDS's brand colour follow tenant branding (specs/013 + specs/038 FR-003).
//
// SLDS 2.264.1 hard-codes its brand palette as literals ~700 times (e.g. `#0176d3`,
// `rgb(1, 118, 211)`) as fallbacks inside component styling hooks, so overriding the global brand
// hooks alone recolours only part of the UI. This rewrites every brand literal to
// `var(--slds-r-color-brand-N, <literal>)`. With no override the fallback renders exactly the stock
// colour; lib/branding.ts sets `--slds-r-color-brand-*` on <html> and every brand pixel follows.
//
// The `--slds-r-color-brand-*` definitions themselves are skipped (rewriting them would make the
// variable refer to itself).
const BRAND = {
  5: "#000310", 10: "#001639", 15: "#03234d", 20: "#032d60", 30: "#014486", 35: "#044f98",
  40: "#0b5cab", 45: "#0469c1", 50: "#0176d3", 55: "#0684ef", 60: "#1b96ff", 65: "#57a3fd",
  70: "#78b0fd", 80: "#aacbff", 85: "#c2d8ff", 90: "#d8e6fe", 95: "#eef4ff",
};

const hexToRgb = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));

// One alternation per shade: `#0176d3` (any case) or `rgb(1, 118, 211)` (any spacing).
const RULES = Object.entries(BRAND).map(([step, hex]) => {
  const [r, g, b] = hexToRgb(hex);
  return {
    re: new RegExp(`${hex}\\b|rgb\\(\\s*${r}\\s*,\\s*${g}\\s*,\\s*${b}\\s*\\)`, "gi"),
    step,
  };
});

module.exports = () => ({
  postcssPlugin: "slds-brand",
  Declaration(decl) {
    if (decl.prop.startsWith("--slds-r-color-brand-")) return;
    if (decl.value.includes("var(--slds-r-color-brand-")) return; // already rewritten
    let v = decl.value;
    for (const { re, step } of RULES) v = v.replace(re, (lit) => `var(--slds-r-color-brand-${step}, ${lit})`);
    if (v !== decl.value) decl.value = v;
  },
});
module.exports.postcss = true;
module.exports.BRAND = BRAND;
