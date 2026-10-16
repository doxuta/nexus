-- AO-THUONG-HIEU p1 (docs/tenant-branding.md): per-tenant white-label document.
-- NULL = default Nexus branding. Shape: {appName?, logoUrl?, accentColor?} — validated app-side.
ALTER TABLE tenants ADD COLUMN branding JSON NULL;
