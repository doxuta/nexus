package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/metaversion"
)

// DraftAppStore implements appStore around the live SQL repository (specs/063-draft-publish S7-4):
//
//	not delivered            -> everything goes live, reads are live
//	delivered + builder      -> Update and ReplaceNav become ONE "app" draft (properties + menu); reads
//	                            overlay it when the caller asked (ReadDraft)
//	delivered + anyone else  -> writes go live (direct), reads are live - ZERO extra queries
//
// Create and SoftDelete always apply at once ("Áp ngay", US5-5): a new app has nothing to preview
// against, and a deleted app takes its roles and nav with it. Deleting also drops the app's draft, or a
// forced publish would resurrect it.
type DraftAppStore struct {
	live   appStore
	drafts draftStore
	gate   *DraftGate
}

// NewDraftAppStore wraps live.
func NewDraftAppStore(live appStore, drafts draftStore, gate *DraftGate) *DraftAppStore {
	return &DraftAppStore{live: live, drafts: drafts, gate: gate}
}

var _ appStore = (*DraftAppStore)(nil)

// Create implements appStore (always applies at once).
func (s *DraftAppStore) Create(ctx context.Context, in appmeta.CreateInput) (string, error) {
	return s.live.Create(ctx, in)
}

// SoftDelete implements appStore (always applies at once; also drops the app's draft).
func (s *DraftAppStore) SoftDelete(ctx context.Context, tenantID, id string) error {
	if err := s.live.SoftDelete(ctx, tenantID, id); err != nil {
		return err
	}
	if _, err := s.drafts.DeleteByKey(ctx, tenantID, DocTypeApp, id); err != nil {
		return fmt.Errorf("services.DraftAppStore.SoftDelete: drop draft: %w", err)
	}
	return nil
}

// Restore implements appRestorer (always applies at once, like Create: it is the undo of an applied-at-once delete).
func (s *DraftAppStore) Restore(ctx context.Context, tenantID, id, name string, in appmeta.UpdateInput) (bool, error) {
	r, ok := s.live.(appRestorer)
	if !ok {
		return false, errors.New("services.DraftAppStore.Restore: live store cannot restore")
	}
	return r.Restore(ctx, tenantID, id, name, in)
}

// current is the document a draft write starts from: the existing draft, else the live app.
func (s *DraftAppStore) current(ctx context.Context, tenantID, id string) (appDoc, error) {
	d, err := s.drafts.GetByKey(ctx, tenantID, DocTypeApp, id)
	if err != nil {
		return appDoc{}, err
	}
	if d != nil {
		var doc appDoc
		if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
			return appDoc{}, fmt.Errorf("decode draft %s: %w", id, err)
		}
		return doc, nil
	}
	app, err := s.live.GetByID(ctx, tenantID, id)
	if err != nil {
		return appDoc{}, err // ErrAppNotFound passes through
	}
	nav, err := s.live.ListNav(ctx, tenantID, id)
	if err != nil {
		return appDoc{}, err
	}
	return appDocOf(app, nav), nil
}

func (s *DraftAppStore) save(ctx context.Context, tenantID, id string, doc appDoc, actor string) error {
	body, err := doc.marshal()
	if err != nil {
		return err
	}
	return s.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeApp, DocKey: id, Op: metaversion.OpUpsert,
		Doc: body, Label: doc.Label, UserID: draftActor(ctx, actor)})
}

// Update implements appStore.
func (s *DraftAppStore) Update(ctx context.Context, tenantID, id string, in appmeta.UpdateInput) error {
	mode, err := s.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftAppStore.Update: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return s.live.Update(ctx, tenantID, id, in)
	}
	doc, err := s.current(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return err
		}
		return fmt.Errorf("services.DraftAppStore.Update: %w", err)
	}
	doc.Label, doc.Description, doc.Icon, doc.Color = in.Label, in.Description, in.Icon, in.Color
	doc.IsDefault, doc.SortOrder = in.IsDefault, in.SortOrder
	if in.HomeDashboardID != nil {
		doc.HomeDashboardID = *in.HomeDashboardID
	}
	if in.HomeQueueIDs != nil {
		doc.HomeQueueIDs = nonEmpty(*in.HomeQueueIDs)
	}
	if in.DefaultRoleIDs != nil {
		doc.DefaultRoleIDs = nonEmpty(*in.DefaultRoleIDs)
	}
	if err := s.save(ctx, tenantID, id, doc, in.UpdatedBy); err != nil {
		return fmt.Errorf("services.DraftAppStore.Update: %w", err)
	}
	return nil
}

// ReplaceNav implements appStore.
func (s *DraftAppStore) ReplaceNav(ctx context.Context, tenantID, appID string, items []appmeta.NavItemInput) error {
	mode, err := s.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftAppStore.ReplaceNav: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return s.live.ReplaceNav(ctx, tenantID, appID, items)
	}
	doc, err := s.current(ctx, tenantID, appID)
	if err != nil {
		if errors.Is(err, appmeta.ErrAppNotFound) {
			return err
		}
		return fmt.Errorf("services.DraftAppStore.ReplaceNav: %w", err)
	}
	doc.Nav = make([]appDocNav, len(items))
	for i, it := range items {
		doc.Nav[i] = appDocNav{ItemType: it.ItemType, RefID: it.RefID, Label: it.Label}
	}
	if err := s.save(ctx, tenantID, appID, doc, ""); err != nil {
		return fmt.Errorf("services.DraftAppStore.ReplaceNav: %w", err)
	}
	return nil
}

// overlay applies the draft to a live app read.
func overlayApp(live appmeta.App, d *metaversion.DraftRow) (appmeta.App, error) {
	if d == nil {
		live.DraftState = draftStateLive
		return live, nil
	}
	var doc appDoc
	if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
		return live, fmt.Errorf("decode draft %s: %w", live.ID, err)
	}
	live.Label, live.IsDefault, live.SortOrder = doc.Label, doc.IsDefault, doc.SortOrder
	live.Description, live.Icon, live.Color = ptrOrNil(doc.Description), ptrOrNil(doc.Icon), ptrOrNil(doc.Color)
	live.HomeDashboardID, live.NavCount = doc.HomeDashboardID, len(doc.Nav)
	live.HomeQueueIDs, live.DefaultRoleIDs = append([]string{}, doc.HomeQueueIDs...), append([]string{}, doc.DefaultRoleIDs...)
	live.UpdatedAt, live.DraftState = d.UpdatedAt, draftStateEdited
	return live, nil
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetByID implements appStore.
func (s *DraftAppStore) GetByID(ctx context.Context, tenantID, id string) (*appmeta.App, error) {
	app, err := s.live.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return s.overlayOne(ctx, tenantID, app)
}

// GetByName implements appStore.
func (s *DraftAppStore) GetByName(ctx context.Context, tenantID, name string) (*appmeta.App, error) {
	app, err := s.live.GetByName(ctx, tenantID, name)
	if err != nil {
		return nil, err
	}
	return s.overlayOne(ctx, tenantID, app)
}

func (s *DraftAppStore) overlayOne(ctx context.Context, tenantID string, app *appmeta.App) (*appmeta.App, error) {
	on, err := s.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.overlayOne: %w", err)
	}
	if !on {
		return app, nil
	}
	d, err := s.drafts.GetByKey(ctx, tenantID, DocTypeApp, app.ID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.overlayOne: %w", err)
	}
	out, err := overlayApp(*app, d)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.overlayOne: %w", err)
	}
	return &out, nil
}

// List implements appStore.
func (s *DraftAppStore) List(ctx context.Context, tenantID string) ([]appmeta.App, error) {
	rows, err := s.live.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	on, err := s.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.List: %w", err)
	}
	if !on {
		return rows, nil
	}
	drafts, err := s.drafts.ListByType(ctx, tenantID, DocTypeApp)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.List: %w", err)
	}
	byID := make(map[string]metaversion.DraftRow, len(drafts))
	for _, d := range drafts {
		byID[d.DocKey] = d
	}
	newDefault := ""
	for i := range rows {
		var d *metaversion.DraftRow
		if r, ok := byID[rows[i].ID]; ok {
			d = &r
		}
		wasDefault := rows[i].IsDefault
		if rows[i], err = overlayApp(rows[i], d); err != nil {
			return nil, fmt.Errorf("services.DraftAppStore.List: %w", err)
		}
		if rows[i].IsDefault && !wasDefault && rows[i].ID > newDefault { // Publish applies in DocKey (id) order, last wins
			newDefault = rows[i].ID
		}
	}
	if newDefault != "" { // at most one default: the preview must show what Publish will produce
		for i := range rows {
			if rows[i].ID != newDefault {
				rows[i].IsDefault = false
			}
		}
	}
	return rows, nil
}

// ListNav implements appStore.
func (s *DraftAppStore) ListNav(ctx context.Context, tenantID, appID string) ([]appmeta.NavItem, error) {
	on, err := s.gate.ReadOverlay(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftAppStore.ListNav: %w", err)
	}
	if on {
		d, err := s.drafts.GetByKey(ctx, tenantID, DocTypeApp, appID)
		if err != nil {
			return nil, fmt.Errorf("services.DraftAppStore.ListNav: %w", err)
		}
		if d != nil {
			var doc appDoc
			if err := json.Unmarshal(d.DraftJSON, &doc); err != nil {
				return nil, fmt.Errorf("services.DraftAppStore.ListNav: decode draft %s: %w", appID, err)
			}
			out := make([]appmeta.NavItem, len(doc.Nav))
			for i, n := range doc.Nav {
				out[i] = appmeta.NavItem{ID: appID + "#" + strconv.Itoa(i), TenantID: tenantID, AppID: appID, ItemType: n.ItemType,
					RefID: n.RefID, Label: ptrOrNil(n.Label), SortOrder: i, CreatedAt: d.UpdatedAt}
			}
			return out, nil
		}
	}
	return s.live.ListNav(ctx, tenantID, appID)
}
