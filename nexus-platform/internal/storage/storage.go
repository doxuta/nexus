// Package storage abstracts where uploaded file blobs physically live, so the file-upload
// feature (ENGINE-COVERAGE-SPEC §EPIC CROSS "File / media / document attachment") can swap the
// local-filesystem backend for S3/GCS later without touching the service/handler layers.
//
// Design: the StorageBackend interface deals ONLY in opaque, server-generated keys plus byte
// streams — it never sees the original client filename and never builds a path from untrusted
// input, so a malicious "../../etc/passwd" filename cannot escape the storage root (path-traversal
// defense lives here, at the boundary). The metadata (original name, size, mime, tenant scope)
// is the database's job (sys_files), kept entirely out of the blob layer.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned by Open/Delete when the key does not resolve to a stored blob. The
// service maps it to a 404 (a file row may reference a key whose blob was removed out of band).
var ErrNotFound = errors.New("storage: object not found")

// Backend is the pluggable blob store. Keys are opaque, server-generated, forward-slash
// separated relative paths (e.g. "<tenant>/<uuid>") — the backend is responsible for mapping a
// key to its own namespace safely. Implementations MUST reject any key that would escape their
// root (path traversal) and MUST treat keys as already-sanitized server values, never as client
// input to re-parse.
type Backend interface {
	// Put streams r into the store under key, returning the number of bytes written. An
	// existing object at key is overwritten. The caller is responsible for generating a
	// collision-free key (the service uses a UUID), so overwrite is effectively never hit.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Open returns a ReadCloser for the blob at key, or ErrNotFound. The caller MUST Close it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the blob at key. Deleting a missing key is a no-op (idempotent) — it does
	// NOT return ErrNotFound, so a double-delete or a delete of an already-cleaned blob is safe.
	Delete(ctx context.Context, key string) error
}

// LocalBackend stores blobs under a single root directory on the local filesystem. The root is
// FILE_STORAGE_DIR (default ./storage). Keys are written as files beneath the root with their
// parent directories created on demand; every resolved path is verified to stay within the root
// so a crafted key can never traverse out.
type LocalBackend struct {
	root string
}

// NewLocalBackend returns a LocalBackend rooted at the given directory, creating it if absent.
// An empty root falls back to "./storage". The root is resolved to an absolute, cleaned path so
// the containment check (resolveKey) is reliable regardless of the process working directory.
func NewLocalBackend(root string) (*LocalBackend, error) {
	if strings.TrimSpace(root) == "" {
		root = "./storage"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage.NewLocalBackend: abs root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("storage.NewLocalBackend: mkdir root: %w", err)
	}
	return &LocalBackend{root: abs}, nil
}

// resolveKey maps an opaque key to an absolute on-disk path and verifies it stays within root
// (path-traversal defense). The key is cleaned and any attempt to escape (".." segments, absolute
// paths) is rejected. Keys use forward slashes; they are converted to the OS separator here.
func (b *LocalBackend) resolveKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("storage: empty key")
	}
	// Normalize to the OS separator, then clean. filepath.Clean collapses ".." so we can detect
	// an escape by checking the cleaned absolute path is still under root.
	rel := filepath.FromSlash(key)
	full := filepath.Clean(filepath.Join(b.root, rel))
	// Containment: full must be root itself (never, keys are non-empty) or strictly under root +
	// separator. Comparing with the trailing separator prevents a sibling like "<root>-evil".
	if full != b.root && !strings.HasPrefix(full, b.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("storage: key %q escapes storage root", key)
	}
	return full, nil
}

// Put writes r to the file at key, creating parent directories. Returns bytes written.
func (b *LocalBackend) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	full, err := b.resolveKey(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return 0, fmt.Errorf("storage.LocalBackend.Put: mkdir: %w", err)
	}
	// O_TRUNC so a re-Put of the same key overwrites cleanly (no stale tail bytes).
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, fmt.Errorf("storage.LocalBackend.Put: open: %w", err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// Best-effort cleanup of a partial write so a half-written blob never lingers.
		_ = os.Remove(full)
		return n, fmt.Errorf("storage.LocalBackend.Put: write: %w", err)
	}
	return n, nil
}

// Open returns a ReadCloser for the blob at key, mapping a missing file to ErrNotFound.
func (b *LocalBackend) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := b.resolveKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage.LocalBackend.Open: %w", err)
	}
	return f, nil
}

// Delete removes the blob at key. A missing file is a no-op (idempotent).
func (b *LocalBackend) Delete(ctx context.Context, key string) error {
	full, err := b.resolveKey(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage.LocalBackend.Delete: %w", err)
	}
	return nil
}
