package services

// sample_wipe_test.go — specs/064-preview-as-role-sample-data S9-7 (T111).
// SampleDataService.Wipe over in-memory fakes: the service reads the root list ONCE, compares it with
// the confirmed total, builds the Pinned set from relation defaults, walks tables child-first through
// RecordService.PurgeSample (here a scripted fake) and maps the repository's refusals to `kept`.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// wipeStore adds the wipe reads/writes to the shared sample-store fake.
type wipeStore struct {
	fakeSampleStore
	roots   map[string][]dynrepo.SampleRootRow // tenant|entity -> rows
	reset   map[string]bool                    // entity.field -> ResetSequenceIfEmpty answer
	resets  []string                           // entity.field asked
	rootTen []string
}

func (s *wipeStore) SampleRoots(_ context.Context, tenantID, entity string) ([]dynrepo.SampleRootRow, error) {
	s.rootTen = append(s.rootTen, tenantID)
	return s.roots[tenantID+"|"+entity], nil
}

func (s *wipeStore) ResetSequenceIfEmpty(_ context.Context, tenantID, entity, field string) (bool, error) {
	s.rootTen = append(s.rootTen, tenantID)
	s.resets = append(s.resets, entity+"."+field)
	return s.reset[entity+"."+field], nil
}

// wipeLister lists a genCatalog's live entities (the Pinned scan covers the whole tenant).
type wipeLister genCatalog

func (l wipeLister) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	var out []metadata.EntityDefinition
	for key, label := range l.entities {
		if t, name, _ := strings.Cut(key, "|"); t == tenantID {
			out = append(out, metadata.EntityDefinition{ID: "id-" + name, TenantID: t, Name: name, Label: label})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type wipeCall struct {
	tenant, entity, id string
	pinned             map[string]dynrepo.PurgePin
	scope              RecordScope
}

// wipePurger answers PurgeSample from a per-root script (consumed in order; empty = success of the
// root alone). extra lists the other rows a successful purge took along (a required child).
type wipePurger struct {
	calls  []wipeCall
	script map[string][]error
	extra  map[string][]PurgedRecordRef
}

func (p *wipePurger) PurgeSample(_ context.Context, scope RecordScope, tenantID, entity, id string, pinned map[string]dynrepo.PurgePin) (*RecordPurgeReport, error) {
	p.calls = append(p.calls, wipeCall{tenantID, entity, id, pinned, scope})
	key := entity + "/" + id
	if steps := p.script[key]; len(steps) > 0 {
		err := steps[0]
		p.script[key] = steps[1:]
		if err != nil {
			return nil, err
		}
	}
	rep := &RecordPurgeReport{Entity: entity, ID: id, Records: []PurgedRecordRef{{Entity: entity, ID: id}}, Cleared: []PurgeClearedRef{}, Blocked: []dynrepo.PurgeBlocker{}}
	rep.Records = append(rep.Records, p.extra[key]...)
	if entity == "customers" {
		rep.BlobsRemoved = 1 // one attachment per customer row
	}
	return rep, nil
}

func (p *wipePurger) order() []string {
	var out []string
	for _, c := range p.calls {
		out = append(out, c.entity+"/"+c.id)
	}
	return out
}

func rootsOf(ids ...string) []dynrepo.SampleRootRow {
	var out []dynrepo.SampleRootRow
	for _, id := range ids {
		out = append(out, dynrepo.SampleRootRow{ID: id, BatchID: "batch-" + id[:1]})
	}
	return out
}

func intp(n int) *int { return &n }

// wipeFixture: t1 has customers c1,c2 · appointments a1 (required customer) · services s1,s2 (auto_number code).
func wipeFixture(t *testing.T, cat genCatalog) (*SampleDataService, *wipeStore, *wipePurger, *spyAuditor) {
	t.Helper()
	store := &wipeStore{
		fakeSampleStore: fakeSampleStore{names: map[string][]string{"t1": {"appointments", "customers", "services", "gone_entity"}}},
		roots: map[string][]dynrepo.SampleRootRow{
			"t1|customers": rootsOf("c1", "c2"), "t1|appointments": rootsOf("a1"), "t1|services": rootsOf("s1", "s2"),
			"t2|customers": rootsOf("x1"),
		},
		reset: map[string]bool{"services.code": true},
	}
	purger := &wipePurger{script: map[string][]error{}, extra: map[string][]PurgedRecordRef{}}
	aud := &spyAuditor{}
	svc := NewSampleDataService(store, cat, &genWriter{}).WithAuditor(aud).WithWipe(purger, wipeLister(cat))
	return svc, store, purger, aud
}

func TestSampleDataService_Wipe_ConfirmationAndLock(t *testing.T) {
	svc, _, purger, _ := wipeFixture(t, threeTableApp())

	t.Run("missing expectTotal is SAMPLE_WIPE_CONFIRM_REQUIRED, nothing purged", func(t *testing.T) {
		_, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{})
		if codeOf(err) != "SAMPLE_WIPE_CONFIRM_REQUIRED" || len(purger.calls) != 0 {
			t.Fatalf("err = %v calls = %v", err, purger.order())
		}
	})
	t.Run("a stale total (compared on the root list itself) is SAMPLE_WIPE_STALE with the current number", func(t *testing.T) {
		_, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(4)})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SAMPLE_WIPE_STALE" || ve.Details["current"] != 5 || len(purger.calls) != 0 {
			t.Fatalf("err = %#v calls = %v; want SAMPLE_WIPE_STALE current=5 and no purge", err, purger.order())
		}
	})
	t.Run("a busy tenant lock is SAMPLE_BUSY", func(t *testing.T) {
		release, ok := svc.tryLock("t1")
		if !ok {
			t.Fatal("lock not free")
		}
		defer release()
		_, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
		if codeOf(err) != "SAMPLE_BUSY" || len(purger.calls) != 0 {
			t.Fatalf("err = %v calls = %v; want SAMPLE_BUSY", err, purger.order())
		}
	})
	t.Run("an explicitly requested table that is not live is SAMPLE_ENTITY_NOT_FOUND", func(t *testing.T) {
		_, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{Entities: []string{"nope"}, ExpectTotal: intp(0)})
		if codeOf(err) != "SAMPLE_ENTITY_NOT_FOUND" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSampleDataService_Wipe_DeletesChildFirstAndReports(t *testing.T) {
	svc, store, purger, aud := wipeFixture(t, threeTableApp())

	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
	if err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	order := purger.order()
	if slices.Index(order, "appointments/a1") > slices.Index(order, "customers/c1") || slices.Index(order, "appointments/a1") > slices.Index(order, "customers/c2") {
		t.Fatalf("order = %v; a child table must be purged before its parent", order)
	}
	if len(order) != 5 {
		t.Fatalf("order = %v; every root is purged exactly once", order)
	}
	for _, c := range purger.calls {
		if c.tenant != "t1" || !c.scope.IsAdmin {
			t.Fatalf("call = %+v; want tenant t1 and the caller's admin scope", c)
		}
	}
	got := map[string]int{}
	for _, d := range res.Deleted {
		got[d.Entity] = d.Count
	}
	if got["customers"] != 2 || got["appointments"] != 1 || got["services"] != 2 || len(res.Kept) != 0 {
		t.Fatalf("deleted = %+v kept = %+v", res.Deleted, res.Kept)
	}
	if res.BlobsRemoved != 2 {
		t.Fatalf("blobsRemoved = %d; want the sum of every purge's blobs", res.BlobsRemoved)
	}
	if len(res.SequencesReset) != 1 || res.SequencesReset[0] != (SampleSequenceRef{Entity: "services", Field: "code"}) {
		t.Fatalf("sequencesReset = %+v; want only services.code (customers has no counter, the fake said yes for services)", res.SequencesReset)
	}
	if !slices.Contains(store.resets, "services.code") {
		t.Fatalf("resets asked = %v", store.resets)
	}
	for _, tn := range store.rootTen {
		if tn != "t1" {
			t.Fatalf("store asked for tenant %q; want t1 only", tn)
		}
	}
	if len(aud.events) != 1 || aud.events[0].EntityName != audit.SampleDataMeta || aud.events[0].Action != audit.ActionDelete ||
		aud.events[0].TenantID != "t1" || aud.events[0].ActorUserID != "u1" {
		t.Fatalf("audit = %+v; want exactly one __sample_data__ delete summary (the hard_delete rows are RecordService.Purge's)", aud.events)
	}
}

func TestSampleDataService_Wipe_PinnedFromRelationDefaults(t *testing.T) {
	cat := threeTableApp()
	pinned := fdef("customer", "relation", true, map[string]any{"relatedEntity": "customers"})
	pinned.DefaultVal = "c2"
	cat.fields["t1|appointments"] = []metadata.FieldDefinition{pinned, fdef("day", "date", true, nil)}
	svc, _, purger, _ := wipeFixture(t, cat)
	blocker := &ValidationError{Code: "RECORD_PURGE_LIVE_CHILDREN", Msg: "x",
		Details: map[string]any{"blockers": []dynrepo.PurgeBlocker{{Entity: "appointments", Field: "customer", Count: 1, Kind: dynrepo.BlockerFieldDefault}}}}
	purger.script["customers/c2"] = []error{blocker}

	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
	if err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	for _, c := range purger.calls {
		if pin, ok := c.pinned[dynrepo.PinKey("customers", "c2")]; !ok || pin != (dynrepo.PurgePin{Entity: "appointments", Field: "customer"}) || len(c.pinned) != 1 {
			t.Fatalf("call %s/%s pinned = %+v; want customers/c2 -> appointments.customer, passed to EVERY purge", c.entity, c.id, c.pinned)
		}
	}
	if len(res.Kept) != 1 || res.Kept[0].Entity != "customers" || res.Kept[0].RecordID != "c2" || res.Kept[0].Reason != "field_default" ||
		len(res.Kept[0].BlockedBy) != 1 || res.Kept[0].BlockedBy[0].Field != "customer" {
		t.Fatalf("kept = %+v; want c2 kept as field_default naming appointments.customer", res.Kept)
	}
}

func TestSampleDataService_Wipe_MapsRefusalsToKept(t *testing.T) {
	svc, _, purger, _ := wipeFixture(t, threeTableApp())
	real := &ValidationError{Code: "RECORD_PURGE_LIVE_CHILDREN", Msg: "x",
		Details: map[string]any{"blockers": []dynrepo.PurgeBlocker{{Entity: "orders", Field: "customer", Count: 2}}}}
	tooLarge := &ValidationError{Code: "RECORD_PURGE_TOO_LARGE", Msg: "x", Details: map[string]any{"max": 500}}
	conflict := fmt.Errorf("services.RecordService.Purge: %w", dynrepo.ErrWriteConflict)
	purger.script["customers/c1"] = []error{real}
	purger.script["customers/c2"] = []error{tooLarge, tooLarge} // retried once in the second pass, still too large
	purger.script["services/s1"] = []error{conflict, conflict}  // retried once at once, then busy
	purger.script["services/s2"] = []error{dynrepo.ErrRecordNotFound}
	purger.script["appointments/a1"] = []error{conflict, nil} // the retry succeeds

	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
	if err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	reasons := map[string]string{}
	for _, k := range res.Kept {
		reasons[k.Entity+"/"+k.RecordID] = k.Reason
		if k.BlockedBy == nil {
			t.Fatalf("kept %+v: blockedBy must be [] not null", k)
		}
	}
	want := map[string]string{"customers/c1": "referenced_by_real", "customers/c2": "too_large", "services/s1": "busy"}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Fatalf("kept reasons = %v; want %v", reasons, want)
	}
	count := map[string]int{}
	for _, c := range purger.order() {
		count[c]++
	}
	if count["customers/c2"] != 2 || count["services/s1"] != 2 || count["appointments/a1"] != 2 || count["customers/c1"] != 1 || count["services/s2"] != 1 {
		t.Fatalf("calls = %v; want too_large retried in pass two, a write conflict retried once, nothing else repeated", count)
	}
	deleted := map[string]int{}
	for _, d := range res.Deleted {
		deleted[d.Entity] = d.Count
	}
	if deleted["appointments"] != 1 || deleted["customers"] != 0 || deleted["services"] != 0 {
		t.Fatalf("deleted = %v; want only the appointment (s2 was already gone, the rest kept)", deleted)
	}
	if len(res.Kept[0].BlockedBy) == 0 && res.Kept[0].Reason == "referenced_by_real" {
		t.Fatal("referenced_by_real must carry its blockers")
	}
}

func TestSampleDataService_Wipe_KeptRowThatGoesWithAnotherClusterIsNotKept(t *testing.T) {
	svc, _, purger, _ := wipeFixture(t, threeTableApp())
	conflict := fmt.Errorf("x: %w", dynrepo.ErrWriteConflict)
	purger.script["appointments/a1"] = []error{conflict, conflict}                       // busy on its own...
	purger.extra["customers/c1"] = []PurgedRecordRef{{Entity: "appointments", ID: "a1"}} // ...but c1's cluster takes it
	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
	if err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if len(res.Kept) != 0 {
		t.Fatalf("kept = %+v; a row removed by a later cluster is deleted, not kept", res.Kept)
	}
}

func TestSampleDataService_Wipe_OtherErrorStopsAndRerunFinishes(t *testing.T) {
	svc, store, purger, aud := wipeFixture(t, threeTableApp())
	boom := errors.New("disk on fire")
	purger.script["customers/c1"] = []error{boom}

	_, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(5)})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v; want the unexpected error to stop the wipe", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("audit = %d events; the clusters that were deleted before the stop still get their summary", len(aud.events))
	}
	// the next run reads only what is left (the fake store models the committed deletions)
	store.roots["t1|appointments"] = nil
	store.roots["t1|services"] = nil
	store.roots["t1|customers"] = rootsOf("c1", "c2")
	purger.calls = nil
	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(2)})
	if err != nil || len(res.Deleted) != 1 || res.Deleted[0].Count != 2 {
		t.Fatalf("rerun = %+v, %v; want the remaining two deleted", res, err)
	}
}

func TestSampleDataService_Wipe_ResultArraysAreNeverNull(t *testing.T) {
	svc, store, _, _ := wipeFixture(t, threeTableApp())
	store.roots = map[string][]dynrepo.SampleRootRow{}
	res, err := svc.Wipe(context.Background(), genScope, "t1", "u1", WipeRequest{ExpectTotal: intp(0)})
	if err != nil || res.Deleted == nil || res.Kept == nil || res.SequencesReset == nil {
		t.Fatalf("res = %#v, %v; want empty arrays, never nil", res, err)
	}
}
