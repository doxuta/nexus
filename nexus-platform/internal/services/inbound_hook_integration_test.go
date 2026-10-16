//go:build integration

// CONG-TIN-HIEU integration (../specs/015-inbound-hooks/spec.md §8) on real MySQL: config-time validation
// matrix (slug/entity/field/match-not-unique), then the signed receive path end-to-end —
// create → upsert-by-unique-key → uniform 401s → record-validation 422 — with the delivery
// log capturing every outcome. The writes run the REAL RecordService (validation + tenant
// isolation + audit identical to a hand-made request).
package services

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
)

const ihTenant = "bbbbbbbb-2b2b-2b2b-2b2b-bbbbbbbbbbbb"
const ihUser = "00000000-0000-0000-0000-000000000002"
const ihSlug = "lab-hooks"

// ihResolver fakes the slug→tenant lookup (the real one is auth.UserRepository; the seam is
// one method and the interesting behavior — uniform 401 on unknown slug — is in the service).
type ihResolver struct{ tenants map[string]string }

func (r ihResolver) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	if id, ok := r.tenants[slug]; ok {
		return &auth.Tenant{ID: id, Slug: slug}, nil
	}
	return nil, auth.ErrTenantNotFound
}

func ihSetup(t *testing.T) (*InboundHookService, *RecordService, *EntityService, *inboundhook.Repository) {
	t.Helper()
	gdb, sdb := o3aSetup(t)
	metaRepo := metadata.NewRepository(gdb)
	dynRepo := dynrepo.New(sdb)
	entitySvc := NewEntityService(dynRepo, metaRepo)
	recordSvc := NewRecordService(metaRepo, dynRepo)
	store := inboundhook.New(gdb)
	svc := NewInboundHookService(store, recordSvc, ihResolver{tenants: map[string]string{ihSlug: ihTenant}}, zap.NewNop())
	return svc, recordSvc, entitySvc, store
}

func ihCreateOrdersEntity(t *testing.T, entitySvc *EntityService) {
	t.Helper()
	_, _, err := entitySvc.CreateEntity(context.Background(), ihTenant, CreateEntityInput{
		Name: "ih_orders", Label: "Orders",
		Fields: []FieldInput{
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
			{Name: "amount", Label: "Amount", FieldType: "number"},
			{Name: "status", Label: "Status", FieldType: "picklist", Options: map[string]any{"values": []any{"Chờ thu", "Đã thu"}}},
		},
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
}

func ihSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestInboundHook_ValidationMatrix(t *testing.T) {
	svc, _, entitySvc, _ := ihSetup(t)
	ihCreateOrdersEntity(t, entitySvc)
	ctx := context.Background()

	base := InboundHookInput{
		Name: "bank-in", EntityName: "ih_orders",
		Mapping: []inboundhook.Mapping{{Source: "data.code", Field: "code"}},
	}
	cases := []struct {
		name  string
		mut   func(InboundHookInput) InboundHookInput
		field string
	}{
		{"bad slug name", func(in InboundHookInput) InboundHookInput { in.Name = "Có Dấu!"; return in }, "name"},
		{"unknown entity", func(in InboundHookInput) InboundHookInput { in.EntityName = "nope"; return in }, "entityName"},
		{"empty mapping", func(in InboundHookInput) InboundHookInput { in.Mapping = nil; return in }, "mapping"},
		{"unknown mapping field", func(in InboundHookInput) InboundHookInput {
			in.Mapping = []inboundhook.Mapping{{Source: "x", Field: "ghost"}}
			return in
		}, "ghost"},
		{"match field not unique", func(in InboundHookInput) InboundHookInput { in.MatchField = "amount"; return in }, "matchField"},
		{"match field unknown", func(in InboundHookInput) InboundHookInput { in.MatchField = "ghost"; return in }, "matchField"},
		// Review W2: a match field the mapping never feeds silently defeats upsert.
		{"match field not fed by mapping", func(in InboundHookInput) InboundHookInput {
			in.Mapping = []inboundhook.Mapping{{Source: "data.amount", Field: "amount"}}
			in.MatchField = "code"
			return in
		}, "matchField"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, ihTenant, ihUser, tc.mut(base))
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "HOOK_INVALID" || ve.Details["field"] != tc.field {
				t.Fatalf("want HOOK_INVALID on %s, got %v", tc.field, err)
			}
		})
	}
}

func TestInboundHook_ReceiveUpsertE2E(t *testing.T) {
	svc, recordSvc, entitySvc, _ := ihSetup(t)
	ihCreateOrdersEntity(t, entitySvc)
	ctx := context.Background()

	hook, err := svc.Create(ctx, ihTenant, ihUser, InboundHookInput{
		Name: "bank-in", EntityName: "ih_orders",
		Mapping: []inboundhook.Mapping{
			{Source: "data.code", Field: "code"},
			{Source: "data.amount", Field: "amount"},
			{Source: "data.status", Field: "status"},
		},
		MatchField: "code",
	})
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}
	if len(hook.Secret) != 52 || hook.Secret[:4] != "whk_" {
		t.Fatalf("minted secret shape: %q", hook.Secret)
	}

	adminScope := RecordScope{UserID: ihUser, AccessLevel: "all", IsAdmin: true}
	countRecords := func() int {
		lr, err := recordSvc.List(ctx, adminScope, ihTenant, "ih_orders", ListQuery{Limit: 50})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return len(lr.Records)
	}

	// 1) First signal → created.
	body1 := []byte(`{"data":{"code":"DH-0042","amount":150000,"status":"Chờ thu"}}`)
	res, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign(hook.Secret, body1), body1)
	if err != nil || res.Status != "created" || res.RecordID == "" {
		t.Fatalf("first receive = %+v, %v", res, err)
	}
	if countRecords() != 1 {
		t.Fatal("record not created")
	}

	// 2) Same unique code again → updated (not a duplicate).
	body2 := []byte(`{"data":{"code":"DH-0042","amount":150000,"status":"Đã thu"}}`)
	res2, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign(hook.Secret, body2), body2)
	if err != nil || res2.Status != "updated" || res2.RecordID != res.RecordID {
		t.Fatalf("second receive = %+v, %v", res2, err)
	}
	if countRecords() != 1 {
		t.Fatal("upsert duplicated the record")
	}
	got, err := recordSvc.Get(ctx, adminScope, ihTenant, "ih_orders", res.RecordID, nil, nil)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["status"] != "Đã thu" {
		t.Fatalf("status after update = %v", got.Data["status"])
	}

	// 3) Uniform 401s: bad sig / unknown hook / unknown tenant / disabled hook — no writes.
	if _, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign("wrong", body1), body1); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("bad sig: %v", err)
	}
	if _, err := svc.Receive(ctx, ihSlug, "ghost", ihSign(hook.Secret, body1), body1); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("unknown hook: %v", err)
	}
	if _, err := svc.Receive(ctx, "ghost-tenant", "bank-in", ihSign(hook.Secret, body1), body1); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("unknown tenant: %v", err)
	}
	off := false
	if _, err := svc.Update(ctx, ihTenant, hook.ID, InboundHookInput{
		Name: "bank-in", EntityName: "ih_orders",
		Mapping:    []inboundhook.Mapping{{Source: "data.code", Field: "code"}},
		MatchField: "code", IsActive: &off,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign(hook.Secret, body1), body1); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("disabled hook: %v", err)
	}
	if countRecords() != 1 {
		t.Fatal("a rejected signal wrote a record")
	}

	// 4) Re-enable; a payload failing record validation → real 422 + delivery log "error".
	on := true
	if _, err := svc.Update(ctx, ihTenant, hook.ID, InboundHookInput{
		Name: "bank-in", EntityName: "ih_orders",
		Mapping: []inboundhook.Mapping{
			{Source: "data.code", Field: "code"},
			{Source: "data.status", Field: "status"},
		},
		MatchField: "code", IsActive: &on,
	}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	bad := []byte(`{"data":{"code":"DH-0099","status":"KhongCoTrongPicklist"}}`)
	_, err = svc.Receive(ctx, ihSlug, "bank-in", ihSign(hook.Secret, bad), bad)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_INVALID_PICKLIST" {
		t.Fatalf("bad payload: want RECORD_INVALID_PICKLIST, got %v", err)
	}

	// 5) Rotate: old signature dies, new one works.
	rotated, err := svc.RotateSecret(ctx, ihTenant, hook.ID)
	if err != nil || rotated.Secret == hook.Secret {
		t.Fatalf("rotate: %v (same secret? %v)", err, rotated.Secret == hook.Secret)
	}
	if _, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign(hook.Secret, body1), body1); !errors.Is(err, ErrHookUnauthorized) {
		t.Fatalf("old secret after rotate: %v", err)
	}
	body3 := []byte(`{"data":{"code":"DH-0100","status":"Chờ thu"}}`)
	if res3, err := svc.Receive(ctx, ihSlug, "bank-in", ihSign(rotated.Secret, body3), body3); err != nil || res3.Status != "created" {
		t.Fatalf("new secret after rotate: %+v, %v", res3, err)
	}

	// 6) Delivery log captured the outcomes (created + updated + error + created).
	deliveries, err := svc.ListDeliveries(ctx, ihTenant, hook.ID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	byStatus := map[string]int{}
	for _, d := range deliveries {
		byStatus[d.Status]++
	}
	if byStatus["created"] < 2 || byStatus["updated"] < 1 || byStatus["error"] < 1 {
		t.Fatalf("delivery statuses = %v", byStatus)
	}

	// 7) Soft-delete tombstones the slug (review W3) — the same name is creatable again.
	if err := svc.Delete(ctx, ihTenant, hook.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Create(ctx, ihTenant, ihUser, InboundHookInput{
		Name: "bank-in", EntityName: "ih_orders",
		Mapping: []inboundhook.Mapping{{Source: "data.code", Field: "code"}}, MatchField: "code",
	}); err != nil {
		t.Fatalf("re-create after delete must succeed (tombstoned slug), got: %v", err)
	}
}

// PAYMENT-MATCH (DONG-TIEN p2, ../specs/016-payment-flow/spec.md §8) — the closed loop on real MySQL: a bank
// credit signal lands as a transaction record AND flips the referenced invoice's status;
// misses (no ref in memo / unknown ref / amount mismatch) become hộp-chờ rows, never request
// failures. Config-time matrix guards the same field-level contract as the base hook.
func TestInboundHook_PaymentMatchE2E(t *testing.T) {
	svc, recordSvc, entitySvc, _ := ihSetup(t)
	ctx := context.Background()

	// Invoices (target) + bank transactions (hook entity).
	if _, _, err := entitySvc.CreateEntity(ctx, ihTenant, CreateEntityInput{
		Name: "ih_invoices", Label: "Invoices",
		Fields: []FieldInput{
			{Name: "code", Label: "Code", FieldType: "text", IsUnique: true},
			{Name: "total", Label: "Total", FieldType: "number"},
			{Name: "status", Label: "Status", FieldType: "picklist", Options: map[string]any{"values": []any{"Chờ thu", "Đã thu"}}},
		},
	}); err != nil {
		t.Fatalf("create invoices: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, ihTenant, CreateEntityInput{
		Name: "ih_bank_txns", Label: "Bank Txns",
		Fields: []FieldInput{
			{Name: "memo", Label: "Memo", FieldType: "text"},
			{Name: "amount", Label: "Amount", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("create txns: %v", err)
	}

	adminScope := RecordScope{UserID: ihUser, AccessLevel: "all", IsAdmin: true}
	invID, err := recordSvc.Create(ctx, adminScope, ihTenant, ihUser, "ih_invoices",
		map[string]any{"code": "INV-0042", "total": float64(150000), "status": "Chờ thu"})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	pm := &inboundhook.PaymentMatch{
		TargetEntity: "ih_invoices", RefField: "code", RefPattern: `INV-\d+`,
		MemoSource: "data.desc", StatusField: "status", StatusValue: "Đã thu",
		AmountSource: "data.amount", AmountField: "total",
	}
	hook, err := svc.Create(ctx, ihTenant, ihUser, InboundHookInput{
		Name: "casso-in", EntityName: "ih_bank_txns",
		Mapping: []inboundhook.Mapping{
			{Source: "data.desc", Field: "memo"},
			{Source: "data.amount", Field: "amount"},
		},
		PaymentMatch: pm,
	})
	if err != nil {
		t.Fatalf("create hook with match: %v", err)
	}

	send := func(body string) *ReceiveResult {
		t.Helper()
		res, err := svc.Receive(ctx, ihSlug, "casso-in", ihSign(hook.Secret, []byte(body)), []byte(body))
		if err != nil {
			t.Fatalf("receive %s: %v", body, err)
		}
		return res
	}

	// 1) Matched: memo carries INV-0042, amount equals total → invoice flips to Đã thu.
	send(`{"data":{"desc":"CK TT INV-0042 tu VCB","amount":150000}}`)
	inv, err := recordSvc.Get(ctx, adminScope, ihTenant, "ih_invoices", invID, nil, nil)
	if err != nil || inv.Data["status"] != "Đã thu" {
		t.Fatalf("invoice after match: %v %v", inv.Data["status"], err)
	}

	// 1b) Double payment: the SAME settled ref again → hộp chờ (khả năng thu trùng), no re-fire.
	send(`{"data":{"desc":"CK TT INV-0042 lan 2","amount":150000}}`)

	// 2) Unmatched: unknown ref → transaction still recorded, hộp chờ row carries the reason.
	send(`{"data":{"desc":"CK TT INV-9999","amount":1}}`)
	// 3) Unmatched: amount mismatch (reset invoice first so a blind match would be visible).
	if err := recordSvc.Update(ctx, adminScope, ihTenant, "ih_invoices", invID,
		map[string]any{"status": "Chờ thu"}, nil); err != nil {
		t.Fatalf("reset: %v", err)
	}
	send(`{"data":{"desc":"CK TT INV-0042","amount":99}}`)
	inv, _ = recordSvc.Get(ctx, adminScope, ihTenant, "ih_invoices", invID, nil, nil)
	if inv.Data["status"] != "Chờ thu" {
		t.Fatal("amount mismatch must NOT flip the invoice")
	}

	deliveries, err := svc.ListDeliveries(ctx, ihTenant, hook.ID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	counts := map[string]int{}
	for _, d := range deliveries {
		if d.MatchStatus != nil {
			counts[*d.MatchStatus]++
		}
		if d.MatchStatus != nil && *d.MatchStatus == "matched" && (d.MatchRecordID == nil || *d.MatchRecordID != invID) {
			t.Fatalf("matched delivery must carry the target id, got %v", d.MatchRecordID)
		}
	}
	if counts["matched"] != 1 || counts["unmatched"] != 3 {
		t.Fatalf("match outcomes = %v, want 1 matched / 3 unmatched (incl. thu trùng)", counts)
	}

	// 4) Config matrix: bad regex / non-unique ref / half an amount pair all die at save time.
	for _, tc := range []struct {
		name  string
		mut   func(inboundhook.PaymentMatch) inboundhook.PaymentMatch
		field string
	}{
		{"bad regex", func(p inboundhook.PaymentMatch) inboundhook.PaymentMatch { p.RefPattern = "("; return p }, "paymentMatch.refPattern"},
		{"ref not unique", func(p inboundhook.PaymentMatch) inboundhook.PaymentMatch { p.RefField = "total"; return p }, "paymentMatch.refField"},
		{"half amount pair", func(p inboundhook.PaymentMatch) inboundhook.PaymentMatch { p.AmountField = ""; return p }, "paymentMatch.amountField"},
		{"unknown status field", func(p inboundhook.PaymentMatch) inboundhook.PaymentMatch { p.StatusField = "ghost"; return p }, "paymentMatch.statusField"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := tc.mut(*pm)
			_, err := svc.Create(ctx, ihTenant, ihUser, InboundHookInput{
				Name: "x-" + strings.ReplaceAll(tc.name, " ", "-"), EntityName: "ih_bank_txns",
				Mapping:      []inboundhook.Mapping{{Source: "data.desc", Field: "memo"}},
				PaymentMatch: &bad,
			})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "HOOK_INVALID" || ve.Details["field"] != tc.field {
				t.Fatalf("want HOOK_INVALID on %s, got %v", tc.field, err)
			}
		})
	}
}

// Tiền từ hệ thống NGOÀI đến dưới dạng số thuần — engine phải tự quy đổi (2026-08-02).
//
// Tìm ra khi chạy vòng tiền thật qua HTTP lần đầu: ngân hàng gửi {"amount": 150000000} và cú ghi
// chết ngay với 422 RECORD_INVALID_TYPE vì field `currency` của engine đòi {amount, currencyCode}.
// Bắt một đối tác nói đúng hình dạng NỘI BỘ của mình thì không gọi là tích hợp — mọi tích hợp có
// field tiền sẽ chết ở dòng đầu tiên. Coercion nằm ở tầng mapping của hook, đúng chỗ dữ liệu lạ
// bước vào.
func TestInboundHook_CoercesScalarAmountIntoCurrency(t *testing.T) {
	svc, recordSvc, entitySvc, _ := ihSetup(t)
	ctx := context.Background()

	if _, _, err := entitySvc.CreateEntity(ctx, ihTenant, CreateEntityInput{
		Name: "ih_txn_cur", Label: "Txns",
		Fields: []FieldInput{
			{Name: "memo", Label: "Memo", FieldType: "text"},
			{Name: "amount", Label: "Amount", FieldType: "currency", Options: map[string]any{"currencyCode": "VND"}},
		},
	}); err != nil {
		t.Fatalf("create entity: %v", err)
	}

	hook, err := svc.Create(ctx, ihTenant, ihUser, InboundHookInput{
		Name: "bank-scalar", EntityName: "ih_txn_cur",
		Mapping: []inboundhook.Mapping{
			{Source: "desc", Field: "memo"},
			{Source: "amount", Field: "amount"},
		},
	})
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}

	// Đúng hình dạng một feed ngân hàng thật gửi: số thuần, không object.
	body := `{"desc":"CK thanh toan","amount":150000000}`
	res, err := svc.Receive(ctx, ihSlug, "bank-scalar", ihSign(hook.Secret, []byte(body)), []byte(body))
	if err != nil {
		t.Fatalf("Receive với số tiền dạng scalar phải thành công, không phải 422: %v", err)
	}

	adminScope := RecordScope{UserID: ihUser, AccessLevel: "all", IsAdmin: true}
	rec, err := recordSvc.Get(ctx, adminScope, ihTenant, "ih_txn_cur", res.RecordID, nil, nil)
	if err != nil {
		t.Fatalf("đọc lại bản ghi: %v", err)
	}
	got, ok := rec.Data["amount"].(map[string]any)
	if !ok {
		t.Fatalf("amount lưu dạng %T (%v); muốn object {amount,currencyCode}", rec.Data["amount"], rec.Data["amount"])
	}
	// So bằng SỐ, không bằng chuỗi: engine lưu tiền dạng float64 nên fmt.Sprint ra "1.5e+08".
	if n, ok := got["amount"].(float64); !ok || n != 150000000 {
		t.Errorf("amount = %v (%T); muốn 150000000", got["amount"], got["amount"])
	}
	// currencyCode phải lấy từ CẤU HÌNH FIELD, không phải hằng số đoán bừa.
	if got["currencyCode"] != "VND" {
		t.Errorf("currencyCode = %v; muốn VND (đọc từ options của field)", got["currencyCode"])
	}

	// Chuỗi số cũng nhận (nhiều API ngân hàng báo tiền dạng chuỗi).
	body2 := `{"desc":"CK lan hai","amount":"250000"}`
	res2, err := svc.Receive(ctx, ihSlug, "bank-scalar", ihSign(hook.Secret, []byte(body2)), []byte(body2))
	if err != nil {
		t.Fatalf("Receive với số tiền dạng chuỗi: %v", err)
	}
	rec2, _ := recordSvc.Get(ctx, adminScope, ihTenant, "ih_txn_cur", res2.RecordID, nil, nil)
	m, ok := rec2.Data["amount"].(map[string]any)
	if !ok {
		t.Fatalf("chuỗi số phải quy đổi thành object; nhận %v (%T)", rec2.Data["amount"], rec2.Data["amount"])
	}
	if n, ok := m["amount"].(float64); !ok || n != 250000 {
		t.Errorf("amount = %v; muốn 250000", m["amount"])
	}
}
