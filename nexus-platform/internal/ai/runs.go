package ai

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Run is one ledger record for a single AI-Builder chat turn (sys_ai_runs, spec §5). PromptHash is
// the sha256 hex of the last user message — the raw prompt is NEVER stored (it can carry customer
// business data). PromptTokens/OutputTokens/LatencyMS are nullable in the schema but modeled as
// ints here; the writer stores them as NULL when the value is 0 AND the turn never reached a
// gateway response (see RunRecorder impl), otherwise the measured value.
type Run struct {
	ID           string
	TenantID     string
	UserID       string
	Feature      string
	Model        string
	PromptHash   string
	PromptTokens int
	OutputTokens int
	ToolCalls    int
	Outcome      string
	LatencyMS    int
}

// RunRecorder persists an AI-run ledger row. Declared consumer-side so the agent depends on the
// interface, not the sqlx impl; unit tests inject a fake, production wires *SQLRunRepository.
type RunRecorder interface {
	Record(ctx context.Context, run Run) error
}

// SQLRunRepository writes sys_ai_runs rows over sqlx with parameterized queries. It mirrors the
// audit.SQLRepository style: a fixed table, every value a `?` placeholder, tenant_id pinned on the
// row (multi-tenant invariant). now is an injectable clock (defaults to time.Now) so tests can pin
// created_at if needed.
type SQLRunRepository struct {
	db  *sqlx.DB
	now func() time.Time
}

// NewSQLRunRepository wires the ledger writer over an existing sqlx connection.
func NewSQLRunRepository(db *sqlx.DB) *SQLRunRepository {
	return &SQLRunRepository{db: db, now: time.Now}
}

// Record appends one row to sys_ai_runs. Token counts are stored NULL when 0 (a turn that never
// received a gateway response has no token data — NULL is more honest than a fabricated 0); a
// non-zero count is stored verbatim. tool_calls is stored as-is (0 is meaningful: a turn with no
// tool use). created_at is set from the injectable clock in UTC.
func (r *SQLRunRepository) Record(ctx context.Context, run Run) error {
	const q = `INSERT INTO sys_ai_runs
		(id, tenant_id, user_id, feature, model, prompt_hash,
		 prompt_tokens, output_tokens, tool_calls, outcome, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		run.ID, run.TenantID, run.UserID, run.Feature, run.Model, run.PromptHash,
		nullableInt(run.PromptTokens), nullableInt(run.OutputTokens), run.ToolCalls,
		run.Outcome, nullableInt(run.LatencyMS), r.now().UTC(),
	); err != nil {
		return fmt.Errorf("ai.SQLRunRepository.Record: %w", err)
	}
	return nil
}

// TokensSince returns the tenant's total token spend (prompt + output) recorded in sys_ai_runs at
// or after `since` — the M4 daily-budget reader (spec §12), called on every gated chat turn
// (BudgetGuard.Acquire step 3). Both token columns are nullable (a turn that never reached the
// gateway stores NULL), so each is COALESCE'd to 0 before summing; the outer COALESCE keeps a
// no-rows day at 0 instead of SQL NULL. created_at is always written UTC by Record (r.now().UTC()),
// so callers pass a UTC day boundary. The query is parameterized and tenant-pinned, seeking on the
// composite idx_tenant_created (tenant_id, created_at — migration 000039) so the hot-path lookup
// stays O(rows-today), not a scan of the tenant's full history; its error is PROPAGATED — the
// budget gate must fail closed, unlike the fail-open ledger write.
func (r *SQLRunRepository) TokensSince(ctx context.Context, tenantID string, since time.Time) (int64, error) {
	const q = `SELECT COALESCE(SUM(COALESCE(prompt_tokens,0)+COALESCE(output_tokens,0)),0)
		FROM sys_ai_runs WHERE tenant_id = ? AND created_at >= ?`
	var total int64
	if err := r.db.GetContext(ctx, &total, q, tenantID, since.UTC()); err != nil {
		return 0, fmt.Errorf("ai.SQLRunRepository.TokensSince: %w", err)
	}
	return total, nil
}

// TokensSinceByUser is TokensSince narrowed to ONE user of the tenant — the reader behind the
// assistant's per-user daily-share gate (ASSISTANT_DAILY_TOKEN_SHARE_PCT): sys_ai_runs records
// user_id on every row (migration 000036), so a user's share of the tenant budget is a plain
// tenant-pinned SUM with one extra predicate. It seeks on the SAME composite idx_tenant_created
// (tenant_id, created_at — migration 000039) and filters user_id over today's tenant rows — that
// residue is bounded by the daily budget itself, so no user_id index is needed. Same COALESCE and
// UTC-boundary contract as TokensSince; same fail-closed error propagation.
func (r *SQLRunRepository) TokensSinceByUser(ctx context.Context, tenantID, userID string, since time.Time) (int64, error) {
	const q = `SELECT COALESCE(SUM(COALESCE(prompt_tokens,0)+COALESCE(output_tokens,0)),0)
		FROM sys_ai_runs WHERE tenant_id = ? AND user_id = ? AND created_at >= ?`
	var total int64
	if err := r.db.GetContext(ctx, &total, q, tenantID, userID, since.UTC()); err != nil {
		return 0, fmt.Errorf("ai.SQLRunRepository.TokensSinceByUser: %w", err)
	}
	return total, nil
}

// nullableInt maps a 0 to SQL NULL and any other value to itself, so the nullable INT columns
// (prompt_tokens/output_tokens/latency_ms) store NULL rather than a fabricated 0 when the metric
// was never captured.
func nullableInt(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}
