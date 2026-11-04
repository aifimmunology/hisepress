package hisepress

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var templateRe = regexp.MustCompile(`<var\s+name=["']?(\w+)["']?\s*/>`)

// BuiltInVars are computed when referenced, e.g. <var name=RandomString />.
// Add your own if you need other generated values. A suite variable with the
// same name takes precedence.
var BuiltInVars = map[string]func() string{
	"Timestamp": func() string { return time.Now().Format(time.RFC3339) },
	"RandomInt": func() string {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			return strconv.FormatInt(time.Now().UnixNano()%1_000_000, 10)
		}
		return n.String()
	},
	"RandomString": func() string {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return strconv.FormatInt(time.Now().UnixNano(), 16)[:10]
		}
		return hex.EncodeToString(b)
	},
}

// expand replaces every template reference in s. Built-ins are evaluated once
// per map passed in, so references that share a map agree; nil means just this
// string.
func expand(s string, lookup func(string) (string, bool), builtins map[string]string) (string, error) {
	if !strings.Contains(s, "<var") {
		return s, nil
	}
	if builtins == nil {
		builtins = map[string]string{}
	}
	var firstErr error
	out := templateRe.ReplaceAllStringFunc(s, func(m string) string {
		name := templateRe.FindStringSubmatch(m)[1]
		if v, ok := lookup(name); ok {
			return v
		}
		if v, ok := builtins[name]; ok {
			return v
		}
		if f, ok := BuiltInVars[name]; ok {
			v := f()
			builtins[name] = v
			return v
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("no value for template variable %q in %q", name, s)
		}
		return m
	})
	return out, firstErr
}

// stringify renders a variable value for use in a template.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case fmt.Stringer:
		return t.String()
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// resolveVariables expands template references inside string variables, so
// one variable can be defined in terms of another (or of a built-in). Chains
// are followed; cycles are reported as errors.
func resolveVariables(vars map[string]any) error {
	builtins := map[string]string{}
	for pass := 0; pass <= len(vars); pass++ {
		changed := false
		for k, v := range vars {
			s, ok := v.(string)
			if !ok || !templateRe.MatchString(s) {
				continue
			}
			ns, err := expand(s, func(name string) (string, bool) {
				if name == k {
					return "", false
				}
				ov, has := vars[name]
				if !has {
					return "", false
				}
				if os, isStr := ov.(string); isStr && templateRe.MatchString(os) {
					// not resolved yet; leave for a later pass
					return templateRef(name), true
				}
				return stringify(ov), true
			}, builtins)
			if err != nil {
				return fmt.Errorf("variable %q: %w", k, err)
			}
			if ns != s {
				vars[k] = ns
				changed = true
			}
		}
		if !changed {
			for k, v := range vars {
				if s, ok := v.(string); ok && templateRe.MatchString(s) {
					return fmt.Errorf("variable %q could not be resolved (circular reference?): %s", k, s)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("variables reference each other in a cycle")
}

func templateRef(name string) string { return "<var name=" + name + " />" }

// substituteStep returns a copy of step with every string in it expanded.
// Working on a copy keeps the suite reusable across runs.
func substituteStep(step Step, vars map[string]any) (Step, error) {
	return substitute(step, vars)
}

// substitute returns a copy of v with every string inside it expanded.
func substitute[T any](v T, vars map[string]any) (T, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return v, err
	}
	lookup := func(name string) (string, bool) {
		val, ok := vars[name]
		if !ok {
			return "", false
		}
		return stringify(val), true
	}
	builtins := map[string]string{} // shared, so RandomString agrees across fields
	generic, err = walkStrings(generic, func(s string) (string, error) { return expand(s, lookup, builtins) })
	if err != nil {
		return v, err
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return v, err
	}
	var result T
	dec = json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		return v, err
	}
	return result, nil
}

func walkStrings(v any, f func(string) (string, error)) (any, error) {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			nv, err := walkStrings(t[i], f)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	case map[string]any:
		for k, mv := range t {
			nv, err := walkStrings(mv, f)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}
