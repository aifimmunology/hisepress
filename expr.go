package hisepress

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/maja42/goval"
)

// EvalFunctions can be called from condition expressions. Add your own.
var EvalFunctions = map[string]goval.ExpressionFunction{
	"len": func(args ...any) (any, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("len() requires exactly one argument")
		}
		v := reflect.ValueOf(args[0])
		switch v.Kind() {
		case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
			return v.Len(), nil
		default:
			return nil, fmt.Errorf("cannot evaluate the length of a %T", args[0])
		}
	},
}

// evaluate runs expr against vars.
func evaluate(expr string, vars map[string]any) (any, error) {
	return goval.NewEvaluator().Evaluate(expr, vars, EvalFunctions)
}

// matches reports whether an expression's result equals the expected value.
// Numbers compare by value, so 44 (from an expression) equals 44 or 44.0
// (from JSON or YAML).
func matches(expected, got any) bool {
	if ef, ok := asFloat(expected); ok {
		if gf, ok := asFloat(got); ok {
			return ef == gf
		}
		return false
	}
	return reflect.DeepEqual(expected, got)
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}
