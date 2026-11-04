package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeSuite(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunExitCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok": true}`)
	}))
	defer srv.Close()

	suite := writeSuite(t, "ok.yaml", fmt.Sprintf(`
tags: [smoke]
variables: {base: %s}
steps:
  - request: {url: "<var name=base />/"}
    assignments: [{var: ok, jsonPath: $.ok}]
    conditions: [{condition: ok, expectedValue: true}]
`, srv.URL))

	var out, errb bytes.Buffer
	if code := run([]string{"run", suite}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Fatalf("summary missing:\n%s", out.String())
	}

	// flags may follow the path; a bad override value is a usage error
	out.Reset()
	if code := run([]string{"run", suite, "--report", "json", "--var", "base=" + srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("exit %d\n%s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"status": "pass"`) {
		t.Fatalf("json report missing:\n%s", out.String())
	}

	// pointing at a dead server is a test failure (1), not a usage error (2)
	out.Reset()
	if code := run([]string{"run", suite, "--var", "base=http://127.0.0.1:1"}, &out, &errb); code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}

	// a tag that matches nothing is an error
	if code := run([]string{"run", suite, "--tag", "nope"}, &out, &errb); code != 2 {
		t.Fatalf("expected exit 2 for no matching suites, got %d", code)
	}
}

func TestValidateAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	bad := writeSuite(t, "bad.json", `{"steps": [{"request": {"url": "x"}, "code": {"name": "f"}}]}`)
	if code := run([]string{"validate", bad}, &out, &errb); code != 2 {
		t.Fatalf("expected exit 2 for an invalid suite, got %d", code)
	}
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("expected exit 2 with no args, got %d", code)
	}
}

func TestOutDirAndReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "nope", http.StatusTeapot)
			return
		}
		fmt.Fprint(w, `{"ok": true}`)
	}))
	defer srv.Close()
	suite := writeSuite(t, "my suite.yaml", fmt.Sprintf(`
name: My Suite!
steps:
  - name: fine
    request: {url: %s/}
  - name: broken
    request: {url: %s/bad}
  - name: after
    request: {url: %s/}
`, srv.URL, srv.URL, srv.URL))
	out := filepath.Join(t.TempDir(), "nested", "out")

	var stdout, stderr bytes.Buffer
	code := run([]string{"run", suite, "--out", out, "--report", "junit"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d\n%s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "<?xml") || !strings.Contains(stdout.String(), `<failure message=`) {
		t.Fatalf("stdout should be junit xml with a failure:\n%s", stdout.String())
	}
	for _, name := range []string{"01-my-suite.log", "results.json", "junit.xml"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	logBytes, _ := os.ReadFile(filepath.Join(out, "01-my-suite.log"))
	if !strings.Contains(string(logBytes), "SUITE FAILED") {
		t.Errorf("log file lacks run log:\n%s", logBytes)
	}
	junit, _ := os.ReadFile(filepath.Join(out, "junit.xml"))
	if !strings.Contains(string(junit), `tests="3" failures="1" errors="0" skipped="1"`) {
		t.Errorf("unexpected junit counts:\n%s", junit)
	}
}

func TestUntrustedFlag(t *testing.T) {
	suite := writeSuite(t, "exec.yaml", `
steps:
  - exec: {command: [python3, anything.py]}
`)
	var out, errb bytes.Buffer
	// flag may come before or after the path
	for _, args := range [][]string{{"run", "--untrusted", suite}, {"run", suite, "--untrusted"}} {
		errb.Reset()
		if code := run(args, &out, &errb); code != 2 {
			t.Fatalf("%v: expected exit 2, got %d", args, code)
		}
		if !strings.Contains(errb.String(), "untrusted mode") {
			t.Fatalf("%v: stderr should explain, got %q", args, errb.String())
		}
	}
}

func TestSchemaCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"schema"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("schema output is not JSON: %v", err)
	}
	if doc["$schema"] == nil || doc["$defs"] == nil {
		t.Fatalf("unexpected schema: %.200s", out.String())
	}
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok": true}`)
	}))
}

func TestVarOverrides(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	suite := writeSuite(t, "s.yaml", fmt.Sprintf(`
variables: {base: %s}
steps: [{request: {url: "<var name=base />/"}}]
`, srv.URL))
	var out, errb bytes.Buffer

	// a mistyped --var must not silently run with the default
	if code := run([]string{"run", suite, "--var", "bsae=http://127.0.0.1:1"}, &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "no suite defines") {
		t.Fatalf("expected exit 2 naming the bad variable, got %d: %s", code, errb.String())
	}

	// environment overrides apply where they match and are ignored elsewhere
	t.Setenv("HISEPRESS_VAR_NOT_A_VARIABLE", "x")
	t.Setenv("HISEPRESS_VAR_BASE", "http://127.0.0.1:1")
	errb.Reset()
	if code := run([]string{"run", suite}, &out, &errb); code != 1 {
		t.Fatalf("HISEPRESS_VAR_BASE should have redirected the request (and failed it); exit %d: %s", code, errb.String())
	}
	// and an explicit --var beats the environment
	out.Reset()
	if code := run([]string{"run", suite, "--var", "base=" + srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("--var should win over the environment, exit %d\n%s", code, out.String())
	}
}

func TestReportsSurviveALaterSuiteError(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a-good.yaml"), []byte(fmt.Sprintf("steps: [{request: {url: %s/}}]", srv.URL)), 0o644)
	os.WriteFile(filepath.Join(dir, "b-bad.yaml"), []byte("steps: [{exec: {command: [x]}}]"), 0o644)
	out := filepath.Join(t.TempDir(), "out")

	var stdout, stderr bytes.Buffer
	code := run([]string{"run", dir, "--untrusted", "--out", out}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected exit 2 for the refused suite, got %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(out, "results.json"))
	if err != nil {
		t.Fatalf("the first suite's results were lost: %v", err)
	}
	if !strings.Contains(string(raw), `"status": "pass"`) {
		t.Fatalf("results.json should hold the passing suite:\n%s", raw)
	}
}

func TestOutputFilesArePrivate(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	suite := writeSuite(t, "s.yaml", fmt.Sprintf("steps: [{request: {url: %s/}}]", srv.URL))
	out := filepath.Join(t.TempDir(), "out")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", suite, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on windows")
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("out dir mode %v, want 0700", info.Mode().Perm())
	}
	files, _ := filepath.Glob(filepath.Join(out, "*"))
	if len(files) != 3 {
		t.Fatalf("expected 3 output files, got %v", files)
	}
	for _, f := range files {
		if info, _ := os.Stat(f); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, info.Mode().Perm())
		}
	}
}

func TestAllowEnvFlag(t *testing.T) {
	srv := okServer(t)
	defer srv.Close()
	t.Setenv("HP_CLI_TOKEN", "a-long-enough-token")
	suite := writeSuite(t, "s.yaml", fmt.Sprintf(`
defaultAuth: p
auth: {p: {type: bearer, tokenEnv: HP_CLI_TOKEN}}
steps: [{request: {url: %s/}}]
`, srv.URL))
	var out, errb bytes.Buffer
	if code := run([]string{"run", suite, "--untrusted"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "HP_CLI_TOKEN") {
		t.Fatalf("expected the env read to be refused, got %d: %s", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"run", suite, "--untrusted", "--allow-env", "HP_CLI_TOKEN"}, &out, &errb); code != 0 {
		t.Fatalf("expected pass with --allow-env, got %d: %s", code, errb.String())
	}
}
