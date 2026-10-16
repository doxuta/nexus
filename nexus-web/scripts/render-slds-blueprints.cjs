// Render every SLDS showcase example (compiled webpack bundles shipped in the npm package) to static HTML.
// Usage (from nexus-web/): node scripts/render-slds-blueprints.cjs [out-dir] [component-prefix]
// Env SLDS_PKG overrides the package root (default: ./node_modules/@salesforce-ux/design-system).
const fs = require('fs'), path = require('path'), vm = require('vm');
const req = require('module').createRequire(path.resolve('package.json')); // resolve React from cwd (nexus-web)
const React = req('react');
const RDS = req('react-dom/server');
const ReactDOM = req('react-dom');
const PKG = process.env.SLDS_PKG || path.resolve('node_modules/@salesforce-ux/design-system');
const SHOW = path.join(PKG, '__internal/chunked/showcase');
const OUT = path.resolve(process.argv[2] || 'slds-blueprints');
const only = process.argv[3];
const version = require(path.join(PKG, 'package.json')).version;
const common = fs.readFileSync(path.join(SHOW, 'common.js'), 'utf8');
const files = [];
(function walk(d) {
  for (const f of fs.readdirSync(d)) {
    const p = path.join(d, f);
    if (fs.statSync(p).isDirectory()) walk(p);
    else if (f === 'example.jsx.js') files.push(p);
  }
})(path.join(SHOW, 'ui/components'));
let bundles = 0, examples = 0;
for (const f of files) {
  const rel = path.relative(path.join(SHOW, 'ui/components'), path.dirname(f));
  if (only && !rel.startsWith(only)) continue;
  const ctx = { React, ReactDOM, console: { ...console, error() {}, warn() {} }, setTimeout, clearTimeout };
  ctx.self = ctx; ctx.window = ctx; ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(common, ctx);
  vm.runInContext(fs.readFileSync(f, 'utf8'), ctx);
  const mod = ctx.SLDS[Object.keys(ctx.SLDS).find((k) => k.includes('example.jsx'))];
  let html = `<!-- SLDS ${version} blueprint: ${rel} -->\n`;
  const groups = [['default', [].concat(mod.default || [])], ['examples', mod.examples || []], ['states', mod.states || []]];
  for (const [g, arr] of groups) for (const ex of arr) {
    const el = ex && (ex.element || (React.isValidElement(ex) ? ex : null));
    if (!el) continue;
    html += `\n<!-- ===== ${g}: ${ex.id || ''} — ${ex.label || ''} ===== -->\n${RDS.renderToStaticMarkup(el)}\n`;
    examples++;
  }
  const o = path.join(OUT, `${rel}.html`);
  fs.mkdirSync(path.dirname(o), { recursive: true });
  fs.writeFileSync(o, html);
  bundles++;
}
console.log({ version, bundles, examples, out: OUT });
