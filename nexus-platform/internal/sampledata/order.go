package sampledata

import "sort"

// PlanInput is what Plan decides on. Entities are the tables loaded for this run (the selected
// ones plus any required parent the caller found empty); Existing says which tables already have
// live rows in the database (their ids can be pointed at without generating them).
type PlanInput struct {
	Entities []Entity
	Selected []string
	Existing map[string]bool
	HasUsers bool
}

// Skip is a table the run leaves out, with the reason code.
type Skip struct{ Entity, Code string }

// PlanResult is the run order (parents first), the parents added on top of the selection and the
// tables skipped.
type PlanResult struct {
	Order        []string
	AddedParents []string
	Skipped      []Skip
}

// RequiredTargets lists the other tables e cannot be filled without: the distinct targets of its
// required relation fields (a self reference is not "another table").
func RequiredTargets(e Entity) []string {
	var out []string
	seen := map[string]bool{e.Name: true}
	for _, f := range e.Fields {
		if f.Type != "relation" || !f.Required {
			continue
		}
		if t, _ := f.Options["relatedEntity"].(string); t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

type edge struct {
	child, parent string
	field         string
	required      bool
}

// Plan orders the tables so a parent is generated before the children that point at it, skips
// tables that cannot be filled, and breaks reference cycles: an optional edge (or one whose
// parent already has rows in the database) is simply not waited for; a cycle of required edges
// over empty tables cannot be satisfied and is skipped with SAMPLE_PARENT_EMPTY.
func Plan(in PlanInput) PlanResult {
	byName := make(map[string]Entity, len(in.Entities))
	names := make([]string, 0, len(in.Entities))
	for _, e := range in.Entities {
		byName[e.Name] = e
		names = append(names, e.Name)
	}
	sort.Strings(names)

	skipped := map[string]string{}
	for _, name := range names {
		if code := staticBlocker(byName[name], byName, in); code != "" {
			skipped[name] = code
		}
	}

	var edges []edge
	for _, name := range names {
		for _, f := range byName[name].Fields {
			switch f.Type {
			case "relation":
				if t, _ := f.Options["relatedEntity"].(string); t != "" && t != name {
					if _, ok := byName[t]; ok {
						edges = append(edges, edge{name, t, f.Name, f.Required})
					}
				}
			case "polymorphic_relation":
				for _, t := range stringList(f.Options["targetEntities"]) {
					if _, ok := byName[t]; ok && t != name {
						edges = append(edges, edge{name, t, f.Name, false})
					}
				}
			}
		}
	}
	dropped := map[int]bool{}

	// propagate: a table that needs a skipped parent (with no rows of its own in the database) is skipped too.
	propagate := func() {
		for changed := true; changed; {
			changed = false
			for _, name := range names {
				if skipped[name] != "" {
					continue
				}
				for _, f := range byName[name].Fields {
					if !f.Required {
						continue
					}
					if f.Type == "relation" {
						t, _ := f.Options["relatedEntity"].(string)
						if _, loaded := byName[t]; t != name && loaded && skipped[t] != "" && !in.Existing[t] {
							skipped[name], changed = CodeParentEmpty, true
						}
					}
					if f.Type == "polymorphic_relation" && !polyUsable(f, byName, skipped, in.Existing) {
						skipped[name], changed = CodeParentEmpty, true
					}
				}
			}
		}
	}

	var order []string
	done := map[string]bool{}
	for {
		propagate()
		var remaining []string
		for _, name := range names {
			if skipped[name] == "" && !done[name] {
				remaining = append(remaining, name)
			}
		}
		if len(remaining) == 0 {
			break
		}
		var ready []string
		for _, name := range remaining {
			if waitsFor(name, edges, dropped, done, skipped) == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) > 0 {
			for _, name := range ready {
				order = append(order, name)
				done[name] = true
			}
			continue
		}
		// Stuck on a cycle: stop waiting on an optional edge, or on a parent that already has rows.
		if i := firstDroppable(edges, dropped, remaining, in.Existing); i >= 0 {
			dropped[i] = true
			continue
		}
		skipped[firstOnCycle(edges, dropped, remaining)] = CodeParentEmpty
	}

	res := PlanResult{Order: order}
	selected := make(map[string]bool, len(in.Selected))
	for _, s := range in.Selected {
		selected[s] = true
	}
	for _, name := range order {
		if !selected[name] {
			res.AddedParents = append(res.AddedParents, name)
		}
	}
	sort.Strings(res.AddedParents)
	for _, name := range names {
		if skipped[name] != "" {
			res.Skipped = append(res.Skipped, Skip{name, skipped[name]})
		}
	}
	return res
}

// staticBlocker is the reason e cannot be filled whatever the other tables do.
func staticBlocker(e Entity, byName map[string]Entity, in PlanInput) string {
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
			if !in.HasUsers {
				set(CodeNoUser)
			}
		case "relation":
			t, _ := f.Options["relatedEntity"].(string)
			if _, loaded := byName[t]; t == e.Name || !loaded {
				if !in.Existing[t] {
					set(CodeParentEmpty)
				}
			}
		case "polymorphic_relation":
			if !polyUsable(f, byName, nil, in.Existing) {
				set(CodeParentEmpty)
			}
		}
	}
	return code
}

// polyUsable: some target of f is loaded (and not skipped) or already has rows.
func polyUsable(f Field, byName map[string]Entity, skipped map[string]string, existing map[string]bool) bool {
	for _, t := range stringList(f.Options["targetEntities"]) {
		if _, loaded := byName[t]; (loaded && skipped[t] == "") || existing[t] {
			return true
		}
	}
	return false
}

// waitsFor counts the parents name still has to wait for.
func waitsFor(name string, edges []edge, dropped map[int]bool, done map[string]bool, skipped map[string]string) int {
	n := 0
	for i, e := range edges {
		if e.child == name && !dropped[i] && !done[e.parent] && skipped[e.parent] == "" {
			n++
		}
	}
	return n
}

// firstDroppable is the first edge between two stuck tables that need not be waited for.
func firstDroppable(edges []edge, dropped map[int]bool, stuck []string, existing map[string]bool) int {
	in := toSet(stuck)
	for i, e := range edges {
		if !dropped[i] && in[e.child] && in[e.parent] && (!e.required || existing[e.parent]) {
			return i
		}
	}
	return -1
}

// firstOnCycle is the first stuck table that can reach itself through the remaining edges.
func firstOnCycle(edges []edge, dropped map[int]bool, stuck []string) string {
	in := toSet(stuck)
	for _, start := range stuck {
		seen := map[string]bool{}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for i, e := range edges {
				if dropped[i] || e.child != cur || !in[e.parent] {
					continue
				}
				if e.parent == start {
					return start
				}
				if !seen[e.parent] {
					seen[e.parent] = true
					queue = append(queue, e.parent)
				}
			}
		}
	}
	return stuck[0] // unreachable for a real cycle; keeps the loop finite
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}
