// Package ittest holds shared, test-only helpers for integration tests that must
// self-provision a throwaway, migrated MySQL database (mirroring internal/services'
// o3aSetup and internal/handlers' setupMigratedDB gold pattern).
//
// The real implementation lives in ittest.go behind the `integration` build tag. This
// file carries only the package clause (no build tag) so plain `go build ./...` /
// `go vet ./...` / `go test ./...` (no tag) don't fail on an all-excluded package. Nothing
// here is linked into the server binary — it is imported only by *_integration_test.go files.
package ittest
