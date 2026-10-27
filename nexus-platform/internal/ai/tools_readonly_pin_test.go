package ai

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Issue #22 item 3: MCP (internal/mcpserver) and the in-app builder run every tool through toolExecutor,
// and "agents propose, humans dispose" holds only while nothing the executor can reach writes. The tool
// names are pinned elsewhere (read_* / propose_* only); this pins what the executor is BUILT from: every
// interface it can call is a narrow read interface, and it holds no func, channel or concrete service.

// readVerbs are the method names an executor dependency may have. A new method fails the pin until a
// human reads it and adds it here: Get*/List*/Load*/Find* read; the rest are named one by one.
var readVerbs = regexp.MustCompile(`^(Get|List|Load|Find)[A-Z]|^Get$|^(Authorize|HeadCutoff|ColumnExists|RefEntity|AccessProbes|PrivilegedHolders|PacksOf|AggregateScalar)$`)

// writeWords may not appear anywhere in a read method's name: GetOrCreate, FindOrCreate or ListAndPurge
// start like a read and then write.
var writeWords = []string{"Create", "Update", "Delete", "Insert", "Upsert", "Save", "Set", "Put", "Write", "Apply",
	"Publish", "Revoke", "Grant", "Remove", "Purge", "Touch", "Append", "Mark", "Settle", "Exec", "Execute", "Store",
	"Add", "Drop", "Reset", "Clear", "Bump", "Commit", "Rollback", "Send", "Enqueue", "Post", "Patch", "Replace",
	"Restore", "Install", "Import", "Move", "Rename", "Assign"}

var camelWords = regexp.MustCompile(`[A-Z][a-z0-9]*`)

func isReadMethod(name string) bool {
	if !readVerbs.MatchString(name) {
		return false
	}
	return !slices.ContainsFunc(camelWords.FindAllString(name, -1), func(w string) bool { return slices.Contains(writeWords, w) })
}

// sinks are types the walk does not enter, each with the reason a human read it: they write, but never to
// tenant data, the draft or the model.
var sinks = map[reflect.Type]string{
	reflect.TypeOf(zap.Logger{}): "the server log: warn writes a line there when a best-effort read fails",
}

// executorRoots are the types the executor reaches: its own fields, and the probes it gets by
// type-asserting its reader to AccessProbeCarrier (the only interface it asserts to, pinned below).
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
		GetOrCreateEntity() error
		FindOrCreateRole() error
		ListAndPurgeDrafts() error
		ListFieldsIncludingDeleted() error
	}
	type withWriter struct {
		reader MetadataReader
		probes *struct{ Store writer }
		hook   func()
		log    *zap.Logger  // a sink: not entered
		core   zapcore.Core // what the logger holds, reached another way: entered
	}
	got := strings.Join(readOnlyViolations([]reflect.Type{reflect.TypeOf(withWriter{})}), "\n")
	for _, want := range []string{"CreateEntity", "GetOrCreateEntity", "FindOrCreateRole", "ListAndPurgeDrafts", "func", "core (zapcore.Core) has Write"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pin let %q through: %s", want, got)
		}
	}
	if strings.Contains(got, "GetEntityByName") || strings.Contains(got, "ListFieldsIncludingDeleted") || strings.Contains(got, ".log") {
		t.Errorf("a read method was reported: %s", got)
	}
}

// The executor reaches further dependencies only by asserting a value to another type, which reflection
// cannot see. So the package's source is read: every type assertion or type-switch case to anything but
// plain data (basic types, maps, slices, any) must be listed here, and an interface the executor reaches
// that way must also be a root above.
var allowedAssertions = map[string]string{
	"AccessProbeCarrier": "the reader's probes: a root of the pin",
	"UserTokenLedger":    "the budget guard's ledger (agent loop), not reachable from toolExecutor",
	"*replayer":          "the cassette gateway (cassette.go), not reachable from toolExecutor",
}

func TestToolExecutor_TypeAssertionsArePinned(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, bad := range assertionViolations(t, f, nil) {
			t.Error(bad)
		}
	}
}

// The scan must catch the shapes a regex missed: a package-qualified or pointer target, a chained
// assertion on an embedded reader, a type switch.
func TestToolExecutor_TypeAssertionScanCatchesEveryShape(t *testing.T) {
	src := `package ai
func (e *toolExecutor) x() {
	_, _ = e.reader.(AccessProbeCarrier)
	_, _ = e.reader.(services.MetadataWriter)
	_ = e.reader.(*readerWithAccessProbes).MetadataReader.(MetadataReader)
	r := e.reader
	switch r.(type) {
	case interface{ CreateEntity() error }, nil:
	}
	_, _ = e.args.(map[string]any)
	_, _ = e.args.(string)
}`
	got := strings.Join(assertionViolations(t, "sample.go", []byte(src)), "\n")
	for _, want := range []string{"services.MetadataWriter", "*readerWithAccessProbes", "MetadataReader", "CreateEntity"} {
		if !strings.Contains(got, want) {
			t.Errorf("the scan let an assertion to %s through: %s", want, got)
		}
	}
	for _, ok := range []string{"AccessProbeCarrier", "map[string]any", "string", "nil"} {
		if strings.Contains(got, "to "+ok+":") {
			t.Errorf("the scan reported %s: %s", ok, got)
		}
	}
}

// assertionViolations parses one file (src, or the file at name when src is nil) and reports each type
// assertion or type-switch case whose target is neither plain data nor in allowedAssertions.
func assertionViolations(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	var source any // a nil []byte would parse as an empty file
	if src != nil {
		source = src
	}
	file, err := parser.ParseFile(fset, name, source, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	render := func(n ast.Node) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, n)
		return b.String()
	}
	var out []string
	check := func(at token.Pos, target ast.Expr) {
		if isPlainData(target) {
			return
		}
		if typ := render(target); allowedAssertions[typ] == "" {
			out = append(out, fset.Position(at).String()+" asserts to "+typ+": list it in allowedAssertions, and add an interface the executor reaches to executorRoots")
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.TypeAssertExpr:
			if n.Type != nil { // nil = the x.(type) of a switch, judged by its cases below
				check(n.Pos(), n.Type)
			}
		case *ast.TypeSwitchStmt:
			for _, c := range n.Body.List {
				for _, target := range c.(*ast.CaseClause).List {
					check(target.Pos(), target)
				}
			}
		}
		return true
	})
	return out
}

// isPlainData reports a target that carries no behaviour: a predeclared type, a map, a slice or an array
// of plain data, or the empty interface.
func isPlainData(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return slices.Contains([]string{"any", "nil", "string", "bool", "error", "byte", "rune", "int", "int8", "int16", "int32",
			"int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64"}, e.Name)
	case *ast.MapType:
		return isPlainData(e.Key) && isPlainData(e.Value)
	case *ast.ArrayType:
		return isPlainData(e.Elt)
	case *ast.InterfaceType:
		return len(e.Methods.List) == 0
	}
	return false
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
		if sinks[t] != "" {
			return
		}
		switch t.Kind() {
		case reflect.Interface:
			for i := range t.NumMethod() {
				if m := t.Method(i); !isReadMethod(m.Name) {
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
