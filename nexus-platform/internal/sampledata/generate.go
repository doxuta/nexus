// Package sampledata generates Vietnamese sample rows from a table's field definitions
// (specs/064-preview-as-role-sample-data S9-5). It is pure: no database, no HTTP, no dependency
// beyond the standard library. The caller (services.SampleDataService) reads the metadata, feeds
// it in as Entity/Field, hands the rows to RecordService.CreateBulk and so every ordinary
// validation, relation check and auto_number assignment still runs on them.
//
// Values use the Go shapes the record validators read from decoded JSON: numbers are float64,
// arrays []any, objects map[string]any.
package sampledata

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Codes a table is skipped with (they are not HTTP codes; the response lists them under "skipped").
const (
	CodeFileRequired = "SAMPLE_FILE_REQUIRED"
	CodeNoUser       = "SAMPLE_NO_USER"
	CodeParentEmpty  = "SAMPLE_PARENT_EMPTY"
)

// Field is one column of a table as the generator needs it (a projection of sys_fields).
type Field struct {
	Name, Label, Type string
	Required, Unique  bool
	Options           map[string]any
}

// Entity is one table with its live fields.
type Entity struct {
	Name, Label string
	Fields      []Field
}

// Candidates are the existing records new rows may point at.
type Candidates struct {
	Relation map[string][]string // entity name -> record ids
	Users    []string            // active user ids of the tenant
}

// serverComputed fields get their value from the server; the generator never sends one.
var serverComputed = map[string]bool{"formula": true, "lookup": true, "rollup": true, "auto_number": true}

var handled = map[string]bool{
	"text": true, "number": true, "boolean": true, "date": true, "datetime": true, "email": true,
	"picklist": true, "relation": true, "multi_select": true, "currency": true, "rating": true,
	"rich_text": true, "composite": true, "file": true, "image": true, "polymorphic_relation": true,
	"user": true, "formula": true, "lookup": true, "rollup": true, "auto_number": true,
}

// Handles reports whether the generator knows what to do with a field type (services pins this
// against the engine's allowed field types so a new type cannot slip in unnoticed).
func Handles(fieldType string) bool { return handled[fieldType] }

// Rows builds n rows for e. batch seeds the generator (the same batch gives the same rows) and
// tags unique values so a second run never collides with the first. A non-empty skip code means
// the table cannot be filled right now (a required field has nothing to point at) and rows is nil.
func Rows(e Entity, n int, batch string, now time.Time, c Candidates) (rows []map[string]any, skip string) {
	if code := blocker(e, c); code != "" {
		return nil, code
	}
	seed := hash64(batch + "|" + e.Name)
	g := &gen{
		e: e, r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), now: now.UTC(), c: c,
		tag: fmt.Sprintf("%06x", hash64(batch)&0xffffff), batchNum: hash64(batch) % 1_000_000,
		pat: map[string]*regexp.Regexp{}, cat: map[string]category{}, entTok: tokens(e.Name + " " + e.Label),
		dependents: map[string]Field{},
	}
	fields := orderFields(e.Fields)
	for _, f := range fields {
		if f.Type == "text" {
			if p, _ := f.Options["pattern"].(string); p != "" {
				// An invalid pattern is a config defect the server reports per row; nothing to match here.
				if re, err := regexp.Compile("^(?:" + p + ")$"); err == nil {
					g.pat[f.Name] = re
				}
			}
		}
		if parent, _ := f.Options["controllingField"].(string); f.Type == "picklist" && parent != "" {
			if _, seen := g.dependents[parent]; !seen {
				g.dependents[parent] = f
			}
		}
	}
	rows = make([]map[string]any, n)
	for i := range rows {
		row := map[string]any{}
		for _, f := range fields {
			if v, ok := g.value(f, i+1, row); ok {
				row[f.Name] = v
			}
		}
		rows[i] = row
	}
	return rows, ""
}

// blocker names the first reason a required field cannot be filled (file before user before parent).
func blocker(e Entity, c Candidates) string {
	code := ""
	rank := map[string]int{"": 0, CodeParentEmpty: 1, CodeNoUser: 2, CodeFileRequired: 3}
	set := func(s string) {
		if rank[s] > rank[code] {
			code = s
		}
	}
	for _, f := range e.Fields {
		if !f.Required {
			continue
		}
		switch f.Type {
		case "file", "image":
			set(CodeFileRequired)
		case "user":
			if len(c.Users) == 0 {
				set(CodeNoUser)
			}
		case "relation":
			if t, _ := f.Options["relatedEntity"].(string); len(c.Relation[t]) == 0 {
				set(CodeParentEmpty)
			}
		case "polymorphic_relation":
			if len(polyTargets(f, c)) == 0 {
				set(CodeParentEmpty)
			}
		}
	}
	return code
}

// polyTargets lists the targetEntities of f that have candidate records.
func polyTargets(f Field, c Candidates) []string {
	var out []string
	list, _ := f.Options["targetEntities"].([]any)
	for _, t := range list {
		if s, _ := t.(string); s != "" && len(c.Relation[s]) > 0 {
			out = append(out, s)
		}
	}
	return out
}

type gen struct {
	e          Entity
	r          *rand.Rand
	now        time.Time
	c          Candidates
	tag        string
	batchNum   uint64
	pat        map[string]*regexp.Regexp // compiled once per field, never per row
	cat        map[string]category
	entTok     map[string]bool
	dependents map[string]Field // controlling picklist name -> its first dependent child
}

// orderFields puts a dependent picklist after the picklist that controls it (stable otherwise).
func orderFields(fields []Field) []Field {
	byName := make(map[string]Field, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}
	depth := func(f Field) int {
		d := 0
		for cur := f; d <= len(fields); d++ {
			parent, _ := cur.Options["controllingField"].(string)
			p, ok := byName[parent]
			if cur.Type != "picklist" || parent == "" || !ok {
				break
			}
			cur = p
		}
		return d
	}
	out := make([]Field, 0, len(fields))
	maxDepth := 0
	depths := make([]int, len(fields))
	for i, f := range fields {
		depths[i] = depth(f)
		maxDepth = max(maxDepth, depths[i])
	}
	for d := 0; d <= maxDepth; d++ {
		for i, f := range fields {
			if depths[i] == d {
				out = append(out, f)
			}
		}
	}
	return out
}

// value generates one field of row n (1-based); ok=false leaves the field out of the row.
func (g *gen) value(f Field, n int, row map[string]any) (any, bool) {
	if serverComputed[f.Type] {
		return nil, false
	}
	switch f.Type {
	case "text":
		return g.text(f, n)
	case "email":
		return fmt.Sprintf("mau.%d.%s@example.com", n, g.tag), true
	case "number":
		return g.number(f, n), true
	case "currency":
		return g.currency(f), true
	case "rating":
		lo, hi := 1, 5
		if v, ok := optInt(f.Options, "min"); ok {
			lo = v
		}
		if v, ok := optInt(f.Options, "max"); ok {
			hi = v
		}
		return float64(lo + g.r.IntN(max(hi-lo, 0)+1)), true
	case "boolean":
		return g.r.IntN(2) == 0, true
	case "date":
		return g.at().Format("2006-01-02"), true
	case "datetime":
		return g.at().Add(time.Duration(8+g.r.IntN(10))*time.Hour + time.Duration(g.r.IntN(60))*time.Minute).Format(time.RFC3339), true
	case "picklist":
		return g.picklist(f, row)
	case "multi_select":
		values := stringList(f.Options["values"])
		if len(values) == 0 {
			return nil, false
		}
		first := g.r.IntN(len(values))
		out := []any{values[first]}
		if len(values) > 1 && g.r.IntN(2) == 0 {
			out = append(out, values[(first+1+g.r.IntN(len(values)-1))%len(values)])
		}
		return out, true
	case "rich_text":
		inner := sentences[g.r.IntN(len(sentences))]
		if m, ok := optInt(f.Options, "maxLength"); ok && m > 0 {
			inner = truncate(inner, m-len("<p></p>"))
		}
		return "<p>" + inner + "</p>", true
	case "composite":
		return g.composite(f, n, row)
	case "relation":
		return g.relation(f, n)
	case "polymorphic_relation":
		if !f.Required {
			return nil, false
		}
		targets := polyTargets(f, g.c)
		t := targets[g.r.IntN(len(targets))]
		ids := g.c.Relation[t]
		return map[string]any{"type": t, "id": ids[g.r.IntN(len(ids))]}, true
	case "user":
		if !f.Required || len(g.c.Users) == 0 {
			return nil, false
		}
		return g.c.Users[g.r.IntN(len(g.c.Users))], true
	}
	return nil, false // file, image (optional) and any type this version does not know
}

// at is a random day within 90 days of now.
func (g *gen) at() time.Time {
	d := g.now.Truncate(24 * time.Hour)
	return d.AddDate(0, 0, g.r.IntN(181)-90)
}

func (g *gen) relation(f Field, n int) (any, bool) {
	target, _ := f.Options["relatedEntity"].(string)
	ids := g.c.Relation[target]
	if len(ids) == 0 || (!f.Required && g.r.Float64() >= 0.7) {
		return nil, false
	}
	oneToOne, _ := f.Options["cardinality"].(string)
	if f.Unique || oneToOne == "1-1" {
		if n-1 < len(ids) {
			return ids[n-1], true
		}
		if !f.Required {
			return nil, false
		}
	}
	return ids[g.r.IntN(len(ids))], true
}

func (g *gen) picklist(f Field, row map[string]any) (any, bool) {
	values := stringList(f.Options["values"])
	if len(values) == 0 {
		return nil, false
	}
	if parent, _ := f.Options["controllingField"].(string); parent != "" {
		// A dependent value is only valid for the parent's current value.
		pv, _ := row[parent].(string)
		deps, _ := f.Options["dependentValues"].(map[string]any)
		allowed := stringList(deps[pv])
		if pv == "" || len(allowed) == 0 {
			return nil, false
		}
		return allowed[g.r.IntN(len(allowed))], true
	}
	if child, ok := g.dependents[f.Name]; ok {
		// A controlling picklist is always filled, with a value its child has a list for.
		deps, _ := child.Options["dependentValues"].(map[string]any)
		var withList []string
		for _, v := range values {
			if len(stringList(deps[v])) > 0 {
				withList = append(withList, v)
			}
		}
		if len(withList) > 0 {
			return withList[g.r.IntN(len(withList))], true
		}
	}
	return values[g.r.IntN(len(values))], true
}

func (g *gen) composite(f Field, n int, _ map[string]any) (any, bool) {
	subs, _ := f.Options["subFields"].([]any)
	out := map[string]any{}
	sub := &gen{e: g.e, r: g.r, now: g.now, c: g.c, tag: g.tag, batchNum: g.batchNum, pat: g.pat, cat: g.cat, entTok: g.entTok, dependents: map[string]Field{}}
	for _, raw := range subs {
		m, _ := raw.(map[string]any)
		name, _ := m["name"].(string)
		ft, _ := m["fieldType"].(string)
		opts, _ := m["options"].(map[string]any)
		required, _ := m["required"].(bool)
		if name == "" || serverComputed[ft] || ft == "composite" || ft == "relation" {
			continue
		}
		if v, ok := sub.value(Field{Name: f.Name + "." + name, Label: name, Type: ft, Required: required, Options: opts}, n, out); ok {
			out[name] = v
		}
	}
	return out, len(out) > 0
}

func (g *gen) number(f Field, n int) float64 {
	lo, hi := 1.0, 1000.0
	mn, hasMin := optFloat(f.Options, "min")
	mx, hasMax := optFloat(f.Options, "max")
	switch {
	case hasMin && hasMax:
		lo, hi = mn, mx
	case hasMin:
		lo, hi = mn, mn+999
	case hasMax:
		lo, hi = math.Min(1, mx), mx
	}
	if hi < lo {
		hi = lo
	}
	if f.Unique {
		// A unique number walks a per-batch offset (batchNum*100+n) so two batches do not collide.
		// Without an explicit max the range is wide: the 1..1000 default would fold every batch
		// onto ten 20-number windows.
		if !hasMax {
			hi = lo + 1e9
		}
		step := 1.0
		if st, ok := optFloat(f.Options, "step"); ok && st > 0 {
			step = st
		}
		first := math.Ceil(lo/step) * step
		if first > hi {
			return lo
		}
		slots := math.Floor((hi-first)/step) + 1
		k := math.Mod(float64(g.batchNum*100)+float64(n), slots)
		return math.Round((first+step*k)*1e9) / 1e9
	}
	if step, ok := optFloat(f.Options, "step"); ok && step > 0 {
		first := math.Ceil(lo/step) * step
		if first > hi {
			return lo
		}
		return math.Round((first+step*float64(g.r.IntN(int((hi-first)/step)+1)))*1e9) / 1e9
	}
	lo, hi = math.Ceil(lo), math.Floor(hi)
	if hi < lo {
		return lo
	}
	return lo + float64(g.r.Int64N(int64(hi-lo)+1))
}

func (g *gen) currency(f Field) any {
	code := "VND"
	if c, _ := f.Options["currencyCode"].(string); c != "" {
		code = c
	}
	lo, hi := 100000.0, 50000000.0
	mn, hasMin := optFloat(f.Options, "min")
	mx, hasMax := optFloat(f.Options, "max")
	if hasMin {
		lo = mn
	}
	if hasMax {
		hi = mx
	}
	if hi < lo {
		hi = lo
	}
	var amount float64
	if first := math.Ceil(lo/1000) * 1000; first+1000 <= hi {
		amount = first + 1000*float64(g.r.Int64N(int64((hi-first)/1000)+1))
	} else {
		lo, hi = math.Ceil(lo), math.Floor(hi)
		amount = lo + float64(g.r.Int64N(int64(max(hi-lo, 0))+1))
	}
	return map[string]any{"amount": amount, "currencyCode": code}
}

// ---- text ----

type category int

const (
	catGeneric category = iota
	catPerson
	catGiven
	catFamily
	catCompany
	catService
	catPhone
	catAddress
	catCode
	catNote
)

func (g *gen) category(f Field) category {
	key := f.Name + "|" + f.Label
	if c, ok := g.cat[key]; ok {
		return c
	}
	t := tokens(f.Name + " " + f.Label)
	has := func(words ...string) bool {
		for _, w := range words {
			if t[w] {
				return true
			}
		}
		return false
	}
	ctx := func(words ...string) bool {
		for _, w := range words {
			if t[w] || g.entTok[w] {
				return true
			}
		}
		return false
	}
	c := catGeneric
	switch {
	case has("sdt", "phone", "dienthoai", "mobile", "tel", "hotline", "zalo"):
		c = catPhone
	case has("diachi", "address"):
		c = catAddress
	case has("first", "given"):
		c = catGiven
	case has("last", "family", "surname"):
		c = catFamily
	case has("ten", "name", "hoten", "fullname"):
		switch {
		case ctx("congty", "company", "doanhnghiep", "organization", "tochuc", "account", "accounts"):
			c = catCompany
		case ctx("dichvu", "service", "services", "sanpham", "product", "products", "khoahoc", "course"):
			c = catService
		default:
			c = catPerson
		}
	case has("ma", "code", "sku"):
		c = catCode
	case has("ghichu", "note", "mota", "description", "noidung", "tieude", "title", "subject"):
		c = catNote
	}
	g.cat[key] = c
	return c
}

// text returns the value of a text field: a hint-shaped Vietnamese string that satisfies the
// field's pattern, maxLength and uniqueness.
func (g *gen) text(f Field, n int) (any, bool) {
	maxLen, _ := optInt(f.Options, "maxLength")
	forms := g.forms(f, n)
	if re := g.pat[f.Name]; re != nil {
		for _, s := range forms {
			if s = truncate(s, maxLen); re.MatchString(s) {
				return s, true
			}
		}
		if !f.Required {
			return nil, false // nothing the pattern accepts: leave it blank rather than fail the row
		}
		return truncate(forms[0], maxLen), true // required: the server reports the row, not hidden here
	}
	s := forms[0]
	if f.Unique {
		suffix := fmt.Sprintf("-%s-%d", g.tag, n)
		if maxLen > 0 && maxLen-len([]rune(suffix)) < 1 {
			return truncate(s, maxLen), true
		}
		if maxLen > 0 {
			s = truncate(s, maxLen-len([]rune(suffix)))
		}
		return s + suffix, true
	}
	return truncate(s, maxLen), true
}

// forms are the acceptable shapes of one value, most natural first (a pattern picks the first match).
func (g *gen) forms(f Field, n int) []string {
	switch g.category(f) {
	case catPerson:
		return []string{g.personName()}
	case catGiven:
		if g.r.IntN(2) == 0 {
			return []string{maleGiven[g.r.IntN(len(maleGiven))]}
		}
		return []string{femaleGiven[g.r.IntN(len(femaleGiven))]}
	case catFamily:
		return []string{familyNames[g.r.IntN(len(familyNames))]}
	case catCompany:
		return []string{companyKinds[g.r.IntN(len(companyKinds))] + " " + companyBrands[g.r.IntN(len(companyBrands))]}
	case catService:
		return []string{serviceNames[g.r.IntN(len(serviceNames))]}
	case catPhone:
		a, b, c := g.r.IntN(100), g.r.IntN(1000), g.r.IntN(1000)
		return []string{fmt.Sprintf("09%02d %03d %03d", a, b, c), fmt.Sprintf("09%02d%03d%03d", a, b, c)}
	case catAddress:
		return []string{fmt.Sprintf("%d %s, %s, %s", 1+g.r.IntN(299), streets[g.r.IntN(len(streets))], districts[g.r.IntN(len(districts))], cities[g.r.IntN(len(cities))])}
	case catCode:
		return []string{fmt.Sprintf("MS%04d", g.r.IntN(10000))}
	case catNote:
		return []string{sentences[g.r.IntN(len(sentences))]}
	}
	label := f.Label
	if label == "" {
		label = f.Name
	}
	return []string{fmt.Sprintf("Mẫu %s %d", label, n)}
}

func (g *gen) personName() string {
	family := familyNames[g.r.IntN(len(familyNames))]
	if g.r.IntN(2) == 0 {
		return family + " " + maleMiddle[g.r.IntN(len(maleMiddle))] + " " + maleGiven[g.r.IntN(len(maleGiven))]
	}
	return family + " " + femaleMiddle[g.r.IntN(len(femaleMiddle))] + " " + femaleGiven[g.r.IntN(len(femaleGiven))]
}

// ---- helpers ----

func hash64(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s)) // hash.Hash.Write never returns an error
	return h.Sum64()
}

func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}

func optFloat(o map[string]any, key string) (float64, bool) {
	switch v := o[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

func optInt(o map[string]any, key string) (int, bool) {
	v, ok := optFloat(o, key)
	return int(v), ok
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

var viFold = func() map[rune]rune {
	m := map[rune]rune{'đ': 'd'}
	for base, group := range map[rune]string{
		'a': "àáạảãâầấậẩẫăằắặẳẵ", 'e': "èéẹẻẽêềếệểễ", 'i': "ìíịỉĩ",
		'o': "òóọỏõôồốộổỗơờớợởỡ", 'u': "ùúụủũưừứựửữ", 'y': "ỳýỵỷỹ",
	} {
		for _, r := range group {
			m[r] = base
		}
	}
	return m
}()

// tokens splits a field name/label into lower-case ASCII words (camelCase and snake_case alike,
// Vietnamese accents folded) plus the word run joined together, so "ho_ten" matches "ho", "ten"
// and "hoten".
func tokens(s string) map[string]bool {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		if unicode.IsUpper(r) && prevLower {
			b.WriteByte(' ')
		}
		prevLower = unicode.IsLower(r)
		r = unicode.ToLower(r)
		if f, ok := viFold[r]; ok {
			r = f
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	out := map[string]bool{}
	words := strings.Fields(b.String())
	for _, w := range words {
		out[w] = true
	}
	// Adjacent pairs and the whole run: "dien thoai" → "dienthoai", "ho ten" → "hoten".
	for i := 0; i+1 < len(words); i++ {
		out[words[i]+words[i+1]] = true
	}
	out[strings.Join(words, "")] = true
	return out
}
