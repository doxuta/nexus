package main

// main_wiring_test.go — DAILY-QUEUE #156 (specs/045-api-key-throttle §5). The request brakes only
// exist if main.go hands them to RegisterAPIRoutes. Every handler-level wiring test passes its OWN
// options (WithDataRateLimiter, WithSSORateLimiter, …), so deleting one of those lines here kept
// every gate green while production silently lost the brake. main() cannot run in a unit test
// (it needs MySQL, Redis, secrets), so this test reads main.go's source instead: each hardening
// option must reach the RegisterAPIRoutes call, carrying the value built by its real constructor,
// and a limiter's MaxAttempts/Window (and the data limiter's MaxInFlight) must come from config —
// never a literal that switches the brake off.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// hardeningOptions maps each RegisterAPIRoutes option main.go must pass to the source its argument
// must start with (after following a `name := …` assignment), plus the config keys that must be
// set from a non-literal expression.
var hardeningOptions = []struct {
	option, source string
	configKeys     []string
}{
	{"handlers.WithBodySizeLimit", "cfg.MaxRequestBodyBytes", nil},
	{"handlers.WithAuthRateLimiter", "middleware.AuthRateLimit(", []string{"MaxAttempts", "Window"}},
	{"handlers.WithSSORateLimiter", "middleware.SSORateLimit(", []string{"MaxAttempts", "Window"}},
	{"handlers.WithAssistantRateLimiter", "middleware.AssistantRateLimit(", []string{"MaxAttempts", "Window"}},
	{"handlers.WithDataRateLimiter", "middleware.DataRateLimit(", []string{"MaxAttempts", "Window", "MaxInFlight"}},
	// specs/062 L1a: RequireBuilderTier replaced RequirePlatformOperator here so a confirmed builder
	// workspace-session token reaches the builder surfaces; a home token still gets the same live read.
	{"handlers.WithOperatorTier", "middleware.RequireBuilderTier(", nil},
	// specs/062 L1a: without the resolver every delegation token answers 401 (fail closed), so the
	// feature would be silently dead — pin that main.go wires the real service.
	{"handlers.WithWorkspaceResolver", "services.NewWorkspaceSessionService(", nil},
	{"handlers.WithLiveSubject", "services.NewCachedUserLoader(", nil},
}

func TestMainWiresEveryHardeningOption(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	render := func(e ast.Expr) string {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, e); err != nil {
			t.Fatalf("render expression: %v", err)
		}
		return b.String()
	}

	// `name := expr` bindings, so an option argument like dataLimiter traces back to its constructor.
	bound := map[string]ast.Expr{}
	var register *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE && len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					bound[id.Name] = n.Rhs[0]
				}
			}
		case *ast.CallExpr:
			if render(n.Fun) == "handlers.RegisterAPIRoutes" {
				register = n
			}
		}
		return true
	})
	if register == nil {
		t.Fatal("main.go never calls handlers.RegisterAPIRoutes")
	}

	passed := map[string]ast.Expr{}
	for _, arg := range register.Args {
		call, ok := arg.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		value := call.Args[0]
		if id, ok := value.(*ast.Ident); ok && bound[id.Name] != nil {
			value = bound[id.Name]
		}
		passed[render(call.Fun)] = value
	}

	for _, want := range hardeningOptions {
		value, ok := passed[want.option]
		if !ok {
			t.Errorf("main.go does not pass %s to RegisterAPIRoutes — that brake is off in production", want.option)
			continue
		}
		if src := render(value); !strings.HasPrefix(src, want.source) {
			t.Errorf("%s gets %q, want a value from %s…", want.option, src, want.source)
			continue
		}
		if len(want.configKeys) == 0 {
			continue
		}
		fields := map[string]ast.Expr{}
		if call, ok := value.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						fields[render(kv.Key)] = kv.Value
					}
				}
			}
		}
		for _, key := range want.configKeys {
			v, ok := fields[key]
			switch {
			case !ok:
				t.Errorf("%s: config key %s is not set — a zero value switches the limiter off", want.option, key)
			case isLiteral(v):
				t.Errorf("%s: config key %s = %s is a literal, want it read from config", want.option, key, render(v))
			}
		}
	}
}

// TestMainWiresTheIdempotencyStore (queue #174, specs/047): every service that serves an
// Idempotency-Key path must get the claim store in main.go — without it a keyed request fails
// closed with a 500, and no handler-level test (they wire their own) would notice.
func TestMainWiresTheIdempotencyStore(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	bound := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok && a.Tok == token.DEFINE && len(a.Lhs) == 1 && len(a.Rhs) == 1 {
			if id, ok := a.Lhs[0].(*ast.Ident); ok {
				var b bytes.Buffer
				if err := printer.Fprint(&b, fset, a.Rhs[0]); err != nil {
					t.Fatalf("render %s: %v", id.Name, err)
				}
				bound[id.Name] = b.String()
			}
		}
		return true
	})
	for _, svc := range []string{"recordSvc", "importSvc"} {
		src, ok := bound[svc]
		if !ok {
			t.Errorf("main.go no longer builds %s — update this lock", svc)
			continue
		}
		// A builder chain spans lines: compare with the whitespace squeezed out.
		if !strings.Contains(strings.Join(strings.Fields(src), ""), ".WithIdempotencyStore(idempotency.NewSQLRepository(") {
			t.Errorf("%s is built without WithIdempotencyStore: Idempotency-Key requests on it fail closed", svc)
		}
	}
}

// TestMainWiresTheFieldACLCatalog (queue #316): FieldACLService only derives "a formula over a hidden
// field is hidden too" when it holds the metadata catalog (field_acl.go skips the derivation when
// catalog is nil — fail-open). Every test of #316 calls WithCatalog itself, so dropping the line from
// main.go's newFieldACL kept every gate green while production leaked `weighted_value`. main() cannot
// run in a unit test, so read the source: NewFieldACLService must be built in exactly one place and
// that place must chain .WithCatalog(.
func TestMainWiresTheFieldACLCatalog(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Squeeze whitespace so a builder chain split across lines still matches.
	flat := strings.Join(strings.Fields(string(src)), "")
	const ctor = "services.NewFieldACLService("
	if n := strings.Count(flat, ctor); n != 1 {
		t.Fatalf("main.go builds the field-ACL service in %d places, want exactly one (newFieldACL)", n)
	}
	rest := flat[strings.Index(flat, ctor):]
	// The chained call must follow the constructor's own argument list, before the statement ends.
	if end := strings.Index(rest, "}"); end < 0 || !strings.Contains(rest[:end], ").WithCatalog(") {
		t.Error("newFieldACL builds the field-ACL service without .WithCatalog(: formulas over hidden fields leak (queue #316)")
	}
}

// TestMainWiresTheAPIKeyProbeBrake (queue #356, specs/045 FR-9): the per-IP brake on failed `nxk_`
// lookups needs BOTH ends — the service armed from config, and the Auth stage told which proxies may
// vouch for X-Forwarded-For. Every unit test wires its own; dropping either line from main.go would
// leave all of them green and the brake silently off (no budget) or keyed on the proxy's address
// (one shared bucket for every client behind it).
func TestMainWiresTheAPIKeyProbeBrake(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	flat := strings.Join(strings.Fields(string(src)), "")
	for _, want := range []string{
		".WithProbeBrake(cfg.APIKeyProbeMaxPerMin)",
		"handlers.WithTrustedProxies(cfg.TrustedProxyCIDRs)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("main.go lost %s: the API-key failed-lookup brake is off or keyed on the wrong address", want)
		}
	}
}

// TestMainWiresTheEmailResend (specs/066 L2): "Gửi lại" has THREE ends that every unit test supplies
// for itself — the live rule store (WithLiveRepo(workflowRepo), the raw repo the engine runs, never the
// draft-aware wrapper), the renderer and the SAME sender instance the engine mails through, and the
// auditor. Dropping any of them from main.go keeps all of those green while the route answers 500
// EMAIL_RESEND_UNAVAILABLE (or, with the draft repo, mails unpublished wording) in production.
func TestMainWiresTheEmailResend(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	flat := strings.Join(strings.Fields(string(src)), "")
	for _, want := range []string{
		"services.NewWorkflowService(draftWorkflowRepo).WithLiveRepo(workflowRepo)",
		"services.NewEmailLogService(emailLogRepo).",
		".WithRenderer(workflowSvc).WithSender(workflowEmailSender).WithAuditor(auditDispatcher)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("main.go lost %s: the email resend is unwired or reads the draft layer", want)
		}
	}
}

// isLiteral reports a bare constant (0, -1, "x"), possibly negated — the shape that hard-codes a
// limiter setting instead of reading it from config.
func isLiteral(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok {
		e = u.X
	}
	_, ok := e.(*ast.BasicLit)
	return ok
}
