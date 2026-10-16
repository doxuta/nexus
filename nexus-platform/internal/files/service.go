package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
	"nexus/platform/internal/storage"
)

// ErrFileTooLarge / ErrMimeNotAllowed are upload-policy rejections surfaced to the handler as
// 413 / 415 respectively. ErrEmptyFile guards a zero-byte multipart part (no file selected).
var (
	ErrFileTooLarge   = errors.New("files: upload exceeds size limit")
	ErrMimeNotAllowed = errors.New("files: content type not allowed")
	ErrEmptyFile      = errors.New("files: empty upload")
)

// RecordGuard is the slice of *services.RecordService the file subsystem needs: it authorizes
// the parent record under tenant + A5 scope, confirms the target field is a file/image field,
// and syncs the field's [{id,name}] reference array after an upload/delete. Declared here
// (consumer-side) so FileService stays testable with a fake and does not hard-depend on the
// concrete record service beyond these two calls.
type RecordGuard interface {
	ResolveFileField(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, fieldName string) (string, error)
	SetFileRefs(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, fieldName string, refs []map[string]any) error
}

// Limits captures the upload policy (size cap + mime whitelist), resolved from config.FileConfig.
type Limits struct {
	MaxBytes int64             // ≤ 0 means no cap
	MimeOK   func(string) bool // reports whether a content type passes the whitelist
}

// IDGen generates a new file id (UUID). Injected so tests are deterministic.
type IDGen func() string

// Service is the file-attachment business logic: it authorizes against the record, enforces the
// upload policy, persists the blob on the storage backend, catalogs it in sys_files, and keeps
// the record's file field reference array in sync. It knows nothing about HTTP.
type Service struct {
	repo    *Repository
	backend storage.Backend
	guard   RecordGuard
	limits  Limits
	newID   IDGen

	// acl resolves the caller's row-level access on the file's PARENT entity. Optional only so
	// existing constructions keep compiling; production wires it (see WithACL).
	acl ACLResolver
}

// ACLResolver answers "may this role act on this entity, and over which rows". Identical to
// middleware.ACLChecker — declared here so files keeps no dependency on the HTTP layer.
type ACLResolver interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (allowed bool, accessLevel string, err error)
}

// WithACL arms per-file authorization.
//
// Download and delete are mounted at /files/:id — a flat path with NO :entity segment — so the
// route-level ACL middleware, which authorizes on c.Param("entity"), cannot run there at all.
// Nothing then wrote the resolved access level into the request, and scopeOf() correctly failed
// CLOSED to AccessNone, so the parent-record re-check found no record and every non-admin got 404
// on every attachment. Grant-independent: even a user holding "all" was refused.
//
// The asymmetry was the tell — upload rides an entity-scoped route and worked, and GET /:entity/:id
// still returned the attachment refs, so the list rendered and only the click failed.
//
// The entity is not knowable until the file row is read, so the decision belongs HERE, after
// GetByID, rather than in a middleware that would have to guess. Chainable.
func (s *Service) WithACL(a ACLResolver) *Service {
	s.acl = a
	return s
}

// scopeFor re-resolves the caller's row scope against the file's parent entity.
//
// action is "view" for a download and "edit" for an upload or a removal — the same verbs the record
// routes authorize, so a person who may read a record may read what is attached to it, and only
// someone who may edit the record may attach or detach (specs/060 #348: removal used to ask for
// "delete", which let a technician upload a photo but not take back a wrong one).
//
// Admins keep the scope they arrived with (is_admin bypasses ACL everywhere else too). A denial
// or a resolver error yields AccessNone, so the existing parent-record check turns it into the
// same 404 the record read gives — no new refusal shape, and no existence oracle.
func (s *Service) scopeFor(ctx context.Context, scope services.RecordScope, tenantID, entity, action string) services.RecordScope {
	if scope.IsAdmin || s.acl == nil {
		return scope
	}
	// specs/051: a download reads the parent like GET /:entity/:id does, so it sees a record shared
	// with the caller; a delete/upload is a write and never does (shares are read-only).
	scope.IncludeShares = action == "view"
	allowed, level, err := s.acl.Authorize(ctx, tenantID, scope.RoleID, entity, action)
	if err != nil || !allowed {
		scope.AccessLevel = string(dynrepo.AccessNone)
		return scope
	}
	scope.AccessLevel = level
	return scope
}

// NewService wires the file service. newID defaults to a UUID generator when nil.
func NewService(repo *Repository, backend storage.Backend, guard RecordGuard, limits Limits, newID IDGen) *Service {
	if limits.MimeOK == nil {
		// Fail closed: with no whitelist configured, reject everything rather than accept all.
		limits.MimeOK = func(string) bool { return false }
	}
	if newID == nil {
		newID = defaultIDGen
	}
	return &Service{repo: repo, backend: backend, guard: guard, limits: limits, newID: newID}
}

// UploadInput carries one multipart upload. Content is the streamed file body; Size is the
// declared content length (≤ 0 when unknown — the service enforces the cap during the copy).
type UploadInput struct {
	TenantID   string
	EntityName string
	RecordID   string
	FieldName  string
	Filename   string // original client filename (display only)
	Mime       string
	Size       int64
	Content    io.Reader
}

// UploadResult is the public shape returned to the client after a successful upload.
type UploadResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// Upload authorizes the parent record + field, enforces the size/mime policy, stores the blob,
// catalogs it, and re-syncs the record's file field reference array. Returns the new file's
// public descriptor. Authorization errors (ErrEntityNotFound / ErrRecordNotFound / FIELD_NOT_FILE)
// propagate from the guard so the handler maps them to 404/422 exactly like a record op.
func (s *Service) Upload(ctx context.Context, scope services.RecordScope, in UploadInput) (*UploadResult, error) {
	// 0) Re-resolve at the action this call actually PERFORMS: attaching a file MUTATES an existing
	// record (syncRecordRefs writes the parent's file-ref array), so the grant is "edit". The route
	// is pinned to "edit" too (router.go editGroup, specs/060) — it used to ride the record group,
	// where POST→"create" handed a role with create:all and edit:own all-rows authority over a write
	// limited to its own rows. Resolving here keeps the service correct whatever the mount says, the
	// same way Open and Delete resolve view / delete. A denial resolves to AccessNone, which makes
	// the ResolveFileField probe below answer not-found — the same closed refusal an unreadable
	// record gives, so no existence oracle opens up.
	scope = s.scopeFor(ctx, scope, in.TenantID, in.EntityName, "edit")
	// 1) Authorize: record must be readable + field must be a file/image field.
	entity, err := s.guard.ResolveFileField(ctx, scope, in.TenantID, in.EntityName, in.RecordID, in.FieldName)
	if err != nil {
		return nil, err
	}
	// 2) Policy: mime whitelist (checked before any bytes are stored).
	if !s.limits.MimeOK(in.Mime) {
		return nil, ErrMimeNotAllowed
	}
	// Pre-reject an over-cap declared size (Content-Length) before streaming; the copy below
	// re-checks against the actual byte count in case the declared size lied.
	if s.limits.MaxBytes > 0 && in.Size > s.limits.MaxBytes {
		return nil, ErrFileTooLarge
	}

	id := s.newID()
	// Storage key is server-generated from the verified tenant + id — NEVER the client filename
	// (path-traversal / collision defense). The backend stores under "<tenant>/<id>".
	key := storageKey(in.TenantID, id)

	// 3) Stream the blob, enforcing the cap mid-copy so a lying Content-Length cannot bypass it.
	reader := in.Content
	if s.limits.MaxBytes > 0 {
		// +1 so a file exactly at the cap passes but one byte over trips the limit check below.
		reader = io.LimitReader(in.Content, s.limits.MaxBytes+1)
	}
	// Digest the bytes as the backend consumes them — one pass, no second read of the blob — so the
	// catalog can later prove a restored copy byte-for-byte (restore-drill.sh, specs/036 §2.5).
	digest := sha256.New()
	written, err := s.backend.Put(ctx, key, io.TeeReader(reader, digest))
	if err != nil {
		return nil, fmt.Errorf("files.Upload: store blob: %w", err)
	}
	if written == 0 {
		_ = s.backend.Delete(ctx, key)
		return nil, ErrEmptyFile
	}
	if s.limits.MaxBytes > 0 && written > s.limits.MaxBytes {
		_ = s.backend.Delete(ctx, key)
		return nil, ErrFileTooLarge
	}

	// 4) Catalog the file row (tenant-scoped). On failure, roll back the stored blob.
	row := File{
		ID:         id,
		TenantID:   in.TenantID,
		EntityName: entity,
		RecordID:   in.RecordID,
		FieldName:  in.FieldName,
		Name:       sanitizeDisplayName(in.Filename),
		Size:       written,
		Mime:       strings.TrimSpace(in.Mime),
		Path:       key,
		Sha256:     hex.EncodeToString(digest.Sum(nil)),
		CreatedBy:  scope.UserID,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		_ = s.backend.Delete(ctx, key)
		return nil, fmt.Errorf("files.Upload: %w", err)
	}

	// 5) Re-sync the record's file field reference array from the catalog (authoritative list).
	if err := s.syncRecordRefs(ctx, scope, in.TenantID, entity, in.RecordID, in.FieldName); err != nil {
		// A REFUSED record write (a specs/048 before_save veto, a locked field, rules unreadable) is a
		// decision, not a glitch: roll the file back, or the next upload's re-sync — which rebuilds the field from
		// every live sys_files row — would attach the refused file after all.
		if refusedWrite(err) {
			// The undo is a decision already taken: it must outlive a client that hangs up right here.
			undo := context.WithoutCancel(ctx)
			if rerr := s.repo.SoftDelete(undo, in.TenantID, id); rerr != nil {
				return nil, fmt.Errorf("files.Upload: roll back refused file: %w", errors.Join(err, rerr))
			}
			_ = s.backend.Delete(undo, key) // best-effort, like the rollbacks above
			return nil, fmt.Errorf("files.Upload: %w", err)
		}
		// The blob + catalog row are committed; a sync failure leaves an orphan ref the FE can
		// reconcile by re-reading. We surface it so the caller knows the field may be stale,
		// but do NOT delete the successfully-stored file.
		return nil, fmt.Errorf("files.Upload: sync record field: %w", err)
	}

	return &UploadResult{ID: id, Name: row.Name, Size: written, URL: PublicURL(id)}, nil
}

// Open returns the blob stream + its catalog row for download, tenant + A5 scoped. The caller
// must Close the returned ReadCloser. A file in another tenant, a soft-deleted file, or one
// whose parent record the caller may not read all surface as ErrFileNotFound (no leak).
func (s *Service) Open(ctx context.Context, scope services.RecordScope, tenantID, id string) (io.ReadCloser, *File, error) {
	f, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, nil, err
	}
	// Re-authorize against the parent record under A5 row scope: a user who can no longer read
	// the record must not download its attachments either. ResolveFileField returns
	// ErrRecordNotFound when the scope hides the record — we remap to ErrFileNotFound so the
	// file endpoint never reveals that the record exists but is out of scope.
	if _, err := s.guard.ResolveFileField(ctx, s.scopeFor(ctx, scope, tenantID, f.EntityName, "view"),
		tenantID, f.EntityName, f.RecordID, f.FieldName); err != nil {
		if errors.Is(err, services.ErrRecordNotFound) {
			return nil, nil, ErrFileNotFound
		}
		return nil, nil, err
	}
	rc, err := s.backend.Open(ctx, f.Path)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, ErrFileNotFound
		}
		return nil, nil, fmt.Errorf("files.Open: %w", err)
	}
	return rc, f, nil
}

// Delete soft-deletes a file (tenant + A5 scoped), removes its blob, and re-syncs the record's
// file field reference array. Same not-found semantics as Open.
func (s *Service) Delete(ctx context.Context, scope services.RecordScope, tenantID, id string) error {
	f, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	// ONE resolved scope for the whole operation. Resolving it twice — or worse, authorizing with
	// the resolved scope and then WRITING with the unresolved one — is how this used to end
	// half-done: the row was soft-deleted and the blob removed, and only then did the record's
	// reference array fail to update, leaving a phantom attachment listed on a record whose file
	// no longer exists. A delete that half-completes is worse than one that refuses.
	// specs/060 (#348): detaching rewrites the parent's file-ref array, i.e. it CHANGES the record —
	// the same floor as upload ("edit"), not the stricter "delete" grant that removes the record.
	sc := s.scopeFor(ctx, scope, tenantID, f.EntityName, "edit")
	if _, err := s.guard.ResolveFileField(ctx, sc, tenantID, f.EntityName, f.RecordID, f.FieldName); err != nil {
		if errors.Is(err, services.ErrRecordNotFound) {
			return ErrFileNotFound
		}
		return err
	}
	if err := s.repo.SoftDelete(ctx, tenantID, id); err != nil {
		return err
	}
	// ORDER MATTERS, and the comment above records the version that got it wrong. Re-sync the
	// record's reference array FIRST, then remove the bytes. If the sync fails we return an error
	// with the blob still present, so the record still points at something real and a later Upload
	// or Delete on the same (record, field) rebuilds the array from the catalog. The old order —
	// destroy first, fix the reference second — meant a sync failure left the record listing a file
	// whose bytes were gone, and the retry was blocked: GetByID filters deleted_at IS NULL, so the
	// second call 404s on the row this one already tombstoned. Irrecoverable in the product.
	if err := s.syncRecordRefs(ctx, sc, tenantID, f.EntityName, f.RecordID, f.FieldName); err != nil {
		// A REFUSED record write (a specs/048 before_save veto on a locked record, a required file
		// field emptied) wrote nothing, so the delete must not happen either: un-tombstone the row.
		// Leaving it deleted answered 422 with the rule's words while the download already 404'd and
		// the next re-sync dropped the file for good — a lock bypassed to lose an attachment.
		if refusedWrite(err) {
			undo := context.WithoutCancel(ctx) // see Upload: the undo must outlive the request
			if rerr := s.repo.Restore(undo, tenantID, id); rerr != nil {
				return fmt.Errorf("files.Delete: restore refused delete: %w", errors.Join(err, rerr))
			}
			return fmt.Errorf("files.Delete: %w", err)
		}
		return fmt.Errorf("files.Delete: sync record field: %w", err)
	}
	// Best-effort blob removal (idempotent). A failure here leaves an orphan blob — dead weight,
	// reconcilable by an out-of-band sweep — which is the strictly recoverable half of the trade.
	_ = s.backend.Delete(ctx, f.Path)
	return nil
}

// backfillBatch bounds one ListUnhashed page of the sha256 backfill.
const backfillBatch = 500

// BackfillStats is the outcome of one BackfillHashes pass. Unreadable lists the storage keys
// ("<tenant>/<id>") whose blob could not be opened or read: those rows keep sha256 NULL and the
// restore drill keeps checking them by size — and reports them MISSING if the bytes are truly gone.
type BackfillStats struct {
	Hashed     int
	Unreadable []string
}

// BackfillHashes digests every live sys_files row that has no sha256 yet (uploaded before migration
// 000068, or by an older image after it — deploy.sh rolls code back without migrating down), so the
// restore drill can compare those attachments byte-for-byte too. Idempotent: only NULL rows are read
// and SetSha256 never overwrites. The digest attests the bytes AS STORED NOW, not as uploaded — for a
// pre-000068 row the current blob is the only source of truth left.
//
// A blob that cannot be read is reported in Unreadable and skipped, never fatal (one lost file must
// not stop the other tenants' rows from being hashed); a database error aborts and is returned.
func (s *Service) BackfillHashes(ctx context.Context) (BackfillStats, error) {
	var st BackfillStats
	tenants, err := s.repo.TenantsWithUnhashed(ctx)
	if err != nil {
		return st, fmt.Errorf("files.BackfillHashes: %w", err)
	}
	for _, tenantID := range tenants {
		after := ""
		for {
			rows, err := s.repo.ListUnhashed(ctx, tenantID, after, backfillBatch)
			if err != nil {
				return st, fmt.Errorf("files.BackfillHashes: %w", err)
			}
			for _, f := range rows {
				after = f.ID
				sum, err := s.hashBlob(ctx, f.Path)
				if err != nil {
					st.Unreadable = append(st.Unreadable, f.Path)
					continue
				}
				if err := s.repo.SetSha256(ctx, tenantID, f.ID, sum); err != nil {
					return st, fmt.Errorf("files.BackfillHashes: %w", err)
				}
				st.Hashed++
			}
			if len(rows) < backfillBatch {
				break
			}
		}
	}
	return st, nil
}

// hashBlob streams the blob at key through sha256 (lowercase hex, the form Upload stores).
func (s *Service) hashBlob(ctx context.Context, key string) (string, error) {
	rc, err := s.backend.Open(ctx, key)
	if err != nil {
		return "", fmt.Errorf("files.hashBlob: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("files.hashBlob: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// refusedWrite reports whether the record service REFUSED a ref-array write before touching the
// row: a ValidationError (a specs/048 before_save veto, a required or locked field, a version
// conflict — every one is raised before or instead of the single UPDATE) or the fail-closed
// ErrRecordHooksUnavailable. Nothing was written, so the file mutation that preceded it must be
// undone. Any other error is ambiguous (the write may have landed) and keeps the older handling.
func refusedWrite(err error) bool {
	var ve *services.ValidationError
	return errors.As(err, &ve) || errors.Is(err, services.ErrRecordHooksUnavailable)
}

// syncRecordRefs rebuilds the [{id,name}] reference array for a (record, field) from the live
// sys_files rows and writes it into the record's data JSON via the guard. It is the single point
// that keeps the record field consistent with the catalog after every mutation.
func (s *Service) syncRecordRefs(ctx context.Context, scope services.RecordScope, tenantID, entity, recordID, field string) error {
	rows, err := s.repo.ListByRecordField(ctx, tenantID, entity, recordID, field)
	if err != nil {
		return err
	}
	refs := make([]map[string]any, len(rows))
	for i, r := range rows {
		refs[i] = map[string]any{"id": r.ID, "name": r.Name}
	}
	return s.guard.SetFileRefs(ctx, scope, tenantID, entity, recordID, field, refs)
}

// StorageKey is storageKey exported for callers OUTSIDE this package that must address the same
// blob — today the tenant purge, which deletes a retired tenant's blobs after removing their
// sys_files rows. Exported rather than letting the caller re-spell "<tenant>/<id>": two copies of a
// key convention are two things that drift, and a drifted key silently deletes nothing.
func StorageKey(tenantID, id string) string { return storageKey(tenantID, id) }

// storageKey builds the opaque, server-generated backend key for a blob: "<tenant>/<id>". Both
// segments are server values (verified tenant + generated UUID), never client input, so the key
// cannot carry a path-traversal payload.
func storageKey(tenantID, id string) string {
	return tenantID + "/" + id
}

// sanitizeDisplayName trims the original filename and strips any directory components a browser
// might include, keeping only the base name for display. It is NEVER used to build a storage
// path (that is storageKey's server-generated value), so this is purely cosmetic hygiene.
func sanitizeDisplayName(name string) string {
	name = strings.TrimSpace(name)
	// Strip both unix and windows path separators.
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "file"
	}
	return name
}

// PublicURL is the download URL for a file id (the GET /files/:id route). Kept here so the
// service and handler agree on the shape; it is a relative API path the FE resolves.
func PublicURL(id string) string {
	return "/api/v1/files/" + id
}

// defaultIDGen is the production UUID generator. Tests inject their own via NewService.
var defaultIDGen = uuid.NewString
