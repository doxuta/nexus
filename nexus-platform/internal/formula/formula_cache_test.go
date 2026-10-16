package formula

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Queue #108 (formula-ast-cache, bug-class B5): Evaluate runs once per formula field per record
// write — N times for an N-row import — and used to tokenize, parse and type-check the SAME
// expression string every time. The parsed tree is a pure function of the string, so it is
// cached under the string: an import pays one parse per distinct expression, not one per row.

// TestEvaluate_ReusesParsedExpression proves the warm path does no parsing. Tokenizing alone
// allocates the rune and token slices and the parser allocates a node per operator, so a warm
// call that still parsed would show dozens of allocations; the cached path allocates at most
// the string→any key boxing of the map lookup.
func TestEvaluate_ReusesParsedExpression(t *testing.T) {
	const expr = "IF(qty >= 10, price * qty * 0.95, price * qty) + 0.108"
	r := staticResolver(map[string]float64{"qty": 12, "price": 100})
	if _, _, err := Evaluate(expr, r); err != nil { // cold: parses once
		t.Fatalf("Evaluate: %v", err)
	}
	allocs := testing.AllocsPerRun(200, func() {
		if _, _, err := Evaluate(expr, r); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
	})
	if allocs > 1 {
		t.Fatalf("warm Evaluate allocated %.0f times per call; want ≤ 1 (the expression is being re-parsed)", allocs)
	}
}

// TestEvaluate_SharedTreeIsConcurrencySafe: record writes run on many goroutines at once and
// now share one tree per expression; each must see only its own record's values. Meaningful
// under -race (CI runs it that way).
func TestEvaluate_SharedTreeIsConcurrencySafe(t *testing.T) {
	const expr = "IF(x > 50, x * 2, x) + 0.1086"
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				x := float64(g*100 + i)
				want := x + 0.1086
				if x > 50 {
					want = x*2 + 0.1086
				}
				got, _, err := Evaluate(expr, staticResolver(map[string]float64{"x": x}))
				if err != nil || got != want {
					t.Errorf("goroutine %d x=%v: got (%v, %v); want %v", g, x, got, err, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestEvaluate_CacheIsKeyedByExpression: the key is the expression itself, so no invalidation
// is needed — an edited formula is a different string. Each expression keeps its own result,
// and a cached failure stays the same failure (a mistyped stored formula still writes null).
func TestEvaluate_CacheIsKeyedByExpression(t *testing.T) {
	r := staticResolver(map[string]float64{"a": 4, "b": 2})
	for round := 0; round < 3; round++ {
		for expr, want := range map[string]float64{"a * b + 0.1081": 8.1081, "a / b + 0.1082": 2.1082, "IF(a > b, a, b) + 0.1083": 4.1083} {
			got, dz, err := Evaluate(expr, r)
			if err != nil || dz || got != want {
				t.Fatalf("round %d: Evaluate(%q) = (%v, %v, %v); want %v", round, expr, got, dz, err, want)
			}
		}
		if _, _, err := Evaluate("IF(a, 1, 2) + 0.1084", r); !errors.Is(err, ErrTypeMismatch) {
			t.Fatalf("round %d: mistyped formula err = %v; want ErrTypeMismatch", round, err)
		}
		if _, _, err := Evaluate("a + + 0.1085 )", r); err == nil {
			t.Fatalf("round %d: syntax error must stay an error", round)
		}
	}
}

// Review 2026-09-30 (cross-tenant OOM): a parsed tree costs ~100× its source (a node per
// operand/operator — 2000 runes of a+a+… is ~195 KB), and the cache used to keep every
// distinct expression until restart, with no cap on its length. A tenant admin looping "save a
// new long formula, write one record" grew the process every tenant shares without bound.

// cachedBytes sums the keys the cache holds — the quantity its budget bounds.
func cachedBytes() int {
	n := 0
	programs.Range(func(k, _ any) bool { n += len(k.(string)); return true })
	return n
}

// TestCompiled_CacheStaysWithinBudget: 4× the budget of distinct ~1 KB formulas, each evaluated
// once (an edit + a record write). The cache stays within its budget, keeps working (the
// latest formula is still cached) and every result is right.
func TestCompiled_CacheStaysWithinBudget(t *testing.T) {
	r := staticResolver(map[string]float64{"a": 2})
	pad := strings.Repeat(" ", 1000)
	var last string
	for i := 0; i < 4*cacheBudget/len(pad); i++ {
		last = pad + "a * " + strconv.Itoa(i)
		got, _, err := Evaluate(last, r)
		if err != nil || got != float64(2*i) {
			t.Fatalf("Evaluate(formula %d) = (%v, %v); want %d", i, got, err, 2*i)
		}
	}
	if n := cachedBytes(); n > cacheBudget {
		t.Fatalf("cache holds %d bytes of expressions; want ≤ %d", n, cacheBudget)
	}
	if _, ok := programs.Load(last); !ok {
		t.Fatal("the latest formula is not cached — the budget must evict old entries, not stop caching")
	}
}

// TestCompiled_OverLongExpressionIsNotCached: a formula longer than MaxLen (only one saved
// before the cap existed can reach Evaluate) still evaluates, but is never kept.
func TestCompiled_OverLongExpressionIsNotCached(t *testing.T) {
	expr := strings.Repeat("a+", MaxLen/2) + "0.1087"
	got, dz, err := Evaluate(expr, staticResolver(map[string]float64{"a": 0}))
	if err != nil || dz || got != 0.1087 {
		t.Fatalf("Evaluate(over-long) = (%v, %v, %v); want 0.1087", got, dz, err)
	}
	if _, ok := programs.Load(expr); ok {
		t.Fatalf("a %d-rune formula (MaxLen %d) was cached", len(expr), MaxLen)
	}
}
