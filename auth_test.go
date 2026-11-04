package hisepress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echoServer reports the auth-related headers it received.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"authorization": r.Header.Get("Authorization"),
			"apiKey":        r.Header.Get("X-Api-Key"),
			"xToken":        r.Header.Get("X-Token"),
		})
	}))
}

func mustReq(u string) *http.Request {
	r, _ := http.NewRequest("GET", u, nil)
	return r
}

func TestAuthProfiles(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	t.Setenv("HP_TEST_TOKEN", "from-env")
	basic := base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q, dynamic: late-token}
defaultAuth: literal
auth:
  literal: {type: bearer, token: abc123}
  fromenv: {type: bearer, tokenEnv: HP_TEST_TOKEN}
  custom:  {type: bearer, token: raw, header: X-Token, scheme: none}
  user:    {type: basic, username: alice, password: s3cret}
  apikey:  {type: header, header: X-Api-Key, value: key-1}
  templated: {type: bearer, token: "<var name=dynamic />"}
steps:
  - name: default
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer abc123"', expectedValue: true}]
  - name: env
    auth: fromenv
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer from-env"', expectedValue: true}]
  - name: custom header, bare token
    auth: custom
    request: {url: "<var name=base />/"}
    assignments: [{var: tok, jsonPath: $.xToken}, {var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'tok == "raw" && got == ""', expectedValue: true}]
  - name: basic
    auth: user
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Basic %s"', expectedValue: true}]
  - name: api key
    auth: apikey
    request: {url: "<var name=base />/"}
    assignments: [{var: key, jsonPath: $.apiKey}, {var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'key == "key-1" && got == ""', expectedValue: true}]
  - name: none disables the default
    auth: none
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == ""', expectedValue: true}]
  - name: variables are visible to profiles
    auth: templated
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer late-token"', expectedValue: true}]
`, srv.URL, basic))
	res, log := run(t, s, Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

// The run log records requests, never their headers. (A server that echoes
// credentials back in its response would put them in the log; this one doesn't.)
func TestAuthSecretsNotLogged(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer hunter2-secret" {
			seen.Add(1)
		}
		w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()
	t.Setenv("HP_LOG_SECRET", "hunter2-secret")
	s := parse(t, "yaml", fmt.Sprintf(`
defaultAuth: p
auth: {p: {type: bearer, tokenEnv: HP_LOG_SECRET}}
steps: [{request: {url: %q}}]
`, srv.URL))
	res, log := run(t, s, Options{Verbose: true})
	if !res.Passed() || seen.Load() != 1 {
		t.Fatalf("auth header should have been sent (seen=%d)\n%s", seen.Load(), log)
	}
	if strings.Contains(log, "hunter2-secret") {
		t.Errorf("run log leaks the secret:\n%s", log)
	}
}

func TestAuthStatelessProfilesSeeLaterAssignments(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	// a "login" step assigns a token that a bearer profile then uses
	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q, tok: first}
auth: {p: {type: bearer, token: "<var name=tok />"}}
steps:
  - auth: p
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}, {var: tok, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer first"', expectedValue: true}]
  - auth: p
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer Bearer first"', expectedValue: true}]
`, srv.URL))
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("the profile should be rebuilt with the reassigned variable\n%s", log)
	}
}

func TestAuthPrecedenceAndOptionsFallback(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q}
auth: {p: {type: bearer, token: from-profile}}
steps:
  - name: library-level auth is the fallback
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer from-options"', expectedValue: true}]
  - name: a step profile beats it
    auth: p
    request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer from-profile"', expectedValue: true}]
`, srv.URL))
	res, log := run(t, s, Options{Auth: &headerAuth{header: "Authorization", value: "Bearer from-options"}})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

func TestAuthMissingEnvFailsStep(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
auth: {p: {type: bearer, tokenEnv: HP_DEFINITELY_UNSET_VAR}}
steps:
  - auth: p
    request: {url: %q}
`, srv.URL))
	res, _ := run(t, s, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "HP_DEFINITELY_UNSET_VAR") {
		t.Fatalf("expected a clear missing-env error, got %+v", res.Steps[0])
	}
}

func TestAuthHostAllowlist(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	for hosts, want := range map[string]bool{
		`["127.0.0.1"]`:   true,
		`["example.com"]`: false,
		`["*.invalid"]`:   false,
	} {
		s := parse(t, "yaml", fmt.Sprintf(`
auth: {p: {type: bearer, token: t, hosts: %s}}
steps:
  - auth: p
    request: {url: %q}
`, hosts, srv.URL))
		res, _ := run(t, s, Options{})
		if res.Passed() != want {
			t.Errorf("hosts %s: passed=%v, want %v (%+v)", hosts, res.Passed(), want, res.Steps[0])
		}
	}
	if checkHost([]string{"*.example.com"}, mustReq("https://api.example.com/x")) != nil ||
		checkHost([]string{"*.example.com"}, mustReq("https://evilexample.com/x")) == nil {
		t.Error("wildcard hosts should match subdomains only")
	}
}

func TestOAuth2ClientCredentialsCachesToken(t *testing.T) {
	api := echoServer(t)
	defer api.Close()
	var fetches atomic.Int32
	var gotForm url.Values
	var gotBasic string
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		r.ParseForm()
		gotForm = r.PostForm
		u, p, _ := r.BasicAuth()
		gotBasic = u + ":" + p
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", fetches.Load()), "expires_in": 3600})
	}))
	defer tokens.Close()
	t.Setenv("HP_TEST_SECRET", "shh")

	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q}
defaultAuth: oauth
auth:
  oauth:
    type: oauth2-client-credentials
    tokenUrl: %s/token
    clientId: my-client
    clientSecretEnv: HP_TEST_SECRET
    scope: read write
steps:
  - request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer tok-1"', expectedValue: true}]
  - request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer tok-1"', expectedValue: true}]
`, api.URL, tokens.URL))
	res, log := run(t, s, Options{})
	if !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
	if fetches.Load() != 1 {
		t.Errorf("token fetched %d times, want 1 (cached)", fetches.Load())
	}
	if gotForm.Get("grant_type") != "client_credentials" || gotForm.Get("scope") != "read write" {
		t.Errorf("unexpected token form: %v", gotForm)
	}
	if gotBasic != "my-client:shh" {
		t.Errorf("client credentials should arrive as basic auth, got %q", gotBasic)
	}
}

func TestOAuth2RefreshesExpiredTokenAndBodyStyle(t *testing.T) {
	var fetches atomic.Int32
	var gotForm url.Values
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		r.ParseForm()
		gotForm = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 0.02})
	}))
	defer tokens.Close()
	a, err := newOAuth2Auth(AuthProfile{TokenURL: tokens.URL, ClientID: "id", ClientSecret: "sec", AuthStyle: "body"}, AuthEnv{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.Apply(context.Background(), mustReq("https://x/")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if fetches.Load() != 2 {
		t.Errorf("expected a refetch after expiry, got %d fetches", fetches.Load())
	}
	if gotForm.Get("client_id") != "id" || gotForm.Get("client_secret") != "sec" {
		t.Errorf("body style should send credentials in the form: %v", gotForm)
	}
}

func TestOAuth2ErrorResponse(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	}))
	defer tokens.Close()
	a, _ := newOAuth2Auth(AuthProfile{TokenURL: tokens.URL, ClientID: "id", ClientSecret: "sec"}, AuthEnv{})
	err := a.Apply(context.Background(), mustReq("https://x/"))
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("expected a 401 error quoting the response, got %v", err)
	}
}

func TestExecAuth(t *testing.T) {
	needPython(t)
	srv := echoServer(t)
	defer srv.Close()
	s := parse(t, "yaml", fmt.Sprintf(`
variables: {base: %q}
defaultAuth: script
auth:
  script:
    type: exec
    command: [python3, -c, "print('script-token')"]
  jsonscript:
    type: exec
    command: [python3, -c, "import json; print(json.dumps({'token': 'json-token', 'expiresIn': 60}))"]
    header: X-Token
    scheme: none
steps:
  - request: {url: "<var name=base />/"}
    assignments: [{var: got, jsonPath: $.authorization}]
    conditions: [{condition: 'got == "Bearer script-token"', expectedValue: true}]
  - auth: jsonscript
    request: {url: "<var name=base />/"}
    assignments: [{var: tok, jsonPath: $.xToken}]
    conditions: [{condition: 'tok == "json-token"', expectedValue: true}]
`, srv.URL))
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}

	bad := parse(t, "yaml", fmt.Sprintf(`
auth: {p: {type: exec, command: [python3, -c, "import sys; sys.stderr.write('no creds'); sys.exit(2)"]}}
steps:
  - auth: p
    request: {url: %q}
`, srv.URL))
	res, _ := run(t, bad, Options{})
	if res.Status != StatusFail || !strings.Contains(res.Steps[0].Error, "no creds") {
		t.Fatalf("expected the command's stderr in the error, got %+v", res.Steps[0])
	}

	// untrusted mode also covers exec auth profiles
	var ue *UntrustedError
	if _, err := s.Run(context.Background(), Options{Untrusted: true}); !errors.As(err, &ue) {
		t.Fatalf("expected an UntrustedError, got %v", err)
	}
}

func TestExecAuthCachesToken(t *testing.T) {
	needPython(t)
	counter := t.TempDir() + "/count"
	script := fmt.Sprintf(`
import os
n = int(open(%q).read()) if os.path.exists(%q) else 0
open(%q, "w").write(str(n + 1))
print("tok-%%d" %% (n + 1))`, counter, counter, counter)
	a, err := newExecAuth(AuthProfile{Command: []string{"python3", "-c", script}}, AuthEnv{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		req := mustReq("https://x/")
		if err := a.Apply(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("call %d: got %q, want the cached tok-1", i, got)
		}
	}
}

func TestCustomAuthType(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()
	RegisterAuth("test-custom", func(p AuthProfile, env AuthEnv) (Authenticator, error) {
		return &headerAuth{header: "X-Api-Key", value: "custom-" + p.Options["who"]}, nil
	})
	s := parse(t, "yaml", fmt.Sprintf(`
auth: {c: {type: test-custom, options: {who: bob}}}
steps:
  - auth: c
    request: {url: %q}
    assignments: [{var: key, jsonPath: $.apiKey}]
    conditions: [{condition: 'key == "custom-bob"', expectedValue: true}]
`, srv.URL))
	if res, log := run(t, s, Options{}); !res.Passed() {
		t.Fatalf("expected pass\n%s", log)
	}
}

func TestAuthValidation(t *testing.T) {
	cases := map[string]string{
		"unknown type":      `auth: {p: {type: nope}}`,
		"no type":           `auth: {p: {token: x}}`,
		"bearer no token":   `auth: {p: {type: bearer}}`,
		"token and env":     `auth: {p: {type: bearer, token: a, tokenEnv: B}}`,
		"basic no password": `auth: {p: {type: basic, username: u}}`,
		"header no header":  `auth: {p: {type: header, value: v}}`,
		"oauth no url":      `auth: {p: {type: oauth2-client-credentials, clientId: a, clientSecret: b}}`,
		"oauth bad style":   `auth: {p: {type: oauth2-client-credentials, tokenUrl: u, clientId: a, clientSecret: b, authStyle: x}}`,
		"exec no command":   `auth: {p: {type: exec}}`,
		"reserved name":     `auth: {none: {type: bearer, token: x}}`,
		"unknown default":   `defaultAuth: ghost`,
		"unknown step auth": `steps: [{auth: ghost, request: {url: u}}]`,
		"auth on non-request": `auth: {p: {type: bearer, token: x}}
steps: [{auth: p, exec: {command: [x]}}]`,
	}
	for name, body := range cases {
		src := body
		if !strings.Contains(body, "steps:") {
			src += "\nsteps: [{request: {url: u}}]"
		}
		s, err := ParseSuite([]byte(src), "yaml")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(s.Validate()) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
