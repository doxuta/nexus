package dynrepo

import (
	"strings"
	"testing"
)

// #283 rowscope-by-user-field — the access level "records whose <user field> is me", encoded as
// "field:<name>" in acl_role_actions.access_level. The predicate reads the user field's generated
// `<name>_id` column (ddl/create_table.go) next to the owner term, so the record's creator keeps
// seeing what they made.

func TestFieldAccessLevel_RoundTrip(t *testing.T) {
	cases := []struct {
		raw     string
		field   string
		wantOK  bool
		comment string
	}{
		{"field:bac_si", "bac_si", true, "plain user field"},
		{"field:giao_vien2", "giao_vien2", true, "digits allowed"},
		{"field:", "", false, "empty field"},
		{"field:Bac_Si", "", false, "uppercase rejected (identifiers are lowercase snake_case)"},
		{"field:bac si", "", false, "space rejected"},
		{"field:x`; DROP TABLE users;--", "", false, "injection attempt rejected"},
		{"field:" + strings.Repeat("a", 27), "", false, "over the 26-char identifier cap"},
		{"owner", "", false, "not a field level"},
		{"", "", false, "empty"},
	}
	for _, c := range cases {
		t.Run(c.comment, func(t *testing.T) {
			f, ok := AccessLevel(c.raw).UserField()
			if ok != c.wantOK || f != c.field {
				t.Fatalf("UserField(%q) = (%q,%v); want (%q,%v)", c.raw, f, ok, c.field, c.wantOK)
			}
		})
	}
	if got := FieldAccessLevel("bac_si"); got != "field:bac_si" {
		t.Fatalf("FieldAccessLevel = %q", got)
	}
}

func TestRowFilter_FieldLevel(t *testing.T) {
	const uid = "doc-1"

	t.Run("unaliased: owner OR <field>_id", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: FieldAccessLevel("bac_si")}.rowFilter()
		want := " AND (COALESCE(`owner_id`, `created_by`) = ? OR `bac_si_id` = ?)"
		if sql != want {
			t.Fatalf("sql = %q; want %q", sql, want)
		}
		if len(args) != 2 || args[0] != uid || args[1] != uid {
			t.Fatalf("args = %v; want [%s %s]", args, uid, uid)
		}
	})

	t.Run("aliased for a joined table", func(t *testing.T) {
		sql, _ := RowScope{UserID: uid, Level: FieldAccessLevel("bac_si")}.rowFilterFor("j0")
		want := " AND (COALESCE(`j0`.`owner_id`, `j0`.`created_by`) = ? OR `j0`.`bac_si_id` = ?)"
		if sql != want {
			t.Fatalf("sql = %q; want %q", sql, want)
		}
	})

	t.Run("malformed field name fails closed", func(t *testing.T) {
		for _, lvl := range []AccessLevel{"field:", "field:Bad Name", "field:a`b", "field:" + AccessLevel(strings.Repeat("z", 40))} {
			sql, args := RowScope{UserID: uid, Level: lvl}.rowFilter()
			if !strings.Contains(sql, "1=0") || len(args) != 0 {
				t.Fatalf("level %q: sql=%q args=%v; want 1=0", lvl, sql, args)
			}
		}
	})

	t.Run("admin still sees all", func(t *testing.T) {
		sql, args := RowScope{UserID: uid, Level: FieldAccessLevel("bac_si"), IsAdmin: true}.rowFilter()
		if sql != "" || len(args) != 0 {
			t.Fatalf("admin: sql=%q args=%v", sql, args)
		}
	})

	t.Run("manual share is one more OR term on reads", func(t *testing.T) {
		s, err := RowScope{UserID: uid, Level: FieldAccessLevel("bac_si"), IncludeShares: true}.forRead("t-1", "nk_lich_hen")
		if err != nil {
			t.Fatal(err)
		}
		sql, args := s.rowFilter()
		if !strings.Contains(sql, "`bac_si_id` = ?") || !strings.Contains(sql, "sys_record_shares") {
			t.Fatalf("sql = %q", sql)
		}
		if len(args) != 5 { // owner, field, share tenant, share entity, share user
			t.Fatalf("args = %v; want 5", args)
		}
	})

	t.Run("share-only marker excludes the share term but keeps the field term", func(t *testing.T) {
		s, _ := RowScope{UserID: uid, Level: FieldAccessLevel("bac_si"), IncludeShares: true}.forRead("t-1", "nk_lich_hen")
		expr, args := s.sharedOnlyExpr()
		if !strings.Contains(expr, "`bac_si_id` = ?") || strings.Contains(expr, "sys_record_shares") || len(args) != 2 {
			t.Fatalf("expr=%q args=%v", expr, args)
		}
	})
}

func TestAccessLevel_IsOwnerScoped(t *testing.T) {
	for lvl, want := range map[AccessLevel]bool{
		AccessOwner: true, AccessTeam: true, FieldAccessLevel("bac_si"): true,
		AccessAll: false, AccessNone: false, "bogus": false,
	} {
		if got := lvl.IsOwnerScoped(); got != want {
			t.Errorf("IsOwnerScoped(%q) = %v; want %v", lvl, got, want)
		}
	}
}
