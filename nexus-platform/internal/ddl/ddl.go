// Package ddl builds the data-definition statements (CREATE TABLE / ALTER) that the
// metadata engine runs to materialize obj_* entity tables. Identifiers in DDL cannot
// be parameterized with `?`, so every identifier MUST pass SanitizeIdentifier before
// it is composed into a statement — this is the project's primary defense against SQL
// injection on the dynamic-schema path.
package ddl

import (
	"fmt"
	"regexp"
	"strings"
)

// maxIdentifierLen caps user-supplied entity/field names so the LONGEST identifier the
// DDL engine derives from them stays within MySQL's hard 64-character limit. The engine
// never stores a name as-is: it composes prefixed/suffixed identifiers, and the longest
// one is the relation FK index built in BuildAddRelationColumn / BuildCreateTable:
//
//	idx_ + obj_ + <entity> + _ + <field> + _id   = 12 + len(entity) + len(field)
//	 (4)    (4)              (1)            (3)
//
// Both <entity> and <field> can independently be at the cap, so the worst case is
// 12 + 2*cap. Solving 12 + 2*cap <= 64 gives cap <= 26. We pick 26: an entity AND a
// field both at exactly 26 chars yield a 64-char index name, the maximum MySQL allows.
// (The unique index uq_obj_<entity>_<field> = 8 + entity + field is shorter, so the
// relation index is the binding constraint.) Rejecting over-long names HERE — at
// SanitizeIdentifier, called from entity/field validation — turns what was a cryptic
// runtime DDL failure (ER_TOO_LONG_IDENT / 500) into a clean 422 at create time.
const maxIdentifierLen = 26

// identifierPattern allows lowercase snake_case starting with a letter, with a length
// capped at maxIdentifierLen total chars (first char + up to maxIdentifierLen-1 more).
// This is intentionally stricter than MySQL: it keeps generated table/column/index names
// predictable, safe to inline into DDL, and guaranteed to fit MySQL's 64-char identifier
// limit even after the engine prefixes/suffixes them (see maxIdentifierLen).
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,25}$`)

// maxComposedIdentifierLen is MySQL's own identifier limit. It bounds names the ENGINE composed
// from parts that were each already sanitized — never a name a user typed.
const maxComposedIdentifierLen = 64

// composedIdentifierPattern is identifierPattern's charset rule without the component-length cap.
var composedIdentifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// SanitizeComposedIdentifier validates an identifier the engine BUILT by joining sanitized parts —
// "obj_<entity>", "rel_<a>_<b>", "<field>_id" — rather than one a caller supplied.
//
// Why this exists as a second function instead of relaxing SanitizeIdentifier: the 26-char cap is
// not a charset rule, it is a BUDGET. Each component must stay short so the index names the engine
// derives FROM it ("idx_rel_<a>_<b>_<col>") still fit MySQL's 64 chars. Applying that budget again
// to the already-composed result double-counts it and rejects perfectly legal tables.
//
// It was not theoretical: the shipped CRM pack's only N-N link table is
// "rel_crm_contacts_crm_opportunities" (34 chars), so re-sanitizing the composed name in the report
// JOIN builder made EVERY N-N report join fail on the pack we ship — while the older N-N path
// (dynrepo/relation.go), which uses ddl.JoinParts' result directly, worked fine. Defense-in-depth
// is kept: the charset is still enforced, so nothing quoted, spaced or punctuated can reach SQL.
func SanitizeComposedIdentifier(name string) (string, error) {
	if strings.TrimSpace(name) != name {
		return "", fmt.Errorf("ddl.SanitizeComposedIdentifier: identifier %q has surrounding whitespace", name)
	}
	if len(name) > maxComposedIdentifierLen {
		return "", fmt.Errorf("ddl.SanitizeComposedIdentifier: %q is %d chars, over MySQL's %d-char identifier limit",
			name, len(name), maxComposedIdentifierLen)
	}
	if !composedIdentifierPattern.MatchString(name) {
		return "", fmt.Errorf("ddl.SanitizeComposedIdentifier: %q must match ^[a-z][a-z0-9_]*$ (lowercase snake_case)", name)
	}
	return name, nil
}

// reservedWords are SQL keywords disallowed as entity/field identifiers to avoid
// ambiguous or broken DDL. Not exhaustive of MySQL's full list, but covers the
// keywords most likely to collide on the dynamic-schema path.
var reservedWords = map[string]struct{}{
	"select": {}, "insert": {}, "update": {}, "delete": {}, "drop": {}, "create": {},
	"alter": {}, "table": {}, "from": {}, "where": {}, "order": {}, "group": {},
	"join": {}, "union": {}, "index": {}, "into": {}, "values": {}, "set": {},
	"and": {}, "or": {}, "not": {}, "null": {}, "primary": {}, "key": {}, "foreign": {},
	"references": {}, "default": {}, "unique": {}, "database": {}, "schema": {},
	"grant": {}, "having": {}, "limit": {}, "offset": {}, "as": {}, "on": {}, "in": {},
	"like": {}, "between": {}, "exists": {}, "distinct": {}, "case": {}, "when": {},
	"then": {}, "else": {}, "end": {}, "desc": {}, "asc": {}, "add": {}, "column": {},
	"constraint": {}, "truncate": {}, "replace": {}, "rename": {}, "use": {}, "lock": {},
	"begin": {}, "commit": {}, "rollback": {}, "trigger": {}, "procedure": {}, "view": {},
	"with": {}, "true": {}, "false": {}, "by": {},
	// Static API route segments — an entity with these names would be shadowed by the
	// static /admin, /metadata, /health, /auth, /api routes in the dynamic dispatcher.
	"admin": {}, "metadata": {}, "health": {}, "auth": {}, "api": {},
}

// SanitizeIdentifier validates an entity/field name as given (it never lowercases) against
// the allowlist. It returns the identifier or an error explaining the rejection. A name that fails here must never reach a DDL statement.
func SanitizeIdentifier(name string) (string, error) {
	if strings.TrimSpace(name) != name {
		// Reject surrounding whitespace rather than silently trimming, so callers are
		// forced to pass clean names (a trailing space would otherwise pass the regex
		// after normalization and hide a malformed identifier).
		return "", fmt.Errorf("ddl.SanitizeIdentifier: identifier %q has surrounding whitespace", name)
	}
	// Validate as-is — do NOT lowercase. Silently lowercasing a camelCase field name
	// (e.g. "isActive" -> "isactive") would let the field be created but never written
	// under its declared key, since record validation matches the stored lowercase name.
	// Identifiers must be lowercase snake_case up front.
	if !identifierPattern.MatchString(name) {
		return "", fmt.Errorf("ddl.SanitizeIdentifier: %q must match ^[a-z][a-z0-9_]{0,25}$ (lowercase snake_case, max %d chars so generated index names stay within MySQL's 64-char limit)", name, maxIdentifierLen)
	}
	if _, reserved := reservedWords[name]; reserved {
		return "", fmt.Errorf("ddl.SanitizeIdentifier: %q is a reserved word", name)
	}
	return name, nil
}
