package tenantbackup

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"strings"
	"testing"
)

const (
	blobA = "11111111-1111-4111-8111-111111111111"
	blobB = "22222222-2222-4222-8222-222222222222"
)

func sampleManifest() Manifest {
	return Manifest{
		FormatVersion: FormatVersion,
		BackupID:      "b1",
		Tenant:        ManifestTenant{ID: "t1", Slug: "qa-x", Name: "QA X"},
		Kind:          "manual",
		CreatedAt:     "2026-10-07T01:02:03.004Z",
		CreatedBy:     "Nguyễn Văn A (Nexus)",
		SchemaVersion: 84,
		Tables: []ManifestTable{
			{Name: "sys_files", Class: "restore", Rows: 2, Columns: []Column{{"id", "char"}}},
			{Name: "team_memberships", Class: "restore", Rows: 1, Columns: []Column{{"team_id", "char"}},
				OwnerFK: &OwnerFK{Parent: "teams", Column: "team_id"}},
		},
		Files:           ManifestFiles{Count: 2, Bytes: 7, Missing: []string{}, ShaMismatch: []string{}},
		ExcludedTables:  []ExcludedTable{{Table: "sys_api_keys", Reason: "khoá API"}},
		ExcludedColumns: []ExcludedColumn{{Table: "users", Column: "password_hash", Reason: "mật khẩu"}},
	}
}

func buildZip(t *testing.T, fn func(w *Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	fn(w)
	return buf.Bytes()
}

func TestArchiveRoundTrip(t *testing.T) {
	m := sampleManifest()
	data := buildZip(t, func(w *Writer) {
		tw, err := w.Table("sys_files")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(`{"id":"a"}` + "\n" + `{"id":"b"}` + "\n"))
		n, sum, err := w.Blob(blobA, strings.NewReader("abcd"))
		if err != nil || n != 4 || len(sum) != 64 {
			t.Fatalf("Blob = %d %q %v", n, sum, err)
		}
		if err := w.JSON("tenant.json", map[string]any{"branding": nil}); err != nil {
			t.Fatal(err)
		}
		if err := w.Finish(Readme(m), m); err != nil {
			t.Fatal(err)
		}
	})

	r, err := OpenReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	got, err := r.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if got.BackupID != "b1" || got.Tenant.Slug != "qa-x" || len(got.Tables) != 2 || got.Tables[1].OwnerFK == nil ||
		got.Tables[1].OwnerFK.Parent != "teams" || got.ExcludedColumns[0].Column != "password_hash" {
		t.Errorf("manifest did not round-trip: %+v", got)
	}
	rows, err := r.Table("sys_files")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for {
		line, err := rows.ReadLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.TrimSpace(string(line)))
	}
	if strings.Join(lines, "|") != `{"id":"a"}|{"id":"b"}` {
		t.Errorf("rows = %v", lines)
	}
	b, err := r.Blob(blobA)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(b)
	if string(body) != "abcd" {
		t.Errorf("blob = %q", body)
	}
	if ids := r.BlobIDs(); len(ids) != 1 || ids[0] != blobA {
		t.Errorf("BlobIDs = %v", ids)
	}
}

func TestArchiveMethods(t *testing.T) {
	m := sampleManifest()
	data := buildZip(t, func(w *Writer) {
		tw, _ := w.Table("sys_files")
		_, _ = tw.Write([]byte(strings.Repeat(`{"id":"a"}`+"\n", 100)))
		_, _, _ = w.Blob(blobA, strings.NewReader(strings.Repeat("x", 1000)))
		_ = w.Finish(Readme(m), m)
	})
	zr, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	for _, f := range zr.File {
		switch {
		case strings.HasPrefix(f.Name, "blobs/") && f.Method != zip.Store:
			t.Errorf("%s must be Stored (attachments are already compressed)", f.Name)
		case strings.HasPrefix(f.Name, "tables/") && f.Method != zip.Deflate:
			t.Errorf("%s must be Deflated", f.Name)
		}
	}
}

// handZip builds a zip with arbitrary entry names (bypassing the Writer's own validation).
func handZip(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("{}"))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestOpenReader_AllowlistRefusals(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"stray entry", "evil.sh"},
		{"parent traversal", "../x"},
		{"table traversal", "tables/../../etc/passwd"},
		{"nested table path", "tables/a/b.jsonl"},
		{"table with no policy rule", "tables/not_a_table.jsonl"},
		{"table that is never exported (Skip class)", "tables/sys_api_keys.jsonl"},
		{"blob with a non-uuid name", "blobs/not-a-uuid"},
		{"blob traversal", "blobs/../manifest.json"},
		{"absolute path", "/etc/passwd"},
		{"backslash", `tables\sys_files.jsonl`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := handZip(t, "manifest.json", tc.entry)
			_, err := OpenReader(bytes.NewReader(data), int64(len(data)))
			var e *Error
			if !errors.As(err, &e) || e.Code != "TENANT_BACKUP_CORRUPT" {
				t.Fatalf("want TENANT_BACKUP_CORRUPT, got %v", err)
			}
		})
	}
}

func TestOpenReader_DuplicateEntryAndNotAZip(t *testing.T) {
	data := handZip(t, "manifest.json", "manifest.json")
	if _, err := OpenReader(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Error("a duplicate entry name must be refused")
	}
	var e *Error
	_, err := OpenReader(strings.NewReader("not a zip at all"), 16)
	if !errors.As(err, &e) || e.Code != "TENANT_BACKUP_CORRUPT" {
		t.Errorf("a non-zip must be TENANT_BACKUP_CORRUPT, got %v", err)
	}
}

// A header that lies about the uncompressed size must not let the reader inflate without bound.
func TestReader_LyingHeaderIsBounded(t *testing.T) {
	var raw bytes.Buffer
	fw, _ := flate.NewWriter(&raw, flate.BestCompression)
	_, _ = fw.Write(bytes.Repeat([]byte("A"), 1<<20)) // 1 MiB of real content
	_ = fw.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fh := &zip.FileHeader{Name: "manifest.json", Method: zip.Deflate}
	fh.CompressedSize64 = uint64(raw.Len())
	fh.UncompressedSize64 = 10 // the lie
	w, err := zw.CreateRaw(fh)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(raw.Bytes())
	_ = zw.Close()

	r, err := OpenReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	_, err = r.Manifest()
	var e *Error
	if !errors.As(err, &e) || e.Code != "TENANT_BACKUP_CORRUPT" {
		t.Fatalf("a lying UncompressedSize64 must be TENANT_BACKUP_CORRUPT, got %v", err)
	}
}

func TestWriter_RefusesBadNames(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if _, err := w.Table("../x"); err == nil {
		t.Error("Table must refuse a traversal name")
	}
	if _, err := w.Table("sys_api_keys"); err == nil {
		t.Error("Table must refuse a class that is never exported")
	}
	if _, _, err := w.Blob("../etc", strings.NewReader("x")); err == nil {
		t.Error("Blob must refuse a non-uuid id")
	}
}

func TestReadme_NamesNoOperatorIdentity(t *testing.T) {
	m := sampleManifest()
	txt := Readme(m)
	for _, must := range []string{"qa-x", "Nguyễn Văn A (Nexus)", "mật khẩu"} {
		if !strings.Contains(txt, must) {
			t.Errorf("README must mention %q", must)
		}
	}
	// No raw UUID of an operator / workshop may leak: only the tenant's own id is in the manifest.
	if strings.Contains(txt, "@") {
		t.Errorf("README must not carry an email address:\n%s", txt)
	}
}
