package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

const (
	defaultImportMaxBytes = 5 * 1024 * 1024 // 5 MiB
	defaultImportMaxRows  = 5000
)

// ImportMaxBytes returns the byte cap for import file upload (env IMPORT_MAX_BYTES).
func ImportMaxBytes() int64 {
	if v := os.Getenv("IMPORT_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return defaultImportMaxBytes
}

// ImportMaxRows returns the row cap for import (env IMPORT_MAX_ROWS).
func ImportMaxRows() int {
	if v := os.Getenv("IMPORT_MAX_ROWS"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil && n > 0 {
			return n
		}
	}
	return defaultImportMaxRows
}

// ImportStore is the minimal repository interface the ImportService needs.
// It is intentionally separate from RecordStore to keep the import surface clean.
type ImportStore interface {
	// InsertRecords batch-inserts pre-validated rows into the entity's obj_* table in ONE
	// transaction. It resolves team_set_id once for the creator and applies it to all rows.
	// Returns the ids of the rows inserted, in input order. uniqueFields (RM-28) is the entity's active
	// unique fields so the repo dual-writes the pivot (set-based over the batch) in the same tx.
	InsertRecords(ctx context.Context, entity, tenantID, userID string, rows []map[string]any, uniqueFields []dynrepo.UniqueFieldSpec) ([]string, error)
	// BatchExistingValuesForField returns the subset of values that already exist in the
	// entity's obj_* table for the given tenant. It issues at most one SELECT per chunk of
	// dynrepo.DedupChunkSize values (O(⌈N/chunk⌉) queries) instead of one query per value
	// (O(N)). When the field has a generated column (spec.IsUnique or spec.IsIndexed) the
	// physical column is queried so the B-tree index is used; otherwise the JSON expression
	// is used (same semantics as before, but batched). Used for dedup.
	BatchExistingValuesForField(ctx context.Context, entity, tenantID, fieldName string, spec dynrepo.DedupFieldSpec, values []string) (map[string]struct{}, error)
}

// RelationBatchChecker is the minimal interface ImportService needs to validate that
// relation-field FK values in CSV rows point to live same-tenant records (B3 fix).
// It is a subset of RecordStore (same signature as BatchRecordExists) declared here
// consumer-side so ImportService does not need to import the full RecordStore surface.
// Production wires *dynrepo.Repository; nil disables FK validation (backward-compat).
type RelationBatchChecker interface {
	BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error)
}

// ImportRowError captures a per-row import failure (validation or dedup).
type ImportRowError struct {
	Row     int    `json:"row"`     // 1-based data row number (not counting header)
	Field   string `json:"field"`   // field name that caused the error (empty if row-level)
	Code    string `json:"code"`    // machine-readable error code
	Message string `json:"message"` // human-readable message
}

// ImportResult is the summary returned by a successful import call.
type ImportResult struct {
	Total    int `json:"total"`
	Inserted int `json:"inserted"`
	Failed   int `json:"failed"`
	// Valid is how many rows passed every check — the rows a real run writes (specs/061 FR-011).
	Valid int `json:"valid"`
	// DryRun echoes ImportInput.DryRun: nothing was written (the "Xem trước" step).
	DryRun bool             `json:"dryRun"`
	Errors []ImportRowError `json:"errors"`
	// AuditJobsFailed counts the history-writing jobs that could not be queued after the rows were
	// committed (specs/048 S7a): those rows exist but have no "Tạo" history line. 0 = all queued.
	AuditJobsFailed int `json:"auditJobsFailed,omitempty"`
}

// columnMapping maps CSV header name → FieldSpec of the target field.
type columnMapping map[string]FieldSpec

// validRow bundles a clean payload with its original 1-based CSV row number so that
// applyDedup can report the correct line when marking a duplicate — even when earlier
// rows were rejected by validation and the valid-row index no longer matches the CSV
// line number.
type validRow struct {
	rowNum  int            // 1-based original CSV data-row number (after header)
	payload map[string]any // clean, coerced, validated payload ready for insert
	// hookOuts is what the value-changing before_save rules did to this row (specs/048 S5) — counted
	// in the rules' summary runs only if the row is stored.
	hookOuts []workflow.MutationOutcome
}

// ImportService handles CSV import: parse → map → coerce → validate → batch insert.
type ImportService struct {
	catalog EntityCatalog
	store   ImportStore
	logger  *zap.Logger
	// afterInsert runs once per committed batch with the inserted rows' payloads — the seam that
	// refreshes stored roll-up totals over the parents the batch touched (specs/083). nil = off.
	afterInsert func(ctx context.Context, tenantID, entity string, images ...map[string]any)
	// fieldACL enforces field-level write security (A6) on the import path, mirroring the
	// same guard on Create/Update. Optional: nil skips the check (unit-test default).
	// Production wires *FieldACLService via WithFieldACL. Admin (IsAdmin=true) bypasses.
	fieldACL fieldACLEnforcer
	// relChecker validates that relation-field FK values in CSV rows point to live
	// same-tenant records (B3 fix). Optional: nil skips FK validation (backward-compat).
	// Production wires *dynrepo.Repository via WithRelationChecker.
	relChecker RelationBatchChecker
	// sequences backs the IMPORT-AUTONUM auto_number seam: it claims + assigns a per-tenant/
	// entity/field sequence value for every auto_number field of each surviving row, through the
	// SAME atomic primitive (assignSequenceValues → dynrepo.ClaimSequence) the Create write-path
	// uses (ENG #6), so imported records get their auto_number instead of a permanently-blank one.
	// Optional: nil = feature DISARMED (auto_number simply not assigned — preserves the prior
	// import behaviour for any caller that does not wire it, and never fails the batch). Production
	// wires *dynrepo.Repository via WithSequenceClaimer (same concrete type as the store).
	sequences SequenceClaimer
	// beforeRules judges every row against the entity's before_save rules (specs/048 S2) — the
	// SAME CachedLoader RecordService reads. nil (the default) imports every row unjudged.
	beforeRules workflow.WorkflowLoader
	// beforeRuns receives one summary run per rule that refused rows of a file (specs/048 S3).
	beforeRuns *workflow.RunSink
	// authz gates relation lookup BY VALUE on the caller's VIEW grant on the target entity
	// (specs/061 FR-015). nil = ungated (unit tests); production wires *ACLService.
	authz entityAuthorizer
	// idem backs ImportIdempotent (Idempotency-Key, specs/047 FR-12). nil ⇒ a keyed import fails
	// closed. Production wires *idempotency.SQLRepository via WithIdempotencyStore.
	idem IdempotencyStore
	// eventJobs parks the post-commit history job (specs/048 S7a). nil = imported rows get no
	// "Tạo" history line. Production wires the jobs store via WithEventJobs.
	eventJobs JobEnqueuer
}

// WithBeforeHooks arms the before_save rules on the import path (specs/048 S2): each row is judged
// exactly like a single create, and a refused row is reported by its CSV line. runs (S3) is the
// SAME RunSink RecordService writes through; nil records no run.
func (s *ImportService) WithBeforeHooks(loader workflow.WorkflowLoader, runs *workflow.RunSink) *ImportService {
	s.beforeRules = loader
	s.beforeRuns = runs
	return s
}

// WithIdempotencyStore wires the Idempotency-Key claim store (production: *idempotency.SQLRepository).
func (s *ImportService) WithIdempotencyStore(st IdempotencyStore) *ImportService {
	s.idem = st
	return s
}

// ImportIdempotent is Import guarded by an Idempotency-Key (specs/047 FR-12, see runIdempotent):
// a resend of the same file + mapping + dedupField + matchBy replays the first import's summary
// instead of inserting the rows again. replay != nil ⇒ answer it verbatim. The file is read once
// into memory to fingerprint it — the handler has already capped its size at IMPORT_MAX_BYTES.
// A preview (specs/061 FR-010) writes nothing, so the key protects nothing: it runs as is, never
// claims the key and never replays the import the key names.
func (s *ImportService) ImportIdempotent(ctx context.Context, in ImportInput, key string) (result *ImportResult, replay json.RawMessage, err error) {
	if in.DryRun {
		result, err = s.Import(ctx, in)
		return result, nil, err
	}
	content, err := io.ReadAll(in.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("services.ImportService.ImportIdempotent: read file: %w", err)
	}
	fileSum := sha256.Sum256(content)
	hash := idempotencyRequestHash("import\x00"+in.EntityName, map[string]any{
		"mapping": in.Mapping, "dedupField": in.DedupField, "matchBy": in.MatchBy, "runAutomations": in.RunAutomations, "fileSha256": hex.EncodeToString(fileSum[:]),
	})
	in.Reader = bytes.NewReader(content)
	// The lease needs to probe a row of the batch; a store that cannot (the unit-test fakes) runs the
	// import unleased, as FR-12 did.
	var lease *batchLease
	if prober, ok := s.store.(interface {
		RecordExists(ctx context.Context, entity, tenantID, id string) (bool, error)
	}); ok {
		lease = &batchLease{committed: func(ctx context.Context, baseID string) (bool, error) {
			sanitized, serr := ddl.SanitizeIdentifier(in.EntityName)
			if serr != nil {
				return false, ErrEntityNotFound
			}
			e, _, rerr := (&RecordService{catalog: s.catalog}).resolve(ctx, in.TenantID, sanitized)
			if rerr != nil {
				return false, rerr
			}
			return prober.RecordExists(ctx, e.Name, in.TenantID, baseID)
		}} // no rebuild: the response is the plan the run saved before its transaction
	}
	replay, err = runIdempotent(ctx, s.idem, s.logger, lease, in.TenantID, in.UserID, in.EntityName, key, hash,
		func(ctx context.Context, b claimedBatch) (any, bool, error) {
			rin := in
			if b.baseID != "" {
				// One transaction in the request context: cancelled, it rolls back and the claim is
				// released. Bounded by idempotencyBatchTimeout — the bound the lease relies on.
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, idempotencyBatchTimeout)
				defer cancel()
				rin.leaseBase, rin.savePlan = b.baseID, b.plan
			}
			var rerr error
			result, rerr = s.Import(ctx, rin)
			return result, result != nil && result.Inserted > 0, rerr
		})
	return result, replay, err
}

// NewImportService wires the import service with its dependencies.
func NewImportService(catalog EntityCatalog, store ImportStore, logger *zap.Logger) *ImportService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ImportService{catalog: catalog, store: store, logger: logger}
}

// WithAfterInsert injects the post-batch hook that keeps stored roll-up totals in step (specs/083).
// Chainable; nil leaves it off.
func (s *ImportService) WithAfterInsert(f func(ctx context.Context, tenantID, entity string, images ...map[string]any)) *ImportService {
	s.afterInsert = f
	return s
}

// WithFieldACL injects the field-level-security enforcer (A6) so CSV import rejects a
// mapping that includes a field the caller's role may not write (FIELD_FORBIDDEN), mirroring
// the same guard on RecordService.Create. Chainable; a nil enforcer is ignored (keeps the
// default nil — skip) so a mis-wired caller never panics. Production wires *FieldACLService.
func (s *ImportService) WithFieldACL(f fieldACLEnforcer) *ImportService {
	if f != nil {
		s.fieldACL = f
	}
	return s
}

// WithRelationChecker injects the relation-FK validator (B3 fix) so CSV import verifies
// that every relation-field value in each row points to a live same-tenant record before
// inserting. Chainable; a nil checker is ignored (nil = skip FK validation, backward-compat).
// Production wires *dynrepo.Repository which already implements BatchRecordExists.
func (s *ImportService) WithRelationChecker(c RelationBatchChecker) *ImportService {
	if c != nil {
		s.relChecker = c
	}
	return s
}

// WithSequenceClaimer injects the IMPORT-AUTONUM auto_number sequence claimer so CSV import
// assigns every auto_number field's value through the SAME atomic per-tenant/entity/field claim
// (ENG #6, assignSequenceValues → dynrepo.ClaimSequence) the Create write-path uses, instead of
// leaving imported records with a permanently-blank auto_number. Chainable; a nil claimer is
// ignored (keeps the nil default = feature DISARMED: auto_number is simply not assigned, and the
// batch never fails on its account — mirrors WithRelationChecker's nil-disables convention).
// Production wires *dynrepo.Repository (the same concrete type already backing the store).
func (s *ImportService) WithSequenceClaimer(c SequenceClaimer) *ImportService {
	if c != nil {
		s.sequences = c
	}
	return s
}

// ImportInput is the parsed input from the multipart form.
type ImportInput struct {
	TenantID string
	UserID   string
	// RoleID is the caller's resolved role (from the auth scope). Used by the field-level
	// write guard (A6) to reject a mapping that touches a field the role may not edit.
	RoleID string
	// IsAdmin, when true, bypasses the field-level write guard entirely (mirrors RecordService).
	IsAdmin    bool
	EntityName string
	// Mapping maps CSV header name → entity field name.
	Mapping    map[string]string
	DedupField string
	Reader     io.Reader
	FileSize   int64 // actual bytes read for the size check
	// DryRun (specs/061 FR-010) runs every check of a real import and writes nothing: no insert,
	// no auto_number claimed, no before_save run recorded. It is the "Xem trước" step.
	DryRun bool
	// MatchBy (specs/061 FR-012) maps a relation field to the field of its target entity that the
	// column's cells name ("Khớp theo"). Absent = the relation's label field (FR-013).
	MatchBy map[string]string
	// RunAutomations (specs/048 S7b) makes the post-commit job fire each imported row's after_create
	// workflow rules and BPM approvals as a hand-made create does. false (default) = history only:
	// a big file must not send a mail/webhook per row by surprise.
	RunAutomations bool

	// leaseBase / savePlan are set by ImportIdempotent for a leased claim (specs/047 FR-13): the batch's
	// base id (its rows get dynrepo.BatchRowID(leaseBase, i) as ids) and the writer of the response the
	// import answers with if its transaction commits, called BEFORE that transaction.
	leaseBase string
	savePlan  func(ctx context.Context, response any) error
}

// Import runs the full CSV import pipeline. It enforces size/row caps, maps columns,
// validates each row via ValidateRecord (reuse), deduplicates, and batch-inserts valid rows.
// Partial success: invalid rows are captured in ImportResult.Errors; valid rows are inserted.
func (s *ImportService) Import(ctx context.Context, in ImportInput) (*ImportResult, error) {
	s.logger.Info("import.Start",
		zap.String("tenant_id", in.TenantID),
		zap.String("user_id", in.UserID),
		zap.String("entity_name", in.EntityName),
	)

	// --- File size cap (checked by handler, but validate here too for safety) ---
	maxBytes := ImportMaxBytes()
	if in.FileSize > maxBytes {
		return nil, invalid("REQUEST_TOO_LARGE",
			"file vượt kích thước tối đa %d bytes", maxBytes)
	}

	// runAutomations rides the post-commit job; with the queue off nothing would fire and the user
	// would never know — refuse up front (a preview writes nothing, so it is exempt).
	if in.RunAutomations && !in.DryRun && s.eventJobs == nil {
		return nil, invalid("IMPORT_AUTOMATIONS_UNAVAILABLE",
			"không thể chạy tự động hoá khi nhập: hàng đợi việc nền đang tắt")
	}

	// --- Load entity + field specs ---
	sanitized, err := ddl.SanitizeIdentifier(in.EntityName)
	if err != nil {
		return nil, ErrEntityNotFound
	}
	e, specs, err := (&RecordService{catalog: s.catalog}).resolve(ctx, in.TenantID, sanitized)
	if err != nil {
		return nil, err
	}

	// The caller's hidden fields (queue #317): a name in this set is refused everywhere below exactly
	// like a name the entity does not declare. nil for an admin / no enforcer. Fails closed.
	var hidden map[string]struct{}
	if s.fieldACL != nil {
		h, herr := s.fieldACL.HiddenFields(ctx, in.TenantID, in.RoleID, e.Name, in.IsAdmin)
		if herr != nil {
			return nil, fmt.Errorf("services.ImportService.Import: field-acl: %w", herr)
		}
		hidden = h
	}

	// --- Pre-validate dedupField (must be a declared field) ---
	if in.DedupField != "" {
		found := false
		for _, sp := range specs {
			if _, isHidden := hidden[sp.Name]; sp.Name == in.DedupField && !isHidden {
				found = true
				break
			}
		}
		if !found {
			return nil, invalid("RECORD_UNKNOWN_FIELD",
				"dedupField %q không tồn tại trong entity %q", in.DedupField, in.EntityName)
		}
	}

	// --- Parse CSV ---
	headers, rawRows, err := parseCSV(in.Reader)
	if err != nil {
		return nil, invalid("IMPORT_INVALID_CSV", "không thể đọc file CSV: %s", err.Error())
	}

	// --- Row cap ---
	maxRows := ImportMaxRows()
	if len(rawRows) > maxRows {
		return nil, invalid("IMPORT_TOO_LARGE",
			"file có %d dòng, vượt quá giới hạn %d", len(rawRows), maxRows)
	}

	// --- Build column mapping: csv header → FieldSpec (also validates required coverage) ---
	cm, err := buildMapping(headers, in.Mapping, specs, hidden)
	if err != nil {
		// queue #144: an unmapped required field the caller's role may not write is not a column
		// they forgot — say so, with the admin's way out, exactly as Create does.
		return nil, explainRequiredForCaller(ctx, s.fieldACL, s.logger, in.RoleID, in.IsAdmin, in.TenantID, e.Name, specs)(err)
	}

	// --- A6 field-level write guard (RM-42): reject the whole import if the mapping includes
	// a field the caller's role may not write. The column→field mapping is uniform across all
	// rows, so a forbidden mapped field forbids the entire import. We build a representative
	// payload keyed by the target field names (values are irrelevant — CheckWriteFields only
	// inspects the keys) and call the enforcer ONCE, up front, BEFORE any row processing or
	// insert. This mirrors the Create write guard order (CheckWriteFields before ValidateRecord
	// before insert). Admin (IsAdmin=true) bypasses inside the enforcer. A nil enforcer (tests)
	// is a no-op, preserving the existing nil-safe convention.
	if s.fieldACL != nil {
		repr := make(map[string]any, len(cm))
		for _, sp := range cm {
			repr[sp.Name] = nil
		}
		if aclErr := s.fieldACL.CheckWriteFields(ctx, in.TenantID, in.RoleID, e.Name, in.IsAdmin, repr); aclErr != nil {
			return nil, aclErr
		}
	}

	// --- specs/061 FR-012: how each mapped relation column names its target (whole-file checks) ---
	matchPlan, err := s.planRelationMatches(ctx, in, specs, cm)
	if err != nil {
		return nil, err
	}

	// --- Process rows: map + coerce + validate ---
	// validRows carries both the clean payload AND the original 1-based CSV row number so
	// that downstream dedup errors are attributed to the correct line even when validation
	// failures break the contiguity between CSV position and valid-set index.
	validRows, failedRows, rowErrors := processRows(headers, rawRows, cm, specs)
	// queue #321: a hidden child's stored default (or a hidden parent) must not be named in a row's
	// RECORD_DEPENDENT_PICKLIST_INVALID — the same scrub Create applies. hidden is empty for admins.
	for i := range rowErrors {
		if rowErrors[i].Code == codeRecordDependentInvalid && dependentPicklistNamesHidden(specs, hidden, rowErrors[i].Field) {
			rowErrors[i].Field = ""
			rowErrors[i].Message = hiddenDependentPicklistRefusal().Error()
		}
	}

	// --- Relation cells (B3 fix + specs/061 FR-014): every relation cell becomes a live
	// same-tenant record id — the cell may BE the id, or name the record by its match field.
	// Batched: one BatchRecordExists per target entity (RM-11) + one value lookup per relation
	// field. Rows with a cell that resolved to no record, or to several, leave validRows with one
	// error per bad cell. nil relChecker = no FK validation (backward-compat for unit tests).
	// queue #362: rows that already failed ride along so their relation cells are named in the same
	// pass (one batched lookup for the file); they are dropped again below, never written.
	if s.relChecker != nil {
		var relErrors []ImportRowError
		all := append(append(make([]validRow, 0, len(validRows)+len(failedRows)), validRows...), failedRows...)
		all, relErrors = s.resolveRelations(ctx, in.TenantID, specs, all, matchPlan)
		rowErrors = append(rowErrors, relErrors...)
		// queue #357: `user` cells (person in charge) name the user by id, e-mail or full name.
		var userErrors []ImportRowError
		all, userErrors = s.resolveUsers(ctx, in.TenantID, specs, all)
		rowErrors = append(rowErrors, userErrors...)
		validRows = onlyRowsOf(all, validRows)
	}

	// --- specs/048 S2/S5: before_save rules, per row ---
	// After every field check (mapping, type, required, relation) and BEFORE the dedup probe and the
	// auto_number claim: the value-changing rules rewrite the row first, so the duplicate checks
	// compare what will be stored; a refused row is reported by its CSV line, never probed as a
	// duplicate and claims no number. Unreadable rules refuse the whole import (503), nothing written.
	// The rules' summary runs are written when Import returns, counting only the rows stored.
	var stored [][]workflow.MutationOutcome
	if s.beforeRules != nil {
		var vetoErrors []ImportRowError
		var flush func([][]workflow.MutationOutcome)
		validRows, vetoErrors, flush, err = s.judgeRows(ctx, in, e.Name, specs, validRows)
		if err != nil {
			return nil, err
		}
		if flush != nil {
			defer func() { flush(stored) }()
		}
		rowErrors = append(rowErrors, vetoErrors...)
	}

	// --- Dedup: check DB for existing values AND in-file duplicates ---
	if in.DedupField != "" {
		// Collect all values for dedupField from valid rows to batch-check existence.
		existing, dbErr := s.loadExistingValues(ctx, e.Name, in.TenantID, in.DedupField, specs, validRows)
		if dbErr != nil {
			return nil, fmt.Errorf("import.Import: dedup probe: %w", dbErr)
		}
		var dedupErrors []ImportRowError
		validRows, dedupErrors = applyDedup(validRows, in.DedupField, existing)
		rowErrors = append(rowErrors, dedupErrors...)
	}

	// --- queue #358: every OTHER unique field the rows carry is probed the same way, DB and file. The
	// unique constraint would otherwise refuse the whole batch (409 IMPORT_BATCH_DUPLICATE) with no
	// word on which row — in the preview as in the run. A colliding row is now named by its CSV line and
	// skipped (RECORD_DUPLICATE, like the dedup field); the 409 stays only for a row that lands between
	// this probe and the insert. A field the caller cannot see is not probed: "already exists" would
	// tell them a value they may not read is taken.
	for _, sp := range specs {
		if _, isHidden := hidden[sp.Name]; !sp.IsUnique || sp.Name == in.DedupField || isHidden {
			continue
		}
		existing, dbErr := s.loadExistingValues(ctx, e.Name, in.TenantID, sp.Name, specs, validRows)
		if dbErr != nil {
			return nil, fmt.Errorf("import.Import: unique probe %q: %w", sp.Name, dbErr)
		}
		var uniqueErrors []ImportRowError
		validRows, uniqueErrors = applyDedup(validRows, sp.Name, existing)
		rowErrors = append(rowErrors, uniqueErrors...)
	}

	// --- IMPORT-AUTONUM: assign auto_number values per surviving row (ENG #6) ---
	// Runs AFTER validation + relation-FK + dedup, so a sequence number is only claimed for a row
	// that will actually be inserted (gaps-allowed for a later batch-insert failure — the same
	// documented trade-off as the Create path, see assignSequenceValues). One atomic claim per
	// auto_number field per row, through the SAME primitive the write-path uses. nil claimer =
	// feature disarmed (auto_number left unassigned, batch never fails on its account). A per-row
	// claim error drops + reports that row (row-level failure), never a whole-batch abort — matching
	// processRows/resolveRelations. The CSV column-mapping already rejects an auto_number
	// mapping (computedReadonlyCode → RECORD_AUTONUMBER_READONLY in buildMapping), so the claimed
	// value is the only source for the field — no client CSV value can override it.
	// A dry run (specs/061 FR-010) stops here: a number is claimed only for a row that is written.
	if s.sequences != nil && !in.DryRun {
		var seqErrors []ImportRowError
		validRows, seqErrors = s.assignImportSequences(ctx, in.TenantID, e.Name, specs, validRows)
		rowErrors = append(rowErrors, seqErrors...)
	}

	// Extract plain payloads for the batch insert (strip the row-number wrapper).
	payloads := make([]map[string]any, 0, len(validRows))
	for _, vr := range validRows {
		payloads = append(payloads, vr.payload)
	}

	// --- Batch insert valid rows (1 transaction) ---
	// If the batch hits a unique-constraint violation (ErrDuplicateUnique) the entire
	// transaction is rolled back by the DB. We cannot determine which row caused it, so
	// we surface a 409-coded error rather than a silent 500. The caller should use
	// dedupField to prevent this: with dedup active, duplicates are caught before insert.
	total := len(rawRows)
	summary := func(inserted, auditJobsFailed int) *ImportResult {
		written := inserted
		if in.DryRun {
			written = len(payloads) // what the real run would write — the preview's "failed" is the rest
		}
		return &ImportResult{
			Total:           total,
			Inserted:        inserted,
			Failed:          total - written,
			Valid:           len(payloads),
			DryRun:          in.DryRun,
			Errors:          rowErrors,
			AuditJobsFailed: auditJobsFailed,
		}
	}
	inserted, auditJobsFailed := 0, 0
	if len(payloads) > 0 && !in.DryRun {
		ictx := ctx
		if in.leaseBase != "" {
			// FR-13: save what this import answers if it commits BEFORE the transaction — the resend
			// that finds the batch committed after a crash replays it (the per-row refusals cannot be
			// re-derived from the rows). Fail closed: no plan, no write.
			if perr := in.savePlan(ctx, summary(len(payloads), 0)); perr != nil {
				return nil, fmt.Errorf("import.Import: %w", perr)
			}
			ictx = dynrepo.WithBatchBaseID(ctx, in.leaseBase)
		}
		insertedIDs, insertErr := s.store.InsertRecords(ictx, e.Name, in.TenantID, in.UserID, payloads, uniqueFieldSpecs(specs))
		if insertErr != nil {
			if errors.Is(insertErr, dynrepo.ErrDuplicateUnique) {
				return nil, &ValidationError{
					Code: "IMPORT_BATCH_DUPLICATE",
					Msg:  "batch insert aborted: một hoặc nhiều giá trị vi phạm unique constraint — dùng dedupField để lọc trước",
				}
			}
			return nil, fmt.Errorf("import.Import: insert: %w", insertErr)
		}
		inserted = len(insertedIDs)
		if s.afterInsert != nil {
			s.afterInsert(ictx, in.TenantID, e.Name, payloads...) // specs/083: once per import batch
		}
		auditJobsFailed = s.enqueueImportEvents(ctx, in, e.Name, insertedIDs)
		for _, vr := range validRows {
			stored = append(stored, vr.hookOuts)
		}
	}

	result := summary(inserted, auditJobsFailed)

	s.logger.Info("import.Done",
		zap.String("tenant_id", in.TenantID),
		zap.String("entity_name", in.EntityName),
		zap.Int("total", total),
		zap.Int("inserted", inserted),
		zap.Int("failed", result.Failed),
		zap.Bool("dry_run", in.DryRun),
	)
	return result, nil
}

// onlyRowsOf keeps, of rows, those whose CSV line is in want (order of rows).
func onlyRowsOf(rows, want []validRow) []validRow {
	in := make(map[int]struct{}, len(want))
	for _, w := range want {
		in[w.rowNum] = struct{}{}
	}
	kept := make([]validRow, 0, len(want))
	for _, r := range rows {
		if _, ok := in[r.rowNum]; ok {
			kept = append(kept, r)
		}
	}
	return kept
}

// loadExistingValues collects the distinct string values of dedupField from valid rows,
// then issues a SINGLE batched query per chunk (via BatchExistingValuesForField) to find
// which values already exist in the DB. Returns a set of already-stored values for use by
// applyDedup (O(1) lookup per row). Replaces the old per-value O(N) loop.
//
// Column selection (mirrors RM-13):
//   - IsUnique → physical generated column (utf8mb4_bin, byte-exact, index used).
//   - IsIndexed → physical generated column (typed per D1, index used).
//   - Otherwise → JSON_UNQUOTE(JSON_EXTRACT(data,'$.field')) — same semantics as the old path.
//
// specs is the full field list for the entity; it is searched once to resolve dedupField's
// IsUnique/IsIndexed flags before the batched query is issued.
func (s *ImportService) loadExistingValues(ctx context.Context, entity, tenantID, field string, specs []FieldSpec, rows []validRow) (map[string]struct{}, error) {
	// Resolve the dedup field's generated-column flags from the spec list.
	var dedupFieldSpec dynrepo.DedupFieldSpec
	for _, sp := range specs {
		if sp.Name == field {
			dedupFieldSpec = dynrepo.DedupFieldSpec{
				IsUnique:  sp.IsUnique,
				IsIndexed: sp.IsIndexed,
			}
			break
		}
	}

	// Collect distinct non-empty string values from the valid rows.
	seen := make(map[string]struct{}, len(rows))
	for _, vr := range rows {
		v, ok := vr.payload[field]
		if !ok {
			continue
		}
		val, ok := v.(string)
		if !ok || val == "" {
			continue
		}
		seen[val] = struct{}{}
	}
	if len(seen) == 0 {
		return map[string]struct{}{}, nil
	}

	// Convert to a slice for the batched IN() query.
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}

	// One batched call (O(⌈N/chunk⌉) queries) instead of one query per distinct value.
	return s.store.BatchExistingValuesForField(ctx, entity, tenantID, field, dedupFieldSpec, values)
}

// judgeRows runs the entity's create-time before_save rules over every row through the SAME batch
// hook bulk create atomic uses — one rule load for the file, at most one hidden-field lookup. The
// value-changing rules rewrite each row's payload in place (S5); a refused row is dropped and
// reported once per broken rule (FR-015), under the rule's anchor only if the importer's role may see
// that field (R8). flush (nil when the entity has no rule) writes one summary run per rule and must be
// called once the file is decided, with the outcomes of the rows stored (S3, FR-031).
func (s *ImportService) judgeRows(ctx context.Context, in ImportInput, entity string, specs []FieldSpec, rows []validRow) (kept []validRow, errs []ImportRowError, flush func([][]workflow.MutationOutcome), err error) {
	// ponytail: a transient RecordService carries the three collaborators the hook reads — the
	// same borrowing Import already does for resolve().
	rs := &RecordService{beforeRules: s.beforeRules, beforeRuns: s.beforeRuns, fieldACL: s.fieldACL, logger: s.logger}
	if in.DryRun {
		rs.beforeRuns = nil // a preview judges every row but leaves no run in the rule's history (FR-010)
	}
	judge, flush, err := rs.createBatchHook(ctx, RecordScope{UserID: in.UserID, RoleID: in.RoleID, IsAdmin: in.IsAdmin}, in.TenantID, entity, specs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("import.judgeRows: %w", err)
	}
	if judge == nil {
		return rows, nil, nil, nil
	}
	kept = make([]validRow, 0, len(rows))
	for _, vr := range rows {
		outs, verr := judge(vr.payload)
		if verr == nil {
			vr.hookOuts = outs
			kept = append(kept, vr)
			continue
		}
		var ve *ValidationError
		var vs []workflow.Violation
		if errors.As(verr, &ve) {
			vs, _ = ve.Details["violations"].([]workflow.Violation)
		}
		if len(vs) == 0 { // never silently drop a refused row
			errs = append(errs, asImportRowError(vr.rowNum, verr))
			continue
		}
		for _, v := range vs {
			errs = append(errs, ImportRowError{Row: vr.rowNum, Field: v.Field, Code: ve.Code, Message: v.Message})
		}
	}
	return kept, errs, flush, nil
}

// assignImportSequences claims + assigns every auto_number field value for each surviving row,
// delegating to assignSequenceValues (record_orchestration.go) so the import path shares the EXACT
// atomic per-tenant/entity/field claim (ENG #6) the Create write-path uses (IMPORT-AUTONUM). Called
// only when s.sequences != nil (opt-in). One claim per row: a claim that fails (e.g. a DB error) is
// a per-row failure — the row is dropped from kept and reported as an ImportRowError (asImportRowError
// maps a wrapped claim error to IMPORT_ERROR, matching resolveRelations's DB-error convention)
// — never a whole-batch abort. Rows with no auto_number field are a no-op (assignSequenceValues just
// iterates specs and finds nothing to claim), so an entity without auto_number keeps its payload.
func (s *ImportService) assignImportSequences(ctx context.Context, tenantID, entityName string, specs []FieldSpec, rows []validRow) (kept []validRow, errs []ImportRowError) {
	kept = make([]validRow, 0, len(rows))
	for _, vr := range rows {
		if err := assignSequenceValues(ctx, s.sequences, tenantID, entityName, specs, vr.payload); err != nil {
			errs = append(errs, asImportRowError(vr.rowNum, err))
			continue
		}
		kept = append(kept, vr)
	}
	return kept, errs
}

// --- Pure functions (unit-testable without DB) --------------------------------

// parseCSV reads all rows from r using encoding/csv. Returns headers and data rows.
// Returns an error when r is empty or the CSV cannot be parsed. A file saved by Excel is read as
// Excel wrote it (specs/061 FR-008): the UTF-8 BOM of "CSV UTF-8" is dropped — the browser drops it
// when it reads the header for the mapping, so a kept BOM silently unmapped the FIRST column — a
// header with more ';' than ',' (a Vietnamese-region Excel) is split on ';', and every header cell
// is trimmed, as the screen trims it (a kept trailing space unmapped that column the same way).
func parseCSV(r io.Reader) (headers []string, rows [][]string, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("parseCSV: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	firstLine := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		firstLine = data[:i]
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.TrimLeadingSpace = true
	if bytes.Count(firstLine, []byte(";")) > bytes.Count(firstLine, []byte(",")) {
		reader.Comma = ';'
	}

	headers, err = reader.Read()
	if err != nil {
		// io.EOF means empty file — return a meaningful error.
		return nil, nil, fmt.Errorf("parseCSV: không đọc được header: %w", err)
	}
	if len(headers) == 0 {
		return nil, nil, fmt.Errorf("parseCSV: file không có cột nào")
	}
	for i, h := range headers {
		headers[i] = strings.TrimSpace(h)
	}

	rows, err = reader.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("parseCSV: %w", err)
	}
	return headers, rows, nil
}

// buildMapping validates that every target field in mapping exists in specs, and that
// all required fields are covered by some mapped column. Returns a columnMapping (csv
// header → FieldSpec) for fast per-row lookup.
//
// hidden is the caller's field-ACL hidden set (nil for an admin): a mapping that targets one of those
// fields fails with the SAME error as a target that does not exist (queue #317, §4.4b) — from inside
// this loop, not a pre-check, so which refusal wins against another bad entry is decided by the one
// map-range order for both.
func buildMapping(headers []string, raw map[string]string, specs []FieldSpec, hidden map[string]struct{}) (columnMapping, error) {
	byName := make(map[string]FieldSpec, len(specs))
	for _, sp := range specs {
		byName[sp.Name] = sp
	}

	// Track which CSV headers are in mapping (to handle only the ones the caller provided).
	headerSet := make(map[string]struct{}, len(headers))
	for _, h := range headers {
		headerSet[h] = struct{}{}
	}

	cm := make(columnMapping)
	mappedFields := make(map[string]struct{})
	for csvHeader, fieldName := range raw {
		sp, ok := byName[fieldName]
		if _, isHidden := hidden[fieldName]; !ok || isHidden {
			return nil, invalid("RECORD_UNKNOWN_FIELD",
				"mapping chỉ tới field %q không tồn tại trong metadata", fieldName)
		}
		// Formula/lookup/rollup fields are server-computed and cannot be imported from CSV
		// (ENG-COMPUTED #2/#3 parity: computedReadonlyCode is the single source of truth for
		// which field types are readonly-from-client, shared with ValidateRecord/
		// ValidatePartialRecord — a bare FieldType=="formula" check here let a lookup/rollup
		// column slip through CSV import even though POST/PUT already rejected it).
		if code, readonly := computedReadonlyCode(sp.FieldType); readonly {
			return nil, invalid(code,
				"field %q là computed field (%s) — không thể import từ CSV", fieldName, sp.FieldType)
		}
		cm[strings.TrimSpace(csvHeader)] = sp // headers are trimmed (parseCSV): so is a key sent as written
		mappedFields[fieldName] = struct{}{}
	}

	// Check that all required fields have some mapping covering them.
	// Formula/lookup/rollup fields are always server-computed and must NEVER appear in the CSV
	// mapping — they are excluded from the required-coverage check regardless of IsRequired
	// (which should never be true for a computed field — no config-time guard currently
	// enforces that, so we guard defensively here rather than assume it). Mirrors the mapping
	// loop above: same computedReadonlyCode single source of truth, so a required lookup/
	// rollup field is skipped here too instead of wrongly demanding a CSV mapping that would
	// only ever be rejected as RECORD_LOOKUP_READONLY/RECORD_ROLLUP_READONLY anyway.
	//
	// queue #144: a required field the engine fills from its default is covered too — Create lets
	// the payload omit it (ValidateRecord materializes the default), so demanding a CSV column for
	// it refused imports the form accepts. The error carries the field so the caller's lock
	// explainer can tell "you forgot the column" from "your role may not write it".
	for _, sp := range specs {
		if _, readonly := computedReadonlyCode(sp.FieldType); readonly {
			continue // always server-computed, never CSV-mappable
		}
		if sp.IsRequired && !defaultFills(sp) {
			if _, covered := mappedFields[sp.Name]; !covered {
				return nil, invalidField("IMPORT_REQUIRED_FIELD_NOT_MAPPED", sp.Name, "field bắt buộc %q chưa có mapping", sp.Name)
			}
		}
	}
	return cm, nil
}

// coerceCell converts a raw CSV string cell to the Go type expected by ValidateRecord
// for the given FieldSpec. Returns nil for an empty cell (let ValidateRecord handle
// required/default logic). A cell written for people is read as people write it (specs/061):
// picklist labels, Vietnamese-grouped amounts, day-first dates — see import_values.go. Every
// refusal names the field (FR-009) so the import report can point at the cell.
func coerceCell(spec FieldSpec, cell string) (any, error) {
	trimmed := strings.TrimSpace(cell)
	if trimmed == "" {
		return nil, nil
	}
	switch spec.FieldType {
	case "number", "rating":
		f, ok := parseHumanNumber(trimmed)
		if !ok {
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name, "giá trị %q không phải số", cell)
		}
		return f, nil
	case "boolean":
		b, ok := humanBools[strings.ToLower(trimmed)]
		if !ok {
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name, "giá trị %q không phải Có/Không", cell)
		}
		return b, nil
	case "picklist":
		code, found, ambiguous, configured := resolveChoice(spec.Options, trimmed)
		if !configured {
			return cell, nil // no declared values: ValidateRecord judges, as before
		}
		if !found {
			return nil, choiceError(spec, trimmed, ambiguous)
		}
		return code, nil
	case "multi_select":
		return coerceMultiSelect(spec, trimmed)
	case "currency":
		if strings.HasPrefix(trimmed, "{") {
			var m map[string]any
			if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
				return nil, invalidField("RECORD_INVALID_TYPE", spec.Name,
					"giá trị %q không đọc được — ghi số tiền (vd 3.200.000) hoặc JSON {\"amount\":...,\"currencyCode\":\"...\"}", cell)
			}
			return m, nil
		}
		amount, code, ok := parseHumanCurrency(trimmed)
		if !ok {
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name, "giá trị %q không phải số tiền (vd 3.200.000 hoặc 3.200.000 VND)", cell)
		}
		if code == "" {
			code, _ = spec.Options["currencyCode"].(string)
		}
		if code == "" { // Q7: never guess a currency the field does not declare
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name, "giá trị %q thiếu mã tiền tệ (vd 100 USD)", cell)
		}
		// A written code is kept verbatim: checkCurrency refuses one that differs from the
		// field's (RECORD_CURRENCY_MISMATCH) — never relabelled, never converted.
		return map[string]any{"amount": amount, "currencyCode": code}, nil
	case "date", "datetime":
		out, matched, valid := parseDayFirst(trimmed, spec.FieldType == "datetime")
		if !matched {
			return cell, nil // machine form: ValidateRecord judges it exactly as before
		}
		if !valid {
			return nil, invalidField("RECORD_INVALID_DATE", spec.Name, "ngày %q không có thật", cell)
		}
		return out, nil
	case "composite":
		var m map[string]any
		if err := json.Unmarshal([]byte(cell), &m); err != nil {
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name, "field %q (composite): cần JSON object", spec.Name)
		}
		return m, nil
	default:
		// text, email, relation, rich_text — store as string. Relation cells (an id, a name or a
		// code of the target record) are resolved later, in one batch per target (specs/061 FR-014).
		// ACTIVITY-CORE #3a: polymorphic_relation deliberately falls through here → a raw string
		// reaches checkPolymorphic, which requires a {type,id} object and rejects it as
		// RECORD_INVALID_TYPE. So CSV import CANNOT create a polymorphic value in slice 1, and the
		// import path needs no existence probe (there is never a {type,id} to probe). ⚠️ If a
		// future slice adds object-parsing here (a `case "polymorphic_relation"` doing
		// json.Unmarshal like currency/composite), it MUST also wire checkPolymorphicTargets into
		// resolveRelations — otherwise import would accept a dangling/cross-tenant target.
		return cell, nil
	}
}

// coerceMultiSelect reads a multi_select cell (FR-002): a JSON array as before, one choice that
// matches the whole cell (a label may hold a comma), or a list split on ';' (else ','). Each item
// resolves like a picklist cell. A field with no declared values passes items on untouched.
func coerceMultiSelect(spec FieldSpec, cell string) (any, error) {
	var items []any
	switch {
	case strings.HasPrefix(cell, "["):
		if err := json.Unmarshal([]byte(cell), &items); err != nil {
			return nil, invalidField("RECORD_INVALID_TYPE", spec.Name,
				"giá trị %q không đọc được — ghi các lựa chọn cách nhau bằng dấu ; (vd A; B)", cell)
		}
	default:
		if code, found, _, _ := resolveChoice(spec.Options, cell); found {
			return []any{code}, nil
		}
		sep := ","
		if strings.Contains(cell, ";") {
			sep = ";"
		}
		for _, part := range strings.Split(cell, sep) {
			if p := strings.TrimSpace(part); p != "" {
				items = append(items, p)
			}
		}
	}
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			continue // ValidateRecord refuses a non-string element
		}
		code, found, ambiguous, configured := resolveChoice(spec.Options, s)
		if !configured {
			return items, nil
		}
		if !found {
			return nil, choiceError(spec, s, ambiguous)
		}
		items[i] = code
	}
	return items, nil
}

// choiceError refuses a picklist/multi_select cell with the choices a person can read (FR-001).
func choiceError(spec FieldSpec, value string, ambiguous bool) error {
	if ambiguous {
		return invalidField("RECORD_INVALID_PICKLIST", spec.Name,
			"%q trùng nhãn của nhiều lựa chọn — ghi đúng mã lựa chọn", value)
	}
	return invalidField("RECORD_INVALID_PICKLIST", spec.Name,
		"%q không có trong danh sách: %s", value, choiceHint(spec.Options))
}

// mapRow converts a single CSV row (parallel to headers) into a payload map using
// the resolved columnMapping. Unmapped columns are silently skipped.
// Returns the cells that read and coerce errors as *ValidationError so the caller can capture them per-row — EVERY bad
// cell of the row, joined (errors.Join), so the preview marks them all at once (specs/061 SC-002).
func mapRow(headers []string, row []string, cm columnMapping) (map[string]any, error) {
	payload := make(map[string]any, len(cm))
	var cellErrs []error
	for i, header := range headers {
		spec, ok := cm[header]
		if !ok {
			continue // unmapped column — skip
		}
		cell := ""
		if i < len(row) {
			cell = row[i]
		}
		val, err := coerceCell(spec, cell)
		if err != nil {
			cellErrs = append(cellErrs, err)
			continue
		}
		if val != nil {
			payload[spec.Name] = val
		}
		// nil means empty cell: omit from payload so ValidateRecord handles required/default.
	}
	// The payload holds the cells that read, even beside errors (queue #362: the rest of the row is
	// still judged); errors.Join(nil...) is nil.
	return payload, errors.Join(cellErrs...)
}

// processRows iterates over raw CSV rows, maps + coerces + validates each one.
// Returns valid rows as []validRow (carrying the original 1-based CSV row number) and
// per-row errors. The original row number is preserved so that downstream dedup errors
// are reported against the actual CSV line, not the position in the filtered valid-set.
//
// queue #362: a row is judged through every check, not stopped at its first failing layer — a
// wrong-type cell no longer hides a missing required field or a bad email of the same row, and
// every problem of the row is reported. failed carries the rows that did not pass, with the payload
// that could be read (bad cells left out): Import still runs them through the relation and user
// lookups so the preview names those cells too, then drops them — they are never written.
func processRows(headers []string, rawRows [][]string, cm columnMapping, specs []FieldSpec) (valid, failed []validRow, errs []ImportRowError) {
	for i, row := range rawRows {
		rowNum := i + 1 // 1-based CSV data-row number (after header)

		// Step 1: map + coerce CSV strings to typed Go values.
		payload, err := mapRow(headers, row, cm)
		var cellErrs []error
		if err != nil {
			cellErrs = unjoin(err)
		}

		// Step 2: validate (reuse the record validator — do NOT re-implement). With bad cells the
		// payload lacks them, so what they would cause (a "required" for a cell that was only
		// mistyped, a dependent-picklist verdict on a parent that did not read) is not reported twice.
		clean, valErrs := validateRecordAll(specs, payload, nil)
		if len(cellErrs) > 0 {
			bad := make(map[string]bool, len(cellErrs))
			for _, ce := range cellErrs {
				bad[asImportRowError(rowNum, ce).Field] = true
			}
			kept := valErrs[:0:0]
			for _, ve := range valErrs {
				if r := asImportRowError(rowNum, ve); !bad[r.Field] && r.Code != codeRecordDependentInvalid {
					kept = append(kept, ve)
				}
			}
			valErrs = kept
		}
		if len(cellErrs)+len(valErrs) > 0 {
			for _, e := range append(cellErrs, valErrs...) {
				errs = append(errs, asImportRowError(rowNum, e))
			}
			failed = append(failed, validRow{rowNum: rowNum, payload: payload})
			continue
		}

		// Step 3: compute formula fields from the validated row data (same as Create path).
		applyComputedFields(specs, clean)
		seedStoredCounts(specs, clean) // specs/083: a new parent starts at 0

		valid = append(valid, validRow{rowNum: rowNum, payload: clean})
	}
	return valid, failed, errs
}

// applyDedup filters valid rows by dedupField: rows whose value is already in existing
// (DB probe set) or appeared earlier in this file are rejected as RECORD_DUPLICATE.
// Because each validRow carries its original 1-based CSV row number, errors point to
// the correct source line regardless of how many rows were rejected by validation before.
func applyDedup(rows []validRow, dedupField string, existing map[string]struct{}) ([]validRow, []ImportRowError) {
	var kept []validRow
	var errs []ImportRowError

	seen := make(map[string]struct{}, len(rows))
	for _, vr := range rows {
		raw, ok := vr.payload[dedupField]
		if !ok {
			// No value for dedupField in this row — keep as-is.
			kept = append(kept, vr)
			continue
		}
		val, ok := raw.(string)
		if !ok {
			// Non-string dedup value — keep and let the insert surface any constraint.
			kept = append(kept, vr)
			continue
		}
		// Check DB-existing set first, then in-file seen set.
		if _, inDB := existing[val]; inDB {
			errs = append(errs, ImportRowError{
				Row:     vr.rowNum,
				Field:   dedupField,
				Code:    "RECORD_DUPLICATE",
				Message: fmt.Sprintf("giá trị %q đã tồn tại", val),
			})
			continue
		}
		if _, inFile := seen[val]; inFile {
			errs = append(errs, ImportRowError{
				Row:     vr.rowNum,
				Field:   dedupField,
				Code:    "RECORD_DUPLICATE",
				Message: fmt.Sprintf("giá trị %q trùng với dòng trước trong file", val),
			})
			continue
		}
		seen[val] = struct{}{}
		kept = append(kept, vr)
	}
	return kept, errs
}

// unjoin splits an errors.Join result back into its parts (a lone error is its own part).
func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// asImportRowError converts any error (especially *ValidationError) to an ImportRowError.
// Field is carried over when the validator named one (invalidField: RECORD_REQUIRED_MISSING,
// RECORD_CURRENCY_MISMATCH, …) — ImportScreen renders `err.field ?? "—"`, so dropping it here
// showed a dash for every field-level reject (queue #7 found the gap).
func asImportRowError(rowNum int, err error) ImportRowError {
	if ve, ok := err.(*ValidationError); ok {
		return ImportRowError{
			Row:     rowNum,
			Field:   ve.Field,
			Code:    ve.Code,
			Message: ve.Msg,
		}
	}
	return ImportRowError{
		Row:     rowNum,
		Code:    "IMPORT_ERROR",
		Message: err.Error(),
	}
}
