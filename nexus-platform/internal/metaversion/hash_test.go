package metaversion

import (
	"testing"
	"time"
)

func TestDocHash_StableAcrossKeyOrderAndWhitespace(t *testing.T) {
	a := []byte(`{"name":"X","actions":[{"type":"notify","n":1.50}],"cond":{"b":1,"a":2}}`)
	b := []byte("{\n  \"cond\": {\"a\": 2, \"b\": 1},\n  \"actions\": [ {\"n\": 1.50, \"type\": \"notify\"} ],\n  \"name\": \"X\"\n}")
	ha, err := DocHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := DocHash(b)
	if ha != hb || len(ha) != 64 {
		t.Fatalf("same document hashed differently: %s vs %s", ha, hb)
	}
	hc, _ := DocHash([]byte(`{"name":"Y","actions":[{"type":"notify","n":1.50}],"cond":{"b":1,"a":2}}`))
	if hc == ha {
		t.Fatal("a changed value kept the hash")
	}
	if _, err := DocHash([]byte(`{bad`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestDocHash_NullAndEmptyAgree(t *testing.T) {
	h1, _ := DocHash(nil)
	h2, _ := DocHash([]byte("null"))
	if h1 != h2 {
		t.Fatal("empty and null must canonicalise alike (a NULL condition column)")
	}
}

func TestChangesetHash(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 1, 2, 3, 456_000_000, time.UTC)
	rows := []DraftRow{{ID: "b", UpdatedAt: t0}, {ID: "a", UpdatedAt: t0}}
	h := ChangesetHash(rows)
	if h != ChangesetHash([]DraftRow{rows[1], rows[0]}) {
		t.Fatal("order of rows changed the hash")
	}
	rows[0].UpdatedAt = t0.Add(time.Millisecond)
	if ChangesetHash(rows) == h {
		t.Fatal("a row whose updated_at moved kept the hash")
	}
	if ChangesetHash(nil) != ChangesetHash([]DraftRow{}) {
		t.Fatal("nil and empty differ")
	}
}
