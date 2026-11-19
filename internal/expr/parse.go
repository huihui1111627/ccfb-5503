package expr

import (
	"fmt"
	"strconv"
	"strings"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// Parse compiles an expression from its textual form.
//
// Grammar (precedence low to high):
//
//	or_expr    := and_expr (OR and_expr)*
//	and_expr   := not_expr (AND not_expr)*
//	not_expr   := NOT not_expr | cmp_expr
//	cmp_expr   := add_expr ((= <> < <= > >=) add_expr)?
//	add_expr   := mul_expr (('+'|'-') mul_expr)*
//	mul_expr   := unary_expr (('*'|'/'|'%') unary_expr)*
//	unary_expr := ('-'|'+') unary_expr | primary
//	primary    := number | string | TRUE | FALSE | NULL | ident | func '(' args ')'
//	            | '(' or_expr ')' | '(' or_expr ')'::type
//	            | INTERVAL 'literal'
func Parse(text string, env *Env) (Expr, error) {
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, env: env}
	e, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("unexpected token %q in expression %q", p.peek().text, text)
	}
	return e, nil
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokNumber
	tokString
	tokIdent
	tokOp
	tokLParen
	tokRParen
	tokComma
	tokColon
)

type token struct {
	kind tokKind
	text string
}

func lex(s string) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, token{tokLParen, "("})
			i++
		case c == ')':
			out = append(out, token{tokRParen, ")"})
			i++
		case c == ',':
			out = append(out, token{tokComma, ","})
			i++
		case c == ':' && i+1 < len(s) && s[i+1] == ':':
			out = append(out, token{tokColon, "::"})
			i += 2
		case c == '\'':
			j := i + 1
			var b strings.Builder
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			out = append(out, token{tokString, b.String()})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			j := i
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			// exponent
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
			out = append(out, token{tokNumber, s[i:j]})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			out = append(out, token{tokIdent, s[i:j]})
			i = j
		default:
			// operators
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "<=", ">=", "<>", "!=", "::":
				out = append(out, token{tokOp, two})
				i += 2
				continue
			}
			switch c {
			case '+', '-', '*', '/', '%', '=', '<', '>':
				out = append(out, token{tokOp, string(c)})
				i++
			default:
				return nil, fmt.Errorf("unexpected character %q in expression", c)
			}
		}
	}
	out = append(out, token{tokEOF, ""})
	return out, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c == '.'
}

type parser struct {
	toks []token
	pos  int
	env  *Env
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token {
	t := p.toks[p.pos]
	p.pos++
	return t
}
func (p *parser) acceptOp(op string) bool {
	if p.peek().kind == tokOp && strings.EqualFold(p.peek().text, op) {
		p.pos++
		return true
	}
	return false
}
func (p *parser) acceptIdent(kw string) bool {
	if p.peek().kind == tokIdent && strings.EqualFold(p.peek().text, kw) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseOr() (Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptIdent("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = CompileBinary("OR", l, r)
	}
	return l, nil
}

func (p *parser) parseAnd() (Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptIdent("AND") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = CompileBinary("AND", l, r)
	}
	return l, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.acceptIdent("NOT") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return CompileBinary("=", CompileBinary("AND", x, Lit(value.Bool(true))), Lit(value.Bool(false))), nil
	}
	return p.parseCmp()
}

func (p *parser) parseCmp() (Expr, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if p.peek().kind == tokOp {
		op := strings.ToUpper(p.peek().text)
		switch op {
		case "=", "<>", "!=", "<", "<=", ">", ">=":
			p.next()
			if op == "!=" {
				op = "<>"
			}
			r, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			return CompileBinary(op, l, r), nil
		}
	}
	// IS [NOT] NULL
	if p.acceptIdent("IS") {
		negate := p.acceptIdent("NOT")
		if !p.acceptIdent("NULL") {
			return nil, fmt.Errorf("expected NULL after IS")
		}
		e := &isNullExpr{x: l, negate: negate}
		return e, nil
	}
	return l, nil
}

type isNullExpr struct {
	x      Expr
	negate bool
}

func (e *isNullExpr) Eval(row table.Row) (value.Value, error) {
	v, err := e.x.Eval(row)
	if err != nil {
		return value.Null, err
	}
	res := v.IsNull()
	if e.negate {
		res = !res
	}
	return value.Bool(res), nil
}
func (e *isNullExpr) HasColumn() bool  { return e.x.HasColumn() }
func (e *isNullExpr) Kind() value.Kind { return value.KindBool }
func (e *isNullExpr) String() string {
	if e.negate {
		return e.x.String() + " IS NOT NULL"
	}
	return e.x.String() + " IS NULL"
}

func (p *parser) parseAdd() (Expr, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptOp("+"):
			op = "+"
		case p.acceptOp("-"):
			op = "-"
		default:
			return l, nil
		}
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = CompileBinary(op, l, r)
	}
}

func (p *parser) parseMul() (Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptOp("*"):
			op = "*"
		case p.acceptOp("/"):
			op = "/"
		case p.acceptOp("%"):
			op = "%"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = CompileBinary(op, l, r)
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if p.acceptOp("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return CompileUnary("-", x), nil
	}
	if p.acceptOp("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokNumber:
		p.next()
		text := t.text
		if strings.ContainsAny(text, ".eE") {
			// decimal literals are numeric for exactness
			if _, err := strconv.ParseFloat(text, 64); err != nil {
				return nil, fmt.Errorf("bad number %q", text)
			}
			if strings.ContainsAny(text, "eE") {
				f, _ := strconv.ParseFloat(text, 64)
				return Lit(value.Float(f)), nil
			}
			return NumericLiteral(text), nil
		}
		i, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer literal out of range: %q", text)
		}
		return Lit(value.Int(i)), nil
	case tokString:
		p.next()
		return Lit(value.Str(t.text)), nil
	case tokLParen:
		p.next()
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.next()
		return p.maybeCast(e)
	case tokIdent:
		name := t.text
		upper := strings.ToUpper(name)
		switch upper {
		case "NULL":
			p.next()
			return Lit(value.Null), nil
		case "TRUE":
			p.next()
			return Lit(value.Bool(true)), nil
		case "FALSE":
			p.next()
			return Lit(value.Bool(false)), nil
		case "INTERVAL":
			p.next()
			if p.peek().kind == tokString {
				s := p.next().text
				iv, err := ParseInterval(s)
				if err != nil {
					return nil, err
				}
				return Lit(value.IntervalV(iv)), nil
			}
			// INTERVAL '1' DAY style
			if p.peek().kind == tokNumber {
				num := p.next().text
				unit := ""
				if p.peek().kind == tokIdent {
					unit = strings.ToLower(p.next().text)
				}
				iv, err := ParseInterval(num + " " + unit)
				if err != nil {
					return nil, err
				}
				return Lit(value.IntervalV(iv)), nil
			}
			return nil, fmt.Errorf("expected string after INTERVAL")
		case "DATE", "TIMESTAMP", "TIMESTAMPTZ", "TIME":
			p.next()
			if p.peek().kind != tokString {
				return nil, fmt.Errorf("expected string literal after %s", upper)
			}
			s := p.next().text
			v, err := parseTemporalLiteral(upper, s)
			if err != nil {
				return nil, err
			}
			return Lit(v), nil
		case "CAST":
			p.next()
			if p.peek().kind != tokLParen {
				return nil, fmt.Errorf("expected ( after CAST")
			}
			p.next()
			e, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			if !p.acceptIdent("AS") {
				return nil, fmt.Errorf("expected AS in CAST")
			}
			typ, err := p.parseTypeName()
			if err != nil {
				return nil, err
			}
			if p.peek().kind != tokRParen {
				return nil, fmt.Errorf("missing ) in CAST")
			}
			p.next()
			return castExpr(e, typ)
		}
		// function call?
		if p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tokLParen {
			p.next() // name
			p.next() // (
			var args []Expr
			if p.peek().kind != tokRParen {
				for {
					a, err := p.parseOr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peek().kind == tokComma {
						p.next()
						continue
					}
					break
				}
			}
			if p.peek().kind != tokRParen {
				return nil, fmt.Errorf("missing ) after arguments of %s", name)
			}
			p.next()
			return compileFunc(name, args)
		}
		p.next()
		if p.env == nil {
			return nil, fmt.Errorf("no schema to resolve column %q", name)
		}
		return Column(p.env, name)
	}
	return nil, fmt.Errorf("unexpected token %q", t.text)
}

func (p *parser) maybeCast(e Expr) (Expr, error) {
	if p.peek().kind == tokColon {
		p.next()
		typ, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		return castExpr(e, typ)
	}
	return e, nil
}

func (p *parser) parseTypeName() (string, error) {
	if p.peek().kind != tokIdent {
		return "", fmt.Errorf("expected type name")
	}
	t := p.next().text
	typ := strings.ToLower(t)
	// consume optional precision: numeric(10,2)
	if (typ == "numeric" || typ == "decimal") && p.peek().kind == tokLParen {
		p.next()
		for p.peek().kind != tokRParen && p.peek().kind != tokEOF {
			p.next()
		}
		if p.peek().kind != tokRParen {
			return "", fmt.Errorf("unterminated type precision")
		}
		p.next()
	}
	return typ, nil
}
