package gen

import (
	"fmt"
	"math/rand"
	"strings"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// QueryGen builds random but valid window queries against a schema.
//
// Profile "bounded" restricts frames to bounded offsets, so the brute-force
// reference stays affordable on large datasets.
type QueryGen struct {
	Schema *table.Schema
	Rng    *rand.Rand
	// Profile selects the mix of features: "race" uses race-typical queries,
	// "adversarial" exercises frames and NULLs much harder.
	Profile string
}

// NewQueryGen creates a generator.
func NewQueryGen(schema *table.Schema, seed int64, profile string) *QueryGen {
	return &QueryGen{Schema: schema, Rng: rand.New(rand.NewSource(seed)), Profile: profile}
}

// colKind finds a column by name.
func (g *QueryGen) colName(idx int) string { return g.Schema.Columns[idx].Name }

// columnsOfKinds returns indexes of columns whose kinds satisfy pred.
func (g *QueryGen) columnsOfKinds(pred func(value.Kind) bool) []int {
	var out []int
	for i, c := range g.Schema.Columns {
		if pred(c.Kind) {
			out = append(out, i)
		}
	}
	return out
}

// pick returns a random element.
func (g *QueryGen) pick(xs []int) int { return xs[g.Rng.Intn(len(xs))] }

// QueryCase is a generated query plus its feature tags.
type QueryCase struct {
	SQL      string
	Features []string
}

// Next generates one query.
func (g *QueryGen) Next() QueryCase {
	var b strings.Builder
	var feats []string
	b.WriteString("SELECT ")
	nItems := 1 + g.Rng.Intn(3)
	for i := 0; i < nItems; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		item, f := g.genItem(i)
		b.WriteString(item)
		feats = append(feats, f...)
	}
	b.WriteString(" FROM races")
	if g.Rng.Float64() < 0.15 {
		b.WriteString(" ORDER BY 1")
		feats = append(feats, "output-order")
	}
	return QueryCase{SQL: b.String(), Features: feats}
}

func (g *QueryGen) numCols() []int {
	return g.columnsOfKinds(func(k value.Kind) bool { return k.IsNumericKind() })
}

func (g *QueryGen) allCols() []int {
	out := make([]int, len(g.Schema.Columns))
	for i := range out {
		out[i] = i
	}
	return out
}

func (g *QueryGen) comparableCols() []int {
	return g.columnsOfKinds(func(k value.Kind) bool { return k != value.KindBool })
}

func (g *QueryGen) genItem(i int) (string, []string) {
	var feats []string
	kind := g.genFuncKind()
	switch kind {
	case "aggregate":
		fname := []string{"sum", "count", "min", "max", "avg"}[g.Rng.Intn(5)]
		var arg string
		if fname == "count" && g.Rng.Float64() < 0.3 {
			arg = "*"
		} else {
			cols := g.numCols()
			if len(cols) == 0 {
				arg = "*"
			} else if fname == "count" && g.Rng.Float64() < 0.5 {
				arg = g.colName(g.pick(g.allCols()))
			} else {
				arg = g.colName(g.pick(cols))
			}
		}
		call := fname + "(" + arg + ")"
		if g.Rng.Float64() < 0.08 {
			call += " FILTER (WHERE " + g.genPredicate() + ")"
			feats = append(feats, "filter")
		}
		item := call + " OVER " + g.genWindow(&feats)
		return item + " AS w" + fmt.Sprint(i), append([]string{"aggregate:" + fname}, feats...)
	case "ranking":
		fname := []string{"row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile"}[g.Rng.Intn(6)]
		call := fname + "()"
		if fname == "ntile" {
			call = fmt.Sprintf("ntile(%d)", 1+g.Rng.Intn(8))
		}
		item := call + " OVER " + g.genWindow(&feats)
		return item + " AS w" + fmt.Sprint(i), append([]string{"ranking:" + fname}, feats...)
	case "offset":
		fname := []string{"lag", "lead"}[g.Rng.Intn(2)]
		cols := g.comparableCols()
		arg := g.colName(g.pick(cols))
		call := fname + "(" + arg
		if g.Rng.Float64() < 0.7 {
			call += ", " + g.genOffsetExpr(false)
			if g.Rng.Float64() < 0.5 {
				call += ", " + g.genDefaultExpr(arg)
			}
		}
		call += ")"
		if g.Rng.Float64() < 0.2 {
			call += " IGNORE NULLS"
			feats = append(feats, "ignore-nulls")
		}
		item := call + " OVER " + g.genWindow(&feats)
		return item + " AS w" + fmt.Sprint(i), append([]string{"offset:" + fname}, feats...)
	default:
		fname := []string{"first_value", "last_value", "nth_value"}[g.Rng.Intn(3)]
		cols := g.comparableCols()
		arg := g.colName(g.pick(cols))
		call := fname + "(" + arg
		if fname == "nth_value" {
			call += ", " + fmt.Sprint(1+g.Rng.Intn(5))
		}
		call += ")"
		if g.Rng.Float64() < 0.2 {
			call += " IGNORE NULLS"
			feats = append(feats, "ignore-nulls")
		}
		item := call + " OVER " + g.genWindow(&feats)
		return item + " AS w" + fmt.Sprint(i), append([]string{"value:" + fname}, feats...)
	}
}

func (g *QueryGen) genFuncKind() string {
	r := g.Rng.Float64()
	switch {
	case r < 0.45:
		return "aggregate"
	case r < 0.65:
		return "ranking"
	case r < 0.83:
		return "offset"
	default:
		return "value"
	}
}

// genPredicate builds a simple boolean expression over a numeric column.
func (g *QueryGen) genPredicate() string {
	cols := g.numCols()
	c := g.colName(g.pick(cols))
	op := []string{">", ">=", "<", "<=", "=", "<>"}[g.Rng.Intn(6)]
	return fmt.Sprintf("%s %s %d", c, op, g.Rng.Intn(6)*100)
}

// genOffsetExpr builds a frame offset expression: a constant, or a per-row
// expression over integer columns (the engine's extension).
func (g *QueryGen) genOffsetExpr(allowColumn bool) string {
	if allowColumn && g.Rng.Float64() < 0.5 {
		// per-row expression, guaranteed non-negative
		return g.genNonNegativeIntExpr()
	}
	choices := []string{"0", "1", "2", "3", "5", "10", fmt.Sprintf("%d", 1+g.Rng.Intn(4))}
	return choices[g.Rng.Intn(len(choices))]
}

// genNonNegativeIntExpr builds an expression guaranteed to be >= 0 for every
// row: abs(e) or e % k with k>0 never negative.
func (g *QueryGen) genNonNegativeIntExpr() string {
	intCols := g.columnsOfKinds(func(k value.Kind) bool { return k == value.KindInt })
	if len(intCols) == 0 {
		return "1"
	}
	c := g.colName(g.pick(intCols))
	switch g.Rng.Intn(3) {
	case 0:
		return fmt.Sprintf("coalesce(abs(%s) %% %d, 0)", c, 1+g.Rng.Intn(5))
	case 1:
		return fmt.Sprintf("coalesce(%s %% %d, 1)", c, 1+g.Rng.Intn(4))
	default:
		return fmt.Sprintf("(coalesce(%s %% %d, 0))::int + %d", c, 1+g.Rng.Intn(3), g.Rng.Intn(2))
	}
}

func (g *QueryGen) genDefaultExpr(arg string) string {
	kind := value.KindNull
	if idx, ok := g.Schema.Index(arg); ok {
		kind = g.Schema.Columns[idx].Kind
	}
	switch kind {
	case value.KindInt, value.KindFloat, value.KindNumeric:
		switch g.Rng.Intn(4) {
		case 0:
			return "0"
		case 1:
			return "-1"
		case 2:
			return "NULL"
		default:
			return arg + " * 0 - 999"
		}
	case value.KindString:
		return []string{"''", "NULL", "coalesce(" + arg + ", '')"}[g.Rng.Intn(3)]
	default:
		return "NULL"
	}
}

// genWindow builds "OVER (...)" with random partition/order/frame.
func (g *QueryGen) genWindow(feats *[]string) string {
	var parts []string
	var order []string
	if g.Rng.Float64() < 0.65 {
		// partition by 1 or 2 columns
		k := 1
		if g.Rng.Float64() < 0.3 {
			k = 2
		}
		used := map[string]bool{}
		var cols []string
		for len(cols) < k {
			c := g.colName(g.pick(g.allCols()))
			if used[c] {
				break
			}
			used[c] = true
			cols = append(cols, c)
		}
		parts = append(parts, "PARTITION BY "+strings.Join(cols, ", "))
		*feats = append(*feats, "partition")
	}
	hasOrder := g.Rng.Float64() < 0.9
	var orderKind value.Kind
	if hasOrder {
		oc := g.pick(g.comparableCols())
		orderKind = g.Schema.Columns[oc].Kind
		key := g.colName(oc)
		if g.Rng.Float64() < 0.25 {
			key = key + " " + []string{"ASC", "DESC"}[g.Rng.Intn(2)]
		}
		if g.Rng.Float64() < 0.45 {
			key = strings.TrimSpace(key) + " " + []string{"NULLS FIRST", "NULLS LAST"}[g.Rng.Intn(2)]
			*feats = append(*feats, "nulls-placement")
		}
		order = append(order, "ORDER BY "+key)
		// occasionally add a second order key
		if g.Rng.Float64() < 0.12 {
			oc2 := g.pick(g.comparableCols())
			order[0] += ", " + g.colName(oc2)
			*feats = append(*feats, "multi-order")
		}
	}
	singleOrder := len(order) == 1
	if singleOrder && strings.Contains(order[0], ",") {
		singleOrder = false
	}
	frame := g.genFrame(hasOrder, orderKind, singleOrder, order)
	parts = append(parts, order...)
	if frame != "" {
		parts = append(parts, frame)
	}
	if len(parts) == 0 {
		return "()"
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// genFrame builds a frame clause.
func (g *QueryGen) genFrame(hasOrder bool, orderKind value.Kind, singleOrder bool, order []string) string {
	if g.Profile == "bounded" {
		return g.genBoundedFrame(hasOrder)
	}
	unit := "ROWS"
	if hasOrder {
		switch g.Rng.Intn(10) {
		case 0, 1:
			unit = "RANGE"
		case 2:
			unit = "GROUPS"
		}
	} else {
		// Without ORDER BY only ROWS makes sense for bounds.
		unit = "ROWS"
	}
	if unit == "RANGE" {
		if !singleOrder {
			// RANGE with offset requires exactly one ORDER BY column.  Without
			// offsets, RANGE CURRENT ROW is legal with multiple keys, so keep
			// RANGE only for the non-offset default frame.
			unit = "RANGE-NO-OFFSET"
		} else if !(orderKind.IsNumericKind() || orderKind.IsTemporalKind() || orderKind == value.KindInterval) {
			unit = "ROWS"
		}
	}
	// Exclusion
	excl := ""
	switch g.Rng.Intn(10) {
	case 0:
		excl = " EXCLUDE CURRENT ROW"
	case 1:
		excl = " EXCLUDE GROUP"
	case 2:
		excl = " EXCLUDE TIES"
	case 3:
		excl = " EXCLUDE NO OTHERS"
	}
	// Decide whether to use unbounded/current/offset bounds.
	if unit == "RANGE-NO-OFFSET" {
		if excl != "" || g.Rng.Float64() < 0.5 {
			return fmt.Sprintf("RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW%s", excl)
		}
		return ""
	}
	start, end := g.genBounds(unit, orderKind, hasOrder)
	// Default frame (no explicit clause) sometimes.
	if start == "" && end == "" && excl == "" && g.Rng.Float64() < 0.5 {
		return ""
	}
	if start == "" && end == "" {
		start, end = "UNBOUNDED PRECEDING", "CURRENT ROW"
		if !hasOrder {
			start, end = "UNBOUNDED PRECEDING", "UNBOUNDED FOLLOWING"
		}
	}
	return fmt.Sprintf("%s BETWEEN %s AND %s%s", unit, start, end, excl)
}

// genBounds returns start and end bound text.  Empty strings mean "use the
// default", which the caller may exploit to omit the frame.
func (g *QueryGen) genBounds(unit string, orderKind value.Kind, hasOrder bool) (string, string) {
	// 20% of the time omit the frame entirely (use defaults)
	if g.Rng.Float64() < 0.2 {
		return "", ""
	}
	var start, end string

	offsetFor := func(mode string, isStart bool) string {
		switch unit {
		case "ROWS":
			// per-row offsets are allowed and are an extension
			if g.Rng.Float64() < 0.35 {
				return g.genNonNegativeIntExpr() + " " + mode
			}
			return g.genOffsetExpr(false) + " " + mode
		case "GROUPS":
			return g.genOffsetExpr(false) + " " + mode
		case "RANGE":
			return g.genRangeOffset(orderKind, mode)
		}
		return "1 " + mode
	}

	switch g.Rng.Intn(6) {
	case 0:
		start = "UNBOUNDED PRECEDING"
	case 1:
		start = "CURRENT ROW"
	case 2, 3:
		start = offsetFor("PRECEDING", true)
	case 4:
		start = offsetFor("FOLLOWING", true)
	case 5:
		start = offsetFor("PRECEDING", true)
	}
	switch g.Rng.Intn(6) {
	case 0:
		end = "UNBOUNDED FOLLOWING"
	case 1:
		end = "CURRENT ROW"
	case 2, 3:
		end = offsetFor("PRECEDING", false)
	case 4, 5:
		end = offsetFor("FOLLOWING", false)
	}
	// Fix illegal combinations the same way the SQL standard rejects them,
	// but generate only legal ones: if start is FOLLOWING, end must be
	// FOLLOWING/UNBOUNDED FOLLOWING; if start is CURRENT, end != PRECEDING.
	if isFollowing(start) {
		end = offsetFor("FOLLOWING", false)
	}
	if start == "CURRENT ROW" && isPreceding(end) {
		end = "CURRENT ROW"
	}
	if !hasOrder {
		// GROUPS is invalid without ORDER BY; caller already avoids it.
	}
	return start, end
}

func isFollowing(s string) bool {
	return strings.HasSuffix(s, "FOLLOWING") && !strings.HasPrefix(s, "UNBOUNDED")
}
func isPreceding(s string) bool {
	return strings.HasSuffix(s, "PRECEDING") && !strings.HasPrefix(s, "UNBOUNDED")
}

// genRangeOffset builds a RANGE offset appropriate for the order column type.
func (g *QueryGen) genRangeOffset(orderKind value.Kind, mode string) string {
	switch {
	case orderKind.IsNumericKind():
		// integer order columns require an integral offset
		if orderKind == value.KindInt {
			return fmt.Sprintf("%d %s", g.Rng.Intn(50), mode)
		}
		if g.Rng.Float64() < 0.5 {
			return fmt.Sprintf("%d %s", g.Rng.Intn(50), mode)
		}
		return fmt.Sprintf("%.1f %s", float64(g.Rng.Intn(100))/10, mode)
	case orderKind.IsTemporalKind():
		units := []string{"seconds", "minutes", "hours", "days"}
		return fmt.Sprintf("INTERVAL '%d %s' %s", 1+g.Rng.Intn(120), units[g.Rng.Intn(len(units))], mode)
	case orderKind == value.KindInterval:
		return fmt.Sprintf("INTERVAL '%d minutes' %s", 1+g.Rng.Intn(60), mode)
	default:
		// should not happen: choose numeric fallback
		return fmt.Sprintf("%d %s", g.Rng.Intn(5), mode)
	}
}

// GenCorpus builds n queries with mixed profiles.
func GenCorpus(schema *table.Schema, n int, seed int64, profile string) ([]QueryCase, error) {
	qg := NewQueryGen(schema, seed, profile)
	out := make([]QueryCase, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, qg.Next())
	}
	return out, nil
}

// genBoundedFrame builds a frame whose both bounds are small constant offsets,
// keeping the brute-force reference O(n·k).
func (g *QueryGen) genBoundedFrame(hasOrder bool) string {
	if !hasOrder {
		return ""
	}
	// Only ROWS keeps the brute-force reference at O(n*k): a GROUPS offset
	// bounds the number of peer groups, not rows, so a low-cardinality order
	// column can still produce huge frames, and RANGE edges are found by
	// scanning.  ROWS/GROUPS/RANGE are all covered by the moderate-size pass.
	unit := "ROWS"
	off := func(mode string) string {
		return fmt.Sprintf("%d %s", 1+g.Rng.Intn(20), mode)
	}
	start := off("PRECEDING")
	end := off("FOLLOWING")
	excl := ""
	switch g.Rng.Intn(8) {
	case 0:
		excl = " EXCLUDE CURRENT ROW"
	case 1:
		excl = " EXCLUDE GROUP"
	case 2:
		excl = " EXCLUDE TIES"
	}
	return fmt.Sprintf("%s BETWEEN %s AND %s%s", unit, start, end, excl)
}
