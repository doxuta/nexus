const config = {
  plugins: {
    "@tailwindcss/postcss": {},
    // Runs on the CSS Tailwind already inlined (SLDS included): brand literals → tenant hooks.
    "./postcss-slds-brand.cjs": {},
  },
};

export default config;
