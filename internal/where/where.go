// Package where parses and evaluates the SQL-92 `where` subset ArcGIS clients send:
// = <> != < <= > >=, AND/OR/NOT, parentheses, [NOT] IN, [NOT] LIKE, IS [NOT] NULL, [NOT] BETWEEN,
// UPPER/LOWER, string and number literals. Comparisons follow what Koop's alasql does: a null
// operand makes a comparison false, `=` is strict, `<>` and the orderings are JavaScript's loose
// ones, and LIKE ignores case. Field names match case-insensitively.
package where

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/UGS-GIO/ugs-warehouse/geoservices/internal/js"
)

// Expr is a compiled where clause.
type Expr interface {
	eval(row Row) any
}

// Row gives a field's value by its resolved index: nil, bool, int64, float64 or string.
type Row func(field int) any

// Resolver maps a field name to an index, or false when there is no such field.
type Resolver func(name string) (int, bool)

// Match reports whether row satisfies e.
func Match(e Expr, row Row) bool { return js.Truthy(e.eval(row)) }

// Error is a where clause that does not parse.
type Error struct{ Where string }

func (e *Error) Error() string { return `Invalid "where" parameter: ` + e.Where }

// Parse compiles where. Fields the resolver does not know evaluate as null.
func Parse(where string, resolve Resolver) (Expr, error) {
	toks, err := lex(where)
	if err != nil {
		return nil, &Error{where}
	}
	p := &parser{toks: toks, resolve: resolve}
	e, err := p.or()
	if err != nil || p.peek().kind != tEOF {
		return nil, &Error{where}
	}
	return e, nil
}

// Fields lists the field names a where clause mentions (lower-cased).
func Fields(where string) []string {
	toks, err := lex(where)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range toks {
		if t.kind == tIdent {
			out = append(out, strings.ToLower(t.text))
		}
	}
	return out
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

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, token{kind: tLParen})
			i++
		case c == ')':
			out = append(out, token{kind: tRParen})
			i++
		case c == ',':
			out = append(out, token{kind: tComma})
			i++
		case c == '\'':
			var b strings.Builder
			i++
			for {
				if i >= len(s) {
					return nil, fmt.Errorf("unterminated string")
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			out = append(out, token{kind: tString, text: b.String()})
		case c == '"' || c == '`' || c == '[':
			end := byte('"')
			if c == '`' {
				end = '`'
			} else if c == '[' {
				end = ']'
			}
			j := strings.IndexByte(s[i+1:], end)
			if j < 0 {
				return nil, fmt.Errorf("unterminated identifier")
			}
			out = append(out, token{kind: tIdent, text: s[i+1 : i+1+j]})
			i += j + 2
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
				k := j + 1
				if k < len(s) && (s[k] == '+' || s[k] == '-') {
					k++
				}
				if k < len(s) && s[k] >= '0' && s[k] <= '9' {
					for k < len(s) && s[k] >= '0' && s[k] <= '9' {
						k++
					}
					j = k
				}
			}
			f, err := strconv.ParseFloat(s[i:j], 64)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tNumber, num: f, text: s[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '.' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			word := s[i:j]
			if keywords[strings.ToUpper(word)] {
				out = append(out, token{kind: tKeyword, text: strings.ToUpper(word)})
			} else {
				out = append(out, token{kind: tIdent, text: word})
			}
			i = j
		default:
			for _, op := range []string{"<>", "!=", "<=", ">=", "==", "=", "<", ">", "-", "+"} {
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

type parser struct {
	toks    []token
	pos     int
	resolve Resolver
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

func (p *parser) or() (Expr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orExpr{l, r}
	}
	return l, nil
}

func (p *parser) and() (Expr, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.keyword("AND") {
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = andExpr{l, r}
	}
	return l, nil
}

func (p *parser) not() (Expr, error) {
	if p.keyword("NOT") {
		e, err := p.not()
		if err != nil {
			return nil, err
		}
		return notExpr{e}, nil
	}
	return p.predicate()
}

func (p *parser) predicate() (Expr, error) {
	l, err := p.value()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tOp && t.text != "-" && t.text != "+" {
		p.pos++
		r, err := p.value()
		if err != nil {
			return nil, err
		}
		return cmpExpr{t.text, l, r}, nil
	}
	if p.keyword("IS") {
		neg := p.keyword("NOT")
		if !p.keyword("NULL") {
			return nil, errSyntax
		}
		return isNullExpr{l, neg}, nil
	}
	neg := p.keyword("NOT")
	switch {
	case p.keyword("IN"):
		if p.next().kind != tLParen {
			return nil, errSyntax
		}
		var list []Expr
		for {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
			if t := p.next(); t.kind == tRParen {
				break
			} else if t.kind != tComma {
				return nil, errSyntax
			}
		}
		return inExpr{l, list, neg}, nil
	case p.keyword("LIKE"):
		pat, err := p.value()
		if err != nil {
			return nil, err
		}
		esc := ""
		if p.keyword("ESCAPE") {
			t := p.next()
			if t.kind != tString {
				return nil, errSyntax
			}
			esc = t.text
		}
		return &likeExpr{v: l, pattern: pat, escape: esc, neg: neg}, nil
	case p.keyword("BETWEEN"):
		lo, err := p.value()
		if err != nil {
			return nil, err
		}
		if !p.keyword("AND") {
			return nil, errSyntax
		}
		hi, err := p.value()
		if err != nil {
			return nil, err
		}
		return betweenExpr{l, lo, hi, neg}, nil
	}
	if neg {
		return nil, errSyntax
	}
	return l, nil
}

func (p *parser) value() (Expr, error) {
	t := p.next()
	switch t.kind {
	case tNumber:
		return lit{t.num}, nil
	case tString:
		return lit{t.text}, nil
	case tOp:
		if t.text == "-" || t.text == "+" {
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			if l, ok := v.(lit); ok {
				if f, ok := l.v.(float64); ok {
					if t.text == "-" {
						f = -f
					}
					return lit{f}, nil
				}
			}
			return nil, errSyntax
		}
	case tKeyword:
		switch t.text {
		case "NULL":
			return lit{nil}, nil
		case "TRUE":
			return lit{true}, nil
		case "FALSE":
			return lit{false}, nil
		case "DATE", "TIMESTAMP":
			// A literal only when a string follows (DATE '2020-01-01'); otherwise a column name.
			if p.peek().kind == tString {
				ms, err := parseTime(p.next().text)
				if err != nil {
					return nil, errSyntax
				}
				return lit{ms}, nil
			}
			return p.column(t.text), nil
		case "ESCAPE":
			return p.column(t.text), nil
		}
	case tLParen:
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next().kind != tRParen {
			return nil, errSyntax
		}
		return e, nil
	case tIdent:
		if p.peek().kind == tLParen {
			p.pos++
			var args []Expr
			if p.peek().kind != tRParen {
				for {
					a, err := p.or()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peek().kind != tComma {
						break
					}
					p.pos++
				}
			}
			if p.next().kind != tRParen {
				return nil, errSyntax
			}
			name := strings.ToUpper(t.text)
			if (name == "UPPER" || name == "LOWER" || name == "UCASE" || name == "LCASE") && len(args) == 1 {
				return funcExpr{name, args[0]}, nil
			}
			return nil, errSyntax
		}
		return p.column(t.text), nil
	}
	return nil, errSyntax
}

// column is a field reference; an unknown name evaluates as null.
func (p *parser) column(name string) Expr {
	if i, ok := p.resolve(name); ok {
		return field(i)
	}
	return lit{nil}
}

func parseTime(s string) (float64, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli()), nil
		}
	}
	return 0, fmt.Errorf("bad time %q", s)
}

// --- evaluation, with JavaScript's value semantics

type lit struct{ v any }
type field int
type andExpr struct{ l, r Expr }
type orExpr struct{ l, r Expr }
type notExpr struct{ e Expr }
type cmpExpr struct {
	op   string
	l, r Expr
}
type isNullExpr struct {
	v   Expr
	neg bool
}
type inExpr struct {
	v    Expr
	list []Expr
	neg  bool
}
type likeExpr struct {
	v, pattern Expr
	escape     string
	neg        bool
	cache      map[string]*regexp.Regexp
}
type betweenExpr struct {
	v, lo, hi Expr
	neg       bool
}
type funcExpr struct {
	name string
	arg  Expr
}

func (e lit) eval(Row) any       { return e.v }
func (e field) eval(row Row) any { return row(int(e)) }
func (e andExpr) eval(row Row) any {
	return js.Truthy(e.l.eval(row)) && js.Truthy(e.r.eval(row))
}
func (e orExpr) eval(row Row) any {
	return js.Truthy(e.l.eval(row)) || js.Truthy(e.r.eval(row))
}
func (e notExpr) eval(row Row) any { return !js.Truthy(e.e.eval(row)) }

func (e isNullExpr) eval(row Row) any { return (e.v.eval(row) == nil) != e.neg }

func (e cmpExpr) eval(row Row) any {
	a, b := e.l.eval(row), e.r.eval(row)
	if js.IsNullish(a) || js.IsNullish(b) {
		return false
	}
	switch e.op {
	case "=", "==":
		return js.StrictEqual(a, b)
	case "<>", "!=":
		return !js.LooseEqual(a, b)
	case "<":
		return js.Less(a, b)
	case ">":
		return js.Less(b, a)
	case "<=":
		return js.LessEqual(a, b)
	case ">=":
		return js.LessEqual(b, a)
	}
	return false
}

func (e inExpr) eval(row Row) any {
	v := e.v.eval(row)
	if js.IsNullish(v) {
		return false
	}
	for _, x := range e.list {
		if js.StrictEqual(v, x.eval(row)) {
			return !e.neg
		}
	}
	return e.neg
}

func (e *likeExpr) eval(row Row) any {
	v, pat := e.v.eval(row), e.pattern.eval(row)
	if js.IsNullish(v) || js.IsNullish(pat) {
		return false
	}
	p := js.String(pat)
	re, ok := e.cache[p]
	if !ok {
		re = likeRegexp(p, e.escape)
		if e.cache == nil {
			e.cache = map[string]*regexp.Regexp{}
		}
		e.cache[p] = re
	}
	return re.MatchString(js.String(v)) != e.neg
}

func (e betweenExpr) eval(row Row) any {
	v, lo, hi := e.v.eval(row), e.lo.eval(row), e.hi.eval(row)
	if js.IsNullish(v) || js.IsNullish(lo) || js.IsNullish(hi) {
		return false
	}
	return (js.LessEqual(lo, v) && js.LessEqual(v, hi)) != e.neg
}

func (e funcExpr) eval(row Row) any {
	v := e.arg.eval(row)
	if v == nil {
		return nil
	}
	if e.name == "UPPER" || e.name == "UCASE" {
		return strings.ToUpper(js.String(v))
	}
	return strings.ToLower(js.String(v))
}

// likeRegexp builds alasql's LIKE pattern: % any run, _ one character, case-insensitive.
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
