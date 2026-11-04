package hisepress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 42, "name": in["name"]})
		default:
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "first"}, {"id": 2, "name": "second"}})
		}
	})
	mux.HandleFunc("/items/42", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "tag": r.URL.Query().Get("tag"), "auth": r.Header.Get("X-Test")})
	})
	mux.HandleFunc("/job", func(w http.ResponseWriter, r *http.Request) {
		state := "running"
		if polls.Add(1) >= 3 {
			state = "done"
		}
		json.NewEncoder(w).Encode(map[string]any{"state": state})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return httptest.NewServer(mux)
}

func parse(t *testing.T, format, src string) *Suite {
	t.Helper()
	s, err := ParseSuite([]byte(src), format)
	if err != nil {
		t.Fatal(err)
	}
	s.Name = "test"
	if errs := s.Validate(); len(errs) > 0 {
		t.Fatal(errs)
	}
	return s
}

func run(t *testing.T, s *Suite, opts Options) (*Result, string) {
	t.Helper()
	var out strings.Builder
	opts.Output = &out
	if opts.RetryInitialDelay == 0 {
		opts.RetryInitialDelay = time.Millisecond
	}
	res, err := s.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, out.String()
}

func TestChainedRequestsJSON(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{
	  "variables": {"base": %q, "title": "widget-<var name=RandomString />"},
	  "steps": [
	    {"name": "create", "expectedHTTPStatus": 201,
	     "request": {"url": "<var name=base />/items", "method": "POST", "body": {"name": "<var name=title />"}},
	     "assignments": [{"var": "id", "jsonpath": "$.id"}, {"var": "name", "jsonPath": "$.name"}],
	     "conditions": [{"condition": "len(name) == 17", "expectedValue": true}]},
	    {"name": "fetch",
	     "request": {"url": "<var name=base />/items/<var name=id />", "params": {"tag": "x"}, "headers": {"X-Test": "yes"}},
	     "assignments": [{"var": "tag", "jsonPath": "$.tag"}],
	     "conditions": [{"condition": "tag == \"x\" && id == 42", "expectedValue": true}]},
	    {"name": "list", "request": {"url": "<var name=base />/items"}, "expectedResultsLength": 2}
	  ]}`, srv.URL))
	res, out := run(t, s, Options{})
	if !res.Passed() {
		t.Fatalf("expected pass, got %s\n%s", res.Status, out)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("expected 3 step results, got %d", len(res.Steps))
	}
}

func TestYAMLSuite(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
variables:
  base: %s
steps:
  - name: list
    request:
      url: <var name=base />/items
    assignments:
      - {var: second, jsonPath: "$[1].name"}
    conditions:
      - condition: second == "second"
        expectedValue: true
`, srv.URL))
	if res, out := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", out)
	}
}

func TestStatusMismatchFailsAndLaterStepsSkip(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{"steps": [
	  {"name": "boom", "request": {"url": %q}},
	  {"name": "never", "request": {"url": %q}},
	  {"name": "cleanup", "alwaysRun": true, "request": {"url": %q}}
	]}`, srv.URL+"/missing", srv.URL+"/items", srv.URL+"/items"))
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail {
		t.Fatalf("expected fail, got %s", res.Status)
	}
	got := []string{res.Steps[0].Status, res.Steps[1].Status, res.Steps[2].Status}
	want := []string{StatusFail, StatusSkipped, StatusPass}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step statuses %v, want %v", got, want)
		}
	}
}

func TestExpectedNon2xxStatus(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{"steps": [
	  {"request": {"url": %q}, "expectedHTTPStatus": 404}]}`, srv.URL+"/missing"))
	if res, out := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", out)
	}
}

func TestWaitRetriesUntilConditionMet(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{"steps": [
	  {"wait": true, "timeoutInMinutes": 1, "request": {"url": %q},
	   "assignments": [{"var": "state", "jsonPath": "$.state"}],
	   "conditions": [{"condition": "state == \"done\"", "expectedValue": true}]}]}`, srv.URL+"/job"))
	res, out := run(t, s, Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", out)
	}
	if !strings.Contains(out, "will retry") {
		t.Fatalf("expected retries in log:\n%s", out)
	}
}

func TestFailureConditionStopsWaiting(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{"steps": [
	  {"wait": true, "request": {"url": %q},
	   "assignments": [{"var": "state", "jsonPath": "$.state"}],
	   "conditions": [{"condition": "state == \"never\"", "expectedValue": true}],
	   "failureConditions": [{"condition": "state == \"running\"", "expectedValue": true}]}]}`, srv.URL+"/job"))
	res, out := run(t, s, Options{})
	if res.Status != StatusFail || !strings.Contains(out, "Failure condition") && !strings.Contains(out, "failure condition") {
		t.Fatalf("expected failure-condition stop, got %s\n%s", res.Status, out)
	}
}

func TestSkipConditionsAndOverrides(t *testing.T) {
	s := parse(t, "json", `{"variables": {"mode": "off", "count": 1, "flag": false},
	  "steps": [{"name": "guarded", "skipConditions": [{"condition": "mode == \"off\"", "expectedValue": true}],
	             "request": {"url": "127.0.0.1:1/unreachable"}}]}`)
	if res, _ := run(t, s, Options{}); res.Steps[0].Status != StatusSkipped {
		t.Fatalf("step should be skipped, got %s", res.Steps[0].Status)
	}
	// overriding the variable (case-insensitively) un-skips the step, which
	// then fails on the unreachable address
	res, _ := run(t, s, Options{Overrides: map[string]string{"MODE": "on"}})
	if res.Steps[0].Status != StatusFail {
		t.Fatalf("step should have run and failed, got %s", res.Steps[0].Status)
	}
	if _, err := s.Run(context.Background(), Options{Overrides: map[string]string{"count": "abc"}}); err == nil {
		t.Fatal("expected a conversion error for a non-numeric override")
	}
}

func TestSuiteNotMutatedAcrossRuns(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "json", fmt.Sprintf(`{"variables": {"u": %q}, "steps": [
	  {"request": {"url": "<var name=u />/items"}, "assignments": [{"var": "u", "jsonPath": "$[0].name"}]}]}`, srv.URL))
	for i := 0; i < 2; i++ {
		if res, out := run(t, s, Options{}); !res.Passed() {
			t.Fatalf("run %d failed\n%s", i, out)
		}
	}
}

func TestCodeStepsAndArtifacts(t *testing.T) {
	s := parse(t, "json", `{"steps": [
	  {"code": {"name": "double", "args": {"n": "21"}},
	   "assignments": [{"var": "answer", "jsonPath": "$.value"}, {"var": "first", "jsonPath": "$.list[0]"}],
	   "conditions": [{"condition": "answer == 42 && first == \"a\"", "expectedValue": true}]}]}`)
	fn := func(ctx context.Context, step *Step, args map[string]string) (any, error) {
		var n int
		fmt.Sscan(args["n"], &n)
		step.AddArtifact("file", "/tmp/out.txt")
		return map[string]any{"value": n * 2, "list": []string{"a", "b"}}, nil
	}
	res, out := run(t, s, Options{Funcs: map[string]Func{"double": fn}})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", out)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != "/tmp/out.txt" {
		t.Fatalf("artifact missing: %+v", res.Artifacts)
	}
	// unregistered function
	bad := parse(t, "json", `{"steps": [{"code": {"name": "nope"}}]}`)
	if res, _ := run(t, bad, Options{}); res.Status != StatusFail {
		t.Fatalf("expected fail for unknown function, got %s", res.Status)
	}
}

func TestUnknownVariableFailsStep(t *testing.T) {
	s := parse(t, "json", `{"steps": [{"request": {"url": "<var name=nope />/x"}}]}`)
	if res, _ := run(t, s, Options{}); res.Status != StatusFail {
		t.Fatalf("expected fail, got %s", res.Status)
	}
}

func TestVariablesReferenceEachOther(t *testing.T) {
	vars := map[string]any{"a": "<var name=b />/x", "b": "<var name=c />-y", "c": "z"}
	if err := resolveVariables(vars); err != nil {
		t.Fatal(err)
	}
	if vars["a"] != "z-y/x" {
		t.Fatalf("got %v", vars["a"])
	}
	if err := resolveVariables(map[string]any{"a": "<var name=b />", "b": "<var name=a />"}); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"request and code": `{"steps": [{"request": {"url": "x"}, "code": {"name": "f"}}]}`,
		"code with wait":   `{"steps": [{"code": {"name": "f"}, "wait": true}]}`,
		"no url":           `{"steps": [{"request": {"method": "GET"}}]}`,
		"no steps":         `{"steps": []}`,
		"orphan assign":    `{"steps": [{"assignments": [{"var": "a", "jsonPath": "$.a"}]}]}`,
	}
	for name, src := range cases {
		s, err := ParseSuite([]byte(src), "json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(s.Validate()) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	if _, err := ParseSuite([]byte(`{"steps": [{"requset": {}}]}`), "json"); err == nil {
		t.Error("expected unknown field to be rejected")
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"example.com/a":        "https://example.com/a",
		"localhost:8080/a":     "http://localhost:8080/a",
		"127.0.0.1:9/x?y=1":    "http://127.0.0.1:9/x?y=1",
		"http://example.com/a": "http://example.com/a",
		"https://localhost/a":  "https://localhost/a",
	} {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasTag(t *testing.T) {
	s := &Suite{Tags: []string{"IDE Testing"}}
	if !s.HasTag("ide-testing") || s.HasTag("other") {
		t.Fatal("tag matching should ignore case and punctuation")
	}
}

func TestStepOutDirAndLog(t *testing.T) {
	s := parse(t, "json", `{"steps": [{"code": {"name": "where"}, "assignments": [{"var": "d", "jsonPath": "$.dir"}],
	  "conditions": [{"condition": "d == \"/out\"", "expectedValue": true}]}]}`)
	fn := func(ctx context.Context, step *Step, args map[string]string) (any, error) {
		return map[string]any{"dir": step.OutDir()}, nil
	}
	res, out := run(t, s, Options{Funcs: map[string]Func{"where": fn}, OutDir: "/out"})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", out)
	}
	if res.Log != out || !strings.Contains(res.Log, "Final Status: pass") {
		t.Fatalf("Result.Log should equal the written log, got %q", res.Log)
	}
}

// --- exec steps ---

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// execSuite parses a suite whose File is in testdata, so exec steps run with
// testdata as their working directory.
func execSuite(t *testing.T, src string) *Suite {
	t.Helper()
	s := parse(t, "yaml", src)
	s.File = "testdata/suite.yaml"
	return s
}

func TestExecStep(t *testing.T) {
	needPython(t)
	out := t.TempDir()
	s := execSuite(t, `
variables: {greeting: hello}
steps:
  - name: compute
    exec:
      command: [python3, compute.py]
      args: {a: "40", b: "2"}
      env: {CUSTOM: from-step}
      passVars: true
    assignments:
      - {var: sum, jsonPath: $.sum}
      - {var: greet, jsonPath: $.vars.greeting}
      - {var: dir, jsonPath: $.cwd}
      - {var: envA, jsonPath: $.envArgA}
      - {var: envOut, jsonPath: $.envOutDir}
      - {var: custom, jsonPath: $.custom}
    conditions:
      - {condition: 'sum == 42 && greet == "hello" && dir == "testdata"', expectedValue: true}
      - {condition: 'envA == "40" && custom == "from-step"', expectedValue: true}
`)
	res, log := run(t, s, Options{OutDir: out})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
	if !strings.Contains(log, "compute.py running, mode=ok") {
		t.Fatalf("stderr should be in the run log:\n%s", log)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Type != "text" {
		t.Fatalf("expected the script's artifact, got %+v", res.Artifacts)
	}
	if _, err := os.Stat(res.Artifacts[0].Path); err != nil {
		t.Fatalf("artifact file missing: %v", err)
	}
}

func TestExecDoesNotPassVarsByDefault(t *testing.T) {
	needPython(t)
	s := execSuite(t, `
variables: {secret: hunter2}
steps:
  - exec: {command: [python3, compute.py]}
    assignments: [{var: v, jsonPath: $.vars}]
    conditions: [{condition: v == nil, expectedValue: true}]
`)
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("vars should be null unless passVars is set\n%s", log)
	}
}

func TestExecBetweenRequests(t *testing.T) {
	needPython(t)
	srv := newServer(t)
	defer srv.Close()
	s := execSuite(t, fmt.Sprintf(`
variables: {base: %q}
steps:
  - name: get
    request: {url: "<var name=base />/items"}
    assignments: [{var: first, jsonPath: "$[0].id"}]
  - name: script
    exec: {command: [python3, compute.py], args: {a: "<var name=first />", b: "41"}}
    assignments: [{var: id, jsonPath: $.sum}]
  - name: use it
    request: {url: "<var name=base />/items/<var name=id />"}
    assignments: [{var: seen, jsonPath: $.id}]
    conditions: [{condition: seen == 42, expectedValue: true}]
`, srv.URL))
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

func TestExecFailureTimeoutAndNonJSON(t *testing.T) {
	needPython(t)
	fail := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py], args: {mode: fail}}
`)
	res, log := run(t, fail, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "status 3") ||
		!strings.Contains(res.Steps[0].Error, "something went wrong") {
		t.Fatalf("expected exit status and stderr in the error, got %q\n%s", res.Steps[0].Error, log)
	}

	slow := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py], args: {mode: sleep}, timeoutSeconds: 1}
`)
	begin := time.Now()
	res, _ = run(t, slow, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "timed out") {
		t.Fatalf("expected timeout, got %+v", res.Steps[0])
	}
	if time.Since(begin) > 10*time.Second {
		t.Fatalf("the script was not killed promptly")
	}

	// non-JSON output passes when nothing needs to be assigned...
	text := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py], args: {mode: text}}
`)
	if res, log := run(t, text, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
	// ...and fails when an assignment needs JSON
	textAssign := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py], args: {mode: text}}
    assignments: [{var: x, jsonPath: $.x}]
`)
	if res, _ := run(t, textAssign, Options{}); res.Status != StatusFail {
		t.Fatalf("expected fail, got %s", res.Status)
	}

	missing := execSuite(t, `
steps:
  - exec: {command: [definitely-not-a-real-program-xyz]}
`)
	if res, _ := run(t, missing, Options{}); res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "could not run") {
		t.Fatalf("expected a could-not-run error, got %+v", res.Steps[0])
	}
}

func TestUntrustedRefusesExecSteps(t *testing.T) {
	s := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py]}
`)
	var ue *UntrustedError
	if _, err := s.Run(context.Background(), Options{Untrusted: true}); !errors.As(err, &ue) {
		t.Fatalf("expected an UntrustedError, got %v", err)
	}
}

func TestExecValidation(t *testing.T) {
	for name, src := range map[string]string{
		"no command":       `{"steps": [{"exec": {"command": []}}]}`,
		"exec and request": `{"steps": [{"exec": {"command": ["x"]}, "request": {"url": "u"}}]}`,
		"exec with wait":   `{"steps": [{"exec": {"command": ["x"]}, "wait": true}]}`,
	} {
		s, err := ParseSuite([]byte(src), "json")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(s.Validate()) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
