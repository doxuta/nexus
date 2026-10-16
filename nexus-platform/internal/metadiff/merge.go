package metadiff

import (
	"reflect"
)

// keyedSet describes an array that is a SET with a stable key per element (specs/069 FR-090).
// noDelete: a removal on the pack side never removes what the builder still has (picklist values).
type keyedSet struct {
	key      func(el any) (string, bool)
	noDelete bool
}

func fieldKey(names ...string) func(any) (string, bool) {
	return func(el any) (string, bool) {
		if s, ok := el.(string); ok && len(names) > 0 && names[0] == "value" {
			return s, true
		}
		m, ok := el.(map[string]any)
		if !ok {
			return "", false
		}
		k := ""
		for i, n := range names {
			s, ok := m[n].(string)
			if !ok || s == "" {
				return "", false
			}
			if i > 0 {
				k += "\x00"
			}
			k += s
		}
		return k, true
	}
}

// keyedSets[itemType][dotted path of the array]. Every other array merges as one block.
var keyedSets = map[string]map[string]keyedSet{
	"role":      {"actions": {key: fieldKey("entityName", "action")}},
	"field":     {"options.values": {key: fieldKey("value"), noDelete: true}},
	"dashboard": {"widgets": {key: fieldKey("id")}},
	"approval":  {"steps": {key: fieldKey("id")}},
}

type mnode struct {
	v   any
	has bool
}

func (a mnode) eq(b mnode) bool { return a.has == b.has && reflect.DeepEqual(a.v, b.v) }

// Merge3 merges `theirs` (the new pack) into `ours` (the live tenant) relative to `base` (what the
// last install wrote), all three in canonical form. Objects merge recursively, arrays are one
// block except the keyed sets above. ok=false when both sides changed the same path (no merge is
// offered). taken lists where the merged document differs from ours — i.e. what came from the pack.
func Merge3(itemType string, base, ours, theirs any) (merged any, taken []string, ok bool) {
	n, ok := merge3(itemType, "", mnode{base, base != nil}, mnode{ours, ours != nil}, mnode{theirs, theirs != nil})
	if !ok || !n.has {
		return nil, nil, false
	}
	return n.v, Diff(ours, n.v), true
}

func merge3(typ, path string, b, o, t mnode) (mnode, bool) {
	switch {
	case o.eq(t):
		return o, true
	case b.eq(o):
		return t, true
	case b.eq(t):
		return o, true
	}
	if set, has := keyedSets[typ][path]; has {
		return mergeSet(typ, path, set, b, o, t)
	}
	om, ook := o.v.(map[string]any)
	tm, tok := t.v.(map[string]any)
	if !o.has || !t.has || !ook || !tok {
		return mnode{}, false
	}
	bm, _ := b.v.(map[string]any)
	if b.has && bm == nil {
		return mnode{}, false
	}
	out := map[string]any{}
	keys := map[string]bool{}
	for _, m := range []map[string]any{bm, om, tm} {
		for k := range m {
			keys[k] = true
		}
	}
	for k := range keys {
		sub := k
		if path != "" {
			sub = path + "." + k
		}
		bv, bh := bm[k]
		ov, oh := om[k]
		tv, th := tm[k]
		r, ok := merge3(typ, sub, mnode{bv, bh}, mnode{ov, oh}, mnode{tv, th})
		if !ok {
			return mnode{}, false
		}
		if r.has {
			out[k] = r.v
		}
	}
	return mnode{out, true}, true
}

// mergeSet merges a keyed array: the builder's order first, then keys only the pack has.
func mergeSet(typ, path string, set keyedSet, b, o, t mnode) (mnode, bool) {
	index := func(n mnode) (map[string]any, []string, bool) {
		els, isArr := n.v.([]any)
		if !n.has || !isArr {
			return map[string]any{}, nil, !n.has
		}
		m := make(map[string]any, len(els))
		order := make([]string, 0, len(els))
		for _, el := range els {
			k, ok := set.key(el)
			if !ok {
				return nil, nil, false
			}
			if _, dup := m[k]; dup {
				return nil, nil, false
			}
			m[k] = el
			order = append(order, k)
		}
		return m, order, true
	}
	bm, _, ok1 := index(b)
	om, oorder, ok2 := index(o)
	tm, torder, ok3 := index(t)
	if !ok1 || !ok2 || !ok3 || !o.has || !t.has {
		return mnode{}, false
	}
	out := []any{}
	seen := map[string]bool{}
	for _, k := range append(append([]string{}, oorder...), torder...) {
		if seen[k] {
			continue
		}
		seen[k] = true
		bv, bh := bm[k]
		ov, oh := om[k]
		tv, th := tm[k]
		if set.noDelete && oh && !th {
			out = append(out, ov)
			continue
		}
		r, ok := merge3(typ, path+"["+k+"]", mnode{bv, bh}, mnode{ov, oh}, mnode{tv, th})
		if !ok {
			return mnode{}, false
		}
		if r.has {
			out = append(out, r.v)
		}
	}
	return mnode{out, true}, true
}
