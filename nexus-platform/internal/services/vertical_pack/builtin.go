package vertical_pack

import "sort"

// Built-in vertical packs ship in-code so a fresh tenant can be provisioned with a complete,
// opinionated schema without first hand-building one to export. Each pack uses only field types
// and config the field service already supports (text/email/picklist/number/currency/date/
// relation/...), valid relation targets that exist within the same pack, and grants over its own
// entities — so installing a built-in pack always passes validation.

// builtinPacks is the registry of named built-in packs, keyed by a stable slug.
var builtinPacks = map[string]func() *Pack{
	"crm":       crmPack,
	"clinic":    clinicPack,
	"ticketing": ticketingPack,
	"lms":       lmsPack,
	"pm":        pmPack,
}

// BuiltinPack returns a fresh copy of the named built-in pack and whether it exists. The slug is
// lowercase (e.g. "crm", "clinic"). A fresh build per call means a caller can mutate the returned
// pack without corrupting the registry.
func BuiltinPack(slug string) (*Pack, bool) {
	build, ok := builtinPacks[slug]
	if !ok {
		return nil, false
	}
	p := build()
	// The samples of a pack WE ship are demo data: Install marks them (specs/064 S9-6). Set here, at
	// the one place a built-in is built, so the install handler, the workspace starter and a future
	// caller cannot forget it.
	p.DemoSamples = true
	return p, true
}

// BuiltinSlugs returns the sorted list of built-in pack slugs (for a listing endpoint / docs).
func BuiltinSlugs() []string {
	out := make([]string, 0, len(builtinPacks))
	for k := range builtinPacks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuiltinInfo is the read-only listing view of one shipped built-in pack (FE-CATCHUP-2): its
// stable slug, its REAL self-describing manifest (name/version/description/author — the same
// header install records into the ledger), and how many entities installing it would create.
// It exists so GET /admin/verticals/builtin can show a pack's true version pre-install instead
// of the FE falling back to a static catalog.
type BuiltinInfo struct {
	Slug        string    `json:"slug"`
	Manifest    *Manifest `json:"manifest"`
	EntityCount int       `json:"entityCount"`
}

// BuiltinInfos returns one BuiltinInfo per shipped pack, sorted by slug. Each entry is built
// from a fresh BuiltinPack copy (cheap — only run per listing request, never per record), so
// callers can serialize the returned manifests without risking registry mutation.
func BuiltinInfos() []BuiltinInfo {
	slugs := BuiltinSlugs()
	out := make([]BuiltinInfo, 0, len(slugs))
	for _, slug := range slugs {
		pack, ok := BuiltinPack(slug)
		if !ok {
			continue // unreachable: slugs come from the same registry
		}
		out = append(out, BuiltinInfo{
			Slug:        slug,
			Manifest:    pack.Manifest,
			EntityCount: len(pack.Entities),
		})
	}
	return out
}

// crmPack is the full CRM vertical: entities accounts, contacts, opportunities, leads,
// activities, quotes, bank_txns, products, quote_lines and contracts, with B1-indexed picklists
// for GROUP BY dashboard queries, a N-N stakeholder relationship (contacts ↔ opportunities),
// sample data, list/detail layouts, and two roles.
//
// Deepened (../specs/030-crm-pack-deepening/spec.md) to showcase every advanced engine field type:
//   - auto_number: opp_code (OPP-xxxxx) on opportunities, lead_code (LEAD-xxxx) on leads —
//     two independent per-(tenant,entity,field) sequences. padding MUST be float64 (the
//     config validator type-asserts .(float64)).
//   - lookup: contacts.account_name (via account → accounts.name) and
//     opportunities.account_industry (via account → accounts.industry) — always resolved at
//     read time, null-safe.
//   - rollup (DeferredFields, Pass 2b — CANNOT go inline: crm_accounts is created FIRST by
//     dependency order, before crm_opportunities exists): accounts.total_opp_value (sum),
//     accounts.opportunity_count (count), accounts.avg_opp_value (avg) over the `account`
//     1-N; opportunities.stakeholder_count (count) over the N-N (needs Pass 2 first).
//     Rollups resolve only with an explicit ?rollup= opt-in (ENG-COMPUTED #3).
//   - 1-1 relation: contacts.primary_for (cardinality "1-1", inverse "primary_contact") —
//     at most ONE live contact may claim an account (tenant-scoped UNIQUE on primary_for_id);
//     a second claim is a 409 RECORD_RELATION_ALREADY_BOUND. Never IsUnique on a relation
//     (FIELD_RELATION_UNIQUE_UNSUPPORTED) — exclusivity is exactly options.cardinality.
//   - explicit inverseField on the three account relations (contacts/opportunities/
//     activities) — all four inverse names on crm_accounts pairwise distinct with
//     primary_contact (no FIELD_RELATION_INVERSE_TAKEN).
//
// v1.2.0 (PACK-TEAMS + PACK-WORKFLOWS) also ships the engine-depth AUTOMATION primitives now
// shippable in a pack, so a freshly-installed CRM tenant gets working sales automation out of
// the box — not just schema:
//   - Team "Đội Sales" (PACK-TEAMS): install seeds it after roles + adds the install user as its
//     first active member, so lead-routing has ≥1 ring member immediately.
//   - ASSIGN-1 lead-routing: the "Định tuyến lead mới" workflow round-robins every NEW crm_lead
//     over "Đội Sales" into the `assigned_to` field. `team` carries the NAME here — install
//     resolves it to the seeded team's id (a pack cannot know it). assigned_to is a `user` field;
//     assign accepts `user` or legacy `text` (assignTargetTypeOK, else ASSIGN_FIELD_TYPE_INVALID),
//     so tenants installed before the `user` type keep their text column and keep routing.
//   - TIMER-1 follow-up SLA: the "Nhắc theo dõi hoạt động" date_offset workflow fires 1 day BEFORE
//     a crm_activity's due_date. (due_date is a `date` field — buildTimerConfig accepts date OR
//     datetime for the anchor, so a date field is a valid timer anchor.)
//
// Both rules install through the SAME validated admin path as POST /admin/workflows
// (buildWorkflowRule + WF-01 field-target guard) and are idempotent by name on re-install.
//
// v1.3.0 (QUOTE-LINE-ITEMS) adds crm_quotes + crm_quote_lines — a real customer-facing
// composition of THREE engine primitives, not new engine code:
//   - crm_quote_lines.line_total = a `formula` field (quantity * unit_price, resultType
//     "number") — computed at WRITE time (applyComputedFields) and persisted as a plain
//     float64 into obj_crm_quote_lines.data.line_total.
//   - crm_quotes.total = a DEFERRED `rollup` (sum) over crm_quote_lines.line_total via the
//     `quote` 1-N relation — the SAME read-time rollup engine as total_opp_value, made possible
//     by the 2026-07-12 rollup-over-formula relaxation (rollup_validate.go): a number-resultType
//     formula's result is persisted, so SUM(CAST(JSON_EXTRACT(data,'$.line_total')...)) reads it
//     exactly like a real number field. DEFERRED (Pass 2b) because crm_quote_lines does not
//     exist yet when crm_quotes is created — same structural reason as total_opp_value.
//   - crm_quotes.account = a plain 1-N relation → crm_accounts (inverseField "quotes"), the
//     FIFTH inverse name on crm_accounts (pairwise distinct with contacts/opportunities/
//     activities/primary_contact).
//
// v1.4.0 (CONTRACT-RENEWAL) adds crm_contracts — a pure declarative compose of already-shipped
// primitives (entity + 1-N relation + currency + picklist + date + TWO date_offset timer
// workflows), NO new engine code:
//   - crm_contracts.account = a plain 1-N relation → crm_accounts (inverseField "contracts"),
//     the SIXTH inverse name on crm_accounts (pairwise distinct with the five above).
//   - contract_number = an auto_number (CON-xxxxx), a fourth independent per-(tenant,entity,
//     field) sequence (after opp_code/lead_code/quote_number); NEVER seeded (readonly).
//   - "Nhắc gia hạn hợp đồng" = a date_offset TIMER-1 firing 30 days BEFORE end_date, gated on
//     status "Hiệu lực" so only an active contract is reminded.
//   - "Tự động hết hạn hợp đồng" = a date_offset TIMER-1 firing 1 day AFTER end_date, gated on
//     status "Hiệu lực" (fire-time re-eval on the CURRENT record), then field_update status →
//     "Hết hạn" + $owner notification — the SAME timer-after + status-gate + field_update +
//     notify shape as Ticketing SLA-ESCALATION / LMS TUITION-OVERDUE. NO rollup/formula here.
//
// v1.5.0 (DASHBOARD-ENRICH) adds THREE widgets to the "CRM Overview" dashboard so the
// v1.3.0/v1.4.0 entities (crm_contracts, crm_quotes) surface on the freshly-installed board —
// a pure pack-literal compose of the shipped PackDashboard/PackWidget primitive, NO new engine
// code:
//   - "Tổng hợp đồng" = a COUNT kpi over crm_contracts (scalar headline, mirrors w2's
//     "Tổng giá trị pipeline" / Ticketing "Tổng ticket" house pattern).
//   - "Hợp đồng theo trạng thái" = COUNT crm_contracts GROUP BY status (indexed picklist, B1).
//   - "Báo giá theo trạng thái" = COUNT crm_quotes GROUP BY status (indexed picklist, B1).
//
// No SUM widget on crm_quotes.total: `total` is a DEFERRED rollup resolved ONLY under an
// explicit ?rollup opt-in (G12), so it does not participate in the aggregate engine a widget runs.
//
// v1.6.0 (PACK-REPORTS) ships canned saved reports so a freshly-installed CRM tenant gets working
// saved reports (not just a dashboard) — a pure pack-literal compose of the shipped PackReport
// primitive, NO new engine code:
//   - "Pipeline theo giai đoạn" = a `summary` report on crm_opportunities: SUM(amount) GROUP BY
//     stage. stage is an indexed picklist (B1, generated column), so the aggregate engine groups
//     without a full-scan at RUN time. Installed through the SAME validated path as
//     POST /admin/reports (CreateReport → ValidateReportConfig), idempotent by name on re-install.
//
// v1.7.0 (PACK-REPORTS-WORKLIST) adds a `tabular` work-list report — same PackReport primitive,
// composing the report engine's tabular + filter support, NO new engine code:
//   - "Danh sách hợp đồng hiệu lực" = a `tabular` report on crm_contracts projecting
//     [contract_number, account, end_date, value, status] filtered to status eq "Hiệu lực" (the
//     active-contracts collections list). The filter reuses the SAME list filter engine as a live
//     ?status=... query, so RUN returns only the active contracts.
//
// Follow-up #83 (tracked, not fixed here): the workflow `field_update` action bypasses
// RecordService and does NOT call applyComputedFields — an automation editing a line's
// quantity/unit_price would leave line_total (and crm_quotes.total) stale until the next
// client write. Applies to every formula field in every pack, not specific to quotes.
//
// HARD RULE: Samples must NEVER carry a computed/server-assigned key (opp_code, lead_code,
// account_name, account_industry, total_opp_value, opportunity_count, avg_opp_value,
// stakeholder_count, weighted_value, line_total, total) — RecordService.Create rejects those
// as readonly (422) and the whole install aborts + rolls back.
//
// B1 note: industry (crm_accounts), stage (crm_opportunities), status (crm_leads), and
// type (crm_activities) carry Options["indexed"]=true so the DDL materialises a VIRTUAL
// generated column + index that MySQL can use for GROUP BY / ORDER BY without a full-scan.
//
// B2 note: no "owner" field is created — every obj_* row carries the system column owner_id
// (migration 000061; starts equal to the immutable created_by, changed only by
// PUT /:entity/:id/owner), so a pack-level owner field would duplicate it.
func crmPack() *Pack {
	active := true
	return &Pack{
		Manifest: &Manifest{
			Name:    "CRM",
			Slug:    "crm",
			Version: "1.16.0", // 1.16.0: quote-line layouts hiện product+list_price; reconcile pass đưa field mới vào tenant nâng cấp; 1.15.0: danh mục sản phẩm + số tiền trên báo giá + nối cơ hội→báo giá→hợp đồng + rep thấy được khách hàng; 1.14.0: pack mang sẵn hook đối soát (cài phát chạy luôn); 1.13.0: crm_bank_txns + quote_number unique + "Đã thu"
			// 1.8.0: PACK-QUEUES — 2 work queues (lead inbox + closing-this-month) + close_date indexed
			Description: "Khách hàng, liên hệ, cơ hội, leads và hoạt động bán hàng (CRM đầy đủ).",
			Author:      "Nexus",
		},
		Name:        "CRM",
		Description: "Khách hàng, liên hệ, cơ hội, leads và hoạt động bán hàng (CRM đầy đủ).",

		// ------------------------------------------------------------------ entities
		Entities: []PackEntity{
			// crm_accounts — company / organisation
			{
				Name:  "crm_accounts",
				Label: "Khách hàng",
				Fields: []PackField{
					{Name: "name", Label: "Tên công ty", FieldType: "text", IsRequired: true},
					{Name: "industry", Label: "Ngành", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Technology", "Retail", "Healthcare", "Education", "Finance", "Manufacturing", "Other"},
						"valueLabels": map[string]any{"Technology": "Công nghệ", "Retail": "Bán lẻ", "Healthcare": "Y tế", "Education": "Giáo dục", "Finance": "Tài chính", "Manufacturing": "Sản xuất", "Other": "Khác"},
						"indexed":     true, // B1: GROUP BY industry on dashboard
					}},
					{Name: "website", Label: "Website", FieldType: "text"},
					{Name: "phone", Label: "Điện thoại", FieldType: "text"},
					// The company's own address. Everything the CRM might ever send OUT — a quote, a
					// contract, a payment request — needs somewhere to send it, and the pack had
					// only per-contact emails, which are not where you send a company's invoice.
					{Name: "email", Label: "Email công ty", FieldType: "text"},
					{Name: "annual_revenue", Label: "Doanh thu năm", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
				},
			},

			// crm_contacts — individual people; belongs to one account (1-N FK-on-child)
			{
				Name:  "crm_contacts",
				Label: "Liên hệ",
				Fields: []PackField{
					{Name: "first_name", Label: "Tên", FieldType: "text", IsRequired: true},
					{Name: "last_name", Label: "Họ", FieldType: "text", IsRequired: true},
					// email as text+unique (not "email" type) per spec B2
					{Name: "email", Label: "Email", FieldType: "text", IsUnique: true},
					{Name: "phone", Label: "Điện thoại", FieldType: "text"},
					{Name: "title", Label: "Chức danh", FieldType: "text"},
					// 1-N: contact belongs to one account. Explicit inverseField (ENG-REL2 #4):
					// the parent side on crm_accounts is discoverable as "contacts".
					{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"inverseField":  "contacts",
					}},
					// lookup (ENG-COMPUTED #2): always resolved at read time from the linked
					// account's name; null when account is unset. Never client-writable.
					{Name: "account_name", Label: "Tên khách hàng", FieldType: "lookup", Options: map[string]any{
						"via":         "account",
						"sourceField": "name",
					}},
					// 1-1 (ENG-REL2 #5): "primary contact of an account" — exclusive pointer.
					// cardinality "1-1" materialises a CASE-guarded FK column + tenant-scoped
					// UNIQUE (tenant_id, primary_for_id); a second live contact claiming the
					// same account is a 409 RECORD_RELATION_ALREADY_BOUND. Inline is safe:
					// crm_accounts exists first by dependency order. NEVER IsUnique here
					// (FIELD_RELATION_UNIQUE_UNSUPPORTED) — cardinality IS the exclusivity.
					{Name: "primary_for", Label: "Là liên hệ chính của", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"cardinality":   "1-1",
						"inverseField":  "primary_contact",
					}},
				},
			},

			// crm_opportunities — deals / sales pipeline; belongs to one account (1-N)
			{
				Name:  "crm_opportunities",
				Label: "Cơ hội",
				Fields: []PackField{
					// auto_number (ENG #6): server-assigned display code OPP-00001.. per
					// (tenant, entity, field) sequence. padding MUST be float64 — the config
					// validator type-asserts .(float64); a Go int fails as
					// FIELD_AUTONUMBER_PADDING_INVALID. Not unique: nothing needs to look a deal up
					// by its code. IsUnique would be legal since RM-28 (per-tenant pivot — see
					// quote_number below, which needs it for payment matching).
					{Name: "opp_code", Label: "Mã cơ hội", FieldType: "auto_number", Options: map[string]any{
						"prefix":  "OPP-",
						"padding": float64(5),
					}},
					{Name: "name", Label: "Tên cơ hội", FieldType: "text", IsRequired: true},
					{Name: "amount", Label: "Giá trị", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					{Name: "stage", Label: "Giai đoạn", FieldType: "picklist", IsRequired: true, Options: map[string]any{
						"values":      []any{"Prospecting", "Qualification", "Proposal", "Negotiation", "Closed Won", "Closed Lost"},
						"valueLabels": map[string]any{"Prospecting": "Tìm kiếm", "Qualification": "Đánh giá", "Proposal": "Báo giá", "Negotiation": "Đàm phán", "Closed Won": "Thắng", "Closed Lost": "Thua"},
						"indexed":     true, // B1: GROUP BY stage for funnel dashboard
					}},
					{Name: "close_date", Label: "Ngày chốt", FieldType: "date", Options: map[string]any{
						"indexed": true, // PACK-QUEUES: sortable queue "co_hoi_chot_thang_nay" + fast date-token filters
					}},
					{Name: "probability", Label: "Xác suất (%)", FieldType: "number", Options: map[string]any{
						"min": float64(0),
						"max": float64(100),
					}},
					// weighted_value = amount.amount * probability / 100 (computed, read-only to clients)
					{Name: "weighted_value", Label: "Giá trị kỳ vọng", FieldType: "formula", Options: map[string]any{
						"formula":      "amount * probability / 100",
						"resultType":   "currency",
						"currencyCode": "VND",
					}},
					// 1-N: opportunity belongs to one account. Explicit inverseField:
					// the parent side on crm_accounts is discoverable as "opportunities".
					{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"inverseField":  "opportunities",
					}},
					// lookup preserving the source field's original JSON type (picklist string).
					{Name: "account_industry", Label: "Ngành của khách hàng", FieldType: "lookup", Options: map[string]any{
						"via":         "account",
						"sourceField": "industry",
					}},
				},
			},

			// crm_leads — inbound leads before qualification
			{
				Name:  "crm_leads",
				Label: "Khách hàng tiềm năng",
				Fields: []PackField{
					// Second auto_number — proves per-(tenant,entity,field) sequence isolation
					// from opp_code (both count independently from 1). padding = float64 (G2).
					{Name: "lead_code", Label: "Mã lead", FieldType: "auto_number", Options: map[string]any{
						"prefix":  "LEAD-",
						"padding": float64(4),
					}},
					{Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true},
					{Name: "email", Label: "Email", FieldType: "text"},
					{Name: "company", Label: "Công ty", FieldType: "text"},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"New", "Working", "Qualified", "Lost"},
						"valueLabels": map[string]any{"New": "Mới", "Working": "Đang xử lý", "Qualified": "Đạt chuẩn", "Lost": "Mất"},
						"indexed":     true, // B1: GROUP BY status for lead pipeline
					}},
					{Name: "source", Label: "Nguồn", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Web", "Referral", "Cold Call", "Event", "Other"},
						"valueLabels": map[string]any{"Web": "Website", "Referral": "Giới thiệu", "Cold Call": "Gọi chào", "Event": "Sự kiện", "Other": "Khác"},
					}},
					// ASSIGN-1 routing target: the "Định tuyến lead mới" workflow writes the picked
					// rep's userId here. `user`, NOT `text`: the older comment here claimed a reference to
					// users was impossible, which was true before the `user` field type existed and left
					// this screen showing a bare UUID to salespeople. The type validates the target is a
					// live ACTIVE user and ?expand= renders their name. Tenants installed BEFORE this keep
					// their text column (install is idempotent — FIELD_ALREADY_EXISTS is a skip) and keep
					// routing, since assign accepts both types. Left unset on samples (routing fires only
					// on live after_create; Pass 8 workflows install after the seed rows anyway).
					{Name: "assigned_to", Label: "Phân công cho", FieldType: "user"},
				},
			},

			// crm_activities — calls, meetings, tasks linked to an account
			{
				Name:  "crm_activities",
				Label: "Hoạt động",
				Fields: []PackField{
					{Name: "subject", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
					{Name: "type", Label: "Loại", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Call", "Meeting", "Email", "Task"},
						"valueLabels": map[string]any{"Call": "Cuộc gọi", "Meeting": "Cuộc họp", "Email": "Email", "Task": "Việc cần làm"},
						"indexed":     true, // B1: GROUP BY type for activity reports
					}},
					{Name: "due_date", Label: "Ngày hẹn", FieldType: "date"},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Planned", "Completed", "Cancelled"},
						"valueLabels": map[string]any{"Planned": "Đã lên lịch", "Completed": "Hoàn thành", "Cancelled": "Đã huỷ"},
					}},
					// 1-N: activity belongs to one account. Explicit inverseField:
					// the parent side on crm_accounts is discoverable as "activities".
					{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"inverseField":  "activities",
					}},
					// POLYMORPHIC-1: what this activity is ABOUT — any of account/contact/opportunity.
					// Timeline for a record = GET /crm_activities?related_to_type=<e>&related_to_id=<id>.
					// Optional + unseeded in v1 (polymorphic sample @ref is a follow-up).
					{Name: "related_to", Label: "Liên quan tới", FieldType: "polymorphic_relation", Options: map[string]any{
						"targetEntities": []any{"crm_accounts", "crm_contacts", "crm_opportunities"},
					}},
				},
			},

			// crm_quotes — a sales quote (báo giá) for an account. QUOTE-LINE-ITEMS: `total` is a
			// DEFERRED rollup (sum) over crm_quote_lines.line_total — see the deferred fields
			// section below for why it cannot be declared inline.
			{
				Name:  "crm_quotes",
				Label: "Báo giá",
				Fields: []PackField{
					// auto_number — third independent per-(tenant,entity,field) sequence in this
					// pack (after opp_code/lead_code). padding MUST be float64.
					// IsUnique: the quote number is the reference a customer types into a bank transfer
					// memo, and inboundhook.PaymentMatch REQUIRES its refField to be unique — without
					// it the pack's own money loop (QR → transfer → auto-mark paid) cannot be wired.
					// Legal since RM-28: uniqueness is per tenant via sys_unique_index uq_pivot
					// (tenant_id, entity_name, field_name, value), so two tenants may both hold QUO-00001.
					{Name: "quote_number", Label: "Số báo giá", FieldType: "auto_number", IsUnique: true, Options: map[string]any{
						"prefix":  "QUO-",
						"padding": float64(5),
					}},
					{Name: "name", Label: "Tiêu đề báo giá", FieldType: "text", IsRequired: true},
					// 1-N: quote belongs to one account. Explicit inverseField: the parent side on
					// crm_accounts is discoverable as "quotes" (fifth inverse name, pairwise
					// distinct from contacts/opportunities/activities/primary_contact).
					{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"inverseField":  "quotes",
					}},
					// The amount actually being asked for. crm_quotes.total is a DEFERRED rollup, and a
					// rollup is not a stored money value — so the payment QR, which needs a
					// currency/number field on the record, never rendered on the one entity the
					// whole reconciliation loop targets. Writable on purpose: the sum of the lines
					// is what the lines say; what you ask the customer to pay is a decision.
					{Name: "amount_due", Label: "Số tiền thanh toán", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					// The deal this quote came from. Without it a quote, its opportunity and the
					// contract that follows were three unconnected rows describing the same deal.
					{Name: "opportunity", Label: "Cơ hội", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_opportunities",
						"inverseField":  "quotes",
					}},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", IsRequired: true, Options: map[string]any{
						// "Đã thu" is the state the bank-reconciliation hook flips a quote INTO
						// (inboundhook PaymentMatch.statusValue). Without it in the picklist the write is
						// rejected at record-validation time and the money loop dead-letters every match.
						"values":  []any{"Nháp", "Đã gửi", "Chấp nhận", "Từ chối", "Đã thu"},
						"indexed": true, // B1: GROUP BY status for a future quotes dashboard
					}},
					// MỘT XƯỞNG W3 #5: the record-detail "Tệp" tab lists a record's `file`/`image`
					// fields — and an inventory of every builtin pack found ZERO of either, so that
					// tab was empty 100% of the time after ANY install. The engine has supported
					// both types for a long time; this was a pack-CONTENT gap, not an engine gap.
					// A quote is exactly where a real salesperson expects to attach the signed PDF.
					{Name: "attachment", Label: "Tệp đính kèm", FieldType: "file"},
				},
			},

			// crm_bank_txns — the landing table for bank-credit signals (DONG-TIEN p2). Every
			// inbound webhook delivery writes ONE row here; the PaymentMatch step then reads the
			// memo, finds QUO-xxxxx in it, and flips that quote to "Đã thu".
			//
			// Why the pack needs its own table: inboundhook always upserts into a target entity
			// FIRST and runs PaymentMatch after (inbound_hook_service.go). Without a landing table
			// the only way to wire the loop was to aim the hook at crm_quotes itself, which would
			// have the bank feed creating quotes. Measured 2026-08-02: the engine has supported
			// this loop with an integration test since it landed, and the flagship pack could not
			// express it — engine capability with no pack to carry it.
			//
			// It also doubles as the audit trail a finance person asks for ("show me the transfer
			// that closed this quote"), which is why the raw memo is kept verbatim.
			{
				Name:  "crm_bank_txns",
				Label: "Giao dịch ngân hàng",
				Fields: []PackField{
					{Name: "memo", Label: "Nội dung chuyển khoản", FieldType: "text", IsRequired: true},
					{Name: "amount", Label: "Số tiền", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					{Name: "txn_ref", Label: "Mã giao dịch", FieldType: "text"},
					{Name: "txn_date", Label: "Thời điểm", FieldType: "datetime"},
					{Name: "matched_quote", Label: "Báo giá khớp", FieldType: "text"},
				},
			},

			// crm_products — the price list. Without it every quote line retyped the product name and
			// its price as free text, so two quotes for the same thing disagreed by a typo and no
			// report could group by product. Declared BEFORE crm_quote_lines: install creates
			// entities in slice order and the relation's target must already exist (the same
			// load-bearing ordering as tk_customers before tk_tickets).
			{
				Name:  "crm_products",
				Label: "Sản phẩm",
				Fields: []PackField{
					{Name: "name", Label: "Tên sản phẩm", FieldType: "text", IsRequired: true},
					// Unique so the catalogue cannot grow two rows for the same thing — the exact
					// duplication free-text lines produced.
					{Name: "code", Label: "Mã sản phẩm", FieldType: "text", IsUnique: true},
					{Name: "list_price", Label: "Giá niêm yết", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					{Name: "unit", Label: "Đơn vị tính", FieldType: "picklist", Options: map[string]any{
						"values": []any{"Cái", "Bộ", "Giờ", "Tháng", "Gói"},
					}},
					{Name: "is_active", Label: "Còn kinh doanh", FieldType: "boolean"},
					{Name: "description", Label: "Mô tả", FieldType: "text"},
				},
			},

			// crm_quote_lines — one line item of a quote: product × quantity × unit_price.
			// line_total is a `formula` field (computed at WRITE time, persisted into `data`) —
			// the composition primitive crm_quotes.total's rollup sums.
			{
				Name:  "crm_quote_lines",
				Label: "Dòng báo giá",
				Fields: []PackField{
					// 1-N: line belongs to one quote. Explicit inverseField: the parent side on
					// crm_quotes is discoverable as "lines".
					{Name: "quote", Label: "Báo giá", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_quotes",
						"inverseField":  "lines",
					}},
					// The catalogue link. product_name stays as the free-text fallback for a one-off
					// item that is not in the price list — dropping it would break every existing
					// quote line and force a catalogue row for every ad-hoc charge.
					{Name: "product", Label: "Sản phẩm", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_products",
						"inverseField":  "quote_lines",
					}},
					// Read-time copy of the catalogue price, so a line shows what the product costs
					// without anyone retyping it. unit_price stays the WRITABLE field: a negotiated
					// price must be able to differ from the list price, and overwriting it from the
					// catalogue would silently undo the discount someone agreed.
					{Name: "list_price", Label: "Giá niêm yết", FieldType: "lookup", Options: map[string]any{
						"via":         "product",
						"sourceField": "list_price",
					}},
					{Name: "product_name", Label: "Tên tự nhập", FieldType: "text"},
					{Name: "quantity", Label: "Số lượng", FieldType: "number", Options: map[string]any{
						"min": float64(0),
					}},
					{Name: "unit_price", Label: "Đơn giá", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					// formula (ENG-COMPUTED, write-time): quantity * unit_price.amount, resultType
					// "number" — a plain float64 persisted into data.line_total (NOT an
					// {amount,currencyCode} object), which is exactly what makes it rollup-able
					// (rollup_validate.go's 2026-07-12 relaxation requires resultType "number").
					{Name: "line_total", Label: "Thành tiền", FieldType: "formula", Options: map[string]any{
						"formula":    "quantity * unit_price",
						"resultType": "number",
					}},
				},
			},

			// crm_contracts — a signed contract for an account (CONTRACT-RENEWAL). belongs-to one
			// account (1-N FK-on-child). Its two date_offset workflows below (renewal reminder +
			// auto-expire) key on end_date. NO rollup/formula here — a pure entity+relation+timer
			// composition of already-shipped primitives.
			{
				Name:  "crm_contracts",
				Label: "Hợp đồng",
				Fields: []PackField{
					// auto_number — fourth independent per-(tenant,entity,field) sequence in this pack
					// (after opp_code/lead_code/quote_number). padding MUST be float64. Not unique (no
					// matcher reads it); unlike quote_number, which is IsUnique for payment matching —
					// legal since RM-28's per-tenant pivot.
					// NEVER seeded (server-assigned readonly → 422 would abort install).
					{Name: "contract_number", Label: "Số hợp đồng", FieldType: "auto_number", Options: map[string]any{
						"prefix":  "CON-",
						"padding": float64(5),
					}},
					{Name: "name", Label: "Tên hợp đồng", FieldType: "text", IsRequired: true},
					// 1-N: contract belongs to one account. Explicit inverseField "contracts" — the SIXTH
					// inverse name on crm_accounts, pairwise distinct from contacts/opportunities/
					// activities/primary_contact/quotes.
					{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_accounts",
						"inverseField":  "contracts",
					}},
					// The quote this contract was signed from — the third link that turns
					// opportunity → quote → contract into one chain instead of three islands where
					// the same deal is typed three times over.
					{Name: "quote", Label: "Báo giá", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "crm_quotes",
						"inverseField":  "contracts",
					}},
					{Name: "start_date", Label: "Ngày bắt đầu", FieldType: "date"},
					{Name: "end_date", Label: "Ngày kết thúc", FieldType: "date"},
					{Name: "value", Label: "Giá trị hợp đồng", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", IsRequired: true, Options: map[string]any{
						"values":  []any{"Nháp", "Hiệu lực", "Hết hạn", "Đã gia hạn"},
						"indexed": true, // B1: GROUP BY status for a contracts dashboard / renewal report
					}},
					// W3 #5 (see crm_quotes.attachment): a contract without a place for the signed
					// scan is not a contract anyone can actually work with.
					{Name: "signed_copy", Label: "Bản hợp đồng đã ký", FieldType: "file"},
				},
			},
		},

		// ------------------------------------------------------------------ N-N relationships
		// contacts ↔ opportunities: a contact can be a stakeholder on many opportunities;
		// an opportunity can have many stakeholder contacts.
		// join table name: rel_crm_contacts_crm_opportunities (JoinParts sorts alphabetically)
		NNRelationships: []PackNNRelationship{
			{
				Name:    "crm_contacts_opportunities",
				EntityA: "crm_contacts",
				EntityB: "crm_opportunities",
			},
		},

		// ------------------------------------------------------------------ deferred fields
		// Parent-side rollups (ENG-COMPUTED #3), added in install Pass 2b AFTER all entities +
		// the N-N relationship exist. They CANNOT go inline: crm_accounts is created FIRST by
		// dependency order, so a rollup over crm_opportunities would fail config-time
		// validation (FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND); stakeholder_count additionally
		// needs the N-N registered in Pass 2. Mode keys are exclusive (G4): FK-on-child uses
		// viaRelation, N-N uses relationship — both at once is FIELD_ROLLUP_RELATION_AMBIGUOUS.
		// count carries NO aggField; sum/avg REQUIRE one (number/currency/rating).
		// NOTE: rollups resolve ONLY with an explicit ?rollup=<names> opt-in (G12); no filter
		// option exists, so names never imply "open only" (G5 — total_opp_value, not
		// total_open_value).
		DeferredFields: []PackDeferredField{
			{EntityName: "crm_accounts", Field: PackField{
				Name: "total_opp_value", Label: "Tổng giá trị cơ hội", FieldType: "rollup",
				Options: map[string]any{"childEntity": "crm_opportunities", "viaRelation": "account", "metric": "sum", "aggField": "amount"},
			}},
			{EntityName: "crm_accounts", Field: PackField{
				Name: "opportunity_count", Label: "Số cơ hội", FieldType: "rollup",
				Options: map[string]any{"childEntity": "crm_opportunities", "viaRelation": "account", "metric": "count"},
			}},
			// AVG rollup — Salesforce has no native avg rollup; engine differentiator.
			{EntityName: "crm_accounts", Field: PackField{
				Name: "avg_opp_value", Label: "Giá trị cơ hội TB", FieldType: "rollup",
				Options: map[string]any{"childEntity": "crm_opportunities", "viaRelation": "account", "metric": "avg", "aggField": "amount"},
			}},
			// N-N rollup mode: `relationship` key (NOT viaRelation) names the registered N-N.
			{EntityName: "crm_opportunities", Field: PackField{
				Name: "stakeholder_count", Label: "Số stakeholder", FieldType: "rollup",
				Options: map[string]any{"childEntity": "crm_contacts", "relationship": "crm_contacts_opportunities", "metric": "count"},
			}},
			// QUOTE-LINE-ITEMS: total = SUM(crm_quote_lines.line_total) over the `quote` 1-N.
			// DEFERRED for the same structural reason as the three crm_accounts rollups above:
			// crm_quotes is created before crm_quote_lines exists. aggField "line_total" is a
			// `formula` field (resultType "number") — allowed ONLY by the 2026-07-12
			// rollup-over-formula relaxation (rollup_validate.go); since #262 a currency-resultType
			// formula is accepted too (summed on ".amount"), only a text formula is rejected.
			{EntityName: "crm_quotes", Field: PackField{
				Name: "total", Label: "Tổng tiền", FieldType: "rollup",
				Options: map[string]any{"childEntity": "crm_quote_lines", "viaRelation": "quote", "metric": "sum", "aggField": "line_total"},
			}},
		},

		// ------------------------------------------------------------------ sample data
		// 4 accounts → 4 contacts → 5 opportunities (various stages) → 4 leads → 4 activities
		// "@ref" values in Data are resolved to real UUIDs at install time (Pass 3).
		// Server-assigned/computed keys (opp_code, lead_code, lookups, rollups, weighted_value)
		// are NEVER seeded (G1 — readonly 422 would abort the whole install); assignSequences
		// stamps opp_code/lead_code inside RecordService.Create in listed order.
		Samples: []PackSample{
			{
				Entity: "crm_accounts",
				Records: []PackRecord{
					{
						Ref: "@acme",
						Data: map[string]any{
							"name":           "Acme Corp",
							"industry":       "Technology",
							"website":        "https://acme.example.com",
							"phone":          "+1-555-0100",
							"annual_revenue": map[string]any{"amount": float64(1000000), "currencyCode": "VND"},
						},
					},
					{
						Ref: "@globex",
						Data: map[string]any{
							"name":           "Globex Inc",
							"industry":       "Finance",
							"website":        "https://globex.example.com",
							"phone":          "+1-555-0200",
							"annual_revenue": map[string]any{"amount": float64(2000000), "currencyCode": "VND"},
						},
					},
					{
						Ref: "@initech",
						Data: map[string]any{
							"name":           "Initech",
							"industry":       "Retail",
							"phone":          "+1-555-0300",
							"annual_revenue": map[string]any{"amount": float64(500000), "currencyCode": "VND"},
						},
					},
					// Zero opportunities — demos the rollup empty-state on read:
					// opportunity_count = 0, total_opp_value = null, avg_opp_value = null.
					{
						Ref: "@umbrella",
						Data: map[string]any{
							"name":           "Umbrella Health",
							"industry":       "Healthcare",
							"website":        "https://umbrella.example.com",
							"phone":          "+1-555-0400",
							"annual_revenue": map[string]any{"amount": float64(3500000), "currencyCode": "VND"},
						},
					},
				},
			},
			{
				Entity: "crm_contacts",
				Records: []PackRecord{
					{
						Ref: "@alice",
						Data: map[string]any{
							"first_name":  "Alice",
							"last_name":   "Smith",
							"email":       "alice@acme.example.com",
							"title":       "VP Sales",
							"account":     "@acme",
							"primary_for": "@acme", // Alice is Acme's ONE primary contact (1-1)
						},
					},
					{
						Ref: "@bob",
						Data: map[string]any{
							"first_name":  "Bob",
							"last_name":   "Jones",
							"email":       "bob@globex.example.com",
							"title":       "CFO",
							"account":     "@globex",
							"primary_for": "@globex",
						},
					},
					{
						Ref: "@carol",
						Data: map[string]any{
							"first_name":  "Carol",
							"last_name":   "Lee",
							"email":       "carol@initech.example.com",
							"title":       "Procurement Manager",
							"account":     "@initech",
							"primary_for": "@initech",
						},
					},
					// Dave deliberately carries NO primary_for key — load-bearing: if he also
					// claimed @acme, the 1-1 tenant-scoped UNIQUE would 409 and abort install.
					{
						Ref: "@dave",
						Data: map[string]any{
							"first_name": "Dave",
							"last_name":  "Kim",
							"email":      "dave@acme.example.com",
							"title":      "CTO",
							"account":    "@acme",
						},
					},
				},
			},
			{
				Entity: "crm_opportunities",
				Records: []PackRecord{
					{
						Ref: "@opp_acme_q1",
						Data: map[string]any{
							"name":        "Acme Q1 Expansion",
							"amount":      map[string]any{"amount": float64(75000), "currencyCode": "VND"},
							"stage":       "Proposal",
							"close_date":  "2026-09-30",
							"probability": float64(60),
							"account":     "@acme",
						},
					},
					{
						Ref: "@opp_globex_ent",
						Data: map[string]any{
							"name":        "Globex Enterprise Suite",
							"amount":      map[string]any{"amount": float64(250000), "currencyCode": "VND"},
							"stage":       "Negotiation",
							"close_date":  "2026-08-15",
							"probability": float64(80),
							"account":     "@globex",
						},
					},
					{
						Ref: "@opp_initech_pilot",
						Data: map[string]any{
							"name":        "Initech Pilot",
							"amount":      map[string]any{"amount": float64(15000), "currencyCode": "VND"},
							"stage":       "Qualification",
							"close_date":  "2026-10-01",
							"probability": float64(0),
							"account":     "@initech",
						},
					},
					{
						Ref: "@opp_acme_upsell",
						Data: map[string]any{
							"name":        "Acme Platform Upsell",
							"amount":      map[string]any{"amount": float64(40000), "currencyCode": "VND"},
							"stage":       "Prospecting",
							"probability": float64(20),
							"account":     "@acme",
						},
					},
					// 3rd Acme opp → acme rollups: count 3, sum 235000, avg 78333.33… VND.
					{
						Ref: "@opp_acme_renewal",
						Data: map[string]any{
							"name":        "Acme Annual Renewal",
							"amount":      map[string]any{"amount": float64(120000), "currencyCode": "VND"},
							"stage":       "Closed Won",
							"close_date":  "2026-06-30",
							"probability": float64(100),
							"account":     "@acme",
						},
					},
				},
			},
			{
				Entity: "crm_leads",
				Records: []PackRecord{
					{
						Data: map[string]any{
							"full_name": "Eve Martinez",
							"email":     "eve@prospect.example.com",
							"company":   "Prospect LLC",
							"status":    "New",
							"source":    "Web",
						},
					},
					{
						Data: map[string]any{
							"full_name": "Frank Nguyen",
							"email":     "frank@startup.example.com",
							"company":   "StartupCo",
							"status":    "Working",
							"source":    "Referral",
						},
					},
					{
						Data: map[string]any{
							"full_name": "Grace Chen",
							"company":   "MidMarket Inc",
							"status":    "Qualified",
							"source":    "Event",
						},
					},
					{
						Data: map[string]any{
							"full_name": "Hank Wilson",
							"email":     "hank@bigco.example.com",
							"company":   "BigCo Ltd",
							"status":    "Lost",
							"source":    "Cold Call",
						},
					},
				},
			},
			{
				Entity: "crm_activities",
				Records: []PackRecord{
					{
						Data: map[string]any{
							"subject":  "Introductory call with Acme",
							"type":     "Call",
							"due_date": "2026-07-05",
							"status":   "Completed",
							"account":  "@acme",
						},
					},
					{
						Data: map[string]any{
							"subject":  "Globex demo meeting",
							"type":     "Meeting",
							"due_date": "2026-07-10",
							"status":   "Planned",
							"account":  "@globex",
						},
					},
					{
						Data: map[string]any{
							"subject":  "Follow-up email to Initech",
							"type":     "Email",
							"due_date": "2026-07-08",
							"status":   "Planned",
							"account":  "@initech",
						},
					},
					{
						Data: map[string]any{
							"subject":  "Send renewal contract to Acme",
							"type":     "Task",
							"due_date": "2026-07-15",
							"status":   "Planned",
							"account":  "@acme",
						},
					},
				},
			},
			// QUOTE-LINE-ITEMS: 2 quotes → 5 line items with known quantity×unit_price so the
			// rollup total is a computable, discriminating value (never seed quote_number/
			// line_total/total — all three are server-computed, readonly-from-client).
			// @quote_acme: 3×500000 + 1×2000000 + 2×750000 = 1500000+2000000+1500000 = 5,000,000 VND.
			// @quote_globex: 5×1200000 + 1×3500000 = 6000000+3500000 = 9,500,000 VND.
			{
				Entity: "crm_quotes",
				Records: []PackRecord{
					{
						Ref: "@quote_acme",
						Data: map[string]any{
							"name":    "Báo giá mở rộng Acme Q1",
							"account": "@acme",
							"status":  "Đã gửi",
						},
					},
					{
						Ref: "@quote_globex",
						Data: map[string]any{
							"name":    "Báo giá triển khai Globex Enterprise",
							"account": "@globex",
							"status":  "Nháp",
						},
					},
				},
			},
			{
				Entity: "crm_quote_lines",
				Records: []PackRecord{
					{
						Data: map[string]any{
							"quote":        "@quote_acme",
							"product_name": "Nexus License",
							"quantity":     float64(3),
							"unit_price":   map[string]any{"amount": float64(500000), "currencyCode": "VND"},
						},
					},
					{
						Data: map[string]any{
							"quote":        "@quote_acme",
							"product_name": "Onboarding Package",
							"quantity":     float64(1),
							"unit_price":   map[string]any{"amount": float64(2000000), "currencyCode": "VND"},
						},
					},
					{
						Data: map[string]any{
							"quote":        "@quote_acme",
							"product_name": "Support Plan",
							"quantity":     float64(2),
							"unit_price":   map[string]any{"amount": float64(750000), "currencyCode": "VND"},
						},
					},
					{
						Data: map[string]any{
							"quote":        "@quote_globex",
							"product_name": "Enterprise Suite",
							"quantity":     float64(5),
							"unit_price":   map[string]any{"amount": float64(1200000), "currencyCode": "VND"},
						},
					},
					{
						Data: map[string]any{
							"quote":        "@quote_globex",
							"product_name": "Data Migration",
							"quantity":     float64(1),
							"unit_price":   map[string]any{"amount": float64(3500000), "currencyCode": "VND"},
						},
					},
				},
			},
			// CONTRACT-RENEWAL: 3 contracts on existing sample accounts with varied status/dates so
			// a contracts dashboard has real buckets and the two date_offset workflows have live
			// anchors. contract_number is NEVER seeded (auto_number, server-assigned readonly). Only
			// "Hiệu lực" contracts are reminded/auto-expired (the workflow status gate); the "Nháp"
			// and "Đã gia hạn" samples prove the gate excludes non-active contracts.
			{
				Entity: "crm_contracts",
				Records: []PackRecord{
					{
						Data: map[string]any{
							"name":       "Hợp đồng dịch vụ Acme 2026",
							"account":    "@acme",
							"start_date": "2026-01-01",
							"end_date":   "2026-12-31",
							"value":      map[string]any{"amount": float64(100000000), "currencyCode": "VND"},
							"status":     "Hiệu lực",
						},
					},
					{
						Data: map[string]any{
							"name":       "Hợp đồng triển khai Globex",
							"account":    "@globex",
							"start_date": "2026-03-01",
							"end_date":   "2027-02-28",
							"value":      map[string]any{"amount": float64(250000000), "currencyCode": "VND"},
							"status":     "Nháp",
						},
					},
					{
						Data: map[string]any{
							"name":       "Hợp đồng bảo trì Initech",
							"account":    "@initech",
							"start_date": "2025-06-01",
							"end_date":   "2026-05-31",
							"value":      map[string]any{"amount": float64(50000000), "currencyCode": "VND"},
							"status":     "Đã gia hạn",
						},
					},
				},
			},
		},

		// ------------------------------------------------------------------ N-N links
		// Stakeholders: Alice and Dave on Acme Q1 Expansion; Bob on Globex Enterprise Suite;
		// Carol on Initech Pilot. → stakeholder_count rollup: 2 / 1 / 1 / 0 / 0.
		Links: []PackLink{
			{Relation: "crm_contacts_opportunities", FromRef: "@alice", ToRef: "@opp_acme_q1"},
			{Relation: "crm_contacts_opportunities", FromRef: "@dave", ToRef: "@opp_acme_q1"},
			{Relation: "crm_contacts_opportunities", FromRef: "@bob", ToRef: "@opp_globex_ent"},
			{Relation: "crm_contacts_opportunities", FromRef: "@carol", ToRef: "@opp_initech_pilot"},
		},

		// ------------------------------------------------------------------ layouts
		Layouts: []PackLayout{
			// List layouts — all 7 entities
			{EntityName: "crm_accounts", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "name"},
					map[string]any{"field": "industry"},
					map[string]any{"field": "phone"},
					map[string]any{"field": "annual_revenue"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_contacts", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "first_name"},
					map[string]any{"field": "last_name"},
					map[string]any{"field": "email"},
					map[string]any{"field": "title"},
					map[string]any{"field": "account"},
					map[string]any{"field": "account_name"}, // lookup — always resolved
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_opportunities", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "opp_code"}, // auto_number display code
					map[string]any{"field": "name"},
					map[string]any{"field": "stage"},
					map[string]any{"field": "amount"},
					map[string]any{"field": "close_date"},
					map[string]any{"field": "account"},
					map[string]any{"field": "account_industry"}, // lookup — always resolved
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_leads", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "lead_code"}, // auto_number display code
					map[string]any{"field": "full_name"},
					map[string]any{"field": "company"},
					map[string]any{"field": "status"},
					map[string]any{"field": "source"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_activities", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "subject"},
					map[string]any{"field": "type"},
					map[string]any{"field": "due_date"},
					map[string]any{"field": "status"},
					map[string]any{"field": "account"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_quotes", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "quote_number"}, // auto_number display code
					map[string]any{"field": "name"},
					map[string]any{"field": "account"},
					map[string]any{"field": "status"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_quote_lines", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "quote"},
					// product (catalogue) first, product_name (free-text fallback) after — a rep who
					// picked from the catalogue should see the product, not just their typed note.
					map[string]any{"field": "product"},
					map[string]any{"field": "product_name"},
					map[string]any{"field": "quantity"},
					map[string]any{"field": "list_price"},
					map[string]any{"field": "unit_price"},
					map[string]any{"field": "line_total"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},

			// Detail layouts — accounts and opportunities
			// N-N relation tab is not representable in L1 section-based layouts;
			// stakeholder links are accessible via the /relations API endpoint.
			{EntityName: "crm_accounts", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin cơ bản",
						"columns": 2,
						"fields":  []any{"name", "industry", "phone", "website", "annual_revenue"},
					},
					// Rollups are opt-in (G12): the FE only sees these values when it requests
					// ?rollup=total_opp_value,opportunity_count,avg_opp_value on the record read.
					map[string]any{
						"title":   "Pipeline",
						"columns": 2,
						"fields":  []any{"total_opp_value", "opportunity_count", "avg_opp_value"},
					},
				},
			}},
			{EntityName: "crm_opportunities", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin cơ hội",
						"columns": 2,
						"fields":  []any{"opp_code", "name", "stage", "amount", "probability", "close_date"},
					},
					map[string]any{
						"title":   "Giá trị kỳ vọng",
						"columns": 1,
						"fields":  []any{"weighted_value"},
					},
					// stakeholder_count is a rollup — opt-in via ?rollup=stakeholder_count (G12).
					map[string]any{
						"title":   "Liên kết",
						"columns": 1,
						"fields":  []any{"account", "account_industry", "stakeholder_count"},
					},
				},
			}},
			// crm_quotes detail: `total` is a rollup — opt-in via ?rollup=total (G12). No
			// related-list layout primitive exists for line items (mirrors the N-N-relation-tab
			// note above) — line items are browsed via GET /crm_quote_lines?quote=<id>.
			{EntityName: "crm_quotes", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin báo giá",
						"columns": 2,
						"fields":  []any{"quote_number", "name", "account", "status"},
					},
					map[string]any{
						"title":   "Tổng tiền",
						"columns": 1,
						"fields":  []any{"total"},
					},
				},
			}},
			{EntityName: "crm_quote_lines", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Chi tiết dòng báo giá",
						"columns": 2,
						"fields":  []any{"quote", "product", "product_name", "quantity", "list_price", "unit_price", "line_total"},
					},
				},
			}},
			// crm_contracts list + detail (CONTRACT-RENEWAL) — reference only real fields.
			{EntityName: "crm_contracts", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "contract_number"}, // auto_number display code
					map[string]any{"field": "name"},
					map[string]any{"field": "account"},
					map[string]any{"field": "start_date"},
					map[string]any{"field": "end_date"},
					map[string]any{"field": "value"},
					map[string]any{"field": "status"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "crm_contracts", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin hợp đồng",
						"columns": 2,
						"fields":  []any{"contract_number", "name", "account", "status", "start_date", "end_date", "value"},
					},
				},
			}},
		},

		// ------------------------------------------------------------------ dashboards
		// "CRM Overview" seeds one dashboard with 4 widgets so a freshly-installed CRM tenant
		// gets a working pipeline + KPI view on real data without any manual config step.
		//
		// Widget design notes:
		//   w1 (bar)  — pipeline funnel: SUM(amount) of crm_opportunities grouped by stage.
		//               GroupBy=stage is indexed (generated column, B1), so the aggregate engine
		//               resolves it without a full JSON_EXTRACT scan. Field="amount" on a currency
		//               field → engine uses the "$.amount" sub-path for the SUM.
		//   w2 (kpi)  — total pipeline value: SUM(amount) crm_opportunities (no groupBy → scalar).
		//   w3 (bar)  — opportunities by stage (count per stage): COUNT grouped by stage (indexed).
		//               Complements w1 by showing deal count alongside deal value.
		//   w4 (bar)  — leads by status: COUNT crm_leads grouped by status (indexed, B1).
		//   w5 (kpi)  — total contracts: COUNT crm_contracts (scalar headline). DASHBOARD-ENRICH.
		//   w6 (bar)  — contracts by status: COUNT crm_contracts grouped by status (indexed, B1).
		//   w7 (bar)  — quotes by status: COUNT crm_quotes grouped by status (indexed, B1).
		Dashboards: []PackDashboard{
			{
				Name:        "CRM Overview",
				Description: "Pipeline funnel, tổng giá trị và leads theo trạng thái.",
				Widgets: []PackWidget{
					{
						ID:      "w1",
						Title:   "Pipeline theo giai đoạn (giá trị)",
						Type:    "bar",
						Entity:  "crm_opportunities",
						Metric:  "sum",
						Field:   "amount",
						GroupBy: "stage",
					},
					{
						ID:     "w2",
						Title:  "Tổng giá trị pipeline",
						Type:   "kpi",
						Entity: "crm_opportunities",
						Metric: "sum",
						Field:  "amount",
					},
					{
						ID:      "w3",
						Title:   "Số cơ hội theo giai đoạn",
						Type:    "bar",
						Entity:  "crm_opportunities",
						Metric:  "count",
						GroupBy: "stage",
					},
					{
						ID:      "w4",
						Title:   "Leads theo trạng thái",
						Type:    "bar",
						Entity:  "crm_leads",
						Metric:  "count",
						GroupBy: "status",
					},
					// DASHBOARD-ENRICH (v1.5.0): surface crm_contracts + crm_quotes on the board.
					{
						ID:     "w5",
						Title:  "Tổng hợp đồng",
						Type:   "kpi",
						Entity: "crm_contracts",
						Metric: "count",
					},
					{
						ID:      "w6",
						Title:   "Hợp đồng theo trạng thái",
						Type:    "bar",
						Entity:  "crm_contracts",
						Metric:  "count",
						GroupBy: "status", // indexed picklist (Nháp/Hiệu lực/Hết hạn/Đã gia hạn), B1
					},
					{
						ID:      "w7",
						Title:   "Báo giá theo trạng thái",
						Type:    "bar",
						Entity:  "crm_quotes",
						Metric:  "count",
						GroupBy: "status", // indexed picklist (Nháp/Đã gửi/Chấp nhận/Từ chối), B1
					},
				},
			},
		},

		// ------------------------------------------------------------------ reports (PACK-REPORTS)
		// Two canned saved reports so a freshly-installed CRM tenant gets working saved reports: a
		// summary pipeline breakdown + a tabular active-contracts work-list (PACK-REPORTS-WORKLIST).
		// "Pipeline theo giai đoạn": SUM(amount) of crm_opportunities GROUP BY stage — stage is an
		// indexed picklist (B1), so the aggregate engine groups without a full-scan at run time.
		// Installed through the SAME validated CreateReport path as POST /admin/reports; idempotent
		// by name on re-install; NOT exported (pack-authored only).
		// PACK-HOOKS: the money loop now arrives WIRED. Without this the builder hand-built the
		// hook, mapping, regex and secret for every single customer — the "90% engine" promise
		// falling exactly one link short of the capability it was proudest of.
		//
		// No secret here by construction (Pack.InboundHooks): install mints a fresh one per tenant
		// and warns the admin to fetch it before telling the bank to send.
		//
		// Shape mirrors the run proven over real HTTP on 2026-08-02: a bank credit lands as ONE row
		// in crm_bank_txns, then QUO-xxxxx is pulled from the transfer memo and that quote flips to
		// "Đã thu". No matchField on purpose: every credit is a NEW transaction, never an upsert
		// over a previous one.
		InboundHooks: []PackInboundHook{
			{
				Name:       "bank-credit",
				EntityName: "crm_bank_txns",
				Mapping: []PackHookMapping{
					{Source: "desc", Field: "memo"},
					{Source: "amount", Field: "amount"},
					{Source: "ref", Field: "txn_ref"},
				},
				PaymentMatch: &PackHookPaymentMatch{
					TargetEntity: "crm_quotes",
					RefField:     "quote_number",
					RefPattern:   `QUO-[0-9]{5}`,
					MemoSource:   "desc",
					StatusField:  "status",
					StatusValue:  "Đã thu",
				},
			},
		},

		Reports: []PackReport{
			{
				Name:       "Pipeline theo giai đoạn",
				EntityName: "crm_opportunities",
				Type:       "summary",
				Config: PackReportConfig{
					Metric:  "sum",
					Field:   "amount",
					GroupBy: "stage",
				},
				IsActive: &active,
			},
			{
				// PACK-REPORTS-WORKLIST (tabular): the active-contracts collections list — every
				// crm_contract whose status is "Hiệu lực". Columns project the FK relation `account`
				// (a STORED generated column off data.$.account → the target UUID) alongside scalars.
				// The `status eq "Hiệu lực"` filter reuses the SAME list filter engine (eq on a
				// picklist), byte-identical to the picklist option, so the report narrows to the
				// active contracts instead of listing all of them.
				Name:       "Danh sách hợp đồng hiệu lực",
				EntityName: "crm_contracts",
				Type:       "tabular",
				Config: PackReportConfig{
					Columns: []string{"contract_number", "account", "end_date", "value", "status"},
					Filters: []PackReportFilter{{Field: "status", Op: "eq", Values: []string{"Hiệu lực"}}},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ teams (PACK-TEAMS)
		// The sales team the lead-routing rule below round-robins over. Install seeds it (idempotent
		// by name) AFTER roles and BEFORE workflows, adds the install user as its first active member,
		// and maps its NAME → the minted id so "Định tuyến lead mới" resolves.
		Teams: []PackTeam{
			{Name: "Đội Sales"},
		},

		// ------------------------------------------------------------------ work queues (PACK-QUEUES)
		// "Việc Hôm Nay" from day 1: a role-scoped inbox for the sales rep (new untouched leads)
		// and an everyone queue for opportunities closing this month (relative token — Wave 3.6).
		WorkQueues: []PackWorkQueue{
			{
				Name: "leads_moi_chua_xu_ly", Label: "Lead mới chưa xử lý", EntityName: "crm_leads",
				RoleName:  "crm_sales_rep",
				Filters:   []PackQueueFilter{{Field: "status", Operator: "eq", Value: "New"}},
				SortField: "created_at", SortOrder: "desc",
			},
			{
				Name: "co_hoi_chot_thang_nay", Label: "Cơ hội chốt tháng này", EntityName: "crm_opportunities",
				Filters:   []PackQueueFilter{{Field: "close_date", Operator: "eq", Value: "this_month"}},
				SortField: "close_date", SortOrder: "asc", Position: 1,
			},
		},

		// ------------------------------------------------------------------ workflows (PACK-WORKFLOWS)
		// Two automation rules that showcase the ASSIGN-1 (lead-routing) and TIMER-1 (follow-up SLA)
		// engine-depth primitives shipped in a pack. Install creates them LAST (Pass 8, after samples)
		// so they do NOT fire on the seed rows. Both go through the SAME config-time validation as
		// POST /admin/workflows (buildWorkflowRule + WF-01 field-target guard).
		Workflows: []PackWorkflow{
			{
				// ASSIGN-1 lead-routing: round-robin every NEW lead over "Đội Sales" into assigned_to.
				// `team` carries the team NAME here — install resolves it to the seeded team's id
				// (a pack cannot know the id). assigned_to is the `user` field added on crm_leads above.
				Name:       "Định tuyến lead mới",
				EntityName: "crm_leads",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "assign", Strategy: "round_robin", Team: "Đội Sales", Field: "assigned_to"},
				},
				IsActive: &active,
			},
			{
				// TIMER-1 date_offset: fire 1 day BEFORE a crm_activity's due_date (follow-up reminder).
				// due_date is a `date` field on crm_activities — buildTimerConfig accepts date OR datetime
				// as a timer anchor, so this installs cleanly without changing the field type.
				Name:       "Nhắc theo dõi hoạt động",
				EntityName: "crm_activities",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "due_date",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "days"},
						Direction: "before",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Sắp tới hạn theo dõi", Message: "Một hoạt động của bạn sắp tới hạn."},
				},
				IsActive: &active,
			},
			{
				// CONTRACT-RENEWAL reminder: fire 30 days BEFORE a contract's end_date, IF still active.
				// end_date is a `date` field — buildTimerConfig accepts date OR datetime as a timer anchor
				// (proven by crm_activities.due_date / lms_enrollments.tuition_due). The status gate (eq
				// "Hiệu lực") is re-evaluated at FIRE time on the CURRENT record, so a draft/renewed/expired
				// contract is NOT reminded — only a still-active one nearing its end.
				Name:       "Nhắc gia hạn hợp đồng",
				EntityName: "crm_contracts",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "status", Op: "eq", Value: "Hiệu lực"},
					},
					Timer: &PackWorkflowTimer{
						DateField: "end_date",
						Offset:    PackWorkflowTimerOffset{Amount: 30, Unit: "days"},
						Direction: "before",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Hợp đồng sắp hết hạn", Message: "Hợp đồng sắp hết hạn — cân nhắc gia hạn."},
				},
				IsActive: &active,
			},
			{
				// CONTRACT-RENEWAL auto-expire: fire 1 day AFTER a contract's end_date and, IF it is STILL
				// active, mark it expired. Direction "after" is the same TIMER-1 engine as the reminder
				// above, just the forward direction (mirrors Ticketing SLA-ESCALATION / LMS TUITION-OVERDUE).
				// The status gate (eq "Hiệu lực") is authoritative at FIRE time on the CURRENT record: a
				// contract renewed (status → "Đã gia hạn") or still draft before end_date+1day fails the gate
				// and is NOT touched — only a still-active past-end contract expires. field_update flips
				// status → "Hết hạn" (a valid client-writable picklist value, so WF-01 permits the overwrite).
				Name:       "Tự động hết hạn hợp đồng",
				EntityName: "crm_contracts",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "status", Op: "eq", Value: "Hiệu lực"},
					},
					Timer: &PackWorkflowTimer{
						DateField: "end_date",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "days"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "status", Value: "Hết hạn"},
					{Type: "in_app_notification", UserID: "$owner", Title: "Hợp đồng đã hết hạn", Message: "Hợp đồng đã hết hạn — trạng thái chuyển sang Hết hạn."},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ roles
		Roles: []PackRole{
			{
				Name:  "crm_sales_rep",
				Label: "Nhân viên kinh doanh",
				Actions: []PackRoleAction{
					// accounts: a rep MUST see the company list — every opportunity, quote and contract
					// hangs off an account, so "cannot see accounts" means "cannot link anything".
					// It was view=team, and team resolves through team_set_id, which is derived from
					// the CREATOR's memberships at insert time: seeded rows created by the installer
					// carry no team, so a rep saw an EMPTY list and could not create one either.
					// The acceptance gate read that as a pass because it only checks HTTP 200.
					{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_accounts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_accounts", Action: "edit", AccessLevel: "owner"},
					// products: the price list is reference data — read-only for a rep, curated by
					// the manager. A rep who cannot read it cannot pick a product on a quote line.
					{EntityName: "crm_products", Action: "view", AccessLevel: "all"},
					// contacts: team view, create all, own edit
					{EntityName: "crm_contacts", Action: "view", AccessLevel: "team"},
					{EntityName: "crm_contacts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_contacts", Action: "edit", AccessLevel: "owner"},
					// opportunities: own view/create/edit
					{EntityName: "crm_opportunities", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_opportunities", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_opportunities", Action: "edit", AccessLevel: "owner"},
					// leads: own view/create/edit
					{EntityName: "crm_leads", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_leads", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_leads", Action: "edit", AccessLevel: "owner"},
					// activities: own view/create/edit
					{EntityName: "crm_activities", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_activities", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_activities", Action: "edit", AccessLevel: "owner"},
					// quotes / quote lines: own view/create/edit (QUOTE-LINE-ITEMS)
					{EntityName: "crm_quotes", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_quotes", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_quotes", Action: "edit", AccessLevel: "owner"},
					{EntityName: "crm_quote_lines", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_quote_lines", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_quote_lines", Action: "edit", AccessLevel: "owner"},
					// contracts: own view/create/edit (CONTRACT-RENEWAL) — same shape as quotes.
					{EntityName: "crm_contracts", Action: "view", AccessLevel: "owner"},
					{EntityName: "crm_contracts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_contracts", Action: "edit", AccessLevel: "owner"},
				},
			},
			{
				Name:  "crm_sales_manager",
				Label: "Quản lý kinh doanh",
				Actions: []PackRoleAction{
					{EntityName: "crm_accounts", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_accounts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_accounts", Action: "edit", AccessLevel: "all"},
					// The manager owns the price list — a rep reads it, a manager curates it.
					{EntityName: "crm_products", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_products", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_products", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_products", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_accounts", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_contacts", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_contacts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_contacts", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_contacts", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_opportunities", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_opportunities", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_opportunities", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_opportunities", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_leads", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_leads", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_leads", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_leads", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_activities", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_activities", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_activities", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_activities", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_quotes", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_quotes", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_quotes", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_quotes", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_quote_lines", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_quote_lines", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_quote_lines", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_quote_lines", Action: "delete", AccessLevel: "all"},
					{EntityName: "crm_contracts", Action: "view", AccessLevel: "all"},
					{EntityName: "crm_contracts", Action: "create", AccessLevel: "all"},
					{EntityName: "crm_contracts", Action: "edit", AccessLevel: "all"},
					{EntityName: "crm_contracts", Action: "delete", AccessLevel: "all"},
				},
			},
		},
	}
}

// clinicPack is a minimal clinic vertical: patients → appointments. An appointment belongs to a
// patient (1-N). Demonstrates date/picklist fields and a unique patient code.
func clinicPack() *Pack {
	return &Pack{
		Manifest: &Manifest{
			Name:        "Clinic",
			Slug:        "clinic",
			Version:     "1.0.1", // 1.0.1: DP-2.1 — patient relation labelField "full_name"
			Description: "Bệnh nhân và lịch hẹn khám (phòng khám cơ bản).",
			Author:      "Nexus",
		},
		Name:        "Clinic",
		Description: "Bệnh nhân và lịch hẹn khám (phòng khám cơ bản).",
		Entities: []PackEntity{
			{
				Name:  "clinic_patients",
				Label: "Bệnh nhân",
				Fields: []PackField{
					{Name: "code", Label: "Mã bệnh nhân", FieldType: "text", IsUnique: true, IsRequired: true},
					{Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true},
					{Name: "dob", Label: "Ngày sinh", FieldType: "date"},
					{Name: "gender", Label: "Giới tính", FieldType: "picklist", Options: map[string]any{
						"values": []any{"Nam", "Nữ", "Khác"},
					}},
					{Name: "phone", Label: "Điện thoại", FieldType: "text"},
				},
			},
			{
				Name:  "clinic_appointments",
				Label: "Lịch hẹn",
				Fields: []PackField{
					{Name: "scheduled_date", Label: "Ngày hẹn", FieldType: "date", IsRequired: true},
					{Name: "reason", Label: "Lý do khám", FieldType: "text"},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", IsRequired: true, Options: map[string]any{
						"values": []any{"Đã đặt", "Đã xác nhận", "Hoàn thành", "Huỷ"},
					}},
					// belongs-to patient (1-N).
					{Name: "patient", Label: "Bệnh nhân", FieldType: "relation", Options: map[string]any{
						// clinic_patients labels itself by "full_name" (no "name" field) — see DP-2.1.
						"labelField":    "full_name",
						"relatedEntity": "clinic_patients",
					}},
				},
			},
		},
		Layouts: []PackLayout{
			{EntityName: "clinic_patients", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "code"},
					map[string]any{"field": "full_name"},
					map[string]any{"field": "phone"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "clinic_appointments", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "scheduled_date"},
					map[string]any{"field": "status"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
		},
		Roles: []PackRole{
			{
				Name:  "clinic_receptionist",
				Label: "Lễ tân",
				Actions: []PackRoleAction{
					{EntityName: "clinic_patients", Action: "view", AccessLevel: "all"},
					{EntityName: "clinic_patients", Action: "create", AccessLevel: "all"},
					{EntityName: "clinic_patients", Action: "edit", AccessLevel: "all"},
					{EntityName: "clinic_appointments", Action: "view", AccessLevel: "all"},
					{EntityName: "clinic_appointments", Action: "create", AccessLevel: "all"},
					{EntityName: "clinic_appointments", Action: "edit", AccessLevel: "all"},
				},
			},
			{
				Name:  "clinic_doctor",
				Label: "Bác sĩ",
				Actions: []PackRoleAction{
					{EntityName: "clinic_patients", Action: "view", AccessLevel: "all"},
					{EntityName: "clinic_appointments", Action: "view", AccessLevel: "owner"},
					{EntityName: "clinic_appointments", Action: "edit", AccessLevel: "owner"},
				},
			},
		},
	}
}

// ticketingPack is the built-in Support/Ticketing vertical (campaign-3): two entities
// (tk_customers → tk_tickets) that showcase ALL FOUR engine-depth primitives now shippable in a
// pack — timer/SLA, polymorphic, dashboard, and assignment:
//   - TIMER-1 date_offset (timer/SLA): the "Nhắc hạn SLA" workflow fires 2 hours BEFORE a ticket's
//     due_date.
//   - polymorphic_relation: tk_tickets.related_to points at EITHER a customer OR a parent ticket
//     (options.targetEntities ["tk_customers","tk_tickets"] — a self-reference plus a sibling).
//   - dashboard reporting: "Tổng quan Ticketing" groups tk_tickets by status + priority, which is
//     why those two picklists carry Options["indexed"]=true (a GROUP BY on a non-indexed JSON field
//     would full-scan; the indexed generated column lets MySQL group without one — B1).
//   - ASSIGN-1 (assignment): the "Định tuyến ticket mới" workflow round-robins every NEW ticket
//     over the "Đội hỗ trợ" team into the assigned_to field. PACK-TEAMS seeds that team (the pack
//     names it; install resolves the name → the seeded team's id), so the routing rule ships in the
//     pack instead of requiring a hand-built team + id.
//   - PACK-WORKFLOWS: all automation rules ship in the pack and install through the SAME validated
//     admin path as POST /admin/workflows (buildWorkflowRule + WF-01 field-target guard).
//
// v1.1.0 (SLA-ESCALATION) adds a past-due escalation rule that composes shipped primitives (no new
// engine code): "Leo thang ticket quá hạn SLA" is a date_offset TIMER-1 firing 1 hour AFTER a
// ticket's due_date (Direction "after" — the same timer engine as "Nhắc hạn SLA", just the other
// direction). Its condition GATES on status via match "or" over two `eq` rules (status Open OR
// Pending) — the workflow condition evaluator has no `in` operator, so two `eq` rules joined by
// "or" is the idiomatic membership gate. The gate is authoritative at FIRE time: the workflow_timer
// job handler re-evaluates the condition on the CURRENT record, so a ticket resolved/closed before
// due_date+1h fails the gate and is NOT escalated. When it does fire it runs three actions —
// field_update priority → "Urgent" (a valid tk_tickets.priority value; a picklist is client-writable
// so WF-01 permits the overwrite), assign round_robin over the new "Đội quản lý SLA" escalation team
// into assigned_to (re-routing off the front-line agent to the SLA-management tier), and an $owner
// in_app_notification. The two due_date timers ("before" reminder + "after" escalation) park
// independently (the wake-up payload keys on the workflow id), so both coexist on the same anchor.
//
// ENTITY ORDER (load-bearing): tk_customers MUST precede tk_tickets in the Entities slice. When
// CreateEntity materialises tk_tickets, its polymorphic related_to targets BOTH tk_tickets (self —
// skipped, its table is created in the same call) AND tk_customers (a sibling that must already
// exist, else 422 FIELD_POLYMORPHIC_TARGET_NOT_FOUND). The regular `customer` relation (→
// tk_customers) additionally makes orderEntities order tk_customers first regardless of slice order,
// but we keep the slice order explicit so the dependency is obvious on the page.
func ticketingPack() *Pack {
	active := true
	return &Pack{
		Manifest: &Manifest{
			Name:        "Ticketing",
			Slug:        "ticketing",
			Version:     "1.8.0", // 1.8.0: #81 — đổi ưu tiên tính lại hạn (chỉ rút ngắn, không đè hạn gõ tay) + trường dấu due_date_auto; 1.7.0: SLA-DEADLINE — 4 bậc set_deadline tự điền due_date (giờ làm việc); 1.6.0: valueLabels tiếng Việt cho status/priority/channel; 1.5.0: assigned_to kiểu `user`
			Description: "Khách hàng và phiếu hỗ trợ (ticket) với SLA, phân loại và báo cáo.",
			Author:      "Nexus",
		},
		Name:        "Ticketing",
		Description: "Khách hàng và phiếu hỗ trợ (ticket) với SLA, phân loại và báo cáo.",

		// ------------------------------------------------------------------ entities
		Entities: []PackEntity{
			// tk_customers — the party a ticket is about. MUST come before tk_tickets so the
			// polymorphic related_to target ("tk_customers") exists at tk_tickets create time.
			{
				Name:  "tk_customers",
				Label: "Khách hàng",
				Fields: []PackField{
					{Name: "name", Label: "Tên", FieldType: "text", IsRequired: true},
					{Name: "email", Label: "Email", FieldType: "text"},
					{Name: "phone", Label: "Điện thoại", FieldType: "text"},
					{Name: "company", Label: "Công ty", FieldType: "text"},
				},
			},

			// tk_tickets — the support ticket. belongs-to one customer (1-N FK-on-child) and carries
			// a polymorphic related_to (customer OR parent ticket).
			{
				Name:  "tk_tickets",
				Label: "Phiếu hỗ trợ",
				Fields: []PackField{
					{Name: "subject", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
					{Name: "description", Label: "Mô tả", FieldType: "text"},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Open", "Pending", "Resolved", "Closed"},
						"valueLabels": map[string]any{"Open": "Đang mở", "Pending": "Chờ xử lý", "Resolved": "Đã xử lý", "Closed": "Đã đóng"},
						"indexed":     true, // B1: GROUP BY status on the dashboard
					}},
					{Name: "priority", Label: "Ưu tiên", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Low", "Medium", "High", "Urgent"},
						"valueLabels": map[string]any{"Low": "Thấp", "Medium": "Trung bình", "High": "Cao", "Urgent": "Khẩn"},
						"indexed":     true, // B1: GROUP BY priority on the dashboard
					}},
					{Name: "channel", Label: "Kênh", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"Email", "Phone", "Web", "Chat"},
						"valueLabels": map[string]any{"Email": "Email", "Phone": "Điện thoại", "Web": "Website", "Chat": "Chat"},
					}},
					{Name: "due_date", Label: "Hạn xử lý", FieldType: "datetime"},
					// #81 (1.8.0): the machine's signature on due_date. Every SLA rule that writes a
					// deadline writes the same instant here, so the recompute tiers below can tell a
					// deadline the software promised (due_date == due_date_auto) from one an agent typed
					// (they differ, or there is no stamp) and never shorten the latter. On no layout:
					// it is bookkeeping, not something an agent reads or edits.
					{Name: "due_date_auto", Label: "Hạn SLA do máy đặt", FieldType: "datetime"},
					// 1-N: ticket belongs to one customer. Explicit inverseField: the parent side on
					// tk_customers is discoverable as "tickets".
					{Name: "customer", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "tk_customers",
						"inverseField":  "tickets",
					}},
					// POLYMORPHIC-1: what this ticket is ABOUT — a customer or a PARENT ticket
					// (self-reference). Optional + unseeded in v1 (a polymorphic {type,id:@ref} in
					// samples is not supported yet — see the Samples note below).
					{Name: "related_to", Label: "Liên quan tới", FieldType: "polymorphic_relation", Options: map[string]any{
						"targetEntities": []any{"tk_customers", "tk_tickets"},
					}},
					// ASSIGN-1 routing target: the "Định tuyến ticket mới" workflow writes the picked
					// agent's userId here. `user`, NOT `text` — see the identical field on crm_leads: the
					// "relation-to-users is impossible" note predates the `user` field type and is what made
					// support agents read a raw UUID. Existing installs keep text and keep working.
					{Name: "assigned_to", Label: "Phân công cho", FieldType: "user"},
				},
			},
		},

		// ------------------------------------------------------------------ sample data
		// 4 customers → 6 tickets (varied status/priority/channel so the dashboard has real buckets).
		// "@ref" values in Data are resolved to real UUIDs at install (Pass 3). related_to is left
		// UNSET on every ticket: a polymorphic {type,id:@ref} in samples is not supported yet (the
		// @ref resolver only rewrites top-level "@" string values, not the {type,id} object shape).
		Samples: []PackSample{
			{
				Entity: "tk_customers",
				Records: []PackRecord{
					{Ref: "@c_acme", Data: map[string]any{"name": "Acme Corp", "email": "support@acme.example.com", "phone": "+1-555-0100", "company": "Acme Corp"}},
					{Ref: "@c_globex", Data: map[string]any{"name": "Globex Inc", "email": "help@globex.example.com", "phone": "+1-555-0200", "company": "Globex Inc"}},
					{Ref: "@c_initech", Data: map[string]any{"name": "Initech", "email": "it@initech.example.com", "phone": "+1-555-0300", "company": "Initech"}},
					{Ref: "@c_umbrella", Data: map[string]any{"name": "Umbrella Health", "email": "care@umbrella.example.com", "phone": "+1-555-0400", "company": "Umbrella Health"}},
				},
			},
			{
				Entity: "tk_tickets",
				Records: []PackRecord{
					{Data: map[string]any{"subject": "Không đăng nhập được", "description": "Người dùng báo lỗi 401 khi đăng nhập.", "status": "Open", "priority": "High", "channel": "Email", "due_date": "2026-07-15T10:00:00Z", "customer": "@c_acme"}},
					{Data: map[string]any{"subject": "Hệ thống chạy chậm", "description": "Trang danh sách tải hơn 10 giây.", "status": "Open", "priority": "Urgent", "channel": "Phone", "due_date": "2026-07-12T09:00:00Z", "customer": "@c_acme"}},
					{Data: map[string]any{"subject": "Yêu cầu xuất hoá đơn", "description": "Cần hoá đơn VAT cho tháng 6.", "status": "Pending", "priority": "Medium", "channel": "Web", "due_date": "2026-07-20T17:00:00Z", "customer": "@c_globex"}},
					{Data: map[string]any{"subject": "Đổi email liên hệ", "description": "Cập nhật email chính của tài khoản.", "status": "Resolved", "priority": "Low", "channel": "Chat", "due_date": "2026-07-10T14:00:00Z", "customer": "@c_globex"}},
					{Data: map[string]any{"subject": "Tài khoản bị khoá", "description": "Khoá sau nhiều lần đăng nhập sai.", "status": "Closed", "priority": "Medium", "channel": "Email", "due_date": "2026-07-08T11:00:00Z", "customer": "@c_initech"}},
					{Data: map[string]any{"subject": "Hỏi về gói dịch vụ", "description": "Khách muốn nâng cấp gói.", "status": "Open", "priority": "High", "channel": "Web", "due_date": "2026-07-18T16:00:00Z", "customer": "@c_umbrella"}},
				},
			},
		},

		// ------------------------------------------------------------------ layouts
		Layouts: []PackLayout{
			{EntityName: "tk_customers", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "name"},
					map[string]any{"field": "email"},
					map[string]any{"field": "phone"},
					map[string]any{"field": "company"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "tk_tickets", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "subject"},
					map[string]any{"field": "status"},
					map[string]any{"field": "priority"},
					map[string]any{"field": "channel"},
					map[string]any{"field": "customer"},
					map[string]any{"field": "due_date"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "tk_customers", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin khách hàng",
						"columns": 2,
						"fields":  []any{"name", "email", "phone", "company"},
					},
				},
			}},
			// related_to (polymorphic) is not placed in a section-based layout — the attach picker
			// lives on the /relations API, mirroring how crm_activities.related_to is handled.
			{EntityName: "tk_tickets", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin phiếu",
						"columns": 2,
						"fields":  []any{"subject", "status", "priority", "channel", "due_date", "customer"},
					},
					map[string]any{
						"title":   "Mô tả",
						"columns": 1,
						"fields":  []any{"description"},
					},
				},
			}},
		},

		// ------------------------------------------------------------------ dashboards
		// "Tổng quan Ticketing" seeds a KPI + two grouped bars so a freshly-installed Support tenant
		// gets a working ticket overview on real data. w2/w3 GROUP BY status/priority — both indexed
		// generated columns (B1), so the aggregate engine groups without a full JSON_EXTRACT scan.
		Dashboards: []PackDashboard{
			{
				Name:        "Tổng quan Ticketing",
				Description: "Tổng số phiếu và phân bố theo trạng thái / ưu tiên.",
				Widgets: []PackWidget{
					{ID: "w1", Title: "Tổng ticket", Type: "kpi", Entity: "tk_tickets", Metric: "count"},
					{ID: "w2", Title: "Theo trạng thái", Type: "bar", Entity: "tk_tickets", Metric: "count", GroupBy: "status"},
					{ID: "w3", Title: "Theo ưu tiên", Type: "bar", Entity: "tk_tickets", Metric: "count", GroupBy: "priority"},
				},
			},
		},

		// ------------------------------------------------------------------ reports (PACK-REPORTS)
		// Two canned saved reports so a freshly-installed Support tenant gets working saved reports.
		// "Ticket theo trạng thái": COUNT of tk_tickets GROUP BY status — status is an indexed
		// picklist (B1), so the aggregate engine groups without a full-scan at run time. "Danh sách
		// ticket đang mở" (PACK-REPORTS-WORKLIST): a tabular open-work queue filtered status in
		// {Open, Pending}. Both installed through the SAME validated CreateReport path as
		// POST /admin/reports; idempotent by name on re-install; NOT exported (pack-authored only).
		Reports: []PackReport{
			{
				Name:       "Ticket theo trạng thái",
				EntityName: "tk_tickets",
				Type:       "summary",
				Config: PackReportConfig{
					Metric:  "count",
					GroupBy: "status",
				},
				IsActive: &active,
			},
			{
				// PACK-REPORTS-WORKLIST (tabular): the open-work queue — every ticket still needing
				// attention (status in {Open, Pending}). The `in` operator is valid on a picklist
				// (multiValueTypes), so ONE filter with two values captures both open states without
				// a second report; both values are byte-identical to the status picklist options.
				Name:       "Danh sách ticket đang mở",
				EntityName: "tk_tickets",
				Type:       "tabular",
				Config: PackReportConfig{
					Columns: []string{"subject", "priority", "status", "due_date", "assigned_to"},
					Filters: []PackReportFilter{{Field: "status", Op: "in", Values: []string{"Open", "Pending"}}},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ teams (PACK-TEAMS)
		// Two teams the routing/escalation rules below round-robin over. Install seeds each (idempotent
		// by name) AFTER roles and BEFORE workflows, adds the install user as its first active member,
		// and maps its NAME → the minted id so the assign actions resolve.
		//   - "Đội hỗ trợ": the front-line support tier "Định tuyến ticket mới" assigns new tickets to.
		//   - "Đội quản lý SLA": the escalation tier "Leo thang ticket quá hạn SLA" re-routes a past-due
		//     ticket to (SLA-ESCALATION). A distinct tier is the realistic shape — escalation hands the
		//     ticket up to management, not back into the same round-robin ring.
		Teams: []PackTeam{
			{Name: "Đội hỗ trợ"},
			{Name: "Đội quản lý SLA"},
		},

		// ------------------------------------------------------------------ work queues (PACK-QUEUES)
		// The SLA cockpit: agent-scoped due-today inbox (lte + "today" resolves to end-of-day, so
		// overdue tickets ride along) and an everyone queue for open urgent work.
		WorkQueues: []PackWorkQueue{
			{
				Name: "ticket_toi_han_hom_nay", Label: "Ticket tới hạn hôm nay", EntityName: "tk_tickets",
				RoleName: "tk_agent",
				Filters: []PackQueueFilter{
					{Field: "due_date", Operator: "lte", Value: "today"},
					{Field: "status", Operator: "in", Value: "Open,Pending"},
				},
				// due_date is datetime — not D1-indexable (RFC3339 generated-column gap), so RM-09
				// forbids sorting on it. Oldest-first by created_at is the working proxy.
				SortField: "created_at", SortOrder: "asc",
			},
			{
				Name: "ticket_khan_dang_mo", Label: "Ticket khẩn đang mở", EntityName: "tk_tickets",
				Filters: []PackQueueFilter{
					{Field: "priority", Operator: "in", Value: "High,Urgent"},
					{Field: "status", Operator: "in", Value: "Open,Pending"},
				},
				SortField: "created_at", SortOrder: "asc", Position: 1,
			},
		},

		// ------------------------------------------------------------------ workflows
		// Automation rules that showcase PACK-WORKFLOWS + the TIMER-1 (timer/SLA), ASSIGN-1
		// (assignment) and SLA-DEADLINE (set_deadline) primitives. Install creates them LAST
		// (Pass 8, after samples) so they do NOT fire on the seed inserts.
		//
		// v1.7.0 (SLA-DEADLINE, DAILY-QUEUE #11) closes the hole that made every rule below
		// decorative on a fresh install: BOTH due_date timers ("Nhắc hạn SLA" and "Leo thang ticket
		// quá hạn SLA") anchor on tk_tickets.due_date, and nothing ever FILLED due_date. An agent
		// raising a ticket got no reminder and no escalation unless a human typed a deadline by
		// hand — or unless an admin hand-built the missing rule, which is the "nối tay" the queue
		// item names. The four tiers below stamp it with set_deadline (specs/037), so the pack's
		// SLA works the moment it is installed.
		Workflows: []PackWorkflow{
			// SLA-DEADLINE tiers. Four after_create rules, one per service level, each writing the
			// commitment into due_date in BUSINESS hours — the unit DAILY-QUEUE #10 opened, so a
			// ticket raised 16:00 Friday is due Monday morning rather than at 20:00 on a Friday
			// nobody is at work. Three properties are load-bearing; changing any of them silently
			// breaks a promise, so ticketing_sla_test.go pins all three:
			//
			//   1. EVERY tier is guarded by `due_date isnull`. set_deadline OVERWRITES by design
			//      (specs/037 §4) and due_date sits on the create form, so without the guard the
			//      machine would replace a deadline the agent negotiated with the customer —
			//      silently, which is the bug class DAILY-QUEUE #63 exists for.
			//   2. The tiers COVER every priority, including a ticket raised with none. The
			//      standard tier is expressed with `neq` rather than `eq "Low"` precisely for
			//      that: an unset priority is not equal to Urgent/High/Medium either, so it falls
			//      into the standard tier instead of dropping out of SLA entirely. (The evaluator
			//      has no `in`; a single flat match level is all a condition has.)
			//   3. The tiers do not overlap, so exactly one rule stamps the field. The other three
			//      record a `skipped` run — that is the W6 trust signal, not noise.
			//
			// The deadline is stamped BEFORE the timers park: the dispatcher runs the whole action
			// loop first and only then reads the record back to schedule date_offset wake-ups
			// (AsyncDispatcher.dispatch — the property DAILY-QUEUE #6 established). So a
			// machine-set due_date is what both timers anchor on, with no ordering dependency
			// against the routing rule that also runs on after_create.
			{
				Name:       "Đặt hạn SLA — phiếu Khẩn",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "due_date", Op: "isnull"},
						{Field: "priority", Op: "eq", Value: "Urgent"},
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Offset: &PackWorkflowTimerOffset{Amount: 4, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				Name:       "Đặt hạn SLA — phiếu Cao",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "due_date", Op: "isnull"},
						{Field: "priority", Op: "eq", Value: "High"},
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Offset: &PackWorkflowTimerOffset{Amount: 8, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				Name:       "Đặt hạn SLA — phiếu Trung bình",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "due_date", Op: "isnull"},
						{Field: "priority", Op: "eq", Value: "Medium"},
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Offset: &PackWorkflowTimerOffset{Amount: 24, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				// The standard tier AND the catch-all: "Thấp", plus anything the picklist gains
				// later, plus a ticket raised with no priority at all. See note 2 above for why
				// this is three `neq` rules and not `eq "Low"`.
				Name:       "Đặt hạn SLA — phiếu thường",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "due_date", Op: "isnull"},
						{Field: "priority", Op: "neq", Value: "Urgent"},
						{Field: "priority", Op: "neq", Value: "High"},
						{Field: "priority", Op: "neq", Value: "Medium"},
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Offset: &PackWorkflowTimerOffset{Amount: 40, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			// #81 recompute tiers (1.8.0). Changing a ticket's priority re-derives its deadline as if
			// it had been raised at the new level — FROM CREATION (anchor created_at), because the
			// promise was made when the customer asked, not when someone re-triaged. tighten_only
			// applies it only when it is EARLIER than the deadline on the ticket and only when that
			// deadline is the machine's own (due_date still equals the due_date_auto stamp): a
			// promised deadline is never lengthened, a hand-typed one never touched, and a ticket
			// stamped before 1.8.0 (no stamp) is never shortened. ticketing_sla_test.go pins that
			// each tier's amount equals its creation tier's — the two must never disagree.
			{
				Name:       "Tính lại hạn SLA — đổi sang Khẩn",
				EntityName: "tk_tickets",
				Trigger:    "after_update",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{{Field: "priority", Op: "changedTo", Value: "Urgent"}},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Mode: "tighten_only", Anchor: "created_at",
						Offset: &PackWorkflowTimerOffset{Amount: 4, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				Name:       "Tính lại hạn SLA — đổi sang Cao",
				EntityName: "tk_tickets",
				Trigger:    "after_update",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{{Field: "priority", Op: "changedTo", Value: "High"}},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Mode: "tighten_only", Anchor: "created_at",
						Offset: &PackWorkflowTimerOffset{Amount: 8, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				Name:       "Tính lại hạn SLA — đổi sang Trung bình",
				EntityName: "tk_tickets",
				Trigger:    "after_update",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{{Field: "priority", Op: "changedTo", Value: "Medium"}},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Mode: "tighten_only", Anchor: "created_at",
						Offset: &PackWorkflowTimerOffset{Amount: 24, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				Name:       "Tính lại hạn SLA — đổi sang Thấp",
				EntityName: "tk_tickets",
				Trigger:    "after_update",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{{Field: "priority", Op: "changedTo", Value: "Low"}},
				},
				Actions: []PackWorkflowAction{
					{Type: "set_deadline", Field: "due_date", StampField: "due_date_auto", Mode: "tighten_only", Anchor: "created_at",
						Offset: &PackWorkflowTimerOffset{Amount: 40, Unit: "business_hours"}},
				},
				IsActive: &active,
			},
			{
				// TIMER-1 date_offset: fire 2 hours BEFORE the ticket's due_date (SLA reminder).
				Name:       "Nhắc hạn SLA",
				EntityName: "tk_tickets",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "due_date",
						Offset:    PackWorkflowTimerOffset{Amount: 2, Unit: "hours"},
						Direction: "before",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Ticket sắp tới hạn SLA", Message: "Ticket của bạn sắp tới hạn xử lý."},
				},
				IsActive: &active,
			},
			{
				// after_create: acknowledge every new ticket to its owner.
				Name:       "Tự động phản hồi ticket mới",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Ticket mới đã tạo", Message: "Một ticket mới vừa được ghi nhận."},
				},
				IsActive: &active,
			},
			{
				// ASSIGN-1 routing: round-robin every NEW ticket over "Đội hỗ trợ" into assigned_to.
				// `team` carries the team NAME here — install resolves it to the seeded team's id
				// (a pack cannot know the id). This is the 4th engine-depth primitive shipped in a pack.
				Name:       "Định tuyến ticket mới",
				EntityName: "tk_tickets",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "assign", Strategy: "round_robin", Team: "Đội hỗ trợ", Field: "assigned_to"},
				},
				IsActive: &active,
			},
			{
				// SLA-ESCALATION (v1.1.0) date_offset: fire 1 hour AFTER a ticket's due_date and, IF the
				// ticket is STILL unresolved, escalate it. Direction "after" is the same TIMER-1 engine as
				// "Nhắc hạn SLA" above, just the forward direction. The condition GATES on status: the
				// evaluator has no `in` operator, so membership is match "or" over two `eq` rules (Open OR
				// Pending). The gate is re-evaluated at FIRE time on the CURRENT record (the workflow_timer
				// handler), so a ticket resolved/closed before due_date+1h fails it and is NOT escalated —
				// only a still-open past-due ticket escalates. Actions: bump priority → "Urgent"
				// (field_update; priority is a client-writable picklist so WF-01 permits it), re-route off
				// the front-line agent to the "Đội quản lý SLA" tier (assign round_robin into assigned_to),
				// and notify the owner.
				Name:       "Leo thang ticket quá hạn SLA",
				EntityName: "tk_tickets",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Match: "or",
					Rules: []PackWorkflowRule{
						{Field: "status", Op: "eq", Value: "Open"},
						{Field: "status", Op: "eq", Value: "Pending"},
					},
					Timer: &PackWorkflowTimer{
						DateField: "due_date",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "hours"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "Urgent"},
					{Type: "assign", Strategy: "round_robin", Team: "Đội quản lý SLA", Field: "assigned_to"},
					{Type: "in_app_notification", UserID: "$owner", Title: "Ticket quá hạn SLA — đã leo thang", Message: "Ticket của bạn đã quá hạn SLA và được nâng mức ưu tiên lên Urgent, phân công cho đội quản lý SLA."},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ roles
		Roles: []PackRole{
			{
				Name:  "tk_agent",
				Label: "Nhân viên hỗ trợ",
				Actions: []PackRoleAction{
					{EntityName: "tk_tickets", Action: "view", AccessLevel: "all"},
					{EntityName: "tk_tickets", Action: "create", AccessLevel: "all"},
					{EntityName: "tk_tickets", Action: "edit", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "view", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "create", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "edit", AccessLevel: "all"},
				},
			},
			{
				Name:  "tk_manager",
				Label: "Quản lý hỗ trợ",
				Actions: []PackRoleAction{
					{EntityName: "tk_tickets", Action: "view", AccessLevel: "all"},
					{EntityName: "tk_tickets", Action: "create", AccessLevel: "all"},
					{EntityName: "tk_tickets", Action: "edit", AccessLevel: "all"},
					{EntityName: "tk_tickets", Action: "delete", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "view", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "create", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "edit", AccessLevel: "all"},
					{EntityName: "tk_customers", Action: "delete", AccessLevel: "all"},
				},
			},
		},
	}
}

// lmsPack is the built-in Education/LMS vertical (campaign-3 finale, the 3rd shipped vertical after
// CRM + Ticketing). It is deliberately scoped to demonstrate the two engine-depth primitives the
// other two packs DON'T lead with — parent-side ROLLUP (a gradebook) and a date_offset TIMER (a
// tuition-due reminder) — rather than re-demonstrating polymorphic (CRM/Ticketing) or N-N.
//
// The model is a classic junction: lms_courses and lms_students are the two parents, lms_enrollments
// is the child of BOTH (two 1-N FK-on-child relations — those two relations ARE the many-to-many, so
// no explicit N-N join table is needed). Two parent-side rollups on lms_courses (deferred to Pass 2b,
// exactly like crm_accounts' rollups — the parent is created before the child, so an inline rollup
// over lms_enrollments would fail config-time with FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND):
//   - enrollment_count (count over the `course` 1-N) — how many enrolments a course has.
//   - avg_grade (avg over the child `grade` number field) — the course's mean grade. Rollups resolve
//     ONLY with an explicit ?rollup= opt-in and are NEVER seeded (readonly → 422 aborts install).
//
// Automation (PACK-TEAMS + PACK-WORKFLOWS, same validated admin path as POST /admin/workflows):
//   - TIMER-1 date_offset: "Nhắc đóng học phí" fires 3 days BEFORE an enrolment's tuition_due (a
//     `date` field — buildTimerConfig accepts date OR datetime as a timer anchor).
//   - ASSIGN-1: "Phân bổ tư vấn viên" round-robins every NEW enrolment over "Đội Tư vấn" into the
//     assigned_advisor field. `team` carries the NAME (a pack cannot know the seeded id);
//     install resolves it to the team's real id. assigned_advisor is a `user` field from v1.4.0 (fresh
//     installs); older tenants keep `text`, which assign still accepts (assignTargetTypeOK).
//
// v1.1.0 (TUITION-OVERDUE) adds a past-due escalation rule composing shipped primitives (no new
// engine code), directly paralleling the Ticketing SLA-ESCALATION: "Đánh dấu quá hạn học phí" is a
// date_offset TIMER-1 firing 1 day AFTER an enrolment's tuition_due (Direction "after" — the same
// timer engine as "Nhắc đóng học phí", just the forward direction). Its condition GATES on status
// via a single `eq` rule (status "Chờ đóng") — status is a 3-value picklist (Chờ đóng / Đã đóng /
// Quá hạn), so ONE eq on the still-pending value is the whole membership gate (no `or` needed); the
// default match "and" over one rule is correct. The gate is authoritative at FIRE time: the
// workflow_timer job handler re-evaluates the condition on the CURRENT record, so an enrolment PAID
// (status → "Đã đóng") before tuition_due+1day fails the gate and is NOT marked overdue. When it
// does fire it runs two actions — field_update status → "Quá hạn" (a valid lms_enrollments.status
// value; a picklist is client-writable so WF-01 permits the overwrite) and an $owner
// in_app_notification. The two tuition_due timers ("before" reminder + "after" overdue) park
// independently (the wake-up payload keys on the workflow id), so both coexist on the same anchor.
//
// ENTITY ORDER (load-bearing): the two parents (lms_courses, lms_students) MUST precede the junction
// lms_enrollments so its two relation targets exist when its FK columns are materialised — mirroring
// tk_customers before tk_tickets. (orderEntities also topo-sorts by the relations, but the slice
// order keeps the dependency obvious on the page.)
//
// B1 note: status on BOTH lms_courses and lms_enrollments carries Options["indexed"]=true so the two
// dashboard GROUP BY queries hit an indexed generated column instead of a full JSON_EXTRACT scan.
func lmsPack() *Pack {
	active := true
	return &Pack{
		Manifest: &Manifest{
			Name:        "LMS",
			Slug:        "lms",
			Version:     "1.4.0", // 1.4.0: #243 — assigned_advisor kiểu `user` (lượt cài mới; tenant cũ giữ text); 1.3.1: DP-2.1 — course relation labelField "title" (expand label was empty)
			Description: "Khóa học, học viên và ghi danh với học phí, điểm số và nhắc hạn.",
			Author:      "Nexus",
		},
		Name:        "LMS",
		Description: "Khóa học, học viên và ghi danh với học phí, điểm số và nhắc hạn.",

		// ------------------------------------------------------------------ entities
		Entities: []PackEntity{
			// lms_courses — a course. Parent of lms_enrollments (1-N). MUST precede lms_enrollments.
			{
				Name:  "lms_courses",
				Label: "Khóa học",
				Fields: []PackField{
					{Name: "title", Label: "Tên khóa học", FieldType: "text", IsRequired: true},
					{Name: "code", Label: "Mã khóa học", FieldType: "text", IsUnique: true},
					{Name: "tuition", Label: "Học phí", FieldType: "currency", Options: map[string]any{
						"currencyCode": "VND",
					}},
					{Name: "start_date", Label: "Ngày khai giảng", FieldType: "date"},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":  []any{"Nháp", "Đang mở", "Đang học", "Kết thúc"},
						"indexed": true, // B1: GROUP BY status on the dashboard
					}},
				},
			},

			// lms_students — a learner. Parent of lms_enrollments (1-N). MUST precede lms_enrollments.
			{
				Name:  "lms_students",
				Label: "Học viên",
				Fields: []PackField{
					{Name: "name", Label: "Họ tên", FieldType: "text", IsRequired: true},
					{Name: "email", Label: "Email", FieldType: "text"},
					{Name: "phone", Label: "Điện thoại", FieldType: "text"},
				},
			},

			// lms_enrollments — the junction: an enrolment links ONE course + ONE student (two 1-N
			// FK-on-child relations that together model the course↔student many-to-many).
			{
				Name:  "lms_enrollments",
				Label: "Ghi danh",
				Fields: []PackField{
					// 1-N: enrolment belongs to one course. Explicit inverseField: the parent side on
					// lms_courses is discoverable as "enrollments".
					{Name: "course", Label: "Khóa học", FieldType: "relation", Options: map[string]any{
						// lms_courses has no "name" field — its human title lives in "title".
						// Without this, ?expand=course resolves {id, label:""} (DP-2.1).
						"labelField":    "title",
						"relatedEntity": "lms_courses",
						"inverseField":  "enrollments",
					}},
					// 1-N: enrolment belongs to one student. inverseField "enrollments" lands on
					// lms_students (a DIFFERENT entity from lms_courses' inverse — no collision).
					{Name: "student", Label: "Học viên", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "lms_students",
						"inverseField":  "enrollments",
					}},
					{Name: "grade", Label: "Điểm số", FieldType: "number", Options: map[string]any{
						"min":  float64(0),
						"max":  float64(10),
						"step": float64(0.1),
					}},
					{Name: "tuition_due", Label: "Hạn đóng học phí", FieldType: "date", Options: map[string]any{
						"indexed": true, // PACK-QUEUES: sortable queue "hoc_phi_qua_han" + fast overdue filters
					}},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":  []any{"Chờ đóng", "Đã đóng", "Quá hạn"},
						"indexed": true, // B1: GROUP BY status on the dashboard
					}},
					// ASSIGN-1 routing target: the "Phân bổ tư vấn viên" workflow writes the picked
					// advisor's userId here. A `user` field (v1.4.0, #243) so the screen shows a name via
					// ?expand=; tenants installed before keep `text`, which assign still accepts
					// (assignTargetTypeOK). Left unset on samples (routing fires only on
					// live after_create; the workflows pass installs AFTER the seed rows, so seeds are
					// never routed).
					{Name: "assigned_advisor", Label: "Tư vấn viên phụ trách", FieldType: "user"},
				},
			},
		},

		// ------------------------------------------------------------------ deferred fields
		// Parent-side rollups on lms_courses (the gradebook), added in Pass 2b AFTER lms_enrollments
		// exists. They CANNOT go inline: lms_courses is created FIRST by dependency order, so a rollup
		// over lms_enrollments would fail config-time (FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND). count
		// carries NO aggField; avg REQUIRES one (the child `grade` number field). Re-install is
		// idempotent: FIELD_ALREADY_EXISTS is a skip.
		DeferredFields: []PackDeferredField{
			{EntityName: "lms_courses", Field: PackField{
				Name: "enrollment_count", Label: "Số ghi danh", FieldType: "rollup",
				Options: map[string]any{"childEntity": "lms_enrollments", "viaRelation": "course", "metric": "count"},
			}},
			{EntityName: "lms_courses", Field: PackField{
				Name: "avg_grade", Label: "Điểm trung bình", FieldType: "rollup",
				Options: map[string]any{"childEntity": "lms_enrollments", "viaRelation": "course", "metric": "avg", "aggField": "grade"},
			}},
		},

		// ------------------------------------------------------------------ sample data
		// 3 courses (varied status) → 4 students → 6 enrolments. "@ref" values in Data are resolved to
		// real UUIDs at install (Pass 3). @co_go carries 3 enrolments (grades 8.5/7.5/9.2 → avg 8.4) so
		// the gradebook rollup is a discriminating assertion. Rollup fields (enrollment_count/avg_grade)
		// and assigned_advisor are NEVER seeded (readonly rollup → 422 abort; advisor is routing-filled).
		Samples: []PackSample{
			{
				Entity: "lms_courses",
				Records: []PackRecord{
					{Ref: "@co_go", Data: map[string]any{"title": "Lập trình Go cơ bản", "code": "GO-101", "tuition": map[string]any{"amount": float64(5000000), "currencyCode": "VND"}, "start_date": "2026-08-01", "status": "Đang học"}},
					{Ref: "@co_react", Data: map[string]any{"title": "React nâng cao", "code": "REACT-201", "tuition": map[string]any{"amount": float64(6000000), "currencyCode": "VND"}, "start_date": "2026-09-01", "status": "Đang mở"}},
					{Ref: "@co_ai", Data: map[string]any{"title": "Nhập môn Trí tuệ nhân tạo", "code": "AI-301", "tuition": map[string]any{"amount": float64(8000000), "currencyCode": "VND"}, "start_date": "2026-10-01", "status": "Nháp"}},
				},
			},
			{
				Entity: "lms_students",
				Records: []PackRecord{
					{Ref: "@s1", Data: map[string]any{"name": "Nguyễn Văn An", "email": "an@student.example.com", "phone": "+84-90-1000001"}},
					{Ref: "@s2", Data: map[string]any{"name": "Trần Thị Bình", "email": "binh@student.example.com", "phone": "+84-90-1000002"}},
					{Ref: "@s3", Data: map[string]any{"name": "Lê Văn Cường", "email": "cuong@student.example.com", "phone": "+84-90-1000003"}},
					{Ref: "@s4", Data: map[string]any{"name": "Phạm Thị Dung", "email": "dung@student.example.com", "phone": "+84-90-1000004"}},
				},
			},
			{
				Entity: "lms_enrollments",
				Records: []PackRecord{
					// @co_go: 3 enrolments — grades 8.5 + 7.5 + 9.2 = 25.2 → avg 8.4 (the test's key assertion).
					{Data: map[string]any{"course": "@co_go", "student": "@s1", "grade": float64(8.5), "tuition_due": "2026-08-01", "status": "Đã đóng"}},
					{Data: map[string]any{"course": "@co_go", "student": "@s2", "grade": float64(7.5), "tuition_due": "2026-08-05", "status": "Chờ đóng"}},
					{Data: map[string]any{"course": "@co_go", "student": "@s3", "grade": float64(9.2), "tuition_due": "2026-08-10", "status": "Đã đóng"}},
					{Data: map[string]any{"course": "@co_react", "student": "@s1", "grade": float64(6.0), "tuition_due": "2026-08-15", "status": "Chờ đóng"}},
					{Data: map[string]any{"course": "@co_react", "student": "@s4", "grade": float64(8.0), "tuition_due": "2026-07-01", "status": "Quá hạn"}},
					{Data: map[string]any{"course": "@co_ai", "student": "@s2", "grade": float64(9.0), "tuition_due": "2026-08-20", "status": "Chờ đóng"}},
				},
			},
		},

		// ------------------------------------------------------------------ layouts
		Layouts: []PackLayout{
			{EntityName: "lms_courses", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "title"},
					map[string]any{"field": "code"},
					map[string]any{"field": "tuition"},
					map[string]any{"field": "start_date"},
					map[string]any{"field": "status"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "lms_students", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "name"},
					map[string]any{"field": "email"},
					map[string]any{"field": "phone"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "lms_enrollments", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "course"},
					map[string]any{"field": "student"},
					map[string]any{"field": "grade"},
					map[string]any{"field": "status"},
					map[string]any{"field": "tuition_due"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "lms_courses", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin khóa học",
						"columns": 2,
						"fields":  []any{"title", "code", "tuition", "start_date", "status"},
					},
					// Rollups are opt-in (?rollup=enrollment_count,avg_grade on the record read) — the
					// deferred fields exist by Pass 6 (added in Pass 2b), so the layout validates cleanly.
					map[string]any{
						"title":   "Điểm danh",
						"columns": 2,
						"fields":  []any{"enrollment_count", "avg_grade"},
					},
				},
			}},
			{EntityName: "lms_students", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin học viên",
						"columns": 2,
						"fields":  []any{"name", "email", "phone"},
					},
				},
			}},
			{EntityName: "lms_enrollments", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin ghi danh",
						"columns": 2,
						"fields":  []any{"course", "student", "grade", "status", "tuition_due", "assigned_advisor"},
					},
				},
			}},
		},

		// ------------------------------------------------------------------ dashboards
		// "Tổng quan Đào tạo": a KPI + two grouped bars. w2/w3 GROUP BY status on lms_enrollments /
		// lms_courses — both indexed generated columns (B1), so the aggregate engine groups without a
		// full JSON_EXTRACT scan.
		Dashboards: []PackDashboard{
			{
				Name:        "Tổng quan Đào tạo",
				Description: "Tổng số ghi danh và phân bố theo trạng thái ghi danh / khóa học.",
				Widgets: []PackWidget{
					{ID: "w1", Title: "Tổng ghi danh", Type: "kpi", Entity: "lms_enrollments", Metric: "count"},
					{ID: "w2", Title: "Ghi danh theo trạng thái", Type: "bar", Entity: "lms_enrollments", Metric: "count", GroupBy: "status"},
					{ID: "w3", Title: "Khóa học theo trạng thái", Type: "bar", Entity: "lms_courses", Metric: "count", GroupBy: "status"},
				},
			},
		},

		// ------------------------------------------------------------------ teams (PACK-TEAMS)
		// The advisor team the routing rule below round-robins over. Install seeds it (idempotent by
		// name) AFTER roles and BEFORE workflows, adds the install user as its first active member, and
		// maps its NAME → the minted id so "Phân bổ tư vấn viên" resolves.
		Teams: []PackTeam{
			{Name: "Đội Tư vấn"},
		},

		// ------------------------------------------------------------------ work queues (PACK-QUEUES)
		// Tuition chasing: enrollments whose fee deadline has passed (lt + "today" = before today)
		// and are still awaiting payment, oldest deadline first.
		WorkQueues: []PackWorkQueue{
			{
				Name: "hoc_phi_qua_han", Label: "Học phí quá hạn", EntityName: "lms_enrollments",
				RoleName: "lms_teacher",
				Filters: []PackQueueFilter{
					{Field: "tuition_due", Operator: "lt", Value: "today"},
					{Field: "status", Operator: "eq", Value: "Chờ đóng"},
				},
				SortField: "tuition_due", SortOrder: "asc",
			},
		},

		// ------------------------------------------------------------------ workflows (PACK-WORKFLOWS)
		// Three automation rules (TIMER-1 reminder + ASSIGN-1 routing + TUITION-OVERDUE escalation).
		// Install creates them LAST (after samples) so they do NOT fire on the seed inserts. All go
		// through the SAME config-time validation as POST /admin/workflows (buildWorkflowRule + WF-01
		// field-target guard).
		Workflows: []PackWorkflow{
			{
				// TIMER-1 date_offset: fire 3 days BEFORE an enrolment's tuition_due (tuition reminder).
				// tuition_due is a `date` field — buildTimerConfig accepts date OR datetime as a timer anchor.
				Name:       "Nhắc đóng học phí",
				EntityName: "lms_enrollments",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "tuition_due",
						Offset:    PackWorkflowTimerOffset{Amount: 3, Unit: "days"},
						Direction: "before",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Sắp tới hạn đóng học phí", Message: "Ghi danh của bạn sắp tới hạn đóng học phí."},
				},
				IsActive: &active,
			},
			{
				// ASSIGN-1 routing: round-robin every NEW enrolment over "Đội Tư vấn" into assigned_advisor.
				// `team` carries the team NAME here — install resolves it to the seeded team's id.
				Name:       "Phân bổ tư vấn viên",
				EntityName: "lms_enrollments",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "assign", Strategy: "round_robin", Team: "Đội Tư vấn", Field: "assigned_advisor"},
				},
				IsActive: &active,
			},
			{
				// TUITION-OVERDUE (v1.1.0) date_offset: fire 1 day AFTER an enrolment's tuition_due and, IF
				// it is STILL unpaid, mark it overdue. Direction "after" is the same TIMER-1 engine as "Nhắc
				// đóng học phí" above, just the forward direction. The condition GATES on status with a
				// single `eq` rule (status "Chờ đóng") — status is a 3-value picklist (Chờ đóng / Đã đóng /
				// Quá hạn), so one eq on the still-pending value is the whole membership gate (default match
				// "and" over one rule). The gate is re-evaluated at FIRE time on the CURRENT record (the
				// workflow_timer handler), so an enrolment PAID (status → "Đã đóng") before tuition_due+1day
				// fails the gate and is NOT marked overdue — only a still-pending past-due enrolment
				// escalates; an already-"Quá hạn" one is not re-escalated either. Actions: flip status →
				// "Quá hạn" (field_update; status is a client-writable picklist so WF-01 permits it) and
				// notify the owner.
				Name:       "Đánh dấu quá hạn học phí",
				EntityName: "lms_enrollments",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Rules: []PackWorkflowRule{
						{Field: "status", Op: "eq", Value: "Chờ đóng"},
					},
					Timer: &PackWorkflowTimer{
						DateField: "tuition_due",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "days"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "status", Value: "Quá hạn"},
					{Type: "in_app_notification", UserID: "$owner", Title: "Học phí quá hạn", Message: "Học phí quá hạn — ghi danh đã chuyển trạng thái Quá hạn."},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ reports (PACK-REPORTS-WORKLIST)
		// One tabular work-list report: the overdue-tuition collections list — every enrolment whose
		// status is "Quá hạn" (the same state the "Đánh dấu quá hạn học phí" workflow flips a past-due
		// unpaid enrolment into). Columns project the two FK relations `student`/`course` (STORED
		// generated columns off data.$.student / data.$.course → target UUIDs) plus the tuition_due
		// date and status. The `status eq "Quá hạn"` filter reuses the SAME list filter engine, so the
		// report narrows to the overdue enrolments instead of listing all of them. Installed through the
		// SAME validated CreateReport path as POST /admin/reports; idempotent by name; NOT exported.
		Reports: []PackReport{
			{
				Name:       "Danh sách ghi danh quá hạn",
				EntityName: "lms_enrollments",
				Type:       "tabular",
				Config: PackReportConfig{
					Columns: []string{"student", "course", "tuition_due", "status"},
					Filters: []PackReportFilter{{Field: "status", Op: "eq", Values: []string{"Quá hạn"}}},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ roles
		Roles: []PackRole{
			{
				Name:  "lms_teacher",
				Label: "Nhân viên đào tạo",
				Actions: []PackRoleAction{
					{EntityName: "lms_courses", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_courses", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_courses", Action: "edit", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "edit", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "edit", AccessLevel: "all"},
				},
			},
			{
				Name:  "lms_manager",
				Label: "Quản lý đào tạo",
				Actions: []PackRoleAction{
					{EntityName: "lms_courses", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_courses", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_courses", Action: "edit", AccessLevel: "all"},
					{EntityName: "lms_courses", Action: "delete", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "edit", AccessLevel: "all"},
					{EntityName: "lms_students", Action: "delete", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "view", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "create", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "edit", AccessLevel: "all"},
					{EntityName: "lms_enrollments", Action: "delete", AccessLevel: "all"},
				},
			},
		},
	}
}

// pmPack is the built-in Project-Management vertical — a reference vertical that composes the FULL
// shipped primitive set into one pack (declarative literal only, NO engine code). It mirrors the
// ticketingPack + lmsPack templates verbatim: a parent→child 1-N model, parent-side DEFERRED
// rollups, before/after date_offset timers, round-robin assignment, an escalation gate, a
// dashboard, canned reports, a team, layouts, roles and samples.
//
// The model is a classic parent→child: pm_projects is the parent, pm_tasks is the child of ONE
// project (a 1-N FK-on-child relation), directly paralleling lms_courses → lms_enrollments (minus
// the second parent) and tk_customers → tk_tickets. Two parent-side rollups on pm_projects
// (deferred to Pass 2b, exactly like lms_courses' gradebook rollups — the parent is created before
// the child, so an inline rollup over pm_tasks would fail config-time with
// FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND):
//   - task_count (count over the `project` 1-N) — how many tasks a project has.
//   - total_estimate (sum over the child `estimate_hours` number field) — the project's total
//     estimated effort. Rollups resolve ONLY with an explicit ?rollup= opt-in and are NEVER seeded
//     (readonly → 422 aborts install).
//
// Automation (PACK-TEAMS + PACK-WORKFLOWS, same validated admin path as POST /admin/workflows):
//   - ASSIGN-1: "Phân bổ công việc" round-robins every NEW task over "Đội dự án" into the
//     `assignee` field. `team` carries the NAME (a pack cannot know the seeded id); install
//     resolves it to the team's real id. assignee is a `user` field (assign accepts `user` or
//     legacy `text` — assignTargetTypeOK, else ASSIGN_FIELD_TYPE_INVALID).
//   - TIMER-1 date_offset (reminder): "Nhắc hạn công việc" fires 1 day BEFORE a task's due_date (a
//     `date` field — buildTimerConfig accepts date OR datetime as a timer anchor).
//   - TIMER-1 date_offset (escalation): "Leo thang công việc trễ" fires 1 day AFTER a task's
//     due_date (Direction "after" — the same timer engine as the reminder, just the forward
//     direction), and IF the task is STILL not done, escalates it. status is a 4-value picklist (To
//     Do / Đang làm / Đã xong / Bị chặn); the condition evaluator has no `neq`-list / `in` operator,
//     so the "not done" membership gate is match "or" over three `eq` rules (To Do OR Đang làm OR Bị
//     chặn) — mirroring the Ticketing SLA-ESCALATION or-of-eq gate, just three states not two. The
//     gate is re-evaluated at FIRE time on the CURRENT record (the workflow_timer handler), so a
//     task completed (status → "Đã xong") before due_date+1day fails the gate and is NOT escalated.
//     When it does fire it runs two actions — field_update priority → "Khẩn" (a valid pm_tasks
//     priority value; a picklist is client-writable so WF-01 permits the overwrite) and an $owner
//     in_app_notification. The two due_date timers ("before" reminder + "after" escalation) park
//     independently (the wake-up payload keys on the workflow id), so both coexist on the anchor.
//
// ENTITY ORDER (load-bearing): pm_projects MUST precede pm_tasks so the `project` relation target
// exists when pm_tasks' FK column is materialised (mirrors lms_courses before lms_enrollments /
// tk_customers before tk_tickets). orderEntities also topo-sorts by the relation, but the slice
// order keeps the dependency obvious on the page.
//
// B1 note: status + priority on pm_tasks and status on pm_projects carry Options["indexed"]=true so
// the dashboard/report GROUP BY queries hit an indexed generated column instead of a full
// JSON_EXTRACT scan.
func pmPack() *Pack {
	active := true
	return &Pack{
		Manifest: &Manifest{
			Name:        "Project Management",
			Slug:        "pm",
			Version:     "1.1.0", // 1.1.0: assignee là kiểu `user` (hiện TÊN người, không phải UUID); 1.0.1: valueLabel "To Do" → "Cần làm"
			Description: "Dự án và công việc với ước lượng, phân công, nhắc hạn và leo thang.",
			Author:      "Nexus",
		},
		Name:        "Project Management",
		Description: "Dự án và công việc với ước lượng, phân công, nhắc hạn và leo thang.",

		// ------------------------------------------------------------------ entities
		Entities: []PackEntity{
			// pm_projects — a project. Parent of pm_tasks (1-N). MUST precede pm_tasks.
			{
				Name:  "pm_projects",
				Label: "Dự án",
				Fields: []PackField{
					{Name: "name", Label: "Tên dự án", FieldType: "text", IsRequired: true},
					{Name: "code", Label: "Mã dự án", FieldType: "text", IsUnique: true},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":  []any{"Nháp", "Đang chạy", "Tạm dừng", "Hoàn thành"},
						"indexed": true, // B1: GROUP BY status on the dashboard
					}},
					{Name: "start_date", Label: "Ngày bắt đầu", FieldType: "date"},
					{Name: "end_date", Label: "Ngày kết thúc", FieldType: "date"},
					{Name: "owner", Label: "Người phụ trách", FieldType: "text"},
				},
			},

			// pm_tasks — the task. belongs-to one project (1-N FK-on-child).
			{
				Name:  "pm_tasks",
				Label: "Công việc",
				Fields: []PackField{
					// 1-N: task belongs to one project. Explicit inverseField: the parent side on
					// pm_projects is discoverable as "tasks".
					{Name: "project", Label: "Dự án", FieldType: "relation", Options: map[string]any{
						"relatedEntity": "pm_projects",
						"inverseField":  "tasks",
					}},
					{Name: "title", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
					{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{
						"values":      []any{"To Do", "Đang làm", "Đã xong", "Bị chặn"},
						"valueLabels": map[string]any{"To Do": "Cần làm"},
						"indexed":     true, // B1: GROUP BY status on the dashboard / report
					}},
					{Name: "priority", Label: "Ưu tiên", FieldType: "picklist", Options: map[string]any{
						"values":  []any{"Thấp", "Trung bình", "Cao", "Khẩn"},
						"indexed": true, // B1: GROUP BY priority on the dashboard
					}},
					// ASSIGN-1 routing target: the "Phân bổ công việc" workflow writes the picked
					// member's userId here. `user`, NOT `text` — see the identical field on crm_leads: the
					// "users is not a sys_entity" note predates the `user` field type; assignTargetTypeOK
					// accepts both, so tenants installed before this keep their text column (install is
					// idempotent — FIELD_ALREADY_EXISTS is a skip) and keep routing. Left unset on samples
					// (routing fires only on live after_create; the workflows pass installs AFTER the seed
					// rows, so seeds are never routed).
					{Name: "assignee", Label: "Người thực hiện", FieldType: "user"},
					{Name: "due_date", Label: "Hạn hoàn thành", FieldType: "date"},
					{Name: "estimate_hours", Label: "Giờ ước lượng", FieldType: "number", Options: map[string]any{
						"min": float64(0),
					}},
				},
			},
		},

		// ------------------------------------------------------------------ deferred fields
		// Parent-side rollups on pm_projects, added in Pass 2b AFTER pm_tasks exists. They CANNOT go
		// inline: pm_projects is created FIRST by dependency order, so a rollup over pm_tasks would
		// fail config-time (FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND). count carries NO aggField; sum
		// REQUIRES one (the child `estimate_hours` number field). Re-install is idempotent:
		// FIELD_ALREADY_EXISTS is a skip.
		DeferredFields: []PackDeferredField{
			{EntityName: "pm_projects", Field: PackField{
				Name: "task_count", Label: "Số công việc", FieldType: "rollup",
				Options: map[string]any{"childEntity": "pm_tasks", "viaRelation": "project", "metric": "count"},
			}},
			{EntityName: "pm_projects", Field: PackField{
				Name: "total_estimate", Label: "Tổng giờ ước lượng", FieldType: "rollup",
				Options: map[string]any{"childEntity": "pm_tasks", "viaRelation": "project", "metric": "sum", "aggField": "estimate_hours"},
			}},
		},

		// ------------------------------------------------------------------ sample data
		// 3 projects (varied status) → 5 tasks. "@ref" values in Data are resolved to real UUIDs at
		// install (Pass 3). @proj_web carries 3 tasks (estimate_hours 8/5/3 → total_estimate 16) and
		// @proj_mobile carries 2 (4/6) so the rollup is a DISCRIMINATING per-project assertion: a
		// rollup that ignored viaRelation grouping would return count 5 / total 26 for @proj_web
		// instead of 3 / 16. @proj_infra has ZERO tasks (rollup empty-state). Rollup fields
		// (task_count/total_estimate) and assignee are NEVER seeded (readonly rollup → 422 abort;
		// assignee is routing-filled).
		Samples: []PackSample{
			{
				Entity: "pm_projects",
				Records: []PackRecord{
					{Ref: "@proj_web", Data: map[string]any{"name": "Website bán hàng", "code": "PRJ-001", "status": "Đang chạy", "start_date": "2026-07-01", "end_date": "2026-10-01", "owner": "Nguyễn Văn A"}},
					{Ref: "@proj_mobile", Data: map[string]any{"name": "Ứng dụng di động", "code": "PRJ-002", "status": "Nháp", "start_date": "2026-08-01", "end_date": "2026-12-01", "owner": "Trần Thị B"}},
					// Zero tasks — demos the rollup empty-state: task_count = 0, total_estimate = null.
					{Ref: "@proj_infra", Data: map[string]any{"name": "Nâng cấp hạ tầng", "code": "PRJ-003", "status": "Hoàn thành", "start_date": "2026-05-01", "end_date": "2026-06-30", "owner": "Lê Văn C"}},
				},
			},
			{
				Entity: "pm_tasks",
				Records: []PackRecord{
					// @proj_web: 3 tasks — estimate_hours 8 + 5 + 3 = 16 (total_estimate), count 3 (the
					// test's key assertion). Status spans Đã xong / Đang làm / To Do.
					{Data: map[string]any{"project": "@proj_web", "title": "Thiết kế trang chủ", "status": "Đã xong", "priority": "Cao", "due_date": "2026-08-01", "estimate_hours": float64(8)}},
					{Data: map[string]any{"project": "@proj_web", "title": "API đăng nhập", "status": "Đang làm", "priority": "Khẩn", "due_date": "2026-08-10", "estimate_hours": float64(5)}},
					{Data: map[string]any{"project": "@proj_web", "title": "Kiểm thử E2E", "status": "To Do", "priority": "Trung bình", "due_date": "2026-08-15", "estimate_hours": float64(3)}},
					// @proj_mobile: 2 tasks — estimate_hours 4 + 6 = 10. Status To Do / Bị chặn.
					{Data: map[string]any{"project": "@proj_mobile", "title": "Dựng khung app", "status": "To Do", "priority": "Cao", "due_date": "2026-09-01", "estimate_hours": float64(4)}},
					{Data: map[string]any{"project": "@proj_mobile", "title": "Tích hợp push", "status": "Bị chặn", "priority": "Thấp", "due_date": "2026-09-10", "estimate_hours": float64(6)}},
				},
			},
		},

		// ------------------------------------------------------------------ layouts
		Layouts: []PackLayout{
			{EntityName: "pm_projects", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "name"},
					map[string]any{"field": "code"},
					map[string]any{"field": "status"},
					map[string]any{"field": "start_date"},
					map[string]any{"field": "end_date"},
					map[string]any{"field": "owner"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "pm_tasks", ViewType: "list", LayoutJSON: map[string]any{
				"version": 1,
				"columns": []any{
					map[string]any{"field": "title"},
					map[string]any{"field": "project"},
					map[string]any{"field": "status"},
					map[string]any{"field": "priority"},
					map[string]any{"field": "assignee"},
					map[string]any{"field": "due_date"},
				},
				"defaultSort": map[string]any{"field": "created_at", "order": "desc"},
			}},
			{EntityName: "pm_projects", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin dự án",
						"columns": 2,
						"fields":  []any{"name", "code", "status", "owner", "start_date", "end_date"},
					},
					// Rollups are opt-in (?rollup=task_count,total_estimate on the record read) — the
					// deferred fields exist by the layout pass (added in Pass 2b), so this validates.
					map[string]any{
						"title":   "Tiến độ",
						"columns": 2,
						"fields":  []any{"task_count", "total_estimate"},
					},
				},
			}},
			{EntityName: "pm_tasks", ViewType: "detail", LayoutJSON: map[string]any{
				"version": 1,
				"sections": []any{
					map[string]any{
						"title":   "Thông tin công việc",
						"columns": 2,
						"fields":  []any{"title", "project", "status", "priority", "assignee", "due_date", "estimate_hours"},
					},
				},
			}},
		},

		// ------------------------------------------------------------------ dashboards
		// "Tổng quan Dự án": a KPI + two grouped bars. w2/w3 GROUP BY status/priority on pm_tasks —
		// both indexed generated columns (B1), so the aggregate engine groups without a full
		// JSON_EXTRACT scan.
		Dashboards: []PackDashboard{
			{
				Name:        "Tổng quan Dự án",
				Description: "Tổng số công việc và phân bố theo trạng thái / ưu tiên.",
				Widgets: []PackWidget{
					{ID: "w1", Title: "Tổng công việc", Type: "kpi", Entity: "pm_tasks", Metric: "count"},
					{ID: "w2", Title: "Công việc theo trạng thái", Type: "bar", Entity: "pm_tasks", Metric: "count", GroupBy: "status"},
					{ID: "w3", Title: "Công việc theo ưu tiên", Type: "bar", Entity: "pm_tasks", Metric: "count", GroupBy: "priority"},
				},
			},
		},

		// ------------------------------------------------------------------ teams (PACK-TEAMS)
		// The project team the routing rule below round-robins over. Install seeds it (idempotent by
		// name) AFTER roles and BEFORE workflows, adds the install user as its first active member,
		// and maps its NAME → the minted id so "Phân bổ công việc" resolves.
		Teams: []PackTeam{
			{Name: "Đội dự án"},
		},

		// ------------------------------------------------------------------ workflows (PACK-WORKFLOWS)
		// Three automation rules (ASSIGN-1 routing + TIMER-1 reminder + escalation). Install creates
		// them LAST (after samples) so they do NOT fire on the seed inserts. All go through the SAME
		// config-time validation as POST /admin/workflows (buildWorkflowRule + WF-01 field-target guard).
		Workflows: []PackWorkflow{
			{
				// ASSIGN-1 routing: round-robin every NEW task over "Đội dự án" into assignee.
				// `team` carries the team NAME here — install resolves it to the seeded team's id.
				Name:       "Phân bổ công việc",
				EntityName: "pm_tasks",
				Trigger:    "after_create",
				Actions: []PackWorkflowAction{
					{Type: "assign", Strategy: "round_robin", Team: "Đội dự án", Field: "assignee"},
				},
				IsActive: &active,
			},
			{
				// TIMER-1 date_offset: fire 1 day BEFORE a task's due_date (deadline reminder).
				// due_date is a `date` field — buildTimerConfig accepts date OR datetime as a timer anchor.
				Name:       "Nhắc hạn công việc",
				EntityName: "pm_tasks",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Timer: &PackWorkflowTimer{
						DateField: "due_date",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "days"},
						Direction: "before",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "in_app_notification", UserID: "$owner", Title: "Công việc sắp tới hạn", Message: "Một công việc của bạn sắp tới hạn hoàn thành."},
				},
				IsActive: &active,
			},
			{
				// Escalation date_offset: fire 1 day AFTER a task's due_date and, IF the task is STILL
				// not done, escalate it. Direction "after" is the same TIMER-1 engine as "Nhắc hạn công
				// việc" above, just the forward direction. status is a 4-value picklist (To Do / Đang
				// làm / Đã xong / Bị chặn); the evaluator has no `neq`-list / `in` operator, so the
				// "not done" gate is match "or" over three `eq` rules (To Do OR Đang làm OR Bị chặn) —
				// mirroring the Ticketing SLA-ESCALATION or-of-eq gate, just three states not two. The
				// gate is re-evaluated at FIRE time on the CURRENT record (the workflow_timer handler),
				// so a task completed (status → "Đã xong") before due_date+1day fails the gate and is
				// NOT escalated. Actions: bump priority → "Khẩn" (field_update; priority is a
				// client-writable picklist so WF-01 permits it) and notify the owner.
				Name:       "Leo thang công việc trễ",
				EntityName: "pm_tasks",
				Trigger:    "date_offset",
				Condition: &PackWorkflowCondition{
					Match: "or",
					Rules: []PackWorkflowRule{
						{Field: "status", Op: "eq", Value: "To Do"},
						{Field: "status", Op: "eq", Value: "Đang làm"},
						{Field: "status", Op: "eq", Value: "Bị chặn"},
					},
					Timer: &PackWorkflowTimer{
						DateField: "due_date",
						Offset:    PackWorkflowTimerOffset{Amount: 1, Unit: "days"},
						Direction: "after",
					},
				},
				Actions: []PackWorkflowAction{
					{Type: "field_update", Field: "priority", Value: "Khẩn"},
					{Type: "in_app_notification", UserID: "$owner", Title: "Công việc quá hạn", Message: "Công việc của bạn đã quá hạn và được nâng mức ưu tiên lên Khẩn."},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ reports (PACK-REPORTS)
		// Two canned saved reports so a freshly-installed PM tenant gets working saved reports: a
		// summary status breakdown + a tabular open-work list. "Công việc theo trạng thái": COUNT of
		// pm_tasks GROUP BY status — status is an indexed picklist (B1), so the aggregate engine
		// groups without a full-scan at run time. "Danh sách việc đang mở" (tabular): the open-work
		// queue — every task still needing attention (status in {To Do, Đang làm, Bị chặn}). The `in`
		// operator is valid on a picklist, so ONE filter with three values captures all open states;
		// values are byte-identical to the status picklist options. Both installed through the SAME
		// validated CreateReport path as POST /admin/reports; idempotent by name; NOT exported.
		Reports: []PackReport{
			{
				Name:       "Công việc theo trạng thái",
				EntityName: "pm_tasks",
				Type:       "summary",
				Config: PackReportConfig{
					Metric:  "count",
					GroupBy: "status",
				},
				IsActive: &active,
			},
			{
				Name:       "Danh sách việc đang mở",
				EntityName: "pm_tasks",
				Type:       "tabular",
				Config: PackReportConfig{
					Columns: []string{"title", "project", "priority", "status", "due_date", "assignee"},
					Filters: []PackReportFilter{{Field: "status", Op: "in", Values: []string{"To Do", "Đang làm", "Bị chặn"}}},
				},
				IsActive: &active,
			},
		},

		// ------------------------------------------------------------------ roles
		Roles: []PackRole{
			{
				Name:  "pm_member",
				Label: "Thành viên dự án",
				Actions: []PackRoleAction{
					{EntityName: "pm_projects", Action: "view", AccessLevel: "all"},
					{EntityName: "pm_projects", Action: "create", AccessLevel: "all"},
					{EntityName: "pm_projects", Action: "edit", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "view", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "create", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "edit", AccessLevel: "all"},
				},
			},
			{
				Name:  "pm_manager",
				Label: "Quản lý dự án",
				Actions: []PackRoleAction{
					{EntityName: "pm_projects", Action: "view", AccessLevel: "all"},
					{EntityName: "pm_projects", Action: "create", AccessLevel: "all"},
					{EntityName: "pm_projects", Action: "edit", AccessLevel: "all"},
					{EntityName: "pm_projects", Action: "delete", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "view", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "create", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "edit", AccessLevel: "all"},
					{EntityName: "pm_tasks", Action: "delete", AccessLevel: "all"},
				},
			},
		},
	}
}
