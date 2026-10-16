package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/view"
)

// viewNameMaxRunes is the sys_views.name VARCHAR(150) cap, enforced by rune count (not bytes) so
// a 150-character Vietnamese name is accepted (a byte cap would false-reject multibyte names).
const viewNameMaxRunes = 150

// viewConfigMaxBytes bounds the opaque config_json document (W9b: "cap its size sanely"). The
// server never parses the config's inner shape, so this byte cap — well below the global 1 MiB
// request-body limit — is the ONLY guard against a view row bloating the table with a multi-MB
// blob. 64 KiB comfortably fits a rich filter/sort/column/density preset. Enforced on the raw
// bytes (a size limit is byte-oriented, unlike the rune-counted name cap).
const viewConfigMaxBytes = 64 * 1024

// viewRepo is the persistence surface ViewService consumes. Satisfied by *view.SQLRepository;
// declared consumer-side so the service is unit-testable with a fake and never imports a concrete
// DB type beyond the shared view value types.
type viewRepo interface {
	Create(ctx context.Context, in view.CreateInput) (string, error)
	ListForUser(ctx context.Context, tenantID, userID, entityName string) ([]view.Record, error)
	Get(ctx context.Context, tenantID, id string) (*view.Record, error)
	Update(ctx context.Context, tenantID, id string, in view.UpdateInput) error
	Delete(ctx context.Context, tenantID, id string) error
}

// viewEntityChecker verifies a saved view targets a real entity in the tenant (a view for a
// deleted/unknown entity is junk). Satisfied by *MetaCache / the metadata repository — the SAME
// GetEntityByName surface the report service uses.
type viewEntityChecker interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
}

// ViewService is the business layer for W9b saved views. It validates the definition (name /
// config / target entity) and enforces the ownership model (../specs/018-saved-views/spec.md §Business Logic):
//
//   - SHARED view (user_id NULL): visible to every tenant member; create/update/delete require
//     admin.
//   - PERSONAL view (user_id != NULL): visible + mutable only by its owner. Another user's personal
//     view is treated as not-found (404) on any mutate — no cross-user existence leak.
//
// The view CRUD is NOT a data-read gate: a view is a pure UI preset re-applied through the ACL'd
// list endpoint, so the service does not entity-ACL-gate view management (any authenticated tenant
// member may keep personal views).
type ViewService struct {
	repo     viewRepo
	entities viewEntityChecker
	logger   *zap.Logger
}

// NewViewService wires the view service. logger may be nil (a no-op logger is substituted).
func NewViewService(repo viewRepo, entities viewEntityChecker, logger *zap.Logger) *ViewService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ViewService{repo: repo, entities: entities, logger: logger}
}

// CreateViewInput is the validated create payload. Shared requests a tenant-wide view (admin-only).
type CreateViewInput struct {
	EntityName string
	Name       string
	Config     json.RawMessage
	Shared     bool
}

// UpdateViewInput is the mutable-fields replace payload (name + config). The target entity and the
// ownership are immutable.
type UpdateViewInput struct {
	Name   string
	Config json.RawMessage
}

// List returns the views visible to the caller for an entity: shared ∪ the caller's own personal.
func (s *ViewService) List(ctx context.Context, tenantID, userID, entityName string) ([]view.Record, error) {
	name := strings.TrimSpace(entityName)
	if name == "" {
		return nil, invalid("VIEW_ENTITY_REQUIRED", "tham số entity là bắt buộc")
	}
	rows, err := s.repo.ListForUser(ctx, tenantID, userID, name)
	if err != nil {
		return nil, fmt.Errorf("services.ViewService.List: %w", err)
	}
	return rows, nil
}

// Create validates and inserts a view, returning its id. A shared view requires isAdmin (403
// VIEW_FORBIDDEN otherwise); a personal view is owned by the caller. The target entity must exist.
func (s *ViewService) Create(ctx context.Context, tenantID, userID string, isAdmin bool, in CreateViewInput) (string, error) {
	entityName, name, cfg, err := s.validateDefinition(ctx, tenantID, in.EntityName, in.Name, in.Config)
	if err != nil {
		return "", err
	}
	// Shared (tenant-wide) views are governance artifacts — only an admin may create one. A
	// personal view is always owned by the caller.
	var owner *string
	if in.Shared {
		if !isAdmin {
			return "", invalid("VIEW_FORBIDDEN", "chỉ admin mới tạo được view dùng chung (shared)")
		}
		owner = nil
	} else {
		// Defense-in-depth (security 🔵-1): the repo coerces an empty owner to NULL = SHARED. A
		// personal view MUST carry a real owner — an empty userID (only reachable via a
		// malformed-but-signed token) must not silently escalate a personal view to shared.
		uid := strings.TrimSpace(userID)
		if uid == "" {
			return "", invalid("VIEW_FORBIDDEN", "không xác định được người sở hữu view cá nhân")
		}
		owner = &uid
	}
	id, err := s.repo.Create(ctx, view.CreateInput{
		TenantID:   tenantID,
		UserID:     owner,
		EntityName: entityName,
		Name:       name,
		Config:     cfg,
		CreatedBy:  userID,
	})
	if err != nil {
		return "", fmt.Errorf("services.ViewService.Create: %w", err)
	}
	return id, nil
}

// Update replaces a view's name + config. The caller must be allowed to mutate the view
// (mutableView): own personal, or admin for a shared view. entityName is the entity the view is
// addressed under (the /:entity/views/:id path segment) — a mismatch with the view's stored entity
// is treated as not-found so a view can never be reached under the wrong entity. entity_name itself
// is immutable, so config is re-validated for JSON shape only (not against the entity again).
func (s *ViewService) Update(ctx context.Context, tenantID, userID string, isAdmin bool, entityName, id string, in UpdateViewInput) error {
	if _, err := s.mutableView(ctx, tenantID, userID, isAdmin, entityName, id); err != nil {
		return err
	}
	name, err := s.validateName(in.Name)
	if err != nil {
		return err
	}
	cfg, err := s.validateConfig(in.Config)
	if err != nil {
		return err
	}
	if err := s.repo.Update(ctx, tenantID, id, view.UpdateInput{Name: name, Config: cfg, UpdatedBy: userID}); err != nil {
		return s.mapRepoErr("Update", err)
	}
	return nil
}

// Delete soft-deletes a view the caller is allowed to mutate (mutableView). entityName is the
// entity the view is addressed under (the /:entity/views/:id path segment); a mismatch is 404.
func (s *ViewService) Delete(ctx context.Context, tenantID, userID string, isAdmin bool, entityName, id string) error {
	if _, err := s.mutableView(ctx, tenantID, userID, isAdmin, entityName, id); err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, tenantID, id); err != nil {
		return s.mapRepoErr("Delete", err)
	}
	return nil
}

// mutableView fetches a view and enforces the mutate-permission rules, returning the row on
// success. It is the single ownership gate shared by Update and Delete so they can never drift:
//   - unknown / cross-tenant / deleted id     → VIEW_NOT_FOUND (404).
//   - view addressed under the WRONG entity    → VIEW_NOT_FOUND (404) — REST honesty, so the
//     /:entity/views/:id path cannot mutate a view belonging to a different entity.
//   - another user's PERSONAL view             → VIEW_NOT_FOUND (404) — no cross-user leak.
//   - a SHARED view + non-admin caller         → VIEW_FORBIDDEN (403).
//   - own personal, or shared + admin          → allowed.
func (s *ViewService) mutableView(ctx context.Context, tenantID, userID string, isAdmin bool, entityName, id string) (*view.Record, error) {
	rec, err := s.repo.Get(ctx, tenantID, id)
	if err != nil {
		return nil, s.mapRepoErr("Get", err)
	}
	// The view must belong to the entity it is addressed under. This is REST correctness (the id is
	// globally unique, so the path entity must match), NOT the security boundary — ownership below
	// gates the mutation entity-independently. A mismatch reveals nothing (treated as not-found).
	if en := strings.TrimSpace(entityName); en != "" && rec.EntityName != en {
		return nil, invalid("VIEW_NOT_FOUND", "không tìm thấy view")
	}
	if rec.UserID != nil { // personal view
		if *rec.UserID != userID {
			// Belongs to another user — reveal nothing (treat as not-found).
			return nil, invalid("VIEW_NOT_FOUND", "không tìm thấy view")
		}
		return rec, nil // own personal
	}
	// Shared view — admin only.
	if !isAdmin {
		return nil, invalid("VIEW_FORBIDDEN", "chỉ admin mới sửa/xoá được view dùng chung (shared)")
	}
	return rec, nil
}

// validateDefinition validates the fields shared by create: name, target-entity existence, and
// config JSON. It returns the cleaned entity name, name, and config bytes.
func (s *ViewService) validateDefinition(ctx context.Context, tenantID, entityName, name string, cfg json.RawMessage) (string, string, []byte, error) {
	en := strings.TrimSpace(entityName)
	if en == "" {
		return "", "", nil, invalid("VIEW_ENTITY_REQUIRED", "entity là bắt buộc")
	}
	// The target entity must exist in this tenant (a view for a missing entity is junk).
	if _, err := s.entities.GetEntityByName(ctx, tenantID, en); err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return "", "", nil, invalid("ENTITY_NOT_FOUND", "entity %q không tồn tại", en)
		}
		return "", "", nil, fmt.Errorf("services.ViewService.validateDefinition: %w", err)
	}
	cleanName, err := s.validateName(name)
	if err != nil {
		return "", "", nil, err
	}
	cleanCfg, err := s.validateConfig(cfg)
	if err != nil {
		return "", "", nil, err
	}
	return en, cleanName, cleanCfg, nil
}

// validateName trims and bounds the view name (non-empty, ≤150 runes).
func (s *ViewService) validateName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", invalidField("VIEW_NAME_REQUIRED", "name", "tên view là bắt buộc")
	}
	if utf8.RuneCountInString(n) > viewNameMaxRunes {
		return "", invalidField("VIEW_NAME_TOO_LONG", "name", "tên view vượt quá %d ký tự", viewNameMaxRunes)
	}
	return n, nil
}

// validateConfig checks that config is a present, valid JSON OBJECT. The shape of the object is
// FE-defined (filters/sort/columns/density) and stored verbatim — the server only guards that it is
// well-formed JSON and not a scalar/array/null, so a malformed body can never land in the column.
func (s *ViewService) validateConfig(cfg json.RawMessage) ([]byte, error) {
	trimmed := strings.TrimSpace(string(cfg))
	if trimmed == "" || trimmed == "null" {
		return nil, invalidField("VIEW_CONFIG_INVALID", "config", "config là bắt buộc và phải là JSON object")
	}
	// Size cap BEFORE the JSON parse: reject an oversized blob fast (a size limit is byte-oriented).
	if len(trimmed) > viewConfigMaxBytes {
		return nil, invalidField("VIEW_CONFIG_TOO_LARGE", "config", "config vượt quá %d KiB", viewConfigMaxBytes/1024)
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, invalidField("VIEW_CONFIG_INVALID", "config", "config không phải JSON hợp lệ")
	}
	// Require an object (not an array/scalar) — a view preset is a keyed document.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, invalidField("VIEW_CONFIG_INVALID", "config", "config phải là JSON object")
	}
	return []byte(trimmed), nil
}

// mapRepoErr translates a repository error into the service-level ValidationError the handler maps
// to HTTP: ErrViewNotFound → 404 VIEW_NOT_FOUND; anything else is wrapped for a logged 500.
func (s *ViewService) mapRepoErr(op string, err error) error {
	if errors.Is(err, view.ErrViewNotFound) {
		return invalid("VIEW_NOT_FOUND", "không tìm thấy view")
	}
	return fmt.Errorf("services.ViewService.%s: %w", op, err)
}
