package services

// audit_docs.go — the audit SNAPSHOTS of the configuration documents a builder (or the customer's
// admin) edits: workflow rule, report, dashboard, branding, app + nav, inbound hook, payment config
// (specs/062-customer-workspace L4, FR-023). One file, so "what may a diff carry" is read in one place:
//
//   - every snapshot is an explicit allowlist (a struct with its own json tags), never the stored
//     row — a column added to the table later does not silently start flowing into the trail;
//   - secrets are replaced by auditRedacted BEFORE audit.ComputeDiff runs (the diff keeps the key, so
//     the trail still says "the signing key changed", never what it is).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/report"
	"nexus/platform/internal/workflow"
)

// auditRedacted stands in for any secret value in an audit diff.
const auditRedacted = "[redacted]"

// ── workflow rule ───────────────────────────────────────────────────────────────────────────────

type workflowAuditDoc struct {
	Name            string          `json:"name"`
	EntityName      string          `json:"entityName"`
	Trigger         string          `json:"trigger"`
	Condition       json.RawMessage `json:"condition"`
	Actions         json.RawMessage `json:"actions"`
	IsActive        bool            `json:"isActive"`
	IntervalSeconds int             `json:"intervalSeconds"`
}

// workflowAuditSnapshot is the diff-able view of a stored rule; nil in, nil out (create / delete have
// only one side). Actions go through redactWorkflowActionsForAudit.
func workflowAuditSnapshot(r *workflow.WorkflowRecord) any {
	if r == nil {
		return nil
	}
	return &workflowAuditDoc{
		Name: r.Name, EntityName: r.EntityName, Trigger: r.Trigger, Condition: r.Condition,
		Actions: redactWorkflowActionsForAudit(r.Actions), IsActive: r.IsActive, IntervalSeconds: r.IntervalSeconds,
	}
}

// redactWorkflowActionsForAudit masks what an action may carry that must never reach a trail the
// customer can read and export: the webhook signing key (`secret`), every `headers` value (they are
// auth headers), and the credential-bearing parts of a `url` / vault `path` (userinfo, query values —
// workflow.RedactTarget, the same mask the admin API applies). Keys are matched case-insensitively,
// as encoding/json fills Action.Secret from "SECRET" too. A body that does not decode as a list of
// objects is replaced whole: it may hold a secret this function cannot find.
func redactWorkflowActionsForAudit(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // a field_update constant like 12345678901234567 must survive the round trip
	var acts []map[string]any
	if err := dec.Decode(&acts); err != nil {
		return json.RawMessage(`"` + auditRedacted + `"`)
	}
	for _, a := range acts {
		for k, v := range a { // assigning the current key while ranging is defined
			switch {
			case strings.EqualFold(k, "secret"):
				if s, ok := v.(string); !ok || s != "" {
					a[k] = auditRedacted
				}
			case strings.EqualFold(k, "headers"):
				if m, isMap := v.(map[string]any); isMap {
					for name := range m {
						m[name] = auditRedacted
					}
				} else if v != nil {
					a[k] = auditRedacted
				}
			case strings.EqualFold(k, "url") || strings.EqualFold(k, "path"):
				if s, ok := v.(string); ok {
					a[k] = workflow.RedactTarget(s)
				}
			}
		}
	}
	out, err := json.Marshal(acts)
	if err != nil {
		return json.RawMessage(`"` + auditRedacted + `"`)
	}
	return out
}

// ── report / dashboard ──────────────────────────────────────────────────────────────────────────

type reportAuditDoc struct {
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Type       string          `json:"type"`
	Config     json.RawMessage `json:"config"`
	IsActive   bool            `json:"isActive"`
}

func reportAuditSnapshot(r *report.Record) any {
	if r == nil {
		return nil
	}
	return &reportAuditDoc{Name: r.Name, EntityName: r.EntityName, Type: r.Type, Config: r.Config, IsActive: r.IsActive}
}

type dashboardAuditDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Widgets     json.RawMessage `json:"widgets"`
}

func dashboardAuditSnapshot(r *dashboard.Record) any {
	if r == nil {
		return nil
	}
	d := &dashboardAuditDoc{Name: r.Name, Widgets: r.Widgets}
	if r.Description != nil {
		d.Description = *r.Description
	}
	return d
}

// ── branding ────────────────────────────────────────────────────────────────────────────────────

// brandingLogoAuditMax: a logo may be a ~72KB base64 data URI; the trail keeps a short stand-in for
// anything longer, so one logo change cannot bloat audit_log (and its CSV export).
const brandingLogoAuditMax = 256

// brandingAuditSnapshot is the branding document as a trail shows it: a long logoUrl is replaced by
// its first characters plus length and a short sha256, enough to tell two images apart.
func brandingAuditSnapshot(b Branding) any {
	if len(b.LogoURL) > brandingLogoAuditMax {
		sum := sha256.Sum256([]byte(b.LogoURL))
		b.LogoURL = fmt.Sprintf("%s…[%d ký tự, sha256:%s]", b.LogoURL[:40], len(b.LogoURL), hex.EncodeToString(sum[:])[:12])
	}
	return b
}

// ── app + nav ───────────────────────────────────────────────────────────────────────────────────

type appAuditDoc struct {
	Name            string `json:"name"`
	Label           string `json:"label"`
	Description     string `json:"description"`
	Icon            string `json:"icon"`
	Color           string `json:"color"`
	IsDefault       bool   `json:"isDefault"`
	SortOrder       int    `json:"sortOrder"`
	HomeDashboardID string `json:"homeDashboardId"`
}

func appAuditSnapshot(a *appmeta.App) any {
	if a == nil {
		return nil
	}
	d := &appAuditDoc{Name: a.Name, Label: a.Label, IsDefault: a.IsDefault, SortOrder: a.SortOrder, HomeDashboardID: a.HomeDashboardID}
	if a.Description != nil {
		d.Description = *a.Description
	}
	if a.Icon != nil {
		d.Icon = *a.Icon
	}
	if a.Color != nil {
		d.Color = *a.Color
	}
	return d
}

type appNavAuditItem struct {
	ItemType string `json:"itemType"`
	RefID    string `json:"refId"`
	Label    string `json:"label"`
}

// appNavAuditSnapshot is an app's whole nav as one "nav" key (the write is replace-all, so the diff
// shows the list before and after in order).
func appNavAuditSnapshot(items []appmeta.NavItem) any {
	nav := make([]appNavAuditItem, 0, len(items))
	for _, it := range items {
		l := ""
		if it.Label != nil {
			l = *it.Label
		}
		nav = append(nav, appNavAuditItem{ItemType: it.ItemType, RefID: it.RefID, Label: l})
	}
	return map[string]any{"nav": nav}
}

// ── inbound hook ────────────────────────────────────────────────────────────────────────────────

type hookAuditDoc struct {
	Name         string                    `json:"name"`
	EntityName   string                    `json:"entityName"`
	IsActive     bool                      `json:"isActive"`
	MatchField   string                    `json:"matchField"`
	Mapping      []inboundhook.Mapping     `json:"mapping"`
	PaymentMatch *inboundhook.PaymentMatch `json:"paymentMatch"`
	// Secret is only ever auditRedacted (create). An allowlist struct, not inboundhook.Hook, whose
	// `secret` json tag would carry the real key into the diff.
	Secret string `json:"secret,omitempty"`
}

// hookAuditSnapshot is the secret-free view of a hook. withSecretMarker adds secret:"[redacted]" so a
// create row says a key was minted.
func hookAuditSnapshot(h *inboundhook.Hook, withSecretMarker bool) any {
	if h == nil {
		return nil
	}
	d := &hookAuditDoc{Name: h.Name, EntityName: h.EntityName, IsActive: h.IsActive, Mapping: h.Mapping, PaymentMatch: h.PaymentMatch}
	if h.MatchField != nil {
		d.MatchField = *h.MatchField
	}
	if withSecretMarker {
		d.Secret = auditRedacted
	}
	return d
}
