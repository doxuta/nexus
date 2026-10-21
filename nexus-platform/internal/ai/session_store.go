package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// ErrSessionNotFound is returned when a session id does not resolve to a row owned by the
// (tenantID, userID) making the request — whether it never existed, belongs to another user, or
// belongs to another tenant. The three cases are DELIBERATELY indistinguishable: the handler maps
// this to a 404 with an identical body so a guessed id never discloses that a session exists under
// another owner (spec §7 — no existence leak, never a 403).
var ErrSessionNotFound = errors.New("ai: session not found")

// ErrProposalNotFound is returned by SettleProposal when (seq, index) names no proposal card in a
// builder session the caller owns — the same answer for another owner's session as for a missing
// card, so a guessed id discloses nothing.
var ErrProposalNotFound = errors.New("ai: proposal not found")

// maxTitleRunes clamps a session title to the VARCHAR(200) column (counted in runes, matching
// utf8mb4 character semantics). AutoTitle already truncates to ~60; this is a defensive backstop
// for any caller-supplied title.
const maxTitleRunes = 200

// autoTitleRunes is the target length of an auto-derived title (spec §11: rune-truncate the first
// user message, NO LLM call).
const autoTitleRunes = 60

// Session feature values (sys_ai_sessions.feature, mig 000059 — EASE audit F9-5): which AI
// surface owns a thread. The admin AI Builder and the every-user ⌘K assistant share the ONE
// session store; the feature column keeps their rails separate — an admin's ⌘K threads must not
// appear in the Builder session list, and assistant threads are listed/resumed only through the
// /assistant/sessions routes. EVERY session statement pins feature alongside (tenant_id, user_id):
// a cross-feature id resolves to ErrSessionNotFound exactly like a cross-user one (no leak, and a
// ⌘K turn can never append onto a Builder thread or vice versa).
const (
	SessionFeatureBuilder   = "builder"
	SessionFeatureAssistant = "assistant"
)

// assistantSessionListCap bounds GET /assistant/sessions: the ⌘K palette shows a short "recent
// threads" rail, not an archive — 20 newest is plenty and keeps the palette payload tiny.
const assistantSessionListCap = 20

// Session is one saved AI-Builder conversation thread (sys_ai_sessions). It is per (TenantID,
// UserID): a session belongs to exactly one builder. The raw conversation lives in sys_ai_messages
// — this row is the header (title + timestamps).
//
// NOTE ON CONTENT-AT-REST (spec §11): unlike the sys_ai_runs ledger (which is hash-only), the
// session store keeps the RAW conversation because resuming a chat requires it. This is the
// tenant's own data, scoped per (tenant, user) exactly like an obj_* record. The sys_ai_runs ledger
// is a separate store with a separate purpose (audit/cost) and is not touched by anything here.
type Session struct {
	ID        string    `db:"id"`
	TenantID  string    `db:"tenant_id"`
	UserID    string    `db:"user_id"`
	Title     string    `db:"title"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// SessionSummary is the compact row the session-list endpoint returns (newest first).
type SessionSummary struct {
	ID        string    `db:"id"`
	Title     string    `db:"title"`
	UpdatedAt time.Time `db:"updated_at"`
}

// Message is one persisted turn of a session (sys_ai_messages) in domain form. Role is always
// server-written "user" or "assistant" — a client-supplied role never reaches this table. DraftJSON
// is the assistant turn's proposed draft[] (nil when the turn produced no draft); it round-trips so
// the FE can re-render an applicable draft-card on resume.
type Message struct {
	ID        string
	SessionID string
	TenantID  string
	Seq       int
	Role      string
	Content   string
	DraftJSON json.RawMessage
	// Settled lists the indexes of DraftJSON's items the builder applied, discarded or undid in the
	// Studio (issue #42, SettleProposal); nil = none settled.
	Settled   []int
	CreatedAt time.Time
}

// messageRow is the scan target for a sys_ai_messages row. draft_json is nullable, so it scans into
// a *json.RawMessage (a bare json.RawMessage cannot receive SQL NULL — the workflow repo uses the
// same pointer pattern for its nullable JSON column); the mapper converts a nil pointer to a nil
// Message.DraftJSON.
type messageRow struct {
	ID          string           `db:"id"`
	SessionID   string           `db:"session_id"`
	TenantID    string           `db:"tenant_id"`
	Seq         int              `db:"seq"`
	Role        string           `db:"role"`
	Content     string           `db:"content"`
	DraftJSON   *json.RawMessage `db:"draft_json"`
	SettledJSON *json.RawMessage `db:"settled_json"`
	CreatedAt   time.Time        `db:"created_at"`
}

// SessionStore persists AI-Builder sessions + messages over sqlx with parameterized queries. It
// mirrors the SQLRunRepository style: fixed table names, every value a `?` placeholder, and — the
// non-negotiable invariant — tenant_id (plus user_id for sessions) pins EVERY statement. now is an
// injectable clock (defaults to time.Now) so tests can pin timestamps.
type SessionStore struct {
	db  *sqlx.DB
	now func() time.Time
}

// NewSessionStore wires the store over an existing sqlx connection.
func NewSessionStore(db *sqlx.DB) *SessionStore {
	return &SessionStore{db: db, now: time.Now}
}

// ListSessions returns the caller's own non-deleted BUILDER sessions, newest-updated first. It is
// pinned to tenant_id AND user_id: a builder only ever sees their own sessions. Since mig 000059 it
// also pins feature='builder' — an admin's ⌘K assistant threads no longer leak into the Builder
// rail (F9-5). id is the deterministic tie-break for sessions sharing an updated_at.
func (s *SessionStore) ListSessions(ctx context.Context, tenantID, userID string) ([]SessionSummary, error) {
	return s.listSessions(ctx, tenantID, userID, SessionFeatureBuilder, 0)
}

// ListAssistantSessions returns the caller's own non-deleted ⌘K ASSISTANT threads, newest-updated
// first, capped at assistantSessionListCap (the palette rail, not an archive).
func (s *SessionStore) ListAssistantSessions(ctx context.Context, tenantID, userID string) ([]SessionSummary, error) {
	return s.listSessions(ctx, tenantID, userID, SessionFeatureAssistant, assistantSessionListCap)
}

// listSessions is the one list body both feature wrappers share. limit <= 0 means no LIMIT clause
// (the Builder rail's historical behavior). The (tenant_id, user_id) index already narrows to one
// user's handful of rows; feature is a residual filter over those.
func (s *SessionStore) listSessions(ctx context.Context, tenantID, userID, feature string, limit int) ([]SessionSummary, error) {
	q := `SELECT id, title, updated_at
		FROM sys_ai_sessions
		WHERE tenant_id = ? AND user_id = ? AND feature = ? AND deleted_at IS NULL
		ORDER BY updated_at DESC, id DESC`
	args := []any{tenantID, userID, feature}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	var rows []SessionSummary
	if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("ai.SessionStore.listSessions: %w", err)
	}
	return rows, nil
}

// GetSession loads a BUILDER session and its messages, GUARDED by ownership: the session row must
// match (tenantID, userID), carry feature='builder' and be non-deleted, else ErrSessionNotFound
// (never another owner's — or the assistant surface's — session, no existence leak).
func (s *SessionStore) GetSession(ctx context.Context, tenantID, userID, id string) (Session, []Message, error) {
	return s.getSession(ctx, tenantID, userID, id, SessionFeatureBuilder)
}

// GetAssistantSession is GetSession for the ⌘K assistant surface (feature='assistant').
func (s *SessionStore) GetAssistantSession(ctx context.Context, tenantID, userID, id string) (Session, []Message, error) {
	return s.getSession(ctx, tenantID, userID, id, SessionFeatureAssistant)
}

// getSession is the one get body both feature wrappers share. The message read is pinned to the
// same tenant_id as belt-and-suspenders on top of the session ownership check, ordered by seq
// (with created_at, id tie-breaks for full determinism even in the accepted single-user seq race).
func (s *SessionStore) getSession(ctx context.Context, tenantID, userID, id, feature string) (Session, []Message, error) {
	const sq = `SELECT id, tenant_id, user_id, title, created_at, updated_at
		FROM sys_ai_sessions
		WHERE id = ? AND tenant_id = ? AND user_id = ? AND feature = ? AND deleted_at IS NULL`
	var sess Session
	if err := s.db.GetContext(ctx, &sess, sq, id, tenantID, userID, feature); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, nil, ErrSessionNotFound
		}
		return Session{}, nil, fmt.Errorf("ai.SessionStore.GetSession: %w", err)
	}

	const mq = `SELECT id, session_id, tenant_id, seq, role, COALESCE(content, '') AS content, draft_json, settled_json, created_at
		FROM sys_ai_messages
		WHERE session_id = ? AND tenant_id = ?
		ORDER BY seq ASC, created_at ASC, id ASC`
	var rows []messageRow
	if err := s.db.SelectContext(ctx, &rows, mq, id, tenantID); err != nil {
		return Session{}, nil, fmt.Errorf("ai.SessionStore.GetSession messages: %w", err)
	}
	msgs := make([]Message, 0, len(rows))
	for _, r := range rows {
		m := Message{
			ID: r.ID, SessionID: r.SessionID, TenantID: r.TenantID,
			Seq: r.Seq, Role: r.Role, Content: r.Content, CreatedAt: r.CreatedAt,
		}
		if r.DraftJSON != nil {
			m.DraftJSON = *r.DraftJSON
		}
		if r.SettledJSON != nil {
			if err := json.Unmarshal(*r.SettledJSON, &m.Settled); err != nil {
				return Session{}, nil, fmt.Errorf("ai.SessionStore.GetSession settled_json: %w", err)
			}
		}
		msgs = append(msgs, m)
	}
	return sess, msgs, nil
}

// AppendTurn persists one whole chat turn (the new user message + the assistant reply, with its
// draft) ATOMICALLY, creating the session first when sessionID is empty. This is the ONLY write path
// into sys_ai_messages — it replaces what used to be a separate CreateSession + two AppendMessage
// calls from the handler (post-review FIX 1): a mid-turn DB failure with 3 separate transactions
// could durably leave an orphan user-turn with no assistant reply (resume would replay it as an
// unanswered message) or an empty ghost session. A single BeginTxx…Commit makes the whole turn
// all-or-nothing — there is no partial state to observe.
//
//   - sessionID == ""  → a NEW session is INSERTed inside this same transaction, owned by
//     (tenantID, userID), titled newTitle; baseSeq = 0. The generated id is returned.
//   - sessionID != ""  → the session must resolve under a USER-scoped ownership guard
//     (`id=? AND tenant_id=? AND user_id=?  AND deleted_at IS NULL`) — a foreign/guessed/deleted id
//     (including a session soft-deleted in the race window between the caller's earlier ownership
//     read and this call) returns ErrSessionNotFound with NOTHING written (tx rolled back). This is
//     stricter than the prior tenant-only guard: the store's own invariant no longer depends on the
//     caller having already checked user-ownership (post-review security note).
//     baseSeq is computed inside the SAME transaction (`MAX(seq)+1`), so it can never race a
//     concurrent append into a stale seq.
//
// Both messages (user @ baseSeq, assistant @ baseSeq+1) are inserted, session.updated_at is bumped,
// and the transaction commits. Returns the (possibly newly created) session id.
//
// Feature (mig 000059): AppendTurn is the BUILDER write path — it mints sessions with
// feature='builder' and its existing-session guard only resolves builder threads. The ⌘K
// assistant writes through AppendAssistantTurn; the two surfaces can never append onto each
// other's threads (a cross-feature id is ErrSessionNotFound, same no-leak contract as cross-user).
func (s *SessionStore) AppendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error) {
	return s.appendTurn(ctx, tenantID, userID, sessionID, newTitle, userContent, assistantContent, assistantDraft, SessionFeatureBuilder)
}

// AppendAssistantTurn is AppendTurn for the ⌘K assistant surface (feature='assistant').
func (s *SessionStore) AppendAssistantTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage) (string, error) {
	return s.appendTurn(ctx, tenantID, userID, sessionID, newTitle, userContent, assistantContent, assistantDraft, SessionFeatureAssistant)
}

// appendTurn is the one atomic turn-write body both feature wrappers share.
func (s *SessionStore) appendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage, feature string) (string, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("ai.SessionStore.AppendTurn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	now := s.now().UTC()
	baseSeq := 0

	if sessionID == "" {
		sessionID = uuid.NewString()
		const ins = `INSERT INTO sys_ai_sessions (id, tenant_id, user_id, title, feature, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, ins, sessionID, tenantID, userID, clampRunes(newTitle, maxTitleRunes), feature, now, now); err != nil {
			return "", fmt.Errorf("ai.SessionStore.AppendTurn: create session: %w", err)
		}
	} else {
		// USER-scoped ownership guard — the store no longer trusts the caller to have already
		// checked ownership; this is the single source of truth for "may this (tenant,user) write
		// this session". feature is pinned too: a ⌘K turn cannot land on a Builder thread.
		// FOR UPDATE holds the session row until commit, so two turns written at once (an MCP agent
		// calling tools in parallel, issue #3) take turns at MAX(seq)+1 instead of sharing a seq —
		// the Studio keys a card by its seq. The read below runs after the lock, on a fresh snapshot.
		var one int
		const guard = `SELECT 1 FROM sys_ai_sessions
			WHERE id = ? AND tenant_id = ? AND user_id = ? AND feature = ? AND deleted_at IS NULL FOR UPDATE`
		if err := tx.GetContext(ctx, &one, guard, sessionID, tenantID, userID, feature); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrSessionNotFound
			}
			return "", fmt.Errorf("ai.SessionStore.AppendTurn: guard: %w", err)
		}

		const seqQ = `SELECT COALESCE(MAX(seq), -1) + 1 FROM sys_ai_messages WHERE session_id = ? AND tenant_id = ?`
		if err := tx.GetContext(ctx, &baseSeq, seqQ, sessionID, tenantID); err != nil {
			return "", fmt.Errorf("ai.SessionStore.AppendTurn: next seq: %w", err)
		}
	}

	const insMsg = `INSERT INTO sys_ai_messages
		(id, session_id, tenant_id, seq, role, content, draft_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insMsg,
		uuid.NewString(), sessionID, tenantID, baseSeq, "user", userContent, nil, now,
	); err != nil {
		return "", fmt.Errorf("ai.SessionStore.AppendTurn: insert user message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, insMsg,
		uuid.NewString(), sessionID, tenantID, baseSeq+1, "assistant", assistantContent, nullableJSON(assistantDraft), now,
	); err != nil {
		return "", fmt.Errorf("ai.SessionStore.AppendTurn: insert assistant message: %w", err)
	}

	const bump = `UPDATE sys_ai_sessions SET updated_at = ? WHERE id = ? AND tenant_id = ?`
	if _, err := tx.ExecContext(ctx, bump, now, sessionID, tenantID); err != nil {
		return "", fmt.Errorf("ai.SessionStore.AppendTurn: bump: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("ai.SessionStore.AppendTurn: commit: %w", err)
	}
	return sessionID, nil
}

// SettleProposal records that the builder applied, discarded or undid the proposal card at index of
// the assistant turn seq of their own BUILDER session (issue #42): an MCP thread stops replaying it
// (PendingProposals). Settling twice is a no-op. A card that is not in a live builder session of
// (tenantID, userID) resolves to ErrProposalNotFound with nothing written.
func (s *SessionStore) SettleProposal(ctx context.Context, tenantID, userID, sessionID string, seq, index int) error {
	var n int
	const q = `SELECT COALESCE(JSON_LENGTH(m.draft_json), 0)
		FROM sys_ai_messages m JOIN sys_ai_sessions s ON s.id = m.session_id AND s.tenant_id = m.tenant_id
		WHERE m.session_id = ? AND m.tenant_id = ? AND m.seq = ? AND m.role = 'assistant'
		  AND s.user_id = ? AND s.feature = ? AND s.deleted_at IS NULL`
	err := s.db.GetContext(ctx, &n, q, sessionID, tenantID, seq, userID, SessionFeatureBuilder)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (index < 0 || index >= n)) {
		return ErrProposalNotFound
	}
	if err != nil {
		return fmt.Errorf("ai.SessionStore.SettleProposal: %w", err)
	}
	// One statement under the row lock, so two cards of the same turn settled at once both land.
	const u = `UPDATE sys_ai_messages SET settled_json = JSON_ARRAY_APPEND(COALESCE(settled_json, JSON_ARRAY()), '$', ?)
		WHERE session_id = ? AND tenant_id = ? AND seq = ? AND role = 'assistant'
		  AND NOT JSON_CONTAINS(COALESCE(settled_json, JSON_ARRAY()), ?)`
	if _, err := s.db.ExecContext(ctx, u, index, sessionID, tenantID, seq, strconv.Itoa(index)); err != nil {
		return fmt.Errorf("ai.SessionStore.SettleProposal: %w", err)
	}
	return nil
}

// PendingProposals is every proposal of a thread the builder has not settled, in thread order: what
// an MCP tool call replays into its draft so it may build on the earlier ones, as within one chat
// turn (issue #42). A card applied in the Studio is left out — the validators read it where Apply
// does (the live schema, the builder's drafts, and for a records card's "$ref" the record engine's
// ref registry) and read nothing once it is rolled back — and so is a card discarded or undone. A
// turn whose draft does not decode contributes nothing.
func PendingProposals(msgs []Message) []DraftItem {
	var out []DraftItem
	for _, m := range msgs {
		var items []DraftItem
		if len(m.DraftJSON) == 0 || json.Unmarshal(m.DraftJSON, &items) != nil {
			continue
		}
		for i, it := range items {
			if !slices.Contains(m.Settled, i) {
				out = append(out, it)
			}
		}
	}
	return out
}

// SoftDeleteSession soft-deletes a BUILDER session, ownership-guarded to (tenantID, userID). A row
// that is missing / another user's / another tenant's / another feature's / already deleted matches
// nothing → ErrSessionNotFound (same no-leak contract as GetSession). Messages are left in place (a
// removed session keeps its history behind the soft-delete marker).
func (s *SessionStore) SoftDeleteSession(ctx context.Context, tenantID, userID, id string) error {
	return s.softDeleteSession(ctx, tenantID, userID, id, SessionFeatureBuilder)
}

// SoftDeleteAssistantSession is SoftDeleteSession for the ⌘K assistant surface.
func (s *SessionStore) SoftDeleteAssistantSession(ctx context.Context, tenantID, userID, id string) error {
	return s.softDeleteSession(ctx, tenantID, userID, id, SessionFeatureAssistant)
}

// softDeleteSession is the one soft-delete body both feature wrappers share.
func (s *SessionStore) softDeleteSession(ctx context.Context, tenantID, userID, id, feature string) error {
	const q = `UPDATE sys_ai_sessions SET deleted_at = ?
		WHERE id = ? AND tenant_id = ? AND user_id = ? AND feature = ? AND deleted_at IS NULL`
	res, err := s.db.ExecContext(ctx, q, s.now().UTC(), id, tenantID, userID, feature)
	if err != nil {
		return fmt.Errorf("ai.SessionStore.SoftDeleteSession: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ai.SessionStore.SoftDeleteSession: rows affected: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// AutoTitle derives a session title from the first user message WITHOUT an LLM call (spec §11): it
// collapses whitespace/newlines to single spaces, then rune-truncates to autoTitleRunes (with an
// ellipsis when it had to cut). An empty/whitespace message yields a stable default. Exported so the
// chat handler (package handlers) can build the title for a brand-new session.
func AutoTitle(firstUserMsg string) string {
	title := strings.TrimSpace(strings.Join(strings.Fields(firstUserMsg), " "))
	if title == "" {
		return "Phiên mới"
	}
	if utf8.RuneCountInString(title) <= autoTitleRunes {
		return title
	}
	return clampRunes(title, autoTitleRunes-1) + "…"
}

// clampRunes truncates s to at most n runes (never splitting a multi-byte rune).
func clampRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// nullableJSON maps an empty/nil draft to SQL NULL and any non-empty JSON to itself, so draft_json
// stores NULL (not a literal "null"/"[]") when a turn produced no draft.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
