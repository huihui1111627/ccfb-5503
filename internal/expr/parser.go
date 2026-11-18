package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// parser 递归下降分析器。
type parser struct {
	toks []Token
	pos  int
}

// Parse 解析一条完整 SELECT 语句。
func Parse(sql string) (*SelectStmt, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	if !p.acceptOp(";") {
		// 容忍结尾多余分号之外不允许有内容。
	}
	if p.peek().Kind != TkEOF {
		return nil, p.errf("unexpected token %q after statement", p.peek().Text)
	}
	return stmt, nil
}

// ParseExpr 仅解析一个表达式（供测试与页面内编辑表达式用）。
func ParseExpr(s string) (Node, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TkEOF {
		return nil, p.errf("unexpected token %q after expression", p.peek().Text)
	}
	return n, nil
}

func (p *parser) parseSelect() (*SelectStmt, error) {
	if err := p.expectKeyword("select"); err != nil {
		return nil, err
	}
	stmt := &SelectStmt{}
	items, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	stmt.Selects = items
	if p.acceptKeyword("from") {
		nm, err := p.parseTableName()
		if err != nil {
			return nil, err
		}
		stmt.From = nm
	}
	if p.acceptKeyword("where") {
		stmt.Where, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("group") {
		return nil, p.errf("GROUP BY is not supported by the window engine; use window specifications instead")
	}
	if p.acceptKeyword("window") {
		stmt.Window, err = p.parseWindowDefs()
		if err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("order") {
		if err := p.expectKeyword("by"); err != nil {
			return nil, err
		}
		stmt.OrderBy, err = p.parseOrderItems()
		if err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("limit") {
		t := p.next()
		if t.Kind != TkInt {
			return nil, p.errf("LIMIT expects an integer")
		}
		n, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, p.errf("bad LIMIT: %v", err)
		}
		stmt.Limit = &n
	}
	return stmt, nil
}

func (p *parser) parseSelectList() ([]SelectItem, error) {
	var items []SelectItem
	for {
		if t := p.peek(); t.Kind == TkOp && t.Text == "*" {
			p.next()
			items = append(items, SelectItem{Star: true})
		} else if p.peek().Kind == TkIdent && p.peekAhead(1).Kind == TkOp && p.peekAhead(1).Text == "." &&
			p.peekAhead(2).Kind == TkOp && p.peekAhead(2).Text == "*" {
			tbl := p.next().Text
			p.next() // .
			p.next() // *
			items = append(items, SelectItem{Star: true, StarTable: tbl})
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := SelectItem{Expr: e}
			// 别名：AS x / AS "x" / 裸 ident（非保留字时）。
			if p.acceptKeyword("as") {
				t := p.next()
				if t.Kind != TkIdent {
					return nil, p.errf("expected alias name")
				}
				item.Alias = t.Text
			} else if t := p.peek(); t.Kind == TkIdent && !isReserved(t.Text) {
				item.Alias = p.next().Text
			}
			items = append(items, item)
		}
		if !p.acceptOp(",") {
			break
		}
	}
	return items, nil
}

func (p *parser) parseTableName() (string, error) {
	t := p.next()
	if t.Kind != TkIdent {
		return "", p.errf("expected table name")
	}
	name := t.Text
	// 可选 schema.table
	if p.acceptOp(".") {
		t2 := p.next()
		if t2.Kind != TkIdent {
			return "", p.errf("expected table name after schema.")
		}
		name = t2.Text
	}
	// 吞掉可选别名（AS alias / 裸 alias），引擎单表查询不需要别名绑定。
	if p.acceptKeyword("as") {
		if t := p.peek(); t.Kind == TkIdent && !isReserved(t.Text) {
			p.next()
		}
	} else if t := p.peek(); t.Kind == TkIdent && !isReserved(t.Text) && !isClauseKeyword(t.Text) {
		p.next()
	}
	return name, nil
}

func (p *parser) parseWindowDefs() ([]NamedWindow, error) {
	var defs []NamedWindow
	for {
		nameTok := p.next()
		if nameTok.Kind != TkIdent {
			return nil, p.errf("expected window name")
		}
		// 标准语法 WINDOW name AS (...)，AS 可选。
		p.acceptKeyword("as")
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		spec, err := p.parseOverBody()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		defs = append(defs, NamedWindow{Name: nameTok.Text, Spec: spec})
		if !p.acceptOp(",") {
			break
		}
	}
	return defs, nil
}

// parseOverBody 解析 OVER (...) 内部（不含外层括号）。
func (p *parser) parseOverBody() (*OverSpec, error) {
	spec := &OverSpec{}
	// 具名窗口引用：OVER w 在外层处理；这里处理括号内 "w AS (...)" 风格的
	// 继承：WINDOW w AS (PARTITION BY ...), w2 AS (w ORDER BY ...)。
	if t := p.peek(); t.Kind == TkIdent && !isClauseKeyword(t.Text) &&
		!(p.peekAhead(1).Kind == TkOp && p.peekAhead(1).Text == "(") {
		k := keywordLower(t.Text)
		if k != "partition" && k != "order" && k != "rows" && k != "range" && k != "groups" {
			spec.RefName = p.next().Text
		}
	}
	if p.acceptKeyword("partition") {
		if err := p.expectKeyword("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			spec.Partition = append(spec.Partition, e)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKeyword("order") {
		if err := p.expectKeyword("by"); err != nil {
			return nil, err
		}
		var err error
		spec.OrderBy, err = p.parseOrderItems()
		if err != nil {
			return nil, err
		}
	}
	if k := keywordLower(p.peek().Text); k == "rows" || k == "range" || k == "groups" {
		var err error
		spec.Frame, err = p.parseFrame(k)
		if err != nil {
			return nil, err
		}
	}
	// OVER 内部、帧之后的 IGNORE NULLS / RESPECT NULLS（PostgreSQL 位置）。
	if p.acceptKeyword("ignore") {
		if err := p.expectKeyword("nulls"); err != nil {
			return nil, err
		}
		spec.IgnoreNulls = true
	} else if p.acceptKeyword("respect") {
		if err := p.expectKeyword("nulls"); err != nil {
			return nil, err
		}
		spec.IgnoreNulls = false
	}
	return spec, nil
}

func (p *parser) parseOrderItems() ([]OrderItem, error) {
	var items []OrderItem
	for {
		// 帧关键字（ROWS/RANGE/GROUPS）不是排序项的一部分，提前停止。
		if k := keywordLower(p.peek().Text); k == "rows" || k == "range" || k == "groups" {
			break
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		item := OrderItem{X: e}
		if p.acceptKeyword("asc") {
			item.Desc = false
		} else if p.acceptKeyword("desc") {
			item.Desc = true
		}
		if p.acceptKeyword("nulls") {
			t := p.next()
			k := keywordLower(t.Text)
			if k != "first" && k != "last" {
				return nil, p.errf("expected FIRST or LAST after NULLS")
			}
			item.Nulls = k
		}
		items = append(items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	return items, nil
}

func (p *parser) parseFrame(unit string) (*FrameSpec, error) {
	// 消费单位关键字（ROWS/RANGE/GROUPS），调用方只负责判定是哪一个。
	p.next()
	f := &FrameSpec{Unit: unit}
	if p.acceptKeyword("between") {
		b, err := p.parseFrameBound()
		if err != nil {
			return nil, err
		}
		f.Start = *b
		if err := p.expectKeyword("and"); err != nil {
			return nil, err
		}
		b2, err := p.parseFrameBound()
		if err != nil {
			return nil, err
		}
		f.End = b2
	} else {
		b, err := p.parseFrameBound()
		if err != nil {
			return nil, err
		}
		f.Start = *b
	}
	if p.acceptKeyword("exclude") {
		t := p.next()
		k := keywordLower(t.Text)
		switch k {
		case "current":
			if err := p.expectKeyword("row"); err != nil {
				return nil, err
			}
			f.Exclude = "current row"
		case "group":
			f.Exclude = "group"
		case "ties":
			f.Exclude = "ties"
		case "no":
			if err := p.expectKeyword("others"); err != nil {
				return nil, err
			}
			f.Exclude = "no others"
		default:
			return nil, p.errf("expected EXCLUDE target")
		}
	}
	return f, nil
}

func (p *parser) parseFrameBound() (*FrameBound, error) {
	t := p.next()
	k := keywordLower(t.Text)
	switch k {
	case "unbounded":
		k2, err := p.expectOneOf("preceding", "following")
		if err != nil {
			return nil, err
		}
		return &FrameBound{Kind: "unbounded " + k2}, nil
	case "current":
		if err := p.expectKeyword("row"); err != nil {
			return nil, err
		}
		return &FrameBound{Kind: "current row"}, nil
	case "preceding", "following":
		// 裸 PRECEDING/FOLLOWING 不合法，必须带偏移。
		return nil, p.errf("%s requires an offset expression", strings.ToUpper(k))
	default:
		// 偏移表达式：回退一个 token，按表达式解析，然后吃 PRECEDING/FOLLOWING。
		p.pos--
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		dir, err := p.expectOneOf("preceding", "following")
		if err != nil {
			return nil, err
		}
		return &FrameBound{Kind: dir, Offset: e}, nil
	}
}

// ---- 表达式优先级 --------------------------------------------------------

func (p *parser) parseExpr() (Node, error) { return p.parseOr() }

func (p *parser) parseOr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "or", X: left, Y: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("and") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: "and", X: left, Y: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Node, error) {
	if p.acceptKeyword("not") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &UnaryOp{Op: "not", X: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Node, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.Kind == TkOp {
		switch t.Text {
		case "=", "<>", "!=", "<", ">", "<=", ">=":
			p.next()
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			op := t.Text
			if op == "!=" {
				op = "<>"
			}
			return &BinaryOp{Op: op, X: left, Y: right}, nil
		}
	}
	kw := keywordLower(t.Text)
	switch kw {
	case "is":
		p.next()
		negate := p.acceptKeyword("not")
		if err := p.expectKeyword("null"); err != nil {
			return nil, err
		}
		return &IsNullExpr{X: left, Negate: negate}, nil
	case "between", "not":
		if kw == "not" {
			// NOT BETWEEN / NOT IN
			saved := p.pos
			p.next()
			if k2 := keywordLower(p.peek().Text); k2 == "between" || k2 == "in" {
				n, err := p.parseBetweenIn(left, true)
				if err != nil {
					return nil, err
				}
				return n, nil
			}
			p.pos = saved
			return left, nil
		}
		n, err := p.parseBetweenIn(left, false)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "in":
		return p.parseBetweenIn(left, false)
	case "like":
		p.next()
		right, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		return &BinaryOp{Op: "like", X: left, Y: right}, nil
	}
	return left, nil
}

func (p *parser) parseBetweenIn(left Node, negate bool) (Node, error) {
	kw := keywordLower(p.next().Text) // between / in
	if kw == "between" {
		low, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("and"); err != nil {
			return nil, err
		}
		high, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		return &BetweenExpr{X: left, Low: low, High: high, Negate: negate}, nil
	}
	// IN (...)
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var list []Node
	if !p.checkOp(")") {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &InList{X: left, List: list, Negate: negate}, nil
}

func (p *parser) parseAdditive() (Node, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != TkOp || (t.Text != "+" && t.Text != "-" && t.Text != "||") {
			break
		}
		p.next()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: t.Text, X: left, Y: right}
	}
	return left, nil
}

func (p *parser) parseMultiplicative() (Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != TkOp || (t.Text != "*" && t.Text != "/" && t.Text != "%") {
			break
		}
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryOp{Op: t.Text, X: left, Y: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Node, error) {
	t := p.peek()
	if t.Kind == TkOp && (t.Text == "-" || t.Text == "+") {
		p.next()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if t.Text == "+" {
			return x, nil
		}
		return &UnaryOp{Op: "-", X: x}, nil
	}
	n, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	return n, nil
}

// parsePostfix 处理 ::CAST，再交给 primary。
func (p *parser) parsePostfix() (Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("::") {
		tp, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		n = &CastExpr{X: n, Type: tp}
	}
	return n, nil
}

func (p *parser) parsePrimary() (Node, error) {
	t := p.peek()
	// CAST(expr AS type)：CAST 是保留字，普通 ident 分支不会接收，这里显式处理。
	if t.Kind == TkIdent && keywordLower(t.Text) == "cast" &&
		p.peekAhead(1).Kind == TkOp && p.peekAhead(1).Text == "(" {
		p.next() // cast
		p.next() // (
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("as"); err != nil {
			return nil, err
		}
		tp, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &CastExpr{X: inner, Type: tp}, nil
	}
	switch t.Kind {
	case TkInt:
		p.next()
		n, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, p.errf("bad integer literal: %v", err)
		}
		return &Literal{Value: n}, nil
	case TkFloat:
		p.next()
		f, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return nil, p.errf("bad float literal: %v", err)
		}
		return &Literal{Value: f}, nil
	case TkString:
		p.next()
		// interval '...' / timestamp '...' 类型前缀字面量
		return &Literal{Value: t.Text}, nil
	case TkOp:
		if t.Text == "(" {
			p.next()
			n, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return n, nil
		}
		if t.Text == "*" {
			return nil, p.errf("'*' can only be used in COUNT(*) / SELECT *")
		}
		return nil, p.errf("unexpected operator %q", t.Text)
	case TkIdent:
		return p.parseIdentOrCall()
	default:
		return nil, p.errf("unexpected end of expression")
	}
}

func (p *parser) parseIdentOrCall() (Node, error) {
	t := p.next()
	name := t.Text
	kw := keywordLower(name)

	// 前缀类型字面量：INTERVAL '30 seconds'、TIMESTAMP '2026-...'
	if (kw == "interval" || kw == "timestamp") && p.peek().Kind == TkString {
		st := p.next().Text
		return p.parseTypedLiteral(kw, st)
	}

	// 括号函数调用 / 窗口调用
	if p.acceptOp("(") {
		return p.parseCall(kw)
	}

	// CASE / NULL / TRUE / FALSE
	switch kw {
	case "case":
		return p.parseCase()
	case "null":
		return &Literal{Value: nil}, nil
	case "true":
		return &Literal{Value: true}, nil
	case "false":
		return &Literal{Value: false}, nil
	}

	// 普通列引用，可能带 table. 限定
	ref := &ColumnRef{Name: name}
	if p.acceptOp(".") {
		t2 := p.next()
		if t2.Kind != TkIdent {
			return nil, p.errf("expected column name after '.'")
		}
		ref.Table = name
		ref.Name = t2.Text
	}
	return ref, nil
}

func (p *parser) parseTypedLiteral(kind, text string) (Node, error) {
	switch kind {
	case "interval":
		iv, err := ParseIntervalLiteral(text)
		if err != nil {
			return nil, p.errf("%v", err)
		}
		return &Literal{Value: iv}, nil
	case "timestamp":
		tm, err := ParseTimestampLiteral(text)
		if err != nil {
			return nil, p.errf("%v", err)
		}
		return &Literal{Value: tm}, nil
	}
	return nil, p.errf("unknown typed literal %s", kind)
}

func (p *parser) parseCall(fnLower string) (Node, error) {
	// 窗口函数判定：右括号后是否紧跟 FILTER/OVER。
	call := &FuncCall{Name: fnLower}
	if p.checkOp("*") {
		p.next()
		call.Star = true
	} else if !p.checkOp(")") {
		if p.acceptKeyword("distinct") {
			call.Distinct = true
		}
		for {
			// INTERVAL '..' 这种作为参数时，primary 已能处理前缀。
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, e)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}

	// 窗口调用
	if keywordLower(p.peek().Text) == "filter" || keywordLower(p.peek().Text) == "over" ||
		keywordLower(p.peek().Text) == "ignore" || keywordLower(p.peek().Text) == "respect" {
		wc := &WindowCall{Func: fnLower, Args: call.Args, Star: call.Star, Distinct: call.Distinct}
		// IGNORE NULLS / RESPECT NULLS 可出现在 OVER 之前。
		if p.acceptKeyword("ignore") {
			if err := p.expectKeyword("nulls"); err != nil {
				return nil, err
			}
			wc.IgnoreNulls = true
		} else if p.acceptKeyword("respect") {
			if err := p.expectKeyword("nulls"); err != nil {
				return nil, err
			}
		}
		if p.acceptKeyword("filter") {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			if err := p.expectKeyword("where"); err != nil {
				return nil, err
			}
			f, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			wc.Filter = f
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
		}
		if p.acceptKeyword("over") {
			if p.acceptOp("(") {
				spec, err := p.parseOverBody()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				wc.Over = spec
			} else {
				nt := p.next()
				if nt.Kind != TkIdent {
					return nil, p.errf("expected window name after OVER")
				}
				wc.Over = &OverSpec{RefName: nt.Text}
			}
			// IGNORE NULLS / RESPECT NULLS 也可出现在 OVER 之后。
			if p.acceptKeyword("ignore") {
				if err := p.expectKeyword("nulls"); err != nil {
					return nil, err
				}
				wc.IgnoreNulls = true
			} else if p.acceptKeyword("respect") {
				if err := p.expectKeyword("nulls"); err != nil {
					return nil, err
				}
			}
			// nth_value 的 FROM FIRST/LAST：仅当 FROM 后确实紧跟 FIRST/LAST 时
			// 才消费，否则该 FROM 属于 SELECT ... FROM 表名子句。
			if p.checkKeyword("from") {
				nk := keywordLower(p.peekAhead(1).Text)
				if nk == "first" || nk == "last" {
					p.next()
					if p.acceptKeyword("last") {
						wc.FromLast = true
					} else {
						if err := p.expectKeyword("first"); err != nil {
							return nil, err
						}
					}
				}
			}
			return wc, nil
		}
	}
	return call, nil
}

func (p *parser) parseCase() (Node, error) {
	c := &CaseExpr{}
	var operand Node
	// 简单 CASE：CASE x WHEN v ...
	if !p.checkKeyword("when") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		operand = e
	}
	for p.acceptKeyword("when") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("then"); err != nil {
			return nil, err
		}
		res, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if operand != nil {
			cond = &BinaryOp{Op: "=", X: operand, Y: cond}
		}
		c.Whens = append(c.Whens, When{Cond: cond, Result: res})
	}
	if p.acceptKeyword("else") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.Else = e
	}
	if err := p.expectKeyword("end"); err != nil {
		return nil, err
	}
	return c, nil
}

// parseTypeName 解析 CAST 目标类型名，支持带括号精度的形式（直接吞掉）。
func (p *parser) parseTypeName() (string, error) {
	t := p.next()
	if t.Kind != TkIdent {
		return "", p.errf("expected type name")
	}
	name := keywordLower(t.Text)
	// 多词类型：double precision、timestamp with time zone 等。
	for {
		next := p.peek()
		if next.Kind != TkIdent {
			break
		}
		nk := keywordLower(next.Text)
		if nk == "precision" || nk == "with" || nk == "without" || nk == "zone" || nk == "time" || nk == "varying" {
			p.next()
			name += " " + nk
			continue
		}
		break
	}
	if p.acceptOp("(") {
		// 跳过 (n) 或 (n, m)
		depth := 1
		for depth > 0 {
			t2 := p.next()
			if t2.Kind == TkEOF {
				return "", p.errf("unterminated type parameter list")
			}
			if t2.Kind == TkOp && t2.Text == "(" {
				depth++
			}
			if t2.Kind == TkOp && t2.Text == ")" {
				depth--
			}
		}
	}
	return normalizeTypeName(name), nil
}

func normalizeTypeName(name string) string {
	switch name {
	case "int", "int4", "smallint", "bigint":
		return "integer"
	case "float", "float8", "real", "numeric", "decimal", "double precision":
		return "double precision"
	case "varchar", "character varying", "char", "character", "text":
		return "text"
	case "bool", "boolean":
		return "boolean"
	case "timestamp", "timestamp with time zone", "timestamp without time zone", "timestamptz":
		return "timestamp with time zone"
	case "interval":
		return "interval"
	}
	return name
}

// ---- token 辅助 -----------------------------------------------------------

func (p *parser) peek() Token { return p.toks[p.pos] }
func (p *parser) peekAhead(n int) Token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) checkOp(op string) bool {
	t := p.peek()
	return t.Kind == TkOp && t.Text == op
}

func (p *parser) acceptOp(op string) bool {
	if p.checkOp(op) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errf("expected %q, got %q", op, p.peek().Text)
	}
	return nil
}

func (p *parser) checkKeyword(k string) bool {
	t := p.peek()
	return t.Kind == TkIdent && keywordLower(t.Text) == k
}

func (p *parser) acceptKeyword(k string) bool {
	if p.checkKeyword(k) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectKeyword(k string) error {
	if !p.acceptKeyword(k) {
		return p.errf("expected %s, got %q", strings.ToUpper(k), p.peek().Text)
	}
	return nil
}

func (p *parser) expectOneOf(ks ...string) (string, error) {
	t := p.peek()
	k := keywordLower(t.Text)
	for _, want := range ks {
		if k == want {
			p.next()
			return k, nil
		}
	}
	return "", p.errf("expected one of %v, got %q", ks, t.Text)
}

func (p *parser) errf(format string, args ...any) error {
	t := p.peek()
	return fmt.Errorf("sql parse error at token %q (pos %d): %s", t.Text, t.Pos, fmt.Sprintf(format, args...))
}

func keywordLower(s string) string { return strings.ToLower(s) }

// isReserved 判断 ident 是否为保留字（不能当裸别名）。
func isReserved(s string) bool {
	switch keywordLower(s) {
	case "select", "from", "where", "window", "order", "by", "limit", "as",
		"partition", "rows", "range", "groups", "between", "and", "or", "not",
		"null", "is", "in", "like", "case", "when", "then", "else", "end",
		"asc", "desc", "nulls", "first", "last", "over", "filter", "group",
		"having", "union", "intersect", "except", "on", "join", "left", "right",
		"inner", "outer", "full", "cross", "distinct", "all", "cast", "interval",
		"timestamp", "current", "unbounded", "preceding", "following", "exclude",
		"ties", "others", "true", "false", "escape":
		return true
	}
	return false
}

// isClauseKeyword 用于表名后裸别名判断，避免吞掉子句开头。
func isClauseKeyword(s string) bool {
	switch keywordLower(s) {
	case "where", "window", "order", "limit", "group", "having", "union",
		"intersect", "except", "left", "right", "inner", "full", "cross", "join":
		return true
	}
	return false
}
