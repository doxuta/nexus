package ai

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Issue #22 item 3: MCP (internal/mcpserver) and the in-app builder run every tool through toolExecutor,
// and "agents propose, humans dispose" holds only while nothing the executor can reach writes. The tool
// names are pinned elsewhere (read_* / propose_* only); this pins what the executor is BUILT from: every
// interface it can call is a narrow read interface, and it holds no func, channel or concrete service.

// readVerbs are the method names an executor dependency may have. A new method fails the pin until a
// human reads it and adds it here: Get*/List*/Load*/Find* read; the rest are named one by one.
var readVerbs = regexp.MustCompile(`^(Get|List|Load|Find)[A-Z]|^Get$|^(Authorize|HeadCutoff|ColumnExists|RefEntity|AccessProbes|PrivilegedHolders)$`)

// executorRoots are the types the executor reaches: its own fields, and the probes it gets by
// type-asserting its reader to AccessProbeCarrier (the only assertion it makes, pinned below).
var executorRoots = []reflect.Type{
	reflect.TypeOf(toolExecutor{}),
	reflect.TypeOf((*AccessProbeCarrier)(nil)).Elem(),
	reflect.TypeOf(AccessProbes{}),
}

func TestToolExecutor_ReachesReadOnlyDependenciesOnly(t *testing.T) {
	for _, bad := range readOnlyViolations(executorRoots) {
		t.Error(bad)
	}
}

// The pin must catch what it guards against: a field or a probe that can write.
func TestToolExecutor_ReadOnlyPinCatchesAWriter(t *testing.T) {
	type writer interface {
		GetEntityByName() error
		CreateEntity() error
	}
	type withWriter struct {
		reader MetadataReader
		probes *struct{ Store writer }
		hook   func()
	}
	got := strings.Join(readOnlyViolations([]reflect.Type{reflect.TypeOf(withWriter{})}), "\n")
	for _, want := range []string{"CreateEntity", "func"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pin let %q through: %s", want, got)
		}
	}
	if strings.Contains(got, "GetEntityByName") {
		t.Errorf("a read method was reported: %s", got)
	}
}

// The executor reaches further dependencies only by asserting its reader to another interface; each
// such interface must be a root above. Read from source: reflection cannot see an assertion.
func TestToolExecutor_ReaderAssertionsArePinned(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	assert := regexp.MustCompile(`\breader\.\((\w+)\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range assert.FindAllStringSubmatch(string(src), -1) {
			if !slices.ContainsFunc(executorRoots, func(r reflect.Type) bool { return r.Name() == m[1] }) {
				t.Errorf("%s asserts the reader to %s: add it to executorRoots so the read-only pin covers it", f, m[1])
			}
		}
	}
}

// readOnlyViolations walks the types reachable from roots and reports every method that is not a read
// verb, and every func or channel (code the executor could call that the pin cannot read).
func readOnlyViolations(roots []reflect.Type) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(path string, t reflect.Type)
	walk = func(path string, t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Interface:
			for i := range t.NumMethod() {
				if m := t.Method(i); !readVerbs.MatchString(m.Name) {
					out = append(out, path+" ("+t.String()+") has "+m.Name+": not a read")
				}
			}
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			out = append(out, path+" is a "+t.Kind().String()+": the executor must hold read interfaces, not code")
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path, t.Elem())
		case reflect.Map:
			walk(path, t.Key())
			walk(path, t.Elem())
		case reflect.Struct:
			for i := range t.NumField() {
				f := t.Field(i)
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	for _, r := range roots {
		walk(r.Name(), r)
	}
	return out
}
