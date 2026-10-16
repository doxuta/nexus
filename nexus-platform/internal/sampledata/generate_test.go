package sampledata

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var fixedNow = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

func opts(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func vals(s ...string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func mustRows(t *testing.T, e Entity, n int, batch string, c Candidates) []map[string]any {
	t.Helper()
	rows, skip := Rows(e, n, batch, fixedNow, c)
	if skip != "" {
		t.Fatalf("Rows skipped with %s, want rows", skip)
	}
	if len(rows) != n {
		t.Fatalf("len(rows) = %d, want %d", len(rows), n)
	}
	return rows
}

// TestRows_ByFieldType: one table row per engine field type (shape the validators read).
func TestRows_ByFieldType(t *testing.T) {
	cand := Candidates{Relation: map[string][]string{"accounts": {"acc-1", "acc-2"}}, Users: []string{"u-1"}}
	tests := []struct {
		name  string
		field Field
		check func(t *testing.T, v any)
	}{
		{"text maxLength", Field{Name: "note", Type: "text", Required: true, Options: opts("maxLength", float64(8))}, func(t *testing.T, v any) {
			s, ok := v.(string)
			if !ok || s == "" || utf8.RuneCountInString(s) > 8 {
				t.Fatalf("got %#v, want non-empty string of <= 8 runes", v)
			}
		}},
		{"email", Field{Name: "mail", Type: "email", Required: true}, func(t *testing.T, v any) {
			if s, _ := v.(string); !regexp.MustCompile(`^mau\.\d+\.[0-9a-f]{6}@example\.com$`).MatchString(s) {
				t.Fatalf("got %#v, want mau.<n>.<batch6>@example.com", v)
			}
		}},
		{"number range+step", Field{Name: "qty", Type: "number", Required: true, Options: opts("min", float64(10), "max", float64(50), "step", float64(5))}, func(t *testing.T, v any) {
			f, ok := v.(float64)
			if !ok || f < 10 || f > 50 || int(f)%5 != 0 {
				t.Fatalf("got %#v, want multiple of 5 in [10,50]", v)
			}
		}},
		{"currency default VND", Field{Name: "price", Type: "currency", Required: true}, func(t *testing.T, v any) {
			m, ok := v.(map[string]any)
			amt, _ := m["amount"].(float64)
			if !ok || m["currencyCode"] != "VND" || amt < 100000 || amt > 50000000 || int64(amt)%1000 != 0 {
				t.Fatalf("got %#v, want {amount 100k..50M step 1000, VND}", v)
			}
		}},
		{"currency configured code and bounds", Field{Name: "price", Type: "currency", Required: true, Options: opts("currencyCode", "USD", "min", float64(5), "max", float64(9))}, func(t *testing.T, v any) {
			m, _ := v.(map[string]any)
			amt, _ := m["amount"].(float64)
			if m["currencyCode"] != "USD" || amt < 5 || amt > 9 {
				t.Fatalf("got %#v, want USD within [5,9]", v)
			}
		}},
		{"rating", Field{Name: "stars", Type: "rating", Required: true, Options: opts("max", float64(10))}, func(t *testing.T, v any) {
			f, ok := v.(float64)
			if !ok || f < 1 || f > 10 || f != float64(int(f)) {
				t.Fatalf("got %#v, want integer in [1,10]", v)
			}
		}},
		{"boolean", Field{Name: "vip", Type: "boolean", Required: true}, func(t *testing.T, v any) {
			if _, ok := v.(bool); !ok {
				t.Fatalf("got %#v, want bool", v)
			}
		}},
		{"date", Field{Name: "d", Type: "date", Required: true}, func(t *testing.T, v any) {
			d, err := time.Parse("2006-01-02", fmt.Sprint(v))
			if err != nil || d.Before(fixedNow.AddDate(0, 0, -91)) || d.After(fixedNow.AddDate(0, 0, 91)) {
				t.Fatalf("got %#v (%v), want a date within 90 days of now", v, err)
			}
		}},
		{"datetime", Field{Name: "dt", Type: "datetime", Required: true}, func(t *testing.T, v any) {
			d, err := time.Parse(time.RFC3339, fmt.Sprint(v))
			if err != nil || d.Before(fixedNow.AddDate(0, 0, -91)) || d.After(fixedNow.AddDate(0, 0, 91)) {
				t.Fatalf("got %#v (%v), want RFC3339 within 90 days of now", v, err)
			}
		}},
		{"picklist", Field{Name: "st", Type: "picklist", Required: true, Options: opts("values", vals("Mới", "Đang xử lý"))}, func(t *testing.T, v any) {
			if v != "Mới" && v != "Đang xử lý" {
				t.Fatalf("got %#v, want one of options.values", v)
			}
		}},
		{"multi_select", Field{Name: "tags", Type: "multi_select", Required: true, Options: opts("values", vals("a", "b", "c"))}, func(t *testing.T, v any) {
			arr, ok := v.([]any)
			if !ok || len(arr) < 1 || len(arr) > 2 {
				t.Fatalf("got %#v, want []any of 1-2 values", v)
			}
		}},
		{"rich_text maxLength", Field{Name: "body", Type: "rich_text", Required: true, Options: opts("maxLength", float64(40))}, func(t *testing.T, v any) {
			s, _ := v.(string)
			if !strings.HasPrefix(s, "<p>") || !strings.HasSuffix(s, "</p>") || utf8.RuneCountInString(s) > 40 {
				t.Fatalf("got %q, want <p>…</p> within 40 runes", s)
			}
		}},
		{"composite", Field{Name: "contact", Type: "composite", Required: true, Options: opts("subFields", []any{
			map[string]any{"name": "city", "fieldType": "text"},
			map[string]any{"name": "mail", "fieldType": "email"},
		})}, func(t *testing.T, v any) {
			m, ok := v.(map[string]any)
			if !ok || m["city"] == nil || m["mail"] == nil || len(m) != 2 {
				t.Fatalf("got %#v, want object with both sub-fields", v)
			}
		}},
		{"relation required", Field{Name: "account", Type: "relation", Required: true, Options: opts("relatedEntity", "accounts")}, func(t *testing.T, v any) {
			if v != "acc-1" && v != "acc-2" {
				t.Fatalf("got %#v, want a candidate id", v)
			}
		}},
		{"polymorphic required", Field{Name: "related", Type: "polymorphic_relation", Required: true, Options: opts("targetEntities", vals("accounts"))}, func(t *testing.T, v any) {
			m, ok := v.(map[string]any)
			if !ok || m["type"] != "accounts" || (m["id"] != "acc-1" && m["id"] != "acc-2") {
				t.Fatalf("got %#v, want {type: accounts, id: candidate}", v)
			}
		}},
		{"user required", Field{Name: "owner", Type: "user", Required: true}, func(t *testing.T, v any) {
			if v != "u-1" {
				t.Fatalf("got %#v, want the user candidate", v)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := mustRows(t, Entity{Name: "things", Label: "Thing", Fields: []Field{tc.field}}, 6, "batch-1", cand)
			for _, r := range rows {
				tc.check(t, r[tc.field.Name])
			}
		})
	}
}

// TestRows_ServerComputedAndOptionalAreLeftOut: formula/lookup/rollup/auto_number never carry a
// value; optional file/image/user/polymorphic stay blank.
func TestRows_ServerComputedAndOptionalAreLeftOut(t *testing.T) {
	e := Entity{Name: "things", Fields: []Field{
		{Name: "f", Type: "formula"}, {Name: "l", Type: "lookup"}, {Name: "r", Type: "rollup"}, {Name: "a", Type: "auto_number"},
		{Name: "file", Type: "file"}, {Name: "img", Type: "image"}, {Name: "owner", Type: "user"},
		{Name: "rel", Type: "polymorphic_relation", Options: opts("targetEntities", vals("accounts"))},
		{Name: "name", Type: "text", Required: true},
	}}
	for _, r := range mustRows(t, e, 5, "b", Candidates{Users: []string{"u"}}) {
		if len(r) != 1 || r["name"] == nil {
			t.Fatalf("row = %#v, want only the required text", r)
		}
	}
}

func TestRows_Deterministic(t *testing.T) {
	e := Entity{Name: "customers", Label: "Khách hàng", Fields: []Field{
		{Name: "name", Label: "Tên", Type: "text", Required: true},
		{Name: "mail", Type: "email", Unique: true},
		{Name: "stars", Type: "rating"}, {Name: "d", Type: "date"},
	}}
	a, _ := Rows(e, 8, "batch-x", fixedNow, Candidates{})
	b, _ := Rows(e, 8, "batch-x", fixedNow, Candidates{})
	c, _ := Rows(e, 8, "batch-y", fixedNow, Candidates{})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same batch gave different rows:\n%v\n%v", a, b)
	}
	if reflect.DeepEqual(a, c) {
		t.Fatal("different batches gave identical rows")
	}
}

// TestRows_UniqueSuffixPerBatch: a second run on the same table must not collide on unique fields.
func TestRows_UniqueSuffixPerBatch(t *testing.T) {
	e := Entity{Name: "customers", Fields: []Field{
		{Name: "name", Type: "text", Required: true, Unique: true},
		{Name: "mail", Type: "email", Required: true, Unique: true},
		{Name: "code", Type: "number", Required: true, Unique: true, Options: opts("min", float64(1), "max", float64(999999999))},
		{Name: "pat", Type: "text", Unique: true, Options: opts("pattern", `[A-Z]{3}`)},
	}}
	seen := map[string]bool{}
	for _, batch := range []string{"b1", "b2", "b3"} {
		for _, r := range mustRows(t, e, 20, batch, Candidates{}) {
			for _, f := range []string{"name", "mail", "code"} {
				k := fmt.Sprint(f, "=", r[f])
				if seen[k] {
					t.Fatalf("duplicate unique value %s across rows/batches", k)
				}
				seen[k] = true
			}
			if _, has := r["pat"]; has && !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(fmt.Sprint(r["pat"])) {
				t.Fatalf("pat = %v: a patterned unique text gets no suffix and must match or be left out", r["pat"])
			}
		}
	}
}

// TestRows_UniqueNumberWithoutMaxAcrossBatches: a bare unique number (default range) must not fold
// onto a few small windows; step must still hold.
func TestRows_UniqueNumberWithoutMaxAcrossBatches(t *testing.T) {
	for name, o := range map[string]map[string]any{"bare": nil, "step": opts("step", float64(5))} {
		e := Entity{Name: "t", Fields: []Field{{Name: "no", Type: "number", Required: true, Unique: true, Options: o}}}
		seen := map[float64]bool{}
		for b := 0; b < 30; b++ {
			for _, r := range mustRows(t, e, 20, fmt.Sprintf("batch-%d", b), Candidates{}) {
				v, _ := r["no"].(float64)
				if seen[v] {
					t.Fatalf("%s: duplicate unique number %v across 30 batches", name, v)
				}
				seen[v] = true
				if name == "step" && math.Mod(v, 5) != 0 {
					t.Fatalf("step: %v is not a multiple of 5", v)
				}
			}
		}
	}
}

func TestRows_UniqueSuffixRespectsMaxLength(t *testing.T) {
	e := Entity{Name: "t", Fields: []Field{{Name: "name", Type: "text", Required: true, Unique: true, Options: opts("maxLength", float64(20))}}}
	for _, r := range mustRows(t, e, 20, "b1", Candidates{}) {
		if n := utf8.RuneCountInString(fmt.Sprint(r["name"])); n > 20 {
			t.Fatalf("name %q has %d runes, want <= 20", r["name"], n)
		}
	}
}

// TestRows_VietnameseHints: the field name decides what a text looks like.
func TestRows_VietnameseHints(t *testing.T) {
	e := Entity{Name: "customers", Label: "Khách hàng", Fields: []Field{
		{Name: "ho_ten", Label: "Họ tên", Type: "text", Required: true},
		{Name: "dien_thoai", Label: "Điện thoại", Type: "text", Required: true},
		{Name: "dia_chi", Label: "Địa chỉ", Type: "text", Required: true},
		{Name: "ma", Label: "Mã", Type: "text", Required: true},
		{Name: "other", Label: "Trường khác", Type: "text", Required: true},
	}}
	r := mustRows(t, e, 3, "b", Candidates{})[0]
	if !regexp.MustCompile(`^09\d\d \d{3} \d{3}$`).MatchString(fmt.Sprint(r["dien_thoai"])) {
		t.Errorf("dien_thoai = %v, want 09xx xxx xxx", r["dien_thoai"])
	}
	if name := fmt.Sprint(r["ho_ten"]); len(strings.Fields(name)) < 2 || !regexp.MustCompile(`\p{L}`).MatchString(name) {
		t.Errorf("ho_ten = %q, want a Vietnamese full name", name)
	}
	if !strings.Contains(fmt.Sprint(r["dia_chi"]), ",") {
		t.Errorf("dia_chi = %v, want a street, district, city address", r["dia_chi"])
	}
	if s := fmt.Sprint(r["ma"]); s == "" {
		t.Errorf("ma empty")
	}
	if s := fmt.Sprint(r["other"]); !strings.HasPrefix(s, "Mẫu ") {
		t.Errorf("other = %q, want the generic \"Mẫu <label> <n>\" fallback", s)
	}
}

// TestRows_PatternPicksAMatchingForm: a phone pattern without spaces still gets valid rows.
func TestRows_PatternPicksAMatchingForm(t *testing.T) {
	e := Entity{Name: "c", Fields: []Field{{Name: "sdt", Type: "text", Required: true, Options: opts("pattern", `0\d{9}`)}}}
	for _, r := range mustRows(t, e, 5, "b", Candidates{}) {
		if !regexp.MustCompile(`^0\d{9}$`).MatchString(fmt.Sprint(r["sdt"])) {
			t.Fatalf("sdt = %v does not match the field pattern", r["sdt"])
		}
	}
}

func TestRows_DependentPicklist(t *testing.T) {
	e := Entity{Name: "c", Fields: []Field{
		// child listed BEFORE its controlling field on purpose
		{Name: "district", Type: "picklist", Required: true, Options: opts(
			"values", vals("Ba Đình", "Hoàn Kiếm", "Hải Châu"),
			"controllingField", "province",
			"dependentValues", map[string]any{"Hà Nội": vals("Ba Đình", "Hoàn Kiếm"), "Đà Nẵng": vals("Hải Châu")})},
		{Name: "province", Type: "picklist", Options: opts("values", vals("Hà Nội", "Đà Nẵng"))},
	}}
	allowed := map[string][]string{"Hà Nội": {"Ba Đình", "Hoàn Kiếm"}, "Đà Nẵng": {"Hải Châu"}}
	for _, r := range mustRows(t, e, 20, "b", Candidates{}) {
		p, _ := r["province"].(string)
		ok := false
		for _, d := range allowed[p] {
			ok = ok || d == r["district"]
		}
		if p == "" || !ok {
			t.Fatalf("row %v: district is not valid for province", r)
		}
	}
}

func TestRows_OptionalRelationIsSometimesFilled(t *testing.T) {
	e := Entity{Name: "c", Fields: []Field{{Name: "account", Type: "relation", Options: opts("relatedEntity", "accounts")}}}
	filled := 0
	for _, r := range mustRows(t, e, 20, "b", Candidates{Relation: map[string][]string{"accounts": {"a1"}}}) {
		if r["account"] != nil {
			filled++
		}
	}
	if filled == 0 || filled == 20 {
		t.Fatalf("filled %d/20 optional relations, want some but not all (~70%%)", filled)
	}
}

func TestRows_OneToOneRelationUsesEachParentOnce(t *testing.T) {
	e := Entity{Name: "c", Fields: []Field{{Name: "account", Type: "relation", Required: true, Options: opts("relatedEntity", "accounts", "cardinality", "1-1")}}}
	seen := map[any]bool{}
	for _, r := range mustRows(t, e, 3, "b", Candidates{Relation: map[string][]string{"accounts": {"a1", "a2", "a3"}}}) {
		if seen[r["account"]] {
			t.Fatalf("1-1 parent %v used twice", r["account"])
		}
		seen[r["account"]] = true
	}
}

func TestRows_Skips(t *testing.T) {
	tests := []struct {
		name  string
		field Field
		cand  Candidates
		want  string
	}{
		{"required file", Field{Name: "f", Type: "file", Required: true}, Candidates{}, "SAMPLE_FILE_REQUIRED"},
		{"required image", Field{Name: "f", Type: "image", Required: true}, Candidates{}, "SAMPLE_FILE_REQUIRED"},
		{"required user, none", Field{Name: "u", Type: "user", Required: true}, Candidates{}, "SAMPLE_NO_USER"},
		{"required relation, no parent rows", Field{Name: "a", Type: "relation", Required: true, Options: opts("relatedEntity", "accounts")}, Candidates{}, "SAMPLE_PARENT_EMPTY"},
		{"required polymorphic, no target rows", Field{Name: "p", Type: "polymorphic_relation", Required: true, Options: opts("targetEntities", vals("accounts"))}, Candidates{}, "SAMPLE_PARENT_EMPTY"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, skip := Rows(Entity{Name: "c", Fields: []Field{tc.field}}, 3, "b", fixedNow, tc.cand)
			if skip != tc.want || rows != nil {
				t.Fatalf("Rows = (%v, %q), want (nil, %q)", rows, skip, tc.want)
			}
		})
	}
}

// TestHandles_KnownTypes: services pins the real engine list against Handles in its own test.
func TestHandles_KnownTypes(t *testing.T) {
	for _, ft := range []string{"text", "number", "boolean", "date", "datetime", "email", "picklist", "relation",
		"multi_select", "currency", "rating", "rich_text", "composite", "file", "image", "formula", "lookup",
		"rollup", "auto_number", "polymorphic_relation", "user"} {
		if !Handles(ft) {
			t.Errorf("Handles(%q) = false", ft)
		}
	}
	if Handles("hologram") {
		t.Error("Handles(hologram) = true")
	}
}
