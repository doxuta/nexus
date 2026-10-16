package tenantbackup

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// FormatVersion is the zip layout version written into every manifest.
const FormatVersion = 1

// Entry names. The zip is a trust boundary the moment it leaves the server (and a restore reads it
// back), so the layout is a closed allowlist: README.txt, manifest.json, tenant.json,
// tables/<table>.jsonl and blobs/<uuid>. Anything else — "../x", a nested path, a table the policy
// never exports — is TENANT_BACKUP_CORRUPT, not "ignored".
const (
	entryReadme   = "README.txt"
	entryManifest = "manifest.json"
	entryTenant   = "tenant.json"
	tablePrefix   = "tables/"
	tableSuffix   = ".jsonl"
	blobPrefix    = "blobs/"

	// maxManifestBytes bounds manifest.json however the header lies.
	maxManifestBytes = 32 << 20
)

var uuidRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func corrupt(format string, a ...any) *Error {
	return &Error{Code: "TENANT_BACKUP_CORRUPT", Msg: "Bản sao lưu bị hỏng: " + fmt.Sprintf(format, a...)}
}

// Manifest is manifest.json: what the file holds and what it deliberately leaves out.
type Manifest struct {
	FormatVersion int            `json:"formatVersion"`
	BackupID      string         `json:"backupId"`
	Tenant        ManifestTenant `json:"tenant"`
	Kind          string         `json:"kind"`
	CreatedAt     string         `json:"createdAt"`
	// CreatedBy is a LABEL only ("Họ tên (Tên xưởng)" / "Tự sao lưu theo lịch"): the file is handed
	// to the customer, so it never carries an operator user id or the workshop's id / email.
	CreatedBy       string             `json:"createdBy"`
	SchemaVersion   int64              `json:"schemaVersion"`
	SchemaDirty     bool               `json:"schemaDirty"`
	EngineBuild     string             `json:"engineBuild"`
	Tables          []ManifestTable    `json:"tables"`
	Files           ManifestFiles      `json:"files"`
	ExcludedTables  []ExcludedTable    `json:"excludedTables"`
	ExcludedColumns []ExcludedColumn   `json:"excludedColumns"`
	ExcludedJSON    []ExcludedJSONPath `json:"excludedJsonPaths"`
}

// ManifestTenant identifies the customer the file belongs to.
type ManifestTenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// ManifestTable describes one tables/<name>.jsonl entry.
type ManifestTable struct {
	Name    string   `json:"name"`
	Class   string   `json:"class"`
	Rows    int64    `json:"rows"`
	Columns []Column `json:"columns"`
	OwnerFK *OwnerFK `json:"ownerFk,omitempty"`
}

// ManifestFiles summarises the attached files: byte count and the ids that could not be copied
// faithfully (a missing blob or one whose bytes no longer match sys_files.sha256).
type ManifestFiles struct {
	Count       int      `json:"count"`
	Bytes       int64    `json:"bytes"`
	Missing     []string `json:"missing"`
	ShaMismatch []string `json:"shaMismatch"`
}

// ExcludedTable / ExcludedColumn / ExcludedJSONPath list what was left out, with the reason: the
// file must say what it does NOT contain.
type ExcludedTable struct {
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

type ExcludedColumn struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Reason string `json:"reason"`
}

type ExcludedJSONPath struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Path   string `json:"path"`
	Count  int    `json:"count"`
	Reason string `json:"reason"`
}

// Writer streams the zip. Tables are Deflated, attachment bytes Stored (already compressed).
type Writer struct {
	zw    *zip.Writer
	names map[string]bool
}

// NewWriter starts a zip on w. The caller closes w after Finish.
func NewWriter(w io.Writer) *Writer { return &Writer{zw: zip.NewWriter(w), names: map[string]bool{}} }

func (a *Writer) create(name string, method uint16) (io.Writer, error) {
	if a.names[name] {
		return nil, fmt.Errorf("tenantbackup.Writer: duplicate entry %q", name)
	}
	a.names[name] = true
	return a.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
}

// Table opens tables/<name>.jsonl. The table must be one the policy exports.
func (a *Writer) Table(name string) (io.Writer, error) {
	if !exportableTable(name) {
		return nil, fmt.Errorf("tenantbackup.Writer.Table: %q is not an exportable table", name)
	}
	return a.create(tablePrefix+name+tableSuffix, zip.Deflate)
}

// Blob copies one attachment into blobs/<id>, returning bytes written and the sha256 of what was
// actually copied (the caller compares it with sys_files.sha256).
func (a *Writer) Blob(id string, r io.Reader) (int64, string, error) {
	if !uuidRe.MatchString(id) {
		return 0, "", fmt.Errorf("tenantbackup.Writer.Blob: %q is not a uuid", id)
	}
	w, err := a.create(blobPrefix+id, zip.Store)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), r)
	if err != nil {
		return n, "", fmt.Errorf("tenantbackup.Writer.Blob %s: %w", id, err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// JSON writes a small JSON entry (tenant.json).
func (a *Writer) JSON(name string, v any) error {
	if name != entryTenant {
		return fmt.Errorf("tenantbackup.Writer.JSON: %q is not a JSON entry", name)
	}
	w, err := a.create(name, zip.Deflate)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(v)
}

// Finish writes README.txt and manifest.json LAST (the manifest carries the counts, known only
// now; zip readers go through the central directory so the order does not matter) and closes the
// zip stream.
func (a *Writer) Finish(readme string, m Manifest) error {
	w, err := a.create(entryReadme, zip.Deflate)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, readme); err != nil {
		return fmt.Errorf("tenantbackup.Writer.Finish: readme: %w", err)
	}
	mw, err := a.create(entryManifest, zip.Deflate)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(mw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("tenantbackup.Writer.Finish: manifest: %w", err)
	}
	if err := a.zw.Close(); err != nil {
		return fmt.Errorf("tenantbackup.Writer.Finish: close: %w", err)
	}
	return nil
}

func exportableTable(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return false
	}
	r, ok := RuleFor(name)
	return ok && r.Class.InFile()
}

// Reader reads a backup zip through its central directory after the allowlist check.
type Reader struct {
	files   map[string]*zip.File
	blobIDs []string
}

// OpenReader validates every entry name against the allowlist and indexes the zip. Any mismatch is
// TENANT_BACKUP_CORRUPT.
func OpenReader(ra io.ReaderAt, size int64) (*Reader, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, corrupt("không mở được tệp zip (%v)", err)
	}
	r := &Reader{files: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		name := f.Name
		if _, dup := r.files[name]; dup {
			return nil, corrupt("mục trùng tên %q", name)
		}
		switch {
		case name == entryReadme || name == entryManifest || name == entryTenant:
		case strings.HasPrefix(name, tablePrefix) && strings.HasSuffix(name, tableSuffix):
			if !exportableTable(strings.TrimSuffix(strings.TrimPrefix(name, tablePrefix), tableSuffix)) {
				return nil, corrupt("bảng không hợp lệ %q", name)
			}
		case strings.HasPrefix(name, blobPrefix):
			id := strings.TrimPrefix(name, blobPrefix)
			if !uuidRe.MatchString(id) {
				return nil, corrupt("tên tệp đính kèm không hợp lệ %q", name)
			}
			r.blobIDs = append(r.blobIDs, id)
		default:
			return nil, corrupt("mục lạ trong tệp %q", name)
		}
		r.files[name] = f
	}
	sort.Strings(r.blobIDs)
	return r, nil
}

// bounded stops a read at the size the entry claims: a header that under-reports the uncompressed
// size (a zip bomb) errors instead of inflating without limit, and any decompression / CRC fault is
// reported as a corrupt backup.
type bounded struct {
	r    io.Reader
	max  int64
	n    int64
	name string
}

func (b *bounded) Read(p []byte) (int, error) {
	k, err := b.r.Read(p)
	b.n += int64(k)
	if b.n > b.max {
		return k, corrupt("mục %q lớn hơn kích thước khai báo", b.name)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return k, corrupt("mục %q không đọc được (%v)", b.name, err)
	}
	return k, err
}

type entryReader struct {
	io.Reader
	closer io.Closer
}

func (e entryReader) Close() error { return e.closer.Close() }

func (r *Reader) open(name string, hardMax int64) (io.ReadCloser, error) {
	f, ok := r.files[name]
	if !ok {
		return nil, corrupt("thiếu mục %q", name)
	}
	max := int64(f.UncompressedSize64)
	if hardMax > 0 && max > hardMax {
		return nil, corrupt("mục %q quá lớn", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, corrupt("không mở được mục %q (%v)", name, err)
	}
	return entryReader{Reader: &bounded{r: io.LimitReader(rc, max+1), max: max, name: name}, closer: rc}, nil
}

// Manifest reads and decodes manifest.json.
func (r *Reader) Manifest() (*Manifest, error) {
	rc, err := r.open(entryManifest, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var m Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		var ce *Error
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, corrupt("manifest không đọc được (%v)", err)
	}
	return &m, nil
}

// TenantJSON returns tenant.json verbatim.
func (r *Reader) TenantJSON() (json.RawMessage, error) {
	rc, err := r.open(entryTenant, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// HasTable reports whether tables/<name>.jsonl is in the file.
func (r *Reader) HasTable(name string) bool {
	_, ok := r.files[tablePrefix+name+tableSuffix]
	return ok
}

// RowReader yields the lines of one table. Lines are read with bufio.Reader.ReadBytes, not a
// Scanner: one `data` JSON can be several MB and a Scanner's token cap would truncate it.
type RowReader struct {
	br *bufio.Reader
	rc io.ReadCloser
}

// ReadLine returns the next line (with its trailing newline), or io.EOF.
func (rr *RowReader) ReadLine() ([]byte, error) {
	line, err := rr.br.ReadBytes('\n')
	if len(line) > 0 {
		return line, nil
	}
	return nil, err
}

// Close releases the entry.
func (rr *RowReader) Close() error { return rr.rc.Close() }

// Table opens tables/<name>.jsonl.
func (r *Reader) Table(name string) (*RowReader, error) {
	rc, err := r.open(tablePrefix+name+tableSuffix, 0)
	if err != nil {
		return nil, err
	}
	return &RowReader{br: bufio.NewReaderSize(rc, 1<<20), rc: rc}, nil
}

// Blob opens blobs/<id>.
func (r *Reader) Blob(id string) (io.ReadCloser, error) {
	if !uuidRe.MatchString(id) {
		return nil, corrupt("tên tệp đính kèm không hợp lệ %q", id)
	}
	return r.open(blobPrefix+id, 0)
}

// BlobIDs lists the attachment ids in the file, sorted.
func (r *Reader) BlobIDs() []string { return append([]string(nil), r.blobIDs...) }

// Readme is README.txt: what the file is, what it holds and what it leaves out, in plain
// Vietnamese for the customer. It names the customer and the label of whoever made it — never an
// id or an email of the workshop.
func Readme(m Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BẢN SAO LƯU DỮ LIỆU — %s (%s)\n", m.Tenant.Name, m.Tenant.Slug)
	b.WriteString("=====================================================\n\n")
	fmt.Fprintf(&b, "Tạo lúc : %s\n", m.CreatedAt)
	fmt.Fprintf(&b, "Tạo bởi : %s\n", m.CreatedBy)
	fmt.Fprintf(&b, "Loại    : %s\n\n", m.Kind)
	b.WriteString("TỆP NÀY LÀ GÌ\n")
	b.WriteString("  Một bản chụp toàn bộ dữ liệu của hệ thống của khách tại thời điểm trên: hồ sơ, cấu hình,\n")
	b.WriteString("  phân quyền, quy trình và các tệp đính kèm. Dữ liệu của khách khác KHÔNG nằm trong tệp.\n\n")
	b.WriteString("BÊN TRONG\n")
	b.WriteString("  manifest.json        danh sách bảng, số dòng, cột, tệp đính kèm, những gì bị loại\n")
	b.WriteString("  tenant.json          thương hiệu\n")
	b.WriteString("  tables/<bảng>.jsonl  mỗi dòng là một bản ghi (JSON)\n")
	b.WriteString("  blobs/<mã>           nội dung thật của các tệp đính kèm\n\n")
	b.WriteString("KHÔNG CÓ TRONG TỆP (cố ý)\n")
	b.WriteString("  mật khẩu, mã xác thực hai bước và mã dự phòng, mọi token và phiên đăng nhập, khoá API,\n")
	b.WriteString("  khoá kết nối ngoài, webhook vào, khoá bí mật đăng nhập SSO, việc nền, thông tin thanh toán,\n")
	b.WriteString("  và khoá ký / header tuỳ chỉnh của các hành động gửi webhook ra.\n")
	for _, x := range m.ExcludedColumns {
		fmt.Fprintf(&b, "  - cột %s.%s: %s\n", x.Table, x.Column, x.Reason)
	}
	b.WriteString("\nLƯU Ý\n")
	b.WriteString("  Tệp vẫn chứa dữ liệu thật của khách (thông tin cá nhân, dữ liệu kinh doanh). Giữ ở nơi an toàn.\n")
	b.WriteString("  Khôi phục chỉ thực hiện được bởi đội ngũ Nexus, từ bản đang lưu trên máy chủ.\n")
	return b.String()
}
