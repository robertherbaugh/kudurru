package oidccore

import (
	"context"
	"encoding/json"
	"math"
	"strconv"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/traits"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// CEL compilation and execution budgets are part of the versioned profile.
const (
	MaxExpressions          = 64
	MaxExpressionBytes      = 4096
	MaxExpressionTotalBytes = 64 << 10
	MaxASTNodes             = 1024
	MaxExpressionCost       = 10000
	MaxEvaluationCost       = 100000
)

type resultKind uint8

const (
	boolResult resultKind = iota
	stringResult
	groupsResult
)

type expression struct {
	program   cel.Program
	kind      resultKind
	usesEmail bool
}
type expressionBuilder struct {
	env          *cel.Env
	count, bytes int
	emailClaim   string
}

func (b *expressionBuilder) compile(source string, kind resultKind) (*expression, error) {
	if source == "" {
		return nil, nil
	}
	b.count++
	b.bytes += len(source)
	if len(source) > MaxExpressionBytes || b.bytes > MaxExpressionTotalBytes || b.count > MaxExpressions {
		return nil, ErrBounds
	}
	if b.env == nil {
		env, err := cel.NewEnv(cel.Variable("claims", cel.MapType(cel.StringType, cel.DynType)), cel.ParserExpressionSizeLimit(MaxExpressionBytes), cel.ParserRecursionLimit(64), cel.ParserErrorRecoveryLimit(8))
		if err != nil {
			return nil, ErrPolicy
		}
		b.env = env
	}
	a, issues := b.env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, ErrPolicy
	}
	want := []*cel.Type{cel.BoolType, cel.StringType, cel.ListType(cel.StringType)}[kind]
	// A dynamic output (including list(dyn)) is accepted only provisionally;
	// run enforces the exact bool/string/list(string) result at runtime.
	if !a.OutputType().IsAssignableType(want) {
		return nil, ErrPolicy
	}
	checked, err := cel.AstToCheckedExpr(a)
	if err != nil {
		return nil, ErrPolicy
	}
	nodes := 0
	if !boundedAST(checked.Expr, 0, 0, &nodes) {
		return nil, ErrBounds
	}
	p, err := b.env.Program(a, cel.CostLimit(MaxExpressionCost), cel.InterruptCheckFrequency(16))
	if err != nil {
		return nil, ErrPolicy
	}
	return &expression{program: p, kind: kind, usesEmail: referencesEmail(checked.Expr, b.emailClaim)}, nil
}

func referencesEmail(e *exprpb.Expr, configured string) bool {
	if s := e.GetSelectExpr(); s != nil && s.Operand.GetIdentExpr().GetName() == "claims" && (s.Field == "email" || s.Field == configured) {
		return true
	}
	if c := e.GetCallExpr(); c != nil && c.Function == "_[_]" && len(c.Args) == 2 && c.Args[0].GetIdentExpr().GetName() == "claims" {
		name := c.Args[1].GetConstExpr().GetStringValue()
		// A dynamic top-level claim lookup cannot prove it avoids email.
		if name == "" || name == "email" || name == configured {
			return true
		}
	}
	for _, child := range astChildren(e) {
		if referencesEmail(child, configured) {
			return true
		}
	}
	return false
}

func boundedAST(e *exprpb.Expr, depth, comprehensions int, nodes *int) bool {
	if e == nil || depth > 64 || comprehensions > 4 || *nodes >= MaxASTNodes {
		return false
	}
	*nodes++
	if e.GetComprehensionExpr() != nil {
		comprehensions++
	}
	for _, child := range astChildren(e) {
		if !boundedAST(child, depth+1, comprehensions, nodes) {
			return false
		}
	}
	return true
}

func astChildren(e *exprpb.Expr) []*exprpb.Expr {
	children := []*exprpb.Expr{}
	switch {
	case e.GetCallExpr() != nil:
		c := e.GetCallExpr()
		if c.Target != nil {
			children = append(children, c.Target)
		}
		children = append(children, c.Args...)
	case e.GetSelectExpr() != nil:
		children = append(children, e.GetSelectExpr().Operand)
	case e.GetListExpr() != nil:
		children = append(children, e.GetListExpr().Elements...)
	case e.GetStructExpr() != nil:
		for _, entry := range e.GetStructExpr().Entries {
			if entry.GetMapKey() != nil {
				children = append(children, entry.GetMapKey())
			}
			children = append(children, entry.Value)
		}
	case e.GetComprehensionExpr() != nil:
		c := e.GetComprehensionExpr()
		children = append(children, c.IterRange, c.AccuInit, c.LoopCondition, c.LoopStep, c.Result)
	}
	return children
}

type evaluation struct {
	ctx    context.Context
	claims map[string]any
	input  map[string]any
	cost   uint64
}

func (e *evaluation) run(expr *expression) (any, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	// Reserve one whole per-program budget before execution. Programs stay
	// immutable; a nearly spent aggregate budget denies conservatively rather
	// than recompiling a program or overshooting the cap on the query path.
	if e.cost > MaxEvaluationCost-MaxExpressionCost {
		return nil, ErrBounds
	}
	if e.input == nil {
		v, err := celJSON(e.claims)
		if err != nil {
			return nil, err
		}
		e.input = map[string]any{"claims": v}
	}
	value, details, err := expr.program.ContextEval(e.ctx, e.input)
	if contextErr := e.ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil || types.IsError(value) || types.IsUnknown(value) {
		return nil, ErrPolicy
	}
	if details == nil || details.ActualCost() == nil {
		return nil, ErrPolicy
	}
	e.cost += *details.ActualCost()
	if e.cost > MaxEvaluationCost {
		return nil, ErrBounds
	}
	switch expr.kind {
	case boolResult:
		if v, ok := value.(types.Bool); ok {
			return bool(v), nil
		}
	case stringResult:
		if v, ok := value.(types.String); ok && validOptionalText(string(v), MaxNameBytes) {
			return string(v), nil
		}
	case groupsResult:
		list, ok := value.(traits.Lister)
		if !ok {
			return nil, ErrPolicy
		}
		size, ok := list.Size().(types.Int)
		if !ok || size < 0 || size > MaxGroups {
			return nil, ErrBounds
		}
		out := make([]string, 0, int(size))
		for i := types.Int(0); i < size; i++ {
			v, ok := list.Get(i).(types.String)
			if !ok || !validText(string(v), MaxNameBytes) {
				return nil, ErrPolicy
			}
			out = append(out, string(v))
		}
		return out, nil
	}
	return nil, ErrPolicy
}

// Scalar equality retains exact json.Number spelling. CEL follows its explicit
// int/uint/double model: out-of-range integers and nonfinite decimals deny.
func celJSON(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return i, nil
		}
		if u, err := strconv.ParseUint(string(v), 10, 64); err == nil {
			return u, nil
		}
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, ErrClaims
		}
		// Whole decimal integers outside the supported integer range must not
		// silently lose precision through a double conversion.
		if !containsDecimal(string(v)) {
			return nil, ErrClaims
		}
		return f, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			item, err := celJSON(item)
			if err != nil {
				return nil, err
			}
			out[k] = item
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			item, err := celJSON(item)
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	default:
		return v, nil
	}
}

func containsDecimal(s string) bool {
	for _, c := range s {
		if c == '.' || c == 'e' || c == 'E' {
			return true
		}
	}
	return false
}
