// Package hisepress runs declarative, multi-step API integration tests.
//
// A Suite is a list of Steps. Each step either makes an HTTP request or calls
// a registered Go function. Values can be captured from results with jsonpath
// and assigned to variables, which later steps reference with
// <var name=foo />. Steps are checked with boolean expressions, can be skipped
// conditionally, and can retry until they succeed (wait).
package hisepress

import (
	"bytes"
	"encoding/json"
)

// Run and step status values.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusError   = "error"
	StatusSkipped = "skipped"
)

// Suite is a named, ordered list of steps plus the variables they share.
type Suite struct {
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Variables   map[string]any `json:"variables,omitempty"`
	// Auth declares named authentication profiles (see AuthProfile).
	Auth map[string]AuthProfile `json:"auth,omitempty"`
	// DefaultAuth names the profile used by steps that don't set their own.
	DefaultAuth string `json:"defaultAuth,omitempty"`
	// Secrets names variables whose values are redacted from logs and reports.
	Secrets []string `json:"secrets,omitempty"`
	Steps   []Step   `json:"steps"`

	// File is the path the suite was loaded from, if any.
	File string `json:"-"`
}

// Step is one unit of work in a suite. It has at most one of Request, Code and
// Exec; with none it only evaluates its conditions.
type Step struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Request     *Request `json:"request,omitempty"`
	Code        *Code    `json:"code,omitempty"`
	Exec        *Exec    `json:"exec,omitempty"`
	// Auth names the profile for this step's request: "none" for no
	// authentication, or empty for the suite's defaultAuth.
	Auth string `json:"auth,omitempty"`

	// Wait retries the step (with exponential backoff, capped at one minute)
	// until it succeeds, times out, or a failure condition is met.
	Wait             bool `json:"wait,omitempty"`
	TimeoutInMinutes int  `json:"timeoutInMinutes,omitempty"`

	AlwaysRun      bool `json:"alwaysRun,omitempty"`
	AlwaysContinue bool `json:"alwaysContinue,omitempty"`

	// ExpectedHTTPStatus is the exact status to expect. When zero, any 2xx is
	// a success.
	ExpectedHTTPStatus    int  `json:"expectedHTTPStatus,omitempty"`
	ExpectedResultsLength *int `json:"expectedResultsLength,omitempty"`

	Assignments       []Assignment `json:"assignments,omitempty"`
	Conditions        []Condition  `json:"conditions,omitempty"`
	SkipConditions    []Condition  `json:"skipConditions,omitempty"`
	FailureConditions []Condition  `json:"failureConditions,omitempty"`

	artifacts []Artifact
	outDir    string
}

// Request describes an HTTP request.
type Request struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Params  map[string]any    `json:"params,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Body is sent as JSON when it is a map or list, and as-is when it is a
	// string. When Files is set, a map Body supplies extra form fields.
	Body any `json:"body,omitempty"`
	// Files maps multipart form field names to local file paths, relative to
	// the suite file's directory.
	Files map[string]string `json:"files,omitempty"`
	// FollowRedirects defaults to true. Set false to receive a 3xx response
	// as the result (for example to assert expectedHTTPStatus: 302).
	FollowRedirects *bool `json:"followRedirects,omitempty"`
}

// Code calls a registered function (see Register).
type Code struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args,omitempty"`
}

// Condition is an expression and the value it is expected to evaluate to
// (true if expectedValue is omitted). Expressions can reference suite
// variables and the functions in EvalFunctions.
type Condition struct {
	Condition     string `json:"condition"`
	ExpectedValue any    `json:"expectedValue"`
}

// UnmarshalJSON makes an omitted expectedValue mean true, and keeps unknown
// fields an error (decoders don't pass DisallowUnknownFields into custom
// unmarshalers).
func (c *Condition) UnmarshalJSON(b []byte) error {
	var aux struct {
		Condition     string          `json:"condition"`
		ExpectedValue json.RawMessage `json:"expectedValue"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&aux); err != nil {
		return err
	}
	c.Condition = aux.Condition
	if aux.ExpectedValue == nil {
		c.ExpectedValue = true
		return nil
	}
	c.ExpectedValue = nil
	return json.Unmarshal(aux.ExpectedValue, &c.ExpectedValue)
}

// Assignment copies the value at JSONPath in a step's result into a variable.
type Assignment struct {
	Var      string `json:"var"`
	JSONPath string `json:"jsonPath"`
}

// Artifact is a file produced by a run.
type Artifact struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// AddArtifact records a file produced by this step. Intended for use by
// registered code functions.
func (s *Step) AddArtifact(artifactType, path string) {
	s.artifacts = append(s.artifacts, Artifact{Type: artifactType, Path: path})
}

// OutDir is the directory (from Options.OutDir) where a code function should
// write any files it produces. It is empty if none was configured.
func (s *Step) OutDir() string { return s.outDir }
