// Package where parses and evaluates the SQL-92 `where` subset ArcGIS clients send:
// = <> != < <= > >=, AND/OR/NOT, parentheses, [NOT] IN, [NOT] LIKE, IS [NOT] NULL, [NOT] BETWEEN,
// + - * / and || arithmetic, UPPER/LOWER, TRUE/FALSE, string, number, DATE and TIMESTAMP literals. It follows ArcGIS Online
// hosted layers: logic is SQL's three-valued one, a literal converts to the type of the field it
// is compared with, and other type mismatches are errors. Strings compare ignoring case (collation
// caseSensitive false), LIKE included. Field names match case-insensitively.
package where

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UGS-GIO/geoparquet-featureserver/internal/jsonx"
)

type expr interface {
	eval(row Row) any
}

// Clause is a compiled where clause.
type Clause struct {
	root  expr
	where string
	bad   bool
}

// Row gives a field's value by its resolved index: nil, bool, int64, float64 or string.
type Row func(field int) any

// Resolver maps a field name to an index and its type (Integer, Double, Date or String), or
// false when there is no such field.
type Resolver func(name string) (int, string, bool)

// Match reports whether row satisfies the clause.
func (c *Clause) Match(row Row) bool { return c.root.eval(row) == true }

// Err reports what a matched row could not evaluate, which fails the query as on SQL Server: a
// string meeting a number that does not convert to one, or a division by zero.
func (c *Clause) Err() error {
	if c.bad {
		return &Error{c.where}
	}
	return nil
}

// Error is a where clause that does not parse.
type Error struct{ Where string }

func (e *Error) Error() string { return `Invalid "where" parameter: ` + e.Where }

// Parse compiles where. An unknown field or a type mismatch is an error.
func Parse(where string, resolve Resolver) (*Clause, error) {
	toks, err := lex(where)
	if err != nil {
		return nil, &Error{where}
	}
	p := &parser{toks: toks, resolve: resolve, clause: &Clause{where: where}}
	n, err := p.or()
	if err != nil || n.k != kBool || p.peek().kind != tEOF {
		return nil, &Error{where}
	}
	p.clause.root = n.e
	return p.clause, nil
}

// --- lexer

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tKeyword
	tString
	tNumber
	tOp
	tLParen
	tRParen
	tComma
)

type token struct {
	kind tokKind
	text string
	num  float64
}

var keywords = map[string]bool{"AND": true, "OR": true, "NOT": true, "IN": true, "LIKE": true,
	"IS": true, "NULL": true, "BETWEEN": true, "ESCAPE": true, "TRUE": true, "FALSE": true,
	"DATE": true, "TIMESTAMP": true}

var punct = map[byte]tokKind{'(': tLParen, ')': tRParen, ',': tComma}

var (
	number = regexp.MustCompile(`^[0-9.]+([eE][+-]?[0-9]+)?`)
	name   = regexp.MustCompile(`^[_A-Za-z][_.A-Za-z0-9]*`)
	quoted = regexp.MustCompile(`^'(?:[^']|'')*'`)
)

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ',':
			out = append(out, token{kind: punct[c]})
			i++
		case c == '\'':
			m := quoted.FindString(s[i:])
			if m == "" {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, token{kind: tString, text: strings.ReplaceAll(m[1:len(m)-1], "''", "'")})
			i += len(m)
		case c == '"' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := strings.IndexByte(s[i+1:], end)
			if j < 0 {
				return nil, fmt.Errorf("unterminated identifier")
			}
			out = append(out, token{kind: tIdent, text: s[i+1 : i+1+j]})
			i += j + 2
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			n := number.FindString(s[i:])
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tNumber, num: f, text: n})
			i += len(n)
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			word := name.FindString(s[i:])
			if keywords[strings.ToUpper(word)] {
				out = append(out, token{kind: tKeyword, text: strings.ToUpper(word)})
			} else {
				out = append(out, token{kind: tIdent, text: word})
			}
			i += len(word)
		default:
			for _, op := range []string{"<>", "!=", "<=", ">=", "==", "=", "<", ">", "-", "+", "*", "/", "||"} {
				if strings.HasPrefix(s[i:], op) {
					out = append(out, token{kind: tOp, text: op})
					i += len(op)
					goto next
				}
			}
			return nil, fmt.Errorf("unexpected %q", c)
		next:
		}
	}
	return append(out, token{kind: tEOF}), nil
}

// --- parser

// kind is an expression's SQL type. Numbers and dates evaluate as float64 (dates in epoch
// milliseconds), strings as string, predicates as true, false or nil for unknown.
type kind uint8

const (
	kNull kind = iota // the NULL literal, of any type
	kNum
	kStr
	kDate
	kBool
)

// node is a parsed expression and its type. A literal converts to the type it is compared with.
type node struct {
	e        expr
	k        kind
	lit, int bool
}

type parser struct {
	toks    []token
	pos     int
	resolve Resolver
	clause  *Clause
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) keyword(k string) bool {
	if t := p.peek(); t.kind == tKeyword && t.text == k {
		p.pos++
		return true
	}
	return false
}

var errSyntax = fmt.Errorf("syntax")

var comparison = map[string]bool{"=": true, "==": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *parser) or() (node, error) {
	return p.chain("OR", p.and)
}

func (p *parser) and() (node, error) {
	return p.chain("AND", p.not)
}

// chain parses predicates joined by the keyword kw, left to right.
func (p *parser) chain(kw string, operand func() (node, error)) (node, error) {
	l, err := operand()
	if err != nil {
		return l, err
	}
	for p.keyword(kw) {
		r, err := operand()
		if err != nil {
			return r, err
		}
		if l.k != kBool || r.k != kBool {
			return r, errSyntax
		}
		l = node{e: logicExpr{kw == "AND", l.e, r.e}, k: kBool}
	}
	return l, nil
}

func (p *parser) not() (node, error) {
	if p.keyword("NOT") {
		n, err := p.not()
		if err != nil || n.k != kBool {
			return n, errSyntax
		}
		return node{e: notExpr{n.e}, k: kBool}, nil
	}
	return p.predicate()
}

func pred(e expr) (node, error) { return node{e: e, k: kBool}, nil }

func (p *parser) predicate() (node, error) {
	l, err := p.sum()
	if err != nil {
		return l, err
	}
	t := p.peek()
	if t.kind == tOp && comparison[t.text] {
		p.pos++
		r, err := p.sum()
		if err != nil {
			return r, err
		}
		if err := p.unify(&l, &r, t.text); err != nil {
			return l, err
		}
		return pred(cmpExpr{t.text, l.e, r.e})
	}
	if p.keyword("IS") {
		neg := p.keyword("NOT")
		if !p.keyword("NULL") {
			return l, errSyntax
		}
		return pred(isNullExpr{l.e, neg})
	}
	neg := p.keyword("NOT")
	switch {
	case p.keyword("IN"):
		if p.next().kind != tLParen {
			return l, errSyntax
		}
		var list []node
		for {
			v, err := p.sum()
			if err != nil {
				return v, err
			}
			list = append(list, v)
			if t := p.next(); t.kind == tRParen {
				break
			} else if t.kind != tComma {
				return l, errSyntax
			}
		}
		if err := p.unifyAll(&l, list, "IN"); err != nil {
			return l, err
		}
		in := inExpr{v: l.e, neg: neg}
		for _, v := range list {
			in.list = append(in.list, v.e)
		}
		return pred(in)
	case p.keyword("LIKE"):
		pat, err := p.sum()
		if err != nil {
			return pat, err
		}
		esc := ""
		if p.keyword("ESCAPE") {
			t := p.next()
			if t.kind != tString {
				return l, errSyntax
			}
			esc = t.text
		}
		if l.k == kBool || l.k == kDate || pat.k != kNull && pat.k != kStr && !(pat.lit && pat.k == kNum) {
			return l, errSyntax
		}
		return pred(&likeExpr{text(l), text(pat), esc, neg, map[string]*regexp.Regexp{}})
	case p.keyword("BETWEEN"):
		lo, err := p.sum()
		if err != nil {
			return lo, err
		}
		if !p.keyword("AND") {
			return l, errSyntax
		}
		hi, err := p.sum()
		if err != nil {
			return hi, err
		}
		bounds := []node{lo, hi}
		if err := p.unifyAll(&l, bounds, "BETWEEN"); err != nil {
			return l, err
		}
		return pred(betweenExpr{l.e, bounds[0].e, bounds[1].e, neg})
	}
	if neg {
		return l, errSyntax
	}
	return l, nil
}

// sum parses terms joined by + - or ||, and term values joined by * or /, left to right.
func (p *parser) sum() (node, error)  { return p.arith(p.term, "+", "-", "||") }
func (p *parser) term() (node, error) { return p.arith(p.value, "*", "/") }

func (p *parser) arith(operand func() (node, error), ops ...string) (node, error) {
	l, err := operand()
	for err == nil && p.peek().kind == tOp && slices.Contains(ops, p.peek().text) {
		op := p.next().text
		var r node
		if r, err = operand(); err == nil {
			l, err = p.combine(op, l, r)
		}
	}
	return l, err
}

// combine types l op r: + adds numbers or joins strings, || joins strings, the rest are numeric.
// Integers divide to an integer, as on SQL Server.
func (p *parser) combine(op string, l, r node) (node, error) {
	if op == "||" {
		if l.k != kStr && l.k != kNull || r.k != kStr && r.k != kNull {
			return l, errSyntax
		}
		return node{e: arithExpr{op: "+", l: l.e, r: r.e}, k: kStr}, nil
	}
	if err := p.unify(&l, &r, op); err != nil {
		return l, err
	}
	k := max(l.k, r.k)
	if k == kDate || k == kStr && op != "+" {
		return l, errSyntax
	}
	if z, ok := r.e.(lit); ok && op == "/" && z.v == 0.0 {
		return l, errSyntax
	}
	return node{e: arithExpr{op, l.e, r.e, l.int && r.int, p.clause}, k: k, int: l.int && r.int}, nil
}

func (p *parser) value() (node, error) {
	t := p.next()
	switch t.kind {
	case tNumber:
		return numLit(t), nil
	case tString:
		return node{e: lit{t.text}, k: kStr, lit: true}, nil
	case tOp:
		if t.text == "-" || t.text == "+" {
			if n := p.next(); n.kind == tNumber {
				v := numLit(n)
				if t.text == "-" {
					v.e = lit{-v.e.(lit).v.(float64)}
				}
				return v, nil
			}
		}
	case tKeyword:
		switch t.text {
		case "NULL":
			return node{e: lit{nil}, k: kNull, lit: true}, nil
		case "TRUE", "FALSE":
			return node{e: lit{t.text == "TRUE"}, k: kBool}, nil
		case "DATE", "TIMESTAMP":
			// A literal only when a string follows (DATE '2020-01-01'); otherwise a column name.
			if p.peek().kind == tString {
				ms, err := parseTime(p.next().text)
				if err != nil {
					return node{}, errSyntax
				}
				return node{e: lit{ms}, k: kDate}, nil
			}
			return p.column(t.text)
		case "ESCAPE":
			return p.column(t.text)
		}
	case tLParen:
		n, err := p.or()
		if err != nil {
			return n, err
		}
		if p.next().kind != tRParen {
			return n, errSyntax
		}
		n.lit = false
		return n, nil
	case tIdent:
		if p.peek().kind == tLParen {
			p.pos++
			var args []node
			if p.peek().kind != tRParen {
				for {
					a, err := p.or()
					if err != nil {
						return a, err
					}
					args = append(args, a)
					if p.peek().kind != tComma {
						break
					}
					p.pos++
				}
			}
			if p.next().kind != tRParen {
				return node{}, errSyntax
			}
			name := strings.ToUpper(t.text)
			if (name == "UPPER" || name == "LOWER" || name == "UCASE" || name == "LCASE") && len(args) == 1 && args[0].k != kBool {
				return node{e: funcExpr{name == "UPPER" || name == "UCASE", text(args[0])}, k: kStr}, nil
			}
			return node{}, errSyntax
		}
		return p.column(t.text)
	}
	return node{}, errSyntax
}

func numLit(t token) node {
	return node{e: lit{t.num}, k: kNum, lit: true, int: !strings.ContainsAny(t.text, ".eE")}
}

func (p *parser) column(name string) (node, error) {
	i, typ, ok := p.resolve(name)
	if !ok {
		return node{}, errSyntax
	}
	switch typ {
	case "Integer":
		return node{e: field(i), k: kNum, int: true}, nil
	case "Double":
		return node{e: field(i), k: kNum}, nil
	case "Date":
		return node{e: field(i), k: kDate}, nil
	}
	return node{e: field(i), k: kStr}, nil
}

// unify gives a and b one type for operator op, as ArcGIS Online does: a literal converts to the
// other side's type (a string to a number or a date, and for = and <> a number to a string), and
// otherwise a string converts to the number it meets as each row is read.
func (p *parser) unify(a, b *node, op string) error {
	if a.k == kBool || b.k == kBool {
		return errSyntax
	}
	if a.k == b.k || a.k == kNull || b.k == kNull {
		return nil
	}
	if b.lit && convert(b, *a, op) || a.lit && convert(a, *b, op) {
		return nil
	}
	if a.k == kStr && b.k == kNum && !a.lit {
		*a = node{e: toNum{a.e, b.int, p.clause}, k: kNum, int: b.int}
		return nil
	}
	if b.k == kStr && a.k == kNum && !b.lit {
		*b = node{e: toNum{b.e, a.int, p.clause}, k: kNum, int: a.int}
		return nil
	}
	return errSyntax
}

// unifyAll unifies v with each of list, twice, so a type v takes from a later item reaches the
// earlier ones.
func (p *parser) unifyAll(v *node, list []node, op string) error {
	for range 2 {
		for i := range list {
			if err := p.unify(v, &list[i], op); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	integer = regexp.MustCompile(`^[-+]?[0-9]+$`)
	decimal = regexp.MustCompile(`^[-+]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][-+]?[0-9]+)?$`)
)

// convert turns literal n into to's type, reporting whether it can.
func convert(n *node, to node, op string) bool {
	v := n.e.(lit).v
	switch {
	case n.k == kStr && to.k == kNum:
		f, ok := parseNum(v.(string), to.int)
		if !ok {
			return false
		}
		*n = node{e: lit{f}, k: kNum, lit: true, int: to.int}
	case n.k == kStr && to.k == kDate:
		ms, err := parseTime(strings.TrimSpace(v.(string)))
		if err != nil {
			return false
		}
		*n = node{e: lit{ms}, k: kDate, lit: true}
	case n.k == kNum && to.k == kStr && (op == "=" || op == "==" || op == "<>" || op == "!="):
		*n = node{e: lit{jsonx.NumberString(v.(float64))}, k: kStr, lit: true}
	default:
		return false
	}
	return true
}

// parseNum reads s as SQL Server converts a string to an integer, or else to a decimal.
func parseNum(s string, integral bool) (float64, bool) {
	s = strings.TrimSpace(s)
	if !integer.MatchString(s) && (integral || !decimal.MatchString(s)) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// text is n as a string, for LIKE and UPPER/LOWER.
func text(n node) expr {
	if n.k == kNum {
		return toText{n.e}
	}
	return n.e
}

func parseTime(s string) (float64, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli()), nil
		}
	}
	return 0, fmt.Errorf("bad time %q", s)
}

// --- evaluation

type lit struct{ v any }
type field int
type toText struct{ e expr }
type toNum struct {
	e      expr
	int    bool
	clause *Clause
}
type logicExpr struct {
	and  bool
	l, r expr
}
type notExpr struct{ e expr }
type arithExpr struct {
	op     string
	l, r   expr
	int    bool
	clause *Clause
}
type cmpExpr struct {
	op   string
	l, r expr
}
type isNullExpr struct {
	v   expr
	neg bool
}
type inExpr struct {
	v    expr
	list []expr
	neg  bool
}
type likeExpr struct {
	v, pattern expr
	escape     string
	neg        bool
	cache      map[string]*regexp.Regexp
}
type betweenExpr struct {
	v, lo, hi expr
	neg       bool
}
type funcExpr struct {
	upper bool
	arg   expr
}

func (e lit) eval(Row) any { return e.v }

// eval reads numbers and dates as float64, and NaN as null.
func (e field) eval(row Row) any {
	switch v := row(int(e)).(type) {
	case int64:
		return float64(v)
	case float64:
		if math.IsNaN(v) {
			return nil
		}
		return v
	case bool:
		if v {
			return 1.0
		}
		return 0.0
	default:
		return v
	}
}

func (e toText) eval(row Row) any {
	if f, ok := e.e.eval(row).(float64); ok {
		return jsonx.NumberString(f)
	}
	return nil
}

func (e toNum) eval(row Row) any {
	v, ok := e.e.eval(row).(string)
	if !ok {
		return nil
	}
	if f, ok := parseNum(v, e.int); ok {
		return f
	}
	e.clause.bad = true
	return nil
}

func (e logicExpr) eval(row Row) any {
	l := e.l.eval(row)
	if l == !e.and {
		return l // AND stops at false, OR at true
	}
	r := e.r.eval(row)
	if l == nil && r != !e.and {
		return nil
	}
	return r
}

func (e arithExpr) eval(row Row) any {
	a, b := e.l.eval(row), e.r.eval(row)
	if a == nil || b == nil {
		return nil
	}
	if s, ok := a.(string); ok {
		return s + b.(string)
	}
	x, y := a.(float64), b.(float64)
	switch e.op {
	case "+":
		return x + y
	case "-":
		return x - y
	case "*":
		return x * y
	}
	if y == 0 {
		e.clause.bad = true // divide by zero
		return nil
	}
	if e.int {
		return math.Trunc(x / y)
	}
	return x / y
}

func (e notExpr) eval(row Row) any {
	if v, ok := e.e.eval(row).(bool); ok {
		return !v
	}
	return nil
}

func (e isNullExpr) eval(row Row) any { return (e.v.eval(row) == nil) != e.neg }

// Fold lower-cases a and b when both are strings, so they compare ignoring case.
func Fold(a, b any) (any, any) {
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return strings.ToLower(sa), strings.ToLower(sb)
		}
	}
	return a, b
}

// compare orders two non-null values of one type, strings ignoring case.
func compare(a, b any) int {
	if x, ok := a.(float64); ok {
		y := b.(float64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	return strings.Compare(strings.ToLower(a.(string)), strings.ToLower(b.(string)))
}

func (e cmpExpr) eval(row Row) any {
	a, b := e.l.eval(row), e.r.eval(row)
	if a == nil || b == nil {
		return nil
	}
	c := compare(a, b)
	switch e.op {
	case "=", "==":
		return c == 0
	case "<>", "!=":
		return c != 0
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	}
	return c >= 0
}

func (e inExpr) eval(row Row) any {
	v := e.v.eval(row)
	if v == nil {
		return nil
	}
	var out any = e.neg
	for _, x := range e.list {
		x := x.eval(row)
		if x == nil {
			out = nil // no match is unknown when the list has a null
		} else if compare(v, x) == 0 {
			return !e.neg
		}
	}
	return out
}

func (e *likeExpr) eval(row Row) any {
	v, pat := e.v.eval(row), e.pattern.eval(row)
	if v == nil || pat == nil {
		return nil
	}
	p := pat.(string)
	re, ok := e.cache[p]
	if !ok {
		re = likeRegexp(p, e.escape)
		e.cache[p] = re
	}
	return re.MatchString(v.(string)) != e.neg
}

func (e betweenExpr) eval(row Row) any {
	v, lo, hi := e.v.eval(row), e.lo.eval(row), e.hi.eval(row)
	in := logicExpr{true, lit{bound(lo, v)}, lit{bound(v, hi)}}.eval(row)
	if b, ok := in.(bool); ok {
		return b != e.neg
	}
	return nil
}

// bound is a <= b, unknown when either is null.
func bound(a, b any) any {
	if a == nil || b == nil {
		return nil
	}
	return compare(a, b) <= 0
}

func (e funcExpr) eval(row Row) any {
	v, ok := e.arg.eval(row).(string)
	if !ok {
		return nil
	}
	if e.upper {
		return strings.ToUpper(v)
	}
	return strings.ToLower(v)
}

// likeRegexp builds a LIKE pattern: % any run, _ one character, case-insensitive.
func likeRegexp(p, escape string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?is)^")
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case escape != "" && string(c) == escape && i+1 < len(rs):
			b.WriteString(regexp.QuoteMeta(string(rs[i+1])))
			i++
		case c == '%':
			b.WriteString(".*")
		case c == '_':
			b.WriteString("[^\n]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
