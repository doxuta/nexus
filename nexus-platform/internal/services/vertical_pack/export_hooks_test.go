package vertical_pack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/services"
)

// fakeHookWriter serves ListAdmin for the export path. Create is unused here (export never writes)
// but must exist to satisfy inboundHookWriter.
type fakeHookWriter struct{ existing []inboundhook.Hook }

func (f *fakeHookWriter) ListAdmin(context.Context, string) ([]inboundhook.Hook, error) {
	return f.existing, nil
}

func (f *fakeHookWriter) Create(context.Context, string, string, services.InboundHookInput) (*inboundhook.Hook, error) {
	return nil, nil
}

func strp(s string) *string { return &s }

// TestExport_Hooks pins the three properties that decide whether a configured tenant survives being
// packaged: the hook travels, its SECRET does not, and a half-portable hook says so out loud.
func TestExport_Hooks(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	for _, n := range []string{"hx_txns", "hx_quotes"} {
		if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: n, Label: n}); err != nil {
			t.Fatalf("seed entity %s: %v", n, err)
		}
	}

	hw := &fakeHookWriter{existing: []inboundhook.Hook{
		{
			ID: "h1", Name: "bank-credit", EntityName: "hx_txns",
			Secret:     "live-signing-key-do-not-ship",
			MatchField: strp("txn_ref"),
			Mapping: []inboundhook.Mapping{
				{Source: "desc", Field: "memo"},
				{Source: "amount", Field: "amount"},
				{Source: "ref", Field: "txn_ref"},
			},
			PaymentMatch: &inboundhook.PaymentMatch{
				TargetEntity: "hx_quotes", RefField: "quote_number", RefPattern: "QUO-[0-9]{5}",
				MemoSource: "desc", StatusField: "status", StatusValue: "Đã thu",
				AmountSource: "amount", AmountField: "total",
			},
			IsActive: true,
		},
		{
			// Landing entity IS selected, payment target is NOT → exported, but warned.
			ID: "h2", Name: "half-portable", EntityName: "hx_txns", Secret: "s2",
			Mapping:      []inboundhook.Mapping{{Source: "ref", Field: "txn_ref"}},
			PaymentMatch: &inboundhook.PaymentMatch{TargetEntity: "hx_elsewhere", RefField: "no", RefPattern: "X"},
			IsActive:     false,
		},
		{
			// Landing entity is NOT selected → dropped entirely (would install pointing at nothing).
			ID: "h3", Name: "foreign", EntityName: "hx_other", Secret: "s3",
			Mapping: []inboundhook.Mapping{{Source: "a", Field: "b"}}, IsActive: true,
		},
	}}

	svc := NewService(Deps{
		EntityReader:      meta,
		LayoutReader:      fakeApLayouts{},
		RoleReader:        fakeApRoleReader{},
		InboundHookWriter: hw,
	})

	pack, warnings, err := svc.Export(ctx, "tenant-1", []string{"hx_txns", "hx_quotes"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	if len(pack.InboundHooks) != 2 {
		t.Fatalf("InboundHooks = %+v, want the two hooks landing on a selected entity", pack.InboundHooks)
	}
	// Sorted by name: "bank-credit" < "half-portable".
	got := pack.InboundHooks[0]
	if got.Name != "bank-credit" || got.EntityName != "hx_txns" || got.MatchField != "txn_ref" {
		t.Errorf("exported hook = %+v, want name/entity/matchField carried", got)
	}
	if len(got.Mapping) != 3 || got.Mapping[0].Source != "desc" || got.Mapping[0].Field != "memo" {
		t.Errorf("mapping = %+v, want all three source→field pairs in order", got.Mapping)
	}
	if got.PaymentMatch == nil || got.PaymentMatch.TargetEntity != "hx_quotes" ||
		got.PaymentMatch.RefPattern != "QUO-[0-9]{5}" || got.PaymentMatch.StatusValue != "Đã thu" ||
		got.PaymentMatch.AmountField != "total" {
		t.Errorf("paymentMatch = %+v, want every matching field carried", got.PaymentMatch)
	}
	if got.IsActive == nil || !*got.IsActive {
		t.Errorf("IsActive = %v, want the live hook to arrive live", got.IsActive)
	}
	if pack.InboundHooks[1].IsActive == nil || *pack.InboundHooks[1].IsActive {
		t.Errorf("a paused hook must not come back live at the customer")
	}

	// THE invariant: no serialization of this pack may contain a secret, by any field name. Checking
	// the struct field would be circular (PackInboundHook has none by construction) — so check the
	// bytes that actually travel and land in sys_pack_items.
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatalf("marshal pack: %v", err)
	}
	for _, secret := range []string{"live-signing-key-do-not-ship", "s2", "s3"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("exported pack leaks a hook secret (%q) — install must mint its own", secret)
		}
	}

	var hasTargetWarn, hasDropWarn bool
	for _, w := range warnings {
		if strings.Contains(w, "half-portable") && strings.Contains(w, "hx_elsewhere") {
			hasTargetWarn = true
		}
		if strings.Contains(w, "Bỏ 1 hook") {
			hasDropWarn = true
		}
	}
	if !hasTargetWarn {
		t.Errorf("warnings = %v, want the out-of-selection payment target called out", warnings)
	}
	if !hasDropWarn {
		t.Errorf("warnings = %v, want the dropped foreign-entity hook counted", warnings)
	}
}
