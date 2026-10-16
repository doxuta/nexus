package vertical_pack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/services"
)

// appMinter is the consumer-side slice of *services.AppService that install needs to mint the
// sidebar App container for a freshly installed pack (Phase 4, ../specs/010-app-container/spec.md §0.2:
// "Cài một vertical-pack → mint một sys_app; sys_app là container DUY NHẤT hiện trong sidebar").
// Optional dependency (installStore precedent): nil keeps the pre-App behavior — install creates
// metadata only, no sidebar app — so existing tests and callers stay untouched.
type appMinter interface {
	CreateApp(ctx context.Context, tenantID string, in services.CreateAppInput) (*appmeta.App, error)
	ReplaceNav(ctx context.Context, tenantID, appID string, items []services.AppNavItemInput) ([]appmeta.NavItem, error)
	// ListApps backs the F7-3 default-lens pre-check (does the tenant already have a default
	// app?). Satisfied by *services.AppService.
	ListApps(ctx context.Context, tenantID string) ([]appmeta.App, error)
	// GetApp reads an app with its live menu — the pack upgrade appends new tables to the end of it.
	GetApp(ctx context.Context, tenantID, id string) (*appmeta.App, []appmeta.NavItem, error)
}

// findAppBySlug resolves an app by its slug via the port's existing ListApps — no new method, and
// no new query on the happy path (it is only called on the APP_ALREADY_EXISTS branch). nil when the
// listing fails or the slug is absent, which the caller treats as "leave it alone".
func (s *Service) findAppBySlug(ctx context.Context, tenantID, slug string) *appmeta.App {
	apps, err := s.appMint.ListApps(ctx, tenantID)
	if err != nil {
		return nil
	}
	for i := range apps {
		if apps[i].Name == slug {
			return &apps[i]
		}
	}
	return nil
}

// WithAppService wires the optional app minter (production: *services.AppService, constructed in
// cmd/server/main.go). Returns s for chaining, mirroring the RecordService.WithX wiring style.
func (s *Service) WithAppService(m appMinter) *Service {
	s.appMint = m
	return s
}

// appIconForSlug maps a pack slug to a lucide icon name for the minted app — a fixed mapping
// because the pack manifest carries no icon field (v1). Unknown slugs get the generic "package".
func appIconForSlug(slug string) string {
	switch slug {
	case "crm":
		return "briefcase"
	case "clinic":
		return "stethoscope"
	default:
		return "package"
	}
}

// mintApp mints ONE sys_app for a SUCCESSFULLY installed pack: slug = manifest slug (kebab
// converted to the app's snake shape), label = manifest name, nav = the pack's entities in PACK
// order followed by its dashboards, each resolved name→id via the tenant registry (a post-install
// read, so freshly created AND pre-existing/skipped resources both resolve).
//
// It is strictly FAIL-OPEN: the install's metadata is already committed and correct, and the app
// is a cosmetic lens over it (../specs/010-app-container/spec.md §1: app = lens, never an access boundary) — so a
// mint/nav failure logs zap.Warn and records AppError on the result instead of failing the
// install. Packs without a manifest slug (legacy hand-written packs) mint nothing: there is no
// stable slug to key an app on.
//
// IDEMPOTENCY CHOICE (documented per spec Phase 4): when the app slug already exists — a
// re-install — the existing app is left COMPLETELY untouched (no nav rewrite). This mirrors the
// install's own semantics (re-install skips existing entities; fresh tenant mints, re-install
// no-op) and guarantees a hand-customized nav in Studio Apps is never clobbered by re-running a
// pack. The skip is visible on the result as AppMinted=false + AppSlug set + empty AppError.
func (s *Service) mintApp(ctx context.Context, tenantID, userID string, pack *Pack, res *InstallResult) {
	if s.appMint == nil {
		return // optional dep absent → old behavior (no app minted)
	}
	manifestSlug := ""
	if pack.Manifest != nil {
		manifestSlug = strings.TrimSpace(pack.Manifest.Slug)
	}
	if manifestSlug == "" {
		s.logger.Debug("vertical_pack.mintApp: pack has no manifest slug — no app minted",
			zap.String("tenant_id", tenantID))
		return
	}

	// Manifest slugs are kebab ([a-z0-9-], ≤150); app slugs are snake ([a-z][a-z0-9_], ≤60).
	// Convert dashes and cap the length; a shape CreateApp still rejects (e.g. digit-leading)
	// surfaces as a recorded AppError below — fail-open, never failing the committed install.
	slug := strings.ReplaceAll(manifestSlug, "-", "_")
	if r := []rune(slug); len(r) > 60 {
		slug = string(r[:60])
	}
	res.AppSlug = slug

	label := strings.TrimSpace(pack.Manifest.Name)
	if label == "" {
		label = strings.TrimSpace(pack.Name)
	}
	if label == "" {
		label = slug // CreateApp requires a non-empty label
	}
	desc := strings.TrimSpace(pack.Manifest.Description)
	if desc == "" {
		desc = strings.TrimSpace(pack.Description)
	}
	// PACK-EXPORT-APPSLUG: honor the pack's own icon (an exported app carries the maker's chosen
	// icon) and fall back to the slug-derived guess for built-in / icon-less packs.
	icon := strings.TrimSpace(pack.Manifest.Icon)
	if icon == "" {
		icon = appIconForSlug(manifestSlug)
	}

	// F7-3 (default app lens): a freshly minted app becomes the tenant's DEFAULT app IFF the
	// tenant currently has NO default — so the first pack install lands users in the curated
	// app on login instead of "Tất cả dữ liệu", while an admin's explicit default choice is
	// NEVER stolen. The flag rides the CreateApp insert, whose repo path clears competing
	// defaults in the SAME transaction (appmeta.Create → clearDefault — the identical ≤1-default
	// discipline as the PUT/UpdateApp path), so no separate update call is needed. Fail-open:
	// if the pre-check read errors we cannot know whether a default exists → mint WITHOUT the
	// flag (never risk stealing) and never fail the committed install.
	makeDefault := false
	if apps, lerr := s.appMint.ListApps(ctx, tenantID); lerr != nil {
		s.logger.Warn("vertical_pack.mintApp: default-app pre-check failed — minting without default",
			zap.String("tenant_id", tenantID),
			zap.String("app_slug", slug),
			zap.Error(lerr))
	} else {
		makeDefault = true
		for _, a := range apps {
			if a.IsDefault {
				makeDefault = false
				break
			}
		}
	}

	app, err := s.appMint.CreateApp(ctx, tenantID, services.CreateAppInput{
		Name:        slug,
		Label:       label,
		Description: desc,
		Icon:        icon,
		IsDefault:   makeDefault,
		ActorID:     userID,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) && ve.Code == "APP_ALREADY_EXISTS" {
			// "Already exists" is not always "already fine". If a PREVIOUS install created the app
			// and then failed to write its nav (the warning path below), the tenant is left with an
			// EMPTY app — and re-running the install could never repair it, because it stopped
			// here. The customer sees an app with nothing in it and the only documented remedy is
			// to build the nav by hand.
			//
			// So: adopt the existing app and fall through to the nav step ONLY when its nav is
			// empty. Never when it has entries — a re-install must not overwrite a nav the customer
			// (or a later delivery) arranged. Fill-only, by construction.
			existing := s.findAppBySlug(ctx, tenantID, slug)
			if existing == nil || existing.NavCount > 0 {
				s.logger.Info("vertical_pack.mintApp: app already exists — left untouched (idempotent re-install)",
					zap.String("tenant_id", tenantID),
					zap.String("app_slug", slug))
				return
			}
			s.logger.Warn("vertical_pack.mintApp: adopting an existing app whose nav is EMPTY — filling it",
				zap.String("tenant_id", tenantID), zap.String("app_slug", slug))
			app = existing
		} else {
			res.AppError = err.Error()
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"KHÔNG tạo được app %q cho khuôn này (%v) — mọi bảng/quyền đã cài xong; tự tạo app trong Quản lý apps rồi gắn bảng vào.", slug, err))
			s.logger.Warn("vertical_pack.mintApp: create app failed (install itself succeeded)",
				zap.String("tenant_id", tenantID),
				zap.String("user_id", userID),
				zap.String("app_slug", slug),
				zap.Error(err))
			return
		}
	}
	res.AppMinted = true

	items, err := s.packNavItems(ctx, tenantID, pack)
	if err != nil {
		res.AppError = err.Error()
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"App %q đã tạo nhưng KHÔNG dựng được danh sách màn hình (%v) — vào Quản lý apps gắn bảng vào app.", slug, err))
		s.logger.Warn("vertical_pack.mintApp: resolve nav refs failed (app minted with empty nav)",
			zap.String("tenant_id", tenantID),
			zap.String("app_slug", slug),
			zap.Error(err))
		return
	}
	if len(items) == 0 {
		return // nothing to reference (defensive — a non-empty pack always has entities)
	}
	if _, err := s.appMint.ReplaceNav(ctx, tenantID, app.ID, items); err != nil {
		// The app shell exists but its nav write failed. AppMinted stays true (the row IS there),
		// which makes this the CONFUSING branch: the app appears in the sidebar and is EMPTY, with
		// nothing on screen explaining why. That is precisely why it needs a warning and not just
		// a field nobody reads.
		res.AppError = err.Error()
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"App %q đã tạo nhưng CHƯA gắn được bảng nào vào menu (%v) — mở app sẽ thấy trống; vào Quản lý apps gắn bảng.", slug, err))
		s.logger.Warn("vertical_pack.mintApp: nav write failed (app minted with empty nav)",
			zap.String("tenant_id", tenantID),
			zap.String("app_slug", slug),
			zap.Error(err))
	}
}

// packNavItems resolves the pack's entities (in pack order) then dashboards (after entities) to
// nav item inputs, name→id via the tenant registry. Labels are left empty so the nav uses each
// referenced object's own label (appmeta convention: "" → NULL). An unresolvable name — which a
// successful install should never produce — is skipped with a warning rather than failing, and
// duplicate refs are deduped (ReplaceNav rejects duplicate (type, ref) pairs).
func (s *Service) packNavItems(ctx context.Context, tenantID string, pack *Pack) ([]services.AppNavItemInput, error) {
	ents, err := s.entityRd.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.packNavItems: list entities: %w", err)
	}
	entityIDByName := make(map[string]string, len(ents))
	for _, e := range ents {
		entityIDByName[e.Name] = e.ID
	}

	seen := make(map[string]bool, len(pack.Entities)+len(pack.Dashboards))
	items := make([]services.AppNavItemInput, 0, len(pack.Entities)+len(pack.Dashboards))
	for _, pe := range pack.Entities {
		name := strings.TrimSpace(pe.Name)
		id, ok := entityIDByName[name]
		if !ok {
			s.logger.Warn("vertical_pack.packNavItems: pack entity not in registry after install — dropped from nav",
				zap.String("tenant_id", tenantID),
				zap.String("entity_name", name))
			continue
		}
		key := appmeta.ItemTypeEntity + ":" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, services.AppNavItemInput{ItemType: appmeta.ItemTypeEntity, RefID: id})
	}

	if s.dashboardWr != nil && len(pack.Dashboards) > 0 {
		dashes, err := s.dashboardWr.ListDashboards(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("vertical_pack.packNavItems: list dashboards: %w", err)
		}
		// Name-keyed best effort (dashboards have no unique name constraint; install's Pass 7
		// skip-by-name keeps a pack from duplicating them, so the map is unambiguous in practice).
		dashIDByName := make(map[string]string, len(dashes))
		for _, d := range dashes {
			dashIDByName[d.Name] = d.ID
		}
		for _, pd := range pack.Dashboards {
			name := strings.TrimSpace(pd.Name)
			id, ok := dashIDByName[name]
			if !ok {
				s.logger.Warn("vertical_pack.packNavItems: pack dashboard not found after install — dropped from nav",
					zap.String("tenant_id", tenantID),
					zap.String("dashboard_name", name))
				continue
			}
			key := appmeta.ItemTypeDashboard + ":" + id
			if seen[key] {
				continue
			}
			seen[key] = true
			items = append(items, services.AppNavItemInput{ItemType: appmeta.ItemTypeDashboard, RefID: id})
		}
	}
	return items, nil
}
