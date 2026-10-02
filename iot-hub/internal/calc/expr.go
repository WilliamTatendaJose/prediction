// Package calc evaluates calculated fields: arithmetic formulas over a
// sensor's fields, and time integrals (e.g. kWh from kW).
//
// Formulas are parsed by a small recursive-descent parser into a tree;
// nothing is ever executed as code. Grammar:
//
//	cond   = and { ("or" | "||") and }
//	and    = not { ("and" | "&&") not }
//	not    = ["not" | "!"] cmp
//	cmp    = expr [ ("<" | "<=" | ">" | ">=" | "=" | "==" | "!=" | "<>") expr ]
//	expr   = term { ("+" | "-") term }
//	term   = unary { ("*" | "/" | "%") unary }
//	unary  = ["-" | "+"] power
//	power  = atom [ "^" unary ]
//	atom   = number | field | func "(" cond { "," cond } ")" | "(" cond ")"
//
// Comparisons and logic give 1 (true) or 0 (false); "and"/"or" short-
// circuit, so if(flow > 0 and level / flow > 2, …) never divides by zero.
//
// Functions: abs, sqrt, min, max, round(x[, digits]), clamp(x, lo, hi), if(c, a, b).
package calc

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

type node interface {
	eval(vars func(string) (float64, bool)) (float64, error)
}

type num float64
type ref string
type neg struct{ x node }
type bin struct {
	op   byte
	l, r node
}
type call struct {
	fn   string
	args []node
}

var ErrMissing = errors.New("missing input")

func (n num) eval(func(string) (float64, bool)) (float64, error) { return float64(n), nil }

func (r ref) eval(vars func(string) (float64, bool)) (float64, error) {
	v, ok := vars(string(r))
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrMissing, string(r))
	}
	return v, nil
}

func (n neg) eval(vars func(string) (float64, bool)) (float64, error) {
	v, err := n.x.eval(vars)
	return -v, err
}

func (b bin) eval(vars func(string) (float64, bool)) (float64, error) {
	l, err := b.l.eval(vars)
	if err != nil {
		return 0, err
	}
	r, err := b.r.eval(vars)
	if err != nil {
		return 0, err
	}
	switch b.op {
	case '+':
		return l + r, nil
	case '-':
		return l - r, nil
	case '*':
		return l * r, nil
	case '/':
		if r == 0 {
			return 0, errors.New("division by zero")
		}
		return l / r, nil
	case '%':
		if r == 0 {
			return 0, errors.New("modulo by zero")
		}
		return math.Mod(l, r), nil
	case '^':
		return math.Pow(l, r), nil
	}
	return 0, fmt.Errorf("unknown operator %c", b.op)
}

type cmp struct {
	op   string
	l, r node
}

func (c cmp) eval(vars func(string) (float64, bool)) (float64, error) {
	l, err := c.l.eval(vars)
	if err != nil {
		return 0, err
	}
	r, err := c.r.eval(vars)
	if err != nil {
		return 0, err
	}
	var t bool
	switch c.op {
	case "<":
		t = l < r
	case "<=":
		t = l <= r
	case ">":
		t = l > r
	case ">=":
		t = l >= r
	case "=", "==":
		t = l == r
	case "!=", "<>":
		t = l != r
	}
	return b2f(t), nil
}

type logic struct {
	and  bool
	l, r node
}

func (g logic) eval(vars func(string) (float64, bool)) (float64, error) {
	l, err := g.l.eval(vars)
	if err != nil {
		return 0, err
	}
	if g.and && l == 0 || !g.and && l != 0 {
		return b2f(l != 0), nil // short-circuit
	}
	r, err := g.r.eval(vars)
	return b2f(r != 0), err
}

type not struct{ x node }

func (n not) eval(vars func(string) (float64, bool)) (float64, error) {
	v, err := n.x.eval(vars)
	return b2f(v == 0), err
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

var arity = map[string][2]int{"abs": {1, 1}, "sqrt": {1, 1}, "min": {2, 8}, "max": {2, 8}, "round": {1, 2}, "clamp": {3, 3}, "if": {3, 3}}

func (c call) eval(vars func(string) (float64, bool)) (float64, error) {
	if c.fn == "if" { // lazy: only the chosen branch needs its inputs
		cond, err := c.args[0].eval(vars)
		if err != nil {
			return 0, err
		}
		if cond != 0 {
			return c.args[1].eval(vars)
		}
		return c.args[2].eval(vars)
	}
	a := make([]float64, len(c.args))
	for i, x := range c.args {
		v, err := x.eval(vars)
		if err != nil {
			return 0, err
		}
		a[i] = v
	}
	switch c.fn {
	case "abs":
		return math.Abs(a[0]), nil
	case "sqrt":
		if a[0] < 0 {
			return 0, errors.New("sqrt of a negative number")
		}
		return math.Sqrt(a[0]), nil
	case "min", "max":
		v := a[0]
		for _, x := range a[1:] {
			if (c.fn == "min") == (x < v) {
				v = x
			}
		}
		return v, nil
	case "round":
		p := 1.0
		if len(a) == 2 {
			p = math.Pow(10, math.Round(a[1]))
		}
		return math.Round(a[0]*p) / p, nil
	case "clamp":
		return math.Max(a[1], math.Min(a[2], a[0])), nil
	}
	return 0, fmt.Errorf("unknown function %s", c.fn)
}

// Expr is a parsed formula.
type Expr struct {
	src  string
	root node
	refs []string
}

func (e *Expr) String() string   { return e.src }
func (e *Expr) Fields() []string { return e.refs }

// Eval computes the formula. A missing input or a non-finite result is an
// error: a calculated value is better absent than wrong.
func (e *Expr) Eval(vars func(string) (float64, bool)) (float64, error) {
	v, err := e.root.eval(vars)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("result is not a finite number")
	}
	return v, nil
}

type parser struct {
	s    string
	pos  int
	refs map[string]bool
}

const maxLen = 500

func Parse(src string) (*Expr, error) {
	if len(src) > maxLen {
		return nil, fmt.Errorf("formula longer than %d characters", maxLen)
	}
	p := &parser{s: src, refs: map[string]bool{}}
	root, err := p.cond(0)
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos != len(p.s) {
		return nil, fmt.Errorf("unexpected %q at position %d", p.s[p.pos:], p.pos+1)
	}
	e := &Expr{src: src, root: root}
	for r := range p.refs {
		e.refs = append(e.refs, r)
	}
	return e, nil
}

func (p *parser) skip() {
	for p.pos < len(p.s) && unicode.IsSpace(rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skip()
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

const maxDepth = 50 // bounds recursion on hostile input like "((((((…"

// word consumes a case-insensitive keyword (not a prefix of a longer name).
func (p *parser) word(w string) bool {
	p.skip()
	end := p.pos + len(w)
	if end > len(p.s) || !strings.EqualFold(p.s[p.pos:end], w) || end < len(p.s) && isIdent(p.s[end], false) {
		return false
	}
	p.pos = end
	return true
}

func (p *parser) symbol(sym string) bool {
	p.skip()
	if strings.HasPrefix(p.s[p.pos:], sym) {
		p.pos += len(sym)
		return true
	}
	return false
}

func (p *parser) cond(depth int) (node, error) {
	if depth > maxDepth {
		return nil, errors.New("formula nested too deeply")
	}
	l, err := p.and(depth)
	if err != nil {
		return nil, err
	}
	for p.word("or") || p.symbol("||") {
		r, err := p.and(depth)
		if err != nil {
			return nil, err
		}
		l = logic{false, l, r}
	}
	return l, nil
}

func (p *parser) and(depth int) (node, error) {
	l, err := p.not(depth)
	if err != nil {
		return nil, err
	}
	for p.word("and") || p.symbol("&&") {
		r, err := p.not(depth)
		if err != nil {
			return nil, err
		}
		l = logic{true, l, r}
	}
	return l, nil
}

func (p *parser) not(depth int) (node, error) {
	if p.word("not") || p.peek() == '!' && !strings.HasPrefix(p.s[p.pos:], "!=") && p.symbol("!") {
		x, err := p.not(depth + 1)
		return not{x}, err
	}
	return p.cmp(depth)
}

func (p *parser) cmp(depth int) (node, error) {
	l, err := p.expr(depth)
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"<=", ">=", "==", "!=", "<>", "<", ">", "="} {
		if p.symbol(op) {
			r, err := p.expr(depth)
			if err != nil {
				return nil, err
			}
			return cmp{op, l, r}, nil
		}
	}
	return l, nil
}

func (p *parser) expr(depth int) (node, error) {
	if depth > maxDepth {
		return nil, errors.New("formula nested too deeply")
	}
	l, err := p.term(depth)
	if err != nil {
		return nil, err
	}
	for c := p.peek(); c == '+' || c == '-'; c = p.peek() {
		p.pos++
		r, err := p.term(depth)
		if err != nil {
			return nil, err
		}
		l = bin{c, l, r}
	}
	return l, nil
}

func (p *parser) term(depth int) (node, error) {
	l, err := p.unary(depth)
	if err != nil {
		return nil, err
	}
	for c := p.peek(); c == '*' || c == '/' || c == '%'; c = p.peek() {
		p.pos++
		r, err := p.unary(depth)
		if err != nil {
			return nil, err
		}
		l = bin{c, l, r}
	}
	return l, nil
}

func (p *parser) unary(depth int) (node, error) {
	if depth > maxDepth {
		return nil, errors.New("formula nested too deeply")
	}
	switch p.peek() {
	case '-':
		p.pos++
		x, err := p.unary(depth + 1)
		return neg{x}, err
	case '+':
		p.pos++
		return p.unary(depth + 1)
	}
	return p.power(depth)
}

func (p *parser) power(depth int) (node, error) {
	base, err := p.atom(depth)
	if err != nil {
		return nil, err
	}
	if p.peek() == '^' { // right-associative: 2^3^2 = 2^9
		p.pos++
		exp, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return bin{'^', base, exp}, nil
	}
	return base, nil
}

// isIdent: field references use letters, digits, '_' and '.'. A '-' in a
// field name cannot be referenced (it would read as minus); use '_'.
func isIdent(c byte, first bool) bool {
	return c == '_' || unicode.IsLetter(rune(c)) || (!first && (unicode.IsDigit(rune(c)) || c == '.'))
}

func (p *parser) atom(depth int) (node, error) {
	c := p.peek()
	switch {
	case c == 0:
		return nil, errors.New("formula ends unexpectedly")
	case c == '(':
		p.pos++
		x, err := p.cond(depth + 1)
		if err != nil {
			return nil, err
		}
		if p.peek() != ')' {
			return nil, fmt.Errorf("missing ) at position %d", p.pos+1)
		}
		p.pos++
		return x, nil
	case c >= '0' && c <= '9' || c == '.':
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.' || p.s[p.pos] == 'e' || p.s[p.pos] == 'E' ||
			(p.s[p.pos] == '-' || p.s[p.pos] == '+') && p.pos > start && (p.s[p.pos-1] == 'e' || p.s[p.pos-1] == 'E')) {
			p.pos++
		}
		v, err := strconv.ParseFloat(p.s[start:p.pos], 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", p.s[start:p.pos])
		}
		return num(v), nil
	case isIdent(c, true):
		start := p.pos
		for p.pos < len(p.s) && isIdent(p.s[p.pos], false) {
			p.pos++
		}
		name := p.s[start:p.pos]
		if p.peek() == '(' {
			fn := strings.ToLower(name)
			ar, ok := arity[fn]
			if !ok {
				return nil, fmt.Errorf("unknown function %s()", name)
			}
			p.pos++
			var args []node
			if p.peek() != ')' {
				for {
					a, err := p.cond(depth + 1)
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peek() != ',' {
						break
					}
					p.pos++
				}
			}
			if p.peek() != ')' {
				return nil, fmt.Errorf("missing ) after %s(", name)
			}
			p.pos++
			if len(args) < ar[0] || len(args) > ar[1] {
				return nil, fmt.Errorf("%s() takes %d to %d arguments", fn, ar[0], ar[1])
			}
			return call{fn, args}, nil
		}
		p.refs[name] = true
		return ref(name), nil
	}
	return nil, fmt.Errorf("unexpected %q at position %d", string(c), p.pos+1)
}
