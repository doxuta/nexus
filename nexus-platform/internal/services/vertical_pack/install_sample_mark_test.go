package vertical_pack

// specs/064-preview-as-role-sample-data S9-6 (T100): the samples of a BUILT-IN pack carry the
// sample mark (source=pack, batch = the install's ledger id); a pack a client uploads cannot ask
// for the mark; the existing install behaviour is unchanged (install_samples_skip_test).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// markWriter is a recordWriter that remembers the sample mark (if any) of every create and mints
// ids "rec-N". failName makes the row whose data["name"] equals it fail with RECORD_REQUIRED_MISSING.
type markWriter struct {
	mu       sync.Mutex
	seq      int
	failName string
	// failKind picks what the failName row fails with: "" = RECORD_REQUIRED_MISSING, "veto" =
	// RECORD_HOOK_VETO, "dup" = RECORD_DUPLICATE, "infra" = a plain (non-validation) error.
	failKind string
	// failEntity, when set, fails EVERY row of that entity the failKind way.
	failEntity string
	rows       []markedRow
}

type markedRow struct {
	entity string
	data   map[string]any
	id     string
	mark   dynrepo.SampleMark
	marked bool
}

func (w *markWriter) Create(ctx context.Context, _ services.RecordScope, _, _, entity string, payload map[string]any) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if name, _ := payload["name"].(string); (w.failName != "" && name == w.failName) || (w.failEntity != "" && entity == w.failEntity) {
		switch w.failKind {
		case "veto":
			return "", invalid("RECORD_HOOK_VETO", "quy tắc từ chối")
		case "dup":
			return "", invalid("RECORD_DUPLICATE", "trùng")
		case "infra":
			return "", fmt.Errorf("Error 1364: Field 'secret_col' doesn't have a default value")
		}
		return "", invalid("RECORD_REQUIRED_MISSING", "field %q là bắt buộc", "name")
	}
	w.seq++
	m, ok := dynrepo.SampleMarkFrom(ctx)
	id := fmt.Sprintf("rec-%d", w.seq)
	w.rows = append(w.rows, markedRow{entity: entity, data: payload, id: id, mark: m, marked: ok})
	return id, nil
}

// ledgerFake is an installStore: ListInstalls answers newest first.
type ledgerFake struct {
	mu   sync.Mutex
	recs []InstallRecord
}

func (l *ledgerFake) RecordInstall(_ context.Context, rec InstallRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, rec)
	return nil
}

func (l *ledgerFake) ListInstalls(_ context.Context, tenantID string) ([]InstallRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []InstallRecord
	for i := len(l.recs) - 1; i >= 0; i-- {
		if l.recs[i].TenantID == tenantID {
			out = append(out, l.recs[i])
		}
	}
	return out, nil
}

func newMarkTestService(w *markWriter, lk *fakeSkipLinker, led *ledgerFake) *Service {
	meta := &fakeMintMeta{}
	d := Deps{EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta, RecordWriter: w}
	if lk != nil {
		d.Linker = lk
	}
	if led != nil {
		d.InstallStore = led
	}
	return NewService(d)
}

func TestInstall_DemoSamplesMarksEverySampleRowAsPack(t *testing.T) {
	w, led := &markWriter{}, &ledgerFake{}
	svc := newMarkTestService(w, &fakeSkipLinker{}, led)
	pack := skipTestPack()
	pack.DemoSamples = true

	if _, err := svc.Install(context.Background(), "tenant-1", "user-1", pack); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(w.rows) != 5 {
		t.Fatalf("created %d rows, want 5", len(w.rows))
	}
	if len(led.recs) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(led.recs))
	}
	for i, r := range w.rows {
		if !r.marked || r.mark.Source != dynrepo.SampleSourcePack {
			t.Errorf("row %d: marked=%v source=%q, want a pack mark", i, r.marked, r.mark.Source)
		}
		if r.mark.BatchID != led.recs[0].ID {
			t.Errorf("row %d: batch %q, want the install's ledger id %q", i, r.mark.BatchID, led.recs[0].ID)
		}
	}
}

func TestInstall_WithoutDemoSamplesRowsAreNotMarked(t *testing.T) {
	w := &markWriter{}
	svc := newMarkTestService(w, &fakeSkipLinker{}, &ledgerFake{})

	if _, err := svc.Install(context.Background(), "tenant-1", "user-1", skipTestPack()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for i, r := range w.rows {
		if r.marked {
			t.Errorf("row %d carries a mark although the pack is not a demo pack", i)
		}
	}
}

// A client-supplied pack can never turn the mark on: the field has no JSON name.
func TestPack_DemoSamplesIsNeverReadFromJSON(t *testing.T) {
	var p Pack
	if err := json.Unmarshal([]byte(`{"entities":[],"demoSamples":true,"DemoSamples":true}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.DemoSamples {
		t.Fatal("DemoSamples was set from the request body")
	}
	out, err := json.Marshal(&Pack{DemoSamples: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(out)), "demosamples") {
		t.Fatalf("DemoSamples leaked into JSON: %s", out)
	}
}

// Every shipped pack seeds marked samples; the same pack decoded from JSON does not.
func TestBuiltinPack_AllCarryDemoSamples(t *testing.T) {
	for _, slug := range BuiltinSlugs() {
		p, ok := BuiltinPack(slug)
		if !ok || !p.DemoSamples {
			t.Errorf("BuiltinPack(%q): ok=%v DemoSamples=%v, want true", slug, ok, ok && p.DemoSamples)
		}
	}
}
