package hisepress

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// authNone, used as a step's auth, turns authentication off for that step.
const authNone = "none"

// Built-in profile types.
const (
	AuthBearer      = "bearer"
	AuthBasic       = "basic"
	AuthHeader      = "header"
	AuthOAuth2      = "oauth2-client-credentials"
	AuthExec        = "exec"
	defaultAuthHdr  = "Authorization"
	defaultScheme   = "Bearer"
	defaultTokenTTL = 5 * time.Minute
	tokenSkew       = 30 * time.Second
	authFetchLimit  = 60 * time.Second
)

// AuthProfile describes how to authenticate requests. Every secret can be
// given literally or, better, by naming an environment variable with the
// matching ...Env field, which keeps it out of suite files. All string fields
// can use <var name=x /> templates; they are expanded when the profile is
// first used.
//
//	bearer:   token | tokenEnv; header (default Authorization); scheme (default Bearer)
//	basic:    username | usernameEnv; password | passwordEnv
//	header:   header; value | valueEnv            (API keys)
//	oauth2-client-credentials:
//	          tokenUrl; clientId | clientIdEnv; clientSecret | clientSecretEnv;
//	          scope; audience; authStyle (header, the default, or body);
//	          header and scheme as for bearer
//	exec:     command (prints a token on stdout, or JSON {"token", "expiresIn"});
//	          cacheSeconds (default 300); header and scheme as for bearer
//
// For bearer, oauth2 and exec, scheme "none" sends the bare token.
//
// hosts, if set, restricts the profile to those hostnames ("api.example.com",
// or "*.example.com" for subdomains); a request to any other host fails
// rather than leaking the credential. Requests carrying credentials do not
// follow redirects to other hosts, and credentials are not sent over plain
// http except to localhost unless allowInsecure is set.
//
// Other types can be added with RegisterAuth and configured through Options.
type AuthProfile struct {
	Type  string   `json:"type"`
	Hosts []string `json:"hosts,omitempty"`
	// AllowInsecure permits sending credentials over plain http to hosts
	// other than localhost. Off by default.
	AllowInsecure bool `json:"allowInsecure,omitempty"`

	Header string `json:"header,omitempty"`
	Scheme string `json:"scheme,omitempty"`

	Token    string `json:"token,omitempty"`
	TokenEnv string `json:"tokenEnv,omitempty"`

	Username    string `json:"username,omitempty"`
	UsernameEnv string `json:"usernameEnv,omitempty"`
	Password    string `json:"password,omitempty"`
	PasswordEnv string `json:"passwordEnv,omitempty"`

	Value    string `json:"value,omitempty"`
	ValueEnv string `json:"valueEnv,omitempty"`

	TokenURL        string `json:"tokenUrl,omitempty"`
	ClientID        string `json:"clientId,omitempty"`
	ClientIDEnv     string `json:"clientIdEnv,omitempty"`
	ClientSecret    string `json:"clientSecret,omitempty"`
	ClientSecretEnv string `json:"clientSecretEnv,omitempty"`
	Scope           string `json:"scope,omitempty"`
	Audience        string `json:"audience,omitempty"`
	AuthStyle       string `json:"authStyle,omitempty"`

	Command      []string `json:"command,omitempty"`
	CacheSeconds int      `json:"cacheSeconds,omitempty"`

	// Options carries settings for custom types registered with RegisterAuth.
	Options map[string]string `json:"options,omitempty"`
}

// AuthEnv is what an AuthFactory gets from the run.
type AuthEnv struct {
	Client *http.Client
	// Dir is the suite file's directory, the working directory for commands.
	Dir string
}

// AuthFactory builds an Authenticator from a profile (templates already
// expanded). Return errors for bad configuration.
type AuthFactory func(p AuthProfile, env AuthEnv) (Authenticator, error)

var (
	authMu        sync.RWMutex
	authFactories = map[string]AuthFactory{
		AuthBearer: newBearerAuth,
		AuthBasic:  newBasicAuth,
		AuthHeader: newHeaderAuth,
		AuthOAuth2: newOAuth2Auth,
		AuthExec:   newExecAuth,
	}
)

// RegisterAuth adds a profile type, for example one that obtains tokens from
// a cloud provider. The factory is called when a step first uses a profile of
// that type; the Authenticator it returns is reused for the rest of the run,
// so it should cache its own tokens.
func RegisterAuth(profileType string, factory AuthFactory) {
	authMu.Lock()
	defer authMu.Unlock()
	authFactories[profileType] = factory
}

func authFactory(profileType string) (AuthFactory, bool) {
	authMu.RLock()
	defer authMu.RUnlock()
	f, ok := authFactories[profileType]
	return f, ok
}

// statelessAuth types are rebuilt on every use, so that templates in them can
// pick up variables assigned by earlier steps (e.g. a token from a login
// call). The rest hold cached tokens and are built once per run.
func statelessAuth(t string) bool { return t == AuthBearer || t == AuthBasic || t == AuthHeader }

// --- validation ---

// validateAuth checks profiles and references to them.
func (s *Suite) validateAuth() []error {
	var errs []error
	names := make([]string, 0, len(s.Auth))
	for n := range s.Auth {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == authNone {
			errs = append(errs, fmt.Errorf("auth profile cannot be named %q; it means no authentication", authNone))
		}
		if err := validateProfile(s.Auth[n]); err != nil {
			errs = append(errs, fmt.Errorf("auth profile %q: %w", n, err))
		}
	}
	check := func(where, name string) {
		if name == "" || name == authNone {
			return
		}
		if _, ok := s.Auth[name]; !ok {
			errs = append(errs, fmt.Errorf("%s uses auth profile %q, which the suite does not define", where, name))
		}
	}
	check("defaultAuth", s.DefaultAuth)
	for i, st := range s.Steps {
		name := st.Name
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		check(name, st.Auth)
		if st.Auth != "" && st.Request == nil {
			errs = append(errs, fmt.Errorf("%s: auth only applies to request steps", name))
		}
	}
	return errs
}

func pair(what, literal, env string) error {
	switch {
	case literal != "" && env != "":
		return fmt.Errorf("set either %s or %sEnv, not both", what, what)
	case literal == "" && env == "":
		return fmt.Errorf("%s (or %sEnv) is required", what, what)
	}
	return nil
}

func validateProfile(p AuthProfile) error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	switch p.Type {
	case "":
		return fmt.Errorf("type is required")
	case AuthBearer:
		add(pair("token", p.Token, p.TokenEnv))
	case AuthBasic:
		add(pair("username", p.Username, p.UsernameEnv))
		add(pair("password", p.Password, p.PasswordEnv))
	case AuthHeader:
		if p.Header == "" {
			add(fmt.Errorf("header is required"))
		}
		add(pair("value", p.Value, p.ValueEnv))
	case AuthOAuth2:
		if p.TokenURL == "" {
			add(fmt.Errorf("tokenUrl is required"))
		}
		add(pair("clientId", p.ClientID, p.ClientIDEnv))
		add(pair("clientSecret", p.ClientSecret, p.ClientSecretEnv))
		if p.AuthStyle != "" && p.AuthStyle != "header" && p.AuthStyle != "body" {
			add(fmt.Errorf("authStyle must be header or body"))
		}
	case AuthExec:
		if len(p.Command) == 0 || p.Command[0] == "" {
			add(fmt.Errorf("command is required"))
		}
	default:
		if _, ok := authFactory(p.Type); !ok {
			add(fmt.Errorf("unknown type %q", p.Type))
		}
	}
	return errors.Join(errs...)
}

// --- helpers ---

func secret(what, literal, envName string) (string, error) {
	if envName != "" {
		v := os.Getenv(envName)
		if v == "" {
			return "", fmt.Errorf("environment variable %s (for %s) is not set", envName, what)
		}
		return v, nil
	}
	if literal == "" {
		return "", fmt.Errorf("%s is required", what)
	}
	return literal, nil
}

func headerName(p AuthProfile) string {
	if p.Header != "" {
		return p.Header
	}
	return defaultAuthHdr
}

func headerValue(p AuthProfile, token string) string {
	scheme := p.Scheme
	if scheme == "" {
		scheme = defaultScheme
	}
	if strings.EqualFold(scheme, "none") {
		return token
	}
	return scheme + " " + token
}

// checkHost enforces a profile's host allowlist.
func checkHost(hosts []string, req *http.Request) error {
	if len(hosts) == 0 {
		return nil
	}
	host := req.URL.Hostname()
	for _, h := range hosts {
		if strings.HasPrefix(h, "*.") {
			if strings.HasSuffix(strings.ToLower(host), strings.ToLower(h[1:])) {
				return nil
			}
		} else if strings.EqualFold(host, h) {
			return nil
		}
	}
	return configErr("auth profile is restricted to hosts %v, refusing to send credentials to %q", hosts, host)
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkTransport refuses to send credentials in clear text.
func checkTransport(allowInsecure bool, req *http.Request) error {
	if allowInsecure || req.URL.Scheme == "https" || isLoopback(req.URL.Hostname()) {
		return nil
	}
	return configErr("refusing to send credentials over %s to %q; use https, or set allowInsecure on the auth profile",
		req.URL.Scheme, req.URL.Hostname())
}

// guard applies a profile's host and transport rules to a request.
func guard(hosts []string, allowInsecure bool, req *http.Request) error {
	if err := checkHost(hosts, req); err != nil {
		return err
	}
	return checkTransport(allowInsecure, req)
}

// secretProvider is implemented by authenticators that can say which strings
// must never appear in logs.
type secretProvider interface{ Secrets() []string }

func buildAuth(p AuthProfile, env AuthEnv) (Authenticator, error) {
	f, ok := authFactory(p.Type)
	if !ok {
		return nil, fmt.Errorf("unknown auth type %q", p.Type)
	}
	return f(p, env)
}

// --- static credentials ---

type headerAuth struct {
	hosts         []string
	insecure      bool
	header, value string
	secrets       []string
}

func (h *headerAuth) Secrets() []string { return append([]string{h.value}, h.secrets...) }

func (h *headerAuth) Apply(_ context.Context, req *http.Request) error {
	if err := guard(h.hosts, h.insecure, req); err != nil {
		return err
	}
	req.Header.Set(h.header, h.value)
	return nil
}

func newBearerAuth(p AuthProfile, _ AuthEnv) (Authenticator, error) {
	tok, err := secret("token", p.Token, p.TokenEnv)
	if err != nil {
		return nil, err
	}
	return &headerAuth{hosts: p.Hosts, insecure: p.AllowInsecure, header: headerName(p),
		value: headerValue(p, tok), secrets: []string{tok}}, nil
}

func newBasicAuth(p AuthProfile, _ AuthEnv) (Authenticator, error) {
	user, err := secret("username", p.Username, p.UsernameEnv)
	if err != nil {
		return nil, err
	}
	pass, err := secret("password", p.Password, p.PasswordEnv)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return &headerAuth{hosts: p.Hosts, insecure: p.AllowInsecure, header: defaultAuthHdr,
		value: "Basic " + enc, secrets: []string{enc, pass}}, nil
}

func newHeaderAuth(p AuthProfile, _ AuthEnv) (Authenticator, error) {
	val, err := secret("value", p.Value, p.ValueEnv)
	if err != nil {
		return nil, err
	}
	return &headerAuth{hosts: p.Hosts, insecure: p.AllowInsecure, header: p.Header, value: val, secrets: []string{val}}, nil
}

// --- token fetching (oauth2, exec) ---

// tokenAuth caches a fetched token until shortly before it expires.
type tokenAuth struct {
	p       AuthProfile
	secrets []string // fixed secrets, e.g. an OAuth2 client secret
	fetch   func(ctx context.Context) (token string, ttl time.Duration, err error)

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Secrets returns the current token.
func (t *tokenAuth) Secrets() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == "" {
		return t.secrets
	}
	return append([]string{t.token, headerValue(t.p, t.token)}, t.secrets...)
}

func (t *tokenAuth) Apply(ctx context.Context, req *http.Request) error {
	if err := guard(t.p.Hosts, t.p.AllowInsecure, req); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == "" || !time.Now().Before(t.expires) {
		tok, ttl, err := t.fetch(ctx)
		if err != nil {
			return err
		}
		if ttl <= 0 {
			ttl = defaultTokenTTL
		}
		skew := min(tokenSkew, ttl/2)
		t.token, t.expires = tok, time.Now().Add(ttl-skew)
	}
	req.Header.Set(headerName(t.p), headerValue(t.p, t.token))
	return nil
}

func newOAuth2Auth(p AuthProfile, env AuthEnv) (Authenticator, error) {
	id, err := secret("clientId", p.ClientID, p.ClientIDEnv)
	if err != nil {
		return nil, err
	}
	sec, err := secret("clientSecret", p.ClientSecret, p.ClientSecretEnv)
	if err != nil {
		return nil, err
	}
	if u, err := url.Parse(p.TokenURL); err != nil {
		return nil, fmt.Errorf("invalid tokenUrl: %w", err)
	} else if u.Scheme != "https" && !p.AllowInsecure && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("tokenUrl must use https (or set allowInsecure): the client secret is sent to it")
	}
	client := env.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &tokenAuth{p: p, secrets: []string{sec}, fetch: func(ctx context.Context) (string, time.Duration, error) {
		form := url.Values{"grant_type": {"client_credentials"}}
		if p.Scope != "" {
			form.Set("scope", p.Scope)
		}
		if p.Audience != "" {
			form.Set("audience", p.Audience)
		}
		if p.AuthStyle == "body" {
			form.Set("client_id", id)
			form.Set("client_secret", sec)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if p.AuthStyle != "body" {
			req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(sec))
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, fmt.Errorf("token request to %s failed: %w", p.TokenURL, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return "", 0, err
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", 0, fmt.Errorf("token request to %s returned %s: %s",
				p.TokenURL, resp.Status, truncate(string(body), 300))
		}
		var tr struct {
			AccessToken string      `json:"access_token"`
			ExpiresIn   json.Number `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &tr); err != nil {
			return "", 0, fmt.Errorf("token response from %s was not valid JSON: %w", p.TokenURL, err)
		}
		if tr.AccessToken == "" {
			return "", 0, fmt.Errorf("token response from %s had no access_token", p.TokenURL)
		}
		var ttl time.Duration
		if secs, err := tr.ExpiresIn.Float64(); err == nil {
			ttl = time.Duration(secs * float64(time.Second))
		}
		return tr.AccessToken, ttl, nil
	}}, nil
}

func newExecAuth(p AuthProfile, env AuthEnv) (Authenticator, error) {
	if len(p.Command) == 0 || p.Command[0] == "" {
		return nil, fmt.Errorf("command is required")
	}
	cache := defaultTokenTTL
	if p.CacheSeconds > 0 {
		cache = time.Duration(p.CacheSeconds) * time.Second
	}
	return &tokenAuth{p: p, fetch: func(ctx context.Context) (string, time.Duration, error) {
		ctx, cancel := context.WithTimeout(ctx, authFetchLimit)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.Command[0], p.Command[1:]...)
		cmd.Dir = env.Dir
		var stdout, stderr cappedBuffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		configureProcess(cmd)
		cmd.WaitDelay = execWaitDelay
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", 0, fmt.Errorf("auth command %s timed out after %s", p.Command[0], authFetchLimit)
			}
			return "", 0, fmt.Errorf("auth command %s failed: %w. %s", p.Command[0], err, tail(stderr.String(), 300))
		}
		out := string(bytes.TrimSpace(stdout.Bytes()))
		if out == "" {
			return "", 0, fmt.Errorf("auth command %s printed no token", p.Command[0])
		}
		if strings.HasPrefix(out, "{") {
			var j struct {
				Token     string      `json:"token"`
				ExpiresIn json.Number `json:"expiresIn"`
			}
			if err := json.Unmarshal([]byte(out), &j); err != nil || j.Token == "" {
				return "", 0, fmt.Errorf("auth command %s printed JSON without a token", p.Command[0])
			}
			ttl := cache
			if secs, err := j.ExpiresIn.Float64(); err == nil && secs > 0 {
				ttl = time.Duration(secs * float64(time.Second))
			}
			return j.Token, ttl, nil
		}
		return out, cache, nil
	}}, nil
}
