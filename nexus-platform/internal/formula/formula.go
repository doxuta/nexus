// Package formula provides a small, safe expression evaluator for Nexus computed fields
// (fieldType "formula"). It is hand-rolled with no external dependencies:
//
//	Tokenize → parse (recursive-descent) → type-check → evaluate to float64 (or a string).
//
// Supported: + - * / with standard precedence, parentheses, unary minus; comparisons
// > >= < <= = == != <> (lower precedence than arithmetic); AND / OR (looser still, OR
// loosest; case-insensitive); IF(condition, valueIfTrue, valueIfFalse) and the Excel forms
// AND(c1, c2, …) / OR(c1, c2, …). Operands are numeric literals (int/float), "double-quoted"
// text literals, and bare identifiers that resolve to field values supplied by the caller.
// An identifier followed by "(" is a function call; AND/OR are operators only right after an
// operand (where an identifier used to be a syntax error); anywhere else an identifier is a
// field reference, so no field name became a reserved word.
//
// Typed values (specs/042-formula-if-compare, specs/053-formula-text-and-or): every
// expression has a static type — number, boolean or text. Comparisons take two numbers and
// yield a boolean; AND/OR take two booleans; IF needs a boolean condition and two branches of
// the same type; arithmetic needs numbers; text is only ever a value (a literal or an IF of
// texts). The whole formula must produce what its field stores: a number (resultType number|
// currency) or text (resultType text); a boolean is never stored. A mismatch is
// ErrTypeMismatch — caught when the field is SAVED, never coerced at record time.
//
// Design constraints:
//   - NO string operations (no text comparison or concatenation), NO functions other than
//     IF/AND/OR, NO external record access.
//   - IF, AND and OR short-circuit: only what decides the result is evaluated, so
//     IF(x > 0, a / x, 0) is 0 at x = 0 and IF(x = 0 OR a / x > 2, …) takes the first branch —
//     not null.
//   - Division by zero: Evaluate returns (0, true) where the bool "divByZero" signals the
//     caller should write null rather than 0 to the stored data.
//   - Validate does the combined parse + type-check + ref-type-check the orchestration
//     layer needs at field-definition time (no DB access here — callers pass the known
//     field set).
package formula

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// ErrTypeMismatch marks an expression that parses but mixes types (e.g. a bare comparison
// as the whole formula, a number as the IF condition, a comparison or text inside
// arithmetic, a result that does not match resultType). Callers map it to
// FIELD_FORMULA_TYPE_MISMATCH.
var ErrTypeMismatch = errors.New("formula: sai kiểu")

// MaxLen is the longest expression, in runes, a formula field may save: validateFormulaConfig
// refuses a longer one (FIELD_FORMULA_INVALID) on every save path, and the evaluation cache
// never keeps one. The longest formula on the dev DB (2026-09-30, 52 fields) is 26 runes; a
// four-level grading IF with Vietnamese labels is about 120.
const MaxLen = 2000

// --- Token types ----------------------------------------------------------------

type tokenKind int

const (
	tokNumber tokenKind = iota
	tokIdent
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokLParen
	tokRParen
	tokComma
	// Comparison operators — keep tokLT..tokNEQ contiguous (isComparison is a range check).
	tokLT  // <
	tokLTE // <=
	tokGT  // >
	tokGTE // >=
	tokEQ  // = or ==
	tokNEQ // != or <>
	tokString
	// AND / OR never come out of the lexer (they lex as tokIdent — see isKeyword); the
	// parser stamps these kinds on the logical nodes it builds.
	tokAnd
	tokOr
	tokEOF
)

// isComparison reports whether k is one of the six comparison operators.
func isComparison(k tokenKind) bool { return k >= tokLT && k <= tokNEQ }

type token struct {
	kind tokenKind
	text string // original text slice; for tokNumber: the numeric literal string; for tokString: the text between the quotes
}

// isKeyword reports whether t is the identifier kw, case-insensitively.
func isKeyword(t token, kw string) bool { return t.kind == tokIdent && strings.EqualFold(t.text, kw) }

// --- Lexer (tokenizer) ----------------------------------------------------------

// Comparison operators. "=" and "==" are the same operator (Excel/Salesforce habit vs
// programmer habit), as are "!=" and "<>".
var (
	twoRuneOps = map[string]tokenKind{"<=": tokLTE, ">=": tokGTE, "==": tokEQ, "!=": tokNEQ, "<>": tokNEQ}
	oneRuneOps = map[rune]tokenKind{'<': tokLT, '>': tokGT, '=': tokEQ}
)

// tokenize turns the expression string into a flat token slice. It returns an error
// for any unrecognized character. Whitespace is silently skipped.
func tokenize(expr string) ([]token, error) {
	var tokens []token
	i := 0
	runes := []rune(expr)
	n := len(runes)
	for i < n {
		r := runes[i]
		if unicode.IsSpace(r) {
			i++
			continue
		}
		switch r {
		case '+':
			tokens = append(tokens, token{tokPlus, "+"})
			i++
		case '-':
			tokens = append(tokens, token{tokMinus, "-"})
			i++
		case '*':
			tokens = append(tokens, token{tokStar, "*"})
			i++
		case '/':
			tokens = append(tokens, token{tokSlash, "/"})
			i++
		case '(':
			tokens = append(tokens, token{tokLParen, "("})
			i++
		case ')':
			tokens = append(tokens, token{tokRParen, ")"})
			i++
		case ',':
			tokens = append(tokens, token{tokComma, ","})
			i++
		case '"':
			// Text literal: everything up to the next double quote, no escapes.
			j := i + 1
			for j < n && runes[j] != '"' {
				j++
			}
			if j == n {
				return nil, fmt.Errorf("formula: chuỗi mở tại vị trí %d chưa có dấu \" đóng", i)
			}
			tokens = append(tokens, token{tokString, string(runes[i+1 : j])})
			i = j + 1
		case '<', '>', '=', '!':
			// Two-rune operators win over their one-rune prefix: <= >= == != <>.
			if i+1 < n {
				if k, ok := twoRuneOps[string(runes[i:i+2])]; ok {
					tokens = append(tokens, token{k, string(runes[i : i+2])})
					i += 2
					continue
				}
			}
			k, ok := oneRuneOps[r]
			if !ok { // a lone '!' — there is no logical NOT
				return nil, fmt.Errorf("formula: ký tự không hợp lệ %q tại vị trí %d", r, i)
			}
			tokens = append(tokens, token{k, string(r)})
			i++
		default:
			if unicode.IsDigit(r) || r == '.' {
				// Numeric literal: integer or float.
				j := i
				dotSeen := false
				for j < n && (unicode.IsDigit(runes[j]) || (runes[j] == '.' && !dotSeen)) {
					if runes[j] == '.' {
						dotSeen = true
					}
					j++
				}
				tokens = append(tokens, token{tokNumber, string(runes[i:j])})
				i = j
			} else if unicode.IsLetter(r) || r == '_' {
				// Identifier: field name or keyword.
				j := i
				for j < n && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_') {
					j++
				}
				tokens = append(tokens, token{tokIdent, string(runes[i:j])})
				i = j
			} else {
				return nil, fmt.Errorf("formula: ký tự không hợp lệ %q tại vị trí %d", r, i)
			}
		}
	}
	tokens = append(tokens, token{tokEOF, ""})
	return tokens, nil
}

// --- Recursive-descent parser ---------------------------------------------------
//
// Grammar (standard arithmetic, then comparisons, then AND, OR binding loosest):
//   expr   ::= and ( OR and )*
//   and    ::= cmp ( AND cmp )*
//   cmp    ::= sum ( CMP sum )*            CMP = > >= < <= = == != <>
//   sum    ::= term ( ("+" | "-") term )*
//   term   ::= unary ( ("*" | "/") unary )*
//   unary  ::= "-" unary | primary
//   primary::= NUMBER | STRING | IDENT | IDENT "(" expr ("," expr)* ")" | "(" expr ")"
//
// Chained comparisons (a < b < c) parse but fail the type check (boolean < number).

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) consume() token {
	t := p.tokens[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) expect(k tokenKind) (token, error) {
	t := p.consume()
	if t.kind != k {
		return t, fmt.Errorf("formula: cú pháp sai — mong đợi %v nhưng gặp %q", k, t.text)
	}
	return t, nil
}

// nodeKind differentiates AST node types.
type nodeKind int

const (
	nodeNumber nodeKind = iota // leaf: float64 literal
	nodeIdent                  // leaf: field reference (identifier)
	nodeBinop                  // binary operator: left op right (arithmetic or comparison)
	nodeUnary                  // unary minus
	nodeIf                     // IF(cond, left, right)
	nodeString                 // leaf: text literal
	nodeLogic                  // left AND/OR right (op.kind tokAnd / tokOr)
)

// node is the AST representation. The tree is kept small and heap-allocated only for
// operator nodes (binary/unary); literals are cheap. Given that expressions are small
// (< 50 tokens in practice), allocations are negligible.
type node struct {
	kind nodeKind

	// leaf fields
	num   float64 // nodeNumber
	ident string  // nodeIdent
	str   string  // nodeString

	// internal fields
	op    token // nodeBinop / nodeUnary / nodeLogic
	cond  *node // nodeIf
	left  *node // nodeBinop / nodeLogic / nodeIf (value if true)
	right *node // nodeBinop / nodeLogic / nodeUnary (child) / nodeIf (value if false)

	// text marks a comparison whose operands are text (set by typeOf before the tree is shared).
	text bool

	// memoised typeOf result: nested comparisons would otherwise re-type each child 2^depth times.
	typed   bool
	typ     valueType
	typeErr error
}

func (p *parser) parseExpr() (*node, error) { return p.parseLogic(tokOr) }

// parseLogic parses one level of the AND/OR chain: kind tokOr is the loosest level (its
// operands are AND chains), tokAnd the next (its operands are comparisons). An AND/OR
// identifier only reaches here in operator position — right after a complete operand.
func (p *parser) parseLogic(kind tokenKind) (*node, error) {
	kw, operand := "OR", func() (*node, error) { return p.parseLogic(tokAnd) }
	if kind == tokAnd {
		kw, operand = "AND", p.parseComparison
	}
	left, err := operand()
	if err != nil {
		return nil, err
	}
	for isKeyword(p.peek(), kw) {
		p.consume()
		right, err := operand()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeLogic, op: token{kind, kw}, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseComparison() (*node, error) {
	left, err := p.parseAddSub()
	if err != nil {
		return nil, err
	}
	for isComparison(p.peek().kind) {
		op := p.consume()
		right, err := p.parseAddSub()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinop, op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAddSub() (*node, error) {
	left, err := p.parseMulDiv()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokPlus && t.kind != tokMinus {
			break
		}
		op := p.consume()
		right, err := p.parseMulDiv()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinop, op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseMulDiv() (*node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokStar && t.kind != tokSlash {
			break
		}
		op := p.consume()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinop, op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (*node, error) {
	if p.peek().kind == tokMinus {
		op := p.consume()
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &node{kind: nodeUnary, op: op, right: child}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (*node, error) {
	t := p.peek()
	switch t.kind {
	case tokNumber:
		p.consume()
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, fmt.Errorf("formula: số không hợp lệ %q: %w", t.text, err)
		}
		return &node{kind: nodeNumber, num: v}, nil
	case tokString:
		p.consume()
		return &node{kind: nodeString, str: t.text}, nil
	case tokIdent:
		p.consume()
		if p.peek().kind == tokLParen {
			return p.parseCall(t.text)
		}
		return &node{kind: nodeIdent, ident: t.text}, nil
	case tokLParen:
		p.consume() // consume "("
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, fmt.Errorf("formula: thiếu dấu đóng ngoặc ')'")
		}
		return inner, nil
	case tokEOF:
		return nil, fmt.Errorf("formula: biểu thức không đầy đủ (kết thúc bất ngờ)")
	default:
		return nil, fmt.Errorf("formula: cú pháp sai tại %q", t.text)
	}
}

// parseCall parses the argument list of a function call whose name was just consumed.
// IF(cond, then, else) and the Excel forms AND(c1, c2, …) / OR(c1, c2, …) are the only
// functions; arity is a syntax rule, argument types are checked later by typeOf. AND()/OR()
// fold into the same left-nested nodeLogic chain the infix operators build.
func (p *parser) parseCall(name string) (*node, error) {
	var logic tokenKind // stays tokNumber (0) for IF
	switch strings.ToUpper(name) {
	case "IF":
	case "AND":
		logic = tokAnd
	case "OR":
		logic = tokOr
	default:
		return nil, fmt.Errorf("formula: hàm %q không được hỗ trợ — chỉ có IF(điều kiện, giá trị nếu đúng, giá trị nếu sai), AND(…) và OR(…)", name)
	}
	p.consume() // "("
	var args []*node
	for {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if p.peek().kind != tokComma {
			break
		}
		p.consume()
	}
	if p.peek().kind != tokRParen {
		return nil, fmt.Errorf("formula: thiếu dấu đóng ngoặc ')' của %s", strings.ToUpper(name))
	}
	p.consume()
	if logic != 0 {
		if len(args) < 2 {
			return nil, fmt.Errorf("formula: %s cần ít nhất 2 điều kiện, gặp %d", strings.ToUpper(name), len(args))
		}
		out := args[0]
		for _, a := range args[1:] {
			out = &node{kind: nodeLogic, op: token{logic, strings.ToUpper(name)}, left: out, right: a}
		}
		return out, nil
	}
	if len(args) != 3 {
		return nil, fmt.Errorf("formula: IF cần đúng 3 tham số (điều kiện, giá trị nếu đúng, giá trị nếu sai), gặp %d", len(args))
	}
	return &node{kind: nodeIf, cond: args[0], left: args[1], right: args[2]}, nil
}

// parse tokenizes and parses expr, returning the AST root or an error. It also verifies
// the token stream is fully consumed (no trailing junk).
func parse(expr string) (*node, error) {
	toks, err := tokenize(expr)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: toks}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("formula: ký tự thừa sau biểu thức: %q", p.peek().text)
	}
	return root, nil
}

// --- Type check -----------------------------------------------------------------

// valueType is the static type of an expression. Identifiers are always numbers: Validate
// only admits number/currency references, and the Resolver returns float64. Text only comes
// from literals.
type valueType int

const (
	typeNumber valueType = iota
	typeBool
	typeText
)

// Result is what a whole formula must produce — its field's options.resultType, with
// "currency" counting as a number (the stored amount).
type Result int

const (
	ResultNumber Result = iota // resultType number | currency
	ResultText                 // resultType text
)

// typeOf returns the static type of n, or an error wrapping ErrTypeMismatch.
func typeOf(n *node) (valueType, error) {
	if !n.typed {
		n.typ, n.typeErr = typeOfNode(n)
		n.typed = true
	}
	return n.typ, n.typeErr
}

func typeOfNode(n *node) (valueType, error) {
	switch n.kind {
	case nodeString:
		return typeText, nil
	case nodeUnary:
		if err := wantNumber(n.right, "dấu trừ '-'"); err != nil {
			return 0, err
		}
		return typeNumber, nil
	case nodeBinop:
		if isComparison(n.op.kind) && textOperands(n) {
			if err := checkTextCompare(n); err != nil {
				return 0, err
			}
			n.text = true
			return typeBool, nil
		}
		if err := wantNumber(n.left, fmt.Sprintf("phép %q", n.op.text)); err != nil {
			return 0, err
		}
		if err := wantNumber(n.right, fmt.Sprintf("phép %q", n.op.text)); err != nil {
			return 0, err
		}
		if isComparison(n.op.kind) {
			return typeBool, nil
		}
		return typeNumber, nil
	case nodeLogic:
		if err := wantBool(n.left, n.op.text); err != nil {
			return 0, err
		}
		if err := wantBool(n.right, n.op.text); err != nil {
			return 0, err
		}
		return typeBool, nil
	case nodeIf:
		if err := wantBool(n.cond, "điều kiện của IF"); err != nil {
			return 0, err
		}
		lt, err := typeOf(n.left)
		if err != nil {
			return 0, err
		}
		rt, err := typeOf(n.right)
		if err != nil {
			return 0, err
		}
		if lt != rt {
			return 0, fmt.Errorf("%w: hai giá trị của IF phải cùng kiểu (đều là số, đều là chữ, hoặc đều là phép so sánh)", ErrTypeMismatch)
		}
		return lt, nil
	}
	return typeNumber, nil // nodeNumber, nodeIdent
}

// textOperands reports whether a comparison has a text operand (a literal or an IF of texts), which
// makes it a text comparison: IF(stage = "won", amount, 0). A bare identifier is a number to
// typeOf, so only a literal/IF side opens it; Validate then checks the field really is text.
func textOperands(n *node) bool {
	lt, lerr := typeOf(n.left)
	rt, rerr := typeOf(n.right)
	return lerr == nil && rerr == nil && (lt == typeText || rt == typeText)
}

// checkTextCompare: text is compared only for equality, and each side is text or a field name.
func checkTextCompare(n *node) error {
	if n.op.kind != tokEQ && n.op.kind != tokNEQ {
		return fmt.Errorf("%w: chữ chỉ so sánh được bằng = hoặc != (không dùng %q)", ErrTypeMismatch, n.op.text)
	}
	if n.left.kind != nodeIdent && n.right.kind != nodeIdent {
		return fmt.Errorf("%w: so sánh chữ cần một trường (vd stage = \"won\"), không so hai chuỗi cố định", ErrTypeMismatch)
	}
	for _, side := range []*node{n.left, n.right} {
		if side.kind == nodeIdent {
			continue
		}
		if t, _ := typeOf(side); t != typeText {
			return fmt.Errorf("%w: không so sánh được chữ với số — hai vế của %q phải cùng là chữ", ErrTypeMismatch, n.op.text)
		}
	}
	return nil
}

// wantNumber checks that operand n is a number; where names the operator for the message.
func wantNumber(n *node, where string) error {
	t, err := typeOf(n)
	if err != nil {
		return err
	}
	switch t {
	case typeBool:
		return fmt.Errorf("%w: %s chỉ nhận số — phép so sánh chỉ dùng được làm điều kiện của IF", ErrTypeMismatch, where)
	case typeText:
		return fmt.Errorf("%w: %s chỉ nhận số — chữ trong ngoặc kép chỉ dùng được làm giá trị của IF, không tính toán hay so sánh được", ErrTypeMismatch, where)
	}
	return nil
}

// wantBool checks that n is a condition (a comparison, or comparisons joined by AND/OR).
func wantBool(n *node, where string) error {
	t, err := typeOf(n)
	if err != nil {
		return err
	}
	if t != typeBool {
		return fmt.Errorf("%w: %s cần một phép so sánh (vd amount >= 100000000), không phải một số hay chữ", ErrTypeMismatch, where)
	}
	return nil
}

// resultOf type-checks a whole formula and returns what it produces. A boolean is never a
// storable value: a formula field stores a number, a currency amount or text.
func resultOf(root *node) (valueType, error) {
	t, err := typeOf(root)
	if err != nil {
		return 0, err
	}
	if t == typeBool {
		return 0, fmt.Errorf("%w: kết quả công thức phải là số hoặc chữ — phép so sánh chỉ dùng được làm điều kiện của IF, vd IF(amount >= 100, 1, 0)", ErrTypeMismatch)
	}
	return t, nil
}

// wantResult checks a formula that produces got against the result its field stores.
func wantResult(got valueType, want Result) error {
	switch {
	case want == ResultText && got != typeText:
		return fmt.Errorf("%w: resultType \"text\" cần công thức ra chữ, vd IF(score >= 8, \"Giỏi\", \"Khá\")", ErrTypeMismatch)
	case want == ResultNumber && got != typeNumber:
		return fmt.Errorf("%w: công thức ra chữ nhưng field lưu số — đặt resultType \"text\", hoặc cho mọi giá trị của IF là số", ErrTypeMismatch)
	}
	return nil
}

// --- Evaluator ------------------------------------------------------------------

// Resolver maps a field identifier to its numeric value. The caller supplies a concrete
// implementation: fieldValue resolves the record data; identifiers not found → 0.
type Resolver func(name string) float64

// TextResolver maps a field identifier used in a text comparison to its stored text (a picklist
// code or a text value); a missing value is "". May be nil when no formula compares text.
type TextResolver func(name string) string

// env bundles both resolvers for the evaluator.
type env struct {
	num Resolver
	txt TextResolver
}

func (e env) text(name string) string {
	if e.txt == nil {
		return ""
	}
	return e.txt(name)
}

// evalNode recursively evaluates a number- or boolean-typed AST node.
// Returns (result, divByZero). divByZero propagates upward: any node containing an
// EVALUATED div-by-zero sub-expression is also considered divByZero (the overall result is
// null). What IF/AND/OR do not need is never evaluated, so it cannot poison the result.
//
// ponytail: booleans travel as 1/0 in the float64 — the type check guarantees a boolean is
// only ever read by IF/AND/OR, so it can never leak into a stored value. Text never reaches
// here: a text-typed subtree is only literals and IFs (typeOf), walked by evalText.
func evalNode(n *node, e env) (float64, bool) {
	switch n.kind {
	case nodeNumber:
		return n.num, false
	case nodeIdent:
		return e.num(n.ident), false
	case nodeUnary:
		v, dz := evalNode(n.right, e)
		return -v, dz
	case nodeIf:
		c, cz := evalNode(n.cond, e)
		if cz {
			return 0, true // an undecidable condition has no value
		}
		if c != 0 {
			return evalNode(n.left, e)
		}
		return evalNode(n.right, e)
	case nodeLogic:
		l, lz := evalNode(n.left, e)
		if lz {
			return 0, true // an undecidable left side decides nothing
		}
		// Short-circuit: a false left decides AND, a true left decides OR.
		if n.op.kind == tokAnd && l == 0 {
			return 0, false
		}
		if n.op.kind == tokOr && l != 0 {
			return 1, false
		}
		r, rz := evalNode(n.right, e)
		return boolNum(r != 0), rz
	case nodeBinop:
		if n.text {
			l, lz := textOperand(n.left, e)
			r, rz := textOperand(n.right, e)
			if lz || rz {
				return 0, true
			}
			return boolNum((l == r) == (n.op.kind == tokEQ)), false
		}
		l, lz := evalNode(n.left, e)
		r, rz := evalNode(n.right, e)
		if lz || rz {
			return 0, true
		}
		switch n.op.kind {
		case tokLT:
			return boolNum(l < r), false
		case tokLTE:
			return boolNum(l <= r), false
		case tokGT:
			return boolNum(l > r), false
		case tokGTE:
			return boolNum(l >= r), false
		case tokEQ:
			return boolNum(l == r), false
		case tokNEQ:
			return boolNum(l != r), false
		case tokPlus:
			return l + r, false
		case tokMinus:
			return l - r, false
		case tokStar:
			return l * r, false
		case tokSlash:
			if r == 0 {
				return 0, true // division by zero → null sentinel
			}
			return l / r, false
		}
	}
	return 0, false
}

// evalText evaluates a text-typed node: a literal, or an IF whose branches are text.
// textOperand is one side of a text comparison: a field's stored text, or a text literal/IF.
func textOperand(n *node, e env) (string, bool) {
	if n.kind == nodeIdent {
		return e.text(n.ident), false
	}
	return evalText(n, e)
}

func evalText(n *node, e env) (string, bool) {
	if n.kind == nodeIf {
		c, cz := evalNode(n.cond, e)
		if cz {
			return "", true
		}
		if c != 0 {
			return evalText(n.left, e)
		}
		return evalText(n.right, e)
	}
	return n.str, false // nodeString — typeOf admits nothing else as text
}

func boolNum(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Evaluate parses, type-checks and evaluates a NUMBER formula using the provided Resolver
// for identifiers. The parse + type-check is cached per expression string (see programs), so
// an import evaluating the same formula for N rows parses it once.
// It returns (result, false, nil) on success.
// It returns (0, true, nil) when division by zero occurs — the caller MUST write null.
// It returns (0, false, error) on a parse error or a type error (errors.Is ErrTypeMismatch),
// including a formula that produces text; a stored formula that slipped past save-time
// validation is never coerced into a number.
func Evaluate(expr string, resolve Resolver) (result float64, divByZero bool, err error) {
	return EvaluateWith(expr, resolve, nil)
}

// EvaluateWith is Evaluate for a formula that may compare text fields (txt resolves them).
func EvaluateWith(expr string, resolve Resolver, txt TextResolver) (result float64, divByZero bool, err error) {
	p := compiled(expr)
	if err := p.check(ResultNumber); err != nil {
		return 0, false, err
	}
	v, dz := evalNode(p.root, env{resolve, txt})
	return v, dz, nil
}

// EvaluateText is Evaluate for a TEXT formula (resultType "text"): same cache, same
// divByZero contract (a condition that divides by zero leaves the value null), and an
// ErrTypeMismatch error for a formula that does not produce text.
func EvaluateText(expr string, resolve Resolver) (result string, divByZero bool, err error) {
	return EvaluateTextWith(expr, resolve, nil)
}

// EvaluateTextWith is EvaluateText for a formula that may compare text fields.
func EvaluateTextWith(expr string, resolve Resolver, txt TextResolver) (result string, divByZero bool, err error) {
	p := compiled(expr)
	if err := p.check(ResultText); err != nil {
		return "", false, err
	}
	s, dz := evalText(p.root, env{resolve, txt})
	return s, dz, nil
}

// Check is the config-time structural check: expr parses, type-checks and produces want. It
// needs no field metadata (every identifier is a number at record time) and, unlike
// Evaluate, never touches the cache — it judges text an admin is typing, not a saved formula.
func Check(expr string, want Result) error {
	root, err := parse(expr)
	if err != nil {
		return err
	}
	got, err := resultOf(root)
	if err != nil {
		return err
	}
	return wantResult(got, want)
}

// --- Compiled-expression cache (queue #108, bug-class B5) -------------------------

// program is a parsed + type-checked expression: all Evaluate/EvaluateText need besides the
// record's values. The tree is never mutated after parse, so one program is shared by every
// goroutine evaluating that expression. A parse/type error is cached as well — a bad stored
// formula fails the same way on every row without being re-parsed.
type program struct {
	root *node
	typ  valueType // typeNumber or typeText when err is nil
	err  error
}

// check returns the program's cached error, or a mismatch when it does not produce want.
func (p *program) check(want Result) error {
	if p.err != nil {
		return p.err
	}
	return wantResult(p.typ, want)
}

// programs maps an expression string to its *program. The key IS the expression, so there is
// nothing to invalidate: editing a formula stores a different string. Only Evaluate and
// EvaluateText fill it, and their callers pass saved sys_fields options; config-time checks
// (Check, Validate) parse uncached, so the live editor's keystrokes never land here.
//
// The cache is bounded, because every tenant shares it and a parsed tree costs ~100× its
// source (a node per operand/operator — 2000 runes of a+a+… is ~195 KB):
//   - an expression longer than MaxLen is evaluated but never kept (only a formula saved before
//     the cap existed can be that long);
//   - the keys held stay within cacheBudget bytes (≤ ~25 MB of trees). A store that would pass
//     it drops the whole cache first, so a tenant admin re-saving formulas in a loop cannot grow
//     the process — old expressions refill on their next use (review 2026-09-30).
//
// ponytail: drop-all instead of LRU. 256 KiB holds ~5000 typical 50-rune formulas; a process
// actively using more than that re-parses after each drop (a few µs per formula, still
// correct). Upgrade to an LRU if that ever shows in a profile.
var (
	programs     sync.Map
	programBytes atomic.Int64 // len of the keys stored since the last drop
)

const cacheBudget = 256 << 10

func compiled(expr string) *program {
	if p, ok := programs.Load(expr); ok {
		return p.(*program)
	}
	p := &program{}
	p.root, p.err = parse(expr)
	if p.err == nil {
		p.typ, p.err = resultOf(p.root)
	}
	if utf8.RuneCountInString(expr) > MaxLen {
		return p
	}
	if programBytes.Add(int64(len(expr))) > cacheBudget {
		programs.Clear()
		programBytes.Store(int64(len(expr)))
	}
	actual, _ := programs.LoadOrStore(expr, p)
	return actual.(*program)
}

// --- Validate (config-time) -----------------------------------------------------

// FieldInfo is the minimal info about an existing field that Validate needs to
// check formula references: just the name and whether it is numeric (number or currency).
type FieldInfo struct {
	Name      string
	FieldType string // "number", "currency", "formula", "lookup", "rollup", or any other type
	// ValueType is what a "lookup" field yields: the field type of the value it mirrors on the
	// related entity (queue #284). Empty = the caller could not resolve it. Ignored for the rest.
	ValueType string
}

// Validate parses expr and performs config-time semantic checks:
//   - The expression must parse without syntax errors (FIELD_FORMULA_INVALID).
//   - It must type-check: comparisons only as a condition, a number or text result
//     (FIELD_FORMULA_TYPE_MISMATCH). Whether that result matches the field's resultType is
//     Check's job (validateFormulaConfig runs it first on every save path).
//   - Every identifier in the expression must match a field in knownFields
//     (FIELD_FORMULA_UNKNOWN_REF).
//   - Referenced fields must be of type "number" or "currency", a "rollup", or a "lookup"
//     mirroring a number/currency (FieldInfo.ValueType; queue #284) — else
//     FIELD_FORMULA_NON_NUMERIC_REF.
//   - No reference to another "formula" field (FIELD_FORMULA_NESTED).
//   - selfName is the name of the formula field being defined; a self-reference is
//     rejected as FIELD_FORMULA_UNKNOWN_REF (it is not yet in knownFields, so it
//     naturally falls through — but we add an explicit check for a clear error).
//
// Returns the sorted list of referenced field names (for callers that want to record
// dependencies) and an error if any rule is violated.
func Validate(expr string, selfName string, knownFields []FieldInfo) (referencedFields []string, err error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("formula: biểu thức không được rỗng")
	}
	root, err := parse(expr)
	if err != nil {
		return nil, fmt.Errorf("FIELD_FORMULA_INVALID: %w", err)
	}
	if _, err := resultOf(root); err != nil {
		return nil, fmt.Errorf("FIELD_FORMULA_TYPE_MISMATCH: %w", err)
	}

	// Build lookup map.
	byName := make(map[string]FieldInfo, len(knownFields))
	for _, f := range knownFields {
		byName[f.Name] = f
	}

	// Collect all identifiers from the AST.
	idents := collectIdents(root)
	textUse := map[string]int{} // occurrences as a side of a text comparison
	countTextIdents(root, textUse)
	total := map[string]int{}
	for _, name := range idents {
		total[name]++
	}

	seen := make(map[string]struct{}, len(idents))
	for _, name := range idents {
		if _, already := seen[name]; already {
			continue
		}
		seen[name] = struct{}{}

		// Self-reference guard.
		if name == selfName {
			return nil, fmt.Errorf("FIELD_FORMULA_UNKNOWN_REF: field %q không thể tự tham chiếu chính nó", name)
		}

		fi, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("FIELD_FORMULA_UNKNOWN_REF: field %q không tồn tại trong entity này", name)
		}
		if textUse[name] > 0 {
			if textUse[name] != total[name] {
				return nil, fmt.Errorf("FIELD_FORMULA_TYPE_MISMATCH: trường %q vừa được so sánh với chữ vừa được tính như số", name)
			}
			if fi.FieldType != "picklist" && fi.FieldType != "text" {
				return nil, fmt.Errorf("FIELD_FORMULA_TYPE_MISMATCH: trường %q (kiểu %q) không so sánh được với chữ — chỉ trường chọn-một-trong-danh-sách hoặc văn bản mới so sánh với chữ", name, refKind(fi))
			}
			referencedFields = append(referencedFields, name)
			continue
		}
		if fi.FieldType == "formula" {
			return nil, fmt.Errorf("FIELD_FORMULA_NESTED: không cho phép tham chiếu tới field formula khác (%q)", name)
		}
		if !numericRef(fi) {
			return nil, fmt.Errorf("FIELD_FORMULA_NON_NUMERIC_REF: trường %q (kiểu %q) không phải số hay tiền tệ nên không dùng được trong công thức", name, refKind(fi))
		}
		referencedFields = append(referencedFields, name)
	}
	return referencedFields, nil
}

// numericRef reports whether a formula may read the field: number/currency, a rollup (always a
// number or money), or a lookup that mirrors a number/currency (queue #284).
func numericRef(fi FieldInfo) bool {
	switch fi.FieldType {
	case "number", "currency", "rollup":
		return true
	case "lookup":
		return fi.ValueType == "number" || fi.ValueType == "currency"
	}
	return false
}

// refKind is the type named in a refusal: for a lookup, the type of what it mirrors.
func refKind(fi FieldInfo) string {
	if fi.FieldType == "lookup" {
		if fi.ValueType == "" {
			return "tra cứu chưa xác định"
		}
		return "tra cứu " + fi.ValueType
	}
	return fi.FieldType
}

// Derived returns, sorted and de-duplicated, the lookup/rollup fields expr reads. Those are
// resolved at READ time and are not in the stored record, so a formula with any is computed at
// read time too (queue #284) instead of being written at save. An expression that does not parse
// returns nil.
func Derived(expr string, fields []FieldInfo) []string {
	idents, err := Identifiers(expr)
	if err != nil {
		return nil
	}
	readTime := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f.FieldType == "lookup" || f.FieldType == "rollup" {
			readTime[f.Name] = struct{}{}
		}
	}
	var out []string
	seen := map[string]struct{}{}
	for _, id := range idents {
		if _, ok := readTime[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Identifiers returns the field names expr reads, in source order (duplicates kept). It shares
// the Evaluate cache, so calling it per request on a saved formula costs a map hit, not a parse.
// An expression that does not parse returns an error and nil names — the caller must treat
// "cannot tell what it reads" as "reads anything" (field-ACL derivation fails closed on it).
func Identifiers(expr string) ([]string, error) {
	p := compiled(expr)
	if p.root == nil {
		return nil, p.err
	}
	return collectIdents(p.root), nil
}

// countTextIdents counts, per name, the identifiers that are a direct side of a text comparison.
func countTextIdents(n *node, out map[string]int) {
	if n == nil {
		return
	}
	if n.kind == nodeBinop && n.text {
		for _, side := range []*node{n.left, n.right} {
			if side.kind == nodeIdent {
				out[side.ident]++
			}
		}
	}
	countTextIdents(n.cond, out)
	countTextIdents(n.left, out)
	countTextIdents(n.right, out)
}

// collectIdents does a depth-first traversal of the AST and returns all nodeIdent names.
func collectIdents(n *node) []string {
	if n == nil {
		return nil
	}
	switch n.kind {
	case nodeIdent:
		return []string{n.ident}
	case nodeNumber:
		return nil
	case nodeUnary:
		return collectIdents(n.right)
	case nodeBinop, nodeLogic:
		l := collectIdents(n.left)
		r := collectIdents(n.right)
		return append(l, r...)
	case nodeIf:
		out := collectIdents(n.cond)
		out = append(out, collectIdents(n.left)...)
		return append(out, collectIdents(n.right)...)
	}
	return nil
}
