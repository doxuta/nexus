package services

// publish_service_test.go — specs/063-draft-publish T026 (unit part): who may call, what is refused
// before any SQL, and how errors map. The transactional behaviour (conflict, invalid, rollback,
// races) is in publish_integration_test.go against a real MySQL.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"nexus/platform/internal/metaversion"
)

// nilDepsPublishService has no DB: every case below must be answered BEFORE any SQL.
func nilDepsPublishService() *PublishService {
	return NewPublishService(nil, nil, nil, metaversion.NewRegistry(NewWorkflowDocAdapter()), NewDraftGate(&fakeDelivered{}), nil, nil)
}

const goodHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPublishService_OnlyTheBuilderOfTheTenantMayCall(t *testing.T) {
	s := nilDepsPublishService()
	callers := map[string]context.Context{
		"no caller (route without DraftCaller)": context.Background(),
		"legacy customer admin":                 legacyAdminCtx(),
		"empty caller (no Subject)":             metaversion.WithCaller(context.Background(), metaversion.Caller{}),
		"builder of ANOTHER tenant":             metaversion.WithCaller(context.Background(), metaversion.Caller{UserID: "b", TenantID: "other", IsBuilder: true}),
	}
	for name, ctx := range callers {
		t.Run(name, func(t *testing.T) {
			calls := map[string]func() error{
				"State":      func() error { _, err := s.State(ctx, dwTenant); return err },
				"Changes":    func() error { _, _, err := s.Changes(ctx, dwTenant); return err },
				"Baseline":   func() error { _, err := s.Baseline(ctx, dwTenant, ""); return err },
				"Publish":    func() error { _, err := s.Publish(ctx, dwTenant, PublishRequest{ChangesetHash: goodHash}); return err },
				"Discard":    func() error { return s.Discard(ctx, dwTenant, "00000000-0000-0000-0000-000000000001") },
				"DiscardAll": func() error { return s.DiscardAll(ctx, dwTenant, goodHash) },
			}
			for op, call := range calls {
				wantCode(t, call(), "FORBIDDEN")
				_ = op
			}
		})
	}
}

// Input is judged at the boundary, before any query: a long note would be MySQL 1406 ⇒ 500, and a
// hash of the wrong shape can never match.
func TestPublishService_InputValidatedBeforeSQL(t *testing.T) {
	s := nilDepsPublishService()
	ctx := builderCtx()
	long := strings.Repeat("ế", maxPublishNoteRunes+1) // runes, not bytes
	cases := []struct {
		name string
		call func() error
	}{
		{"note over 500 runes on publish", func() error {
			_, err := s.Publish(ctx, dwTenant, PublishRequest{ChangesetHash: goodHash, Note: long})
			return err
		}},
		{"note over 500 runes on baseline", func() error { _, err := s.Baseline(ctx, dwTenant, long); return err }},
		{"hash too short", func() error { _, err := s.Publish(ctx, dwTenant, PublishRequest{ChangesetHash: "abc"}); return err }},
		{"hash with non-hex", func() error {
			_, err := s.Publish(ctx, dwTenant, PublishRequest{ChangesetHash: strings.Repeat("z", 64)})
			return err
		}},
		{"hash upper-case", func() error {
			_, err := s.Publish(ctx, dwTenant, PublishRequest{ChangesetHash: strings.Repeat("A", 64)})
			return err
		}},
		{"discard-all hash empty", func() error { return s.DiscardAll(ctx, dwTenant, "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wantCode(t, tc.call(), "INVALID_BODY") })
	}
	// Exactly 500 runes is fine for the note check itself.
	if err := s.validateNote(strings.Repeat("ế", maxPublishNoteRunes)); err != nil {
		t.Fatalf("500 runes refused: %v", err)
	}
}

func TestPublishService_FailMapsLockTimeoutsToBusy(t *testing.T) {
	s := nilDepsPublishService()
	for _, n := range []uint16{1213, 1205} {
		wantCode(t, s.fail("Publish", &mysql.MySQLError{Number: n, Message: "x"}), "PUBLISH_BUSY")
	}
	other := s.fail("Publish", &mysql.MySQLError{Number: 1062, Message: "dup"})
	var ve *ValidationError
	if errors.As(other, &ve) {
		t.Fatalf("a duplicate-key error became a ValidationError %q", ve.Code)
	}
	if !strings.Contains(other.Error(), "services.PublishService.Publish") {
		t.Fatalf("error not wrapped with its origin: %v", other)
	}
	// A ValidationError passes through untouched (DRAFTS_CHANGED etc).
	in := invalid("DRAFTS_CHANGED", "x")
	if s.fail("Publish", in) != in {
		t.Fatal("a ValidationError was rewrapped")
	}
}

func TestChangeOpAndConflict(t *testing.T) {
	hash := func(doc string) *string {
		h, _ := metaversion.DocHash([]byte(doc))
		return &h
	}
	live := []byte(`{"name":"a"}`)
	cases := []struct {
		name      string
		draft     metaversion.DraftRow
		live      []byte
		exists    bool
		wantOp    string
		wantConfl bool
	}{
		{"edit of an unchanged rule", metaversion.DraftRow{Op: "upsert", BaseHash: hash(`{"name":"a"}`)}, live, true, "update", false},
		{"edit after the customer admin changed it", metaversion.DraftRow{Op: "upsert", BaseHash: hash(`{"name":"OLD"}`)}, live, true, "update", true},
		{"edit after the customer admin deleted it", metaversion.DraftRow{Op: "upsert", BaseHash: hash(`{"name":"a"}`)}, nil, false, "update", true},
		{"brand-new rule", metaversion.DraftRow{Op: "upsert"}, nil, false, "create", false},
		{"brand-new rule whose id appeared live", metaversion.DraftRow{Op: "upsert"}, live, true, "create", true},
		{"delete of an unchanged rule", metaversion.DraftRow{Op: "delete", BaseHash: hash(`{"name":"a"}`)}, live, true, "delete", false},
		{"delete of a rule changed meanwhile", metaversion.DraftRow{Op: "delete", BaseHash: hash(`{"name":"OLD"}`)}, live, true, "delete", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := changeOp(tc.draft); got != tc.wantOp {
				t.Errorf("changeOp = %q, want %q", got, tc.wantOp)
			}
			got, err := draftConflicts(tc.draft, tc.live, tc.exists)
			if err != nil || got != tc.wantConfl {
				t.Errorf("draftConflicts = %v, %v; want %v", got, err, tc.wantConfl)
			}
		})
	}
}
