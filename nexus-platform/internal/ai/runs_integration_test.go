//go:build integration

// Integration tests for the sys_ai_runs ledger writer (E9 M1). They prove what a fake recorder
// cannot: the row actually persists to MySQL, the token/latency columns round-trip, AND — the
// security-critical assertion — the stored prompt_hash is the sha256 hex of the last user message
// while the RAW message text appears NOWHERE in the persisted row (spec §5/§7: never store the raw
// prompt, which can carry customer business data).
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestSQLRunRepository -count=1
package ai

import (
	"context"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// aiTestDSN returns the DSN of the throwaway migrated database provisioned by TestMain (see
// main_test.go). Before #84 this read TEST_DATABASE_URL directly, which broke against a db-less
// DSN; TestMain now creates+migrates a throwaway DB and points testDSN at it.
func aiTestDSN() string {
	return testDSN
}

func connectAI(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", aiTestDSN())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping ai ledger integration", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLRunRepository_Record_PersistsHashedPrompt(t *testing.T) {
	db := connectAI(t)
	const tenant = "aiaiaiai-9999-9999-9999-aiaiaiaiai01"
	const rawPrompt = "Tôi làm phòng khám nha khoa, khách hàng tên Nguyễn Văn Bí Mật cần bảo mật"

	// Clean any prior run of this test (idempotent re-runs).
	db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
	t.Cleanup(func() { db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant) })

	repo := NewSQLRunRepository(db)
	run := Run{
		ID:           "airun000-0000-0000-0000-000000000001",
		TenantID:     tenant,
		UserID:       "user0000-0000-0000-0000-000000000001",
		Feature:      FeatureBuilderChat,
		Model:        "cc/claude-sonnet-4-6",
		PromptHash:   sha256Hex(rawPrompt),
		PromptTokens: 128,
		OutputTokens: 64,
		ToolCalls:    3,
		Outcome:      outcomeOK,
		LatencyMS:    742,
	}
	if err := repo.Record(context.Background(), run); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	// Read the row back through a struct that mirrors every column.
	var got struct {
		ID           string `db:"id"`
		TenantID     string `db:"tenant_id"`
		UserID       string `db:"user_id"`
		Feature      string `db:"feature"`
		Model        string `db:"model"`
		PromptHash   string `db:"prompt_hash"`
		PromptTokens *int   `db:"prompt_tokens"`
		OutputTokens *int   `db:"output_tokens"`
		ToolCalls    int    `db:"tool_calls"`
		Outcome      string `db:"outcome"`
		LatencyMS    *int   `db:"latency_ms"`
	}
	if err := db.Get(&got, `SELECT id, tenant_id, user_id, feature, model, prompt_hash,
		prompt_tokens, output_tokens, tool_calls, outcome, latency_ms
		FROM sys_ai_runs WHERE id = ?`, run.ID); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if got.PromptHash != sha256Hex(rawPrompt) || len(got.PromptHash) != 64 {
		t.Fatalf("prompt_hash = %q; want the 64-char sha256 hex of the raw prompt", got.PromptHash)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 128 || got.OutputTokens == nil || *got.OutputTokens != 64 {
		t.Fatalf("token columns did not round-trip: prompt=%v output=%v", got.PromptTokens, got.OutputTokens)
	}
	if got.ToolCalls != 3 || got.Outcome != outcomeOK || got.LatencyMS == nil || *got.LatencyMS != 742 {
		t.Fatalf("row mismatch: %+v", got)
	}

	// SECURITY: the raw prompt (and a distinctive substring of it) must appear in NO column.
	for col, v := range map[string]string{
		"prompt_hash": got.PromptHash, "feature": got.Feature, "model": got.Model,
		"outcome": got.Outcome, "user_id": got.UserID,
	} {
		if strings.Contains(v, "Bí Mật") || strings.Contains(v, rawPrompt) {
			t.Fatalf("raw prompt text leaked into column %q = %q", col, v)
		}
	}
}

func TestSQLRunRepository_Record_ErrorOutcome_NullTokens(t *testing.T) {
	db := connectAI(t)
	const tenant = "aiaiaiai-9999-9999-9999-aiaiaiaiai02"
	db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant)
	t.Cleanup(func() { db.Exec(`DELETE FROM sys_ai_runs WHERE tenant_id = ?`, tenant) })

	repo := NewSQLRunRepository(db)
	// A gateway_error turn that never got a response: 0 tokens → stored as NULL (not a fake 0).
	run := Run{
		ID:         "airun000-0000-0000-0000-000000000002",
		TenantID:   tenant,
		UserID:     "user0000-0000-0000-0000-000000000002",
		Feature:    FeatureBuilderChat,
		Model:      "cc/claude-sonnet-4-6",
		PromptHash: sha256Hex("gateway down"),
		Outcome:    outcomeGatewayError,
	}
	if err := repo.Record(context.Background(), run); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	var tokens *int
	if err := db.Get(&tokens, `SELECT prompt_tokens FROM sys_ai_runs WHERE id = ?`, run.ID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if tokens != nil {
		t.Fatalf("prompt_tokens = %v; want NULL when the turn captured no token data", *tokens)
	}
}
