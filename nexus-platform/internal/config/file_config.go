package config

import (
	"strings"
)

// File-upload defaults (File-upload M1, ENGINE-COVERAGE-SPEC §EPIC CROSS file/image).
const (
	// defaultFileStorageDir is the root the LocalBackend stores blobs under when
	// FILE_STORAGE_DIR is unset. Per-tenant subdirs are created beneath it.
	defaultFileStorageDir = "./storage"
	// defaultMaxUploadBytes caps a single uploaded file at 10 MiB (MAX_UPLOAD_BYTES override).
	// A larger upload is rejected with 413 before the blob is streamed to storage.
	defaultMaxUploadBytes = 10 << 20 // 10 MiB
)

// defaultUploadMimeWhitelist is the conservative set of content types accepted when
// UPLOAD_MIME_WHITELIST is unset. It covers the common document/image attachments every
// vertical needs (images, PDF, office docs, plain text, CSV) without opening the door to
// executables/scripts. Override with a comma-separated UPLOAD_MIME_WHITELIST; set it to "*"
// to accept any type (NOT recommended in production).
var defaultUploadMimeWhitelist = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml",
	"application/pdf",
	"text/plain", "text/csv",
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/zip",
}

// FileConfig holds the file-upload runtime settings loaded from the environment.
type FileConfig struct {
	// StorageDir is the LocalBackend root (FILE_STORAGE_DIR). Per-tenant subdirs live under it.
	StorageDir string
	// MaxUploadBytes is the per-file size cap (MAX_UPLOAD_BYTES). A value ≤ 0 means no cap.
	MaxUploadBytes int64
	// MimeWhitelist is the set of accepted content types (UPLOAD_MIME_WHITELIST). A single "*"
	// element disables the whitelist (accept any type).
	MimeWhitelist []string
}

// LoadFileConfig reads the file-upload settings from the environment, applying safe defaults so
// the process boots without any var set. Mirrors LoadConfig's getEnv* helpers.
func LoadFileConfig() *FileConfig {
	dir := getEnv("FILE_STORAGE_DIR", defaultFileStorageDir)
	maxBytes := getEnvInt64("MAX_UPLOAD_BYTES", defaultMaxUploadBytes)

	var whitelist []string
	if raw := getEnv("UPLOAD_MIME_WHITELIST", ""); raw != "" {
		for _, m := range strings.Split(raw, ",") {
			if m = strings.TrimSpace(m); m != "" {
				whitelist = append(whitelist, strings.ToLower(m))
			}
		}
	}
	if len(whitelist) == 0 {
		whitelist = append(whitelist, defaultUploadMimeWhitelist...)
	}

	return &FileConfig{
		StorageDir:     dir,
		MaxUploadBytes: maxBytes,
		MimeWhitelist:  whitelist,
	}
}

// MimeAllowed reports whether the given content type passes the whitelist. A "*" entry accepts
// everything. The check is case-insensitive and ignores any "; charset=..." parameter suffix
// (a multipart upload often sends "text/plain; charset=utf-8").
func (c *FileConfig) MimeAllowed(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	for _, allowed := range c.MimeWhitelist {
		if allowed == "*" || allowed == m {
			return true
		}
	}
	return false
}
