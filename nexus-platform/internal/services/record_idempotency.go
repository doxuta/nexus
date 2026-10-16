package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/idempotency"
)

// Idempotency-Key on POST /:entity (specs/047-idempotency-key-on-create, DAILY-QUEUE #29/#30).
const (
	// idempotencyWindow is how long a key replays its first outcome (Stripe's 24 h). After it, the
	// claim counts as absent and the janitor deletes it.
	idempotencyWindow = 24 * time.Hour
	// idempotencyClaimAttempts bounds the claim loop: a claim that vanishes between our INSERT and
	// our SELECT (its owner's create failed and released it) or an expired claim is re-claimed.
	idempotencyClaimAttempts = 3
	// idempotencyWriteTimeout bounds the claim's claim/release/complete writes, which run DETACHED
	// from the request context (see CreateIdempotent).
	idempotencyWriteTimeout = 5 * time.Second
	// idempotencyCreateTimeout bounds the create a claim guards — the default HTTP_WRITE_TIMEOUT: a
	// create still running after it has lost its response anyway. The bound is what makes the lease
	// safe: past it the create's transaction is cancelled, so it cannot commit later.
	idempotencyCreateTimeout = 30 * time.Second
	// idempotencyLease is how long a processing claim is presumed alive (specs/047 FR-11). Past it,
	// the create it guards has certainly finished or been cancelled (create timeout + complete
	// write, with room for a commit in flight at the deadline), so a resend reconciles the claim
	// against obj_* instead of answering 409 until the 24 h window ends.
	idempotencyLease = 2 * time.Minute
	// idempotencyBatchTimeout bounds a leased atomic bulk / import run, claim to commit (specs/047
	// FR-13): past it the run's transaction is cancelled, so it cannot commit later — what makes
	// idempotencyBatchLease safe. A batch is validated, then written in one transaction, hence a
	// larger bound than a single create's.
	idempotencyBatchTimeout = 60 * time.Second
	// idempotencyBatchLease is how long a leased atomic bulk / import claim is presumed alive: the
	// run bound, the complete write, and room for a commit in flight at the deadline.
	idempotencyBatchLease = 3 * time.Minute
)

// IdempotencyStore persists the Idempotency-Key claims of POST /:entity. Satisfied by
// *idempotency.SQLRepository; faked in unit tests. Every method is pinned to (tenant, principal, key).
type IdempotencyStore interface {
	// Claim inserts a processing claim; idempotency.ErrKeyExists when the key is already claimed.
	Claim(ctx context.Context, c idempotency.Claim) error
	// Get reads the claim; idempotency.ErrNotFound when there is none.
	Get(ctx context.Context, tenantID, principalID, key string) (*idempotency.Claim, error)
	// Complete flips the processing claim holding recordID to completed.
	Complete(ctx context.Context, tenantID, principalID, key, recordID string) error
	// CompleteResponse flips a processing bulk/import claim holding recordID ("" = no lease) to
	// completed, storing its response.
	CompleteResponse(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error
	// SavePlan stores, before the batch's transaction, the response the processing claim holding
	// recordID will answer with if the batch commits (specs/047 FR-13).
	SavePlan(ctx context.Context, tenantID, principalID, key, recordID string, response []byte) error
	// Release deletes the processing claim holding recordID ("" = bulk/import), whose write failed
	// before commit.
	Release(ctx context.Context, tenantID, principalID, key, recordID string) error
	// Takeover moves a stale processing claim from oldRecordID to newRecordID; false = lost the race.
	Takeover(ctx context.Context, tenantID, principalID, key, oldRecordID, newRecordID string, expiresAt time.Time) (bool, error)
	// DeleteExpired deletes the claim only if it expired before now.
	DeleteExpired(ctx context.Context, tenantID, principalID, key string, now time.Time) error
}

// WithIdempotencyStore wires the Idempotency-Key claim store (production: *idempotency.SQLRepository).
func (s *RecordService) WithIdempotencyStore(st IdempotencyStore) *RecordService {
	s.idem = st
	return s
}

// CreateIdempotent is Create guarded by an Idempotency-Key, scoped to (tenantID, userID, key) —
// userID is the verified principal (a user id, or an API key id). It returns the record id and
// whether the call was a replay of an earlier one:
//
//   - first use of the key → the key is claimed (processing), Create runs unchanged, the claim is
//     completed with the new id → (id, false, nil);
//   - the key completed earlier with the same request → the record's existence is re-checked in the
//     tenant (no response body is stored) → (id, true, nil); a since-deleted record answers
//     ErrRecordNotFound. The check ignores the caller's row scope — see replayedRecordExists;
//   - the key carries a different request (body or entity) → 422 IDEMPOTENCY_KEY_REUSED;
//   - the key's first request is still running → 409 IDEMPOTENCY_IN_PROGRESS;
//   - the key is still processing past its lease (the process died between claim and complete, or
//     the commit's outcome was unknown) → reconciled against obj_* by the record id the claim chose
//     before its insert: the record exists → the claim is completed and replayed; it does not → this
//     call takes the claim over and creates (specs/047 FR-11).
//
// A create that fails before commit releases the key, so a corrected resend runs; one whose commit
// outcome is unknown keeps it (a resend would otherwise create a second record) until the lease
// reconciles it. The claim, release and complete writes run on a context detached from the request:
// the client that hung up is exactly the caller this exists for, and writing with its cancelled
// context would leave the key stuck in processing. If Complete fails after the record committed, the
// record's id is still returned and the key stays processing until its lease — the reconciliation
// then finds the record and replays it, never a second record.
func (s *RecordService) CreateIdempotent(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payload map[string]any, key string) (string, bool, error) {
	if s.idem == nil {
		return "", false, errors.New("services.RecordService.CreateIdempotent: idempotency store not wired")
	}
	if userID == "" || key == "" {
		return "", false, errors.New("services.RecordService.CreateIdempotent: empty principal or key")
	}
	hash := idempotencyRequestHash(entityName, payload)
	for attempt := 0; attempt < idempotencyClaimAttempts; attempt++ {
		now := time.Now()
		recordID := uuid.NewString()
		owned, existing, err := claimStep(ctx, s.idem, tenantID, userID, entityName, key, hash, recordID, now)
		if err != nil {
			return "", false, err
		}
		if owned {
			return s.createClaimed(ctx, scope, tenantID, userID, entityName, payload, key, recordID)
		}
		if existing == nil {
			continue // released or expired under us — claim again
		}
		if existing.Status != idempotency.StatusCompleted {
			// expires_at is the claim (or takeover) time plus the window. A claim written before FR-11
			// names no record: nothing to reconcile, it waits out its window.
			claimedAt := existing.ExpiresAt.Add(-idempotencyWindow)
			if existing.RecordID == "" || now.Before(claimedAt.Add(idempotencyLease)) {
				return "", false, idempotencyInProgress(key)
			}
			id, replayed, done, err := s.reconcileStaleClaim(ctx, scope, tenantID, userID, entityName, payload, key, existing.RecordID)
			if done {
				return id, replayed, err
			}
			continue // another resend took the claim over first — read it again
		}
		if err := s.replayedRecordExists(ctx, tenantID, userID, entityName, existing.RecordID); err != nil {
			return "", false, err
		}
		return existing.RecordID, true, nil
	}
	return "", false, idempotencyInProgress(key)
}

// claimStep makes ONE claim attempt on (tenantID, userID, key) for the request fingerprinted by
// hash, shared by every Idempotency-Key path so the claim rules cannot drift between them:
//   - owned=true: this call now holds a fresh processing claim (holding recordID; "" for bulk/import);
//   - owned=false, existing=nil: the claim vanished under us (its owner released it) or had expired
//     and was cleared — try again;
//   - existing != nil: the live claim of the SAME request that this call lost to (completed or still
//     processing) — the caller decides replay / 409 / lease;
//   - a different request under the key: 422 IDEMPOTENCY_KEY_REUSED.
//
// The claim INSERT runs on a context detached from the request: a client hanging up at that instant
// can see its autocommit INSERT land while the driver reports the cancellation, leaving a claim that
// no request stands behind.
func claimStep(ctx context.Context, store IdempotencyStore, tenantID, userID, entityName, key, hash, recordID string, now time.Time) (owned bool, existing *idempotency.Claim, err error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
	err = store.Claim(wctx, idempotency.Claim{
		TenantID: tenantID, PrincipalID: userID, Key: key, EntityName: entityName,
		RequestHash: hash, RecordID: recordID, ExpiresAt: now.Add(idempotencyWindow),
	})
	cancel()
	if err == nil {
		return true, nil, nil
	}
	if !errors.Is(err, idempotency.ErrKeyExists) {
		return false, nil, fmt.Errorf("services.claimStep: claim: %w", err)
	}
	existing, err = store.Get(ctx, tenantID, userID, key)
	if errors.Is(err, idempotency.ErrNotFound) {
		return false, nil, nil // released between our INSERT and this read
	}
	if err != nil {
		return false, nil, fmt.Errorf("services.claimStep: read claim: %w", err)
	}
	if existing.ExpiresAt.Before(now) {
		// Past its window but not swept yet: it no longer binds the key. Strictly before, the same
		// boundary DeleteExpired's `expires_at < ?` uses — otherwise a claim expiring exactly at `now`
		// would be judged expired here, survive the delete, and spin the loop into 409.
		if err := store.DeleteExpired(ctx, tenantID, userID, key, now); err != nil {
			return false, nil, fmt.Errorf("services.claimStep: clear expired claim: %w", err)
		}
		return false, nil, nil
	}
	if existing.RequestHash != hash {
		return false, nil, invalid("IDEMPOTENCY_KEY_REUSED",
			"Idempotency-Key %q đã dùng cho một yêu cầu khác trong 24 giờ qua — mỗi yêu cầu tạo mới cần một khoá riêng", key)
	}
	return false, existing, nil
}

// runIdempotent guards a multi-row write — POST /:entity/bulk or /:entity/import (specs/047 FR-12)
// — by an Idempotency-Key. Its outcome is a per-row envelope that no single record id can
// re-derive, so, unlike CreateIdempotent, the response is STORED at completion and replayed
// verbatim:
//   - first use → run under a claim. wrote=true (at least one row committed) → the claim completes
//     with the JSON of response, and (nil, nil) is returned — the caller answers with the response
//     its run closure captured. wrote=false (nothing written: an atomic abort, every row refused),
//     with or without an error → the claim is released so a corrected resend runs. An error after
//     rows were written (wrote=true — a partial batch that broke mid-way) or whose commit outcome is
//     unknown keeps the claim: a resend could write the rows twice.
//   - the same request completed before → (stored response, nil): the caller answers 200 +
//     Idempotent-Replay. A different request → 422 IDEMPOTENCY_KEY_REUSED; still running → 409.
//
// Lease (FR-13): a batch written in ONE transaction — an atomic bulk, an import — can be leased.
// lease != nil makes the claim carry a base id chosen before the write; the run gives its rows the
// ids derived from it (dynrepo.WithBatchBaseID), so a claim left processing past
// idempotencyBatchLease (the process died; the commit's outcome was unknown) is reconciled by
// probing ONE row: it exists → the whole batch committed → the saved plan / rebuilt response is
// stored and replayed; it does not → the resend takes the claim over and runs the batch again.
// lease == nil (partial bulk: rows commit one by one, so no single id tells which committed) keeps
// the FR-12 behaviour — a stuck claim waits out its 24 h window (409), never a second batch.
//
// run receives the request context and the claim's batch (base id, plan writer). A run that commits
// row by row must detach from the request context (see CreateBulkIdempotent): cancelled mid-way it
// would fail after writing and keep the claim. A run in ONE transaction (import) may keep it —
// cancelled, it rolls back and the claim is released. A leased run MUST bound itself by
// idempotencyBatchTimeout: that bound is what makes the lease safe.
func runIdempotent(ctx context.Context, store IdempotencyStore, log *zap.Logger, lease *batchLease, tenantID, userID, entityName, key, hash string,
	run func(ctx context.Context, b claimedBatch) (response any, wrote bool, err error)) (json.RawMessage, error) {
	if store == nil {
		return nil, errors.New("services.runIdempotent: idempotency store not wired")
	}
	if userID == "" || key == "" {
		return nil, errors.New("services.runIdempotent: empty principal or key")
	}
	for attempt := 0; attempt < idempotencyClaimAttempts; attempt++ {
		now := time.Now()
		baseID := ""
		if lease != nil {
			baseID = uuid.NewString()
		}
		owned, existing, err := claimStep(ctx, store, tenantID, userID, entityName, key, hash, baseID, now)
		if err != nil {
			return nil, err
		}
		if owned {
			return nil, runClaimed(ctx, store, log, tenantID, userID, entityName, key, baseID, run)
		}
		if existing == nil {
			continue // released or expired under us — claim again
		}
		if existing.Status == idempotency.StatusCompleted && len(existing.Response) > 0 {
			return json.RawMessage(existing.Response), nil
		}
		// A claim written before FR-13 (or a partial bulk's) names no base id: nothing to reconcile.
		if lease == nil || existing.Status == idempotency.StatusCompleted || existing.RecordID == "" ||
			now.Before(existing.ExpiresAt.Add(-idempotencyWindow).Add(idempotencyBatchLease)) {
			return nil, idempotencyInProgress(key)
		}
		replay, retakenID, err := reconcileStaleBatch(ctx, store, log, lease, tenantID, userID, entityName, key, existing)
		if err != nil {
			return nil, err
		}
		if replay != nil {
			return replay, nil
		}
		if retakenID != "" {
			return nil, runClaimed(ctx, store, log, tenantID, userID, entityName, key, retakenID, run)
		}
		// another resend took the claim over first — read it again
	}
	return nil, idempotencyInProgress(key)
}

// batchLease is what a multi-row write gives runIdempotent to make its claim leasable (FR-13).
type batchLease struct {
	// committed reports whether the batch written under baseID committed: its row 0 (the row whose
	// id IS baseID) exists in the tenant. The batch is one transaction, so one row stands for all.
	committed func(ctx context.Context, baseID string) (bool, error)
	// rebuild derives a committed batch's response from baseID alone (an atomic bulk: every id is
	// BatchRowID(baseID, i)). nil ⇒ the response saved by the plan (claimedBatch.plan) is replayed.
	rebuild func(baseID string) (any, error)
}

// claimedBatch is a claim a run owns: the batch's base id ("" without a lease) and the writer of
// its plan — the response the run will answer with if its transaction commits, saved BEFORE it so a
// resend that finds the batch committed can replay what the dead run would have answered. plan is
// nil without a lease.
type claimedBatch struct {
	baseID string
	plan   func(ctx context.Context, response any) error
}

// reconcileStaleBatch settles a leased claim stuck in processing past its lease. replay != nil ⇒
// the batch had committed and its response was stored: answer it. retakenID != "" ⇒ nothing
// committed and this call won the takeover: run the batch under that id. Both empty ⇒ another
// resend took the claim over first; read it again.
func reconcileStaleBatch(ctx context.Context, store IdempotencyStore, log *zap.Logger, lease *batchLease, tenantID, userID, entityName, key string,
	existing *idempotency.Claim) (replay json.RawMessage, retakenID string, err error) {
	committed, err := lease.committed(ctx, existing.RecordID)
	if err != nil {
		return nil, "", fmt.Errorf("services.reconcileStaleBatch: probe batch: %w", err)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
	defer cancel()
	if committed {
		body := existing.Response
		if len(body) == 0 && lease.rebuild != nil {
			response, rerr := lease.rebuild(existing.RecordID)
			if rerr != nil {
				return nil, "", fmt.Errorf("services.reconcileStaleBatch: rebuild response: %w", rerr)
			}
			if body, rerr = json.Marshal(response); rerr != nil {
				return nil, "", fmt.Errorf("services.reconcileStaleBatch: marshal rebuilt response: %w", rerr)
			}
		}
		if len(body) == 0 {
			// Unreachable by construction (a leased import saves its plan before writing); never
			// guess a response for rows that exist.
			return nil, "", fmt.Errorf("services.reconcileStaleBatch: batch %s committed but no response was saved", existing.RecordID)
		}
		if cerr := store.CompleteResponse(wctx, tenantID, userID, key, existing.RecordID, body); cerr != nil {
			// The batch IS written and the body is known: answer it; the claim stays processing and the
			// next resend reconciles it again — never a second batch.
			log.Error("services.reconcileStaleBatch: complete reconciled claim",
				zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName), zap.Error(cerr))
		}
		return body, "", nil
	}
	// Nothing committed, and past the lease nothing can commit any more: take the claim over.
	newID := uuid.NewString()
	won, err := store.Takeover(wctx, tenantID, userID, key, existing.RecordID, newID, time.Now().Add(idempotencyWindow))
	if err != nil {
		return nil, "", fmt.Errorf("services.reconcileStaleBatch: take over stale claim: %w", err)
	}
	if !won {
		return nil, "", nil
	}
	return nil, newID, nil
}

// runClaimed runs a multi-row write under a claim this call owns (holding baseID, "" without a
// lease), then completes (storing the response) or releases the claim — see runIdempotent.
func runClaimed(ctx context.Context, store IdempotencyStore, log *zap.Logger, tenantID, userID, entityName, key, baseID string,
	run func(ctx context.Context, b claimedBatch) (response any, wrote bool, err error)) error {
	b := claimedBatch{baseID: baseID}
	if baseID != "" {
		b.plan = func(ctx context.Context, response any) error {
			body, err := json.Marshal(response)
			if err != nil {
				return fmt.Errorf("services.runClaimed: marshal plan: %w", err)
			}
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
			defer cancel()
			if err := store.SavePlan(pctx, tenantID, userID, key, baseID, body); err != nil {
				return fmt.Errorf("services.runClaimed: save plan: %w", err)
			}
			return nil
		}
	}
	response, wrote, err := run(ctx, b)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
	defer cancel()
	fields := []zap.Field{zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName)}
	if err != nil && (wrote || errors.Is(err, dynrepo.ErrCommitOutcomeUnknown)) {
		// Rows are, or may be, written although the call failed (a partial batch that broke mid-way,
		// a commit whose outcome is unknown): releasing would let a resend write them twice. The
		// claim stays processing (409 on resend) until its lease (leased batch) or its window.
		log.Warn("services.runIdempotent: failed after writing, claim kept", append(fields, zap.Error(err))...)
		return err
	}
	if err != nil || !wrote {
		if rerr := store.Release(wctx, tenantID, userID, key, baseID); rerr != nil {
			log.Error("services.runIdempotent: release claim", append(fields, zap.Error(rerr))...)
		}
		return err
	}
	body, err := json.Marshal(response)
	if err == nil {
		err = store.CompleteResponse(wctx, tenantID, userID, key, baseID, body)
	}
	if err != nil {
		// The rows ARE written: answer with them. The claim stays processing (409 on resend) until its
		// lease (leased batch: the reconciliation replays it) or its window — never a second batch.
		log.Error("services.runIdempotent: complete claim", append(fields, zap.Error(err))...)
	}
	return nil
}

// CreateBulkIdempotent is CreateBulk guarded by an Idempotency-Key (specs/047 FR-12/FR-13, see
// runIdempotent). replay != nil ⇒ the same request completed before: answer it verbatim.
func (s *RecordService) CreateBulkIdempotent(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payloads []map[string]any, mode BulkMode, key string) (result BulkCreateResult, replay json.RawMessage, err error) {
	// Fingerprint BEFORE CreateBulk runs: it strips each row's "$ref" marker in place.
	hash := idempotencyRequestHash("bulk\x00"+string(mode)+"\x00"+entityName, map[string]any{"records": payloads})
	var lease *batchLease
	if mode == BulkModeAtomic {
		// One transaction: the batch is leasable, and — every row created or none — its response is
		// a function of the row count and the base id.
		n := len(payloads)
		lease = &batchLease{
			committed: func(ctx context.Context, baseID string) (bool, error) {
				e, _, err := s.resolve(ctx, tenantID, entityName)
				if err != nil {
					return false, err
				}
				return s.store.RecordExists(ctx, e.Name, tenantID, baseID)
			},
			rebuild: func(baseID string) (any, error) { return rebuildAtomicBulkResult(baseID, n), nil },
		}
	}
	replay, err = runIdempotent(ctx, s.idem, s.log(), lease, tenantID, userID, entityName, key, hash,
		func(ctx context.Context, b claimedBatch) (any, bool, error) {
			// The batch runs DETACHED from the request. A partial row commits on its own, so a client
			// hanging up mid-batch — the case the key exists for — would cancel the rows after it,
			// fail the batch after writing, and keep the key processing for 24 h: every resend 409,
			// no way to learn which rows exist. Detached, the batch finishes and stores its response,
			// and the resend replays it. Bounded by the single create's timeout per row; a leased
			// atomic batch by idempotencyBatchTimeout in all (the bound its lease relies on).
			timeout := time.Duration(len(payloads)) * idempotencyCreateTimeout
			if b.baseID != "" {
				timeout = idempotencyBatchTimeout
			}
			bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			if b.baseID != "" {
				bctx = dynrepo.WithBatchBaseID(bctx, b.baseID)
			}
			var rerr error
			result, rerr = s.CreateBulk(bctx, scope, tenantID, userID, entityName, payloads, mode)
			return result, result.Created > 0, rerr
		})
	return result, replay, err
}

// rebuildAtomicBulkResult is the response of a committed atomic bulk of n rows written under baseID:
// every row created, with the id dynrepo derived for it (BatchRowID) — what createBulkAtomic answered.
func rebuildAtomicBulkResult(baseID string, n int) BulkCreateResult {
	result := BulkCreateResult{Mode: string(BulkModeAtomic), Created: n, Results: make([]BulkRowResult, 0, n)}
	for i := 0; i < n; i++ {
		result.Results = append(result.Results, BulkRowResult{Index: i, Status: "created", ID: dynrepo.BatchRowID(baseID, i)})
	}
	return result
}

// replayedRecordExists checks that a replayed key's record still exists: pinned to the tenant, soft-
// deleted rows excluded (a since-deleted record answers ErrRecordNotFound → 404), but NOT narrowed
// by the caller's row scope. POST's scope carries the row level of the CREATE grant (RequireACL maps
// POST to create), not of view; a record reassigned since (round-robin `assign`, a manual reassign)
// falls outside an owner/team create scope, and a create grant at level none sees nothing. Those
// replays would answer 404 for a record that exists, and a client reading 404 as "not created" POSTs
// again with a fresh key — the duplicate this exists to prevent. The replay hands back only the id
// this same principal already received on the first call, so no row scope protects anything here.
func (s *RecordService) replayedRecordExists(ctx context.Context, tenantID, userID, entityName, recordID string) error {
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}
	if _, err := s.store.GetRecord(ctx, e.Name, tenantID, recordID, dynrepo.RowScope{UserID: userID, Level: dynrepo.AccessAll}); err != nil {
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return err
		}
		return fmt.Errorf("services.RecordService.CreateIdempotent: read replayed record: %w", err)
	}
	return nil
}

// reconcileStaleClaim settles a processing claim past its lease that still holds staleID — the
// record id its create was going to use. done=false means another resend took the claim over first
// and the caller should read it again.
func (s *RecordService) reconcileStaleClaim(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payload map[string]any, key, staleID string) (id string, replayed, done bool, err error) {
	e, _, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", false, true, err
	}
	committed, err := s.store.RecordExists(ctx, e.Name, tenantID, staleID)
	if err != nil {
		return "", false, true, fmt.Errorf("services.RecordService.CreateIdempotent: reconcile stale claim: %w", err)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
	defer cancel()
	if committed {
		// The create committed and only its Complete was lost: finish it, answer as a replay.
		if cerr := s.idem.Complete(wctx, tenantID, userID, key, staleID); cerr != nil {
			s.log().Error("services.RecordService.CreateIdempotent: complete reconciled claim",
				zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName),
				zap.String("record_id", staleID), zap.Error(cerr))
		}
		return staleID, true, true, nil
	}
	// Nothing committed, and past the lease nothing can commit any more: take the claim over.
	recordID := uuid.NewString()
	won, err := s.idem.Takeover(wctx, tenantID, userID, key, staleID, recordID, time.Now().Add(idempotencyWindow))
	if err != nil {
		return "", false, true, fmt.Errorf("services.RecordService.CreateIdempotent: take over stale claim: %w", err)
	}
	if !won {
		return "", false, false, nil
	}
	id, replayed, err = s.createClaimed(ctx, scope, tenantID, userID, entityName, payload, key, recordID)
	return id, replayed, true, err
}

// createClaimed runs Create under a claim this call owns — holding recordID, the id the record is
// inserted with — then completes or releases the claim.
func (s *RecordService) createClaimed(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payload map[string]any, key, recordID string) (string, bool, error) {
	cctx, cancelCreate := context.WithTimeout(dynrepo.WithRecordID(ctx, recordID), idempotencyCreateTimeout)
	id, err := s.Create(cctx, scope, tenantID, userID, entityName, payload)
	cancelCreate()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idempotencyWriteTimeout)
	defer cancel()
	if err != nil {
		if errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
			// The record may exist: releasing would let a resend create a second one. The claim stays
			// processing; after its lease a resend reconciles it against obj_*.
			s.log().Warn("services.RecordService.CreateIdempotent: commit outcome unknown, claim kept",
				zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName),
				zap.String("record_id", recordID), zap.Error(err))
			return "", false, err
		}
		if rerr := s.idem.Release(wctx, tenantID, userID, key, recordID); rerr != nil {
			// The key stays processing until its lease: resends get 409, then the reconciliation.
			s.log().Error("services.RecordService.CreateIdempotent: release claim after failed create",
				zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName), zap.Error(rerr))
		}
		return "", false, err
	}
	if cerr := s.idem.Complete(wctx, tenantID, userID, key, recordID); cerr != nil {
		// The record IS created: answer with it. The key stays processing (409 on resend) until the
		// lease reconciliation finds the record — never a second record.
		s.log().Error("services.RecordService.CreateIdempotent: complete claim after create",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.String("entity_name", entityName),
			zap.String("record_id", id), zap.Error(cerr))
	}
	return id, false, nil
}

func (s *RecordService) log() *zap.Logger {
	if s.logger != nil {
		return s.logger
	}
	return zap.NewNop()
}

func idempotencyInProgress(key string) error {
	return invalid("IDEMPOTENCY_IN_PROGRESS",
		"Yêu cầu đầu tiên với Idempotency-Key %q vẫn đang xử lý — thử lại sau giây lát để nhận kết quả của nó", key)
}

// idempotencyRequestHash fingerprints a create request: hex SHA-256 of entity + NUL + the JSON of
// the BOUND payload. json.Marshal sorts map keys at every level and JSON numbers bind as float64,
// so key order, whitespace and 2 vs 2.0 do not change the fingerprint — no need to re-read the raw
// body. A payload that cannot marshal (impossible after bindJSON) hashes its error text, which can
// only ever match itself.
func idempotencyRequestHash(entityName string, payload map[string]any) string {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(err.Error())
	}
	h := sha256.New()
	h.Write([]byte(entityName))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
