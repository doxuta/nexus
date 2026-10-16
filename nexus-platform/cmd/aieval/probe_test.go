package main

// The probe is the piece that decides whether a run says "the app works", so it has to be right
// about a few unglamorous things: it must call the SAME admin endpoints the Studio's Apply button
// calls, it must ask the three usability questions in the customer's order, and it must clean up
// what it created even when the answer is no. All of that is testable against a fake server for
// free — no model, no tokens — which is the only way this stays runnable as a normal gate.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNexus records every path it is called on and can be told to fail one of them.
type fakeNexus struct {
	calls   []string
	failOn  string // path substring that answers 422
	rowID   string
	noEntry bool // /metadata/entities answers an empty list
}

func (f *fakeNexus) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if f.failOn != "" && strings.Contains(r.URL.Path, f.failOn) {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"code":"ENTITY_ALREADY_EXISTS","error":"trùng tên"}`))
			return
		}
		switch {
		case r.URL.Path == "/api/v1/metadata/entities":
			if f.noEntry {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"name":"hop_dong"}]}`))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/hop_dong"):
			id := f.rowID
			if id == "" {
				id = "rec-1"
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + id + `"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{}}`))
		}
	}))
}

const entityDraft = `[{"kind":"entity","entity":{"name":"hop_dong","label":"Hợp đồng","fields":[]}}]`

func TestApplyAndProbe_HappyPathAsksAllThreeQuestionsThenCleansUp(t *testing.T) {
	f := &fakeNexus{}
	srv := f.server()
	defer srv.Close()

	ok, detail := applyAndProbe(srv.Client(), srv.URL, "tok", json.RawMessage(entityDraft), probeSpec{})
	if !ok {
		t.Fatalf("probe failed on a healthy server: %s", detail)
	}

	want := []string{
		"POST /api/v1/admin/entities",            // applied through the REAL admin API
		"GET /api/v1/metadata/entities",          // 1. registered?
		"POST /api/v1/hop_dong",                  // 2. takes a row?
		"GET /api/v1/hop_dong/rec-1",             // 3. gives it back?
		"DELETE /api/v1/admin/entities/hop_dong", // and cleans up after itself
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, f.calls[i], want[i])
		}
	}
}

// The failure that matters most: a table that exists on paper but refuses data. Before the probe,
// that case scored a clean PASS on draft shape alone.
func TestApplyAndProbe_TableThatRefusesARowIsAFailure(t *testing.T) {
	f := &fakeNexus{failOn: "/api/v1/hop_dong"}
	srv := f.server()
	defer srv.Close()

	ok, detail := applyAndProbe(srv.Client(), srv.URL, "tok", json.RawMessage(entityDraft), probeSpec{})
	if ok {
		t.Fatal("a table that refuses every row was reported as usable")
	}
	if !strings.Contains(detail, "REFUSED a row") {
		t.Errorf("detail = %q, want it to name the refusal", detail)
	}
	if !containsCall(f.calls, "DELETE /api/v1/admin/entities/hop_dong") {
		t.Error("cleanup did not run after a failed probe — the lab tenant keeps the debris")
	}
}

// Applied but not registered: metadata and the physical table out of step. Silent before.
func TestApplyAndProbe_AppliedButMissingFromMetadata(t *testing.T) {
	f := &fakeNexus{noEntry: true}
	srv := f.server()
	defer srv.Close()

	ok, detail := applyAndProbe(srv.Client(), srv.URL, "tok", json.RawMessage(entityDraft), probeSpec{})
	if ok {
		t.Fatal("an entity missing from /metadata/entities was reported as usable")
	}
	if !strings.Contains(detail, "MISSING from /metadata/entities") {
		t.Errorf("detail = %q", detail)
	}
	if !containsCall(f.calls, "DELETE /api/v1/admin/entities/hop_dong") {
		t.Error("cleanup did not run")
	}
}

// A draft carrying only kinds the probe does not apply must FAIL loudly, not pass by doing nothing.
// "I applied none of it and found no problems" is the worst possible green.
func TestApplyAndProbe_NothingAppliedIsNotSuccess(t *testing.T) {
	f := &fakeNexus{}
	srv := f.server()
	defer srv.Close()

	ok, detail := applyAndProbe(srv.Client(), srv.URL, "tok",
		json.RawMessage(`[{"kind":"dashboard"},{"kind":"report"}]`), probeSpec{})
	if ok {
		t.Fatal("a draft with nothing applicable was reported as usable")
	}
	if !strings.Contains(detail, "no entity was created") {
		t.Errorf("detail = %q", detail)
	}
}

// Kinds the probe does not apply must be NAMED in the result. A run that says "probe OK" while
// silently skipping the layout and the app reads as fuller coverage than it is.
func TestApplyAndProbe_ReportsWhatItDidNotApply(t *testing.T) {
	f := &fakeNexus{}
	srv := f.server()
	defer srv.Close()

	draft := `[{"kind":"entity","entity":{"name":"hop_dong","label":"HĐ","fields":[]}},{"kind":"layout"},{"kind":"app"}]`
	ok, detail := applyAndProbe(srv.Client(), srv.URL, "tok", json.RawMessage(draft), probeSpec{})
	if !ok {
		t.Fatalf("probe failed: %s", detail)
	}
	if !strings.Contains(detail, "KHÔNG áp") || !strings.Contains(detail, "layout") || !strings.Contains(detail, "app") {
		t.Errorf("detail = %q, want the skipped kinds named", detail)
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
