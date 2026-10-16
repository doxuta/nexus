package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLocalBackend_PutOpenRoundTrip(t *testing.T) {
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	ctx := context.Background()
	want := []byte("hello nexus file engine")
	n, err := b.Put(ctx, "tenant-1/abc-123", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(want)) {
		t.Fatalf("Put bytes = %d, want %d", n, len(want))
	}
	rc, err := b.Open(ctx, "tenant-1/abc-123")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}
}

func TestLocalBackend_OpenMissingIsNotFound(t *testing.T) {
	b, _ := NewLocalBackend(t.TempDir())
	_, err := b.Open(context.Background(), "tenant-1/nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing err = %v, want ErrNotFound", err)
	}
}

func TestLocalBackend_DeleteIdempotent(t *testing.T) {
	b, _ := NewLocalBackend(t.TempDir())
	ctx := context.Background()
	if _, err := b.Put(ctx, "t/x", strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "t/x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second delete of a now-missing blob must be a no-op (idempotent), not an error.
	if err := b.Delete(ctx, "t/x"); err != nil {
		t.Fatalf("Delete (idempotent) = %v, want nil", err)
	}
	if _, err := b.Open(ctx, "t/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete err = %v, want ErrNotFound", err)
	}
}

func TestLocalBackend_RejectsPathTraversal(t *testing.T) {
	b, _ := NewLocalBackend(t.TempDir())
	ctx := context.Background()
	for _, key := range []string{"../escape", "../../etc/passwd", "tenant/../../../secret", ""} {
		if _, err := b.Put(ctx, key, strings.NewReader("x")); err == nil {
			t.Fatalf("Put(%q) succeeded, want path-traversal/empty rejection", key)
		}
		if _, err := b.Open(ctx, key); err == nil {
			t.Fatalf("Open(%q) succeeded, want rejection", key)
		}
	}
}
