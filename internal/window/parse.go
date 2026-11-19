package window

import (
	"fmt"
	"strconv"
	"strings"

	"dragonboat/windowengine/internal/expr"
	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// sqlToken is one lexical token of the query language.
type sqlToken struct {
	kind int // tIdent, tNumber, tString, tPunct, tOp, tEOF
	text string
	pos  int
}

const (
	tEOF = iota
	tIdent
	tNumber
	tString
	tPunct
	tOp
)

func lexSQL(s string) ([]sqlToken, error) {
	var out []sqlToken
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		start := i
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 >= len(s) {
				return nil, fmt.Errorf("unterminated comment")
			}
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
			out = append(out, sqlToken{tString, b.String(), start})
			i = j + 1
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				b.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			out = append(out, sqlToken{tIdent, b.String(), start})
			i = j + 1
		case c >= '0' && c <= '9' || (c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
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
			out = append(out, sqlToken{tNumber, s[i:j], start})
			i = j
		case isSQLIdentStart(c):
			j := i
			for j < len(s) && isSQLIdentPart(s[j]) {
				j++
			}
			out = append(out, sqlToken{tIdent, s[i:j], start})
			i = j
		case c == '(' || c == ')' || c == ',' || c == ';' || c == '*':
			out = append(out, sqlToken{tPunct, string(c), start})
			i++
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "<=", ">=", "<>", "!=", "::":
				out = append(out, sqlToken{tOp, two, start})
				i += 2
				continue
			}
			switch c {
			case '+', '-', '/', '%', '=', '<', '>', '.':
				out = append(out, sqlToken{tOp, string(c), start})
				i++
			default:
				return nil, fmt.Errorf("unexpected character %q", c)
			}
		}
	}
	out = append(out, sqlToken{tEOF, "", len(s)})
	return out, nil
}

func isSQLIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isSQLIdentPart(c byte) bool {
	return isSQLIdentStart(c) || c >= '0' && c <= '9'
}

// ParseQuery parses a query against a table schema.
func ParseQuery(text string, schema *table.Schema) (*Query, string, error) {
	toks, err := lexSQL(text)
	if err != nil {
		return nil, "", err
	}
	p := &sqlParser{toks: toks, text: text, schema: schema}
	return p.parse()
}

type sqlParser struct {
	toks   []sqlToken
	pos    int
	text   string
	schema *table.Schema
	env    *expr.Env
}

func (p *sqlParser) peek() sqlToken { return p.toks[p.pos] }
func (p *sqlParser) peekN(n int) sqlToken {
	if p.pos+n >= len(p.toks) {
		return sqlToken{kind: tEOF}
	}
	return p.toks[p.pos+n]
}
func (p *sqlParser) next() sqlToken {
	t := p.toks[p.pos]
	if t.kind != tEOF {
		p.pos++
	}
	return t
}
func (p *sqlParser) kw(s string) bool {
	t := p.peek()
	return t.kind == tIdent && strings.EqualFold(t.text, s)
}
func (p *sqlParser) acceptKw(s string) bool {
	if p.kw(s) {
		p.pos++
		return true
	}
	return false
}
func (p *sqlParser) expectKw(s string) error {
	if !p.acceptKw(s) {
		return fmt.Errorf("expected %s near %q", s, p.peek().text)
	}
	return nil
}
func (p *sqlParser) acceptPunct(s string) bool {
	t := p.peek()
	if t.kind == tPunct && t.text == s {
		p.pos++
		return true
	}
	return false
}
func (p *sqlParser) expectPunct(s string) error {
	if !p.acceptPunct(s) {
		return fmt.Errorf("expected %q near %q", s, p.peek().text)
	}
	return nil
}

// sliceText returns the original source between two token positions.
func (p *sqlParser) sliceText(from, to int) string {
	if from >= len(p.toks) {
		return ""
	}
	start := p.toks[from].pos
	end := len(p.text)
	if to < len(p.toks) {
		end = p.toks[to].pos
	}
	if start > end {
		return ""
	}
	return strings.TrimSpace(p.text[start:end])
}

func (p *sqlParser) envFor(schema *table.Schema) *expr.Env { return &expr.Env{Schema: schema} }

func (p *sqlParser) parse() (*Query, string, error) {
	if p.schema == nil {
		return nil, "", fmt.Errorf("no input schema")
	}
	p.env = p.envFor(p.schema)
	if err := p.expectKw("SELECT"); err != nil {
		return nil, "", err
	}
	q := &Query{}
	// select list
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, "", err
		}
		if item != nil {
			q.Items = append(q.Items, item)
		}
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectKw("FROM"); err != nil {
		return nil, "", err
	}
	tableName := ""
	if p.peek().kind == tIdent {
		tableName = p.next().text
	} else {
		return nil, "", fmt.Errorf("expected table name after FROM")
	}
	// WINDOW clause
	if p.acceptKw("WINDOW") {
		for {
			if p.peek().kind != tIdent {
				return nil, "", fmt.Errorf("expected window name")
			}
			name := p.next().text
			if err := p.expectKw("AS"); err != nil {
				return nil, "", err
			}
			if err := p.expectPunct("("); err != nil {
				return nil, "", err
			}
			spec, err := p.parseWindowSpec(name)
			if err != nil {
				return nil, "", err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, "", err
			}
			q.Windows = append(q.Windows, spec)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	// Output ORDER BY (optional)
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, "", err
		}
		keys, err := p.parseSortList(&p.pos)
		if err != nil {
			return nil, "", err
		}
		q.OutputOrder = keys
	}
	p.acceptPunct(";")
	if p.peek().kind != tEOF {
		return nil, "", fmt.Errorf("unexpected token %q at end of query", p.peek().text)
	}
	// Resolve window references and validate.
	if err := q.resolve(p); err != nil {
		return nil, "", err
	}
	if tableName == "" {
		return nil, "", fmt.Errorf("missing table")
	}
	return q, tableName, nil
}

// parseSortList parses ORDER BY expressions starting at *pos, stopping at a
// token that is not part of the list.
func (p *sqlParser) parseSortList(pos *int) ([]SortKey, error) {
	var keys []SortKey
	for {
		start := *pos
		// find expression extent: up to ASC/DESC/NULLS/FIRST/LAST/comma/EOF/RParen
		depth := 0
		j := start
		for j < len(p.toks) {
			t := p.toks[j]
			if t.kind == tEOF {
				break
			}
			if t.kind == tPunct {
				if t.text == "(" {
					depth++
				} else if t.text == ")" {
					if depth == 0 {
						break
					}
					depth--
				} else if t.text == "," && depth == 0 {
					break
				}
			}
			if depth == 0 && t.kind == tIdent {
				u := strings.ToUpper(t.text)
				if u == "ASC" || u == "DESC" || u == "NULLS" || u == "ROWS" || u == "RANGE" || u == "GROUPS" || u == "EXCLUDE" {
					break
				}
			}
			j++
		}
		if j == start {
			return nil, fmt.Errorf("empty ORDER BY expression")
		}
		exprText := p.sliceText(start, j)
		e, err := expr.Parse(exprText, p.env)
		if err != nil {
			return nil, fmt.Errorf("ORDER BY: %w", err)
		}
		key := SortKey{Expr: e, Desc: false, NullsFirst: false, Text: exprText, Kind: expr.KindOf(e)}
		if n, err := strconv.Atoi(strings.TrimSpace(exprText)); err == nil && n > 0 {
			key.Ordinal = n
		}
		if p.toks[j].kind == tIdent && strings.EqualFold(p.toks[j].text, "ASC") {
			j++
		} else if p.toks[j].kind == tIdent && strings.EqualFold(p.toks[j].text, "DESC") {
			key.Desc = true
			j++
		}
		if j+1 < len(p.toks) && p.toks[j].kind == tIdent && strings.EqualFold(p.toks[j].text, "NULLS") {
			nxt := p.toks[j+1]
			if nxt.kind == tIdent && strings.EqualFold(nxt.text, "FIRST") {
				key.NullsFirst = true
				j += 2
			} else if nxt.kind == tIdent && strings.EqualFold(nxt.text, "LAST") {
				key.NullsFirst = false
				j += 2
			} else {
				return nil, fmt.Errorf("expected FIRST or LAST after NULLS")
			}
		} else {
			// PostgreSQL defaults: ASC NULLS LAST, DESC NULLS FIRST.
			key.NullsFirst = key.Desc
		}
		keys = append(keys, key)
		*pos = j
		if j < len(p.toks) && p.toks[j].kind == tPunct && p.toks[j].text == "," {
			*pos = j + 1
			continue
		}
		break
	}
	return keys, nil
}

// parseWindowSpec parses the inside of OVER (...).
func (p *sqlParser) parseWindowSpec(name string) (*Spec, error) {
	spec := &Spec{Name: name}
	if p.acceptKw("PARTITION") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			start := p.pos
			j := p.findExprEnd(start, true)
			if j == start {
				return nil, fmt.Errorf("empty PARTITION BY expression")
			}
			text := p.sliceText(start, j)
			e, err := expr.Parse(text, p.env)
			if err != nil {
				return nil, fmt.Errorf("PARTITION BY: %w", err)
			}
			spec.Partition = append(spec.Partition, e)
			spec.PartitionT = append(spec.PartitionT, text)
			p.pos = j
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		pos := p.pos
		keys, err := p.parseSortList(&pos)
		if err != nil {
			return nil, err
		}
		p.pos = pos
		spec.Order = keys
		for _, k := range keys {
			spec.OrderT = append(spec.OrderT, k.Text)
		}
	}
	frame, err := p.parseFrame()
	if err != nil {
		return nil, err
	}
	if frame != nil {
		spec.Frame = frame
		// stop at ')'
	}
	return spec, nil
}

// findExprEnd finds the token index where an expression in a list ends.
func (p *sqlParser) findExprEnd(start int, listContext bool) int {
	depth := 0
	j := start
	for j < len(p.toks) {
		t := p.toks[j]
		if t.kind == tEOF {
			break
		}
		if t.kind == tPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth == 0 {
					return j
				}
				depth--
			case ",":
				if depth == 0 && listContext {
					return j
				}
			}
		}
		if depth == 0 && t.kind == tIdent && isClauseKeyword(t.text) {
			if j > start {
				return j
			}
		}
		j++
	}
	return j
}

// isClauseKeyword lists words that terminate an expression in window clauses.
func isClauseKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "ROWS", "RANGE", "GROUPS", "EXCLUDE", "ASC", "DESC", "NULLS",
		"PRECEDING", "FOLLOWING", "UNBOUNDED", "CURRENT", "AND", "BETWEEN",
		"PARTITION", "ORDER", "OVER", "FILTER":
		return true
	}
	return false
}

// parseFrame parses an optional frame clause.
func (p *sqlParser) parseFrame() (*Frame, error) {
	unit := Rows
	haveUnit := false
	switch {
	case p.acceptKw("ROWS"):
		unit, haveUnit = Rows, true
	case p.acceptKw("RANGE"):
		unit, haveUnit = Range, true
	case p.acceptKw("GROUPS"):
		unit, haveUnit = Groups, true
	}
	if !haveUnit {
		if p.kw("EXCLUDE") {
			return nil, semErrf("EXCLUDE requires a frame clause")
		}
		return nil, nil
	}
	f := &Frame{Unit: unit}
	if p.acceptKw("BETWEEN") {
		start, err := p.parseBound()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("AND"); err != nil {
			return nil, err
		}
		end, err := p.parseBound()
		if err != nil {
			return nil, err
		}
		f.Start = start
		f.End = end
	} else {
		// single-bound form: ROWS <bound> means start = bound, end = CURRENT ROW
		start, err := p.parseBound()
		if err != nil {
			return nil, err
		}
		f.Start = start
		f.End = Bound{Mode: BoundCurrentRow}
	}
	if p.acceptKw("EXCLUDE") {
		switch {
		case p.acceptKw("CURRENT"):
			if err := p.expectKw("ROW"); err != nil {
				return nil, err
			}
			f.Exclusion = ExcludeCurrentRow
		case p.acceptKw("GROUP"):
			f.Exclusion = ExcludeGroup
		case p.acceptKw("TIES"):
			f.Exclusion = ExcludeTies
		case p.acceptKw("NO"):
			if err := p.expectKw("OTHERS"); err != nil {
				return nil, err
			}
			f.Exclusion = ExcludeNoOthers
		default:
			return nil, fmt.Errorf("expected CURRENT ROW, GROUP, TIES or NO OTHERS after EXCLUDE")
		}
	}
	if err := validateFrameSyntax(f); err != nil {
		return nil, err
	}
	return f, nil
}

func (p *sqlParser) parseBound() (Bound, error) {
	if p.acceptKw("UNBOUNDED") {
		switch {
		case p.acceptKw("PRECEDING"):
			return Bound{Mode: BoundUnboundedPreceding}, nil
		case p.acceptKw("FOLLOWING"):
			return Bound{Mode: BoundUnboundedFollowing}, nil
		}
		return Bound{}, fmt.Errorf("expected PRECEDING or FOLLOWING after UNBOUNDED")
	}
	if p.acceptKw("CURRENT") {
		if err := p.expectKw("ROW"); err != nil {
			return Bound{}, err
		}
		return Bound{Mode: BoundCurrentRow}, nil
	}
	// offset expression, then PRECEDING/FOLLOWING
	start := p.pos
	j := p.pos
	depth := 0
	for j < len(p.toks) {
		t := p.toks[j]
		if t.kind == tEOF {
			break
		}
		if t.kind == tPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth == 0 {
					goto scanned
				}
				depth--
			case ",":
				if depth == 0 {
					goto scanned
				}
			}
		}
		if depth == 0 && t.kind == tIdent {
			u := strings.ToUpper(t.text)
			if u == "PRECEDING" || u == "FOLLOWING" || u == "AND" {
				break
			}
		}
		j++
	}
scanned:
	if j == start || j >= len(p.toks) || p.toks[j].kind != tIdent {
		return Bound{}, fmt.Errorf("expected PRECEDING or FOLLOWING after frame offset")
	}
	dir := strings.ToUpper(p.toks[j].text)
	if dir != "PRECEDING" && dir != "FOLLOWING" {
		return Bound{}, fmt.Errorf("expected PRECEDING or FOLLOWING after frame offset, got %q", p.toks[j].text)
	}
	text := p.sliceText(start, j)
	if strings.TrimSpace(text) == "" {
		return Bound{}, fmt.Errorf("missing frame offset expression")
	}
	e, err := expr.Parse(text, p.env)
	if err != nil {
		return Bound{}, fmt.Errorf("frame offset: %w", err)
	}
	p.pos = j + 1
	mode := BoundPreceding
	if dir == "FOLLOWING" {
		mode = BoundFollowing
	}
	return Bound{Mode: mode, Offset: e}, nil
}

func validateFrameSyntax(f *Frame) error {
	if f.Start.Mode == BoundUnboundedFollowing {
		return semErrf("frame start cannot be UNBOUNDED FOLLOWING")
	}
	if f.End.Mode == BoundUnboundedPreceding {
		return semErrf("frame end cannot be UNBOUNDED PRECEDING")
	}
	if f.Start.Mode == BoundCurrentRow && f.End.Mode == BoundPreceding {
		return semErrf("frame starting from current row cannot have preceding rows")
	}
	if f.Start.Mode == BoundFollowing && (f.End.Mode == BoundPreceding || f.End.Mode == BoundCurrentRow) {
		return semErrf("frame starting from following row cannot have preceding rows")
	}
	return nil
}

// parseSelectItem parses one SELECT-list item.
func (p *sqlParser) parseSelectItem() (*WindowItem, error) {
	start := p.pos
	if p.acceptPunct("*") {
		// expand all columns
		return nil, nil // handled by caller? we expand below via wildcard marker
	}
	// Detect a top-level OVER keyword in this item.
	depth := 0
	overIdx := -1
	j := start
	for j < len(p.toks) {
		t := p.toks[j]
		if t.kind == tEOF {
			break
		}
		if t.kind == tPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				if depth == 0 {
					goto done
				}
				depth--
			case ",":
				if depth == 0 {
					goto done
				}
			}
		}
		if depth == 0 && t.kind == tIdent && strings.EqualFold(t.text, "OVER") {
			overIdx = j
			break
		}
		j++
	}
done:
	if overIdx < 0 {
		// passthrough expression, possibly with alias
		end := p.findExprEnd(start, true)
		text := p.sliceText(start, end)
		alias := ""
		// trailing AS ident
		if end >= 2 && p.toks[end-2].kind == tIdent && strings.EqualFold(p.toks[end-2].text, "AS") {
			alias = p.toks[end-1].text
			end -= 2
			text = p.sliceText(start, end)
		}
		e, err := expr.Parse(text, p.env)
		if err != nil {
			return nil, err
		}
		p.pos = p.findExprEnd(p.pos, true)
		if p.pos >= 2 && p.toks[p.pos-2].kind == tIdent && strings.EqualFold(p.toks[p.pos-2].text, "AS") {
			_ = alias
		}
		name := alias
		if name == "" {
			name = defaultName(text)
		}
		item := &WindowItem{OutName: name, SourceText: text, Kind: FuncAggregate}
		item.Passthrough = e
		return item, nil
	}
	// function part: [func call ...] [IGNORE NULLS] [FILTER (...)] OVER ...
	funcStart := start
	// arguments inside first parentheses
	if p.toks[funcStart].kind != tIdent || p.toks[funcStart+1].kind != tPunct || p.toks[funcStart+1].text != "(" {
		return nil, fmt.Errorf("expected window function before OVER")
	}
	fname := p.toks[funcStart].text
	// find matching close paren
	open := funcStart + 1
	depth = 0
	closeIdx := -1
	for k := open; k < len(p.toks); k++ {
		t := p.toks[k]
		if t.kind == tPunct {
			if t.text == "(" {
				depth++
			} else if t.text == ")" {
				depth--
				if depth == 0 {
					closeIdx = k
					break
				}
			}
		}
	}
	if closeIdx < 0 {
		return nil, fmt.Errorf("unbalanced parentheses in function call")
	}
	var args []expr.Expr
	var argTexts []string
	if closeIdx == open+2 && p.toks[open+1].kind == tPunct && p.toks[open+1].text == "*" {
		argTexts = append(argTexts, "*")
	} else if closeIdx > open+1 {
		pos := open + 1
		for {
			end := p.findExprEndAt(pos, closeIdx)
			text := p.sliceText(pos, end)
			e, err := expr.Parse(text, p.env)
			if err != nil {
				return nil, fmt.Errorf("argument of %s: %w", fname, err)
			}
			args = append(args, e)
			argTexts = append(argTexts, text)
			pos = end
			if pos < closeIdx && p.toks[pos].kind == tPunct && p.toks[pos].text == "," {
				pos++
				continue
			}
			break
		}
	}
	k := closeIdx + 1
	ignoreNulls := false
	if k+1 < len(p.toks) && p.toks[k].kind == tIdent && strings.EqualFold(p.toks[k].text, "IGNORE") &&
		p.toks[k+1].kind == tIdent && strings.EqualFold(p.toks[k+1].text, "NULLS") {
		ignoreNulls = true
		k += 2
	}
	var filter expr.Expr
	if k < len(p.toks) && p.toks[k].kind == tIdent && strings.EqualFold(p.toks[k].text, "FILTER") {
		if k+1 >= len(p.toks) || p.toks[k+1].kind != tPunct || p.toks[k+1].text != "(" {
			return nil, fmt.Errorf("expected ( after FILTER")
		}
		flOpen := k + 1
		depth = 0
		flClose := -1
		for m := flOpen; m < len(p.toks); m++ {
			if p.toks[m].kind == tPunct {
				if p.toks[m].text == "(" {
					depth++
				} else if p.toks[m].text == ")" {
					depth--
					if depth == 0 {
						flClose = m
						break
					}
				}
			}
		}
		if flClose < 0 {
			return nil, fmt.Errorf("unbalanced parentheses in FILTER")
		}
		// FILTER (WHERE expr)
		inner := p.sliceText(flOpen+1, flClose)
		inner = strings.TrimSpace(inner)
		if len(inner) >= 5 && strings.EqualFold(inner[:5], "WHERE") {
			inner = strings.TrimSpace(inner[5:])
		}
		fe, err := expr.Parse(inner, p.env)
		if err != nil {
			return nil, fmt.Errorf("FILTER: %w", err)
		}
		filter = fe
		k = flClose + 1
	}
	if k >= len(p.toks) || p.toks[k].kind != tIdent || !strings.EqualFold(p.toks[k].text, "OVER") {
		return nil, fmt.Errorf("expected OVER after window function call")
	}
	k++
	item := &WindowItem{
		Call: FuncCall{
			Name:        fname,
			Args:        args,
			Filter:      filter,
			IgnoreNulls: ignoreNulls,
			ArgTexts:    argTexts,
			Star:        closeIdx == open+2 && p.toks[open+1].kind == tPunct && p.toks[open+1].text == "*",
		},
	}
	kind, err := FuncKindFor(fname)
	if err != nil {
		return nil, err
	}
	item.Kind = kind
	item.SourceText = defaultFuncText(fname, argTexts)
	// window specification: name or (spec)
	if k < len(p.toks) && p.toks[k].kind == tIdent &&
		!(p.toks[k].text == "(") {
		item.RefName = p.toks[k].text
		p.pos = k + 1
	} else {
		if k >= len(p.toks) || p.toks[k].kind != tPunct || p.toks[k].text != "(" {
			return nil, fmt.Errorf("expected window definition after OVER")
		}
		openParen := k
		depth = 0
		closeParen := -1
		for m := openParen; m < len(p.toks); m++ {
			if p.toks[m].kind == tPunct {
				if p.toks[m].text == "(" {
					depth++
				} else if p.toks[m].text == ")" {
					depth--
					if depth == 0 {
						closeParen = m
						break
					}
				}
			}
		}
		if closeParen < 0 {
			return nil, fmt.Errorf("unbalanced parentheses in OVER")
		}
		// Parse spec text between parens using a sub-parser.
		sub := &sqlParser{toks: p.toks[openParen+1 : closeParen], text: p.text, schema: p.schema, env: p.env}
		// Adjust token positions to be relative to the full text: sliceText uses
		// positions, which are absolute, so the sub-parser works unchanged as
		// long as we append EOF.
		sub.toks = append(append([]sqlToken(nil), sub.toks...), sqlToken{kind: tEOF, pos: p.toks[closeParen].pos})
		spec, err := sub.parseWindowSpec("")
		if err != nil {
			return nil, err
		}
		if sub.peek().kind != tEOF {
			return nil, fmt.Errorf("unexpected token %q in window definition", sub.peek().text)
		}
		item.Spec = spec
		p.pos = closeParen + 1
	}
	// optional alias
	if p.acceptKw("AS") {
		if p.peek().kind != tIdent {
			return nil, fmt.Errorf("expected alias after AS")
		}
		item.OutName = p.next().text
	} else if p.peek().kind == tIdent && !p.isSelectListKeyword(p.peek().text) {
		item.OutName = p.next().text
	}
	if item.OutName == "" {
		item.OutName = defaultName(item.SourceText)
	}
	return item, nil
}

func (p *sqlParser) isSelectListKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "FROM", "WHERE", "GROUP", "HAVING", "WINDOW", "ORDER", "LIMIT", "UNION":
		return true
	}
	return false
}

// findExprEndAt finds a comma at the given depth before limit.
func (p *sqlParser) findExprEndAt(start, limit int) int {
	depth := 0
	for j := start; j < limit; j++ {
		t := p.toks[j]
		if t.kind == tPunct {
			switch t.text {
			case "(":
				depth++
			case ")":
				depth--
			case ",":
				if depth == 0 {
					return j
				}
			}
		}
	}
	return limit
}

func defaultName(text string) string {
	t := strings.TrimSpace(text)
	if len(t) > 40 {
		t = t[:40]
	}
	t = strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(t)
	return t
}

func defaultFuncText(name string, args []string) string {
	return strings.ToLower(name) + "(" + strings.Join(args, ", ") + ")"
}

// resolve links window references, validates function arguments and frames.
func (q *Query) resolve(p *sqlParser) error {
	byName := map[string]*Spec{}
	for _, w := range q.Windows {
		byName[w.Name] = w
	}
	for _, it := range q.Items {
		if it.Passthrough != nil {
			continue
		}
		if it.RefName != "" {
			w, ok := byName[it.RefName]
			if !ok {
				return fmt.Errorf("unknown window %q", it.RefName)
			}
			it.Spec = w
		}
		if it.Spec == nil {
			// default: whole partition, no ordering
			it.Spec = &Spec{}
		}
		if err := validateCall(it); err != nil {
			return err
		}
	}
	// Resolve output ORDER BY ordinals against the SELECT list.
	names := make([]string, len(q.Items))
	for i, it := range q.Items {
		names[i] = it.OutName
	}
	for i := range q.OutputOrder {
		k := &q.OutputOrder[i]
		if k.Ordinal > 0 {
			if k.Ordinal > len(names) {
				return fmt.Errorf("ORDER BY position %d is not in select list", k.Ordinal)
			}
			k.OrdinalName = names[k.Ordinal-1]
		}
	}
	return nil
}

func validateCall(it *WindowItem) error {
	sp := it.Spec
	if it.Call.Star && !strings.EqualFold(it.Call.Name, "count") {
		return semErrf("only count(*) is supported")
	}
	// Duplicate window definitions may be identical; nothing to do here.
	switch it.Kind {
	case FuncRanking:
		switch strings.ToLower(it.Call.Name) {
		case "ntile":
			if len(it.Call.Args) != 1 {
				return semErrf("ntile takes exactly one argument")
			}
		default:
			if len(it.Call.Args) != 0 {
				return semErrf("%s takes no arguments", it.Call.Name)
			}
		}
	case FuncOffset:
		if len(it.Call.Args) < 1 || len(it.Call.Args) > 3 {
			return semErrf("%s takes one to three arguments", it.Call.Name)
		}
	case FuncValue:
		switch strings.ToLower(it.Call.Name) {
		case "nth_value":
			if len(it.Call.Args) != 2 {
				return semErrf("nth_value takes two arguments")
			}
		default:
			if len(it.Call.Args) != 1 {
				return semErrf("%s takes one argument", it.Call.Name)
			}
		}
	case FuncAggregate:
		lower := strings.ToLower(it.Call.Name)
		if lower == "count" {
			if len(it.Call.Args) > 1 {
				return semErrf("count takes at most one argument")
			}
		} else {
			if len(it.Call.Args) != 1 {
				return semErrf("%s takes exactly one argument", lower)
			}
			if it.Call.IgnoreNulls {
				return semErrf("IGNORE NULLS is not supported for %s", lower)
			}
		}
	}
	// Frame validation against the spec, as PostgreSQL does.
	if sp.Frame != nil {
		f := sp.Frame
		if f.Unit == Groups && len(sp.Order) == 0 {
			return semErrf("GROUPS mode requires an ORDER BY clause")
		}
		if f.Unit == Range && len(sp.Order) == 0 && hasOffsetBound(f) {
			return semErrf("RANGE with offset PRECEDING/FOLLOWING requires an ORDER BY clause")
		}
		if f.Unit == Range && hasOffsetBound(f) && len(sp.Order) != 1 {
			return semErrf("RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column")
		}
	}
	if it.Kind == FuncRanking || it.Kind == FuncOffset {
		// frame is syntactically allowed but ignored (as in PostgreSQL for
		// ranking functions)
	}
	return nil
}

func hasOffsetBound(f *Frame) bool {
	return f.Start.Mode == BoundPreceding || f.Start.Mode == BoundFollowing ||
		f.End.Mode == BoundPreceding || f.End.Mode == BoundFollowing
}

// parseNumberToken is a helper for tests.
func parseNumberToken(s string) (value.Value, error) {
	if strings.ContainsAny(s, ".") {
		return value.Numeric(s), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return value.Null, err
	}
	return value.Int(n), nil
}
