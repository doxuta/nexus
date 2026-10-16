// KHUON-SONG-P1 diff engine (../specs/011-pack-provenance/spec.md §4.3): the read-only "Đối chiếu khuôn"
// report. For every provenance row of one installed pack, compare the as-installed content
// against a live re-serialization of the tenant (Export) and classify:
//
//	unchanged  — canonical forms equal
//	modified   — both sides present, forms differ (changedPaths lists where)
//	missing    — the live item is CONFIRMED gone (existence-probed, never inferred from Export
//	             dropping it — Export is fidelity-FILTERED, not a source of truth on existence)
//	undiffable — the item exists but the export serializer cannot round-trip it (e.g. a workflow
//	             referencing a business calendar, a report with joins) — honest instead of lying
//
// Comparison happens on PER-TYPE CANONICALIZED forms of BOTH sides (vet BLOCKER 1): map-level
// deep-compare fixes key ORDER but not key PRESENCE or array order, so write-path defaults are
// filled (workflow condition.match ""→"and", isActive absent→true) and order-insensitive arrays
// are sorted (role.actions by (entityName, action), mirroring ListActionsForRole's ORDER BY)
// before comparing. The must-pass gate: a fresh builtin install diffs 100% unchanged.
package vertical_pack

import (
	"context"
	"encoding/json"
	"fmt"
	"nexus/platform/internal/metadiff"
	"nexus/platform/internal/metaversion"
	"sort"
	"strings"
)

// DiffItem is one item's comparison outcome.
type DiffItem struct {
	ItemType    string `json:"itemType"`
	ItemKey     string `json:"itemKey"`
	PackVersion string `json:"packVersion"`
	Status      string `json:"status"` // unchanged | modified | missing | undiffable
	// Protected = the builder chose "Luôn giữ" (specs/069 FR-024). A flag, not a status: it never
	// changes what the diff says about the item (a protected, modified item is still "modified").
	Protected    bool            `json:"protected,omitempty"`
	ChangedPaths []string        `json:"changedPaths,omitempty"`
	PackContent  json.RawMessage `json:"packContent"`
	LiveContent  json.RawMessage `json:"liveContent,omitempty"`
}

// DiffReport is the GET /admin/verticals/installs/:slug/diff payload.
type DiffReport struct {
	Slug        string     `json:"slug"`
	Versions    []string   `json:"versions"` // distinct pack_version values across the rows (an item keeps its writing install's version)
	Untracked   bool       `json:"untracked"`
	Items       []DiffItem `json:"items"`
	GeneratedAt string     `json:"generatedAt"`
}

// changedPathsCap bounds the per-item path list (spec §4.3) — enough to point a human at the
// divergence without shipping a full JSON diff.
const changedPathsCap = metadiff.Cap

// ErrInstallNotFound signals the slug has neither provenance rows nor a ledger row for this
// tenant — the handler maps it to 404.
var ErrInstallNotFound = fmt.Errorf("vertical_pack: install not found")

// DiffInstalled builds the provenance diff report for one installed pack.
func (s *Service) DiffInstalled(ctx context.Context, tenantID, slug string) (*DiffReport, error) {
	// "So với lúc cài" compares the LIVE configuration with the pack, not the builder's drafts (specs/063 FR-044).
	ctx = metaversion.WithLiveRead(ctx)
	if s.items == nil {
		// Provenance store not wired — behave like an untracked install (honest-disabled).
		return &DiffReport{Slug: slug, Untracked: true, Items: []DiffItem{}, Versions: []string{}}, nil
	}
	slug = strings.TrimSpace(slug)
	rows, err := s.items.ListItems(ctx, tenantID, slug)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.DiffInstalled: list items: %w", err)
	}
	if len(rows) == 0 {
		// No provenance. Distinguish "installed before provenance existed" (untracked: true)
		// from "never installed at all" (404) via the ledger.
		if s.installs != nil {
			recs, lerr := s.installs.ListInstalls(ctx, tenantID)
			if lerr == nil {
				for _, r := range recs {
					if strings.TrimSpace(r.Slug) == slug {
						return &DiffReport{Slug: slug, Untracked: true, Items: []DiffItem{}, Versions: []string{}}, nil
					}
				}
			}
		}
		return nil, ErrInstallNotFound
	}

	// Live side: ONE full export (nil selection — a pack entity the customer deleted must surface
	// as "missing", not hard-fail the export with ENTITY_NOT_FOUND). No sample data.
	live, _, err := s.Export(ctx, tenantID, nil, false, nil)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.DiffInstalled: export live state: %w", err)
	}
	liveIdx := s.packContentIndex(live)

	report := &DiffReport{Slug: slug, Items: make([]DiffItem, 0, len(rows))}
	versions := map[string]bool{}
	for _, row := range rows {
		versions[row.PackVersion] = true
		item := DiffItem{
			ItemType:    row.Type,
			ItemKey:     row.Key,
			PackVersion: row.PackVersion,
			PackContent: row.Content,
			Protected:   row.Protected,
		}
		liveRaw, inExport := liveIdx[row.Type+"/"+row.Key]
		if !inExport {
			item.Status = s.classifyAbsent(ctx, tenantID, row.Type, row.Key)
		} else {
			item.LiveContent = liveRaw
			paths, cmpErr := comparePackItems(row.Type, row.Content, liveRaw)
			switch {
			case cmpErr != nil:
				// Malformed stored/live JSON — never guess. Surface as undiffable.
				item.Status = "undiffable"
			case len(paths) == 0:
				item.Status = "unchanged"
			default:
				item.Status = "modified"
				item.ChangedPaths = paths
			}
		}
		report.Items = append(report.Items, item)
	}
	for v := range versions {
		report.Versions = append(report.Versions, v)
	}
	sort.Strings(report.Versions)
	// Stable output order: by type then key (the FE groups by type).
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].ItemType != report.Items[j].ItemType {
			return report.Items[i].ItemType < report.Items[j].ItemType
		}
		return report.Items[i].ItemKey < report.Items[j].ItemKey
	})
	return report, nil
}

// classifyAbsent decides missing-vs-undiffable for an item absent from the export. Export DROPS
// live items it cannot round-trip (calendar-bound workflows, joined reports, unreferenced teams),
// so absence alone never proves deletion: probe the authoritative list paths where wired; when a
// probe is impossible, answer "undiffable" — never a false "missing".
func (s *Service) classifyAbsent(ctx context.Context, tenantID, itemType, key string) string {
	switch itemType {
	case ItemWorkflow:
		if s.workflowWr != nil {
			if wfs, err := s.workflowWr.ListWorkflows(ctx, tenantID); err == nil {
				for _, w := range wfs {
					if strings.TrimSpace(w.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemReport:
		if s.reportWr != nil {
			if rs, err := s.reportWr.ListReports(ctx, tenantID); err == nil {
				for _, r := range rs {
					if strings.TrimSpace(r.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemTeam:
		if s.teamWr != nil {
			if ts, err := s.teamWr.ListTeams(ctx, tenantID); err == nil {
				for _, t := range ts {
					if strings.TrimSpace(t.Name) == key {
						// Teams are only exported when a workflow references them — existence
						// without export presence is the normal "undiffable" case.
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemWorkQueue:
		// exportWorkQueues drops a still-alive queue the customer disabled (inactive) or whose
		// role was deleted — probe before saying gone (same spec rule as workflows/reports).
		if s.workQueueWr != nil {
			if qs, err := s.workQueueWr.ListAdmin(ctx, tenantID); err == nil {
				for _, q := range qs {
					if strings.TrimSpace(q.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemApproval:
		// exportApprovals drops a still-alive process it cannot round-trip (a user-approver step,
		// a role id no longer resolving, undecodable stored steps/criteria JSON) — probe before
		// saying gone. Key is the "<entity>:<name>" pair the install pass dedupes on.
		if s.approvalWr != nil {
			if ps, err := s.approvalWr.ListProcesses(ctx, tenantID); err == nil {
				for _, p := range ps {
					if strings.TrimSpace(p.EntityName)+":"+strings.TrimSpace(p.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemInboundHook:
		// exportHooks drops a hook whose landing entity is outside the export — probe the live list.
		if s.hookWr != nil {
			if hs, err := s.hookWr.ListAdmin(ctx, tenantID); err == nil {
				for _, h := range hs {
					if strings.TrimSpace(h.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	case ItemDashboard:
		// exportDashboards drops a still-alive dashboard whose every widget references an entity
		// outside the export (e.g. the customer deleted that entity) — probe before saying gone
		// (review SHOULD-FIX 1; same spec rule as workflows/reports).
		if s.dashboardWr != nil {
			if ds, err := s.dashboardWr.ListDashboards(ctx, tenantID); err == nil {
				for _, d := range ds {
					if strings.TrimSpace(d.Name) == key {
						return "undiffable"
					}
				}
				return "missing"
			}
		}
		return "undiffable"
	default:
		// entity/deferred_field/layout/role/nn_relationship: Export reads these unfiltered from
		// their repositories — absence there IS deletion.
		return "missing"
	}
}

// packContentIndex serializes every tracked item of a pack into "type/key" → raw JSON, using the
// SAME walk as the provenance collector (write-semantics irrelevant here; the same
// writer-availability guard keeps both sides symmetric).
func (s *Service) packContentIndex(p *Pack) map[string]json.RawMessage {
	idx := make(map[string]json.RawMessage)
	for _, it := range provenanceItems(p, &InstallPlan{}, nil, s.writerAvailability()) {
		idx[it.Type+"/"+it.Key] = it.Content
	}
	return idx
}

// ── canonicalize + compare (the shared rules live in internal/metadiff) ─────────────────────

// comparePackItems canonicalizes both sides per type and deep-compares, returning changed paths
// (empty = equal).
func comparePackItems(itemType string, packRaw, liveRaw json.RawMessage) ([]string, error) {
	a, err := decodeCanonical(itemType, packRaw)
	if err != nil {
		return nil, err
	}
	b, err := decodeCanonical(itemType, liveRaw)
	if err != nil {
		return nil, err
	}
	var paths []string
	diffValue("", a, b, &paths)
	return paths, nil
}

func decodeCanonical(itemType string, raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("vertical_pack.decodeCanonical: %w", err)
	}
	canonicalize(itemType, v)
	return v, nil
}

func canonicalize(itemType string, v any) { metadiff.Canonicalize(itemType, v) }

// diffValue appends dotted paths where a and b differ, stopping at changedPathsCap.
func diffValue(path string, a, b any, out *[]string) {
	metadiff.Walk(path, a, b, out, changedPathsCap)
}

// diffPaths is the unbounded sibling used by the upgrade plan (specs/069 T019).
func diffPaths(a, b any) []string { return metadiff.Diff(a, b) }
