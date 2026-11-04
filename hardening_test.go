package hisepress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- redirects ---

func TestCredentialedRequestsDoNotFollowCrossHostRedirects(t *testing.T) {
	var reached atomic.Bool
	var leaked atomic.Value
	leaked.Store("")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		leaked.Store(r.Header.Get("X-Api-Key"))
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-other":
			http.Redirect(w, r, other.URL+"/", http.StatusFound)
		case "/same-1":
			http.Redirect(w, r, "/same-2", http.StatusFound)
		default:
			json.NewEncoder(w).Encode(map[string]string{"key": r.Header.Get("X-Api-Key")})
		}
	}))
	defer main.Close()

	suite := func(step string) *Suite {
		return parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q}
auth: {k: {type: header, header: X-Api-Key, value: key-for-main, hosts: [127.0.0.1]}}
defaultAuth: k
steps:
  - %s
`, main.URL, step))
	}

	// a custom-header credential must not follow a redirect to another host
	res, log := run(t, suite(`{request: {url: "<var name=base />/to-other"}}`), Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "was not followed") {
		t.Fatalf("expected the redirect to be refused, got %+v\n%s", res.Steps[0], log)
	}
	if reached.Load() || leaked.Load() != "" {
		t.Fatalf("the other host was contacted (reached=%v, header=%q)", reached.Load(), leaked.Load())
	}

	// the 3xx can be asserted on
	res, log = run(t, suite(`{request: {url: "<var name=base />/to-other"}, expectedHTTPStatus: 302}`), Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}

	// same-host redirects are followed, and keep the credential
	res, log = run(t, suite(`{request: {url: "<var name=base />/same-1"}, assignments: [{var: key, jsonPath: $.key}], conditions: [{condition: 'key == "key-for-main"'}]}`), Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}

	// without credentials, a cross-host redirect is followed as usual
	res, log = run(t, suite(`{auth: none, request: {url: "<var name=base />/to-other"}}`), Options{})
	if !res.Passed() || !reached.Load() || leaked.Load() != "" {
		t.Fatalf("an unauthenticated request should follow the redirect without credentials (reached=%v, header=%q)\n%s",
			reached.Load(), leaked.Load(), log)
	}

	// followRedirects: false returns the 3xx for any request
	res, log = run(t, suite(`{auth: none, request: {url: "<var name=base />/same-1", followRedirects: false}, expectedHTTPStatus: 302}`), Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

func TestCredentialsRefusedOverCleartext(t *testing.T) {
	a := &headerAuth{header: "X-K", value: "v"}
	for url, wantErr := range map[string]bool{
		"http://example.com/":  true,
		"https://example.com/": false,
		"http://localhost:80/": false,
		"http://127.0.0.1/":    false,
		"http://[::1]:80/":     false,
	} {
		err := a.Apply(context.Background(), mustReq(url))
		if (err != nil) != wantErr || (err != nil && !isConfigError(err)) {
			t.Errorf("%s: err=%v, want error=%v (as a config error)", url, err, wantErr)
		}
	}
	insecure := &headerAuth{header: "X-K", value: "v", insecure: true}
	if err := insecure.Apply(context.Background(), mustReq("http://example.com/")); err != nil {
		t.Errorf("allowInsecure should permit http: %v", err)
	}
	if _, err := newOAuth2Auth(AuthProfile{TokenURL: "http://login.example.com/t", ClientID: "a", ClientSecret: "b"}, AuthEnv{}); err == nil {
		t.Error("an http token endpoint should be refused: the client secret is sent to it")
	}
}

// --- errors that retrying cannot fix ---

func TestWaitDoesNotRetryConfigErrors(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
auth: {p: {type: bearer, tokenEnv: HP_DEFINITELY_UNSET_VAR_2}}
steps:
  - auth: p
    wait: true
    timeoutInMinutes: 1
    request: {url: %q}
  - name: bad url
    wait: true
    timeoutInMinutes: 1
    alwaysRun: true
    request: {url: "http://[::1"}
`, srv.URL))
	start := time.Now()
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail || time.Since(start) > 10*time.Second {
		t.Fatalf("config errors should fail at once, got %s after %s", res.Status, time.Since(start))
	}
	if !strings.Contains(res.Steps[0].Error, "HP_DEFINITELY_UNSET_VAR_2") || res.Steps[1].Status != StatusFail {
		t.Fatalf("unexpected step results: %+v", res.Steps)
	}
}

// --- overrides ---

func TestUnknownOverrideIsAnError(t *testing.T) {
	s := parse(t, "yaml", "variables: {base: x}\nsteps: [{}]")
	_, err := s.Run(context.Background(), Options{Overrides: map[string]string{"bsae": "y"}})
	if err == nil || !strings.Contains(err.Error(), "does not define") {
		t.Fatalf("expected an error naming the unknown variable, got %v", err)
	}
}

// --- request/response handling ---

func TestExpectedResultsLengthNeedsAnArray(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
steps:
  - request: {url: %q}
    expectedResultsLength: 5
`, srv.URL+"/items/42")) // returns an object
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "not a JSON array") {
		t.Fatalf("expected a failure explaining the response is not an array, got %+v", res.Steps[0])
	}
}

func TestQueryStringIsSentAsWritten(t *testing.T) {
	var raw atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw.Store(r.URL.RawQuery)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, tc := range []struct{ step, want string }{
		{`request: {url: "<var name=base />/q?f=a;b&flag&z=1"}`, "f=a;b&flag&z=1"},
		{`request: {url: "<var name=base />/q?f=a;b&flag", params: {z: "1"}}`, "f=a;b&flag&z=1"},
		{`request: {url: "<var name=base />/q", params: {k: [a, b], n: 3}}`, "k=a&k=b&n=3"},
	} {
		s := parse(t, "yaml", fmt.Sprintf("variables: {base: %q}\nsteps:\n  - %s\n", srv.URL, tc.step))
		if res, log := run(t, s, Options{}); !res.Passed() || raw.Load() != tc.want {
			t.Errorf("%s: sent %q, want %q\n%s", tc.step, raw.Load(), tc.want, log)
		}
	}
}

func TestNormalizeURLWithSchemeLookalike(t *testing.T) {
	for in, want := range map[string]string{
		"api.example.com/r?u=http://x": "https://api.example.com/r?u=http://x",
		"localhost:8080/r?u=https://x": "http://localhost:8080/r?u=https://x",
		"ftp://example.com/x":          "ftp://example.com/x",
	} {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResponseSizeIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxResponseBytes+1))
	}))
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf("steps: [{request: {url: %q}}]", srv.URL))
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "larger than") {
		t.Fatalf("expected an oversized-response failure, got %+v", res.Steps[0])
	}
}

func TestBuiltInsAgreeWithinAStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"a": r.URL.Query().Get("a"), "h": r.Header.Get("X-R")})
	}))
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
steps:
  - request:
      url: %q
      params: {a: "<var name=RandomString />"}
      headers: {X-R: "<var name=RandomString />"}
    assignments: [{var: a, jsonPath: $.a}, {var: h, jsonPath: $.h}]
    conditions: [{condition: 'a == h && len(a) == 10'}]
`, srv.URL))
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("a built-in should have one value within a step\n%s", log)
	}
}

// --- step ordering and conditions ---

func TestCleanupRunsAfterCancellation(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
steps:
  - name: main
    request: {url: %q}
  - name: cleanup
    alwaysRun: true
    request: {url: %q}
`, srv.URL, srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := s.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].Status != StatusSkipped || res.Steps[1].Status != StatusPass || res.Status != StatusError {
		t.Fatalf("expected main skipped, cleanup passing, run in error: %+v (%s)", res.Steps, res.Status)
	}
}

func TestSkipConditionErrorsDoNotMaskFailure(t *testing.T) {
	srv := newServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
steps:
  - name: fails
    request: {url: %q}
  - name: skipped without evaluating its broken condition
    skipConditions: [{condition: "this is not an expression ((", expectedValue: true}]
  - name: forced, with a broken condition
    alwaysRun: true
    skipConditions: [{condition: "this is not an expression ((", expectedValue: true}]
`, srv.URL+"/missing"))
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail {
		t.Fatalf("the suite failed at step 1 and must stay failed, got %s", res.Status)
	}
	if res.Steps[1].Status != StatusSkipped || res.Steps[2].Status != StatusError {
		t.Fatalf("unexpected step statuses: %+v", res.Steps)
	}
}

func TestExpectedValueDefaultsToTrue(t *testing.T) {
	s := parse(t, "yaml", `
variables: {x: 1}
steps:
  - conditions:
      - {condition: "x == 1"}
      - {condition: "x == 2", expectedValue: false}
`)
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
	wrong := parse(t, "yaml", "variables: {x: 1}\nsteps: [{conditions: [{condition: 'x == 2'}]}]")
	if res, _ := run(t, wrong, Options{}); res.Status != StatusFail {
		t.Fatal("a condition with no expectedValue must expect true")
	}
	if _, err := ParseSuite([]byte(`steps: [{conditions: [{condition: x, expectedvalue: true, typo: 1}]}]`), "yaml"); err == nil {
		t.Fatal("unknown fields in a condition must still be rejected")
	}
}

// --- secrets ---

func TestSecretsAreRedactedFromLogsAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/teapot" {
			w.WriteHeader(http.StatusTeapot)
			fmt.Fprint(w, `{"detail": "your token tok-abcdef123456 is bad"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok-abcdef123456", "authorization": r.Header.Get("Authorization")})
	}))
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q}
secrets: [tok]
defaultAuth: p
auth: {p: {type: bearer, token: bearer-secret-9999}}
steps:
  - name: login
    request: {url: "<var name=base />/"}
    assignments: [{var: tok, jsonPath: $.token}]
  - name: fails and quotes the body
    request: {url: "<var name=base />/teapot"}
`, srv.URL))
	res, log := run(t, s, Options{Verbose: true})
	if res.Status != StatusFail {
		t.Fatalf("expected the second step to fail, got %s", res.Status)
	}
	for _, secret := range []string{"tok-abcdef123456", "bearer-secret-9999"} {
		if strings.Contains(log, secret) || strings.Contains(res.Log, secret) {
			t.Errorf("log leaks %q:\n%s", secret, log)
		}
		for _, st := range res.Steps {
			if strings.Contains(st.Error, secret) {
				t.Errorf("step error leaks %q: %s", secret, st.Error)
			}
		}
	}
	if !strings.Contains(log, redacted) {
		t.Errorf("expected redaction markers in the log:\n%s", log)
	}
}

// --- untrusted mode ---

func TestUntrustedRefusals(t *testing.T) {
	cases := map[string]struct {
		src    string
		allow  []string
		reason string
	}{
		"exec step":      {`steps: [{exec: {command: [x]}}]`, nil, "exec steps"},
		"exec auth":      {`auth: {p: {type: exec, command: [x]}}` + "\nsteps: [{}]", nil, "runs a command"},
		"env credential": {`auth: {p: {type: bearer, tokenEnv: AWS_SECRET_ACCESS_KEY}}` + "\nsteps: [{}]", nil, "AWS_SECRET_ACCESS_KEY"},
		"env not listed": {`auth: {p: {type: bearer, tokenEnv: B}}` + "\nsteps: [{}]", []string{"A"}, "B"},
	}
	for name, tc := range cases {
		s := parse(t, "yaml", tc.src)
		_, err := s.Run(context.Background(), Options{Untrusted: true, AllowEnv: tc.allow})
		var ue *UntrustedError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: expected an UntrustedError mentioning %q, got %v", name, tc.reason, err)
		}
	}
}

func TestUntrustedAllowsListedEnvAndRunsNormally(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	t.Setenv("HP_ALLOWED_TOKEN", "allowed-token-value")
	s := parse(t, "yaml", fmt.Sprintf(`
defaultAuth: p
auth: {p: {type: bearer, tokenEnv: HP_ALLOWED_TOKEN}}
steps:
  - request: {url: %q}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer allowed-token-value"'}]
`, srv.URL))
	res, log := run(t, s, Options{Untrusted: true, AllowEnv: []string{"HP_ALLOWED_TOKEN"}})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

func TestUntrustedConfinesUploadedFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	dir, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("ok"), 0o600))
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600))
	must(os.Symlink(outside, filepath.Join(dir, "escape")))

	suiteWith := func(path string) *Suite {
		s := parse(t, "yaml", fmt.Sprintf("steps:\n  - request: {url: %q, method: POST, files: {f: %q}}\n", srv.URL, path))
		s.File = filepath.Join(dir, "suite.yaml")
		return s
	}
	for _, tc := range []struct {
		path      string
		untrusted bool
		wantPass  bool
	}{
		{"inside.txt", true, true},
		{filepath.Join(dir, "inside.txt"), true, true},
		{"../" + filepath.Base(outside) + "/secret.txt", true, false},
		{filepath.Join(outside, "secret.txt"), true, false},
		{"escape/secret.txt", true, false}, // symlink out of the directory
		{"missing.txt", true, false},
		// trusted mode: paths resolve against the suite directory, and may leave it
		{"inside.txt", false, true},
		{filepath.Join(outside, "secret.txt"), false, true},
	} {
		res, log := run(t, suiteWith(tc.path), Options{Untrusted: tc.untrusted})
		if res.Passed() != tc.wantPass {
			t.Errorf("file %q (untrusted=%v): passed=%v, want %v\n%s", tc.path, tc.untrusted, res.Passed(), tc.wantPass, log)
		}
	}
}

func TestUntrustedBlocksMetadataAddresses(t *testing.T) {
	start := time.Now()
	_, err := untrustedClient().Get("http://169.254.169.254/latest/meta-data/")
	if err == nil || !strings.Contains(err.Error(), "untrusted mode refuses") {
		t.Fatalf("expected the dial to be refused, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the refusal should be immediate, not a connection timeout")
	}

	s := parse(t, "yaml", `steps: [{request: {url: "http://169.254.169.254/"}}]`)
	res, _ := run(t, s, Options{Untrusted: true})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "refuses to connect") {
		t.Fatalf("expected the step to fail on the blocked address, got %+v", res.Steps[0])
	}
	for _, ip := range []string{"fe80::1", "169.254.1.1", "0.0.0.0", "fd00:ec2::254"} {
		if !blockedAddress(netIP(t, ip)) {
			t.Errorf("%s should be blocked", ip)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "93.184.216.34", "::1"} {
		if blockedAddress(netIP(t, ip)) {
			t.Errorf("%s should not be blocked", ip)
		}
	}
}

// --- exec cancellation ---

func TestExecReportsCancellation(t *testing.T) {
	needPython(t)
	s := execSuite(t, `
steps:
  - exec: {command: [python3, compute.py], args: {mode: sleep}}
`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	res, err := s.Run(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "cancelled") {
		t.Fatalf("expected a cancellation error, got %+v", res.Steps[0])
	}
}

func netIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad ip %q", s)
	}
	return ip
}
