package hisepress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var nonWord = regexp.MustCompile(`\W+`)

// SuiteFileExts are the file extensions LoadSuites will pick up.
var SuiteFileExts = []string{".json", ".yaml", ".yml"}

// ParseSuite decodes a suite. format is "json", "yaml" or "yml". Unknown
// fields are rejected so typos surface immediately. Field names are matched
// case-insensitively (jsonPath and jsonpath both work).
func ParseSuite(data []byte, format string) (*Suite, error) {
	switch strings.ToLower(format) {
	case "json":
	case "yaml", "yml":
		var generic any
		if err := yaml.Unmarshal(data, &generic); err != nil {
			return nil, fmt.Errorf("invalid yaml: %w", err)
		}
		generic, err := jsonCompatible(generic)
		if err != nil {
			return nil, err
		}
		if data, err = json.Marshal(generic); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported suite format %q", format)
	}
	var s Suite
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("invalid suite: %w", err)
	}
	return &s, nil
}

// jsonCompatible converts YAML-decoded values (which may have non-string map
// keys) into values encoding/json can marshal.
func jsonCompatible(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, mv := range t {
			nv, err := jsonCompatible(mv)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, mv := range t {
			nv, err := jsonCompatible(mv)
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = nv
		}
		return out, nil
	case []any:
		for i := range t {
			nv, err := jsonCompatible(t[i])
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	default:
		return v, nil
	}
}

// LoadSuite reads and validates the suite in path. The format comes from the
// file extension.
func LoadSuite(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ParseSuite(data, strings.TrimPrefix(filepath.Ext(path), "."))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.File = path
	if s.Name == "" {
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		s.Name = strings.TrimSpace(nonWord.ReplaceAllString(base, " "))
	}
	if errs := s.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return s, nil
}

// LoadSuites loads every suite file in dir (non-recursively), keeping only
// suites that carry all of the given tags. Results are sorted by file name.
func LoadSuites(dir string, tags ...string) ([]*Suite, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var suites []*Suite
	for _, e := range entries {
		if e.IsDir() || !isSuiteFile(e.Name()) {
			continue
		}
		s, err := LoadSuite(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		keep := true
		for _, t := range tags {
			if !s.HasTag(t) {
				keep = false
				break
			}
		}
		if keep {
			suites = append(suites, s)
		}
	}
	sort.Slice(suites, func(i, j int) bool { return suites[i].File < suites[j].File })
	return suites, nil
}

func isSuiteFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range SuiteFileExts {
		if ext == e {
			return true
		}
	}
	return false
}

// HasTag reports whether the suite has the tag. Matching ignores case and
// punctuation: "IDE Testing", "ide-testing" and "IDETESTING" are the same.
func (s *Suite) HasTag(tag string) bool {
	tag = cleanTag(tag)
	for _, t := range s.Tags {
		if cleanTag(t) == tag {
			return true
		}
	}
	return false
}

func cleanTag(t string) string { return strings.ToLower(nonWord.ReplaceAllString(t, "")) }

// Validate performs static checks and returns every problem found.
func (s *Suite) Validate() []error {
	errs := s.validateAuth()
	if len(s.Steps) == 0 {
		errs = append(errs, fmt.Errorf("suite has no steps"))
	}
	for i, st := range s.Steps {
		name := st.Name
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("%s: %s", name, fmt.Sprintf(format, args...)))
		}
		kinds := 0
		for _, set := range []bool{st.Request != nil, st.Code != nil, st.Exec != nil} {
			if set {
				kinds++
			}
		}
		if kinds > 1 {
			bad("has more than one of request, code and exec; use one")
		}
		if (st.Code != nil || st.Exec != nil) && st.Wait {
			bad("code and exec steps run once and cannot set wait")
		}
		if st.Exec != nil && (len(st.Exec.Command) == 0 || st.Exec.Command[0] == "") {
			bad("exec has no command")
		}
		if st.Request != nil && st.Request.URL == "" {
			bad("request has no url")
		}
		if st.Code != nil && st.Code.Name == "" {
			bad("code has no name")
		}
		for _, a := range st.Assignments {
			if a.Var == "" || a.JSONPath == "" {
				bad("assignments need both var and jsonPath")
			}
			if st.Request == nil && st.Code == nil && st.Exec == nil {
				bad("assignments need a request, code or exec step to read from")
			}
		}
		for _, group := range [][]Condition{st.Conditions, st.SkipConditions, st.FailureConditions} {
			for _, c := range group {
				if strings.TrimSpace(c.Condition) == "" {
					bad("condition is empty")
				}
			}
		}
	}
	return errs
}
