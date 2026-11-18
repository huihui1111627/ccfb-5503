package expr

import (
	"fmt"
	"math"
	"strings"
	"time"

	"dragonwindow/internal/types"
)

// RowContext 是一次逐行求值的上下文。
type RowContext struct {
	// ColMap 按 ColumnRef 指针取绑定后的物理列下标。
	ColMap map[*ColumnRef]int
	Row    int
	Vals   func(col, row int) any
}

// EvalError 表示运行期求值错误（除零、类型不匹配等），与 PostgreSQL 的
// ERROR 语义对齐：整条查询失败而不是产生 NULL。
type EvalError struct{ Msg string }

func (e *EvalError) Error() string { return e.Msg }

func evalErr(format string, args ...any) error {
	return &EvalError{Msg: fmt.Sprintf(format, args...)}
}

// Eval 在给定上下文上求表达式值。
func Eval(n Node, ctx *RowContext) (v any, err error) {
	// 错误集中转换为 EvalError，便于上层区分。
	defer func() {
		if r := recover(); r != nil {
			if ee, ok := r.(*EvalError); ok {
				err = ee
				return
			}
			panic(r)
		}
	}()
	return evalNode(n, ctx), nil
}

func evalNode(n Node, ctx *RowContext) any {
	switch x := n.(type) {
	case *Literal:
		return x.Value
	case *ColumnRef:
		idx := ctx.ColMap[x]
		return ctx.Vals(idx, ctx.Row)
	case *UnaryOp:
		v := evalNode(x.X, ctx)
		if v == nil {
			return nil
		}
		switch x.Op {
		case "-":
			switch t := v.(type) {
			case int64:
				return -t
			case float64:
				return -t
			}
			panic(evalErr("operator does not exist: - %s", types.TypeName(v)))
		case "not":
			b, ok := v.(bool)
			if !ok {
				panic(evalErr("argument of NOT must be boolean, got %s", types.TypeName(v)))
			}
			return !b
		}
		panic(evalErr("unknown unary operator %s", x.Op))
	case *BinaryOp:
		return evalBinary(x, ctx)
	case *BetweenExpr:
		v := evalNode(x.X, ctx)
		lo := evalNode(x.Low, ctx)
		hi := evalNode(x.High, ctx)
		if v == nil || lo == nil || hi == nil {
			return nil
		}
		c1, err := types.Compare(v, lo)
		if err != nil {
			panic(evalErr("%v", err))
		}
		c2, err := types.Compare(v, hi)
		if err != nil {
			panic(evalErr("%v", err))
		}
		res := c1 >= 0 && c2 <= 0
		if x.Negate {
			res = !res
		}
		return res
	case *InList:
		v := evalNode(x.X, ctx)
		if v == nil {
			return nil
		}
		foundNull := false
		for _, e := range x.List {
			m := evalNode(e, ctx)
			if m == nil {
				foundNull = true
				continue
			}
			eq, err := types.Equal(v, m)
			if err != nil {
				panic(evalErr("%v", err))
			}
			if eq {
				return !x.Negate
			}
		}
		if foundNull {
			return nil
		}
		return x.Negate
	case *IsNullExpr:
		v := evalNode(x.X, ctx)
		isNull := v == nil
		if x.Negate {
			return !isNull
		}
		return isNull
	case *CaseExpr:
		for _, w := range x.Whens {
			c := evalNode(w.Cond, ctx)
			if c == nil {
				continue
			}
			b, ok := c.(bool)
			if !ok {
				panic(evalErr("CASE WHEN condition must be boolean, got %s", types.TypeName(c)))
			}
			if b {
				return evalNode(w.Result, ctx)
			}
		}
		if x.Else != nil {
			return evalNode(x.Else, ctx)
		}
		return nil
	case *CastExpr:
		return evalCast(x, ctx)
	case *FuncCall:
		return evalScalarFunc(x, ctx)
	case *WindowCall:
		// 窗口函数在引擎里预先计算并绑定为虚拟列，正常路径不会走到这里。
		panic(evalErr("internal error: window call should have been precomputed"))
	}
	panic(evalErr("internal error: cannot evaluate node %T", n))
}

func evalBinary(x *BinaryOp, ctx *RowContext) any {
	switch x.Op {
	case "and":
		return evalAnd(x, ctx)
	case "or":
		return evalOr(x, ctx)
	}
	a := evalNode(x.X, ctx)
	b := evalNode(x.Y, ctx)
	// 普通运算遇 NULL 得 NULL（比较、算术、LIKE、|| 皆然）。
	if a == nil || b == nil {
		return nil
	}
	switch x.Op {
	case "+", "-", "*", "/", "%":
		return evalArith(x.Op, a, b)
	case "=", "<>", "<", ">", "<=", ">=":
		c, err := types.Compare(a, b)
		if err != nil {
			panic(evalErr("%v", err))
		}
		var res bool
		switch x.Op {
		case "=":
			res = c == 0
		case "<>":
			res = c != 0
		case "<":
			res = c < 0
		case ">":
			res = c > 0
		case "<=":
			res = c <= 0
		case ">=":
			res = c >= 0
		}
		return res
	case "like":
		s, ok1 := a.(string)
		p, ok2 := b.(string)
		if !ok1 || !ok2 {
			panic(evalErr("LIKE requires text operands"))
		}
		return likeMatch(s, p)
	case "||":
		return types.ToText(a) + types.ToText(b)
	}
	panic(evalErr("unknown operator %s", x.Op))
}

// 三值逻辑 AND：false AND NULL = false；NULL AND NULL = NULL。
func evalAnd(x *BinaryOp, ctx *RowContext) any {
	a := evalNode(x.X, ctx)
	if a != nil {
		ab, ok := a.(bool)
		if !ok {
			panic(evalErr("argument of AND must be boolean, got %s", types.TypeName(a)))
		}
		if !ab {
			return false
		}
	}
	b := evalNode(x.Y, ctx)
	if b != nil {
		bb, ok := b.(bool)
		if !ok {
			panic(evalErr("argument of AND must be boolean, got %s", types.TypeName(b)))
		}
		if !bb {
			return false
		}
	}
	if a == nil || b == nil {
		return nil
	}
	return true
}

func evalOr(x *BinaryOp, ctx *RowContext) any {
	a := evalNode(x.X, ctx)
	if a != nil {
		ab, ok := a.(bool)
		if !ok {
			panic(evalErr("argument of OR must be boolean, got %s", types.TypeName(a)))
		}
		if ab {
			return true
		}
	}
	b := evalNode(x.Y, ctx)
	if b != nil {
		bb, ok := b.(bool)
		if !ok {
			panic(evalErr("argument of OR must be boolean, got %s", types.TypeName(b)))
		}
		if bb {
			return true
		}
	}
	if a == nil || b == nil {
		return nil
	}
	return false
}

func evalArith(op string, a, b any) any {
	// interval 运算：timestamp ± interval、interval ± interval
	return arithTyped(op, a, b)
}

func arithTyped(op string, a, b any) any {
	// 时间戳与间隔
	if t1, ok := a.(time.Time); ok {
		if iv, ok := b.(types.Interval); ok {
			if op == "+" || op == "-" {
				if op == "-" {
					iv.Micros = -iv.Micros
				}
				return t1.Add(microsToDuration(iv.Micros))
			}
		}
		if t2, ok := b.(time.Time); ok && op == "-" {
			return types.Interval{Micros: t1.Sub(t2).Microseconds()}
		}
	}
	if iv1, ok := a.(types.Interval); ok {
		if iv2, ok2 := b.(types.Interval); ok2 {
			switch op {
			case "+":
				return types.Interval{Micros: iv1.Micros + iv2.Micros}
			case "-":
				return types.Interval{Micros: iv1.Micros - iv2.Micros}
			}
		}
		if n, ok := numericValue(b); ok && op == "*" {
			return types.Interval{Micros: int64(float64(iv1.Micros) * n)}
		}
	}
	if n, ok := numericValue(a); ok {
		if iv, ok := b.(types.Interval); ok && op == "*" {
			return types.Interval{Micros: int64(n * float64(iv.Micros))}
		}
	}
	// 纯数值
	na, oka := numericValue(a)
	nb, okb := numericValue(b)
	if !oka || !okb {
		panic(evalErr("operator does not exist: %s %s %s", types.TypeName(a), op, types.TypeName(b)))
	}
	_, aIsInt := a.(int64)
	_, bIsInt := b.(int64)
	bothInt := aIsInt && bIsInt
	if bothInt {
		x, y := a.(int64), b.(int64)
		switch op {
		case "+":
			return x + y
		case "-":
			return x - y
		case "*":
			return x * y
		case "/":
			if y == 0 {
				panic(evalErr("division by zero"))
			}
			// PostgreSQL 整数除法向零截断。
			return int64(math.Trunc(float64(x) / float64(y)))
		case "%":
			if y == 0 {
				panic(evalErr("division by zero"))
			}
			return truncatedMod(x, y)
		}
	}
	switch op {
	case "+":
		return na + nb
	case "-":
		return na - nb
	case "*":
		return na * nb
	case "/":
		if nb == 0 {
			panic(evalErr("division by zero"))
		}
		return na / nb
	case "%":
		if nb == 0 {
			panic(evalErr("division by zero"))
		}
		return math.Mod(na, nb)
	}
	panic(evalErr("unknown arithmetic operator %s", op))
}

// truncatedMod 与 PostgreSQL % 对齐：结果符号跟随被除数。
func truncatedMod(x, y int64) int64 {
	q := int64(math.Trunc(float64(x) / float64(y)))
	return x - q*y
}

func numericValue(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

func microsToDuration(micros int64) time.Duration {
	return time.Duration(micros) * time.Microsecond
}

func evalCast(x *CastExpr, ctx *RowContext) any {
	v := evalNode(x.X, ctx)
	if v == nil {
		return nil
	}
	switch x.Type {
	case "integer":
		n, err := types.ToInt(v)
		if err != nil {
			panic(evalErr("%v", err))
		}
		return n
	case "double precision":
		f, err := types.ToFloat(v)
		if err != nil {
			panic(evalErr("%v", err))
		}
		return f
	case "text":
		return types.ToText(v)
	case "boolean":
		b, err := types.ToBool(v)
		if err != nil {
			panic(evalErr("%v", err))
		}
		return b
	case "timestamp with time zone":
		if s, ok := v.(string); ok {
			t, err := ParseTimestampLiteral(s)
			if err != nil {
				panic(evalErr("%v", err))
			}
			return t
		}
		t, err := types.ToTime(v)
		if err != nil {
			panic(evalErr("%v", err))
		}
		return t
	case "interval":
		if s, ok := v.(string); ok {
			iv, err := ParseIntervalLiteral(s)
			if err != nil {
				panic(evalErr("%v", err))
			}
			return iv
		}
		if iv, ok := v.(types.Interval); ok {
			return iv
		}
		panic(evalErr("cannot cast %s to interval", types.TypeName(v)))
	}
	panic(evalErr("unsupported cast target type: %s", x.Type))
}

func evalScalarFunc(f *FuncCall, ctx *RowContext) any {
	args := make([]any, len(f.Args))
	for i, a := range f.Args {
		args[i] = evalNode(a, ctx)
	}
	switch f.Name {
	case "coalesce":
		for _, v := range args {
			if v != nil {
				return v
			}
		}
		return nil
	case "nullif":
		if len(args) != 2 {
			panic(evalErr("nullif takes 2 arguments"))
		}
		if args[0] == nil || args[1] == nil {
			return args[0]
		}
		eq, err := types.Equal(args[0], args[1])
		if err != nil {
			panic(evalErr("%v", err))
		}
		if eq {
			return nil
		}
		return args[0]
	case "greatest", "least":
		var best any
		for _, v := range args {
			if v == nil {
				continue // PostgreSQL: NULL 被忽略
			}
			if best == nil {
				best = v
				continue
			}
			c, err := types.Compare(v, best)
			if err != nil {
				panic(evalErr("%v", err))
			}
			if (f.Name == "greatest" && c > 0) || (f.Name == "least" && c < 0) {
				best = v
			}
		}
		return best
	case "abs":
		oneNumArg(f.Name, args)
		switch x := args[0].(type) {
		case int64:
			if x < 0 {
				return -x
			}
			return x
		case float64:
			return math.Abs(x)
		}
		panic(evalErr("abs requires numeric argument"))
	case "round":
		oneNumArg(f.Name, args)
		x, _ := types.ToFloat(args[0])
		return int64(math.Round(x))
	case "ceil", "ceiling":
		oneNumArg(f.Name, args)
		x, _ := types.ToFloat(args[0])
		return int64(math.Ceil(x))
	case "floor":
		oneNumArg(f.Name, args)
		x, _ := types.ToFloat(args[0])
		return int64(math.Floor(x))
	case "sqrt":
		oneNumArg(f.Name, args)
		x, _ := types.ToFloat(args[0])
		return math.Sqrt(x)
	case "power", "pow":
		if len(args) != 2 {
			panic(evalErr("%s takes 2 arguments", f.Name))
		}
		a, _ := types.ToFloat(args[0])
		b, _ := types.ToFloat(args[1])
		return math.Pow(a, b)
	case "mod":
		if len(args) != 2 {
			panic(evalErr("mod takes 2 arguments"))
		}
		return arithTyped("%", args[0], args[1])
	case "upper":
		oneTextArg(f.Name, args)
		return strings.ToUpper(args[0].(string))
	case "lower":
		oneTextArg(f.Name, args)
		return strings.ToLower(args[0].(string))
	case "length", "char_length", "character_length":
		oneTextArg(f.Name, args)
		return int64(len([]rune(args[0].(string))))
	case "concat":
		var b strings.Builder
		for _, v := range args {
			if v == nil {
				continue // PostgreSQL concat 忽略 NULL
			}
			b.WriteString(types.ToText(v))
		}
		return b.String()
	case "substr", "substring":
		if len(args) < 2 || len(args) > 3 {
			panic(evalErr("%s takes 2 or 3 arguments", f.Name))
		}
		s, ok := args[0].(string)
		if !ok {
			panic(evalErr("%s first argument must be text", f.Name))
		}
		start, _ := types.ToInt(args[1]) // SQL 下标从 1 开始
		runes := []rune(s)
		if start < 1 {
			// PostgreSQL 负值/零值语义：起点平移。
			if int64(len(runes))+start < 1 {
				return ""
			}
		}
		from := int(start) - 1
		if from < 0 {
			from = 0
		}
		if from > len(runes) {
			return ""
		}
		if len(args) == 3 {
			cnt, _ := types.ToInt(args[2])
			if cnt <= 0 {
				return ""
			}
			to := from + int(cnt)
			if to > len(runes) {
				to = len(runes)
			}
			return string(runes[from:to])
		}
		return string(runes[from:])
	case "nullifstr":
		// 内部便捷函数，SQL 里不会出现。
		return args[0]
	case "extract":
		panic(evalErr("EXTRACT not supported; use scalar expressions instead"))
	}
	panic(evalErr("function %s does not exist", f.Name))
}

func oneNumArg(name string, args []any) {
	if len(args) != 1 || !types.IsNumeric(args[0]) {
		panic(evalErr("%s requires one numeric argument", name))
	}
}

func oneTextArg(name string, args []any) {
	if len(args) != 1 {
		panic(evalErr("%s requires one text argument", name))
	}
	if _, ok := args[0].(string); !ok {
		panic(evalErr("%s requires one text argument", name))
	}
}

// likeMatch 实现 SQL LIKE：% 任意序列、_ 单个字符，\\ 转义。
func likeMatch(s, pattern string) bool {
	return likeRecursive(s, pattern)
}

func likeRecursive(s, p string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			// 连续 % 合并。
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if likeRecursive(s[i:], p) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			s = s[1:]
			p = p[1:]
		case '\\':
			if len(p) < 2 {
				return false
			}
			if len(s) == 0 || s[0] != p[1] {
				return false
			}
			s = s[1:]
			p = p[2:]
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
			s = s[1:]
			p = p[1:]
		}
	}
	return len(s) == 0
}
