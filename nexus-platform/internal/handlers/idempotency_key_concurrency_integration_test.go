//go:build integration

// Idempotency-Key on POST /:entity — the CONCURRENCY half (DAILY-QUEUE #30,
// specs/047-idempotency-key-on-create FR-5/FR-6): duplicates that arrive while the first request is
// still running get 409 IDEMPOTENCY_IN_PROGRESS, and a storm of same-key POSTs creates exactly one
// record — real goroutines against real MySQL 8 through the full HTTP stack (fixture in
// idempotency_key_integration_test.go).
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestIdempotencyKey_(InFlight|Concurrent)
package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// gatedInsertStore holds every InsertRecord until release is closed, so a test can keep the
// first request INSIDE its create while a duplicate arrives (deterministic in-flight overlap).
type gatedInsertStore struct {
	*dynrepo.Repository
	entered chan struct{}
	release chan struct{}
}

func (s *gatedInsertStore) InsertRecord(ctx context.Context, entity, tenantID, userID string, data map[string]any, uf []dynrepo.UniqueFieldSpec) (string, error) {
	s.entered <- struct{}{}
	<-s.release
	return s.Repository.InsertRecord(ctx, entity, tenantID, userID, data, uf)
}

// #30 — while the first request is still inside its create, a duplicate gets 409
// IDEMPOTENCY_IN_PROGRESS; once the first finishes, the resend is a 200 replay.
func TestIdempotencyKey_InFlightDuplicateIs409(t *testing.T) {
	gate := &gatedInsertStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f := newIdemFixture(t, func(d *dynrepo.Repository) services.RecordStore {
		gate.Repository = d
		return gate
	})
	path := "/api/v1/" + f.entity
	body := map[string]any{"memo": "Thu tiền đơn 2001", "amount": 1}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.post(t, path, "k-2001", body) }()
	<-gate.entered // the first request holds the claim and is inside InsertRecord

	// Without the guard the duplicate walks into InsertRecord too and parks on the gate — bound
	// the wait so that failure mode is a red test, not a hung suite.
	dupDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { dupDone <- f.post(t, path, "k-2001", body) }()
	var dup *httptest.ResponseRecorder
	select {
	case dup = <-dupDone:
	case <-gate.entered:
		close(gate.release)
		<-done
		<-dupDone
		t.Fatal("the in-flight duplicate reached InsertRecord — no claim guards the key")
	case <-time.After(10 * time.Second):
		close(gate.release)
		t.Fatal("the in-flight duplicate neither answered nor reached InsertRecord within 10s")
	}
	if dup.Code != http.StatusConflict || decodeCode(dup.Body.Bytes()) != "IDEMPOTENCY_IN_PROGRESS" {
		close(gate.release)
		t.Fatalf("in-flight duplicate = %d %s, want 409 IDEMPOTENCY_IN_PROGRESS", dup.Code, dup.Body.String())
	}
	close(gate.release)
	first := <-done
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s, want 201", first.Code, first.Body.String())
	}
	replay := f.post(t, path, "k-2001", body)
	if replay.Code != http.StatusOK || createdID(t, replay) != createdID(t, first) {
		t.Fatalf("resend after completion = %d %s, want 200 with id %s", replay.Code, replay.Body.String(), createdID(t, first))
	}
	if n := f.rows(t); n != 1 {
		t.Fatalf("obj rows = %d, want 1", n)
	}
}

// #30 — N goroutines released together with the same key + body: exactly ONE record and ONE 201;
// every other answer is 409 IDEMPOTENCY_IN_PROGRESS or a 200 replay of that same record — never a
// 500, never a second row.
func TestIdempotencyKey_ConcurrentSameKeyCreatesOneRecord(t *testing.T) {
	f := newIdemFixture(t, nil)
	path := "/api/v1/" + f.entity
	const n = 12
	var (
		start sync.WaitGroup
		wg    sync.WaitGroup
	)
	start.Add(1)
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			results[i] = f.post(t, path, "storm-1", map[string]any{"memo": "Thu tiền đơn 3001", "amount": 3})
		}(i)
	}
	start.Done()
	wg.Wait()

	var created, inProgress, replays int
	var createdRecord string
	for i, w := range results {
		switch {
		case w.Code == http.StatusCreated:
			created++
			createdRecord = createdID(t, w)
		case w.Code == http.StatusConflict && decodeCode(w.Body.Bytes()) == "IDEMPOTENCY_IN_PROGRESS":
			inProgress++
		case w.Code == http.StatusOK && w.Header().Get("Idempotent-Replay") == "true":
			replays++
		default:
			t.Fatalf("request %d = %d %s, want 201 / 409 IDEMPOTENCY_IN_PROGRESS / 200 replay", i, w.Code, w.Body.String())
		}
	}
	if created != 1 {
		t.Fatalf("201 count = %d (409=%d, replay=%d), want exactly 1", created, inProgress, replays)
	}
	for _, w := range results {
		if w.Code == http.StatusOK && createdID(t, w) != createdRecord {
			t.Fatalf("replay carried %s, want the one created record %s", createdID(t, w), createdRecord)
		}
	}
	if rows := f.rows(t); rows != 1 {
		t.Fatalf("obj rows = %d, want 1", rows)
	}
	t.Logf("12 concurrent same-key POSTs: 201=%d 409=%d replay=%d", created, inProgress, replays)
}
